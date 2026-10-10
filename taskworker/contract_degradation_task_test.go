// The contract degradation check task: registered in both workloads, armed by
// startup only while degraded.yml enables it, inert while it is disabled, and
// re-armed one check period after each run.
package taskworker

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// The pending owners of the check.
func contractDegradationOwners(ctx context.Context) []*task.Task {
	name := task.NewTaskTarget(work.CheckContractDegradation).TargetFunctionName()
	owners := []*task.Task{}
	for _, pending := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
		if pending.FunctionName == name {
			owners = append(owners, pending)
		}
	}
	return owners
}

// Startup scheduling in a session the scheduling functions accept.
func scheduleContractDegradationForTest(ctx context.Context, schedule func(*session.ClientSession, server.PgTx)) {
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	server.Tx(ctx, func(tx server.PgTx) {
		schedule(clientSession, tx)
	})
}

// Pure: both workload profiles register the check and the subnet operator
// workload schedules it, since both create contracts.
func TestCheckContractDegradationRegisteredInBothProfiles(t *testing.T) {
	name := task.NewTaskTarget(work.CheckContractDegradation).TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		present := worker.HasTarget(name)
		worker.Close()
		if !present {
			t.Fatalf("profile %q does not register %s", profile, name)
		}
	}
	scheduled := false
	for _, operatorTask := range subnetOperatorTasks() {
		if operatorTask.target.TargetFunctionName() == name {
			scheduled = operatorTask.schedule != nil
		}
	}
	if !scheduled {
		t.Fatal("the subnet operator workload does not schedule the contract degradation check")
	}
}

// With degraded.yml absent or malformed, startup arms no owner and reaps a
// left-over one, and a claimed left-over run neither measures nor publishes
// nor schedules a successor.
func TestCheckContractDegradationDisabledNeitherRunsNorWrites(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)

		// absent, misspelled, and disabled
		for _, config := range []string{"", "enable: true\n", "enabled: false\n"} {
			func() {
				if config != "" {
					defer server.Config.PushSimpleResource(model.NetworkDegradationResourceName, []byte(config))()
				}
				if model.NetworkDegradationEnabled() {
					t.Fatal("fixture enabled the check")
				}

				// a left-over owner from an enabled deployment
				clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
				defer clientSession.Cancel()
				task.ScheduleTask(work.CheckContractDegradation, &work.CheckContractDegradationArgs{}, clientSession,
					task.RunOnce("check_contract_degradation"), task.RunAt(server.NowUtc().Add(time.Hour)))
				InitTasks(ctx)
				if owners := contractDegradationOwners(ctx); len(owners) != 0 {
					t.Fatalf("startup kept %d disabled owners", len(owners))
				}

				result, err := work.CheckContractDegradation(&work.CheckContractDegradationArgs{}, clientSession)
				if err != nil || result == nil || !result.Disabled {
					t.Fatalf("a disabled run ran: %+v %v", result, err)
				}
				if _, present, err := model.GetContractDegradationState(ctx); err != nil || present {
					t.Fatalf("a disabled run published a state: present=%t err=%v", present, err)
				}
				server.Tx(ctx, func(tx server.PgTx) {
					server.Raise(work.CheckContractDegradationPost(&work.CheckContractDegradationArgs{}, result, clientSession, tx))
				})
				if owners := contractDegradationOwners(ctx); len(owners) != 0 {
					t.Fatalf("a disabled post scheduled %d successors", len(owners))
				}
			}()
		}
	})
}

// With degraded.yml enabled, startup arms one owner due now, the run publishes
// a state, and the post re-arms the same owner one check period later.
func TestCheckContractDegradationReschedulesOneCheckPeriodLater(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		defer server.Config.PushSimpleResource(model.NetworkDegradationResourceName, []byte("enabled: true\n"))()

		start := server.NowUtc()
		scheduleContractDegradationForTest(ctx, work.ScheduleCheckContractDegradation)
		owners := contractDegradationOwners(ctx)
		if len(owners) != 1 || owners[0].RunOnceKey != task.RunOnce("check_contract_degradation").String() {
			t.Fatalf("startup armed %d owners, want one run-once owner", len(owners))
		}
		if owners[0].RunAt.After(start.Add(5 * time.Second)) {
			t.Fatalf("startup owner runs at %s, want now", owners[0].RunAt)
		}

		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		result, err := work.CheckContractDegradation(&work.CheckContractDegradationArgs{}, clientSession)
		if err != nil || result == nil || result.Disabled {
			t.Fatalf("an enabled run did not run: %+v %v", result, err)
		}
		state, present, err := model.GetContractDegradationState(ctx)
		if err != nil || !present || state.ZeroCost {
			t.Fatalf("an enabled run with no contracts did not publish a charging state: %+v %t %v", state, present, err)
		}

		// the claimed owner is gone when its post runs
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1`, task.RunOnce("check_contract_degradation").String()))
		})
		beforePost := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(work.CheckContractDegradationPost(&work.CheckContractDegradationArgs{}, result, clientSession, tx))
		})
		owners = contractDegradationOwners(ctx)
		if len(owners) != 1 {
			t.Fatalf("post armed %d owners, want one", len(owners))
		}
		delay := owners[0].RunAt.Sub(beforePost)
		if delay < model.ContractDegradationCheckInterval-time.Second || model.ContractDegradationCheckInterval+5*time.Second < delay {
			t.Fatalf("post re-arms %s later, want %s", delay, model.ContractDegradationCheckInterval)
		}
		if model.ContractDegradationCheckInterval != 15*time.Minute {
			t.Fatalf("check period %s, want 15 minutes", model.ContractDegradationCheckInterval)
		}
	})
}
