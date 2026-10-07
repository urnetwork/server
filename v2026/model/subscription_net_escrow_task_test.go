package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

func legacyMirrorTestEnv(t *testing.T, run func(testing.TB, context.Context)) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		run(t, ctx)
	})
}

func legacyMirrorTestOwner(t testing.TB, ctx context.Context, id server.Id) *task.Task {
	t.Helper()
	var taskId server.Id
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task
            WHERE function_name=$1 AND (args_json::jsonb->>'balance_id')::uuid=$2`,
			task.NewTaskTarget(ApplyLegacyNetEscrowMirror).TargetFunctionName(), id).Scan(&taskId))
	})
	owner := task.GetTasks(ctx, taskId)[taskId]
	if owner == nil {
		t.Fatal("durable mirror owner absent")
	}
	return owner
}

func legacyMirrorTestRun(t testing.TB, ctx context.Context, id server.Id) (*LegacyNetEscrowMirrorResult, func(server.PgTx) ([]server.PostFunction, error)) {
	t.Helper()
	result, post, err := task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost).RunSpecific(ctx, legacyMirrorTestOwner(t, ctx, id))
	if err != nil || result == nil || post == nil {
		t.Fatalf("mirror execution failed: %v", err)
	}
	return result, post
}

// The test owns only the ordinary finalizer's delete/post transaction seam.
// Full copy/delete/post/final-record behavior is covered by the dense page's
// real TaskWorker execution. These barriers force both sides of that delete.
func legacyMirrorTestDelete(ctx context.Context, tx server.PgTx, id server.Id, post func(server.PgTx) ([]server.PostFunction, error)) {
	server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE task_id=$1`, id))
	posts, err := post(tx)
	server.Raise(err)
	if len(posts) != 0 {
		panic("mirror finalization unexpectedly returned external work")
	}
}

func TestLegacyNetEscrowMirrorProducerBeforeDeleteRetriesAndRequeues(t *testing.T) {
	legacyMirrorTestEnv(t, func(t testing.TB, ctx context.Context) {
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
		server.RunPosts(ctx, posts...)
		server.Tx(ctx, func(tx server.PgTx) { queueLegacyNetEscrowMirrorsInTx(ctx, tx, []server.Id{f.balanceId}) })
		owner := legacyMirrorTestOwner(t, ctx, f.balanceId)
		old, post := legacyMirrorTestRun(t, ctx, f.balanceId)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		producer, err := conn.Begin(ctx)
		server.Raise(err)
		defer producer.Rollback(context.Background())
		server.RaisePgResult(producer.Exec(ctx, `SELECT 1 FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balanceId))
		server.RaisePgResult(producer.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=23 WHERE contract_id=$1 AND balance_id=$2`, contract.ContractId, f.balanceId))
		queueLegacyNetEscrowMirrorsInTx(ctx, producer, []server.Id{f.balanceId})
		blocker := contractLifecycleTestBackendPid(t, ctx, producer)
		var attempts atomic.Int64
		done := make(chan error, 1)
		go func() {
			var finishErr error
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					attempts.Add(1)
					// Establish the same repeatable-read snapshot as the ordinary
					// finished-row copy before DELETE waits for this producer.
					var raw string
					server.Raise(tx.QueryRow(ctx, `SELECT args_json FROM pending_task WHERE task_id=$1`, owner.TaskId).Scan(&raw))
					legacyMirrorTestDelete(ctx, tx, owner.TaskId, post)
				})
			}, func(err error) { finishErr = err })
			done <- finishErr
		}()
		requireContractLifecycleBlockedBy(t, ctx, producer, blocker)
		server.Raise(producer.Commit(ctx))
		select {
		case err = <-done:
			server.Raise(err)
		case <-ctx.Done():
			t.Fatal("producer-before-delete finalization did not join")
		}
		if attempts.Load() < 2 {
			t.Fatal("fixture did not force the stale finalizer snapshot to retry")
		}
		next := legacyMirrorTestOwner(t, ctx, f.balanceId)
		if next.TaskId == owner.TaskId {
			t.Fatal("changed committed revision lost its replacement owner")
		}
		fresh, _ := legacyMirrorTestRun(t, ctx, f.balanceId)
		if fresh.Revision <= old.Revision || Testing_NetEscrowByteCount(ctx, f.balanceId) != 23 {
			t.Fatal("replacement owner did not repair the newer reservation")
		}
	})
}

