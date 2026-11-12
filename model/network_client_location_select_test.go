package model

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func selectTestString(value string) *string {
	return &value
}

func selectTestInt(value int) *int {
	return &value
}

// Absent members take their defaults (FP2-PROTOCOL.md §5.1), and the echo
// carries all six (§5.2).
func TestFindProviders2SelectionDefaultsAndEcho(t *testing.T) {
	for _, c := range []struct {
		name  string
		count int
		sel   FindProviders2Select
		want  findProviders2Selection
	}{
		{
			name:  "empty",
			count: 6,
			want:  findProviders2Selection{ip: "dualstack", protocol: "dualproto", targetCount: 6, dualstackTargetCount: 1},
		},
		{
			name:  "count below one",
			count: 0,
			want:  findProviders2Selection{ip: "dualstack", protocol: "dualproto", targetCount: 1, dualstackTargetCount: 1},
		},
		{
			name:  "limited window",
			count: 3,
			sel:   FindProviders2Select{Ip: selectTestString("ipv4"), Protocol: selectTestString("tcp"), TargetCount: selectTestInt(3), HeldCount: selectTestInt(1), DualstackTargetCount: selectTestInt(0), HeldDualstackCount: selectTestInt(0)},
			want:  findProviders2Selection{ip: "ipv4", protocol: "tcp", targetCount: 3, heldCount: 1},
		},
		{
			// an ipv4 request has no IPv6 minimum, whatever it sends (200)
			name:  "ipv4 with an IPv6 minimum",
			count: 2,
			sel:   FindProviders2Select{Ip: selectTestString("ipv4"), DualstackTargetCount: selectTestInt(2)},
			want:  findProviders2Selection{ip: "ipv4", protocol: "dualproto", targetCount: 2},
		},
		{
			name:  "quality window",
			count: 20,
			sel:   FindProviders2Select{TargetCount: selectTestInt(6), HeldCount: selectTestInt(4), DualstackTargetCount: selectTestInt(1), HeldDualstackCount: selectTestInt(1)},
			want:  findProviders2Selection{ip: "dualstack", protocol: "dualproto", targetCount: 6, heldCount: 4, dualstackTargetCount: 1, heldDualstackCount: 1},
		},
	} {
		sel := c.sel
		selection, err := newFindProviders2Selection(&FindProviders2Args{Count: c.count, Select: &sel})
		if err != nil {
			t.Fatalf("%s: %s", c.name, err)
		}
		if *selection != c.want {
			t.Fatalf("%s: got %+v, want %+v", c.name, *selection, c.want)
		}
		echoBytes, err := json.Marshal(selection.echo())
		connect.AssertEqual(t, err, nil)
		want := fmt.Sprintf(`{"ip":%q,"protocol":%q,"target_count":%d,"held_count":%d,"dualstack_target_count":%d,"held_dualstack_count":%d}`,
			c.want.ip, c.want.protocol, c.want.targetCount, c.want.heldCount, c.want.dualstackTargetCount, c.want.heldDualstackCount)
		connect.AssertEqual(t, string(echoBytes), want)
	}
}

