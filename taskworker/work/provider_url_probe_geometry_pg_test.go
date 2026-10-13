// Shard rows survive taskworkers on different config versions, and a lost
// shard returns without a taskworker restart. The fixtures build stamped
// arguments through their stored JSON form, as the task system keeps them.
package work

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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

// The stored JSON fields of a snapshot.
func testStoredArgsFields(t testing.TB, args *ProviderEgressProbeArgs) map[string]any {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

// A shard snapshot of settings as its writer stores it, stamped with the
// writer's config version and write time.
func testStampedGeometryArgs(t testing.TB, settings providerEgressProbeSettings, shardIndex int, version string, writtenAt time.Time) *ProviderEgressProbeArgs {
	t.Helper()
	fields := testStoredArgsFields(t, providerEgressProbeArgs(settings, shardIndex))
	fields["geometry_generation"] = version
	fields["geometry_written_at"] = writtenAt.UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(fields)
	if err != nil {
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

// Shards 4-7 vanished from Main and returned only when a taskworker restarted
// and ran its startup schedule. A pass of any configured shard must recreate a
// shard whose row was deleted, from the current settings and due at once,
// while the waiting shards keep their rows, arguments and run times.
func TestProviderEgressProbePassRecreatesDeletedShard(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testNewerConfigVersion)
		settings := testUrlGeometrySettings(8, 64)
		withProviderEgressProbeSettings(t, settings)
		passCount := 0
		driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
			passCount++
			return &ProviderEgressProbeResult{}, nil
		})
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleProviderEgressProbeTasks(clientSession, tx)
		})
		// Shard 0 is due, every other shard waits an hour, and shard 5's row is gone.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at = $2 WHERE function_name = $1`,
				ProviderEgressProbeTaskFunctionNames()[0], server.NowUtc().Add(time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at = $2 WHERE run_once_key = $1`,
				task.RunOnce("provider_egress_probe", 0).String(), server.NowUtc().Add(-time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce("provider_egress_probe", 5).String()))
		})
		waitingShardIndexRows := map[int]*testProbeRow{}
		for _, shardIndex := range []int{1, 2, 3, 4, 6, 7} {
			waitingShardIndexRows[shardIndex] = testPendingProbeRow(t, ctx, shardIndex)
		}

		before := server.NowUtc()
		finishedCount, retriedCount := testEvalProviderEgressProbeTasks(t, ctx, 1)
		after := server.NowUtc()
		if finishedCount != 1 || retriedCount != 0 || passCount != 1 {
			t.Fatalf("shard 0's pass did not complete once: finished=%d retried=%d passes=%d", finishedCount, retriedCount, passCount)
		}
		recreated := testPendingProbeRow(t, ctx, 5)
		if recreated == nil {
			t.Fatal("a pass of shard 0 did not recreate deleted shard 5")
		}
		wantArgs := testStoredArgsFields(t, providerEgressProbeArgs(settings, 5))
		wantArgs["geometry_generation"] = testNewerConfigVersion
		wantArgs["geometry_written_at"] = recreated.args["geometry_written_at"]
		writtenAtText, _ := recreated.args["geometry_written_at"].(string)
		writtenAt, err := time.Parse(time.RFC3339Nano, writtenAtText)
		if err != nil || !reflect.DeepEqual(recreated.args, wantArgs) ||
			writtenAt.Before(before.Add(-time.Second)) || after.Add(time.Second).Before(writtenAt) {
			t.Fatalf("recreated shard 5 does not carry this instance's current geometry: %v", recreated.args)
		}
		if after.Add(time.Second).Before(recreated.runAt) {
			t.Fatalf("recreated shard 5 waits until %s", recreated.runAt)
		}
		for shardIndex, waiting := range waitingShardIndexRows {
			next := testPendingProbeRow(t, ctx, shardIndex)
			if next == nil || next.taskId != waiting.taskId || !next.runAt.Equal(waiting.runAt) || !reflect.DeepEqual(next.args, waiting.args) {
				t.Errorf("recreating shard 5 changed waiting shard %d", shardIndex)
			}
		}
		if count := testPendingProbeRowCount(t, ctx); count != settings.ShardCount {
			t.Errorf("%d probe rows, want %d", count, settings.ShardCount)
		}
	})
}

// A finishing pass deletes its claimed row and inserts its successor in one
// transaction, so it holds its shard's key until it commits. A post-step that
// inserted into that key would wait for the pass, and two finishing passes
// doing so would wait on each other until PostgreSQL aborted one as a
// deadlock. Here a pass of shard 2 has deleted its row and stays open while
// shard 1's post-step runs, which must finish without waiting on it. The lock
// timeout only turns such a wait, which would last until the holder ends after
// the post-step, into a failure.
func TestProviderEgressProbePostDoesNotWaitOnAnotherFinishingShard(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testNewerConfigVersion)
		settings := testUrlGeometrySettings(3, 8)
		withProviderEgressProbeSettings(t, settings)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleProviderEgressProbeTasks(clientSession, tx)
		})

		holder, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		holderTx, err := holder.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
		if err != nil {
			holder.Release()
			t.Fatal(err)
		}
		released := false
		release := func() {
			if !released {
				released = true
				_ = holderTx.Rollback(context.Background())
				holder.Release()
			}
		}
		defer release()
		tag, err := holderTx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1`, task.RunOnce("provider_egress_probe", 2).String())
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("the finishing pass of shard 2 did not hold its row: rows=%d err=%v", tag.RowsAffected(), err)
		}

		failure := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1`, task.RunOnce("provider_egress_probe", 1).String()))
				server.Raise(ProviderEgressProbePost(providerEgressProbeArgs(settings, 1), &ProviderEgressProbeResult{}, clientSession, tx))
			}, server.OptNoRetry())
		})
		release()
		if failure != nil {
			t.Fatalf("shard 1's post-step waited on the finishing pass of shard 2: %v", failure)
		}
		for shardIndex := range settings.ShardCount {
			if testPendingProbeRow(t, ctx, shardIndex) == nil {
				t.Errorf("shard %d has no row", shardIndex)
			}
		}
	})
}

// A shrink to 6 shards retires shards 6 and 7. A taskworker still on the older
// 8-shard config keeps running its own leftover shard 7 until a newer instance
// retires it, and hands the newer shards back. Neither of its post-steps may
// recreate the retired shard 6 while the newer geometry is live.
func TestProviderEgressProbeOlderConfigRecreatesNoRetiredShard(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testOlderConfigVersion)
		older := testUrlGeometrySettings(8, 64)
		withProviderEgressProbeSettings(t, older)
		driftTaskRunner(t, func(_ context.Context, args *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
			if args.ShardIndex != 7 {
				t.Errorf("the older config ran a pass of newer shard %d", args.ShardIndex)
			}
			return &ProviderEgressProbeResult{}, nil
		})
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		newer := testUrlGeometrySettings(6, 96)
		server.Tx(ctx, func(tx server.PgTx) {
			for shardIndex := range newer.ShardCount {
				runAt := server.NowUtc().Add(time.Hour)
				if shardIndex == 2 {
					runAt = server.NowUtc().Add(-time.Minute)
				}
				args := testStampedGeometryArgs(t, newer, shardIndex, testNewerConfigVersion, server.NowUtc())
				scheduleProviderEgressProbeAt(clientSession, tx, args, runAt)
			}
			args := testStampedGeometryArgs(t, older, 7, testOlderConfigVersion, server.NowUtc())
			scheduleProviderEgressProbeAt(clientSession, tx, args, server.NowUtc().Add(-time.Minute))
		})

		if finishedCount, retriedCount := testEvalProviderEgressProbeTasks(t, ctx, 2); finishedCount != 2 || retriedCount != 0 {
			t.Fatalf("the older config finished %d and retried %d of its 2 claims", finishedCount, retriedCount)
		}
		if next := testPendingProbeRow(t, ctx, 6); next != nil {
			t.Fatalf("the older config recreated shard 6, which the newer 6-shard geometry retired, with shard_count=%v", next.args["shard_count"])
		}
		if next := testPendingProbeRow(t, ctx, 7); next == nil || next.args["shard_count"] != float64(8) {
			t.Fatalf("the older config lost its own shard 7: %+v", next)
		}
		if next := testPendingProbeRow(t, ctx, 2); next == nil || next.args["shard_count"] != float64(6) {
			t.Fatalf("the older config did not hand back newer shard 2: %+v", next)
		}
		if count := testPendingProbeRowCount(t, ctx); count != newer.ShardCount+1 {
			t.Fatalf("%d probe rows, want the 6 newer shards and the older shard 7", count)
		}
	})
}

// A taskworker restarting on an older, larger config schedules its whole
// shard set at startup. While a newer geometry is live it must neither
// recreate the shards that geometry retired nor merge into the newer rows.
func TestScheduleProviderEgressProbeTasksLeavesNewerGeometry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testOlderConfigVersion)
		withProviderEgressProbeSettings(t, testUrlGeometrySettings(8, 64))
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		newer := testUrlGeometrySettings(6, 96)
		server.Tx(ctx, func(tx server.PgTx) {
			for shardIndex := range newer.ShardCount {
				args := testStampedGeometryArgs(t, newer, shardIndex, testNewerConfigVersion, server.NowUtc())
				scheduleProviderEgressProbeAt(clientSession, tx, args, server.NowUtc().Add(time.Hour))
			}
		})
		newerShardIndexRows := map[int]*testProbeRow{}
		for shardIndex := range newer.ShardCount {
			newerShardIndexRows[shardIndex] = testPendingProbeRow(t, ctx, shardIndex)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleProviderEgressProbeTasks(clientSession, tx)
		}, server.TxReadCommitted)
		for _, shardIndex := range []int{6, 7} {
			if next := testPendingProbeRow(t, ctx, shardIndex); next != nil {
				t.Errorf("an older config's startup recreated shard %d, which the newer geometry retired", shardIndex)
			}
		}
		for shardIndex, row := range newerShardIndexRows {
			next := testPendingProbeRow(t, ctx, shardIndex)
			if next == nil || !next.runAt.Equal(row.runAt) || !reflect.DeepEqual(next.args, row.args) {
				t.Errorf("an older config's startup changed newer shard %d", shardIndex)
			}
		}
	})
}
