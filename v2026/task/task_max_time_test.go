// Max-time cancellation must retain its cause across panic and return boundaries.
package task

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The channel release, then context cancellation, forces the real timer ordering.
func TestTaskMaxTimePanicRetainsTimeout(t *testing.T) {
	timer := make(chan time.Time)
	target := NewTaskTarget(func(args struct{}, clientSession *session.ClientSession) (struct{}, error) {
		close(timer)
		<-clientSession.Ctx.Done()
		panic(clientSession.Ctx.Err())
	})
	target.runAfter = func(time.Duration) <-chan time.Time { return timer }
	_, post, err := target.RunSpecific(context.Background(), &Task{ArgsJson: `{}`})
	if err == nil || !strings.Contains(err.Error(), "Timeout") || !strings.Contains(err.Error(), "Interrupted: context canceled") {
		t.Fatalf("max-time panic lost its cause: %v", err)
	}
	if post != nil {
		t.Fatal("timed-out task must not publish post work")
	}
}

// The retried post target had the same panic bypass as ordinary task work.
func TestTaskPostMaxTimePanicRetainsTimeout(t *testing.T) {
	timer := make(chan time.Time)
	target := NewTaskTargetWithPost(
		func(args struct{}, clientSession *session.ClientSession) (struct{}, error) { return struct{}{}, nil },
		func(args struct{}, result struct{}, clientSession *session.ClientSession, tx server.PgTx) error {
			close(timer)
			<-clientSession.Ctx.Done()
			panic(clientSession.Ctx.Err())
		},
	)
	target.runAfter = func(time.Duration) <-chan time.Time { return timer }
	_, err := target.RunPost(context.Background(), &FinishedTask{ArgsJson: `{}`, ResultJson: `{}`}, nil)
	if err == nil || !strings.Contains(err.Error(), "Timeout") || !strings.Contains(err.Error(), "Interrupted: context canceled") {
		t.Fatalf("max-time post panic lost its cause: %v", err)
	}
}

// Post return, swallowed cancellation and parent cancellation stay distinct.
func TestTaskPostMaxTimeReturnedResultsRetainTimeout(t *testing.T) {
	syntheticErr := errors.New("synthetic post canceled")
	for _, taskErr := range []error{syntheticErr, nil} {
		timer := make(chan time.Time)
		target := NewTaskTargetWithPost(
			func(args struct{}, clientSession *session.ClientSession) (struct{}, error) { return struct{}{}, nil },
			func(args struct{}, result struct{}, clientSession *session.ClientSession, tx server.PgTx) error {
				close(timer)
				<-clientSession.Ctx.Done()
				return taskErr
			},
		)
		target.runAfter = func(time.Duration) <-chan time.Time { return timer }
		_, err := target.RunPost(context.Background(), &FinishedTask{ArgsJson: `{}`, ResultJson: `{}`}, nil)
		if err == nil || !strings.Contains(err.Error(), "Timeout") || (taskErr != nil && !errors.Is(err, taskErr)) {
			t.Fatalf("max-time post return lost cause: %v", err)
		}
	}
}

// Parent cancellation of a post target must not acquire a false max-time cause.
func TestTaskPostParentCancellationIsNotMaxTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := make(chan time.Time)
	target := NewTaskTargetWithPost(
		func(args struct{}, clientSession *session.ClientSession) (struct{}, error) { return struct{}{}, nil },
		func(args struct{}, result struct{}, clientSession *session.ClientSession, tx server.PgTx) error {
			cancel()
			<-clientSession.Ctx.Done()
			panic(clientSession.Ctx.Err())
		},
	)
	target.runAfter = func(time.Duration) <-chan time.Time { return timer }
	_, err := target.RunPost(ctx, &FinishedTask{ArgsJson: `{}`, ResultJson: `{}`}, nil)
	if err == nil || strings.Contains(err.Error(), "Timeout") || !strings.Contains(err.Error(), "Interrupted: context canceled") {
		t.Fatalf("post parent cancellation misclassified: %v", err)
	}
}

