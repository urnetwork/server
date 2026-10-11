package connect

import (
	"context"
	"errors"
	connectlib "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// One authoritative recheck of a lease, seen through Testing_InterposeRecheck.
type drivenLeaseRecheck struct {
	// started after the revocation committed
	afterRevocation bool
	// held past the lease's check deadline before reaching the store
	held bool
	err  error
}

// Drives the rechecks of one admitted transport's lease from the test rather
// than from the lease's timers. These tests dispatch no session event, so
// without a driven recheck a lease finds a revocation only at its 60 second
// scheduled recheck or, after a recheck the store did not answer, at its 5,
// 10, 20, 40 then 60 second backoff, and a loaded store can push that past
// any test budget.
//
// Rechecks before the revocation reach the store as usual. After it, the store
// answers a recheck only once the test permits it; any other recheck is held
// until the lease's check deadline, then sent to the real store, which refuses
// the expired request as unavailable. Without permits the lease's own schedule
// never finds the revocation, so the test's outcome does not depend on it.
type drivenLeaseRechecks struct {
	lease   *session.AuthorizationLease
	revoked atomic.Bool
	// at most one pending permit for the store to answer a recheck
	permits  chan struct{}
	rechecks chan drivenLeaseRecheck
}

// Interposes on every recheck of the lease. Call it from the admission hook,
// before the lease can run a recheck the test does not see.
func newDrivenLeaseRechecks(lease *session.AuthorizationLease) *drivenLeaseRechecks {
	self := &drivenLeaseRechecks{
		lease:    lease,
		permits:  make(chan struct{}, 1),
		rechecks: make(chan drivenLeaseRecheck, 64),
	}
	lease.Testing_InterposeRecheck(func(check func(context.Context, *session.ByJwt) error) func(context.Context, *session.ByJwt) error {
		return func(ctx context.Context, credential *session.ByJwt) error {
			recheck := drivenLeaseRecheck{afterRevocation: self.revoked.Load()}
			if recheck.afterRevocation {
				select {
				case <-self.permits:
				default:
					// the store does not answer within the lease's check deadline
					recheck.held = true
					<-ctx.Done()
				}
			}
			recheck.err = check(ctx, credential)
			select {
			case self.rechecks <- recheck:
			default:
				// a lease no test drives
			}
			return recheck.err
		}
	})
	return self
}

// Call once the revocation has committed. The first recheck after it is held
// unanswered, and the lease must keep the transport: it rechecks again only if
// it did not retire. Each later recheck is permitted to reach the store, and
// one the loaded store still does not answer is driven again at once. The
// first answered recheck must find the revocation, which retires the transport
// as revoked.
func (self *drivenLeaseRechecks) findRevocation(ctx context.Context, t testing.TB) {
	self.revoked.Store(true)
	heldCount := 0
	unansweredCount := 0
	for {
		if 0 < heldCount {
			select {
			case self.permits <- struct{}{}:
			default:
			}
		}
		self.lease.Recheck()
		var recheck drivenLeaseRecheck
		select {
		case recheck = <-self.rechecks:
		case <-self.lease.Testing_Done():
			// a lease reports the recheck that retires it before it ends
			select {
			case recheck = <-self.rechecks:
			default:
				t.Fatalf("lease ended without finding the revocation after %d held and %d unanswered rechecks", heldCount, unansweredCount)
			}
		case <-ctx.Done():
			t.Fatalf("lease stopped rechecking after %d held and %d unanswered rechecks", heldCount, unansweredCount)
		}
		switch {
		case errors.Is(recheck.err, session.ErrSessionRevoked):
			return
		case !recheck.afterRevocation:
			// a scheduled recheck that started before the revocation committed
		case errors.Is(recheck.err, session.ErrAuthUnavailable):
			if recheck.held {
				heldCount += 1
			} else {
				unansweredCount += 1
			}
		default:
			t.Fatal("answered recheck after the revocation did not find it", recheck.err)
		}
	}
}

// The lease of the connection a platform transport kept. The transport dials
// several candidates in parallel and keeps the first to connect, and the
// platform admits each candidate that reaches it, so one transport can hold
// several leases at once. Each discarded candidate's lease ends with its
// connection; waits for those to end and returns the one left.
func establishedLeaseRechecks(ctx context.Context, t testing.TB, rechecksList []*drivenLeaseRechecks) *drivenLeaseRechecks {
	for {
		liveRechecksList := []*drivenLeaseRechecks{}
		for _, rechecks := range rechecksList {
			select {
			case <-rechecks.lease.Testing_Done():
			default:
				liveRechecksList = append(liveRechecksList, rechecks)
			}
		}
		switch len(liveRechecksList) {
		case 0:
			t.Fatalf("all %d leases ended", len(rechecksList))
		case 1:
			return liveRechecksList[0]
		}
		cases := []reflect.SelectCase{{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())}}
		for _, rechecks := range liveRechecksList {
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(rechecks.lease.Testing_Done())})
		}
		if chosen, _, _ := reflect.Select(cases); chosen == 0 {
			t.Fatalf("%d of %d leases are still live", len(liveRechecksList), len(rechecksList))
		}
	}
}

// Actual platform relay, resident StreamOpen, loopback ICE/DTLS, and packet
// delivery run before revocation. All session notifications are disabled, so
// the test drives the rechecks of the lease of the connection the attacker's
// transport kept (establishedLeaseRechecks, drivenLeaseRechecks). The attacker
// retains its client/P2P lifecycle after its platform socket is closed.
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
			claims   *session.ByJwt
			rechecks *drivenLeaseRechecks
		}
		admissions := make(chan admitted, 16)
		env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, func(settings *ExchangeSettings) { settings.KeyEventDelivery.Enabled = false }, func(settings *ConnectHandlerSettings) {
			settings.testingAfterAuthorizationLease = func(claims *session.ByJwt, lease *session.AuthorizationLease) {
				select {
				case admissions <- admitted{claims: claims, rechecks: newDrivenLeaseRechecks(lease)}:
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
		attackerRechecksList := []*drivenLeaseRechecks{}
		collect := func(value admitted) {
			if value.claims.ClientId != nil && *value.claims.ClientId == attackerId {
				attackerRechecksList = append(attackerRechecksList, value.rechecks)
			}
		}
		for len(attackerRechecksList) == 0 {
			select {
			case value := <-admissions:
				collect(value)
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
		// The connection that carried the relay was admitted before it delivered.
		for drained := false; !drained; {
			select {
			case value := <-admissions:
				collect(value)
			default:
				drained = true
			}
		}
		attackerRechecks := establishedLeaseRechecks(ctx, t, attackerRechecksList)
		// Remote-worker authority uses its own request context and journal, sharing
		// only the stores with the serving exchange. No local event is dispatched.
		actor := session.Testing_CreateClientSession(context.Background(), honestLogin)
		defer actor.Cancel()
		result, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *attackerLogin.SessionId, OperationId: server.NewId()}, actor)
		if err != nil || result.Status != "revoked" {
			t.Fatal(result, err)
		}
		// No local event reaches this exchange, so the revocation is found by a
		// recheck the test drives, past one the store does not answer.
		attackerRechecks.findRevocation(ctx, t)
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
