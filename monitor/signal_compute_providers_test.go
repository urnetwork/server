package monitor

// SIGNALS.md §32.2 (CSRV-M12-06): each provider page at its line and not below it, each provider its own target.

import (
	"testing"
)

// A canary gap of 2.1% pages its provider, and 1.9% does not page the other.
func TestComputeCanaryGapPagesPastTwoPercent(t *testing.T) {
	findings := computeFamilyJudge(computeProvidersFamily(), computeTestSettings("providers"), []*sidecarStorageSample{
		computeTestLive(),
		computeTestSample("compute-canary-gap", "value", 0.021, map[string]string{"provider": "provider-a"}),
		computeTestSample("compute-canary-gap", "value", 0.019, map[string]string{"provider": "provider-b"}),
	})
	gaps := computeTestFindings(findings, "compute-canary-gap")
	if gaps["provider-a"].healthy || !gaps["provider-b"].healthy {
		t.Errorf("gaps: %+v", gaps)
	}
}

// A provider failing every call for the window pages, and one with a call answered does not; one with no call is not
// judged failing.
func TestComputeProviderFailingEveryCallPages(t *testing.T) {
	findings := computeFamilyJudge(computeProvidersFamily(), computeTestSettings("providers"), []*sidecarStorageSample{
		computeTestLive(),
		computeTestSample("compute-provider-failing", "calls", 40, map[string]string{"provider": "provider-a"}),
		computeTestSample("compute-provider-failing", "ok", 0, map[string]string{"provider": "provider-a"}),
		computeTestSample("compute-provider-failing", "calls", 40, map[string]string{"provider": "provider-b"}),
		computeTestSample("compute-provider-failing", "ok", 1, map[string]string{"provider": "provider-b"}),
		computeTestSample("compute-provider-failing", "calls", 0, map[string]string{"provider": "provider-c"}),
	})
	failing := computeTestFindings(findings, "compute-provider-failing")
	if failing["provider-a"].healthy || !failing["provider-b"].healthy || !failing["provider-c"].healthy {
		t.Errorf("failing: %+v", failing)
	}
}

// Attestation failures page past 5%; disputes page past 0.5% only from 3 networks; a balance under a day pages.
func TestComputeProviderSharesPageAtTheirLines(t *testing.T) {
	findings := computeFamilyJudge(computeProvidersFamily(), computeTestSettings("providers"), []*sidecarStorageSample{
		computeTestLive(),
		computeTestSample("compute-attestation-failures", "all", 100, map[string]string{"provider": "provider-a"}),
		computeTestSample("compute-attestation-failures", "failed", 6, map[string]string{"provider": "provider-a"}),
		computeTestSample("compute-attestation-failures", "all", 100, map[string]string{"provider": "provider-b"}),
		computeTestSample("compute-attestation-failures", "failed", 4, map[string]string{"provider": "provider-b"}),
		computeTestSample("compute-dispute-rate", "calls", 1000, map[string]string{"provider": "provider-a"}),
		computeTestSample("compute-dispute-rate", "disputes", 6, map[string]string{"provider": "provider-a"}),
		computeTestSample("compute-dispute-rate", "networks", 3, map[string]string{"provider": "provider-a"}),
		computeTestSample("compute-dispute-rate", "calls", 1000, map[string]string{"provider": "provider-b"}),
		computeTestSample("compute-dispute-rate", "disputes", 6, map[string]string{"provider": "provider-b"}),
		computeTestSample("compute-dispute-rate", "networks", 2, map[string]string{"provider": "provider-b"}),
		computeTestSample("compute-provider-balance-low", "value", 0.9, map[string]string{"provider": "provider-a"}),
		computeTestSample("compute-provider-balance-low", "value", 1.1, map[string]string{"provider": "provider-b"}),
	})
	attestation := computeTestFindings(findings, "compute-attestation-failures")
	disputes := computeTestFindings(findings, "compute-dispute-rate")
	balance := computeTestFindings(findings, "compute-provider-balance-low")
	if attestation["provider-a"].healthy || !attestation["provider-b"].healthy {
		t.Errorf("attestation: %+v", attestation)
	}
	if disputes["provider-a"].healthy || !disputes["provider-b"].healthy {
		t.Errorf("disputes: %+v", disputes)
	}
	if balance["provider-a"].healthy || !balance["provider-b"].healthy {
		t.Errorf("balance: %+v", balance)
	}
}
