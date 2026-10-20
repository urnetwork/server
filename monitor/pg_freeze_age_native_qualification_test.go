// Execute the owning SQL body on the Root-allocated PostgreSQL18 TestEnv.
// Real catalogs and deterministic temporary catalog shadows are separate roots.
// The psql/SSH transport and a real anti-wraparound worker remain outside proof.
package monitor

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

const pgFreezeAgeNativeHost = "pg-freeze-local-fixture"
const pgFreezeAgeNativeCanary = "PRIVATE_CANARY_FREEZE_NATIVE"

// The only SQL adaptation binds the psql hostname variable. No CTE, relation,
// predicate, projection, expression, ordering or bound is replaced.
func pgFreezeAgeNativeSql(t testing.TB) string {
	t.Helper()
	prologue, body, found := strings.Cut(pgFreezeAgeQuery, "\n")
	if !found || prologue != "\\set monitor_primary_hostname `hostname -s 2>/dev/null || printf ' unavailable'`" ||
		strings.Count(body, ":'monitor_primary_hostname'") != 1 {
		t.Fatal("native qualification lost the exact producer SQL boundary")
	}
	return strings.Replace(body, ":'monitor_primary_hostname'", "$1", 1)
}

// SQL errors stay fixed; private rows, statements and credentials are not logged.
func pgFreezeAgeNativeExec(t testing.TB, ctx context.Context, conn server.PgConn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatal("native freeze-age fixture statement failed")
	}
}

