// Baseline-compatible causal control: this file depends only on the existing
// Signal registry API, so an absent reader fails the named assertion.
package monitor

import (
	"testing"
	"time"
)

// Exact catalog/registry ownership is part of the reader, not a documentation
// claim about unrelated aggregate host-memory probes.
func TestPgHugePagesAndJwtRejectionsRegistered(t *testing.T) {
	seen := map[string]bool{}
	for _, signal := range NewSignals() {
		if signal.Key() == "pg-huge-pages" || signal.Key() == "jwt-rejections" {
			if seen[signal.Key()] || signal.Cadence() != time.Minute {
				t.Fatal("new reader registration is duplicate or has wrong policy")
			}
			seen[signal.Key()] = true
		}
	}
	if len(seen) != 2 {
		t.Fatal("the concrete huge-page or JWT metric reader is absent")
	}
}
