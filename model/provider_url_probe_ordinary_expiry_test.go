// Ordinary claims share one completed-run expiry sweep per shard interval.
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

// Counts the completed-run expiry sweeps sent by real claim transactions. The
// pending and promotion scans have no adjusting statement and are not counted.
type urlProbeExpirySweepCounter struct {
	count atomic.Int64
}

func (self *urlProbeExpirySweepCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "completed_next_expiry_at<=$1") && strings.Contains(data.SQL, "adjusted AS (") {
		self.count.Add(1)
	}
	return ctx
}

func (self *urlProbeExpirySweepCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Runs claims inside one traced pool scope and returns the sweeps they sent.
func testingCountUrlProbeExpirySweeps(t testing.TB, ctx context.Context, claims func()) int64 {
	t.Helper()
	counter := &urlProbeExpirySweepCounter{}
	scope, err := server.NewTestPgQueryScope(ctx, counter)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}()
	claims()
	return counter.count.Load()
}

// Seeds a fleet and makes the most populated shard of four due, oldest first,
// with every other shard in the future. Returns that shard and its due order.
func testingUrlProbeDueShardOrder(t testing.TB, ctx context.Context, now time.Time, fleet, minimum int) (int, []server.Id) {
	t.Helper()
	testingSeedUrlProbeFleet(t, now, fleet)
	shard := 0
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT slot_id%4 FROM provider_egress_probe_cycle
			GROUP BY 1 ORDER BY COUNT(*) DESC,1 LIMIT 1`).Scan(&shard))
	})
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$1::timestamp+interval '1 day'
			WHERE slot_id%4<>$2`, now, shard))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle AS cycle
			SET next_attempt_at=$1::timestamp-ranked.rank*interval '1 second'
			FROM (SELECT client_id,row_number() OVER (ORDER BY client_id) AS rank
				FROM provider_egress_probe_cycle WHERE slot_id%4=$2) AS ranked
			WHERE cycle.client_id=ranked.client_id`, now, shard))
	})
	order := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT client_id FROM provider_egress_probe_cycle
			WHERE slot_id%4=$1 ORDER BY next_attempt_at,client_id`, shard)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var clientId server.Id
				server.Raise(rows.Scan(&clientId))
				order = append(order, clientId)
			}
		})
	})
	if len(order) < minimum {
		t.Fatalf("shard %d/4 has %d providers, want at least %d", shard, len(order), minimum)
	}
	return shard, order
}

// One counted receipt that completed at completedAt, with its exact cycle count
// and next expiry, as the completion writer would have left it.
func testingUrlProbeCountedRun(t testing.TB, ctx context.Context, clientIds []server.Id, completedAt time.Time) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, clientId := range clientIds {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_url_probe_run
				(client_id,claim_ordinal,claimed_at,completed_at,received_at,counted)
				VALUES($1,1,$2,$2,$2,true)`, clientId, completedAt))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET claim_ordinal=1,
				completed_run_count=1,completed_next_expiry_at=$2::timestamp+interval '4 hours'
				WHERE client_id=$1`, clientId, completedAt))
		}
	})
}

