package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// Client identity-key model: long-lived Ed25519 public keys published by
// clients via the `ClientKey` control message, one per `client_id`.
//
// Redis is the source of truth (no SQL table): the string key `ckey_<clientId>`
// holds the raw 32-byte key with no TTL, so entries persist until removed.

func clientPublicKeyRedisKey(clientId server.Id) string {
	return fmt.Sprintf("ckey_%s", clientId)
}

// GetClientPublicKey returns the published key, or nil if none was published.
func GetClientPublicKey(
	ctx context.Context,
	clientId server.Id,
) (publicKey []byte, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		bytes, err := r.Get(ctx, clientPublicKeyRedisKey(clientId)).Bytes()
		if err == nil {
			publicKey = bytes
			return
		}
		if errors.Is(err, redis.Nil) {
			return
		}
		returnErr = err
	})
	return
}

// SetClientPublicKey stores the published key; an empty/nil key removes it.
func SetClientPublicKey(
	ctx context.Context,
	clientId server.Id,
	publicKey []byte,
) {
	server.Redis(ctx, func(r server.RedisClient) {
		if 0 < len(publicKey) {
			// expiration = 0 → never expires
			r.Set(ctx, clientPublicKeyRedisKey(clientId), publicKey, 0)
		} else {
			r.Del(ctx, clientPublicKeyRedisKey(clientId))
		}
	})
}

// RemoveClientPublicKey deletes the entry, called from
// `RemoveDisconnectedNetworkClients` so a reaped client_id doesn't leave its key
// behind. Safe to call when no entry exists.
func RemoveClientPublicKey(
	ctx context.Context,
	clientId server.Id,
) {
	server.Redis(ctx, func(r server.RedisClient) {
		r.Del(ctx, clientPublicKeyRedisKey(clientId))
	})
}
