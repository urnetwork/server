// Shard rows survive taskworkers on different config versions. The fixtures
// build stamped arguments through their stored JSON form, as the task system
// keeps them.
package work

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Synthetic config versions in warp's shape, older first.
const (
	testOlderConfigVersion = "2026.10.12-synthetic+1000000100"
	testNewerConfigVersion = "2026.10.13-synthetic+1000000200"
)

// URL settings with one geometry.
func testUrlGeometrySettings(shardCount int, concurrency int) providerEgressProbeSettings {
	settings := defaultProviderEgressProbeSettings("example.test")
	settings.ShardCount = shardCount
	settings.UrlProbe.Limit, settings.UrlProbe.Concurrency = concurrency, concurrency
	return settings
}

// A shard snapshot of settings as its writer stores it, stamped with the
// writer's config version and write time.
func testStampedGeometryArgs(t testing.TB, settings providerEgressProbeSettings, shardIndex int, version string, writtenAt time.Time) *ProviderEgressProbeArgs {
	t.Helper()
	data, err := json.Marshal(providerEgressProbeArgs(settings, shardIndex))
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["geometry_generation"] = version
	fields["geometry_written_at"] = writtenAt.UTC().Format(time.RFC3339Nano)
	if data, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	args := &ProviderEgressProbeArgs{}
	if err := json.Unmarshal(data, args); err != nil {
		t.Fatal(err)
	}
	return args
}

// One shard's pending row, with its arguments as stored.
type testProbeRow struct {
	taskId server.Id
	args   map[string]any
	runAt  time.Time
}

// The pending row of one shard, or nil when the shard has no row.
func testPendingProbeRow(t testing.TB, ctx context.Context, shardIndex int) *testProbeRow {
	t.Helper()
	var row *testProbeRow
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT task_id, args_json, run_at FROM pending_task WHERE run_once_key = $1`,
			task.RunOnce("provider_egress_probe", shardIndex).String())
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

// Every pending probe row.
func testPendingProbeRowCount(t testing.TB, ctx context.Context) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name = $1`,
			ProviderEgressProbeTaskFunctionNames()[0]).Scan(&count))
	})
	return
}

// Claims and runs up to n due passes with their post-steps through the real
// task evaluator, as a taskworker does. A pass outlasts the default task max
// time, so each evaluation claims it alone.
func testEvalProviderEgressProbeTasks(t testing.TB, ctx context.Context, n int) (finishedCount int, retriedCount int) {
	t.Helper()
	worker := task.NewTaskWorkerWithDefaults(ctx)
	defer worker.Close()
	worker.AddTargets(task.NewTaskTargetWithPost(ProviderEgressProbe, ProviderEgressProbePost))
	for range n {
		finished, retried, postRetried, err := worker.EvalTasks(1)
		if err != nil {
			t.Fatalf("task evaluation failed: %v", err)
		}
		if len(finished)+len(retried)+len(postRetried) == 0 {
			break
		}
		finishedCount += len(finished)
		retriedCount += len(retried) + len(postRetried)
	}
	return
}

// Runs one claimed pass and its post-step as the task system does.
func testRunProbeWithPost(t testing.TB, ctx context.Context, clientSession *session.ClientSession, args *ProviderEgressProbeArgs) {
	t.Helper()
	result, err := ProviderEgressProbe(args, clientSession)
	if err != nil {
		t.Fatalf("shard %d pass failed: %v", args.ShardIndex, err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		if err := ProviderEgressProbePost(args, result, clientSession, tx); err != nil {
			t.Fatalf("shard %d post failed: %v", args.ShardIndex, err)
		}
	})
}

