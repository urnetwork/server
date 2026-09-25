package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// Every key is generated for this local TestEnv owner; no global Redis hook,
// server outage, live address, provider identity or request traffic is used.
func clientScoreReadFailureFixture(t testing.TB, mode string) (map[server.Id]*ClientScore, error, error, server.Id) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	locationId, candidateId := server.NewId(), server.NewId()
	callerId := server.Id{}
	if strings.HasPrefix(mode, "alias") {
		callerId = server.NewId()
	}
	legacy := clientScoreLocationCountsKey(false, RankModeQuality, locationId, callerId)
	v4Counts := clientScoreLocationFacetCountsKey(false, RankModeQuality, locationId, callerId, ipFamilyFacetV4Only)
	v4Sample := clientScoreLocationFacetSampleKey(false, RankModeQuality, locationId, callerId, ipFamilyFacetV4Only, 0)
	dualCounts := clientScoreLocationFacetCountsKey(false, RankModeQuality, locationId, callerId, ipFamilyFacetDualstack)
	dualSample := clientScoreLocationFacetSampleKey(false, RankModeQuality, locationId, callerId, ipFamilyFacetDualstack, 0)
	alias := clientScoreLocationAliasKey(false, RankModeQuality, locationId, callerId)
	baselineCounts := clientScoreLocationFacetCountsKey(false, RankModeQuality, locationId, server.Id{}, ipFamilyFacetV4Only)
	baselineSample := clientScoreLocationFacetSampleKey(false, RankModeQuality, locationId, server.Id{}, ipFamilyFacetV4Only, 0)
	keys := []string{legacy, v4Counts, v4Sample, dualCounts, dualSample, alias, baselineCounts, baselineSample}
	defer server.Redis(ctx, func(client server.RedisClient) {
		pipe := client.Pipeline()
		for _, key := range keys {
			pipe.Del(ctx, key)
		}
		_, err := pipe.Exec(ctx)
		server.Raise(err)
	})
	facets := []ipFamilyFacet{ipFamilyFacetV4Only}
	var expected error
	server.Redis(ctx, func(client server.RedisClient) {
		set := func(key string, value any) {
			t.Helper()
			server.Raise(client.Set(ctx, key, gobEncodeForTest(t, value), time.Minute).Err())
		}
		valid := func(countsKey, sampleKey string) {
			t.Helper()
			set(countsKey, []int{1})
			set(sampleKey, []*ClientScore{clientScoreForTest(candidateId, ClientScoreIpFamilyV4)})
		}
		var failedKey, earlierMissingKey string
		switch mode {
		case "counts-pipeline":
			failedKey = legacy
		case "counts-command":
			failedKey, earlierMissingKey = v4Counts, legacy
		case "samples-pipeline":
			set(v4Counts, []int{1})
			failedKey = v4Sample
		case "samples-command":
			facets = []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}
			set(dualCounts, []int{1})
			set(v4Counts, []int{1})
			failedKey, earlierMissingKey = v4Sample, dualSample
		case "partial-samples":
			facets = []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}
			valid(dualCounts, dualSample)
			set(v4Counts, []int{1})
			failedKey = v4Sample
		case "alias-command":
			valid(v4Counts, v4Sample)
			failedKey, earlierMissingKey = alias, legacy
		case "missing":
			// Truly missing keys retain the existing empty-cache policy.
		case "encoded-empty":
			set(v4Counts, []int{})
		case "valid":
			valid(v4Counts, v4Sample)
		case "alias-baseline":
			server.Raise(client.Set(ctx, alias, clientScoreAliasBaselineValue, time.Minute).Err())
			valid(baselineCounts, baselineSample)
		case "alias-fallback":
			server.Raise(client.Set(ctx, alias, clientScoreAliasBaselineValue, time.Minute).Err())
			valid(v4Counts, v4Sample)
		default:
			t.Fatal("unknown synthetic read fixture mode")
		}
		if failedKey == "" {
			return
		}
		// GET on a list yields a real Redis command error after PING succeeds.
		server.Raise(client.RPush(ctx, failedKey, "synthetic-non-string").Err())
		server.Raise(client.Expire(ctx, failedKey, time.Minute).Err())
		expected = client.Get(ctx, failedKey).Err()
		if expected == nil || !strings.HasPrefix(expected.Error(), "WRONGTYPE ") {
			t.Fatal("fixture did not establish the intended Redis read error")
		}
		if earlierMissingKey != "" {
			pipe := client.Pipeline()
			missing := pipe.Get(ctx, earlierMissingKey)
			failed := pipe.Get(ctx, failedKey)
			_, err := pipe.Exec(ctx)
			if !errors.Is(err, redis.Nil) || !errors.Is(missing.Err(), redis.Nil) || !errors.Is(failed.Err(), expected) {
				t.Fatal("fixture did not establish a missing first key masking a later command error")
			}
		}
	})
	scores, err := loadClientScores(false, RankModeQuality, ctx,
		map[server.Id]bool{locationId: true}, map[server.Id]bool{}, callerId, 100, facets)
	return scores, err, expected, candidateId
}

func TestClientScoreReadRejectsCountPipelineFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		scores, err, expected, _ := clientScoreReadFailureFixture(t, "counts-pipeline")
		if expected == nil || !errors.Is(err, expected) || scores != nil {
			t.Fatalf("count-read failure became a successful empty/partial pool: error_preserved=%t pool_nil=%t size=%d", errors.Is(err, expected), scores == nil, len(scores))
		}
	})
}

func TestClientScoreReadRejectsMaskedCountFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		scores, err, expected, _ := clientScoreReadFailureFixture(t, "counts-command")
		if expected == nil || !errors.Is(err, expected) || scores != nil {
			t.Fatalf("earlier missing key hid a later count-read failure: error_preserved=%t pool_nil=%t size=%d", errors.Is(err, expected), scores == nil, len(scores))
		}
	})
}

func TestClientScoreReadRejectsSamplePipelineFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		scores, err, expected, _ := clientScoreReadFailureFixture(t, "samples-pipeline")
		if expected == nil || !errors.Is(err, expected) || scores != nil {
			t.Fatalf("sample-read failure became a successful empty pool: error_preserved=%t pool_nil=%t size=%d", errors.Is(err, expected), scores == nil, len(scores))
		}
	})
}

func TestClientScoreReadRejectsMaskedSampleFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		scores, err, expected, _ := clientScoreReadFailureFixture(t, "samples-command")
		if expected == nil || !errors.Is(err, expected) || scores != nil {
			t.Fatalf("earlier missing sample hid a later sample-read failure: error_preserved=%t pool_nil=%t size=%d", errors.Is(err, expected), scores == nil, len(scores))
		}
	})
}

func TestClientScoreReadRejectsPartialPoolAfterSampleFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		scores, err, expected, _ := clientScoreReadFailureFixture(t, "partial-samples")
		if expected == nil || !errors.Is(err, expected) || scores != nil {
			t.Fatalf("a healthy sample concealed an independent read failure: error_preserved=%t pool_nil=%t size=%d", errors.Is(err, expected), scores == nil, len(scores))
		}
	})
}

func TestClientScoreReadRejectsAliasFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		scores, err, expected, _ := clientScoreReadFailureFixture(t, "alias-command")
		if expected == nil || !errors.Is(err, expected) || scores != nil {
			t.Fatalf("failed alias read silently selected legacy fallback: error_preserved=%t pool_nil=%t size=%d", errors.Is(err, expected), scores == nil, len(scores))
		}
	})
}

func TestClientScoreReadRetainsMissingKeyControl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		scores, err, expected, _ := clientScoreReadFailureFixture(t, "missing")
		if err != nil || expected != nil || len(scores) != 0 {
			t.Fatalf("genuine cache absence changed: error=%v pool_size=%d", err, len(scores))
		}
	})
}

func TestClientScoreReadRetainsEncodedEmptyControl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		scores, err, expected, _ := clientScoreReadFailureFixture(t, "encoded-empty")
		if err != nil || expected != nil || len(scores) != 0 {
			t.Fatalf("valid encoded-empty facet changed: error=%v pool_size=%d", err, len(scores))
		}
	})
}

func TestClientScoreReadRetainsValidProviderControl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		scores, err, expected, candidateId := clientScoreReadFailureFixture(t, "valid")
		if err != nil || expected != nil || len(scores) != 1 || scores[candidateId] == nil ||
			scores[candidateId].ScaledWeights[RankModeQuality] != 1 {
			t.Fatalf("valid provider pool or weight changed: error=%v pool_size=%d", err, len(scores))
		}
	})
}

func TestClientScoreReadRetainsAliasBaselineControl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		scores, err, expected, candidateId := clientScoreReadFailureFixture(t, "alias-baseline")
		if err != nil || expected != nil || len(scores) != 1 || scores[candidateId] == nil {
			t.Fatalf("successful baseline alias changed: error=%v pool_size=%d", err, len(scores))
		}
	})
}

func TestClientScoreReadRetainsAliasMissingBaselineFallbackControl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		scores, err, expected, candidateId := clientScoreReadFailureFixture(t, "alias-fallback")
		if err != nil || expected != nil || len(scores) != 1 || scores[candidateId] == nil {
			t.Fatalf("missing-baseline caller fallback changed: error=%v pool_size=%d", err, len(scores))
		}
	})
}
