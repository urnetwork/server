//go:build sidecar_pending

package controller

// BRIDGE-M4-22's rows on T3 (§22.4; FS-38, FS-56, S8, FI-11, I-12, D-BRIDGE-17, D-BRIDGE-21): sidecartest's BridgeNet,
// the same schedules and scenario sidecartest runs on T2, with every control frame its clients send, each contract they
// ask for among them, answered by server's controller on the private stack (ConnectControlFrames, as api's control
// route hands it). Each sidecar's own client and each window client is a network_client row of a network created under
// the bridge network's own id, a window a child whose stored acl_subject_client_id is its subject's, so C13's seals are
// the ones server writes from its rows (PROTO-M4-02) and the host's provide keys the ones its live client registered.
// The operator's minted contracts go around the controller, through the host's provide secret, which the test stack
// holds and nothing in server hands out.
//
// What is not server's here: the authorization log runs on sidecartest's FakeApi, not server's sequencer, which this
// package cannot import (server/api imports it); the exchange is sidecartest's in-process one, not server's, so the
// frames these clients send pass no resident, and a client's key reaches server through the platform, which stores it
// as the controller stores a ClientKey published in band. Run under -tags sidecar_pending through .stack/server-test.sh.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// server's platform for a BridgeNet: its clients as rows, and every control frame they send to server's controller,
// which takes their provide modes and keys and signs every contract between them. Control can be held at a barrier
// (holdControl). Safe for concurrent use.
type bridgeM4Platform struct {
	t testing.TB
	// the run's context, which carries the contract origin notifications every model call here needs
	ctx context.Context

	stateLock sync.Mutex
	networks  map[server.Id]bool
	// each client's subject, its own for a sidecar's client
	subjects map[server.Id]server.Id
	// open while control is held, and every control call that finds it waits for its close; nil while control runs
	controlHold chan struct{}
	// the control calls the latest hold kept waiting
	heldControlCount int

	// the control calls past the hold's check, which a new hold waits out
	runningControlCount *connect.MonitorValue[int]
}

var _ sidecartest.BridgeNetPlatform = (*bridgeM4Platform)(nil)

// A control call's context may carry a func() under this key, which Control calls each time a hold keeps the call
// waiting.
type bridgeM4ControlHeldKey struct{}

func newBridgeM4Platform(t testing.TB, ctx context.Context) *bridgeM4Platform {
	return &bridgeM4Platform{
		t:                   t,
		ctx:                 ctx,
		networks:            map[server.Id]bool{},
		subjects:            map[server.Id]server.Id{},
		runningControlCount: connect.NewMonitorValue(0),
	}
}

// A network under the bridge network's id the first time one is named, with a balance its contracts escrow from.
func (self *bridgeM4Platform) network(networkId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.networks[networkId] {
		return
	}
	self.networks[networkId] = true
	model.Testing_CreateNetwork(self.ctx, networkId, "bridge-m4-"+networkId.String(), server.NewId())
	server.Raise(model.AddBasicTransferBalance(self.ctx, networkId, 1024*1024*1024, server.NowUtc(), server.NowUtc().Add(time.Hour)))
}

