package controller

// A provider that declared its transports leave through a proxy counts for no
// unit (model/verify_proxied_egress_model.go): no capture the flag candidacy,
// whatever a presence sample recorded before it declared it, and no auto-fleet
// binding. The census excludes it as having no unit (no_location), a reason
// it already has, so its format does not change. Pure: the facts are built
// here. A direct host and a provider that declared nothing are unchanged.

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A proxied provider's unit is none, whatever the samples recorded; without
// the mark the same samples give it one.
func TestCtfResolveProviderUnitOfAProxiedProvider(t *testing.T) {
	sampledUnit, address := ctfTestHash("proxy prefix"), ctfTestHash("proxy address")
	sampled := []*ctfSampledUnit{{countryCode: "is", unit: sampledUnit, family: 4, lastSampleIndex: 600}}

	egress := ctfResolveProviderUnit(&model.CtfEgress{Proxied: true, Addresses: [][32]byte{address}}, sampled, "is")
	if egress.Unit != nil || !egress.Proxied || len(egress.Addresses) != 1 {
		t.Fatalf("a proxied provider resolved to %+v", egress)
	}
	if egress := ctfResolveProviderUnit(&model.CtfEgress{Addresses: [][32]byte{address}}, sampled, "is"); egress.Unit == nil || *egress.Unit != sampledUnit {
		t.Fatalf("a provider that declared nothing resolved to %+v", egress)
	}
}

// Six proxy slots of one host, each at its own proxy's prefix and present all
// epoch, are no candidates: each fails as having no unit. The host's direct
// provider and a provider that declared nothing stay candidates.
func TestCtfCensusProxiedProvidersAreNoCandidates(t *testing.T) {
	f := newCtfCensusFixture()
	ids := ctfTestClientIds(8)
	for i, id := range ids {
		f.provider(id, "is", "prefix "+id.String(), "address "+id.String())
		f.present(model.CtfRoleProvider, id, "is", 0, 672)
		if i < 6 {
			// the census resolves a proxied provider's unit from its egress and
			// the units its samples recorded
			sampled := []*ctfSampledUnit{{countryCode: "is", unit: *f.facts.Providers[id].Egress.Unit, family: 4, lastSampleIndex: 671}}
			f.facts.Providers[id].Egress = ctfResolveProviderUnit(&model.CtfEgress{Proxied: true, Addresses: f.facts.Providers[id].Egress.Addresses}, sampled, "is")
		}
	}
	decision := decideCtfCensus(f.facts)

	candidates := 0
	for i, id := range ids {
		member := ctfMember(t, decision, model.CtfRoleProvider, id)
		if i < 6 {
			if member.Candidate || member.Failed != ctfFailedOf(model.CtfReasonNoLocation) || member.Unit != nil {
				t.Errorf("proxy slot %d: candidate %v failed %b unit %v, want no unit and no candidacy", i, member.Candidate, member.Failed, member.Unit)
			}
			continue
		}
		if !member.Candidate || member.Failed != 0 {
			t.Errorf("provider %d: candidate %v failed %b, want a candidate", i, member.Candidate, member.Failed)
		}
		candidates += 1
	}
	if candidates != 2 {
		t.Fatalf("%d candidates, want 2", candidates)
	}
}

// The auto-fleet planner chooses no device without a prefix pin, as a
// proxied device has none, and none whose pin is the zero hash.
func TestSnFleetAutoCandidatesSkipDevicesWithoutAPrefix(t *testing.T) {
	direct, proxied, zero := server.Id{0xc0, 1}, server.Id{0xc0, 2}, server.Id{0xc0, 3}
	evidence := &snFleetAutoEvidence{
		ClientKeys: map[server.Id][32]byte{direct: {0xa0, 1}, proxied: {0xa0, 2}, zero: {0xa0, 3}},
		Eligible:   map[server.Id]bool{direct: true, proxied: true, zero: true},
		// the proxied device has no pin; the third's is the zero hash
		Prefixes: map[server.Id][32]byte{direct: {0xd0, 1}, zero: {}},
		PerFleet: 7,
	}
	candidates := snFleetAutoCandidates([]server.Id{direct, proxied, zero}, evidence, map[server.Id]time.Time{}, map[server.Id]bool{}, nil, 7)
	if len(candidates) != 1 || candidates[0].ClientId != direct {
		ids := []server.Id{}
		for _, candidate := range candidates {
			ids = append(ids, candidate.ClientId)
		}
		t.Fatalf("candidates %v, want the direct device alone", ids)
	}
}
