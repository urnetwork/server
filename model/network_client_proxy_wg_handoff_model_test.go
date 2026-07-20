package model

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// The wg handoff store carries the drained instance's peer endpoints to the
// replacement instance, consumed exactly once (PROXYDRAIN1.md §3.4).
func TestProxyWgHandoff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		proxyHost := fmt.Sprintf("handofftest%d", time.Now().UnixNano())
		block := "g1"

		// absent: zero time, no peers
		exportTime, peers := TakeProxyWgHandoff(ctx, proxyHost, block)
		connect.AssertEqual(t, true, exportTime.IsZero())
		connect.AssertEqual(t, 0, len(peers))

		// the ttl comes from the caller's settings
		// (ProxySettings.WgHandoffRequestTtl); the model only guards <= 0
		ttl := 10 * time.Minute

		now := server.NowUtc()
		exported := []*ProxyWgHandoffPeer{
			{
				ClientIpv4:        netip.MustParseAddr("10.5.0.2"),
				Endpoint:          "203.0.113.9:51820",
				LastHandshakeTime: now.Add(-30 * time.Second),
			},
			{
				ClientIpv4:        netip.MustParseAddr("10.5.0.3"),
				Endpoint:          "198.51.100.4:40001",
				LastHandshakeTime: now.Add(-2 * time.Minute),
			},
		}
		SetProxyWgHandoff(ctx, proxyHost, block, now, exported, ttl)

		exportTime, peers = TakeProxyWgHandoff(ctx, proxyHost, block)
		connect.AssertEqual(t, true, exportTime.Equal(now))
		connect.AssertEqual(t, 2, len(peers))
		byAddr := map[netip.Addr]*ProxyWgHandoffPeer{}
		for _, peer := range peers {
			byAddr[peer.ClientIpv4] = peer
		}
		peer := byAddr[netip.MustParseAddr("10.5.0.2")]
		connect.AssertEqual(t, true, peer != nil)
		connect.AssertEqual(t, "203.0.113.9:51820", peer.Endpoint)
		connect.AssertEqual(t, true, peer.LastHandshakeTime.Equal(now.Add(-30*time.Second)))

		// consumed exactly once (getdel): a crash-looping replacement must
		// not re-initiate from a stale export
		exportTime, peers = TakeProxyWgHandoff(ctx, proxyHost, block)
		connect.AssertEqual(t, true, exportTime.IsZero())
		connect.AssertEqual(t, 0, len(peers))

		// a newer export replaces an older one
		SetProxyWgHandoff(ctx, proxyHost, block, now, exported[:1], ttl)
		SetProxyWgHandoff(ctx, proxyHost, block, now.Add(time.Second), exported[1:], ttl)
		exportTime, peers = TakeProxyWgHandoff(ctx, proxyHost, block)
		connect.AssertEqual(t, true, exportTime.Equal(now.Add(time.Second)))
		connect.AssertEqual(t, 1, len(peers))
		connect.AssertEqual(t, netip.MustParseAddr("10.5.0.3"), peers[0].ClientIpv4)

		// Deploy generations are isolated. The replacement publishes its
		// generation before redirect; the old instance reads that marker and
		// writes only that generation's handoff key.
		generation := server.NewId().String()
		otherGeneration := server.NewId().String()
		BeginProxyWgHandoff(ctx, proxyHost, block, generation, ttl)
		connect.AssertEqual(t, generation, CurrentProxyWgHandoffGeneration(ctx, proxyHost, block))
		SetProxyWgHandoffForGeneration(ctx, proxyHost, block, generation, now, nil, ttl)

		exportTime, peers = TakeProxyWgHandoffForGeneration(ctx, proxyHost, block, otherGeneration)
		connect.AssertEqual(t, true, exportTime.IsZero())
		connect.AssertEqual(t, 0, len(peers))
		exportTime, peers = TakeProxyWgHandoffForGeneration(ctx, proxyHost, block, generation)
		connect.AssertEqual(t, true, exportTime.Equal(now))
		connect.AssertEqual(t, 0, len(peers))

		// The caller-provided ttl is actually applied to both keys: a short
		// ttl expires the generation request and the export.
		shortTtl := 100 * time.Millisecond
		BeginProxyWgHandoff(ctx, proxyHost, block, generation, shortTtl)
		SetProxyWgHandoffForGeneration(ctx, proxyHost, block, generation, now, exported, shortTtl)
		select {
		case <-time.After(500 * time.Millisecond):
		}
		connect.AssertEqual(t, "", CurrentProxyWgHandoffGeneration(ctx, proxyHost, block))
		exportTime, peers = TakeProxyWgHandoffForGeneration(ctx, proxyHost, block, generation)
		connect.AssertEqual(t, true, exportTime.IsZero())
		connect.AssertEqual(t, 0, len(peers))
	})
}
