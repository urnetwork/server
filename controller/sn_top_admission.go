package controller

// Rule (a) of the SN Top board (owner decisions 2026-11-20, "build Top 200
// rule (a)" and "add all three"): a provider's prefix counts toward its
// network's units only if the provider passes Speed or Quality admission
// (decideProviderEgress, as the score export decided it, smoothed over the
// export's rolling window, model/sn_top_admission_model.go). Two excuses:
//
//   - in a country whose URL probes make no UDP leg (qualityprobe.yml
//     country_settings, udp_probes: false) Online membership counts instead,
//     since there the probes judge HTTPS legs alone;
//   - in an autonomous system where UDP fails at the network level, admission
//     with the UDP failures forgiven counts instead (snTopUdpExcusedAsns).
//
// A withheld provider shows no prefix (model.SnTopProvider.UnitWithheld), as a
// provider whose transport declared a proxy egress, or whose probed exit
// differs from its networks, shows none (verify_proxied_egress_model.go); the
// marks apply first, and admission never gives a marked provider a unit back.
//
// The rule ranks the live board from model.SnTopUnitRuleFirstEpoch. Before it,
// the fold ranks the live board as before and a shadow board under the rule
// (GET /sn/top?board=shadow); at the first fold of that epoch the live board
// takes the shadow's network EMA state, so it opens where the shadow stood.
// Each fold stores each network's unit status (GET /sn/top/me).

