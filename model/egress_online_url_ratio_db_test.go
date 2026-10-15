package model

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The Online URL ratio against the database: the passes in the order the
// taskworker runs them, FindProviders2, the location and dashboard counts,
// probe admission, provider status and inspection, under provider.yml as an
// operator sets it.

// The providers the online bucket holds, as the dashboard counts them.
func onlineUrlRatioTestOnlineCount(ctx context.Context) int64 {
	onlineCount := int64(0)
	for _, count := range CountProviderEgress(ctx).BucketIndexCounts[ProviderEgressBucketOnline] {
		onlineCount += count
	}
	return onlineCount
}

// A provider measured 3 of 5 is online for its failing ratio while the ratio
// is off. At 0.8 it leaves the non-forced samples, the Quality, Speed and
// Online answers, the location counts and the dashboard counts, for
// low_success, while unprobed and single-run providers stay. It is no hard
// exclusion: force_minimum with speed and its explicit client id still reach
// it, and the probe still claims it. Once its ratio reaches 0.8 it is back,
// in Speed and Quality too.
func TestOnlineUrlRatioLeavesEveryBucketAndCountAndReturns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		healthy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, healthy, city, 0, "us")
		passing := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, passing.clientId, server.NowUtc(), 5, 1)
		low := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, low.clientId, server.NowUtc(), 5, 2)
		// the current reliability history URL probe admission requires
		testingInsertProviderReliabilityWitness(ctx, low.clientId)
		warming := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, warming.clientId, server.NowUtc(), 1, 1)
		unprobed := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		for _, provider := range []*egressTestProvider{healthy, passing, low, warming, unprobed} {
			for lookbackIndex := range 3 {
				egressTestReliability(ctx, provider.clientId, lookbackIndex, 1, 1)
			}
		}
		kept := []*egressTestProvider{healthy, passing, warming, unprobed}

		// the ratio off: online for its failing ratio, as before the ratio
		egressTestPasses(ctx, t)
		assertOnlineSuccessTestAnswer(t, "ratio off", egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, false, server.NewId()),
			append([]*egressTestProvider{low}, kept...), nil)
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 5)
		connect.AssertEqual(t, onlineUrlRatioTestOnlineCount(ctx), int64(5))
		connect.AssertEqual(t, GetClientProviderStatus(ctx, low.networkId, low.clientId).Reason, ProviderStatusReasonEgressFailing)

		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte("online_url_success_ratio: 0.8\n"))
		defer pop()
		egressTestPasses(ctx, t)
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			clientScores := egressTestCachedScores(ctx, t, city, rankMode, false)
			if _, ok := clientScores[low.clientId]; ok {
				t.Errorf("%s: the provider below the ratio is in the non-forced sample", rankMode)
			}
			for name, provider := range map[string]*egressTestProvider{"warming": warming, "unprobed": unprobed} {
				if score, ok := clientScores[provider.clientId]; !ok || !score.Online {
					t.Errorf("%s: the %s provider is not online", rankMode, name)
				}
			}
			assertOnlineSuccessTestAnswer(t, string(rankMode), egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, 10, false, server.NewId()),
				kept, []*egressTestProvider{low})
		}
		assertOnlineSuccessTestAnswer(t, "ratio 0.8", egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, false, server.NewId()),
			kept, []*egressTestProvider{low})
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 4)
		counts := CountProviderEgress(ctx)
		connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedLowSuccess], int64(1))
		connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedNoSuccess], int64(0))
		connect.AssertEqual(t, onlineUrlRatioTestOnlineCount(ctx), int64(4))
		connect.AssertEqual(t, GetClientProviderStatus(ctx, low.networkId, low.clientId).Reason, ProviderStatusReasonEgressLowSuccess)
		inspection := InspectProviderEgress(ctx, low.clientId)
		if inspection.Reason != ProviderExcludedLowSuccess || !inspection.BelowOnlineUrlRatio || inspection.OnlineUrlSuccessBaselineRatio != 0.8 || !inspection.OnlineUrlRatioBaselineCountryExemption || inspection.Online || inspection.Counted || inspection.HardExcluded {
			t.Errorf("inspection %+v, want low_success below the published 0.8, out of every bucket and the counts, not hard excluded", inspection)
		}

		// force_minimum with speed and the explicit client id keep only the
		// common gates
		forced := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeSpeed, 10, true, server.NewId())
		if !slices.Contains(egressTestIds(forced), low.clientId) {
			t.Error("force_minimum lost the provider below the ratio")
		}
		named := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &low.clientId}}, RankModeSpeed, 1, false, server.NewId())
		connect.AssertEqual(t, egressTestIds(named), []server.Id{low.clientId})
		// and the probe still claims it, the only way it can come back
		due := ClaimProviderUrlProbeDue(ctx, server.NowUtc(), 100, 0, 1)
		if !slices.ContainsFunc(due, func(provider ProviderUrlProbeDue) bool { return provider.ClientId == low.clientId }) {
			t.Error("the provider below the ratio is no longer probed")
		}

		// five more successes: 8 of 10 reaches 0.8, and the next passes bring
		// it back to Online, the counts, and Quality and Speed
		egressTestHealth(ctx, low.clientId, server.NowUtc(), 5, 0)
		egressTestPasses(ctx, t)
		assertOnlineSuccessTestAnswer(t, "recovered", egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, false, server.NewId()),
			append([]*egressTestProvider{low}, kept...), nil)
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 5)
		connect.AssertEqual(t, CountProviderEgress(ctx).ReasonCounts[ProviderExcludedLowSuccess], int64(0))
		connect.AssertEqual(t, onlineUrlRatioTestOnlineCount(ctx), int64(5))
		score, ok := egressTestCachedScores(ctx, t, city, RankModeSpeed, false)[low.clientId]
		if !ok || !score.Online || !score.PassesMinimums[RankModeSpeed] || !score.PassesMinimums[RankModeQuality] {
			t.Fatal("the recovered provider is not back in Online, Speed and Quality")
		}
		connect.AssertEqual(t, GetClientProviderStatus(ctx, low.networkId, low.clientId).Reason, ProviderStatusReasonNone)
	})
}

