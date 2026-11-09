//go:build sidecar_pending

package model

// Counting sidecar clients (DESIGN.md 20.8): §20.8's table, the sidecar_clients gates at
// enrollment and connect, and the create cap per tier. MG0 holds the owner row, so these
// run under -tags sidecar_pending, through .stack/server-test.sh.
// sidecar_client_count_release_test.go holds the release leg.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A client of the network with an owner row of these marks, or none when the client is
// an app device without one. A child takes parentId as its source.
func testingSidecarCountClient(t testing.TB, ctx context.Context, networkId server.Id, parentId *server.Id, owner bool, strictExp bool, declaresNoEgress bool) server.Id {
	t.Helper()
	clientId := server.NewId()
	if parentId == nil {
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic", "synthetic")
	} else {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_client (client_id, network_id, device_id, source_client_id, description, create_time, auth_time)
					SELECT $1, network_id, device_id, client_id, 'synthetic', $3, $3
					FROM network_client WHERE client_id = $2
				`,
				clientId,
				*parentId,
				server.NowUtc(),
			))
		})
	}
	if owner {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_client_owner (client_id, admin_refused, strict_exp, declares_no_egress)
					VALUES ($1, true, $2, $3)
				`,
				clientId,
				strictExp,
				declaresNoEgress,
			))
		})
	}
	return clientId
}

// §20.8's four kinds of client in one network.
type sidecarCountKinds struct {
	networkId        server.Id
	appDevice        server.Id
	sidecarChild     server.Id
	declaredSidecar  server.Id
	egressSidecar    server.Id
	ordinaryOccupant server.Id
}

func testingSidecarCountKinds(t testing.TB, ctx context.Context) *sidecarCountKinds {
	t.Helper()
	kinds := &sidecarCountKinds{networkId: server.NewId()}
	Testing_CreateNetwork(ctx, kinds.networkId, fmt.Sprintf("sidecarcount%s", kinds.networkId), server.NewId())
	// an app device client, minted admin_refused with no sidecar mark
	kinds.appDevice = testingSidecarCountClient(t, ctx, kinds.networkId, nil, true, false, false)
	kinds.sidecarChild = testingSidecarCountClient(t, ctx, kinds.networkId, &kinds.appDevice, true, true, false)
	kinds.declaredSidecar = testingSidecarCountClient(t, ctx, kinds.networkId, nil, true, true, true)
	kinds.egressSidecar = testingSidecarCountClient(t, ctx, kinds.networkId, nil, true, true, false)
	// a legacy client with no owner row, which takes the one concurrent slot
	kinds.ordinaryOccupant = testingSidecarCountClient(t, ctx, kinds.networkId, nil, false, false, false)
	Testing_ClearNetworkPeersEnabledCache()
	return kinds
}

