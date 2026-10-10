// Request-filtered pages cannot spend the discovery sample before the same
// requested market's public online fallback is examined.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Every preferred page has the same rejection cause, making the boundary
// independent of page shuffle and of overlap between the two mode samples.
// The healthy control proves that usable preferred rows still fill first.
func TestFindProviders2PostFilterRefillReachesSameTargetOnline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, cause := range []string{"network", "hard", "family", "duplicate", "same_network"} {
			locationId, callerNetworkId := server.NewId(), server.NewId()
			preferred := []*ClientScore{}
			allowed := map[server.Id]bool{}
			hardMembers := []any{providerHardExclusionsReadyMember}
			for range 1000 {
				score := onlineBackfillScore(true, 1)
				score.IpFamilies = ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
				switch cause {
				case "network":
					score.NetworkOnly = true
				case "hard":
					hardMembers = append(hardMembers, score.ClientId.String())
				case "family":
					// A stale un-faceted or mismatched page still needs the
					// request's authoritative family check before selection.
					score.IpFamilies = ClientScoreIpFamilyV6
				case "duplicate":
					if 0 < len(preferred) {
						score = preferred[0]
					}
					allowed[score.ClientId] = true
				case "same_network":
					score.NetworkOnly, score.NetworkId = true, callerNetworkId
					allowed[score.ClientId] = true
				}
				preferred = append(preferred, score)
			}
			fallback := []*ClientScore{}
			for range 20 {
				score := onlineBackfillScore(true, 1)
				fallback = append(fallback, score)
				if cause != "same_network" {
					allowed[score.ClientId] = true
				}
			}
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
				pipe.SAdd(ctx, providerHardExclusionsKey, hardMembers...)
				_, err := pipe.Exec(ctx)
				server.Raise(err)
			})
			clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(callerNetworkId, server.NewId(), "postfilter-sample-test", false, false))
			result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &locationId}}}, clientSession)
			if err != nil || result == nil || len(result.Providers) != 20 {
				got := 0
				if result != nil {
					got = len(result.Providers)
				}
				t.Fatalf("%s preferred pages hid same-target eligible supply: returned=%d want=20 err=%v", cause, got, err)
			}
			for _, provider := range result.Providers {
				if !allowed[provider.ClientId] || provider.Tier != 2*egressTestBackfillOffset() {
					t.Fatalf("%s refill widened an eligibility or online-tier boundary", cause)
				}
			}
			assertEgressTestNoRepeats(t, result.Providers)
		}
	})
}

