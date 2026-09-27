package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The backfill of connect/GEOMAP.md §10.3 as protection against a mass probe
// failure: a short bucket borrows from the other bucket, then from the online
// bucket, each borrowed provider tiered behind every native one, never across
// an exclusion.

// How many providers every test asks for.
const egressTestBackfillCount = 10

// The default offset the borrowed carry.
func egressTestBackfillOffset() int {
	return DefaultEgressIndexSettings().BackfillTierOffset
}

// Fails when a provider is answered twice.
func assertEgressTestNoRepeats(t testing.TB, providers []*FindProvidersProvider) {
	t.Helper()
	seenClientIds := map[server.Id]bool{}
	for _, provider := range providers {
		if seenClientIds[provider.ClientId] {
			t.Fatalf("provider %s answered twice", provider.ClientId)
		}
		seenClientIds[provider.ClientId] = true
	}
}

// Fails when the answer's tiers go down: the client ranks by tier, so an
// answer in order has tiers that never fall.
func assertEgressTestTiersKeepOrder(t testing.TB, providers []*FindProvidersProvider) {
	t.Helper()
	for i := 1; i < len(providers); i += 1 {
		if providers[i].Tier < providers[i-1].Tier {
			t.Fatalf("tier %d at %d follows tier %d: the answer's order is not the client's", providers[i].Tier, i, providers[i-1].Tier)
		}
	}
}

// Connects a probed provider that failed ten of sixty loads: out of quality,
// in speed at the given performance.
func egressTestOverLine(ctx context.Context, t testing.TB, city *Location, performance egressTestPerformance) *egressTestProvider {
	provider := egressTestConnect(ctx, t, city, performance, nil, nil)
	egressTestProbed(ctx, provider, city, 10, "us")
	return provider
}

// Every probed provider over the one-in-ten line: quality is empty, and a
// quality request still answers in full, every provider borrowed from speed in
// speed order at its speed tier plus the offset.
func TestBackfillMassQualityFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		speedTiers := map[server.Id]int{}
		for i := range egressTestBackfillCount {
			performance := egressTestFast
			speedTier := 0
			if i%2 == 1 {
				performance = egressTestSpeedTierOne
				speedTier = 1
			}
			speedTiers[egressTestOverLine(ctx, t, city, performance).clientId] = speedTier
		}
		egressTestPasses(ctx, t)

		providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, egressTestBackfillCount, false, server.NewId())
		connect.AssertEqual(t, len(providers), egressTestBackfillCount)
		assertEgressTestNoRepeats(t, providers)
		assertEgressTestTiersKeepOrder(t, providers)
		for _, provider := range providers {
			speedTier, ok := speedTiers[provider.ClientId]
			if !ok {
				t.Fatalf("provider %s is not one of the location's", provider.ClientId)
			}
			connect.AssertEqual(t, provider.Tier, speedTier+egressTestBackfillOffset())
		}
	})
}

// The probe pipeline down: every provider's evidence older than seven days, or
// never taken, with the flag on. Quality and speed are both empty, and both
// requests still answer in full from the online bucket; the counts keep every
// provider, since the unprobed never fail closed.
func TestBackfillProbePipelineDown(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testing_enableProviderEgressTest(t)
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		clientIds := []server.Id{}
		for i := range egressTestBackfillCount {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			if i%2 == 0 {
				egressTestHealth(ctx, provider.clientId, server.NowUtc().Add(-ProviderEgressLocationMaxAge-time.Hour), 60, 0)
			}
			clientIds = append(clientIds, provider.clientId)
		}
		egressTestPasses(ctx, t)

		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			providers := egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, egressTestBackfillCount, false, server.NewId())
			connect.AssertEqual(t, len(providers), egressTestBackfillCount)
			assertEgressTestNoRepeats(t, providers)
			for _, provider := range providers {
				if !slices.Contains(clientIds, provider.ClientId) {
					t.Fatalf("%s: provider %s is not one of the location's", rankMode, provider.ClientId)
				}
				connect.AssertEqual(t, provider.Tier, 2*egressTestBackfillOffset())
			}
		}
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), egressTestBackfillCount)
	})
}

