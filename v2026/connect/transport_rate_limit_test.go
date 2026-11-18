package connect

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Round trip of the connection rate limit counters through the real
// Connect/disconnect paths: under the limits connections are allowed, over
// the burst and total limits they are rejected, and both counters carry ttls.
// Pins the per-ip key formats (`{connect_<ipHashHex>}burst_<window>` and
// `{connect_<ipHashHex>}total_<handlerId>`, sharing one per-ip hash tag so
// the connect tx pipeline stays single-slot). The test redis is standalone,
// not a cluster, so this proves functional equivalence of the per-ip-tag
// keys, not slot placement.
func TestConnectionRateLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		settings := DefaultConnectionRateLimitSettings()
		settings.BurstConnectionCount = 2
		settings.BurstConnectionDelay = 1 * time.Millisecond
		settings.MaxTotalConnectionCount = 3
		settings.TotalConnectionDelay = 1 * time.Millisecond

		// keep clear of a burst window roll so the burst counts below stay in
		// one window
		windowSeconds := int64(settings.BurstDuration / time.Second)
		for {
			nowUnix := server.NowUtc().Unix()
			if 5 <= windowSeconds-nowUnix%windowSeconds {
				break
			}
			select {
			case <-time.After(1 * time.Second):
			}
		}
		window := server.NowUtc().Unix() / windowSeconds

		handlerId := server.NewId()
		clientIp := "203.0.113.7"
		clientAddress := fmt.Sprintf("%s:40000", clientIp)

		clientIpHash := server.ClientIpHashForAddr(netip.MustParseAddr(clientIp))
		clientIpHashHex := hex.EncodeToString(clientIpHash[:])
		burstKey := fmt.Sprintf("{connect_%s}burst_%d", clientIpHashHex, window)
		totalKey := fmt.Sprintf("{connect_%s}total_%s", clientIpHashHex, handlerId)

		newRateLimit := func() *ConnectionRateLimit {
			rateLimit, err := NewConnectionRateLimit(ctx, clientAddress, handlerId, settings)
			connect.AssertEqual(t, nil, err)
			return rateLimit
		}

		// under both limits
		disconnects := []func(){}
		for range 2 {
			err, disconnect := newRateLimit().Connect()
			connect.AssertEqual(t, nil, err)
			disconnects = append(disconnects, disconnect)
		}

		// the third connection exceeds the burst limit (but not the total)
		err, disconnect := newRateLimit().Connect()
		connect.AssertNotEqual(t, nil, err)
		connect.AssertEqual(t, "Burst connection count exceeded.", err.Error())
		disconnects = append(disconnects, disconnect)

		// the fourth connection exceeds the total limit
		err, disconnect = newRateLimit().Connect()
		connect.AssertNotEqual(t, nil, err)
		connect.AssertEqual(t, "Total connection count exceeded.", err.Error())
		disconnects = append(disconnects, disconnect)

		// the counters live under the new per-ip keys and carry ttls
		server.Redis(ctx, func(r server.RedisClient) {
			burstCount, err := r.Get(ctx, burstKey).Int64()
			connect.AssertEqual(t, nil, err)
			connect.AssertEqual(t, int64(4), burstCount)
			burstTtl := r.TTL(ctx, burstKey).Val()
			connect.AssertEqual(t, true, 0 < burstTtl && burstTtl <= settings.BurstDuration)

			totalCount, err := r.Get(ctx, totalKey).Int64()
			connect.AssertEqual(t, nil, err)
			connect.AssertEqual(t, int64(4), totalCount)
			totalTtl := r.TTL(ctx, totalKey).Val()
			connect.AssertEqual(t, true, 0 < totalTtl && totalTtl <= settings.TotalExpiration)
		})

		// disconnect must always be called, even for rate limited connections
		for _, disconnect := range disconnects {
			disconnect()
		}
		server.Redis(ctx, func(r server.RedisClient) {
			totalCount, err := r.Get(ctx, totalKey).Int64()
			connect.AssertEqual(t, nil, err)
			connect.AssertEqual(t, int64(0), totalCount)
			totalTtl := r.TTL(ctx, totalKey).Val()
			connect.AssertEqual(t, true, 0 < totalTtl && totalTtl <= settings.TotalExpiration)
		})

		// a disconnect whose decr recreates an expired total key must not
		// leave it without a ttl
		_, disconnect = newRateLimit().Connect()
		server.Redis(ctx, func(r server.RedisClient) {
			r.Del(ctx, totalKey)
		})
		disconnect()
		server.Redis(ctx, func(r server.RedisClient) {
			totalCount, err := r.Get(ctx, totalKey).Int64()
			connect.AssertEqual(t, nil, err)
			connect.AssertEqual(t, int64(-1), totalCount)
			totalTtl := r.TTL(ctx, totalKey).Val()
			connect.AssertEqual(t, true, 0 < totalTtl && totalTtl <= settings.TotalExpiration)
		})
	})
}
