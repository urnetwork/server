// The serving taskworker registers and schedules the capability probe; a
// subnet operator leaves it alone, as it does every external probe.
package taskworker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/capprobe"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Only the production registry holds the capability probe's target.
func TestCapabilityProbeIsProductionOnly(t *testing.T) {
	name := work.NewCapabilityProbeTaskTarget().TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		present := worker.HasTarget(name)
		worker.Close()
		if present != (profile == WorkloadProfileProduction) {
			t.Fatalf("capability probe registration profile=%q present=%t", profile, present)
		}
	}
	for _, definition := range subnetOperatorTasks() {
		if definition.target.TargetFunctionName() == name {
			t.Fatal("the subnet operator schedules the capability probe")
		}
	}
}

// The pending rows of the probe's one run-once key, and the run time and
// arguments of the last.
func capabilityProbeTestRow(t testing.TB, ctx context.Context) (count int, runAt time.Time, args work.CapabilityProbeArgs) {
	t.Helper()
	argsJson := "{}"
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx,
			`SELECT count(*), COALESCE(max(run_at), 'epoch'::timestamp), COALESCE(max(args_json), '{}') FROM pending_task WHERE run_once_key = $1`,
			task.RunOnce("capability_probe").String(),
		).Scan(&count, &runAt, &argsJson))
	})
	server.Raise(json.Unmarshal([]byte(argsJson), &args))
	return
}

