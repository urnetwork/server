// Real transaction-owned projection barriers qualify admission cleanup order.
package model

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

type legacyAdmissionPostReleaseKey struct{}

// Each observation comes from an actual release command, rather than key
// absence that could be explained by the two-second lease expiring.
type legacyAdmissionPostReleaseObservation struct {
	postPending bool
	err         error
}

// The hook belongs to one marked financial call. It holds the actual optional
// hole command and records whether cleanup runs before that command returns.
// The operation's existing context deadline lets the unfixed owner unwind;
// elapsed time and lease expiry are not the ordering oracle.
type legacyAdmissionPostReleaseGate struct {
	contractId   string
	ownerKey     string
	postEntered  chan struct{}
	allowPost    chan struct{}
	postReturned atomic.Bool
	acquires     atomic.Int64
	releases     atomic.Int64
	observed     chan legacyAdmissionPostReleaseObservation
}

// These hooks affect only the marked target commands on the existing client.
func (self *legacyAdmissionPostReleaseGate) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

// Projection and admission owners use individual commands, not pipelines.
func (self *legacyAdmissionPostReleaseGate) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// Observe the production release script and hold only the exact hole event.
func (self *legacyAdmissionPostReleaseGate) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		gate, _ := ctx.Value(legacyAdmissionPostReleaseKey{}).(*legacyAdmissionPostReleaseGate)
		args := command.Args()
		if gate != self || command.Name() != "eval" || len(args) < 5 {
			return next(ctx, command)
		}
		if args[1] == contractHoleEventScript && len(args) >= 10 && args[9] == self.contractId {
			close(self.postEntered)
			defer self.postReturned.Store(true)
			select {
			case <-self.allowPost:
				return next(ctx, command)
			case <-ctx.Done():
				command.SetErr(ctx.Err())
				return ctx.Err()
			}
		}
		if args[3] != self.ownerKey {
			return next(ctx, command)
		}
		if args[1] == legacySettlementAdmissionAcquireLua {
			err := next(ctx, command)
			if err == nil {
				self.acquires.Add(1)
			}
			return err
		}
		if args[1] == legacySettlementAdmissionReleaseLua {
			err := next(ctx, command)
			self.releases.Add(1)
			select {
			case self.observed <- legacyAdmissionPostReleaseObservation{postPending: !self.postReturned.Load(), err: err}:
			default:
			}
			return err
		}
		return next(ctx, command)
	}
}

// A held real contract-hole post must not retain a lease after PG has committed
// and released its grant. Financial, proof, durable-mirror and replay checks run
// before the causal ordering assertion, including on the unfixed source.
func TestLegacySettlementAdmissionCleanupDoesNotJoinContractHolePost(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		fixture, id := legacySettlementTestIntent(t, ctx)
		refreshNetEscrow(ctx, []server.Id{fixture.balanceId})
		beforeReports, beforeProof := legacyAdmissionPostReleaseEvidence(ctx, id)
		if len(beforeProof) != 0 {
			t.Fatal("ordinary queued fixture unexpectedly had terminal usage")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var mirrorPending bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pending_task
				WHERE function_name=$1 AND (args_json::jsonb->>'balance_id')::uuid=$2)`,
				task.NewTaskTarget(ApplyLegacyNetEscrowMirror).TargetFunctionName(), fixture.balanceId).Scan(&mirrorPending))
			if mirrorPending {
				t.Fatal("fixture already had the financial transaction's mirror owner")
			}
		})
		gate := &legacyAdmissionPostReleaseGate{
			contractId: id.String(), ownerKey: legacySettlementAdmissionKey(fixture.balanceId),
			postEntered: make(chan struct{}), allowPost: make(chan struct{}),
			observed: make(chan legacyAdmissionPostReleaseObservation, 2),
		}
		allowPost := sync.OnceFunc(func() { close(gate.allowPost) })
		server.Raise(server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
			client.AddHook(gate)
			return nil
		}))
		marked := context.WithValue(ctx, legacyAdmissionPostReleaseKey{}, gate)
		type completion struct {
			completed bool
			busy      bool
			err       error
		}
		done := make(chan completion, 1)
		retired := make(chan struct{})
		go func() {
			defer close(retired)
			completed, busy, _, err := flushLegacySettlement(marked, id)
			done <- completion{completed: completed, busy: busy, err: err}
		}()
		defer func() {
			allowPost()
			cancel()
			select {
			case <-retired:
			case <-time.After(10 * time.Second):
				t.Error("financial owner did not retire after its projection was released")
			}
		}()
		select {
		case <-gate.postEntered:
		case <-retired:
			select {
			case <-gate.postEntered:
			default:
				t.Fatal("financial commit did not reach its real transaction-owned hole post")
			}
		case <-ctx.Done():
			t.Fatal("transaction-owned hole post barrier was not reached")
		}
		var committedProof []byte
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE NOWAIT`, fixture.balanceId))
			var settled, intentAbsent bool
			var credit, unsettled int64
			server.Raise(tx.QueryRow(ctx, `SELECT outcome='settled',provider_usage,
				NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1),
				(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
				(SELECT count(*) FROM transfer_escrow WHERE contract_id=$1 AND NOT settled)
				FROM transfer_contract WHERE contract_id=$1`, id, fixture.balanceId).Scan(
				&settled, &committedProof, &intentAbsent, &credit, &unsettled))
			if !settled || !intentAbsent || credit != 989 || unsettled != 0 || len(committedProof) == 0 {
				t.Fatal("post barrier preceded the exact durable financial commit")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		legacyMirrorTestOwner(t, ctx, fixture.balanceId)
		requireLegacyProviderDurability(t, ctx, fixture, id, 11)
		var observed legacyAdmissionPostReleaseObservation
		select {
		case observed = <-gate.observed:
		case <-ctx.Done():
			t.Fatal("admission cleanup never reached its real Redis command")
		}
		allowPost()
		var result completion
		select {
		case result = <-done:
		case <-ctx.Done():
			t.Fatal("financial owner did not complete after its post was released")
		}
		if result.err != nil || !result.completed || result.busy {
			t.Fatal("post ordering changed ordinary financial completion", result.err)
		}
		requireLegacySettlementTestState(t, ctx, fixture, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, fixture, id, 11)
		legacyMirrorTestOwner(t, ctx, fixture.balanceId)
		legacyAdmissionRequireToken(t, ctx, fixture.balanceId, "")
		reports, proof := legacyAdmissionPostReleaseEvidence(ctx, id)
		usage, err := decodeContractUsageSnapshot(proof)
		if !bytes.Equal(beforeReports, reports) || !bytes.Equal(committedProof, proof) || err != nil || usage == nil ||
			usage.ByteCount != 11 || len(usage.Providers) != 1 || usage.Providers[0].NetworkId != fixture.destinationNetworkId ||
			usage.Providers[0].ClientId != fixture.destinationId || usage.Providers[0].ByteCount != 11 {
			t.Fatal("post cleanup changed report custody or immutable provider usage", err)
		}
		again, err := FlushLegacySettlements(ctx, int(id[15])%LegacySettlementShardCount, nil, 1)
		if err != nil || again.Visited != 0 || again.Completed != 0 {
			t.Fatal("post cleanup replay repeated finance", err)
		}
		requireLegacySettlementTestState(t, ctx, fixture, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, fixture, id, 11)
		replayReports, replayProof := legacyAdmissionPostReleaseEvidence(ctx, id)
		if !bytes.Equal(reports, replayReports) || !bytes.Equal(proof, replayProof) {
			t.Fatal("post cleanup replay changed reports or terminal usage")
		}
		if observed.err != nil || gate.acquires.Load() != 1 || gate.releases.Load() != 1 {
			t.Fatal("admission fixture lost its exact acquire/cleanup commands", observed.err, gate.acquires.Load(), gate.releases.Load())
		}
		if !observed.postPending {
			t.Fatal("admission cleanup waited for the transaction-owned contract-hole post")
		}
	})
}

