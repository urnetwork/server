// Execution error metrics survive a rolled-back business attempt because they
// describe the returned function outcome, never its committed financial work.
package task

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestTaskExecutionErrorMetricCountsRolledBackFunction(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE synthetic_execution_metric (id uuid PRIMARY KEY)`))
		})
		id := server.NewId()
		target := NewTaskTarget(func(struct{}, *session.ClientSession) (result struct{}, resultErr error) {
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO synthetic_execution_metric(id) VALUES($1)`, id))
					server.Raise(context.DeadlineExceeded)
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { resultErr = err })
			return
		})
		name := target.TargetFunctionName()
		metricName := taskMetricName(name)
		worker := &TaskWorker{ctx: ctx, drainCtx: ctx, targetMetricNames: map[string]string{name: metricName}}
		counter := taskExecutionErrorsTotal.WithLabelValues(metricName, "system", "deadline")
		before := testutil.ToFloat64(counter)
		result := worker.executeTask(ctx, &Task{TaskId: server.NewId(), FunctionName: name, ArgsJson: `{}`}, target)
		if result.err != context.DeadlineExceeded || testutil.ToFloat64(counter) != before+1 {
			t.Fatal("rolled-back function lost its execution error observation")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM synthetic_execution_metric`).Scan(&count))
			if count != 0 {
				t.Fatal("execution telemetry committed a rolled-back business row")
			}
		})
	})
}
