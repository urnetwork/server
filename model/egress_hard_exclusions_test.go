// The hard-exclusion cache must distinguish a published empty set from an
// absent, expired or legacy set. All providers and Redis contents are local
// synthetic fixtures; the actual FindProviders2 path and durable rules run.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The marker is not a provider id. Keeping its wire value here lets the same
// test compile against the pre-fix reader and writer for causal RED.
const hardExclusionReadyMemberForTest = "ready:v1"

// Removes only this fixture's cache key; a database read-through must not
// replace it with a partial set containing only the requested candidates.
func hardExclusionTestClear(ctx context.Context, t testing.TB) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		if err := r.Del(ctx, providerHardExclusionsKey).Err(); err != nil {
			t.Fatal(err)
		}
	})
}

// A missing cache cannot bypass a current dark or TLS verdict on the direct
// path, including force_minimum. The connected healthy sibling stays usable.
func TestProviderHardExclusionsMissingCacheKeepsDirectSafety(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Harbor Test City", "Example Region", "Exampleland", "zz")
		healthy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		dark := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		tls := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestBlackhole(ctx, dark.clientId)
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: tls.clientId, MeasuredAt: server.NowUtc(),
			OKCount: 1, Total: 1, TLSAuthenticationFailure: true,
		})
		hardExclusionTestClear(ctx, t)
		for _, forceMinimum := range []bool{false, true} {
			for _, clientId := range []server.Id{dark.clientId, tls.clientId, healthy.clientId} {
				providers := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &clientId}},
					RankModeQuality, 1, forceMinimum, server.NewId())
				want := 0
				if clientId == healthy.clientId {
					want = 1
				}
				if len(providers) != want {
					t.Errorf("direct hard-exclusion boundary force_minimum=%t healthy=%t: got %d, want %d",
						forceMinimum, clientId == healthy.clientId, len(providers), want)
				}
			}
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if count, err := r.Exists(ctx, providerHardExclusionsKey).Result(); err != nil || count != 0 {
				t.Fatalf("candidate-only read published an incomplete global cache: exists=%d err=%v", count, err)
			}
		})
	})
}

// An old writer can publish a nonempty set without the readiness marker. It
// must not hide a newer durable exclusion absent from that legacy snapshot.
func TestProviderHardExclusionsLegacyCacheReadsThrough(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		dark, tls, healthy := server.NewId(), server.NewId(), server.NewId()
		egressTestBlackhole(ctx, dark)
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: tls, MeasuredAt: server.NowUtc(), Total: 1, TLSAuthenticationFailure: true,
		})
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.SAdd(ctx, providerHardExclusionsKey, dark.String()).Err(); err != nil {
				t.Fatal(err)
			}
		})
		excluded, err := getProviderHardExclusions(ctx, []server.Id{dark, tls, healthy})
		if err != nil || len(excluded) != 2 || !excluded[dark] || !excluded[tls] || excluded[healthy] {
			t.Fatalf("legacy cache was treated as complete: dark=%t tls=%t healthy=%t count=%d err=%v",
				excluded[dark], excluded[tls], excluded[healthy], len(excluded), err)
		}
	})
}

// Even an empty count pass publishes evidence of its complete exclusion set,
// with the same expiry as a nonempty pass. Key absence cannot mean both states.
func TestProviderHardExclusionsEmptyPublicationIsObservable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		hardExclusionTestClear(ctx, t)
		if err := UpdateClientLocations(ctx, time.Hour); err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			members, err := r.SMembers(ctx, providerHardExclusionsKey).Result()
			if err != nil || len(members) != 1 || members[0] != hardExclusionReadyMemberForTest {
				t.Fatalf("empty publication is indistinguishable from missing: members=%d err=%v", len(members), err)
			}
			ttl, err := r.TTL(ctx, providerHardExclusionsKey).Result()
			if err != nil || ttl <= 0 || time.Hour < ttl {
				t.Fatalf("empty publication lost its bounded expiry: ttl=%s err=%v", ttl, err)
			}
		})
	})
}

// A successfully marked cache keeps its existing one-count-pass snapshot
// contract. A candidate missing from this authoritative set is not a cache
// error, and a cache hit remains an exclusion.
func TestProviderHardExclusionsMarkedSnapshotControl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		excludedId, allowedId := server.NewId(), server.NewId()
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.SAdd(ctx, providerHardExclusionsKey,
				hardExclusionReadyMemberForTest, excludedId.String()).Err(); err != nil {
				t.Fatal(err)
			}
		})
		excluded, err := getProviderHardExclusions(ctx, []server.Id{excludedId, allowedId, excludedId})
		if err != nil || len(excluded) != 1 || !excluded[excludedId] || excluded[allowedId] {
			t.Fatalf("valid snapshot membership changed: count=%d err=%v", len(excluded), err)
		}
		hardExclusionTestClear(ctx, t)
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.SAdd(ctx, providerHardExclusionsKey, hardExclusionReadyMemberForTest).Err(); err != nil {
				t.Fatal(err)
			}
		})
		excluded, err = getProviderHardExclusions(ctx, []server.Id{allowedId})
		if err != nil || len(excluded) != 0 {
			t.Fatalf("authoritative empty snapshot was rejected: count=%d err=%v", len(excluded), err)
		}
	})
}

