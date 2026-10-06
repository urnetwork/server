// Native-first ordering and cache-failure availability are separate contracts:
// unknown is never exhausted, but usable same-target fallback must still answer.
package model

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
)

// Installed only on the test environment's Redis client. The exact request key
// triggers once, after a successful read, without a sleep or production seam.
type nativeCancelReadHook struct {
	key      string
	cancel   context.CancelFunc
	canceled atomic.Bool
}

func (self *nativeCancelReadHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *nativeCancelReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (self *nativeCancelReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		err := next(ctx, commands)
		if err == nil {
			for _, command := range commands {
				if command.Name() == "get" && 1 < len(command.Args()) && command.Args()[1] == self.key && self.canceled.CompareAndSwap(false, true) {
					self.cancel()
				}
			}
		}
		return err
	}
}

// The additive native diagnostic must not invalidate the deployed monitor's
// existing selection-schema-2 witness or require traffic to advertise support.
func TestNativeSelectionSchemaRetainsMonitorCompatibility(t *testing.T) {
	if got := selectionMetricCount(t, "urnetwork_findproviders2_selection_schema_version", nil); got != 2 {
		t.Fatalf("native availability changed the supported selection schema: %g", got)
	}
	if got := selectionMetricCount(t, "urnetwork_findproviders2_native_source_schema_version", nil); got != 1 {
		t.Fatalf("quiet native-source diagnostics have no schema witness: %g", got)
	}
}

func nativeTestChangeSchema(t testing.TB, location server.Id, mode RankMode, state string) {
	t.Helper()
	if state == "complete" || state == "absent" {
		return
	}
	server.Redis(t.Context(), func(r server.RedisClient) {
		key := clientScoreNativeKey(clientScoreLocationCountsKey(false, mode, location, server.Id{}))
		if state == "corrupt" {
			server.Raise(r.Set(t.Context(), key, "synthetic-incomplete-pointer", time.Hour).Err())
			return
		}
		if state != "bootstrap" {
			t.Fatal("unsupported synthetic native source state")
		}
		keys := []string{key}
		for _, caller := range countryCodeLocationIds() {
			keys = append(keys, clientScoreNativeKey(clientScoreLocationCountsKey(false, mode, location, caller)))
		}
		server.Raise(r.Del(t.Context(), keys...).Err())
	})
}

// Both requested modes exercise the real selector. A bounded legacy source
// can prove exhaustion; an unread legacy remainder or damaged native source
// cannot. A full 20-provider response must still expose that difference.
func TestNativeFindProvidersUnavailableSourcesPreserveAvailability(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		for _, requested := range []RankMode{RankModeQuality, RankModeSpeed} {
			alternate, _ := backfillRankMode(requested)
			for _, testCase := range []struct {
				name, primary, other, primaryOutcome, otherOutcome string
				primaryCount, otherCount, onlineCount, wantCount   int
			}{
				{name: "primary_missing_alternate_native", primary: "absent", other: "complete", otherCount: 20, onlineCount: 40, wantCount: 20, primaryOutcome: "unavailable", otherOutcome: "quota"},
				{name: "optional_missing_primary_and_online", primary: "complete", other: "absent", primaryCount: 5, onlineCount: 40, wantCount: 20, primaryOutcome: "exhausted", otherOutcome: "unavailable"},
				{name: "corrupt_primary_alternate_native", primary: "corrupt", other: "complete", otherCount: 20, onlineCount: 40, wantCount: 20, primaryOutcome: "unavailable", otherOutcome: "quota"},
				{name: "both_corrupt_online", primary: "corrupt", other: "corrupt", onlineCount: 40, wantCount: 20, primaryOutcome: "unavailable", otherOutcome: "unavailable"},
				{name: "bootstrap_complete_legacy", primary: "bootstrap", other: "bootstrap", onlineCount: 40, wantCount: 20, primaryOutcome: "exhausted", otherOutcome: "exhausted"},
				{name: "bootstrap_unread_legacy", primary: "bootstrap", other: "bootstrap", onlineCount: 4000, wantCount: 20, primaryOutcome: "unavailable", otherOutcome: "unavailable"},
				{name: "all_missing", primary: "absent", other: "absent", primaryOutcome: "unavailable", otherOutcome: "unavailable"},
				{name: "both_corrupt_empty", primary: "corrupt", other: "corrupt", primaryOutcome: "unavailable", otherOutcome: "unavailable"},
				{name: "complete_empty", primary: "complete", other: "complete", primaryOutcome: "exhausted", otherOutcome: "exhausted"},
			} {
				location := server.NewId()
				scores := map[ipFamilyFacet][]*ClientScore{}
				nativeTiers := map[server.Id]int{}
				for _, tier := range []struct {
					mode  RankMode
					count int
					tier  int
				}{{requested, testCase.primaryCount, 0}, {alternate, testCase.otherCount, egressTestBackfillOffset()}} {
					for range tier.count {
						score := nativeTestScore(tier.mode, ipFamilyFacetV4Only)
						scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
						nativeTiers[score.ClientId] = tier.tier
					}
				}
				for range testCase.onlineCount {
					scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], onlineBackfillScore(true, 1))
				}
				for _, source := range []struct {
					mode  RankMode
					state string
				}{{requested, testCase.primary}, {alternate, testCase.other}} {
					if source.state != "absent" {
						nativeTestPublishLocation(t, location, source.mode, scores)
						nativeTestChangeSchema(t, location, source.mode, source.state)
					}
				}
				type metricCase struct {
					labels map[string]string
					before float64
					want   float64
				}
				metrics := []metricCase{}
				for _, source := range []struct {
					mode            RankMode
					source, outcome string
				}{{requested, "primary", testCase.primaryOutcome}, {alternate, "alternate", testCase.otherOutcome}} {
					for _, outcome := range []string{"quota", "exhausted", "unavailable"} {
						labels := map[string]string{"rank_mode": source.mode, "source": source.source, "outcome": outcome}
						metric := metricCase{labels: labels, before: selectionMetricCount(t, "urnetwork_findproviders2_native_source_outcomes_total", labels)}
						if source.outcome == outcome {
							metric.want = 1
						}
						metrics = append(metrics, metric)
					}
				}
				clientSession := testingCreateProviderSearchSession(t.Context(), jwt.NewByJwt(server.NewId(), server.NewId(), "native-availability-test", false, false))
				result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: requested}, clientSession)
				if err != nil || result == nil || len(result.Providers) != testCase.wantCount {
					t.Fatalf("native source unavailability blocked same-target fallback: case=%s mode=%s want=%d result=%v err=%v", testCase.name, requested, testCase.wantCount, result, err)
				}
				for index, provider := range result.Providers {
					tier, native := nativeTiers[provider.ClientId]
					if index < testCase.primaryCount+testCase.otherCount {
						if !native || provider.Tier != tier {
							t.Fatalf("degraded fallback displaced validated native priority: case=%s mode=%s index=%d", testCase.name, requested, index)
						}
					} else if native || provider.Tier != 2*egressTestBackfillOffset() {
						t.Fatalf("online fallback lost its lower priority: case=%s mode=%s index=%d", testCase.name, requested, index)
					}
				}
				assertEgressTestNoRepeats(t, result.Providers)
				for _, metric := range metrics {
					if after := selectionMetricCount(t, "urnetwork_findproviders2_native_source_outcomes_total", metric.labels); after != metric.before+metric.want {
						t.Fatalf("native source state became false exhaustion or lost degraded observability: case=%s labels=%v delta=%g want=%g", testCase.name, metric.labels, after-metric.before, metric.want)
					}
				}
			}
		}
	})
}

