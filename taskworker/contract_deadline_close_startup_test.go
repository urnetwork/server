// Both workload initializers arm the continuous pass that retires contracts
// carrying their own deadline, and a restart keeps its position.
package taskworker

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
)

func TestTaskworkerStartupArmsTheContractDeadlinePass(t *testing.T) {
	testStartupArmsContractDeadlinePass(t, WorkloadProfileProduction)
}

func TestTaskworkerSubnetStartupArmsTheContractDeadlinePass(t *testing.T) {
	testStartupArmsContractDeadlinePass(t, WorkloadProfileSubnetOperator)
}

// Contracts issued since the startup pass passed their position have no queued
// close, and the prior initializer armed nothing else that retires them
// through an owner batch. Names only, so the prior worker fails here rather
// than at compile time.
func testStartupArmsContractDeadlinePass(t *testing.T, profile WorkloadProfile) {
	t.Setenv("WARP_DOMAIN", "startup-deadline.example")
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)
		const functionName = "github.com/urnetwork/server/taskworker/work.CloseContractDeadlines"
		key := task.RunOnce("close_contract_deadlines").String()
		readPass := func() (count int, function string, args string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*),coalesce(max(function_name),''),coalesce(max(args_json),'')
					FROM pending_task WHERE run_once_key=$1`, key).Scan(&count, &function, &args))
			})
			return
		}
		server.Raise(initTaskScheduleForProfile(ctx, profile))
		count, function, _ := readPass()
		if count != 1 || function != functionName {
			t.Fatal("startup armed no contract deadline pass", profile, count, function)
		}
		registered, err := InitTaskWorkerForProfile(ctx, nil, profile)
		server.Raise(err)
		defer registered.Close()
		if !registered.HasTarget(functionName) {
			t.Fatal("profile omitted the contract deadline pass", profile)
		}

		// A retained position survives another startup.
		position := `{"fresh":"` + server.NewId().String() + `"}`
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=$2 WHERE run_once_key=$1`, key, position))
		})
		server.Raise(initTaskScheduleForProfile(ctx, profile))
		if count, _, args := readPass(); count != 1 || args != position {
			t.Fatal("startup replaced the pass's retained position", profile, count, args)
		}
	})
}
