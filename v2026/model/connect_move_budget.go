// The move budget (connect/MOVE.md §5.4, D8): per-minute window counters of
// moves for the fleet, for each source host and for each target host, which
// every connect process's mover reserves from before it commits a move.
//
// One script checks all three counters and increments them together, so a
// reservation is all or nothing and concurrent movers never exceed a cap. It
// answers "allowed" or the first cap that refused, in the order fleet, source,
// target. Windows are minutes of the Redis node's clock (TIME in the script),
// so no process clock decides a window (MOVE.md §6).
//
// Each counter is a small hash {window, count}; a counter whose window is not
// the current one counts zero and is reset by the next reservation. Counters
// expire two windows after their last reservation.
//
// The keys share the hash tag `{connect_move}`, a fixed tag, because the
// reservation is one atomic script over the fleet counter and two host
// counters: there is no narrower tag those keys share. The family is a few
// dozen tiny keys (one per edge host and direction), so the slot carries
// almost nothing.
package model

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// The caps that refused a reservation.
const (
	ConnectMoveBudgetCapFleet  = "fleet"
	ConnectMoveBudgetCapSource = "source"
	ConnectMoveBudgetCapTarget = "target"
)

// The answer of an allowed reservation.
const connectMoveBudgetAllowed = "allowed"

// The window of the per-minute caps, in seconds of the Redis node's clock.
// This is the unit of the caps, not a tunable.
const connectMoveBudgetWindowSeconds = 60

// A counter outlives its window by one window, for a reader near the
// boundary, then expires.
const connectMoveBudgetCounterTtlSeconds = 2 * connectMoveBudgetWindowSeconds

// The per-minute counter of the fleet's moves.
const connectMoveBudgetFleetKey = "{connect_move}budget"

// The per-minute counter of moves from a source host.
func connectMoveBudgetSourceKey(host string) string {
	return "{connect_move}budget_source_" + host
}

// The per-minute counter of moves to a target host.
func connectMoveBudgetTargetKey(host string) string {
	return "{connect_move}budget_target_" + host
}

// Caps in moves per minute. A non-positive cap allows none.
type ConnectMoveBudgetCaps struct {
	Fleet  int
	Source int
	Target int
}

// KEYS[1], KEYS[2] and KEYS[3] are the fleet, source and target counters and
// ARGV[1], ARGV[2] and ARGV[3] their caps; ARGV[4] is the window in seconds and
// ARGV[5] the counter TTL in seconds. Returns "allowed" after incrementing all
// three, or the name of the first cap that refused, incrementing none.
var connectMoveBudgetReserveScript = redis.NewScript(`
local time = redis.call('TIME')
local window = math.floor(tonumber(time[1]) / tonumber(ARGV[4]))
local names = {'fleet', 'source', 'target'}
local counts = {}
for i = 1, 3 do
  local values = redis.call('HMGET', KEYS[i], 'window', 'count')
  local count = 0
  if tonumber(values[1]) == window then
    count = tonumber(values[2]) or 0
  end
  if tonumber(ARGV[i]) <= count then
    return names[i]
  end
  counts[i] = count
end
for i = 1, 3 do
  redis.call('HSET', KEYS[i], 'window', window, 'count', counts[i] + 1)
  redis.call('EXPIRE', KEYS[i], tonumber(ARGV[5]))
end
return 'allowed'
`)

// Reserves one move from sourceHost to targetHost in the current window.
// refusedCap is empty when allowed, else the first cap that refused:
// ConnectMoveBudgetCapFleet, ConnectMoveBudgetCapSource or
// ConnectMoveBudgetCapTarget. One bounded script, never retried: ctx must
// carry a deadline. An error reserves nothing the caller can rely on, and the
// caller moves nothing; a lost response after the script ran only spends a
// slot.
func ReserveConnectMoveBudget(
	ctx context.Context,
	sourceHost string,
	targetHost string,
	caps ConnectMoveBudgetCaps,
) (refusedCap string, returnErr error) {
	if sourceHost == "" || targetHost == "" {
		return "", fmt.Errorf("move budget needs a source and a target host")
	}
	returnErr = server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		answer, err := connectMoveBudgetReserveScript.Run(
			ctx,
			r,
			[]string{
				connectMoveBudgetFleetKey,
				connectMoveBudgetSourceKey(sourceHost),
				connectMoveBudgetTargetKey(targetHost),
			},
			caps.Fleet,
			caps.Source,
			caps.Target,
			connectMoveBudgetWindowSeconds,
			connectMoveBudgetCounterTtlSeconds,
		).Text()
		if err != nil {
			return err
		}
		switch answer {
		case connectMoveBudgetAllowed:
			refusedCap = ""
		case ConnectMoveBudgetCapFleet, ConnectMoveBudgetCapSource, ConnectMoveBudgetCapTarget:
			refusedCap = answer
		default:
			return fmt.Errorf("move budget answered %q", answer)
		}
		return nil
	}, server.OptRedisNoPreflight())
	if returnErr != nil {
		return "", returnErr
	}
	return
}