import (
	"context"
	"slices"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

const (
	// the export's admission set is missing or older than
	// model.SnTopAdmissionMaxAge at the fold
	snTopSkippedAdmissionStale = "admission_stale"
	// the admission set could not be read
	snTopSkippedAdmissionUnreadable = "admission_unreadable"
)

// The network-level UDP excuse: an autonomous system where at least
// snTopUdpExcuseMinNetworks established networks, and at least
// snTopUdpExcuseNetworkShare of the established networks measured there, have
// their providers there mostly fail UDP while they pass over HTTPS. Counted by
// network, as the h3 and TLS place rules count, so one network's identities
// make no excuse for themselves or their neighbours; established is created
// at least snTopUdpExcuseNetworkMinAge before the fold. A network whose
// providers fail UDP in every system it is measured in, in at least
// snTopUdpEverywhereMinAsns of them, fails UDP because of what it runs, not
// where: it neither counts toward an excuse nor is excused.
const (
	snTopUdpExcuseMinNetworks   = 5
	snTopUdpExcuseNetworkShare  = 2.0 / 3.0
	snTopUdpExcuseNetworkMinAge = 14 * 24 * time.Hour
	snTopUdpEverywhereMinAsns   = 2
	snTopUdpEverywhereShare     = 0.9
	// a network's providers in a system "mostly" fail UDP from this share
	snTopUdpNetworkFailingShare = 0.5
)

// A provider withheld only because the rolling window has not caught up with
// its current admission: it is admitted now and counts again once its track
// reaches model.SnTopAdmissionRestoreAt.
const snTopUnitReasonWindow = "window"

// The fold's rule: the export's decisions, the countries whose URL probes make
// no UDP leg, the systems excused at the network level and each provider's
// systems, the networks that fail UDP everywhere, and the marked providers.
type snTopUnitRule struct {
	admission         *model.SnTopAdmission
	udpProbesDisabled func(countryCode string) bool
	asns              map[server.Id][]uint32
	excusedAsns       map[uint32]bool
	failingEverywhere map[server.Id]bool
	marked            map[server.Id]bool
}

// The rule from its sources at now, or the reason the fold cannot apply it.
func newSnTopUnitRule(
	admission *model.SnTopAdmission,
	countrySettings map[string]egresshealth.CountrySettings,
	now time.Time,
) (*snTopUnitRule, string) {
	if !admission.Fresh(now) {
		return nil, snTopSkippedAdmissionStale
	}
	pool := &egresshealth.Pool{CountrySettings: countrySettings}
	return &snTopUnitRule{
		admission:         admission,
		udpProbesDisabled: pool.UdpProbesDisabled,
		asns:              map[server.Id][]uint32{},
		excusedAsns:       map[uint32]bool{},
		failingEverywhere: map[server.Id]bool{},
		marked:            map[server.Id]bool{},
	}, ""
}

// Reads the rule's sources for the fold's providers at now.
func loadSnTopUnitRule(ctx context.Context, now time.Time, input *model.SnTopProjectionInput) (*snTopUnitRule, string) {
	admission, err := model.ReadSnTopAdmission(ctx)
	if err != nil {
		glog.Errorf("[sntop]the export's admission set is unreadable: %s\n", err)
		return nil, snTopSkippedAdmissionUnreadable
	}
	var countrySettings map[string]egresshealth.CountrySettings
	if sites, err := LoadProviderEgressSites(); err == nil {
		countrySettings = sites.CountrySettings
	} else {
		glog.Errorf("[sntop]qualityprobe.yml is unavailable for its udp country settings; no country counts Online: %s\n", err)
	}
	rule, unavailable := newSnTopUnitRule(admission, countrySettings, now)
	if rule == nil {
		return nil, unavailable
	}
	clientIds := make([]server.Id, 0, len(input.Providers))
	for _, provider := range input.Providers {
		clientIds = append(clientIds, provider.ClientId)
	}
	rule.marked = model.GetVerifyProxiedEgress(ctx, clientIds)
	rule.asns = model.GetProviderConnectionNetworkAsnsForClients(ctx, clientIds)
	rule.excusedAsns, rule.failingEverywhere = snTopUdpExcusedAsns(input.Providers, admission, rule.asns, input.NetworkCreateTimes, now)
	return rule, ""
}

// The systems excused at the network level, and the networks that fail UDP
// everywhere, from the fold's providers.
func snTopUdpExcusedAsns(
	providers []model.SnTopProvider,
	admission *model.SnTopAdmission,
	asns map[server.Id][]uint32,
	networkCreateTimes map[server.Id]time.Time,
	now time.Time,
) (excused map[uint32]bool, failingEverywhere map[server.Id]bool) {
	type tally struct {
		probed int
		// UDP fails, and the provider passes with UDP forgiven
		failingHttpsPassing int
		// UDP fails, whatever HTTPS does
		udpFailing int
	}
	networkAsns := map[server.Id]map[uint32]*tally{}
	seen := map[server.Id]bool{}
	for _, provider := range providers {
		if seen[provider.ClientId] {
			continue
		}
		seen[provider.ClientId] = true
		entry := admission.Providers[provider.ClientId]
		if entry == nil || !entry.Probed {
			continue
		}
		for _, asn := range asns[provider.ClientId] {
			if networkAsns[provider.NetworkId] == nil {
				networkAsns[provider.NetworkId] = map[uint32]*tally{}
			}
			t := networkAsns[provider.NetworkId][asn]
			if t == nil {
				t = &tally{}
				networkAsns[provider.NetworkId][asn] = t
			}
			t.probed += 1
			if entry.UdpDowngraded {
				t.udpFailing += 1
				if entry.TcpAdmitted {
					t.failingHttpsPassing += 1
				}
			}
		}
	}

	failingEverywhere = map[server.Id]bool{}
	for networkId, byAsn := range networkAsns {
		if len(byAsn) < snTopUdpEverywhereMinAsns {
			continue
		}
		everywhere := true
		for _, t := range byAsn {
			if float64(t.udpFailing) < snTopUdpEverywhereShare*float64(t.probed) {
				everywhere = false
				break
			}
		}
		failingEverywhere[networkId] = everywhere
	}

	type asnNetworks struct {
		established int
		failing     int
	}
	byAsn := map[uint32]*asnNetworks{}
	for networkId, networkByAsn := range networkAsns {
		if failingEverywhere[networkId] {
			continue
		}
		createTime, ok := networkCreateTimes[networkId]
		if !ok || now.Sub(createTime) < snTopUdpExcuseNetworkMinAge {
			continue
		}
		for asn, t := range networkByAsn {
			a := byAsn[asn]
			if a == nil {
				a = &asnNetworks{}
				byAsn[asn] = a
			}
			a.established += 1
			if snTopUdpNetworkFailingShare*float64(t.probed) <= float64(t.failingHttpsPassing) {
				a.failing += 1
			}
		}
	}
	excused = map[uint32]bool{}
	for asn, a := range byAsn {
		if snTopUdpExcuseMinNetworks <= a.failing && snTopUdpExcuseNetworkShare*float64(a.established) <= float64(a.failing) {
			excused[asn] = true
		}
	}
	return
}

// Whether the provider's unit counts under the rule, and why not.
func (self *snTopUnitRule) decide(provider *model.SnTopProvider) (counts bool, reason string) {
	if self.marked[provider.ClientId] {
		return false, model.SnTopUnitReasonExitDiffers
	}
	entry := self.admission.Providers[provider.ClientId]
	if entry == nil {
		// the export scored it nowhere: it is not online
		return false, model.SnTopUnitReasonUnprobed
	}
	excuses := model.SnTopAdmissionExcuses{
		UdpProbesDisabled: self.udpProbesDisabled != nil && self.udpProbesDisabled(entry.CountryCode),
	}
	if !self.failingEverywhere[provider.NetworkId] {
		for _, asn := range self.asns[provider.ClientId] {
			if self.excusedAsns[asn] {
				excuses.UdpNetworkLevel = true
				break
			}
		}
	}
	if entry.CountsUnits(excuses) {
		return true, model.SnTopUnitReasonNone
	}
	if entry.Reason == model.SnTopUnitReasonNone {
		return false, snTopUnitReasonWindow
	}
	return false, entry.Reason
}

// The fold's providers under the rule: a copy with each withheld provider
// marked, and each network's unit status.
type snTopRuledProviders struct {
	providers []model.SnTopProvider
	withheld  int
	networks  map[server.Id]*model.SnTopNetworkUnits
}

func snTopApplyUnitRule(providers []model.SnTopProvider, rule *snTopUnitRule) *snTopRuledProviders {
	ruled := &snTopRuledProviders{
		providers: slices.Clone(providers),
		networks:  map[server.Id]*model.SnTopNetworkUnits{},
	}
	for i := range ruled.providers {
		provider := &ruled.providers[i]
		units := ruled.networks[provider.NetworkId]
		if units == nil {
			units = &model.SnTopNetworkUnits{
				WithheldByReason:  map[string]int{},
				WithheldProviders: []model.SnTopProviderUnit{},
			}
			ruled.networks[provider.NetworkId] = units
		}
		counts, reason := rule.decide(provider)
		if counts {
			units.CountedProviders += 1
			continue
		}
		provider.UnitWithheld = true
		ruled.withheld += 1
		units.WithheldByReason[reason] += 1
		if len(units.WithheldProviders) < model.SnTopNetworkUnitsListLimit {
			units.WithheldProviders = append(units.WithheldProviders, model.SnTopProviderUnit{ClientId: provider.ClientId, Reason: reason})
		}
	}
	return ruled
}

// Tier B's view of the fold's providers: a withheld provider has no prefix for
// the representatives either, as a zero pin has none.
func snTopVerifyCohortRankedProviders(providers []model.SnTopProvider) []model.VerifyCohortRankedProvider {
	ranked := make([]model.VerifyCohortRankedProvider, 0, len(providers))
	for _, provider := range providers {
		ranked = append(ranked, model.VerifyCohortRankedProvider{
			ClientId:   provider.ClientId,
			NetworkId:  provider.NetworkId,
			Eligible:   provider.Eligible,
			PrefixHash: provider.PrefixHash,
			HasPrefix:  provider.HasPrefix && !provider.UnitWithheld,
		})
	}
	return ranked
}

// The shadow board of a fold: the shadow's ranked rows beside the live ones.
func snTopShadowBoardOf(
	tempo model.SnTopTempo,
	now time.Time,
	shadow *model.SnTopProjection,
	live *model.SnTopProjection,
	networks map[server.Id]*model.SnTopNetworkUnits,
) *model.SnTopShadowBoard {
	board := &model.SnTopShadowBoard{
		TempoIndex:      tempo.Index,
		SettlementEpoch: tempo.SettlementEpoch,
		ComputedAt:      now,
		RuleFirstEpoch:  model.SnTopUnitRuleFirstEpoch,
		TotalRanked:     shadow.TotalRanked,
		Rows:            []model.SnTopShadowRow{},
	}
	liveRows := map[server.Id]*model.SnTopNetworkProjection{}
	for _, row := range live.Networks {
		liveRows[row.NetworkId] = row
	}
	for _, row := range shadow.Networks {
		if row.Position <= 0 || model.SnTopShadowRows < row.Position {
			continue
		}
		shadowRow := model.SnTopShadowRow{
			NetworkId:      row.NetworkId,
			Position:       row.Position,
			ProjectedScore: model.SnTopFinite(row.ProjectedScore),
			RawLastTempo:   model.SnTopFinite(row.RawLastTempo),
			Prefixes:       row.Prefixes,
			Label:          row.Label,
			Claimed:        row.Claimed,
		}
		if liveRow := liveRows[row.NetworkId]; liveRow != nil {
			shadowRow.LivePosition = liveRow.Position
			shadowRow.LivePrefixes = liveRow.Prefixes
		}
		if units := networks[row.NetworkId]; units != nil {
			for _, count := range units.WithheldByReason {
				shadowRow.WithheldProviders += count
			}
		}
		board.Rows = append(board.Rows, shadowRow)
	}
	slices.SortFunc(board.Rows, func(a, b model.SnTopShadowRow) int { return a.Position - b.Position })
	return board
}

// Stamps each network's unit status with the fold.
func snTopStampNetworkUnits(networks map[server.Id]*model.SnTopNetworkUnits, tempo model.SnTopTempo, now time.Time, ruleLive bool) {
	for _, units := range networks {
		units.TempoIndex = tempo.Index
		units.ComputedAt = now
		units.RuleLive = ruleLive
		units.RuleFirstEpoch = model.SnTopUnitRuleFirstEpoch
	}
}

// What a fold ranks under the rule.
type snTopUnitRulePlan struct {
	// the live board's input
	live *model.SnTopProjectionInput
	// the shadow board's input, nil once the rule ranks the live board
	shadow *model.SnTopProjectionInput
}

// Plans the fold: before the rule's first epoch the live board is unchanged
// and a shadow board ranks the ruled providers, its EMA continuing from the
// shadow states of the tempo just before; from that epoch the live board ranks
// the ruled providers, and its first fold of the rule takes the shadow's
// states when they continue, so it opens where the shadow board stood.
func planSnTopUnitRule(
	input *model.SnTopProjectionInput,
	ruled *snTopRuledProviders,
	ruleLive bool,
	latestRuleLive bool,
	tempoIndex int64,
	shadowTempoIndex int64,
	shadowStates map[server.Id]model.SnTopUnitState,
) snTopUnitRulePlan {
	shadowContinues := shadowStates != nil && shadowTempoIndex == tempoIndex-1
	if ruleLive {
		live := *input
		live.Providers = ruled.providers
		if !latestRuleLive && shadowContinues {
			live.PriorNetworks = shadowStates
		}
		return snTopUnitRulePlan{live: &live}
	}
	shadow := *input
	shadow.Providers = ruled.providers
	shadow.PriorNetworks = nil
	if shadowContinues {
		shadow.PriorNetworks = shadowStates
	}
	return snTopUnitRulePlan{live: input, shadow: &shadow}
}
