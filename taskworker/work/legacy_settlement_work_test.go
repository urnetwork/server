// Exercise the real task rows and continuation policy, not a synthetic timer.
package work

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestLegacySettlementTaskPartitionsAndBoundedContinuation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleFlushLegacySettlements(owner, tx)
			ScheduleFlushLegacySettlements(owner, tx)
		})
		seen := map[int]bool{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT args_json,run_at,run_max_time_seconds FROM pending_task WHERE run_once_key LIKE '["flush_legacy_settlements_%'`)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var data []byte
					var due time.Time
					var seconds float64
					server.Raise(rows.Scan(&data, &due, &seconds))
					var args FlushLegacySettlementsArgs
					server.Raise(json.Unmarshal(data, &args))
					if seen[args.Shard] || args.Shard < 0 || args.Shard >= 16 || args.Cursor != nil || seconds != 30 || due.Before(server.NowUtc()) || due.After(server.NowUtc().Add(3*time.Second)) {
						t.Fatal("invalid scheduled partition", args.Shard, seconds)
					}
					seen[args.Shard] = true
				}
			})
		})
		if len(seen) != 16 {
			t.Fatal("missing independent task keys", len(seen))
		}
		for _, test := range []struct {
			name             string
			more             bool
			released, failed int
			immediate        bool
		}{
			{"progress", true, 512, 0, true}, {"busy", true, 0, 0, false}, {"partial_failure", true, 512, 1, false}, {"caught_up", false, 3, 0, false},
		} {
			cursor := model.LegacySettlementCursor{NextAttemptTime: server.NowUtc(), ContractId: server.NewId(), PassEndTime: server.NowUtc(),
				HeadAfter: &model.LegacySettlementPosition{NextAttemptTime: server.NowUtc().Add(-time.Hour), ContractId: server.NewId()}}
			before := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key='["flush_legacy_settlements_3"]'`))
				server.Raise(FlushLegacySettlementsPost(&FlushLegacySettlementsArgs{Shard: 3}, &FlushLegacySettlementsResult{model.LegacySettlementFlushResult{Cursor: &cursor, More: test.more, Completed: test.released, Failed: test.failed}}, owner, tx))
			})
			server.Db(ctx, func(conn server.PgConn) {
				var data []byte
				var due time.Time
				server.Raise(conn.QueryRow(ctx, `SELECT args_json,run_at FROM pending_task WHERE run_once_key='["flush_legacy_settlements_3"]'`).Scan(&data, &due))
				var args FlushLegacySettlementsArgs
				server.Raise(json.Unmarshal(data, &args))
				if args.Cursor == nil || args.Cursor.ContractId != cursor.ContractId || !args.Cursor.NextAttemptTime.Equal(cursor.NextAttemptTime) || !args.Cursor.PassEndTime.Equal(cursor.PassEndTime) ||
					args.Cursor.HeadAfter == nil || args.Cursor.HeadAfter.ContractId != cursor.HeadAfter.ContractId || !args.Cursor.HeadAfter.NextAttemptTime.Equal(cursor.HeadAfter.NextAttemptTime) {
					t.Fatal("cursor lost", test.name)
				}
				if test.immediate && due.Sub(before) > time.Second || !test.immediate && due.Sub(before) < time.Second {
					t.Fatal("incorrect continuation", test.name, due.Sub(before))
				}
			})
		}
		// Cancellation reaches the owning flush function; it cannot acknowledge work.
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		short := session.NewLocalClientSession(canceled, "0.0.0.0:0", nil)
		defer short.Cancel()
		if _, err := FlushLegacySettlements(&FlushLegacySettlementsArgs{Shard: 0}, short); err == nil {
			t.Fatal("canceled flush succeeded")
		}
	})
}
