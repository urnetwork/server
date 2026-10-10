// Cancellation provenance is captured at the actual target body, before cleanup.
package task

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestTaskCollectorInterruptionRequiresBodyCancellationAuthority(t *testing.T) {
	for _, item := range []struct {
		name          string
		collector     bool
		localFirst    bool
		panicCanceled bool
		nestedPanic   bool
		maxTime       bool
		success       bool
		failure       error
		want          bool
	}{
		{name: "returned", collector: true, failure: context.Canceled, want: true},
		{name: "panicked", collector: true, panicCanceled: true, want: true},
		{name: "nested-panicked", collector: true, panicCanceled: true, nestedPanic: true, want: true},
		{name: "database-marker", collector: true, failure: errors.Join(server.DbContextDoneError, context.Canceled), want: true},
		{name: "own-child-canceled-first", collector: true, localFirst: true, failure: context.Canceled},
		{name: "ordinary-parent", failure: context.Canceled},
		{name: "deadline", collector: true, failure: context.DeadlineExceeded},
		{name: "mixed-sql", collector: true, failure: errors.Join(context.Canceled, &pgconn.PgError{Code: "40001"})},
		{name: "text-only", collector: true, failure: errors.New("context canceled")},
		{name: "max-time-first", collector: true, maxTime: true, failure: context.Canceled},
		{name: "success", collector: true, success: true},
	} {
		evalCtx, stopCollector := context.WithCancelCause(t.Context())
		worker := NewTaskWorkerWithDefaults(t.Context())
		timer := make(chan time.Time)
		target := NewTaskTarget(func(_ struct{}, client *session.ClientSession) (struct{}, error) {
			if item.localFirst {
				client.Cancel()
			}
			if item.maxTime {
				close(timer)
				<-client.Ctx.Done()
			}
			if item.collector {
				stopCollector(errTaskCollectorInterrupted)
			} else {
				stopCollector(nil)
			}
			<-client.Ctx.Done()
			if item.panicCanceled {
				if item.nestedPanic {
					panic(taskPanicError(client.Ctx.Err()))
				}
				panic(client.Ctx.Err())
			}
			if item.success {
				return struct{}{}, nil
			}
			return struct{}{}, item.failure
		})
		target.runAfter = func(time.Duration) <-chan time.Time { return timer }
		worker.AddTargets(target)
		result := worker.executeTask(evalCtx, &Task{TaskId: server.NewId(), FunctionName: target.TargetFunctionName(), ArgsJson: `{}`}, target)
		worker.Close()
		stopCollector(nil)
		if result.collectorInterrupted != item.want {
			t.Fatal("task cancellation authority was misclassified", item.name, result.collectorInterrupted, result.err)
		}
		if item.want && (!errors.Is(result.err, context.Canceled) || result.runPost != nil) {
			t.Fatal("collector interruption lost its typed cause or published success", item.name)
		}
		if item.panicCanceled && (!strings.HasPrefix(result.err.Error(), "Interrupted: ") || strings.Contains(result.err.Error(), "Unhandled")) {
			t.Fatal("typed panic recovery replaced its existing diagnostic", result.err)
		}
		if item.maxTime && (result.err == nil || !strings.Contains(result.err.Error(), "Timeout")) {
			t.Fatal("collector cancellation erased the earlier task timeout")
		}
	}
}

func TestTaskCollectorInterruptionRejectsUnattestedAndIncompleteFailures(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errTaskCollectorInterrupted)
	cycle := &taskCauseTestOne{}
	cycle.cause = cycle
	for _, failure := range []error{
		context.Canceled,
		taskPanicError(context.Canceled),
		&taskCollectorInterruption{cause: cycle},
		&taskCollectorInterruption{cause: &taskCauseTestMany{causes: []error{context.Canceled, nil}}},
		&taskCollectorInterruption{cause: errors.Join(context.Canceled, context.DeadlineExceeded)},
		&taskCollectorInterruption{cause: errors.Join(context.Canceled, errors.New("Timeout"))},
	} {
		if taskCollectorInterrupted(ctx, failure) {
			t.Fatal("an unattested or incomplete error acquired interrupted retry authority")
		}
	}
	if taskCollectorInterrupted(t.Context(), &taskCollectorInterruption{cause: context.Canceled}) {
		t.Fatal("body attestation without collector cancellation acquired retry authority")
	}
}
