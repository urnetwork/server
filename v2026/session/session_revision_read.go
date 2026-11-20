package session

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/urnetwork/server/v2026"
)

// ReadNetworkSessionRevision is the revision a session hint carries, read
// without writing (S1 W11). connect reads it for every network with a hint
// listener, every minute and on every inventory event, and used to run the
// inventory script for it (NetworkSessionRevision): about 5.8k scripts a
// second on 2026-11-18, each pruning the network, returning every live
// session and extending the shared keys, though a hint needs only the
// generation and event id. One MGET of the two keys answers it. A network
// whose revision keys are missing (expired, or never written) starts its
// incarnation through the inventory read, as before. Expired sessions still
// leave the inventory through maintenance, list reads and mints, which prune.
func ReadNetworkSessionRevision(ctx context.Context, networkId server.Id) (string, int64, error) {
	start := time.Now()
	var values []any
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		values, err = r.MGet(ctx, SessionKey(networkId, "generation"), SessionKey(networkId, "eid")).Result()
		return err
	})
	if err != nil {
		err = errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)
		observeSessionStorage("revision", start, err)
		return "", 0, err
	}
	observeSessionStorage("revision", start, nil)
	if len(values) == 2 {
		generation, _ := values[0].(string)
		eventText, _ := values[1].(string)
		if eventId, parseErr := strconv.ParseInt(eventText, 10, 64); generation != "" && parseErr == nil && 0 < eventId {
			return generation, eventId, nil
		}
	}
	return NetworkSessionRevision(ctx, networkId)
}
