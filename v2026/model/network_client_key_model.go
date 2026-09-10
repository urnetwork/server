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
// Legacy clients retain their Redis source. Once signed operator history is
// present, its durable SQL head is authoritative and Redis is only projection.

func clientPublicKeyRedisKey(clientId server.Id) string {
	return fmt.Sprintf("ckey_%s", clientId)
}

// GetClientPublicKey returns the published key, or nil if none was published.
func GetClientPublicKey(
	ctx context.Context,
	clientId server.Id,
) (publicKey []byte, returnErr error) {
	if key, found, err := stClientKeyCurrent(ctx, clientId); err != nil || found {
		return key, err
	}
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
	if _, found, err := stClientKeyCurrent(ctx, clientId); err != nil || found {
		server.Raise(errors.Join(errors.New("signed client-key history requires the authenticated operator registration path"), err))
	}
	server.Redis(ctx, func(r server.RedisClient) {
		// redis is the source of truth for ckeys (accepted posture: an
		// unrouteable resident gets a new one nominated), so a failed publish
		// must raise rather than silently drop the client's key
		if 0 < len(publicKey) {
			// expiration = 0 → never expires
			server.Raise(r.Set(ctx, clientPublicKeyRedisKey(clientId), publicKey, 0).Err())
		} else {
			server.Raise(r.Del(ctx, clientPublicKeyRedisKey(clientId)).Err())
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
	retireStClientKeyHistory(ctx, clientId)
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Del(ctx, clientPublicKeyRedisKey(clientId)).Err())
	})
}
