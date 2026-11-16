// Exercises the public probe with the same latency bands and complete slot
// coverage while stage shape and optional aggregate context vary independently.
package monitor

import (
	"math"
	"strings"
	"testing"
)

// Stage share is application wall-time evidence, never an exclusive query or
// pool diagnosis, even when optional aggregate queue context is high or zero.
func TestContractStageLatencyDoesNotInferCauseFromStageShare(t *testing.T) {
	for _, test := range []struct {
		name           string
		concentrated   bool
		queue, control float64
	}{
		{name: "spread-context-unavailable", concentrated: false, queue: math.NaN(), control: math.NaN()},
		{name: "spread-context-high", concentrated: false, queue: 900, control: 0.16},
		{name: "concentrated-context-zero", concentrated: true, queue: 0, control: 0.002},
		{name: "concentrated-context-high", concentrated: true, queue: 900, control: 0.16},
	} {
		fixture := newContractStageFixture(contractStageNow)
		fixture.scalars["pgbouncer_waiting"] = test.queue
		fixture.scalars["control_mean"] = test.control
		expectedSeverity := SeverityPage
		if test.concentrated {
			fixture.stages = map[string]float64{"companion_origin": 0.3 * 10000, "relationship": 0.02 * 10000, "other": 0.008 * 10000}
			fixture.scalars["hold_mean"] = 0.32
			expectedSeverity = SeverityWarn
		}
		alerts := runContractStageFixture(t, contractStageNow, fixture.json(t), nil)
		alert := requireAlertClass(t, alerts, "contract-stage-latency")
		if len(alerts) != 1 || alert.Severity != expectedSeverity || alert.Sustain != 1 {
			t.Fatalf("case %s: latency verdict changed", test.name)
		}
		if !strings.Contains(alert.Mechanism, "Stage totals alone do not identify a query, pool, host or network cause.") {
			t.Fatalf("case %s: unsupported causal diagnosis", test.name)
		}
		for _, overclaim := range []string{"That is PgBouncer", "not pool saturation", "pool queueing slows every database stage"} {
			if strings.Contains(alert.Mechanism, overclaim) {
				t.Fatalf("case %s: exclusive cause retained", test.name)
			}
		}
		if test.concentrated && !strings.Contains(alert.Observed, "concentrated_stage=companion_origin") {
			t.Fatal("stage concentration evidence disappeared")
		}
		if !test.concentrated && !strings.Contains(alert.Observed, "concentrated_stage=none") {
			t.Fatal("broad stage evidence changed")
		}
	}
}
