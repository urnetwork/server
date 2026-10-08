// Collection and continuation deadlines are verified on durable task rows.
package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A first schedule owns the window; repeats neither debounce it nor overwrite
// a ready continuation's scope. Production and test policies stay isolated.
func TestLegacyPayerCollectionWindowKeepsEarliestInitialDeadline(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, scenario := range []struct {
			ctx    context.Context
			window time.Duration
		}{
			{ctx: t.Context(), window: 30 * time.Second},
			{ctx: Testing_WithLegacyPayerSettlementCollectionWindow(t.Context()), window: 5 * time.Second},
		} {
			ctx := scenario.ctx
			owner := session.NewLocalClientSession(ctx, "", nil)
			payer := server.NewId()
			key := task.RunOnce("flush_legacy_payer_settlements", payer).String()
			var first time.Time
			before := server.NowUtc()
			withLegacyPayerQueueTestTx(ctx, []server.Id{payer}, func(tx server.PgTx) {
				QueueLegacyPayerSettlementsInTx(owner, tx, payer)
				server.Raise(tx.QueryRow(ctx, `SELECT run_at FROM pending_task WHERE run_once_key=$1`, key).Scan(&first))
			})
			after := server.NowUtc()
			if first.Before(before.Add(scenario.window-time.Millisecond)) || first.After(after.Add(scenario.window+time.Millisecond)) {
				t.Fatal("initial owner did not retain its collection window", scenario.window, first.Sub(before))
			}
			withLegacyPayerQueueTestTx(ctx, []server.Id{payer}, func(tx server.PgTx) {
				for range 3 {
					QueueLegacyPayerSettlementsInTx(owner, tx, payer)
				}
				var due time.Time
				var count int
				server.Raise(tx.QueryRow(ctx, `SELECT min(run_at),count(*) FROM pending_task WHERE run_once_key=$1`, key).Scan(&due, &count))
				if count != 1 || !due.Equal(first) {
					t.Fatal("repeated discoveries restarted the earliest window", scenario.window, first, due, count)
				}
			})
			readyPayer := server.NewId()
			readyAt := server.NowUtc().Add(-time.Minute)
			cursor := &LegacySettlementCursor{NextAttemptTime: readyAt, ContractId: server.NewId(), PassEndTime: server.NowUtc()}
			withLegacyPayerQueueTestTx(ctx, []server.Id{readyPayer}, func(tx server.PgTx) {
				ScheduleLegacyPayerSettlementsInTx(owner, tx, readyPayer, cursor, readyAt)
				QueueLegacyPayerSettlementsInTx(owner, tx, readyPayer)
				var due time.Time
				var argsJson []byte
				server.Raise(tx.QueryRow(ctx, `SELECT run_at,args_json FROM pending_task WHERE run_once_key=$1`,
					task.RunOnce("flush_legacy_payer_settlements", readyPayer).String()).Scan(&due, &argsJson))
				var args LegacyPayerSettlementArgs
				server.Raise(json.Unmarshal(argsJson, &args))
				if !due.Equal(readyAt) || args.Cursor == nil || args.Cursor.ContractId != cursor.ContractId {
					t.Fatal("initial wake delayed or replaced a ready continuation", scenario.window, due, args)
				}
			})
			owner.Cancel()
		}
		if legacyPayerSettlementCollectionWindow(t.Context()) != 30*time.Second {
			t.Fatal("test collection policy escaped its context")
		}
	})
}

// A real completed EOF keeps the database pass cutoff. An arrival afterward
// receives a collection deadline instead of inventing an immediate Post wake.
func TestLegacyPayerEofNewArrivalGetsCollectionWindow(t *testing.T) {
	testLegacyPayerNewArrivalCollectionWindow(t, false)
}

// A budget/full-page yield can retain a forward cursor even when only a later
// arrival remains. Its new window must reset the old pass cutoff as well.
func TestLegacyPayerNewPassResetsCompletedForwardCursor(t *testing.T) {
	testLegacyPayerNewArrivalCollectionWindow(t, true)
}

