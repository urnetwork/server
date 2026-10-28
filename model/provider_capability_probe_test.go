// The capability probe's eligibility rule, pure: the Online population, as
// the score export decides it.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The capability probe measures the Online population modern-first priority
// and modern-only requests draw from (FP2-MODERN.md): every provider the
// egress rules keep in the Online bucket, the Quality and Speed members
// included, whatever its evidence and the modern provider gate. A provider
// without a URL run, one whose failing ratio keeps it Online and one ARIN
// keeps out of Quality are measured, with the gate off or on; the hard
// exclusions (reliability, ARIN risk, the URL security quarantine) are not.
func TestCapabilityProbeEligibilityIsTheOnlinePopulation(t *testing.T) {
	modern, legacy, stale, noEvidence, nonQuality := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	failing, unprobed, unreliable, risky, insecure := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	filter := modernGateTestFilter(map[server.Id]*ProviderModernEvidenceRecord{
		modern:     modernGateTestRecord(true, true, time.Hour),
		legacy:     modernGateTestRecord(false, false, time.Hour),
		stale:      modernGateTestRecord(true, true, ProviderModernEvidenceMaxAge+time.Hour),
		noEvidence: nil,
		nonQuality: nil,
		failing:    nil,
		unreliable: nil,
		risky:      nil,
		insecure:   nil,
	})
	// one success of five is under the ratio
	filter.healthCounts[failing] = ProviderEgressHealthCounts{
		MeasuredAt:      modernGateTestNow.Add(-time.Minute),
		FirstMeasuredAt: modernGateTestNow.Add(-time.Hour),
		OKCount:         1,
		Total:           5,
	}
	filter.arinNonQuality[nonQuality] = true
	filter.reliabilityFailed[unreliable] = true
	filter.arinRisk[risky] = true
	filter.tlsAuthenticationFailed[insecure] = true
	clientIds := []server.Id{modern, legacy, stale, noEvidence, nonQuality, failing, unprobed, unreliable, risky, insecure}
	for _, gate := range []bool{false, true} {
		eligible := providerCapabilityProbeEligible(filter, map[server.Id]*string{}, clientIds, modernGateTestSettings(gate), false)
		connect.AssertEqual(t, eligible, []server.Id{modern, legacy, stale, noEvidence, nonQuality, failing, unprobed})
		// the gate takes the providers without modern evidence out of the
		// native buckets, and the probe still measures them
		if gate {
			for _, clientId := range []server.Id{legacy, stale, noEvidence} {
				decision := modernGateTestDecide(filter, clientId, modernGateTestSettings(gate))
				if decision.quality || decision.speed || decision.reason != ProviderExcludedNotModern || !decision.online {
					t.Errorf("the gate kept %s in a native bucket or took it out of online: %+v", clientId, decision)
				}
			}
		}
	}
	// the order is the caller's, and an empty cohort is empty
	connect.AssertEqual(t, providerCapabilityProbeEligible(filter, map[server.Id]*string{}, []server.Id{unprobed, modern}, modernGateTestSettings(false), false), []server.Id{unprobed, modern})
	connect.AssertEqual(t, providerCapabilityProbeEligible(filter, map[server.Id]*string{}, nil, modernGateTestSettings(false), false), []server.Id{})
}

// The native URL-run minimums decide only Quality and Speed: a provider with
// one passing URL run is Online, as one with two is, so both are measured
// with the gate off or on. Before FP2-MODERN.md the probe waited for the
// second run, which admits a provider to Speed.
func TestCapabilityProbeMeasuresProvidersShortOfTheRunMinimums(t *testing.T) {
	once, twice := server.NewId(), server.NewId()
	filter := modernGateTestFilter(map[server.Id]*ProviderModernEvidenceRecord{once: nil, twice: nil})
	measuredAt := modernGateTestNow.Add(-time.Minute)
	filter.healthCounts[once] = ProviderEgressHealthCounts{MeasuredAt: measuredAt, FirstMeasuredAt: measuredAt, OKCount: 1, Total: 1}
	filter.healthCounts[twice] = ProviderEgressHealthCounts{MeasuredAt: measuredAt, FirstMeasuredAt: measuredAt.Add(-20 * time.Minute), OKCount: 2, Total: 2}
	for _, gate := range []bool{false, true} {
		connect.AssertEqual(t, providerCapabilityProbeEligible(filter, map[server.Id]*string{}, []server.Id{once, twice}, modernGateTestSettings(gate), false), []server.Id{once, twice})
	}
}

