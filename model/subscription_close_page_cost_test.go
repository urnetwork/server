// Synthetic retained close history exercises exact live page selectors. This
// regression preserves the accounting rules, raw page cap and scan cursor.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/urnetwork/server"
	"testing"
	"time"
)

const testingClosePageCurrentSql = `
                WITH bounded AS MATERIALIZED (
                    SELECT contract_id,source_id,destination_id,dispute,create_time,usage_unverified,expiration_time
                    FROM transfer_contract
                    WHERE open AND (create_time,contract_id)>($5,$6) AND create_time <= $7
                    ORDER BY create_time,contract_id LIMIT $4
                )
                SELECT t.contract_id,t.source_id,t.destination_id,t.dispute,
                    source_contract_close.close_time,source_contract_close.used_transfer_byte_count,source_contract_close.checkpoint,
                    destination_contract_close.close_time,destination_contract_close.used_transfer_byte_count,destination_contract_close.checkpoint,
                    t.create_time,
                    NOT COALESCE((SELECT true FROM legacy_settlement_intent pending WHERE pending.contract_id=t.contract_id),false)
                    AND (t.usage_unverified OR (t.create_time <= $3
                        AND NOT EXISTS(SELECT 1 FROM contract_close recent_close WHERE recent_close.contract_id=t.contract_id AND recent_close.close_time > $3))),
                    CASE WHEN COALESCE(t.expiration_time, t.create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC'
                        THEN COALESCE(t.expiration_time, t.create_time + interval '60 minutes') END AS next_expiration_time
                FROM bounded t
                LEFT JOIN contract_close source_contract_close ON source_contract_close.contract_id=t.contract_id AND source_contract_close.party=$1
                LEFT JOIN contract_close destination_contract_close ON destination_contract_close.contract_id=t.contract_id AND destination_contract_close.party=$2
                ORDER BY t.create_time,t.contract_id
			`

const testingDisputePageCurrentSql = `
                WITH bounded AS MATERIALIZED (
                    SELECT contract_id,source_id,destination_id,create_time,usage_unverified,expiration_time
                    FROM transfer_contract
                    WHERE dispute AND outcome IS NULL AND (create_time,contract_id)>($3,$4) AND create_time <= $5
                    ORDER BY create_time,contract_id LIMIT $2
                )
                SELECT t.contract_id,t.source_id,t.destination_id,t.create_time,
                    NOT COALESCE((SELECT true FROM legacy_settlement_intent pending WHERE pending.contract_id=t.contract_id),false)
                    AND (t.usage_unverified OR (t.create_time <= $1
                        AND NOT EXISTS(SELECT 1 FROM contract_close recent_close WHERE recent_close.contract_id=t.contract_id AND recent_close.close_time > $1))),
                    CASE WHEN COALESCE(t.expiration_time, t.create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC'
                        THEN COALESCE(t.expiration_time, t.create_time + interval '60 minutes') END AS next_expiration_time
                FROM bounded t ORDER BY t.create_time,t.contract_id
			`

// Keep the original SQL unwrapped for the execution plan. Only the separate
// result-equivalence read aggregates private synthetic fixture rows into a hash.
// Both variants include the scalar deadline hint; keep row_to_json so duplicate
// source/destination report field names retain both values in the digest.
func testingLoadedClosePagePlan(t testing.TB, ctx context.Context, query string, args ...any) testingUrlCompletedPlan {
	t.Helper()
	var plans []testingUrlCompletedPlan
	server.Db(ctx, func(conn server.PgConn) {
		var raw []byte
		server.Raise(conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, args...).Scan(&raw))
		server.Raise(json.Unmarshal(raw, &plans))
	}, server.OptNoRetry())
	if len(plans) != 1 {
		t.Fatal("missing close page plan")
	}
	return plans[0]
}

