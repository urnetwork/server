package server

// The model routes' per-address limit (DESIGN-COMPUTE §14.3, CO-49; CSRV-M11-01; compute-plan-model-mirror-store,
// QD-1409): each request of a route is counted against its caller's /29 or /56, by the address's peppered hash
// (ClientIpHashForAddr), on the {address_<hash>} attempt histories every public route's limit keeps, which api-3
// covers, under the route's own key prefix:
//
//	compute_model_<route>.{address_<hash>}.address_<window>
//
// a fixed, epoch-aligned window's counter, which expires at its window's end (IncrementRateLimitWindowToEnd), so no
// count outlives the window it limits. The key names no file and no address: the api keeps how many model requests an
// address hash made this window, never what it fetched. An infrastructure address (WARP_LIMIT_EXCLUDE_SUBNETS)
// counts nothing and touches no store.

import (
	"context"
	"fmt"
	"time"
)

// What one count decided.
type ComputeModelRouteAddressCount struct {
	Allowed bool
	// an infrastructure address, not counted
	Excluded bool
	// the address's requests of the route this window, this one included
	Count int64
	// for a refused request, the rest of its window
	RetryAfter time.Duration
}

// The key prefix of a route's counts.
func computeModelRouteKeyPrefix(route string) (string, error) {
	if route == "" {
		return "", fmt.Errorf("compute model limit: no route")
	}
	for i := 0; i < len(route); i += 1 {
		if route[i] < 'a' || 'z' < route[i] {
			return "", fmt.Errorf("compute model limit: the route %q", route)
		}
	}
	return "compute_model_" + route + ".", nil
}

// The counter's key for a route and an address hash, before its window's suffix.
func ComputeModelRouteAddressKey(route string, clientIpHashHex string) (string, error) {
	keyPrefix, err := computeModelRouteKeyPrefix(route)
	if err != nil {
		return "", err
	}
	addressRedisKey, _ := IpRateLimitAttemptRedisKeys(keyPrefix, "", clientIpHashHex)
	return addressRedisKey, nil
}

// Counts one request of route from clientAddress, an ip:port, as of now, against limit requests in each window.
// A count past limit is refused with the rest of its window; a non-positive limit refuses every request; a window
// under a millisecond is an error, as is a count Redis did not keep.
func CountComputeModelRouteAddress(
	ctx context.Context,
	clientAddress string,
	route string,
	window time.Duration,
	limit int64,
	now time.Time,
) (*ComputeModelRouteAddressCount, error) {
	if window < time.Millisecond {
		return nil, fmt.Errorf("compute model limit: a window of %s", window)
	}
	client, err := NewRateLimitClient(clientAddress)
	if err != nil {
		return nil, err
	}
	if client.Excluded() {
		return &ComputeModelRouteAddressCount{Allowed: true, Excluded: true}, nil
	}
	key, err := ComputeModelRouteAddressKey(route, client.IpHashHex())
	if err != nil {
		return nil, err
	}
	count, err := incrementRateLimitWindowToEndAt(ctx, key, window, now)
	if err != nil {
		return nil, err
	}
	result := &ComputeModelRouteAddressCount{
		Allowed: count <= limit,
		Count:   count,
	}
	if !result.Allowed {
		windowMillis := window.Milliseconds()
		result.RetryAfter = time.Duration(windowMillis-now.UnixMilli()%windowMillis) * time.Millisecond
	}
	return result, nil
}
