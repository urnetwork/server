package router

import (
	"context"
	"encoding/json"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
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
