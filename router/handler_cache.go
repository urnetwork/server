package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A key's fill lease has two roles, each with its own limit. While a fill
// runs, it makes overlapping misses wait instead of starting duplicate fills,
// so it must cover a normal fill. After a fill fails, it fences retries. Every
// API process answers a leased key's misses with 503, so a fence as long as
// the fill lease would close a key fleet-wide for minutes after a brief Redis
// or database stall.
const (
	// Bounds a fill in progress. Short-lived entries use their own ttl.
	handlerCacheFillLeaseTtlLimit = 2 * time.Minute
	// Bounds the fence a failed fill leaves. One retry hint still limits a
	// failing key to one fill attempt per fence across the fleet, and a client
	// that honors Retry-After finds the key open again.
	handlerCacheFailureFenceLimit = handlerCacheRetryAfter
	handlerCacheRetryAfter        = 5 * time.Second
	// The fence write runs on failure paths, while Redis may be stalled.
	handlerCacheFenceRedisTimeout = 2 * time.Second
	// A stored value expires up to ttl/divisor early; see handlerCacheValueTtl.
	handlerCacheValueTtlJitterDivisor = 10
)

// Keeps a fill-in-progress lease proportional to the cache lifetime.
func handlerCacheFillLeaseTtl(cacheTtl time.Duration) time.Duration {
	if 0 < cacheTtl && cacheTtl < handlerCacheFillLeaseTtlLimit {
		return cacheTtl
	}
	return handlerCacheFillLeaseTtlLimit
}

// Keeps the failure fence within the lifetime of the data it protects.
func handlerCacheFailureFenceTtl(cacheTtl time.Duration) time.Duration {
	if 0 < cacheTtl && cacheTtl < handlerCacheFailureFenceLimit {
		return cacheTtl
	}
	return handlerCacheFailureFenceLimit
}

// A uniform offset in [0, span]. A test fixes it.
var handlerCacheValueTtlJitter = func(span time.Duration) time.Duration {
	return time.Duration(mathrand.Int64N(int64(span) + 1))
}

// A stored value lives for a uniform point of [ttl - ttl/10, ttl]. Values
// written together, as after a fleet-wide restart, then stop expiring and
// refilling together, and each refill draws again. Jitter only shortens, so no
// value outlives the freshness its caller asked for; it costs about 5% more
// fills.
func handlerCacheValueTtl(ttl time.Duration) time.Duration {
	span := ttl / handlerCacheValueTtlJitterDivisor
	if span <= 0 {
		return ttl
	}
	return ttl - min(max(handlerCacheValueTtlJitter(span), 0), span)
}

// Physical keys share one Redis Cluster hash tag so publishing a value and
// releasing its lease can be one guarded atomic script. The digest also keeps
// caller/network identity out of operational Redis key listings.
type handlerCacheKeys struct {
	value string
	fill  string
}

// Storage is abstracted at the four correctness boundaries so concurrency,
// expiry, ambiguous writes, and failures can be tested without wall clocks.
type handlerCacheStore interface {
	get(context.Context, string) (string, bool, error)
	set(context.Context, string, string, time.Duration, bool) error
	acquire(context.Context, string, string, time.Duration) (bool, error)
	publish(context.Context, handlerCacheKeys, string, string, time.Duration) (bool, error)
	release(context.Context, string, string) error
	fence(context.Context, string, string, time.Duration) (bool, error)
}

// Uses the shared Redis cluster for cross-process value and lease ownership.
type redisHandlerCacheStore struct{}

// Carries the router's existing status-string contract plus the retry hint
// RaiseHttpError discovers through errors.As.
type handlerCacheUnavailableError struct{}

func (self *handlerCacheUnavailableError) Error() string {
	return "503 Cached response is temporarily unavailable."
}

func (self *handlerCacheUnavailableError) RetryAfterSeconds() int {
	return int(handlerCacheRetryAfter / time.Second)
}

var defaultHandlerCacheStore handlerCacheStore = redisHandlerCacheStore{}

