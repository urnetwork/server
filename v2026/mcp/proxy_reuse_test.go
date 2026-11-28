package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Every fetch without a live handle used to provision a new hosted proxy, and
// nothing removed one: each new conversation added an active proxy that
// counted against the network's proxy cap. Two calls for the same location
// now share the network's proxy, the handle they return reuses it as before,
// and a removed proxy is replaced by a new one.
func TestAcquireProxyReusesTheNetworksProxyForALocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		networkName := fmt.Sprintf("mcp-reuse-%s", networkId)
		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
		clientSession := session.NewLocalClientSession(
			ctx,
			"",
			session.NewByJwt(networkId, userId, networkName, false, false),
		)
		defer clientSession.Cancel()
		binding := identityStateBinding(userId.String(), networkId, "oauth-client", McpResource)

		activeProxyCount := func() int {
			result, err := model.GetNetworkProxies(clientSession)
			connect.AssertEqual(t, err, nil)
			return len(result.Clients)
		}

		first, upgrade, err := acquireProxy(clientSession, binding, "", "")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, upgrade == nil, true)
		connect.AssertEqual(t, first.created, true)

		// a second call without a handle (a new conversation) shares the proxy
		second, upgrade, err := acquireProxy(clientSession, binding, "", "")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, upgrade == nil, true)
		connect.AssertEqual(t, second.created, false)
		connect.AssertEqual(t, second.proxyClient.ProxyId, first.proxyClient.ProxyId)
		connect.AssertEqual(t, activeProxyCount(), 1)

		// the returned handle reuses it too
		third, upgrade, err := acquireProxy(clientSession, binding, second.signedProxyId, "")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, upgrade == nil, true)
		connect.AssertEqual(t, third.proxyClient.ProxyId, first.proxyClient.ProxyId)

		// a removed proxy is replaced, not resurrected
		removed, err := model.RemoveNetworkClient(
			&model.RemoveNetworkClientArgs{ClientId: first.proxyClient.ClientId},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removed.Error == nil, true)
		replaced, upgrade, err := acquireProxy(clientSession, binding, "", "")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, upgrade == nil, true)
		connect.AssertEqual(t, replaced.created, true)
		connect.AssertNotEqual(t, replaced.proxyClient.ProxyId, first.proxyClient.ProxyId)
		connect.AssertEqual(t, activeProxyCount(), 1)
	})
}
