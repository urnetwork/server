// Public registration is the baseline-compatible missing-probe regression;
// it can run against the old registry without any new production APIs.
package monitor

import (
	"testing"
	"time"
)

// The measured floor and incomplete forecast own independent clear scopes.
func TestPgDiskCoverageRegisteredWithIndependentKeys(t *testing.T) {
	want := map[string]string{"pg-disk-space": "2.4b", "pg-disk-runway": "2.4c"}
	seen := map[string]bool{}
	for _, signal := range NewSignals() {
		number, ok := want[signal.Key()]
		if !ok {
			continue
		}
		if seen[signal.Key()] || signal.Number() != number || signal.Cadence() != 15*time.Minute {
			t.Fatal("disk predicates lost independent catalog identities or the fifteen-minute policy")
		}
		seen[signal.Key()] = true
	}
	if len(seen) != len(want) {
		t.Fatal("database root safety-line and explicit forecast-coverage signals are missing")
	}
}
