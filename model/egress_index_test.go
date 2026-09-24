package model

import (
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
)

// The egress index of connect/GEOMAP.md §10.3 and the rules that decide the
// buckets, pure: no database, no cache.

// A run of `total` scored loads, ten in each class that classFailures names
// with that many of them failed, and the rest of the loads passing in "site".
func egressTestRun(measuredAt time.Time, total int, classFailures map[string]int) *EgressHealthRun {
	classResults := map[string]ProviderEgressHealthClassResult{}
	loads := 0
	failures := 0
	for class, failed := range classFailures {
		classResults[class] = ProviderEgressHealthClassResult{
			OK:    10 - failed,
			Total: 10,
		}
		loads += 10
		failures += failed
	}
	site := classResults["site"]
	site.OK += total - loads
	site.Total += total - loads
	classResults["site"] = site
	return &EgressHealthRun{
		MeasuredAt:   measuredAt,
		OkCount:      total - failures,
		Total:        total,
		ClassResults: classResults,
	}
}

// Every failed load of a class costs the class's weight, and the sum is held
// at the cap.
func TestEgressIndexCountsFailedLoadsPerClass(t *testing.T) {
	now := server.NowUtc()
	settings := DefaultEgressIndexSettings()

	run := egressTestRun(now, 60, map[string]int{
		"dns":          0,
		"connectivity": 1,
		"cdn":          2,
	})
	index := ComputeEgressIndex(run, now, settings)
	assert.Equal(t, index.Evidence, true)
	assert.Equal(t, index.Index, 3)
	assert.Equal(t, index.Quality, true)
	assert.Equal(t, *index.QualityVerdict(), true)

	// the weights are per class: a cdn failure made to cost two
	settings.ClassWeights["cdn"] = 2
	assert.Equal(t, ComputeEgressIndex(run, now, settings).Index, 1+2*2)
	// ... and at three the sum, seven, is held at the cap of six
	settings.ClassWeights["cdn"] = 3
	assert.Equal(t, ComputeEgressIndex(run, now, settings).Index, settings.MaxFailureIndex)
}

// Each of the four classes reads its own weight.
func TestEgressIndexWeightsEveryClass(t *testing.T) {
	now := server.NowUtc()
	for _, class := range []string{"dns", "connectivity", "cdn", "site"} {
		settings := DefaultEgressIndexSettings()
		settings.ClassWeights[class] = 2
		run := egressTestRun(now, 60, map[string]int{class: 1})
		index := ComputeEgressIndex(run, now, settings)
		if index.Index != 2 {
			t.Errorf("one %s failure at weight 2 is index %d, want 2", class, index.Index)
		}
	}
}

// A broken run is held at MaxFailureIndex, which is a setting.
func TestEgressIndexCapsTheFailures(t *testing.T) {
	now := server.NowUtc()
	settings := DefaultEgressIndexSettings()

	run := egressTestRun(now, 80, map[string]int{
		"dns":          5,
		"connectivity": 5,
		"cdn":          5,
	})
	index := ComputeEgressIndex(run, now, settings)
	assert.Equal(t, index.Index, settings.MaxFailureIndex)
	assert.Equal(t, index.Index, 6)
	// 15 of 80 failed: over the one-in-ten line
	assert.Equal(t, index.Quality, false)

	settings.MaxFailureIndex = 10
	assert.Equal(t, ComputeEgressIndex(run, now, settings).Index, 10)
}

// A failure the class tally does not account for, and a class the weights do
// not name, both pay DefaultClassWeight.
func TestEgressIndexUnattributedFailuresPayTheDefaultWeight(t *testing.T) {
	now := server.NowUtc()
	settings := DefaultEgressIndexSettings()

	// a run with no class breakdown still pays for every failed load
	run := &EgressHealthRun{
		MeasuredAt:   now,
		OkCount:      127,
		Total:        131,
		ClassResults: map[string]ProviderEgressHealthClassResult{},
	}
	assert.Equal(t, ComputeEgressIndex(run, now, settings).Index, 4)

	// a class the weights do not name is scored at the default weight
	run = &EgressHealthRun{
		MeasuredAt: now,
		OkCount:    58,
		Total:      60,
		ClassResults: map[string]ProviderEgressHealthClassResult{
			"video": {OK: 8, Total: 10},
			"site":  {OK: 50, Total: 50},
		},
	}
	assert.Equal(t, ComputeEgressIndex(run, now, settings).Index, 2)
	settings.DefaultClassWeight = 2
	assert.Equal(t, ComputeEgressIndex(run, now, settings).Index, 4)
}

