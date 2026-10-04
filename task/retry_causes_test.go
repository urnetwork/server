// Malformed target failures stay bounded through hints, caps and final metrics.
package task

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Constant text is safe for persistence even when the cause graph cycles.
type taskCauseTestOne struct{ cause error }

func (self *taskCauseTestOne) Error() string { return "synthetic task cause" }
func (self *taskCauseTestOne) Unwrap() error { return self.cause }

// Keep all-nil and over-wide graphs observable by the shared inspector.
type taskCauseTestMany struct{ causes []error }

func (self *taskCauseTestMany) Error() string   { return "synthetic task joined cause" }
func (self *taskCauseTestMany) Unwrap() []error { return self.causes }

// Policy must not consult arbitrary custom matchers for drain/cancellation.
type taskCauseTestMatch struct{}

func (self *taskCauseTestMatch) Error() string { return "synthetic task matcher" }
func (self *taskCauseTestMatch) Is(error) bool { panic("task custom Is invoked") }
func (self *taskCauseTestMatch) As(any) bool   { panic("task custom As invoked") }

// A malformed target cannot acquire short backoff or a successful drain metric.
func TestTaskRetryAndMetricsBoundIncompleteCauseGraphs(t *testing.T) {
	oldBase, oldCap := RescheduleTimeout, RescheduleBackoffMaxTimeout
	RescheduleTimeout, RescheduleBackoffMaxTimeout = 2*time.Second, time.Hour
	t.Cleanup(func() { RescheduleTimeout, RescheduleBackoffMaxTimeout = oldBase, oldCap })
	cycle := &taskCauseTestOne{}
	cycle.cause = cycle
	var deep error = ErrDrained
	for range 40 {
		deep = &taskCauseTestOne{cause: deep}
	}
	wide := make([]error, 256)
	for index := range wide {
		wide[index] = ErrDrained
	}
	ordinary := errorRescheduleDelay(RescheduleTimeout, RescheduleBackoffMaxTimeout, 16, rescheduleBackoffMaxExponent, 0.5)
	for index, cause := range []error{cycle, deep, &taskCauseTestMany{causes: wide}, &taskCauseTestOne{},
		&taskCauseTestMany{}, &taskCauseTestMany{causes: []error{nil, nil}}, errors.Join(cycle, ErrDrained)} {
		for _, err := range []error{cause, WithRetryDelay(cause, 3*time.Second)} {
			for _, target := range []Target{nil, &errorRetryCappedTarget{maxDelay: 5 * time.Minute}} {
				delay, delta := taskTargetErrorRetryDelay(target, err, 16, 0.5)
				if delay != ordinary || delta != 1 || taskMetricOutcome(err) != "failed" {
					t.Fatal("incomplete task cause acquired cadence, cap, or drain outcome", index, delay, delta)
				}
			}
		}
	}
}

// Completed public sentinels retain precedence, independent of matching hooks.
func TestTaskRetryCauseInspectionRetainsTypedSentinelPrecedence(t *testing.T) {
	oldBase, oldCap := RescheduleTimeout, RescheduleBackoffMaxTimeout
	RescheduleTimeout, RescheduleBackoffMaxTimeout = 2*time.Second, time.Hour
	t.Cleanup(func() { RescheduleTimeout, RescheduleBackoffMaxTimeout = oldBase, oldCap })
	matcher := &taskCauseTestMatch{}
	ordinary := errorRescheduleDelay(RescheduleTimeout, RescheduleBackoffMaxTimeout, 16, rescheduleBackoffMaxExponent, 0.5)
	for _, item := range []struct {
		err     error
		delay   time.Duration
		delta   int
		outcome string
	}{
		{err: matcher, delay: ordinary, delta: 1, outcome: "failed"},
		{err: WithRetryDelay(matcher, 3*time.Second), delay: 3 * time.Second, delta: 1, outcome: "failed"},
		{err: WithRetryDelay(errors.Join(matcher, context.Canceled), 3*time.Second), delay: ordinary, delta: 1, outcome: "failed"},
		{err: errors.Join(matcher, ErrDrained, ErrTargetNotFound), delay: 3 * time.Second, delta: 0, outcome: "drained"},
		{err: errors.Join(matcher, ErrTargetNotFound), delay: 17 * time.Second, delta: 1, outcome: "target_not_found"},
	} {
		delay, delta := taskErrorRetryDelay(item.err, 16, 0.5)
		if delay != item.delay || delta != item.delta || taskMetricOutcome(item.err) != item.outcome {
			t.Fatal("completed typed task precedence changed", delay, delta, taskMetricOutcome(item.err))
		}
	}
	if taskMetricOutcome(nil) != "succeeded" {
		t.Fatal("successful task lost its outcome")
	}
}
