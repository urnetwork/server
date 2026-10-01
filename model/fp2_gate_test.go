// Public buckets share common gates and differ only in probe and ARIN evidence.
package model

import (
	"testing"
	"time"
)

// A missing, stale or ordinarily failing probe preserves online supply in both rollout states.
func TestFp2ClarifiedProbeGates(t *testing.T) {
	index := 0
	passing, failing := true, false
	cases := []struct {
		name    string
		facts   providerEgressFacts
		quality bool
		speed   bool
	}{
		{name: "legacy unprobed"},
		{name: "unprobed", facts: providerEgressFacts{egressIndex: &index}},
		{name: "ordinary failing", facts: providerEgressFacts{egressIndex: &index, egressQuality: &failing}},
		{name: "passing", facts: providerEgressFacts{egressIndex: &index, egressQuality: &passing}, quality: true, speed: true},
		{name: "cheap blackhole", facts: providerEgressFacts{blackholed: true}},
		{name: "probe country observation", facts: providerEgressFacts{countryMismatch: true, egressIndex: &index, egressQuality: &passing}, quality: true, speed: true},
	}
	for _, c := range cases {
		for _, flag := range []bool{false, true} {
			decision := decideProviderEgress(&c.facts, flag)
			if decision.hardExcluded || decision.quality != c.quality || decision.speed != c.speed || !decision.online || !decision.counted {
				t.Errorf("%s flag=%t: got %+v; want quality=%t speed=%t online=counted=true", c.name, flag, decision, c.quality, c.speed)
			}
		}
	}
}

// At precisely eight hours a passing run no longer qualifies for either probed bucket.
func TestFp2QualityEvidenceEightHourBoundary(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	settings := DefaultEgressIndexSettings()
	for _, age := range []time.Duration{8*time.Hour - time.Nanosecond, 8 * time.Hour, 8*time.Hour + time.Nanosecond} {
		index := ComputeEgressIndex(&EgressHealthRun{MeasuredAt: now.Add(-age), OkCount: 100, Total: 100}, now, settings)
		if index.Evidence != (age < 8*time.Hour) {
			t.Errorf("age=%s evidence=%t, want %t", age, index.Evidence, age < 8*time.Hour)
		}
	}
}

// The indexer owns the success ratio; small samples are valid and zero is unknown.
func TestFp2UrlSuccessRatio(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		ok, total         int
		evidence, quality bool
	}{
		{ok: 0, total: 0},
		{ok: 1, total: 1, evidence: true, quality: true},
		{ok: 4, total: 5, evidence: true, quality: true},
		{ok: 79, total: 100, evidence: true},
		{ok: 3, total: 5, evidence: true},
		{ok: 2, total: 3, evidence: true},
		{ok: 1, total: 2, evidence: true},
		{ok: 0, total: 3, evidence: true},
	} {
		index := ComputeEgressIndex(&EgressHealthRun{MeasuredAt: now, OkCount: c.ok, Total: c.total}, now, DefaultEgressIndexSettings())
		if index.Evidence != c.evidence || index.Quality != c.quality {
			t.Errorf("%d/%d: got %+v, want evidence=%t quality=%t", c.ok, c.total, index, c.evidence, c.quality)
		}
		decision := decideProviderEgress(&providerEgressFacts{egressQuality: index.QualityVerdict()}, false)
		if decision.quality != c.quality || decision.speed != c.quality || !decision.online || !decision.counted || decision.hardExcluded {
			t.Errorf("%d/%d: shared ratio gate changed bucket membership: %+v", c.ok, c.total, decision)
		}
	}
}

// Explicit ARIN exceptions and observed reliability failures are the shared gates.
func TestFp2CommonAndArinQualityGates(t *testing.T) {
	passing := true
	for _, c := range []struct {
		facts                  providerEgressFacts
		quality, speed, online bool
	}{
		{facts: providerEgressFacts{arinNonQuality: true, egressQuality: &passing}, speed: true, online: true},
		{facts: providerEgressFacts{arinRisk: true, egressQuality: &passing}},
		{facts: providerEgressFacts{reliabilityFailed: true, egressQuality: &passing}},
		{facts: providerEgressFacts{tlsAuthenticationFailed: true, egressQuality: &passing}},
	} {
		decision := decideProviderEgress(&c.facts, false)
		if decision.quality != c.quality || decision.speed != c.speed || decision.online != c.online || decision.counted != c.online {
			t.Errorf("facts=%+v decision=%+v", c.facts, decision)
		}
	}
}

// More accepted successes improve ranking without turning the threshold into zero weight.
func TestFp2UrlProbeSuccessImprovesRanking(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	previousWeight := 0.0
	previousIndex := 100
	for _, okCount := range []int{60, 70, 80, 90, 100} {
		weight := providerUrlProbeSuccessWeight(ProviderEgressHealthCounts{OKCount: okCount, Total: 100})
		index := ComputeEgressIndex(&EgressHealthRun{MeasuredAt: now, OkCount: okCount, Total: 100}, now, DefaultEgressIndexSettings())
		if weight <= previousWeight || index.Index > previousIndex || index.Quality != (80 <= okCount) {
			t.Fatalf("success=%d/100 weight=%f index=%+v", okCount, weight, index)
		}
		previousWeight, previousIndex = weight, index.Index
	}
	if providerUrlProbeSuccessWeight(ProviderEgressHealthCounts{}) != 1 {
		t.Fatal("unmeasured online provider lost its neutral weight")
	}
	if providerUrlProbeSuccessWeight(ProviderEgressHealthCounts{Total: 3}) <= 0 {
		t.Fatal("failed measured provider became unavailable online")
	}
}