// Every provider slower than the speed cutoffs: speed has no natives, and a
// speed request still answers in full with the quality providers those cutoffs
// excluded, in quality order at their quality tier plus the offset.
func TestBackfillMassSpeedCutoffFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		qualityTiers := map[server.Id]int{}
		for i := range egressTestBackfillCount {
			provider := egressTestConnect(ctx, t, city, egressTestSlow, nil, nil)
			// 60 ms is one point of quality latency; each failed load is 20 more
			failed := i % 3
			egressTestProbed(ctx, provider, city, failed, "us")
			qualityTiers[provider.clientId] = min(20*failed+1, MaxClientScore) / ClientScorePerTier
		}
		egressTestPasses(ctx, t)

		providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeSpeed, egressTestBackfillCount, false, server.NewId())
		connect.AssertEqual(t, len(providers), egressTestBackfillCount)
		assertEgressTestNoRepeats(t, providers)
		assertEgressTestTiersKeepOrder(t, providers)
		for _, provider := range providers {
			qualityTier, ok := qualityTiers[provider.ClientId]
			if !ok {
				t.Fatalf("provider %s is not one of the location's", provider.ClientId)
			}
			connect.AssertEqual(t, provider.Tier, qualityTier+egressTestBackfillOffset())
		}
	})
}

// Two natives and eight borrowed: the natives first in native order, the
// borrowed after in their own order at their tier plus the offset, and no
// provider twice.
func TestBackfillMixedNativesAndBorrowed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		nativeClientIds := []server.Id{}
		for range 2 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			egressTestProbed(ctx, provider, city, 0, "us")
			nativeClientIds = append(nativeClientIds, provider.clientId)
		}
		speedTiers := map[server.Id]int{}
		for i := range 8 {
			performance := egressTestFast
			speedTier := 0
			if i%2 == 1 {
				performance = egressTestSpeedTierOne
				speedTier = 1
			}
			speedTiers[egressTestOverLine(ctx, t, city, performance).clientId] = speedTier
		}
		egressTestPasses(ctx, t)

		providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, egressTestBackfillCount, false, server.NewId())
		connect.AssertEqual(t, len(providers), egressTestBackfillCount)
		assertEgressTestNoRepeats(t, providers)
		assertEgressTestTiersKeepOrder(t, providers)
		for i, provider := range providers {
			if i < 2 {
				if !slices.Contains(nativeClientIds, provider.ClientId) {
					t.Fatalf("answer %d is not a native", i)
				}
				connect.AssertEqual(t, provider.Tier, 0)
				continue
			}
			speedTier, ok := speedTiers[provider.ClientId]
			if !ok {
				t.Fatalf("answer %d is neither native nor borrowed from speed", i)
			}
			connect.AssertEqual(t, provider.Tier, speedTier+egressTestBackfillOffset())
		}
	})
}

// Quality short: first the speed bucket's providers quality does not hold,
// in speed order, then the online bucket, tiered behind every other borrowed
// provider.
func TestBackfillQualityShortBorrowsSpeedThenOnline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		nativeClientIds := []server.Id{}
		for range 2 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			egressTestProbed(ctx, provider, city, 0, "us")
			nativeClientIds = append(nativeClientIds, provider.clientId)
		}
		overLine := []server.Id{}
		for range 3 {
			overLine = append(overLine, egressTestOverLine(ctx, t, city, egressTestFast).clientId)
		}
		onlineClientIds := []server.Id{}
		for range 5 {
			onlineClientIds = append(onlineClientIds, egressTestConnect(ctx, t, city, egressTestFast, nil, nil).clientId)
		}
		egressTestPasses(ctx, t)

		providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, egressTestBackfillCount, false, server.NewId())
		connect.AssertEqual(t, len(providers), egressTestBackfillCount)
		assertEgressTestNoRepeats(t, providers)
		assertEgressTestTiersKeepOrder(t, providers)
		offset := egressTestBackfillOffset()
		for i, provider := range providers {
			switch {
			case i < 2:
				connect.AssertEqual(t, slices.Contains(nativeClientIds, provider.ClientId), true)
				connect.AssertEqual(t, provider.Tier, 0)
			case i < 5:
				connect.AssertEqual(t, slices.Contains(overLine, provider.ClientId), true)
				connect.AssertEqual(t, provider.Tier, 0+offset)
			default:
				connect.AssertEqual(t, slices.Contains(onlineClientIds, provider.ClientId), true)
				connect.AssertEqual(t, provider.Tier, 2*offset)
			}
		}
	})
}

