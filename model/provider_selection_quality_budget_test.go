package model

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func qualityBudgetFixture(t testing.TB) (server.Id, map[server.Id]bool) {
	t.Helper()
	enableSubscriberQualityPolicy(t)
	nativeTestEnableReader(t)
	previous := providerSubscriberNegativeCache
	providerSubscriberNegativeCache = newSubscriberNegativeCache(subscriberNegativeCapacity, time.Now)
	providerSubscriberNegativeCache.observe = observeSubscriberEligibilityEvent
	t.Cleanup(func() { providerSubscriberNegativeCache = previous })
	egressTestCity(t.Context(), "Quality Budget City", "Quality Budget Region", "United States", "us")
	resetCountryCodeLocationIds()
	location := countryCodeLocationIds()["us"]
	if location == (server.Id{}) {
		t.Fatal("fixture requires the real US best-available target")
	}
	quality := map[ipFamilyFacet][]*ClientScore{}
	for range 1000 {
		quality[ipFamilyFacetV4Only] = append(quality[ipFamilyFacetV4Only], nativeTestScore(RankModeQuality, ipFamilyFacetV4Only))
	}
	speed := map[ipFamilyFacet][]*ClientScore{}
	speedIds := map[server.Id]bool{}
	for range 3 {
		score := nativeTestScore(RankModeSpeed, ipFamilyFacetV4Only)
		speed[ipFamilyFacetV4Only] = append(speed[ipFamilyFacetV4Only], score)
		speedIds[score.ClientId] = true
	}
	nativeTestPublishLocation(t, location, RankModeQuality, quality)
	nativeTestPublishLocation(t, location, RankModeSpeed, speed)
	_ = locationDirectory()
	return location, speedIds
}

func qualityBudgetFind(ctx context.Context, location server.Id, bestAvailable bool) (*FindProviders2Result, error) {
	spec := &ProviderSpec{LocationId: &location}
	if bestAvailable {
		spec = &ProviderSpec{BestAvailable: true}
	}
	clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "quality-budget", false, false))
	return FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{spec}, RankMode: RankModeQuality, Count: 3, ForceCount: true}, clientSession)
}

// A normal count-three request must not validate every member of its 1,000-row
// native sample. Positive results remain fresh reads on both cold and warm calls.
func TestFindProviders2QualityValidatesOnlyNeededCandidates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		location, _ := qualityBudgetFixture(t)
		for _, bestAvailable := range []bool{false, true, false, true} {
			beforeRows := testutil.ToFloat64(subscriberEligibilityEventCounters["negative_miss"])
			beforeBatches := testutil.ToFloat64(subscriberEligibilityEventCounters["sql_batch"])
			result, err := qualityBudgetFind(t.Context(), location, bestAvailable)
			if err != nil || result == nil || len(result.Providers) != 3 {
				t.Fatalf("native count-three request failed: result=%v err=%v", result, err)
			}
			for _, provider := range result.Providers {
				if provider.Tier >= egressTestBackfillOffset() {
					t.Fatal("healthy native Quality request borrowed another bucket")
				}
			}
			rows := testutil.ToFloat64(subscriberEligibilityEventCounters["negative_miss"]) - beforeRows
			batches := testutil.ToFloat64(subscriberEligibilityEventCounters["sql_batch"]) - beforeBatches
			if rows != 3 || batches != 1 {
				t.Fatalf("validated rows=%g batches=%g; want only three weighted candidates in one current-fact batch", rows, batches)
			}
			t.Logf("best_available=%t native_sample=1000 returned=3 subscriber_candidates=%g sql_batches=%g", bestAvailable, rows, batches)
		}
	})
}

// Hold the real subscriber table unavailable. Cancellation of native Quality
// validation must leave the request alive to return same-target native Speed.
func TestFindProviders2QualityBlockedDatabaseKeepsFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		location, speedIds := qualityBudgetFixture(t)
		server.Db(t.Context(), func(conn server.PgConn) {
			held, err := conn.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer held.Rollback(context.WithoutCancel(t.Context()))
			server.RaisePgResult(held.Exec(t.Context(), "LOCK TABLE network_client_location IN ACCESS EXCLUSIVE MODE"))
			for _, bestAvailable := range []bool{false, true} {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				beforeRows := testutil.ToFloat64(subscriberEligibilityEventCounters["negative_miss"])
				started := time.Now()
				result, err := qualityBudgetFind(ctx, location, bestAvailable)
				elapsed, requestErr := time.Since(started), ctx.Err()
				cancel()
				if err != nil || requestErr != nil || result == nil || len(result.Providers) != 3 {
					t.Fatalf("blocked subscriber DB blocked fallback: elapsed=%s result=%v err=%v request=%v", elapsed, result, err, requestErr)
				}
				for _, provider := range result.Providers {
					if !speedIds[provider.ClientId] || provider.Tier != egressTestBackfillOffset() {
						t.Fatal("unvalidated Quality entered the native tier or wrong-target fallback")
					}
				}
				if rows := testutil.ToFloat64(subscriberEligibilityEventCounters["negative_miss"]) - beforeRows; rows != 3 {
					t.Fatalf("blocked request attempted %g subscriber candidates, want 3", rows)
				}
				t.Logf("best_available=%t blocked_pg=true fallback=3 elapsed=%s request_alive=true", bestAvailable, elapsed)
			}
		}, server.OptNoRetry())
	})
}

