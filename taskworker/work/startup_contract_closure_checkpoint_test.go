package work

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Count only the exact children plus the scanner's one declared checkpoint
// owner. Unrelated owners must not satisfy a publication barrier.
func startupCheckpointObservedChildren(event server.PgOwnershipEvent, children map[server.PgOwnershipKey]bool) int {
	parent := task.RunOnceOwnershipKey(task.RunOnce("schedule_open_contract_closures_on_startup"))
	count := 0
	for _, key := range event.Keys {
		if children[key] {
			count++
		} else if key != parent {
			return 0
		}
	}
	return count
}

func startupCheckpointOriginalArgs(t testing.TB, raw string) string {
	t.Helper()
	args, err := parseStartupContractClosureCheckpoint(raw)
	if err != nil {
		t.Fatal("checkpoint did not retain a valid original payload", err)
	}
	if args.Progress != nil {
		return args.Progress.OriginalArgs
	}
	return raw
}

type startupCheckpointBeforeRunTarget struct {
	*startupContractClosureTarget
	before func(context.Context, *task.Task) error
}

func (self *startupCheckpointBeforeRunTarget) Run(ctx context.Context, queued *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	if self.before != nil {
		if err := self.before(ctx, queued); err != nil {
			return nil, nil, err
		}
	}
	return self.startupContractClosureTarget.Run(ctx, queued)
}