// Unrelated historical rows must not change the selected raw page or any
// source/destination report and quiet/pending eligibility value in that page.
func testingLoadedClosePageDigest(t testing.TB, ctx context.Context, query string, args ...any) (string, int) {
	t.Helper()
	var digest string
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT md5(coalesce(string_agg(row_to_json(q)::text,E'\n' ORDER BY q.create_time,q.contract_id),'')),count(*) FROM (`+query+`)q`, args...).Scan(&digest, &count))
	}, server.OptNoRetry())
	return digest, count
}

// Separate raw contract-head traversal from downstream close-report work.
// The retained-history population exceeds the selected page by twentyfold.
func TestForceClosePageRetainedHistoryBounded(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		now := server.NowUtc().Truncate(time.Microsecond)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
       (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,expiration_time,dispute,outcome,close_time,provider_usage)
       SELECT md5('synthetic-close-contract-'||g)::uuid,md5('synthetic-network')::uuid,
         md5('synthetic-source')::uuid,md5('synthetic-network')::uuid,md5('synthetic-destination')::uuid,1,
         $1::timestamp-interval '2 hours'+(g%50000)*interval '1 microsecond',$1::timestamp+interval '1 hour',
         g BETWEEN 50001 AND 100000,CASE WHEN g>100000 THEN 'settled' ELSE NULL END,
				CASE WHEN g>100000 THEN $1::timestamp-interval '1 hour' ELSE NULL END,
				CASE WHEN g>100000 THEN '{"version":1,"byte_count":0,"providers":[]}'::jsonb ELSE NULL END
       FROM generate_series(1,600000)g`, now))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
       SELECT md5('synthetic-close-contract-'||g)::uuid,party,0,
         CASE WHEN g%5=0 THEN $1::timestamp ELSE $1::timestamp-interval '2 hours' END,
         g<=100000 AND g%3=0 FROM generate_series(1,600000)g CROSS JOIN (VALUES('source'),('destination'))p(party)`, now))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome)
       SELECT md5('synthetic-close-contract-'||g)::uuid,get_byte(uuid_send(md5('synthetic-close-contract-'||g)::uuid),15)%16,'settled'
       FROM generate_series(1,100000)g WHERE g%7=0`))
			// Exercise missing one/both reports as well as complete/checkpoint rows.
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM contract_close WHERE contract_id IN
                (SELECT md5('synthetic-close-contract-'||g)::uuid FROM generate_series(1,100000)g WHERE g%11=0)
                AND party='source'`))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM contract_close WHERE contract_id IN
                (SELECT md5('synthetic-close-contract-'||g)::uuid FROM generate_series(1,100000)g WHERE g%13=0)
                AND party='destination'`))
			for _, table := range []string{"transfer_contract", "contract_close", "legacy_settlement_intent"} {
				server.RaisePgResult(tx.Exec(ctx, "ANALYZE "+table))
			}
		})
		// Both untouched pages and cursor continuations retain all skipped raw rows.
		for _, continued := range []bool{false, true} {
			position := time.Time{}
			if continued {
				position = now.Add(-2 * time.Hour).Add(25000 * time.Microsecond)
			}
			for _, disputed := range []bool{false, true} {
				args := []any{ContractPartySource, ContractPartyDestination, now.Add(-5 * time.Minute), 25000, position, server.Id{}, now}
				queries := []struct {
					name, query string
					bounded     bool
				}{{"current", testingClosePageCurrentSql, false}, {"production_keyed", forceCloseOpenContractPageSql, true}}
				if disputed {
					args = []any{now.Add(-5 * time.Minute), 25000, position, server.Id{}, now}
					queries = []struct {
						name, query string
						bounded     bool
					}{{"current", testingDisputePageCurrentSql, false}, {"production_dispute", forceCloseDisputedContractPageSql, true}}
				}
				expected, count := testingLoadedClosePageDigest(t, ctx, queries[0].query, args...)
				if count != 25000 {
					t.Fatalf("synthetic raw page size=%d want25000", count)
				}
				for _, q := range queries {
					got, gotCount := testingLoadedClosePageDigest(t, ctx, q.query, args...)
					if got != expected || gotCount != count {
						t.Fatalf("dispute=%t continued=%t %s changed page contents", disputed, continued, q.name)
					}
					plan := testingLoadedClosePagePlan(t, ctx, q.query, args...)
					closeRows, closeLoops, closeBuffers := float64(0), float64(0), float64(0)
					unboundedCloseScan := false
					testingWalkUrlCompletedPlan(plan.Plan, func(n testingUrlCompletedPlanNode) {
						if n.RelationName != "contract_close" {
							return
						}
						examined := (n.ActualRows + n.RowsRemoved + n.RowsRechecked) * n.ActualLoops
						closeRows += examined
						closeLoops += n.ActualLoops
						closeBuffers += n.SharedHits + n.SharedReads
						if examined > float64(3*count) || (n.NodeType == "Seq Scan" && n.ActualRows+n.RowsRemoved > float64(count)) {
							unboundedCloseScan = true
						}
					})
					t.Logf("seeded_report_rows=1200000 raw_page=%d dispute=%t continued=%t variant=%s exec_ms=%.3f plan_ms=%.3f close_examined=%.0f close_loops=%.0f close_buffers=%.0f total_buffers=%.0f global_close_scan=%t", count, disputed, continued, q.name, plan.ExecutionTime, plan.PlanningTime, closeRows, closeLoops, closeBuffers, plan.Plan.SharedHits+plan.Plan.SharedReads, unboundedCloseScan)
					if q.bounded && (unboundedCloseScan || closeRows > float64(8*count)) {
						t.Fatal(fmt.Sprintf("keyed close lookup grew with retained history: examined=%.0f", closeRows))
					}
				}
			}
		}
	})
}
