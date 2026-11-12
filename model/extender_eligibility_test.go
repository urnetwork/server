// The shared extender eligibility (extender_eligibility.go, sn WHITEPAPER.md
// §8.6.2 E1 and E3–E5), one rule at a time. Pure: the records are built here
// (st_extender_census_test.go's helpers), so a failure names the rule. Both
// the extender pool's census and capture the flag's judge extenders by these.
package model

import (
	"math/rand/v2"
	"net/netip"
	"slices"
	"testing"

	"github.com/urfoundation/sn/ledger"
	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func extenderEligibilityOf(t *testing.T, judged ExtenderEligibilities, extender byte) *ExtenderEligibility {
	t.Helper()
	for _, eligibility := range judged {
		if eligibility.Record.ExtenderId == contractPayoutTestId(extender) {
			return eligibility
		}
	}
	t.Fatalf("extender %d is not judged", extender)
	return nil
}

func extenderEligibilityTestRules() *ExtenderEligibilityRules {
	rules := stCensusTestRules()
	rules.CanaryNetworks = map[server.Id]bool{}
	return rules
}

// Every client of the records has a wallet.
func extenderEligibilityPayAll(rules *ExtenderEligibilityRules, records []*StExtenderCensusRecord) {
	for _, record := range records {
		rules.Coldkeys[record.ClientId] = [32]byte{0xc0, byte(record.ClientId[15])}
	}
}

// E1: active and unrevoked, with an active address; only then a unit, of the
// active IPv4 address's /29, else of the active IPv6 address's /48.
func TestExtenderEligibilityE1(t *testing.T) {
	revoked := stCensusTestRecord(3, 3, "198.51.100.30")
	revoked.Revoked = true
	inactive := stCensusTestRecord(4, 4, "198.51.100.40")
	inactive.Active = false
	records := []*StExtenderCensusRecord{
		stCensusTestRecord(1, 1, "198.51.100.10", "2001:db8:1::1"),
		stCensusTestRecord(2, 2, "-198.51.100.20", "2001:db8:2::2"),
		revoked,
		inactive,
		stCensusTestRecord(5, 5, "-198.51.100.50", "-2001:db8:5::5"),
		stCensusTestRecord(6, 6),
	}
	rules := extenderEligibilityTestRules()
	extenderEligibilityPayAll(rules, records)
	judged, err := DecideExtenderEligibility(records, rules)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		extender byte
		active   bool
		family   uint8
		unitOf   string
	}{
		{1, true, ledger.UnitIPv4, "198.51.100.10"},
		{2, true, ledger.UnitIPv6, "2001:db8:2::2"},
		{3, false, ledger.UnitNone, ""},
		{4, false, ledger.UnitNone, ""},
		{5, false, ledger.UnitNone, ""},
		{6, false, ledger.UnitNone, ""},
	} {
		e := extenderEligibilityOf(t, judged, c.extender)
		var unit [32]byte
		if c.unitOf != "" {
			unit = stCensusTestUnit(netip.MustParseAddr(c.unitOf))
		}
		if e.Active() != c.active || e.Family != c.family || e.Unit != unit || e.Eligible() != c.active {
			t.Fatalf("extender %d: active %v family %d eligible %v, want %v, %d and the unit of %q", c.extender, e.Active(), e.Family, e.Eligible(), c.active, c.family, c.unitOf)
		}
	}
	if e := extenderEligibilityOf(t, judged, 3); e.Flags != ledger.CensusRevoked {
		t.Fatalf("a revoked record's flags are %b", e.Flags)
	}
}

// E3's chain: A and B share an IPv4 address, B and C an IPv6 /64, and A and C
// nothing. The three are one component, so only A, the lowest (client id,
// extender id), counts. (A rule that compared each record only with the
// records counted before it would count C as well.)
func TestExtenderEligibilityE3Chain(t *testing.T) {
	records := []*StExtenderCensusRecord{
		stCensusTestRecord(1, 1, "198.51.100.10"),
		stCensusTestRecord(2, 2, "198.51.100.10", "2001:db8:aa:1::2"),
		stCensusTestRecord(3, 3, "2001:db8:aa:1::3"),
		stCensusTestRecord(4, 4, "203.0.113.4"),
	}
	rules := extenderEligibilityTestRules()
	extenderEligibilityPayAll(rules, records)
	judged, err := DecideExtenderEligibility(records, rules)
	if err != nil {
		t.Fatal(err)
	}
	for extender, duplicate := range map[byte]bool{1: false, 2: true, 3: true, 4: false} {
		e := extenderEligibilityOf(t, judged, extender)
		if e.Duplicate() != duplicate || e.Eligible() == duplicate {
			t.Fatalf("extender %d: duplicate %v eligible %v, want duplicate %v", extender, e.Duplicate(), e.Eligible(), duplicate)
		}
	}
}

