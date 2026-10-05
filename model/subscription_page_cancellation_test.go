// Error inspection cannot turn missing or unobserved causes into page progress.
package model

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server"
)

// Constant diagnostics do not traverse a deliberately malformed graph.
type settlementPageTestCause struct {
	cause error
	reads *int
}

// Formatting does not follow the synthetic cycle.
func (self *settlementPageTestCause) Error() string { return "synthetic settlement cause" }

// Count only production's bounded unwrap observations.
func (self *settlementPageTestCause) Unwrap() error {
	if self.reads != nil {
		*self.reads++
	}
	return self.cause
}

// Unlike errors.Join, this wrapper retains absent children for inspection.
type settlementPageTestCauses struct{ causes []error }

// Formatting leaves children untouched.
func (self *settlementPageTestCauses) Error() string { return "synthetic settlement causes" }

// Retain all declared branches, including absent children.
func (self *settlementPageTestCauses) Unwrap() []error { return self.causes }

// Custom matchers must not authorize a checkpoint or receive traversal.
type settlementPageTestMatcher struct{}

// Diagnostics are independent of identity matching.
func (self *settlementPageTestMatcher) Error() string { return "synthetic settlement matcher" }

// Classification must never delegate its authority to this matcher.
func (self *settlementPageTestMatcher) Is(error) bool { panic("custom settlement Is invoked") }

// Typed inspection must use observed nodes without custom substitution.
func (self *settlementPageTestMatcher) As(any) bool { panic("custom settlement As invoked") }

// The predicate is checked directly: unrelated panic logging owns its own
// inspection, while only this complete census can authorize a page checkpoint.
func TestSettlementPageCancellationRejectsIncompleteAndForeignCauses(t *testing.T) {
	reads := 0
	cycle := &settlementPageTestCause{reads: &reads}
	cycle.cause = cycle
	var typedNil *settlementPageTestCause
	var deep error = context.Canceled
	for range 33 {
		deep = &settlementPageTestCause{cause: deep}
	}
	wide := make([]error, 128)
	for index := range wide {
		wide[index] = context.Canceled
	}
	for index, cause := range []error{
		nil, typedNil, cycle, deep, &settlementPageTestCause{}, &settlementPageTestCauses{},
		&settlementPageTestCauses{causes: []error{nil, context.Canceled}},
		&settlementPageTestCauses{causes: wide},
		&settlementPageTestMatcher{},
		errors.Join(server.DbContextDoneError, context.Canceled, &settlementPageTestMatcher{}),
		errors.Join(server.DbContextDoneError, errors.New("synthetic physical failure"), context.Canceled),
	} {
		if isSettlementPageCancellation(cause) {
			t.Fatal("incomplete or foreign cause authorized a settlement checkpoint", index)
		}
	}
	if reads != 32 {
		t.Fatal("settlement cancellation inspection lost its finite depth bound", reads)
	}
	if !isSettlementPageCancellation(&settlementPageTestCauses{causes: wide[:127]}) {
		t.Fatal("complete cancellation graph at the exact node bound was refused")
	}
}
