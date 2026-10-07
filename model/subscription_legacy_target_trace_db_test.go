package model

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Actual PG ownership, unchanged financial state, released grant, and no-op
// replay prove the trace does not replace either the queue or financial owner.
func TestLegacyTargetTraceHeldGrantReleaseAndReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		fixture, id := legacySettlementTestIntent(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, fixture.balanceId))
		observed, trace := legacyTraceTestContext(ctx, id, "automatic_page", nil)
		blocked, err := FlushLegacySettlements(observed, int(id[15])%16, nil, 1)
		if err != nil || blocked.Visited != 1 || blocked.BusyGrantSetMismatch != 1 || blocked.Completed != 0 || blocked.Trace == nil || blocked.Trace.Selected != 1 {
			t.Fatal("trace changed held-grant result")
		}
		if !legacyTraceHas(trace.snapshot(), "grant_lock", "returned") || !legacyTraceHas(trace.snapshot(), "attempt_result", "grant_set_mismatch") {
			t.Fatal("actual ownership refusal not classified")
		}
		requireLegacySettlementTestState(t, ctx, fixture, id, true, false, 1000, 100)
		server.Raise(held.Rollback(ctx))
		observed, _ = legacyTraceTestContext(ctx, id, "automatic_page", nil)
		settled, err := FlushLegacySettlements(observed, int(id[15])%16, nil, 1)
		if err != nil || settled.Completed != 1 || !legacyTraceHas(settled.Trace, "commit", "confirmed_tx_return") || !legacyTraceHas(settled.Trace, "attempt_result", "completed") || settled.Trace.InputCursor == settled.Trace.NextCursor {
			t.Fatal("commit/cursor authority not retained")
		}
		requireLegacySettlementTestState(t, ctx, fixture, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, fixture, id, 11)
		requireRedisExpiryClock(t, ctx, "11")
		observed, _ = legacyTraceTestContext(ctx, id, "automatic_page", nil)
		replay, err := FlushLegacySettlements(observed, int(id[15])%16, nil, 1)
		if err != nil || replay.Visited != 0 || replay.Trace.Selected != 0 || legacyTraceHas(replay.Trace, "commit", "confirmed_tx_return") {
			t.Fatal("empty replay became a target attempt")
		}
		requireLegacyProviderDurability(t, ctx, fixture, id, 11)
		requireRedisExpiryClock(t, ctx, "11")
	})
}

func TestLegacyTargetTracePointBodyRollback(t *testing.T) {
	testLegacyTargetTracePointRefusal(t, false)
}

func TestLegacyTargetTracePointCommitRefusal(t *testing.T) {
	testLegacyTargetTracePointRefusal(t, true)
}

func testLegacyTargetTracePointRefusal(t *testing.T, deferred bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		fixture, id := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION trace_refuse_sweep() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION USING ERRCODE='23514', MESSAGE='synthetic trace rollback'; END $$`))
			statement := `CREATE TRIGGER trace_refuse AFTER INSERT ON transfer_escrow_sweep FOR EACH ROW EXECUTE FUNCTION trace_refuse_sweep()`
			if deferred {
				statement = `CREATE CONSTRAINT TRIGGER trace_refuse AFTER INSERT ON transfer_escrow_sweep DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION trace_refuse_sweep()`
			}
			server.RaisePgResult(tx.Exec(ctx, statement))
		})
		release := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER trace_refuse ON transfer_escrow_sweep`))
			})
		}
		observed, _ := legacyTraceTestContext(ctx, id, "explicit_apply", nil)
		result, err := DrainLegacySettlements(observed, LegacySettlementDrainRequest{ExpectedPayerNetworkId: fixture.sourceNetworkId, ContractIds: []server.Id{id}, Apply: true})
		if err != nil || len(result.Contracts) != 1 || result.Contracts[0].Status != "failed" || result.Contracts[0].FinancialCommitAcknowledged || result.Trace == nil || result.Trace.Origin != "explicit_apply" {
			t.Fatal("point owner refusal changed")
		}
		if legacyTraceHas(result.Trace, "commit", "confirmed_tx_return") || !legacyTraceHas(result.Trace, "attempt_result", "constraint_refused") {
			t.Fatal("refused commit was reported as acknowledged")
		}
		if deferred != legacyTraceHas(result.Trace, "db_commit_call", "observed") {
			t.Fatal("body rollback and commit refusal were conflated")
		}
		requireLegacySettlementTestState(t, ctx, fixture, id, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, fixture, id, 0)
		release()
		observed, _ = legacyTraceTestContext(ctx, id, "explicit_apply", nil)
		result, err = DrainLegacySettlements(observed, LegacySettlementDrainRequest{ExpectedPayerNetworkId: fixture.sourceNetworkId, ContractIds: []server.Id{id}, Apply: true})
		if err != nil || !result.Contracts[0].FinancialCommitAcknowledged || !legacyTraceHas(result.Trace, "commit", "confirmed_tx_return") {
			t.Fatal("released point owner did not preserve commit")
		}
		requireLegacySettlementTestState(t, ctx, fixture, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, fixture, id, 11)
	})
}

