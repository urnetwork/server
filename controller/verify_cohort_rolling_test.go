package controller

// The rolling census's controller rules (sn VERIFY-COHORT-V2.md §2.6, §5.3):
// the headroom held for fleets bound from the next epoch, and what a head
// summary's census report stores. Pure: no database or Redis.

import (
	"testing"

	"github.com/urfoundation/sn/protocol/headsummary"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Fleets whose bindings start at the next epoch hold room for at most a
// per-fleet share of their members; fleets live now, renewals that keep a
// live binding's start, and bindings that start later hold none.
func TestSnUpcomingClaimedMembersHoldsAPerFleetShare(t *testing.T) {
	const epoch = 6
	fleet := func(b byte) [32]byte { return [32]byte{b} }
	version := func(fleetId byte, uid uint16, from uint64) *model.SnFleetBindingVersion {
		return &model.SnFleetBindingVersion{
			ClientId: server.NewId(), Generation: 1, FleetId: fleet(fleetId), Hotkey: fleet(fleetId + 100), Uid: uid,
			ValidFromEpoch: from, SignedValidToEpoch: from + 11, ValidToEpoch: from + 11,
		}
	}
	versions := []*model.SnFleetBindingVersion{
		// live now: already in the priority set
		version(1, 145, 2),
		// bound for epoch 7: nine members, seven held
		version(2, 31, 7), version(2, 31, 7), version(2, 31, 7), version(2, 31, 7), version(2, 31, 7),
		version(2, 31, 7), version(2, 31, 7), version(2, 31, 7), version(2, 31, 7),
		// bound for epoch 7: one member
		version(3, 44, 7),
		// bound for epoch 8: not yet
		version(4, 80, 8),
	}
	if upcoming := snUpcomingClaimedMembers(versions, epoch, 7); upcoming != 8 {
		t.Fatalf("upcoming %d, want 7 + 1", upcoming)
	}
	if upcoming := snUpcomingClaimedMembers(versions, epoch, 0); upcoming != 0 {
		t.Fatalf("a zero share held %d", upcoming)
	}
	if upcoming := snUpcomingClaimedMembers(nil, epoch, 7); upcoming != 0 {
		t.Fatalf("no bindings held %d", upcoming)
	}
}

// A summary's census report stores as the decoder accepted it: the census, and
// with coverage the recipient cut's bindings as the denominator. A validator
// that predates the report stores nothing, and the coverage gauge reads 1 with
// no client bound.
func TestSnHeadSummaryCensusStoresTheReport(t *testing.T) {
	binding := headsummary.EvidenceBinding{ClientId: "00000000-0000-0000-0000-000000000001"}
	census, observed := uint64(1403), uint64(1)
	summary := &SnHeadSummary{
		Schema: headsummary.SchemaV2, CensusProviders: &census, BoundClientsObserved: &observed,
		EvidenceCuts: []headsummary.EvidenceCut{{NoId: 1, Bindings: []headsummary.EvidenceBinding{binding, binding}}},
	}
	gotCensus, gotBound, gotObserved := snHeadSummaryCensus(summary)
	if gotCensus == nil || *gotCensus != 1403 || gotBound == nil || *gotBound != 2 || gotObserved == nil || *gotObserved != 1 {
		t.Fatalf("stored %v %v %v", gotCensus, gotBound, gotObserved)
	}
	if ratio, ok := snHeadSummaryCoverageRatio(&model.SnHeadSummary{BoundClients: gotBound, BoundClientsObserved: gotObserved}); !ok || ratio != 0.5 {
		t.Fatalf("coverage %v %t", ratio, ok)
	}
	*summary.CensusProviders = 7
	if *gotCensus != 1403 {
		t.Fatal("the stored census aliases the decoded summary")
	}

	older := &SnHeadSummary{Schema: headsummary.SchemaV2, EvidenceCuts: summary.EvidenceCuts}
	if gotCensus, gotBound, gotObserved := snHeadSummaryCensus(older); gotCensus != nil || gotBound != nil || gotObserved != nil {
		t.Fatal("a validator that predates the report stored one")
	}
	if _, ok := snHeadSummaryCoverageRatio(&model.SnHeadSummary{}); ok {
		t.Fatal("a summary without coverage set the gauge")
	}
	zero := uint64(0)
	if ratio, ok := snHeadSummaryCoverageRatio(&model.SnHeadSummary{BoundClients: &zero, BoundClientsObserved: &zero}); !ok || ratio != 1 {
		t.Fatalf("no client bound reads %v %t, want 1", ratio, ok)
	}
}
