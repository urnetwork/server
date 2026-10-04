// Current progress checkpoints retain the bounded error inspection contract.
package task

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Malformed or canceled histories cannot become authority to skip prior work.
func TestTaskRetryCheckpointBoundsCauseGraphsAndRevalidates(t *testing.T) {
	cycle := &taskCauseTestOne{}
	cycle.cause = cycle
	var deep error = context.Canceled
	for range 40 {
		deep = &taskCauseTestOne{cause: deep}
	}
	for _, cause := range []error{cycle, deep, &taskCauseTestOne{}, errors.Join(cycle, ErrDrained), context.Canceled, context.DeadlineExceeded, ErrDrained, ErrTargetNotFound} {
		err := WithRetryDelayAndArgs(cause, time.Minute, map[string]string{"cursor": "next"})
		if taskRetryArgsJson(err) != nil {
			t.Fatal("incomplete or canceled cause acquired completed-work checkpoint authority")
		}
	}
	mutable := &taskCauseTestOne{cause: errors.New("synthetic completed batch failure")}
	hint := WithRetryDelayAndArgs(mutable, time.Minute, map[string]string{"cursor": "next"})
	if taskRetryArgsJson(hint) == nil {
		t.Fatal("ordinary complete failure lost its explicit checkpoint")
	}
	mutable.cause = mutable
	if taskRetryArgsJson(hint) != nil {
		t.Fatal("changed cause retained stale checkpoint authority")
	}
	if taskRetryArgsJson(WithRetryDelayAndArgs(&taskCauseTestMatch{}, time.Minute, map[string]string{"cursor": "next"})) == nil {
		t.Fatal("a custom matcher changed the structurally complete failure")
	}
}
