package controller

import (
	"bytes"
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A freshly allocated WireGuard egress enters the same keyed namespace the
// /verify reader uses. This catches an unkeyed model-default feed before the
// periodic refresh can turn one real address into two reverse-index entries.
func TestAuthNetworkClientFeedsConfiguredProxyEgressNamespace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientID := server.NewId()
		ip := netip.MustParseAddr("2001:db8:31::1")
		settings := model.DefaultVerifySettings()
		settings.EgressHashKey = bytes.Repeat([]byte{0x31}, 32)
		result := &model.AuthNetworkClientResult{
			ClientId: &clientID,
			ProxyConfigResult: &model.ProxyConfigResult{
				ProxyClient: model.ProxyClient{
					WgConfig: &model.WgConfig{ClientIpv4: ip},
				},
			},
		}

		feedAuthNetworkClientVerifyEgress(ctx, result, settings)
		defer model.RemoveVerifyEgressForClient(ctx, clientID)
		got := model.ResolveVerifyEgress(ctx, ip, settings)
		if got == nil || *got != clientID {
			t.Fatalf("configured proxy namespace resolved %v, want %s", got, clientID)
		}
		if wrong := model.ResolveVerifyEgress(ctx, ip, model.DefaultVerifySettings()); wrong != nil {
			t.Fatalf("unkeyed namespace attributed configured proxy %s", *wrong)
		}
	})
}
