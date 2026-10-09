// Registered payer discovery must leave the existing registration lane enough
// of the same bounded turn to admit old intents with no payer key.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The second discovery call waits on its actual supplied deadline. The normal
// registration owner must still commit the missing key, preserving both the
// completed payer prefix and every financial/retry field on the old intent.
func TestLegacyDispatchSlowDiscoveryStillRegistersMissingPayer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture, id := legacySettlementTestIntent(t, ctx)
		shard := int(id[15]) % LegacySettlementShardCount
		now := server.NowUtc().Truncate(time.Microsecond)
		due := now.Add(-64 * time.Hour)
		after := &LegacySettlementCursor{NextAttemptTime: now.Add(-90 * time.Hour),
			ContractId: server.NewId(), PassEndTime: now.Add(-84 * time.Hour)}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
				SET payer_network_id=NULL,next_attempt_time=$2 WHERE contract_id=$1`, id, due))
		}, server.TxReadCommitted, server.OptNoRetry())
		readyPayer := server.NewId()
		payerAfter := &LegacySettlementPayerCursor{End: readyPayer, PassEndTime: now}
		bounded, cancel := context.WithTimeoutCause(ctx, 5*time.Second, errLegacySettlementDispatchBudget)
		defer cancel()
		probes, registrations := 0, 0
		var registrationContextLive bool
		result, err := dispatchLegacySettlementPayersPage(ctx, bounded, shard, after, payerAfter,
			func(callCtx context.Context, _ int, _ *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition) {
				probes++
				if probes == 1 {
					return &readyPayer, &LegacySettlementPosition{NextAttemptTime: due, ContractId: id}
				}
				<-callCtx.Done()
				server.Raise(callCtx.Err())
				return nil, nil
			}, func(callCtx context.Context, callShard int, cursor *LegacySettlementCursor) (*LegacySettlementCursor, int) {
				registrations++
				registrationContextLive = callCtx.Err() == nil && bounded.Err() == nil
				return registerLegacySettlementPayerDispatchPage(callCtx, callShard, cursor)
			})
		if registrations != 1 || !registrationContextLive || result.Registered != 1 || result.RegistrationFailed {
			t.Fatal("slow registered-payer discovery starved the missing-payer registration lane",
				registrations, registrationContextLive, result.Registered, result.RegistrationFailed, err)
		}
		if err != nil || probes != 2 || result.Probes != 1 || !result.More || result.Cursor != after ||
			len(result.PayerNetworkIds) != 1 || result.PayerNetworkIds[0] != readyPayer ||
			result.PayerCursor == nil || result.PayerCursor.After == nil || *result.PayerCursor.After != readyPayer || payerAfter.After != nil {
			t.Fatal("discovery deadline changed ready-prefix or cursor custody", result, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var registered, unchanged bool
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id=$2,
				next_attempt_time=$3 AND failure_code='none' AND outcome='settled' AND NOT clear_dispute
				FROM legacy_settlement_intent WHERE contract_id=$1`, id, fixture.sourceNetworkId, due).Scan(&registered, &unchanged))
			if !registered || !unchanged {
				t.Fatal("registration lost exact payer metadata or changed financial eligibility")
			}
		})
		requireLegacySettlementTestState(t, ctx, fixture, id, true, false, 1000, 100)
	})
}
