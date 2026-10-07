package model

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

const legacySettlementExpectedGrantCountBaseline = `SELECT count(*) FROM transfer_escrow JOIN transfer_balance USING(balance_id) WHERE contract_id=$1`

// This is deliberately distorted local cardinality, not measured Main statistics.
// Both prepared-plan modes must keep a single contract's lookup serial while the
// old exact query remains a positive control for parallel worker startup.
func TestLegacySettlementPreflightAvoidsParallelStartup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `
    SET random_page_cost=1.1; SET seq_page_cost=1;
    SET parallel_setup_cost=1000; SET parallel_tuple_cost=0.1;
    SET max_parallel_workers_per_gather=4;
    SET min_parallel_table_scan_size='8MB'; SET min_parallel_index_scan_size='512kB';
    SET work_mem='64MB'; SET effective_cache_size='768GB';
    CREATE UNLOGGED TABLE legacy_plan_balance(balance_id uuid PRIMARY KEY) WITH (autovacuum_enabled=false);
    CREATE UNLOGGED TABLE legacy_plan_escrow(contract_id uuid NOT NULL,balance_id uuid NOT NULL,
      PRIMARY KEY(contract_id,balance_id)) WITH (autovacuum_enabled=false);
    INSERT INTO legacy_plan_balance SELECT md5('synthetic-balance:'||i::text)::uuid FROM generate_series(0,99999)i;
    INSERT INTO legacy_plan_escrow SELECT md5('synthetic-contract:'||(i%1024)::text)::uuid,
      md5('synthetic-balance:'||((i/1024)*301%100000)::text)::uuid FROM generate_series(0,1024*320-1)i;
    INSERT INTO legacy_plan_escrow VALUES(md5('thin')::uuid,md5('synthetic-balance:50000')::uuid),
      (md5('thin')::uuid,md5('missing-balance')::uuid);
    INSERT INTO legacy_plan_escrow SELECT md5('wide')::uuid,md5('synthetic-balance:'||i::text)::uuid FROM generate_series(1000,2023)i;
   `))
			defer func() {
				server.RaisePgResult(conn.Exec(context.Background(), `RESET ALL; DROP TABLE legacy_plan_escrow; DROP TABLE legacy_plan_balance`))
			}()
			server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) legacy_plan_balance`))
			server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) legacy_plan_escrow`))
			server.RaisePgResult(conn.Exec(ctx, `DO $stats$ BEGIN
    PERFORM pg_clear_attribute_stats('public','legacy_plan_escrow','contract_id',false);
    IF NOT pg_restore_attribute_stats('schemaname','public','relname','legacy_plan_escrow','attname','contract_id',
      'inherited',false,'n_distinct',10::real,'null_frac',0::real,'avg_width',16::integer)
      THEN RAISE EXCEPTION 'fixture contract statistics refused'; END IF;
    PERFORM pg_clear_attribute_stats('public','legacy_plan_escrow','balance_id',false);
    IF NOT pg_restore_attribute_stats('schemaname','public','relname','legacy_plan_escrow','attname','balance_id',
      'inherited',false,'n_distinct',20000::real,'null_frac',0::real,'avg_width',16::integer)
      THEN RAISE EXCEPTION 'fixture balance statistics refused'; END IF;
    IF NOT pg_restore_relation_stats('schemaname','public','relname','legacy_plan_balance','reltuples',50000000::real)
      THEN RAISE EXCEPTION 'fixture relation statistics refused'; END IF;
   END $stats$; SET statement_timeout='5s'`))
			rewrite := func(query string) string {
				return strings.ReplaceAll(strings.ReplaceAll(query, "transfer_escrow", "legacy_plan_escrow"), "transfer_balance", "legacy_plan_balance")
			}
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(conn.Exec(ctx, "SELECT set_config('plan_cache_mode',$1,false)", mode))
				for _, query := range []struct{ name, sql string }{{"baseline", legacySettlementExpectedGrantCountBaseline}, {"owning", legacySettlementExpectedGrantCountSQL}} {
					server.RaisePgResult(conn.Exec(ctx, "PREPARE legacy_preflight_plan(uuid) AS "+rewrite(query.sql)))
					work := settlementReadExplain(ctx, conn, "EXECUTE legacy_preflight_plan(md5('thin')::uuid)")
					if query.name == "baseline" {
						if work.workersPlanned == 0 {
							t.Fatal("controlled baseline did not plan parallel startup")
						}
					} else if work.workersPlanned != 0 || work.workersStarted != 0 || work.buffers > 32 {
						t.Fatalf("owning preflight amplified one contract: planned=%d launched=%d buffers=%d", work.workersPlanned, work.workersStarted, work.buffers)
					}
					// The inner join must still ignore a missing unused grant and retain all
					// existing grants, including a contract wider than worker journal pages.
					for _, shape := range []struct {
						name string
						want int
					}{{"thin", 1}, {"synthetic-contract:0", 320}, {"wide", 1024}, {"absent", 0}} {
						var got int
						server.Raise(conn.QueryRow(ctx, "EXECUTE legacy_preflight_plan(md5('"+shape.name+"')::uuid)").Scan(&got))
						if got != shape.want {
							t.Fatalf("%s/%s/%s count=%d want=%d", mode, query.name, shape.name, got, shape.want)
						}
					}
					server.RaisePgResult(conn.Exec(ctx, "DEALLOCATE legacy_preflight_plan"))
					t.Logf("%s/%s workers_planned=%d workers_launched=%d buffers=%d elapsed_ms=%.3f", mode, query.name, work.workersPlanned, work.workersStarted, work.buffers, work.executionMs)
				}
			}
		}, server.OptReadWrite())
	})
}
