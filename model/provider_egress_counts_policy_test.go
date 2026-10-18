package model

import (
	"testing"
	"time"
)

// A fixed instant before the modern provider gate's default start, so no key
// below reads the clock.
var countsPolicyTestNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// Every field of the policy changes the key: each count rule below, the
// resolved enable, the probe policy, the reliability minimums, the modern
// gate's decision at the key's instant, the Online URL ratio and its country
// exemption (schema 4), and the published ARIN serving epoch (schema 5).
func TestProviderEgressCountsPolicyKeySeparatesCountRules(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	minimums := map[int]float64{1: .95, 2: .7, 3: .6}
	base := providerEgressCountsPolicyKey(settings, false, minimums, 2, countsPolicyTestNow, 0)
	for name, change := range map[string]func(*EgressIndexSettings){
		"index":       func(s *EgressIndexSettings) { s.MaxFailureIndex++ },
		"evidence":    func(s *EgressIndexSettings) { s.EvidenceMaxAge -= time.Minute },
		"numerator":   func(s *EgressIndexSettings) { s.QualityOkNumerator-- },
		"denominator": func(s *EgressIndexSettings) { s.QualityOkDenominator++ },
		"country":     func(s *EgressIndexSettings) { s.CountryGate = !s.CountryGate },
		"url success": func(s *EgressIndexSettings) { s.OnlineRequiresUrlSuccess = !s.OnlineRequiresUrlSuccess },
		"floor":       func(s *EgressIndexSettings) { s.OnlineSuccessCountryFloor += 0.01 },
		"margin":      func(s *EgressIndexSettings) { s.OnlineSuccessCountryFloorMargin += 0.01 },
		"modern": func(s *EgressIndexSettings) {
			on := true
			s.QualitySpeedRequiresModernProvider = &on
		},
		"modern start": func(s *EgressIndexSettings) { s.QualitySpeedModernProviderStart = countsPolicyTestNow },
		"url ratio":    func(s *EgressIndexSettings) { s.OnlineUrlSuccessRatio += 0.1 },
		"url ratio exemption": func(s *EgressIndexSettings) {
			s.OnlineUrlRatioCountryExemption = !s.OnlineUrlRatioCountryExemption
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := *settings
			change(&changed)
			if providerEgressCountsPolicyKey(&changed, false, minimums, 2, countsPolicyTestNow, 0) == base {
				t.Fatal("different count policy reused the snapshot key")
			}
		})
	}
	if providerEgressCountsPolicyKey(settings, true, minimums, 2, countsPolicyTestNow, 0) == base ||
		providerEgressCountsPolicyKey(settings, false, minimums, -1, countsPolicyTestNow, 0) == base ||
		providerEgressCountsPolicyKey(settings, false, map[int]float64{1: .8, 2: .7, 3: .6}, 2, countsPolicyTestNow, 0) == base {
		t.Fatal("resolved enable, probe-policy error, or reliability change reused the snapshot key")
	}
	// Quality counts follow the published ARIN serving epoch
	if providerEgressCountsPolicyKey(settings, false, minimums, 2, countsPolicyTestNow, 1700000000) == base ||
		providerEgressCountsPolicyKey(settings, false, minimums, 2, countsPolicyTestNow, 1700000000) == providerEgressCountsPolicyKey(settings, false, minimums, 2, countsPolicyTestNow, 1700086400) {
		t.Fatal("a different ARIN serving epoch reused the snapshot key")
	}
}

