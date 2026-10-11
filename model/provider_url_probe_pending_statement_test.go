// Expiry-pending scans are paid only by the admission order that reads them.
package model

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Counts the expiry-pending scans sent by the real claim transaction.
type urlProbeExpiryPendingCounter struct {
	count atomic.Int64
}

func (self *urlProbeExpiryPendingCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "WITH pending AS MATERIALIZED") && strings.Contains(data.SQL, "completed_next_expiry_at<=$1") {
		self.count.Add(1)
	}
	return ctx
}

func (self *urlProbeExpiryPendingCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Ordinary admission discards whether expiry maintenance is still pending, yet
// every claim re-read each owned slot's expired head to answer it. With dead
// queue entries that the xmin horizon keeps, that scan was about a third of
// the scheduler's buffer reads on Main. Priority admission still needs it.
func TestUrlDueOrdinaryClaimSkipsUnreadExpiryPendingScan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		clientIds := testingUrlCompletionClients(t, now, 2)
		cases := []struct {
			since            time.Time
			priority         bool
			pendingStatement bool
		}{
			{since: now, priority: false, pendingStatement: false},
			{since: now.Add(-8 * time.Hour), priority: true, pendingStatement: true},
		}
		for index, c := range cases {
			testingUrlCompletionPriority(t, c.since)
			if providerUrlProbeCompletedPriorityReady(now) != c.priority {
				t.Fatalf("case%d priority readiness fixture is wrong", index)
			}
			// Use the owner shard of eight, so a claim reaches its normal query.
			var shard int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT slot_id%8 FROM provider_egress_probe_cycle WHERE client_id=$1`, clientIds[index]).Scan(&shard))
			})
			counter := &urlProbeExpiryPendingCounter{}
			var result ProviderUrlProbeDueResult
			func() {
				scope, err := server.NewTestPgQueryScope(ctx, counter)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := scope.Close(); err != nil {
						t.Error(err)
					}
				}()
				result = ClaimProviderUrlProbeDueWithStatus(ctx, now, 1, shard, 8)
			}()
			if result.CompletedRunPriorityReady != c.priority || result.PriorityMaintenancePending {
				t.Fatalf("case%d claim result=%+v", index, result)
			}
			if !c.priority && len(result.Providers) != 1 {
				t.Fatalf("case%d ordinary claim admitted %d providers, want 1", index, len(result.Providers))
			}
			if sent := counter.count.Load(); (0 < sent) != c.pendingStatement {
				t.Fatalf("case%d priority=%t sent %d expiry-pending scans, want any=%t", index, c.priority, sent, c.pendingStatement)
			}
		}
	})
}