// §20.8's table as a table: each kind of client against each of the three counters, the
// limits enforced and dark.
func TestSidecarClientCountTable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if !server.SidecarMg0Linked() {
			t.Fatal("a pending build does not link MG0")
		}
		kinds := testingSidecarCountKinds(t, ctx)

		// concurrent_clients: one slot, held by the ordinary occupant
		defer Testing_SetEnforceConcurrentClients(true)()
		defer Testing_SetConcurrentClientsLimit(1, 1)()
		AddNetworkPeer(ctx, kinds.networkId, &NetworkPeer{ClientId: kinds.ordinaryOccupant}, server.NewId(), time.Hour)

		for _, c := range []struct {
			name     string
			clientId server.Id
			// whether it takes a concurrent slot, counts toward the create cap and
			// the peer valve, and counts toward sidecar_clients
			concurrent bool
			createCap  bool
			sidecar    bool
			category   NetworkPeerCategory
			topLevel   bool
		}{
			{name: "app device", clientId: kinds.appDevice, concurrent: true, createCap: true, sidecar: false, category: NetworkPeerCategoryClient, topLevel: true},
			{name: "sidecar child", clientId: kinds.sidecarChild, concurrent: false, createCap: false, sidecar: true, category: NetworkPeerCategoryClient, topLevel: false},
			{name: "declared no-egress", clientId: kinds.declaredSidecar, concurrent: false, createCap: false, sidecar: true, category: NetworkPeerCategorySidecar, topLevel: true},
			{name: "with egress", clientId: kinds.egressSidecar, concurrent: true, createCap: true, sidecar: true, category: NetworkPeerCategoryClient, topLevel: true},
		} {
			_, topLevel, category, peer, _ := GetNetworkPeerProfile(ctx, c.clientId)
			if peer == nil || topLevel != c.topLevel || category != c.category {
				t.Errorf("%s: top-level %t, category %d", c.name, topLevel, category)
			}
			// the concurrent gate refuses a counted client with the slot taken,
			// and passes an exempt one
			if canConnect := CanConnectNetworkPeer(ctx, c.clientId, false); canConnect == c.concurrent {
				t.Errorf("%s: concurrent gate passes %t, want counted %t", c.name, canConnect, c.concurrent)
			}
			_, marked := getSidecarClientMark(ctx, c.clientId)
			connect.AssertEqual(t, marked, c.sidecar)
		}

		// the create cap's count: the app device, the sidecar with egress and the
		// ordinary occupant; no child and no declared client
		var count int
		server.Tx(ctx, func(tx server.PgTx) {
			count = countNetworkActiveTopLevelClients(ctx, tx, kinds.networkId, 100)
		})
		connect.AssertEqual(t, count, 3)

		// sidecar_clients counts every marked client its residents register, child
		// and declared included, and never the app device
		defer Testing_SetEnforceSidecarLimits(true)()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"sidecar_clients": 20})()
		connect.AssertEqual(t, SidecarClientCountingOn(), true)
		for _, clientId := range []server.Id{kinds.sidecarChild, kinds.declaredSidecar, kinds.egressSidecar} {
			AddNetworkSidecarClient(ctx, kinds.networkId, clientId, time.Hour)
		}
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, kinds.networkId), int64(3))
		// the {np_} count is untouched by them: one ordinary occupant
		connect.AssertEqual(t, GetNetworkEnforceableConnectedCount(ctx, kinds.networkId), 1)

		// dark: nothing is counted or refused, and the gates do no I/O
		Testing_SetEnforceSidecarLimits(false)
		connect.AssertEqual(t, SidecarClientCountingOn(), false)
		attempts, stop := server.DenyPostgresForTest(t)
		for _, clientId := range []server.Id{kinds.appDevice, kinds.sidecarChild, kinds.declaredSidecar, kinds.egressSidecar} {
			connect.AssertEqual(t, CanConnectSidecarClient(ctx, clientId), true)
		}
		connect.AssertEqual(t, SidecarClientsExceeded(ctx, kinds.networkId) == nil, true)
		stop()
		connect.AssertEqual(t, attempts(), uint64(0))
	})
}

// A declared client is never listed: its category keeps it out of the peer registry,
// and the peer valve does not count it, so 101 of them leave a network's peers on.
func TestDeclaredClientUnlisted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("declared%s", networkId), server.NewId())
		for i := 0; i < LimitTopLevelClientIdsPerNetwork+1; i += 1 {
			testingSidecarCountClient(t, ctx, networkId, nil, true, true, true)
		}
		Testing_ClearNetworkPeersEnabledCache()
		connect.AssertEqual(t, NetworkPeersEnabled(ctx, networkId), true)

		// the same count of ordinary clients turns the peer list off
		other := server.NewId()
		Testing_CreateNetwork(ctx, other, fmt.Sprintf("ordinary%s", other), server.NewId())
		for i := 0; i < LimitTopLevelClientIdsPerNetwork+1; i += 1 {
			testingSidecarCountClient(t, ctx, other, nil, false, false, false)
		}
		Testing_ClearNetworkPeersEnabledCache()
		connect.AssertEqual(t, NetworkPeersEnabled(ctx, other), false)

		declared := testingSidecarCountClient(t, ctx, networkId, nil, true, true, true)
		_, topLevel, category, peer, _ := GetNetworkPeerProfile(ctx, declared)
		connect.AssertEqual(t, topLevel, true)
		connect.AssertEqual(t, peer != nil, true)
		connect.AssertEqual(t, category, NetworkPeerCategorySidecar)
	})
}