// Derives both physical keys from the fully namespaced logical cache key.
func newHandlerCacheKeys(key string) handlerCacheKeys {
	sum := sha256.Sum256([]byte(key))
	hashTag := hex.EncodeToString(sum[:16])
	prefix := fmt.Sprintf("handler_cache:{%s}", hashTag)
	return handlerCacheKeys{
		value: prefix + ":value",
		fill:  prefix + ":fill",
	}
}

// Contains Redis wrapper panics as ordinary cache errors. Cache availability
// must not crash a request handler or expose backend details to its caller.
func handlerCacheRedisOperation(
	ctx context.Context,
	operation func(server.RedisClient) error,
) (returnErr error) {
	server.HandleError(func() {
		server.Redis(ctx, func(client server.RedisClient) {
			returnErr = operation(client)
		})
	}, func(err error) {
		returnErr = err
	})
	return
}

// Distinguishes an absent key from a transport or Redis failure.
func (self redisHandlerCacheStore) get(ctx context.Context, key string) (value string, exists bool, returnErr error) {
	returnErr = handlerCacheRedisOperation(ctx, func(client server.RedisClient) error {
		var err error
		value, err = client.Get(ctx, key).Result()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		exists = true
		return nil
	})
	return
}

// Writes an explicit warm result with either replace or first-writer semantics.
func (self redisHandlerCacheStore) set(
	ctx context.Context,
	key string,
	value string,
	ttl time.Duration,
	onlyIfAbsent bool,
) error {
	return handlerCacheRedisOperation(ctx, func(client server.RedisClient) error {
		if onlyIfAbsent {
			return client.SetNX(ctx, key, value, ttl).Err()
		}
		return client.Set(ctx, key, value, ttl).Err()
	})
}

// Claims one expiring fill attempt across every API process.
func (self redisHandlerCacheStore) acquire(
	ctx context.Context,
	key string,
	token string,
	ttl time.Duration,
) (acquired bool, returnErr error) {
	returnErr = handlerCacheRedisOperation(ctx, func(client server.RedisClient) error {
		var err error
		acquired, err = client.SetNX(ctx, key, token, ttl).Result()
		return err
	})
	return
}

// Publishes only while the caller's unique lease is still current.
func (self redisHandlerCacheStore) publish(
	ctx context.Context,
	keys handlerCacheKeys,
	token string,
	value string,
	ttl time.Duration,
) (published bool, returnErr error) {
	const script = `
		if redis.call('GET', KEYS[2]) ~= ARGV[1] then
			return 0
		end
		redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
		redis.call('DEL', KEYS[2])
		return 1
	`
	returnErr = handlerCacheRedisOperation(ctx, func(client server.RedisClient) error {
		result, err := client.Eval(
			ctx,
			script,
			[]string{keys.value, keys.fill},
			token,
			value,
			max(int64(1), ttl.Milliseconds()),
		).Int64()
		published = result == 1
		return err
	})
	return
}

// Removes a lease only if it still belongs to this request.
func (self redisHandlerCacheStore) release(ctx context.Context, key string, token string) error {
	return handlerCacheRedisOperation(ctx, func(client server.RedisClient) error {
		_, err := server.RedisRemoveIfEqual(client, ctx, key, []byte(token)).Result()
		return err
	})
}

// Cuts a failed owner's lease to the failure fence. Only the token's own lease
// changes, and only toward expiry: a lease due sooner keeps its time, and a
// replacement owner's lease is never touched. The deadline pool bounds the
// call, which runs on failure paths while Redis may be stalled.
func (self redisHandlerCacheStore) fence(
	ctx context.Context,
	key string,
	token string,
	ttl time.Duration,
) (fenced bool, returnErr error) {
	const script = `
		if redis.call('GET', KEYS[1]) ~= ARGV[1] then
			return 0
		end
		local remaining = redis.call('PTTL', KEYS[1])
		if remaining < 0 or tonumber(ARGV[2]) < remaining then
			redis.call('PEXPIRE', KEYS[1], ARGV[2])
		end
		return 1
	`
	operationCtx, cancel := context.WithTimeout(ctx, handlerCacheFenceRedisTimeout)
	defer cancel()
	returnErr = server.RedisWithDeadline(operationCtx, func(client server.RedisClient) error {
		result, err := client.Eval(
			operationCtx,
			script,
			[]string{key},
			token,
			max(int64(1), ttl.Milliseconds()),
		).Int64()
		fenced = result == 1
		return err
	}, server.OptRedisNoPreflight())
	return
}

