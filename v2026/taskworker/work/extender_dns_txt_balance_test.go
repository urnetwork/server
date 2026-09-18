package work

// Family balance in the TXT sets.
//
// The gossip release balances families explicitly, by interleaving two pools
// (see connect.balanceRecordsByIpFamily). The TXT path reaches the same
// property a different way and must NOT be routed through a dropping sampler,
// for a reason that is easy to miss:
//
//	"Built from the sampled sets rather than the pools so a client is vouched
//	 for exactly the addresses it was answered with"
//
// A TXT set vouches for every extender behind the addresses the A and AAAA sets
// answered with. Dropping an extender to hit a family target would leave a
// client holding an address from A/AAAA that no TXT record vouches for, and it
// could not verify that extender at all -- a worse outcome than an unbalanced
// sample.
//
// Balance is instead a consequence of the address sampling upstream: A draws up
// to sampleCount from the v4 pool and AAAA up to sampleCount from the v6 pool,
// independently. These tests pin that the resulting TXT set is balanced when
// both pools are deep and degrades to "more of what it has" when one is short,
// so the guarantee has the same standing as the gossip one rather than being
// incidental.

import (
	mathrand "math/rand/v2"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// txtExtenderFamilies reports, for the TXT set at `location`, how many distinct
// extenders behind it are reachable over each family.
func txtExtenderFamilies(
	t *testing.T,
	desiredSets []*extenderDnsRecordSet,
	addresses []*model.NetworkExtenderDnsAddress,
	location string,
) (ipv4 int, ipv6 int) {
	t.Helper()

	extenderFamilies := map[server.Id]map[int]bool{}
	for _, address := range addresses {
		if _, ok := extenderFamilies[address.ExtenderId]; !ok {
			extenderFamilies[address.ExtenderId] = map[int]bool{}
		}
		extenderFamilies[address.ExtenderId][address.IpVersion] = true
	}

	// which extenders the address sets for this location answered with
	answered := map[server.Id]bool{}
	ipExtenders := map[string]server.Id{}
	for _, address := range addresses {
		ipExtenders[address.Ip.String()] = address.ExtenderId
	}
	for _, desiredSet := range desiredSets {
		if desiredSet.continentCode != location || desiredSet.ipVersion == 0 {
			continue
		}
		for _, ip := range desiredSet.ips {
			if extenderId, ok := ipExtenders[ip]; ok {
				answered[extenderId] = true
			}
		}
	}
	for extenderId := range answered {
		if extenderFamilies[extenderId][4] {
			ipv4 += 1
		}
		if extenderFamilies[extenderId][6] {
			ipv6 += 1
		}
	}
	return ipv4, ipv6
}

// Both pools deep: the answered set carries sampleCount of each family.
func TestExtenderDnsTxtBalancesWhenBothFamiliesAreDeep(t *testing.T) {
	addresses := []*model.NetworkExtenderDnsAddress{}
	addresses = append(addresses, testExtenderDnsAddresses("us", 4, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)...)
	addresses = append(addresses, testExtenderDnsAddresses("us", 6, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)...)

	desiredSets := sampleTestExtenderDnsRecordSets(addresses, 4)
	ipv4, ipv6 := txtExtenderFamilies(t, desiredSets, addresses, "NA")
	if ipv4 != 4 || ipv6 != 4 {
		t.Fatalf("answered %d v4 and %d v6 extenders, want 4 and 4", ipv4, ipv6)
	}
}

// "if there are no more ipv4/ipv6 then it can use more of what it has": a thin
// v6 pool contributes everything it has and the v4 side is unaffected.
func TestExtenderDnsTxtUsesWhatItHasWhenOneFamilyIsShort(t *testing.T) {
	addresses := []*model.NetworkExtenderDnsAddress{}
	addresses = append(addresses, testExtenderDnsAddresses("us", 4, 1, 2, 3, 4, 5, 6, 7, 8)...)
	addresses = append(addresses, testExtenderDnsAddresses("us", 6, 1, 2)...)

	desiredSets := sampleTestExtenderDnsRecordSets(addresses, 4)
	ipv4, ipv6 := txtExtenderFamilies(t, desiredSets, addresses, "NA")
	if ipv6 != 2 {
		t.Fatalf("answered %d v6 extenders, want all 2 that exist", ipv6)
	}
	if ipv4 != 4 {
		t.Fatalf("answered %d v4 extenders, want the full sample of 4", ipv4)
	}
}

// A family with nothing anywhere produces no set at all, so the other family is
// answered alone rather than with an empty record.
func TestExtenderDnsTxtWithOnlyOneFamily(t *testing.T) {
	addresses := testExtenderDnsAddresses("us", 4, 1, 2, 3, 4, 5, 6)

	desiredSets := sampleTestExtenderDnsRecordSets(addresses, 4)
	ipv4, ipv6 := txtExtenderFamilies(t, desiredSets, addresses, "NA")
	if ipv4 != 4 || ipv6 != 0 {
		t.Fatalf("answered %d v4 and %d v6 extenders, want 4 and 0", ipv4, ipv6)
	}
}

// The invariant that rules out a dropping sampler here: every address the A and
// AAAA sets answer with belongs to an extender the TXT set vouches for.
func TestExtenderDnsTxtVouchesForEveryAnsweredAddress(t *testing.T) {
	addresses := []*model.NetworkExtenderDnsAddress{}
	addresses = append(addresses, testExtenderDnsAddresses("us", 4, 1, 2, 3, 4, 5, 6)...)
	addresses = append(addresses, testExtenderDnsAddresses("us", 6, 1, 2, 3)...)

	signed := map[server.Id]bool{}
	desiredSets := sampleExtenderDnsRecordSets(
		addresses,
		4,
		mathrand.New(mathrand.NewPCG(7, 11)),
		func(extenderId server.Id) (string, bool) {
			signed[extenderId] = true
			return "record-" + extenderId.String(), true
		},
	)

	ipExtenders := map[string]server.Id{}
	for _, address := range addresses {
		ipExtenders[address.Ip.String()] = address.ExtenderId
	}
	for _, desiredSet := range desiredSets {
		if desiredSet.ipVersion == 0 {
			continue
		}
		for _, ip := range desiredSet.ips {
			extenderId, ok := ipExtenders[ip]
			if !ok {
				t.Fatalf("answered with %s, which belongs to no extender", ip)
			}
			if !signed[extenderId] {
				t.Fatalf("answered with %s but no TXT record vouches for its extender", ip)
			}
		}
	}
}
