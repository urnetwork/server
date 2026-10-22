package monitor

import "testing"

// Overlay this file alone on the unchanged parent for the causal RED gate.
func TestProviderScoreReadErrorsRegisteredCausal(t *testing.T) {
	count := 0
	for _, signal := range NewSignals() {
		if signal.Key() == "provider-score-read-errors" && signal.Number() == "2.9e" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("the registered monitor has no unique score-reader error detector; completed responses cannot establish this source")
	}
}
