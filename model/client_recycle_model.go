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
// api processes report it. Its value is the setting time in unix millis. Best
// effort: a lost write or event leaves the provider retrying, as before.
package model

import (
	"context"
	"fmt"
	"time"

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

// Sets the client's recycle flag unless it is set, with cooldown as its ttl.
// One single-key write on the client's slot, never retried: ctx must carry a
// deadline. Returns whether this call set the flag; false with no error is a
// flag still in its cooldown.
func SetClientRecycle(ctx context.Context, clientId server.Id, cooldown time.Duration) (set bool, returnErr error) {
	if cooldown <= 0 {
		return false, fmt.Errorf("client recycle cooldown must be positive")
	}
	returnErr = server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		var err error
		set, err = r.SetNX(ctx, clientRecycleKey(clientId), server.NowUtc().UnixMilli(), cooldown).Result()
		return err
	}, server.OptRedisNoPreflight())
	if returnErr != nil {
		set = false
	}
	return
}
