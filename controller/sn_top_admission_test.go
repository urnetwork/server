// Rule (a) of the SN Top board at the fold (sn_top_admission.go, owner
// decisions 2026-11-20): the fold withholds the unit of every provider the
// export did not admit over its rolling window, counts Online membership
// instead where the probe catalog turns UDP probes off, forgives UDP where it
// fails at the network level (and only there), keeps the marks first, records
// each network's reasons, skips the tempo without a fresh set once the rule
// is live, runs a shadow board before its first epoch and opens the live
// board where the shadow stood. Before the rule every provider with a prefix
// counted. Pure: the fold's inputs are built here.
package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

func snTopAdmissionTestId(n int) server.Id {
	var id server.Id
	id[0] = 0x5a
	id[14] = byte(n >> 8)
	id[15] = byte(n)
	return id
}

func snTopAdmissionTestPrefix(n int) [32]byte {
	var h [32]byte
	h[0] = byte(n >> 8)
	h[1] = byte(n)
	h[31] = 1
	return h
}

// the probe catalog's country settings: UDP probes off in one country
func snTopAdmissionTestCountrySettings() map[string]egresshealth.CountrySettings {
	off := false
	return map[string]egresshealth.CountrySettings{"ru": {UdpProbes: &off}}
}

var snTopAdmissionTestNow = time.Date(2026, 11, 20, 22, 40, 0, 0, time.UTC)

func snTopAdmissionTestRule(t *testing.T, set map[server.Id]*model.SnTopAdmissionProvider) *snTopUnitRule {
	t.Helper()
	rule, unavailable := newSnTopUnitRule(&model.SnTopAdmission{MeasuredAt: snTopAdmissionTestNow.Add(-time.Minute), Providers: set}, snTopAdmissionTestCountrySettings(), snTopAdmissionTestNow)
	connect.AssertEqual(t, unavailable, "")
	return rule
}

func snTopAdmissionTestProvider(clientId server.Id, networkId server.Id, prefix [32]byte) model.SnTopProvider {
	return model.SnTopProvider{ClientId: clientId, NetworkId: networkId, Eligible: true, PrefixHash: prefix, HasPrefix: true, Presence: 1}
}

// One network per case, each with one provider and its own prefix:
//
//	1 admitted                                   counts
//	2 not admitted (URL)                         withheld: url
//	3 online only, udp-off country               counts (the carve-out)
//	4 online only, udp-on country                withheld: url
//	5 not in the export's set                    withheld: unprobed
//	6 admitted, but marked (a declared proxy
//	  egress or an exit that differs)            withheld: exit_differs
//	7 admitted now, but its window has not
//	  restored it yet                            withheld: window
func TestSnTopFoldCountsOnlyAdmittedProviders(t *testing.T) {
	providers := []model.SnTopProvider{}
	set := map[server.Id]*model.SnTopAdmissionProvider{}
	for n := 1; n <= 7; n++ {
		providers = append(providers, snTopAdmissionTestProvider(snTopAdmissionTestId(100+n), snTopAdmissionTestId(n), snTopAdmissionTestPrefix(n)))
	}
	client := func(n int) server.Id { return snTopAdmissionTestId(100 + n) }
	set[client(1)] = model.Testing_SnTopAdmissionProvider(client(1), "us", true, true, true)
	set[client(2)] = model.Testing_SnTopAdmissionProvider(client(2), "us", false, false, false)
	set[client(3)] = model.Testing_SnTopAdmissionProvider(client(3), "ru", false, true, false)
	set[client(4)] = model.Testing_SnTopAdmissionProvider(client(4), "vn", false, true, false)
	set[client(6)] = model.Testing_SnTopAdmissionProvider(client(6), "ca", true, true, true)
	recovering := model.Testing_SnTopAdmissionProvider(client(7), "us", true, true, true)
	recovering.AdmittedTrack = model.SnTopAdmissionTrack{Score: 0.5, Counts: false}
	recovering.OnlineTrack = recovering.AdmittedTrack
	recovering.TcpAdmittedTrack = recovering.AdmittedTrack
	set[client(7)] = recovering
	rule := snTopAdmissionTestRule(t, set)
	rule.marked = map[server.Id]bool{client(6): true}

	ruled := snTopApplyUnitRule(providers, rule)
	connect.AssertEqual(t, ruled.withheld, 5)
	want := map[int]string{2: model.SnTopUnitReasonUrl, 4: model.SnTopUnitReasonUrl, 5: model.SnTopUnitReasonUnprobed, 6: model.SnTopUnitReasonExitDiffers, 7: snTopUnitReasonWindow}
	for n := 1; n <= 7; n++ {
		units := ruled.networks[snTopAdmissionTestId(n)]
		if reason, ok := want[n]; ok {
			connect.AssertEqual(t, units.CountedProviders, 0)
			connect.AssertEqual(t, units.WithheldProviders, []model.SnTopProviderUnit{{ClientId: client(n), Reason: reason}})
			connect.AssertEqual(t, units.WithheldByReason, map[string]int{reason: 1})
		} else {
			connect.AssertEqual(t, units.CountedProviders, 1)
			connect.AssertEqual(t, len(units.WithheldProviders), 0)
		}
		connect.AssertEqual(t, ruled.providers[n-1].UnitWithheld, want[n] != "")
	}
	// the input is not changed: the live board ranks it before the switch
	for _, provider := range providers {
		connect.AssertEqual(t, provider.UnitWithheld, false)
	}

	projection := model.ProjectSnTop(&model.SnTopProjectionInput{TempoIndex: 10, Providers: ruled.providers})
	ranked := map[server.Id]int{}
	for _, row := range projection.Networks {
		if 0 < row.Position {
			ranked[row.NetworkId] = row.Prefixes
		}
	}
	connect.AssertEqual(t, ranked, map[server.Id]int{snTopAdmissionTestId(1): 1, snTopAdmissionTestId(3): 1})
}

