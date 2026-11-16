// Execute the owning query through the public signal on temporary relations.
// Transaction time keeps the two-minute boundary exact without wall-clock waits.
package monitor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The standard attested environment owns provisioning; the fixture shadows
// every queried relation and rolls back its only transaction before release.
func connectionOrphansSqlFixture(t *testing.T, callback func(testing.TB, server.PgTx)) {
	t.Helper()
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			server.RaisePgResult(tx.Exec(ctx, `
				SET LOCAL search_path = pg_temp;
				SET LOCAL TIME ZONE 'UTC';
				CREATE TEMP TABLE network_client_handler (
				 handler_id uuid PRIMARY KEY,
				 heartbeat_time timestamp NOT NULL
				) ON COMMIT DROP;
				CREATE TEMP TABLE network_client_connection (
				 handler_id uuid,
				 connected boolean NOT NULL,
				 connect_time timestamp NOT NULL
				) ON COMMIT DROP;
			`))
			callback(t, tx)
		})
	})
}

// Preserve actual query cardinality and errors while rendering integer cells
// in the text representation expected by SignalSource.
func connectionOrphansSqlRows(ctx context.Context, tx server.PgTx, query string) ([]Row, error) {
	result, err := tx.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	rows := []Row{}
	for result.Next() {
		var values [7]int64
		if err := result.Scan(&values[0], &values[1], &values[2], &values[3], &values[4], &values[5], &values[6]); err != nil {
			return nil, err
		}
		row := make(Row, len(values))
		for index, value := range values {
			row[index] = strconv.FormatInt(value, 10)
		}
		rows = append(rows, row)
	}
	return rows, result.Err()
}

// Assert the database row before reducer health. On the grouped pre-fix query,
// the empty-set regression reaches the explicit zero-row causal assertion.
func connectionOrphansSqlRun(t testing.TB, tx server.PgTx, want Row) Alerts {
	t.Helper()
	var rows []Row
	var queryErr error
	queryCount := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		queryCount++
		rows, queryErr = connectionOrphansSqlRows(t.Context(), tx, query)
		return rows, queryErr
	}}
	alerts, err := NewConnectionOrphansSignal().Run(t.Context(), syntheticSettings(source))
	if queryCount != 1 || queryErr != nil {
		t.Fatalf("connection-orphan query execution: calls=%d err=%v", queryCount, queryErr)
	}
	if len(rows) != 1 {
		t.Fatalf("connection-orphan SQL aggregate: expected one seven-column row, got %d rows", len(rows))
	}
	if got := strings.Join(rows[0], ","); got != strings.Join(want, ",") {
		t.Fatalf("connection-orphan SQL aggregate: got %s, want %s", got, strings.Join(want, ","))
	}
	if err != nil {
		t.Fatalf("connection-orphan public adapter rejected a complete aggregate: %v", err)
	}
	return alerts
}

// An actually empty source population must produce seven authoritative zeros.
func TestConnectionOrphansSignalSqlEmptyPopulation(t *testing.T) {
	connectionOrphansSqlFixture(t, func(t testing.TB, tx server.PgTx) {
		alerts := connectionOrphansSqlRun(t, tx, Row{"0", "0", "0", "0", "0", "0", "0"})
		if len(alerts) != 0 {
			t.Fatal("empty connection and handler tables produced an orphan alert")
		}
	})
}

// Handler observations remain authoritative even when no connection is open.
func TestConnectionOrphansSignalSqlHandlerOnlyPopulation(t *testing.T) {
	connectionOrphansSqlFixture(t, func(t testing.TB, tx server.PgTx) {
		server.RaisePgResult(tx.Exec(t.Context(), `
			INSERT INTO network_client_handler VALUES
			 ($1, now() - interval '2 minutes'),
			 ($2, now() - interval '3 minutes');
		`, server.NewId(), server.NewId()))
		alerts := connectionOrphansSqlRun(t, tx, Row{"0", "0", "0", "0", "2", "1", "0"})
		if len(alerts) != 0 {
			t.Fatal("handlers without connected rows produced an orphan alert")
		}
	})
}

// Durable disconnected history must not create an open population or an age.
func TestConnectionOrphansSignalSqlDisconnectedPopulation(t *testing.T) {
	connectionOrphansSqlFixture(t, func(t testing.TB, tx server.PgTx) {
		server.RaisePgResult(tx.Exec(t.Context(), `
			INSERT INTO network_client_connection VALUES
			 ($1, false, now() - interval '1 day'),
			 (NULL, false, now() - interval '2 days');
		`, server.NewId()))
		alerts := connectionOrphansSqlRun(t, tx, Row{"0", "0", "0", "0", "0", "0", "0"})
		if len(alerts) != 0 {
			t.Fatal("disconnected history produced an orphan alert")
		}
	})
}