// The existing TestEnv owns the ephemeral database and all outer fixture
// custody. One transaction owns every table and local setting; rollback is
// bounded and joined even if the assertion context ends.
func pgFreezeAgeNativeTransaction(t testing.TB, conn server.PgConn, run func(context.Context)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	pgFreezeAgeNativeExec(t, ctx, conn, "BEGIN ISOLATION LEVEL REPEATABLE READ")
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := conn.Exec(cleanupCtx, "ROLLBACK"); err != nil {
			t.Error("native freeze-age transaction cleanup failed")
		}
	}()
	pgFreezeAgeNativeExec(t, ctx, conn, "SET LOCAL statement_timeout = '5s'; SET LOCAL lock_timeout = '1s'")
	var version int
	var recovery, readAllStats bool
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,
 pg_is_in_recovery(), pg_has_role(current_user,'pg_read_all_stats','USAGE') OR
 (SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname=current_user)`).Scan(&version, &recovery, &readAllStats); err != nil {
		t.Fatal("native PostgreSQL capability observation failed")
	}
	if version < 180000 || version >= 190000 || recovery {
		t.Fatal("native freeze-age qualification requires an allocated PostgreSQL18 primary")
	}
	t.Logf("native_pg_version_num=%d read_all_stats=%t sql_body_only=true real_antiwrap_worker_observed=false", version, readAllStats)
	run(ctx)
}

// Execute, drain and close the real PG result; each emitted record must have
// exactly eighteen text cells and the producer's finite row/cell bounds.
func pgFreezeAgeNativeRows(t testing.TB, ctx context.Context, conn server.PgConn) []Row {
	t.Helper()
	rows, err := conn.Query(ctx, pgFreezeAgeNativeSql(t), pgFreezeAgeNativeHost)
	if err != nil {
		t.Fatal("native freeze-age producer query failed")
	}
	defer rows.Close()
	if len(rows.FieldDescriptions()) != 18 {
		t.Fatal("native producer projection width changed")
	}
	result := []Row{}
	for rows.Next() {
		if len(result) >= 4098 {
			t.Fatal("native producer exceeded its record bound")
		}
		row := make(Row, 18)
		destinations := make([]any, 18)
		for i := range row {
			destinations[i] = &row[i]
		}
		if rows.Scan(destinations...) != nil {
			t.Fatal("native producer emitted a non-text or null cell")
		}
		for _, cell := range row {
			if len(cell) > 256 || strings.Contains(cell, pgFreezeAgeNativeCanary) {
				t.Fatal("native producer exceeded its fixed private-cell boundary")
			}
		}
		result = append(result, row)
	}
	if rows.Err() != nil {
		t.Fatal("native producer result did not complete")
	}
	return result
}

// The native source preserves the signal's own child context at the SQL call.
// Other source methods stay inert; this driver owns no host or subprocess path.
type pgFreezeAgeNativeSource struct {
	*syntheticSource
	t     testing.TB
	conn  server.PgConn
	rows  []Row
	calls int
}

// The source never fabricates rows or substitutes the outer fixture context.
func (self *pgFreezeAgeNativeSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	self.calls++
	if query != pgFreezeAgeQuery {
		self.t.Fatal("native signal requested a different producer")
	}
	self.rows = pgFreezeAgeNativeRows(self.t, ctx, self.conn)
	return self.rows, nil
}

// The source seam only routes the public signal to the real connection. It
// never fabricates result rows; JSON and Markdown see the actual projection.
func pgFreezeAgeNativeObserve(t testing.TB, ctx context.Context, conn server.PgConn, signal Signal) (Alerts, []Row) {
	t.Helper()
	source := &pgFreezeAgeNativeSource{syntheticSource: &syntheticSource{}, t: t, conn: conn}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: pgFreezeAgeNativeHost, Roles: []string{"pg-primary"}}}
	settings.Now = time.Now
	alerts, err := signal.Run(ctx, settings)
	if err != nil || source.calls != 1 {
		t.Fatal("native producer did not reach the public signal exactly once")
	}
	var jsonl bytes.Buffer
	if WriteAlertsJSONL(&jsonl, alerts) != nil {
		t.Fatal("native public JSON rendering failed")
	}
	if strings.Contains(jsonl.String(), pgFreezeAgeNativeCanary) || strings.Contains(AlertsMarkdown(alerts), pgFreezeAgeNativeCanary) {
		t.Fatal("native private text entered public output")
	}
	return alerts, source.rows
}

// The real catalogs prove actual source fields, default/lower effective limits,
// ordinary and TOAST coverage, complete framing and native parser compatibility.
func TestPgFreezeAgeNativeCatalogProducer(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			pgFreezeAgeNativeTransaction(t, conn, func(ctx context.Context) {
				pgFreezeAgeNativeExec(t, ctx, conn, `CREATE TABLE freeze_native_plain (id integer);
 CREATE TABLE "PRIVATE_CANARY_FREEZE_NATIVE" (id integer,payload text)
 WITH (autovacuum_freeze_max_age=100000);`)
				var global, configuredFailsafe, count, lowerOid, toastOid int64
				if err := conn.QueryRow(ctx, `SELECT current_setting('autovacuum_freeze_max_age')::bigint,
 current_setting('vacuum_failsafe_age')::bigint,
 (SELECT count(*) FROM pg_catalog.pg_class WHERE relkind IN ('r','t')),
 '"PRIVATE_CANARY_FREEZE_NATIVE"'::regclass::oid::bigint,
 (SELECT reltoastrelid::bigint FROM pg_catalog.pg_class WHERE oid='"PRIVATE_CANARY_FREEZE_NATIVE"'::regclass)`).
					Scan(&global, &configuredFailsafe, &count, &lowerOid, &toastOid); err != nil {
					t.Fatal("real catalog expectation query failed")
				}
				if global <= 100000 || count < 3 || count > 4096 || toastOid == 0 {
					t.Fatal("allocated catalog cannot establish the required default/lower/TOAST controls")
				}
				expected := map[string][3]string{}
				native, err := conn.Query(ctx, `SELECT oid::text,age(relfrozenxid)::text,relfrozenxid::text,relkind::text
 FROM pg_catalog.pg_class WHERE relkind IN ('r','t') ORDER BY oid`)
				if err != nil {
					t.Fatal("independent native catalog census failed")
				}
				for native.Next() {
					var oid, age, frozen, kind string
					if native.Scan(&oid, &age, &frozen, &kind) != nil {
						native.Close()
						t.Fatal("independent native catalog row failed")
					}
					expected[oid] = [3]string{age, frozen, kind}
				}
				readErr := native.Err()
				native.Close()
				if readErr != nil || len(expected) != int(count) {
					t.Fatal("independent native catalog was incomplete")
				}
				alerts, rows := pgFreezeAgeNativeObserve(t, ctx, conn, NewPgFreezeAgeSignal())
				for _, alert := range alerts {
					if alert.unobservable {
						t.Fatal("real catalog producer failed its typed source contract")
					}
				}
				if len(rows) != int(count)+2 || rows[0][0] != "meta" || rows[len(rows)-1][0] != "end" ||
					rows[0][6] != strconv.FormatInt(global, 10) || rows[0][7] != strconv.FormatInt(configuredFailsafe, 10) ||
					rows[0][8] != strconv.FormatInt(count, 10) || rows[len(rows)-1][1] != strconv.FormatInt(count, 10) {
					t.Fatal("native census or settings framing changed")
				}
				seen := map[string]bool{}
				for _, row := range rows[1 : len(rows)-1] {
					want, found := expected[row[1]]
					if !found || seen[row[1]] || row[0] != "relation" || row[2] != want[0] || row[16] != want[1] || row[17] != want[2] {
						t.Fatal("native relation age/identity census diverged")
					}
					seen[row[1]] = true
					limit := global
					if row[1] == strconv.FormatInt(lowerOid, 10) {
						limit = 100000
					}
					if row[3] != strconv.FormatInt(limit, 10) {
						t.Fatal("native effective freeze setting diverged")
					}
				}
				if !seen[strconv.FormatInt(toastOid, 10)] {
					t.Fatal("real TOAST relation was omitted")
				}
			})
		}, server.OptReadWrite())
	})
}

// Temporary relations affect only this connection. PostgreSQL still evaluates
// the exact owning SELECT, age(), setting functions, CASEs, joins and JSON
// version fallbacks. These rows are declared shadows, not observed workers.
func pgFreezeAgeNativeShadows(t testing.TB, ctx context.Context, conn server.PgConn) {
	t.Helper()
	pgFreezeAgeNativeExec(t, ctx, conn, `CREATE TEMP TABLE pg_class (
 oid oid,relfrozenxid xid,relkind "char",reloptions text[]) ON COMMIT DROP;
 CREATE TEMP TABLE pg_stat_all_tables (relid oid,last_autovacuum timestamptz) ON COMMIT DROP;
 CREATE TEMP TABLE pg_stat_progress_vacuum (datid oid,relid oid,pid integer,phase text,
 heap_blks_total bigint,heap_blks_scanned bigint,heap_blks_vacuumed bigint,
 index_vacuum_count bigint,indexes_processed bigint,indexes_total bigint) ON COMMIT DROP;
 CREATE TEMP TABLE pg_stat_activity (pid integer,backend_start timestamptz,query_start timestamptz,
 backend_type text,query text) ON COMMIT DROP;
 INSERT INTO pg_class SELECT 4000000001::oid,
 (((pg_current_xact_id()::text::bigint-current_setting('autovacuum_freeze_max_age')::bigint-1+4294967296)%4294967296)::text)::xid,
 'r',NULL;
 INSERT INTO pg_stat_progress_vacuum SELECT oid,4000000001::oid,1000000001,'vacuuming indexes',100,100,0,0,1,13
 FROM pg_catalog.pg_database WHERE datname=current_database();
 INSERT INTO pg_stat_progress_vacuum VALUES (0,4000000001::oid,1000000002,'vacuuming indexes',100,100,0,0,13,13);
 INSERT INTO pg_stat_activity VALUES
 (1000000001,pg_postmaster_start_time()+interval '1 microsecond',pg_postmaster_start_time()+interval '2 microseconds',
 'autovacuum worker','autovacuum: VACUUM "PRIVATE_CANARY_FREEZE_NATIVE" (to prevent wraparound)'),
 (1000000002,pg_postmaster_start_time()+interval '1 microsecond',pg_postmaster_start_time()+interval '2 microseconds',
 'autovacuum worker','autovacuum: VACUUM "PRIVATE_CANARY_FREEZE_NATIVE" (to prevent wraparound)');`)
}

// Native SQL proves current-database matching, exact generations, strict index
// advancement and the mode/privacy projection; the worker inputs are shadows.
func TestPgFreezeAgeNativeProjectedProgressAndPrivacy(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			pgFreezeAgeNativeTransaction(t, conn, func(ctx context.Context) {
				pgFreezeAgeNativeShadows(t, ctx, conn)
				signal := NewPgFreezeAgeSignal()
				alerts, rows := pgFreezeAgeNativeObserve(t, ctx, conn, signal)
				if len(rows) != 3 || rows[1][4] != "1000000001" || rows[1][7] != "antiwrap" || rows[1][13] != "1" || len(alerts) != 1 || alerts[0].Class != "pg-freeze-age-risk" {
					t.Fatal("native current-database or first-pass authority changed")
				}
				backend, e1 := strconv.ParseInt(rows[1][5], 10, 64)
				query, e2 := strconv.ParseInt(rows[1][6], 10, 64)
				if e1 != nil || e2 != nil || query-backend != 1 {
					t.Fatal("native source lost microsecond pass identity")
				}
				alerts, _ = pgFreezeAgeNativeObserve(t, ctx, conn, signal)
				if len(alerts) != 1 || alerts[0].Class != "pg-freeze-age-risk" {
					t.Fatal("native unchanged index counters claimed advancement")
				}
				pgFreezeAgeNativeExec(t, ctx, conn, "UPDATE pg_stat_progress_vacuum SET indexes_processed=2 WHERE pid=1000000001")
				alerts, _ = pgFreezeAgeNativeObserve(t, ctx, conn, signal)
				if len(alerts) != 0 {
					t.Fatal("native positive same-pass index work failed recovery")
				}
				pgFreezeAgeNativeExec(t, ctx, conn, "UPDATE pg_stat_activity SET query_start=query_start+interval '1 microsecond' WHERE pid=1000000001; UPDATE pg_stat_progress_vacuum SET indexes_processed=3 WHERE pid=1000000001")
				alerts, _ = pgFreezeAgeNativeObserve(t, ctx, conn, signal)
				if len(alerts) != 1 || alerts[0].Class != "pg-freeze-age-risk" {
					t.Fatal("native new query generation borrowed prior progress")
				}
				for _, mode := range []struct{ query, backend, expected string }{
					{query: "autovacuum: VACUUM PRIVATE_CANARY_FREEZE_NATIVE", backend: "autovacuum worker", expected: "ordinary"},
					{query: "PRIVATE_CANARY_FREEZE_NATIVE", backend: "autovacuum worker", expected: "unknown"},
					{query: "autovacuum: VACUUM PRIVATE_CANARY_FREEZE_NATIVE (to prevent wraparound)", backend: "client backend", expected: "unknown"},
				} {
					pgFreezeAgeNativeExec(t, ctx, conn, "UPDATE pg_stat_activity SET query=$1,backend_type=$2 WHERE pid=1000000001", mode.query, mode.backend)
					alerts, rows = pgFreezeAgeNativeObserve(t, ctx, conn, NewPgFreezeAgeSignal())
					if len(alerts) != 1 || alerts[0].Class != "pg-freeze-age-risk" || rows[1][7] != mode.expected {
						t.Fatal("native mode discriminator gained unsupported authority")
					}
				}
				pgFreezeAgeNativeExec(t, ctx, conn, "ALTER TABLE pg_stat_progress_vacuum DROP COLUMN indexes_processed; ALTER TABLE pg_stat_progress_vacuum DROP COLUMN indexes_total")
				alerts, rows = pgFreezeAgeNativeObserve(t, ctx, conn, NewPgFreezeAgeSignal())
				if len(alerts) != 1 || alerts[0].Class != "pg-freeze-age-risk" || rows[1][13] != "-1" || rows[1][14] != "-1" {
					t.Fatal("native missing version fields became measured zero or erased age risk")
				}
			})
		}, server.OptReadWrite())
	})
}

// The native producer emits at most4096relations while preserving the complete
// denominator. An overflow, even with a valid end marker, stays unknown.
func TestPgFreezeAgeNativeCensusBoundAndMissingProgress(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			pgFreezeAgeNativeTransaction(t, conn, func(ctx context.Context) {
				pgFreezeAgeNativeShadows(t, ctx, conn)
				pgFreezeAgeNativeExec(t, ctx, conn, "DELETE FROM pg_stat_progress_vacuum; DELETE FROM pg_stat_activity")
				alerts, rows := pgFreezeAgeNativeObserve(t, ctx, conn, NewPgFreezeAgeSignal())
				if len(alerts) != 1 || alerts[0].Class != "pg-freeze-age-risk" || rows[1][4] != "0" || rows[1][7] != "none" || rows[1][8] != "none" {
					t.Fatal("native absent pass erased independent age risk")
				}
				pgFreezeAgeNativeExec(t, ctx, conn, `TRUNCATE pg_class;
 INSERT INTO pg_class SELECT i::oid,(pg_current_xact_id()::text::bigint%4294967296)::text::xid,'r',NULL FROM generate_series(1,4096) i;`)
				alerts, rows = pgFreezeAgeNativeObserve(t, ctx, conn, NewPgFreezeAgeSignal())
				if len(alerts) != 0 || len(rows) != 4098 || rows[0][8] != "4096" || rows[4097][1] != "4096" {
					t.Fatal("native exact complete cap failed")
				}
				pgFreezeAgeNativeExec(t, ctx, conn, "INSERT INTO pg_class SELECT 4097::oid,(pg_current_xact_id()::text::bigint%4294967296)::text::xid,'t',NULL")
				alerts, rows = pgFreezeAgeNativeObserve(t, ctx, conn, NewPgFreezeAgeSignal())
				if len(rows) != 4098 || rows[0][8] != "4097" || rows[4097][1] != "4096" || len(alerts) != 1 || !alerts[0].unobservable || alerts[0].Class != "pg-freeze-age-unobservable" {
					t.Fatal("native over-cap census acquired subset health")
				}
			})
		}, server.OptReadWrite())
	})
}