// A fixture of autonomous systems: per system, networks whose providers there
// fail UDP while passing over HTTPS ("failing") and networks whose providers
// pass UDP ("passing"), each with one provider, all established.
type snTopUdpExcuseFixture struct {
	providers []model.SnTopProvider
	set       map[server.Id]*model.SnTopAdmissionProvider
	asns      map[server.Id][]uint32
	created   map[server.Id]time.Time
	next      int
}

func newSnTopUdpExcuseFixture() *snTopUdpExcuseFixture {
	return &snTopUdpExcuseFixture{
		set:     map[server.Id]*model.SnTopAdmissionProvider{},
		asns:    map[server.Id][]uint32{},
		created: map[server.Id]time.Time{},
		next:    1,
	}
}

// Adds one provider of the network in the system.
func (self *snTopUdpExcuseFixture) provider(networkId server.Id, asn uint32, failing bool, age time.Duration) server.Id {
	clientId := snTopAdmissionTestId(1000 + self.next)
	self.providers = append(self.providers, snTopAdmissionTestProvider(clientId, networkId, snTopAdmissionTestPrefix(self.next)))
	self.next += 1
	entry := model.Testing_SnTopAdmissionProvider(clientId, "us", !failing, true, true)
	self.set[clientId] = entry
	self.asns[clientId] = append(self.asns[clientId], asn)
	if _, ok := self.created[networkId]; !ok {
		self.created[networkId] = snTopAdmissionTestNow.Add(-age)
	}
	return clientId
}

// Adds count single-provider networks in the system.
func (self *snTopUdpExcuseFixture) networks(first int, count int, asn uint32, failing bool) {
	for i := range count {
		self.provider(snTopAdmissionTestId(first+i), asn, failing, 30*24*time.Hour)
	}
}

func (self *snTopUdpExcuseFixture) excused() (map[uint32]bool, map[server.Id]bool) {
	return snTopUdpExcusedAsns(self.providers, &model.SnTopAdmission{MeasuredAt: snTopAdmissionTestNow, Providers: self.set}, self.asns, self.created, snTopAdmissionTestNow)
}

// The thresholds: five established networks failing UDP there, two thirds of
// those measured there. Four are not enough; five of eight (62%) are not; a
// network younger than two weeks does not count.
func TestSnTopUdpExcuseThresholds(t *testing.T) {
	fixture := newSnTopUdpExcuseFixture()
	// AS 64500: five of seven networks fail UDP: excused
	fixture.networks(1, 5, 64500, true)
	fixture.networks(10, 2, 64500, false)
	// AS 64501: four failing networks only
	fixture.networks(20, 4, 64501, true)
	// AS 64502: five failing of eight measured, under two thirds
	fixture.networks(30, 5, 64502, true)
	fixture.networks(40, 3, 64502, false)
	// AS 64503: four established failing networks and two young ones
	fixture.networks(50, 4, 64503, true)
	fixture.provider(snTopAdmissionTestId(60), 64503, true, 3*24*time.Hour)
	fixture.provider(snTopAdmissionTestId(61), 64503, true, 13*24*time.Hour)

	excused, everywhere := fixture.excused()
	connect.AssertEqual(t, excused, map[uint32]bool{64500: true})
	connect.AssertEqual(t, len(everywhere), 0)

	// in the excused system a failing provider counts on its TCP admission,
	// in the others it is withheld for UDP
	rule := snTopAdmissionTestRule(t, fixture.set)
	rule.asns, rule.excusedAsns, rule.failingEverywhere = fixture.asns, excused, everywhere
	ruled := snTopApplyUnitRule(fixture.providers, rule)
	connect.AssertEqual(t, ruled.networks[snTopAdmissionTestId(1)].CountedProviders, 1)
	connect.AssertEqual(t, ruled.networks[snTopAdmissionTestId(20)].WithheldByReason, map[string]int{model.SnTopUnitReasonUdp: 1})
	connect.AssertEqual(t, ruled.networks[snTopAdmissionTestId(60)].WithheldByReason, map[string]int{model.SnTopUnitReasonUdp: 1})
}

