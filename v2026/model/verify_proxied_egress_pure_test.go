// A provider whose transports declared a proxy egress counts for no unit
// (verify_proxied_egress_model.go): no extender unit (E1), no SN Top board
// prefix from its zero hops, and no tier B prefix. Pure: the inputs are built
// here. A provider that declared nothing, and a direct host, are unchanged.
// Addresses are RFC 5737 documentation addresses.
package model

import (
	"net/netip"
	"testing"

	"github.com/urfoundation/sn/v2026/ledger"

	"github.com/urnetwork/server/v2026"
)

// E1: a record whose provider client declared a proxy egress is not active, so
// it has no unit, in the extender pool's census and capture the flag's alike.
// It holds no address either: a direct record on the same address is not its
// duplicate. A client that declared nothing keeps its unit.
func TestExtenderEligibilityProxiedClientHasNoUnit(t *testing.T) {
	records := func() []*StExtenderCensusRecord {
		return []*StExtenderCensusRecord{
			// a direct host's extender
			stCensusTestRecord(1, 1, "198.51.100.10"),
			// two proxy slots, one on the direct host's address
			stCensusTestRecord(2, 2, "198.51.100.10"),
			stCensusTestRecord(3, 3, "203.0.113.30"),
			// a client that declared nothing
			stCensusTestRecord(4, 4, "192.0.2.40"),
		}
	}
	judge := func(proxied ...byte) ExtenderEligibilities {
		t.Helper()
		records := records()
		rules := stCensusTestRules()
		rules.ProxiedClientIds = map[server.Id]bool{}
		for _, client := range proxied {
			rules.ProxiedClientIds[contractPayoutTestId(client)] = true
		}
		extenderEligibilityPayAll(rules, records)
		eligibilities, err := DecideExtenderEligibility(records, rules)
		if err != nil {
			t.Fatal(err)
		}
		if err := ledger.CheckCensusRows(eligibilities.censusRows()); err != nil {
			t.Fatalf("the census rows do not pass the ledger's rules: %s", err)
		}
		return eligibilities
	}

	// before any declaration, the slots are extenders like any other
	unmarked := judge()
	if e := extenderEligibilityOf(t, unmarked, 3); !e.Eligible() || e.Unit != stCensusTestUnit(netip.MustParseAddr("203.0.113.30")) {
		t.Fatalf("an extender that declared nothing: eligible %v, flags %b", e.Eligible(), e.Flags)
	}
	if e := extenderEligibilityOf(t, unmarked, 2); !e.Duplicate() {
		t.Fatalf("a second record on one address is not a duplicate: flags %b", e.Flags)
	}

	marked := judge(2, 3)
	for _, c := range []struct {
		extender byte
		eligible bool
		unitOf   string
	}{
		{1, true, "198.51.100.10"},
		{2, false, ""},
		{3, false, ""},
		{4, true, "192.0.2.40"},
	} {
		e := extenderEligibilityOf(t, marked, c.extender)
		var unit [32]byte
		family := ledger.UnitNone
		if c.unitOf != "" {
			unit = stCensusTestUnit(netip.MustParseAddr(c.unitOf))
			family = ledger.UnitIPv4
		}
		if e.Eligible() != c.eligible || e.Active() != c.eligible || e.Unit != unit || e.Family != family || e.Duplicate() {
			t.Fatalf("extender %d: eligible %v active %v duplicate %v family %d, want eligible %v and the unit of %q", c.extender, e.Eligible(), e.Active(), e.Duplicate(), e.Family, c.eligible, c.unitOf)
		}
	}
}

