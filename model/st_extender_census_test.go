// The SN25 extender census (sn WHITEPAPER.md §8.6.2): E1, the unit, E3's
// deduplication, E4 and E5 as the census rows carry them. Pure: the records
// are built here, so a failure names the rule.
package model

import (
	"crypto/sha256"
	"net/netip"
	"slices"
	"testing"

	"github.com/urfoundation/sn/ledger"
	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A unit key like §8.4's: the /29 of an IPv4 address, the /48 of an IPv6 one,
// hashed, so two addresses of one block share a unit.
func stCensusTestUnit(ip netip.Addr) [32]byte {
	bits := 29
	if ip.Is6() {
		bits = 48
	}
	return sha256.Sum256([]byte(netip.PrefixFrom(ip, bits).Masked().String()))
}

func stCensusTestRecord(extender byte, client byte, addresses ...string) *StExtenderCensusRecord {
	record := &StExtenderCensusRecord{ExtenderId: contractPayoutTestId(extender), NetworkId: contractPayoutTestId(client + 100), ClientId: contractPayoutTestId(client), Active: true}
	for _, address := range addresses {
		active := address[0] != '-'
		if !active {
			address = address[1:]
		}
		record.Addresses = append(record.Addresses, StExtenderCensusAddress{Ip: netip.MustParseAddr(address), Active: active})
	}
	return record
}

func stCensusTestRules() *StExtenderCensusRules {
	return &StExtenderCensusRules{Unit: stCensusTestUnit, Durable: map[server.Id]bool{}, Declared: map[server.Id]bool{}, Coldkeys: map[server.Id][32]byte{}, HeadBound: map[server.Id]bool{}}
}

func stCensusRowOf(t *testing.T, rows []ledger.CensusRow, extender byte) ledger.CensusRow {
	t.Helper()
	i := slices.IndexFunc(rows, func(row ledger.CensusRow) bool { return row.ExtenderID == [16]byte(contractPayoutTestId(extender)) })
	if i < 0 {
		t.Fatalf("extender %d has no census row", extender)
	}
	return rows[i]
}

// E1 and the unit: a record counts as active when it is active and unrevoked
// and one of its addresses is active; its unit is its active IPv4 address's
// /29, else its active IPv6 address's /48. A record without an active
// address, or revoked, has no unit.
func TestBuildStExtenderCensusActivityAndUnits(t *testing.T) {
	revoked := stCensusTestRecord(4, 4, "198.51.100.30")
	revoked.Active, revoked.Revoked = false, true
	inactive := stCensusTestRecord(5, 5, "198.51.100.40")
	inactive.Active = false
	records := []*StExtenderCensusRecord{
		stCensusTestRecord(1, 1, "198.51.100.9", "2001:db8:1::9"),
		stCensusTestRecord(2, 2, "-198.51.100.20", "2001:db8:2::2"),
		stCensusTestRecord(3, 3, "-198.51.100.25", "-2001:db8:3::3"),
		revoked,
		inactive,
	}
	rows, err := BuildStExtenderCensus(records, stCensusTestRules())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		extender byte
		flags    ledger.CensusFlags
		family   uint8
		unitOf   string
	}{
		{1, ledger.CensusActive, ledger.UnitIPv4, "198.51.100.9"},
		{2, ledger.CensusActive, ledger.UnitIPv6, "2001:db8:2::2"},
		{3, 0, ledger.UnitNone, ""},
		{4, ledger.CensusRevoked, ledger.UnitNone, ""},
		{5, 0, ledger.UnitNone, ""},
	} {
		row := stCensusRowOf(t, rows, c.extender)
		var unit [32]byte
		if c.unitOf != "" {
			unit = stCensusTestUnit(netip.MustParseAddr(c.unitOf))
		}
		if row.Flags != c.flags || row.Family != c.family || row.Unit != unit {
			t.Fatalf("extender %d: flags %b family %d, want %b and %d with the unit of %q", c.extender, row.Flags, row.Family, c.flags, c.family, c.unitOf)
		}
	}
}

// E3: of the active records that share a provider client, an IPv4 address or
// an IPv6 /64, directly or through others, only the lowest by client id and
// extender id counts. A record that shares nothing counts. An inactive
// record shares nothing: it cannot make an active one a duplicate. Two
// extenders in one /29 that share no address both count, in one unit.
func TestBuildStExtenderCensusDeduplicates(t *testing.T) {
	sameClient := stCensusTestRecord(21, 1, "203.0.113.200")
	records := []*StExtenderCensusRecord{
		stCensusTestRecord(11, 1, "198.51.100.9"),
		sameClient, // client 1 again: a duplicate
		stCensusTestRecord(12, 2, "198.51.100.9"),                   // client 1's IPv4 address
		stCensusTestRecord(13, 3, "2001:db8:aa:1::1"),               // counts
		stCensusTestRecord(14, 4, "192.0.2.77", "2001:db8:aa:1::2"), // client 3's /64
		stCensusTestRecord(15, 5, "192.0.2.77"),                     // client 4's address: a duplicate through 4
		stCensusTestRecord(16, 6, "198.51.100.10"),                  // client 1's /29, another address: counts
		stCensusTestRecord(17, 7, "-198.51.100.30", "2001:db8:bb::7"),
		stCensusTestRecord(18, 8, "198.51.100.30"), // only client 7's inactive address: counts
	}
	inactive := stCensusTestRecord(19, 9, "203.0.113.5")
	inactive.Active = false
	records = append(records, inactive, stCensusTestRecord(20, 10, "203.0.113.5")) // an inactive record's address: counts
	rows, err := BuildStExtenderCensus(records, stCensusTestRules())
	if err != nil {
		t.Fatal(err)
	}
	duplicates := map[byte]bool{21: true, 12: true, 14: true, 15: true}
	for _, record := range records {
		extender := byte(0)
		for n := byte(1); n < 255; n++ {
			if record.ExtenderId == contractPayoutTestId(n) {
				extender = n
			}
		}
		row := stCensusRowOf(t, rows, extender)
		if record.Active && (row.Flags&ledger.CensusDuplicate != 0) != duplicates[extender] {
			t.Fatalf("extender %d: duplicate %v, want %v", extender, row.Flags&ledger.CensusDuplicate != 0, duplicates[extender])
		}
	}
	if a, b := stCensusRowOf(t, rows, 11), stCensusRowOf(t, rows, 16); a.Unit != b.Unit || a.Flags != ledger.CensusActive || b.Flags != ledger.CensusActive {
		t.Fatal("two extenders of one /29 that share no address do not both count in one unit")
	}
	for i := 1; i < len(rows); i++ {
		if c := server.Id(rows[i-1].ClientID).Cmp(server.Id(rows[i].ClientID)); c > 0 || c == 0 && server.Id(rows[i-1].ExtenderID).Cmp(server.Id(rows[i].ExtenderID)) >= 0 {
			t.Fatalf("rows %d and %d are out of order", i-1, i)
		}
	}
}

