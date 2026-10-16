// HandleError counts contention refusals instead of reporting them.
package server

import (
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A contention refusal, alone, wrapped or joined only with others, is counted
// without an unexpected-error report; anything else is still reported. Every
// recovery still reaches its handlers.
func TestHandleErrorCountsContentionRefusalsWithoutAReport(t *testing.T) {
	reports := 0
	logf := unexpectedErrorLogf
	unexpectedErrorLogf = func(string, ...any) { reports++ }
	defer func() { unexpectedErrorLogf = logf }()
	busy := NewContentionError("synthetic owner is busy")
	other := errors.New("synthetic invariant failure")
	cases := []struct {
		name       string
		value      any
		contention bool
	}{
		{name: "refusal", value: busy, contention: true},
		{name: "wrapped refusal", value: fmt.Errorf("synthetic close: %w", busy), contention: true},
		{name: "joined refusals", value: errors.Join(busy, NewContentionError("synthetic second owner")), contention: true},
		{name: "wrapped joined refusals", value: fmt.Errorf("synthetic visit: %w", errors.Join(busy, busy)), contention: true},
		{name: "nested wrapped refusals", value: fmt.Errorf("synthetic visit: %w", errors.Join(fmt.Errorf("synthetic row: %w", busy), busy)), contention: true},
		{name: "wrapped batch of refusals", value: fmt.Errorf("synthetic visit: %w", NewErrorCauseBatch([]error{busy, busy})), contention: true},
		{name: "refusal joined with a failure", value: errors.Join(busy, other)},
		{name: "wrapped mixed join", value: fmt.Errorf("synthetic visit: %w", errors.Join(busy, other))},
		{name: "nested wrapped mixed join", value: fmt.Errorf("synthetic visit: %w", errors.Join(busy, fmt.Errorf("synthetic row: %w", errors.Join(busy, other))))},
		{name: "wrapped mixed batch", value: fmt.Errorf("synthetic visit: %w", NewErrorCauseBatch([]error{busy, fmt.Errorf("synthetic row: %w", errors.Join(busy, other))}))},
		{name: "failure", value: other},
		{name: "string", value: "synthetic owner is busy"},
	}
	for _, c := range cases {
		reports = 0
		counted := testutil.ToFloat64(contentionRecoveredCounter)
		var handled error
		recovered := HandleError(func() { panic(c.value) }, func(err error) { handled = err })
		if recovered != c.value || handled == nil {
			t.Errorf("%s: recovery or handler lost", c.name)
		}
		if original, ok := c.value.(error); ok && handled != original {
			t.Errorf("%s: rescue handler lost the original error", c.name)
		}
		delta := testutil.ToFloat64(contentionRecoveredCounter) - counted
		if c.contention && (reports != 0 || delta != 1) {
			t.Errorf("%s: contention reported %d times and counted %v", c.name, reports, delta)
		}
		if !c.contention && (reports != 1 || delta != 0) {
			t.Errorf("%s: failure reported %d times and counted %v", c.name, reports, delta)
		}
	}
}

// Unknown branches and custom matching cannot turn an incomplete cause census
// into a benign refusal. An explicit bounded batch still inspects every row.
func TestIsContentionErrorRequiresEveryConcreteLeaf(t *testing.T) {
	busy := NewContentionError("synthetic busy owner")
	var typedNil *ContentionError
	cycle := &errorCauseTestOne{}
	cycle.cause = cycle
	for index, err := range []error{
		nil, typedNil, cycle, &errorCauseTestOne{}, &errorCauseTestMany{},
		&errorCauseTestMany{causes: []error{busy, nil}},
		&errorCauseTestMatch{}, errorCauseTestDepth(40, busy),
	} {
		if IsContentionError(err) {
			t.Errorf("case %d: an incomplete or non-contention cause was accepted", index)
		}
	}
	rows := make([]error, errorCauseBatchMaximumMembers)
	for index := range rows {
		rows[index] = fmt.Errorf("synthetic row %d: %w", index, busy)
	}
	if !IsContentionError(fmt.Errorf("synthetic visit: %w", NewErrorCauseBatch(rows))) {
		t.Fatal("a complete bounded batch of refusals was reported as unexpected")
	}
	rows[len(rows)-1] = errors.New("synthetic last-row failure")
	if IsContentionError(fmt.Errorf("synthetic visit: %w", NewErrorCauseBatch(rows))) {
		t.Fatal("the final independent failure in a bounded batch was hidden")
	}
}