// A wake absorbed by the resume claim survives an immediate failed body, then
// owns one fresh head pass at its exact earliest future time. Lower IDs cannot
// disappear behind the in-flight cursor, and queued raw arguments are restored.
func TestStartupContractClosureCheckpointRetainsAbsorbedWake(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		baseCtx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), time.Minute)
		defer cancel()
		network, source, destination := newStartupClosureFreeClients(baseCtx)
		ids := newStartupClosureScanContracts(baseCtx, network, source, destination, 1025)
		started := server.NowUtc().Truncate(time.Microsecond).Add(-2 * time.Hour)
		wake := server.NowUtc().Truncate(time.Microsecond).Add(10 * time.Minute)
		lower := server.Id{}
		lower[15] = 1
		timer := make(chan time.Time)
		never := make(chan time.Time)
		contexts := make(chan context.Context, 8)
		first := true
		ctx := task.Testing_WithTaskRunAfter(baseCtx, func(bodyCtx context.Context, budget time.Duration) <-chan time.Time {
			contexts <- bodyCtx
			if first {
				return timer
			}
			return never
		})
		var original string
		var parent server.Id
		key := task.RunOnce("schedule_open_contract_closures_on_startup")
		childKeys := map[server.PgOwnershipKey]bool{}
		for _, id := range ids {
			childKeys[task.RunOnceOwnershipKey(task.RunOnce("close_scheduled_contract", id))] = true
		}
		ctx = server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if !first || event.Kind != server.PgOwnershipReleased || startupCheckpointObservedChildren(event, childKeys) != 256 {
				return
			}
			publisher := session.NewLocalClientSession(baseCtx, "", nil)
			defer publisher.Cancel()
			server.Tx(baseCtx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(baseCtx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,usage_origin_is_source,create_time,expiration_time)
                    VALUES($1,$2,$3,$2,$4,100,true,$5,NULL)`, lower, network, source, destination, started))
				for _, at := range []time.Time{wake.Add(time.Minute), wake} {
					task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures, &ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: started},
						publisher, key, task.RunAt(at))
				}
			}, server.OptNoRetry())
			bodyCtx := <-contexts
			close(timer)
			select {
			case <-bodyCtx.Done():
			case <-baseCtx.Done():
				server.Raise(baseCtx.Err())
			}
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(baseCtx, func(tx server.PgTx) {
			parent = task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures, &ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: started}, owner, key, task.RunAt(started))
		})
		original = task.GetTasks(baseCtx, parent)[parent].ArgsJson
		target := &startupCheckpointBeforeRunTarget{startupContractClosureTarget: NewStartupContractClosureTaskTarget().(*startupContractClosureTarget)}
		worker := startupClosureWorker(ctx, target)
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != parent {
			t.Fatal("first interrupted pass did not retain its pending owner", err)
		}
		first = false
		before := readExpiryRecoveryQueue(t, baseCtx)[key.String()]
		if before.wakeAt == nil || !before.wakeAt.Equal(wake) {
			t.Fatal("normal concurrent request lost its minimum wake")
		}
		target.before = func(_ context.Context, queued *task.Task) error {
			var args ScheduleOpenContractClosuresArgs
			server.Raise(json.Unmarshal([]byte(queued.ArgsJson), &args))
			if args.Progress == nil || args.Progress.RerunAt == nil || !args.Progress.RerunAt.Equal(wake) ||
				args.Progress.After != ids[255] || args.Progress.Generation != queued.RunOnceGeneration {
				t.Error("resume claim did not atomically retain the absorbed wake")
			}
			return errors.New("synthetic exit after committed resume claim")
		}
		makeCloseRetryTaskDue(baseCtx, parent)
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != parent {
			t.Fatal("failed resumed body lost ordinary handback", err)
		}
		retained := readExpiryRecoveryQueue(t, baseCtx)[key.String()]
		var persisted ScheduleOpenContractClosuresArgs
		if retained.wakeAt != nil || json.Unmarshal([]byte(retained.args), &persisted) != nil || persisted.Progress == nil ||
			persisted.Progress.RerunAt == nil || !persisted.Progress.RerunAt.Equal(wake) || persisted.Progress.After != ids[255] {
			t.Fatal("claim/body failure erased the durably absorbed fresh-pass request")
		}
		target.before = nil
		evalStartupClosureTask(t, baseCtx, worker, parent)
		queue := readExpiryRecoveryQueue(t, baseCtx)
		successor := queue[key.String()]
		if successor.id == (server.Id{}) || successor.id == parent || successor.args != original || !successor.runAt.Equal(wake) {
			t.Fatal("resumed EOF lost the exact minimum future head pass or original payload")
		}
		if _, exists := queue[task.RunOnce("close_scheduled_contract", lower).String()]; exists {
			t.Fatal("fixture lower ID was not actually behind the resumed cursor")
		}
		finished, retried, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished)+len(retried)+len(posts) != 0 {
			t.Fatal("future head pass ran before its wake", err)
		}
		evalStartupClosureTask(t, baseCtx, worker, successor.id)
		complete := readExpiryRecoveryQueue(t, baseCtx)
		if len(complete) != len(ids)+1 {
			t.Fatal("fresh head pass omitted the newly inserted lower contract")
		}
		for _, id := range append(ids, lower) {
			if _, exists := complete[task.RunOnce("close_scheduled_contract", id).String()]; !exists {
				t.Fatal("fresh pass lost exact child identity")
			}
		}
	})
}

// A deferred commit refusal must roll back children and progress together.
// A later finalizer refusal must retain EOF progress without replaying children.
func TestStartupContractClosureCheckpointRollbackAndEofRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), time.Minute)
		defer cancel()
		network, source, destination := newStartupClosureFreeClients(ctx)
		ids := newStartupClosureScanContracts(ctx, network, source, destination, 257)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		key := task.RunOnce("schedule_open_contract_closures_on_startup")
		var parent server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			parent = task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures, &ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: server.NowUtc().Add(-time.Hour)}, owner, key, task.RunAt(server.NowUtc().Add(-time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION startup_checkpoint_commit_refuse() RETURNS trigger LANGUAGE plpgsql AS $body$
                BEGIN IF NEW.args_json<>OLD.args_json THEN RAISE EXCEPTION 'synthetic checkpoint commit refusal'; END IF; RETURN NEW; END $body$`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE CONSTRAINT TRIGGER startup_checkpoint_commit_refuse AFTER UPDATE ON pending_task
                DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION startup_checkpoint_commit_refuse()`))
		}, server.OptNoRetry())
		original := task.GetTasks(ctx, parent)[parent].ArgsJson
		worker := startupClosureWorker(ctx, NewStartupContractClosureTaskTarget())
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != parent {
			t.Fatal("checkpoint commit refusal did not remain an ordinary task failure", err)
		}
		queue := readExpiryRecoveryQueue(t, ctx)
		if len(queue) != 1 || queue[key.String()].args != original {
			t.Fatal("failed checkpoint commit published children or advanced cursor")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER startup_checkpoint_commit_refuse ON pending_task`))
			server.RaisePgResult(tx.Exec(ctx, `DROP FUNCTION startup_checkpoint_commit_refuse()`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION startup_checkpoint_finish_refuse() RETURNS trigger LANGUAGE plpgsql AS $body$
                BEGIN RAISE EXCEPTION 'synthetic checkpoint finalizer refusal'; END $body$`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER startup_checkpoint_finish_refuse BEFORE INSERT ON finished_task FOR EACH ROW EXECUTE FUNCTION startup_checkpoint_finish_refuse()`))
		}, server.OptNoRetry())
		makeCloseRetryTaskDue(ctx, parent)
		var caught error
		server.HandleError(func() { _, _, _, err = worker.EvalTasks(1); server.Raise(err) }, func(err error) { caught = err })
		if caught == nil {
			t.Fatal("fixture did not refuse the actual successful finalizer")
		}
		queued := readExpiryRecoveryQueue(t, ctx)
		var args ScheduleOpenContractClosuresArgs
		if len(queued) != len(ids)+1 || json.Unmarshal([]byte(queued[key.String()].args), &args) != nil || args.Progress == nil || args.Progress.After != ids[len(ids)-1] {
			t.Fatal("finalizer rollback lost committed EOF progress")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER startup_checkpoint_finish_refuse ON finished_task`))
			server.RaisePgResult(tx.Exec(ctx, `DROP FUNCTION startup_checkpoint_finish_refuse()`))
		}, server.OptNoRetry())
		task.ReleaseTask(ctx, parent)
		evalStartupClosureTask(t, ctx, worker, parent)
		complete := readExpiryRecoveryQueue(t, ctx)
		if len(complete) != len(ids) {
			t.Fatal("EOF retry did not finish exact owner")
		}
		for _, id := range ids {
			key := task.RunOnce("close_scheduled_contract", id).String()
			before, after := queued[key], complete[key]
			if before.id != after.id || before.args != after.args || before.generation != after.generation || !before.runAt.Equal(after.runAt) {
				t.Fatal("EOF handback retry replayed an already committed child")
			}
		}
		if row := task.GetFinishedTasks(ctx, parent)[parent]; row == nil || row.ArgsJson != original {
			t.Fatal("successful handback did not restore exact original arguments")
		}
	})
}

// A stale claim, wrong durable function, or wrong key cannot commit even the
// child writes preceding its checkpoint. The actual current claim still works.
func TestStartupContractClosureCheckpointRequiresExactClaim(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), time.Minute)
		defer cancel()
		network, source, destination := newStartupClosureFreeClients(ctx)
		ids := newStartupClosureScanContracts(ctx, network, source, destination, 1)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		var parent server.Id
		key := task.RunOnce("schedule_open_contract_closures_on_startup")
		server.Tx(ctx, func(tx server.PgTx) {
			parent = task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures, &ScheduleOpenContractClosuresArgs{StartedAt: server.NowUtc()}, owner, key, task.RunAt(server.NowUtc().Add(-time.Hour)))
		})
		target := &startupCheckpointBeforeRunTarget{startupContractClosureTarget: NewStartupContractClosureTaskTarget().(*startupContractClosureTarget)}
		target.before = func(runCtx context.Context, queued *task.Task) error {
			for variant := range 4 {
				stale := *queued
				switch variant {
				case 0:
					stale.ClaimGeneration++
				case 1:
					stale.TaskId = server.NewId()
				case 2:
					stale.RunOnceKey = task.RunOnce("synthetic-other-scanner").String()
				}
				child := &CloseScheduledContractArgs{Private: true, ScheduledContractClose: ScheduledContractClose{ContractId: ids[0], Deadline: server.NowUtc()}}
				var caught error
				server.HandleError(func() {
					keys := []server.PgOwnershipKey{task.PendingTaskOwnershipKey(stale.TaskId, &stale.RunOnceKey), task.RunOnceOwnershipKey(task.RunOnce("close_scheduled_contract", ids[0]))}
					server.OwnedTx(runCtx, keys, func(tx server.PgTx) {
						task.ScheduleTaskInTx(tx, CloseScheduledContract, child, owner, task.RunOnce("close_scheduled_contract", ids[0]), task.RequireQueueOwnership(tx))
						if variant == 3 {
							server.RaisePgResult(tx.Exec(runCtx, `UPDATE pending_task SET function_name='synthetic/changed.Function' WHERE task_id=$1`, queued.TaskId))
						}
						task.CheckpointTaskArgsInTx(runCtx, tx, &stale, queued.ArgsJson)
					}, server.TxReadCommitted, server.OptNoRetry())
				}, func(err error) { caught = err })
				if caught == nil {
					t.Error("stale checkpoint committed", variant)
				}
				queue := readExpiryRecoveryQueue(t, ctx)
				if len(queue) != 1 || queue[key.String()].args != queued.ArgsJson {
					t.Error("stale checkpoint advanced parent or published child", variant)
				}
			}
			return nil
		}
		worker := startupClosureWorker(ctx, target)
		defer worker.Close()
		evalStartupClosureTask(t, ctx, worker, parent)
		if len(readExpiryRecoveryQueue(t, ctx)) != 1 {
			t.Fatal("valid fenced claim could not publish after stale refusals")
		}
	})
}

// Both rolling-version cases choose a conservative fresh head. Malformed
// progress remains a per-task error, while an ordinary peer still completes.
func TestStartupContractClosureCheckpointLegacyAndMalformedClaims(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), time.Minute)
		defer cancel()
		network, source, destination := newStartupClosureFreeClients(ctx)
		ids := newStartupClosureScanContracts(ctx, network, source, destination, 2)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		key := task.RunOnce("schedule_open_contract_closures_on_startup")
		started := server.NowUtc().Add(-time.Hour)
		raw, err := json.Marshal(&ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: started})
		server.Raise(err)
		target := NewStartupContractClosureTaskTarget().(*startupContractClosureTarget)
		for _, sameId := range []bool{false, true} {
			id := server.NewId()
			progressId := server.NewId()
			if sameId {
				progressId = id
			}
			saved := &ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: started, Progress: &startupContractClosureProgress{
				TaskId: progressId, After: ids[1], OriginalArgs: string(raw), Generation: 0,
			}}
			encoded, err := json.Marshal(saved)
			server.Raise(err)
			rewritten, err := target.TaskClaimCheckpointArgs(id, string(encoded), 1, nil)
			if err != nil || rewritten != string(raw) {
				t.Fatal("legacy worker cursor/wake could not recover through a fresh head", sameId, err)
			}
		}
		var parent server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			parent = task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures, &ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: started}, owner, key, task.RunAt(started))
			versioned := strings.Replace(target.TargetFunctionName(), "/server/", "/server/v7/", 1)
			if versioned == target.TargetFunctionName() {
				t.Fatal("fixture did not create a retained versioned function")
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET function_name=$2 WHERE task_id=$1`, parent, versioned))
		})
		worker := startupClosureWorker(ctx, target)
		defer worker.Close()
		evalStartupClosureTask(t, ctx, worker, parent)
		if len(readExpiryRecoveryQueue(t, ctx)) != 2 {
			t.Fatal("version-normalized dispatch lost stored-function checkpoint authority")
		}
		peerCalls := 0
		peer := task.NewTaskTarget(func(struct{}, *session.ClientSession) (struct{}, error) { peerCalls++; return struct{}{}, nil })
		worker.AddTargets(peer)
		var badId, peerId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			badId = task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures, &ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: started}, owner, key, task.RunAt(started))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=$2 WHERE task_id=$1`, badId, `{"_startup_scan_progress":{"original_args":"bad"}}`))
			peerId = task.ScheduleTaskInTx(tx, func(struct{}, *session.ClientSession) (struct{}, error) { return struct{}{}, nil }, struct{}{}, owner, task.RunAt(started))
			// The registration's exact canonical function is authoritative.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET function_name=$2 WHERE task_id=$1`, peerId, peer.TargetFunctionName()))
		})
		finished, retried, posts, err := worker.EvalTasks(2)
		if err != nil || len(finished) != 1 || finished[0] != peerId || len(retried) != 1 || retried[0] != badId || len(posts) != 0 || peerCalls != 1 {
			t.Fatal("malformed checkpoint poisoned an unrelated committed claim", err)
		}
	})
}
