// The verification chains survive a taskworker that rejoins on an older
// config. Chain rows are stamped through their stored JSON form, as the task
// system keeps them.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Chain arguments as a taskworker on version stores them at writtenAt.
func testStampedConfigArgs[T any](t testing.TB, version string, writtenAt time.Time) *T {
	t.Helper()
	args := new(T)
	data := fmt.Sprintf(`{"config_generation":%q,"config_written_at":%q}`, version, writtenAt.UTC().Format(time.RFC3339Nano))
	if err := json.Unmarshal([]byte(data), args); err != nil {
		t.Fatal(err)
	}
	return args
}

// The pending row under a run-once key, or nil when there is none.
func testPendingRunOnceRow(t testing.TB, ctx context.Context, runOnceKey string) *testProbeRow {
	t.Helper()
	var row *testProbeRow
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT task_id, args_json, run_at FROM pending_task WHERE run_once_key = $1`, runOnceKey)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				row = &testProbeRow{}
				var argsJson string
				server.Raise(result.Scan(&row.taskId, &argsJson, &row.runAt))
				server.Raise(json.Unmarshal([]byte(argsJson), &row.args))
			}
		})
	})
	return row
}

// A taskworker whose older config and vault disable verification reaped every
// verification chain at startup, even while instances on a newer config ran
// them, and nothing seeded them again until a newer taskworker restarted. The
// reap must keep the chains while any row carries a newer config's live
// stamp, the unstamped refresh chain among them, and still remove the chains
// when their stamps are its own config's.
func TestRemoveDisabledVerifyTasksKeepsANewerConfigsChains(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testOlderConfigVersion)
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		sweepKey := task.RunOnce("sweep_verify_trails").String()
		refreshKey := task.RunOnce("refresh_verify_proxy_egress").String()
		schedule := func(version string) {
			server.Tx(ctx, func(tx server.PgTx) {
				task.ScheduleTaskInTx(tx, SweepVerifyTrails, testStampedConfigArgs[SweepVerifyTrailsArgs](t, version, server.NowUtc()),
					clientSession, task.RunOnce("sweep_verify_trails"), task.RunAt(server.NowUtc().Add(time.Hour)))
				task.ScheduleTaskInTx(tx, RefreshVerifyProxyEgress, &RefreshVerifyProxyEgressArgs{},
					clientSession, task.RunOnce("refresh_verify_proxy_egress"), task.RunAt(server.NowUtc().Add(time.Hour)))
			})
		}
		reap := func() (removedCount int64) {
			server.Tx(ctx, func(tx server.PgTx) {
				removedCount = RemoveDisabledVerifyTasks(ctx, tx)
			}, server.TxReadCommitted)
			return
		}

		schedule(testNewerConfigVersion)
		if removedCount := reap(); removedCount != 0 || testPendingRunOnceRow(t, ctx, sweepKey) == nil || testPendingRunOnceRow(t, ctx, refreshKey) == nil {
			t.Fatalf("an older disabled config reaped %d rows of a newer config's verification chains", removedCount)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = ANY($1)`, []string{sweepKey, refreshKey}))
		})
		schedule(testOlderConfigVersion)
		if removedCount := reap(); removedCount != 2 || testPendingRunOnceRow(t, ctx, sweepKey) != nil {
			t.Fatalf("a disabled config reaped %d of its own 2 chain rows", removedCount)
		}
	})
}

// A taskworker whose older config and vault disable verification claimed a
// chain that instances on a newer config run, did nothing and scheduled no
// successor, which ended the chain until a newer taskworker restarted.
// Through the real task evaluator, it must hand each chain back unchanged,
// due again shortly. The proxy egress refresh is not covered: its chain was
// rewritten on main and is to follow there.
func TestDisabledVerifyChainsHandANewerConfigsChainBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testOlderConfigVersion)
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		due := server.NowUtc().Add(-time.Minute)
		chains := []struct {
			name       string
			family     string
			runOnceKey string
			target     task.Target
			schedule   func(tx server.PgTx)
		}{
			{
				name:       "trail sweep",
				family:     "verify_sweep_trails",
				runOnceKey: task.RunOnce("sweep_verify_trails").String(),
				target:     task.NewTaskTargetWithPost(SweepVerifyTrails, SweepVerifyTrailsPost),
				schedule: func(tx server.PgTx) {
					task.ScheduleTaskInTx(tx, SweepVerifyTrails, testStampedConfigArgs[SweepVerifyTrailsArgs](t, testNewerConfigVersion, server.NowUtc()),
						clientSession, task.RunOnce("sweep_verify_trails"), task.RunAt(due), task.MaxTime(15*time.Minute))
				},
			},
			{
				name:       "stats rollup",
				family:     "verify_rollup_provider_stats",
				runOnceKey: task.RunOnce("rollup_verify_provider_stats").String(),
				target:     task.NewTaskTargetWithPost(RollupVerifyProviderStats, RollupVerifyProviderStatsPost),
				schedule: func(tx server.PgTx) {
					task.ScheduleTaskInTx(tx, RollupVerifyProviderStats, testStampedConfigArgs[RollupVerifyProviderStatsArgs](t, testNewerConfigVersion, server.NowUtc()),
						clientSession, task.RunOnce("rollup_verify_provider_stats"), task.RunAt(due), task.MaxTime(time.Hour))
				},
			},
			{
				name:       "stats retention",
				family:     "verify_remove_old_provider_stats",
				runOnceKey: task.RunOnce("remove_old_verify_provider_stats").String(),
				target:     task.NewTaskTargetWithPost(RemoveOldVerifyProviderStats, RemoveOldVerifyProviderStatsPost),
				schedule: func(tx server.PgTx) {
					task.ScheduleTaskInTx(tx, RemoveOldVerifyProviderStats, testStampedConfigArgs[RemoveOldVerifyProviderStatsArgs](t, testNewerConfigVersion, server.NowUtc()),
						clientSession, task.RunOnce("remove_old_verify_provider_stats"), task.RunAt(due), task.MaxTime(time.Hour))
				},
			},
		}
		for _, chain := range chains {
			server.Tx(ctx, chain.schedule)
			claimed := testPendingRunOnceRow(t, ctx, chain.runOnceKey)
			worker := task.NewTaskWorkerWithDefaults(ctx)
			worker.AddTargets(chain.target)
			before := server.NowUtc()
			handbacks := testHandbackCount(chain.family)
			finished, retried, postRetried, err := worker.EvalTasks(1)
			worker.Close()
			if err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
				t.Fatalf("%s: the older config did not finish its claim: finished=%d retried=%d err=%v", chain.name, len(finished), len(retried)+len(postRetried), err)
			}
			if counted := testHandbackCount(chain.family) - handbacks; counted != 1 {
				t.Errorf("%s: %v hand-backs counted, want 1", chain.name, counted)
			}
			next := testPendingRunOnceRow(t, ctx, chain.runOnceKey)
			if next == nil {
				t.Errorf("%s: an older disabled config ended a newer config's chain", chain.name)
				continue
			}
			if next.taskId == claimed.taskId || !reflect.DeepEqual(next.args, claimed.args) {
				t.Errorf("%s: the older config did not hand the chain back unchanged: %v", chain.name, next.args)
			}
			if next.runAt.Before(before) || before.Add(time.Minute).Before(next.runAt) {
				t.Errorf("%s: the handed-back chain is due at %s, want shortly after %s", chain.name, next.runAt, before)
			}
		}
	})
}
