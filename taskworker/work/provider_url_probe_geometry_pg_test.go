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
// task evaluator and the taskworker's registration of the probe. A pass
// outlasts the default task max time, so each evaluation claims it alone, and
// the post-step's work after the commit has run when an evaluation returns.
func testEvalProviderEgressProbeTasks(t testing.TB, ctx context.Context, n int) (finishedCount int, retriedCount int) {
	t.Helper()
	worker := task.NewTaskWorkerWithDefaults(ctx)
	defer worker.Close()
	worker.AddTargets(NewProviderEgressProbeTaskTarget())
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

// Runs the pending pass of one shard and finishes it in the task worker's
// order, through the taskworker's registration of the probe: the pass, a
// finishing transaction that deletes the claimed row and runs the post-step,
// then the post-step's work for after the commit. between runs inside the
// finishing transaction after the deletion, while the transaction holds the
// shard's key. Neither transaction retries, so a deadlock or serialization
// failure is returned rather than repeated. Safe to call from a goroutine.
func testFinishProbePass(ctx context.Context, shardIndex int, between func()) (failure any) {
	return server.HandleError(func() {
		var taskId server.Id
		var argsJson string
		var maxTimeSeconds int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id, args_json, run_max_time_seconds FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce("provider_egress_probe", shardIndex).String()).Scan(&taskId, &argsJson, &maxTimeSeconds))
		})
		_, runPost, err := NewProviderEgressProbeTaskTarget().Run(ctx, &task.Task{
			TaskId:            taskId,
			ArgsJson:          argsJson,
			ClientAddress:     "192.0.2.1:1234",
			RunMaxTimeSeconds: maxTimeSeconds,
		})
		server.Raise(err)
		var posts []server.PostFunction
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE task_id = $1`, taskId))
			if between != nil {
				between()
			}
			var postErr error
			posts, postErr = runPost(tx)
			server.Raise(postErr)
		}, server.OptNoRetry())
		server.RunPosts(ctx, posts...)
	})
}

// Returns once PostgreSQL reports the backend waiting on a lock. The barrier is
// the server's own report of the wait, not a sleep.
func testAwaitLockWait(t testing.TB, ctx context.Context, pid int32) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		waiting := false
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx,
				`SELECT coalesce(bool_or(wait_event_type = 'Lock'), false) FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&waiting))
		})
		if waiting {
			return
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("backend %d never waited on a lock", pid)
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

// The outcome of a background step, or a failure when it does not end.
func testAwaitOutcome(t testing.TB, done chan any, name string) any {
	t.Helper()
	select {
	case failure := <-done:
		return failure
	case <-time.After(time.Minute):
		t.Fatalf("%s neither finished nor failed", name)
		return nil
	}
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

// A newer build can add argument fields that an older build does not decode.
// An older instance's hand-back encoded the decoded arguments again and
// dropped such a field, so the newer instances got a different snapshot back.
// Through the real task evaluator, the handed-back shard must carry its stored
// arguments as they were, including a field this build does not know.
func TestProviderEgressProbeHandBackKeepsUnknownArgumentFields(t *testing.T) {
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
		runOnceKey := task.RunOnce("provider_egress_probe", 6).String()
		server.Tx(ctx, func(tx server.PgTx) {
			args := testStampedGeometryArgs(t, testUrlGeometrySettings(8, 64), 6, testNewerConfigVersion, server.NowUtc())
			scheduleProviderEgressProbeAt(clientSession, tx, args, server.NowUtc().Add(-time.Minute))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task
				SET args_json = (args_json::jsonb || '{"future_pass_setting": {"lanes": 3}}'::jsonb)::text
				WHERE run_once_key = $1`, runOnceKey))
		})
		claimed := testPendingProbeRow(t, ctx, 6)
		if _, ok := claimed.args["future_pass_setting"]; !ok {
			t.Fatal("the fixture did not store the newer build's field")
		}

		if finishedCount, retriedCount := testEvalProviderEgressProbeTasks(t, ctx, 1); finishedCount != 1 || retriedCount != 0 {
			t.Fatalf("the older config finished %d and retried %d claims of shard 6", finishedCount, retriedCount)
		}
		next := testPendingProbeRow(t, ctx, 6)
		if next == nil || next.taskId == claimed.taskId {
			t.Fatalf("the older config did not hand shard 6 back: %+v", next)
		}
		if !reflect.DeepEqual(next.args, claimed.args) {
			t.Fatalf("the hand-back changed the stored arguments: future_pass_setting=%v", next.args["future_pass_setting"])
		}
	})
}

// While every pass slot of the newer config's instances is busy, a shard waits
// for one of them as older instances hand it back without restamping it, so
// its own stamp ages past any fixed timeout while the newer config restamps
// its other shards. The older config took the aged stamp for a withdrawn
// geometry and retired the shard. Here newer shard 6 was stamped 45 minutes
// ago and newer shard 1 a minute ago, so the older config must hand shard 6
// back. Once no shard of the newer config is live, its geometry counts as
// withdrawn and the older config retires shard 6.
func TestProviderEgressProbeOlderConfigHandsBackWhileAnyNewerShardIsLive(t *testing.T) {
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
		newer := testUrlGeometrySettings(8, 64)
		server.Tx(ctx, func(tx server.PgTx) {
			live := testStampedGeometryArgs(t, newer, 1, testNewerConfigVersion, server.NowUtc().Add(-time.Minute))
			scheduleProviderEgressProbeAt(clientSession, tx, live, server.NowUtc().Add(time.Hour))
			aged := testStampedGeometryArgs(t, newer, 6, testNewerConfigVersion, server.NowUtc().Add(-45*time.Minute))
			scheduleProviderEgressProbeAt(clientSession, tx, aged, server.NowUtc().Add(-time.Minute))
		})
		claimed := testPendingProbeRow(t, ctx, 6)

		if finishedCount, retriedCount := testEvalProviderEgressProbeTasks(t, ctx, 1); finishedCount != 1 || retriedCount != 0 {
			t.Fatalf("the older config finished %d and retried %d claims of shard 6", finishedCount, retriedCount)
		}
		next := testPendingProbeRow(t, ctx, 6)
		if next == nil {
			t.Fatal("the older config retired newer shard 6 while newer shard 1 shows its geometry live")
		}
		if next.taskId == claimed.taskId || !reflect.DeepEqual(next.args, claimed.args) {
			t.Fatalf("the older config did not hand shard 6 back unchanged: shard_count=%v", next.args["shard_count"])
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce("provider_egress_probe", 1).String()))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at = $2 WHERE run_once_key = $1`,
				task.RunOnce("provider_egress_probe", 6).String(), server.NowUtc().Add(-time.Minute)))
		})
		if finishedCount, retriedCount := testEvalProviderEgressProbeTasks(t, ctx, 1); finishedCount != 1 || retriedCount != 0 {
			t.Fatalf("the older config finished %d and retried %d claims of shard 6", finishedCount, retriedCount)
		}
		if next := testPendingProbeRow(t, ctx, 6); next != nil {
			t.Fatalf("the older config kept shard 6 of a withdrawn newer geometry, shard_count=%v", next.args["shard_count"])
		}
	})
}