// The page batch and multi-target manifest readers preserve independently
// verified pages when a sibling is damaged; corrupted rows never enter output.
func TestNativeFindProvidersPartialSourceKeepsValidatedPages(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		for _, failure := range []string{"page", "target"} {
			location := server.NewId()
			scores := map[ipFamilyFacet][]*ClientScore{}
			allowed := map[server.Id]bool{}
			for index := range 400 {
				score := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
				scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
				if failure == "target" || ClientScoreSampleCount <= index {
					allowed[score.ClientId] = true
				}
			}
			nativeTestPublishLocation(t, location, RankModeQuality, scores)
			server.Redis(t.Context(), func(r server.RedisClient) {
				for _, facet := range ipFamilyFacets {
					server.Raise(r.Del(t.Context(), clientScoreLocationFacetCountsKey(false, RankModeQuality, location, server.Id{}, facet)).Err())
				}
				if failure == "page" {
					key := clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, location, server.Id{}))
					pointer, err := r.Get(t.Context(), key).Result()
					if err != nil {
						t.Fatal(err)
					}
					_, slotKey, err := readClientScoreNativeManifest(t.Context(), r, key, pointer)
					if err != nil {
						t.Fatal(err)
					}
					server.Raise(r.HSet(t.Context(), slotKey, clientScoreNativePageField(ipFamilyFacetV4Only, 0), "synthetic-invalid-page").Err())
				}
			})
			args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, ForceCount: true, Count: 5}
			if failure == "target" {
				missing := server.NewId()
				args.Specs = append(args.Specs, &ProviderSpec{LocationId: &missing})
			}
			labels := map[string]string{"rank_mode": RankModeQuality, "source": "primary", "outcome": "unavailable"}
			before := selectionMetricCount(t, "urnetwork_findproviders2_native_source_outcomes_total", labels)
			clientSession := testingCreateProviderSearchSession(t.Context(), jwt.NewByJwt(server.NewId(), server.NewId(), "native-partial-test", false, false))
			result, err := FindProviders2(args, clientSession)
			if err != nil || result == nil || len(result.Providers) != 5 {
				t.Fatalf("unavailable sibling discarded validated native pages: failure=%s result=%v err=%v", failure, result, err)
			}
			for _, provider := range result.Providers {
				if !allowed[provider.ClientId] || provider.Tier != 0 {
					t.Fatal("unavailable sibling admitted an unverified page or changed native priority")
				}
			}
			if after := selectionMetricCount(t, "urnetwork_findproviders2_native_source_outcomes_total", labels); after != before+1 {
				t.Fatal("partial native answer hid source unavailability")
			}
		}
	})
}

