// The catalog requires both aggregate partitions; optional detail is separate.
package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A readable above-band partition is not the catalog's complete aggregate.
func TestStaleDestinationAboveBandMissingCompanionRemainsUnknown(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	for _, companion := range []string{"false", "true"} {
		payload := staleDestinationFixtureJSON(t, now, []staleDestinationFixtureRate{{companion: companion, rate: 65}})
		alerts, err := NewStaleDestinationSignal().Run(context.Background(), staleDestinationSyntheticSettings(t, now, payload))
		if err != nil || len(alerts) != 1 || alerts[0].Class != "stale-destination-instrumentation" ||
			!isVisibilityAlert(alerts[0]) || requireAlertClassCount(alerts, "stale-destination-rate") != 0 {
			t.Fatal("missing companion partition acquired complete aggregate authority")
		}
	}
}

// Both cells must be at one evaluation. A fresh above-band cell cannot borrow
// a different evaluation clock from its otherwise readable companion.
func TestStaleDestinationAboveBandSkewedCompanionRemainsUnknown(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	payload := staleDestinationFixtureJSONTimes(t, []staleDestinationFixtureSample{
		{companion: "false", sampleTime: now, rate: 65},
		{companion: "true", sampleTime: now.Add(-time.Second), rate: 0},
	})
	signal := NewStaleDestinationSignal()
	alerts, err := NewWithSignals(staleDestinationSyntheticSettings(t, now, payload), signal).Run(context.Background())
	if err == nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !isVisibilityAlert(alerts[0]) ||
		requireAlertClassCount(alerts, "stale-destination-rate") != 0 {
		t.Fatal("skewed companion partition acquired complete aggregate authority")
	}
}

// A complete aggregate retains its real finding without optional detail; this
// does not confer source-owner, sender-lane or artifact-adoption authority.
func TestStaleDestinationCompleteAboveBandRetainsIndependentAggregate(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	alerts := runStaleDestinationFixture(t, now, now, 65, 0)
	if len(alerts) != 1 || alerts[0].Class != "stale-destination-rate" || isVisibilityAlert(alerts[0]) ||
		!strings.Contains(alerts[0].Observed, "detail_status=absent") || strings.Contains(alerts[0].Observed, "dominant_source_owner=") {
		t.Fatal("optional detail loss erased or falsely attributed complete aggregate")
	}
	if alerts := runStaleDestinationFixture(t, now, now, 49.5, 0.5); len(alerts) != 0 {
		t.Fatal("complete exact aggregate threshold acquired an alert")
	}
}