// Each refusal of §5.3 is a 400 in the "<code> message" convention, and is
// refused before anything is read.
func TestFindProviders2SelectionRefusals(t *testing.T) {
	for _, c := range []struct {
		sel  FindProviders2Select
		want string
	}{
		{sel: FindProviders2Select{Ip: selectTestString("v6")}, want: `400 unknown select.ip "v6"`},
		{sel: FindProviders2Select{Ip: selectTestString("")}, want: `400 unknown select.ip ""`},
		{sel: FindProviders2Select{Protocol: selectTestString("udp")}, want: `400 unknown select.protocol "udp"`},
		{sel: FindProviders2Select{TargetCount: selectTestInt(0)}, want: "400 select.target_count must be at least 1"},
		{sel: FindProviders2Select{HeldCount: selectTestInt(-1)}, want: "400 select.held_count must not be negative"},
		{sel: FindProviders2Select{DualstackTargetCount: selectTestInt(-1)}, want: "400 select.dualstack_target_count must not be negative"},
		{sel: FindProviders2Select{HeldDualstackCount: selectTestInt(-1)}, want: "400 select.held_dualstack_count must not be negative"},
	} {
		sel := c.sel
		_, err := newFindProviders2Selection(&FindProviders2Args{Count: 5, Select: &sel})
		if err == nil || err.Error() != c.want {
			t.Errorf("got %v, want %s", err, c.want)
		}
	}
	// a member of the wrong JSON type never reaches the selection: the body
	// does not decode
	var args FindProviders2Args
	if err := json.Unmarshal([]byte(`{"select":{"ip":4}}`), &args); err == nil {
		t.Fatal("a select.ip number decoded")
	}
	// null select is legacy mode
	connect.AssertEqual(t, json.Unmarshal([]byte(`{"count":2,"select":null}`), &args), nil)
	if args.Select != nil {
		t.Fatal("null select decoded as selector mode")
	}
}

// ip_family as a filter on IP labels (§6.3).
func TestProviderCapabilityClassesForIpFamily(t *testing.T) {
	all := newProviderCapabilityClassSet(providerCapabilityClasses...)
	for _, c := range []struct {
		ipFamily string
		want     providerCapabilityClassSet
	}{
		{ipFamily: "", want: all},
		{ipFamily: "v4-capable", want: all},
		{ipFamily: "v6-capable", want: newProviderCapabilityClassSet(providerCapabilityClassA, providerCapabilityClassC)},
		{ipFamily: "dualstack", want: newProviderCapabilityClassSet(providerCapabilityClassA, providerCapabilityClassC)},
		{ipFamily: "v4-only", want: newProviderCapabilityClassSet(providerCapabilityClassB, providerCapabilityClassD)},
		{ipFamily: "v6-only", want: 0},
	} {
		classes, err := providerCapabilityClassesForIpFamily(c.ipFamily)
		connect.AssertEqual(t, err, nil)
		if classes != c.want {
			t.Errorf("%q: got %v, want %v", c.ipFamily, classes.classes(), c.want.classes())
		}
	}
	if _, err := providerCapabilityClassesForIpFamily("v4"); err == nil || !strings.HasPrefix(err.Error(), "400 ") {
		t.Fatalf("unknown ip_family: %v", err)
	}
}

// The classes of each phase per request (§6.3) and which providers are
// overflow (the ceiling: a tcp request never gets a dualproto class).
func TestFindProviders2SelectionPhases(t *testing.T) {
	selection := func(ip string, protocol string, dualstackTargetCount int, heldDualstackCount int) *findProviders2Selection {
		return &findProviders2Selection{ip: ip, protocol: protocol, targetCount: 6, dualstackTargetCount: dualstackTargetCount, heldDualstackCount: heldDualstackCount}
	}
	ab := newProviderCapabilityClassSet(providerCapabilityClassA, providerCapabilityClassB)
	cd := newProviderCapabilityClassSet(providerCapabilityClassC, providerCapabilityClassD)

	normal := selection("dualstack", "dualproto", 1, 0)
	connect.AssertEqual(t, normal.ipv6Need(), 1)
	connect.AssertEqual(t, normal.needClasses(), []providerCapabilityClass{providerCapabilityClassA, providerCapabilityClassC})
	connect.AssertEqual(t, normal.mainClasses(), ab)
	connect.AssertEqual(t, normal.overflowClasses(), cd)
	connect.AssertEqual(t, normal.overflow(providerCapabilityClassA), false)
	connect.AssertEqual(t, normal.overflow(providerCapabilityClassC), true)
	connect.AssertEqual(t, normal.overflow(providerCapabilityClassD), true)
	// a held dualstack client meets the IPv6 minimum
	connect.AssertEqual(t, selection("dualstack", "dualproto", 1, 1).ipv6Need(), 0)

	ipv4 := selection("ipv4", "dualproto", 0, 0)
	connect.AssertEqual(t, ipv4.ipv6Need(), 0)
	connect.AssertEqual(t, ipv4.mainClasses(), ab)
	connect.AssertEqual(t, ipv4.overflowClasses(), cd)

	dualstackTcp := selection("dualstack", "tcp", 1, 0)
	connect.AssertEqual(t, dualstackTcp.needClasses(), []providerCapabilityClass{providerCapabilityClassC})
	connect.AssertEqual(t, dualstackTcp.mainClasses(), cd)
	connect.AssertEqual(t, dualstackTcp.overflowClasses(), providerCapabilityClassSet(0))
	connect.AssertEqual(t, dualstackTcp.overflow(providerCapabilityClassC), false)

	limited := selection("ipv4", "tcp", 0, 0)
	connect.AssertEqual(t, limited.ipv6Need(), 0)
	connect.AssertEqual(t, limited.mainClasses(), cd)
	connect.AssertEqual(t, limited.overflowClasses(), providerCapabilityClassSet(0))
}

