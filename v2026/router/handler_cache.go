package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// getCachedJson returns the unmarshaled cached value at key, or nil on a miss
// (absent, unreadable, or stale-schema json).
func getCachedJson[R any](ctx context.Context, key string) *R {
	var cachedValueJson string
	server.Redis(ctx, func(r server.RedisClient) {
		cachedValueJson, _ = r.Get(ctx, key).Result()
	})
	if cachedValueJson == "" {
		return nil
	}
	var cachedValue R
	if err := json.Unmarshal([]byte(cachedValueJson), &cachedValue); err != nil {
		return nil
	}
	return &cachedValue
}

// storeCachedJson stores value at key in the background so the caller does not
// wait on the cache write. force overwrites an existing entry; otherwise the
// first writer wins and the ttl is not extended.
func storeCachedJson(value any, key string, ttl time.Duration, force bool) {
	go server.HandleError(func() {
		valueJson, err := json.Marshal(value)
		if err != nil {
			return
		}
		storeCtx := context.Background()
		server.Redis(storeCtx, func(r server.RedisClient) {
			// ignore the error
			if force {
				r.Set(storeCtx, key, string(valueJson), ttl).Err()
			} else {
				r.SetNX(storeCtx, key, string(valueJson), ttl).Err()
			}
		})
	})
}

func CacheNoAuth[R any](
	impl ImplFunction[*R],
	key string,
	ttl time.Duration,
) ImplFunction[*R] {
	return func(clientSession *session.ClientSession) (*R, error) {
		if cachedValue := getCachedJson[R](clientSession.Ctx, key); cachedValue != nil {
			return cachedValue, nil
		}
		return WarmCacheNoAuth(clientSession, impl, key, ttl, false)
	}
}

func WarmCacheNoAuth[R any](
	clientSession *session.ClientSession,
	impl ImplFunction[*R],
	key string,
	ttl time.Duration,
	force bool,
) (*R, error) {
	value, err := impl(clientSession)
	if err != nil {
		return nil, err
	}
	storeCachedJson(value, key, ttl, force)
	return value, nil
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
		if cachedValue := getCachedJson[R](clientSession.Ctx, keyWithAuth); cachedValue != nil {
			return cachedValue, nil
		}
		return WarmCacheWithAuth(clientSession, impl, key, ttl, false)
	}
}

func WarmCacheWithAuth[R any](
	clientSession *session.ClientSession,
	impl ImplFunction[*R],
	key string,
	ttl time.Duration,
	force bool,
) (*R, error) {
	value, err := impl(clientSession)
	if err != nil {
		return nil, err
	}
	storeCachedJson(value, KeyWithAuth(clientSession, key), ttl, force)
	return value, nil
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
		if cachedValue := getCachedJson[R](clientSession.Ctx, keyWithNetworkAuth); cachedValue != nil {
			return cachedValue, nil
		}
		return WarmCacheWithNetworkAuth(clientSession, impl, key, ttl, false)
	}
}

func WarmCacheWithNetworkAuth[R any](
	clientSession *session.ClientSession,
	impl ImplFunction[*R],
	key string,
	ttl time.Duration,
	force bool,
) (*R, error) {
	value, err := impl(clientSession)
	if err != nil {
		return nil, err
	}
	storeCachedJson(value, KeyWithNetworkAuth(clientSession, key), ttl, force)
	return value, nil
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
		if cachedValue := getCachedJson[R](clientSession.Ctx, keyWithAuth); cachedValue != nil {
			return cachedValue, nil
		}
		return WarmCacheWithAuthInput(clientSession, impl, key, input, ttl, false)
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
	value, err := impl(input, clientSession)
	if err != nil {
		return nil, err
	}
	storeCachedJson(value, KeyWithAuth(clientSession, inputCacheKey(key, input)), ttl, force)
	return value, nil
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
		if cachedValue := getCachedJson[R](clientSession.Ctx, keyWithNetworkAuth); cachedValue != nil {
			return cachedValue, nil
		}
		return WarmCacheWithNetworkAuthInput(clientSession, impl, key, input, ttl, false)
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
	value, err := impl(input, clientSession)
	if err != nil {
		return nil, err
	}
	storeCachedJson(value, KeyWithNetworkAuth(clientSession, inputCacheKey(key, input)), ttl, force)
	return value, nil
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
