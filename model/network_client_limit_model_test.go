package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// DB-backed tests (postgres + redis). Under the owner rule these run only
// after the branch, with its migrations, is merged to main.

func provisionTopLevel(t testing.TB, network *dataCapTestNetwork) *AuthNetworkClientResult {
	result, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "install"}, network.rootSession)
	connect.AssertEqual(t, err, nil)
	return result
}

// A session of the network's root token whose client info names deviceType,
// as an app's sign-in sends it ("" for none, as an agent's).
func testClientInfoSession(ctx context.Context, network *dataCapTestNetwork, deviceType string) *session.ClientSession {
	clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: network.networkId, UserId: network.userId})
	if deviceType != "" {
		clientSession.ClientInfo = connect.ClientInfo{Version: 1, DeviceType: deviceType, AppVersion: "2026.11.8"}
	}
	return clientSession
}

// Owner decision 2026-11-08: a URnetwork app's sign-in creates its client while
// the network is at its concurrent limit, and the connection gate holds that
// client off, where the app offers Get Pro and its device list. Every other
// caller is refused at creation as before: an agent, an app's hosted proxy, and
// an API key that claims to be an app. Before, the app's sign-in was refused at
// creation too, with no session left to reach the paywall or the device list.
func TestAuthNetworkClientAppSignInIsHeldAtConnectNotRefusedAtCreation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer Testing_SetEnforceConcurrentClients(true)()
		defer Testing_SetConcurrentClientsLimit(2, 3)()
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()

		network := newDataCapTestNetworkWithoutEmbed(ctx, "app-sign-in")
		for range 2 {
			result := provisionTopLevel(t, network)
			connect.AssertEqual(t, result.Error, (*AuthNetworkClientError)(nil))
			registerTestPeer(t, ctx, network.networkId, *result.ClientId)
		}

		// an agent is refused at creation
		refused, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "agent"}, testClientInfoSession(ctx, network, ""))
		connect.AssertEqual(t, err, nil)
		if refused.Error == nil || !refused.Error.ClientLimitExceeded || !refused.Error.UpgradeRequired {
			t.Fatalf("agent at the limit = %+v, want the concurrent limit refusal", refused.Error)
		}

		// every app's sign-in is created, and held off at the connection gate
		for _, deviceType := range []string{"android", "ios", "macos", "windows", "linux"} {
			created, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: deviceType}, testClientInfoSession(ctx, network, deviceType))
			connect.AssertEqual(t, err, nil)
			if created.Error != nil || created.ClientId == nil {
				t.Fatalf("the %s app's sign-in at the limit = %+v, want a client", deviceType, created.Error)
			}
			connect.AssertEqual(t, CanConnectNetworkPeer(ctx, *created.ClientId, false), false)
		}

		// an app's hosted proxy keeps the creation gate
		proxy := testAuthProxyClient(t, testClientInfoSession(ctx, network, "ios"))
		if proxy.Error == nil || !proxy.Error.ClientLimitExceeded || proxy.ClientId != nil {
			t.Fatalf("an app's proxy at the limit = %+v, want the concurrent limit refusal", proxy.Error)
		}

		// an API key that claims to be an app keeps it too
		apiKeySession := testClientInfoSession(ctx, network, "ios")
		apiKeySession.ApiKeyAuthenticated = true
		refused, err = AuthNetworkClient(&AuthNetworkClientArgs{Description: "api key"}, apiKeySession)
		connect.AssertEqual(t, err, nil)
		if refused.Error == nil || !refused.Error.ClientLimitExceeded {
			t.Fatalf("an api key claiming an app at the limit = %+v, want the concurrent limit refusal", refused.Error)
		}
	})
}

