// Prevents per-contract parallel startup under distorted historical estimates.
package model

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The old query is a positive control for the synthetic planner distortion.
const settlementReadLegacySql = `
 SELECT transfer_escrow.balance_id, transfer_escrow.balance_byte_count,
  transfer_balance.start_balance_byte_count, transfer_balance.net_revenue_nano_cents
 FROM transfer_escrow INNER JOIN transfer_balance
 ON transfer_balance.balance_id=transfer_escrow.balance_id
 WHERE transfer_escrow.contract_id=$1
 ORDER BY transfer_balance.end_time ASC
`

type settlementReadWork struct {
	rows           int
	buffers        int
	workersPlanned int
	workersStarted int
	estimatedRows  float64
	executionMs    float64
	shapes         []string
}

// Actual rows/buffers and worker launches remain separate from execution wall time.
func settlementReadExplain(ctx context.Context, conn server.PgConn, sql string, args ...any) settlementReadWork {
	var raw []byte
	server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) `+sql, args...).Scan(&raw))
	var envelopes []map[string]any
	server.Raise(json.Unmarshal(raw, &envelopes))
	root := envelopes[0]["Plan"].(map[string]any)
	number := func(node map[string]any, key string) float64 { value, _ := node[key].(float64); return value }
	work := settlementReadWork{estimatedRows: number(root, "Plan Rows"), rows: int(number(root, "Actual Rows")), buffers: int(number(root, "Shared Hit Blocks") + number(root, "Shared Read Blocks")), executionMs: number(envelopes[0], "Execution Time")}
	var visit func(map[string]any)
	visit = func(node map[string]any) {
		work.workersPlanned += int(number(node, "Workers Planned"))
		work.workersStarted += int(number(node, "Workers Launched"))
		shape, _ := node["Node Type"].(string)
		if relation, ok := node["Relation Name"].(string); ok {
			shape += ":" + relation
		}
		work.shapes = append(work.shapes, shape)
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				visit(child.(map[string]any))
			}
		}
	}
	visit(root)
	return work
}

type settlementReadRow struct {
	balanceId             server.Id
	bytes, start, revenue int64
}

// Actual projection and order, not merely counts, are compared with the owner.
func settlementReadRows(ctx context.Context, conn server.PgConn, query string, contractId server.Id) []settlementReadRow {
	rows, err := conn.Query(ctx, query, contractId)
	values := []settlementReadRow{}
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var value settlementReadRow
			server.Raise(rows.Scan(&value.balanceId, &value.bytes, &value.start, &value.revenue))
			values = append(values, value)
		}
	})
	return values
}

// This is controlled synthetic cardinality, not an ANALYZE sampling claim.
// PostgreSQL18's supported statistics importer changes only disposable local
// relation statistics. It cannot be run against Main by this test.
func TestSettlementReadAvoidsParallelStartup(t *testing.T) {
	baseline := settlementReadLegacySql
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			// Match only session-local costing relevant to the reviewed source
			// plan. These do not disable a plan type or affect another session.
			for _, setting := range []struct{ name, value string }{
				{name: "random_page_cost", value: "1.1"},
				{name: "seq_page_cost", value: "1"},
				{name: "parallel_setup_cost", value: "1000"},
				{name: "parallel_tuple_cost", value: "0.1"},
				{name: "max_parallel_workers_per_gather", value: "4"},
				{name: "min_parallel_table_scan_size", value: "8MB"},
				{name: "min_parallel_index_scan_size", value: "512kB"},
				{name: "work_mem", value: "256MB"},
				{name: "effective_cache_size", value: "768GB"},
			} {
				server.RaisePgResult(conn.Exec(ctx, `SELECT set_config($1,$2,false)`, setting.name, setting.value))
			}
			defer func() { server.RaisePgResult(conn.Exec(context.Background(), `RESET ALL`)) }()
			// Only explicit fixture analysis may replace the imported statistics.
			server.RaisePgResult(conn.Exec(ctx, `CREATE UNLOGGED TABLE settlement_plan_balance (LIKE transfer_balance INCLUDING ALL) WITH (autovacuum_enabled=false)`))
			defer func() { server.RaisePgResult(conn.Exec(context.Background(), `DROP TABLE settlement_plan_balance`)) }()
			server.RaisePgResult(conn.Exec(ctx, `CREATE UNLOGGED TABLE settlement_plan_escrow (LIKE transfer_escrow INCLUDING ALL) WITH (autovacuum_enabled=false)`))
			defer func() { server.RaisePgResult(conn.Exec(context.Background(), `DROP TABLE settlement_plan_escrow`)) }()
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO settlement_plan_balance
 (balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,
  net_revenue_nano_cents,subsidy_net_revenue_nano_cents,pro)
 SELECT md5('synthetic-balance:'||i::text)::uuid,$1,
  '2020-01-01'::timestamp,'2030-01-01'::timestamp+i*interval '1 second',
  4096,4096,i%2,0,false FROM generate_series(0,99999) i`, server.NewId()))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO settlement_plan_escrow(contract_id,balance_id,balance_byte_count)
 SELECT md5('synthetic-contract:'||(i%4096)::text)::uuid,
  md5('synthetic-balance:'||((i/4096)*301%100000)::text)::uuid,i%3
 FROM generate_series(0,4096*329-1) i`))
			thinId := server.NewId()
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO settlement_plan_escrow(contract_id,balance_id,balance_byte_count)
 VALUES($1,md5('synthetic-balance:50000')::uuid,1024)`, thinId))
			wideId := server.NewId()
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO settlement_plan_escrow(contract_id,balance_id,balance_byte_count)
 SELECT $1,md5('synthetic-balance:'||i::text)::uuid,i%3
 FROM generate_series(1000,2023) i`, wideId))
			tiedId := server.NewId()
			server.RaisePgResult(conn.Exec(ctx, `UPDATE settlement_plan_balance
 SET end_time='2030-01-03'::timestamp,net_revenue_nano_cents=CASE balance_id
 WHEN md5('synthetic-balance:90001')::uuid THEN 7 ELSE 11 END
 WHERE balance_id IN (md5('synthetic-balance:90000')::uuid,
 md5('synthetic-balance:90001')::uuid,md5('synthetic-balance:90002')::uuid)`))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO settlement_plan_escrow(contract_id,balance_id,balance_byte_count)
 SELECT $1,md5('synthetic-balance:'||i::text)::uuid,(i-90000)*1024
 FROM generate_series(90000,90002) i`, tiedId))
			// A foreign/missing balance keeps the original inner-join behavior;
			// no assumed schema repair may turn it into a settlement input.
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO settlement_plan_escrow(contract_id,balance_id,balance_byte_count)
 VALUES($1,$2,8192)`, thinId, server.NewId()))
			server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) settlement_plan_balance`))
			server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) settlement_plan_escrow`))
			server.RaisePgResult(conn.Exec(ctx, `SELECT pg_clear_attribute_stats('public','settlement_plan_escrow','contract_id',false)`))
			var restored bool
			server.Raise(conn.QueryRow(ctx, `SELECT pg_restore_attribute_stats(
 'schemaname','public','relname','settlement_plan_escrow','attname','contract_id',
 'inherited',false,'n_distinct',40::real,'null_frac',0::real,'avg_width',16::integer)`).Scan(&restored))
			if !restored {
				t.Fatal("synthetic attribute statistics rejected")
			}
			// Preserve a broad balance-key estimate too. Leaving the physical
			// fixture's329 distinct keys invents a cheap Memoize shortcut absent
			// from the observed per-request parallel settlement plan.
			server.RaisePgResult(conn.Exec(ctx, `SELECT pg_clear_attribute_stats('public','settlement_plan_escrow','balance_id',false)`))
			server.Raise(conn.QueryRow(ctx, `SELECT pg_restore_attribute_stats(
 'schemaname','public','relname','settlement_plan_escrow','attname','balance_id',
 'inherited',false,'n_distinct',20000::real,'null_frac',0::real,'avg_width',16::integer)`).Scan(&restored))
			if !restored {
				t.Fatal("synthetic balance-key statistics rejected")
			}
			server.Raise(conn.QueryRow(ctx, `SELECT pg_restore_relation_stats(
 'schemaname','public','relname','settlement_plan_balance','reltuples',50000000::real)`).Scan(&restored))
			if !restored {
				t.Fatal("synthetic relation statistics rejected")
			}
			var distinct float32
			server.Raise(conn.QueryRow(ctx, `SELECT n_distinct FROM pg_stats WHERE schemaname='public'
 AND tablename='settlement_plan_escrow' AND attname='contract_id' AND NOT inherited`).Scan(&distinct))
			if distinct != 40 {
				t.Fatal("controlled cardinality not installed")
			}
			// The outer test bounds fixture construction; this tighter fence
			// applies only to the candidate reads under investigation.
			server.RaisePgResult(conn.Exec(ctx, `SET statement_timeout='5s'`))
			var maximumWorkers, maximumWorkersPerGather, memoize string
			server.Raise(conn.QueryRow(ctx, `SELECT current_setting('max_parallel_workers'),
 current_setting('max_parallel_workers_per_gather'),current_setting('enable_memoize')`).Scan(
				&maximumWorkers, &maximumWorkersPerGather, &memoize))
			t.Logf("local planner maximum_workers=%s maximum_per_gather=%s memoize=%s", maximumWorkers, maximumWorkersPerGather, memoize)
			rewrite := func(query string) string {
				return strings.ReplaceAll(strings.ReplaceAll(query, "transfer_escrow", "settlement_plan_escrow"), "transfer_balance", "settlement_plan_balance")
			}
			var legacyId server.Id
			server.Raise(conn.QueryRow(ctx, `SELECT md5('synthetic-contract:0')::uuid`).Scan(&legacyId))
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(conn.Exec(ctx, "SELECT set_config('plan_cache_mode',$1,false)", mode))
				for _, query := range []struct{ name, sql string }{
					{name: "legacy", sql: baseline},
					{name: "owning", sql: settlementEscrowReadSql},
				} {
					server.RaisePgResult(conn.Exec(ctx, "PREPARE settlement_read_plan(uuid) AS "+rewrite(query.sql)))
					work := settlementReadExplain(ctx, conn, "EXECUTE settlement_read_plan('"+thinId.String()+"'::uuid)")
					server.RaisePgResult(conn.Exec(ctx, "DEALLOCATE settlement_read_plan"))
					if work.rows != 1 || work.estimatedRows < 20000 {
						t.Fatalf("%s/%s did not retain the controlled estimate and one-row result", mode, query.name)
					}
					t.Logf("%s/%s estimated_rows=%.0f rows=%d buffers=%d planned=%d launched=%d", mode, query.name,
						work.estimatedRows, work.rows, work.buffers, work.workersPlanned, work.workersStarted)
					if query.name == "legacy" {
						if work.workersPlanned == 0 {
							t.Fatal("legacy control did not reproduce per-contract parallel startup")
						}
					} else if work.workersPlanned != 0 || work.workersStarted != 0 || work.buffers > 32 {
						t.Fatalf("owning settlement read amplified one-row work: planned=%d launched=%d buffers=%d",
							work.workersPlanned, work.workersStarted, work.buffers)
					}
				}
			}
			// Compare exact projections for all shapes. Expiry ties have no
			// specified internal order, so compare their set separately.
			for _, geometry := range []struct {
				name       string
				contractId server.Id
				rows       int
			}{
				{name: "thin_with_missing_balance", contractId: thinId, rows: 1},
				{name: "legacy", contractId: legacyId, rows: 329},
				{name: "uncapped", contractId: wideId, rows: 1024},
				{name: "equal_expiry_mixed_prices", contractId: tiedId, rows: 3},
				{name: "missing_contract", contractId: server.NewId(), rows: 0},
			} {
				want := settlementReadRows(ctx, conn, rewrite(baseline), geometry.contractId)
				if len(want) != geometry.rows {
					t.Fatalf("synthetic geometry %s rows=%d want=%d", geometry.name, len(want), geometry.rows)
				}
				got := settlementReadRows(ctx, conn, rewrite(settlementEscrowReadSql), geometry.contractId)
				if geometry.name == "equal_expiry_mixed_prices" {
					compare := func(a, b settlementReadRow) int { return a.balanceId.Cmp(b.balanceId) }
					slices.SortFunc(want, compare)
					slices.SortFunc(got, compare)
				}
				if !slices.Equal(got, want) {
					t.Errorf("owning settlement changed the projection or expiry order for %s", geometry.name)
				}
			}
		}, server.OptReadWrite())
	})
}
