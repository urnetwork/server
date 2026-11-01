package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// On real Redis, a failed five-minute fill, also one whose caller went away
// mid-fill, holds its key for the failure fence only, and published and warmed
// values carry the jittered expiry. The fence script cuts only its own token's
// lease, only toward expiry, and never recreates an expired lease.
func TestHandlerCacheFenceNativeRedis(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		store := redisHandlerCacheStore{}
		id := server.NewId().String()
		failedKey := "fence-native-failed-" + id
		canceledKey := "fence-native-canceled-" + id
		publishedKey := "fence-native-published-" + id
		warmedKey := "fence-native-warmed-" + id
		leaseKeys := newHandlerCacheKeys("fence-native-lease-" + id)
		cleanup := []string{leaseKeys.value, leaseKeys.fill}
		for _, key := range []string{failedKey, canceledKey, publishedKey, warmedKey} {
			keys := newHandlerCacheKeys(key)
			cleanup = append(cleanup, keys.value, keys.fill)
		}
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			_ = handlerCacheRedisOperation(cleanupCtx, func(client server.RedisClient) error {
				for _, key := range cleanup {
					if err := client.Del(cleanupCtx, key).Err(); err != nil {
						return err
					}
				}
				return nil
			})
		}()
		pttl := func(key string) time.Duration {
			var remaining time.Duration
			if err := handlerCacheRedisOperation(ctx, func(client server.RedisClient) error {
				var err error
				remaining, err = client.PTTL(ctx, key).Result()
				return err
			}); err != nil {
				t.Fatalf("pttl %s: %v", key, err)
			}
			return remaining
		}

		ttl := 5 * time.Minute
		fence := handlerCacheFailureFenceTtl(ttl)
		if _, err := cacheJson(ctx, store, failedKey, ttl, func() (*handlerCacheTestResult, error) {
			return nil, errors.New("synthetic fill failure")
		}); err == nil {
			t.Fatal("failed fill returned no error")
		}
		if remaining := pttl(newHandlerCacheKeys(failedKey).fill); remaining <= 0 || fence < remaining {
			t.Errorf("failed fill holds its key for %v, want at most the %v failure fence", remaining, fence)
		}
		callerCtx, cancelCaller := context.WithCancel(ctx)
		defer cancelCaller()
		if _, err := cacheJson(callerCtx, store, canceledKey, ttl, func() (*handlerCacheTestResult, error) {
			cancelCaller()
			return nil, callerCtx.Err()
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled fill = %v, want context.Canceled", err)
		}
		if remaining := pttl(newHandlerCacheKeys(canceledKey).fill); remaining <= 0 || fence < remaining {
			t.Errorf("fill whose caller went away holds its key for %v, want at most the %v failure fence", remaining, fence)
		}

		prior := handlerCacheValueTtlJitter
		defer func() { handlerCacheValueTtlJitter = prior }()
		handlerCacheValueTtlJitter = func(span time.Duration) time.Duration { return span }
		valueTtl := 30 * time.Second
		shortest := valueTtl - valueTtl/10
		if _, err := cacheJson(ctx, store, publishedKey, valueTtl, func() (*handlerCacheTestResult, error) {
			return &handlerCacheTestResult{Message: "published"}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := warmCacheJson(ctx, store, warmedKey, valueTtl, true, func() (*handlerCacheTestResult, error) {
			return &handlerCacheTestResult{Message: "warmed"}, nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{publishedKey, warmedKey} {
			if remaining := pttl(newHandlerCacheKeys(key).value); remaining <= shortest-time.Second || shortest < remaining {
				t.Errorf("%s expires in %v, want just under the %v jitter bound", key, remaining, shortest)
			}
		}

		lease := func(token string, ttl time.Duration) {
			if err := handlerCacheRedisOperation(ctx, func(client server.RedisClient) error {
				return client.Set(ctx, leaseKeys.fill, token, ttl).Err()
			}); err != nil {
				t.Fatal(err)
			}
		}
		lease("owner", 2*time.Minute)
		if ok, err := store.fence(ctx, leaseKeys.fill, "owner", fence); err != nil || !ok {
			t.Fatalf("own lease fence = (%v, %v), want applied", ok, err)
		}
		if remaining := pttl(leaseKeys.fill); remaining <= 0 || fence < remaining {
			t.Errorf("fenced lease expires in %v, want within (0, %v]", remaining, fence)
		}
		if token, exists, err := store.get(ctx, leaseKeys.fill); err != nil || !exists || token != "owner" {
			t.Errorf("fence changed the lease to (%q, %v, %v)", token, exists, err)
		}

		lease("replacement", 2*time.Minute)
		if ok, err := store.fence(ctx, leaseKeys.fill, "owner", fence); err != nil || ok {
			t.Errorf("stale owner fence = (%v, %v), want refused", ok, err)
		}
		if remaining := pttl(leaseKeys.fill); remaining <= time.Minute {
			t.Errorf("stale owner cut the replacement's lease to %v", remaining)
		}

		lease("owner", time.Second)
		if ok, err := store.fence(ctx, leaseKeys.fill, "owner", fence); err != nil || !ok {
			t.Errorf("short lease fence = (%v, %v), want applied", ok, err)
		}
		if remaining := pttl(leaseKeys.fill); remaining <= 0 || time.Second < remaining {
			t.Errorf("fence extended a 1s lease to %v", remaining)
		}

		if err := handlerCacheRedisOperation(ctx, func(client server.RedisClient) error {
			return client.Del(ctx, leaseKeys.fill).Err()
		}); err != nil {
			t.Fatal(err)
		}
		if ok, err := store.fence(ctx, leaseKeys.fill, "owner", fence); err != nil || ok {
			t.Errorf("expired lease fence = (%v, %v), want refused", ok, err)
		}
		if _, exists, err := store.get(ctx, leaseKeys.fill); err != nil || exists {
			t.Errorf("fence recreated an expired lease: (%v, %v)", exists, err)
		}
	})
}
