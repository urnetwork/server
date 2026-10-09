// Both real workload initializers seed the independent all-nonterminal pass.
package taskworker

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

func TestTaskworkerStartupSchedulesAllOpenContractClosures(t *testing.T) {
	testStartupAllOpenContractClosures(t, WorkloadProfileProduction)
}

func TestTaskworkerSubnetStartupSchedulesAllOpenContractClosures(t *testing.T) {
	testStartupAllOpenContractClosures(t, WorkloadProfileSubnetOperator)
}

// This test fails on the prior initializer even if the new target declarations
// are present: the actual startup queue must publish the new scan and children.
func testStartupAllOpenContractClosures(t *testing.T, profile WorkloadProfile) {
	t.Setenv("WARP_DOMAIN", "startup-close.example")
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)
		networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-initializer-close", server.NewId())
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "synthetic-source", "synthetic")
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "synthetic-destination", "synthetic")
		ids := make([]server.Id, 3)
		for index := range ids {
			id, err := model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
			server.Raise(err)
			server.Raise(model.CloseContract(ctx, id, sourceId, 17, true))
			ids[index] = id
		}
		model.SetContractDispute(ctx, ids[0], true)
		past := server.NowUtc().Truncate(time.Microsecond).Add(-time.Minute)
		future := server.NowUtc().Truncate(time.Microsecond).Add(10 * time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, ids[0], past))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, ids[1], future))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL WHERE contract_id=$1`, ids[2]))
		})
		for range 2 {
			server.Raise(initTaskScheduleForProfile(ctx, profile))
		}
		var id server.Id
		var raw string
		var runAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE run_once_key=$1`, task.RunOnce("schedule_open_contract_closures").String()).Scan(&count))
			if count != 1 {
				t.Fatal("actual startup did not seed exactly one all-open closure scan", profile, count)
			}
			server.Raise(conn.QueryRow(ctx, `SELECT task_id,args_json,run_at FROM pending_task WHERE run_once_key=$1`, task.RunOnce("schedule_open_contract_closures").String()).Scan(&id, &raw, &runAt))
		})
		var args work.ScheduleOpenContractClosuresArgs
		if json.Unmarshal([]byte(raw), &args) != nil || args.StartedAt.IsZero() || args.After != nil || runAt.After(server.NowUtc()) {
			t.Fatal("startup scan lost its fixed start or immediate wake")
		}
		registered, err := InitTaskWorkerForProfile(ctx, nil, profile)
		server.Raise(err)
		for _, target := range []task.Target{work.NewStartupContractClosureTaskTarget(), work.NewScheduledContractClosureTaskTarget(), model.NewLegacySourceSettlementTaskTarget(), model.NewLegacyPayerSettlementTaskTarget()} {
			if !registered.HasTarget(target.TargetFunctionName()) {
				registered.Close()
				t.Fatal("profile omitted a normal closure owner", profile, target.TargetFunctionName())
			}
		}
		registered.Close()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2 WHERE task_id=$1`, id, time.Unix(1, 0).UTC()))
		})
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(work.NewStartupContractClosureTaskTarget())
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
			t.Fatal("actual startup scan did not publish its children", err)
		}
		wanted := []time.Time{past, future, args.StartedAt.Add(model.DefaultContractExpiration)}
		server.Db(ctx, func(conn server.PgConn) {
			for index, contractId := range ids {
				var raw string
				var at time.Time
				server.Raise(conn.QueryRow(ctx, `SELECT args_json,run_at FROM pending_task WHERE run_once_key=$1`, task.RunOnce("close_scheduled_contract", contractId).String()).Scan(&raw, &at))
				var closeArgs work.CloseScheduledContractArgs
				if json.Unmarshal([]byte(raw), &closeArgs) != nil || closeArgs.ContractId != contractId || !closeArgs.Deadline.Equal(wanted[index]) || !at.Equal(wanted[index]) {
					t.Fatal("startup child wake differs from requested formula", index, at, wanted[index])
				}
			}
		})
	})
}
