// The session revocation event. An admitted transport's authority can end
// only by a revocation, so only a revocation rechecks the network's leases at
// once: its key, {ns_<network>}rid, is incremented after every revocation
// record of the network refuses the revoked sessions, and its keyspace event
// rechecks every lease of the network on every connect process
// (connect/key_event_subscriber.go). The inventory revision (eid) still
// changes on every session created, revoked or expired, for the Sessions
// screen's hints, and rechecks nothing.
//
// Before 2026-11-18 the eid event rechecked the leases (S1 W8, S2 N2, N7):
// every new sign-in, code login, hosted proxy bootstrap or API-key client
// rechecked every connection of its network on every process, and a revoke's
// eid event came from its script, before the record tombstones it then wrote,
// so a recheck in between read the session's positive projection and renewed
// the lease.
package session

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

func SessionRevocationEventKey(networkId server.Id) string {
	return SessionKey(networkId, "rid")
}

// The key only needs to live for its event.
const sessionRevocationEventTtl = time.Hour

// INCR publishes the incrby event connect listens for. The ttl's expire event
// is one the key-event drains discard.
const sessionRevocationEventLua = `redis.call('INCR',KEYS[1]);redis.call('PEXPIRE',KEYS[1],ARGV[1]);return 1`

// The event write; a test stands in to observe its order.
var writeSessionRevocationEvent = func(ctx context.Context, networkId server.Id) error {
	return server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		return r.Eval(ctx, sessionRevocationEventLua, []string{SessionRevocationEventKey(networkId)}, sessionRevocationEventTtl.Milliseconds()).Err()
	})
}

// Rechecks the network's admitted transports once every revocation record of
// the network refuses the operation's targets. Best effort: a transport the
// event misses is rechecked by its lease within a minute.
func announceSessionRevocation(ctx context.Context, networkId server.Id) {
	ctx, cancel := context.WithTimeout(ctx, RevocationCheckBudget)
	defer cancel()
	_ = writeSessionRevocationEvent(ctx, networkId)
}
