package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Full-stack integration tests for the Embed server features (EMBED1.md):
// ACL groups, per-client data caps and the Embed plan client allowance, each
// through a real exchange and connect handler. They need the test DB env
// (WARP_ENV=local + postgres/redis/vault); under the owner rule they run only
// after the branch, with its migrations, is merged to main. Skipped under -short.

func waitForEmbedCondition(t testing.TB, ctx context.Context, what string, check func() bool) {
	t.Helper()
	err := TestingWaitForConnectCondition(ctx, 60*time.Second, 200*time.Millisecond, func(ctx context.Context) (bool, string) {
		return check(), what
	})
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func embedClientSession(t testing.TB, ctx context.Context, byClientJwt string) *session.ClientSession {
	byJwt, err := jwt.ParseByJwt(ctx, byClientJwt)
	connect.AssertEqual(t, err, nil)
	return session.Testing_CreateClientSession(ctx, byJwt)
}

// Two default clients see each other. Isolating one removes it from the other's
// peer list with no disconnect marker, keeps it counted once it reconnects, and
// it receives no peer list: a client that joins afterward reaches the default
// client but never the isolated one. Returning it to default lists it again
// and it receives the list.
func TestExchangeAclGroupIsolatesAPeer(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := testing_newPeerDiscoveryEnv(ctx, t)
		defer env.Close()

		clientIdA, byClientJwtA := env.authClient(&model.AuthNetworkClientArgs{Description: "installation a"})
		clientIdB, byClientJwtB := env.authClient(&model.AuthNetworkClientArgs{Description: "installation b"})

		clientA := env.newClient(clientIdA)
		defer clientA.Close()
		clientB := env.newClient(clientIdB)
		defer clientB.Close()
		transportA := env.newTransport(byClientJwtA, server.NewId(), clientA.RouteManager())
		defer transportA.Close()
		transportB := env.newTransport(byClientJwtB, server.NewId(), clientB.RouteManager())
		defer transportB.Close()

		ok := waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return findPeer(connected, clientIdB) != nil
		})
		connect.AssertEqual(t, ok, true)
		ok = waitForPeers(ctx, clientB, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return findPeer(connected, clientIdA) != nil
		})
		connect.AssertEqual(t, ok, true)

		// isolate b with the network's root credential
		result, err := model.SetNetworkClientAclGroup(&model.SetNetworkClientAclGroupArgs{
			ClientId: clientIdB,
			AclGroup: model.NetworkClientAclGroupIsolated,
		}, env.userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*model.NetworkClientAclGroupError)(nil))

		// a stops seeing b, and the registry reports b neither connected nor
		// recently disconnected
		ok = waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return findPeer(connected, clientIdB) == nil
		})
		connect.AssertEqual(t, ok, true)
		_, registryPeers := model.GetNetworkPeers(ctx, env.networkId)
		for _, peer := range registryPeers {
			connect.AssertNotEqual(t, peer.ClientId, clientIdB)
		}

		// b's resident was retired; its transport re-nominates one that
		// registers b as isolated: counted, not listed
		waitForEmbedCondition(t, ctx, "b reconnected and counted as isolated", func() bool {
			_, _, category, _, _ := model.GetNetworkPeerProfile(ctx, clientIdB)
			return category == model.NetworkPeerCategoryIsolated &&
				model.GetResidentForClient(ctx, clientIdB, 0) != nil &&
				model.GetNetworkConnectedCount(ctx, env.networkId) == 2
		})
		peersB, err := model.GetNetworkPeersForSession(embedClientSession(t, ctx, byClientJwtB))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(peersB.Peers), 0)

		// a third, default client joins: a sees it, b never does
		clientIdC, byClientJwtC := env.authClient(&model.AuthNetworkClientArgs{Description: "installation c"})
		clientC := env.newClient(clientIdC)
		defer clientC.Close()
		transportC := env.newTransport(byClientJwtC, server.NewId(), clientC.RouteManager())
		defer transportC.Close()
		ok = waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return findPeer(connected, clientIdC) != nil
		})
		connect.AssertEqual(t, ok, true)
		// several listener poll intervals after a saw c
		select {
		case <-ctx.Done():
		case <-time.After(10 * env.exchange.settings.NetworkPeersPollInterval):
		}
		connectedB, _ := clientB.NetworkPeers()
		connect.AssertEqual(t, findPeer(connectedB, clientIdC), (*connect.NetworkPeer)(nil))

		// back to default: listed again, and b receives the list
		result, err = model.SetNetworkClientAclGroup(&model.SetNetworkClientAclGroupArgs{
			ClientId: clientIdB,
			AclGroup: model.NetworkClientAclGroupDefault,
		}, env.userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*model.NetworkClientAclGroupError)(nil))
		ok = waitForPeers(ctx, clientA, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return findPeer(connected, clientIdB) != nil
		})
		connect.AssertEqual(t, ok, true)
		ok = waitForPeers(ctx, clientB, func(connected []*connect.NetworkPeer, disconnectedCount int) bool {
			return findPeer(connected, clientIdA) != nil && findPeer(connected, clientIdC) != nil
		})
		connect.AssertEqual(t, ok, true)
	})
}