// A taskworker that rejoined a rollout on the previous day's config, 4 shards
// of 8 workers, while its peers ran 8 shards of 64, retired every shard at or
// above 4 that it claimed and rewrote every lower shard to its own geometry.
// Half of the shards disappeared until a taskworker restarted. Through the
// real task evaluator, the older config claims the newer shard 6 and shard 1
// and must reschedule each with its stored arguments unchanged, without
// running a pass.
func TestProviderEgressProbeOlderConfigClaimKeepsNewerShards(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testOlderConfigVersion)
		withProviderEgressProbeSettings(t, testUrlGeometrySettings(4, 8))
		driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
			t.Error("an older config ran a pass of a newer geometry")
			return &ProviderEgressProbeResult{}, nil
		})
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		shardIndexes := []int{6, 1}
		newer := testUrlGeometrySettings(8, 64)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, shardIndex := range shardIndexes {
				args := testStampedGeometryArgs(t, newer, shardIndex, testNewerConfigVersion, server.NowUtc())
				scheduleProviderEgressProbeAt(clientSession, tx, args, server.NowUtc().Add(-time.Minute))
			}
		})
		claimedShardIndexRows := map[int]*testProbeRow{}
		for _, shardIndex := range shardIndexes {
			claimedShardIndexRows[shardIndex] = testPendingProbeRow(t, ctx, shardIndex)
		}

		finishedCount, retriedCount := testEvalProviderEgressProbeTasks(t, ctx, len(shardIndexes))
		if finishedCount != len(shardIndexes) || retriedCount != 0 {
			t.Fatalf("the older config finished %d and retried %d of %d claimed shards", finishedCount, retriedCount, len(shardIndexes))
		}
		for _, shardIndex := range shardIndexes {
			next := testPendingProbeRow(t, ctx, shardIndex)
			switch {
			case next == nil:
				t.Errorf("the older config deleted shard %d of the newer 8-shard geometry", shardIndex)
			case next.taskId == claimedShardIndexRows[shardIndex].taskId:
				t.Errorf("shard %d was not claimed", shardIndex)
			case !reflect.DeepEqual(next.args, claimedShardIndexRows[shardIndex].args):
				t.Errorf("the older config rewrote shard %d to shard_count=%v url_probe=%v", shardIndex, next.args["shard_count"], next.args["url_probe"])
			}
		}
		if count := testPendingProbeRowCount(t, ctx); count != len(shardIndexes) {
			t.Errorf("%d probe rows after the claims, want the %d newer shards", count, len(shardIndexes))
		}
	})
}

// The newest config still converges older snapshots: an in-range shard takes
// its geometry, and a shard it removed is retired.
func TestProviderEgressProbeNewerConfigConvergesOlderShards(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testNewerConfigVersion)
		withProviderEgressProbeSettings(t, testUrlGeometrySettings(6, 96))
		driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
			t.Error("a stale older geometry ran a pass")
			return &ProviderEgressProbeResult{}, nil
		})
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		older := testUrlGeometrySettings(8, 64)
		testRunProbeWithPost(t, ctx, clientSession, testStampedGeometryArgs(t, older, 7, testOlderConfigVersion, server.NowUtc()))
		if next := testPendingProbeRow(t, ctx, 7); next != nil {
			t.Fatalf("the newer 6-shard config kept removed shard 7 with shard_count=%v", next.args["shard_count"])
		}
		testRunProbeWithPost(t, ctx, clientSession, testStampedGeometryArgs(t, older, 2, testOlderConfigVersion, server.NowUtc()))
		next := testPendingProbeRow(t, ctx, 2)
		if next == nil || next.args["shard_count"] != float64(6) || !reflect.DeepEqual(next.args["url_probe"], map[string]any{
			"limit": float64(96), "concurrency": float64(96), "probe_timeout_seconds": float64(60),
		}) {
			t.Fatalf("the newer config did not converge shard 2: %+v", next)
		}
	})
}

// A taskworker whose older config disables probing reaps the whole probe
// family when it starts. It must leave a newer config's live shards, which no
// post-step could recreate once every row is gone, and still remove the rows
// its own config wrote.
func TestProviderEgressProbeOlderDisabledConfigKeepsNewerShards(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testOlderConfigVersion)
		disabled := testUrlGeometrySettings(4, 8)
		disabled.Enabled = false
		withProviderEgressProbeSettings(t, disabled)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		schedule := func(settings providerEgressProbeSettings, version string) {
			server.Tx(ctx, func(tx server.PgTx) {
				for shardIndex := range settings.ShardCount {
					args := testStampedGeometryArgs(t, settings, shardIndex, version, server.NowUtc())
					scheduleProviderEgressProbeAt(clientSession, tx, args, server.NowUtc())
				}
			})
		}
		reap := func() (removedCount int64) {
			server.Tx(ctx, func(tx server.PgTx) {
				removedCount = RemoveDisabledProviderEgressProbeTasks(ctx, tx)
			}, server.TxReadCommitted)
			return
		}

		schedule(testUrlGeometrySettings(8, 64), testNewerConfigVersion)
		if removedCount := reap(); removedCount != 0 || testPendingProbeRowCount(t, ctx) != 8 {
			t.Fatalf("an older disabled config reaped %d rows of a newer live geometry", removedCount)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE function_name = $1`, ProviderEgressProbeTaskFunctionNames()[0]))
		})
		schedule(testUrlGeometrySettings(4, 8), testOlderConfigVersion)
		if removedCount := reap(); removedCount != 4 || testPendingProbeRowCount(t, ctx) != 0 {
			t.Fatalf("a disabled config reaped %d of its own 4 rows", removedCount)
		}
	})
}