// The facets a pool reads and the records each keeps (§6.4).
func TestFindProviders2PoolFacets(t *testing.T) {
	dualstack := ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
	records := map[string]*ClientScore{
		"A":                 {IpFamilies: dualstack},
		"B":                 {IpFamilies: ClientScoreIpFamilyV4},
		"B from dual-stack": {IpFamilies: dualstack, CapabilityDowngrades: ClientScoreCapabilityDowngradeIpv6},
		"C":                 {IpFamilies: dualstack, CapabilityDowngrades: ClientScoreCapabilityDowngradeUdp},
		"D":                 {IpFamilies: ClientScoreIpFamilyV4, CapabilityDowngrades: ClientScoreCapabilityDowngradeUdp},
		"D from dual-stack": {IpFamilies: dualstack, CapabilityDowngrades: ClientScoreCapabilityDowngradeIpv6 | ClientScoreCapabilityDowngradeUdp},
		"v6-only":           {IpFamilies: ClientScoreIpFamilyV6},
	}
	kept := func(poolFacets findProviders2PoolFacets, facet ipFamilyFacet) []string {
		names := []string{}
		filter := poolFacets.filters[facet]
		for name, record := range records {
			if filter != nil && filter(record) {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		return names
	}

	ab := newFindProviders2PoolFacets(newProviderCapabilityClassSet(providerCapabilityClassA, providerCapabilityClassB), false)
	connect.AssertEqual(t, ab.unionFacets(), []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only, clientScoreCapabilityFacetIpv4Dualproto})
	// a downgraded record in today's facets is skipped: its copy is read
	connect.AssertEqual(t, kept(ab, ipFamilyFacetDualstack), []string{"A", "B"})
	connect.AssertEqual(t, kept(ab, ipFamilyFacetV4Only), []string{"A", "B"})
	connect.AssertEqual(t, kept(ab, clientScoreCapabilityFacetIpv4Dualproto), []string{"B from dual-stack"})
	connect.AssertEqual(t, ab.readOptions().noUnfacetedFallback, true)

	cd := newFindProviders2PoolFacets(newProviderCapabilityClassSet(providerCapabilityClassC, providerCapabilityClassD), false)
	connect.AssertEqual(t, cd.unionFacets(), []ipFamilyFacet{clientScoreCapabilityFacetDualstackTcp, clientScoreCapabilityFacetIpv4Tcp})
	connect.AssertEqual(t, len(cd.familyFacets), 0)
	connect.AssertEqual(t, kept(cd, clientScoreCapabilityFacetDualstackTcp), []string{"C"})
	connect.AssertEqual(t, kept(cd, clientScoreCapabilityFacetIpv4Tcp), []string{"D", "D from dual-stack"})

	// forced: today's forced records by the class their downgrades give
	forcedCd := newFindProviders2PoolFacets(newProviderCapabilityClassSet(providerCapabilityClassC, providerCapabilityClassD), true)
	connect.AssertEqual(t, forcedCd.unionFacets(), []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only})
	connect.AssertEqual(t, kept(forcedCd, ipFamilyFacetDualstack), []string{"C", "D", "D from dual-stack"})
	forcedA := newFindProviders2PoolFacets(newProviderCapabilityClassSet(providerCapabilityClassA), true)
	connect.AssertEqual(t, forcedA.unionFacets(), []ipFamilyFacet{ipFamilyFacetDualstack})
	connect.AssertEqual(t, kept(forcedA, ipFamilyFacetDualstack), []string{"A"})
}

