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
		{name: "refusal joined with a failure", value: errors.Join(busy, other)},
		{name: "failure", value: other},
		{name: "string", value: "synthetic owner is busy"},
	}
	for _, c := range cases {
		reports = 0
		counted := testutil.ToFloat64(contentionRecoveredCounter)
		handled := false
		recovered := HandleError(func() { panic(c.value) }, func(error) { handled = true })
		if recovered != c.value || !handled {
			t.Errorf("%s: recovery or handler lost", c.name)
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