// A sidecar's own client is strict_exp and bound to its sidecar_id; a window client is a child of its parent whose
// stored subject is the parent's; the operator's client is a bare client of the network, its own subject.
func (self *bridgeM4Platform) AddClient(ctx context.Context, args *sidecartest.BridgeNetClientArgs) (connect.Id, error) {
	networkId := server.Id(args.NetworkId)
	self.network(networkId)
	clientId := server.NewId()
	model.Testing_CreateDevice(self.ctx, networkId, server.NewId(), clientId, "synthetic", "synthetic")
	subject := clientId
	if args.SidecarId != (sidecar.Id{}) {
		// a sidecar's own client: admin_refused and strict_exp, as POST /network/enroll mints one (§7.4), and bound to
		// its sidecar at generation 1, as POST /sidecar/bind leaves it (§5.15), so the relationship's query names the
		// sidecar a contract gate decides for; the binding's key is the client's once RegisterClientKey holds it
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				self.ctx,
				`
					INSERT INTO network_client_owner (client_id, member_id, tagged, admin_refused, strict_exp, acl_enforcing, declares_no_egress)
					VALUES ($1, NULL, false, true, true, false, true)
				`,
				clientId,
			))
			server.RaisePgResult(tx.Exec(
				self.ctx,
				`
					INSERT INTO sidecar_identity_binding (sidecar_id, generation, client_id, client_public_key, signatures, create_time)
					VALUES ($1, 1, $2, $3, $4, $5)
				`,
				server.Id(args.SidecarId),
				clientId,
				make([]byte, 32),
				[]byte("synthetic binding signatures"),
				server.NowUtc(),
			))
		})
	}
	if args.ParentClientId != nil {
		parent := server.Id(*args.ParentClientId)
		self.stateLock.Lock()
		parentSubject, found := self.subjects[parent]
		self.stateLock.Unlock()
		if !found {
			return connect.Id{}, errors.New("a window client of an unknown parent")
		}
		subject = parentSubject
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				self.ctx,
				`UPDATE network_client SET source_client_id = $2, acl_subject_client_id = $3 WHERE client_id = $1`,
				clientId,
				parent,
				subject,
			))
		})
	}
	self.stateLock.Lock()
	self.subjects[clientId] = subject
	self.stateLock.Unlock()
	return connect.Id(clientId), nil
}

// Every control frame to server's controller, as api's control route hands it: contracts, provide modes and keys.
// While control is held, a call waits for the hold to end before the controller reads anything. A call counts itself
// running before it looks for a hold, so a hold set after the look waits for the call to end.
func (self *bridgeM4Platform) Control(ctx context.Context, clientId connect.Id, frames []*protocol.Frame) ([]*protocol.Frame, error) {
	for {
		self.runningControlCount.Update(func(count int) int { return count + 1 })
		hold := func() chan struct{} {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.controlHold != nil {
				self.heldControlCount += 1
			}
			return self.controlHold
		}()
		if hold == nil {
			break
		}
		self.runningControlCount.Update(func(count int) int { return count - 1 })
		if onHeld, ok := ctx.Value(bridgeM4ControlHeldKey{}).(func()); ok {
			onHeld()
		}
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	defer self.runningControlCount.Update(func(count int) int { return count - 1 })
	return ConnectControlFrames(self.ctx, server.Id(clientId), frames, connect.DefaultContractManagerSettings())
}

// Holds every control call from now on, once each call already past the hold's check has ended, or fails t past
// timeout. Then no control reads PostgreSQL until release, so a flow path tripwire armed between the two sees only the
// flow path: connect asks a send sequence's successor contract as soon as the sequence takes a contract, on its own
// time (ContractPrefetchLeadTime, eager for a same-network peer), and the controller's reads for it are control's, not
// the flow's (CODESTYLE: hold independent setup or control I/O at a barrier to prove established delivery still
// progresses). release answers how many calls the hold kept waiting; it is idempotent.
func (self *bridgeM4Platform) holdControl(timeout time.Duration) (release func() int) {
	self.t.Helper()
	hold := make(chan struct{})
	held := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.controlHold != nil {
			return true
		}
		self.controlHold = hold
		self.heldControlCount = 0
		return false
	}()
	if held {
		self.t.Fatal("control is held already")
	}
	var once sync.Once
	release = func() int {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		once.Do(func() {
			if self.controlHold == hold {
				self.controlHold = nil
			}
			close(hold)
		})
		return self.heldControlCount
	}
	deadline := time.After(timeout)
	for {
		count, changed := self.runningControlCount.Get()
		if count == 0 {
			return release
		}
		select {
		case <-changed:
		case <-deadline:
			release()
			self.t.Fatalf("%d control calls still running after %s", count, timeout)
		}
	}
}

