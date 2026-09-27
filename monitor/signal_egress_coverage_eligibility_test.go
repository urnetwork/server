// Full coverage uses the current eligible population, not every acknowledged
// worker event. Keep that attribution boundary in capacity and deadline alerts.
package monitor

import (
	"strings"
	"testing"
	"time"
)

// A low eligible-row rate remains exposure without alleging lost publication;
// counters outside that population must not manufacture coverage recovery.
func TestEgressFullAttemptRateExplainsEligibility(t *testing.T) {
	f, page := egressFullCapacityFinding("synthetic", egressCoverageGeometry{
		shardCount: 1, fullConcurrency: 2,
	}, []egressCoverageSnapshot{{
		eligible: 1000, fullCurrent: 100, noLocationDue: 900,
		fullAttemptsLastHour: 4,
	}})
	if !page || !strings.Contains(f.observed, "attempted_last_hour=4") ||
		!strings.Contains(f.observed, "projected_drain=225h0m0s") {
		t.Fatal("the unchanged eligible-population capacity boundary did not page")
	}
	text := strings.Join([]string{f.mechanism, f.baseline, f.evidence, f.context, f.action, f.verify}, " ")
	for _, required := range []string{
		"current eligible population", "currently disconnected", "acknowledged",
		"not evidence of publication loss", "same-window", "reconnect",
		"does not establish successful health/location coverage",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("full capacity attribution is missing %q", required)
		}
	}
	if strings.Contains(text, "This is gross full-probe execution capacity") {
		t.Fatal("current membership was mislabeled as all worker execution")
	}
}

// The adjacent deadline screen uses the same eligibility-filtered rate; a
// larger process counter neither proves scheduler starvation nor clears debt.
func TestEgressFullFairnessExplainsEligibility(t *testing.T) {
	findings := egressFullFairnessFindings("synthetic", egressCoverageGeometry{
		shardCount: 1, fullLimit: 2,
	}, []egressCoverageSnapshot{{
		eligible: 100, staleHealthDue: 2, staleHealthExpiredDue: 1,
		staleHealthOldestAgeSeconds: int64(25 * time.Hour / time.Second),
		fullAttemptsLastHour:        4,
	}})
	if len(findings) != 1 || !strings.Contains(findings[0].observed, "deadline_missed=true") {
		t.Fatal("eligibility qualification suppressed the expired deadline")
	}
	text := findings[0].context + " " + findings[0].action
	for _, required := range []string{"current eligible population", "currently disconnected", "acknowledged", "not evidence of publication loss"} {
		if !strings.Contains(text, required) {
			t.Errorf("full deadline attribution is missing %q", required)
		}
	}
}
