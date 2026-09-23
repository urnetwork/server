package taskworker

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// TestSubnetOperatorWorkloadRejectsUnknownProfile covers every public entry
// before any database, vault, or listener access can occur.
func TestSubnetOperatorWorkloadRejectsUnknownProfile(t *testing.T) {
	ctx := context.Background()
	profile := WorkloadProfile("unknown-fixture")
	if err := Run(ctx, RunOptions{Port: 1, Count: 1, BatchSize: 1, WorkloadProfile: profile}); err == nil || !strings.Contains(err.Error(), "unknown taskworker workload profile") {
		t.Fatalf("runner accepted unknown profile: %v", err)
	}
	if err := InitTasksForProfile(ctx, profile); err == nil {
		t.Fatal("scheduler accepted unknown profile")
	}
	if worker, err := InitTaskWorkerForProfile(ctx, nil, profile); err == nil || worker != nil {
		t.Fatal("registry accepted unknown profile")
	}
}

// TestSubnetOperatorWorkloadKeepsRequiredTargets protects both dynamic API
// work and all settlement/verification chains, including their legacy aliases.
func TestSubnetOperatorWorkloadKeepsRequiredTargets(t *testing.T) {
	settings := task.DefaultTaskWorkerSettings()
	worker, err := InitTaskWorkerForProfile(context.Background(), settings, WorkloadProfileSubnetOperator)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if settings.ClaimRegisteredTargetsOnly {
		t.Fatal("profile mutated caller-owned settings used by ordinary workers")
	}
	for _, target := range []task.Target{
		task.NewTaskTarget(work.StSyncChain), task.NewTaskTarget(work.StEpochClose),
		task.NewTaskTarget(work.StCommitRoot), task.NewTaskTarget(work.StDeposit),
		task.NewTaskTarget(work.StFinalizePoke), task.NewTaskTarget(work.SweepVerifyTrails),
		task.NewTaskTarget(work.RollupVerifyProviderStats), task.NewTaskTarget(work.RemoveOldVerifyProviderStats),
		task.NewTaskTarget(work.RefreshVerifyProxyEgress), task.NewTaskTarget(work.CloseExpiredContracts),
		task.NewTaskTarget(work.ReconcileNetEscrow), task.NewTaskTarget(model.RemoveNetworkClientsTask),
		task.NewTaskTarget(work.DbMaintenance), task.NewTaskTarget(work.BackfillClock),
		task.NewTaskTarget(work.UpdateClientLocations), task.NewTaskTarget(work.UpdateClientScores),
		task.NewTaskTarget(work.RollupTransferAuditEvents), task.NewTaskTarget(work.ExportStats),
	} {
		if !worker.HasTarget(target.TargetFunctionName()) {
			t.Errorf("subnet operator lost required target %s", target.TargetFunctionName())
		}
	}
	for _, legacyName := range []string{"main.TaskCleanup", "bringyour.com/service/taskworker/work.CloseExpiredContracts"} {
		if !worker.HasTarget(legacyName) {
			t.Errorf("subnet operator lost retained alias %s", legacyName)
		}
	}
	for _, target := range []task.Target{
		task.NewTaskTarget(work.Payout), task.NewTaskTarget(work.ProcessPendingPayouts),
		task.NewTaskTarget(controller.AdvancePayment), task.NewTaskTarget(controller.PlaySubscriptionRenewal),
		task.NewTaskTarget(controller.OnboardingCampaignStep), task.NewTaskTarget(work.PaymentReconcile),
		task.NewTaskTarget(work.RefreshGeolocationSourcePins), task.NewTaskTarget(work.ProberBootstrap),
		task.NewTaskTarget(work.ProviderEgressProbe), task.NewTaskTarget(work.ExtenderProbe),
		task.NewTaskTarget(work.ExtenderPublish), task.NewTaskTarget(work.WebSearchAnalytics),
	} {
		if worker.HasTarget(target.TargetFunctionName()) {
			t.Errorf("subnet operator admitted unrelated workload %s", target.TargetFunctionName())
		}
	}
}