// Read-through uses the same age/span/count and TLS policy as the exporter.
// A stale dark check expires, a pass dominates retained streak columns, and
// a positive health TLS bit does not expire solely because its row is old.
func TestProviderHardExclusionsMissingCacheUsesExactRules(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		rules := GetProviderEgressRules()
		freshDark, staleDark, passed, firstFailure := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		oldTls, clean, unknown := server.NewId(), server.NewId(), server.NewId()
		Testing_SetProviderBlackholed(ctx, freshDark, now)
		Testing_SetProviderBlackholed(ctx, staleDark, now.Add(-ProviderBlackholeCheckMaxAge-time.Minute))
		Testing_SetProviderBlackholed(ctx, passed, now)
		server.Tx(ctx, func(tx server.PgTx) {
			// Reproduce a legacy four-column writer, retaining old streak state.
			server.RaisePgResult(tx.Exec(ctx,
				"UPDATE provider_blackhole_check SET ok=true, failure='' WHERE client_id=$1", passed))
		})
		firstFailedAt := now
		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: firstFailure, CheckedAt: now, Failure: "all_destinations_failed",
			ConsecutiveFailures: 1, FirstFailedAt: &firstFailedAt,
		})
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: oldTls, MeasuredAt: now.Add(-30 * 24 * time.Hour),
			Total: 1, TLSAuthenticationFailure: true,
		})
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: clean, MeasuredAt: now, OKCount: 1, Total: 1,
		})
		hardExclusionTestClear(ctx, t)
		excluded, err := getProviderHardExclusions(ctx,
			[]server.Id{freshDark, staleDark, passed, firstFailure, oldTls, clean, unknown})
		if err != nil {
			t.Fatal(err)
		}
		if rules.DarkConsecutiveFailures <= 1 {
			t.Fatal("fixture requires the ordinary multi-check dark rule")
		}
		if len(excluded) != 2 || !excluded[freshDark] || !excluded[oldTls] {
			t.Fatalf("read-through changed durable hard-exclusion rules: count=%d dark=%t old_tls=%t",
				len(excluded), excluded[freshDark], excluded[oldTls])
		}
	})
}

// A wrong Redis type is a backend error, not evidence of an empty set. Empty
// candidate requests still need no cache or database read.
func TestProviderHardExclusionsCacheErrorStaysError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.Set(ctx, providerHardExclusionsKey, "synthetic-wrong-type", time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
		})
		excluded, err := getProviderHardExclusions(ctx, []server.Id{server.NewId()})
		if err == nil || len(excluded) != 0 {
			t.Fatal("a cache command failure became usable exclusion evidence")
		}
		excluded, err = getProviderHardExclusions(ctx, nil)
		if err != nil || len(excluded) != 0 {
			t.Fatal("empty candidates unnecessarily read the malformed cache")
		}
	})
}

// More than one bounded database chunk and repeated candidate ids must not
// miss the tail or return providers outside the exact requested population.
func TestProviderHardExclusionsReadThroughCandidateScope(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientIds := make([]server.Id, 530)
		for i := range clientIds {
			clientIds[i] = server.NewId()
		}
		outside := server.NewId()
		for _, clientId := range []server.Id{clientIds[0], clientIds[len(clientIds)-1], outside} {
			egressTestBlackhole(ctx, clientId)
		}
		clientIds = append(clientIds, clientIds[0], clientIds[len(clientIds)-1])
		hardExclusionTestClear(ctx, t)
		excluded, err := getProviderHardExclusions(ctx, clientIds)
		if err != nil || len(excluded) != 2 || !excluded[clientIds[0]] || !excluded[clientIds[529]] || excluded[outside] {
			t.Fatalf("candidate-scoped read-through lost or widened its population: count=%d err=%v", len(excluded), err)
		}
	})
}

// A genuinely missing verdict is not a dark verdict. Cold-cache read-through
// must preserve successful healthy requests instead of globally failing shut.
func TestProviderHardExclusionsMissingHealthyControl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		hardExclusionTestClear(ctx, t)
		ids := []server.Id{server.NewId(), server.NewId()}
		excluded, err := getProviderHardExclusions(ctx, ids)
		if err != nil || len(excluded) != 0 {
			t.Fatalf("missing healthy evidence changed policy: count=%d err=%v", len(excluded), err)
		}
	})
}