func TestLegacyTargetTraceAccountingRefusalRetainsDebtAndRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture, id := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=400 WHERE contract_id=$1 AND party='destination'`, id))
		})
		observed, _ := legacyTraceTestContext(ctx, id, "automatic_page", nil)
		result, err := FlushLegacySettlements(observed, int(id[15])%16, nil, 1)
		if err != nil || result.Failed != 1 || result.Completed != 0 || !legacyTraceHas(result.Trace, "accounting", "insufficient_escrow") || !legacyTraceHas(result.Trace, "retry_state", "committed") || legacyTraceHas(result.Trace, "commit", "confirmed_tx_return") {
			t.Fatal("accounting refusal lost its distinct financial/retry stages")
		}
		requireLegacySettlementTestState(t, ctx, fixture, id, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, fixture, id, 0)
		server.Db(ctx, func(conn server.PgConn) {
			var code string
			var future bool
			server.Raise(conn.QueryRow(ctx, `SELECT failure_code,next_attempt_time>clock_timestamp()+interval '14 minutes' FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&code, &future))
			if code != "accounting" || !future {
				t.Fatal("diagnostic changed retry disposition")
			}
		})
	})
}

func TestLegacyTargetTraceCommittedBlockedPostAndPublisher(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		fixture, id := legacySettlementTestIntent(t, ctx)
		gate := &legacySettlementTimingGate{family: "stream", streamKey: contractStreamKey(id), entered: make(chan struct{}, 1), release: make(chan struct{})}
		unblock := sync.OnceFunc(func() { close(gate.release) })
		defer unblock()
		defer gate.enabled.Store(false)
		server.Redis(ctx, func(client server.RedisClient) { client.AddHook(gate) })
		gate.enabled.Store(true)
		observed, trace := legacyTraceTestContext(ctx, id, "automatic_page", nil)
		// Force every publication to drop while retaining the private result.
		trace.output = make(chan legacyTargetTraceEnvelope)
		type completion struct {
			result LegacySettlementFlushResult
			err    error
		}
		done := make(chan completion, 1)
		go func() {
			r, e := FlushLegacySettlements(observed, int(id[15])%16, nil, 1)
			done <- completion{result: r, err: e}
		}()
		defer func() {
			gate.enabled.Store(false)
			unblock()
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("trace fixture did not join")
			}
		}()
		select {
		case <-gate.entered:
		case <-ctx.Done():
			t.Fatal("real stream callback never arrived")
		}
		snapshot := trace.snapshot()
		if !legacyTraceHas(snapshot, "commit", "confirmed_tx_return") || !legacyTraceHas(snapshot, "stream_post", "batched") || legacyTraceHas(snapshot, "stream_post", "returned") || snapshot.PageReturned || snapshot.Dropped == 0 {
			t.Fatal("blocked post/publication obscured committed versus unfinished stages")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE NOWAIT`, fixture.balanceId))
			var committed bool
			server.Raise(tx.QueryRow(ctx, `SELECT outcome='settled' FROM transfer_contract WHERE contract_id=$1`, id).Scan(&committed))
			if !committed {
				t.Fatal("post barrier preceded actual financial commit")
			}
		})
		gate.enabled.Store(false)
		unblock()
		select {
		case got := <-done:
			if got.err != nil || got.result.Completed != 1 || !legacyTraceHas(got.result.Trace, "stream_post", "batched") || got.result.Timings.Stream.Count != 1 {
				t.Fatal("released callback lost its result")
			}
			done <- got
		case <-ctx.Done():
			t.Fatal("released post failed to join")
		}
		requireLegacySettlementTestState(t, ctx, fixture, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, fixture, id, 11)
		requireRedisExpiryClock(t, ctx, "11")
	})
}
