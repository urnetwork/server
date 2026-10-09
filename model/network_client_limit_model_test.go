package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// DB-backed tests (postgres + redis). Under the owner rule these run only
// after the branch, with its migrations, is merged to main.

func provisionTopLevel(t testing.TB, network *dataCapTestNetwork) *AuthNetworkClientResult {
	result, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "install"}, network.rootSession)
	connect.AssertEqual(t, err, nil)
	return result
}

// An Embed plan row is the network's allowance for both limits: the create
// cap and the concurrent connection limit (creation's upgrade_required and
// connection activation). Without a row the tier's concurrent_clients applies.
func TestNetworkClientAllowanceAppliesToTheConcurrentLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer Testing_SetEnforceConcurrentClients(true)()
		// a free network may hold two connected top-level clients
		defer Testing_SetConcurrentClientsLimit(2, 2)()
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
