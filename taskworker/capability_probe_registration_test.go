// The serving taskworker registers and schedules the capability probe; a
// subnet operator leaves it alone, as it does every external probe.
package taskworker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/capprobe"
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

// Through the production worker: a switched-off probe runs nothing, opens no
// prober shard, and its post-step rechecks in ten minutes; an enabled probe
// over a population without providers completes its sweep from the database
// alone, opens no prober shard, sets the next sweep's start the sweep
// interval after this one's, and runs a waiting pass ten minutes on. Every
// row carries the max time it was scheduled with.
func TestProductionCapabilityProbePassesWithoutProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		// the URL probe's endpoints and a shard geometry clear of slot 255
		withProbePlacementGeometry(t, 4, 8, 0)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		shards := func() int {
			var count int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM prober_shard_run WHERE shard_index = 255`).Scan(&count))
			})
			return count
		}

		maxTimeSeconds := capprobe.DefaultSettings().MaxTimeSeconds
		sweepInterval := time.Duration(capprobe.DefaultSettings().MinSweepIntervalMinutes) * time.Minute
		for _, c := range []struct {
			name      string
			resource  string
			nextSweep bool
		}{
			{name: "switched off", resource: "enabled: false\n", nextSweep: false},
			{name: "enabled", resource: "enabled: true\n", nextSweep: true},
		} {
			pop := server.Config.PushSimpleResource(capprobe.SettingsResourceName, []byte(c.resource))
			server.Tx(ctx, func(tx server.PgTx) {
				work.ScheduleCapabilityProbe(clientSession, tx)
			})
			if count, _, args := capabilityProbeTestRow(t, ctx); count != 1 || args.MaxTimeSeconds != maxTimeSeconds {
				t.Fatalf("%s: scheduled %d rows with %+v, want one carrying a %d s max time", c.name, count, args, maxTimeSeconds)
			}
			// a row becomes claimable a second past its run time (its
			// available_block), and the row runs at once
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `UPDATE pending_task SET run_at = run_at - interval '1 minute' WHERE run_once_key = $1`, task.RunOnce("capability_probe").String()))
			})
			worker := InitTaskWorker(ctx)
			evaluatedAt := server.NowUtc()
			finished, retried, postRetried, err := worker.EvalTasks(1)
			worker.Close()
			pop()
			if err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
				t.Fatalf("%s: the production worker did not finish the pass: finished=%d retried=%d err=%v", c.name, len(finished), len(retried)+len(postRetried), err)
			}
			// a switched-off probe's recheck, or a waiting pass before the next
			// sweep's start, both ten minutes on
			count, runAt, args := capabilityProbeTestRow(t, ctx)
			if count != 1 || runAt.Before(evaluatedAt.Add(9*time.Minute)) || runAt.After(server.NowUtc().Add(11*time.Minute)) || args.MaxTimeSeconds != maxTimeSeconds {
				t.Fatalf("%s: %d successor rows at %s with %+v, want one about ten minutes out", c.name, count, runAt, args)
			}
			nextSweepAt := args.Sweep.NextSweepAt
			if c.nextSweep != !nextSweepAt.IsZero() || c.nextSweep && (nextSweepAt.Before(evaluatedAt.Add(sweepInterval)) || nextSweepAt.After(server.NowUtc().Add(sweepInterval))) {
				t.Fatalf("%s: the successor's next sweep starts at %s, want the sweep interval after this pass: %t", c.name, nextSweepAt, c.nextSweep)
			}
			if shards() != 0 {
				t.Fatalf("%s: a pass with nothing to measure opened a prober shard", c.name)
			}
			// the successor runs now, for the next case
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1`, task.RunOnce("capability_probe").String()))
			})
		}
	})
}
