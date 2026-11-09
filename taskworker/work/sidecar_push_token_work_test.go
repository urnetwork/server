// §18.7's token pass on a private database and the test stack's Redis (SDK-M8-11): queued once, and only
// in a binary that links MG8; each run's state carried in the next run's args; and a run with the wake
// switch off deleting only rows past their 24 h.
package work

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The queued runs of the chain, with their args.
func pushTokenPassPending(t testing.TB, ctx context.Context) []*PushTokenPassArgs {
	t.Helper()
	pending := []*PushTokenPassArgs{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT args_json FROM pending_task WHERE run_once_key = $1`,
			task.RunOnce("push_token_pass").String(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var argsJson string
				server.Raise(result.Scan(&argsJson))
				args := &PushTokenPassArgs{}
				server.Raise(json.Unmarshal([]byte(argsJson), args))
				pending = append(pending, args)
			}
		})
	})
	return pending
}

// The chain is queued once, however often the schedule runs, and not at all in a binary that does not
// link MG8, where a run does nothing.
func TestPushTokenPassScheduledOnlyWithMg8(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				SchedulePushTokenPass(clientSession, tx)
			})
		}
		want := 0
		if server.SidecarMg8Linked() {
			want = 1
		}
		if pending := pushTokenPassPending(t, ctx); len(pending) != want {
			t.Fatalf("%d runs queued, want %d", len(pending), want)
		}
		if !server.SidecarMg8Linked() {
			result, err := PushTokenPass(&PushTokenPassArgs{}, clientSession)
			if err != nil || result.Report.More || result.Report.FoundSlotCount != 0 {
				t.Fatalf("a run without MG8: %+v, %v", result.Report, err)
			}
		}
	})
}

// A run with the wake switch off finds no slot and sweeps nothing, deletes a row past its 24 h, and its
// post queues the next run with its state.
func TestPushTokenPassCarriesItsState(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg8Linked() {
			return
		}
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		defer server.Testing_PushSidecarConfig("sidecar:\n  wake: {enabled: false}\n")()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "pushtokenwork"+networkId.String()[:8], server.NewId())
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		if _, err := model.SetPushToken(ctx, clientId, sidecar.PushTokenPlatformApns, "work-token", nil); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := model.TombstonePushTokenInTx(ctx, tx, []server.Id{clientId})
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE sidecar_push_token SET removed_time = $2 WHERE client_id = $1`, clientId, server.NowUtc().Add(-25*time.Hour)))
		})

		lastStart := server.NowUtc().Add(-time.Minute)
		args := &PushTokenPassArgs{State: &model.PushTokenPassState{LastCompletedStartTime: lastStart}}
		result, err := PushTokenPass(args, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if result.Report.DeletedRowCount != 1 || result.Report.FoundSlotCount != 0 || result.Report.SweptRowCount != 0 {
			t.Fatalf("report %+v", result.Report)
		}
		if row, err := model.GetPushToken(ctx, clientId); err != nil || row != nil {
			t.Fatalf("the row stayed: %v %v", row, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(PushTokenPassPost(args, result, clientSession, tx))
		})
		pending := pushTokenPassPending(t, ctx)
		if len(pending) != 1 || pending[0].State == nil || !pending[0].State.LastCompletedStartTime.Equal(lastStart) {
			t.Fatalf("queued %+v", pending)
		}
	})
}
