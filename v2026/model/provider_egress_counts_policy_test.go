package model

import (
	"testing"
	"time"
)

func TestProviderEgressCountsPolicyKeySeparatesCountRules(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	minimums := map[int]float64{1: .95, 2: .7, 3: .6}
	base := providerEgressCountsPolicyKey(settings, false, minimums, 2)
	for name, change := range map[string]func(*EgressIndexSettings){
		"index":       func(s *EgressIndexSettings) { s.MaxFailureIndex++ },
		"evidence":    func(s *EgressIndexSettings) { s.EvidenceMaxAge -= time.Minute },
		"numerator":   func(s *EgressIndexSettings) { s.QualityOkNumerator-- },
		"denominator": func(s *EgressIndexSettings) { s.QualityOkDenominator++ },
		"country":     func(s *EgressIndexSettings) { s.CountryGate = !s.CountryGate },
	} {
		t.Run(name, func(t *testing.T) {
			changed := *settings
			change(&changed)
			if providerEgressCountsPolicyKey(&changed, false, minimums, 2) == base {
				t.Fatal("different count policy reused the snapshot key")
			}
		})
	}
	if providerEgressCountsPolicyKey(settings, true, minimums, 2) == base ||
		providerEgressCountsPolicyKey(settings, false, minimums, -1) == base ||
		providerEgressCountsPolicyKey(settings, false, map[int]float64{1: .8, 2: .7, 3: .6}, 2) == base {
		t.Fatal("resolved enable, probe-policy error, or reliability change reused the snapshot key")
	}
}

func TestProviderEgressCountsPolicyKeyIgnoresUnrelatedRequestSettings(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	minimums := map[int]float64{1: .95, 2: .7, 3: .6}
	base := providerEgressCountsPolicyKey(settings, false, minimums, 2)
	settings.NativeReaderEnabled = !settings.NativeReaderEnabled
	settings.RequestSettingsMaxAge += time.Minute
	settings.BackfillTierOffset++
	settings.MinScoredLoads++
	settings.ClassWeights = map[string]int{"synthetic": 100}
	settings.DefaultClassWeight++
	settings.EvidenceMaxAge += time.Hour // Already capped by the accepted-health maximum.
	if providerEgressCountsPolicyKey(settings, false, minimums, 2) != base {
		t.Fatal("request-only settings or ineffective evidence age split the aggregate cache")
	}
}
