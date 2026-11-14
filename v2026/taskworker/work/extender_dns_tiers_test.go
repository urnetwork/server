package work

// The dns sets as an open channel of the tiered directory
// (connect/EXTENDER.md R1, R2, R4): the gated tier is in no set, a location
// is bound to its partition across epochs, and a canary is pinned in its one
// region. Pure sampler runs under the fixed secret and epoch; no database.

import (
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The rows of one extender in the gated tier, or a canary of one channel.
func testExtenderDnsTieredAddress(
	countryCode string,
	ipVersion int,
	index int,
	directoryTier int,
	canaryChannel string,
) *model.NetworkExtenderDnsAddress {
	address := testExtenderDnsAddresses(countryCode, ipVersion, index)[0]
	address.DirectoryTier = directoryTier
	address.CanaryChannel = canaryChannel
	return address
}

// Every address of every set, with the set it was answered in.
func testExtenderDnsAnsweredIps(desiredSets []*extenderDnsRecordSet) map[string][]string {
	answered := map[string][]string{}
	for _, desiredSet := range desiredSets {
		for _, ip := range desiredSet.ips {
			answered[ip] = append(answered[ip], desiredSet.setIdentifier())
		}
	}
	return answered
}

// Root cause: operator hosts are in the open channels. A gated address is in
// no dns set and no TXT set, whatever its continent and however short the
// sets around it are (R1).
func TestExtenderDnsSampleExcludesTheGatedTier(t *testing.T) {
	addresses := slices.Concat(
		testExtenderDnsAddresses("DE", 4, 1, 2),
		[]*model.NetworkExtenderDnsAddress{
			testExtenderDnsTieredAddress("DE", 4, 3, connect.ExtenderDirectoryTierGated, ""),
			testExtenderDnsTieredAddress("US", 4, 4, connect.ExtenderDirectoryTierGated, ""),
			testExtenderDnsTieredAddress("US", 6, 5, connect.ExtenderDirectoryTierGated, ""),
			// a gated canary is placed by its gated partition, not by dns
			testExtenderDnsTieredAddress("DE", 4, 6, connect.ExtenderDirectoryTierGated, connect.ExtenderChannelGated),
		},
	)
	signed := map[server.Id]bool{}
	desiredSets := sampleExtenderDnsRecordSets(
		addresses,
		8,
		testExtenderDnsSecret,
		testExtenderDnsEpoch,
		func(extenderId server.Id) (string, bool) {
			signed[extenderId] = true
			return "record-" + extenderId.String(), true
		},
	)
	answered := testExtenderDnsAnsweredIps(desiredSets)
	for _, index := range []int{3, 4, 6} {
		if sets, ok := answered[testExtenderDnsIp(4, index)]; ok {
			t.Fatalf("gated address %d was answered in %v", index, sets)
		}
	}
	if sets, ok := answered[testExtenderDnsIp(6, 5)]; ok {
		t.Fatalf("the gated v6 address was answered in %v", sets)
	}
	// the gated family has no open address: no AAAA set at all
	for _, desiredSet := range desiredSets {
		if desiredSet.ipVersion == 6 {
			t.Fatalf("%s was published for a family with no open address", desiredSet.setIdentifier())
		}
	}
	// the open ones are there, and only they were signed for
	assertTestExtenderDnsSet(t, desiredSets, "extender-EU-A", 2, testExtenderDnsIps(4, 1, 2))
	assertTestExtenderDnsSet(t, desiredSets, "extender-default-A", 2, testExtenderDnsIps(4, 1, 2))
	for _, address := range addresses {
		if address.DirectoryTier == connect.ExtenderDirectoryTierGated && signed[address.ExtenderId] {
			t.Fatalf("a gated extender was signed for a TXT set")
		}
	}
}

// Root cause: one observer enumerates the fleet because the sets are drawn
// from the whole pool every tick. A location is a vantage bound to its
// partition: over many epochs its sets reach the partition and no more, the
// same epoch draws the same set, and the default location is its own vantage
// with its own partition (R2).
func TestExtenderDnsSampleBindsALocationToItsPartition(t *testing.T) {
	indexes := []int{}
	for i := range 64 {
		indexes = append(indexes, 1+i)
	}
	addresses := testExtenderDnsAddresses("DE", 4, indexes...)
	sampleEpoch := func(epoch uint64, setIdentifier string) []string {
		desiredSets := sampleExtenderDnsRecordSets(addresses, testExtenderDnsSampleCount, testExtenderDnsSecret, epoch, nil)
		return assertTestExtenderDnsSet(t, desiredSets, setIdentifier, testExtenderDnsSampleCount, testExtenderDnsIps(4, indexes...)).ips
	}
	if first, again := sampleEpoch(3, "extender-EU-A"), sampleEpoch(3, "extender-EU-A"); !slices.Equal(first, again) {
		t.Fatalf("one epoch drew %v and %v", first, again)
	}
	seen := map[string]bool{}
	for epoch := range uint64(24 * 7) {
		for _, ip := range sampleEpoch(epoch, "extender-EU-A") {
			seen[ip] = true
		}
	}
	// sixty-four addresses make eight partitions of about eight; a week of
	// hourly epochs that saw more than half the pool was not bound to one
	if 32 < len(seen) {
		t.Fatalf("a week of epochs answered %d of %d addresses from one location", len(seen), len(addresses))
	}
	if len(seen) <= testExtenderDnsSampleCount {
		t.Fatalf("a week of epochs never rotated past one set: %d addresses", len(seen))
	}
	// the exact bound: the location's partition under the pinned secret
	keyHexes := []string{}
	for _, address := range addresses {
		keyHexes = append(keyHexes, extenderDnsAddressKeyHex(address))
	}
	members, _, _ := connect.ExtenderPartitionMembers(testExtenderDnsSecret, connect.ExtenderChannelDns, []byte("EU"), keyHexes)
	memberIps := map[string]bool{}
	for _, address := range addresses {
		if slices.Contains(members, extenderDnsAddressKeyHex(address)) {
			memberIps[address.Ip.String()] = true
		}
	}
	for ip := range seen {
		if !memberIps[ip] {
			t.Fatalf("%s was answered from outside the location's partition", ip)
		}
	}
}

// Root cause: a leak cannot be attributed. A dns canary is pinned in its
// own region's sets every epoch and is in no other location's set -- not a
// short continent's fill and not the default sets -- so a block of it names
// its region and nothing else (R4). A canary is still vouched for by its
// region's TXT set, since a client is answered with it.
func TestExtenderDnsSamplePinsACanaryInItsRegionOnly(t *testing.T) {
	canary := testExtenderDnsTieredAddress("DE", 4, 1, connect.ExtenderDirectoryTierOpen, connect.ExtenderChannelDns)
	addresses := slices.Concat(
		[]*model.NetworkExtenderDnsAddress{canary},
		testExtenderDnsAddresses("DE", 4, 2, 3, 4, 5, 6, 7, 8, 9),
		// a short continent, which fills from elsewhere
		testExtenderDnsAddresses("BR", 4, 10),
	)
	canaryIp := canary.Ip.String()
	for epoch := range uint64(48) {
		signed := map[server.Id]bool{}
		desiredSets := sampleExtenderDnsRecordSets(
			addresses,
			testExtenderDnsSampleCount,
			testExtenderDnsSecret,
			epoch,
			func(extenderId server.Id) (string, bool) {
				signed[extenderId] = true
				return "record-" + extenderId.String(), true
			},
		)
		answered := testExtenderDnsAnsweredIps(desiredSets)
		if sets := answered[canaryIp]; !slices.Equal(sets, []string{"extender-EU-A"}) {
			t.Fatalf("epoch %d answered the canary in %v, expected its region's set alone", epoch, sets)
		}
		europe := assertTestExtenderDnsSet(t, desiredSets, "extender-EU-A", testExtenderDnsSampleCount, testExtenderDnsIps(4, 1, 2, 3, 4, 5, 6, 7, 8, 9))
		if europe.ips[0] != canaryIp {
			t.Fatalf("epoch %d did not pin the canary first: %v", epoch, europe.ips)
		}
		// south america is short and filled, never with the canary
		southAmerica := assertTestExtenderDnsSet(t, desiredSets, "extender-SA-A", testExtenderDnsSampleCount, testExtenderDnsIps(4, 2, 3, 4, 5, 6, 7, 8, 9, 10))
		if !slices.Contains(southAmerica.ips, testExtenderDnsIp(4, 10)) {
			t.Fatalf("epoch %d dropped south america's own address: %v", epoch, southAmerica.ips)
		}
		if !signed[canary.ExtenderId] {
			t.Fatalf("epoch %d did not vouch for the canary", epoch)
		}
		if !slices.Contains(testExtenderDnsTxtSet(desiredSets, "EU").records, "record-"+canary.ExtenderId.String()) {
			t.Fatalf("epoch %d left the canary out of its region's TXT set", epoch)
		}
	}
	// and the ip is a documentation address, as every fixture must be
	if !netip.MustParsePrefix("198.51.100.0/24").Contains(canary.Ip) {
		t.Fatal(fmt.Sprintf("the canary fixture is not a documentation address: %s", canaryIp))
	}
}