// E3 judges every active record, whatever else it fails: a lower record that
// cannot be paid (no wallet, a fleet binding, the operator's own network)
// still holds its address, so the record sharing it is a duplicate and the
// address earns nothing in the pool.
func TestExtenderEligibilityE3LowerIneligibleRecordHolds(t *testing.T) {
	for _, c := range []struct {
		name  string
		lower func(rules *ExtenderEligibilityRules, lower *StExtenderCensusRecord)
	}{
		{"no wallet", func(rules *ExtenderEligibilityRules, lower *StExtenderCensusRecord) {
			delete(rules.Coldkeys, lower.ClientId)
		}},
		{"head-bound", func(rules *ExtenderEligibilityRules, lower *StExtenderCensusRecord) {
			rules.HeadBound[lower.ClientId] = true
		}},
		{"durable network", func(rules *ExtenderEligibilityRules, lower *StExtenderCensusRecord) {
			rules.Durable[lower.NetworkId] = true
		}},
		{"declared network", func(rules *ExtenderEligibilityRules, lower *StExtenderCensusRecord) {
			rules.Declared[lower.NetworkId] = true
		}},
		{"canary", func(rules *ExtenderEligibilityRules, lower *StExtenderCensusRecord) {
			lower.Canary = true
		}},
	} {
		lower, higher := stCensusTestRecord(1, 1, "198.51.100.10"), stCensusTestRecord(2, 2, "198.51.100.10")
		records := []*StExtenderCensusRecord{lower, higher}
		rules := extenderEligibilityTestRules()
		extenderEligibilityPayAll(rules, records)
		c.lower(rules, lower)
		judged, err := DecideExtenderEligibility(records, rules)
		if err != nil {
			t.Fatal(err)
		}
		l, h := extenderEligibilityOf(t, judged, 1), extenderEligibilityOf(t, judged, 2)
		if l.Duplicate() || l.Eligible() || !h.Duplicate() || h.Eligible() {
			t.Fatalf("%s: lower duplicate %v eligible %v, higher duplicate %v eligible %v", c.name, l.Duplicate(), l.Eligible(), h.Duplicate(), h.Eligible())
		}
	}
}

// E3 by provider client: a client's lowest active record counts and its other
// active records are duplicates; an inactive record shares nothing, so it can
// make no other record a duplicate. Addresses in one IPv6 /64 are one address;
// two /64s of one /48 are two, in one unit.
func TestExtenderEligibilityE3ClientsAndAddresses(t *testing.T) {
	inactiveLow := stCensusTestRecord(10, 1, "203.0.113.1")
	inactiveLow.Active = false
	inactiveOther := stCensusTestRecord(30, 3, "203.0.113.30")
	inactiveOther.Active = false
	records := []*StExtenderCensusRecord{
		inactiveLow,
		stCensusTestRecord(11, 1, "198.51.100.11"),
		stCensusTestRecord(12, 1, "198.51.100.12"),
		stCensusTestRecord(20, 2, "203.0.113.1"), // only client 1's inactive record's address
		inactiveOther,
		stCensusTestRecord(31, 4, "203.0.113.30"),     // only an inactive record's address
		stCensusTestRecord(40, 5, "2001:db8:bb:1::1"), // the /64 of 41
		stCensusTestRecord(41, 6, "2001:db8:bb:1::ffff"),
		stCensusTestRecord(42, 7, "2001:db8:bb:2::1"), // another /64 of the same /48
	}
	rules := extenderEligibilityTestRules()
	extenderEligibilityPayAll(rules, records)
	judged, err := DecideExtenderEligibility(records, rules)
	if err != nil {
		t.Fatal(err)
	}
	for extender, duplicate := range map[byte]bool{11: false, 12: true, 20: false, 31: false, 40: false, 41: true, 42: false} {
		if e := extenderEligibilityOf(t, judged, extender); e.Duplicate() != duplicate {
			t.Fatalf("extender %d: duplicate %v, want %v", extender, e.Duplicate(), duplicate)
		}
	}
	if extenderEligibilityOf(t, judged, 10).Duplicate() || extenderEligibilityOf(t, judged, 30).Duplicate() {
		t.Fatal("an inactive record is a duplicate")
	}
	if a, b := extenderEligibilityOf(t, judged, 40), extenderEligibilityOf(t, judged, 42); a.Unit != b.Unit || !a.Eligible() || !b.Eligible() {
		t.Fatal("two /64s of one /48 do not both count, in one unit")
	}
}

