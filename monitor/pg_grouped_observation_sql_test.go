// Run the unchanged owning SELECTs on connection-local synthetic relations.
// These controls require separately admitted local-fixture qualification.
package monitor

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// COPY uses PostgreSQL's actual textual projection and the production CSV
// decoder, including int widths, numeric sums, coalesce and empty groupings.
func pgGroupedActualSqlRows(t testing.TB, conn server.PgConn, query string) []Row {
	t.Helper()
	var output bytes.Buffer
	_, err := conn.Conn().PgConn().CopyTo(t.Context(), &output,
		"COPY ("+strings.TrimSuffix(strings.TrimSpace(query), ";")+") TO STDOUT (FORMAT CSV)")
	if err != nil {
		t.Fatal("actual grouped SELECT failed on its local fixture")
	}
	rows, err := parsePgRows(output.String())
	if err != nil {
		t.Fatal("actual grouped CSV projection failed")
	}
	result := make([]Row, len(rows))
	for i, row := range rows {
		result[i] = Row(row)
	}
	return result
}

// Temporary relations shadow catalog/activity and finished-task source names
// on this connection only; every monitored data row is visibly synthetic.
func pgGroupedActualSqlFixture(t testing.TB, conn server.PgConn) {
	t.Helper()
	server.RaisePgResult(conn.Exec(t.Context(), `
CREATE TEMP TABLE pg_stat_activity (
    query_id bigint, backend_type text, state text, pid integer, query text,
    query_start timestamptz, wait_event_type text, wait_event text,
    application_name text, client_addr inet
);
CREATE TEMP TABLE pg_stat_statements (
    queryid bigint, dbid oid, calls bigint, total_exec_time double precision,
    max_exec_time double precision, query text
);
CREATE TEMP TABLE finished_task (
    task_id uuid PRIMARY KEY, function_name text,
    run_start_time timestamptz, run_end_time timestamptz
);
`))
}

// Complete empty sets remain healthy through all four real SELECTs and signals.
// Real int/count/rounding projections then retain maintenance and true failures.
func TestPgGroupedActualSqlProjectionAndPublicBands(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			pgGroupedActualSqlFixture(t, conn)
			source := &syntheticSource{
				postgresFn: func(query string) ([]Row, error) { return pgGroupedActualSqlRows(t, conn, query), nil },
				localFn:    func(string, ...string) (string, error) { return "", nil },
			}
			check := func(signal Signal, class string) {
				t.Helper()
				alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(context.Background())
				if err != nil {
					t.Fatal("actual grouped SELECT lost public result")
				}
				if class == "" {
					if len(alerts) != 0 {
						t.Fatal("qualified empty or maintenance SELECT became anomalous")
					}
					return
				}
				if len(alerts) != 1 || alerts[0].Class != class {
					t.Fatal("actual grouped SELECT lost its required public class")
				}
				if strings.Contains(alerts.Markdown(), "private-grouped-sentinel") {
					t.Fatal("actual SQL sample escaped privacy boundary")
				}
			}
			for _, signal := range []Signal{NewActiveQueriesSignal(), NewWaitEventsSignal(), NewCloseDurationSignal(), NewNetEscrowSignal()} {
				check(signal, "")
			}
			server.RaisePgResult(conn.Exec(t.Context(), `
INSERT INTO pg_stat_activity VALUES
(81, 'client backend', 'active', 2147483001,
 'REINDEX TABLE CONCURRENTLY synthetic_table', clock_timestamp()-interval '10 minutes',
 'Lock', 'virtualxid', 'synthetic-worker', '192.0.2.1');`))
			check(NewActiveQueriesSignal(), "")
			check(NewWaitEventsSignal(), "")
			server.RaisePgResult(conn.Exec(t.Context(), `
UPDATE pg_stat_activity SET query_start=clock_timestamp()-interval '3 hours';`))
			check(NewActiveQueriesSignal(), "persistent-active-query")
			check(NewWaitEventsSignal(), "wait-event-cluster")
			server.RaisePgResult(conn.Exec(t.Context(), `
TRUNCATE pg_stat_activity;
INSERT INTO pg_stat_activity
SELECT 82, 'client backend', 'active', 2147483000+i,
       'SELECT ''private-grouped-sentinel''', clock_timestamp(),
       'Client', 'ClientRead', '', NULL
FROM generate_series(1,5) i;`))
			check(NewWaitEventsSignal(), "")
			server.RaisePgResult(conn.Exec(t.Context(), `
UPDATE pg_stat_activity SET query_start=clock_timestamp()-interval '2 minutes';`))
			check(NewWaitEventsSignal(), "wait-event-cluster")
			server.RaisePgResult(conn.Exec(t.Context(), `
TRUNCATE pg_stat_activity;
INSERT INTO finished_task VALUES
('00000000-0000-0000-0000-000000000001', 'synthetic.work.CloseExpiredContracts', now()-interval '121 seconds', now()),
('00000000-0000-0000-0000-000000000002', 'synthetic.work.ReconcileNetEscrow', now()-interval '121 seconds', now());`))
			check(NewCloseDurationSignal(), "close-duration-overrun")
			check(NewNetEscrowSignal(), "netescrow-reconcile-overrun")
			server.RaisePgResult(conn.Exec(t.Context(), `
TRUNCATE finished_task;
INSERT INTO pg_stat_activity VALUES
(83, 'client backend', 'active', 2147483001,
 'SELECT requested_balance.balance_id FROM unnest(array[1]) requested_balance CROSS JOIN LATERAL (SELECT 1 WHERE transfer_contract.outcome IS NULL) synthetic_page',
 clock_timestamp()-interval '31 minutes', NULL, NULL, 'synthetic-worker', NULL);`))
			check(NewNetEscrowSignal(), "netescrow-reservation-detached")
		})
	})
}

// Causal RED through the actual query: PG accepts NaN in a float column, but the
// optional lifetime profile cannot turn it into valid task attribution.
func TestPgGroupedActualSqlNonfiniteProfileRetainsIncident(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			pgGroupedActualSqlFixture(t, conn)
			server.RaisePgResult(conn.Exec(t.Context(), `
INSERT INTO finished_task VALUES
('00000000-0000-0000-0000-000000000001', 'synthetic.work.ReconcileNetEscrow', now()-interval '121 seconds', now());
INSERT INTO pg_stat_statements
SELECT 81, oid, 1, 'NaN'::float8, 1,
       'SELECT synthetic_value WHERE transfer_escrow.balance_id = ANY(array[1]) AND transfer_contract.outcome IS NULL'
FROM pg_database WHERE datname=current_database();`))
			source := &syntheticSource{
				postgresFn: func(query string) ([]Row, error) { return pgGroupedActualSqlRows(t, conn, query), nil },
				localFn:    func(string, ...string) (string, error) { return "", nil },
			}
			alerts, err := NewWithSignals(syntheticSettings(source), NewNetEscrowSignal()).Run(context.Background())
			if err != nil {
				t.Fatal("actual profile failure discarded completed incident")
			}
			classes := map[string]int{}
			for _, alert := range alerts {
				classes[alert.Class]++
			}
			if classes["cannot-observe"] != 1 || classes["netescrow-reconcile-overrun"] != 1 || len(alerts) != 2 {
				t.Fatal("A5 actual nonfinite statement profile crossed attribution authority")
			}
			if strings.Contains(alerts.Markdown(), "reservation_page_calls=") {
				t.Fatal("actual nonfinite timing reached attribution")
			}
		})
	})
}
