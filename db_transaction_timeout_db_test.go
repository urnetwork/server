// Pins the application role's transaction bound (PARTITION-MAINTENANCE-PLAN.md
// A6) against PostgreSQL: the migration's role default, how PostgreSQL 18
// applies the bound inside a transaction, that a transaction the bound ends
// fails at once with its cause, and that the exempted operations outlive it.
// A test that shortens the bound does it as a default of the test role in the
// test's own database, and restores the migration's value.
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The migration's default, as SHOW renders it.
const testApplicationTransactionTimeout = "15min"

// Sets the test role's default in the test database and starts new sessions,
// so both pools run with it.
func setTestRoleTransactionTimeout(t testing.TB, ctx context.Context, value string) {
	t.Helper()
	MaintenanceDb(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`
			DO $$
			BEGIN
				EXECUTE format('ALTER ROLE CURRENT_USER IN DATABASE %%I SET transaction_timeout = %%L', current_database(), '%s');
			END
			$$
		`, value)))
	}, OptReadWrite(), OptNoRetry())
	PgReset()
}

func showTransactionTimeout(t testing.TB, ctx context.Context, conn PgCanQuery) string {
	t.Helper()
	var value string
	result, err := conn.Query(ctx, `SHOW transaction_timeout`)
	WithPgResult(result, err, func() {
		if !result.Next() {
			t.Fatal("SHOW transaction_timeout returned no row")
		}
		Raise(result.Scan(&value))
	})
	return value
}

func backendPid(t testing.TB, ctx context.Context, conn PgConn) int32 {
	t.Helper()
	var pid int32
	Raise(conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	return pid
}

// The cause the caller sees when the bound ended its transaction.
func requireTransactionTimeoutCause(t testing.TB, recovered any) {
	t.Helper()
	err, ok := recovered.(error)
	var pgErr *pgconn.PgError
	if !ok || !errors.As(err, &pgErr) || pgErr.Code != pgTransactionTimeoutCode {
		t.Fatalf("recovered %v, want the 25P04 cause", recovered)
	}
}

// The migration sets the default for the role each pool connects as, in this
// database only, and a new session of either pool starts with it. Running it
// again keeps the single setting.
func TestMigrationSetsApplicationTransactionTimeout(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		check := func(pool string, use func(func(PgConn))) {
			use(func(conn PgConn) {
				var inDatabase, roleWide, databaseWide int
				Raise(conn.QueryRow(ctx, `
					SELECT
						count(*) FILTER (
							WHERE setting_row.setrole = (SELECT oid FROM pg_roles WHERE rolname = current_user)
							AND setting_row.setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database())
							AND setting = 'transaction_timeout=15min'
						),
						count(*) FILTER (
							WHERE setting_row.setrole = (SELECT oid FROM pg_roles WHERE rolname = current_user)
							AND setting_row.setdatabase = 0
							AND setting LIKE 'transaction_timeout=%'
						),
						count(*) FILTER (
							WHERE setting_row.setrole = 0
							AND setting_row.setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database())
							AND setting LIKE 'transaction_timeout=%'
						)
					FROM pg_db_role_setting setting_row
					CROSS JOIN LATERAL unnest(setting_row.setconfig) AS setting
				`).Scan(&inDatabase, &roleWide, &databaseWide))
				if inDatabase != 1 || roleWide != 0 || databaseWide != 0 {
					t.Fatalf("%s pool role settings: in database=%d role-wide=%d database-wide=%d, want 1, 0, 0", pool, inDatabase, roleWide, databaseWide)
				}
			})
		}
		mainPool := func(callback func(PgConn)) { Db(ctx, callback) }
		maintenancePool := func(callback func(PgConn)) { MaintenanceDb(ctx, callback) }
		check("main", mainPool)
		check("maintenance", maintenancePool)

		migrationSetApplicationTransactionTimeout(ctx)
		check("main", mainPool)
		check("maintenance", maintenancePool)

		PgReset()
		Db(ctx, func(conn PgConn) {
			if value := showTransactionTimeout(t, ctx, conn); value != testApplicationTransactionTimeout {
				t.Fatalf("a new main pool session starts with transaction_timeout=%s, want %s", value, testApplicationTransactionTimeout)
			}
		})
		MaintenanceDb(ctx, func(conn PgConn) {
			if value := showTransactionTimeout(t, ctx, conn); value != testApplicationTransactionTimeout {
				t.Fatalf("a new maintenance pool session starts with transaction_timeout=%s, want %s", value, testApplicationTransactionTimeout)
			}
		})
	})
}

