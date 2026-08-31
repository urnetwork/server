package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

type recordingPaymentPlanTransaction struct {
	statements []string
}

func (r *recordingPaymentPlanTransaction) Exec(_ context.Context, sql string, _ ...any) (server.PgTag, error) {
	r.statements = append(r.statements, sql)
	return server.PgTag{}, nil
}

// TestPaymentPlanConfiguresOnlyItsLocalTransactionGuards is the regression for
// Payout repeatedly failing after the nested reliability work either exceeded
// production's idle-in-transaction timeout or triggered PostgreSQL 18.4's
// high-concurrency temporary-relation local-buffer bug. Both overrides must be
// observable inside this transaction and must use SET LOCAL so they cannot
// weaken or throttle later sessions.
func TestPaymentPlanConfiguresOnlyItsLocalTransactionGuards(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingPaymentPlanTransaction{}
	configurePaymentPlanTransaction(ctx, recorder)
	wantStatements := []string{
		"SET LOCAL idle_in_transaction_session_timeout = 0",
		"SET LOCAL effective_io_concurrency = 32",
	}
	if len(recorder.statements) != len(wantStatements) {
		t.Fatalf("payment-plan transaction configuration = %q, want %q", recorder.statements, wantStatements)
	}
	for i, want := range wantStatements {
		if recorder.statements[i] != want {
			t.Fatalf("payment-plan transaction statement %d = %q, want %q", i, recorder.statements[i], want)
		}
	}

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		server.Db(ctx, func(conn server.PgConn) {
			readSetting := func(queryer interface {
				Query(context.Context, string, ...any) (server.PgResult, error)
			}, name string) string {
				var setting string
				result, err := queryer.Query(ctx, `SELECT current_setting($1)`, name)
				server.WithPgResult(result, err, func() {
					if !result.Next() {
						t.Fatalf("%s query returned no row", name)
					}
					server.Raise(result.Scan(&setting))
				})
				return setting
			}

			baselineIdleTimeout := readSetting(conn, "idle_in_transaction_session_timeout")
			baselineIOConcurrency := readSetting(conn, "effective_io_concurrency")
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			// Start with a deliberately tiny transaction-local timeout, then
			// configure the payment plan and leave the transaction idle long
			// enough that PostgreSQL would close it without the override.
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL idle_in_transaction_session_timeout = '25ms'`))
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL effective_io_concurrency = 200`))
			configurePaymentPlanTransaction(ctx, tx)

			if setting := readSetting(tx, "idle_in_transaction_session_timeout"); setting != "0" {
				t.Fatalf("transaction idle timeout = %q, want 0", setting)
			}
			if setting := readSetting(tx, "effective_io_concurrency"); setting != "32" {
				t.Fatalf("transaction effective_io_concurrency = %q, want 32", setting)
			}
			time.Sleep(100 * time.Millisecond)
			server.RaisePgResult(tx.Exec(ctx, `SELECT 1`))
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if setting := readSetting(conn, "idle_in_transaction_session_timeout"); setting != baselineIdleTimeout {
				t.Fatalf("post-commit idle timeout = %q, want original session value %q", setting, baselineIdleTimeout)
			}
			if setting := readSetting(conn, "effective_io_concurrency"); setting != baselineIOConcurrency {
				t.Fatalf("post-commit effective_io_concurrency = %q, want original session value %q", setting, baselineIOConcurrency)
			}
		})
	})
}