// The board's trail evidence: a hop that recorded the zero hash counts as a
// hop and names its network, but adds no prefix. A hop with a prefix adds it
// once.
func TestSnTopTrailEvidenceSkipsTheZeroPrefix(t *testing.T) {
	evidence := &SnTopTrailEvidence{Prefixes: map[server.Id][][32]byte{}, Networks: map[server.Id]server.Id{}}
	seen := map[server.Id]map[[32]byte]bool{}
	direct, proxied := server.NewId(), server.NewId()
	directNetwork, proxiedNetwork := server.NewId(), server.NewId()
	prefix := [32]byte{0x29}
	zero := [32]byte{}
	evidence.addHop(seen, direct, &directNetwork, prefix[:])
	evidence.addHop(seen, direct, &directNetwork, prefix[:])
	for range 6 {
		evidence.addHop(seen, proxied, &proxiedNetwork, zero[:])
	}
	// a malformed prefix is no hop at all, as before
	evidence.addHop(seen, proxied, &proxiedNetwork, []byte{1, 2, 3})

	if evidence.Hops != 8 {
		t.Fatalf("hops = %d, want 8", evidence.Hops)
	}
	if prefixes := evidence.Prefixes[direct]; len(prefixes) != 1 || prefixes[0] != prefix {
		t.Fatalf("the direct provider's prefixes are %x", prefixes)
	}
	if prefixes, ok := evidence.Prefixes[proxied]; ok {
		t.Fatalf("a provider whose hops recorded the zero hash shows prefixes %x", prefixes)
	}
	if evidence.Networks[proxied] != proxiedNetwork || evidence.Networks[direct] != directNetwork {
		t.Fatalf("networks %v", evidence.Networks)
	}
}

// A provider's board contribution: the zero hash is no prefix, whether a
// trail recorded it or a pin holds it.
func TestSnTopProviderContributionSkipsTheZeroPrefix(t *testing.T) {
	prefix := [32]byte{0x29}
	for _, c := range []struct {
		name     string
		provider SnTopProvider
		want     int
	}{
		{"measured direct", SnTopProvider{Measured: true, TrailPrefixes: [][32]byte{prefix}, Presence: 1}, 1},
		{"measured zero hops", SnTopProvider{Measured: true, TrailPrefixes: [][32]byte{{}}, Presence: 1, PrefixHash: prefix, HasPrefix: true}, 0},
		{"pinned direct", SnTopProvider{PrefixHash: prefix, HasPrefix: true, Presence: 1}, 1},
		{"zero pin", SnTopProvider{PrefixHash: [32]byte{}, HasPrefix: true, Presence: 1}, 0},
		{"no pin", SnTopProvider{Presence: 1}, 0},
	} {
		contribution := snTopProviderContribution(&c.provider)
		if len(contribution) != c.want {
			t.Errorf("%s: contributes %d prefixes, want %d", c.name, len(contribution), c.want)
		}
		if _, ok := contribution[[32]byte{}]; ok {
			t.Errorf("%s: contributes the zero prefix", c.name)
		}
	}
}

// Tier B: a zero pin is no prefix. It takes no prefix from another network's
// representative, and a provider with a real pin goes first.
func TestVerifyCohortRankedZeroPinIsNoPrefix(t *testing.T) {
	first, second := server.NewId(), server.NewId()
	recentTop := []map[server.Id]bool{{first: true, second: true}, {first: true, second: true}, {first: true, second: true}}
	networks := []VerifyCohortRankedNetwork{{NetworkId: first, Position: 1}, {NetworkId: second, Position: 2}}
	select_ := func(providers ...VerifyCohortRankedProvider) []server.Id {
		return SelectVerifyCohortRanked(networks, recentTop, providers, map[server.Id]bool{}, map[server.Id]bool{})
	}

	// each network's only provider holds a zero pin: neither takes the other's
	// place
	if representatives := select_(
		VerifyCohortRankedProvider{ClientId: server.Id{0x01}, NetworkId: first, Eligible: true, HasPrefix: true},
		VerifyCohortRankedProvider{ClientId: server.Id{0x02}, NetworkId: second, Eligible: true, HasPrefix: true},
	); len(representatives) != 2 {
		t.Fatalf("representatives %v: a zero pin took another network's place", representatives)
	}

	// the second network's provider with a real pin goes ahead of its lower
	// client id with a zero pin
	direct := server.Id{0x03}
	if representatives := select_(
		VerifyCohortRankedProvider{ClientId: server.Id{0x01}, NetworkId: first, Eligible: true, PrefixHash: [32]byte{0x01}, HasPrefix: true},
		VerifyCohortRankedProvider{ClientId: server.Id{0x02}, NetworkId: second, Eligible: true, HasPrefix: true},
		VerifyCohortRankedProvider{ClientId: direct, NetworkId: second, Eligible: true, PrefixHash: [32]byte{0x02}, HasPrefix: true},
	); len(representatives) != 2 || representatives[1] != direct {
		t.Fatalf("representatives %v, want the second network's provider with a real pin", representatives)
	}
}
