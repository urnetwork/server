package model

import (
	"net/netip"
	"strconv"
	"testing"

	"github.com/urnetwork/server"
)

// The ipv6 /64 of the connect-time extender tag (extenderAddressCacheIpv6Prefixes)
// and the public provide mode of the contract parties, without a database.

// A caller is the extender of its exact address first, and else, on ipv6, the
// one extender whose addresses share its /64. A /64 that two extenders share
// tags neither past their own addresses; a v4-mapped caller is its v4 address,
// which is never matched by a prefix; a zone is not part of an address; and a
// snapshot that is not there yet tags nothing.
func TestExtenderAddressCacheMatchesAnIpv6CallerByItsPrefix(t *testing.T) {
	extenderId := server.NewId()
	firstSharing := server.NewId()
	secondSharing := server.NewId()
	addrExtenderIds := map[netip.Addr]server.Id{
		netip.MustParseAddr("192.0.2.80"):      extenderId,
		netip.MustParseAddr("2001:db8:80::10"): extenderId,
		// another address of the same extender in its /64 keeps the /64 its own
		netip.MustParseAddr("2001:db8:80::11"): extenderId,
		netip.MustParseAddr("2001:db8:81::10"): firstSharing,
		netip.MustParseAddr("2001:db8:81::20"): secondSharing,
	}
	prefixes := extenderAddressCacheIpv6Prefixes(addrExtenderIds)
	if len(prefixes) != 1 || prefixes[netip.MustParsePrefix("2001:db8:80::/64")] != extenderId {
		t.Fatalf("prefixes = %v, want 2001:db8:80::/64 alone", prefixes)
	}
	snapshot := &extenderAddressCacheSnapshot{
		addrExtenderIds:       addrExtenderIds,
		ipv6PrefixExtenderIds: prefixes,
	}
	for _, c := range []struct {
		ip   string
		want *server.Id
	}{
		{ip: "2001:db8:80::10", want: &extenderId},
		{ip: "2001:db8:80::9e:1", want: &extenderId},
		{ip: "2001:db8:80:0:ffff:ffff:ffff:ffff", want: &extenderId},
		{ip: "fe80::1%eth0"},
		{ip: "2001:db8:80::10%eth0", want: &extenderId},
		{ip: "2001:db8:80::9e:1%eth0", want: &extenderId},
		{ip: "2001:db8:81::20%eth0", want: &secondSharing},
		{ip: "2001:db8:80:1::10"},
		{ip: "2001:db8:81::10", want: &firstSharing},
		{ip: "2001:db8:81::20", want: &secondSharing},
		{ip: "2001:db8:81::30"},
		{ip: "192.0.2.80", want: &extenderId},
		{ip: "::ffff:192.0.2.80", want: &extenderId},
		{ip: "192.0.2.81"},
		{ip: "::ffff:192.0.2.81"},
	} {
		got, found := snapshot.extenderIdForAddress(netip.MustParseAddr(c.ip))
		if c.want == nil {
			if found {
				t.Errorf("%s is tagged %s, want none", c.ip, got)
			}
			continue
		}
		if !found || got != *c.want {
			t.Errorf("%s is tagged %s/%t, want %s", c.ip, got, found, *c.want)
		}
	}

	var noSnapshot *extenderAddressCacheSnapshot
	if got, found := noSnapshot.extenderIdForAddress(netip.MustParseAddr("192.0.2.80")); found {
		t.Fatalf("a missing snapshot tagged %s", got)
	}
}

// Only ipv6 addresses have prefixes, a v4-mapped one being its v4 address.
func TestExtenderAddressCacheHasNoIpv4Prefixes(t *testing.T) {
	prefixes := extenderAddressCacheIpv6Prefixes(map[netip.Addr]server.Id{
		netip.MustParseAddr("192.0.2.90"):        server.NewId(),
		netip.MustParseAddr("::ffff:192.0.2.91"): server.NewId(),
	})
	if len(prefixes) != 0 {
		t.Fatalf("prefixes = %v, want none", prefixes)
	}
}

// The contract party statement names a public provider by the model's own
// public provide mode, which it can only spell as a literal.
func TestContractExtenderPublicProvideModeIsTheModel(t *testing.T) {
	if contractExtenderPublicProvideModeSql != strconv.Itoa(ProvideModePublic) {
		t.Fatalf("the statement's public provide mode is %s, want %d", contractExtenderPublicProvideModeSql, ProvideModePublic)
	}
}