func TestLegacyNetEscrowMirrorDeleteBeforeProducerLeavesFreshOwner(t *testing.T) {
	legacyMirrorTestEnv(t, func(t testing.TB, ctx context.Context) {
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
		server.RunPosts(ctx, posts...)
		server.Tx(ctx, func(tx server.PgTx) { queueLegacyNetEscrowMirrorsInTx(ctx, tx, []server.Id{f.balanceId}) })
		owner := legacyMirrorTestOwner(t, ctx, f.balanceId)
		_, post := legacyMirrorTestRun(t, ctx, f.balanceId)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		finisher, err := conn.Begin(ctx)
		server.Raise(err)
		defer finisher.Rollback(context.Background())
		legacyMirrorTestDelete(ctx, finisher, owner.TaskId, post)
		blocker := contractLifecycleTestBackendPid(t, ctx, finisher)
		done := make(chan error, 1)
		grantAvailable := make(chan struct{})
		go func() {
			var producerErr error
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					// The finishing transaction must own no financial lock.
					server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM transfer_balance WHERE balance_id=$1 FOR UPDATE NOWAIT`, f.balanceId))
					close(grantAvailable)
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=29 WHERE contract_id=$1 AND balance_id=$2`, contract.ContractId, f.balanceId))
					queueLegacyNetEscrowMirrorsInTx(ctx, tx, []server.Id{f.balanceId})
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { producerErr = err })
			done <- producerErr
		}()
		select {
		case <-grantAvailable:
		case err = <-done:
			t.Fatalf("finalization retained financial grant ownership: %v", err)
		case <-ctx.Done():
			t.Fatal("producer grant probe did not complete")
		}
		requireContractLifecycleBlockedBy(t, ctx, finisher, blocker)
		server.Raise(finisher.Commit(ctx))
		select {
		case err = <-done:
			server.Raise(err)
		case <-ctx.Done():
			t.Fatal("delete-before-producer did not join")
		}
		if legacyMirrorTestOwner(t, ctx, f.balanceId).TaskId == owner.TaskId {
			t.Fatal("producer following a committed delete lost its new owner")
		}
		legacyMirrorTestRun(t, ctx, f.balanceId)
		if Testing_NetEscrowByteCount(ctx, f.balanceId) != 29 {
			t.Fatal("fresh owner lost the producer's reservation")
		}
	})
}

