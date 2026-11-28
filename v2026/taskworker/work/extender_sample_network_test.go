package work

// What one network may hold of the samples the operator draws for clients
// (model.ExtenderSampleSettings): the geo dns sets, a country's sets and the
// seed cohort. On 2026-11-16 two networks of one fork build held 86% of the
// active extenders, so every sample was mostly theirs, and one operator could
// see, or drop, most of what a client first reached. Pure sampler runs under
// the fixed secret and epoch, on documentation addresses; no database.

import (
	"slices"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The rows of one network's extenders, one address each, at the indexes given.
func testSampleNetworkDnsAddresses(networkId server.Id, countryCode string, ipVersion int, indexes ...int) []*model.NetworkExtenderDnsAddress {
	addresses := testExtenderDnsAddresses(countryCode, ipVersion, indexes...)
	for _, address := range addresses {
		address.NetworkId = networkId
	}
	return addresses
}

// A fleet of fleetCount extenders and otherCount networks of one extender
// each, every one in countryCode on ipVersion: the fleet's network, the
// others', and every row.
func testSampleNetworkFleet(countryCode string, ipVersion int, fleetCount int, otherCount int) (server.Id, []server.Id, []*model.NetworkExtenderDnsAddress) {
	fleetNetworkId := server.NewId()
	fleetIndexes := []int{}
	for i := range fleetCount {
		fleetIndexes = append(fleetIndexes, 1+i)
	}
	addresses := testSampleNetworkDnsAddresses(fleetNetworkId, countryCode, ipVersion, fleetIndexes...)
	otherNetworkIds := []server.Id{}
	for i := range otherCount {
		otherNetworkId := server.NewId()
		otherNetworkIds = append(otherNetworkIds, otherNetworkId)
		addresses = append(addresses, testSampleNetworkDnsAddresses(otherNetworkId, countryCode, ipVersion, 1+fleetCount+i)...)
	}
	return fleetNetworkId, otherNetworkIds, addresses
}

// How many addresses of each network one set answers.
func testSampleSetNetworkCounts(desiredSet *extenderDnsRecordSet, addresses []*model.NetworkExtenderDnsAddress) map[server.Id]int {
	ipNetworkIds := map[string]server.Id{}
	for _, address := range addresses {
		ipNetworkIds[address.Ip.String()] = address.NetworkId
	}
	counts := map[server.Id]int{}
	for _, ip := range desiredSet.ips {
		counts[ipNetworkIds[ip]] += 1
	}
	return counts
}

// The address sets of one family, by set identifier.
func testSampleAddressSets(desiredSets []*extenderDnsRecordSet, ipVersion int) map[string]*extenderDnsRecordSet {
	sets := map[string]*extenderDnsRecordSet{}
	for _, desiredSet := range desiredSets {
		if desiredSet.ipVersion == ipVersion {
			sets[desiredSet.setIdentifier()] = desiredSet
		}
	}
	return sets
}

// A fleet with most of the directory holds its quota of every dns set, two of
// eight, whenever the other networks can fill the rest. Before, each set was
// the head of the epoch's keyed order of its partition and the ring around it,
// whatever networks held it, so a fleet of 24 beside 8 others held about six
// of each set's eight.
func TestExtenderDnsSetHoldsEachNetworkToItsQuota(t *testing.T) {
	const sampleCount = 8
	quota := model.DefaultExtenderSampleSettings().NetworkQuota(sampleCount)
	fleetNetworkId, otherNetworkIds, addresses := testSampleNetworkFleet("DE", 4, 24, 8)

	sets := testSampleAddressSets(sampleTestExtenderDnsRecordSets(addresses, sampleCount), 4)
	if len(sets) != 2 {
		t.Fatalf("%d A sets, want the continent's and the default", len(sets))
	}
	for setIdentifier, desiredSet := range sets {
		counts := testSampleSetNetworkCounts(desiredSet, addresses)
		if len(desiredSet.ips) != sampleCount || counts[fleetNetworkId] != quota {
			t.Errorf("%s answers %d addresses, %d the fleet's; want %d with the fleet at its quota of %d", setIdentifier, len(desiredSet.ips), counts[fleetNetworkId], sampleCount, quota)
		}
		for _, otherNetworkId := range otherNetworkIds {
			if 1 < counts[otherNetworkId] {
				t.Errorf("%s answers %d addresses of a network of one extender", setIdentifier, counts[otherNetworkId])
			}
		}
	}
}

// Where too few other networks have candidates to fill a set under the quota,
// the fleet fills the rest: the set is as full as it was without the quota,
// and the other network's address is in it.
func TestExtenderDnsSetFillsFromAFleetWhenTooFewOtherNetworksExist(t *testing.T) {
	const sampleCount = 8
	fleetNetworkId, otherNetworkIds, addresses := testSampleNetworkFleet("DE", 4, 24, 1)

	for setIdentifier, desiredSet := range testSampleAddressSets(sampleTestExtenderDnsRecordSets(addresses, sampleCount), 4) {
		counts := testSampleSetNetworkCounts(desiredSet, addresses)
		if len(desiredSet.ips) != sampleCount || counts[otherNetworkIds[0]] != 1 || counts[fleetNetworkId] != sampleCount-1 {
			t.Errorf("%s answers %d addresses, the other network's %d and the fleet's %d; want 8 with the other's one and the fleet's 7", setIdentifier, len(desiredSet.ips), counts[otherNetworkIds[0]], counts[fleetNetworkId])
		}
	}
	// a pool of one network is answered as it always was
	single := testSampleNetworkDnsAddresses(fleetNetworkId, "DE", 4, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12)
	for setIdentifier, desiredSet := range testSampleAddressSets(sampleTestExtenderDnsRecordSets(single, sampleCount), 4) {
		if len(desiredSet.ips) != sampleCount {
			t.Errorf("a pool of one network: %s answers %d addresses, want %d", setIdentifier, len(desiredSet.ips), sampleCount)
		}
	}
}

// The quota keeps a set a function of the secret, the epoch and the pool: two
// ticks of one epoch answer the same set, and with no network holding more than
// its quota of the order, the set is the head of the order as before.
func TestExtenderDnsNetworkQuotaKeepsTheSetDeterministic(t *testing.T) {
	const sampleCount = 8
	_, _, addresses := testSampleNetworkFleet("DE", 4, 24, 8)
	first := testSampleAddressSets(sampleTestExtenderDnsRecordSets(addresses, sampleCount), 4)
	again := testSampleAddressSets(sampleTestExtenderDnsRecordSets(slices.Clone(addresses), sampleCount), 4)
	for setIdentifier, desiredSet := range first {
		if !slices.Equal(desiredSet.ips, again[setIdentifier].ips) {
			t.Errorf("%s answered %v and then %v in one epoch", setIdentifier, desiredSet.ips, again[setIdentifier].ips)
		}
	}

	// every extender its own network: the quota never binds, and the set is
	// what the order alone gives
	spread := []*model.NetworkExtenderDnsAddress{}
	for i := range 32 {
		spread = append(spread, testSampleNetworkDnsAddresses(server.NewId(), "DE", 4, 1+i)...)
	}
	restore := testSampleSettings(t, 0)
	uncapped := testSampleAddressSets(sampleTestExtenderDnsRecordSets(spread, sampleCount), 4)
	restore()
	for setIdentifier, desiredSet := range testSampleAddressSets(sampleTestExtenderDnsRecordSets(spread, sampleCount), 4) {
		if !slices.Equal(desiredSet.ips, uncapped[setIdentifier].ips) {
			t.Errorf("%s answered %v under a quota no network reaches, want the order's %v", setIdentifier, desiredSet.ips, uncapped[setIdentifier].ips)
		}
	}
}

// Replaces the sample settings with a share of maxNetworkShare until the
// returned restore, which the test also runs at its end.
func testSampleSettings(t *testing.T, maxNetworkShare float64) (restore func()) {
	previous := newExtenderSampleSettings
	newExtenderSampleSettings = func() *model.ExtenderSampleSettings {
		return &model.ExtenderSampleSettings{MaxNetworkShare: maxNetworkShare}
	}
	restored := false
	restore = func() {
		if !restored {
			newExtenderSampleSettings = previous
			restored = true
		}
	}
	t.Cleanup(restore)
	return restore
}

// China's set of four records holds a fleet to one of them, its quota of
// four, whenever other networks' records can fill the rest, and its address
// sets answer exactly the extenders its records vouch for.
func TestExtenderDnsCountrySetHoldsEachNetworkToItsQuota(t *testing.T) {
	const sampleCount = 8
	fleetNetworkId, _, addresses := testSampleNetworkFleet("CN", 4, 16, 6)
	extenderNetworkIds := map[server.Id]server.Id{}
	for _, address := range addresses {
		extenderNetworkIds[address.ExtenderId] = address.NetworkId
	}
	recordExtenderIds := map[string]server.Id{}
	desiredSets := sampleExtenderDnsRecordSets(
		addresses,
		sampleCount,
		testExtenderDnsSecret,
		testExtenderDnsEpoch,
		func(extenderId server.Id) (string, bool) {
			record := "record-" + extenderId.String()
			recordExtenderIds[record] = extenderId
			return record, true
		},
		extenderDnsCountrySample{
			extenderDnsCountrySet: extenderDnsCnSet,
			recordCount:           4,
			recordName:            "extender.example",
		},
	)
	quota := model.DefaultExtenderSampleSettings().NetworkQuota(4)
	found := false
	for _, desiredSet := range desiredSets {
		if desiredSet.countryCode != "CN" || desiredSet.ipVersion != 0 {
			continue
		}
		found = true
		fleetRecordCount := 0
		for _, record := range desiredSet.records {
			if extenderNetworkIds[recordExtenderIds[record]] == fleetNetworkId {
				fleetRecordCount += 1
			}
		}
		if len(desiredSet.records) != 4 || fleetRecordCount != quota {
			t.Fatalf("China's set holds %d records, %d the fleet's; want 4 with the fleet at its quota of %d", len(desiredSet.records), fleetRecordCount, quota)
		}
	}
	if !found {
		t.Fatal("China has no TXT set")
	}
}

// The seed cohort holds a fleet to its quota, two of eight, whenever other
// networks can fill it, previous members of the fleet past its quota included;
// and where too few other networks have candidates, the fleet fills it.
func TestExtenderSeedCohortHoldsEachNetworkToItsQuota(t *testing.T) {
	quota := model.DefaultExtenderSampleSettings().NetworkQuota(ExtenderSeedMaxRecordCount)
	fleetNetworkId, _, addresses := testSampleNetworkFleet("DE", 4, 20, 6)
	extenderNetworkIds := map[server.Id]server.Id{}
	keyHexes := []string{}
	for _, address := range addresses {
		extenderNetworkIds[address.ExtenderId] = address.NetworkId
		if address.NetworkId == fleetNetworkId {
			keyHexes = append(keyHexes, extenderDnsAddressKeyHex(address))
		}
	}
	fleetCount := func(cohort []server.Id) int {
		count := 0
		for _, extenderId := range cohort {
			if extenderNetworkIds[extenderId] == fleetNetworkId {
				count += 1
			}
		}
		return count
	}

	cohort := selectExtenderSeedCohort(addresses, testExtenderDnsSecret, testExtenderDnsEpoch, nil, ExtenderSeedMaxRecordCount)
	if len(cohort) != ExtenderSeedMaxRecordCount || fleetCount(cohort) != quota {
		t.Fatalf("a cohort of %d holds %d of the fleet's, want %d with the fleet at its quota of %d", len(cohort), fleetCount(cohort), ExtenderSeedMaxRecordCount, quota)
	}
	// a stored cohort of six of the fleet's keeps only its quota of them
	kept := selectExtenderSeedCohort(addresses, testExtenderDnsSecret, testExtenderDnsEpoch, keyHexes[:6], ExtenderSeedMaxRecordCount)
	if len(kept) != ExtenderSeedMaxRecordCount || fleetCount(kept) != quota {
		t.Fatalf("a cohort drawn after six of the fleet's holds %d of %d, want %d", fleetCount(kept), len(kept), quota)
	}

	_, otherNetworkIds, fewAddresses := testSampleNetworkFleet("DE", 4, 20, 1)
	fewNetworkIds := map[server.Id]server.Id{}
	for _, address := range fewAddresses {
		fewNetworkIds[address.ExtenderId] = address.NetworkId
	}
	filled := selectExtenderSeedCohort(fewAddresses, testExtenderDnsSecret, testExtenderDnsEpoch, nil, ExtenderSeedMaxRecordCount)
	otherCount := 0
	for _, extenderId := range filled {
		if fewNetworkIds[extenderId] == otherNetworkIds[0] {
			otherCount += 1
		}
	}
	if len(filled) != ExtenderSeedMaxRecordCount || otherCount != 1 {
		t.Fatalf("with one other network the cohort holds %d, %d the other's; want 8 with the other's one", len(filled), otherCount)
	}
}
