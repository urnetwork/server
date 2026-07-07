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

func CacheNoAuth[R any](
	impl ImplFunction[*R],
	key string,
	ttl time.Duration,
) ImplFunction[*R] {
	return func(clientSession *session.ClientSession) (*R, error) {
		var cachedValueJson string
		server.Redis(clientSession.Ctx, func(r server.RedisClient) {
			cachedValueJson, _ = r.Get(clientSession.Ctx, key).Result()
		})
		if cachedValueJson != "" {
			var cachedValue R
			err := json.Unmarshal([]byte(cachedValueJson), &cachedValue)
			if err == nil {
				return &cachedValue, nil
			}
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

	// store the value in parallel
	go server.HandleError(func() {
		storeCtx := context.Background()
		valueJson, err := json.Marshal(value)
		if err == nil {
			server.Redis(storeCtx, func(r server.RedisClient) {
				// ignore the error
				if force {
					r.Set(storeCtx, key, string(valueJson), ttl).Err()
				} else {
					r.SetNX(storeCtx, key, string(valueJson), ttl).Err()
				}
			})
		}
	})

	return value, nil
}

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

func CacheWithAuth[R any](
	impl ImplFunction[*R],
	key string,
	ttl time.Duration,
) ImplFunction[*R] {
	return func(clientSession *session.ClientSession) (*R, error) {
		keyWithAuth := KeyWithAuth(clientSession, key)
		var cachedValueJson string
		server.Redis(clientSession.Ctx, func(r server.RedisClient) {
			cachedValueJson, _ = r.Get(clientSession.Ctx, keyWithAuth).Result()
		})
		if cachedValueJson != "" {
			var cachedValue R
			err := json.Unmarshal([]byte(cachedValueJson), &cachedValue)
			if err == nil {
				return &cachedValue, nil
			}
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

	// store the value in parallel
	go server.HandleError(func() {
		storeCtx := context.Background()
		keyWithAuth := KeyWithAuth(clientSession, key)
		valueJson, err := json.Marshal(value)
		if err == nil {
			server.Redis(storeCtx, func(r server.RedisClient) {
				// ignore the error
				if force {
					r.Set(storeCtx, keyWithAuth, string(valueJson), ttl).Err()
				} else {
					r.SetNX(storeCtx, keyWithAuth, string(valueJson), ttl).Err()
				}
			})
		}
	})

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
		var cachedValueJson string
		server.Redis(clientSession.Ctx, func(r server.RedisClient) {
			cachedValueJson, _ = r.Get(clientSession.Ctx, keyWithAuth).Result()
		})
		if cachedValueJson != "" {
			var cachedValue R
			err := json.Unmarshal([]byte(cachedValueJson), &cachedValue)
			if err == nil {
				return &cachedValue, nil
			}
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

	// store the value in parallel
	go server.HandleError(func() {
		storeCtx := context.Background()
		keyWithAuth := KeyWithAuth(clientSession, inputCacheKey(key, input))
		valueJson, err := json.Marshal(value)
		if err == nil {
			server.Redis(storeCtx, func(r server.RedisClient) {
				// ignore the error
				if force {
					r.Set(storeCtx, keyWithAuth, string(valueJson), ttl).Err()
				} else {
					r.SetNX(storeCtx, keyWithAuth, string(valueJson), ttl).Err()
				}
			})
		}
	})

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
