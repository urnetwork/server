package work

// Real queued legacy arguments drain through the evaluator without touching hole
// authority or scheduling a replacement. Direct handlers need no dependencies.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// A fixture-owned Redis tripwire records calls while its measured target runs.
type retiredContractHoleRedisGuard struct {
	active atomic.Bool
	calls  atomic.Int64
}

// Connection construction is outside the target's measured operation.
func (self *retiredContractHoleRedisGuard) DialHook(next redis.DialHook) redis.DialHook { return next }

// Any Redis command during the handler is a regression, including readiness I/O.
func (self *retiredContractHoleRedisGuard) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		if self.active.Load() {
			self.calls.Add(1)
			return errors.New("retired hole task used Redis")
		}
		return next(ctx, command)
	}
}

// Pipelines obey the same tripwire.
func (self *retiredContractHoleRedisGuard) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		if self.active.Load() {
			self.calls.Add(int64(len(commands)))
			return errors.New("retired hole task used Redis pipeline")
		}
		return next(ctx, commands)
	}
}

// Guard only the actual registered handler, leaving generic task custody free
// to persist its claim and successful finalization in PostgreSQL.
type retiredContractHoleTarget struct {
	task.Target
	attempts atomic.Int64
}

// The canonical target name and JSON decoder remain the production ones.
func (self *retiredContractHoleTarget) Run(ctx context.Context, pending *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	guarded := server.WithoutPostgres(ctx)
	defer func() { self.attempts.Add(int64(server.PacketPostgresAttempts(guarded))) }()
	return self.Target.Run(guarded, pending)
}

// Old cursor, error and post-result payloads do not reactivate the retired work.
func TestContractHoleRefreshRetiredHandlersHaveNoDependencies(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := server.WithoutPostgres(t.Context())
		guard := &retiredContractHoleRedisGuard{}
		server.Redis(ctx, func(client server.RedisClient) { client.AddHook(guard) })
		install, cancelInstall := context.WithTimeout(ctx, time.Second)
		server.Raise(server.RedisWithDeadline(install, func(client server.RedisClient) error { client.AddHook(guard); return nil }))
		cancelInstall()
		guard.active.Store(true)
		defer guard.active.Store(false)
		for _, cancelled := range []bool{false, true} {
			runCtx, cancel := context.WithCancel(ctx)
			if cancelled {
				cancel()
			}
			clientSession := session.NewLocalClientSession(runCtx, "0.0.0.0:0", nil)
			args := &RefreshContractHolesArgs{Cursor: &model.ContractHoleCursor{SourceClientId: server.NewId(), DestinationClientId: server.NewId(), ContractId: server.NewId()}, PassStarted: time.Unix(1, 0), FailedPairs: 99, Pages: 100}
			result, err := RefreshContractHoles(args, clientSession)
			if err != nil || result == nil || result.WarmReady {
				t.Fatal("retired handler did not finish", result, err)
			}
			if err := RefreshContractHolesPost(args, &RefreshContractHolesResult{Cursor: args.Cursor, Pairs: 1, FailedPairs: 99}, clientSession, nil); err != nil {
				t.Fatal("retired post failed", err)
			}
			clientSession.Cancel()
			cancel()
		}
		if server.PacketPostgresAttempts(ctx) != 0 || guard.calls.Load() != 0 {
			t.Fatal("retired handler used a dependency", server.PacketPostgresAttempts(ctx), guard.calls.Load())
		}
	})
}

// This is an actual persisted task with the old RunOnce name and cursor. Normal
// evaluator finalization consumes it; no pending row is manually deleted.
func TestContractHoleRefreshRetiredQueuedTaskDrainsWithoutSuccessor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		args := &RefreshContractHolesArgs{Cursor: &model.ContractHoleCursor{SourceClientId: server.NewId(), DestinationClientId: server.NewId(), CreateTime: time.Unix(1, 0), ContractId: server.NewId()}, PassStarted: time.Unix(1, 0), FailedPairs: 7, PairVisits: 17, Pages: 3}
		id := task.ScheduleTask(RefreshContractHoles, args, clientSession, task.RunOnce("refresh_contract_holes"), task.RunAt(time.Unix(1, 0)))
		target := &retiredContractHoleTarget{Target: task.NewTaskTargetWithPost(RefreshContractHoles, RefreshContractHolesPost)}
		worker := task.NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(target)
		guard := &retiredContractHoleRedisGuard{}
		server.Redis(ctx, func(client server.RedisClient) { client.AddHook(guard) })
		install, cancelInstall := context.WithTimeout(ctx, time.Second)
		server.Raise(server.RedisWithDeadline(install, func(client server.RedisClient) error { client.AddHook(guard); return nil }))
		cancelInstall()
		guard.active.Store(true)
		defer guard.active.Store(false)
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != id || len(retried) != 0 || len(posts) != 0 {
			t.Fatal("legacy task did not drain", finished, retried, posts, err)
		}
		if target.attempts.Load() != 0 || guard.calls.Load() != 0 {
			t.Fatal("legacy task performed source work", target.attempts.Load(), guard.calls.Load())
		}
		server.Db(ctx, func(conn server.PgConn) {
			var pending bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pending_task WHERE run_once_key=$1)`, task.RunOnce("refresh_contract_holes").String()).Scan(&pending))
			if pending {
				t.Fatal("retired task scheduled a successor")
			}
		})
	})
}