// Decodes a non-null cached result. Unreadable or stale-schema JSON is a miss;
// JSON null is never a successful handler result.
func getCachedJson[R any](
	ctx context.Context,
	store handlerCacheStore,
	key string,
) (*R, bool, error) {
	cachedValueJson, exists, err := store.get(ctx, key)
	if err != nil || !exists || cachedValueJson == "" {
		return nil, false, err
	}
	var cachedValue *R
	if err := json.Unmarshal([]byte(cachedValueJson), &cachedValue); err != nil || cachedValue == nil {
		return nil, false, nil
	}
	return cachedValue, true, nil
}

// Cuts a lease this request may hold to the failure fence. The caller may be
// gone, so the write does not inherit its cancellation. If Redis cannot take
// the write either, the lease keeps its fill-lease expiry.
func fenceHandlerCacheFill(
	ctx context.Context,
	store handlerCacheStore,
	keys handlerCacheKeys,
	token string,
	ttl time.Duration,
) {
	_, _ = store.fence(context.WithoutCancel(ctx), keys.fill, token, handlerCacheFailureFenceTtl(ttl))
}

// Coordinates one cold fill across every API process sharing Redis. A loser
// rechecks once in case the winner published during acquisition, then fails
// explicitly; it never returns a nil success. Fill and publication failures
// keep the lease, cut to the short failure fence, bounding retry pressure.
func cacheJson[R any](
	ctx context.Context,
	store handlerCacheStore,
	key string,
	ttl time.Duration,
	fill func() (*R, error),
) (*R, error) {
	keys := newHandlerCacheKeys(key)
	if cachedValue, ok, err := getCachedJson[R](ctx, store, keys.value); err != nil {
		return nil, &handlerCacheUnavailableError{}
	} else if ok {
		return cachedValue, nil
	}

	token := server.NewId().String()
	acquired, acquireErr := store.acquire(ctx, keys.fill, token, handlerCacheFillLeaseTtl(ttl))
	if !acquired {
		// SETNX can apply and lose its response; only this unique token proves
		// this request owns that ambiguous acquisition.
		owner, exists, ownerErr := store.get(ctx, keys.fill)
		if ownerErr == nil && exists && owner == token {
			acquired = true
		} else if acquireErr != nil || ownerErr != nil {
			if acquireErr != nil && ownerErr != nil {
				// The lost SETNX may still have set this token: a lease that
				// nobody fills under.
				fenceHandlerCacheFill(ctx, store, keys, token, ttl)
			}
			return nil, &handlerCacheUnavailableError{}
		}
	}
	if !acquired {
		if cachedValue, ok, err := getCachedJson[R](ctx, store, keys.value); err == nil && ok {
			return cachedValue, nil
		}
		return nil, &handlerCacheUnavailableError{}
	}

	// This request owns the lease from here. Every exit that neither publishes
	// nor releases it, a panicking fill included, cuts it to the failure fence.
	settled := false
	defer func() {
		if !settled {
			fenceHandlerCacheFill(ctx, store, keys, token, ttl)
		}
	}()

	// A prior winner can publish between the first miss and this lease. Do not
	// recompute it merely because its old lease already disappeared.
	if cachedValue, ok, err := getCachedJson[R](ctx, store, keys.value); err != nil {
		return nil, &handlerCacheUnavailableError{}
	} else if ok {
		settled = store.release(ctx, keys.fill, token) == nil
		return cachedValue, nil
	}

	value, err := fill()
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, &handlerCacheUnavailableError{}
	}
	valueJson, err := json.Marshal(value)
	if err != nil {
		return nil, &handlerCacheUnavailableError{}
	}
	published, publishErr := store.publish(ctx, keys, token, string(valueJson), handlerCacheValueTtl(ttl))
	if publishErr == nil && published {
		settled = true
		return value, nil
	}

	// A lost script response or an expired lease may still leave a valid value
	// from this or a later winner. Return only that authoritative publication.
	if cachedValue, ok, err := getCachedJson[R](ctx, store, keys.value); err == nil && ok {
		return cachedValue, nil
	}
	return nil, &handlerCacheUnavailableError{}
}

