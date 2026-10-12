// Both workload initializers arm the scheduled close reaper, which stays idle
// until contract_close.yml enables it, and a restart keeps its position.
package taskworker

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
)

func TestTaskworkerStartupArmsTheScheduledCloseReaper(t *testing.T) {
	testStartupArmsScheduledCloseReaper(t, WorkloadProfileProduction)
}

func TestTaskworkerSubnetStartupArmsTheScheduledCloseReaper(t *testing.T) {
	testStartupArmsScheduledCloseReaper(t, WorkloadProfileSubnetOperator)
}

func testStartupArmsScheduledCloseReaper(t *testing.T, profile WorkloadProfile) {
	t.Setenv("WARP_DOMAIN", "startup-reap.example")
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)
		const functionName = "github.com/urnetwork/server/taskworker/work.ReapScheduledContractCloses"
		key := task.RunOnce("reap_scheduled_contract_closes").String()
		readReaper := func() (count int, function string, args string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*),coalesce(max(function_name),''),coalesce(max(args_json),'')
					FROM pending_task WHERE run_once_key=$1`, key).Scan(&count, &function, &args))
			})
			return
		}
		server.Raise(initTaskScheduleForProfile(ctx, profile))
		count, function, _ := readReaper()
		if count != 1 || function != functionName {
			t.Fatal("startup armed no scheduled close reaper", profile, count, function)
		}
		registered, err := InitTaskWorkerForProfile(ctx, nil, profile)
		server.Raise(err)
		defer registered.Close()
		if !registered.HasTarget(functionName) {
			t.Fatal("profile omitted the scheduled close reaper", profile)
		}

		// A retained position survives another startup.
		position := `{"after":{"available_block":1,"priority":10,"max_time_seconds":30,"task_id":"` + server.NewId().String() + `"}}`
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=$2 WHERE run_once_key=$1`, key, position))
		})
		server.Raise(initTaskScheduleForProfile(ctx, profile))
		if count, _, args := readReaper(); count != 1 || args != position {
			t.Fatal("startup replaced the reaper's retained position", profile, count, args)
		}
	})
}
