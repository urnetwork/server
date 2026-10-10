package connect

import (
	"context"
	connectlib "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"testing"
	"time"
)

// Actual platform relay, resident StreamOpen, loopback ICE/DTLS, and packet
// delivery run before revocation. All session notifications are disabled. The
// attacker retains its client/P2P lifecycle after its platform socket is closed.
func TestSessionRevocationRetiresEstablishedRelayAndRogueP2p(t *testing.T) {
	testSessionRevocationDataPlane(t, connectlib.TransportModeH1)
}
func TestSessionRevocationRetiresEstablishedH3AndRogueP2p(t *testing.T) {
	testSessionRevocationDataPlane(t, connectlib.TransportModeH3)
}
func testSessionRevocationDataPlane(t *testing.T, mode connectlib.TransportMode) {
	if testing.Short() {
		t.Skip("requires local Postgres and Redis")
	}
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		restore := session.Testing_SetSessionCreationEnabled(true)
		defer restore()
		type admitted struct {
			claims *session.ByJwt
			lease  *session.AuthorizationLease
		}
		admissions := make(chan admitted, 16)
		env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, func(settings *ExchangeSettings) { settings.KeyEventDelivery.Enabled = false }, func(settings *ConnectHandlerSettings) {
			settings.testingAfterAuthorizationLease = func(claims *session.ByJwt, lease *session.AuthorizationLease) {
				select {
				case admissions <- admitted{claims, lease}:
				default:
				}
			}
		})
		defer env.Close()
		attackerLogin := session.NewByJwt(env.networkId, env.userId, "attacker", false, false)
		honestLogin := session.NewByJwt(env.networkId, env.userId, "honest", false, false)
		for _, claims := range []*session.ByJwt{attackerLogin, honestLogin} {
			if _, err := session.MintNetworkSession(ctx, claims, "password"); err != nil {
				t.Fatal(err)
			}
		}
		auth := func(claims *session.ByJwt) (server.Id, string) {
			actor := session.Testing_CreateClientSession(ctx, claims)
			defer actor.Cancel()
			result, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "lease integration", DeviceSpec: "test"}, actor)
			if err != nil || result.Error != nil {
				t.Fatal(result, err)
			}
			return *result.ClientId, *result.ByClientJwt
		}
		attackerId, attackerJwt := auth(attackerLogin)
		honestId, honestJwt := auth(honestLogin)
		attacker, honest := env.newClient(attackerId), env.newClient(honestId)
		defer attacker.Close()
		defer honest.Close()
		received := recordPeerReceives(honest, attackerId)
		attackerTransport := env.newTransportWithMode(attackerJwt, server.NewId(), attacker.RouteManager(), mode)
		defer attackerTransport.Close()
		honestTransport := env.newTransportWithMode(honestJwt, server.NewId(), honest.RouteManager(), mode)
		defer honestTransport.Close()
		var attackerLease *session.AuthorizationLease
		for attackerLease == nil {
			select {
			case value := <-admissions:
				if value.claims.ClientId != nil && *value.claims.ClientId == attackerId {
					attackerLease = value.lease
				}
			case <-ctx.Done():
				t.Fatal("platform admission did not complete")
			}
		}
		env.setProvideModes(attacker, map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
		env.setProvideModes(honest, map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
		sendSimpleMessage(t, attacker, honestId, connectlib.ForceStream())
		select {
		case <-received:
		case <-ctx.Done():
			t.Fatal("established relay did not deliver")
		}
		writer := attacker.RouteManager().OpenMultiRouteWriter(connectlib.DestinationId(connectlib.Id(honestId)))
		defer attacker.RouteManager().CloseMultiRouteWriter(writer)
		waitForActiveRoutes(ctx, t, writer, 2)
		attacker.ContractManager().AddNoContractPeer(connectlib.Id(honestId))
		honest.ContractManager().AddNoContractPeer(connectlib.Id(attackerId))
		// Remote-worker authority uses its own request context and journal, sharing
		// only the stores with the serving exchange. No local event is dispatched.
		actor := session.Testing_CreateClientSession(context.Background(), honestLogin)
		defer actor.Cancel()
		result, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *attackerLogin.SessionId, OperationId: server.NewId()}, actor)
		if err != nil || result.Status != "revoked" {
			t.Fatal(result, err)
		}
		attackerLease.Testing_Expire(time.Now().Add(91 * time.Second))
		waitForActiveRoutes(ctx, t, writer, 1)
		// A rogue endpoint ignores logout and keeps sending on the established P2P
		// connection. Delivery here proves socket closure alone was insufficient.
		frame := connectlib.RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "rogue-after-platform-close"})
		if !attacker.SendWithTimeout(frame, connectlib.Id(honestId), nil, time.Second, connectlib.NoAck()) {
			t.Fatal("rogue P2P send not admitted")
		}
		select {
		case <-received:
		case <-ctx.Done():
			t.Fatal("fixture did not retain established rogue P2P")
		}
		// Even if every retirement control is dropped, the honest endpoint's
		// independent timer cancels its entire stream/P2P lifecycle by the bound.
		honest.Testing_AdvanceStreamAuthorizationClock(91 * time.Second)
		waitForActiveRoutes(ctx, t, writer, 0)
		// A repeated authoritative registry reset cannot reinstall a retired grant;
		// endpoint generation binding and the receiver tombstone both fence it.
		_, hops := model.GetStreamHops(ctx, honestId)
		for hop := range hops {
			grant, err := model.AuthorizeStream(ctx, hop.StreamId(), server.NowUtc())
			if err != nil {
				t.Fatal(err)
			}
			if grant.Allowed {
				t.Fatal("retired endpoint generation was granted again")
			}
		}
	})
}
