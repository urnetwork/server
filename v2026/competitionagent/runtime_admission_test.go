//go:build linux

// Execution admission uses a native-read seam; tests touch only private durable
// agent state and fake cleanup, never a daemon, namespace or actual host unit.
package competitionagent

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestAgentExecutionRechecksNativeAdmissionBeforeAnyEvaluator(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("synthetic private runtime changed after dispatch")
	calls := 0
	fixture.agent.runtimeAdmission = func(context.Context) (*RuntimeResourceBoundary, error) { calls++; return nil, refusal }
	if err := fixture.agent.Execute(t.Context(), fixture.manifest.Identity.RunId); !errors.Is(err, refusal) || calls != 1 {
		t.Fatal("execution skipped fresh private runtime admission", err)
	}
	state, err := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || state.ExecutionStarted || state.StopCleanupVerified {
		t.Fatal("refused native runtime started or fabricated stopped evaluator custody", err)
	}
}

func TestAgentExecutionAdmissionCannotHoldCleanupPublicationLock(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	fixture.agent.runtimeAdmission = func(ctx context.Context) (*RuntimeResourceBoundary, error) {
		close(entered)
		select {
		case <-resume:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	done := make(chan struct{})
	var executionErr error
	go func() {
		defer close(done)
		executionErr = fixture.agent.Execute(t.Context(), fixture.manifest.Identity.RunId)
	}()
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(resume) }); <-done })
	<-entered
	fixture.joinStoppedRun(t)
	state, err := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || !state.StopCleanupVerified || state.ExecutionStarted {
		t.Fatal("blocked native admission prevented independently acknowledged cleanup", err)
	}
	once.Do(func() { close(resume) })
	<-done
	if executionErr == nil {
		t.Fatal("execution ignored cleanup completed during native admission")
	}
	state, err = fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || state.ExecutionStarted {
		t.Fatal("late native-read return launched a cleaned run", err)
	}
}