// E4 and E5 flags: the gated tier and the durable networks, canaries, the
// declared networks and head-bound clients each mark their records, and the
// coldkey is the provider client's. Only a record with none of them and a
// coldkey is eligible.
func TestBuildStExtenderCensusFlagsAndWallets(t *testing.T) {
	gatedTier := stCensusTestRecord(2, 2, "198.51.100.2")
	gatedTier.DirectoryTier = connect.ExtenderDirectoryTierGated
	canary := stCensusTestRecord(4, 4, "198.51.100.40")
	canary.Canary = true
	records := []*StExtenderCensusRecord{
		stCensusTestRecord(1, 1, "198.51.100.1"),
		gatedTier,
		stCensusTestRecord(3, 3, "198.51.100.30"),
		canary,
		stCensusTestRecord(5, 5, "198.51.100.50"),
		stCensusTestRecord(6, 6, "198.51.100.60"),
		stCensusTestRecord(7, 7, "198.51.100.70"),
	}
	rules := stCensusTestRules()
	rules.Durable[records[2].NetworkId] = true
	rules.Declared[records[4].NetworkId] = true
	rules.HeadBound[records[5].ClientId] = true
	for _, record := range records[:6] {
		rules.Coldkeys[record.ClientId] = [32]byte{byte(record.ClientId[15]), 0xc0}
	}
	rows, err := BuildStExtenderCensus(records, rules)
	if err != nil {
		t.Fatal(err)
	}
	for extender, flag := range map[byte]ledger.CensusFlags{1: 0, 2: ledger.CensusGated, 3: ledger.CensusGated, 4: ledger.CensusCanary, 5: ledger.CensusDeclared, 6: ledger.CensusHeadBound, 7: 0} {
		row := stCensusRowOf(t, rows, extender)
		if row.Flags != ledger.CensusActive|flag {
			t.Fatalf("extender %d: flags %b, want %b", extender, row.Flags, ledger.CensusActive|flag)
		}
		if eligible := row.Eligible(); eligible != (extender == 1) {
			t.Fatalf("extender %d: eligible %v", extender, eligible)
		}
	}
	if row := stCensusRowOf(t, rows, 1); row.Coldkey != rules.Coldkeys[records[0].ClientId] {
		t.Fatal("the coldkey is not the provider client's")
	}
	if row := stCensusRowOf(t, rows, 7); row.Coldkey != ([32]byte{}) {
		t.Fatal("a client without a wallet has a coldkey")
	}
}

// A stored census decodes to its rows, and a stored census that breaks the
// census rules does not decode.
func TestStExtenderCensusStorage(t *testing.T) {
	rows, err := BuildStExtenderCensus([]*StExtenderCensusRecord{
		stCensusTestRecord(1, 1, "198.51.100.1"),
		stCensusTestRecord(2, 2, "-198.51.100.2"),
	}, stCensusTestRules())
	if err != nil {
		t.Fatal(err)
	}
	stored := EncodeStExtenderCensus(rows)
	if len(stored) != 2*ledger.CensusRowBytes {
		t.Fatalf("stored %d bytes", len(stored))
	}
	decoded, err := DecodeStExtenderCensus(stored)
	if err != nil || !slices.Equal(decoded, rows) {
		t.Fatalf("decoded %+v %v", decoded, err)
	}
	if decoded, err := DecodeStExtenderCensus(nil); err != nil || len(decoded) != 0 {
		t.Fatalf("an empty census: %+v %v", decoded, err)
	}
	swapped := append(slices.Clone(stored[ledger.CensusRowBytes:]), stored[:ledger.CensusRowBytes]...)
	if _, err := DecodeStExtenderCensus(swapped); err == nil {
		t.Fatal("decoded an unsorted census")
	}
	if _, err := DecodeStExtenderCensus(stored[:ledger.CensusRowBytes+1]); err == nil {
		t.Fatal("decoded a partial row")
	}
	if _, err := BuildStExtenderCensus([]*StExtenderCensusRecord{stCensusTestRecord(1, 1, "198.51.100.1"), stCensusTestRecord(1, 2, "198.51.100.2")}, stCensusTestRules()); err == nil {
		t.Fatal("built a census that lists an extender twice")
	}
}