// clientId asks a contract to destinationId through Control, as connect asks a sequence's contract: an error unless
// the controller answers with one frame.
func (self *bridgeM4Platform) askContract(ctx context.Context, clientId connect.Id, destinationId connect.Id, byteCount uint64) error {
	frame, err := connect.ToFrame(&protocol.CreateContract{
		DestinationId:     destinationId.Bytes(),
		TransferByteCount: byteCount,
	}, connect.DefaultProtocolVersion)
	if err != nil {
		return err
	}
	// Control borrows the frame; its answers are this caller's
	defer connect.MessagePoolReturn(frame.MessageBytes)
	answers, err := self.Control(ctx, clientId, []*protocol.Frame{frame})
	for _, answer := range answers {
		connect.MessagePoolReturn(answer.MessageBytes)
	}
	if err != nil {
		return err
	}
	if len(answers) != 1 {
		return fmt.Errorf("the controller answered %d frames", len(answers))
	}
	return nil
}

// server holds publicKey as clientId's key, as the controller does a ClientKey a client publishes in band over its
// platform transport, which sidecartest's exchange does not carry to server.
func (self *bridgeM4Platform) RegisterClientKey(ctx context.Context, clientId connect.Id, publicKey []byte) error {
	model.SetClientPublicKey(self.ctx, server.Id(clientId), publicKey)
	// a sidecar's binding names the key its client holds
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`UPDATE sidecar_identity_binding SET client_public_key = $2 WHERE client_id = $1`,
			server.Id(clientId),
			publicKey,
		))
	})
	heldKey, err := model.GetClientPublicKey(self.ctx, server.Id(clientId))
	if err != nil {
		return err
	}
	if !bytes.Equal(heldKey, publicKey) {
		return fmt.Errorf("server holds another key for client %s", clientId)
	}
	return nil
}

// Runs builder's schedule on a Net of its own whose BridgeNet takes server's platform, failing t with its trace.
func bridgeM4RunSchedule(t testing.TB, ctx context.Context, name string, builder fault.Builder) {
	t.Helper()
	net := sidecartest.NewNet(ctx, nil)
	defer net.Close()
	settings := sidecartest.DefaultBridgeNetSettings()
	settings.Platform = newBridgeM4Platform(t, ctx)
	sidecartest.SetNetBridgeSettings(net, settings)
	defer sidecartest.SetNetBridgeSettings(net, nil)
	if result := fault.Run(ctx, builder(net), net); result.Err != nil {
		t.Fatalf("%s on T3: %s", name, result)
	}
}

// FS-38's M4 part on server's CreateContract: the twin, whose delegation server copies into its contract unchanged,
// and each refusal of a delegation naming another key, lapsed, or naming another target or network.
func TestFs38OnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM4RunSchedule(t, ctx, "FS-38's twin", fault.Fs38ValidDelegation)
		bridgeM4RunSchedule(t, ctx, "FS-38", fault.Fs38Case.Build)
	})
}

// FS-38's forged case, beyond the catalog's T2: the operator's contracts minted through the host's provide secret,
// whose client is a row of the network, beside server's honest ones.
func TestFs38ForgedOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM4RunSchedule(t, ctx, "FS-38's forged", fault.Fs38ForgedCase.Build)
	})
}

// FS-56 on server's CreateContract: the held dial reset by a real SidecarRevoke, and its twin forwarding.
func TestFs56OnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM4RunSchedule(t, ctx, "FS-56", fault.Fs56Case.Build)
		bridgeM4RunSchedule(t, ctx, "FS-56's twin", fault.Fs56UnrevokedDialForwards)
	})
}

// S8 on T3: sidecartest's flow under the scenario harness, every contract its clients ask for server's.
func TestScenarioS8OnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		settings := sidecartest.DefaultBridgeNetSettings()
		settings.Platform = newBridgeM4Platform(t, ctx)
		sidecartest.RunScenario(t, sidecartest.BridgeScenarioS8(settings), nil)
	})
}