// A financial rollback discards every registered commit post. The existing
// outer cleanup must still release exactly once, before a healthy retry.
func TestLegacySettlementAdmissionRollbackKeepsOuterCleanup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		fixture, id := legacySettlementTestIntent(t, ctx)
		refreshNetEscrow(ctx, []server.Id{fixture.balanceId})
		beforeReports, beforeProof := legacyAdmissionPostReleaseEvidence(ctx, id)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE FUNCTION fixture_refuse_legacy_escrow_settlement() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'synthetic financial rollback'; END $$;
				CREATE TRIGGER fixture_refuse_legacy_escrow_settlement BEFORE UPDATE OF settled ON transfer_escrow
				FOR EACH ROW WHEN (NEW.settled) EXECUTE FUNCTION fixture_refuse_legacy_escrow_settlement()`))
		})
		hook := &legacyAdmissionFaultHook{}
		server.Raise(server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
			client.AddHook(hook)
			return nil
		}))
		marked := context.WithValue(ctx, legacyAdmissionFaultKey{}, true)
		completed, busy, _, err := flushLegacySettlement(marked, id)
		if err == nil || completed || busy {
			t.Fatal("synthetic rollback did not refuse the ordinary financial transaction", err)
		}
		requireLegacySettlementTestState(t, ctx, fixture, id, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, fixture, id, 0)
		legacyAdmissionRequireToken(t, ctx, fixture.balanceId, "")
		reports, proof := legacyAdmissionPostReleaseEvidence(ctx, id)
		if !bytes.Equal(beforeReports, reports) || !bytes.Equal(beforeProof, proof) || hook.acquired.Load() != 1 || hook.released.Load() != 1 {
			t.Fatal("rollback lost its outer cleanup or altered durable evidence")
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `DROP TRIGGER fixture_refuse_legacy_escrow_settlement ON transfer_escrow;
				DROP FUNCTION fixture_refuse_legacy_escrow_settlement()`))
		})
		completed, busy, _, err = flushLegacySettlement(marked, id)
		if err != nil || !completed || busy {
			t.Fatal("removing only the rollback fault did not preserve funded completion", err)
		}
		requireLegacySettlementTestState(t, ctx, fixture, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, fixture, id, 11)
		legacyAdmissionRequireToken(t, ctx, fixture.balanceId, "")
		if hook.acquired.Load() != 2 || hook.released.Load() != 2 {
			t.Fatal("confirmed commit duplicated the outer fallback cleanup")
		}
		again, err := FlushLegacySettlements(ctx, int(id[15])%LegacySettlementShardCount, nil, 1)
		if err != nil || again.Visited != 0 || again.Completed != 0 {
			t.Fatal("rollback recovery replay repeated finance", err)
		}
		requireLegacySettlementTestState(t, ctx, fixture, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, fixture, id, 11)
	})
}

// Read exact report bytes and immutable usage without mutating either source.
func legacyAdmissionPostReleaseEvidence(ctx context.Context, id server.Id) (reports, proof []byte) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT jsonb_agg(to_jsonb(report) ORDER BY party) FROM contract_close AS report WHERE contract_id=$1),
			provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&reports, &proof))
	})
	return
}