// A connected row at exactly two minutes is still outside the mature set.
func TestConnectionOrphansSignalSqlFreshOrphans(t *testing.T) {
	connectionOrphansSqlFixture(t, func(t testing.TB, tx server.PgTx) {
		server.RaisePgResult(tx.Exec(t.Context(), `
			INSERT INTO network_client_connection VALUES
			 ($1, true, now() - interval '119 seconds'),
			 ($1, true, now() - interval '120 seconds');
		`, server.NewId()))
		alerts := connectionOrphansSqlRun(t, tx, Row{"2", "2", "0", "120", "0", "0", "0"})
		if len(alerts) != 0 {
			t.Fatal("fresh and exact-boundary orphan rows produced an orphan alert")
		}
	})
}

// Linkage still exists, but a stale owner cannot establish fresh lease coverage.
// Visibility must not invent a missing-handler or dead-process incident.
func TestConnectionOrphansSignalSqlOwnedPopulation(t *testing.T) {
	connectionOrphansSqlFixture(t, func(t testing.TB, tx server.PgTx) {
		handlerId := server.NewId()
		server.RaisePgResult(tx.Exec(t.Context(), `
			INSERT INTO network_client_handler VALUES ($1, now() - interval '3 minutes');
		`, handlerId))
		server.RaisePgResult(tx.Exec(t.Context(), `
			INSERT INTO network_client_connection VALUES
			 ($1, true, now() - interval '10 minutes'),
			 ($1, true, now() - interval '30 seconds');
		`, handlerId))
		alerts := connectionOrphansSqlRun(t, tx, Row{"2", "0", "0", "0", "1", "1", "2"})
		if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || alerts[0].Frame != "handler-lease-coverage" {
			t.Fatal("stale existing handlers granted healthy connection ownership")
		}
	})
}

// Existing owners, shared owners, stale owners, fresh and mature missing
// owners, null owners and disconnected history retain their separate counts.
func TestConnectionOrphansSignalSqlPopulatedCounts(t *testing.T) {
	connectionOrphansSqlFixture(t, func(t testing.TB, tx server.PgTx) {
		liveHandlerId, staleHandlerId, missingHandlerId := server.NewId(), server.NewId(), server.NewId()
		server.RaisePgResult(tx.Exec(t.Context(), `
			INSERT INTO network_client_handler VALUES
			 ($1, now()), ($2, now() - interval '3 minutes');
		`, liveHandlerId, staleHandlerId))
		server.RaisePgResult(tx.Exec(t.Context(), `
			INSERT INTO network_client_connection VALUES
			 ($1, true, now() - interval '900 seconds'),
			 ($1, true, now() - interval '30 seconds'),
			 ($2, true, now() - interval '600 seconds'),
			 ($3, true, now() - interval '600 seconds'),
			 ($3, true, now() - interval '30 seconds'),
			 ($3, true, now() - interval '120 seconds'),
			 (NULL, true, now() - interval '300 seconds'),
			 ($3, false, now() - interval '1 day');
		`, liveHandlerId, staleHandlerId, missingHandlerId))
		alerts := connectionOrphansSqlRun(t, tx, Row{"7", "4", "2", "600", "2", "1", "1"})
		if len(alerts) != 2 || alerts[0].Class != "cannot-observe" {
			t.Fatal("stale lease visibility erased independently proved mature orphans")
		}
		alert := alerts[1]
		if alert.Class != "connection-orphans" || alert.Frame != "missing-handler" || alert.Sustain != 2 || alert.Severity != SeverityPage {
			t.Fatal("mature orphan population changed alert identity or sustain")
		}
		wantObserved := "connected_rows=7 orphan_rows=4 mature_orphan_rows=2 oldest_orphan_age_s=600 handler_rows=2 stale_handler_rows=1 stale_owned_open_rows=1"
		if alert.Observed != wantObserved {
			t.Fatalf("mature orphan alert lost aggregate evidence: %s", alert.Observed)
		}
		requireAlertOmits(t, alert, liveHandlerId.String(), staleHandlerId.String(), missingHandlerId.String())
	})
}

// A malformed queried relation must reach the adapter as a database error,
// even though both fixture populations would otherwise be empty and healthy.
func TestConnectionOrphansSignalSqlQueryFailure(t *testing.T) {
	connectionOrphansSqlFixture(t, func(t testing.TB, tx server.PgTx) {
		server.RaisePgResult(tx.Exec(t.Context(), `ALTER TABLE network_client_handler DROP COLUMN heartbeat_time`))
		var queryErr error
		queryCount := 0
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			queryCount++
			rows, err := connectionOrphansSqlRows(t.Context(), tx, query)
			queryErr = err
			return rows, err
		}}
		alerts, err := NewConnectionOrphansSignal().Run(t.Context(), syntheticSettings(source))
		if queryCount != 1 || queryErr == nil || !errors.Is(err, queryErr) || len(alerts) != 0 {
			t.Fatal("database query failure became an authoritative zero aggregate")
		}
	})
}
