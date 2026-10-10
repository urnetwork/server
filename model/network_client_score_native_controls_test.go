// Adjacent publication, fallback, and census boundaries use complete
// generations and explicit source failure, never shuffled discovery luck.
package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The real SQL-to-cache publisher emits separate native and online-union
// populations. One accepted success may qualify; ten is not an admission gate.
func TestNativePublisherSeparatesGateCountsFromOnlineUnion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := testing_healthGateCity(ctx, t)
		clientIds := testing_connectQualifyingProviders(ctx, t, city, 3)
		testing_setProviderEgressHealth(ctx, clientIds[0], 1, 1)
		testing_setProviderEgressHealth(ctx, clientIds[1], 0, 1)
		testing_rollUpEgress(ctx)
		caller := countryCodeLocationIds()[city.CountryCode]
		if caller == (server.Id{}) {
			t.Fatal("fixture caller country was not resolved")
		}
		network := GetNetworkClientNetwork(ctx, clientIds[0])
		if network == nil {
			t.Fatal("fixture native network was not created")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO exclude_network_client_location (network_id, client_location_id) VALUES ($1, $2)`, *network, caller))
		})
		if err := UpdateClientScores(ctx, time.Hour, 1); err != nil {
			t.Fatal(err)
		}
		census, err := GetClientScoreNativeCensus(ctx)
		if err != nil || census == nil {
			t.Fatalf("complete publisher omitted generation-bound census: %v", err)
		}
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			scores, cursor, available, err := loadNativeClientScoresWithCursor(ctx, mode, map[server.Id]bool{city.LocationId: true}, nil, server.Id{}, 100, ipFamilyFacets, nil)
			if err != nil || !available || len(scores) != 1 || scores[clientIds[0]] == nil || cursor.hasMore() {
				t.Fatalf("native publication includes online dilution or excludes 1/1: mode=%s available=%t count=%d err=%v", mode, available, len(scores), err)
			}
			if count := census.Buckets[mode]; count.Providers != 1 || count.Denominators["one"] != 1 {
				t.Fatalf("native census does not match selected-policy admission: mode=%s count=%+v", mode, count)
			}
			filtered, filteredCursor, available, err := loadNativeClientScoresWithCursor(ctx, mode, map[server.Id]bool{city.LocationId: true}, nil, caller, 100, ipFamilyFacets, nil)
			if err != nil || !available || len(filtered) != 0 || filteredCursor.hasMore() {
				t.Fatalf("caller-native override leaked an excluded network or lost empty completion: mode=%s count=%d err=%v", mode, len(filtered), err)
			}
			server.Redis(ctx, func(r server.RedisClient) {
				key := clientScoreNativeKey(clientScoreLocationCountsKey(false, mode, city.LocationId, server.Id{}))
				manifest, _, err := readClientScoreNativeManifest(ctx, r, key, r.Get(ctx, key).Val())
				if err != nil || manifest == nil || manifest.PublicationId != census.PublicationId || !manifest.SourceCompletedAt.Equal(census.SourceCompletedAt) {
					t.Fatalf("target publication does not belong to census generation: %v", err)
				}
			})
		}
		if count := census.Buckets["online"]; count.Providers != 3 || count.Denominators["zero"] != 1 || count.Denominators["one"] != 2 {
			t.Fatalf("online census summed overlapping targets or invented evidence: %+v", count)
		}
		if ratio := census.EgressRatio; ratio == nil || ratio.UnavailableReason != "" || *ratio.PublicOnline != (ClientScoreNativeEgressRatioCount{Providers: 3, Observed: 2, Passed: 1, Failed: 1, NoEvidence: 1}) || *ratio.SourceMap != (ClientScoreNativeEgressRatioCount{Providers: 2, Observed: 2, Passed: 1, Failed: 1}) {
			t.Fatalf("publisher lost ratio-only source populations: %+v", ratio)
		}
		if scores := testing_selectableClientScores(ctx, t, city, false); len(scores) != 3 {
			t.Fatalf("native publication changed legacy online availability: %d", len(scores))
		}
	})
}

// Target overlap and private-network membership cannot inflate a public
// census. These are eight-hour evidence bands, not four-hour scheduler counts.
func TestNativeCensusDeduplicatesTargetsAndPreservesDenominators(t *testing.T) {
	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	completedAt := startedAt.Add(time.Minute)
	scores := map[server.Id]*ClientScore{}
	health := map[server.Id]ProviderEgressHealthCounts{}
	for _, total := range []int{0, 1, 2, 3, 4, 5, 9, 10} {
		score := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		score.PassesMinimums = map[RankMode]bool{RankModeQuality: total > 0, RankModeSpeed: total > 0}
		scores[score.ClientId] = score
		health[score.ClientId] = ProviderEgressHealthCounts{Total: total, OKCount: total}
	}
	private := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
	private.NetworkOnly = true
	scores[private.ClientId] = private
	census := newClientScoreNativeCensus(startedAt, completedAt, startedAt, health,
		map[server.Id]map[server.Id]*ClientScore{server.NewId(): scores, server.NewId(): scores},
		map[server.Id]map[server.Id]*ClientScore{server.NewId(): scores})
	for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
		count := census.Buckets[mode]
		if count.Providers != 7 || count.Denominators["zero"] != 0 || count.Denominators["one"] != 1 || count.Denominators["two"] != 1 || count.Denominators["three_to_four"] != 2 || count.Denominators["five_to_nine"] != 2 || count.Denominators["ten_plus"] != 1 {
			t.Fatalf("census changed public native membership or denominator bands: mode=%s count=%+v", mode, count)
		}
	}
	if count := census.Buckets["online"]; count.Providers != 8 || count.Denominators["zero"] != 1 {
		t.Fatalf("census lost zero-history online membership or summed target overlap: %+v", count)
	}
	if !census.SourceStartedAt.Equal(startedAt) || !census.SourceCompletedAt.Equal(completedAt) || !census.PublishedAt.IsZero() {
		t.Fatal("census replaced source time with delivery time")
	}
}

// A real complete empty census is zero; absent or malformed publication is
// unknown. Repeated reads never make old source timestamps fresh.
func TestNativeCensusDistinguishesZeroUnknownAndFreshness(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		if census, err := GetClientScoreNativeCensus(ctx); err != nil || census != nil {
			t.Fatalf("missing census was not unknown: value=%v err=%v", census, err)
		}
		startedAt := server.NowUtc().Add(-time.Hour)
		census := newClientScoreNativeCensus(startedAt, startedAt.Add(time.Minute), startedAt, nil)
		if err := writeClientScoreNativeCensus(ctx, census, time.Hour); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			read, err := GetClientScoreNativeCensus(ctx)
			if err != nil || read == nil || read.PublicationId != census.PublicationId || !read.SourceStartedAt.Equal(startedAt) || !read.PublishedAt.Equal(census.PublishedAt) {
				t.Fatalf("read refreshed source provenance or lost complete zero: %v", err)
			}
			for _, count := range read.Buckets {
				if count.Providers != 0 {
					t.Fatal("empty complete publication manufactured native supply")
				}
			}
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, clientScoreNativeCensusKey, `{}`, time.Hour).Err())
		})
		if census, err := GetClientScoreNativeCensus(ctx); err == nil || census != nil {
			t.Fatalf("malformed census became healthy zero: value=%v err=%v", census, err)
		}
	})
}

// Exhausted requested natives do not authorize skipping the other native
// bucket, even when that union begins with thousands of online-only rows.
func TestNativeFindProvidersUsesOtherNativesBeforeOnline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		location := server.NewId()
		scores := map[ipFamilyFacet][]*ClientScore{}
		wanted := map[server.Id]int{}
		for range 5 {
			score := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
			scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
			wanted[score.ClientId] = 0
		}
		for range 15 {
			score := nativeTestScore(RankModeSpeed, ipFamilyFacetV4Only)
			scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
			wanted[score.ClientId] = egressTestBackfillOffset()
		}
		for range 6000 {
			score := onlineBackfillScore(true, 1)
			score.IpFamilies = ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
			scores[ipFamilyFacetDualstack] = append(scores[ipFamilyFacetDualstack], score)
		}
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			nativeTestPublishLocation(t, location, mode, scores)
		}
		clientSession := testingCreateProviderSearchSession(t.Context(), session.NewByJwt(server.NewId(), server.NewId(), "native-other-test", false, false))
		result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}}, clientSession)
		if err != nil || result == nil || len(result.Providers) != 20 {
			t.Fatalf("other-native refill did not fill after primary exhaustion: result=%v err=%v", result, err)
		}
		for _, provider := range result.Providers {
			tier, ok := wanted[provider.ClientId]
			if !ok || provider.Tier != tier {
				t.Fatalf("online fallback bypassed other natives: expected_native=%t tier=%d", ok, provider.Tier)
			}
		}
		assertEgressTestNoRepeats(t, result.Providers)
	})
}

// An unknown optional native source preserves validated primary results and
// uses online fallback without pretending that native supply is exhausted.
func TestNativeFindProvidersUnavailableOtherSourceKeepsPrimary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		location := server.NewId()
		scores := map[ipFamilyFacet][]*ClientScore{}
		for range 5 {
			scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], nativeTestScore(RankModeQuality, ipFamilyFacetV4Only))
		}
		for range 40 {
			scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], onlineBackfillScore(true, 1))
		}
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			nativeTestPublishLocation(t, location, mode, scores)
		}
		server.Redis(t.Context(), func(r server.RedisClient) {
			key := clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeSpeed, location, server.Id{}))
			server.Raise(r.Set(t.Context(), key, "synthetic-incomplete-pointer", time.Hour).Err())
		})
		clientSession := testingCreateProviderSearchSession(t.Context(), session.NewByJwt(server.NewId(), server.NewId(), "native-unavailable-test", false, false))
		result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}}, clientSession)
		if err != nil || result == nil || len(result.Providers) != 20 {
			t.Fatalf("unknown other-native source blocked validated online availability: result=%v err=%v", result, err)
		}
		for index, provider := range result.Providers {
			wantTier := 0
			if 5 <= index {
				wantTier = 2 * egressTestBackfillOffset()
			}
			if provider.Tier != wantTier {
				t.Fatal("degraded fallback displaced higher-priority native supply")
			}
		}
	})
}

// A delayed cursor cannot read a newer generation under a reused fixed slot.
func TestNativeCursorRejectsReusedGeneration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, location := t.Context(), server.NewId()
		first := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		nativeTestPublishLocation(t, location, RankModeQuality, map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {first}})
		_, cursor, available, err := loadNativeClientScoresWithCursor(ctx, RankModeQuality, map[server.Id]bool{location: true}, nil, server.Id{}, 0, ipFamilyFacets, nil)
		if err != nil || !available || !cursor.hasMore() {
			t.Fatalf("fixture did not retain unread native page: %v", err)
		}
		for range 2 {
			next := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
			nativeTestPublishLocation(t, location, RankModeQuality, map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {next}})
		}
		if scores, err := cursor.read(ctx, 20); !errors.Is(err, errClientScoreNativeUnavailable) || len(scores) != 0 {
			t.Fatalf("reused slot mixed generations or became exhaustion: count=%d err=%v", len(scores), err)
		}
	})
}

// Replaying the exact Redis command after a lost acknowledgement succeeds;
// another writer or an incomplete page still cannot publish this generation.
func TestNativePublicationCommitIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		server.Redis(t.Context(), func(r server.RedisClient) {
			key := clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, server.NewId(), server.Id{}))
			publication, err := beginClientScoreNativePublication(t.Context(), r, key, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			manifest := clientScoreNativeManifest{Generation: publication.generation, Facets: map[ipFamilyFacet][]clientScoreNativePageInfo{}}
			for _, facet := range ipFamilyFacets {
				manifest.Facets[facet] = []clientScoreNativePageInfo{}
			}
			if err := publication.commit(t.Context(), r, manifest); err != nil {
				t.Fatal(err)
			}
			wire, err := r.HGet(t.Context(), publication.slotKey, "m").Bytes()
			if err != nil {
				t.Fatal(err)
			}
			pointer := r.Get(t.Context(), key).Val()
			result, err := r.Eval(t.Context(), clientScoreNativeCommitScript, []string{key, publication.slotKey}, publication.expected, publication.generation, time.Hour.Milliseconds(), wire, pointer).Int()
			if err != nil || result != 1 {
				t.Fatalf("acknowledgement retry rejected an already complete generation: result=%d err=%v", result, err)
			}
		})
	})
}

// The legacy tail must be released before a second bounded native writer
// starts. The flush barrier is never itself emitted as a Redis key operation.
func TestNativeExportBarrierReleasesLegacyTail(t *testing.T) {
	var retained []clientScoreRedisSet
	executions := 0
	err := runClientScoreExportStream(t.Context(), 8, 1024, 1,
		func(emit func(clientScoreRedisSet) error) error {
			if err := emit(clientScoreRedisSet{key: "synthetic-legacy", value: []byte("payload")}); err != nil {
				return err
			}
			if executions != 0 {
				return fmt.Errorf("fixture failed to retain the legacy tail")
			}
			if err := emit(clientScoreRedisSet{flush: true}); err != nil {
				return err
			}
			if executions != 1 || len(retained) != 1 || retained[0].key != "" || retained[0].value != nil {
				return fmt.Errorf("native publication began with retained legacy payloads")
			}
			return nil
		},
		func(batch []clientScoreRedisSet) error {
			executions++
			retained = batch
			if len(batch) != 1 || batch[0].flush || batch[0].key != "synthetic-legacy" {
				return fmt.Errorf("flush barrier became a Redis payload")
			}
			return nil
		},
		func(context.Context, int) error { return errors.New("unexpected retry") },
	)
	if err != nil || executions != 1 {
		t.Fatalf("native export exceeded its single payload budget: executions=%d err=%v", executions, err)
	}
}

// Both old select branches returned nil for this exact state: context canceled
// and the joined worker channel closed without a queued error. Publication must
// not depend on which ready select case happens to win.
func TestNativeExportCancellationCannotAdvanceCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	workerErrs := make(chan error)
	close(workerErrs)
	published := false
	err := finishClientScoreExport(ctx, workerErrs)
	if err == nil {
		published = true
	}
	if published || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled export advanced native completion: published=%t err=%v", published, err)
	}
	lastPublished := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	census := &ClientScoreNativeCensus{PublishedAt: lastPublished}
	if err := writeClientScoreNativeCensus(ctx, census, time.Hour); !errors.Is(err, context.Canceled) || !census.PublishedAt.Equal(lastPublished) {
		t.Fatalf("canceled census advanced freshness: unchanged=%t err=%v", census.PublishedAt.Equal(lastPublished), err)
	}
}

// A complete healthy export still publishes; a joined error remains visible
// alongside cancellation rather than disappearing behind the context branch.
func TestNativeExportCompletionPreservesWorkerErrorsAndHealthyControl(t *testing.T) {
	healthy := make(chan error)
	close(healthy)
	if err := finishClientScoreExport(t.Context(), healthy); err != nil {
		t.Fatalf("healthy complete export became unavailable: %v", err)
	}
	workerErr := errors.New("synthetic incomplete target")
	failed := make(chan error, 1)
	failed <- workerErr
	close(failed)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := finishClientScoreExport(ctx, failed); !errors.Is(err, workerErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("export finalization discarded its failure boundary: %v", err)
	}
}