// An embedded installation's data cap acts on its real traffic to a public
// provider in another network: a cap of 0 pauses it before it ever gets a
// contract, clearing the cap lets the queued message through, settled usage
// is metered and read back through the GET after the rollup, a cap below that
// usage stops new contracts (a hard stop once the rollup has run), and raising
// the cap resumes traffic.
func TestExchangeDataCapPausesStopsAndResumesTraffic(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		model.Testing_ResetClientDataCapState()
		defer model.Testing_ResetClientDataCapState()

		env := testing_newPeerDiscoveryEnv(ctx, t)
		defer env.Close()

		// a public provider in its own network
		providerNetworkId := server.NewId()
		providerUserId := server.NewId()
		providerNetworkName := fmt.Sprintf("embed-provider-%s", providerNetworkId)
		model.Testing_CreateNetwork(ctx, providerNetworkId, providerNetworkName, providerUserId)
		providerSession := session.Testing_CreateClientSession(ctx, jwt.NewByJwt(providerNetworkId, providerUserId, providerNetworkName, false, false))
		providerResult, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "provider"}, providerSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, providerResult.Error, (*model.AuthNetworkClientError)(nil))
		providerClientId := *providerResult.ClientId
		providerClient := env.newClient(providerClientId)
		defer providerClient.Close()
		providerTransport := env.newTransport(*providerResult.ByClientJwt, server.NewId(), providerClient.RouteManager())
		defer providerTransport.Close()
		env.setProvideModes(providerClient, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Public: true,
		})

		// the embedded installation, paying from the env's funded network
		payerClientId, payerByClientJwt := env.authClient(&model.AuthNetworkClientArgs{Description: "user:alice:laptop"})
		receives := recordPeerReceives(providerClient, payerClientId)

		setCap := func(body string) *model.ClientDataCapResult {
			args := &model.SetClientDataCapArgs{}
			connect.AssertEqual(t, json.Unmarshal([]byte(`{"client_id":"`+payerClientId.String()+`",`+body+`}`), args), nil)
			result, err := model.SetClientDataCap(args, env.userSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error, (*model.ClientDataCapError)(nil))
			return result
		}
		send := func(payerClient *connect.Client) {
			frame, err := connect.ToFrame(&protocol.SimpleMessage{Content: "hello"}, connect.DefaultProtocolVersion)
			connect.AssertEqual(t, err, nil)
			sent := payerClient.SendWithTimeout(frame, connect.Id(providerClientId), func(err error) {}, 3*time.Minute)
			connect.AssertEqual(t, sent, true)
		}
		received := func(within time.Duration) bool {
			select {
			case <-receives:
				return true
			case <-time.After(within):
				return false
			}
		}
		// closes every open contract of the payer, settling each with the given
		// usage on both sides
		closeOpenContracts := func(usedByteCount model.ByteCount) {
			for pair, contractIdParties := range model.GetOpenContractIdsForSourceOrDestination(ctx, payerClientId) {
				for contractId := range contractIdParties {
					model.CloseContract(ctx, contractId, pair.A, usedByteCount, false)
					model.CloseContract(ctx, contractId, pair.B, usedByteCount, false)
				}
			}
		}

		// paused before any traffic: no contract, so nothing arrives
		paused := setCap(`"monthly_byte_limit":0`)
		connect.AssertEqual(t, paused.Capped, true)
		connect.AssertEqual(t, paused.CappedReason, "monthly")

		// a fresh payer client has no contracts, so admission alone decides
		newPayer := func() (*connect.Client, *connect.PlatformTransport) {
			payerClient := env.newClient(payerClientId)
			payerTransport := env.newTransport(payerByClientJwt, server.NewId(), payerClient.RouteManager())
			return payerClient, payerTransport
		}
		payerClient, payerTransport := newPayer()

		send(payerClient)
		connect.AssertEqual(t, received(10*time.Second), false)

		// clearing the cap lets the queued message through
		resumed := setCap(`"monthly_byte_limit":null`)
		connect.AssertEqual(t, resumed.Capped, false)
		connect.AssertEqual(t, received(90*time.Second), true)

		// settle the payer's contracts and roll the usage up: the payer reads
		// it with its own token
		const settledByteCount = model.ByteCount(1024)
		closeOpenContracts(settledByteCount)
		model.RollupClientDataUsage(ctx, server.NowUtc().Add(2*model.ClientDataUsageBlockDuration))
		usage, err := model.GetClientDataCap(&model.GetClientDataCapArgs{}, embedClientSession(t, ctx, payerByClientJwt))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, usage.Error, (*model.ClientDataCapError)(nil))
		if usage.MonthlyUsedByteCount < settledByteCount {
			t.Fatalf("monthly usage %d after settling %d", usage.MonthlyUsedByteCount, settledByteCount)
		}

		// a cap below the metered usage is a hard stop for new contracts. The
		// first payer client is retired with its contracts, so the next message
		// needs a new contract (an open one could carry it: the documented
		// in-flight overshoot)
		payerTransport.Close()
		payerClient.Close()
		closeOpenContracts(0)
		stopped := setCap(`"monthly_byte_limit":1`)
		connect.AssertEqual(t, stopped.Capped, true)
		connect.AssertEqual(t, stopped.CappedReason, "monthly")
		payerClient, payerTransport = newPayer()
		defer payerClient.Close()
		defer payerTransport.Close()
		send(payerClient)
		connect.AssertEqual(t, received(10*time.Second), false)

		// raising the cap resumes traffic
		raised := setCap(fmt.Sprintf(`"monthly_byte_limit":%d`, 1024*1024*1024))
		connect.AssertEqual(t, raised.Capped, false)
		connect.AssertEqual(t, received(90*time.Second), true)
	})
}

