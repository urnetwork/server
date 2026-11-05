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

// Pure: the count stops at the limit itself (the first release counted to
// limit+1), and a count capped there decides every limit as the full count does.
func TestProxyClientCountBoundIsTheLimit(t *testing.T) {
	for _, limit := range []int{1, 2, 1000} {
		bound := proxyClientCountBound(limit)
		connect.AssertEqual(t, bound, limit)
		for count := 0; count <= limit+3; count += 1 {
			capped := min(count, bound)
			connect.AssertEqual(t, proxyClientLimitReached(limit, capped), proxyClientLimitReached(limit, count))
		}
	}
}

func testAuthProxyClient(t testing.TB, userSession *session.ClientSession) *AuthNetworkClientResult {
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

func testAuthDeviceClient(t testing.TB, userSession *session.ClientSession) *AuthNetworkClientResult {
	result, err := AuthNetworkClient(
		&AuthNetworkClientArgs{Description: "device", DeviceSpec: "device"},
		userSession,
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result != nil, true)
	return result
}

// a client derived from sourceClientId, a hosted proxy when proxy is set
func testAuthDerivedClient(t testing.TB, userSession *session.ClientSession, sourceClientId server.Id, proxy bool) *AuthNetworkClientResult {
	args := &AuthNetworkClientArgs{
		SourceClientId: &sourceClientId,
		Description:    "derived",
		DeviceSpec:     "derived",
	}
	if proxy {
		args.ProxyConfig = &ProxyConfig{
			InitialDeviceState: &ExtendedProxyDeviceState{
				ProxyDeviceState: ProxyDeviceState{
					Location: &sdk.ConnectLocation{
						ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
					},
				},
			},
		}
	}
	result, err := AuthNetworkClient(args, userSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result != nil, true)
	return result
}

func testNetworkClientSourceClientId(ctx context.Context, clientId server.Id) (sourceClientId *server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT source_client_id FROM network_client WHERE client_id = $1`,
			clientId,
		).Scan(&sourceClientId))
	})
	return
}

func testNetworkActiveProxyCount(ctx context.Context, networkId server.Id) (count int) {
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

		first := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, first.Error == nil, true)
		second := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, second.Error == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 2)
		// a proxy created with the network token is top-level: no source client,
		// so the top-level walk's partial index has it
		connect.AssertEqual(t, testNetworkClientSourceClientId(ctx, *first.ClientId) == nil, true)
		connect.AssertEqual(t, testNetworkClientSourceClientId(ctx, *second.ClientId) == nil, true)

		// the free plan's limit is reached: refused, and nothing is created
		third := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, third.Error != nil, true)
		connect.AssertEqual(t, third.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, third.Error.UpgradeRequired, true)
		connect.AssertEqual(t, strings.Contains(third.Error.Message, "2 active proxies"), true)
		connect.AssertEqual(t, third.ClientId == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 2)

		// a client that is not a proxy is neither counted nor refused
		device := testAuthDeviceClient(t, userSession)
		connect.AssertEqual(t, device.Error == nil, true)

		// removing a proxy makes room for the next
		removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: *first.ClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removed.Error == nil, true)
		fourth := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, fourth.Error == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 2)
	})
}

func testProxyLimitNetwork(ctx context.Context) (networkId server.Id, userSession *session.ClientSession) {
	networkId = server.NewId()
	userId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "test", userId)
	userSession = session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: networkId,
		UserId:    userId,
	})
	return
}

// The count's walks visit at most proxyClientLimitScanBudget clients each, so a
// network with hundreds of thousands of active clients does not scan them all
// on every proxy create (0.4 s warm, 3.6 s cold for 324,000 on Main). With a
// budget of 1, neither walk can find the free limit of 2, so a third proxy is
// allowed. With the full budget it is refused. Before the budget, the count
// walked every active client and refused the third proxy at any budget.
func TestAuthNetworkClientProxyLimitScanBudgetBoundsTheWalk(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetConcurrentClientsLimit(2, 1000)()
		ctx := context.Background()
		networkId, userSession := testProxyLimitNetwork(ctx)

		connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)
		connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)

		allowed := func() *AuthNetworkClientResult {
			defer Testing_SetProxyClientLimitScanBudget(1)()
			return testAuthProxyClient(t, userSession)
		}()
		connect.AssertEqual(t, allowed.Error == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 3)

		refused := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, refused.Error != nil, true)
		connect.AssertEqual(t, refused.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 3)
	})
}

// Top-level proxies are counted through the partial index of top-level active
// clients, which skips derived clients. With a budget of 3 and the free limit
// of 2: a device, two top-level proxies, then three clients derived from the
// device. The newest 3 active clients are the derived ones, none a proxy, but
// the network has only three top-level clients, so the top-level walk sees both
// proxies and the next proxy is refused. Before the top-level walk, the count
// saw only the newest window and allowed it.
func TestAuthNetworkClientProxyLimitCountsTopLevelProxiesOutsideTheNewestWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetConcurrentClientsLimit(2, 1000)()
		defer Testing_SetProxyClientLimitScanBudget(3)()
		ctx := context.Background()
		networkId, userSession := testProxyLimitNetwork(ctx)

		device := testAuthDeviceClient(t, userSession)
		connect.AssertEqual(t, device.Error == nil, true)
		connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)
		connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)
		for range 3 {
			derived := testAuthDerivedClient(t, userSession, *device.ClientId, false)
			connect.AssertEqual(t, derived.Error == nil, true)
			connect.AssertEqual(t, testNetworkClientSourceClientId(ctx, *derived.ClientId) != nil, true)
		}

		refused := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, refused.Error != nil, true)
		connect.AssertEqual(t, refused.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 2)
	})
}

// A proxy created with a source client is derived: every proxy a client token
// creates is, since a client token can create only children of its own client.
// The partial index of top-level clients cannot see derived proxies, so the
// newest window counts them: two derived proxies reach the free limit of 2,
// and a third, derived or top-level, is refused. A count of top-level clients
// alone let a network create derived proxies without limit.
func TestAuthNetworkClientProxyLimitCountsDerivedProxies(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetConcurrentClientsLimit(2, 1000)()
		ctx := context.Background()
		networkId, userSession := testProxyLimitNetwork(ctx)

		device := testAuthDeviceClient(t, userSession)
		connect.AssertEqual(t, device.Error == nil, true)
		for range 2 {
			proxy := testAuthDerivedClient(t, userSession, *device.ClientId, true)
			connect.AssertEqual(t, proxy.Error == nil, true)
			sourceClientId := testNetworkClientSourceClientId(ctx, *proxy.ClientId)
			connect.AssertEqual(t, sourceClientId != nil && *sourceClientId == *device.ClientId, true)
		}
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 2)

		derived := testAuthDerivedClient(t, userSession, *device.ClientId, true)
		connect.AssertEqual(t, derived.Error != nil, true)
		connect.AssertEqual(t, derived.Error.ClientLimitExceeded, true)
		topLevel := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, topLevel.Error != nil, true)
		connect.AssertEqual(t, topLevel.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 2)
	})
}
