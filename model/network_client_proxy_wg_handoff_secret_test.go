package model

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A blank handoff_encryption_key sealed the handoff under sha256(""), so the
// peers' raw endpoints were stored readable by anyone. Without a key the
// handoff, best effort by design, is not stored at all.
func TestSetProxyWgHandoffStoresNothingWithoutKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		pop := server.Vault.PushSimpleResource("wireguard.yml", []byte("handoff_encryption_key: ''\n"))
		defer pop()
		defer server.Testing_ReloadWgHandoffKey()()

		proxyHost := fmt.Sprintf("handoffsecrettest%d", time.Now().UnixNano())
		now := server.NowUtc()
		SetProxyWgHandoffForGeneration(ctx, proxyHost, "g1", "synthetic-generation", now, []*ProxyWgHandoffPeer{
			{
				ClientIpv4:        netip.MustParseAddr("10.5.0.2"),
				Endpoint:          "203.0.113.9:51820",
				LastHandshakeTime: now.Add(-30 * time.Second),
			},
		}, time.Minute)

		exportTime, peers := TakeProxyWgHandoffForGeneration(ctx, proxyHost, "g1", "synthetic-generation")
		if !exportTime.IsZero() || len(peers) != 0 {
			t.Fatalf("a handoff was stored without a key: %v %v", exportTime, peers)
		}
	})
}
