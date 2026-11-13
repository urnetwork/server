// Request exclusions must not consume the whole bounded discovery sample when
// the same cache already contains admissible fallback candidates.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Compensation only spends the portion of a bounded exclusion set that could
// consume the original sample's survivor headroom, not one page per exclusion.
func TestFindProviders2ExclusionLoadBudgetStaysBounded(t *testing.T) {
	for _, testCase := range []struct {
		count      int
		exclusions int
		want       int
	}{
		{count: 20, exclusions: 0, want: 1000},
		{count: 20, exclusions: 6, want: 1000},
		{count: 20, exclusions: 980, want: 1000},
		{count: 20, exclusions: 981, want: 1001},
		{count: 20, exclusions: 1000, want: 1020},
		{count: 20, exclusions: 1980, want: 2000},
		{count: 20, exclusions: 2400, want: 2420},
		{count: 20, exclusions: 2401, want: 2420},
		{count: 20, exclusions: int(^uint(0) >> 1), want: 2420},
		{count: 200, exclusions: 2400, want: 2600},
		{count: 300, exclusions: 2400, want: 3000},
		{count: 1, exclusions: 999, want: 1000},
		{count: 1, exclusions: 1000, want: 1001},
		{count: 0, exclusions: 2400, want: 1000},
		{count: -1, exclusions: 2400, want: 1000},
	} {
		if got := findProviders2LoadCount(testCase.count, testCase.exclusions); got != testCase.want {
			t.Fatalf("count=%d exclusions=%d load budget=%d want=%d", testCase.count, testCase.exclusions, got, testCase.want)
		}
	}
}

// All five preferred pages are excluded, so the pre-fix selector deterministically
// misses the next facet regardless of either mode's random page order.
func TestFindProviders2BroadExclusionsReachOnlineFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, source := range []string{"client_ids", "destinations", "both"} {
			locationId := server.NewId()
			args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &locationId}}}
			preferred := []*ClientScore{}
			for range 1000 {
				score := onlineBackfillScore(true, 1)
				score.IpFamilies = ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
				preferred = append(preferred, score)
				if source != "destinations" {
					args.ExcludeClientIds = append(args.ExcludeClientIds, score.ClientId)
				}
				if source != "client_ids" {
					args.ExcludeDestinations = append(args.ExcludeDestinations, []server.Id{score.ClientId})
				}
			}
			allowed := map[server.Id]bool{}
			fallback := []*ClientScore{}
			for range 20 {
				score := onlineBackfillScore(true, 1)
				fallback = append(fallback, score)
				allowed[score.ClientId] = true
			}
			hard, network := onlineBackfillScore(true, 2), onlineBackfillScore(true, 2)
			network.NetworkOnly = true
			fallback = append(fallback, hard, network)
			writeSubscriberFactsForScores(ctx, append(preferred, fallback...))
			server.Redis(ctx, func(r server.RedisClient) {
				pipe := r.Pipeline()
				for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
					for _, caller := range countryCodeLocationIds() {
						pipe.Set(ctx, clientScoreLocationAliasKey(false, mode, locationId, caller), clientScoreAliasBaselineValue, time.Minute)
					}
					for facet, scores := range map[ipFamilyFacet][]*ClientScore{
						ipFamilyFacetDualstack: preferred,
						ipFamilyFacetV4Only:    fallback,
						ipFamilyFacetV6Only:    nil,
					} {
						counts := []int{}
						for start := 0; start < len(scores); start += ClientScoreSampleCount {
							page := scores[start:min(start+ClientScoreSampleCount, len(scores))]
							pipe.Set(ctx, clientScoreLocationFacetSampleKey(false, mode, locationId, server.Id{}, facet, len(counts)), gobEncodeForTest(t, page), time.Minute)
							counts = append(counts, len(page))
						}
						pipe.Set(ctx, clientScoreLocationFacetCountsKey(false, mode, locationId, server.Id{}, facet), gobEncodeForTest(t, counts), time.Minute)
					}
				}
				pipe.SAdd(ctx, providerHardExclusionsKey, providerHardExclusionsReadyMember, hard.ClientId.String())
				_, err := pipe.Exec(ctx)
				server.Raise(err)
			})
			clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "exclusion-sample-test", false, false))
			result, err := FindProviders2(args, clientSession)
			if err != nil || result == nil || len(result.Providers) != len(allowed) {
				got := 0
				if result != nil {
					got = len(result.Providers)
				}
				t.Fatalf("%s exclusions consumed discovery before same-target online fallback: providers=%d want=%d error=%v", source, got, len(allowed), err)
			}
			for _, provider := range result.Providers {
				if !allowed[provider.ClientId] || provider.Tier != 2*egressTestBackfillOffset() {
					t.Fatalf("%s widened a request, hard, network, or online-tier boundary", source)
				}
			}
			assertEgressTestNoRepeats(t, result.Providers)
		}
	})
}

// Each real-sized page has two allowed providers and 198 excluded ones. The
// old five-page sample returns exactly ten even though all twenty are cached.
// The optional other-mode cache is empty, so no random cross-mode overlap can
// make the pre-fix result pass by accident.
func TestFindProviders2BroadExclusionsFillSingleFamilySample(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		locationId := server.NewId()
		args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &locationId}}}
		pages := [][]*ClientScore{}
		allowed := map[server.Id]bool{}
		for range 10 {
			page := []*ClientScore{}
			for i := range ClientScoreSampleCount {
				score := onlineBackfillScore(true, 1)
				page = append(page, score)
				if i < 2 {
					allowed[score.ClientId] = true
				} else {
					args.ExcludeClientIds = append(args.ExcludeClientIds, score.ClientId)
				}
			}
			pages = append(pages, page)
			writeSubscriberFactsForScores(ctx, page)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			pipe := r.Pipeline()
			for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
				for _, caller := range countryCodeLocationIds() {
					pipe.Set(ctx, clientScoreLocationAliasKey(false, mode, locationId, caller), clientScoreAliasBaselineValue, time.Minute)
				}
				counts := []int{}
				if mode == RankModeQuality {
					for i, page := range pages {
						counts = append(counts, len(page))
						pipe.Set(ctx, clientScoreLocationFacetSampleKey(false, mode, locationId, server.Id{}, ipFamilyFacetV4Only, i), gobEncodeForTest(t, page), time.Minute)
					}
				}
				pipe.Set(ctx, clientScoreLocationFacetCountsKey(false, mode, locationId, server.Id{}, ipFamilyFacetV4Only), gobEncodeForTest(t, counts), time.Minute)
				pipe.Set(ctx, clientScoreLocationFacetCountsKey(false, mode, locationId, server.Id{}, ipFamilyFacetDualstack), gobEncodeForTest(t, []int{}), time.Minute)
			}
			pipe.SAdd(ctx, providerHardExclusionsKey, providerHardExclusionsReadyMember)
			_, err := pipe.Exec(ctx)
			server.Raise(err)
		})
		clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "single-family-sample-test", false, false))
		result, err := FindProviders2(args, clientSession)
		if err != nil || result == nil || len(result.Providers) != len(allowed) {
			got := 0
			if result != nil {
				got = len(result.Providers)
			}
			t.Fatalf("broad runtime exclusions truncated available online supply: providers=%d want=%d error=%v", got, len(allowed), err)
		}
		for _, provider := range result.Providers {
			if !allowed[provider.ClientId] {
				t.Fatal("explicitly excluded provider escaped compensated sampling")
			}
		}
		assertEgressTestNoRepeats(t, result.Providers)
	})
}