// The country exemption across the passes. In vn one of five measured
// providers has a success, so the URL-success rule exempts vn. At 0.8 with
// the exemption (variant c) vn keeps all six in Online and the counts, the
// 3 of 5 provider below the ratio included. Without it (variant a) the ratio
// applies in vn too, and only the unprobed provider is left: the 3 of 5
// provider is low_success and the four without a success, below every ratio,
// are no_success.
func TestOnlineUrlRatioCountryExemptionInThePasses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Hanoi", "Hanoi", "Vietnam", "vn")

		low := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, low.clientId, server.NowUtc(), 5, 2)
		dead := []*egressTestProvider{}
		for range 4 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 5)
			dead = append(dead, provider)
		}
		unprobed := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		belowRatio := append([]*egressTestProvider{low}, dead...)
		for _, provider := range append([]*egressTestProvider{unprobed}, belowRatio...) {
			for lookbackIndex := range 3 {
				egressTestReliability(ctx, provider.clientId, lookbackIndex, 1, 1)
			}
		}
		withProviderConfig := func(config string, callback func()) {
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
			defer pop()
			callback()
		}

		withProviderConfig("online_url_success_ratio: 0.8\n", func() {
			egressTestPasses(ctx, t)
			assertOnlineSuccessTestAnswer(t, "with the exemption", egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, false, server.NewId()),
				append([]*egressTestProvider{unprobed}, belowRatio...), nil)
			connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 6)
			counts := CountProviderEgress(ctx)
			connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedLowSuccess], int64(0))
			connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedNoSuccess], int64(0))
			connect.AssertEqual(t, GetClientProviderStatus(ctx, low.networkId, low.clientId).Reason, ProviderStatusReasonEgressFailing)
		})

		withProviderConfig("online_url_success_ratio: 0.8\nonline_url_ratio_country_exemption: false\n", func() {
			egressTestPasses(ctx, t)
			assertOnlineSuccessTestAnswer(t, "without the exemption", egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, false, server.NewId()),
				[]*egressTestProvider{unprobed}, belowRatio)
			connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 1)
			counts := CountProviderEgress(ctx)
			connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedLowSuccess], int64(1))
			connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedNoSuccess], int64(4))
			connect.AssertEqual(t, GetClientProviderStatus(ctx, low.networkId, low.clientId).Reason, ProviderStatusReasonEgressLowSuccess)
			connect.AssertEqual(t, GetClientProviderStatus(ctx, dead[0].networkId, dead[0].clientId).Reason, ProviderStatusReasonEgressNoSuccess)
		})
	})
}