// Explicit warm calls intentionally bypass the miss lease. They are an
// operator-directed refresh boundary, not ordinary read-through traffic.
func warmCacheJson[R any](
	ctx context.Context,
	store handlerCacheStore,
	key string,
	ttl time.Duration,
	force bool,
	fill func() (*R, error),
) (*R, error) {
	value, err := fill()
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, &handlerCacheUnavailableError{}
	}
	valueJson, err := json.Marshal(value)
	if err != nil {
		return nil, &handlerCacheUnavailableError{}
	}
	keys := newHandlerCacheKeys(key)
	if err := store.set(ctx, keys.value, string(valueJson), handlerCacheValueTtl(ttl), !force); err != nil {
		return nil, &handlerCacheUnavailableError{}
	}
	return value, nil
}

func CacheNoAuth[R any](
	impl ImplFunction[*R],
	key string,
	ttl time.Duration,
) ImplFunction[*R] {
	return func(clientSession *session.ClientSession) (*R, error) {
		return cacheJson(clientSession.Ctx, defaultHandlerCacheStore, key, ttl, func() (*R, error) {
			return impl(clientSession)
		})
	}
}

func WarmCacheNoAuth[R any](
	clientSession *session.ClientSession,
	impl ImplFunction[*R],
	key string,
	ttl time.Duration,
	force bool,
) (*R, error) {
	return warmCacheJson(clientSession.Ctx, defaultHandlerCacheStore, key, ttl, force, func() (*R, error) {
		return impl(clientSession)
	})
}

// KeyWithAuth namespaces a cache key by network, caller (client or user), and
// caller address hash. Responses cached under it may safely embed
// caller-specific data (e.g. values derived from the caller ip).
func KeyWithAuth(clientSession *session.ClientSession, key string) string {
	var clientAddressHashHex string
	if clientAddressHash, _, err := clientSession.ClientAddressHashPort(); err == nil {
		clientAddressHashHex = hex.EncodeToString(clientAddressHash[:])
	}
	if clientSession.ByJwt.ClientId != nil {
		return fmt.Sprintf("%s@c%s@%s@%s", clientSession.ByJwt.NetworkId, *clientSession.ByJwt.ClientId, clientAddressHashHex, key)
	} else {
		return fmt.Sprintf("%s@u%s@%s@%s", clientSession.ByJwt.NetworkId, clientSession.ByJwt.UserId, clientAddressHashHex, key)
	}
}

// KeyWithNetworkAuth namespaces a cache key by the caller network only, so one
// entry serves every client in the network. Use it (via CacheWithNetworkAuth*)
// solely for responses computed from network_id (and the request input) alone
// — never for responses that vary by client_id, user_id, or caller address
// (e.g. /network/reliability embeds a caller-ip country multiplier).
func KeyWithNetworkAuth(clientSession *session.ClientSession, key string) string {
	return fmt.Sprintf("%s@n@%s", clientSession.ByJwt.NetworkId, key)
}

func CacheWithAuth[R any](
	impl ImplFunction[*R],
	key string,
	ttl time.Duration,
) ImplFunction[*R] {
	return func(clientSession *session.ClientSession) (*R, error) {
		keyWithAuth := KeyWithAuth(clientSession, key)
		return cacheJson(clientSession.Ctx, defaultHandlerCacheStore, keyWithAuth, ttl, func() (*R, error) {
			return impl(clientSession)
		})
	}
}

func WarmCacheWithAuth[R any](
	clientSession *session.ClientSession,
	impl ImplFunction[*R],
	key string,
	ttl time.Duration,
	force bool,
) (*R, error) {
	return warmCacheJson(
		clientSession.Ctx,
		defaultHandlerCacheStore,
		KeyWithAuth(clientSession, key),
		ttl,
		force,
		func() (*R, error) { return impl(clientSession) },
	)
}

