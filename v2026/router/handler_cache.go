package router

import (
	"context"
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
	go func() {
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
	}()

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
	go func() {
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
	}()

	return value, nil
}
