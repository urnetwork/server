// The config-generation hand-back counter, without a database.
package work

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The hand-backs of one family counted so far.
func testHandbackCount(family string) float64 {
	return testutil.ToFloat64(configHandbacks.WithLabelValues(family))
}

// Every family's series exists before its first hand-back, so a dashboard can
// tell no hand-backs from no taskworker.
func TestConfigHandbacksExposeEveryFamily(t *testing.T) {
	if count := testutil.CollectAndCount(configHandbacks); count < len(configHandbackFamilies) {
		t.Fatalf("%d hand-back series, want one for each of %d families", count, len(configHandbackFamilies))
	}
}
