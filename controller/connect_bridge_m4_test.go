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
// which takes their provide modes and keys and signs every contract between them. Safe for concurrent use.
type bridgeM4Platform struct {
	t testing.TB
	// the run's context, which carries the contract origin notifications every model call here needs
	ctx context.Context

	stateLock sync.Mutex
	networks  map[server.Id]bool
	// each client's subject, its own for a sidecar's client
	subjects map[server.Id]server.Id
}

var _ sidecartest.BridgeNetPlatform = (*bridgeM4Platform)(nil)

func newBridgeM4Platform(t testing.TB, ctx context.Context) *bridgeM4Platform {
	return &bridgeM4Platform{t: t, ctx: ctx, networks: map[server.Id]bool{}, subjects: map[server.Id]server.Id{}}
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
		// a sidecar's own client: admin_refused and strict_exp, as POST /network/enroll mints one (§7.4)
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				self.ctx,
				`
					INSERT INTO network_client_owner (client_id, member_id, tagged, admin_refused, strict_exp, acl_enforcing, declares_no_egress)
					VALUES ($1, NULL, false, true, true, false, true)
				`,
				clientId,
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
func (self *bridgeM4Platform) Control(ctx context.Context, clientId connect.Id, frames []*protocol.Frame) ([]*protocol.Frame, error) {
	return ConnectControlFrames(self.ctx, server.Id(clientId), frames, connect.DefaultContractManagerSettings())
}

// server holds publicKey as clientId's key, as the controller does a ClientKey a client publishes in band over its
// platform transport, which sidecartest's exchange does not carry to server.
func (self *bridgeM4Platform) RegisterClientKey(ctx context.Context, clientId connect.Id, publicKey []byte) error {
	model.SetClientPublicKey(self.ctx, server.Id(clientId), publicKey)
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
// NAT acquire no PostgreSQL connection, under server's process-wide tripwire.
func TestBridgeFlowPathReadsNoPostgresOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		net := sidecartest.NewNet(ctx, nil)
		defer net.Close()
		settings := sidecartest.DefaultBridgeNetSettings()
		settings.Platform = newBridgeM4Platform(t, ctx)
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
		buffer := make([]byte, 64)
		if _, err := udp.Read(buffer); err != nil {
			t.Fatal(err)
		}
		if err := window.SendEcho(host.Address, 1); err != nil || !window.WaitEchoReply(1, settings.WaitTimeout) {
			t.Fatalf("the first echo: %v", err)
		}

		attempts, stop := server.DenyPostgresForTest(t)
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
		echoed := window.SendEcho(host.Address, 2) == nil && window.WaitEchoReply(2, settings.WaitTimeout)
		delivered := host.Upstream.WaitFor(settings.WaitTimeout, func(accepts []sidecartest.BridgeNetAccept, datagrams [][]byte) bool {
			return 0 < len(accepts) && string(accepts[0].Bytes) == "firstsecond"
		})
		stop()
		if err != nil || string(buffer[:n]) != "second" || !echoed || !delivered {
			t.Fatalf("the second round: datagram %q %v, echo %t, tcp %t", buffer[:n], err, echoed, delivered)
		}
		if count := attempts(); count != 0 {
			t.Fatalf("the flow path acquired PostgreSQL %d times", count)
		}
	})
}
