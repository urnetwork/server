package monitor

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// This is the pre-fix statement, retained only as an equivalence and planner
// regression control. An inlined normalization is evaluated in each CASE arm.
func pgHistoryInlineControlSQL() string {
	return `WITH history AS MATERIALIZED (
 SELECT queryid::text q,sum(calls)::float8 calls,sum(total_exec_time)::float8 exec_ms,max(max_exec_time)::float8 max_ms,
 min(` + pgSampleFamily("normalized") + `) family
 FROM (SELECT *,lower(btrim(regexp_replace(left(query,2048),'\s+',' ','g'))) normalized FROM pg_stat_statements
 WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())) s
 GROUP BY queryid
 ), selected AS (SELECT * FROM history ORDER BY exec_ms DESC,q LIMIT 5000)
 SELECT json_build_object('kind','history','sample',0,'at',extract(epoch FROM clock_timestamp()),
 'reset',extract(epoch FROM (SELECT stats_reset FROM pg_stat_statements_info)),
 'total',(SELECT count(*) FROM history), 'rows',coalesce((SELECT json_agg(json_build_array(q,calls,exec_ms,max_ms,family)) FROM selected),'[]'::json));`
}

func TestPgQuerySampleHistorySQLNormalizationAndBounds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			pgHistorySQLNormalizationAndBounds(t, conn)
		})
	})
}

