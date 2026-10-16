package controller

import (
	"reflect"
	"testing"

	"github.com/urnetwork/server/model"
)

// The Online URL ratio's low_success reason is part of a complete dashboard
// snapshot: a snapshot carrying it with every other reason is valid and
// round-trips, and one without it, as a schema 3 snapshot from before the
// ratio is, is incomplete. The counts policy schema keeps the two apart.
func TestStatsProviderEgressCacheAcceptsTheLowSuccessReason(t *testing.T) {
	store := newStatsProviderEgressTestStore()
	counts := statsProviderEgressTestCounts()
	counts.ReasonCounts[model.ProviderExcludedLowSuccess] = 5
	if !validStatsProviderEgressCounts(counts) {
		t.Fatal("a snapshot with the low_success reason is not valid")
	}
	value := statsProviderEgressTestWire(t, counts, store.now(), store.now())
	decoded, fresh, err := decodeStatsProviderEgressSnapshot(value, store.now())
	if err != nil || !fresh || decoded.Counts.ReasonCounts[model.ProviderExcludedLowSuccess] != 5 || !reflect.DeepEqual(decoded.Counts, counts) {
		t.Fatalf("the low_success reason did not round-trip: %v", err)
	}

	delete(counts.ReasonCounts, model.ProviderExcludedLowSuccess)
	if validStatsProviderEgressCounts(counts) {
		t.Fatal("a snapshot without the low_success reason is valid")
	}
}
