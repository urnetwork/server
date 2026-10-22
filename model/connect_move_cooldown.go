// The per-client move cooldown (connect/MOVE.md §5.4, §5.5): the key
// `move_cooldown_<clientId>`, set by the mover when it commits a move and
// expiring after MoveClientCooldown. While it lives the client is not a
// balance candidate anywhere in the fleet, which bounds how often any client
// moves and stops a client that declined a move from being asked again at
// once.
//
// The value is the move id that set it, so the mover that set it, and only
// that mover, can withdraw it when its send fails. One key per client and no
// hash tag, so the family spreads over the cluster.
//
// Every call is bounded and never retried: ctx must carry a deadline.
package model

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// The client's cooldown key. No hash tag: one key per client.
func connectMoveCooldownKey(clientId server.Id) string {
	return "move_cooldown_" + clientId.String()
}

// Starts the client's cooldown for ttl, recording moveId. Replaces a cooldown
// the client already has. Non-positive ttl sets nothing: a key without expiry
// would hold the client forever.
func SetConnectMoveCooldown(ctx context.Context, clientId server.Id, moveId server.Id, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.Set(ctx, connectMoveCooldownKey(clientId), moveId.String(), ttl).Err()
	}, server.OptRedisNoPreflight())
}

// The clients of clientIds that have a cooldown. One pipelined round of
// single-key reads, routed per key.
func GetConnectMoveCooldowns(ctx context.Context, clientIds []server.Id) (cooling map[server.Id]bool, returnErr error) {
	cooling = map[server.Id]bool{}
	if len(clientIds) == 0 {
		return cooling, nil
	}
	returnErr = server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		pipeline := r.Pipeline()
		existsCmds := make([]*redis.IntCmd, len(clientIds))
		for i, clientId := range clientIds {
			existsCmds[i] = pipeline.Exists(ctx, connectMoveCooldownKey(clientId))
		}
		if _, err := pipeline.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
		for i, clientId := range clientIds {
			count, err := existsCmds[i].Result()
			if err != nil {
				return err
			}
			if 0 < count {
				cooling[clientId] = true
			}
		}
		return nil
	}, server.OptRedisNoPreflight())
	if returnErr != nil {
		return nil, returnErr
	}
	return
}

// Ends the client's cooldown if moveId set it, for a mover whose send failed.
// A cooldown a later move set is kept.
func RemoveConnectMoveCooldown(ctx context.Context, clientId server.Id, moveId server.Id) error {
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return server.RedisRemoveIfEqual(r, ctx, connectMoveCooldownKey(clientId), []byte(moveId.String())).Err()
	}, server.OptRedisNoPreflight())
}
