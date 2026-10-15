package controller

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	testStatsArindbOldEpoch    = int64(1700000000)
	testStatsArindbLatestEpoch = int64(1700086400)
)

var (
	testStatsArindbLatestHash = "aa00000000000000000000000000000000000000000000000000000000000000"
	testStatsArindbOldHash    = "bb00000000000000000000000000000000000000000000000000000000000000"
	testStatsArindbDecidedAt  = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
)

// A published decision in state, as the indexer writes it.
func testStatsArindbDecision(state string, servingEpoch int64) *model.ArindbCanaryDecision {
	decision := &model.ArindbCanaryDecision{
		Version:        model.ArindbCanaryDecisionVersion,
		DecisionId:     server.NewId().String(),
		State:          state,
		ServingEpoch:   servingEpoch,
		Latest:         model.ArindbCanaryDatabase{State: server.ArinDatabaseLoaded, BuildEpoch: testStatsArindbLatestEpoch, Sha256: testStatsArindbLatestHash},
		Old:            model.ArindbCanaryDatabase{State: server.ArinDatabaseLoaded, BuildEpoch: testStatsArindbOldEpoch, Sha256: testStatsArindbOldHash},
		Coverage:       model.ArindbCanaryCoverage{Providers: 200, Latest: 200, Old: 196, Paired: 196},
		Cohort:         model.ArindbCanaryCohort{ReferenceProviders: 250, LossRatio: 0.2},
		PairSince:      testStatsArindbDecidedAt.Add(-time.Hour),
		Publisher:      model.ArindbCanaryPublisher(),
		ServingChanges: []time.Time{testStatsArindbDecidedAt.Add(-time.Hour), testStatsArindbDecidedAt},
		Buckets:        map[string]*model.ArindbCanaryBucket{},
		DecidedAt:      testStatsArindbDecidedAt,
	}
	for _, bucket := range model.ProviderEgressBuckets {
		decision.Buckets[bucket] = &model.ArindbCanaryBucket{
			ArindbCanaryCount: model.ArindbCanaryCount{Latest: 90, Old: 100},
			LossRatio:         0.1,
			ChurnRatio:        0.3,
			Countries:         map[string]model.ArindbCanaryCount{"aa": {Latest: 90, Old: 99}, "bb": {Latest: 0, Old: 1}},
			EmptiedCountries:  []model.ArindbCanaryEmptiedCountry{{CountryCode: "bb", Old: 1}},
			EmptiedCityCount:  3,
		}
	}
	return decision
}

func statsArindbStateValues(t *testing.T) map[string]float64 {
	t.Helper()
	values := map[string]float64{}
	for _, state := range model.ArindbCanaryStates {
		values[state] = testutil.ToFloat64(statsArindbCanaryStateGauge.gauge.WithLabelValues(state))
	}
	return values
}

// The state is one-hot and the fallback gauge follows it, toggling off as
// soon as a later publication serves the latest again.
func TestStatsArindbCanaryStateIsOneHotAndTheFallbackGaugeToggles(t *testing.T) {
	for _, c := range []struct {
		state    string
		serving  int64
		fallback float64
	}{
		{state: model.ArindbCanaryStateFallback, serving: testStatsArindbOldEpoch, fallback: 1},
		{state: model.ArindbCanaryStateLatest, serving: testStatsArindbLatestEpoch, fallback: 0},
		{state: model.ArindbCanaryStateWarming, serving: testStatsArindbOldEpoch, fallback: 0},
		{state: model.ArindbCanaryStateSingle, serving: 0, fallback: 0},
		{state: model.ArindbCanaryStateFallback, serving: testStatsArindbOldEpoch, fallback: 1},
	} {
		statsPublishArindbCanary(testStatsArindbDecision(c.state, c.serving))
		for state, value := range statsArindbStateValues(t) {
			if want := map[bool]float64{true: 1, false: 0}[state == c.state]; value != want {
				t.Errorf("%s: state %s = %v, want %v", c.state, state, value, want)
			}
		}
		if got := testutil.ToFloat64(statsArindbFallbackActiveGauge.gauge); got != c.fallback {
			t.Errorf("%s: fallback active %v, want %v", c.state, got, c.fallback)
		}
		if got := testutil.ToFloat64(statsArindbServingEpochGauge.gauge); got != float64(c.serving) {
			t.Errorf("%s: serving epoch %v, want %d", c.state, got, c.serving)
		}
	}
}

