package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The network a key is registered to, which an activation reads before it
// probes: none for a key never activated, the first activating network's
// after, and still that network's once another network has tried it.
func TestGetNetworkExtenderKeyNetworkId(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-owner-00003")
		sign, _ := testRecordSigner()

		_, registered := GetNetworkExtenderKeyNetworkId(ctx, publicKey)
		connect.AssertEqual(t, registered, false)

		owner := testExtenderActivation(publicKey, 4, "192.0.2.64")
		ActivateNetworkExtender(ctx, owner, sign)
		networkId, registered := GetNetworkExtenderKeyNetworkId(ctx, publicKey)
		connect.AssertEqual(t, registered, true)
		connect.AssertEqual(t, networkId, owner.NetworkId)

		other := testExtenderActivation(publicKey, 4, "192.0.2.65")
		other.NetworkId = server.NewId()
		connect.AssertEqual(t, ActivateNetworkExtender(ctx, other, sign) == nil, true)
		networkId, registered = GetNetworkExtenderKeyNetworkId(ctx, publicKey)
		connect.AssertEqual(t, registered, true)
		connect.AssertEqual(t, networkId, owner.NetworkId)
	})
}
