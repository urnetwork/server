// Rejected stale hints cannot pin eligible URL work behind a bounded due head.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A hint can become stale between location passes. Retire only the rejected
// head without resetting progress or requiring the next periodic full refresh.
func TestUrlProbeDueRetiresStaleHead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now, 8)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET arin_risk=true
				WHERE client_id IN (SELECT client_id FROM provider_egress_probe_cycle
				ORDER BY next_attempt_at,client_id LIMIT 4)`))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle
				SET success_count=3,error_count=2,outcome_count=5`))
		})
		if due := ClaimProviderUrlProbeDue(ctx, now, 4, 0, 1); len(due) != 0 {
			t.Fatalf("stale hint bypassed the risk gate: admitted=%d", len(due))
		}
		var retired, unchanged int
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT COUNT(*) FILTER (WHERE NOT cycle.eligible),
				COUNT(*) FILTER (WHERE cycle.cycle_started_at=$1 AND cycle.next_attempt_at=$1
				AND cycle.success_count=3 AND cycle.error_count=2 AND cycle.outcome_count=5)
				FROM provider_egress_probe_cycle AS cycle
				JOIN network_client_location_reliability AS provider USING (client_id)
				WHERE provider.arin_risk`, now)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing rejected-head state")
				}
				server.Raise(rows.Scan(&retired, &unchanged))
			})
		})
		if retired != 4 || unchanged != 4 {
			t.Fatalf("rejected stale hints pinned the due head or changed progress: retired=%d unchanged=%d, want 4 each", retired, unchanged)
		}
		if due := ClaimProviderUrlProbeDue(ctx, now, 4, 0, 1); len(due) != 4 {
			t.Fatalf("healthy tail still waits for a full eligibility refresh: admitted=%d, want 4", len(due))
		}
	})
}

// A shard repairs only its locked head, leaving foreign-slot hints and current
// gates intact even when the rejected rows lack a usable public identity.
func TestUrlProbeDueRetiresStaleShardHead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now, 64)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false
				WHERE client_id IN (SELECT client_id FROM provider_egress_probe_cycle
				WHERE slot_id%4=0 ORDER BY next_attempt_at,client_id LIMIT 2)`))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET arin_risk=true
				WHERE client_id IN (SELECT client_id FROM provider_egress_probe_cycle WHERE slot_id%4<>0)`))
		})
		if due := ClaimProviderUrlProbeDue(ctx, now, 2, 0, 4); len(due) != 0 {
			t.Fatalf("inactive shard head was admitted: got=%d", len(due))
		}
		var retiredOwn, retiredForeign int
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT COUNT(*) FILTER (WHERE slot_id%4=0),
				COUNT(*) FILTER (WHERE slot_id%4<>0)
				FROM provider_egress_probe_cycle WHERE NOT eligible`)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing shard repair state")
				}
				server.Raise(rows.Scan(&retiredOwn, &retiredForeign))
			})
		})
		if retiredOwn != 2 || retiredForeign != 0 {
			t.Fatalf("stale shard head did not retire within its ownership: own=%d foreign=%d", retiredOwn, retiredForeign)
		}
		if due := ClaimProviderUrlProbeDue(ctx, now, 2, 0, 4); len(due) != 2 {
			t.Fatalf("healthy shard tail remained blocked: admitted=%d, want 2", len(due))
		}
	})
}