func TestFindProviders2QualityRefillsRejectedCandidates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		location, _ := qualityBudgetFixture(t)
		scores := map[ipFamilyFacet][]*ClientScore{}
		rejected := []server.Id{}
		for range 6 {
			score := nativeTestScore(RankModeQuality, ipFamilyFacetDualstack)
			scores[ipFamilyFacetDualstack] = append(scores[ipFamilyFacetDualstack], score)
			rejected = append(rejected, score.ClientId)
		}
		for range 1000 {
			scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], nativeTestScore(RankModeQuality, ipFamilyFacetV4Only))
		}
		nativeTestPublishLocation(t, location, RankModeQuality, scores)
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), "UPDATE network_client_location SET arin_quality_verified=false WHERE client_id=ANY($1)", rejected))
		})
		before := testutil.ToFloat64(subscriberEligibilityEventCounters["negative_miss"])
		result, err := qualityBudgetFind(t.Context(), location, false)
		if err != nil || result == nil || len(result.Providers) != 3 {
			t.Fatalf("subscriber refusals prevented native refill: result=%v err=%v", result, err)
		}
		for _, provider := range result.Providers {
			if provider.Tier != 0 || provider.IpFamily != string(connect.IpFamilyV4Only) {
				t.Fatalf("refused preferred-facet provider survived, or native priority lost: %+v", provider)
			}
		}
		if checked := testutil.ToFloat64(subscriberEligibilityEventCounters["negative_miss"]) - before; checked != 9 {
			t.Fatalf("validated %g candidates; want six refused then three accepted", checked)
		}
	})
}

func TestFindProviders2QualityUnavailablePreservesOnlineAndStrictPolicy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		location, _ := qualityBudgetFixture(t)
		nativeTestPublishLocation(t, location, RankModeSpeed, nil)
		strictScore := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		writeOnlineBackfillSample(t.Context(), t, location, RankModeQuality, true, []*ClientScore{strictScore})
		server.Db(t.Context(), func(conn server.PgConn) {
			held, err := conn.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer held.Rollback(context.WithoutCancel(t.Context()))
			server.RaisePgResult(held.Exec(t.Context(), "LOCK TABLE network_client_location IN ACCESS EXCLUSIVE MODE"))
			beforeUnavailable := testutil.ToFloat64(findProviders2NativeSourceOutcomes.WithLabelValues(RankModeQuality, "primary", "unavailable"))
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			result, err := qualityBudgetFind(ctx, location, false)
			if err != nil || result == nil || len(result.Providers) != 3 {
				t.Fatalf("online fallback failed: result=%v err=%v", result, err)
			}
			for _, provider := range result.Providers {
				if provider.Tier != 2*egressTestBackfillOffset() {
					t.Fatal("unvalidated subscriber returned as native")
				}
			}
			if got := testutil.ToFloat64(findProviders2NativeSourceOutcomes.WithLabelValues(RankModeQuality, "primary", "unavailable")) - beforeUnavailable; got != 1 {
				t.Fatalf("native source did not report unavailable: %g", got)
			}
			for _, named := range []bool{false, true} {
				args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: RankModeQuality, Count: 3, ForceCount: true, ForceMinimum: true}
				if named {
					args.Specs = append(args.Specs, &ProviderSpec{ClientId: &strictScore.ClientId})
					args.ForceMinimum = false
				}
				clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "strict-quality-budget", false, false))
				result, err := FindProviders2(args, clientSession)
				if err == nil || result != nil {
					t.Fatalf("strict Quality silently borrowed on unavailable facts: named=%t result=%v err=%v", named, result, err)
				}
			}
			canceled, stop := context.WithCancel(t.Context())
			stop()
			server.HandleError(func() { result, err = qualityBudgetFind(canceled, location, false) }, func(failure error) { result, err = nil, failure })
			if err == nil || result != nil {
				t.Fatal("canceled parent was converted into fallback success")
			}
		}, server.OptNoRetry())
	})
}
