package monitor

import (
	"testing"
	"time"
)

// SIGNALS.md §26.5: §25.1's contract-failure-share is the cancellation-share
// rule for incident B. The proxy fleet's eligible returned calls and its
// cancellations (canceled plus panic) per second, five-minute rates recorded
// from read-only Main Mimir at each minute from 14:42Z to 14:53Z on
// 2026-10-26. The share first reaches 20% at 14:48Z (58.8%). §25.1 holds a
// page for six points spanning five minutes, so its reducer pages at 14:53Z,
// one evaluation after §26's five-evaluation reading (14:52Z), and not at
// 14:52Z.
var contractFailureShareIncidentB = struct {
	start    time.Time
	eligible []float64
	failures []float64
}{
	start:    time.Date(2026, 10, 26, 14, 42, 0, 0, time.UTC),
	eligible: []float64{112.461, 106.483, 94.090, 79.631, 65.791, 51.230, 77.572, 104.202, 106.942, 140.244, 214.173, 262.327},
	failures: []float64{0.042, 0.025, 0.018, 0.070, 2.644, 5.081, 45.601, 79.041, 82.257, 113.580, 186.317, 234.422},
}

func TestContractFailureShareReplaysIncidentB(t *testing.T) {
	settings := DefaultContractFailureShareSettings()
	points := func(end int) []contractFailurePoint {
		history := []contractFailurePoint{}
		for index := end - 10; index <= end; index++ {
			at := contractFailureShareIncidentB.start.Add(time.Duration(index) * time.Minute)
			history = append(history, contractFailurePoint{
				known: true, requestedEvalUnix: at.Unix(),
				eligibleRate: contractFailureShareIncidentB.eligible[index], failureRate: contractFailureShareIncidentB.failures[index],
			})
		}
		return history
	}
	// 14:52Z: the six-point page hold still includes 14:47Z (9.9%)
	tier, healthy, held, unknown := contractFailureVerdict(evaluateContractFailureShare("main/proxy", points(10), settings))
	if tier != "" || healthy || !held || unknown {
		t.Fatalf("14:52Z: tier %q healthy %t held %t unknown %t", tier, healthy, held, unknown)
	}
	// 14:53Z: 14:48Z through 14:53Z are all at or above 20%
	tier, _, _, unknown = contractFailureVerdict(evaluateContractFailureShare("main/proxy", points(11), settings))
	if tier != tierPage || unknown {
		t.Fatalf("14:53Z: tier %q unknown %t", tier, unknown)
	}
}
