// The contract degradation check's owner survives taskworkers on different
// config versions. Owners are stamped through their stored JSON form, as the
// task system keeps them.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Owner arguments as a taskworker on version stores them at writtenAt.
func testStampedDegradationArgs(t testing.TB, version string, writtenAt time.Time) *CheckContractDegradationArgs {
	t.Helper()
	args := &CheckContractDegradationArgs{}
	data := fmt.Sprintf(`{"config_generation":%q,"config_written_at":%q}`, version, writtenAt.UTC().Format(time.RFC3339Nano))
	if err := json.Unmarshal([]byte(data), args); err != nil {
		t.Fatal(err)
	}
	return args
}

// Schedules the owner with args at runAt.
func testScheduleDegradationOwner(ctx context.Context, clientSession *session.ClientSession, args *CheckContractDegradationArgs, runAt time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		task.ScheduleTaskInTx(tx, CheckContractDegradation, args, clientSession,
			task.RunOnce("check_contract_degradation"), task.RunAt(runAt), task.MaxTime(5*time.Minute))
	})
}

// The pending owner, or nil when there is none.
func testPendingDegradationOwner(t testing.TB, ctx context.Context) *testProbeRow {
	t.Helper()
	var row *testProbeRow
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT task_id, args_json, run_at FROM pending_task WHERE run_once_key = $1`,
			task.RunOnce("check_contract_degradation").String())
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

// A taskworker whose older config disables the check reaped the check's owner
// at startup, even while a newer config that enables it was running it, and
// nothing armed the check again until a newer taskworker restarted. The reap
// must keep an owner a newer config wrote recently, and still remove one its
// own config wrote.
func TestCheckContractDegradationOlderDisabledConfigKeepsNewerOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testOlderConfigVersion)
		defer server.Config.PushSimpleResource(model.NetworkDegradationResourceName, []byte("enabled: false\n"))()
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		reap := func() (removedCount int64) {
			server.Tx(ctx, func(tx server.PgTx) {
				removedCount = RemoveDisabledContractDegradationTasks(ctx, tx)
			}, server.TxReadCommitted)
			return
		}

		testScheduleDegradationOwner(ctx, clientSession, testStampedDegradationArgs(t, testNewerConfigVersion, server.NowUtc()), server.NowUtc().Add(time.Hour))
		if removedCount := reap(); removedCount != 0 || testPendingDegradationOwner(t, ctx) == nil {
			t.Fatalf("an older disabled config reaped %d owners of a newer config's check", removedCount)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1`, task.RunOnce("check_contract_degradation").String()))
		})
		testScheduleDegradationOwner(ctx, clientSession, testStampedDegradationArgs(t, testOlderConfigVersion, server.NowUtc()), server.NowUtc().Add(time.Hour))
		if removedCount := reap(); removedCount != 1 || testPendingDegradationOwner(t, ctx) != nil {
			t.Fatalf("a disabled config reaped %d of its own owners, want 1", removedCount)
		}
	})
}

// A taskworker whose older config disables the check claimed the owner that a
// newer config's instances run, measured nothing and scheduled no successor,
// which ended the check until a newer taskworker restarted. Through the real
// task evaluator, the older config must hand the owner back unchanged, due
// again shortly, without measuring or publishing.
func TestCheckContractDegradationOlderDisabledConfigHandsBackNewerOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Setenv("WARP_CONFIG_VERSION", testOlderConfigVersion)
		defer server.Config.PushSimpleResource(model.NetworkDegradationResourceName, []byte("enabled: false\n"))()
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		testScheduleDegradationOwner(ctx, clientSession, testStampedDegradationArgs(t, testNewerConfigVersion, server.NowUtc()), server.NowUtc().Add(-time.Minute))
		claimed := testPendingDegradationOwner(t, ctx)

		worker := task.NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(task.NewTaskTargetWithPost(CheckContractDegradation, CheckContractDegradationPost))
		before := server.NowUtc()
		handbacks := testHandbackCount("contract_degradation")
		if finished, retried, postRetried, err := worker.EvalTasks(1); err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
			t.Fatalf("the older config did not finish its claim of the owner: finished=%d retried=%d err=%v", len(finished), len(retried)+len(postRetried), err)
		}
		if counted := testHandbackCount("contract_degradation") - handbacks; counted != 1 {
			t.Errorf("%v hand-backs counted, want 1", counted)
		}
		next := testPendingDegradationOwner(t, ctx)
		if next == nil {
			t.Fatal("an older disabled config ended a newer config's check")
		}
		if next.taskId == claimed.taskId || !reflect.DeepEqual(next.args, claimed.args) {
			t.Fatalf("the older config did not hand the owner back unchanged: %v", next.args)
		}
		if next.runAt.Before(before) || before.Add(time.Minute).Before(next.runAt) {
			t.Fatalf("the handed-back owner is due at %s, want shortly after %s", next.runAt, before)
		}
		if _, present, err := model.GetContractDegradationState(ctx); err != nil || present {
			t.Fatalf("a disabled config published a degradation state: present=%t err=%v", present, err)
		}
	})
}