// The export writes a downgraded provider's classified copy to the
// capability facet of its class, union and native, in a non-forced mode
// only, every capability facet present, an empty one included; today's
// facets and the filter keep the strict records (§4.4).
func TestExportClientScoresWritesCapabilityFacets(t *testing.T) {
	dualstack := ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
	native := func(clientId server.Id, ipFamilies uint8, downgrades uint8, passes bool, online bool) *ClientScore {
		score := clientScoreForTest(clientId, ipFamilies)
		score.CapabilityDowngrades = downgrades
		score.PassesMinimums = map[string]bool{RankModeQuality: passes}
		score.Online = online
		return score
	}
	aId, cId, dId := server.NewId(), server.NewId(), server.NewId()
	a := native(aId, dualstack, 0, true, true)
	// strict: online only; classified copy: a quality native of class C
	c := native(cId, dualstack, ClientScoreCapabilityDowngradeUdp, false, true)
	c.capabilityCopy = native(cId, dualstack, ClientScoreCapabilityDowngradeUdp, true, true)
	// strict: in no bucket; classified copy: online only, class D
	d := native(dId, ClientScoreIpFamilyV4, ClientScoreCapabilityDowngradeUdp, false, false)
	d.capabilityCopy = native(dId, ClientScoreIpFamilyV4, ClientScoreCapabilityDowngradeUdp, false, true)
	scores := map[server.Id]*ClientScore{aId: a, cId: c, dId: d}

	decode := func(payload clientScoreFacetPayload) []server.Id {
		clientIds := []server.Id{}
		for i := range payload.counts {
			sample, err := decodeClientScoreSample(payload.encodeSample(i))
			connect.AssertEqual(t, err, nil)
			for _, score := range sample {
				clientIds = append(clientIds, score.ClientId)
			}
		}
		return clientIds
	}

	payload := exportClientScores(false, RankModeQuality, scores)
	connect.AssertEqual(t, len(payload.capabilityFacets), len(clientScoreCapabilityFacets))
	connect.AssertEqual(t, len(payload.nativeCapabilityFacets), len(clientScoreCapabilityFacets))
	connect.AssertEqual(t, decode(payload.capabilityFacets[clientScoreCapabilityFacetDualstackTcp]), []server.Id{cId})
	connect.AssertEqual(t, decode(payload.nativeCapabilityFacets[clientScoreCapabilityFacetDualstackTcp]), []server.Id{cId})
	connect.AssertEqual(t, decode(payload.capabilityFacets[clientScoreCapabilityFacetIpv4Tcp]), []server.Id{dId})
	connect.AssertEqual(t, len(decode(payload.nativeCapabilityFacets[clientScoreCapabilityFacetIpv4Tcp])), 0)
	// the empty facet is written, its counts an encoded empty list
	connect.AssertEqual(t, len(payload.capabilityFacets[clientScoreCapabilityFacetIpv4Dualproto].counts), 0)
	connect.AssertEqual(t, 0 < len(payload.capabilityFacets[clientScoreCapabilityFacetIpv4Dualproto].countsBytes), true)
	// today's dualstack facet holds the strict records: a (native) and c
	// (online), never d, which the strict decision left out
	todayIds := decode(payload.facets[ipFamilyFacetDualstack])
	slices.SortFunc(todayIds, func(x server.Id, y server.Id) int { return x.Cmp(y) })
	wantIds := []server.Id{aId, cId}
	slices.SortFunc(wantIds, func(x server.Id, y server.Id) int { return x.Cmp(y) })
	connect.AssertEqual(t, todayIds, wantIds)
	connect.AssertEqual(t, decode(payload.nativeFacets[ipFamilyFacetDualstack]), []server.Id{aId})

	forced := exportClientScores(true, RankModeQuality, scores)
	if forced.capabilityFacets != nil || forced.nativeCapabilityFacets != nil {
		t.Fatal("a forced mode wrote capability facets")
	}
}

