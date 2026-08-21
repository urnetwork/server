package model

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func authAttemptRedisCardinality(t testing.TB, ctx context.Context, key string) int64 {
	t.Helper()
	var count int64
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		count, err = r.ZCard(ctx, key).Result()
		if err != nil {
			t.Fatalf("ZCARD %q: %v", key, err)
		}
	})
	return count
}

func authAttemptRedisTTL(t testing.TB, ctx context.Context, key string) time.Duration {
	t.Helper()
	var ttl time.Duration
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		ttl, err = r.PTTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("PTTL %q: %v", key, err)
		}
	})
	return ttl
}

func authAttemptRedisEntries(t testing.TB, ctx context.Context, key string) []redis.Z {
	t.Helper()
	var entries []redis.Z
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		entries, err = r.ZRangeWithScores(ctx, key, 0, -1).Result()
		if err != nil {
			t.Fatalf("ZRANGE %q: %v", key, err)
		}
	})
	return entries
}

func authAttemptDatabaseRowCount(t testing.TB, ctx context.Context) int {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT COUNT(*) FROM user_auth_attempt`)
		server.WithPgResult(result, err, func() {
			if !result.Next() {
				t.Fatal("user_auth_attempt count returned no row")
			}
			server.Raise(result.Scan(&count))
		})
	})
	return count
}

func authAttemptTestSession(ctx context.Context, address string) *session.ClientSession {
	return session.NewLocalClientSession(ctx, address, nil)
}

func redisHashTag(key string) string {
	start := strings.IndexByte(key, '{')
	end := strings.IndexByte(key, '}')
	if start < 0 || end <= start {
		return ""
	}
	return key[start+1 : end]
}

func TestUserAuthAttemptGlobalAndAddressLimitsAreCountBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sessionA := authAttemptTestSession(ctx, "203.0.113.8:41001")
		defer sessionA.Cancel()
		sessionB := authAttemptTestSession(ctx, "203.0.113.24:41002")
		defer sessionB.Cancel()
		sessionC := authAttemptTestSession(ctx, "203.0.113.40:41003")
		defer sessionC.Cancel()

		settings := userAuthAttemptSettings{
			addressLookback: 2 * time.Minute,
			addressLimit:    3,
			globalLookback:  10 * time.Minute,
			globalLimit:     5,
		}
		userAuth := "bounded@example.com"
		now := server.NowUtc()

		attempt := func(clientSession *session.ClientSession, offset time.Duration) (UserAuthAttemptId, bool) {
			return userAuthAttemptAt(&userAuth, clientSession, now.Add(offset), settings)
		}

		tokenA, allow := attempt(sessionA, 0)
		if !allow {
			t.Fatal("first address-A attempt was rejected")
		}
		_, allow = attempt(sessionA, time.Millisecond)
		if !allow {
			t.Fatal("second address-A attempt was rejected")
		}
		tokenB, allow := attempt(sessionB, 2*time.Millisecond)
		if !allow {
			t.Fatal("first address-B attempt was rejected")
		}
		_, allow = attempt(sessionB, 3*time.Millisecond)
		if !allow {
			t.Fatal("second address-B attempt was rejected")
		}
		tokenC, allow := attempt(sessionC, 4*time.Millisecond)
		if allow {
			t.Fatal("fifth global attempt was allowed")
		}

		// Continued rejected attempts rotate the oldest members rather than
		// growing either set beyond its threshold.
		for i := 0; i < 12; i++ {
			_, allow = attempt(sessionA, time.Duration(5+i)*time.Millisecond)
			if allow {
				t.Fatalf("attempt %d after the global threshold was allowed", i)
			}
		}

		if got := authAttemptRedisCardinality(t, ctx, tokenA.addressRedisKey); got != int64(settings.addressLimit) {
			t.Fatalf("address-A history size = %d, want %d", got, settings.addressLimit)
		}
		if got := authAttemptRedisCardinality(t, ctx, tokenB.addressRedisKey); got != 2 {
			t.Fatalf("address-B history size = %d, want 2", got)
		}
		if got := authAttemptRedisCardinality(t, ctx, tokenC.addressRedisKey); got != 1 {
			t.Fatalf("address-C history size = %d, want 1", got)
		}
		if got := authAttemptRedisCardinality(t, ctx, tokenA.globalRedisKey); got != int64(settings.globalLimit) {
			t.Fatalf("global history size = %d, want %d", got, settings.globalLimit)
		}
		if ttl := authAttemptRedisTTL(t, ctx, tokenA.addressRedisKey); ttl <= 0 || settings.addressLookback+time.Second < ttl {
			t.Fatalf("thresholded address history TTL = %v, want at most %v", ttl, settings.addressLookback)
		}
		if ttl := authAttemptRedisTTL(t, ctx, tokenA.globalRedisKey); ttl <= 0 || settings.globalLookback+time.Second < ttl {
			t.Fatalf("thresholded global history TTL = %v, want at most %v", ttl, settings.globalLookback)
		}

		// Identity-less flows retain the prior address-only behavior and are
		// independently capped at the address threshold.
		var anonymousToken UserAuthAttemptId
		for i := 0; i < settings.addressLimit; i++ {
			var anonymousAllowed bool
			anonymousToken, anonymousAllowed = userAuthAttemptAt(
				nil,
				sessionC,
				now.Add(time.Duration(30+i)*time.Millisecond),
				settings,
			)
			if anonymousAllowed != (i < settings.addressLimit-1) {
				t.Fatalf("anonymous attempt %d allow = %t, want %t", i+1, anonymousAllowed, i < settings.addressLimit-1)
			}
		}
		if anonymousToken.globalRedisKey != "" {
			t.Fatalf("anonymous attempt unexpectedly has global key %q", anonymousToken.globalRedisKey)
		}
		if got := authAttemptRedisCardinality(t, ctx, anonymousToken.addressRedisKey); got != int64(settings.addressLimit) {
			t.Fatalf("anonymous history size = %d, want %d", got, settings.addressLimit)
		}

		if got := authAttemptDatabaseRowCount(t, ctx); got != 0 {
			t.Fatalf("Redis limiter wrote %d legacy database rows", got)
		}
	})
}

func TestUserAuthAttemptHistoriesAreTimeBoundedAndOrdered(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := authAttemptTestSession(ctx, "198.51.100.8:42001")
		defer clientSession.Cancel()

		settings := userAuthAttemptSettings{
			addressLookback: 2 * time.Second,
			addressLimit:    4,
			globalLookback:  5 * time.Second,
			globalLimit:     6,
		}
		userAuth := "time-bounded@example.com"
		now := server.NowUtc()

		token, allow := userAuthAttemptAt(&userAuth, clientSession, now, settings)
		if !allow {
			t.Fatal("first attempt was rejected")
		}

		addressTTL := authAttemptRedisTTL(t, ctx, token.addressRedisKey)
		if addressTTL <= settings.addressLookback-time.Second || settings.addressLookback+time.Second < addressTTL {
			t.Fatalf("address history TTL = %v, want approximately %v", addressTTL, settings.addressLookback)
		}
		globalTTL := authAttemptRedisTTL(t, ctx, token.globalRedisKey)
		if globalTTL <= settings.globalLookback-time.Second || settings.globalLookback+time.Second < globalTTL {
			t.Fatalf("global history TTL = %v, want approximately %v", globalTTL, settings.globalLookback)
		}

		// Moving logical time past the address lookback prunes its old member,
		// while the longer global history still retains it.
		_, allow = userAuthAttemptAt(&userAuth, clientSession, now.Add(3*time.Second), settings)
		if !allow {
			t.Fatal("attempt after address lookback was rejected")
		}
		if got := authAttemptRedisCardinality(t, ctx, token.addressRedisKey); got != 1 {
			t.Fatalf("address history retained expired entry: size = %d, want 1", got)
		}
		if got := authAttemptRedisCardinality(t, ctx, token.globalRedisKey); got != 2 {
			t.Fatalf("global history size = %d, want 2", got)
		}
		globalEntries := authAttemptRedisEntries(t, ctx, token.globalRedisKey)
		if len(globalEntries) != 2 || globalEntries[0].Score >= globalEntries[1].Score {
			t.Fatalf("global history is not ordered by time: %+v", globalEntries)
		}

		// At eleven seconds both earlier global members have expired, so only
		// the new attempt remains. Its score is the set's absolute expiry.
		logicalNow := now.Add(11 * time.Second)
		_, allow = userAuthAttemptAt(&userAuth, clientSession, logicalNow, settings)
		if !allow {
			t.Fatal("attempt after global expiration was rejected")
		}
		globalEntries = authAttemptRedisEntries(t, ctx, token.globalRedisKey)
		if len(globalEntries) != 1 {
			t.Fatalf("global history retained expired entries: size = %d, want 1", len(globalEntries))
		}
		if globalEntries[0].Score != float64(logicalNow.Add(settings.globalLookback).UnixMilli()) {
			t.Fatalf("global score = %.0f, want expiry %d", globalEntries[0].Score, logicalNow.Add(settings.globalLookback).UnixMilli())
		}
	})
}

func TestUserAuthAttemptSuccessClearsGlobalAndCurrentAddressOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sessionA := authAttemptTestSession(ctx, "192.0.2.8:43001")
		defer sessionA.Cancel()
		sessionB := authAttemptTestSession(ctx, "192.0.2.24:43002")
		defer sessionB.Cancel()

		settings := userAuthAttemptSettings{
			addressLookback: time.Minute,
			addressLimit:    5,
			globalLookback:  5 * time.Minute,
			globalLimit:     10,
		}
		userAuth := "success@example.com"
		now := server.NowUtc()

		tokenA, _ := userAuthAttemptAt(&userAuth, sessionA, now, settings)
		tokenA, _ = userAuthAttemptAt(&userAuth, sessionA, now.Add(time.Millisecond), settings)
		tokenB, _ := userAuthAttemptAt(&userAuth, sessionB, now.Add(2*time.Millisecond), settings)
		tokenB, _ = userAuthAttemptAt(&userAuth, sessionB, now.Add(3*time.Millisecond), settings)

		SetUserAuthAttemptSuccess(ctx, tokenA, false)
		if got := authAttemptRedisCardinality(t, ctx, tokenA.globalRedisKey); got != 4 {
			t.Fatalf("unsuccessful update changed global history: size = %d, want 4", got)
		}

		SetUserAuthAttemptSuccess(ctx, tokenA, true)
		if got := authAttemptRedisCardinality(t, ctx, tokenA.globalRedisKey); got != 0 {
			t.Fatalf("successful auth left %d global entries", got)
		}
		if got := authAttemptRedisCardinality(t, ctx, tokenA.addressRedisKey); got != 0 {
			t.Fatalf("successful auth left %d current-address entries", got)
		}
		if got := authAttemptRedisCardinality(t, ctx, tokenB.addressRedisKey); got != 2 {
			t.Fatalf("successful auth changed other-address history: size = %d, want 2", got)
		}

		// Identity-less flows have only an address history; success resets it.
		anonymousToken, _ := userAuthAttemptAt(nil, sessionA, now, settings)
		if anonymousToken.globalRedisKey != "" {
			t.Fatalf("anonymous attempt unexpectedly has global key %q", anonymousToken.globalRedisKey)
		}
		SetUserAuthAttemptSuccess(ctx, anonymousToken, true)
		if got := authAttemptRedisCardinality(t, ctx, anonymousToken.addressRedisKey); got != 0 {
			t.Fatalf("anonymous success left %d address entries", got)
		}
	})
}

func TestUserAuthAttemptKeysArePrivateAndClusterCoLocated(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sessionA := authAttemptTestSession(ctx, "203.0.113.72:44001")
		defer sessionA.Cancel()
		sessionB := authAttemptTestSession(ctx, "203.0.113.88:44002")
		defer sessionB.Cancel()
		userA := "private-a@example.com"
		userB := "private-b@example.com"

		tokenAA, allow := UserAuthAttempt(&userA, sessionA)
		if !allow {
			t.Fatal("first user-A attempt was rejected")
		}
		tokenAB, _ := UserAuthAttempt(&userA, sessionB)
		tokenBA, _ := UserAuthAttempt(&userB, sessionA)

		for _, key := range []string{
			tokenAA.addressRedisKey,
			tokenAA.globalRedisKey,
			tokenAB.addressRedisKey,
			tokenBA.addressRedisKey,
			tokenBA.globalRedisKey,
		} {
			if strings.Contains(key, userA) || strings.Contains(key, userB) {
				t.Fatalf("Redis key exposes raw user auth: %q", key)
			}
		}

		userATag := redisHashTag(tokenAA.globalRedisKey)
		if userATag == "" || redisHashTag(tokenAA.addressRedisKey) != userATag || redisHashTag(tokenAB.addressRedisKey) != userATag {
			t.Fatalf("user-A keys are not in one Redis Cluster slot: %q, %q, %q", tokenAA.globalRedisKey, tokenAA.addressRedisKey, tokenAB.addressRedisKey)
		}
		if redisHashTag(tokenBA.globalRedisKey) == userATag {
			t.Fatal("different users share a Redis Cluster hash tag")
		}
	})
}

func TestUserAuthAttemptUpdatesAreAtomicAtThreshold(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := authAttemptTestSession(ctx, "198.51.100.40:45001")
		defer clientSession.Cancel()
		settings := userAuthAttemptSettings{
			addressLookback: time.Minute,
			addressLimit:    8,
			globalLookback:  time.Minute,
			globalLimit:     8,
		}
		userAuth := "concurrent@example.com"
		now := server.NowUtc()

		const attemptCount = 32
		var allowed atomic.Int64
		var firstToken UserAuthAttemptId
		var tokenLock sync.Mutex
		var waitGroup sync.WaitGroup
		waitGroup.Add(attemptCount)
		for i := 0; i < attemptCount; i++ {
			go func() {
				defer waitGroup.Done()
				token, allow := userAuthAttemptAt(&userAuth, clientSession, now, settings)
				if allow {
					allowed.Add(1)
				}
				tokenLock.Lock()
				if firstToken.addressRedisKey == "" {
					firstToken = token
				}
				tokenLock.Unlock()
			}()
		}
		waitGroup.Wait()

		// The current attempt is included in the threshold, preserving the
		// prior limiter's behavior: attempt eight is the first rejected one.
		if got := allowed.Load(); got != int64(settings.addressLimit-1) {
			t.Fatalf("allowed concurrent attempts = %d, want %d", got, settings.addressLimit-1)
		}
		if got := authAttemptRedisCardinality(t, ctx, firstToken.addressRedisKey); got != int64(settings.addressLimit) {
			t.Fatalf("atomic address history size = %d, want %d", got, settings.addressLimit)
		}
		if got := authAttemptRedisCardinality(t, ctx, firstToken.globalRedisKey); got != int64(settings.globalLimit) {
			t.Fatalf("atomic global history size = %d, want %d", got, settings.globalLimit)
		}
	})
}