// Returned errors and swallowed cancellation share the panic path's timeout contract.
func TestTaskMaxTimeReturnedResultsRetainTimeout(t *testing.T) {
	syntheticErr := errors.New("synthetic operation canceled")
	for _, taskErr := range []error{syntheticErr, nil} {
		timer := make(chan time.Time)
		target := NewTaskTarget(func(args struct{}, clientSession *session.ClientSession) (struct{}, error) {
			close(timer)
			<-clientSession.Ctx.Done()
			return struct{}{}, taskErr
		})
		target.runAfter = func(time.Duration) <-chan time.Time { return timer }
		_, post, err := target.RunSpecific(context.Background(), &Task{ArgsJson: `{}`})
		if err == nil || !strings.Contains(err.Error(), "Timeout") || (taskErr != nil && !errors.Is(err, taskErr)) {
			t.Fatalf("max-time return lost cause: %v", err)
		}
		if post != nil {
			t.Fatal("timed-out return must not publish post work")
		}
	}
}

// Worker/drain cancellation remains interrupted, not mislabeled as max-time.
func TestTaskParentCancellationIsNotMaxTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := make(chan time.Time)
	target := NewTaskTarget(func(args struct{}, clientSession *session.ClientSession) (struct{}, error) {
		cancel()
		<-clientSession.Ctx.Done()
		panic(clientSession.Ctx.Err())
	})
	target.runAfter = func(time.Duration) <-chan time.Time { return timer }
	_, post, err := target.RunSpecific(ctx, &Task{ArgsJson: `{}`})
	if err == nil || strings.Contains(err.Error(), "Timeout") || !strings.Contains(err.Error(), "Interrupted: context canceled") || post != nil {
		t.Fatalf("parent cancellation misclassified: %v", err)
	}
}

// The fix must not extend the default floor or a task's explicitly larger budget.
func TestTaskMaxTimeBudgetsStayUnchanged(t *testing.T) {
	for _, seconds := range []int{0, 1, 300} {
		observed := make(chan time.Duration, 1)
		timer := make(chan time.Time)
		target := NewTaskTarget(func(args struct{}, clientSession *session.ClientSession) (struct{}, error) {
			return struct{}{}, nil
		})
		target.runAfter = func(duration time.Duration) <-chan time.Time { observed <- duration; return timer }
		_, post, err := target.RunSpecific(context.Background(), &Task{ArgsJson: `{}`, RunMaxTimeSeconds: seconds})
		if err != nil || post == nil {
			t.Fatalf("successful work failed: %v", err)
		}
		if actual := <-observed; actual != max(time.Duration(seconds)*time.Second, DefaultMaxTime) {
			t.Fatalf("budget changed: %s", actual)
		}
	}
}

// Successful post work retains its existing detached, bounded finalize context.
func TestTaskMaxTimeSuccessKeepsDetachedPost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := make(chan time.Time)
	postCalled := false
	target := NewTaskTargetWithPost(
		func(args struct{}, clientSession *session.ClientSession) (struct{}, error) { return struct{}{}, nil },
		func(args struct{}, result struct{}, clientSession *session.ClientSession, tx server.PgTx) error {
			postCalled = true
			if clientSession.Ctx.Err() != nil {
				return clientSession.Ctx.Err()
			}
			if _, ok := clientSession.Ctx.Deadline(); !ok {
				return errors.New("post deadline missing")
			}
			return nil
		},
	)
	target.runAfter = func(time.Duration) <-chan time.Time { return timer }
	_, post, err := target.RunSpecific(ctx, &Task{ArgsJson: `{}`})
	if err != nil || post == nil {
		t.Fatalf("success lost post: %v", err)
	}
	cancel()
	if _, err := post(nil); err != nil || !postCalled {
		t.Fatalf("completed task post inherited canceled context: %v", err)
	}
}