// The fleets of 2026-11-20 make no excuse: one network's thousands of
// identities count once, so a farm alone never reaches five networks; a
// network that fails UDP in every system it is measured in fails because of
// what it runs, so it neither counts toward an excuse nor is excused even
// where four honest networks fail; and a system where other networks pass
// UDP stays unexcused.
func TestSnTopUdpExcuseRefusesFleets(t *testing.T) {
	fixture := newSnTopUdpExcuseFixture()
	farm := snTopAdmissionTestId(900)
	// a farm with 300 identities in each of three systems, all failing UDP
	for _, asn := range []uint32{64510, 64511, 64512} {
		for range 300 {
			fixture.provider(farm, asn, true, 400*24*time.Hour)
		}
	}
	// in 64510 four honest networks fail UDP too: with the farm that would
	// be five
	fixture.networks(1, 4, 64510, true)
	// in 64511 twenty networks pass UDP
	fixture.networks(10, 20, 64511, false)
	// a second farm in one system only: it counts once, not 50 times
	other := snTopAdmissionTestId(901)
	for range 50 {
		fixture.provider(other, 64513, true, 400*24*time.Hour)
	}
	fixture.networks(40, 3, 64513, true)

	excused, everywhere := fixture.excused()
	connect.AssertEqual(t, everywhere[farm], true)
	connect.AssertEqual(t, everywhere[other], false)
	connect.AssertEqual(t, len(excused), 0)

	// even were its system excused, a network failing everywhere is not
	rule := snTopAdmissionTestRule(t, fixture.set)
	rule.asns, rule.excusedAsns, rule.failingEverywhere = fixture.asns, map[uint32]bool{64510: true}, everywhere
	ruled := snTopApplyUnitRule(fixture.providers, rule)
	connect.AssertEqual(t, ruled.networks[farm].CountedProviders, 0)
	connect.AssertEqual(t, ruled.networks[farm].WithheldByReason, map[string]int{model.SnTopUnitReasonUdp: 900})
	connect.AssertEqual(t, ruled.networks[snTopAdmissionTestId(1)].CountedProviders, 1)
}

// Without a fresh admission set the rule cannot apply.
func TestSnTopUnitRuleNeedsAFreshAdmissionSet(t *testing.T) {
	_, unavailable := newSnTopUnitRule(nil, snTopAdmissionTestCountrySettings(), snTopAdmissionTestNow)
	connect.AssertEqual(t, unavailable, snTopSkippedAdmissionStale)
	stale := &model.SnTopAdmission{MeasuredAt: snTopAdmissionTestNow.Add(-model.SnTopAdmissionMaxAge - time.Second)}
	_, unavailable = newSnTopUnitRule(stale, snTopAdmissionTestCountrySettings(), snTopAdmissionTestNow)
	connect.AssertEqual(t, unavailable, snTopSkippedAdmissionStale)
	fresh := &model.SnTopAdmission{MeasuredAt: snTopAdmissionTestNow.Add(-model.SnTopAdmissionMaxAge)}
	rule, unavailable := newSnTopUnitRule(fresh, nil, snTopAdmissionTestNow)
	connect.AssertEqual(t, unavailable, "")
	// with no udp-off country in the catalog, Online counts nowhere
	connect.AssertEqual(t, rule.udpProbesDisabled("ru"), false)
}

