package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

type legacyGrantWaitPageResult struct {
	page LegacySettlementFlushResult
	err  error
}

// Observe an actual lock wait while retaining the owner. The old SKIP LOCKED
// implementation returns before this handoff and deterministically fails.
func TestLegacySettlementHeadGrantWaitQueuesBeforeRelease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		head, headId, tail, tailIds := legacyHeadRevisitFixture(t, ctx, 2)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, head.balanceId))
		blocker := contractLifecycleTestBackendPid(t, ctx, held)
		shard := int(headId[15]) % LegacySettlementShardCount
		first, err := FlushLegacySettlements(ctx, shard, nil, 1)
		if err != nil || first.Cursor == nil || first.BusyGrantSetMismatch != 1 || first.Visited != 1 {
			t.Fatal("initial forward selection did not retain the busy old owner")
		}
		done := make(chan legacyGrantWaitPageResult, 1)
		joined := false
		defer func() {
			cancel()
			_ = held.Rollback(context.Background())
			if !joined {
				<-done
			}
		}()
		go func() {
			page, err := FlushLegacySettlements(ctx, shard, first.Cursor, 2)
			done <- legacyGrantWaitPageResult{page, err}
		}()
		for {
			select {
			case result := <-done:
				joined = true
				t.Fatalf("old head returned before bounded grant handoff: visited=%d completed=%d busy=%d failed=%d error=%t", result.page.Visited, result.page.Completed, result.page.BusyOrGone, result.page.Failed, result.err != nil)
			default:
			}
			var blocked int
			server.Raise(held.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted AND $1::int=ANY(pg_blocking_pids(pid))`, blocker).Scan(&blocked))
			if blocked > 0 {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("bounded grant wait was not observed")
			case <-time.After(time.Millisecond):
			}
		}
		server.Raise(held.Rollback(ctx))
		result := <-done
		joined = true
		if result.err != nil || result.page.Visited != 2 || result.page.Completed != 2 || result.page.HeadCompleted != 1 || result.page.BusyOrGone != 0 || result.page.Failed != 0 {
			t.Fatal("released queued head failed to commit exactly once")
		}
		if result.page.HeadGrantWaitAttempted != 1 || result.page.HeadGrantWaitCompleted != 1 || result.page.HeadGrantWaitTimedOut != 0 {
			t.Fatal("successful wait counters lost their head subset")
		}
		if result.page.Cursor == nil || result.page.Cursor.ContractId != tailIds[0] || !result.page.Cursor.PassEndTime.Equal(first.Cursor.PassEndTime) {
			t.Fatal("head handoff changed the forward cursor or pass cutoff")
		}
		requireLegacySettlementTestState(t, ctx, head, headId, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, head, headId, 11)
		requireLegacySettlementTestState(t, ctx, tail, tailIds[0], false, true, 999989, 100)
		if complete, _, _, err := flushLegacySettlement(ctx, headId); err != nil || complete {
			t.Fatal("queued head replay repeated a financial transition")
		}
		requireLegacyProviderDurability(t, ctx, head, headId, 11)
	})
}

func TestLegacySettlementHeadGrantWaitTimeoutPreservesDueAndForward(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		head, headId, _, tailIds := legacyHeadRevisitFixture(t, ctx, 8)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, head.balanceId))
		shard := int(headId[15]) % LegacySettlementShardCount
		first, err := FlushLegacySettlements(ctx, shard, nil, 1)
		server.Raise(err)
		started := time.Now()
		page, err := FlushLegacySettlements(ctx, shard, first.Cursor, 8)
		elapsed := time.Since(started)
		if err != nil || page.Visited != 8 || page.Completed != 7 || page.BusyOrGone != 1 || page.Failed != 0 || page.HeadVisited != 1 || page.HeadBusyGrantSetMismatch != 1 {
			t.Fatal("wait timeout became a failure or stopped forward work")
		}
		if page.HeadGrantWaitAttempted != 1 || page.HeadGrantWaitCompleted != 0 || page.HeadGrantWaitTimedOut != 1 || elapsed < 200*time.Millisecond || elapsed > 2*time.Second {
			t.Fatal("bounded wait was not retained as one timed-out attempt")
		}
		if page.Cursor == nil || page.Cursor.ContractId != tailIds[6] || !page.Cursor.PassEndTime.Equal(first.Cursor.PassEndTime) {
			t.Fatal("expected contention changed the forward cursor")
		}
		server.Db(ctx, func(c server.PgConn) {
			var due time.Time
			var code string
			server.Raise(c.QueryRow(ctx, `SELECT next_attempt_time,failure_code FROM legacy_settlement_intent WHERE contract_id=$1`, headId).Scan(&due, &code))
			if !due.Equal(first.Cursor.NextAttemptTime) || code != "none" {
				t.Fatal("expected wait introduced an operational backoff")
			}
		})
		requireLegacySettlementTestState(t, ctx, head, headId, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, head, headId, 0)
		server.Raise(held.Rollback(ctx))
		last, err := FlushLegacySettlements(ctx, shard, page.Cursor, 2)
		if err != nil || last.Completed != 2 || last.HeadGrantWaitCompleted != 1 {
			t.Fatal("timed-out owner could not recover after release")
		}
		requireLegacySettlementTestState(t, ctx, head, headId, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, head, headId, 11)
	})
}

// Cancellation keeps the prior forward prefix and does not turn a canceled
// acquisition into a counted visit, expected timeout, or economic transaction.
func TestLegacySettlementHeadGrantWaitParentCancellation(t *testing.T) {
	legacyHeadGrantWaitCancellationControl(t, false)
}

// Operator cancellation shares 57014 with statement timeout. It must remain an
// operational error rather than being silently treated as expected contention.
func TestLegacySettlementHeadGrantWaitOperatorCancellation(t *testing.T) {
	legacyHeadGrantWaitCancellationControl(t, true)
}

func legacyHeadGrantWaitCancellationControl(t *testing.T, operator bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		head, headId, _, tailIds := legacyHeadRevisitFixture(t, ctx, 2)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, head.balanceId))
		blocker := contractLifecycleTestBackendPid(t, ctx, held)
		shard := int(headId[15]) % LegacySettlementShardCount
		first, err := FlushLegacySettlements(ctx, shard, nil, 1)
		server.Raise(err)
		parent, cancelParent := context.WithCancel(ctx)
		defer cancelParent()
		done := make(chan legacyGrantWaitPageResult, 1)
		joined := false
		defer func() {
			cancelParent()
			_ = held.Rollback(context.Background())
			if !joined {
				<-done
			}
		}()
		go func() {
			page, err := FlushLegacySettlements(parent, shard, first.Cursor, 2)
			done <- legacyGrantWaitPageResult{page, err}
		}()
		pid := requireContractLifecycleBlockedBy(t, ctx, held, blocker)
		if operator {
			var canceled bool
			server.Raise(held.QueryRow(ctx, `SELECT pg_cancel_backend($1)`, pid).Scan(&canceled))
			if !canceled {
				t.Fatal("operator cancellation was not delivered")
			}
		} else {
			cancelParent()
		}
		result := <-done
		joined = true
		if result.page.Cursor == nil || result.page.Cursor.ContractId != tailIds[0] || result.page.Completed != 1 || result.page.HeadGrantWaitTimedOut != 0 || result.page.BusyOrGone != 0 {
			t.Fatal("cancellation lost forward progress or became expected contention")
		}
		if operator {
			if result.err != nil || result.page.HeadFailed != 1 || result.page.Failed != 1 || result.page.HeadGrantWaitAttempted != 1 {
				t.Fatal("operator cancel did not preserve ordinary operational failure")
			}
		} else if result.err == nil || result.page.Visited != 1 || result.page.HeadVisited != 0 || result.page.Failed != 0 {
			t.Fatal("parent cancellation counted or deferred the uncompleted head")
		}
		requireLegacySettlementTestState(t, ctx, head, headId, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, head, headId, 0)
	})
}

// After acquiring one grant, timeout on a second rolls back all owners and all
// state. A new transaction can immediately own the first grant after return.
func TestLegacySettlementHeadGrantWaitPartialOwnershipRollback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		f, id := legacySettlementTestIntent(t, ctx)
		second := server.NewId()
		for bytes.Compare(second[:], f.balanceId[:]) <= 0 {
			second = server.NewId()
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents)
                VALUES($1,$2,now(),now()+interval '30 days',1000,1000,0)`, second, f.sourceNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,100)`, id, second))
		})
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, second))
		wait := &legacySettlementGrantWait{}
		complete, busy, gate, err := flushLegacySettlementWithGrantWait(ctx, id, wait)
		if err != nil || complete || !busy || gate != legacySettlementBusyGrantSet || !wait.attempted || !wait.timedOut {
			t.Fatal("partial grant wait did not return bounded busy")
		}
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE NOWAIT`, f.balanceId))
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, f, id, 0)
		server.Raise(held.Rollback(ctx))
		complete, busy, _, err = flushLegacySettlementWithGrantWait(ctx, id, &legacySettlementGrantWait{})
		if err != nil || !complete || busy {
			t.Fatal("partial wait could not replay after release")
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, f, id, 11)
		requireLegacyOwnedMetadataRedis(t, ctx, second, 0)
	})
}