func TestProviderEgressCountsPolicyKeyIgnoresUnrelatedRequestSettings(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	minimums := map[int]float64{1: .95, 2: .7, 3: .6}
	base := providerEgressCountsPolicyKey(settings, false, minimums, 2, countsPolicyTestNow, 0)
	settings.NativeReaderEnabled = !settings.NativeReaderEnabled
	settings.RequestSettingsMaxAge += time.Minute
	settings.BackfillTierOffset++
	settings.MinScoredLoads++
	settings.ClassWeights = map[string]int{"synthetic": 100}
	settings.DefaultClassWeight++
	settings.EvidenceMaxAge += time.Hour // Already capped by the accepted-health maximum.
	// a start moved without passing the key's instant decides alike
	settings.QualitySpeedModernProviderStart = settings.QualitySpeedModernProviderStart.Add(time.Hour)
	if providerEgressCountsPolicyKey(settings, false, minimums, 2, countsPolicyTestNow, 0) != base {
		t.Fatal("request-only settings or ineffective evidence age split the aggregate cache")
	}
	// the gate is keyed by its decision: an explicit false before the start
	// decides as the absent switch does, so the workers share the snapshot
	off := false
	settings.QualitySpeedRequiresModernProvider = &off
	if providerEgressCountsPolicyKey(settings, false, minimums, 2, countsPolicyTestNow, 0) != base {
		t.Fatal("an explicit false that decides as the absent switch split the aggregate cache")
	}
}

// The dashboard snapshots flip at the modern provider gate's start: with the
// switch absent the key a second before the start differs from the key at
// it, and stays put on either side, so a snapshot counted before the start
// is never read after it. An explicit switch keeps one key across the start.
func TestProviderEgressCountsPolicyKeyFlipsAtTheModernProviderGateStart(t *testing.T) {
	minimums := map[int]float64{1: .95, 2: .7, 3: .6}
	settings := DefaultEgressIndexSettings()
	start := settings.QualitySpeedModernProviderStart
	key := func(settings *EgressIndexSettings, now time.Time) string {
		return providerEgressCountsPolicyKey(settings, false, minimums, 2, now, 0)
	}
	if key(settings, start.Add(-time.Second)) == key(settings, start) {
		t.Fatal("the key a second before the gate's start is the key at it")
	}
	if key(settings, start.Add(-2*time.Second)) != key(settings, start.Add(-time.Second)) || key(settings, start) != key(settings, start.Add(time.Hour)) {
		t.Fatal("the key changed away from the gate's start")
	}
	for _, value := range []bool{false, true} {
		explicit := DefaultEgressIndexSettings()
		explicit.QualitySpeedRequiresModernProvider = &value
		if key(explicit, start.Add(-time.Second)) != key(explicit, start) {
			t.Fatalf("an explicit %t switch changed the key at the start", value)
		}
	}
}

// The native URL-run minimums are code, not settings, held in a var
// (providerNativeMinRuns) beside the policy: a worker built with other
// minimums, or none, counts the buckets differently, so each minimum, and
// their absence, changes the key, and the same minimums give the same key.
func TestProviderEgressCountsPolicyKeySeparatesNativeMinRuns(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	minimums := map[int]float64{1: .95, 2: .7, 3: .6}
	key := func() string {
		return providerEgressCountsPolicyKey(settings, false, minimums, 2, countsPolicyTestNow, 0)
	}
	original := providerNativeMinRuns
	t.Cleanup(func() { providerNativeMinRuns = original })
	base := key()
	for _, c := range []struct {
		name   string
		change func(*providerNativeMinRunsPolicy)
	}{
		{name: "quality runs", change: func(p *providerNativeMinRunsPolicy) { p.QualityMinMeasuredUrlRuns++ }},
		{name: "quality span", change: func(p *providerNativeMinRunsPolicy) { p.QualityMinMeasuredUrlSpan += time.Minute }},
		{name: "speed runs", change: func(p *providerNativeMinRunsPolicy) { p.SpeedMinMeasuredUrlRuns++ }},
		{name: "no minimums", change: func(p *providerNativeMinRunsPolicy) { *p = providerNativeMinRunsPolicy{} }},
	} {
		providerNativeMinRuns = original
		c.change(&providerNativeMinRuns)
		if key() == base {
			t.Errorf("%s: other native URL-run minimums reused the snapshot key", c.name)
		}
	}
	providerNativeMinRuns = original
	if key() != base {
		t.Fatal("the same native URL-run minimums changed the snapshot key")
	}
}