// Both databases carry their epoch, state and hash in every state, and the
// comparison is exported per bucket, per country and as coverage.
func TestStatsArindbCanaryCarriesBothDatabasesAndTheComparison(t *testing.T) {
	for _, state := range model.ArindbCanaryStates {
		statsPublishArindbCanary(testStatsArindbDecision(state, 0))
		if count := testutil.CollectAndCount(statsArindbDatabaseInfoGauge.gauge); count != 2 {
			t.Fatalf("%s: %d database info series, want both", state, count)
		}
		if got := testutil.ToFloat64(statsArindbDatabaseInfoGauge.gauge.WithLabelValues("latest", server.ArinDatabaseLoaded, testStatsArindbLatestHash)); got != 1 {
			t.Errorf("%s: latest info %v", state, got)
		}
		if got := testutil.ToFloat64(statsArindbDatabaseInfoGauge.gauge.WithLabelValues("old", server.ArinDatabaseLoaded, testStatsArindbOldHash)); got != 1 {
			t.Errorf("%s: old info %v", state, got)
		}
		if latest, old := testutil.ToFloat64(statsArindbDatabaseBuildEpochGauge.gauge.WithLabelValues("latest")),
			testutil.ToFloat64(statsArindbDatabaseBuildEpochGauge.gauge.WithLabelValues("old")); latest != float64(testStatsArindbLatestEpoch) || old != float64(testStatsArindbOldEpoch) {
			t.Errorf("%s: epochs %v/%v", state, latest, old)
		}
	}
	for _, bucket := range model.ProviderEgressBuckets {
		if got := testutil.ToFloat64(statsArindbProvidersGauge.gauge.WithLabelValues(bucket, "old")); got != 100 {
			t.Errorf("%s old supply %v", bucket, got)
		}
		if got := testutil.ToFloat64(statsArindbProvidersByCountryGauge.gauge.WithLabelValues(bucket, "latest", "aa")); got != 90 {
			t.Errorf("%s latest supply in aa %v", bucket, got)
		}
		if got := testutil.ToFloat64(statsArindbLossRatioGauge.gauge.WithLabelValues(bucket)); got != 0.1 {
			t.Errorf("%s loss %v", bucket, got)
		}
		if got := testutil.ToFloat64(statsArindbEmptiedCountriesGauge.gauge.WithLabelValues(bucket)); got != 1 {
			t.Errorf("%s emptied countries %v", bucket, got)
		}
		if got := testutil.ToFloat64(statsArindbEmptiedCountryGauge.gauge.WithLabelValues(bucket, "bb")); got != 1 {
			t.Errorf("%s emptied bb %v", bucket, got)
		}
		if got := testutil.ToFloat64(statsArindbEmptiedCitiesGauge.gauge.WithLabelValues(bucket)); got != 3 {
			t.Errorf("%s emptied cities %v", bucket, got)
		}
	}
	if got := testutil.ToFloat64(statsArindbCoverageRatioGauge.gauge.WithLabelValues("paired")); got != 0.98 {
		t.Errorf("paired coverage %v", got)
	}
	if got := testutil.ToFloat64(statsArindbDecisionSecondsGauge.gauge); got != float64(testStatsArindbDecidedAt.Unix()) {
		t.Errorf("decision time %v", got)
	}
}

