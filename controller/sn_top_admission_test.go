// Rule (a) of the SN Top board at the fold (sn_top_admission.go, owner
// decision 2026-11-20): the fold withholds the unit of every provider the
// export did not admit, counts Online membership instead where the probe
// catalog turns UDP probes off, skips the tempo without a fresh set, and gives
// tier B no prefix for a withheld provider. Before the rule every provider
// with a prefix counted. Pure: the fold's inputs are built here.
package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

func snTopAdmissionTestId(n byte) server.Id {
	var id server.Id
	id[0] = 0x5a
	id[15] = n
	return id
}

func snTopAdmissionTestPrefix(n byte) [32]byte {
	var h [32]byte
	h[0] = n
	h[31] = n
	return h
}

// the probe catalog's country settings: UDP probes off in one country
func snTopAdmissionTestCountrySettings() map[string]egresshealth.CountrySettings {
	off := false
	return map[string]egresshealth.CountrySettings{"ru": {UdpProbes: &off}}
}

// One network per case, each with one provider and its own prefix:
//
//	1 admitted                          counts
//	2 not admitted, not online          withheld
//	3 online only, udp-off country      counts (the carve-out)
//	4 online only, udp-on country       withheld
//	5 not in the export's set           withheld
//	6 admitted, but its pin is the zero hash (a declared proxy egress or a
//	  probed exit that differs from its networks)       withheld by the mark
func TestSnTopFoldCountsOnlyAdmittedProviders(t *testing.T) {
	now := time.Date(2026, 11, 20, 22, 40, 0, 0, time.UTC)
	providers := []model.SnTopProvider{}
	set := map[server.Id]*model.SnTopAdmissionProvider{}
	decide := func(n byte, countryCode string, admitted bool, online bool, inSet bool, zeroPin bool) {
		clientId := snTopAdmissionTestId(100 + n)
		prefix := snTopAdmissionTestPrefix(n)
		if zeroPin {
			prefix = [32]byte{}
		}
		providers = append(providers, model.SnTopProvider{
			ClientId:   clientId,
			NetworkId:  snTopAdmissionTestId(n),
			Eligible:   true,
			PrefixHash: prefix,
			HasPrefix:  true,
			Presence:   1,
		})
		if inSet {
			set[clientId] = &model.SnTopAdmissionProvider{ClientId: clientId, CountryCode: countryCode, Admitted: admitted, Online: online}
		}
	}
	decide(1, "us", true, true, true, false)
	decide(2, "us", false, false, true, false)
	decide(3, "ru", false, true, true, false)
	decide(4, "vn", false, true, true, false)
	decide(5, "us", true, true, false, false)
	decide(6, "ca", true, true, true, true)

	unitAdmission, unavailable := newSnTopUnitAdmission(&model.SnTopAdmission{MeasuredAt: now.Add(-time.Minute), Providers: set}, snTopAdmissionTestCountrySettings(), now)
	connect.AssertEqual(t, unavailable, "")
	withheld := snTopApplyUnitAdmission(providers, unitAdmission)
	connect.AssertEqual(t, withheld, 3)
	want := map[byte]bool{1: false, 2: true, 3: false, 4: true, 5: true, 6: false}
	for i, provider := range providers {
		connect.AssertEqual(t, provider.UnitWithheld, want[byte(i+1)])
	}

	projection := model.ProjectSnTop(&model.SnTopProjectionInput{TempoIndex: 10, Providers: providers})
	ranked := map[server.Id]int{}
	for _, row := range projection.Networks {
		if 0 < row.Position {
			ranked[row.NetworkId] = row.Prefixes
		}
	}
	// only the admitted network and the udp-off Online one show a unit; the
	// marked network shows none although admitted
	connect.AssertEqual(t, ranked, map[server.Id]int{snTopAdmissionTestId(1): 1, snTopAdmissionTestId(3): 1})
}

// Without a fresh admission set the fold skips the tempo rather than count
// without the rule.
func TestSnTopFoldSkipsWithoutAFreshAdmissionSet(t *testing.T) {
	now := time.Date(2026, 11, 20, 22, 40, 0, 0, time.UTC)
	_, unavailable := newSnTopUnitAdmission(nil, snTopAdmissionTestCountrySettings(), now)
	connect.AssertEqual(t, unavailable, snTopSkippedAdmissionStale)
	stale := &model.SnTopAdmission{MeasuredAt: now.Add(-model.SnTopAdmissionMaxAge - time.Second)}
	_, unavailable = newSnTopUnitAdmission(stale, snTopAdmissionTestCountrySettings(), now)
	connect.AssertEqual(t, unavailable, snTopSkippedAdmissionStale)
	fresh := &model.SnTopAdmission{MeasuredAt: now.Add(-model.SnTopAdmissionMaxAge)}
	unitAdmission, unavailable := newSnTopUnitAdmission(fresh, nil, now)
	connect.AssertEqual(t, unavailable, "")
	// with no udp-off country in the catalog, Online counts nowhere
	connect.AssertEqual(t, (&model.SnTopAdmissionProvider{CountryCode: "ru", Online: true}).CountsUnits(unitAdmission.udpProbesDisabled), false)
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