// E4: the provider client's coldkey, and its fleet binding at either
// boundary. E5: the gated tier, a durable network, a canary, a declared
// network and a network that hosts a canary each make a record the operator's
// own. Only a record with none of them, and a wallet, is eligible.
func TestExtenderEligibilityE4AndE5(t *testing.T) {
	gatedTier := stCensusTestRecord(2, 2, "198.51.100.2")
	gatedTier.DirectoryTier = connect.ExtenderDirectoryTierGated
	canary := stCensusTestRecord(4, 4, "198.51.100.4")
	canary.Canary = true
	records := []*StExtenderCensusRecord{
		stCensusTestRecord(1, 1, "198.51.100.1"),
		gatedTier,
		stCensusTestRecord(3, 3, "198.51.100.3"),
		canary,
		stCensusTestRecord(5, 5, "198.51.100.5"),
		stCensusTestRecord(6, 6, "198.51.100.6"),
		stCensusTestRecord(7, 7, "198.51.100.7"),
		stCensusTestRecord(8, 8, "198.51.100.8"),
	}
	rules := extenderEligibilityTestRules()
	extenderEligibilityPayAll(rules, records)
	rules.Durable[records[2].NetworkId] = true
	rules.Declared[records[4].NetworkId] = true
	rules.CanaryNetworks[records[5].NetworkId] = true
	rules.HeadBound[records[6].ClientId] = true
	delete(rules.Coldkeys, records[7].ClientId)
	judged, err := DecideExtenderEligibility(records, rules)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		extender  byte
		flag      ledger.CensusFlags
		own, head bool
		payable   bool
	}{
		{1, 0, false, false, true},
		{2, ledger.CensusGated, true, false, true},
		{3, ledger.CensusGated, true, false, true},
		{4, ledger.CensusCanary, true, false, true},
		{5, ledger.CensusDeclared, true, false, true},
		{6, ledger.CensusDeclared, true, false, true},
		{7, ledger.CensusHeadBound, false, true, true},
		{8, 0, false, false, false},
	} {
		e := extenderEligibilityOf(t, judged, c.extender)
		if e.Flags != ledger.CensusActive|c.flag || e.OwnNetwork() != c.own || e.HeadBound() != c.head || e.Payable() != c.payable {
			t.Fatalf("extender %d: flags %b own %v head %v payable %v", c.extender, e.Flags, e.OwnNetwork(), e.HeadBound(), e.Payable())
		}
		if e.Eligible() != (c.extender == 1) {
			t.Fatalf("extender %d: eligible %v", c.extender, e.Eligible())
		}
		if row := e.CensusRow(); row.Eligible() != e.Eligible() || row.Flags != e.Flags || row.Coldkey != e.Coldkey {
			t.Fatalf("extender %d: its census row reads otherwise", c.extender)
		}
	}
}

// A client is judged by its active record with the lowest extender id, else
// by its lowest record; a client with no record has none.
func TestExtenderEligibilityJudged(t *testing.T) {
	inactive := stCensusTestRecord(1, 1, "198.51.100.1")
	inactive.Active = false
	onlyInactive := stCensusTestRecord(5, 2, "198.51.100.5")
	onlyInactive.Active = false
	records := []*StExtenderCensusRecord{
		stCensusTestRecord(3, 1, "198.51.100.3"),
		inactive,
		stCensusTestRecord(2, 1, "198.51.100.2"),
		onlyInactive,
		stCensusTestRecord(4, 2, "-198.51.100.4"),
	}
	rules := extenderEligibilityTestRules()
	extenderEligibilityPayAll(rules, records)
	judged, err := DecideExtenderEligibility(records, rules)
	if err != nil {
		t.Fatal(err)
	}
	if e := judged.Judged(contractPayoutTestId(1)); e == nil || e.Record.ExtenderId != contractPayoutTestId(2) || e.Duplicate() {
		t.Fatalf("client 1 is judged by %+v, want its lowest active record, 2", e)
	}
	if e := judged.Judged(contractPayoutTestId(2)); e == nil || e.Record.ExtenderId != contractPayoutTestId(4) {
		t.Fatalf("client 2 is judged by %+v, want its lowest record, 4", e)
	}
	if judged.Judged(contractPayoutTestId(9)) != nil {
		t.Fatal("a client without records is judged")
	}
}

// The judgement depends on the records only, never on their order, and
// refuses a record listed twice or a rule set without a unit key.
func TestExtenderEligibilityOrderAndRefusals(t *testing.T) {
	records := []*StExtenderCensusRecord{
		stCensusTestRecord(1, 1, "198.51.100.10"),
		stCensusTestRecord(2, 2, "198.51.100.10", "2001:db8:aa:1::2"),
		stCensusTestRecord(3, 3, "2001:db8:aa:1::3"),
		stCensusTestRecord(4, 3, "203.0.113.4"),
		stCensusTestRecord(5, 5, "203.0.113.4"),
		stCensusTestRecord(6, 6, "192.0.2.6"),
	}
	rules := extenderEligibilityTestRules()
	extenderEligibilityPayAll(rules, records)
	want, err := DecideExtenderEligibility(records, rules)
	if err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewPCG(1, 2))
	for range 20 {
		shuffled := slices.Clone(records)
		random.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got, err := DecideExtenderEligibility(shuffled, rules)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.CensusRows(), want.CensusRows()) {
			t.Fatal("the judgement depends on the records' order")
		}
	}
	if _, err := DecideExtenderEligibility(append(slices.Clone(records), records[0]), rules); err == nil {
		t.Fatal("a record listed twice was judged")
	}
	if _, err := DecideExtenderEligibility(records, &ExtenderEligibilityRules{}); err == nil {
		t.Fatal("rules without a unit key judged records")
	}
	zero := extenderEligibilityTestRules()
	zero.Unit = func(netip.Addr) [32]byte { return [32]byte{} }
	if _, err := DecideExtenderEligibility(records, zero); err == nil {
		t.Fatal("an active record with a zero unit was judged")
	}
}