// The probe's prober shards, slot 255: none ever opens for a pass with
// nothing to measure, or for a switched-off probe.
func capabilityProbeTestShards(t testing.TB, ctx context.Context) int {
	t.Helper()
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM prober_shard_run WHERE shard_index = 255`).Scan(&count))
	})
	return count
}

// The capability probe ships switched off (sn-55): a taskworker's startup
// schedule, which every deploy runs, schedules no task for it and opens no
// prober shard while capability_probe.yml is absent or says enabled: false.
// It is switched on by a startup schedule that reads enabled: true, which
// schedules its row due at once, carrying the max time it was scheduled with.
func TestCapabilityProbeShipsSwitchedOff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		// the URL probe's endpoints and a shard geometry clear of slot 255
		withProbePlacementGeometry(t, 4, 8, 0)
		ctx := context.Background()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		for _, c := range []struct {
			name string
			// "" leaves capability_probe.yml absent
			resource  string
			scheduled bool
		}{
			{name: "no capability_probe.yml", resource: "", scheduled: false},
			{name: "enabled: false", resource: "enabled: false\n", scheduled: false},
			{name: "enabled: true", resource: "enabled: true\n", scheduled: true},
		} {
			pop := func() {}
			if c.resource != "" {
				pop = server.Config.PushSimpleResource(capprobe.SettingsResourceName, []byte(c.resource))
			}
			scheduledAt := server.NowUtc()
			err := initTaskScheduleForProfile(ctx, WorkloadProfileProduction)
			pop()
			if err != nil {
				t.Fatal(err)
			}
			count, runAt, args := capabilityProbeTestRow(t, ctx)
			switch {
			case !c.scheduled && count != 0:
				t.Fatalf("%s: the startup schedule scheduled %d capability probe rows, want none", c.name, count)
			case c.scheduled && (count != 1 || runAt.Before(scheduledAt) || runAt.After(server.NowUtc()) || args.MaxTimeSeconds != capprobe.DefaultSettings().MaxTimeSeconds):
				t.Fatalf("%s: the startup schedule scheduled %d rows at %s with %+v, want one due at once", c.name, count, runAt, args)
			}
			if shards := capabilityProbeTestShards(t, ctx); shards != 0 {
				t.Fatalf("%s: the startup schedule opened %d capability probe shards", c.name, shards)
			}
		}
	})
}

// Through the production worker, switched on by capability_probe.yml: the
// row the startup schedule leaves runs a pass that completes its sweep of a
// population without providers from the database alone, opens no prober
// shard, sets the next sweep's start the sweep interval after this one's,
// and schedules a waiting pass ten minutes on, carrying the max time it was
// scheduled with. Switched off again, the next pass runs nothing and
// schedules no successor: the chain ends there.
func TestProductionCapabilityProbePassesWithoutProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		// the URL probe's endpoints and a shard geometry clear of slot 255
		withProbePlacementGeometry(t, 4, 8, 0)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		maxTimeSeconds := capprobe.DefaultSettings().MaxTimeSeconds
		sweepInterval := time.Duration(capprobe.DefaultSettings().MinSweepIntervalMinutes) * time.Minute
		// a row becomes claimable a second past its run time (its
		// available_block); the test runs the pending row at once
		runPendingNow := func() {
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `UPDATE pending_task SET run_at = $2 WHERE run_once_key = $1`, task.RunOnce("capability_probe").String(), server.NowUtc().Add(-time.Minute)))
			})
		}
		evaluate := func(name string) {
			worker := InitTaskWorker(ctx)
			finished, retried, postRetried, err := worker.EvalTasks(1)
			worker.Close()
			if err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
				t.Fatalf("%s: the production worker did not finish the pass: finished=%d retried=%d err=%v", name, len(finished), len(retried)+len(postRetried), err)
			}
		}

		popEnabled := server.Config.PushSimpleResource(capprobe.SettingsResourceName, []byte("enabled: true\n"))
		server.Tx(ctx, func(tx server.PgTx) {
			work.ScheduleCapabilityProbe(clientSession, tx)
		})
		if count, _, args := capabilityProbeTestRow(t, ctx); count != 1 || args.MaxTimeSeconds != maxTimeSeconds {
			t.Fatalf("switched on: scheduled %d rows with %+v, want one carrying a %d s max time", count, args, maxTimeSeconds)
		}
		runPendingNow()
		evaluatedAt := server.NowUtc()
		evaluate("switched on")
		popEnabled()
		count, runAt, args := capabilityProbeTestRow(t, ctx)
		if count != 1 || runAt.Before(evaluatedAt.Add(9*time.Minute)) || runAt.After(server.NowUtc().Add(11*time.Minute)) || args.MaxTimeSeconds != maxTimeSeconds {
			t.Fatalf("switched on: %d successor rows at %s with %+v, want a waiting pass about ten minutes out", count, runAt, args)
		}
		if nextSweepAt := args.Sweep.NextSweepAt; nextSweepAt.Before(evaluatedAt.Add(sweepInterval)) || nextSweepAt.After(server.NowUtc().Add(sweepInterval)) {
			t.Fatalf("switched on: the successor's next sweep starts at %s, want the sweep interval after this pass", nextSweepAt)
		}
		if shards := capabilityProbeTestShards(t, ctx); shards != 0 {
			t.Fatalf("switched on: a pass with nothing to measure opened %d prober shards", shards)
		}

		popDisabled := server.Config.PushSimpleResource(capprobe.SettingsResourceName, []byte("enabled: false\n"))
		runPendingNow()
		evaluate("switched off")
		popDisabled()
		if count, runAt, args := capabilityProbeTestRow(t, ctx); count != 0 {
			t.Fatalf("switched off: the pass scheduled %d successors at %s with %+v, want none", count, runAt, args)
		}
		if shards := capabilityProbeTestShards(t, ctx); shards != 0 {
			t.Fatalf("switched off: the pass opened %d prober shards", shards)
		}
	})
}

// The capability probe's chain restarts this process has counted.
func capabilityProbeTestChainRestarts(t testing.TB) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "urnetwork_capability_probe_chain_restarts_total" {
			return family.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatal("no urnetwork_capability_probe_chain_restarts_total")
	return 0
}

// Re-review finding 2: a pass that a taskworker still running a config that
// switched the probe off claimed ended the probe's chain, and nothing
// scheduled it again until a taskworker restarted with the probe on. A
// production taskworker's runtime, with capability_probe.yml switching the
// probe on, checks the chain every interval and schedules a pass due at once
// when the chain has no row, counting it. While the probe reads as off it
// schedules nothing, and a row that exists keeps its run time and
// arguments.
func TestCapabilityProbeChainReturnsAfterAStaleTaskworkerEndsIt(t *testing.T) {
	previousInterval := capabilityProbeEnsureInterval
	capabilityProbeEnsureInterval = 20 * time.Millisecond
	defer func() { capabilityProbeEnsureInterval = previousInterval }()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		// the URL probe's endpoints and a shard geometry clear of slot 255
		withProbePlacementGeometry(t, 4, 8, 0)
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		pop := server.Config.PushSimpleResource(capprobe.SettingsResourceName, []byte("enabled: true\n"))
		defer func() { pop() }()
		switchTo := func(resource string) {
			pop()
			pop = server.Config.PushSimpleResource(capprobe.SettingsResourceName, []byte(resource))
		}
		// several of the runtime's intervals
		settle := func() {
			select {
			case <-ctx.Done():
			case <-time.After(20 * capabilityProbeEnsureInterval):
			}
		}

		// a serving taskworker, without workers of its own: the test runs
		// the probe's passes itself
		lifetime, cancelLifetime := context.WithCancel(ctx)
		defer cancelLifetime()
		runtimeOwner, err := startTaskworkerRuntime(ctx, lifetime, cancelLifetime, RunOptions{Port: 8080, Count: 0, BatchSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		// The runtime's stats collector and chain check read the st config,
		// capability_probe.yml and the interval that the deferred calls above
		// restore, so they are joined first: without that the race detector
		// reports the stats collector's read against the st config's reset.
		defer func() {
			runtimeOwner.Drain()
			runtimeOwner.WaitFinalHandback()
			cancelLifetime()
			runtimeOwner.(*taskworkerRuntimeOwner).Wait()
		}()
		if count, _, _ := capabilityProbeTestRow(t, ctx); count != 1 {
			t.Fatalf("the startup schedule left %d capability probe rows, want one", count)
		}

		// a taskworker still on a config that switches the probe off claims
		// the next pass, which ends the chain
		switchTo("enabled: false\n")
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `UPDATE pending_task SET run_at = $2 WHERE run_once_key = $1`, task.RunOnce("capability_probe").String(), server.NowUtc().Add(-time.Minute)))
		})
		workerSettings := task.DefaultTaskWorkerSettings()
		workerSettings.ClaimRegisteredTargetsOnly = true
		stale := task.NewTaskWorker(ctx, workerSettings)
		stale.AddTargets(work.NewCapabilityProbeTaskTarget())
		finished, retried, postRetried, err := stale.EvalTasks(1)
		stale.Close()
		if err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
			t.Fatalf("the stale pass did not finish: finished=%d retried=%d err=%v", len(finished), len(retried)+len(postRetried), err)
		}
		restartsBefore := capabilityProbeTestChainRestarts(t)
		settle()
		if count, _, _ := capabilityProbeTestRow(t, ctx); count != 0 {
			t.Fatalf("switched off: %d capability probe rows, want the chain ended", count)
		}

		// this taskworker reads the probe as on, and brings the chain back
		switchTo("enabled: true\n")
		switchedOnAt := server.NowUtc()
		for {
			count, runAt, args := capabilityProbeTestRow(t, ctx)
			if count == 1 {
				if runAt.Before(switchedOnAt.Add(-time.Second)) || runAt.After(server.NowUtc()) || args.Sweep.Cursor != (server.Id{}) || !args.Sweep.StartedAt.IsZero() || args.MaxTimeSeconds != capprobe.DefaultSettings().MaxTimeSeconds {
					t.Fatalf("the chain returned at %s with %+v, want a pass due at once starting a new sweep", runAt, args)
				}
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("the switched-on taskworker did not schedule the ended chain again")
			case <-time.After(capabilityProbeEnsureInterval):
			}
		}
		if restarts := capabilityProbeTestChainRestarts(t) - restartsBefore; restarts != 1 {
			t.Fatalf("counted %v chain restarts, want 1", restarts)
		}

		// a row that exists keeps its run time and arguments
		later := server.NowUtc().Add(time.Hour).Truncate(time.Microsecond)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `UPDATE pending_task SET run_at = $2 WHERE run_once_key = $1`, task.RunOnce("capability_probe").String(), later))
		})
		settle()
		if count, runAt, _ := capabilityProbeTestRow(t, ctx); count != 1 || !runAt.Equal(later) {
			t.Fatalf("the chain's row is %d rows at %s, want its own at %s", count, runAt, later)
		}
		if restarts := capabilityProbeTestChainRestarts(t) - restartsBefore; restarts != 1 {
			t.Fatalf("counted %v chain restarts with the row present, want 1", restarts)
		}
	})
}