// Before the first epoch the live board is unchanged and the shadow ranks the
// ruled providers, its EMA continuing only from the tempo just before; at the
// first fold of the rule the live board takes the shadow's states, and after
// it keeps its own.
func TestPlanSnTopUnitRule(t *testing.T) {
	livePriors := map[server.Id]model.SnTopUnitState{snTopAdmissionTestId(1): {Ema: 9, LastTempoIndex: 99}}
	shadowStates := map[server.Id]model.SnTopUnitState{snTopAdmissionTestId(1): {Ema: 1, LastTempoIndex: 99}}
	input := &model.SnTopProjectionInput{
		TempoIndex:    100,
		Providers:     []model.SnTopProvider{snTopAdmissionTestProvider(snTopAdmissionTestId(101), snTopAdmissionTestId(1), snTopAdmissionTestPrefix(1))},
		PriorNetworks: livePriors,
	}
	withheld := input.Providers[0]
	withheld.UnitWithheld = true
	ruled := &snTopRuledProviders{providers: []model.SnTopProvider{withheld}}

	// shadow run, continuing
	plan := planSnTopUnitRule(input, ruled, false, false, 100, 99, shadowStates)
	connect.AssertEqual(t, plan.live == input, true)
	connect.AssertEqual(t, plan.shadow.Providers[0].UnitWithheld, true)
	connect.AssertEqual(t, plan.shadow.PriorNetworks[snTopAdmissionTestId(1)].Ema, 1.0)
	// shadow run after a gap: the shadow's EMA starts again
	plan = planSnTopUnitRule(input, ruled, false, false, 100, 97, shadowStates)
	connect.AssertEqual(t, plan.shadow.PriorNetworks == nil, true)
	// the first fold of the rule opens where the shadow stood
	plan = planSnTopUnitRule(input, ruled, true, false, 100, 99, shadowStates)
	connect.AssertEqual(t, plan.shadow == nil, true)
	connect.AssertEqual(t, plan.live.Providers[0].UnitWithheld, true)
	connect.AssertEqual(t, plan.live.PriorNetworks[snTopAdmissionTestId(1)].Ema, 1.0)
	// later folds keep the live board's own states
	plan = planSnTopUnitRule(input, ruled, true, true, 100, 99, shadowStates)
	connect.AssertEqual(t, plan.live.PriorNetworks[snTopAdmissionTestId(1)].Ema, 9.0)
	// the input is not changed
	connect.AssertEqual(t, input.Providers[0].UnitWithheld, false)
	connect.AssertEqual(t, input.PriorNetworks[snTopAdmissionTestId(1)].Ema, 9.0)
}

// The shadow board lists the shadow's ranked rows by position, each beside its
// live position and units and its count of withheld providers.
func TestSnTopShadowBoardOf(t *testing.T) {
	a, b := snTopAdmissionTestId(1), snTopAdmissionTestId(2)
	shadow := &model.SnTopProjection{TotalRanked: 1, Networks: []*model.SnTopNetworkProjection{
		{NetworkId: b, Position: 1, Prefixes: 2, ProjectedScore: 2},
		{NetworkId: a, Position: 0},
	}}
	live := &model.SnTopProjection{TotalRanked: 2, Networks: []*model.SnTopNetworkProjection{
		{NetworkId: a, Position: 1, Prefixes: 40},
		{NetworkId: b, Position: 2, Prefixes: 2},
	}}
	networks := map[server.Id]*model.SnTopNetworkUnits{
		a: {WithheldByReason: map[string]int{model.SnTopUnitReasonUdp: 40}},
		b: {WithheldByReason: map[string]int{}},
	}
	tempo := model.SnTopTempo{Index: 1124, SettlementEpoch: model.SnTopUnitRuleFirstEpoch - 1}
	board := snTopShadowBoardOf(tempo, snTopAdmissionTestNow, shadow, live, networks)
	connect.AssertEqual(t, board.RuleFirstEpoch, model.SnTopUnitRuleFirstEpoch)
	connect.AssertEqual(t, board.TotalRanked, 1)
	connect.AssertEqual(t, board.Rows, []model.SnTopShadowRow{{NetworkId: b, Position: 1, ProjectedScore: 2, Prefixes: 2, LivePosition: 2, LivePrefixes: 2}})
}

// Tier B takes no prefix from a withheld provider, as from a zero pin; every
// other provider keeps what it pinned.
func TestSnTopVerifyCohortRankedProvidersWithholdPrefix(t *testing.T) {
	kept := model.SnTopProvider{ClientId: snTopAdmissionTestId(1), NetworkId: snTopAdmissionTestId(2), Eligible: true, PrefixHash: snTopAdmissionTestPrefix(1), HasPrefix: true}
	withheld := kept
	withheld.ClientId = snTopAdmissionTestId(3)
	withheld.UnitWithheld = true
	unpinned := model.SnTopProvider{ClientId: snTopAdmissionTestId(4), NetworkId: snTopAdmissionTestId(2), Eligible: true}
	ranked := snTopVerifyCohortRankedProviders([]model.SnTopProvider{kept, withheld, unpinned})
	connect.AssertEqual(t, len(ranked), 3)
	connect.AssertEqual(t, ranked[0], model.VerifyCohortRankedProvider{ClientId: kept.ClientId, NetworkId: kept.NetworkId, Eligible: true, PrefixHash: kept.PrefixHash, HasPrefix: true})
	connect.AssertEqual(t, ranked[1].HasPrefix, false)
	connect.AssertEqual(t, ranked[1].Eligible, true)
	connect.AssertEqual(t, ranked[2].HasPrefix, false)
}
