// The serving taskworker registers and schedules the capability probe; a
// subnet operator leaves it alone, as it does every external probe.
package taskworker

import (
	"context"
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

// The pending row of the probe's one run-once key, and its run time.
func capabilityProbeTestRow(t testing.TB, ctx context.Context) (count int, runAt time.Time) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx,
			`SELECT count(*), COALESCE(max(run_at), 'epoch'::timestamp) FROM pending_task WHERE run_once_key = $1`,
			task.RunOnce("capability_probe").String(),
		).Scan(&count, &runAt))
	})
	return
}

// Through the production worker: a switched-off probe runs nothing, opens no
// prober shard, and its post-step rechecks in ten minutes; an enabled probe
// over a population without providers completes its sweep from the database
// alone, opens no prober shard, and schedules the next sweep's start.
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

		for _, c := range []struct {
			name     string
			resource string
			next     time.Duration
		}{
			{name: "switched off", resource: "enabled: false\n", next: 10 * time.Minute},
			{name: "enabled", resource: "enabled: true\n", next: time.Duration(capprobe.DefaultSettings().MinSweepIntervalMinutes) * time.Minute},
		} {
			pop := server.Config.PushSimpleResource(capprobe.SettingsResourceName, []byte(c.resource))
			server.Tx(ctx, func(tx server.PgTx) {
				work.ScheduleCapabilityProbe(clientSession, tx)
			})
			worker := InitTaskWorker(ctx)
			finishedAt := server.NowUtc()
			finished, retried, postRetried, err := worker.EvalTasks(1)
			worker.Close()
			pop()
			if err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
				t.Fatalf("%s: the production worker did not finish the pass: finished=%d retried=%d err=%v", c.name, len(finished), len(retried)+len(postRetried), err)
			}
			count, runAt := capabilityProbeTestRow(t, ctx)
			if count != 1 || runAt.Before(finishedAt.Add(c.next-time.Minute)) || runAt.After(server.NowUtc().Add(c.next+time.Minute)) {
				t.Fatalf("%s: %d successor rows at %s, want one about %s out", c.name, count, runAt, c.next)
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