// With enforcement on and a one-client tier, an ordinary client past the tier
// is kicked at connection and creation answers upgrade_required. An Embed plan
// allowance of three lifts both gates, and the allowance is itself enforced
// once three clients are connected.
func TestConnectEmbedPlanAllowanceLiftsTheConcurrentLimit(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer model.Testing_SetEnforceConcurrentClients(true)()
		defer model.Testing_SetConcurrentClientsLimit(1, 1)()
		defer model.Testing_ClearNetworkClientLimitCache()
		testServer := newProviderIntentTestServer(t, ctx)
		userSession := session.Testing_CreateClientSession(ctx, jwt.NewByJwt(testServer.networkId, testServer.userId, testServer.networkName, false, false))

		// the tier's one slot is taken
		testServer.connectOrdinaryClient()

		clientId, byJwt := testServer.createClient()
		kicks := clientLimitKicksCounter.WithLabelValues(connectTransportH1, clientLimitKickCauseConcurrentClientLimit, "enforced")
		kicksBefore := testutil.ToFloat64(kicks)
		ws := testServer.dialH1(t, byJwt, false)
		testingReadClientLimitExceededControl(t, ws)
		testingRequireClientLimitExceededClose(t, ws)
		connect.AssertEqual(t, testutil.ToFloat64(kicks), kicksBefore+1)

		result, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "install"}, userSession)
		connect.AssertEqual(t, err, nil)
		if result.Error == nil || !result.Error.UpgradeRequired {
			t.Fatal("creation was not refused at the tier's concurrent limit")
		}

		// an Embed plan allowance of three
		connect.AssertEqual(t, model.SetNetworkTopLevelClientLimit(ctx, testServer.networkId, 3), nil)
		result, err = model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "install"}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*model.AuthNetworkClientError)(nil))

		ws = testServer.dialH1(t, byJwt, false)
		waitForEmbedCondition(t, ctx, "the client was admitted under the allowance", func() bool {
			return model.GetResidentForClient(ctx, clientId, 0) != nil
		})
		connect.AssertEqual(t, testutil.ToFloat64(kicks), kicksBefore+1)
		_ = ws

		// the allowance is enforced: three connected fills it
		testServer.connectOrdinaryClient()
		waitForEmbedCondition(t, ctx, "three clients connected", func() bool {
			return model.GetNetworkConnectedCount(ctx, testServer.networkId) == 3
		})
		_, otherByJwt := testServer.createClient()
		otherWs := testServer.dialH1(t, otherByJwt, false)
		testingReadClientLimitExceededControl(t, otherWs)
		testingRequireClientLimitExceededClose(t, otherWs)
		connect.AssertEqual(t, testutil.ToFloat64(kicks), kicksBefore+2)
	})
}
