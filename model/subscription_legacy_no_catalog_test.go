// The actual settlement entries must do their bounded work even when a
// catalog-status query is refused. Index presence is a deployment prerequisite.
package model

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type legacySettlementCatalogTripwire struct{ calls atomic.Int64 }

func (self *legacySettlementCatalogTripwire) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "legacy_settlement_intent_payer") &&
		(strings.Contains(data.SQL, "pg_index") || strings.Contains(data.SQL, "pg_get_indexdef")) {
		self.calls.Add(1)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		return canceled
	}
	return ctx
}

func (self *legacySettlementCatalogTripwire) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestLegacySettlementEntriesDoNotReadIndexCatalog(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		payer, payerId := legacySettlementTestIntent(t, ctx)
		missing, missingId := legacySettlementTestIntent(t, ctx)
		financial, financialId := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=NULL,source_client_id=NULL WHERE contract_id=$1`, missingId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=NULL,source_client_id=NULL WHERE contract_id=$1`, missingId))
		})
		tripwire := &legacySettlementCatalogTripwire{}
		scope, err := server.NewTestPgQueryScope(ctx, tripwire)
		server.Raise(err)
		defer func() { server.Raise(scope.Close()) }()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		paid, err := ApplyLegacyPayerSettlements(&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: payer.sourceNetworkId}, owner)
		if err != nil || paid.Completed != 1 || tripwire.calls.Load() != 0 {
			t.Fatal("payer entry consulted catalog instead of running normal work", paid, err, tripwire.calls.Load())
		}
		shard := int(missingId[15]) % LegacySettlementShardCount
		dispatched, err := DispatchLegacySettlementPayers(ctx, shard, nil, nil)
		if err != nil || dispatched.RegistrationFailed || dispatched.Registered != 1 {
			t.Fatal("ordinary registration lost the retained no-payer contract", dispatched, err)
		}
		dispatched, err = DispatchLegacySettlementPayers(ctx, shard, dispatched.Cursor, dispatched.PayerCursor)
		if err != nil || !slices.Contains(dispatched.PayerNetworkIds, missing.sourceNetworkId) {
			t.Fatal("ordinary dispatch lost the registered no-payer contract", dispatched, err)
		}
		registered, err := ApplyLegacyPayerSettlements(&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: missing.sourceNetworkId}, owner)
		if err != nil || registered.Completed != 1 {
			t.Fatal("registered no-payer contract lost its financial owner", registered, err)
		}
		page, err := FlushLegacySettlementShard(ctx, int(financialId[15])%LegacySettlementShardCount, nil, nil, 4)
		if err != nil || page.Completed != 1 {
			t.Fatal("ordinary financial shard changed after catalog removal", page, err)
		}
		empty, err := ApplyLegacyPayerSettlements(&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: server.NewId()}, owner)
		if err != nil || empty.Visited != 0 || tripwire.calls.Load() != 0 {
			t.Fatal("empty payer or another settlement entry attempted a catalog gate", empty, err, tripwire.calls.Load())
		}
		for _, fixture := range []struct {
			value netEscrowOrderingTestFixture
			id    server.Id
		}{{value: payer, id: payerId}, {value: missing, id: missingId}, {value: financial, id: financialId}} {
			requireLegacySettlementTestState(t, ctx, fixture.value, fixture.id, false, true, 989, 0)
			requireLegacyProviderDurability(t, ctx, fixture.value, fixture.id, 11)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var unchanged bool
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id IS NULL FROM transfer_contract WHERE contract_id=$1`, missingId).Scan(&unchanged))
			if !unchanged {
				t.Fatal("registration invented financial payer metadata")
			}
		})
	})
}