// Whether each provider's only receipt is still counted, with matching state.
func testingUrlProbeRunsCounted(t testing.TB, ctx context.Context, clientIds []server.Id, counted bool) {
	t.Helper()
	for _, clientId := range clientIds {
		cycle := testingReadUrlCompletionCycle(t, ctx, clientId)
		var countedRuns int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_url_probe_run
				WHERE client_id=$1 AND counted`, clientId).Scan(&countedRuns))
		})
		retired := cycle.count == 0 && cycle.expiry == nil && countedRuns == 0
		active := cycle.count == 1 && cycle.expiry != nil && countedRuns == 1
		if (counted && !active) || (!counted && !retired) {
			t.Fatalf("provider %s counted=%t: cycle=%+v counted runs=%d", clientId, counted, cycle, countedRuns)
		}
	}
}

// On Main every worker turn sent one claim, and every ordinary claim first
// swept the expired completed-run head of all 256 owned slots, re-reading dead
// versions the xmin horizon kept, for counts ordinary admission never reads.
// Claims inside one interval now share one sweep and select exactly as before;
// the expired runs are still retired, and the next interval sweeps again.
func TestUrlDueOrdinaryClaimsShareOneExpirySweepPerShardInterval(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		shard, order := testingUrlProbeDueShardOrder(t, ctx, now, 64, 10)
		if providerUrlProbeCompletedPriorityReady(now) {
			t.Fatal("fixture must use ordinary admission")
		}
		// The newest due providers stay unclaimed; three expired an hour ago and
		// one expires a second before the next interval's claim.
		expired := order[len(order)-3:]
		later := order[len(order)-4 : len(order)-3]
		testingUrlProbeCountedRun(t, ctx, expired, now.Add(-5*time.Hour))
		testingUrlProbeCountedRun(t, ctx, later, now.Add(-3*time.Hour-time.Second))

		claims := 5
		sweeps := testingCountUrlProbeExpirySweeps(t, ctx, func() {
			for index := range claims {
				due := ClaimProviderUrlProbeDue(ctx, now.Add(time.Duration(index)*time.Microsecond), 1, shard, 4)
				if len(due) != 1 || due[0].ClientId != order[index] {
					t.Fatalf("claim %d changed ordinary selection: got=%+v want=%s", index, due, order[index])
				}
			}
		})
		if sweeps != 1 {
			t.Fatalf("%d ordinary claims in one interval sent %d expiry sweeps, want 1", claims, sweeps)
		}
		testingUrlProbeRunsCounted(t, ctx, expired, false)
		testingUrlProbeRunsCounted(t, ctx, later, true)

		next := now.Add(time.Hour)
		sweeps = testingCountUrlProbeExpirySweeps(t, ctx, func() {
			due := ClaimProviderUrlProbeDue(ctx, next, 1, shard, 4)
			if len(due) != 1 || due[0].ClientId != order[claims] {
				t.Fatalf("next interval changed ordinary selection: got=%+v want=%s", due, order[claims])
			}
		})
		if sweeps != 1 {
			t.Fatalf("the next interval's claim sent %d expiry sweeps, want 1", sweeps)
		}
		testingUrlProbeRunsCounted(t, ctx, later, false)
	})
}

// A full expiry page may leave more expired runs, so it does not close the
// interval: the next claim sweeps again, and only a partial page closes it.
func TestUrlDueOrdinaryFullExpiryPageSweepsAgain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		backlog := providerUrlProbeExpiryClients + 2
		shard, order := testingUrlProbeDueShardOrder(t, ctx, now, 640, backlog+3)
		expired := order[3 : 3+backlog]
		testingUrlProbeCountedRun(t, ctx, expired, now.Add(-5*time.Hour))
		// The full first page and the partial second page each sweep; the
		// third claim falls inside the interval the partial page closed.
		for index, want := range []int64{1, 1, 0} {
			sweeps := testingCountUrlProbeExpirySweeps(t, ctx, func() {
				due := ClaimProviderUrlProbeDue(ctx, now.Add(time.Duration(index)*time.Microsecond), 1, shard, 4)
				if len(due) != 1 || due[0].ClientId != order[index] {
					t.Fatalf("claim %d changed ordinary selection: got=%+v want=%s", index, due, order[index])
				}
			})
			if sweeps != want {
				t.Fatalf("claim %d sent %d expiry sweeps, want %d", index, sweeps, want)
			}
			var remaining int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_egress_probe_cycle
					WHERE completed_next_expiry_at<=$1`, now).Scan(&remaining))
			})
			if want := max(0, backlog-(index+1)*providerUrlProbeExpiryClients); remaining != want {
				t.Fatalf("claim %d left %d expired providers, want %d", index, remaining, want)
			}
		}
		testingUrlProbeRunsCounted(t, ctx, expired, false)
	})
}

// Priority admission orders by completed counts, so it still maintains them
// on every claim, interval or not.
func TestUrlDuePriorityClaimsMaintainOnEveryClaim(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionPriority(t, now.Add(-8*time.Hour))
		shard, order := testingUrlProbeDueShardOrder(t, ctx, now, 64, 4)
		claims := 3
		sweeps := testingCountUrlProbeExpirySweeps(t, ctx, func() {
			for index := range claims {
				result := ClaimProviderUrlProbeDueWithStatus(ctx, now.Add(time.Duration(index)*time.Microsecond), 1, shard, 4)
				if !result.CompletedRunPriorityReady || result.PriorityMaintenancePending ||
					len(result.Providers) != 1 || result.Providers[0].ClientId != order[index] {
					t.Fatalf("priority claim %d: %+v want=%s", index, result, order[index])
				}
			}
		})
		if sweeps != int64(claims) {
			t.Fatalf("%d priority claims sent %d expiry sweeps, want %d", claims, sweeps, claims)
		}
	})
}