func pgHistorySQLNormalizationAndBounds(t testing.TB, conn server.PgConn) {
	t.Helper()
	ctx := t.Context()
	// Temporary relations shadow only this connection's catalog views. The
	// owning statement itself is unchanged, including current-database filtering.
	server.RaisePgResult(conn.Exec(ctx, `CREATE TEMP TABLE pg_stat_statements (
 dbid oid,queryid bigint,calls bigint,total_exec_time float8,max_exec_time float8,query text);
 CREATE TEMP TABLE pg_stat_statements_info (stats_reset timestamptz);
 INSERT INTO pg_stat_statements_info VALUES ('2026-10-01T00:00:00Z');`))
	read := func(sql string) pgSampleWire {
		t.Helper()
		var raw []byte
		server.Raise(conn.QueryRow(ctx, sql).Scan(&raw))
		var w pgSampleWire
		if err := json.Unmarshal(raw, &w); err != nil {
			t.Fatal(err)
		}
		if w.Kind != "history" || w.Sample != 0 || w.At <= 0 || w.Reset == nil {
			t.Fatal("history frame lost its source/reset identity")
		}
		return w
	}
	if empty := read(pgSampleHistorySQL(0)); empty.Total != 0 || len(empty.Rows) != 0 {
		t.Fatal("empty history did not retain a qualified empty frame")
	}
	queries := []struct{ text, family string }{
		{"\n\t COMMIT ;", "commit"},
		{" SELECT balance_id, paid, balance_byte_count, start_time, end_time\n FROM transfer_balance\n FOR UPDATE", "grant_all_lock"},
		{"SELECT snapshot.reserved_byte_count, balance.end_time FROM unnest(array[1]) keys LEFT JOIN transfer_balance_net_escrow_snapshot snapshot ON true", "reservation_snapshot_read"},
		{"INSERT INTO transfer_balance_net_escrow_snapshot VALUES ($1)", "reservation_snapshot_publish"},
		{"SELECT requested_balance.balance_id, COALESCE(revision.revision,0) FROM unnest($1) requested_balance LEFT JOIN transfer_balance_net_escrow_revision revision ON true CROSS JOIN LATERAL (SELECT SUM(selected_escrow.balance_byte_count)) selected", "reservation_census_prefix"},
		{"SELECT * FROM transfer_escrow WHERE contract_id=$1", "escrow_access"},
		// A healthy backup COPY shares the table family with application work.
		// The application declaration is separate evidence, not a query owner.
		{"COPY public.contract_close (contract_id) TO stdout", "contract_close_access"},
		{"SELECT 1 /* private-history-query-sentinel */", "other"},
		{strings.Repeat("x", 2048) + " transfer_escrow", "other"},
	}
	for i, q := range queries {
		server.RaisePgResult(conn.Exec(ctx, `INSERT INTO pg_stat_statements
 SELECT oid,$1,2,10,3,$2 FROM pg_database WHERE datname=current_database()`, i+1, q.text))
	}
	// Identical IDs can have multiple rows (for example across users). Preserve
	// sum/max and the existing minimum family rule before ranking the aggregate.
	server.RaisePgResult(conn.Exec(ctx, `INSERT INTO pg_stat_statements
 SELECT oid,1,4,20,7,'ROLLBACK' FROM pg_database WHERE datname=current_database();
 INSERT INTO pg_stat_statements VALUES (0,999,999,999999,999,'SELECT * FROM transfer_escrow');
 INSERT INTO pg_stat_statements SELECT oid,900,1,1,1,NULL FROM pg_database WHERE datname=current_database();`))
	current, control := read(pgSampleHistorySQL(0)), read(pgHistoryInlineControlSQL())
	current.At, control.At = 0, 0
	if !reflect.DeepEqual(current, control) {
		t.Fatal("materialized history changed grouped endpoints or family selection")
	}
	if current.Total != len(queries)+1 || len(current.Rows) != current.Total {
		t.Fatal("history did not retain exact current-database unique-ID coverage")
	}
	for _, row := range current.Rows {
		var q, family string
		server.Raise(json.Unmarshal(row[0], &q))
		server.Raise(json.Unmarshal(row[4], &family))
		if q == "1" {
			if string(row[1]) != "6" || string(row[2]) != "30" || string(row[3]) != "7" || family != "commit" {
				t.Fatal("duplicate-ID aggregation changed")
			}
		} else if q == "900" {
			if family != "other" {
				t.Fatal("missing history text gained an attributed family")
			}
		} else {
			for i, item := range queries {
				if q == strconv.Itoa(i+1) && family != item.family {
					t.Fatalf("family control %d = %s, want %s", i, family, item.family)
				}
			}
		}
		for _, cell := range row {
			if strings.Contains(string(cell), "private-history-query-sentinel") {
				t.Fatal("history exported query text")
			}
		}
	}
	// Verify the actual planner boundary, without a timing threshold sensitive
	// to the test host. The former expression is repeated in CASE arms; the
	// owning query must contain a single evaluation site in the physical plan.
	countPlanNormalizers := func(sql string) int {
		t.Helper()
		var plan []byte
		server.Raise(conn.QueryRow(ctx, "EXPLAIN (VERBOSE, FORMAT JSON) "+sql).Scan(&plan))
		return strings.Count(string(plan), "regexp_replace(")
	}
	if n := countPlanNormalizers(pgHistoryInlineControlSQL()); n < 10 {
		t.Fatalf("inline regression control did not expose repeated normalization: %d", n)
	}
	if n := countPlanNormalizers(pgSampleHistorySQL(0)); n != 1 {
		t.Fatalf("history planner repeats normalization %d times, want one", n)
	}
	server.RaisePgResult(conn.Exec(ctx, `TRUNCATE pg_stat_statements;
 INSERT INTO pg_stat_statements SELECT (SELECT oid FROM pg_database WHERE datname=current_database()),
 i,1,i,1,'SELECT 1' FROM generate_series(1,5001) i;
 INSERT INTO pg_stat_statements VALUES (0,999999,1,9999999,1,'SELECT 1');`))
	capped := read(pgSampleHistorySQL(0))
	if capped.Total != 5001 || len(capped.Rows) != 5000 || string(capped.Rows[0][0]) != `"5001"` || string(capped.Rows[4999][0]) != `"2"` {
		t.Fatal("history cap lost its unique total or lifetime execution-time ranking")
	}
}
