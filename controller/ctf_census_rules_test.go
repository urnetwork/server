package controller

import (
	"bytes"
	"crypto/sha256"
	"slices"
	"testing"
	"time"

	"github.com/urfoundation/sn/ctf"
	"github.com/urfoundation/sn/ledger"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A census fixture: providers and extenders with presence, boundary facts,
// wallets and the operator's networks, built per test.
type ctfCensusFixture struct {
	facts *ctfCensusFacts
}

func newCtfCensusFixture() *ctfCensusFixture {
	return &ctfCensusFixture{facts: &ctfCensusFacts{
		Required:    model.CtfRequiredSamples(0),
		Providers:   map[server.Id]*ctfProviderEnd{},
		Extenders:   map[server.Id]*ctfExtenderEnd{},
		OwnNetworks: map[server.Id]bool{},
		HeadBound:   map[server.Id]bool{},
		Coldkeys:    map[server.Id][32]byte{},
	}}
}

// Client ids in ascending order, so a test can say which is lower.
func ctfTestClientIds(n int) []server.Id {
	ids := make([]server.Id, n)
	for i := range ids {
		ids[i] = server.Id{0x10, byte(i + 1)}
	}
	return ids
}

func ctfTestHash(label string) [32]byte {
	return sha256.Sum256([]byte(label))
}

// Present in country in samples [from, to).
func (self *ctfCensusFixture) present(role int, clientId server.Id, countryCode string, from int, to int) {
	presence := &model.CtfPresence{Role: role, ClientId: clientId, CountryCode: countryCode}
	for i := from; i < to; i++ {
		presence.Samples.Set(i)
	}
	presence.SampleCount = presence.Samples.Count()
	self.facts.Presences = append(self.facts.Presences, presence)
}

// A provider passing every end rule in country, its unit the prefix label and
// its address the address label, with a wallet.
func (self *ctfCensusFixture) provider(clientId server.Id, countryCode string, unitLabel string, addressLabel string) *ctfProviderEnd {
	networkId := server.Id{0x20, clientId[1]}
	unit := ctfTestHash(unitLabel)
	end := &ctfProviderEnd{
		NetworkId: &networkId, Active: true, TopLevel: true, Connected: true, ValidLocation: true, Online: true, Public: true,
		CountryCode: countryCode,
		Egress:      &model.CtfEgress{Unit: &unit, UnitFamily: 4, Addresses: [][32]byte{ctfTestHash(addressLabel)}},
	}
	self.facts.Providers[clientId] = end
	self.facts.Coldkeys[clientId] = ctfTestHash("coldkey " + clientId.String())
	return end
}

// An extender passing every end rule in country.
func (self *ctfCensusFixture) extender(clientId server.Id, countryCode string, unitLabel string, addressLabel string) *ctfExtenderEnd {
	networkId := server.Id{0x30, clientId[1]}
	unit := ctfTestHash(unitLabel)
	end := &ctfExtenderEnd{
		ExtenderId: server.Id{0x40, clientId[1]}, NetworkId: &networkId, Active: true, CountryCode: countryCode,
		Egress: &model.CtfEgress{Unit: &unit, UnitFamily: 6, Addresses: [][32]byte{ctfTestHash(addressLabel)}},
	}
	self.facts.Extenders[clientId] = end
	self.facts.Coldkeys[clientId] = ctfTestHash("coldkey " + clientId.String())
	return end
}

func ctfMember(t *testing.T, decision *ctfCensusDecision, role int, clientId server.Id) *model.CtfCensusMember {
	t.Helper()
	for _, member := range decision.Members {
		if member.Role == role && member.ClientId == clientId {
			return member
		}
	}
	t.Fatalf("no member %d %s", role, clientId)
	return nil
}

func ctfPot(decision *ctfCensusDecision, countryCode string, role int) *model.CtfCensusPot {
	for _, pot := range decision.Pots {
		if pot.CountryCode == countryCode && pot.Role == role {
			return pot
		}
	}
	return nil
}

func ctfFailedOf(reasons ...int) uint32 {
	var failed uint32
	for _, reason := range reasons {
		failed |= 1 << reason
	}
	return failed
}

// P2 and P3 against the half-epoch threshold: present in the end country in
// at least half of the samples is a candidate; present long enough elsewhere
// is a change of country; present too little anywhere is presence. Both are
// near misses.
func TestCtfCensusPresenceRules(t *testing.T) {
	f := newCtfCensusFixture()
	ids := ctfTestClientIds(5)
	// exactly the threshold
	f.provider(ids[0], "is", "u0", "a0")
	f.present(model.CtfRoleProvider, ids[0], "is", 0, 336)
	// one short
	f.provider(ids[1], "is", "u1", "a1")
	f.present(model.CtfRoleProvider, ids[1], "is", 0, 335)
	// long enough, but in another country than at the end
	f.provider(ids[2], "is", "u2", "a2")
	f.present(model.CtfRoleProvider, ids[2], "de", 0, 400)
	f.present(model.CtfRoleProvider, ids[2], "is", 400, 672)
	// half each in two countries: in neither for half the epoch
	f.provider(ids[3], "is", "u3", "a3")
	f.present(model.CtfRoleProvider, ids[3], "de", 0, 335)
	f.present(model.CtfRoleProvider, ids[3], "is", 335, 670)
	// present in both, most in the end country
	f.provider(ids[4], "de", "u4", "a4")
	f.present(model.CtfRoleProvider, ids[4], "is", 0, 300)
	f.present(model.CtfRoleProvider, ids[4], "de", 300, 672)
	decision := decideCtfCensus(f.facts)

	for i, want := range []struct {
		failed  uint32
		present int
	}{
		{0, 336},
		{ctfFailedOf(model.CtfReasonPresence), 335},
		{ctfFailedOf(model.CtfReasonCountryChanged), 272},
		{ctfFailedOf(model.CtfReasonPresence), 335},
		{0, 372},
	} {
		member := ctfMember(t, decision, model.CtfRoleProvider, ids[i])
		if member.Failed != want.failed || member.PresentSamples != want.present {
			t.Errorf("provider %d: failed %b present %d, want %b %d", i, member.Failed, member.PresentSamples, want.failed, want.present)
		}
		if member.Candidate != (want.failed == 0) || member.NearMiss() != (want.failed != 0) {
			t.Errorf("provider %d: candidate %t near miss %t", i, member.Candidate, member.NearMiss())
		}
	}
	if pot := ctfPot(decision, "is", model.CtfRoleProvider); pot.NearMisses != 3 || pot.Candidates != 1 || pot.Units != 1 {
		t.Errorf("is providers %+v, want 1 unit, 1 candidate, 3 near misses", pot)
	}
}

// Excused samples leave the threshold and the count: a sample found only in
// excused samples counts nothing, and the requirement is half of the rest.
func TestCtfCensusExcusedSamples(t *testing.T) {
	f := newCtfCensusFixture()
	for i := range 100 {
		f.facts.Excused.Set(i)
	}
	f.facts.Required = model.CtfRequiredSamples(f.facts.Excused.Count())
	if f.facts.Required != 286 {
		t.Fatalf("required with 100 excused = %d, want 286", f.facts.Required)
	}
	ids := ctfTestClientIds(2)
	f.provider(ids[0], "is", "u0", "a0")
	f.present(model.CtfRoleProvider, ids[0], "is", 0, 385) // 285 not excused
	f.provider(ids[1], "is", "u1", "a1")
	f.present(model.CtfRoleProvider, ids[1], "is", 100, 386) // 286
	decision := decideCtfCensus(f.facts)
	if member := ctfMember(t, decision, model.CtfRoleProvider, ids[0]); member.Candidate || member.PresentSamples != 285 {
		t.Errorf("provider present in 285 counted samples: %+v", member)
	}
	if member := ctfMember(t, decision, model.CtfRoleProvider, ids[1]); !member.Candidate || member.PresentSamples != 286 {
		t.Errorf("provider present in 286 counted samples: %+v", member)
	}
}

// Every P1, P5 and P6 condition, each alone, and the census counts each member
// under its lowest-numbered reason while recording every rule it fails.
func TestCtfCensusProviderEndRules(t *testing.T) {
	f := newCtfCensusFixture()
	ids := ctfTestClientIds(11)
	for _, id := range ids {
		f.provider(id, "is", "u"+id.String(), "a"+id.String())
		f.present(model.CtfRoleProvider, id, "is", 0, 672)
	}
	f.facts.Providers[ids[0]].Online = false
	f.facts.Providers[ids[1]].Connected = false
	f.facts.Providers[ids[2]].TopLevel = false
	f.facts.Providers[ids[3]].Public = false
	f.facts.Providers[ids[4]].ValidLocation = false
	f.facts.Providers[ids[5]].CountryCode = "xk"
	f.facts.OwnNetworks[*f.facts.Providers[ids[6]].NetworkId] = true
	f.facts.HeadBound[ids[7]] = true
	delete(f.facts.Coldkeys, ids[8])
	f.facts.Providers[ids[9]].Egress = nil
	// offline, not Public and without a wallet: counted as offline
	f.facts.Providers[ids[10]].Online = false
	f.facts.Providers[ids[10]].Public = false
	delete(f.facts.Coldkeys, ids[10])
	decision := decideCtfCensus(f.facts)

	for i, want := range []uint32{
		ctfFailedOf(model.CtfReasonOffline),
		ctfFailedOf(model.CtfReasonOffline),
		ctfFailedOf(model.CtfReasonOffline),
		ctfFailedOf(model.CtfReasonNotPublic),
		ctfFailedOf(model.CtfReasonNoLocation),
		// out of the list, and present in "is" but in "xk" at the end
		ctfFailedOf(model.CtfReasonNoLocation, model.CtfReasonCountryChanged),
		ctfFailedOf(model.CtfReasonOwnNetwork),
		ctfFailedOf(model.CtfReasonHeadBound),
		ctfFailedOf(model.CtfReasonNoWallet),
		ctfFailedOf(model.CtfReasonNoLocation),
		ctfFailedOf(model.CtfReasonOffline, model.CtfReasonNotPublic, model.CtfReasonNoWallet),
	} {
		member := ctfMember(t, decision, model.CtfRoleProvider, ids[i])
		if member.Failed != want || member.Candidate || member.Reason != ctfLowestReason(want) {
			t.Errorf("provider %d: failed %b reason %d, want %b", i, member.Failed, member.Reason, want)
		}
		if member.NearMiss() {
			t.Errorf("provider %d is a near miss", i)
		}
	}
	// the exclusion counts: in its country at the end, "zz" outside the list
	want := []*model.CtfCensusExclusion{
		{CountryCode: "is", Role: 1, Reason: model.CtfReasonOffline, Count: 4},
		{CountryCode: "is", Role: 1, Reason: model.CtfReasonNotPublic, Count: 1},
		{CountryCode: "is", Role: 1, Reason: model.CtfReasonNoLocation, Count: 2},
		{CountryCode: "is", Role: 1, Reason: model.CtfReasonOwnNetwork, Count: 1},
		{CountryCode: "is", Role: 1, Reason: model.CtfReasonHeadBound, Count: 1},
		{CountryCode: "is", Role: 1, Reason: model.CtfReasonNoWallet, Count: 1},
		{CountryCode: "zz", Role: 1, Reason: model.CtfReasonNoLocation, Count: 1},
	}
	if len(decision.Exclusions) != len(want) {
		t.Fatalf("exclusions %d, want %d", len(decision.Exclusions), len(want))
	}
	for i := range want {
		if *decision.Exclusions[i] != *want[i] {
			t.Errorf("exclusion %d = %+v, want %+v", i, decision.Exclusions[i], want[i])
		}
	}
}

// A member the facts do not cover failed every condition they would show:
// a provider that no longer exists, an extender without a record.
func TestCtfCensusMembersWithoutFacts(t *testing.T) {
	f := newCtfCensusFixture()
	ids := ctfTestClientIds(2)
	f.present(model.CtfRoleProvider, ids[0], "is", 0, 672)
	f.present(model.CtfRoleExtender, ids[1], "is", 0, 672)
	decision := decideCtfCensus(f.facts)
	if member := ctfMember(t, decision, model.CtfRoleProvider, ids[0]); member.Reason != model.CtfReasonOffline || member.Failed&ctfFailedOf(model.CtfReasonNotPublic, model.CtfReasonNoLocation, model.CtfReasonNoWallet) == 0 || member.CountryCode != "zz" {
		t.Errorf("provider without facts %+v", member)
	}
	if member := ctfMember(t, decision, model.CtfRoleExtender, ids[1]); member.Reason != model.CtfReasonRevoked || member.ExtenderId != nil {
		t.Errorf("extender without a record %+v", member)
	}
}

// P4: one candidate per keyed IPv4 address or IPv6 /64 per role, the lowest
// client id among the members that pass every other rule; a higher one
// sharing the address is a duplicate even when it also fails another rule,
// and a lower one that fails another rule takes no address.
func TestCtfCensusDuplicateAddresses(t *testing.T) {
	f := newCtfCensusFixture()
	ids := ctfTestClientIds(5)
	f.provider(ids[0], "is", "u0", "shared")
	f.present(model.CtfRoleProvider, ids[0], "is", 0, 672)
	f.provider(ids[1], "is", "u1", "shared")
	f.present(model.CtfRoleProvider, ids[1], "is", 0, 672)
	f.provider(ids[2], "is", "u2", "shared")
	f.present(model.CtfRoleProvider, ids[2], "is", 0, 100)
	// a second address shared with a lower failing member only
	f.provider(ids[3], "is", "u3", "other")
	f.present(model.CtfRoleProvider, ids[3], "is", 0, 10)
	f.provider(ids[4], "is", "u4", "other")
	f.present(model.CtfRoleProvider, ids[4], "is", 0, 672)
	// the same address in the other role is no duplicate
	f.extender(ids[1], "is", "x1", "shared")
	f.present(model.CtfRoleExtender, ids[1], "is", 0, 672)
	decision := decideCtfCensus(f.facts)

	for i, want := range []uint32{
		0,
		ctfFailedOf(model.CtfReasonDuplicate),
		ctfFailedOf(model.CtfReasonDuplicate, model.CtfReasonPresence),
		ctfFailedOf(model.CtfReasonPresence),
		0,
	} {
		if member := ctfMember(t, decision, model.CtfRoleProvider, ids[i]); member.Failed != want {
			t.Errorf("provider %d failed %b, want %b", i, member.Failed, want)
		}
	}
	if member := ctfMember(t, decision, model.CtfRoleExtender, ids[1]); !member.Candidate {
		t.Errorf("extender sharing a provider's address is not a candidate: %+v", member)
	}
	// a duplicate that fails presence is no near miss
	if member := ctfMember(t, decision, model.CtfRoleProvider, ids[2]); member.NearMiss() {
		t.Error("a duplicate counts as a near miss")
	}
}

// Units are tickets: every candidate of a unit enters one draw per role, its
// lowest candidate client id's country, which may not be its own. A member
// that is not a candidate decides nothing.
func TestCtfCensusUnitsEnterTheirLowestClientsDraw(t *testing.T) {
	f := newCtfCensusFixture()
	ids := ctfTestClientIds(4)
	f.provider(ids[0], "is", "shared unit", "a0")
	f.present(model.CtfRoleProvider, ids[0], "is", 0, 672)
	f.provider(ids[1], "de", "shared unit", "a1")
	f.present(model.CtfRoleProvider, ids[1], "de", 0, 672)
	// the lowest member of this unit fails presence, so the next one decides
	f.provider(ids[2], "de", "late unit", "a2")
	f.present(model.CtfRoleProvider, ids[2], "de", 0, 10)
	f.provider(ids[3], "fr", "late unit", "a3")
	f.present(model.CtfRoleProvider, ids[3], "fr", 0, 672)
	decision := decideCtfCensus(f.facts)

	for i, want := range []string{"is", "is", "", "fr"} {
		if member := ctfMember(t, decision, model.CtfRoleProvider, ids[i]); member.DrawCountryCode != want || member.Candidate != (want != "") {
			t.Errorf("provider %d draws in %q, want %q", i, member.DrawCountryCode, want)
		}
	}
	// a member's own country stays its own (the census row's country)
	if member := ctfMember(t, decision, model.CtfRoleProvider, ids[1]); member.CountryCode != "de" {
		t.Errorf("provider 1's own country is %q", member.CountryCode)
	}
	for _, want := range []struct {
		countryCode string
		units       int
		candidates  int
		nearMisses  int
	}{{"is", 1, 2, 0}, {"de", 0, 0, 1}, {"fr", 1, 1, 0}, {"us", 0, 0, 0}} {
		if pot := ctfPot(decision, want.countryCode, model.CtfRoleProvider); pot.Units != want.units || pot.Candidates != want.candidates || pot.NearMisses != want.nearMisses {
			t.Errorf("%s providers %+v, want %d units %d candidates %d near misses", want.countryCode, pot, want.units, want.candidates, want.nearMisses)
		}
	}
	if len(decision.Pots) != 2*ctf.NumCountries {
		t.Fatalf("%d pots, want %d", len(decision.Pots), 2*ctf.NumCountries)
	}
	for i := 1; i < len(decision.Pots); i++ {
		a, b := decision.Pots[i-1], decision.Pots[i]
		if a.CountryCode > b.CountryCode || (a.CountryCode == b.CountryCode && a.Role >= b.Role) {
			t.Fatalf("pots out of order at %d", i)
		}
	}
}

// X1 to X6 for extenders: an inactive or revoked record, a canary, an own
// network and an activation in another country each exclude it; a client may
// be a candidate in both roles.
func TestCtfCensusExtenderRules(t *testing.T) {
	f := newCtfCensusFixture()
	ids := ctfTestClientIds(6)
	for _, id := range ids {
		f.extender(id, "ir", "x"+id.String(), "e"+id.String())
		f.present(model.CtfRoleExtender, id, "ir", 0, 672)
	}
	f.facts.Extenders[ids[1]].Active = false
	f.facts.Extenders[ids[2]].Canary = true
	f.facts.OwnNetworks[*f.facts.Extenders[ids[3]].NetworkId] = true
	f.facts.Extenders[ids[4]].ActivatedElsewhere = true
	// ids[5] is a provider too
	f.provider(ids[5], "ir", "p5", "pa5")
	f.present(model.CtfRoleProvider, ids[5], "ir", 0, 672)
	decision := decideCtfCensus(f.facts)

	for i, want := range []uint32{
		0,
		ctfFailedOf(model.CtfReasonRevoked),
		ctfFailedOf(model.CtfReasonOwnNetwork),
		ctfFailedOf(model.CtfReasonOwnNetwork),
		ctfFailedOf(model.CtfReasonCountryChanged),
		0,
	} {
		member := ctfMember(t, decision, model.CtfRoleExtender, ids[i])
		if member.Failed != want {
			t.Errorf("extender %d failed %b, want %b", i, member.Failed, want)
		}
		if member.ExtenderId == nil || *member.ExtenderId != f.facts.Extenders[ids[i]].ExtenderId {
			t.Errorf("extender %d does not name its record", i)
		}
	}
	if member := ctfMember(t, decision, model.CtfRoleExtender, ids[4]); !member.NearMiss() {
		t.Error("an extender activated elsewhere is not a near miss")
	}
	if !ctfMember(t, decision, model.CtfRoleProvider, ids[5]).Candidate || !ctfMember(t, decision, model.CtfRoleExtender, ids[5]).Candidate {
		t.Error("a client is not a candidate in both roles")
	}
}

// A unit has one family in a census: a member naming a candidate's unit with
// the other family has no valid unit.
func TestCtfCensusUnitFamilyGuard(t *testing.T) {
	f := newCtfCensusFixture()
	ids := ctfTestClientIds(2)
	f.provider(ids[0], "is", "same", "a0")
	f.present(model.CtfRoleProvider, ids[0], "is", 0, 672)
	f.extender(ids[1], "is", "same", "a1")
	f.present(model.CtfRoleExtender, ids[1], "is", 0, 672)
	decision := decideCtfCensus(f.facts)
	if !ctfMember(t, decision, model.CtfRoleProvider, ids[0]).Candidate {
		t.Fatal("the lowest member of the unit is not a candidate")
	}
	if member := ctfMember(t, decision, model.CtfRoleExtender, ids[1]); member.Reason != model.CtfReasonNoLocation {
		t.Errorf("a unit named with two families: %+v", member)
	}
}

// The decided census encodes in the sn census format and decodes to exactly
// its candidates, excused samples and exclusion counts; its hash is the
// header's sha256, which the commitment names.
func TestEncodeCtfCensusRoundTrip(t *testing.T) {
	f := newCtfCensusFixture()
	ids := ctfTestClientIds(6)
	f.facts.Excused.Set(3)
	f.facts.Excused.Set(600)
	f.facts.Required = model.CtfRequiredSamples(2)
	f.provider(ids[0], "is", "u0", "a0")
	f.present(model.CtfRoleProvider, ids[0], "is", 0, 672)
	f.provider(ids[1], "de", "u0", "a1")
	f.present(model.CtfRoleProvider, ids[1], "de", 0, 672)
	f.provider(ids[2], "de", "u2", "a2")
	f.present(model.CtfRoleProvider, ids[2], "de", 0, 100)
	f.provider(ids[3], "xk", "u3", "a3")
	f.present(model.CtfRoleProvider, ids[3], "xk", 0, 672)
	f.extender(ids[4], "ir", "x4", "e4")
	f.present(model.CtfRoleExtender, ids[4], "ir", 0, 672)
	f.extender(ids[0], "is", "x0", "e0")
	f.present(model.CtfRoleExtender, ids[0], "is", 0, 672)
	decision := decideCtfCensus(f.facts)
	encoded, err := encodeCtfCensus(1, 8, &f.facts.Excused, decision)
	if err != nil {
		t.Fatal(err)
	}
	if encoded.Hash != ledger.RefOf(encoded.Header) || !bytes.Equal(encoded.Objects[encoded.Hash], encoded.Header) {
		t.Fatal("the census hash is not the header's sha256")
	}
	if _, ok := encoded.Objects[ctf.CountriesRef]; !ok {
		t.Fatal("the census objects lack the countries object")
	}
	decoded, err := ctf.DecodeCensus(encoded.Header, encoded.Objects)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.NoID != 1 || decoded.Epoch != 8 || !slices.Equal(decoded.ExcusedSamples, []uint16{3, 600}) {
		t.Fatalf("decoded census %d %d %v", decoded.NoID, decoded.Epoch, decoded.ExcusedSamples)
	}
	want := []ctf.Candidate{}
	for _, member := range decision.candidates() {
		country, _ := ctf.ParseCountry(member.CountryCode)
		want = append(want, ctf.Candidate{Role: ctf.Role(member.Role), Country: country, Unit: *member.Unit, Family: uint8(member.UnitFamily), ClientID: [16]byte(member.ClientId)})
	}
	if len(want) != 4 || len(decoded.Candidates) != len(want) {
		t.Fatalf("decoded %d candidates, want %d of 4", len(decoded.Candidates), len(want))
	}
	for _, candidate := range want {
		if !slices.Contains(decoded.Candidates, candidate) {
			t.Errorf("candidate %+v did not survive the encoding", candidate)
		}
	}
	excluded := map[ctf.Exclusion]bool{}
	for _, exclusion := range decoded.Excluded {
		excluded[exclusion] = true
	}
	for _, exclusion := range []ctf.Exclusion{
		{Country: ctf.Country{'d', 'e'}, Role: ctf.RoleProvider, Reason: ctf.ReasonPresence, Count: 1},
		{Country: ctf.CountryUnknown, Role: ctf.RoleProvider, Reason: ctf.ReasonNoLocation, Count: 1},
	} {
		if !excluded[exclusion] {
			t.Errorf("exclusion %+v is not in the census", exclusion)
		}
	}
	if len(decoded.Excluded) != 2 {
		t.Errorf("census holds %d exclusions, want 2", len(decoded.Excluded))
	}
	// the reasons and their order are the package's
	for reason, want := range map[int]ctf.Reason{
		model.CtfReasonRevoked: ctf.ReasonRevoked, model.CtfReasonOffline: ctf.ReasonOffline, model.CtfReasonNotPublic: ctf.ReasonNotPublic,
		model.CtfReasonNoLocation: ctf.ReasonNoLocation, model.CtfReasonOwnNetwork: ctf.ReasonOwnNetwork, model.CtfReasonHeadBound: ctf.ReasonHeadBound,
		model.CtfReasonNoWallet: ctf.ReasonNoWallet, model.CtfReasonDuplicate: ctf.ReasonDuplicate,
		model.CtfReasonCountryChanged: ctf.ReasonCountryChanged, model.CtfReasonPresence: ctf.ReasonPresence,
	} {
		if ctf.Reason(reason) != want {
			t.Errorf("reason %d is not the package's %d", reason, want)
		}
	}
	if model.CtfNumReasons != ctf.NumReasons || model.CtfSamplesPerEpoch != ctf.SamplesPerEpoch || model.CtfCountryUnknown != ctf.CountryUnknown.String() {
		t.Error("the model's census constants are not the package's")
	}
	// the draw puts each unit where the census does
	draw := ctf.DrawCensus(decoded.Candidates, [32]byte{1}, nil)
	for _, member := range decision.candidates() {
		country, _ := ctf.ParseCountry(member.DrawCountryCode)
		if draw.Units[ctf.PotIndex(country.Index(), ctf.Role(member.Role))] == 0 {
			t.Errorf("the draw has no unit where the census puts %s's", member.ClientId)
		}
	}
	if pot := ctfPot(decision, "is", model.CtfRoleProvider); pot.Units != int(draw.Units[ctf.PotIndex(ctf.Country{'i', 's'}.Index(), ctf.RoleProvider)]) {
		t.Errorf("the census counts %d units in is, the draw %d", pot.Units, draw.Units[ctf.PotIndex(ctf.Country{'i', 's'}.Index(), ctf.RoleProvider)])
	}
}

// An epoch with no member still encodes, with no rows and no exclusions.
func TestEncodeCtfCensusEmpty(t *testing.T) {
	decision := decideCtfCensus(newCtfCensusFixture().facts)
	if len(decision.Members) != 0 || len(decision.Pots) != 2*ctf.NumCountries {
		t.Fatalf("empty decision %d members %d pots", len(decision.Members), len(decision.Pots))
	}
	encoded, err := encodeCtfCensus(1, 8, &model.CtfSampleSet{}, decision)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ctf.DecodeCensus(encoded.Header, encoded.Objects)
	if err != nil || len(decoded.Candidates) != 0 || len(decoded.Excluded) != 0 {
		t.Fatalf("empty census decoded %v, %v", decoded, err)
	}
}

// The census unit of a provider: its pin, the pin's family from the samples
// when the live addresses no longer show it, else the latest sampled unit,
// in its end country first.
func TestCtfResolveProviderUnit(t *testing.T) {
	pinned, home, away := ctfTestHash("pinned"), ctfTestHash("home"), ctfTestHash("away")
	address := ctfTestHash("address")
	sampled := []*ctfSampledUnit{
		{countryCode: "is", unit: home, family: 6, lastSampleIndex: 300},
		{countryCode: "de", unit: away, family: 4, lastSampleIndex: 600},
		{countryCode: "de", unit: pinned, family: 4, lastSampleIndex: 10},
	}
	if egress := ctfResolveProviderUnit(&model.CtfEgress{Unit: &pinned, UnitFamily: 6, Addresses: [][32]byte{address}}, sampled, "is"); *egress.Unit != pinned || egress.UnitFamily != 6 || len(egress.Addresses) != 1 {
		t.Errorf("a pin with its family %+v", egress)
	}
	if egress := ctfResolveProviderUnit(&model.CtfEgress{Unit: &pinned}, sampled, "is"); *egress.Unit != pinned || egress.UnitFamily != 4 {
		t.Errorf("a pin without its family %+v", egress)
	}
	if egress := ctfResolveProviderUnit(&model.CtfEgress{Unit: &address}, sampled, "is"); *egress.Unit != address || egress.UnitFamily != 0 {
		t.Errorf("a pin no sample saw %+v", egress)
	}
	if egress := ctfResolveProviderUnit(&model.CtfEgress{Addresses: [][32]byte{address}}, sampled, "is"); *egress.Unit != home || egress.UnitFamily != 6 || len(egress.Addresses) != 1 {
		t.Errorf("no pin, a unit sampled in the end country %+v", egress)
	}
	if egress := ctfResolveProviderUnit(nil, sampled, "fr"); *egress.Unit != away || egress.UnitFamily != 4 {
		t.Errorf("no pin, the latest unit sampled anywhere %+v", egress)
	}
	if egress := ctfResolveProviderUnit(nil, nil, "is"); egress.Unit != nil {
		t.Errorf("no pin and no sample %+v", egress)
	}
}

// An extender member is judged by its client's record active at the end, the
// lowest extender id, else its lowest record.
func TestCtfExtenderRecord(t *testing.T) {
	low, mid, high := server.Id{1}, server.Id{2}, server.Id{3}
	if record := ctfExtenderRecord([]*model.CtfExtenderBoundary{{ExtenderId: high, Active: true}, {ExtenderId: low}, {ExtenderId: mid, Active: true}}); record.ExtenderId != mid {
		t.Errorf("chose %s, want the lowest active", record.ExtenderId)
	}
	if record := ctfExtenderRecord([]*model.CtfExtenderBoundary{{ExtenderId: high}, {ExtenderId: low}}); record.ExtenderId != low {
		t.Errorf("chose %s, want the lowest", record.ExtenderId)
	}
	if ctfExtenderRecord(nil) != nil {
		t.Error("chose a record from none")
	}
}

func TestCtfMemberClientIds(t *testing.T) {
	ids := ctfTestClientIds(3)
	providers, extenders, clients := ctfMemberClientIds([]*model.CtfPresence{
		{Role: model.CtfRoleExtender, ClientId: ids[2]},
		{Role: model.CtfRoleProvider, ClientId: ids[1], CountryCode: "is"},
		{Role: model.CtfRoleProvider, ClientId: ids[1], CountryCode: "de"},
		{Role: model.CtfRoleProvider, ClientId: ids[0]},
		{Role: model.CtfRoleExtender, ClientId: ids[1]},
	})
	if !slices.Equal(providers, ids[:2]) || !slices.Equal(extenders, ids[1:]) || !slices.Equal(clients, ids) {
		t.Fatalf("member ids %v %v %v", providers, extenders, clients)
	}
}

// The sync plans the quarter hour's sample once, from the block it is at, and
// nothing while capture the flag is off.
func TestStCtfSyncPlansEachSampleOnce(t *testing.T) {
	cfg := &StConfig{Enabled: true, ChainId: 964, ContractAddress: [20]byte{1}, NoId: 1}
	SetStConfig(cfg)
	SetStClient(newStubStClient(&StEpochState{}))
	defer SetStConfig(nil)
	defer SetStClient(nil)
	defer model.Testing_SetCtfDataSettings(nil)
	defer ctfPlannedSample.Delete(string(cfg.DeploymentKey()) + "|1")

	state := &StEpochState{Epoch: 8, EpochStartBlock: 9_600_028, TEpochBlocks: 50_400, HeadBlock: 9_600_028 + 75*10 + 3}
	model.Testing_SetCtfDataSettings(&model.CtfDataConfig{})
	if steps, err := StCtfSync(t.Context(), state); err != nil || len(steps) != 0 {
		t.Fatalf("off: %v %v", steps, err)
	}
	model.Testing_SetCtfDataSettings(&model.CtfDataConfig{Presence: model.CtfPresenceConfig{Enabled: true}})
	steps, err := StCtfSync(t.Context(), state)
	if err != nil || len(steps) != 1 || steps[0].Kind != StCtfStepSample || steps[0].Epoch != 8 || steps[0].Index != 10 || steps[0].HeadBlock != state.HeadBlock {
		t.Fatalf("first turn planned %+v %v", steps, err)
	}
	if time.Since(steps[0].PlannedTime) > time.Minute {
		t.Fatal("the sample is not planned now")
	}
	if steps, _ := StCtfSync(t.Context(), state); len(steps) != 0 {
		t.Fatalf("a second turn in the same quarter hour planned %+v", steps)
	}
	state.HeadBlock += 75
	if steps, _ := StCtfSync(t.Context(), state); len(steps) != 1 || steps[0].Index != 11 {
		t.Fatalf("the next quarter hour planned %+v", steps)
	}
	// past the epoch's blocks the mirror has not rolled yet: nothing
	state.HeadBlock = state.EpochStartBlock + state.TEpochBlocks
	if steps, _ := StCtfSync(t.Context(), state); len(steps) != 0 {
		t.Fatalf("a block past the epoch planned %+v", steps)
	}
}