// The 21st connected Community sidecar: enrollment's check answers sidecar_clients'
// refusal, an upgrade since Pro lifts it, and the connect gate refuses a new one while a
// re-nomination of a connected one and a client with no mark pass.
func TestSidecarClientsLimitAtEnrollmentAndConnect(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetEnforceSidecarLimits(true)()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"sidecar_clients": 20})()
		defer Testing_SetSidecarLimits(NetworkTierPro, map[string]int64{"sidecar_clients": 200})()
		kinds := testingSidecarCountKinds(t, ctx)

		connected := []server.Id{}
		for i := 0; i < 20; i += 1 {
			clientId := testingSidecarCountClient(t, ctx, kinds.networkId, nil, true, true, true)
			connect.AssertEqual(t, CanConnectSidecarClient(ctx, clientId), true)
			AddNetworkSidecarClient(ctx, kinds.networkId, clientId, time.Hour)
			connected = append(connected, clientId)
		}
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, kinds.networkId), int64(20))

		refusal := SidecarClientsExceeded(ctx, kinds.networkId)
		if refusal == nil {
			t.Fatal("the 21st sidecar was admitted at enrollment")
		}
		connect.AssertEqual(t, refusal.LimitKey, "sidecar_clients")
		connect.AssertEqual(t, refusal.Limit, int64(20))
		connect.AssertEqual(t, refusal.Used, int64(20))
		connect.AssertEqual(t, refusal.Upgrade, true)

		twentyFirst := testingSidecarCountClient(t, ctx, kinds.networkId, nil, true, true, false)
		connect.AssertEqual(t, CanConnectSidecarClient(ctx, twentyFirst), false)
		// connected clients stay: a re-nomination is no new connection
		connect.AssertEqual(t, CanConnectSidecarClient(ctx, connected[0]), true)
		// the app device carries no mark
		connect.AssertEqual(t, CanConnectSidecarClient(ctx, kinds.appDevice), true)

		// one leaves, and the next is admitted
		RemoveNetworkSidecarClient(ctx, kinds.networkId, connected[0])
		connect.AssertEqual(t, SidecarClientsExceeded(ctx, kinds.networkId) == nil, true)
		connect.AssertEqual(t, CanConnectSidecarClient(ctx, twentyFirst), true)

		// none: no limit
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"sidecar_clients": -1})()
		AddNetworkSidecarClient(ctx, kinds.networkId, twentyFirst, time.Hour)
		AddNetworkSidecarClient(ctx, kinds.networkId, connected[0], time.Hour)
		connect.AssertEqual(t, SidecarClientsExceeded(ctx, kinds.networkId) == nil, true)
	})
}

// An entry counts only until its expiry: a resident that stopped refreshing it drops out
// of sidecar_clients, and the next write ages it out.
func TestSidecarClientEntryExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		live := server.NewId()
		lapsed := server.NewId()
		AddNetworkSidecarClient(ctx, networkId, live, time.Hour)
		AddNetworkSidecarClient(ctx, networkId, lapsed, -time.Second)
		connect.AssertEqual(t, GetNetworkSidecarConnectedCount(ctx, networkId), int64(1))
		connect.AssertEqual(t, isNetworkSidecarClientConnected(ctx, networkId, lapsed), false)
		connect.AssertEqual(t, isNetworkSidecarClientConnected(ctx, networkId, live), true)

		AddNetworkSidecarClient(ctx, networkId, live, time.Hour)
		var remaining int64
		server.Redis(ctx, func(r server.RedisClient) {
			remaining = r.ZCard(ctx, networkSidecarConnectedKey(networkId)).Val()
		})
		connect.AssertEqual(t, remaining, int64(1))
	})
}

// §20.8's create cap per tier: pro.yml's top_level_client_limit, an Embed row over it,
// `none` as the largest allowance, and the constant 100 for an absent value.
func TestTopLevelClientCreateCapPerTier(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		free := server.NewId()
		Testing_CreateNetwork(ctx, free, fmt.Sprintf("capfree%s", free), server.NewId())
		pro := server.NewId()
		Testing_CreateNetwork(ctx, pro, fmt.Sprintf("cappro%s", pro), server.NewId())
		now := server.NowUtc()
		testingAddTierBalance(t, ctx, pro, true, now.Add(-time.Hour), now.Add(time.Hour))
		UpdateProNetwork(ctx, pro)

		limitOf := func(networkId server.Id) (limit int) {
			server.Tx(ctx, func(tx server.PgTx) {
				limit = networkTopLevelClientLimitInTx(ctx, tx, networkId)
			})
			return
		}

		// absent: every tier reads zero, and the cap is the constant
		restore := Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"top_level_client_limit": 0})
		restorePro := Testing_SetSidecarLimits(NetworkTierPro, map[string]int64{"top_level_client_limit": 0})
		restoreTeam := Testing_SetSidecarLimits(NetworkTierTeam, map[string]int64{"top_level_client_limit": 0})
		connect.AssertEqual(t, limitOf(free), LimitTopLevelClientIdsPerNetwork)
		connect.AssertEqual(t, limitOf(pro), LimitTopLevelClientIdsPerNetwork)
		restoreTeam()
		restorePro()
		restore()

		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"top_level_client_limit": 3})()
		defer Testing_SetSidecarLimits(NetworkTierPro, map[string]int64{"top_level_client_limit": 50})()
		defer Testing_SetSidecarLimits(NetworkTierTeam, map[string]int64{"top_level_client_limit": -1})()
		connect.AssertEqual(t, limitOf(free), 3)
		connect.AssertEqual(t, limitOf(pro), 50)
		connect.AssertEqual(t, tierTopLevelClientLimit(NetworkTierTeam), MaxNetworkTopLevelClientLimit)

		// an Embed row still wins
		connect.AssertEqual(t, SetNetworkTopLevelClientLimit(ctx, free, 250), nil)
		connect.AssertEqual(t, limitOf(free), 250)
	})
}