// Without evidence -- no run, a run older than EvidenceMaxAge, or one of fewer
// than MinScoredLoads loads -- the index is 0 and there is no verdict: such a
// provider is not ordered by an index, it is in the online bucket.
func TestEgressIndexWithoutEvidence(t *testing.T) {
	now := server.NowUtc()
	settings := DefaultEgressIndexSettings()

	// no run at all
	missing := ComputeEgressIndex(nil, now, settings)
	assert.Equal(t, missing.Evidence, false)
	assert.Equal(t, missing.Index, 0)
	if missing.QualityVerdict() != nil {
		t.Fatal("a provider with no run has a quality verdict")
	}

	// fewer scored loads than a run needs to be evidence, however it went
	short := egressTestRun(now, settings.MinScoredLoads-1, map[string]int{"cdn": 5})
	assert.Equal(t, ComputeEgressIndex(short, now, settings), EgressIndex{})
	enough := egressTestRun(now, settings.MinScoredLoads, map[string]int{})
	assert.Equal(t, ComputeEgressIndex(enough, now, settings).Evidence, true)
	assert.Equal(t, ComputeEgressIndex(enough, now, settings).Index, 0)
	assert.Equal(t, *ComputeEgressIndex(enough, now, settings).QualityVerdict(), true)

	// stale: past EvidenceMaxAge the run is no evidence, at it still is
	stale := egressTestRun(now.Add(-settings.EvidenceMaxAge-time.Minute), 60, map[string]int{"cdn": 1})
	assert.Equal(t, ComputeEgressIndex(stale, now, settings), EgressIndex{})
	boundary := egressTestRun(now.Add(-settings.EvidenceMaxAge), 60, map[string]int{"cdn": 1})
	assert.Equal(t, ComputeEgressIndex(boundary, now, settings).Evidence, true)
	assert.Equal(t, ComputeEgressIndex(boundary, now, settings).Index, 1)
}

// The 90 % rule over every scored load, exact at the line.
func TestEgressQualityVerdict(t *testing.T) {
	now := server.NowUtc()
	settings := DefaultEgressIndexSettings()

	atLine := &EgressHealthRun{MeasuredAt: now, OkCount: 45, Total: 50}
	belowLine := &EgressHealthRun{MeasuredAt: now, OkCount: 44, Total: 50}
	assert.Equal(t, ComputeEgressIndex(atLine, now, settings).Quality, true)
	assert.Equal(t, ComputeEgressIndex(belowLine, now, settings).Quality, false)
	assert.Equal(t, *ComputeEgressIndex(belowLine, now, settings).QualityVerdict(), false)

	// the ratio is a setting
	settings.QualityOkNumerator = 8
	assert.Equal(t, ComputeEgressIndex(belowLine, now, settings).Quality, true)
}

// The defaults of GEOMAP §10.3, and the backfill offset's derivation from the
// tiers and the client's largest demerit.
func TestEgressIndexSettingsDefaults(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	assert.Equal(t, settings.ClassWeights, map[string]int{"dns": 1, "connectivity": 1, "cdn": 1, "site": 1})
	assert.Equal(t, settings.DefaultClassWeight, 1)
	assert.Equal(t, settings.MaxFailureIndex, 6)
	assert.Equal(t, settings.EvidenceMaxAge, ProviderEgressLocationMaxAge)
	assert.Equal(t, settings.EvidenceMaxAge, 7*24*time.Hour)
	assert.Equal(t, settings.QualityOkNumerator, 9)
	assert.Equal(t, settings.QualityOkDenominator, 10)
	assert.Equal(t, settings.MinScoredLoads, 50)
	assert.Equal(t, settings.CountryGate, true)
	// past the client's largest demerit at both edges of the borrowed band
	assert.Equal(t, MaxNativeClientScoreTier, MaxClientScore/ClientScorePerTier)
	assert.Equal(t, MaxClientTierDemerit, 7)
	assert.Equal(t, settings.BackfillTierOffset, ClientScoreCutoffTier+1+MaxClientTierDemerit)
	assert.Equal(t, settings.BackfillTierOffset, 11)
	// a native demerited as far as the client goes ranks ahead of every
	// borrowed provider
	assert.Equal(t, MaxNativeClientScoreTier+MaxClientTierDemerit < settings.BackfillTierOffset, true)
	// and a provider borrowed past its cutoffs, demerited as far, ahead of
	// every online one
	assert.Equal(t, ClientScoreCutoffTier+settings.BackfillTierOffset+MaxClientTierDemerit < 2*settings.BackfillTierOffset, true)
	assert.Equal(t, settings.MaxIndex(), 6)
	assert.Equal(t, settings.RequestSettingsMaxAge, time.Minute)
}

