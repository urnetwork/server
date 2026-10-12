package model

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"time"

	"github.com/urnetwork/server"
)

// Rate limits of the lag API (lag.dev/PLAN.md A1, A6): guest sessions per
// /24 (/48), and tracks per client and per /24 (/48).
//
// The shared caller limiters bucket an address at /29 (/56)
// (server.ClientIpHashForAddr). The plan counts a whole /24 (/48), the
// smallest block a provider routes to one customer, so the key here is the
// same peppered hash over the wider prefix and the raw address is never part
// of a key. Infrastructure subnets (WARP_LIMIT_EXCLUDE_SUBNETS) are not
// counted, as for every caller limiter. The windows are fixed and epoch
// aligned (server.IncrementRateLimitWindow).

// The prefix lengths a caller is counted by.
const (
	LagClientPrefixBitsV4 = 24
	LagClientPrefixBitsV6 = 48
)

// The /24 (/48) an address is counted in, after unmapping a v4-mapped
// address. Invalid for an invalid address.
func LagClientPrefix(addr netip.Addr) netip.Prefix {
	addr = addr.Unmap()
	switch {
	case addr.Is4():
		return netip.PrefixFrom(addr, LagClientPrefixBitsV4).Masked()
	case addr.Is6():
		return netip.PrefixFrom(addr.WithZone(""), LagClientPrefixBitsV6).Masked()
	default:
		return netip.Prefix{}
	}
}

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
	prefixHash := server.ClientIpHashForAddrPrefix(addr, LagClientPrefixBitsV4, LagClientPrefixBitsV6)
	key := fmt.Sprintf("{lag_%s_prefix_%s}", name, hex.EncodeToString(prefixHash[:]))
	return checkLagRateLimit(ctx, key, limit, window)
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
