package model

import (
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// D4, pure: the Online rule, its URL ratio and its country guard read the
// general catalog's outcomes (ProviderEgressHealthCounts.General); Quality
// and the index read every scored outcome.

// A window of the general catalog's outcomes and scored outcomes from another
// source, as the window's reader loads them: the whole tally, with the general
// one beside it.
func onlineSourceTestCounts(general ProviderEgressHealthCounts, other ProviderEgressHealthCounts) ProviderEgressHealthCounts {
	return ProviderEgressHealthCounts{
		MeasuredAt:      general.MeasuredAt,
		FirstMeasuredAt: general.FirstMeasuredAt,
		OKCount:         general.OKCount + other.OKCount,
		Total:           general.Total + other.Total,
		General:         &general,
	}
}

// A tally without a general part is all general: what every reader and test
// built before the source was recorded holds, and what the window's reader
// loads while every scored outcome is general.
func TestProviderEgressHealthCountsWithoutSourcesAreGeneral(t *testing.T) {
	counts := onlineSuccessTestCounts(3, 4)
	connect.AssertEqual(t, counts.onlineCounts(), counts)
	general := onlineSuccessTestCounts(1, 2)
	connect.AssertEqual(t, onlineSourceTestCounts(general, onlineSuccessTestCounts(2, 2)).onlineCounts(), general)
}

// In a country where the general catalog fails for every provider, scored
// country outcomes that pass neither end the exemption nor put a provider
// under the ratio; a failing country site does not take a provider whose
// general outcomes pass out of Online, and a passing country list does not
// keep one below the ratio on its general outcomes in it.
func TestOnlineUrlRuleAndCountryGuardReadTheGeneralTally(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	settings.OnlineUrlSuccessRatio = 0.8
	ge, ge2, ge3 := server.NewId(), server.NewId(), server.NewId()
	usHealthy, usBrokenCountrySite, usMasked := server.NewId(), server.NewId(), server.NewId()
	clientIdCounts := map[server.Id]ProviderEgressHealthCounts{
		ge:                  onlineSourceTestCounts(onlineSuccessTestCounts(0, 5), onlineSuccessTestCounts(5, 5)),
		ge2:                 onlineSourceTestCounts(onlineSuccessTestCounts(0, 4), onlineSuccessTestCounts(4, 4)),
		ge3:                 onlineSourceTestCounts(onlineSuccessTestCounts(0, 6), onlineSuccessTestCounts(6, 6)),
		usHealthy:           onlineSuccessTestCounts(10, 10),
		usBrokenCountrySite: onlineSourceTestCounts(onlineSuccessTestCounts(10, 10), onlineSuccessTestCounts(0, 10)),
		usMasked:            onlineSourceTestCounts(onlineSuccessTestCounts(2, 10), onlineSuccessTestCounts(30, 30)),
	}
	clientIdCountryCodes := map[server.Id]string{
		ge: "ge", ge2: "ge", ge3: "ge",
		usHealthy: "us", usBrokenCountrySite: "us", usMasked: "us",
	}
	filter := onlineSuccessTestFilter(clientIdCounts, clientIdCountryCodes, settings)

	connect.AssertEqual(t, filter.onlineSuccessBaseline.countryCodeCounts["ge"], providerOnlineSuccessCount{Measured: 3, Succeeded: 0, NoSuccess: 3, Exempt: true})
	connect.AssertEqual(t, filter.onlineSuccessBaseline.countryCodeCounts["us"], providerOnlineSuccessCount{Measured: 3, Succeeded: 3, LowSuccess: 1})

	for _, clientId := range []server.Id{ge, ge2, ge3} {
		decision := onlineSuccessTestDecide(filter, clientId, "ge", settings)
		// half of the scored outcomes fail, which is no Quality verdict
		if !decision.online || !decision.counted || decision.reason != ProviderExcludedHealth {
			t.Errorf("exempt country decision %+v, want online for health", decision)
		}
	}
	healthy := onlineSuccessTestDecide(filter, usHealthy, "us", settings)
	if !healthy.online || !healthy.qualityBeforeModernGate {
		t.Errorf("healthy decision %+v, want online with a Quality verdict", healthy)
	}
	broken := onlineSuccessTestDecide(filter, usBrokenCountrySite, "us", settings)
	if !broken.online || broken.quality || broken.reason != ProviderExcludedHealth {
		t.Errorf("a failing country site decided %+v, want online for health: Quality reads the scored country outcomes, Online does not", broken)
	}
	masked := onlineSuccessTestDecide(filter, usMasked, "us", settings)
	if masked.online || masked.reason != ProviderExcludedLowSuccess {
		t.Errorf("a passing country list decided %+v, want low_success on the general outcomes", masked)
	}
}

// A provider whose only current outcomes come from another source has no
// Online evidence: it keeps the benefit of the doubt, and the guard does not
// count it, as an unprobed provider.
func TestOnlineUrlRuleWithoutGeneralOutcomesIsUnprobed(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	settings.OnlineUrlSuccessRatio = 0.8
	countryOnly, usHealthy := server.NewId(), server.NewId()
	clientIdCounts := map[server.Id]ProviderEgressHealthCounts{
		countryOnly: onlineSourceTestCounts(ProviderEgressHealthCounts{}, onlineSuccessTestCounts(0, 6)),
		usHealthy:   onlineSuccessTestCounts(10, 10),
	}
	filter := onlineSuccessTestFilter(clientIdCounts, map[server.Id]string{countryOnly: "us", usHealthy: "us"}, settings)
	connect.AssertEqual(t, filter.onlineSuccessBaseline.countryCodeCounts["us"], providerOnlineSuccessCount{Measured: 1, Succeeded: 1})
	_, onlineIndex := filter.onlineUrlIndex(countryOnly, settings)
	if onlineIndex.Evidence {
		t.Fatal("outcomes from another source alone are Online evidence")
	}
	decision := onlineSuccessTestDecide(filter, countryOnly, "us", settings)
	if !decision.online || decision.reason == ProviderExcludedNoSuccess || decision.reason == ProviderExcludedLowSuccess {
		t.Errorf("decision %+v, want online: the Online rule has no general outcome to read", decision)
	}
	// its scored outcomes still decide Quality: six failures in six
	if decision.quality {
		t.Errorf("decision %+v, want no Quality from failing scored outcomes", decision)
	}
}