func testLegacyPayerNewArrivalCollectionWindow(t *testing.T, stopAfterPage bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := Testing_WithLegacyPayerSettlementCollectionWindow(t.Context())
		f, first := legacySettlementTestIntent(t, ctx)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		args := &LegacyPayerSettlementArgs{Private: true, PayerNetworkId: f.sourceNetworkId}
		var result *LegacyPayerSettlementResult
		var err error
		if stopAfterPage {
			part, pageErr := runLegacyPayerSettlementPages(ctx, f.sourceNetworkId, nil, 1, false)
			result, err = &part, pageErr
		} else {
			result, err = ApplyLegacyPayerSettlements(args, owner)
		}
		if err != nil || result.Completed != 1 || result.More != stopAfterPage ||
			(result.Cursor != nil) != stopAfterPage || result.PassEndTime.IsZero() {
			t.Fatal("completed turn lost its real fixed pass boundary", result, err)
		}
		second := newLegacyPayerTestIntent(t, ctx, f, server.NewId(), 100, 11)
		var next time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT next_attempt_time FROM legacy_settlement_intent WHERE contract_id=$1`, second).Scan(&next))
		})
		if !next.After(result.PassEndTime) {
			t.Fatal("new-arrival fixture did not cross the completed pass boundary")
		}
		before := server.NowUtc()
		withLegacyPayerQueueTestTx(ctx, []server.Id{f.sourceNetworkId}, func(tx server.PgTx) {
			server.Raise(ApplyLegacyPayerSettlementsPost(args, result, owner, tx))
		})
		after := server.NowUtc()
		server.Db(ctx, func(conn server.PgConn) {
			var due time.Time
			var data []byte
			server.Raise(conn.QueryRow(ctx, `SELECT run_at,args_json FROM pending_task WHERE run_once_key=$1`,
				task.RunOnce("flush_legacy_payer_settlements", f.sourceNetworkId).String()).Scan(&due, &data))
			var nextArgs LegacyPayerSettlementArgs
			server.Raise(json.Unmarshal(data, &nextArgs))
			if due.Before(before.Add(5*time.Second-time.Millisecond)) || due.After(after.Add(5*time.Second+time.Millisecond)) || nextArgs.Cursor != nil {
				t.Fatal("new arrival received an immediate or stale-cursor continuation", due, nextArgs)
			}
		})
		requireLegacySettlementTestState(t, ctx, f, first, false, true, 989, 100)
		requireLegacySettlementTestState(t, ctx, f, second, true, false, 989, 100)
	})
}

// A ready prefix continuation requests immediate service, while a real
// accounting refusal keeps its later retry authority across collection policy.
func TestLegacyPayerPostPreservesReadyAndAccountingDeadlines(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := Testing_WithLegacyPayerSettlementCollectionWindow(t.Context())
		f, id := legacySettlementTestIntent(t, ctx)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		args := &LegacyPayerSettlementArgs{Private: true, PayerNetworkId: f.sourceNetworkId}
		before := server.NowUtc()
		withLegacyPayerQueueTestTx(ctx, []server.Id{f.sourceNetworkId}, func(tx server.PgTx) {
			server.Raise(ApplyLegacyPayerSettlementsPost(args, &LegacyPayerSettlementResult{
				LegacySettlementFlushResult: LegacySettlementFlushResult{More: true, Completed: 256, PassEndTime: before},
				Pages:                       1,
			}, owner, tx))
		})
		server.Db(ctx, func(conn server.PgConn) {
			var due time.Time
			server.Raise(conn.QueryRow(ctx, `SELECT run_at FROM pending_task WHERE run_once_key=$1`,
				task.RunOnce("flush_legacy_payer_settlements", f.sourceNetworkId).String()).Scan(&due))
			if due.Before(before.Add(-time.Millisecond)) || due.After(server.NowUtc()) {
				t.Fatal("already-ready prefix received another collection delay", due.Sub(before))
			}
		})
		withLegacyPayerQueueTestTx(ctx, []server.Id{f.sourceNetworkId}, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key=$1`, task.RunOnce("flush_legacy_payer_settlements", f.sourceNetworkId).String()))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=101 WHERE contract_id=$1`, id))
		})
		result, err := ApplyLegacyPayerSettlements(args, owner)
		if err != nil || result.Failed != 1 || result.Completed != 0 || result.Pages != 1 {
			t.Fatal("actual underfunded payer did not stop its admitted turn", result, err)
		}
		withLegacyPayerQueueTestTx(ctx, []server.Id{f.sourceNetworkId}, func(tx server.PgTx) {
			var retryAt time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT next_attempt_time FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&retryAt))
			server.Raise(ApplyLegacyPayerSettlementsPost(args, result, owner, tx))
			var due time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT run_at FROM pending_task WHERE run_once_key=$1`,
				task.RunOnce("flush_legacy_payer_settlements", f.sourceNetworkId).String()).Scan(&due))
			if !due.Equal(retryAt) || due.Before(server.NowUtc().Add(14*time.Minute)) {
				t.Fatal("collection or continuation shortened the accounting cooldown", retryAt, due)
			}
		})
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
	})
}
