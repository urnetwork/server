// Error diagnostics preserve typed distinctions at the real terminal counter
// boundary; text resemblance and malformed cause graphs grant no attribution.
package task

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type executionMetricCauseGraph struct {
	causes []error
	panic  bool
}

func (self *executionMetricCauseGraph) Error() string { return "synthetic private graph" }
func (self *executionMetricCauseGraph) Unwrap() []error {
	if self.panic {
		panic("synthetic unwrap refusal")
	}
	return self.causes
}

type executionMetricCustomMatcher struct{}

func (executionMetricCustomMatcher) Error() string { return "context canceled" }
func (executionMetricCustomMatcher) Is(error) bool { panic("custom matcher must not run") }

func TestTaskExecutionErrorCauseUsesFiniteTypedLeaves(t *testing.T) {
	cycle := &executionMetricCauseGraph{}
	cycle.causes = []error{cycle}
	for _, test := range []struct {
		err  error
		want string
	}{
		{want: "none"},
		{err: context.Canceled, want: "canceled"},
		{err: fmt.Errorf("synthetic wrapper: %w", context.DeadlineExceeded), want: "deadline"},
		{err: server.DbContextDoneError, want: "db_context_done"},
		{err: errors.Join(server.DbContextDoneError, context.DeadlineExceeded), want: "deadline"},
		{err: errors.Join(context.Canceled, context.Canceled), want: "canceled"},
		{err: ErrDrained, want: "drained"},
		{err: ErrTargetNotFound, want: "target_not_found"},
		{err: &pgconn.PgError{Code: "55P03", Message: "synthetic private lock"}, want: "postgres_lock"},
		{err: &pgconn.PgError{Code: "57014"}, want: "postgres_canceled"},
		{err: &pgconn.PgError{Code: "40001"}, want: "postgres_serialization"},
		{err: &pgconn.PgError{Code: "40P01"}, want: "postgres_deadlock"},
		{err: &pgconn.PgError{Code: "53300"}, want: "postgres_capacity"},
		{err: &pgconn.PgError{Code: "08006"}, want: "postgres_connection"},
		{err: &pgconn.PgError{Code: "synthetic-private-code"}, want: "postgres_other"},
		{err: errors.Join(context.Canceled, &pgconn.PgError{Code: "40001"}), want: "mixed"},
		{err: errors.New("context canceled (SQLSTATE 40001)"), want: "other"},
		{err: executionMetricCustomMatcher{}, want: "other"},
		{err: &executionMetricCauseGraph{causes: []error{context.Canceled, nil}}, want: "unknown"},
		{err: &executionMetricCauseGraph{panic: true}, want: "unknown"},
		{err: (*pgconn.PgError)(nil), want: "unknown"},
		{err: cycle, want: "unknown"},
	} {
		if got := taskExecutionErrorCause(test.err); got != test.want {
			t.Fatalf("typed cause = %s, want %s", got, test.want)
		}
	}
}

// executeTask records its outcome before any finishing transaction or Post.
// A successful result emits no error; one returned failure emits exactly one.
func TestTaskExecutionErrorMetricRecordsAtFunctionBoundary(t *testing.T) {
	var executionErr error
	posts := 0
	target := NewTaskTargetWithPost(func(struct{}, *session.ClientSession) (struct{}, error) {
		return struct{}{}, executionErr
	}, func(struct{}, struct{}, *session.ClientSession, server.PgTx) error {
		posts++
		return nil
	})
	name := target.TargetFunctionName()
	metricName := taskMetricName(name)
	worker := &TaskWorker{ctx: t.Context(), drainCtx: t.Context(), targetMetricNames: map[string]string{name: metricName}}
	queued := &Task{TaskId: server.NewId(), FunctionName: name, ArgsJson: `{}`}
	for _, test := range []struct {
		err  error
		want string
	}{
		{err: context.Canceled, want: "canceled"},
		{err: context.DeadlineExceeded, want: "deadline"},
		{err: errors.New("synthetic private execution refusal"), want: "other"},
		{err: &pgconn.PgError{Code: "40001"}, want: "postgres_serialization"},
	} {
		executionErr = test.err
		counter := taskExecutionErrorsTotal.WithLabelValues(metricName, "system", test.want)
		before := testutil.ToFloat64(counter)
		result := worker.executeTask(t.Context(), queued, target)
		if result.err != test.err || testutil.ToFloat64(counter) != before+1 || posts != 0 || result.runPost != nil {
			t.Fatal("function error counter changed execution, post custody or count", test.want)
		}
	}
	executionErr = nil
	before := testutil.CollectAndCount(taskExecutionErrorsTotal)
	beforeValues := map[string]float64{}
	for _, cause := range []string{"canceled", "deadline", "other", "postgres_serialization"} {
		beforeValues[cause] = testutil.ToFloat64(taskExecutionErrorsTotal.WithLabelValues(metricName, "system", cause))
	}
	result := worker.executeTask(t.Context(), queued, target)
	if result.err != nil || result.runPost == nil || posts != 0 || testutil.CollectAndCount(taskExecutionErrorsTotal) != before {
		t.Fatal("successful execution emitted an error or ran finalization work")
	}
	for cause, value := range beforeValues {
		if testutil.ToFloat64(taskExecutionErrorsTotal.WithLabelValues(metricName, "system", cause)) != value {
			t.Fatal("successful execution incremented an existing error cause", cause)
		}
	}
}
