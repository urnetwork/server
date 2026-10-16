// The capability probe's eligibility rule, pure: the modern provider gate's
// own population.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The capability probe measures the population the modern provider gate
// reads: the providers the egress rules admit to Quality or Speed before that
// gate. A provider the gate takes out for want of evidence stays in it, with
// the gate off or on, or it could never return; the common gates, a failing
// or missing URL ratio keep a provider out; ARIN non-quality keeps Speed.
func TestCapabilityProbeEligibilityIsTheModernGatePopulation(t *testing.T) {
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
		eligible := providerCapabilityProbeEligible(filter, clientIds, modernGateTestSettings(gate), false)
		connect.AssertEqual(t, eligible, []server.Id{modern, legacy, stale, noEvidence, nonQuality})
		// the gate takes the providers without modern evidence out, and the
		// probe still measures them
		if gate {
			for _, clientId := range []server.Id{legacy, stale, noEvidence} {
				decision := modernGateTestDecide(filter, clientId, modernGateTestSettings(gate))
				if decision.quality || decision.speed || decision.reason != ProviderExcludedNotModern {
					t.Errorf("the gate kept %s in a native bucket: %+v", clientId, decision)
				}
			}
		}
	}
	// the order is the caller's, and an empty cohort is empty
	connect.AssertEqual(t, providerCapabilityProbeEligible(filter, []server.Id{nonQuality, modern}, modernGateTestSettings(false), false), []server.Id{nonQuality, modern})
	connect.AssertEqual(t, providerCapabilityProbeEligible(filter, nil, modernGateTestSettings(false), false), []server.Id{})
}

// The native URL-run minimums come before the probe, as before the gate it
// serves (by choice): one passing URL run is no native verdict, so a provider
// is first measured after its second run admits it to Speed.
func TestCapabilityProbeWaitsForTheSpeedRunMinimum(t *testing.T) {
	once, twice := server.NewId(), server.NewId()
	filter := modernGateTestFilter(map[server.Id]*ProviderModernEvidenceRecord{once: nil, twice: nil})
	measuredAt := modernGateTestNow.Add(-time.Minute)
	filter.healthCounts[once] = ProviderEgressHealthCounts{MeasuredAt: measuredAt, FirstMeasuredAt: measuredAt, OKCount: 1, Total: 1}
	filter.healthCounts[twice] = ProviderEgressHealthCounts{MeasuredAt: measuredAt, FirstMeasuredAt: measuredAt.Add(-20 * time.Minute), OKCount: 2, Total: 2}
	connect.AssertEqual(t, providerCapabilityProbeEligible(filter, []server.Id{once, twice}, modernGateTestSettings(true), false), []server.Id{twice})
}