// Speed short after its own cutoffs: first the quality providers those cutoffs
// excluded, in quality order, then the online bucket, tiered behind every
// other borrowed provider.
func TestBackfillSpeedShortBorrowsQualityThenOnline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		nativeClientIds := []server.Id{}
		for range 2 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			egressTestProbed(ctx, provider, city, 0, "us")
			nativeClientIds = append(nativeClientIds, provider.clientId)
		}
		qualityTiers := map[server.Id]int{}
		for i := range 3 {
			provider := egressTestConnect(ctx, t, city, egressTestSlow, nil, nil)
			egressTestProbed(ctx, provider, city, i%2, "us")
			qualityTiers[provider.clientId] = (20*(i%2) + 1) / ClientScorePerTier
		}
		onlineClientIds := []server.Id{}
		for range 5 {
			onlineClientIds = append(onlineClientIds, egressTestConnect(ctx, t, city, egressTestFast, nil, nil).clientId)
		}
		egressTestPasses(ctx, t)

		providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeSpeed, egressTestBackfillCount, false, server.NewId())
		connect.AssertEqual(t, len(providers), egressTestBackfillCount)
		assertEgressTestNoRepeats(t, providers)
		assertEgressTestTiersKeepOrder(t, providers)
		offset := egressTestBackfillOffset()
		for i, provider := range providers {
			switch {
			case i < 2:
				connect.AssertEqual(t, slices.Contains(nativeClientIds, provider.ClientId), true)
				connect.AssertEqual(t, provider.Tier, 0)
			case i < 5:
				qualityTier, ok := qualityTiers[provider.ClientId]
				connect.AssertEqual(t, ok, true)
				connect.AssertEqual(t, provider.Tier, qualityTier+offset)
			default:
				connect.AssertEqual(t, slices.Contains(onlineClientIds, provider.ClientId), true)
				connect.AssertEqual(t, provider.Tier, 2*offset)
			}
		}
	})
}

// With both buckets otherwise empty, a blackholed and a TLS-failed provider --
// probed or unprobed -- are still absent, and so without force_minimum is a
// mislocated one: the answer is short rather than crossing an exclusion.
// force_minimum re-admits the mislocated alone.
func TestBackfillNeverCrossesAnExclusion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")
		// supply elsewhere, so no fleet-wide fallback touches the counts
		elsewhere := egressTestCity(ctx, "Portland", "Oregon", "United States", "us")
		healthyElsewhere := egressTestConnect(ctx, t, elsewhere, egressTestFast, nil, nil)
		egressTestProbed(ctx, healthyElsewhere, elsewhere, 0, "us")

		blackholed := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, blackholed, city, 0, "us")
		egressTestBlackhole(ctx, blackholed.clientId)
		intercepted := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:                 intercepted.clientId,
			MeasuredAt:               server.NowUtc(),
			OKCount:                  60,
			Total:                    60,
			ClassResults:             map[string]ProviderEgressHealthClassResult{"site": {OK: 60, Total: 60}},
			TLSAuthenticationFailure: true,
		})
		onlineBlackholed := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestBlackhole(ctx, onlineBlackholed.clientId)
		mislocated := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, mislocated, city, 0, "gb")
		onlineMislocated := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    onlineMislocated.clientId,
			LocationId:  city.LocationId,
			CountryCode: "gb",
			ObservedAt:  server.NowUtc(),
		})
		egressTestPasses(ctx, t)

		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			providers := egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, egressTestBackfillCount, false, server.NewId())
			if len(providers) != 0 {
				t.Fatalf("%s: backfill crossed an exclusion or the gate: %v", rankMode, egressTestIds(providers))
			}
			forced := egressTestIds(egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, egressTestBackfillCount, true, server.NewId()))
			slices.SortFunc(forced, func(a server.Id, b server.Id) int { return a.Cmp(b) })
			wantForced := []server.Id{mislocated.clientId, onlineMislocated.clientId}
			slices.SortFunc(wantForced, func(a server.Id, b server.Id) int { return a.Cmp(b) })
			connect.AssertEqual(t, forced, wantForced)
		}
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 0)
	})
}