// CacheWithNetworkAuth is CacheWithAuth with one cache entry per network
// instead of per caller. See KeyWithNetworkAuth for when this is safe.
func CacheWithNetworkAuth[R any](
	impl ImplFunction[*R],
	key string,
	ttl time.Duration,
) ImplFunction[*R] {
	return func(clientSession *session.ClientSession) (*R, error) {
		keyWithNetworkAuth := KeyWithNetworkAuth(clientSession, key)
		return cacheJson(clientSession.Ctx, defaultHandlerCacheStore, keyWithNetworkAuth, ttl, func() (*R, error) {
			return impl(clientSession)
		})
	}
}

func WarmCacheWithNetworkAuth[R any](
	clientSession *session.ClientSession,
	impl ImplFunction[*R],
	key string,
	ttl time.Duration,
	force bool,
) (*R, error) {
	return warmCacheJson(
		clientSession.Ctx,
		defaultHandlerCacheStore,
		KeyWithNetworkAuth(clientSession, key),
		ttl,
		force,
		func() (*R, error) { return impl(clientSession) },
	)
}

// CacheWithAuthInput is CacheWithAuth for handlers that take a request body.
// The cache key namespaces by caller (network/client/address) AND by a stable
// hash of the input, so responses for different args (e.g. last_n, client_id)
// do not collide.
func CacheWithAuthInput[T any, R any](
	impl ImplWithInputFunction[T, *R],
	key string,
	ttl time.Duration,
) ImplWithInputFunction[T, *R] {
	return func(input T, clientSession *session.ClientSession) (*R, error) {
		keyWithAuth := KeyWithAuth(clientSession, inputCacheKey(key, input))
		return cacheJson(clientSession.Ctx, defaultHandlerCacheStore, keyWithAuth, ttl, func() (*R, error) {
			return impl(input, clientSession)
		})
	}
}

func WarmCacheWithAuthInput[T any, R any](
	clientSession *session.ClientSession,
	impl ImplWithInputFunction[T, *R],
	key string,
	input T,
	ttl time.Duration,
	force bool,
) (*R, error) {
	return warmCacheJson(
		clientSession.Ctx,
		defaultHandlerCacheStore,
		KeyWithAuth(clientSession, inputCacheKey(key, input)),
		ttl,
		force,
		func() (*R, error) { return impl(input, clientSession) },
	)
}

// CacheWithNetworkAuthInput is CacheWithAuthInput with one cache entry per
// (network, input) instead of per (caller, input). See KeyWithNetworkAuth for
// when this is safe.
func CacheWithNetworkAuthInput[T any, R any](
	impl ImplWithInputFunction[T, *R],
	key string,
	ttl time.Duration,
) ImplWithInputFunction[T, *R] {
	return func(input T, clientSession *session.ClientSession) (*R, error) {
		keyWithNetworkAuth := KeyWithNetworkAuth(clientSession, inputCacheKey(key, input))
		return cacheJson(clientSession.Ctx, defaultHandlerCacheStore, keyWithNetworkAuth, ttl, func() (*R, error) {
			return impl(input, clientSession)
		})
	}
}

func WarmCacheWithNetworkAuthInput[T any, R any](
	clientSession *session.ClientSession,
	impl ImplWithInputFunction[T, *R],
	key string,
	input T,
	ttl time.Duration,
	force bool,
) (*R, error) {
	return warmCacheJson(
		clientSession.Ctx,
		defaultHandlerCacheStore,
		KeyWithNetworkAuth(clientSession, inputCacheKey(key, input)),
		ttl,
		force,
		func() (*R, error) { return impl(input, clientSession) },
	)
}

// inputCacheKey extends a base cache key with a short stable hash of the
// request input so cached responses do not collide across different args.
func inputCacheKey[T any](key string, input T) string {
	inputJson, err := json.Marshal(input)
	if err != nil {
		return key
	}
	sum := sha256.Sum256(inputJson)
	return fmt.Sprintf("%s@%s", key, hex.EncodeToString(sum[:8]))
}
