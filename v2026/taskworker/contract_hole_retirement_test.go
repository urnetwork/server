package taskworker

// Every profile accepts old queued task names but startup never seeds them.

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// A post retry may outlive the old binary that recorded its nonempty cursor.
// Both real profile registries drain that finished row and its queued RunPost.
func TestContractHoleRetirementDrainsRecordedPostThroughBothProfiles(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
			func() {
				worker, err := InitTaskWorkerForProfile(ctx, nil, profile)
				if err != nil {
					t.Fatal(err)
				}
				defer worker.Close()
				args := &work.RefreshContractHolesArgs{PassStarted: time.Unix(1, 0), Cursor: &model.ContractHoleCursor{SourceClientId: server.NewId(), DestinationClientId: server.NewId(), ContractId: server.NewId()}, Pages: 7, FailedPairs: 3}
				id := task.ScheduleTask(work.RefreshContractHoles, args, clientSession, task.RunOnce("refresh_contract_holes"), task.RunAt(time.Unix(1, 0)))
				finished, retried, posts, err := worker.EvalTasks(1)
				if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
					t.Fatal("retired task did not finish through profile", profile, finished, retried, posts, err)
				}
				oldResult, err := json.Marshal(&work.RefreshContractHolesResult{Cursor: args.Cursor, Pairs: 9, FailedPairs: 3})
				server.Raise(err)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE finished_task SET result_json=$2,post_completed=false,post_error='retained pre-retirement post' WHERE task_id=$1`, id, string(oldResult)))
				})
				postId := task.ScheduleTask(worker.RunPost, &task.RunPostArgs{TaskId: id}, clientSession, task.RunAt(time.Unix(1, 0)))
				finished, retried, posts, err = worker.EvalTasks(1)
				if err != nil || len(finished) != 1 || finished[0] != postId || len(retried)+len(posts) != 0 {
					t.Fatal("retained post did not drain", profile, finished, retried, posts, err)
				}
				server.Db(ctx, func(conn server.PgConn) {
					var completed, pending bool
					server.Raise(conn.QueryRow(ctx, `SELECT post_completed,EXISTS(SELECT 1 FROM pending_task WHERE run_once_key=$2) FROM finished_task WHERE task_id=$1`, id, task.RunOnce("refresh_contract_holes").String()).Scan(&completed, &pending))
					if !completed || pending {
						t.Fatal("retired post revived chain", profile, completed, pending)
					}
				})
			}()
		}
	})
}

// Exercise the real profile initializer, rather than checking a source list.
// A retained future task is neither deleted nor re-armed during initialization.
func TestContractHoleRetirementStartupAndCompatibilityRegistry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		name := task.NewTaskTarget(work.RefreshContractHoles).TargetFunctionName()
		for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
			if err := initTaskScheduleForProfile(ctx, profile); err != nil {
				t.Fatal(err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var exists bool
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pending_task WHERE function_name=$1)`, name).Scan(&exists))
				if exists {
					t.Fatal("startup revived contract-hole task", profile)
				}
			})
			worker, err := InitTaskWorkerForProfile(ctx, nil, profile)
			if err != nil {
				t.Fatal(err)
			}
			registered := worker.HasTarget(name)
			worker.Close()
			if !registered {
				t.Fatal("queued retired task would become unknown", profile)
			}
		}
		id := task.ScheduleTask(work.RefreshContractHoles, &work.RefreshContractHolesArgs{PassStarted: time.Unix(1, 0), Cursor: &model.ContractHoleCursor{ContractId: server.NewId()}}, clientSession, task.RunOnce("refresh_contract_holes"), task.RunAt(server.NowUtc().Add(time.Hour)))
		before := task.GetTasks(ctx, id)
		for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
			if err := initTaskScheduleForProfile(ctx, profile); err != nil {
				t.Fatal(err)
			}
			if after := task.GetTasks(ctx, id); !reflect.DeepEqual(before, after) {
				t.Fatal("startup mutated retained retired task", profile)
			}
		}
	})
}