// A cache read failure is explicitly unknown, not native exhaustion. With no
// usable fallback it can answer empty, while an optional failure retains every
// validated higher-priority provider; neither path leaks filtered candidates.
func TestFindProviders2RefillReadErrorsPreserveModeBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, failedMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			locationId := server.NewId()
			allowed := map[server.Id]bool{}
			server.Redis(ctx, func(r server.RedisClient) {
				pipe := r.Pipeline()
				for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
					for _, caller := range countryCodeLocationIds() {
						pipe.Set(ctx, clientScoreLocationAliasKey(false, mode, locationId, caller), clientScoreAliasBaselineValue, time.Minute)
					}
					preferredCount := 1000
					if failedMode == RankModeSpeed && mode == RankModeQuality {
						preferredCount = 5
					}
					counts := []int{}
					for start := 0; start < preferredCount; start += ClientScoreSampleCount {
						page := []*ClientScore{}
						for range min(ClientScoreSampleCount, preferredCount-start) {
							score := onlineBackfillScore(true, 1)
							score.IpFamilies = ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
							if preferredCount == 5 {
								validUntil := server.NowUtc().Add(time.Hour)
								score.EgressValidUntil = &validUntil
								score.PassesMinimums = map[RankMode]bool{mode: true}
								allowed[score.ClientId] = true
							} else {
								score.NetworkOnly = true
							}
							page = append(page, score)
						}
						writeSubscriberFactsForScores(ctx, page)
						pipe.Set(ctx, clientScoreLocationFacetSampleKey(false, mode, locationId, server.Id{}, ipFamilyFacetDualstack, len(counts)), gobEncodeForTest(t, page), time.Minute)
						counts = append(counts, len(page))
					}
					pipe.Set(ctx, clientScoreLocationFacetCountsKey(false, mode, locationId, server.Id{}, ipFamilyFacetDualstack), gobEncodeForTest(t, counts), time.Minute)
					fallbackCounts := []int{}
					if mode == failedMode {
						fallbackCounts = []int{20}
						// The failed page is not reached by the initial 1,000-row
						// sample; only post-filter refill sees this real GET error.
						failedKey := clientScoreLocationFacetSampleKey(false, mode, locationId, server.Id{}, ipFamilyFacetV4Only, 0)
						pipe.RPush(ctx, failedKey, "synthetic-non-string")
						pipe.Expire(ctx, failedKey, time.Minute)
					}
					pipe.Set(ctx, clientScoreLocationFacetCountsKey(false, mode, locationId, server.Id{}, ipFamilyFacetV4Only), gobEncodeForTest(t, fallbackCounts), time.Minute)
				}
				pipe.SAdd(ctx, providerHardExclusionsKey, providerHardExclusionsReadyMember)
				_, err := pipe.Exec(ctx)
				server.Raise(err)
			})
			source := "primary"
			if failedMode == RankModeSpeed {
				source = "alternate"
			}
			labels := map[string]string{"rank_mode": failedMode, "source": source, "outcome": "unavailable"}
			before := selectionMetricCount(t, "urnetwork_findproviders2_native_source_outcomes_total", labels)
			clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "refill-read-test", false, false))
			result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &locationId}}}, clientSession)
			if err != nil || result == nil || len(result.Providers) != len(allowed) {
				t.Fatalf("refill failure changed validated fallback supply: mode=%s err=%v", failedMode, err)
			}
			if after := selectionMetricCount(t, "urnetwork_findproviders2_native_source_outcomes_total", labels); after != before+1 {
				t.Fatal("failed refill was not explicitly unavailable")
			}
			for _, provider := range result.Providers {
				if !allowed[provider.ClientId] {
					t.Fatal("optional refill failure leaked an unvalidated provider")
				}
			}
			assertEgressTestNoRepeats(t, result.Providers)
		}
	})
}

// A published count whose page expired remains a visible miss even when a
// later permitted page supplies the answer. The cursor cannot reread that
// missing page or recover its already-spent row budget.
func TestClientScoreMissingPagesConsumeBudgetWithoutRefetch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		locationId := server.NewId()
		fallback := []*ClientScore{}
		for range 20 {
			fallback = append(fallback, onlineBackfillScore(true, 1))
		}
		server.Redis(ctx, func(r server.RedisClient) {
			pipe := r.Pipeline()
			pipe.Set(ctx, clientScoreLocationFacetCountsKey(false, RankModeQuality, locationId, server.Id{}, ipFamilyFacetDualstack), gobEncodeForTest(t, []int{200, 200, 200, 200, 200}), time.Minute)
			pipe.Set(ctx, clientScoreLocationFacetCountsKey(false, RankModeQuality, locationId, server.Id{}, ipFamilyFacetV4Only), gobEncodeForTest(t, []int{20}), time.Minute)
			pipe.Set(ctx, clientScoreLocationFacetSampleKey(false, RankModeQuality, locationId, server.Id{}, ipFamilyFacetV4Only, 0), gobEncodeForTest(t, fallback), time.Minute)
			_, err := pipe.Exec(ctx)
			server.Raise(err)
		})
		observation := &findProviders2LoadObservation{}
		scores, cursor, err := loadClientScoresWithCursor(false, RankModeQuality, ctx,
			map[server.Id]bool{locationId: true}, nil, server.Id{}, 1000,
			[]ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}, observation)
		if err != nil || len(scores) != 0 || cursor == nil || cursor.readCount != 1000 || observation.missingPages != 5 {
			t.Fatalf("missing-page boundary lost: returned=%d misses=%d err=%v", len(scores), observation.missingPages, err)
		}
		scores, err = cursor.read(ctx, 20)
		if err != nil || len(scores) != 20 || cursor.readCount != 1020 || observation.missingPages != 5 || cursor.hasMore() {
			t.Fatalf("refill reset missing-page evidence or budget: returned=%d misses=%d rows=%d err=%v", len(scores), observation.missingPages, cursor.readCount, err)
		}
	})
}
