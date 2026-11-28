package model

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The move target breaker (connect/MOVE.md §5.5): for each target ip, the move
// sends and the failure reports of one window, and an open latch. Counting per
// ip rather than per host keeps one bad interface or router path from idling a
// whole host. The mover reads the latch before it targets an ip: a window whose
// failures over sends exceed the ratio, with at least the minimum of sends,
// opens it for the breaker timeout.
//
// The keys of one ip share the hash tag `{mtb_<ip>}`, so each script reads and
// writes them on one slot:
//   - `{mtb_<ip>}w` is the window, a hash of `sends` and `failures`. The first
//     send starts it with the window as its ttl. A failure report counts only
//     inside a window, since without sends it weighs against nothing.
//   - `{mtb_<ip>}open` is the latch, with the breaker timeout as its ttl.
//     Opening ends the window, so a closed breaker starts from fresh counts.
//
// Every key carries a ttl.

func residentMoveTargetBreakerTag(ip netip.Addr) string {
	return fmt.Sprintf("{mtb_%s}", ip.Unmap())
}

func residentMoveTargetBreakerWindowKey(ip netip.Addr) string {
	return residentMoveTargetBreakerTag(ip) + "w"
}

func residentMoveTargetBreakerOpenKey(ip netip.Addr) string {
	return residentMoveTargetBreakerTag(ip) + "open"
}

// The thresholds a breaker read applies, from the mover's settings.
type ResidentMoveTargetBreakerSettings struct {
	// the failures over sends of a window that open the breaker
	Ratio float64
	// the sends a window needs before it can open the breaker
	MinSends int
	// how long an opened breaker stays open. Non-positive never opens one.
	Timeout time.Duration
}

// Counts one move sent to ip. The first send of a window starts it; a
// non-positive window counts nothing.
func RecordResidentMoveTargetSend(ctx context.Context, ip netip.Addr, window time.Duration) error {
	if !ip.IsValid() || window <= 0 {
		return nil
	}
	script := `
local sends = redis.call('HINCRBY', KEYS[1], 'sends', 1)
if redis.call('PTTL', KEYS[1]) < 0 then
	redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return sends
`
	return residentMoveIntentRedis(ctx, func(r server.RedisClient) error {
		return r.Eval(
			ctx,
			script,
			[]string{residentMoveTargetBreakerWindowKey(ip)},
			max(1, window.Milliseconds()),
		).Err()
	})
}

// Counts one failure report for a move sent to ip, inside the window the sends
// were counted in. Reports whether it was counted.
func RecordResidentMoveTargetFailure(ctx context.Context, ip netip.Addr) (counted bool, returnErr error) {
	if !ip.IsValid() {
		return false, nil
	}
	script := `
if redis.call('EXISTS', KEYS[1]) == 1 then
	redis.call('HINCRBY', KEYS[1], 'failures', 1)
	return 1
end
return 0
`
	returnErr = residentMoveIntentRedis(ctx, func(r server.RedisClient) error {
		result, err := r.Eval(
			ctx,
			script,
			[]string{residentMoveTargetBreakerWindowKey(ip)},
		).Int()
		counted = result == 1
		return err
	})
	return
}

// Whether the breaker of ip is open. A window whose failures over sends
// exceed the ratio with at least the minimum of sends opens it here, for the
// timeout, and ends the window.
func IsResidentMoveTargetBreakerOpen(
	ctx context.Context,
	ip netip.Addr,
	settings ResidentMoveTargetBreakerSettings,
) (open bool, returnErr error) {
	if !ip.IsValid() || settings.Timeout <= 0 {
		return false, nil
	}
	script := `
if redis.call('EXISTS', KEYS[2]) == 1 then
	return 1
end
local counts = redis.call('HMGET', KEYS[1], 'sends', 'failures')
local sends = tonumber(counts[1]) or 0
local failures = tonumber(counts[2]) or 0
local min_sends = tonumber(ARGV[1])
local ratio = tonumber(ARGV[2])
if 0 < sends and min_sends <= sends and ratio * sends < failures then
	redis.call('SET', KEYS[2], '1', 'PX', ARGV[3])
	redis.call('DEL', KEYS[1])
	return 1
end
return 0
`
	returnErr = residentMoveIntentRedis(ctx, func(r server.RedisClient) error {
		result, err := r.Eval(
			ctx,
			script,
			[]string{
				residentMoveTargetBreakerWindowKey(ip),
				residentMoveTargetBreakerOpenKey(ip),
			},
			settings.MinSends,
			settings.Ratio,
			max(1, settings.Timeout.Milliseconds()),
		).Int()
		open = result == 1
		return err
	})
	return
}
