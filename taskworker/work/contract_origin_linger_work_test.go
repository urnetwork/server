package work

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// DB-backed: the run sweeps the side table's rows past their prune time;
// Post schedules the next run after the settings' MoreInterval while rows were
// left and after RunInterval otherwise; the startup schedule starts the chain
// after StartDelay.
func TestRemoveExpiredContractOriginLingerChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		settings := model.DefaultContractOriginLingerSweepSettings()
		const runOnceKey = `["remove_expired_contract_origin_linger"]`

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO contract_origin_linger (source_id, destination_id, contract_id, prune_time)
				VALUES
					(gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), clock_timestamp() AT TIME ZONE 'UTC' - interval '1 minute'),
					(gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), clock_timestamp() AT TIME ZONE 'UTC' + interval '1 hour')
			`))
		})
		result, err := RemoveExpiredContractOriginLinger(&RemoveExpiredContractOriginLingerArgs{}, clientSession)
		if err != nil || result.More {
			t.Fatalf("the run = %+v, %v; want caught up", result, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var expired, kept int
			server.Raise(conn.QueryRow(ctx, `
				SELECT
					count(*) FILTER (WHERE prune_time <= clock_timestamp() AT TIME ZONE 'UTC'),
					count(*) FILTER (WHERE clock_timestamp() AT TIME ZONE 'UTC' < prune_time)
				FROM contract_origin_linger
			`).Scan(&expired, &kept))
			if expired != 0 || kept != 1 {
				t.Fatalf("the run left %d rows past their prune time and %d ahead, want 0 and 1", expired, kept)
			}
		})

		for _, more := range []bool{true, false} {
			before := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				err := RemoveExpiredContractOriginLingerPost(
					&RemoveExpiredContractOriginLingerArgs{},
					&RemoveExpiredContractOriginLingerResult{More: more},
					clientSession,
					tx,
				)
				if err != nil {
					t.Fatal(err)
				}
				var args RemoveExpiredContractOriginLingerArgs
				runAt := readIdentityRetentionPending(ctx, t, tx, runOnceKey, &args)
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
			ScheduleRemoveExpiredContractOriginLinger(clientSession, tx)
			var args RemoveExpiredContractOriginLingerArgs
			runAt := readIdentityRetentionPending(ctx, t, tx, runOnceKey, &args)
			if delay := runAt.Sub(before); delay < settings.StartDelay-10*time.Second || settings.StartDelay+10*time.Second < delay {
				t.Fatalf("the startup schedule runs %v out, want %v", delay, settings.StartDelay)
			}
		})
	})
}
