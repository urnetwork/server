package server

// The owned root executes synchronously on the real testing.T, including all
// assertions after its callback and the testing.Cleanup stack. Unlike the pure
// retry driver, it never tears down or restores service routes inside the child;
// an outer namespace owner must do that only after complete process-tree join.

import "testing"

// This admission/lifecycle seam is intentionally not wired to TestEnv.Run:
// process containment alone is insufficient authority to provision stores.
func runOwnedTestRootWithSetup(t *testing.T, setup func() error, callback func(testing.TB)) {
	t.Helper()
	if err := ClaimTestProcessGeneration(t.Name()); err != nil {
		t.Fatalf("owned root generation admission: %v", err)
	}
	if err := setup(); err != nil {
		t.Fatalf("owned root setup: %v", err)
	}
	callback(t)
}
