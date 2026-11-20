package taskworker

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/taskworker/work"
)

// §21's mail pass (ABUSE-M9-01): registered with the worker and armed once by InitTasks in a binary that links MG5, so
// SMTP's halts reach §21 as cases and actions and the ladder's suspension request has a chain to kick. A chain armed
// again at a start keeps the cursor its last pass left, and a pass's post carries its cursor to the next run.
func TestInitTasksArmsTheAbuseMailPass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		taskWorker := InitTaskWorker(ctx)
		if functionName := "github.com/urnetwork/server/taskworker/work.AbuseMailPass"; !taskWorker.HasTarget(functionName) {
			t.Fatalf("%s is not a registered task target", functionName)
		}

		want := 0
		if server.SidecarMg5Linked() {
			want = 1
		}
		runOnceKey := fmt.Sprintf("[%q]", model.AbuseMailPassRunOnceKey)
		chain := func() (count int, args *work.AbuseMailPassArgs) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `SELECT args_json FROM pending_task WHERE run_once_key = $1`, runOnceKey)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var argsJson string
						server.Raise(result.Scan(&argsJson))
						args = &work.AbuseMailPassArgs{}
						server.Raise(json.Unmarshal([]byte(argsJson), args))
						count += 1
					}
				})
			})
			return
		}
		for range 2 {
			InitTasks(ctx)
			if count, _ := chain(); count != want {
				t.Fatalf("the mail pass is armed %d times, want %d", count, want)
			}
		}
		if want == 0 {
			return
		}

		// a pass's post carries its cursor on, and a later start keeps it
		cursor := model.AbuseSmtpComplaintCursor{ArrivalTime: time.Date(2026, time.November, 17, 10, 0, 0, 0, time.UTC), ComplaintId: server.NewId()}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1`, runOnceKey))
			clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
			server.Raise(work.AbuseMailPassPost(&work.AbuseMailPassArgs{}, &work.AbuseMailPassResult{Next: cursor}, clientSession, tx))
		})
		InitTasks(ctx)
		if count, args := chain(); count != 1 || !args.ComplaintCursor.ArrivalTime.Equal(cursor.ArrivalTime) || args.ComplaintCursor.ComplaintId != cursor.ComplaintId {
			t.Fatalf("after a post and a start: %d chains, %+v", count, args)
		}
	})
}