// I-12 around the flow path: once a window's contracts exist, its TCP, UDP and ICMP flows through the host's hook and
// NAT acquire no PostgreSQL connection, under server's process-wide tripwire, with control held at a barrier
// (bridgeM4FlowPathRounds).
func TestBridgeFlowPathReadsNoPostgresOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM4FlowPathRounds(t, ctx, sidecartest.DefaultBridgeNetSettings(), false)
	})
}

// The flow path's second round with its echo lost: an echo's path may lose it by design with nothing wrong on the bridge
// (the host's NAT drops a reply its session to a new window cannot seal yet, and darwin's kernel drops one at a full
// datagram ICMP socket; sidecartest's BridgeNetSettings.EchoResendInterval), and the round's wait, which sent the echo
// once, waited out its bound for an answer. Here the echo is lost outright, never sent: the wait that sends nothing sees
// no answer, and the round's wait sends it again and is answered, on the flow the first round's echo opened, its copies
// acquiring no PostgreSQL connection inside the tripwire.
func TestBridgeFlowPathLostEchoSentAgainOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		settings := sidecartest.DefaultBridgeNetSettings()
		// each lost echo costs the wait one interval
		settings.EchoResendInterval = 20 * time.Millisecond
		bridgeM4FlowPathRounds(t, ctx, settings, true)
	})
}

