package connect

// One QUIC transport per udp group, from the inside (D22 (b), LBALT-IMPL.md
// §8): each socket is read as bound into its group's fan-in, a transport
// that ends is replaced, and one destroyed during the drain still gets each
// api connection one GOAWAY.

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	quic "github.com/urnetwork/quic-go"
	"github.com/urnetwork/quic-go/http3"
)

// The udp group of a port kind on address.
func (self *testFrontEnv) quicGroup(address netip.Addr, portKind string) *frontQuicGroup {
	self.t.Helper()
	for _, quicGroup := range self.frontServer.quicGroups {
		if quicGroup.key.address == address && quicGroup.key.portKind == portKind {
			return quicGroup
		}
	}
	self.t.Fatalf("no %s group on %s", portKind, address)
	return nil
}

// Each socket of a udp group is read as it was bound, with no reader between
// it and its group's fan-in: the quic port's are the members of the group's
// quic.ConnGroup, which quic-go reads through its oob path, and the whodis
// port's the members of the group's fan-in, under its one translation. Each
// group's transport reads its ConnGroup or its translation, which only the
// timing of its writes wraps.
func TestFrontQuicGroupSocketsAreReadAsBound(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	env.waitSocketsUp(len(env.frontServer.sockets))
	transportConns := env.connectQuic.builtTransportConns()
	for _, quicGroup := range env.frontServer.quicGroups {
		run := quicGroup.currentRun()
		var transportConn net.PacketConn = run.connGroup
		if quicGroup.key.portKind == frontPortWhodis {
			transportConn = run.translation
		}
		read := slices.ContainsFunc(transportConns, func(conn net.PacketConn) bool {
			if timedConn, ok := conn.(*frontWhodisTimedConn); ok {
				conn = timedConn.PacketConn
			}
			return conn == transportConn
		})
		if transportConn == nil || !read {
			t.Fatalf("the transport of %s reads neither its group's ConnGroup nor its translation", quicGroup)
		}
	}
	for _, socket := range env.frontServer.sockets {
		if socket.key.protocol != frontProtocolUdp {
			continue
		}
		member := func() frontQuicGroupMember {
			env.frontServer.stateLock.Lock()
			defer env.frontServer.stateLock.Unlock()
			return socket.run.(*frontQuicSocketRun).member
		}()
		var memberConn net.PacketConn
		switch member := member.(type) {
		case *quic.ConnGroupMember:
			if socket.key.portKind != frontPortQuic {
				t.Fatalf("the %s socket %s is a ConnGroup's", socket.key.portKind, socket.key)
			}
			memberConn = member.Conn()
		case *frontWhodisFanInMember:
			if socket.key.portKind != frontPortWhodis {
				t.Fatalf("the %s socket %s is the whodis fan-in's", socket.key.portKind, socket.key)
			}
			memberConn = member.packetConn
		default:
			t.Fatalf("the socket %s is a %T", socket.key, member)
		}
		if !slices.Contains(env.binds.boundPacketConns(socket.key.bindAddress()), memberConn) {
			t.Fatalf("the socket %s is read through a %T, not as it was bound", socket.key, memberConn)
		}
	}
}

// A group's transport that ends while the front serves is replaced, on the
// quic and the whodis ports, and the group serves again: its sockets, closed
// with the old transport's fan-in, are re-created into the new one, readiness
// is whole again, and a new connection is served. The quic transport ends
// when its ConnGroup is closed under it, the whodis transport when its
// translation ends, as a panic in one of the translation's workers ends it.
func TestFrontReplacesALostQuicTransport(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.SocketRestartInitialDelay = time.Millisecond
	})
	socketCount := len(env.frontServer.sockets)
	socketsPerPort := env.settings.SocketsPerPort
	env.waitSocketsUp(socketCount)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
		quicGroup := env.quicGroup(address, portKind)
		lost := quicGroup.currentRun()
		bindAddress := env.bindAddress(address, portKind)
		bound := len(env.binds.boundPacketConns(bindAddress))
		transportCount := len(env.connectQuic.builtTransportConns())

		if portKind == frontPortQuic {
			lost.connGroup.Close()
		} else {
			lost.translation.Close()
		}
		env.binds.waitPacketConns(t, bindAddress, bound+socketsPerPort)
		env.waitSocketsUp(socketCount)
		replacement := quicGroup.currentRun()
		if replacement == lost {
			t.Fatalf("%s: the lost transport was kept", portKind)
		}
		if count := len(env.connectQuic.builtTransportConns()); count != transportCount+1 {
			t.Fatalf("%s: %d transports built after the loss, want one more than %d", portKind, count, transportCount)
		}
		if entry := testFrontSocketsEntry(env.statusHeaders(), fmt.Sprintf("udp/%s/%d", address, quicGroup.key.port)); entry != fmt.Sprintf("udp/%s/%d=%d/%d", address, quicGroup.key.port, socketsPerPort, socketsPerPort) {
			t.Fatalf("%s: the group reads %q after the replacement", portKind, entry)
		}
		client, _ := env.apiClient(address, portKind, nil)
		testFrontGetApi(t, client, testFrontInterfaceA, "/after-the-replacement")
	}
	if err := env.frontServer.ctx.Err(); err != nil {
		t.Fatalf("the front ended: %v", err)
	}
}

// A group's transport destroyed while the drain shuts its api connections
// down gracefully, as one whose fan-in is closed under it, destroys every
// connection on it at once, and each gets no second GOAWAY. Each h3 server
// finishes serving its connection while quic-go destroys it, with the close
// window held open and the transport's supervisor held before it closes the
// transport, so only the destroyed connection can end the serve: nothing
// closes the h3 servers before the release.
func TestFrontDrainSendsOneH3GoawayWhenItsTransportIsLost(t *testing.T) {
	const connectionCount = 4
	trace := newTestFrontConnectionTrace(true)
	defer trace.Release()
	lostTransports := make(chan frontSocketKey, 16)
	releaseLost := make(chan struct{})
	var releaseLostOnce sync.Once
	release := func() {
		releaseLostOnce.Do(func() {
			close(releaseLost)
		})
	}
	defer release()
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.connectQuic.tracer = trace.tracer
		env.settings.quicTransportServeEndObserverForTest = func(key frontSocketKey) {
			if key.portKind == frontPortQuic && key.interfaceName == testFrontInterfaceA && key.family == frontFamilyIpv4 {
				lostTransports <- key
				<-releaseLost
			}
		}
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	for range connectionCount {
		conn, err := env.dialQuic(address, frontPortQuic, testFrontApiName, []string{http3.NextProtoH3})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseWithError(0, "")
		env.nextDispatch()
	}
	env.waitOpen(connectionCount)
	env.frontServer.beginDrain()
	trace.waitGoaways(t, connectionCount)

	env.quicGroup(address, frontPortQuic).currentRun().connGroup.Close()
	select {
	case <-lostTransports:
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("the lost transport's serve did not end")
	}
	trace.waitHeld(t, connectionCount)
	trace.waitServedWithoutAnotherGoaway(t, env.frontServer, connectionCount)

	release()
	trace.Release()
	env.frontServer.waitDrained()
	if goaways := trace.goaways.Value(); goaways != connectionCount {
		t.Fatalf("%d GOAWAYs for %d api connections a transport's loss destroyed during the drain", goaways, connectionCount)
	}
}
