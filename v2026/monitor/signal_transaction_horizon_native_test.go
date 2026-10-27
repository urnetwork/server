// The transaction horizon read (SIGNALS.md §2.4l) on the test PostgreSQL: a
// session holding a transaction id is a ranked holder, the census counts
// slots and prepared transactions even when there are none, and no statement
// text leaves the database. Database-backed.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// syntheticSettings names this host the primary
const transactionHorizonNativeHost = "pg-1"
const transactionHorizonNativeCanary = "PRIVATE_CANARY_HORIZON_NATIVE"

// The only adaptation binds the psql hostname variable, as the freeze-age
// native qualification does.
func transactionHorizonNativeSql(t testing.TB, query string) string {
	t.Helper()
	prologue, body, found := strings.Cut(query, "\n")
	if !found || prologue != pgPrimaryHostnameMetaCommand || strings.Count(body, ":'monitor_primary_hostname'") != 1 {
		t.Fatal("the native read lost the producer's SQL boundary")
	}
	return strings.Replace(body, ":'monitor_primary_hostname'", "$1", 1)
}

// Every cell is text; none may carry the holder's statement.
func transactionHorizonNativeRows(t testing.TB, ctx context.Context, query string) []Row {
	t.Helper()
	rows := []Row{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, transactionHorizonNativeSql(t, query), transactionHorizonNativeHost)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := make(Row, transactionHorizonColumns)
				destinations := make([]any, transactionHorizonColumns)
				for i := range row {
					destinations[i] = &row[i]
				}
				server.Raise(result.Scan(destinations...))
				for _, cell := range row {
					if strings.Contains(cell, transactionHorizonNativeCanary) || strings.Contains(cell, "pg_current_xact_id") {
						t.Fatal("the horizon read carried a holder's statement text")
					}
				}
				rows = append(rows, row)
			}
		})
	})
	return rows
}

func TestTransactionHorizonNativeNamesTheOpenTransaction(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		var slots, prepared string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_replication_slots)::text, (SELECT count(*) FROM pg_prepared_xacts)::text`).Scan(&slots, &prepared))
		})

		server.Db(ctx, func(holder server.PgConn) {
			// A transaction id and a snapshot, then idle in the transaction.
			// Repeatable read keeps the snapshot between statements, which
			// dates the horizon from the holder's last statement.
			server.RaisePgResult(holder.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ"))
			defer func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cleanupCancel()
				holder.Exec(cleanupCtx, "ROLLBACK")
			}()
			var holderPid int64
			var xid string
			server.Raise(holder.QueryRow(ctx, fmt.Sprintf(`SELECT pg_backend_pid(), pg_current_xact_id()::text || '%s'`, transactionHorizonNativeCanary)).Scan(&holderPid, &xid))

			// any age warns, so the open transaction is the finding
			settings := DefaultTransactionHorizonSettings()
			settings.WarnAge = time.Microsecond
			var rows []Row
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				rows = transactionHorizonNativeRows(t, ctx, query)
				return rows, nil
			}}
			signalSettings := syntheticSettings(source)
			signalSettings.Now = time.Now
			alerts, err := NewTransactionHorizonSignalWithSettings(settings).Run(ctx, signalSettings)
			if err != nil {
				t.Fatalf("the horizon run failed: %v", err)
			}

			var held Row
			for _, row := range rows {
				if row[0] == "holder" && row[3] == fmt.Sprint(holderPid) {
					held = row
				}
			}
			if held == nil || held[2] != "session" || held[4] != "client backend" || held[5] != "idle in transaction" ||
				held[7] != "application" || strings.HasPrefix(held[8], "-") || strings.HasPrefix(held[9], "-") || held[12] == "0" {
				t.Fatalf("the open transaction is not a dated session holder with its id and snapshot: %v", held)
			}
			end := rows[len(rows)-1]
			if rows[0][0] != "meta" || rows[0][1] != transactionHorizonNativeHost || end[0] != "end" ||
				end[3] == "0" || end[4] != slots || end[6] != prepared {
				t.Fatalf("the census does not bind the frame: meta %v, end %v", rows[0], end)
			}

			found := false
			for _, alert := range alerts {
				if alert.Class != "transaction-horizon" {
					continue
				}
				found = true
				if alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, fmt.Sprintf("slots=%s ", slots)) ||
					!strings.Contains(alert.Observed, fmt.Sprintf("prepared_transactions=%s ", prepared)) ||
					strings.Contains(alert.Evidence, transactionHorizonNativeCanary) {
					t.Fatalf("the open transaction's finding: %s / %s", alert.Observed, alert.Evidence)
				}
			}
			if !found {
				t.Fatal("the open transaction raised no horizon finding")
			}
		})
	})
}
