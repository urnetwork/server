package connect

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A refused nomination is driven by client redials: a fleet reconnecting
// through a deploy drove 16,563 lines in 24h, second only to the control-error
// flood. The refusal is now counted and the detail is V(1); this pins the
// bounded cause set the counter depends on.
func TestNominationRefusedCounterCauses(t *testing.T) {
	count := func(cause string) float64 {
		return testutil.ToFloat64(nominationRefusedCounter.WithLabelValues(cause))
	}

	// the two refusal paths in nominateResident
	for _, cause := range []string{"draining", "concurrent_client_limit"} {
		before := count(cause)
		nominationRefusedCounter.WithLabelValues(cause).Inc()
		if after := count(cause); after != before+1 {
			t.Fatalf("counter{%s} = %v, want %v", cause, after, before+1)
		}
	}

	// the causes are distinct series: a drain refusal must not be readable as
	// a limit refusal, since they call for opposite operator responses
	before := count("concurrent_client_limit")
	nominationRefusedCounter.WithLabelValues("draining").Inc()
	if after := count("concurrent_client_limit"); after != before {
		t.Fatalf("a draining refusal moved the limit counter: %v -> %v", before, after)
	}
}