// The newest config still converges older snapshots, through the real task
// evaluator. Shard 2's own post-step must replace its claimed row with one of
// the newest geometry, and a shard that geometry removed is retired. The
// recreation after each pass creates the newest geometry's missing shards,
// but never the claimed shard's successor: that is a new row only its own
// post-step writes, since a pending row keeps its arguments under a merge.
func TestProviderEgressProbeNewerConfigConvergesOlderShards(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testNewerConfigVersion)
		newest := testUrlGeometrySettings(6, 96)
		withProviderEgressProbeSettings(t, newest)
		driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
			t.Error("a stale older geometry ran a pass")
			return &ProviderEgressProbeResult{}, nil
		})
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		older := testUrlGeometrySettings(8, 64)
		server.Tx(ctx, func(tx server.PgTx) {
			due := testStampedGeometryArgs(t, older, 2, testOlderConfigVersion, server.NowUtc())
			scheduleProviderEgressProbeAt(clientSession, tx, due, server.NowUtc().Add(-time.Minute))
			removed := testStampedGeometryArgs(t, older, 7, testOlderConfigVersion, server.NowUtc())
			scheduleProviderEgressProbeAt(clientSession, tx, removed, server.NowUtc().Add(time.Hour))
		})
		claimed := testPendingProbeRow(t, ctx, 2)

		before := server.NowUtc()
		if finishedCount, retriedCount := testEvalProviderEgressProbeTasks(t, ctx, 1); finishedCount != 1 || retriedCount != 0 {
			t.Fatalf("the newer config finished %d and retried %d claims of shard 2", finishedCount, retriedCount)
		}
		after := server.NowUtc()
		next := testPendingProbeRow(t, ctx, 2)
		if next == nil || next.taskId == claimed.taskId {
			t.Fatalf("shard 2's post-step did not replace its claimed row: %+v", next)
		}
		wantArgs := testStoredArgsFields(t, providerEgressProbeArgs(newest, 2))
		wantArgs["geometry_generation"] = testNewerConfigVersion
		wantArgs["geometry_written_at"] = next.args["geometry_written_at"]
		writtenAtText, _ := next.args["geometry_written_at"].(string)
		writtenAt, err := time.Parse(time.RFC3339Nano, writtenAtText)
		if err != nil || !reflect.DeepEqual(next.args, wantArgs) ||
			writtenAt.Before(before.Add(-time.Second)) || after.Add(time.Second).Before(writtenAt) {
			t.Fatalf("shard 2's post-step did not converge it to the newest geometry: shard_count=%v url_probe=%v generation=%v",
				next.args["shard_count"], next.args["url_probe"], next.args["geometry_generation"])
		}

		// Only shard 7 is due; the recreation made the newest geometry's other shards due at once.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at = $2 WHERE function_name = $1`,
				ProviderEgressProbeTaskFunctionNames()[0], server.NowUtc().Add(time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at = $2 WHERE run_once_key = $1`,
				task.RunOnce("provider_egress_probe", 7).String(), server.NowUtc().Add(-time.Minute)))
		})
		if finishedCount, retriedCount := testEvalProviderEgressProbeTasks(t, ctx, 1); finishedCount != 1 || retriedCount != 0 {
			t.Fatalf("the newer config finished %d and retried %d claims of shard 7", finishedCount, retriedCount)
		}
		if next := testPendingProbeRow(t, ctx, 7); next != nil {
			t.Fatalf("the newer 6-shard config kept removed shard 7 with shard_count=%v", next.args["shard_count"])
		}
		if count := testPendingProbeRowCount(t, ctx); count != newest.ShardCount {
			t.Fatalf("%d probe rows, want the newest geometry's %d", count, newest.ShardCount)
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

// The startup schedule upserts every shard's key in shard order, and a
// finishing pass holds its own shard's key until it commits. A post-step that
// inserted a missing shard inside its finishing transaction waited for a
// startup that had already inserted that shard, while the startup waited for
// the pass's key, and PostgreSQL aborted one of them as a deadlock. Here the
// startup inserts missing shard 1 and waits on the finishing pass of shard 2
// before that pass runs its post-step. Both must commit.
func TestProviderEgressProbeRecreationDoesNotDeadlockWithStartup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testNewerConfigVersion)
		withProviderEgressProbeSettings(t, testUrlGeometrySettings(3, 8))
		driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
			return &ProviderEgressProbeResult{}, nil
		})
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleProviderEgressProbeTasks(clientSession, tx)
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce("provider_egress_probe", 1).String()))
		})

		passHolding := make(chan struct{})
		startupWaiting := make(chan struct{})
		released := false
		release := func() {
			if !released {
				released = true
				close(startupWaiting)
			}
		}
		defer release()
		passDone := make(chan any, 1)
		go func() {
			passDone <- testFinishProbePass(ctx, 2, func() {
				close(passHolding)
				<-startupWaiting
			})
		}()
		<-passHolding
		startupPids := make(chan int32, 1)
		startupDone := make(chan any, 1)
		go func() {
			startupDone <- server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					var pid int32
					server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
					startupPids <- pid
					ScheduleProviderEgressProbeTasks(clientSession, tx)
				}, server.TxReadCommitted, server.OptNoRetry())
			})
		}()
		testAwaitLockWait(t, ctx, <-startupPids)
		release()

		if failure := testAwaitOutcome(t, passDone, "the finishing pass of shard 2"); failure != nil {
			t.Errorf("the finishing pass of shard 2 failed beside the startup schedule: %v", failure)
		}
		if failure := testAwaitOutcome(t, startupDone, "the startup schedule"); failure != nil {
			t.Errorf("the startup schedule failed beside the finishing pass of shard 2: %v", failure)
		}
		for shardIndex := range 3 {
			if testPendingProbeRow(t, ctx, shardIndex) == nil {
				t.Errorf("shard %d has no row", shardIndex)
			}
		}
	})
}

// Two passes finishing together both found shard 1 missing. At repeatable
// read, the later post-step's insert met the row the earlier one had committed
// after the later transaction's snapshot, and failed to serialize. Here the
// pass of shard 2 takes its snapshot, holding its key, before the pass of
// shard 0 finishes, and runs its post-step after. The pass of shard 0 must
// recreate shard 1 without waiting on the held key, both must commit, and
// shard 1 is recreated once.
func TestProviderEgressProbeRecreationByTwoPassesDoesNotFailToSerialize(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testNewerConfigVersion)
		withProviderEgressProbeSettings(t, testUrlGeometrySettings(3, 8))
		driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
			return &ProviderEgressProbeResult{}, nil
		})
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleProviderEgressProbeTasks(clientSession, tx)
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce("provider_egress_probe", 1).String()))
		})

		secondHolding := make(chan struct{})
		firstFinished := make(chan struct{})
		released := false
		release := func() {
			if !released {
				released = true
				close(firstFinished)
			}
		}
		defer release()
		secondDone := make(chan any, 1)
		go func() {
			secondDone <- testFinishProbePass(ctx, 2, func() {
				close(secondHolding)
				<-firstFinished
			})
		}()
		<-secondHolding
		firstDone := make(chan any, 1)
		go func() {
			firstDone <- testFinishProbePass(ctx, 0, nil)
		}()
		if failure := testAwaitOutcome(t, firstDone, "the pass of shard 0 beside the held key of shard 2"); failure != nil {
			t.Errorf("the pass of shard 0 failed: %v", failure)
		}
		if testPendingProbeRow(t, ctx, 1) == nil {
			t.Errorf("the pass of shard 0 did not recreate shard 1")
		}
		release()
		if failure := testAwaitOutcome(t, secondDone, "the pass of shard 2"); failure != nil {
			t.Errorf("the pass of shard 2 failed after shard 1 was recreated: %v", failure)
		}
		if count := testPendingProbeRowCount(t, ctx); count != 3 {
			t.Errorf("%d probe rows, want one for each of 3 shards", count)
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