// provider.yml overrides every setting it names, keeps the default of every
// value out of range, and falls back to the defaults when unreadable.
func TestEgressIndexSettingsOverrides(t *testing.T) {
	load := func(config string) *EgressIndexSettings {
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
		defer pop()
		return egressIndexSettings()
	}

	overridden := load(`
enable_egress_test: true
egress_index:
  class_weights:
    dns: 3
    video: 2
  default_class_weight: 2
  max_failure_index: 9
  evidence_max_age: 72h
  quality_ok_numerator: 4
  quality_ok_denominator: 5
  min_scored_loads: 20
  country_gate: false
  backfill_tier_offset: 5
`)
	assert.Equal(t, overridden.ClassWeights, map[string]int{"dns": 3, "connectivity": 1, "cdn": 1, "site": 1, "video": 2})
	assert.Equal(t, overridden.DefaultClassWeight, 2)
	assert.Equal(t, overridden.MaxFailureIndex, 9)
	assert.Equal(t, overridden.EvidenceMaxAge, 72*time.Hour)
	assert.Equal(t, overridden.QualityOkNumerator, 4)
	assert.Equal(t, overridden.QualityOkDenominator, 5)
	assert.Equal(t, overridden.MinScoredLoads, 20)
	assert.Equal(t, overridden.CountryGate, false)
	assert.Equal(t, overridden.BackfillTierOffset, 5)
	assert.Equal(t, overridden.MaxIndex(), 9)

	// out of range: each keeps its default
	invalid := load(`
egress_index:
  class_weights:
    dns: -1
  max_failure_index: -2
  min_scored_loads: 0
  evidence_max_age: -1h
  quality_ok_numerator: 11
  quality_ok_denominator: 10
  backfill_tier_offset: 2
`)
	defaults := DefaultEgressIndexSettings()
	assert.Equal(t, invalid.ClassWeights, defaults.ClassWeights)
	assert.Equal(t, invalid.MaxFailureIndex, defaults.MaxFailureIndex)
	assert.Equal(t, invalid.MinScoredLoads, defaults.MinScoredLoads)
	assert.Equal(t, invalid.EvidenceMaxAge, defaults.EvidenceMaxAge)
	assert.Equal(t, invalid.QualityOkNumerator, defaults.QualityOkNumerator)
	assert.Equal(t, invalid.QualityOkDenominator, defaults.QualityOkDenominator)
	// an offset below one past the highest native tier would let a borrowed
	// provider tie with a native one
	assert.Equal(t, invalid.BackfillTierOffset, defaults.BackfillTierOffset)
	// and that is the floor, not the default
	assert.Equal(t, load("egress_index:\n  backfill_tier_offset: 3\n").BackfillTierOffset, MaxNativeClientScoreTier+1)

	assert.Equal(t, load("egress_index:\n  evidence_max_age: soon\n").EvidenceMaxAge, defaults.EvidenceMaxAge)

	// an empty file, no block, and an unreadable file are the defaults
	assert.Equal(t, load(""), defaults)
	assert.Equal(t, load("enable_egress_test: true\n"), defaults)
	assert.Equal(t, load("egress_index: [unterminated\n"), defaults)
}

// The evidence time is the newer of the two runs, whichever is present.
func TestEgressEvidenceTimeIsTheNewerRun(t *testing.T) {
	now := server.NowUtc()
	older := now.Add(-2 * time.Hour)

	if egressEvidenceTime(nil, nil) != nil {
		t.Fatal("a provider never probed has an evidence time")
	}
	run := &EgressHealthRun{MeasuredAt: older}
	assert.Equal(t, *egressEvidenceTime(run, nil), older)
	assert.Equal(t, *egressEvidenceTime(nil, &now), now)
	assert.Equal(t, *egressEvidenceTime(run, &now), now)
	assert.Equal(t, *egressEvidenceTime(&EgressHealthRun{MeasuredAt: now}, &older), now)
}