func TestLegacySettlementHeadGrantWaitClassifiesOnlyBudgetErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}, true},
		{&pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}, true},
		{&pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"}, false},
		{&pgconn.PgError{Code: "40P01", Message: "deadlock detected"}, false},
		{context.Canceled, false}, {errors.New("unknown query failure"), false},
	} {
		if legacySettlementGrantWaitExpired(tc.err) != tc.want {
			t.Fatal("incorrect bounded-wait error classification")
		}
	}
}

// The same 55P03 from a later financial statement remains an operational
// failure. Grant-wait recovery must never swallow a rolled-back debit/outcome.
func TestLegacySettlementHeadGrantWaitLaterLockTimeoutIsOperational(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		head, headId, _, _ := legacyHeadRevisitFixture(t, ctx, 2)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, head.balanceId))
		shard := int(headId[15]) % LegacySettlementShardCount
		first, err := FlushLegacySettlements(ctx, shard, nil, 1)
		server.Raise(err)
		server.Raise(held.Rollback(ctx))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION synthetic_head_grant_later_wait() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN
                    IF NEW.contract_id=TG_ARGV[0]::uuid THEN PERFORM pg_advisory_xact_lock(731049); END IF;
                    RETURN NEW;
                END $$;
                CREATE TRIGGER synthetic_head_grant_later_wait BEFORE UPDATE OF outcome ON transfer_contract
                FOR EACH ROW WHEN (OLD.outcome IS NULL AND NEW.outcome IS NOT NULL)
                EXECUTE FUNCTION synthetic_head_grant_later_wait('%s');`, headId)))
		})
		held, err = conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT pg_advisory_xact_lock(731049)`))
		page, err := FlushLegacySettlements(ctx, shard, first.Cursor, 2)
		if err != nil || page.Completed != 1 || page.Failed != 1 || page.HeadFailed != 1 || page.BusyOrGone != 0 || page.HeadGrantWaitAttempted != 1 || page.HeadGrantWaitCompleted != 0 || page.HeadGrantWaitTimedOut != 0 {
			t.Fatal("later financial lock timeout was mistaken for grant acquisition busy")
		}
		requireLegacySettlementTestState(t, ctx, head, headId, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, head, headId, 0)
		server.Db(ctx, func(c server.PgConn) {
			var code string
			var future bool
			server.Raise(c.QueryRow(ctx, `SELECT failure_code,next_attempt_time>now() FROM legacy_settlement_intent WHERE contract_id=$1`, headId).Scan(&code, &future))
			if code != "operational" || !future {
				t.Fatal("later financial failure lost its retry state")
			}
		})
	})
}