// Review finding 2 (FP2-MODERN-REVIEW.md), the rule: Online is decided in
// each provider's published country by the guard, as the score export
// decides it. With a guard over us, where the probe works, and vn, where it
// fails for most providers, a provider without a URL success and one below
// the Online URL ratio are in no bucket in us and not measured; the same
// providers in vn, and one without a published country the guard measured,
// stay Online and are measured. A failing ratio at the Online URL ratio and
// no URL run stay Online everywhere. Without the published countries every
// provider reads as the guard's unmeasured country, and without a guard every
// country is exempt; either way the providers the guard takes out in us come
// back. A hard exclusion is never measured.
func TestCapabilityProbeEligibilityFollowsTheGuardInThePublishedCountry(t *testing.T) {
	usDead, usLow, usFailing, usUnprobed, usRisky := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	vnDead, vnLow, unknownDead := server.NewId(), server.NewId(), server.NewId()
	us, vn := "US", "vn"
	clientIdPublishedCountryCodes := map[server.Id]*string{
		usDead:     &us,
		usLow:      &us,
		usFailing:  &us,
		usUnprobed: &us,
		usRisky:    &us,
		vnDead:     &vn,
		vnLow:      &vn,
		// no published country: its rollup's country location is unknown
		unknownDead: nil,
	}
	// three of four measured in us have a success, one of four in vn
	guard := onlineSuccessTestBaseline(0.36, map[string]providerOnlineSuccessCount{
		"us": {Measured: 4, Succeeded: 3, NoSuccess: 1, LowSuccess: 1},
		"vn": {Measured: 4, Succeeded: 1, NoSuccess: 3, LowSuccess: 1},
	})
	guard.urlSuccessRatio = 0.5
	guard.urlRatioCountryExemption = true
	filter := providerCountFilter{
		now:                     onlineSuccessTestNow,
		arinRisk:                map[server.Id]bool{usRisky: true},
		arinNonQuality:          map[server.Id]bool{},
		reliabilityFailed:       map[server.Id]bool{},
		tlsAuthenticationFailed: map[server.Id]bool{},
		countryCodes:            map[server.Id]string{},
		healthCounts: map[server.Id]ProviderEgressHealthCounts{
			usDead:      onlineSuccessTestCounts(0, 5),
			usLow:       onlineSuccessTestCounts(1, 5),
			usFailing:   onlineSuccessTestCounts(3, 5),
			usRisky:     onlineSuccessTestCounts(5, 5),
			vnDead:      onlineSuccessTestCounts(0, 5),
			vnLow:       onlineSuccessTestCounts(1, 5),
			unknownDead: onlineSuccessTestCounts(0, 5),
		},
		modernEvidence:        map[server.Id]*ProviderModernEvidenceRecord{},
		onlineSuccessBaseline: guard,
	}
	settings := modernGateTestSettings(false)
	clientIds := []server.Id{usDead, usLow, usFailing, usUnprobed, usRisky, vnDead, vnLow, unknownDead}

	connect.AssertEqual(
		t,
		providerCapabilityProbeEligible(filter, clientIdPublishedCountryCodes, clientIds, settings, false),
		[]server.Id{usFailing, usUnprobed, vnDead, vnLow, unknownDead},
	)
	// the export's reasons for the two the probe leaves out
	connect.AssertEqual(t, decideProviderEgress(filter.egressFacts(usDead, &us, nil, nil, settings), false).reason, ProviderExcludedNoSuccess)
	connect.AssertEqual(t, decideProviderEgress(filter.egressFacts(usLow, &us, nil, nil, settings), false).reason, ProviderExcludedLowSuccess)

	everyOnline := []server.Id{usDead, usLow, usFailing, usUnprobed, vnDead, vnLow, unknownDead}
	// without the published countries
	connect.AssertEqual(t, providerCapabilityProbeEligible(filter, map[server.Id]*string{}, clientIds, settings, false), everyOnline)
	// without a guard
	filter.onlineSuccessBaseline = nil
	connect.AssertEqual(t, providerCapabilityProbeEligible(filter, clientIdPublishedCountryCodes, clientIds, settings, false), everyOnline)
}
