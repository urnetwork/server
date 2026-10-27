package monitor

import "testing"

// This file alone overlays the unchanged baseline for causal RED. It compiles
// without the new constructor and fails at the missing registered detector.
func TestContractDegradationRegisteredCausal(t *testing.T) {
	count := 0
	for _, signal := range NewSignals() {
		if signal.Key() == "contract-degradation" && signal.Number() == "2.6b" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("the registered monitor has no unique degradation-valve detector; backlog/rates cannot observe its publication")
	}
}