// The fanout writes every capability facet's counts and samples beside
// today's in a non-forced mode, and none in a forced one.
func TestEmitClientScoreTargetFanoutCapabilityFacets(t *testing.T) {
	keys := clientScoreTargetKeys{
		counts: func(callerId server.Id) string { return "c" },
		filter: func(callerId server.Id) string { return "f" },
		sample: func(callerId server.Id, index int) string { return fmt.Sprintf("s_%d", index) },
		alias:  func(callerId server.Id) string { return "a" },
		facetCounts: func(callerId server.Id, facet ipFamilyFacet) string {
			return fmt.Sprintf("c_%s", facet)
		},
		facetSample: func(callerId server.Id, facet ipFamilyFacet, index int) string {
			return fmt.Sprintf("s_%s_%d", facet, index)
		},
	}
	encodeSample := func(int) []byte { return []byte("sample") }
	facet := func(pages int) clientScoreFacetPayload {
		return clientScoreFacetPayload{countsBytes: []byte("counts"), counts: make([]int, pages), encodeSample: encodeSample}
	}
	emitted := func(withCapability bool) []string {
		emittedKeys := []string{}
		err := emitClientScoreTargetFanout(
			[]server.Id{{}},
			map[server.Id]*ClientScore{},
			map[server.Id]map[server.Id]bool{},
			keys,
			func(map[server.Id]*ClientScore) clientScoreExportPayload {
				payload := clientScoreExportPayload{
					filterBytes: []byte("filter"),
					facets: map[ipFamilyFacet]clientScoreFacetPayload{
						ipFamilyFacetDualstack: facet(1),
						ipFamilyFacetV4Only:    facet(0),
						ipFamilyFacetV6Only:    facet(0),
					},
				}
				if withCapability {
					payload.capabilityFacets = map[ipFamilyFacet]clientScoreFacetPayload{
						clientScoreCapabilityFacetIpv4Dualproto: facet(0),
						clientScoreCapabilityFacetDualstackTcp:  facet(1),
						clientScoreCapabilityFacetIpv4Tcp:       facet(2),
					}
				}
				return payload
			},
			false,
			false,
			func(set clientScoreRedisSet) error {
				emittedKeys = append(emittedKeys, set.key)
				return nil
			},
		)
		connect.AssertEqual(t, err, nil)
		slices.Sort(emittedKeys)
		return emittedKeys
	}
	connect.AssertEqual(t, emitted(false), []string{"c_4", "c_6", "c_d", "f", "s_d_0"})
	connect.AssertEqual(t, emitted(true), []string{"c_4", "c_6", "c_d", "c_i", "c_t", "c_x", "f", "s_d_0", "s_t_0", "s_x_0", "s_x_1"})
}

