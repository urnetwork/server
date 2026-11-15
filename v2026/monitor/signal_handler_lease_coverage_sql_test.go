// The exact owning SELECTs execute against transaction-local temporary tables.
// No production data, precomputed aggregate, wall-clock sleep or native census.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func handlerLeaseSqlFixture(t *testing.T, callback func(testing.TB, server.PgTx)) {
	t.Helper()
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			tx, err := conn.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			server.RaisePgResult(tx.Exec(t.Context(), `
SET LOCAL search_path=pg_temp;
SET LOCAL TIME ZONE 'UTC';
CREATE TEMP TABLE network_client_handler(handler_id text PRIMARY KEY,heartbeat_time timestamp NOT NULL) ON COMMIT DROP;
CREATE TEMP TABLE network_client_connection(client_id text,handler_id text,connect_time timestamp NOT NULL,connected boolean NOT NULL) ON COMMIT DROP;
CREATE TEMP TABLE network_client(client_id text PRIMARY KEY,active boolean NOT NULL,source_client_id text) ON COMMIT DROP;
CREATE TEMP TABLE provide_key(client_id text,provide_mode integer) ON COMMIT DROP;
CREATE TEMP TABLE network_client_location_reliability(client_id text,connected boolean,valid boolean) ON COMMIT DROP;
CREATE TEMP TABLE provider_egress_health(measured_at timestamp,total_count bigint,ok_count bigint,tls_authentication_failure boolean) ON COMMIT DROP;
CREATE TEMP TABLE provider_egress_location(location_id text) ON COMMIT DROP;
CREATE TEMP TABLE location(location_id text,location_type text,country_code text) ON COMMIT DROP;
INSERT INTO location VALUES('12345678-1234-4234-8234-123456789abc','country','us');
`))
			callback(t, tx)
		})
	})
}

// Native pgx values preserve the actual old/new query width for causal RED.
func handlerLeaseSqlRows(ctx context.Context, tx server.PgTx, query string) ([]Row, error) {
	result, err := tx.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	rows := []Row{}
	for result.Next() {
		values, err := result.Values()
		if err != nil {
			return nil, err
		}
		row := make(Row, len(values))
		for i, value := range values {
			if value != nil {
				row[i] = fmt.Sprint(value)
			}
		}
		rows = append(rows, row)
	}
	return rows, result.Err()
}

func handlerLeaseSqlRun(t testing.TB, tx server.PgTx, selection, populated bool) Alerts {
	t.Helper()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) { return handlerLeaseSqlRows(t.Context(), tx, query) }, redisFn: handlerLeaseTestCache(t, populated)}
	var signal Signal = NewConnectionOrphansSignal()
	if selection {
		signal = NewSelectionPopulationSignal()
	}
	alerts, err := signal.Run(t.Context(), syntheticSettings(source))
	if err != nil {
		t.Fatal("exact SQL source failed before lease authority assertion")
	}
	return alerts
}