// Every combination of evidence the rules read, in both flag states.
func TestDecideProviderEgress(t *testing.T) {
	index := func(value int) *int {
		return &value
	}
	verdict := func(value bool) *bool {
		return &value
	}

	// a decision's observable fields, compared whole
	type want struct {
		reason       string
		hardExcluded bool
		quality      bool
		speed        bool
		online       bool
		counted      bool
	}
	tests := []struct {
		name  string
		facts providerEgressFacts
		// the decision with the flag off and on
		off want
		on  want
	}{
		{
			name:  "blackhole wins over everything",
			facts: providerEgressFacts{blackholed: true, tlsAuthenticationFailed: true, countryMismatch: true, egressIndex: index(0), egressQuality: verdict(true)},
			off:   want{reason: ProviderExcludedBlackhole, hardExcluded: true},
			on:    want{reason: ProviderExcludedBlackhole, hardExcluded: true},
		},
		{
			name:  "tls",
			facts: providerEgressFacts{tlsAuthenticationFailed: true, egressIndex: index(0), egressQuality: verdict(true)},
			off:   want{reason: ProviderExcludedTls, hardExcluded: true},
			on:    want{reason: ProviderExcludedTls, hardExcluded: true},
		},
		{
			name:  "country gate, a minimum on every bucket and the counts",
			facts: providerEgressFacts{countryMismatch: true, egressIndex: index(0), egressQuality: verdict(true)},
			off:   want{reason: ProviderExcludedCountry},
			on:    want{reason: ProviderExcludedCountry},
		},
		{
			name:  "probed and passing",
			facts: providerEgressFacts{egressIndex: index(1), egressQuality: verdict(true)},
			off:   want{quality: true, speed: true, counted: true},
			on:    want{quality: true, speed: true, counted: true},
		},
		{
			name:  "probed and over the one-in-ten line: speed only, counted",
			facts: providerEgressFacts{egressIndex: index(6), egressQuality: verdict(false)},
			off:   want{reason: ProviderExcludedHealth, speed: true, counted: true},
			on:    want{reason: ProviderExcludedHealth, speed: true, counted: true},
		},
		{
			name:  "unprobed: online and counted, whatever the flag",
			facts: providerEgressFacts{egressIndex: index(0)},
			off:   want{reason: ProviderExcludedUnprobed, online: true, counted: true},
			on:    want{reason: ProviderExcludedUnprobed, online: true, counted: true},
		},
		{
			name:  "unprobed but mislocated: the gate holds the online bucket too",
			facts: providerEgressFacts{countryMismatch: true, egressIndex: index(0)},
			off:   want{reason: ProviderExcludedCountry},
			on:    want{reason: ProviderExcludedCountry},
		},
		{
			name:  "row before the index, healthy and located: the old rules pass it",
			facts: providerEgressFacts{legacyHealthPasses: true, legacyHealthMeasured: true, legacyCounted: true},
			off:   want{quality: true, speed: true, counted: true},
			on:    want{quality: true, speed: true, counted: true},
		},
		{
			name:  "row before the index, measured unhealthy: the old rules gate both buckets on the flag",
			facts: providerEgressFacts{legacyHealthMeasured: true},
			off:   want{quality: true, speed: true, counted: true},
			on:    want{reason: ProviderExcludedHealth},
		},
		{
			name:  "row before the index, never measured: fails closed under the flag, and is never online",
			facts: providerEgressFacts{},
			off:   want{quality: true, speed: true, counted: true},
			on:    want{reason: ProviderExcludedUnprobed},
		},
		{
			name:  "row before the index, healthy but not located: in the buckets, out of the old count",
			facts: providerEgressFacts{legacyHealthPasses: true, legacyHealthMeasured: true},
			off:   want{quality: true, speed: true, counted: true},
			on:    want{quality: true, speed: true},
		},
	}
	for _, test := range tests {
		for _, flag := range []bool{false, true} {
			expected := test.off
			if flag {
				expected = test.on
			}
			facts := test.facts
			decision := decideProviderEgress(&facts, flag)
			got := want{
				reason:       decision.reason,
				hardExcluded: decision.hardExcluded,
				quality:      decision.quality,
				speed:        decision.speed,
				online:       decision.online,
				counted:      decision.counted,
			}
			if got != expected {
				t.Errorf("%s, flag %t: %+v, want %+v", test.name, flag, got, expected)
			}
		}
	}
}

// Each rank mode borrows from the other one, and no other mode borrows.
func TestBackfillRankMode(t *testing.T) {
	other, ok := backfillRankMode(RankModeQuality)
	assert.Equal(t, other, RankModeSpeed)
	assert.Equal(t, ok, true)
	other, ok = backfillRankMode(RankModeSpeed)
	assert.Equal(t, other, RankModeQuality)
	assert.Equal(t, ok, true)
	_, ok = backfillRankMode("fastest")
	assert.Equal(t, ok, false)
}

// The tiers of the modes: a native within its cutoffs carries at most
// MaxNativeClientScoreTier, a provider past them ClientScoreCutoffTier.
func TestClientScoreTierBounds(t *testing.T) {
	assert.Equal(t, ClientScorePerTier, 20)
	assert.Equal(t, MaxNativeClientScoreTier, 2)
	assert.Equal(t, ClientScoreCutoffTier, 3)
	assert.Equal(t, MaxClientScore/ClientScorePerTier <= MaxNativeClientScoreTier, true)
	assert.Equal(t, MaxNativeClientScoreTier < ClientScoreCutoffTier, true)
}
