// The disabled marker: seeded by a disabling config's startup, restamped while
// that config's taskworkers run it, handed back while it is a newer config's
// live one, and retired by a config that enables probing.
package work

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The pending marker, or nil when there is none.
func testPendingDisabledMarker(t testing.TB, ctx context.Context) *testProbeRow {
	t.Helper()
	return testPendingRunOnceRow(t, ctx, providerEgressProbeDisabledRunOnce().String())
}

// Runs the due marker once through the real task evaluator, registered as the
// taskworker registers it. The post-step's work after the commit has run when
// it returns.
func testEvalDisabledMarker(t testing.TB, ctx context.Context) {
	t.Helper()
	worker := task.NewTaskWorkerWithDefaults(ctx)
	defer worker.Close()
	worker.AddTargets(NewProviderEgressProbeDisabledTaskTarget())
	finished, retried, postRetried, err := worker.EvalTasks(1)
	if err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
		t.Fatalf("the marker did not finish one run: finished=%d retried=%d err=%v", len(finished), len(retried)+len(postRetried), err)
	}
}

// The written time of a stored stamp.
func testStampWrittenAt(t testing.TB, row *testProbeRow) time.Time {
	t.Helper()
	writtenAtText, _ := row.args["config_written_at"].(string)
	writtenAt, err := time.Parse(time.RFC3339Nano, writtenAtText)
	if err != nil {
		t.Fatalf("the stamp has no write time: %v", row.args)
	}
	return writtenAt
}

// A disabling config's startup schedules no shard and seeds the marker,
// stamped with its version and due at once. Each run under that config
// restamps the marker one interval later.
func TestProviderEgressProbeDisablingConfigKeepsItsMarker(t *testing.T) {
	if name := task.NewTaskTarget(ProviderEgressProbeDisabled).TargetFunctionName(); name != testDisabledMarkerFunctionName {
		t.Fatalf("the marker is stored as %s, the fixtures as %s", name, testDisabledMarkerFunctionName)
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testNewerConfigVersion)
		disabled := testUrlGeometrySettings(8, 64)
		disabled.Enabled = false
		withProviderEgressProbeSettings(t, disabled)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleProviderEgressProbeTasks(clientSession, tx)
		}, server.TxReadCommitted)
		if count := testPendingProbeRowCount(t, ctx); count != 0 {
			t.Fatalf("a disabling config's startup scheduled %d shards", count)
		}
		seeded := testPendingDisabledMarker(t, ctx)
		if seeded == nil || seeded.args["config_generation"] != testNewerConfigVersion || server.NowUtc().Add(time.Second).Before(seeded.runAt) {
			t.Fatalf("a disabling config's startup did not seed its marker due now: %+v", seeded)
		}

		// A row becomes claimable one second after its run time.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at = $2 WHERE run_once_key = $1`,
				providerEgressProbeDisabledRunOnce().String(), server.NowUtc().Add(-time.Minute)))
		})
		testEvalDisabledMarker(t, ctx)
		next := testPendingDisabledMarker(t, ctx)
		if next == nil || next.taskId == seeded.taskId || next.args["config_generation"] != testNewerConfigVersion {
			t.Fatalf("a run under the disabling config did not restamp the marker: %+v", next)
		}
		if testStampWrittenAt(t, next).Before(before) {
			t.Fatalf("the restamped marker keeps the stamp written at %s", testStampWrittenAt(t, next))
		}
		want := server.NowUtc().Add(providerEgressProbeDisabledInterval)
		if next.runAt.Before(want.Add(-time.Minute)) || want.Add(time.Second).Before(next.runAt) {
			t.Fatalf("the restamped marker is due at %s, want about %s", next.runAt, want)
		}
	})
}

// A marker a newer config wrote within its timeout is handed back unchanged
// by an older config, whether that config enables or disables probing. A
// config that enables probing retires a marker that is not a newer config's
// live one and recreates its own shard set, which a withdrawn disable had left
// retired until a taskworker restarted. A disabling config restamps the marker
// with its own version. Neither a hand-back nor a restamp creates shards.
func TestProviderEgressProbeDisabledMarkerFollowsTheNewestConfig(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cases := []struct {
			name          string
			version       string
			enabled       bool
			markerVersion string
			markerAge     time.Duration
			want          string
		}{
			{name: "older enabling config, newer live marker", version: testOlderConfigVersion, enabled: true, markerVersion: testNewerConfigVersion, want: "handed back"},
			{name: "older disabling config, newer live marker", version: testOlderConfigVersion, enabled: false, markerVersion: testNewerConfigVersion, want: "handed back"},
			{name: "enabling config, older marker", version: testNewerConfigVersion, enabled: true, markerVersion: testOlderConfigVersion, want: "retired"},
			{name: "older enabling config, withdrawn newer marker", version: testOlderConfigVersion, enabled: true, markerVersion: testNewerConfigVersion,
				markerAge: providerEgressProbeNewerGenerationMinTimeout + time.Minute, want: "retired"},
			{name: "disabling config, older marker", version: testNewerConfigVersion, enabled: false, markerVersion: testOlderConfigVersion, want: "restamped"},
		}
		for _, c := range cases {
			t.Setenv("WARP_CONFIG_VERSION", c.version)
			settings := testUrlGeometrySettings(4, 8)
			settings.Enabled = c.enabled
			withProviderEgressProbeSettings(t, settings)
			// the disabled shard set is retired
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1 OR function_name = $2`,
					providerEgressProbeDisabledRunOnce().String(), ProviderEgressProbeTaskFunctionNames()[0]))
				testInsertDisabledMarker(t, ctx, tx, c.markerVersion, server.NowUtc().Add(-c.markerAge), server.NowUtc().Add(-time.Minute))
			})
			claimed := testPendingDisabledMarker(t, ctx)
			handbacks := testHandbackCount("provider_egress_probe_disabled")
			before := server.NowUtc()
			testEvalDisabledMarker(t, ctx)
			next := testPendingDisabledMarker(t, ctx)
			counted := testHandbackCount("provider_egress_probe_disabled") - handbacks
			if want := map[bool]float64{true: 1, false: 0}[c.want == "handed back"]; counted != want {
				t.Errorf("%s: %v hand-backs counted, want %v", c.name, counted, want)
			}
			switch c.want {
			case "handed back":
				if next == nil || next.taskId == claimed.taskId || !reflect.DeepEqual(next.args, claimed.args) ||
					next.runAt.Before(before) || before.Add(time.Minute).Before(next.runAt) {
					t.Errorf("%s: the marker was not handed back unchanged and due shortly: %+v", c.name, next)
				}
			case "retired":
				if next != nil {
					t.Errorf("%s: the marker was not retired: %v", c.name, next.args)
				}
				if count := testPendingProbeRowCount(t, ctx); count != settings.ShardCount {
					t.Errorf("%s: retiring the marker brought back %d of the %d shards", c.name, count, settings.ShardCount)
				}
			case "restamped":
				if next == nil || next.args["config_generation"] != c.version || testStampWrittenAt(t, next).Before(before) {
					t.Errorf("%s: the marker was not restamped with this config's version: %+v", c.name, next)
				}
			}
			if count := testPendingProbeRowCount(t, ctx); c.want != "retired" && count != 0 {
				t.Errorf("%s: the marker run created %d shards", c.name, count)
			}
		}
	})
}