// A legacy answer carries none of the new fields: a provider without labels
// or overflow and a result without the echo encode exactly as before the
// fields existed (FP2-PROTOCOL.md §5.4, the old-client cell).
func TestFindProviders2LegacyAnswerEncodesWithoutNewFields(t *testing.T) {
	clientId := server.NewId()
	result := &FindProviders2Result{
		Providers: []*FindProvidersProvider{{
			ClientId:                   clientId,
			EstimatedBytesPerSecond:    1024,
			HasEstimatedBytesPerSecond: true,
			Tier:                       2,
			IpFamily:                   "v4-only",
			Match:                      LocationTypeRegion,
		}},
	}
	resultBytes, err := json.Marshal(result)
	connect.AssertEqual(t, err, nil)
	want := fmt.Sprintf(`{"providers":[{"client_id":"%s","estimated_bytes_per_second":1024,"has_estimated_bytes_per_second":true,"tier":2,"ip_family":"v4-only","match":"region"}]}`, clientId)
	connect.AssertEqual(t, string(resultBytes), want)

	result.Providers[0].Capabilities = &FindProvidersCapabilities{Ip: ProviderCapabilityIpIpv4, Protocol: ProviderCapabilityProtocolTcp}
	result.Providers[0].OverflowStep = 1
	result.Select = (&findProviders2Selection{ip: "dualstack", protocol: "dualproto", targetCount: 6, dualstackTargetCount: 1}).echo()
	resultBytes, err = json.Marshal(result)
	connect.AssertEqual(t, err, nil)
	want = fmt.Sprintf(`{"providers":[{"client_id":"%s","estimated_bytes_per_second":1024,"has_estimated_bytes_per_second":true,"tier":2,"ip_family":"v4-only","match":"region","capabilities":{"ip":"ipv4","protocol":"tcp"},"overflow_step":1}],"select":{"ip":"dualstack","protocol":"dualproto","target_count":6,"held_count":0,"dualstack_target_count":1,"held_dualstack_count":0}}`, clientId)
	connect.AssertEqual(t, string(resultBytes), want)
}

// Today's facets hold the same records with and without the capability
// copies: a copy reaches only the capability facets, so a legacy reader,
// which never reads CapabilityDowngrades, decodes exactly the strict records
// it read before (§9, "today's d, 4 and 6 keys"). Gob encodes the type of
// every exported field, so the bytes of a page differ from an older
// exporter's even where every new field is zero; what is pinned is the
// content.
func TestExportClientScoresTodaysFacetsIgnoreCapabilityCopies(t *testing.T) {
	dualstack := ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
	newScores := func(withCopies bool) map[server.Id]*ClientScore {
		scores := map[server.Id]*ClientScore{}
		for i := range 30 {
			clientId, err := server.ParseId(fmt.Sprintf("00000000-0000-0000-0000-%012d", i))
			connect.AssertEqual(t, err, nil)
			score := clientScoreForTest(clientId, []uint8{dualstack, ClientScoreIpFamilyV4, ClientScoreIpFamilyV6}[i%3])
			score.Online = i%2 == 0
			score.PassesMinimums = map[string]bool{RankModeQuality: i%5 == 0}
			if i%3 != 2 && i%4 == 1 {
				score.CapabilityDowngrades = ClientScoreCapabilityDowngradeUdp
				if withCopies {
					classified := *score
					classified.PassesMinimums = map[string]bool{RankModeQuality: true}
					classified.Online = true
					score.capabilityCopy = &classified
				}
			}
			scores[clientId] = score
		}
		return scores
	}
	facetRecords := func(payload map[ipFamilyFacet]clientScoreFacetPayload, facet ipFamilyFacet) map[server.Id]string {
		records := map[server.Id]string{}
		for i := range payload[facet].counts {
			sample, err := decodeClientScoreSample(payload[facet].encodeSample(i))
			connect.AssertEqual(t, err, nil)
			for _, score := range sample {
				recordBytes, err := json.Marshal(score)
				connect.AssertEqual(t, err, nil)
				records[score.ClientId] = string(recordBytes)
			}
		}
		return records
	}
	for _, forceMinimum := range []bool{false, true} {
		with := exportClientScores(forceMinimum, RankModeQuality, newScores(true))
		without := exportClientScores(forceMinimum, RankModeQuality, newScores(false))
		for _, facet := range ipFamilyFacets {
			connect.AssertEqual(t, facetRecords(with.facets, facet), facetRecords(without.facets, facet))
			if !forceMinimum {
				connect.AssertEqual(t, facetRecords(with.nativeFacets, facet), facetRecords(without.nativeFacets, facet))
			}
		}
		connect.AssertEqual(t, with.filterBytes, without.filterBytes)
	}
}