// Each condition the alerts read is one series, 1 while it holds; the latch's
// release progress, the serving changes and the cohort against its recent size
// are exported beside them.
func TestStatsArindbCanaryExportsTheDecisionsConditions(t *testing.T) {
	decision := testStatsArindbDecision(model.ArindbCanaryStateWarming, testStatsArindbOldEpoch)
	decision.WarmingTripped = true
	decision.Latch = &model.ArindbCanaryLatch{LatestEpoch: testStatsArindbLatestEpoch, OldEpoch: testStatsArindbOldEpoch, PassingRuns: 2}
	decision.PairCarriedReason = "latest_invalid"
	decision.LatestUnavailable = true
	statsPublishArindbCanary(decision)
	want := map[string]float64{
		model.ArindbCanaryFlagWarmingTripped: 1, model.ArindbCanaryFlagWarmingServesLatest: 0, model.ArindbCanaryFlagLatched: 1,
		model.ArindbCanaryFlagAccepted: 0, model.ArindbCanaryFlagLatestUnavailable: 1, model.ArindbCanaryFlagPairCarried: 1,
		model.ArindbCanaryFlagFallbackDropped: 0,
	}
	if count := testutil.CollectAndCount(statsArindbDecisionFlagsGauge.gauge); count != len(model.ArindbCanaryFlags) || len(want) != count {
		t.Fatalf("%d flag series, want every flag", count)
	}
	for flag, value := range want {
		if got := testutil.ToFloat64(statsArindbDecisionFlagsGauge.gauge.WithLabelValues(flag)); got != value {
			t.Errorf("flag %s = %v, want %v", flag, got, value)
		}
	}
	if got := testutil.ToFloat64(statsArindbLatchPassingRunsGauge.gauge); got != 2 {
		t.Errorf("latch passing runs %v", got)
	}
	if got := testutil.ToFloat64(statsArindbServingChangesGauge.gauge); got != 2 {
		t.Errorf("serving changes %v", got)
	}
	if current, reference := testutil.ToFloat64(statsArindbCohortProvidersGauge.gauge.WithLabelValues("current")),
		testutil.ToFloat64(statsArindbCohortProvidersGauge.gauge.WithLabelValues("reference")); current != 200 || reference != 250 {
		t.Errorf("cohort %v of %v", current, reference)
	}
	if got := testutil.ToFloat64(statsArindbCohortLossRatioGauge.gauge); got != 0.2 {
		t.Errorf("cohort loss %v", got)
	}
	if got := testutil.ToFloat64(statsArindbPairSinceGauge.gauge); got != float64(testStatsArindbDecidedAt.Add(-time.Hour).Unix()) {
		t.Errorf("pair since %v", got)
	}
	for _, bucket := range model.ProviderEgressBuckets {
		if got := testutil.ToFloat64(statsArindbChurnRatioGauge.gauge.WithLabelValues(bucket)); got != 0.3 {
			t.Errorf("%s churn %v", bucket, got)
		}
		if got := testutil.ToFloat64(statsArindbGainRatioGauge.gauge.WithLabelValues(bucket)); got != 0 {
			t.Errorf("%s gain %v", bucket, got)
		}
	}
	statsPublishArindbCanary(nil)
	for _, flag := range model.ArindbCanaryFlags {
		if got := testutil.ToFloat64(statsArindbDecisionFlagsGauge.gauge.WithLabelValues(flag)); got != 0 {
			t.Errorf("flag %s = %v without a decision", flag, got)
		}
	}
	if testutil.ToFloat64(statsArindbLatchPassingRunsGauge.gauge) != 0 || testutil.ToFloat64(statsArindbServingChangesGauge.gauge) != 0 ||
		testutil.ToFloat64(statsArindbCohortLossRatioGauge.gauge) != 0 || testutil.ToFloat64(statsArindbPairSinceGauge.gauge) != 0 {
		t.Error("no decision kept the latch, serving changes or cohort loss")
	}
}

// The per-country supply, about 900 series, is exported only by the Taskworker
// that published the decision; every Taskworker exports the rest.
func TestStatsArindbCanaryExportsTheCountrySupplyFromThePublisherOnly(t *testing.T) {
	decision := testStatsArindbDecision(model.ArindbCanaryStateLatest, testStatsArindbLatestEpoch)
	decision.Publisher = "another-host/g9/1"
	statsPublishArindbCanary(decision)
	if count := testutil.CollectAndCount(statsArindbProvidersByCountryGauge.gauge); count != 0 {
		t.Fatalf("a Taskworker that did not publish exported %d per-country series", count)
	}
	if count := testutil.CollectAndCount(statsArindbProvidersGauge.gauge); count != 6 {
		t.Fatalf("a Taskworker that did not publish exported %d bucket series, want 6", count)
	}
	decision.Publisher = model.ArindbCanaryPublisher()
	statsPublishArindbCanary(decision)
	if count := testutil.CollectAndCount(statsArindbProvidersByCountryGauge.gauge); count != 12 {
		t.Fatalf("the publisher exported %d per-country series, want 12", count)
	}
}

// Without a readable decision every reader serves each row's latest slot: no
// state, no fallback, serving epoch 0 and no comparison series, while the
// decision time keeps the last decision so its age grows into a stale alert.
func TestStatsArindbCanaryWithoutADecisionServesTheLatestSlot(t *testing.T) {
	statsPublishArindbCanary(testStatsArindbDecision(model.ArindbCanaryStateFallback, testStatsArindbOldEpoch))
	statsPublishArindbCanary(nil)
	for state, value := range statsArindbStateValues(t) {
		if value != 0 {
			t.Errorf("state %s = %v without a decision", state, value)
		}
	}
	if testutil.ToFloat64(statsArindbFallbackActiveGauge.gauge) != 0 || testutil.ToFloat64(statsArindbServingEpochGauge.gauge) != 0 {
		t.Fatal("no decision kept the fallback or its serving epoch")
	}
	for _, vec := range []*statsGaugeVec{statsArindbProvidersGauge, statsArindbProvidersByCountryGauge, statsArindbLossRatioGauge,
		statsArindbGainRatioGauge, statsArindbChurnRatioGauge, statsArindbCohortProvidersGauge,
		statsArindbEmptiedCountryGauge, statsArindbCoverageRatioGauge, statsArindbDatabaseInfoGauge} {
		if count := testutil.CollectAndCount(vec.gauge); count != 0 {
			t.Errorf("a comparison gauge kept %d series without a decision", count)
		}
	}
	if got := testutil.ToFloat64(statsArindbDecisionSecondsGauge.gauge); got != float64(testStatsArindbDecidedAt.Unix()) {
		t.Fatalf("decision time %v, want the last decision's", got)
	}
}