// The concurrent limit's creation refusal offers an upgrade only to a network
// an upgrade lets connect: a free network at the free limit. A Pro network at
// the Pro limit, and a network at its Embed plan row's allowance, are refused
// with client_limit_exceeded and no upgrade_required, so the apps say how to
// free a slot and x402 quotes nothing. Before, every refusal asked for an
// upgrade: the apps put a Pro network at its Pro limit in front of a paywall
// that sold it nothing, and with x402 on it was quoted another Pro month.
func TestAuthNetworkClientConcurrentLimitOffersUpgradeOnlyWhenItLiftsTheLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer Testing_SetEnforceConcurrentClients(true)()
		// free 2 connected, Pro 3
		defer Testing_SetConcurrentClientsLimit(2, 3)()
		defer Testing_ClearNetworkClientLimitCache()

		network := newDataCapTestNetworkWithoutEmbed(ctx, "upgrade")
		clientIds := []server.Id{}
		for range 3 {
			result := provisionTopLevel(t, network)
			connect.AssertEqual(t, result.Error, (*AuthNetworkClientError)(nil))
			clientIds = append(clientIds, *result.ClientId)
		}

		// free with two connected: Pro lifts the limit
		registerTestPeer(t, ctx, network.networkId, clientIds[0])
		registerTestPeer(t, ctx, network.networkId, clientIds[1])
		refused := provisionTopLevel(t, network)
		if refused.Error == nil {
			t.Fatal("the free tier's concurrent limit did not refuse creation")
		}
		connect.AssertEqual(t, refused.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, refused.Error.UpgradeRequired, true)
		connect.AssertEqual(t, refused.ClientId == nil, true)

		// Pro, under the Pro limit: created
		balanceTime := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(AddProTransferBalanceInTx(tx, ctx, network.networkId, ByteCount(1024*1024), balanceTime, balanceTime.Add(time.Hour)))
		})
		connect.AssertEqual(t, IsProFresh(ctx, &network.networkId), true)
		connect.AssertEqual(t, provisionTopLevel(t, network).Error, (*AuthNetworkClientError)(nil))

		// Pro at the Pro limit: no upgrade lifts it
		registerTestPeer(t, ctx, network.networkId, clientIds[2])
		refused = provisionTopLevel(t, network)
		if refused.Error == nil {
			t.Fatal("the Pro tier's concurrent limit did not refuse creation")
		}
		connect.AssertEqual(t, refused.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, refused.Error.UpgradeRequired, false)
		connect.AssertEqual(t, refused.ClientId == nil, true)

		// a free network at its Embed plan row's allowance: no plan changes it
		embedNetwork := newDataCapTestNetwork(ctx, "embed-upgrade")
		embedClientIds := []server.Id{}
		for range 2 {
			result := provisionTopLevel(t, embedNetwork)
			connect.AssertEqual(t, result.Error, (*AuthNetworkClientError)(nil))
			embedClientIds = append(embedClientIds, *result.ClientId)
		}
		connect.AssertEqual(t, SetNetworkTopLevelClientLimit(ctx, embedNetwork.networkId, 2), nil)
		registerTestPeer(t, ctx, embedNetwork.networkId, embedClientIds[0])
		registerTestPeer(t, ctx, embedNetwork.networkId, embedClientIds[1])
		refused = provisionTopLevel(t, embedNetwork)
		if refused.Error == nil {
			t.Fatal("the Embed plan's allowance did not refuse creation")
		}
		connect.AssertEqual(t, refused.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, refused.Error.UpgradeRequired, false)
	})
}

