// This release lifts the application role's transaction_timeout (migration
// 889) in each of its long transactions, so the migration can be applied
// before the next release serves.
package server

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Records the statements a helper sends instead of executing them.
type recordingTransactionTimeoutTx struct {
	statements []string
}

func (self *recordingTransactionTimeoutTx) Exec(_ context.Context, sql string, _ ...any) (PgTag, error) {
	self.statements = append(self.statements, sql)
	return PgTag{}, nil
}

func TestSetTransactionTimeoutInTxStatements(t *testing.T) {
	for _, c := range []struct {
		bound time.Duration
		want  string
	}{
		{bound: 0, want: "SET LOCAL transaction_timeout = 0"},
		{bound: -time.Second, want: "SET LOCAL transaction_timeout = 0"},
		{bound: 2 * time.Hour, want: "SET LOCAL transaction_timeout = 0; SET LOCAL transaction_timeout = 7200000"},
		{bound: 1500 * time.Microsecond, want: "SET LOCAL transaction_timeout = 0; SET LOCAL transaction_timeout = 1"},
		{bound: time.Microsecond, want: "SET LOCAL transaction_timeout = 0; SET LOCAL transaction_timeout = 1"},
	} {
		recorder := &recordingTransactionTimeoutTx{}
		SetTransactionTimeoutInTx(context.Background(), recorder, c.bound)
		if len(recorder.statements) != 1 || recorder.statements[0] != c.want {
			t.Errorf("bound %s: statements %q, want %q", c.bound, recorder.statements, c.want)
		}
	}
}

// Every long transaction of this release that the migration's bound would end
// lifts or replaces it as its transaction's first configuration. A missing
// call is caught here, not by a payout or a census ended in production.
func TestLongOperationsLiftTransactionTimeout(t *testing.T) {
	for _, c := range []struct {
		path     string
		function string
		call     string
	}{
		{"model/account_payment_model_plan.go", "configurePaymentPlanTransaction", "server.SetTransactionTimeoutInTx(ctx, tx, 0)"},
		{"model/network_client_reliability_model.go", "UpdateClientReliabilityScores", "boundReliabilityTransactionInTx(ctx, tx)"},
		{"model/network_client_reliability_model.go", "UpdateNetworkReliabilityWindow", "boundReliabilityTransactionInTx(ctx, tx)"},
		{"model/network_client_reliability_model.go", "UpdateClientLocationReliabilities", "boundReliabilityTransactionInTx(ctx, tx)"},
		{"model/network_client_reliability_model.go", "boundReliabilityTransactionInTx", "server.SetTransactionTimeoutInTx(ctx, tx, reliabilityRunningCheckpointStatementTimeout)"},
		{"taskworker/work/network_client_location_search_work.go", "IndexSearchLocations", "server.SetTransactionTimeoutInTx(clientSession.Ctx, tx, 0)"},
		{"model/st_provider_usage.go", "getStEpochProviderUsageWithCensus", "server.SetTransactionTimeoutInTx(ctx, tx, 0)"},
		{"db_maintenance.go", "dbMaintenanceRunReindex", `conn.Exec(ctx, "SET transaction_timeout = 0")`},
	} {
		source, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		files := token.NewFileSet()
		parsed, err := parser.ParseFile(files, c.path, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		var body string
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == c.function && fn.Body != nil {
				body = string(source[files.Position(fn.Body.Pos()).Offset:files.Position(fn.Body.End()).Offset])
			}
		}
		if body == "" {
			t.Fatalf("%s: function %s not found", c.path, c.function)
		}
		if !strings.Contains(body, c.call) {
			t.Errorf("%s: %s does not lift the application role's transaction bound (%s)", c.path, c.function, c.call)
		}
	}
}

// A transaction that lifts the bound outlives it; one that does not is ended
// with 25P04, the error the migration's bound raises.
func TestSetTransactionTimeoutInTxPgLiftsTheSessionBound(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		boundedSession := func() PgConn {
			conn, err := AcquireMaintenanceDbConn(ctx)
			Raise(err)
			RaisePgResult(conn.Exec(ctx, `SET transaction_timeout = '300ms'`))
			return conn
		}

		lifted := boundedSession()
		defer discardPgConnection(ctx, lifted)
		tx, err := lifted.Begin(ctx)
		Raise(err)
		SetTransactionTimeoutInTx(ctx, tx, 0)
		RaisePgResult(tx.Exec(ctx, `SELECT pg_sleep(0.6)`))
		if err := tx.Commit(ctx); err != nil {
			t.Fatal("a transaction that lifted the bound was ended", err)
		}

		bounded := boundedSession()
		defer discardPgConnection(ctx, bounded)
		tx, err = bounded.Begin(ctx)
		Raise(err)
		_, err = tx.Exec(ctx, `SELECT pg_sleep(0.6)`)
		if err == nil {
			err = tx.Commit(ctx)
		}
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "25P04" {
			t.Fatal("a transaction over the session bound was not ended with 25P04", err)
		}
	})
}
