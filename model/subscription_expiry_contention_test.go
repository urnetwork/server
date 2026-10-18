// Real expiry row and visit wrappers retain the all-causes refusal policy.
package model

import (
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/server"
)

// Cleanup failures remain unexpected through the actual wrappers assembled by
// subscription expiry, while a visit containing only busy rows stays benign.
func TestForceCloseVisitContentionChecksEveryRowCause(t *testing.T) {
	busy := server.NewContentionError("synthetic transfer owner is busy")
	failure := errors.New("synthetic close cleanup failed")
	for _, c := range []struct {
		name       string
		cause      error
		contention bool
	}{
		{name: "busy row", cause: busy, contention: true},
		{name: "wrapped busy rows", cause: fmt.Errorf("synthetic close: %w", errors.Join(busy, busy)), contention: true},
		{name: "mixed cleanup", cause: errors.Join(busy, failure)},
		{name: "wrapped mixed cleanup", cause: fmt.Errorf("synthetic close: %w", errors.Join(busy, failure))},
	} {
		row := &forceCloseRowError{contractId: auditStatsTestId(1), index: 0, cause: c.cause}
		visit := &ForceCloseVisitError{cause: server.NewErrorCauseBatch([]error{row}), attemptedCloseCount: 1, complete: true}
		if got := server.IsContentionError(visit); got != c.contention {
			t.Errorf("%s: contention=%t, want %t", c.name, got, c.contention)
		}
		var handled error
		recovered := server.HandleError(func() { panic(visit) }, func(err error) { handled = err })
		if recovered != visit || handled != visit {
			t.Errorf("%s: visit identity was lost during recovery", c.name)
		}
	}
}