// TestSubnetOperatorWorkloadSchedulesOnlyItsDomain retains the two actual
// incident task types and an old removed target, with no delete/re-arm on init.
func TestSubnetOperatorWorkloadSchedulesOnlyItsDomain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		controller.SetStConfig(&controller.StConfig{Enabled: true})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		retainedIds := []server.Id{
			task.ScheduleTask(work.Payout, &work.SchedulePayoutArgs{}, clientSession, task.RunOnce("payout"), task.RunAt(server.NowUtc().Add(24*time.Hour))),
			task.ScheduleTask(work.RefreshGeolocationSourcePins, &work.RefreshGeolocationSourcePinsArgs{}, clientSession, task.RunOnce("refresh_geolocation_source_pins"), task.RunAt(server.NowUtc().Add(24*time.Hour))),
			task.ScheduleTask(orphanSeedTask, &orphanSeedArgs{}, clientSession),
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := tx.Exec(ctx, `UPDATE pending_task SET function_name = $2 WHERE task_id = $1`, retainedIds[2], removedTaskTargets[0])
			server.Raise(err)
		})
		before := task.GetTasks(ctx, retainedIds...)
		worker, err := InitTaskWorkerForProfile(ctx, nil, WorkloadProfileSubnetOperator)
		if err != nil {
			t.Fatal(err)
		}
		defer worker.Close()
		for range 2 {
			if err := InitTasksForProfile(ctx, WorkloadProfileSubnetOperator); err != nil {
				t.Fatal(err)
			}
			if after := task.GetTasks(ctx, retainedIds...); !reflect.DeepEqual(before, after) {
				t.Fatal("profile init deleted, re-armed, or changed an excluded retained row")
			}
			seeded := map[string]bool{}
			for taskId, pending := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
				if _, retained := before[taskId]; retained {
					continue
				}
				if !worker.HasTarget(pending.FunctionName) {
					t.Errorf("profile seeded unregistered workload %s", pending.FunctionName)
				}
				seeded[pending.FunctionName] = true
			}
			for _, target := range []task.Target{
				task.NewTaskTarget(work.StSyncChain), task.NewTaskTarget(work.SweepVerifyTrails),
				task.NewTaskTarget(work.RollupVerifyProviderStats), task.NewTaskTarget(work.RefreshVerifyProxyEgress),
				task.NewTaskTarget(work.CloseExpiredContracts), task.NewTaskTarget(work.DbMaintenance),
				task.NewTaskTarget(work.ExportStats), task.NewTaskTarget(work.UpdateClientLocations),
			} {
				if !seeded[target.TargetFunctionName()] {
					t.Errorf("profile failed to seed required workload %s", target.TargetFunctionName())
				}
			}
		}
	})
}

// TestSubnetOperatorWorkloadClaimsPastRetainedAndApiTasks uses the actual
// incident task names and an actual local cleanup body; no external job runs.
func TestSubnetOperatorWorkloadClaimsPastRetainedAndApiTasks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		retainedIds := []server.Id{}
		for range 70 {
			retainedIds = append(retainedIds,
				task.ScheduleTask(work.Payout, &work.SchedulePayoutArgs{}, clientSession, task.RunAt(server.NowUtc().Add(-2*time.Hour))),
				task.ScheduleTask(work.RefreshGeolocationSourcePins, &work.RefreshGeolocationSourcePinsArgs{}, clientSession, task.RunAt(server.NowUtc().Add(-2*time.Hour))),
			)
		}
		worker, err := InitTaskWorkerForProfile(ctx, nil, WorkloadProfileSubnetOperator)
		if err != nil {
			t.Fatal(err)
		}
		defer worker.Close()
		// This is an independent controller-style enqueue after worker init;
		// startup scheduling alone cannot prevent its execution.
		retainedIds = append(retainedIds, task.ScheduleTask(controller.AdvancePayment, &controller.AdvancePaymentArgs{}, clientSession, task.RunAt(server.NowUtc().Add(-2*time.Hour))))
		before := task.GetTasks(ctx, retainedIds...)
		allowedId := task.ScheduleTask(work.RemoveExpiredWalletNonces, &work.RemoveExpiredWalletNoncesArgs{}, clientSession, task.RunAt(server.NowUtc().Add(-time.Hour)))
		finished, retried, postRetried, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != allowedId || len(retried)+len(postRetried) != 0 {
			t.Fatalf("required operator work did not pass retained retail/pin backlog: %v/%v/%v %v", finished, retried, postRetried, err)
		}
		if after := task.GetTasks(ctx, retainedIds...); !reflect.DeepEqual(before, after) {
			t.Fatal("operator worker mutated retained or independently enqueued excluded work")
		}
	})
}

// TestSubnetOperatorWorkloadPreservesProductionDefault keeps the ordinary
// public constructors and zero-value profile on the complete production set.
func TestSubnetOperatorWorkloadPreservesProductionDefault(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		worker := InitTaskWorker(ctx)
		defer worker.Close()
		if err := InitTasksForProfile(ctx, WorkloadProfileProduction); err != nil {
			t.Fatal(err)
		}
		seeded := map[string]bool{}
		for _, pending := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
			seeded[pending.FunctionName] = true
		}
		for _, target := range []task.Target{task.NewTaskTarget(work.Payout), task.NewTaskTarget(work.RefreshGeolocationSourcePins), task.NewTaskTarget(work.StSyncChain)} {
			name := target.TargetFunctionName()
			if !worker.HasTarget(name) || !seeded[name] {
				t.Errorf("ordinary production default lost scheduled/registered task %s", name)
			}
		}
		if !worker.HasTarget(task.NewTaskTarget(controller.AdvancePayment).TargetFunctionName()) {
			t.Fatal("ordinary production default lost controller-enqueued retail payments")
		}
	})
}