// A window's two rounds of TCP, UDP and ICMP to a host, on a BridgeNet of server's platform built from settings, failing
// t: the first makes the contracts, both ways, through server's controller, which reads Postgres; the second runs on the
// flows the first opened under server's process-wide tripwire and acquires no PostgreSQL connection.
//
// Control waits at the platform's barrier while the tripwire is armed (bridgeM4Platform.holdControl), and the second
// round must finish on the contracts the first made. Without it the tripwire counted the controller's reads for a
// successor contract connect asks on its own time once the first round's last reply takes a contract, whenever those
// reads came after it was armed. So a contract the host asks the window for inside the tripwire, as connect asks a
// successor, must wait at the barrier, and be answered once released.
//
// Each echo is sent again while its wait has no answer (sidecartest's BridgeNetWindow.WaitEchoAnswered), since its
// path may lose it by design, so the second round's copies ride the flow the first round's echo opened inside the
// tripwire. loseSecondEcho loses the second round's echo outright, never sending it, so only a copy its wait sends can
// be answered.
func bridgeM4FlowPathRounds(t testing.TB, ctx context.Context, settings *sidecartest.BridgeNetSettings, loseSecondEcho bool) {
	t.Helper()
	net := sidecartest.NewNet(ctx, nil)
	defer net.Close()
	platform := newBridgeM4Platform(t, ctx)
	settings.Platform = platform
	world := sidecartest.NewBridgeNet(t, net, settings)
	defer world.Close()
	hostDevice := world.AddDevice("host")
	subject := world.AddDevice("subject")
	host := world.StartHost(hostDevice, &sidecartest.BridgeNetHostSettings{Address: netip.MustParseAddr("100.80.7.20")})
	window := world.OpenWindow("window", subject, host, nil)
	tcpAddress := netip.AddrPortFrom(host.Address, host.Upstream.TcpPort())
	udpAddress := netip.AddrPortFrom(host.Address, host.Upstream.UdpPort())
	// the first round makes the contracts, both ways: control, which reads Postgres
	conn, err := window.DialTcp(tcpAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	udp, err := window.SendUdp(udpAddress, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	udp.SetReadDeadline(time.Now().Add(settings.WaitTimeout))
	buffer := make([]byte, settings.ReadByteCount)
	if _, err := udp.Read(buffer); err != nil {
		t.Fatal(err)
	}
	if err := window.SendEcho(host.Address, 1); err != nil || !window.WaitEchoAnswered(host.Address, 1, settings.WaitTimeout) {
		t.Fatalf("the first echo: %v, sent again %d times", err, window.EchoResendCount())
	}
	firstResendCount := window.EchoResendCount()

	// released after the tripwire stops, and before the world closes, which waits for every control call
	release := platform.holdControl(settings.WaitTimeout)
	defer release()
	attempts, stop := server.DenyPostgresForTest(t)
	// the host asks the window a contract inside the tripwire, as connect asks a sequence's successor on its own time:
	// the hold keeps it waiting, so it reads nothing until release, while the round runs on the first round's contracts
	askHeld := make(chan struct{})
	var askHeldOnce sync.Once
	askCtx := context.WithValue(ctx, bridgeM4ControlHeldKey{}, func() {
		askHeldOnce.Do(func() { close(askHeld) })
	})
	asked := make(chan error, 1)
	go func() {
		asked <- platform.askContract(askCtx, host.Client.ClientId, window.Client.ClientId, settings.ContractByteCount)
	}()
	select {
	case <-askHeld:
	case <-time.After(settings.WaitTimeout):
		stop()
		t.Fatal("the contract asked inside the tripwire never waited at the hold")
	}
	if _, err := conn.Write([]byte("second")); err != nil {
		stop()
		t.Fatal(err)
	}
	if _, err := udp.Write([]byte("second")); err != nil {
		stop()
		t.Fatal(err)
	}
	udp.SetReadDeadline(time.Now().Add(settings.WaitTimeout))
	n, err := udp.Read(buffer)
	// a lost echo has no copy, so the wait that sends nothing sees none answered in twice the time the round's wait
	// takes to send it again, as it would in any time
	if loseSecondEcho && window.WaitEchoReply(2, 2*settings.EchoResendInterval) {
		stop()
		t.Fatal("an echo never sent was answered: the test proves nothing")
	}
	echoed := (loseSecondEcho || window.SendEcho(host.Address, 2) == nil) && window.WaitEchoAnswered(host.Address, 2, settings.WaitTimeout)
	delivered := host.Upstream.WaitFor(settings.WaitTimeout, func(accepts []sidecartest.BridgeNetAccept, datagrams [][]byte) bool {
		return 0 < len(accepts) && string(accepts[0].Bytes) == "firstsecond"
	})
	stop()
	heldControlCount := release()
	select {
	case err := <-asked:
		if err != nil {
			t.Fatalf("the contract asked inside the tripwire, once released: %v", err)
		}
	case <-time.After(settings.WaitTimeout):
		t.Fatal("the contract asked inside the tripwire was never answered once released")
	}
	resendCount := window.EchoResendCount() - firstResendCount
	t.Logf("the second round: its echo sent again %d times, control held %d calls", resendCount, heldControlCount)
	if err != nil || string(buffer[:n]) != "second" || !echoed || !delivered {
		t.Fatalf("the second round: datagram %q %v, echo %t (sent again %d times), tcp %t, control held %d calls", buffer[:n], err, echoed, resendCount, delivered, heldControlCount)
	}
	if loseSecondEcho {
		if resendCount == 0 {
			t.Fatal("the lost echo was answered with no copy sent again: the test proves nothing")
		}
		echoDecisions := slices.DeleteFunc(host.Decisions(window.Client.ClientId), func(decision *sidecartest.BridgeNetDecision) bool {
			return decision.Protocol != connect.IpProtocolIcmp
		})
		if len(echoDecisions) != 1 || !echoDecisions[0].Opens() {
			t.Fatalf("the lost echo's copies did not ride the flow the first echo opened: the host decided %d echo flows of the window", len(echoDecisions))
		}
	}
	if count := attempts(); count != 0 {
		t.Fatalf("the flow path acquired PostgreSQL %d times, its second echo sent again %d times", count, resendCount)
	}
}
