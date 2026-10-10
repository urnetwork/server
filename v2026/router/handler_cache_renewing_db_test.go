package router

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The actual Redis scripts must renew only a still-current token, never revive
// an expired lease, and retain the original atomic publication boundary.
func TestHandlerCacheRenewingNativeRedisOwnership(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		store := redisHandlerCacheStore{}
		keys := newHandlerCacheKeys("renewing-native-" + server.NewId().String())
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			_ = handlerCacheRedisOperation(cleanupCtx, func(client server.RedisClient) error {
				return client.Del(cleanupCtx, keys.value, keys.fill).Err()
			})
		}()
		if ok, err := store.acquire(ctx, keys.fill, "old-token", 30*time.Second); err != nil || !ok {
			t.Fatalf("acquire=(%v,%v)", ok, err)
		}
		if ok, err := store.renew(ctx, keys.fill, "old-token", 30*time.Second); err != nil || !ok {
			t.Fatalf("current renew=(%v,%v)", ok, err)
		}
		if err := store.set(ctx, keys.fill, "replacement-token", 30*time.Second, false); err != nil {
			t.Fatal(err)
		}
		if ok, err := store.renew(ctx, keys.fill, "old-token", 2*time.Minute); err != nil || ok {
			t.Fatalf("old token renewed replacement=(%v,%v)", ok, err)
		}
		if token, exists, err := store.get(ctx, keys.fill); err != nil || !exists || token != "replacement-token" {
			t.Fatalf("replacement token changed=(%q,%v,%v)", token, exists, err)
		}
		if err := handlerCacheRedisOperation(ctx, func(client server.RedisClient) error {
			ttl, err := client.PTTL(ctx, keys.fill).Result()
			if err == nil && (ttl <= 0 || ttl > 30*time.Second) {
				t.Fatalf("stale renewal changed native expiry: %v", ttl)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if ok, err := store.publish(ctx, keys, "old-token", `{"message":"stale"}`, 30*time.Second); err != nil || ok {
			t.Fatalf("stale publication=(%v,%v)", ok, err)
		}
		if ok, err := store.publish(ctx, keys, "replacement-token", `{"message":"current"}`, 30*time.Second); err != nil || !ok {
			t.Fatalf("current publication=(%v,%v)", ok, err)
		}
		if _, exists, err := store.get(ctx, keys.fill); err != nil || exists {
			t.Fatal("successful publication retained its fill token")
		}
		if ok, err := store.acquire(ctx, keys.fill, "expired-token", 30*time.Second); err != nil || !ok {
			t.Fatalf("expiry acquire=(%v,%v)", ok, err)
		}
		if err := handlerCacheRedisOperation(ctx, func(client server.RedisClient) error {
			return client.PExpire(ctx, keys.fill, 0).Err()
		}); err != nil {
			t.Fatal(err)
		}
		if ok, err := store.renew(ctx, keys.fill, "expired-token", 30*time.Second); err != nil || ok {
			t.Fatalf("expired token revived=(%v,%v)", ok, err)
		}
		if _, exists, err := store.get(ctx, keys.fill); err != nil || exists {
			t.Fatal("expiry renewal recreated the key")
		}
	})
}

// A short lease keeps this native cross-owner control bounded. The 70s case and
// exact 30s production policy are covered with virtual time by the paired test.
func TestHandlerCacheRenewingNativeSlowFillCoalesces(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
		defer cancel()
		store := redisHandlerCacheStore{}
		key := "renewing-native-slow-" + server.NewId().String()
		started := make(chan struct{})
		finish := make(chan struct{})
		finished := make(chan handlerCacheTestCallResult, 1)
		var fills atomic.Int64
		go func() {
			value, err := cacheJsonWithRenewingFill(ctx, store, key, 3*time.Second, 10*time.Second,
				func(fillCtx context.Context) (*handlerCacheTestResult, error) {
					fills.Add(1)
					close(started)
					select {
					case <-fillCtx.Done():
						return nil, fillCtx.Err()
					case <-finish:
						return &handlerCacheTestResult{Message: "owner"}, nil
					}
				})
			finished <- handlerCacheTestCallResult{value, err}
		}()
		joined := false
		defer func() {
			cancel()
			if !joined {
				<-finished
			}
		}()
		select {
		case <-started:
		case result := <-finished:
			joined = true
			t.Fatalf("native owner did not start: %v", result.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case <-time.After(3500 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		value, err := cacheJsonWithRenewingFill(ctx, store, key, 3*time.Second, 10*time.Second,
			func(context.Context) (*handlerCacheTestResult, error) {
				fills.Add(1)
				return &handlerCacheTestResult{Message: "duplicate"}, nil
			})
		if value != nil || err == nil || fills.Load() != 1 {
			t.Fatalf("native overlapping fill=(%v,%v), calls=%d", value, err, fills.Load())
		}
		close(finish)
		result := <-finished
		joined = true
		if result.err != nil || result.value == nil || result.value.Message != "owner" {
			t.Fatalf("native owner=(%v,%v)", result.value, result.err)
		}
	})
}

// Run only against the owned isolated Redis fixture. A native paused server
// distinguishes the deadline pool from the ordinary 15s socket policy; a mock
// that merely honors ctx would miss that integration boundary.
func TestHandlerCacheRenewingNativeSocketDeadline(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		store := redisHandlerCacheStore{}
		key := newHandlerCacheKeys("renewing-native-deadline-" + server.NewId().String()).fill
		var pauseUntil time.Time
		defer func() {
			// Join the finite server pause and remove the key on failure too.
			// The deadline pool's native 1s socket cap is shorter than this
			// test's deliberate pause, regardless of the cleanup ctx deadline.
			if remaining := time.Until(pauseUntil); remaining > 0 {
				time.Sleep(remaining)
			}
			cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancelCleanup()
			if err := server.RedisWithDeadline(cleanupCtx, func(client server.RedisClient) error {
				return client.Eval(cleanupCtx, `return redis.call('DEL', KEYS[1])`, []string{key}).Err()
			}); err != nil {
				t.Errorf("deadline fixture cleanup: %v", err)
			}
		}()
		if ok, err := store.acquire(ctx, key, "current", 30*time.Second); err != nil || !ok {
			t.Fatalf("deadline acquire=(%v,%v)", ok, err)
		}
		if ok, err := store.renew(ctx, key, "current", 30*time.Second); err != nil || !ok {
			t.Fatalf("deadline warmup=(%v,%v)", ok, err)
		}
		server.Raise(handlerCacheRedisOperation(ctx, func(client server.RedisClient) error {
			return client.Do(ctx, "CLIENT", "PAUSE", 2500, "ALL").Err()
		}))
		pauseUntil = time.Now().Add(2500 * time.Millisecond)
		callCtx, cancelCall := context.WithTimeout(ctx, 100*time.Millisecond)
		start := time.Now()
		ok, err := store.renew(callCtx, key, "current", 30*time.Second)
		elapsed := time.Since(start)
		cancelCall()
		// This is a deadline-oracle margin below the native 2500ms pause,
		// not a database performance or throughput assertion.
		if ok || err == nil || elapsed >= time.Second {
			t.Fatalf("paused renewal escaped its caller deadline: ok=%v err=%v elapsed=%v", ok, err, elapsed)
		}
	})
}