// The session the helper used goes back to its pool with the pool default,
// after the callback returns and after it panics.
func TestWithoutTransactionTimeoutPgRestoresSessionDefault(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		PgReset()
		MaintenanceDb(ctx, func(conn PgConn) {
			poolDefault := showTransactionTimeout(t, ctx, conn)
			if poolDefault != testApplicationTransactionTimeout {
				t.Fatalf("pool default = %s, want %s", poolDefault, testApplicationTransactionTimeout)
			}
			pid := backendPid(t, ctx, conn)

			WithoutTransactionTimeout(ctx, conn, func() {
				if value := showTransactionTimeout(t, ctx, conn); value != "0" {
					t.Fatalf("inside the exemption transaction_timeout=%s, want 0", value)
				}
				// a transaction the callback begins starts without the bound
				tx, err := conn.Begin(ctx)
				Raise(err)
				if value := showTransactionTimeout(t, ctx, tx); value != "0" {
					t.Fatalf("a transaction inside the exemption has transaction_timeout=%s, want 0", value)
				}
				Raise(tx.Commit(ctx))
			})
			if value := showTransactionTimeout(t, ctx, conn); value != poolDefault || backendPid(t, ctx, conn) != pid {
				t.Fatalf("after the callback transaction_timeout=%s on pid %d, want %s on pid %d", value, backendPid(t, ctx, conn), poolDefault, pid)
			}

			callbackErr := errors.New("synthetic callback failure")
			recovered := captureDbErrorPanic(func() {
				WithoutTransactionTimeout(ctx, conn, func() {
					panic(callbackErr)
				})
			})
			if recovered != callbackErr {
				t.Fatalf("recovered %v, want the callback's panic", recovered)
			}
			if value := showTransactionTimeout(t, ctx, conn); value != poolDefault || backendPid(t, ctx, conn) != pid {
				t.Fatalf("after the panic transaction_timeout=%s, want %s on the same session", value, poolDefault)
			}
		}, OptNoRetry())
	})
}

// A callback that leaves its transaction open cannot be restored inside it, so
// its session is closed and the pool replaces it.
func TestWithoutTransactionTimeoutPgClosesSessionLeftInTransaction(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		PgReset()
		MaintenanceDb(ctx, func(conn PgConn) {
			WithoutTransactionTimeout(ctx, conn, func() {
				RaisePgResult(conn.Exec(ctx, `BEGIN`))
			})
			if !conn.Conn().IsClosed() {
				t.Fatal("a session left inside a transaction went back to the pool exempted")
			}
		}, OptNoRetry())
		MaintenanceDb(ctx, func(conn PgConn) {
			if value := showTransactionTimeout(t, ctx, conn); value != testApplicationTransactionTimeout {
				t.Fatalf("the replacement session has transaction_timeout=%s, want %s", value, testApplicationTransactionTimeout)
			}
		})
	})
}

// Only maintenance pool sessions are marked direct; a main pool session is
// refused before any statement.
func TestWithoutTransactionTimeoutPgRefusesMainPoolSession(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		MaintenanceDb(ctx, func(conn PgConn) {
			if direct, _ := conn.Conn().PgConn().CustomData()[pgDirectSessionKey].(bool); !direct {
				t.Fatal("a maintenance pool session is not marked direct")
			}
		})
		Db(ctx, func(conn PgConn) {
			called := false
			recovered := captureDbErrorPanic(func() {
				WithoutTransactionTimeout(ctx, conn, func() { called = true })
			})
			if recovered != errTransactionTimeoutPooledSession || called {
				t.Fatalf("recovered=%v called=%t, want a refusal", recovered, called)
			}
			if value := showTransactionTimeout(t, ctx, conn); value == "0" {
				t.Fatal("a refused main pool session was exempted")
			}
		})
	})
}

// PostgreSQL 18 inside a transaction: a positive value leaves the running
// timer alone, so only the bound armed at BEGIN applies; SetTransactionTimeoutInTx
// disarms it first, so a positive bound counts from the call and zero lifts it.
func TestSetTransactionTimeoutInTxPgReplacesTheBound(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		PgReset()

		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `SET LOCAL transaction_timeout = '100ms'`))
			// the 15-minute timer armed at BEGIN still runs, not 100 ms
			RaisePgResult(tx.Exec(ctx, `SELECT pg_sleep(0.5)`))
		}, OptNoRetry())

		attempts := 0
		recovered := captureDbErrorPanic(func() {
			Tx(ctx, func(tx PgTx) {
				attempts += 1
				SetTransactionTimeoutInTx(ctx, tx, 200*time.Millisecond)
				RaisePgResult(tx.Exec(ctx, `SELECT pg_sleep(2)`))
			})
		})
		requireTransactionTimeoutCause(t, recovered)
		if attempts != 1 {
			t.Fatalf("attempts=%d, want 1", attempts)
		}

		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `SET transaction_timeout = '200ms'`))
			tx, err := conn.Begin(ctx)
			Raise(err)
			SetTransactionTimeoutInTx(ctx, tx, 0)
			RaisePgResult(tx.Exec(ctx, `SELECT pg_sleep(0.5)`))
			Raise(tx.Commit(ctx))
			RaisePgResult(conn.Exec(ctx, `RESET transaction_timeout`))
			if value := showTransactionTimeout(t, ctx, conn); value != testApplicationTransactionTimeout {
				t.Fatalf("after the lifted transaction the session has transaction_timeout=%s, want %s", value, testApplicationTransactionTimeout)
			}
		}, OptNoRetry())
	})
}

