// The stuck provider recycle flag (CPUOPT3, the rejection creep). Providers on
// connect builds before v2026.9.8 never act on a CreateContractResult error,
// so a provider that holds return traffic for a removed window client asks
// for the refused route on its own schedule for as long as its connection
// lasts; only a new connection and resident end it, as every connect deploy
// does. The api reports a provider it keeps answering from terminal refusals
// (controller/connect_control_stuck_provider.go) by setting
// `{<clientId>}c_recycle`, and the connect process hosting the provider's
// resident recycles it on the key's keyspace event (connect/client_recycle.go).
//
// The key is set only if absent, with a ttl: its presence is the fleet-wide
// cooldown, so a provider is recycled at most once per cooldown however many
// api processes report it. Its value is the setting time in unix millis, which
// a later report that finds the flag reads back: a report long after the flag
// was set is a client still retrying after its recycle. Best effort: a lost
// write or event leaves the provider retrying, as before.
package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

const clientRecycleKeySuffix = "c_recycle"

func clientRecycleKey(clientId server.Id) string {
	return fmt.Sprintf("{%s}%s", clientId, clientRecycleKeySuffix)
}

// The psubscribe pattern for the keyspace channels of every client's recycle
// flag on a db.
func ClientRecycleKeyEventPattern(db int) string {
	return fmt.Sprintf("__keyspace@%d__:{*}%s", db, clientRecycleKeySuffix)
}

// The client id of a keyspace channel matching `ClientRecycleKeyEventPattern`.
func ParseClientRecycleKeyEvent(channel string) (server.Id, bool) {
	return parseClientKeyEvent(channel, clientRecycleKeySuffix)
}

// Sets the client's recycle flag unless it is set, with ttl. One single-key
// SET NX GET on the client's slot, never retried: ctx must carry a deadline.
// Returns whether this call set the flag and, when it found the flag in its
// cooldown instead, when that flag was set (zero if unreadable).
func SetClientRecycle(ctx context.Context, clientId server.Id, ttl time.Duration) (set bool, flagTime time.Time, returnErr error) {
	if ttl <= 0 {
		return false, time.Time{}, fmt.Errorf("client recycle ttl must be positive")
	}
	returnErr = server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		previous, err := r.SetArgs(ctx, clientRecycleKey(clientId), server.NowUtc().UnixMilli(), redis.SetArgs{
			Mode: "NX",
			TTL:  ttl,
			Get:  true,
		}).Result()
		if errors.Is(err, redis.Nil) {
			// no flag to get: this call set it
			set = true
			return nil
		}
		if err != nil {
			return err
		}
		if millis, parseErr := strconv.ParseInt(previous, 10, 64); parseErr == nil {
			flagTime = time.UnixMilli(millis)
		}
		return nil
	}, server.OptRedisNoPreflight())
	if returnErr != nil {
		set = false
		flagTime = time.Time{}
	}
	return
}
