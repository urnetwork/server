// Exercise global task identity, bounded continuation, and cancellation.
package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Repeated worker startups cannot multiply the maintenance lane. Continuation
// derives from committed page progress, not an unbounded cleanup loop.
func TestUrlReceiptRetentionTaskCoalescesAndBoundsContinuation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "192.0.2.1:0", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			for range 8 {
				ScheduleRemoveExpiredProviderUrlProbeRuns(owner, tx)
			}
		})
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM pending_task WHERE run_once_key='["remove_expired_provider_url_probe_runs"]'`).Scan(&count))
			if count != 1 {
				t.Fatal("worker startup multiplied receipt cleanup", count)
			}
		})
		for _, removed := range []int64{0, 1, providerUrlProbeRetentionPageSize - 1, providerUrlProbeRetentionPageSize} {
			before := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key='["remove_expired_provider_url_probe_runs"]'`))
				server.Raise(RemoveExpiredProviderUrlProbeRunsPost(&RemoveExpiredProviderUrlProbeRunsArgs{}, &RemoveExpiredProviderUrlProbeRunsResult{Removed: removed}, owner, tx))
			})
			after := server.NowUtc()
			server.Db(ctx, func(conn server.PgConn) {
				var due time.Time
				var seconds float64
				server.Raise(conn.QueryRow(ctx, `SELECT run_at,run_max_time_seconds FROM pending_task WHERE run_once_key='["remove_expired_provider_url_probe_runs"]'`).Scan(&due, &seconds))
				delay := time.Minute
				if removed == providerUrlProbeRetentionPageSize {
					delay = time.Second
				}
				if seconds != 30 || due.Before(before.Add(delay)) || due.After(after.Add(delay)) {
					t.Fatal("cleanup lost bounded continuation", removed, seconds)
				}
			})
		}
		result, err := RemoveExpiredProviderUrlProbeRuns(&RemoveExpiredProviderUrlProbeRunsArgs{}, owner)
		if err != nil || result == nil || result.Removed != 0 {
			t.Fatal("empty maintenance did not complete", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		stopped := session.NewLocalClientSession(canceled, "192.0.2.2:0", nil)
		defer stopped.Cancel()
		if result, err := RemoveExpiredProviderUrlProbeRuns(&RemoveExpiredProviderUrlProbeRunsArgs{}, stopped); result != nil || err == nil {
			t.Fatal("canceled cleanup claimed success")
		}
	})
}