// Rows the new rollup has not written rank by the old rules and are still
// borrowed when the other bucket is short: a net-type score of two fails the
// quality minimum as it always did, and a quality request borrows such rows
// from speed at their old speed tier plus the offset.
func TestBackfillBorrowsOldRowsByTheOldRules(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		native := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		borrowedClientIds := []server.Id{}
		for range 3 {
			borrowedClientIds = append(borrowedClientIds, egressTestConnect(ctx, t, city, egressTestFast, nil, nil).clientId)
		}
		egressTestPasses(ctx, t)

		// the rows as a binary without the index left them
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE network_client_location_reliability
				SET
					egress_index = NULL,
					egress_quality = NULL,
					egress_evidence_time = NULL,
					max_net_type_score = CASE WHEN client_id = $1 THEN 0 ELSE 2 END,
					max_net_type_score_speed = 0
				`,
				native.clientId,
			))
		})
		connect.AssertEqual(t, UpdateClientScores(ctx, time.Hour, 1), nil)
		connect.AssertEqual(t, UpdateClientLocations(ctx, time.Hour), nil)

		providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, egressTestBackfillCount, false, server.NewId())
		connect.AssertEqual(t, len(providers), 4)
		assertEgressTestNoRepeats(t, providers)
		connect.AssertEqual(t, providers[0].ClientId, native.clientId)
		connect.AssertEqual(t, providers[0].Tier, 0)
		for _, provider := range providers[1:] {
			connect.AssertEqual(t, slices.Contains(borrowedClientIds, provider.ClientId), true)
			// the old speed base is the net-type speed score, zero here
			connect.AssertEqual(t, provider.Tier, 0+egressTestBackfillOffset())
		}
	})
}

// A missing other-mode cache degrades to the native set with no error, and the
// next cache fill restores the backfill.
func TestBackfillMissingOtherModeCache(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		nativeClientIds := []server.Id{}
		for range 2 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			egressTestProbed(ctx, provider, city, 0, "us")
			nativeClientIds = append(nativeClientIds, provider.clientId)
		}
		for range 8 {
			egressTestOverLine(ctx, t, city, egressTestFast)
		}
		egressTestPasses(ctx, t)

		// every speed key of the non-forced cache gone
		server.Redis(ctx, func(r server.RedisClient) {
			keys, err := r.Keys(ctx, "{cs_0_s_*").Result()
			connect.AssertEqual(t, err, nil)
			if len(keys) == 0 {
				t.Fatal("fixture is wrong: there is no speed cache to remove")
			}
			for _, key := range keys {
				connect.AssertEqual(t, r.Del(ctx, key).Err(), nil)
			}
		})
		providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, egressTestBackfillCount, false, server.NewId())
		answeredClientIds := egressTestIds(providers)
		slices.SortFunc(answeredClientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		slices.SortFunc(nativeClientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		connect.AssertEqual(t, answeredClientIds, nativeClientIds)

		connect.AssertEqual(t, UpdateClientScores(ctx, time.Hour, 1), nil)
		providers = egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, egressTestBackfillCount, false, server.NewId())
		connect.AssertEqual(t, len(providers), egressTestBackfillCount)
	})
}

// provider_backfill{rank_mode} counts the borrowed of each answer, and the
// answered-providers counter every provider it held, so a mass failure shows
// as a wave of backfill against the answered total.
func TestBackfillMetricCountsTheBorrowed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		for range 2 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			egressTestProbed(ctx, provider, city, 0, "us")
		}
		for range 8 {
			egressTestOverLine(ctx, t, city, egressTestFast)
		}
		egressTestPasses(ctx, t)

		observed := func() (uint64, float64, float64) {
			metric := &dto.Metric{}
			connect.AssertEqual(t, findProviders2BackfillProviders.WithLabelValues(RankModeQuality).(prometheus.Metric).Write(metric), nil)
			answered := testutil.ToFloat64(findProviders2AnsweredProviders.WithLabelValues(RankModeQuality))
			return metric.GetHistogram().GetSampleCount(), metric.GetHistogram().GetSampleSum(), answered
		}
		answers, borrowed, answered := observed()
		connect.AssertEqual(t, len(egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, egressTestBackfillCount, false, server.NewId())), egressTestBackfillCount)
		nextAnswers, nextBorrowed, nextAnswered := observed()
		connect.AssertEqual(t, nextAnswers-answers, uint64(1))
		connect.AssertEqual(t, nextBorrowed-borrowed, float64(8))
		connect.AssertEqual(t, nextAnswered-answered, float64(egressTestBackfillCount))

		// a forced answer is the caller's override, never a backfill
		egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, egressTestBackfillCount, true, server.NewId())
		forcedAnswers, _, _ := observed()
		connect.AssertEqual(t, forcedAnswers, nextAnswers)
	})
}
