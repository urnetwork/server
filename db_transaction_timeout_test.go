package server

import (
	"context"
	"errors"
	"testing"
	"time"
)

// DB-backed: under the owner rule this runs only after the branch, with its
// migrations, is merged to main.
//
// Inside the callback the session has no transaction_timeout; after it,
// returned or raised, the session setting is reset to its default rather
// than left as a session value.
func TestWithoutTransactionTimeoutLiftsAndResets(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		setting := func(conn PgConn) (value string, source string) {
			Raise(conn.QueryRow(ctx, `SELECT setting, source FROM pg_settings WHERE name = 'transaction_timeout'`).Scan(&value, &source))
			return
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `SET transaction_timeout = '20min'`))
			ran := false
			WithoutTransactionTimeout(ctx, conn, func() {
				ran = true
				if value, _ := setting(conn); value != "0" {
					t.Fatalf("transaction_timeout inside = %s, want 0", value)
				}
			})
			if !ran {
				t.Fatal("the callback did not run")
			}
			if _, source := setting(conn); source == "session" {
				t.Fatal("the lifted timeout stayed a session setting")
			}

			RaisePgResult(conn.Exec(ctx, `SET transaction_timeout = '20min'`))
			var raised error
			HandleError(func() {
				WithoutTransactionTimeout(ctx, conn, func() {
					panic(errors.New("synthetic maintenance failure"))
				})
			}, func(err error) { raised = err })
			if raised == nil {
				t.Fatal("the callback's failure was swallowed")
			}
			if _, source := setting(conn); source == "session" {
				t.Fatal("a failed callback left the lifted timeout as a session setting")
			}
		}, OptReadWrite(), OptNoRetry())
	})
}
