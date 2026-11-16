// Startup wakes a running expiry owner without replacing its captured scan state.
package taskworker

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// The real close target finishes its reads before the channel hands startup
// an active owner. No pending-row or financial lock is held by this barrier.
type startupExpiryCompletionBarrier struct {
	task.Target
	entered chan *task.Task
	release <-chan struct{}
}

func (self *startupExpiryCompletionBarrier) Run(ctx context.Context, queued *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	result, post, err := self.Target.Run(ctx, queued)
	if err != nil {
		return result, post, err
	}
	select {
	case self.entered <- queued:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	select {
	case <-self.release:
		return result, post, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// Production restart must retain an active cursor and promptly wake its successor.
func TestTaskworkerStartupWakesActiveExpiryWithoutReplacingCursor(t *testing.T) {
	testTaskworkerStartupWakesActiveExpiry(t, WorkloadProfileProduction)
}

// The scoped worker uses the same generic coordinator and generation handoff.
func TestTaskworkerSubnetStartupWakesActiveExpiryWithoutReplacingCursor(t *testing.T) {
	testTaskworkerStartupWakesActiveExpiry(t, WorkloadProfileSubnetOperator)
}

// A completed empty scan normally posts its next run 1-5 minutes later. Two
// initializers during its active claim must preserve its args and instead keep
// exactly one successor at the earliest explicit startup wake time.
func testTaskworkerStartupWakesActiveExpiry(t *testing.T, profile WorkloadProfile) {
	t.Setenv("WARP_DOMAIN", "startup-expiry-restart.example")
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		epoch := server.NowUtc().Truncate(time.Microsecond).Add(-time.Hour)
		cursor := &model.ContractExpiryCursor{ScanBefore: epoch,
			Open: &model.ContractExpiryPosition{CreateTime: epoch.Add(-time.Hour), ContractId: server.NewId()}, DisputeDone: true}
		args := &work.CloseExpiredContractsArgs{BlockSize: 1, BlockIndex: 0, Cursor: cursor,
			Sweep: &model.ContractExpirySweepCursor{Historical: cursor, RecentAfter: epoch, HistoricalNext: true}}
		key := task.RunOnce("close_expired_contracts_1_0")
		var activeId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			activeId = task.ScheduleTaskInTx(tx, work.CloseExpiredContracts, args, owner, key,
				task.RunAt(time.Unix(1, 0).UTC()), task.MaxTime(30*time.Minute), task.Priority(task.TaskPriorityFastest))
		}, server.TxReadCommitted, server.OptNoRetry())
		release := make(chan struct{})
		var released sync.Once
		target := &startupExpiryCompletionBarrier{
			Target:  task.NewTaskTargetWithPost(work.CloseExpiredContracts, work.CloseExpiredContractsPost),
			entered: make(chan *task.Task, 1), release: release,
		}
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		worker.AddTargets(target)
		var finished, retried, posts []server.Id
		var evalErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			server.HandleError(func() { finished, retried, posts, evalErr = worker.EvalTasks(1) }, func(err error) { evalErr = err })
		}()
		defer func() {
			released.Do(func() { close(release) })
			select {
			case <-done:
			case <-ctx.Done():
			}
			worker.Close()
		}()
		var claimed *task.Task
		select {
		case claimed = <-target.entered:
		case <-done:
			t.Fatal("expiry owner completed before its startup boundary", evalErr)
		case <-ctx.Done():
			t.Fatal("expiry owner did not reach its startup boundary", ctx.Err())
		}
		if claimed.TaskId != activeId {
			t.Fatal("startup barrier admitted another task")
		}
		beforeStartup := server.NowUtc()
		for range 2 {
			server.Raise(initTaskScheduleForProfile(ctx, profile))
		}
		afterStartup := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) { work.ScheduleCloseExpiredContracts(owner, tx, 0, true) }, server.TxReadCommitted, server.OptNoRetry())
		var requested time.Time
		server.Db(ctx, func(conn server.PgConn) {
			var id server.Id
			var raw string
			var generation, claimGeneration int64
			server.Raise(conn.QueryRow(ctx, `SELECT task_id,args_json,run_once_generation,claim_generation,run_once_wake_at
				FROM pending_task WHERE run_once_key=$1`, key.String()).Scan(&id, &raw, &generation, &claimGeneration, &requested))
			if id != activeId || raw != claimed.ArgsJson || claimGeneration != claimed.ClaimGeneration || generation != claimed.RunOnceGeneration+3 {
				t.Fatal("restart replaced the running expiry cursor, identity or claim", generation, claimed.RunOnceGeneration)
			}
			if requested.Before(beforeStartup) || requested.After(afterStartup) {
				t.Fatal("later request replaced the earliest startup wake", requested, beforeStartup, afterStartup)
			}
		})
		released.Do(func() { close(release) })
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("expiry completion did not join after startup", ctx.Err())
		}
		if evalErr != nil || len(finished) != 1 || finished[0] != activeId || len(retried)+len(posts) != 0 {
			t.Fatal("startup lost the active expiry completion", finished, retried, posts, evalErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var id server.Id
			var raw string
			var runAt time.Time
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name=$1`, target.TargetFunctionName()).Scan(&count))
			server.Raise(conn.QueryRow(ctx, `SELECT task_id,args_json,run_at FROM pending_task WHERE run_once_key=$1`, key.String()).Scan(&id, &raw, &runAt))
			var next work.CloseExpiredContractsArgs
			server.Raise(json.Unmarshal([]byte(raw), &next))
			if count != 1 || id == activeId || !runAt.Equal(requested) || next.Cursor != nil || next.Sweep != nil || next.BlockSize != 1 || next.BlockIndex != 0 {
				t.Fatal("startup wake lost the minimum deadline or restored an exhausted cursor", count, runAt, requested, next)
			}
		})
	})
}
