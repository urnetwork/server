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

// TestPaymentPlanDisablesOnlyItsLocalIdleTransactionTimeout is the regression
// for Payout repeatedly failing with pgconn "conn closed" after the nested
// reliability maintenance transaction exceeded production's five-minute
// idle-in-transaction timeout. The override must be observable inside this
// transaction and must use SET LOCAL so it cannot weaken later sessions.
func TestPaymentPlanDisablesOnlyItsLocalIdleTransactionTimeout(t *testing.T) {
	ctx := context.Background()
	recorder := &recordingPaymentPlanTransaction{}
	configurePaymentPlanTransaction(ctx, recorder)
	if len(recorder.statements) != 1 || recorder.statements[0] != "SET LOCAL idle_in_transaction_session_timeout = 0" {
		t.Fatalf("payment-plan transaction configuration = %q, want one transaction-local timeout override", recorder.statements)
	}

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		server.Db(ctx, func(conn server.PgConn) {
			readSetting := func(queryer interface {
				Query(context.Context, string, ...any) (server.PgResult, error)
			}) string {
				var setting string
				result, err := queryer.Query(ctx, `SELECT current_setting('idle_in_transaction_session_timeout')`)
				server.WithPgResult(result, err, func() {
					if !result.Next() {
						t.Fatal("idle timeout query returned no row")
					}
					server.Raise(result.Scan(&setting))
				})
				return setting
			}

			baselineSetting := readSetting(conn)
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			// Start with a deliberately tiny transaction-local timeout, then
			// configure the payment plan and leave the transaction idle long
			// enough that PostgreSQL would close it without the override.
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL idle_in_transaction_session_timeout = '25ms'`))
			configurePaymentPlanTransaction(ctx, tx)

			setting := readSetting(tx)
			if setting != "0" {
				t.Fatalf("transaction idle timeout = %q, want 0", setting)
			}
			time.Sleep(100 * time.Millisecond)
			server.RaisePgResult(tx.Exec(ctx, `SELECT 1`))
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if setting := readSetting(conn); setting != baselineSetting {
				t.Fatalf("post-commit idle timeout = %q, want original session value %q", setting, baselineSetting)
			}
		})
	})
}