// Degraded availability never relaxes request/security filters or turns their
// own backend errors and caller cancellation into a successful fallback result.
func TestNativeFindProvidersDegradedFallbackKeepsFiltersAndErrors(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		location := server.NewId()
		allowed := map[server.Id]bool{}
		scores := map[ipFamilyFacet][]*ClientScore{}
		for range 5 {
			score := onlineBackfillScore(true, 1)
			scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
			allowed[score.ClientId] = true
		}
		hard, network, family, explicit := onlineBackfillScore(true, 1), onlineBackfillScore(true, 1), onlineBackfillScore(true, 1), onlineBackfillScore(true, 1)
		network.NetworkOnly = true
		family.IpFamilies = ClientScoreIpFamilyV6
		scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], hard, network, family, explicit)
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			nativeTestPublishLocation(t, location, mode, scores)
			nativeTestChangeSchema(t, location, mode, "corrupt")
		}
		server.Redis(t.Context(), func(r server.RedisClient) {
			server.Raise(r.SAdd(t.Context(), providerHardExclusionsKey, hard.ClientId.String()).Err())
		})
		args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, ExcludeClientIds: []server.Id{explicit.ClientId}}
		clientSession := testingCreateProviderSearchSession(t.Context(), jwt.NewByJwt(server.NewId(), server.NewId(), "native-filter-failure-test", false, false))
		result, err := FindProviders2(args, clientSession)
		if err != nil || result == nil || len(result.Providers) != len(allowed) {
			t.Fatalf("degraded fallback lost eligible online providers: result=%v err=%v", result, err)
		}
		for _, provider := range result.Providers {
			if !allowed[provider.ClientId] || provider.Tier != 2*egressTestBackfillOffset() {
				t.Fatal("degraded fallback bypassed a hard, network, family or explicit exclusion")
			}
		}
		server.Redis(t.Context(), func(r server.RedisClient) {
			server.Raise(r.Del(t.Context(), providerHardExclusionsKey).Err())
			server.Raise(r.Set(t.Context(), providerHardExclusionsKey, "synthetic-wrong-type", time.Minute).Err())
		})
		if result, err := FindProviders2(args, clientSession); err == nil || result != nil {
			t.Fatal("native availability fallback swallowed a hard-exclusion read error")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		clientSession = testingCreateProviderSearchSession(ctx, jwt.NewByJwt(server.NewId(), server.NewId(), "native-canceled-test", false, false))
		var canceledResult *FindProviders2Result
		var canceledErr error
		// Redis's pre-canceled PING retains the done sentinel and context
		// cause. The router and HandleError still own the original panic.
		raised := server.HandleError(func() { canceledResult, canceledErr = FindProviders2(args, clientSession) })
		raisedErr, _ := raised.(error)
		if !errors.Is(raisedErr, server.DbContextDoneError) || !errors.Is(raisedErr, context.Canceled) || canceledResult != nil || canceledErr != nil {
			t.Fatalf("pre-canceled request changed the owning Redis cancellation contract: raised=%v error=%v", raised, canceledErr)
		}

		server.Redis(t.Context(), func(r server.RedisClient) {
			server.Raise(r.Del(t.Context(), providerHardExclusionsKey).Err())
		})
		inflightLocation := server.NewId()
		nativeTestPublishLocation(t, inflightLocation, RankModeQuality, map[ipFamilyFacet][]*ClientScore{
			ipFamilyFacetV4Only: {nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)},
		})
		inflightCtx, inflightCancel := context.WithCancel(t.Context())
		defer inflightCancel()
		hook := &nativeCancelReadHook{
			key:    clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, inflightLocation, server.Id{})),
			cancel: inflightCancel,
		}
		server.Redis(t.Context(), func(r server.RedisClient) { r.AddHook(hook) })
		labels := map[string]string{"target_kind": "location_unknown", "request_class": "default_minimum", "ip_family": "any", "rank_mode": RankModeQuality, "outcome": "canceled", "reason": "load_primary"}
		before := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels)
		clientSession = testingCreateProviderSearchSession(inflightCtx, jwt.NewByJwt(server.NewId(), server.NewId(), "native-inflight-canceled-test", false, false))
		result, err = FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &inflightLocation}}}, clientSession)
		if !hook.canceled.Load() || !errors.Is(err, context.Canceled) || result != nil {
			t.Fatalf("in-flight native read cancellation became fallback: hook=%t err=%v", hook.canceled.Load(), err)
		}
		if after := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels); after != before+1 {
			t.Fatal("in-flight native read cancellation lost its terminal diagnosis")
		}
		for _, stage := range []string{"load_primary", "hard_exclusions", "filter", "load_backfill"} {
			if active := selectionMetricCount(t, "urnetwork_findproviders2_stage_inflight", map[string]string{"stage": stage}); active != 0 {
				t.Fatalf("canceled selection retained %g active %s stages", active, stage)
			}
		}
	})
}
