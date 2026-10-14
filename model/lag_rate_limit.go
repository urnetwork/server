package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"time"

	"github.com/urnetwork/server"
)

// Rate limits of the lag API (lag.dev/PLAN.md A1, A6): guest sessions per
// /24 (/48), and tracks per client, per /24 (/48) and per signing agent
// (amendment 12).
//
// The shared caller limiters bucket an address at /29 (/56)
// (server.ClientIpHashForAddr). The plan counts a whole /24 (/48), the
// smallest block a provider routes to one customer, so the key here is the
// same peppered hash over the wider prefix. No address, prefix text or
// address bytes are ever part of a key (amendment 9), and a counter lives in
// Redis only: server.IncrementRateLimitWindow gives each window's key a TTL
// of the window, so it is gone one window after its last count at most.
// Infrastructure subnets (WARP_LIMIT_EXCLUDE_SUBNETS) are not counted, as
// for every caller limiter. The windows are fixed and epoch aligned.

// The prefix lengths a caller is counted by.
const (
	LagClientPrefixBitsV4 = 24
	LagClientPrefixBitsV6 = 48
)

// The outcome of one counted attempt. RetryAfter is the rest of the window
// when the attempt was refused.
type LagRateLimitResult struct {
	Allowed    bool
	Excluded   bool
	Count      int64
	RetryAfter time.Duration
}

// Counts one attempt of `name` from the /24 (/48) of addr against limit per
// window.
func CheckLagPrefixRateLimit(
	ctx context.Context,
	name string,
	addr netip.Addr,
	limit int,
	window time.Duration,
) (LagRateLimitResult, error) {
	addr = addr.Unmap().WithZone("")
	if !addr.IsValid() {
		return LagRateLimitResult{}, fmt.Errorf("the caller address is not valid")
	}
	if server.IsRateLimitExcluded(addr) {
		return LagRateLimitResult{Allowed: true, Excluded: true}, nil
	}
	return checkLagRateLimit(ctx, lagPrefixRateLimitKey(name, addr), limit, window)
}

// The counter key of `name` for the /24 (/48) of a valid, unmapped address:
// the peppered hash of the prefix, never the address or the prefix itself.
func lagPrefixRateLimitKey(name string, addr netip.Addr) string {
	prefixHash := server.ClientIpHashForAddrPrefix(addr, LagClientPrefixBitsV4, LagClientPrefixBitsV6)
	return fmt.Sprintf("{lag_%s_prefix_%s}", name, hex.EncodeToString(prefixHash[:]))
}

// Counts one attempt of `name` by the agent a request's Signature-Agent
// names (Web Bot Auth) against limit per window, so one agent's browsers
// cannot draw on the extenders without bound (amendment 12). The key is a
// hash of the header's value, which lives only as long as the window's
// counter.
func CheckLagSignatureRateLimit(
	ctx context.Context,
	name string,
	signatureAgent string,
	limit int,
	window time.Duration,
) (LagRateLimitResult, error) {
	if signatureAgent == "" {
		return LagRateLimitResult{}, fmt.Errorf("no signature agent to count")
	}
	return checkLagRateLimit(ctx, lagSignatureRateLimitKey(name, signatureAgent), limit, window)
}

// The counter key of `name` for a Signature-Agent value: a hash of the value,
// so a key is bounded whatever the header holds.
func lagSignatureRateLimitKey(name string, signatureAgent string) string {
	digest := sha256.Sum256([]byte(signatureAgent))
	return fmt.Sprintf("{lag_%s_signature_%s}", name, hex.EncodeToString(digest[:]))
}

// Counts one attempt of `name` by a client against limit per window.
func CheckLagClientRateLimit(
	ctx context.Context,
	name string,
	clientId server.Id,
	limit int,
	window time.Duration,
) (LagRateLimitResult, error) {
	key := fmt.Sprintf("{lag_%s_client_%s}", name, clientId)
	return checkLagRateLimit(ctx, key, limit, window)
}

func checkLagRateLimit(
	ctx context.Context,
	key string,
	limit int,
	window time.Duration,
) (LagRateLimitResult, error) {
	if limit <= 0 || window < time.Millisecond {
		return LagRateLimitResult{}, fmt.Errorf("invalid lag rate limit %d per %s", limit, window)
	}
	count, err := server.IncrementRateLimitWindow(ctx, key, window)
	if err != nil {
		return LagRateLimitResult{}, err
	}
	result := LagRateLimitResult{
		Allowed: count <= int64(limit),
		Count:   count,
	}
	if !result.Allowed {
		// the windows are epoch aligned, so the current one ends at the next
		// multiple of the window
		windowMillis := window.Milliseconds()
		nowMillis := server.NowUtc().UnixMilli()
		result.RetryAfter = time.Duration(windowMillis-nowMillis%windowMillis) * time.Millisecond
	}
	return result, nil
}
