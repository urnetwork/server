package model

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Pure: the threshold, and that no pro.yml (limit 0) never refuses.
func TestProxyClientLimitReached(t *testing.T) {
	connect.AssertEqual(t, proxyClientLimitReached(0, 1_000_000), false)
	connect.AssertEqual(t, proxyClientLimitReached(-1, 5), false)
	connect.AssertEqual(t, proxyClientLimitReached(2, 1), false)
	connect.AssertEqual(t, proxyClientLimitReached(2, 2), true)
	connect.AssertEqual(t, proxyClientLimitReached(1000, 300006), true)
	connect.AssertEqual(t, strings.Contains(proxyClientLimitMessage(1000, true), "1000 active proxies"), true)
	connect.AssertEqual(t, strings.Contains(proxyClientLimitMessage(2, false), "upgrade to UR Pro"), true)
}

// 2026-11-04/05: one network held 300,006 active hosted proxies and created
// 14-22k more an hour, and the two proxy hosts ran at 100% CPU. A network now
// holds at most its plan's number of active hosted proxies (free 2, Pro 1000):
// the next proxy is refused and nothing is created, a client that is not a proxy
// is neither counted nor refused, and removing a proxy makes room for the next.
// Before the limit, the third proxy was created.
func TestAuthNetworkClientCapsActiveProxiesAtThePlanLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetConcurrentClientsLimit(2, 1000)()
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)
		userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})

		authProxy := func() *AuthNetworkClientResult {
			result, err := AuthNetworkClient(
				&AuthNetworkClientArgs{
					Description: "proxy",
					DeviceSpec:  "proxy",
					ProxyConfig: &ProxyConfig{
						InitialDeviceState: &ExtendedProxyDeviceState{
							ProxyDeviceState: ProxyDeviceState{
								Location: &sdk.ConnectLocation{
									ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
								},
							},
						},
					},
				},
				userSession,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result != nil, true)
			return result
		}
		activeProxies := func() (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`
						SELECT count(*)
						FROM network_client
						INNER JOIN proxy_client ON proxy_client.client_id = network_client.client_id
						WHERE network_client.network_id = $1 AND network_client.active = true
					`,
					networkId,
				).Scan(&count))
			})
			return
		}

		first := authProxy()
		connect.AssertEqual(t, first.Error == nil, true)
		second := authProxy()
		connect.AssertEqual(t, second.Error == nil, true)
		connect.AssertEqual(t, activeProxies(), 2)

		// the free plan's limit is reached: refused, and nothing is created
		third := authProxy()
		connect.AssertEqual(t, third.Error != nil, true)
		connect.AssertEqual(t, third.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, third.Error.UpgradeRequired, true)
		connect.AssertEqual(t, strings.Contains(third.Error.Message, "2 active proxies"), true)
		connect.AssertEqual(t, third.ClientId == nil, true)
		connect.AssertEqual(t, activeProxies(), 2)

		// a client that is not a proxy is neither counted nor refused
		device, err := AuthNetworkClient(
			&AuthNetworkClientArgs{Description: "device", DeviceSpec: "device"},
			userSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, device.Error == nil, true)

		// removing a proxy makes room for the next
		removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: *first.ClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removed.Error == nil, true)
		fourth := authProxy()
		connect.AssertEqual(t, fourth.Error == nil, true)
		connect.AssertEqual(t, activeProxies(), 2)
	})
}
