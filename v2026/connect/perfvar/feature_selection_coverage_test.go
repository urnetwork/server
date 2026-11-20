// Covers the test-level skip boundary so an unavailable optional setting cannot
// silently suppress the remaining feature-selection assertions.
package perfvar

import "testing"

// The subtest boundary deliberately captures the old test's SkipNow. Ordinary
// positive feature cases stay in the homogeneous loop in the original test.
func TestPerfvarFeatureSelectionKeepsAvailableCoverage(t *testing.T) {
	var skipped bool
	if !t.Run("feature-selection-skip-boundary", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		TestPerfvarFeatureSelection(t)
	}) {
		return
	}
	if skipped {
		t.Fatal("an unavailable setting skipped supported feature and refusal coverage")
	}
}