func TestHandlerLeaseCoverageOrphansSqlCausalAndBoundaries(t *testing.T) {
	handlerLeaseSqlFixture(t, func(t testing.TB, tx server.PgTx) {
		for _, c := range []struct {
			name                  string
			age                   int
			open, orphan          bool
			wantUnknown, wantPage bool
		}{
			{name: "stale open owner", age: 121, open: true, wantUnknown: true},
			{name: "exact120 boundary", age: 120, open: true},
			{name: "fresh owner", age: 119, open: true},
			{name: "unused expired owner", age: 600},
			{name: "mature orphan", age: 0, orphan: true, wantPage: true},
			{name: "independent orphan and stale owner", age: 121, open: true, orphan: true, wantUnknown: true, wantPage: true},
		} {
			server.RaisePgResult(tx.Exec(t.Context(), `TRUNCATE network_client_connection,network_client_handler`))
			server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO network_client_handler VALUES('private-handler-lease',now()-$1::integer*interval '1 second')`, c.age))
			server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO network_client_connection VALUES('private-provider','private-handler-lease',now()-interval '10 minutes',$1)`, c.open))
			if c.orphan {
				server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO network_client_connection VALUES('private-orphan','private-missing-handler',now()-interval '3 minutes',true)`))
			}
			alerts := handlerLeaseSqlRun(t, tx, false, false)
			if handlerLeaseTestHasVisibility(alerts) != c.wantUnknown {
				t.Fatalf("%s: stale existing handlers granted healthy connection ownership", c.name)
			}
			if (requireAlertClassCount(alerts, "connection-orphans") > 0) != c.wantPage {
				t.Fatal("lease coverage changed independently proved orphan policy")
			}
			for _, alert := range alerts {
				requireAlertOmits(t, alert, "private-handler-lease", "private-provider", "private-orphan", "private-missing-handler")
			}
		}
	})
}

func TestHandlerLeaseCoverageSelectionSqlCausalAndPopulation(t *testing.T) {
	handlerLeaseSqlFixture(t, func(t testing.TB, tx server.PgTx) {
		for _, c := range []struct {
			name                                           string
			age, mode, eligible                            int
			open, active, derived, duplicateKey, populated bool
			wantUnknown, wantPage                          bool
		}{
			{name: "cleared location still covered", age: 121, mode: 3, open: true, active: true, wantUnknown: true},
			{name: "Network provider still covered", age: 121, mode: 1, open: true, active: true, wantUnknown: true},
			{name: "duplicate provide keys count once", age: 121, mode: 3, open: true, active: true, duplicateKey: true, wantUnknown: true},
			{name: "exact120 boundary", age: 120, mode: 3, open: true, active: true},
			{name: "fresh independent lease", age: 119, mode: 3, open: true, active: true},
			{name: "stale unused handler", age: 600, mode: 3, active: true},
			{name: "consumer without provide key", age: 600, open: true, active: true},
			{name: "inactive provider", age: 600, mode: 3, open: true},
			{name: "derived provider", age: 600, mode: 3, open: true, active: true, derived: true},
			{name: "small eligible deployment", age: 119, mode: 3, open: true, active: true, eligible: 1000},
			{name: "independent empty export", age: 121, mode: 3, open: true, active: true, eligible: 1001, wantUnknown: true, wantPage: true},
			{name: "populated cache does not attest lease", age: 121, mode: 3, open: true, active: true, populated: true, wantUnknown: true},
		} {
			server.RaisePgResult(tx.Exec(t.Context(), `TRUNCATE network_client_connection,network_client_handler,network_client,provide_key,network_client_location_reliability`))
			server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO network_client_handler VALUES('private-handler-lease',now()-$1::integer*interval '1 second')`, c.age))
			server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO network_client VALUES('private-provider',$1,CASE WHEN $2 THEN 'private-parent' ELSE NULL END)`, c.active, c.derived))
			server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO network_client_connection VALUES('private-provider','private-handler-lease',now()-interval '10 minutes',$1)`, c.open))
			server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO network_client_location_reliability VALUES('private-provider',false,false)`))
			if c.mode > 0 {
				server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO provide_key VALUES('private-provider',$1)`, c.mode))
			}
			if c.duplicateKey {
				server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO provide_key VALUES('private-provider',1)`))
			}
			if c.eligible > 0 {
				server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO network_client SELECT 'eligible-'||i,true,NULL FROM generate_series(1,$1::integer) i`, c.eligible))
				server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO provide_key SELECT 'eligible-'||i,3 FROM generate_series(1,$1::integer) i`, c.eligible))
				server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO network_client_location_reliability SELECT 'eligible-'||i,true,true FROM generate_series(1,$1::integer) i`, c.eligible))
			}
			alerts := handlerLeaseSqlRun(t, tx, true, c.populated)
			if handlerLeaseTestHasVisibility(alerts) != c.wantUnknown {
				t.Fatalf("%s: zero heartbeat-qualified eligibility granted healthy selection recovery or wrong source population", c.name)
			}
			if (requireAlertClassCount(alerts, "selection-empty") > 0) != c.wantPage {
				t.Fatal("lease coverage changed the existing1000 empty-cache policy")
			}
			for _, alert := range alerts {
				if alert.Class == "cannot-observe" && alert.Frame == "handler-lease-coverage" && !strings.Contains(alert.Observed, "stale_owned_open_rows=1 ") {
					t.Fatal("provide keys multiplied open connection coverage")
				}
				requireAlertOmits(t, alert, "private-handler-lease", "private-provider", "private-parent")
			}
		}
	})
}