// A transaction over the bound ends its session with 25P04, on either pool,
// under the default retry options: it is not rerun, its session is not
// reused, and the pool's next session works with the role's default.
func TestTransactionTimeoutPgEndsTransactionWithoutRerun(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setTestRoleTransactionTimeout(t, ctx, "1s")
		defer setTestRoleTransactionTimeout(t, ctx, testApplicationTransactionTimeout)

		for _, pool := range []struct {
			name string
			tx   func(context.Context, func(PgTx), ...any)
			db   func(context.Context, func(PgConn), ...any)
		}{
			{name: "main", tx: Tx, db: Db},
			{name: "maintenance", tx: MaintenanceTx, db: MaintenanceDb},
		} {
			attempts := 0
			var physical *pgconn.PgConn
			recovered := captureDbErrorPanic(func() {
				pool.tx(ctx, func(tx PgTx) {
					attempts += 1
					physical = tx.Conn().PgConn()
					RaisePgResult(tx.Exec(ctx, `SELECT pg_sleep(3)`))
				})
			})
			requireTransactionTimeoutCause(t, recovered)
			if attempts != 1 || physical == nil || !physical.IsClosed() {
				t.Fatalf("%s pool: attempts=%d closed=%t, want one attempt on a closed session", pool.name, attempts, physical != nil && physical.IsClosed())
			}

			// a single statement runs in an implicit transaction the bound covers
			attempts = 0
			recovered = captureDbErrorPanic(func() {
				pool.db(ctx, func(conn PgConn) {
					attempts += 1
					RaisePgResult(conn.Exec(ctx, `SELECT pg_sleep(3)`))
				})
			})
			requireTransactionTimeoutCause(t, recovered)
			if attempts != 1 {
				t.Fatalf("%s pool: a statement over the bound ran %d times, want 1", pool.name, attempts)
			}

			pool.db(ctx, func(conn PgConn) {
				if value := showTransactionTimeout(t, ctx, conn); value != "1s" {
					t.Fatalf("%s pool: the next session has transaction_timeout=%s, want 1s", pool.name, value)
				}
			})
		}
	})
}

// Under a bound shorter than their work, the exempted maintenance operations
// finish: the session helper, a rotation rebuild's session and an online
// migration's session. A plain maintenance statement is ended, as a control.
func TestTransactionTimeoutPgExemptedMaintenanceOutlivesTheBound(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		setTestRoleTransactionTimeout(t, ctx, "1s")
		defer setTestRoleTransactionTimeout(t, ctx, testApplicationTransactionTimeout)

		recovered := captureDbErrorPanic(func() {
			MaintenanceDb(ctx, func(conn PgConn) {
				RaisePgResult(conn.Exec(ctx, `SELECT pg_sleep(2)`))
			}, OptNoRetry())
		})
		requireTransactionTimeoutCause(t, recovered)

		MaintenanceDb(ctx, func(conn PgConn) {
			WithoutTransactionTimeout(ctx, conn, func() {
				RaisePgResult(conn.Exec(ctx, `SELECT pg_sleep(2)`))
			})
			if value := showTransactionTimeout(t, ctx, conn); value != "1s" {
				t.Fatalf("after the exemption transaction_timeout=%s, want the shortened 1s default", value)
			}
		}, OptNoRetry())

		// the rebuild runner takes its statement as given
		if _, err := dbMaintenanceRunReindex(ctx, "pending_task", `SELECT pg_sleep(2)`, 10*time.Second); err != nil {
			t.Fatalf("a rebuild session over the bound failed: %v", err)
		}

		runOnlineSqlMigration(ctx, newOnlineSqlMigration(`SELECT pg_sleep(2)`, ``))
	})
}

// The operator VACUUM FULL runs on a session of its own under the shortened
// bound and reports each table it rewrote.
func TestVacuumFullAllTablesRunsOnItsOwnSession(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `CREATE TABLE synthetic_vacuum_full (value bigint NOT NULL)`))
			RaisePgResult(tx.Exec(ctx, `INSERT INTO synthetic_vacuum_full SELECT generate_series(1, 1000)`))
			RaisePgResult(tx.Exec(ctx, `DELETE FROM synthetic_vacuum_full WHERE value % 2 = 0`))
		})
		excludes := []string{}
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT c.relname FROM pg_class c
				JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname <> 'synthetic_vacuum_full'
			`)
			WithPgResult(result, err, func() {
				for result.Next() {
					var name string
					Raise(result.Scan(&name))
					excludes = append(excludes, name)
				}
			})
		})
		setTestRoleTransactionTimeout(t, ctx, "1s")
		defer setTestRoleTransactionTimeout(t, ctx, testApplicationTransactionTimeout)

		lines := []string{}
		VacuumFullAllTables(ctx, excludes, func(format string, args ...any) {
			lines = append(lines, fmt.Sprintf(format, args...))
		})
		done := false
		for _, line := range lines {
			if strings.Contains(line, "synthetic_vacuum_full done:") {
				done = true
			}
			if strings.Contains(line, "SKIPPED/ERROR") {
				t.Fatalf("VACUUM FULL failed: %s", line)
			}
		}
		if !done {
			t.Fatalf("VACUUM FULL did not report the table: %q", lines)
		}
	})
}
