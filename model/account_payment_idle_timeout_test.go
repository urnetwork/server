package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

type recordingPaymentPlanTransaction struct {
	statements []string
}

func (self *recordingPaymentPlanTransaction) Exec(_ context.Context, sql string, _ ...any) (server.PgTag, error) {
	self.statements = append(self.statements, sql)
	return server.PgTag{}, nil
}

// Retain the temporary-relation buffer guard without disabling idle protection.
// Reliability now runs on this owner and cannot leave an outer transaction idle.
func TestPaymentPlanConfiguresOnlyItsLocalTransactionGuards(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingPaymentPlanTransaction{}
	configurePaymentPlanTransaction(ctx, recorder)
	wantStatements := []string{
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
}

// TestPaymentPlanTransactionGuardsAreLocal verifies the same statements
// against PostgreSQL when the repository's isolated local integration
// environment is configured.
func TestPaymentPlanTransactionGuardsAreLocal(t *testing.T) {
	ctx := context.Background()
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
			// The plan's buffer workaround must preserve configured idle safety.
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL idle_in_transaction_session_timeout = '7s'`))
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL effective_io_concurrency = 200`))
			configurePaymentPlanTransaction(ctx, tx)

			if setting := readSetting(tx, "idle_in_transaction_session_timeout"); setting != "7s" {
				t.Fatalf("transaction idle timeout = %q, want inherited 7s", setting)
			}
			if setting := readSetting(tx, "effective_io_concurrency"); setting != "32" {
				t.Fatalf("transaction effective_io_concurrency = %q, want 32", setting)
			}
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
