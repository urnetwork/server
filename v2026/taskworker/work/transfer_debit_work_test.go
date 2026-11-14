// Exercise the real task rows and continuation policy, not a synthetic timer.
package work

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

func TestTransferDebitTaskPartitionsAndBoundedContinuation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleFlushTransferDebits(owner, tx)
			ScheduleFlushTransferDebits(owner, tx)
		}, server.TxReadCommitted, server.OptNoRetry())
		seen := map[int]bool{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT args_json,run_at,run_max_time_seconds FROM pending_task WHERE run_once_key LIKE '["flush_transfer_debits_%'`)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var data []byte
					var due time.Time
					var seconds float64
					server.Raise(rows.Scan(&data, &due, &seconds))
					var args FlushTransferDebitsArgs
					server.Raise(json.Unmarshal(data, &args))
					if seen[args.Shard] || args.Shard < 0 || args.Shard >= 16 || args.AfterBalanceId != nil || seconds != 30 || due.Before(server.NowUtc()) || due.After(server.NowUtc().Add(3*time.Second)) {
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
			{name: "progress", more: true, released: 512, immediate: true},
			{name: "page_deadline_progress", more: true, released: 1, immediate: true},
			{name: "busy", more: true},
			{name: "partial_failure", more: true, released: 512, failed: 1},
			{name: "caught_up", released: 3},
		} {
			cursor := server.NewId()
			before := server.NowUtc()
			server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(task.RunOnce("flush_transfer_debits_3"))}, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key='["flush_transfer_debits_3"]'`))
				server.Raise(FlushTransferDebitsPost(&FlushTransferDebitsArgs{Shard: 3}, &FlushTransferDebitsResult{TransferDebitFlushResult: model.TransferDebitFlushResult{LastBalanceId: &cursor, More: test.more, Released: test.released, Failed: test.failed}}, owner, tx))
			}, server.TxReadCommitted, server.OptNoRetry())
			server.Db(ctx, func(conn server.PgConn) {
				var data []byte
				var due time.Time
				server.Raise(conn.QueryRow(ctx, `SELECT args_json,run_at FROM pending_task WHERE run_once_key='["flush_transfer_debits_3"]'`).Scan(&data, &due))
				var args FlushTransferDebitsArgs
				server.Raise(json.Unmarshal(data, &args))
				if args.AfterBalanceId == nil || *args.AfterBalanceId != cursor {
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
		if _, err := FlushTransferDebits(&FlushTransferDebitsArgs{Shard: 0}, short); err == nil {
			t.Fatal("canceled flush succeeded")
		}
	})
}