func TestLegacyNetEscrowMirrorFinancialRollbackWarmInvalidationAndOldReader(t *testing.T) {
	legacyMirrorTestEnv(t, func(t testing.TB, ctx context.Context) {
		f, id := legacySettlementTestIntent(t, ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		defer tx.Rollback(context.Background())
		_, complete, busy, _, err := flushLegacySettlementInTx(ctx, tx, id)
		server.Raise(err)
		if !complete || busy {
			t.Fatal("rollback did not reach the actual financial transition")
		}
		server.Raise(tx.Rollback(ctx))
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name=$1`, legacyColdPageMirrorFunction).Scan(&count))
			if count != 0 {
				t.Fatal("rolled-back money retained a mirror owner")
			}
		})
		var posts []func() any
		server.Tx(ctx, func(tx server.PgTx) {
			posts, complete, busy, _, err = flushLegacySettlementInTx(ctx, tx, id)
			server.Raise(err)
		}, server.TxReadCommitted)
		if !complete || busy {
			t.Fatal("committed financial owner did not finish")
		}
		owner := legacyMirrorTestOwner(t, ctx, f.balanceId)
		if len(settlementCacheSnapshot(ctx, []server.Id{f.balanceId})) != 1 {
			t.Fatal("fixture did not commit a warm settlement cache")
		}
		// Overtaking invalidation after warm commit must still have the same
		// durable obligation, even when the foreground callbacks are lost.
		neighbor, _ := createNetEscrowOrderingTestContract(ctx, f, 23)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=29 WHERE contract_id=$1 AND balance_id=$2`, neighbor.ContractId, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE task_id=$1`, owner.TaskId, time.Time{}))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2 WHERE task_id<>$1`, owner.TaskId, server.NowUtc().Add(time.Hour)))
		})
		if len(settlementCacheSnapshot(ctx, []server.Id{f.balanceId})) != 0 {
			t.Fatal("overtaking legacy write failed to invalidate warm authority")
		}
		old := task.NewTaskWorkerWithDefaults(ctx)
		defer old.Close()
		old.EvalTasks(1)
		after := legacyMirrorTestOwner(t, ctx, f.balanceId)
		if after.ArgsJson != owner.ArgsJson || after.RescheduleErrorCount != 1 {
			t.Fatal("older reader did not preserve exact immutable mirror scope")
		}
		legacyMirrorTestRun(t, ctx, f.balanceId)
		server.RunPosts(ctx, posts...)
		if Testing_NetEscrowByteCount(ctx, f.balanceId) != 29 {
			t.Fatal("later repair or delayed post lost the surviving reservation")
		}
		requireLegacyProviderDurability(t, ctx, f, id, 11)
	})
}

type legacyMirrorLostReplyHook struct{ hit atomic.Bool }

func (h *legacyMirrorLostReplyHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *legacyMirrorLostReplyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}
func (h *legacyMirrorLostReplyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		err := next(ctx, commands)
		if err == nil {
			for _, command := range commands {
				args := command.Args()
				if command.Name() == "eval" && len(args) > 1 && args[1] == netEscrowSnapshotScript && !h.hit.Swap(true) {
					return io.ErrUnexpectedEOF
				}
			}
		}
		return err
	}
}

