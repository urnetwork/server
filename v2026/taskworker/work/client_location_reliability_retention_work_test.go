// The client location reliability retention as its taskworker chain runs it
// (model.RemoveOldClientLocationReliabilities): a run deletes the rows of
// children and gone clients 11 days after their last refresh and keeps a
// top-level client's to 31 days, and its post hands the walk's cursor to the
// next run.
package work

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// DB-backed: one run of the task, as the chain claims it with no cursor,
// removes a hosted child's and a gone client's rows refreshed 12 days ago,
// and keeps a top-level client's refreshed 20 days ago, which the provider
// status page still reads, and a child's refreshed 10 days ago. It reads only
// the task's public entry point, so it runs unchanged against the 31-day
// retention before, where the 12-day rows stayed.
func TestRemoveOldClientLocationReliabilitiesKeepsOnlyTopLevelRowsPastElevenDays(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		now := server.NowUtc()
		block := func(age time.Duration) int64 {
			return now.Add(-age).UnixMilli() / model.ReliabilityBlockDuration.Milliseconds()
		}

		networkId := server.NewId()
		topLevelId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), topLevelId, "test", "test")
		child := func() server.Id {
			childId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO network_client (client_id, network_id, description, create_time, auth_time, source_client_id)
					VALUES ($1, $2, 'test', now(), now(), $3)
					`,
					childId,
					networkId,
					topLevelId,
				))
			})
			return childId
		}
		rows := []struct {
			name     string
			clientId server.Id
			age      time.Duration
			kept     bool
		}{
			{name: "top-level client at 20 days", clientId: topLevelId, age: 20 * 24 * time.Hour, kept: true},
			{name: "hosted child at 12 days", clientId: child(), age: 12 * 24 * time.Hour, kept: false},
			{name: "gone client at 12 days", clientId: server.NewId(), age: 12 * 24 * time.Hour, kept: false},
			{name: "hosted child at 10 days", clientId: child(), age: 10 * 24 * time.Hour, kept: true},
		}
		for _, row := range rows {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_client_location_reliability (client_id, network_id, update_block_number) VALUES ($1, $2, $3)`,
					row.clientId,
					networkId,
					block(row.age),
				))
			})
		}

		if _, err := RemoveOldClientLocationReliabilities(&RemoveOldClientLocationReliabilitiesArgs{}, clientSession); err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			var exists bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT EXISTS (SELECT 1 FROM network_client_location_reliability WHERE client_id = $1)`,
					row.clientId,
				).Scan(&exists))
			})
			if exists != row.kept {
				t.Errorf("%s: exists=%t, want %t", row.name, exists, row.kept)
			}
		}
	})
}

// DB-backed: the post schedules the next run with the run's cursor, after the
// settings' MoreInterval while work remains and RunInterval otherwise, under
// the task's max time; a pending run keeps its cursor when a taskworker start
// schedules the chain again.
func TestRemoveOldClientLocationReliabilitiesPostCarriesTheCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		settings := model.DefaultClientLocationReliabilityRetentionSettings()
		runOnceKey := `["remove_old_client_location_reliabilities"]`
		for _, more := range []bool{true, false} {
			updateBlockNumber := int64(29913425)
			clientId := server.NewId()
			cursor := model.ClientLocationReliabilityRetentionCursor{AfterUpdateBlockNumber: &updateBlockNumber, AfterClientId: &clientId}
			before := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1`, runOnceKey))
				err := RemoveOldClientLocationReliabilitiesPost(
					&RemoveOldClientLocationReliabilitiesArgs{},
					&RemoveOldClientLocationReliabilitiesResult{Cursor: cursor, More: more},
					clientSession,
					tx,
				)
				if err != nil {
					t.Fatal(err)
				}
				// a taskworker start schedules the chain with no cursor
				ScheduleRemoveOldClientLocationReliabilities(clientSession, tx)
				var args RemoveOldClientLocationReliabilitiesArgs
				runAt := readIdentityRetentionPending(ctx, t, tx, runOnceKey, &args)
				if args.Cursor.AfterUpdateBlockNumber == nil || *args.Cursor.AfterUpdateBlockNumber != updateBlockNumber ||
					args.Cursor.AfterClientId == nil || *args.Cursor.AfterClientId != clientId {
					t.Fatalf("more %t: the pending run holds cursor %+v, want %+v", more, args.Cursor, cursor)
				}
				wantDelay := settings.RunInterval
				if more {
					wantDelay = settings.MoreInterval
				}
				if delay := runAt.Sub(before); delay < wantDelay-30*time.Second || wantDelay+30*time.Second < delay {
					t.Fatalf("more %t: the next run is %s away, want %s", more, delay, wantDelay)
				}
				var maxTimeSeconds int64
				server.Raise(tx.QueryRow(ctx, `SELECT run_max_time_seconds FROM pending_task WHERE run_once_key = $1`, runOnceKey).Scan(&maxTimeSeconds))
				if time.Duration(maxTimeSeconds)*time.Second < settings.TaskMaxTime {
					t.Fatalf("more %t: the next run's max time is %ds, want at least %s", more, maxTimeSeconds, settings.TaskMaxTime)
				}
			})
		}
	})
}
