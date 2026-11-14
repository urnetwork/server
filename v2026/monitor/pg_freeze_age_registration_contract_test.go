// This registration contract uses only the unchanged baseline Signal API.
// Install this file alone for the actual current173 missing-probe RED.
package monitor

import "testing"

// The baseline must fail behaviorally, without referencing the new constructor,
// query, parser, native driver or any other candidate-only symbol.
func TestPgFreezeAgeRegistryExistsOnBaselineApi(t *testing.T) {
	count := 0
	for _, signal := range NewSignals() {
		if signal.Key() == "pg-freeze-age" {
			count++
		}
	}
	if count == 0 {
		t.Fatal("missing registered pg-freeze-age signal")
	}
	if count != 1 {
		t.Fatal("duplicate registered pg-freeze-age signal")
	}
}