// Provider status and inspection follow the ratio as the score export applied
// it, never the reader's own provider.yml. An export at 0.8 publishes a
// version 3 guard that holds a 3 of 5 provider below it for a reader without
// the ratio, and an export with the ratio off publishes version 2, which
// holds it nowhere for a reader at 0.8.
func TestProviderStatusFollowsTheExportsOnlineUrlRatio(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		healthy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, healthy, city, 0, "us")
		low := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, low.clientId, server.NowUtc(), 5, 2)
		for _, provider := range []*egressTestProvider{healthy, low} {
			for lookbackIndex := range 3 {
				egressTestReliability(ctx, provider.clientId, lookbackIndex, 1, 1)
			}
		}
		withProviderConfig := func(config string, callback func()) {
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
			defer pop()
			callback()
		}

		withProviderConfig("online_url_success_ratio: 0.8\n", func() {
			egressTestPasses(ctx, t)
		})
		// read from Redis, as a reader in another process reads it
		published, err := readProviderOnlineSuccessBaseline(ctx, server.NowUtc())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, published.urlSuccessRatio, 0.8)
		connect.AssertEqual(t, published.urlRatioCountryExemption, true)
		connect.AssertEqual(t, published.countryCodeCounts["us"].LowSuccess, 1)
		for _, config := range []string{"", "online_url_success_ratio: 0\n"} {
			withProviderConfig(config, func() {
				if reason := GetClientProviderStatus(ctx, low.networkId, low.clientId).Reason; reason != ProviderStatusReasonEgressLowSuccess {
					t.Errorf("export at 0.8, reader provider.yml %q: status reason %s, want %s", config, reason, ProviderStatusReasonEgressLowSuccess)
				}
				inspection := InspectProviderEgress(ctx, low.clientId)
				if inspection.Reason != ProviderExcludedLowSuccess || inspection.OnlineUrlSuccessBaselineRatio != 0.8 {
					t.Errorf("export at 0.8, reader provider.yml %q: inspection reason %s ratio %v, want %s at 0.8", config, inspection.Reason, inspection.OnlineUrlSuccessBaselineRatio, ProviderExcludedLowSuccess)
				}
			})
		}

		egressTestPasses(ctx, t)
		published, err = readProviderOnlineSuccessBaseline(ctx, server.NowUtc())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, published.urlSuccessRatio, float64(0))
		withProviderConfig("online_url_success_ratio: 0.8\n", func() {
			connect.AssertEqual(t, GetClientProviderStatus(ctx, low.networkId, low.clientId).Reason, ProviderStatusReasonEgressFailing)
			inspection := InspectProviderEgress(ctx, low.clientId)
			if inspection.Reason != ProviderExcludedHealth || inspection.BelowOnlineUrlRatio || inspection.OnlineUrlSuccessBaselineRatio != 0 {
				t.Errorf("export with the ratio off, reader at 0.8: inspection reason %s below %t ratio %v, want health, not below, ratio off", inspection.Reason, inspection.BelowOnlineUrlRatio, inspection.OnlineUrlSuccessBaselineRatio)
			}
		})
	})
}
