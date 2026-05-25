package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// Client identity-key model: long-lived Ed25519 public keys published
// by clients via the `ClientKey` control message. Keyed on
// `client_id`; one key per client.
//
// Storage:
//   - Redis is the source of truth. There is no backing SQL table.
//     A single string key `ckey_<clientId>` holds the raw 32-byte
//     Ed25519 public key. No TTL — entries persist until explicitly
//     removed.
//
// Lifecycle:
//   - `SetClientPublicKey` writes (or, on empty value, deletes) the
//     redis entry.
//   - `RemoveClientPublicKey` deletes the redis entry, called by
//     `RemoveDisconnectedNetworkClients` when the owning
//     `network_client` row is reaped, so a recycled client_id never
//     inherits a stale predecessor's key.
//   - `GetClientPublicKey` returns the current redis value, or nil
//     when no key has been published (or it was removed).

func clientPublicKeyRedisKey(clientId server.Id) string {
	return fmt.Sprintf("ckey_%s", clientId)
}

// GetClientPublicKey returns the 32-byte Ed25519 long-lived public
// identity key the client published via `ClientKey`, or nil when the
// client has not published a key (or the key was removed because the
// client_id was reaped).
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

// SetClientPublicKey stores the client's published 32-byte Ed25519
// long-lived public identity key into redis. Passing an empty / nil
// key removes the entry. No TTL — entries live until
// `RemoveClientPublicKey` is called or the value is overwritten.
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

// RemoveClientPublicKey deletes the redis entry for `clientId`.
// Called from `RemoveDisconnectedNetworkClients` so a client_id that
// gets reaped doesn't leave its public key behind in redis. Safe to
// call when no entry exists.
func RemoveClientPublicKey(
	ctx context.Context,
	clientId server.Id,
) {
	server.Redis(ctx, func(r server.RedisClient) {
		r.Del(ctx, clientPublicKeyRedisKey(clientId))
	})
}
