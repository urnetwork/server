package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Pure: a config matches on its initial location's id only.
func TestReusableProxyLocationMatches(t *testing.T) {
	bestAvailableJson := []byte(`{"best_available":true}`)
	connect.AssertEqual(t, reusableProxyLocationMatches(`{"initial_device_state":{"location":{"connect_location_id":{"best_available":true},"name":"Best"}}}`, bestAvailableJson), true)
	connect.AssertEqual(t, reusableProxyLocationMatches(`{"initial_device_state":{"location":{"connect_location_id":{"best_available":true}}}}`, bestAvailableJson), true)
	connect.AssertEqual(t, reusableProxyLocationMatches(`{"initial_device_state":{"location":{"connect_location_id":{"location_id":"01a1a8fc-c420-0000-0000-000000000000"}}}}`, bestAvailableJson), false)
	connect.AssertEqual(t, reusableProxyLocationMatches(`{"initial_device_state":null}`, bestAvailableJson), false)
	connect.AssertEqual(t, reusableProxyLocationMatches(`not json`, bestAvailableJson), false)
}

// The MCP fetch tool created a new hosted proxy for every call without a live
// handle, and never removed one. A caller now finds the newest active proxy it
// created for the network and location: never one created by another caller
// (device spec) or for another location or network, never a removed one, and
// only within the scan window.
func TestFindReusableNetworkProxy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		// this test creates more proxies than the free plan's proxy cap
		defer Testing_SetConcurrentClientsLimit(1000, 1000)()
		ctx := context.Background()
		settings := DefaultReusableNetworkProxySettings()

		newSession := func() (server.Id, *session.ClientSession) {
			networkId := server.NewId()
			userId := server.NewId()
			Testing_CreateNetwork(ctx, networkId, "reuse-"+networkId.String(), userId)
			return networkId, session.Testing_CreateClientSession(ctx, &session.ByJwt{
				NetworkId: networkId,
				UserId:    userId,
			})
		}
		networkId, userSession := newSession()
		otherNetworkId, otherSession := newSession()

		bestAvailable := &sdk.ConnectLocation{
			ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
		}
		locationId := sdk.NewId()
		inLocation := &sdk.ConnectLocation{
			ConnectLocationId: &sdk.ConnectLocationId{LocationId: locationId},
			Name:              "Somewhere",
		}
		otherLocationId := sdk.NewId()
		inOtherLocation := &sdk.ConnectLocation{
			ConnectLocationId: &sdk.ConnectLocationId{LocationId: otherLocationId},
		}

		createProxy := func(clientSession *session.ClientSession, deviceSpec string, location *sdk.ConnectLocation) (server.Id, server.Id) {
			result, err := AuthNetworkClient(
				&AuthNetworkClientArgs{
					Description: deviceSpec,
					DeviceSpec:  deviceSpec,
					ProxyConfig: &ProxyConfig{
						InitialDeviceState: &ExtendedProxyDeviceState{
							ProxyDeviceState: ProxyDeviceState{Location: location},
						},
					},
				},
				clientSession,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error == nil, true)
			connect.AssertEqual(t, result.ProxyConfigResult != nil, true)
			return result.ProxyConfigResult.ProxyId, result.ProxyConfigResult.ClientId
		}
		find := func(networkId server.Id, deviceSpec string, location *sdk.ConnectLocation) *server.Id {
			return FindReusableNetworkProxy(ctx, networkId, deviceSpec, location, settings)
		}

		connect.AssertEqual(t, find(networkId, "mcp", bestAvailable) == nil, true)

		bestProxyId, _ := createProxy(userSession, "mcp", bestAvailable)
		locationProxyId, _ := createProxy(userSession, "mcp", inLocation)
		userProxyId, _ := createProxy(userSession, "ur.io proxy https", bestAvailable)
		otherNetworkProxyId, _ := createProxy(otherSession, "mcp", inOtherLocation)

		// by location, never another caller's or another network's proxy
		connect.AssertEqual(t, *find(networkId, "mcp", bestAvailable), bestProxyId)
		connect.AssertEqual(t, *find(networkId, "mcp", &sdk.ConnectLocation{
			ConnectLocationId: &sdk.ConnectLocationId{LocationId: locationId},
		}), locationProxyId)
		connect.AssertEqual(t, find(networkId, "mcp", inOtherLocation) == nil, true)
		connect.AssertEqual(t, *find(networkId, "ur.io proxy https", bestAvailable), userProxyId)
		connect.AssertEqual(t, *find(otherNetworkId, "mcp", inOtherLocation), otherNetworkProxyId)
		connect.AssertEqual(t, find(otherNetworkId, "mcp", bestAvailable) == nil, true)

		// the newest of two matches, and the older one once it is removed
		newerBestProxyId, newerBestClientId := createProxy(userSession, "mcp", bestAvailable)
		connect.AssertEqual(t, *find(networkId, "mcp", bestAvailable), newerBestProxyId)
		removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: newerBestClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removed.Error == nil, true)
		connect.AssertEqual(t, *find(networkId, "mcp", bestAvailable), bestProxyId)

		// the window: the newest active client is the user's proxy, so a window of
		// one finds nothing, and a disabled window never finds anything
		connect.AssertEqual(t, FindReusableNetworkProxy(ctx, networkId, "mcp", bestAvailable, &ReusableNetworkProxySettings{ScanLimit: 1}) == nil, true)
		connect.AssertEqual(t, FindReusableNetworkProxy(ctx, networkId, "mcp", bestAvailable, &ReusableNetworkProxySettings{ScanLimit: 0}) == nil, true)
	})
}
