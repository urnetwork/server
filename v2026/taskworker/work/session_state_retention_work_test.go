package work

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// DB-backed: Post hands both passes' cursor on, after the settings'
// MoreInterval while work remains and after RunInterval otherwise, and the
// startup schedule starts the chain after StartDelay.
func TestRemoveExpiredSessionStatePostKeepsTheCursor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		settings := model.DefaultSessionStateRetentionSettings()
		const runOnceKey = `["remove_expired_session_state"]`

		headAfter, deviceStateAfter := server.NewId(), server.NewId()
		passStart := server.NowUtc().Add(-time.Hour)
		cursor := model.SessionStateRetentionCursor{
			HeadAfterClientId:        &headAfter,
			HeadPassStartTime:        &passStart,
			DeviceStateAfterDeviceId: &deviceStateAfter,
			DeviceStatePassStartTime: &passStart,
			DeviceStatePassComplete:  false,
		}
		for _, more := range []bool{true, false} {
			before := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				err := RemoveExpiredSessionStatePost(
					&RemoveExpiredSessionStateArgs{},
					&RemoveExpiredSessionStateResult{Cursor: cursor, More: more},
					clientSession,
					tx,
				)
				if err != nil {
					t.Fatal(err)
				}
				var args RemoveExpiredSessionStateArgs
				runAt := readIdentityRetentionPending(ctx, t, tx, runOnceKey, &args)
				if args.Cursor.HeadAfterClientId == nil || *args.Cursor.HeadAfterClientId != headAfter ||
					args.Cursor.DeviceStateAfterDeviceId == nil || *args.Cursor.DeviceStateAfterDeviceId != deviceStateAfter ||
					args.Cursor.HeadPassStartTime == nil || !args.Cursor.HeadPassStartTime.Equal(passStart) {
					t.Fatalf("Post scheduled cursor %+v, want %+v", args.Cursor, cursor)
				}
				wantDelay := settings.RunInterval
				if more {
					wantDelay = settings.MoreInterval
				}
				if delay := runAt.Sub(before); delay < wantDelay-10*time.Second || wantDelay+10*time.Second < delay {
					t.Fatalf("Post with more=%t scheduled %v out, want %v", more, delay, wantDelay)
				}
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1`, runOnceKey))
			})
		}

		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleRemoveExpiredSessionState(clientSession, tx)
			var args RemoveExpiredSessionStateArgs
			runAt := readIdentityRetentionPending(ctx, t, tx, runOnceKey, &args)
			if args.Cursor.HeadAfterClientId != nil || args.Cursor.DeviceStateAfterDeviceId != nil {
				t.Fatalf("the startup schedule carried cursor %+v", args.Cursor)
			}
			if delay := runAt.Sub(before); delay < settings.StartDelay-10*time.Second || settings.StartDelay+10*time.Second < delay {
				t.Fatalf("the startup schedule runs %v out, want %v", delay, settings.StartDelay)
			}
		})
	})
}