func TestLegacyNetEscrowMirrorLostReplyDeletedBalanceAndScope(t *testing.T) {
	legacyMirrorTestEnv(t, func(t testing.TB, ctx context.Context) {
		f := newNetEscrowOrderingTestFixture(t, ctx)
		_, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
		server.RunPosts(ctx, posts...)
		server.Tx(ctx, func(tx server.PgTx) { queueLegacyNetEscrowMirrorsInTx(ctx, tx, []server.Id{f.balanceId}) })
		owner := legacyMirrorTestOwner(t, ctx, f.balanceId)
		hook := &legacyMirrorLostReplyHook{}
		server.Redis(ctx, func(r server.RedisClient) { r.AddHook(hook) })
		target := task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost)
		_, post, err := target.RunSpecific(ctx, owner)
		if !hook.hit.Load() || !errors.Is(err, io.ErrUnexpectedEOF) || post != nil {
			t.Fatal("actual lost Redis reply was treated as completed mirror ownership")
		}
		legacyMirrorTestRun(t, ctx, f.balanceId)
		if Testing_NetEscrowByteCount(ctx, f.balanceId) != 17 {
			t.Fatal("reply replay changed exact reservation")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id=$1`, f.balanceId))
			queueLegacyNetEscrowMirrorsInTx(ctx, tx, []server.Id{f.balanceId})
		})
		_, finish := legacyMirrorTestRun(t, ctx, f.balanceId)
		server.Tx(ctx, func(tx server.PgTx) { legacyMirrorTestDelete(ctx, tx, owner.TaskId, finish) })
		if len(task.GetTasks(ctx, owner.TaskId)) != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 || len(settlementCacheSnapshot(ctx, []server.Id{f.balanceId})) != 0 {
			t.Fatal("deleted balance lost tombstone repair or recreated cache authority")
		}
		local := session.NewLocalClientSession(ctx, "", nil)
		defer local.Cancel()
		if _, err := ApplyLegacyNetEscrowMirror(json.RawMessage(`{}`), local); err == nil {
			t.Fatal("non-durable caller bypassed owner identity")
		}
		if _, err := decodeLegacyNetEscrowMirror([]byte(`{"_private_task_arguments":true,"version":2}`)); err == nil {
			t.Fatal("unsupported private scope version was accepted")
		}
	})
}

// A running cold reader owns neither a grant nor its pending row. A real
// financial close on that same balance completes while the census is blocked,
// touches the existing owner, and makes its older snapshot require a successor.
func TestLegacyNetEscrowMirrorColdOwnerAllowsSameGrantClose(t *testing.T) {
	legacyMirrorTestEnv(t, func(t testing.TB, ctx context.Context) {
		f, contractId := legacySettlementTestIntent(t, ctx)
		_, posts := createNetEscrowOrderingTestContract(ctx, f, 23)
		server.RunPosts(ctx, posts...)
		native := createRedisAdmissionTest(ctx, f, 31)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
			queueLegacyNetEscrowMirrorsInTx(ctx, tx, []server.Id{f.balanceId})
		})
		owner := legacyMirrorTestOwner(t, ctx, f.balanceId)
		restore := installLegacyColdPageCensusBarrier(t, ctx)
		defer restore()
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT pg_advisory_xact_lock(731031)`))
		blocker := contractLifecycleTestBackendPid(t, ctx, held)
		type completion struct {
			result *LegacyNetEscrowMirrorResult
			post   func(server.PgTx) ([]server.PostFunction, error)
			err    error
		}
		done := make(chan completion, 1)
		go func() {
			result, post, err := task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost).RunSpecific(ctx, owner)
			done <- completion{result: result, post: post, err: err}
		}()
		requireContractLifecycleBlockedBy(t, ctx, held, blocker)
		completed, busy, _, err := flushLegacySettlement(ctx, contractId)
		if err != nil || !completed || busy {
			t.Fatalf("running census retained financial or pending-row ownership: %v", err)
		}
		if legacyMirrorTestOwner(t, ctx, f.balanceId).TaskId != owner.TaskId {
			t.Fatal("same-grant close did not coalesce into its active immutable owner")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM transfer_balance WHERE balance_id=$1 FOR UPDATE NOWAIT`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM pending_task WHERE task_id=$1 FOR UPDATE NOWAIT`, owner.TaskId))
		})
		server.Raise(held.Rollback(ctx))
		var first completion
		select {
		case first = <-done:
		case <-ctx.Done():
			t.Fatal("released running mirror owner did not join")
		}
		if first.err != nil || first.result == nil || first.post == nil {
			t.Fatalf("released cold owner failed: %v", first.err)
		}
		server.Tx(ctx, func(tx server.PgTx) { legacyMirrorTestDelete(ctx, tx, owner.TaskId, first.post) })
		if legacyMirrorTestOwner(t, ctx, f.balanceId).TaskId == owner.TaskId {
			t.Fatal("cold reader dropped the overtaking financial revision")
		}
		fresh, _ := legacyMirrorTestRun(t, ctx, f.balanceId)
		if fresh.Revision <= first.result.Revision || Testing_NetEscrowByteCount(ctx, f.balanceId) != 54 {
			t.Fatal("successor did not preserve legacy23 plus native31 after real close")
		}
		requireLegacyProviderDurability(t, ctx, f, contractId, 11)
		requireRedisExpiryClock(t, ctx, "11")
		server.Redis(ctx, func(r server.RedisClient) {
			amount, err := r.HGet(ctx, redisContractReservationKeys(f.balanceId)[1], native.ContractId.String()).Int64()
			server.Raise(err)
			if amount != 31 {
				t.Fatal("legacy owner altered the native reservation")
			}
		})
	})
}