// An Embed plan row is the network's allowance for both limits: the create
// cap and the concurrent connection limit (creation's upgrade_required and
// connection activation). Without a row the tier's concurrent_clients applies.
func TestNetworkClientAllowanceAppliesToTheConcurrentLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer Testing_SetEnforceConcurrentClients(true)()
		// a free network may hold two connected top-level clients, and Pro
		// three, so the free tier's refusal offers the upgrade
		defer Testing_SetConcurrentClientsLimit(2, 3)()
		defer Testing_ClearNetworkClientLimitCache()

		network := newDataCapTestNetwork(ctx, "embed")
		clientIds := []server.Id{}
		for range 3 {
			result := provisionTopLevel(t, network)
			connect.AssertEqual(t, result.Error, (*AuthNetworkClientError)(nil))
			clientIds = append(clientIds, *result.ClientId)
		}

		// two connected: the tier is full
		registerTestPeer(t, ctx, network.networkId, clientIds[0])
		registerTestPeer(t, ctx, network.networkId, clientIds[1])
		connect.AssertEqual(t, NetworkConcurrentClientsExceeded(ctx, network.networkId), true)
		connect.AssertEqual(t, CanConnectNetworkPeer(ctx, clientIds[2], false), false)
		// a connected client re-nominating is not a new connection
		connect.AssertEqual(t, CanConnectNetworkPeer(ctx, clientIds[0], false), true)
		result := provisionTopLevel(t, network)
		if result.Error == nil || !result.Error.UpgradeRequired {
			t.Fatal("the tier's concurrent limit did not refuse creation")
		}
		connect.AssertEqual(t, networkNormalClientLimit(ctx, network.networkId), 2)

		// an Embed plan of 5 lifts both gates at once (Set refreshes this
		// process's cache)
		connect.AssertEqual(t, SetNetworkTopLevelClientLimit(ctx, network.networkId, 5), nil)
		connect.AssertEqual(t, networkConcurrentClientLimit(ctx, network.networkId), 5)
		connect.AssertEqual(t, networkNormalClientLimit(ctx, network.networkId), 5)
		connect.AssertEqual(t, NetworkConcurrentClientsExceeded(ctx, network.networkId), false)
		connect.AssertEqual(t, CanConnectNetworkPeer(ctx, clientIds[2], false), true)
		result = provisionTopLevel(t, network)
		connect.AssertEqual(t, result.Error, (*AuthNetworkClientError)(nil))
		clientIds = append(clientIds, *result.ClientId)
		result = provisionTopLevel(t, network)
		connect.AssertEqual(t, result.Error, (*AuthNetworkClientError)(nil))
		clientIds = append(clientIds, *result.ClientId)

		// up to the allowance: five connected fills it
		for _, clientId := range clientIds[2:5] {
			registerTestPeer(t, ctx, network.networkId, clientId)
		}
		connect.AssertEqual(t, NetworkConcurrentClientsExceeded(ctx, network.networkId), true)
		// the create cap is the same allowance: five top-level clients exist
		result = provisionTopLevel(t, network)
		if result.Error == nil || !result.Error.ClientLimitExceeded {
			t.Fatal("the allowance did not refuse creation at five")
		}

		// an isolated client still counts toward both limits
		connect.AssertEqual(t, setAclGroup(t, network.rootSession, clientIds[4], NetworkClientAclGroupIsolated).Error, (*NetworkClientAclGroupError)(nil))
		connect.AssertEqual(t, NetworkConcurrentClientsExceeded(ctx, network.networkId), true)

		// clearing returns to the tier, which five connected exceeds
		ClearNetworkTopLevelClientLimit(ctx, network.networkId)
		connect.AssertEqual(t, networkConcurrentClientLimit(ctx, network.networkId), 2)
		connect.AssertEqual(t, NetworkConcurrentClientsExceeded(ctx, network.networkId), true)

		// enforcement off: nothing is refused, whatever the counts
		func() {
			defer Testing_SetEnforceConcurrentClients(false)()
			connect.AssertEqual(t, NetworkConcurrentClientsExceeded(ctx, network.networkId), false)
			connect.AssertEqual(t, CanConnectNetworkPeer(ctx, server.NewId(), false), true)
			connect.AssertEqual(t, provisionTopLevel(t, network).Error, (*AuthNetworkClientError)(nil))
		}()
	})
}

// A row written outside this process (another api host, or ops) is seen once
// the cached entry expires; a cleared cache sees it at once.
func TestNetworkClientLimitOverrideCache(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer Testing_ClearNetworkClientLimitCache()

		network := newDataCapTestNetwork(ctx, "embed")
		limit, override := networkClientLimitOverride(ctx, network.networkId)
		connect.AssertEqual(t, limit, LimitTopLevelClientIdsPerNetwork)
		connect.AssertEqual(t, override, false)

		// another host sets the row directly: this process still has the
		// cached default
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_top_level_client_limit (network_id, top_level_client_limit, update_time)
					VALUES ($1, $2, $3)
				`,
				network.networkId,
				7,
				server.NowUtc(),
			))
		})
		_, override = networkClientLimitOverride(ctx, network.networkId)
		connect.AssertEqual(t, override, false)

		Testing_ClearNetworkClientLimitCache()
		limit, override = networkClientLimitOverride(ctx, network.networkId)
		connect.AssertEqual(t, limit, 7)
		connect.AssertEqual(t, override, true)

		// Set and Clear refresh this process at once
		connect.AssertEqual(t, SetNetworkTopLevelClientLimit(ctx, network.networkId, 9), nil)
		limit, _ = networkClientLimitOverride(ctx, network.networkId)
		connect.AssertEqual(t, limit, 9)
		ClearNetworkTopLevelClientLimit(ctx, network.networkId)
		_, override = networkClientLimitOverride(ctx, network.networkId)
		connect.AssertEqual(t, override, false)
	})
}

// The create cap reads the row inside the provisioning transaction.
func TestNetworkTopLevelClientLimitInTx(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := newDataCapTestNetwork(ctx, "embed")
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, networkTopLevelClientLimitInTx(ctx, tx, network.networkId), LimitTopLevelClientIdsPerNetwork)
		})
		connect.AssertEqual(t, SetNetworkTopLevelClientLimit(ctx, network.networkId, 250), nil)
		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, networkTopLevelClientLimitInTx(ctx, tx, network.networkId), 250)
		})
	})
}
