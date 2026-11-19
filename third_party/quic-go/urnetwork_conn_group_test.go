package quic

// urnetwork: a Transport over a ConnGroup (URNETWORK.md, "One transport over a
// group of sockets"). A relay stands in for the kernel's choice of socket, so a
// flow moves between the group's sockets when a test says, whatever the
// platform's SO_REUSEPORT delivery; the group's sockets are bound to distinct
// ports so the relay can address each.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
)

// The bound on any one step of these tests; never waited out on success.
const connGroupTestTimeout = 10 * time.Second

const connGroupTestAlpn = "urnetwork-conn-group-test"

// A udp socket on loopback, closed with the test.
func connGroupTestSocket(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// A server tls config with a generated certificate, and a client config that
// trusts it.
func connGroupTestTls(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "conn-group.example"},
		DNSNames:     []string{"conn-group.example"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	serverConfig := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{connGroupTestAlpn},
	}
	clientConfig := &tls.Config{
		RootCAs:    roots,
		ServerName: "conn-group.example",
		NextProtos: []string{connGroupTestAlpn},
	}
	return serverConfig, clientConfig
}

// A relay standing in for the kernel's choice of socket, and for a NAT. The
// client sends to the relay's client side, and the relay forwards each
// datagram from its upstream socket to the group socket it is told; whatever
// reaches the upstream socket, from any socket of the group, goes back to the
// client. A rebinding replaces the upstream socket, as a NAT rebinding changes
// a client's source port, and the old mapping is forgotten.
type connGroupTestRelay struct {
	t          *testing.T
	clientSide *net.UDPConn
	clientAddr atomic.Pointer[net.UDPAddr]
	target     atomic.Pointer[net.UDPAddr]
	upstream   atomic.Pointer[net.UDPConn]
}

// A relay forwarding to target.
func newConnGroupTestRelay(t *testing.T, target net.Addr) *connGroupTestRelay {
	r := &connGroupTestRelay{
		t:          t,
		clientSide: connGroupTestSocket(t),
	}
	r.steer(target)
	r.rebind()
	go r.forwardFromClient()
	return r
}

// Forwards the client's next datagrams to target.
func (r *connGroupTestRelay) steer(target net.Addr) {
	r.target.Store(target.(*net.UDPAddr))
}

// Forwards the client's next datagrams from a new upstream socket, and closes
// the last one.
func (r *connGroupTestRelay) rebind() {
	upstream := connGroupTestSocket(r.t)
	if previous := r.upstream.Swap(upstream); previous != nil {
		previous.Close()
	}
	go r.forwardToClient(upstream)
}

func (r *connGroupTestRelay) forwardFromClient() {
	b := make([]byte, 2048)
	for {
		n, addr, err := r.clientSide.ReadFromUDP(b)
		if err != nil {
			return
		}
		r.clientAddr.Store(addr)
		r.upstream.Load().WriteToUDP(b[:n], r.target.Load())
	}
}

func (r *connGroupTestRelay) forwardToClient(upstream *net.UDPConn) {
	b := make([]byte, 2048)
	for {
		n, _, err := upstream.ReadFromUDP(b)
		if err != nil {
			return
		}
		if clientAddr := r.clientAddr.Load(); clientAddr != nil {
			r.clientSide.WriteToUDP(b[:n], clientAddr)
		}
	}
}

// A server Transport over a group of count sockets on distinct ports, echoing
// the first stream of each connection, and a client connection to it through
// a relay that starts on the first socket.
type connGroupTestEnv struct {
	t        *testing.T
	group    *ConnGroup
	members  []*ConnGroupMember
	relay    *connGroupTestRelay
	accepted chan *Conn
	conn     *Conn
	stream   *Stream
}

func newConnGroupTestEnv(t *testing.T, count int) *connGroupTestEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serverTls, clientTls := connGroupTestTls(t)
	env := &connGroupTestEnv{
		t:        t,
		accepted: make(chan *Conn, 4),
	}
	for i := range count {
		conn := connGroupTestSocket(t)
		if i == 0 {
			env.group = NewConnGroup(conn.LocalAddr())
			t.Cleanup(func() { env.group.Close() })
		}
		member, err := env.group.Add(conn)
		if err != nil {
			t.Fatal(err)
		}
		env.members = append(env.members, member)
	}
	server := &Transport{Conn: env.group}
	t.Cleanup(func() { server.Close() })
	listener, err := server.Listen(serverTls, &Config{MaxIdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			env.accepted <- conn
			go func() {
				stream, err := conn.AcceptStream(ctx)
				if err != nil {
					return
				}
				io.Copy(stream, stream)
			}()
		}
	}()

	env.relay = newConnGroupTestRelay(t, env.members[0].Conn().LocalAddr())
	client := &Transport{Conn: connGroupTestSocket(t)}
	t.Cleanup(func() { client.Close() })
	dialCtx, dialCancel := context.WithTimeout(ctx, connGroupTestTimeout)
	defer dialCancel()
	env.conn, err = client.Dial(dialCtx, env.relay.clientSide.LocalAddr(), clientTls, &Config{
		HandshakeIdleTimeout: connGroupTestTimeout,
		MaxIdleTimeout:       time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.conn.CloseWithError(0, "") })
	env.stream, err = env.conn.OpenStreamSync(dialCtx)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// Sends what on the client's stream and reads its echo, within the step bound.
func (env *connGroupTestEnv) echo(what string) {
	env.t.Helper()
	env.stream.SetDeadline(time.Now().Add(connGroupTestTimeout))
	if _, err := env.stream.Write([]byte(what)); err != nil {
		env.t.Fatalf("%s: %v", what, err)
	}
	echoed := make([]byte, len(what))
	if _, err := io.ReadFull(env.stream, echoed); err != nil {
		env.t.Fatalf("%s: %v", what, err)
	}
	if string(echoed) != what {
		env.t.Fatalf("%s: echoed %q", what, echoed)
	}
}

// Fails unless the server accepted exactly one connection.
func (env *connGroupTestEnv) expectOneConnection() {
	env.t.Helper()
	select {
	case <-env.accepted:
	default:
		env.t.Fatal("the server accepted no connection")
	}
	select {
	case <-env.accepted:
		env.t.Fatal("the server accepted a second connection")
	default:
	}
}

// Waits for a channel or fails the test.
func connGroupTestWait(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(connGroupTestTimeout):
		t.Fatalf("%s did not happen", what)
	}
}

// A connection keeps working when its flow moves to another socket of the
// group, as the kernel moves flows when a reuseport group's size changes, and
// when a NAT rebinding also changes its source port: every socket's packets
// reach the one Transport, which holds the connection.
func TestUrnetworkConnGroupKeepsAConnectionWhoseFlowMovesBetweenSockets(t *testing.T) {
	env := newConnGroupTestEnv(t, 3)
	env.echo("on the first socket")

	env.relay.steer(env.members[2].Conn().LocalAddr())
	env.echo("moved to the third socket")

	env.relay.rebind()
	env.relay.steer(env.members[1].Conn().LocalAddr())
	env.echo("rebound, on the second socket")

	env.expectOneConnection()
}

// A socket whose reads fail leaves the group on its own: its member reports
// the error, writes stop picking it, and the group's other sockets carry the
// connection whose flow it carried, as the kernel moves the flow once the
// socket is gone.
func TestUrnetworkConnGroupKeepsAConnectionThroughTheLossOfItsSocket(t *testing.T) {
	env := newConnGroupTestEnv(t, 3)
	env.echo("before the loss")

	lost := env.members[0]
	lost.Conn().Close()
	connGroupTestWait(t, lost.Done(), "the lost socket's end")
	if lost.Err() == nil {
		t.Fatal("the lost socket reports no error")
	}
	for _, member := range *env.group.writeMembers.Load() {
		if member == lost {
			t.Fatal("writes may still pick the lost socket")
		}
	}

	env.relay.steer(env.members[1].Conn().LocalAddr())
	env.echo("after the loss")
	env.expectOneConnection()
	for _, member := range env.members[1:] {
		select {
		case <-member.Done():
			t.Fatal("a sibling of the lost socket stopped")
		default:
		}
	}
}

// Reads one datagram at peer within the step bound, and returns its source.
func connGroupTestReadFrom(t *testing.T, peer *net.UDPConn, want []byte) *net.UDPAddr {
	t.Helper()
	peer.SetReadDeadline(time.Now().Add(connGroupTestTimeout))
	b := make([]byte, 2048)
	n, source, err := peer.ReadFromUDP(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b[:n], want) {
		t.Fatalf("read %q, want %q", b[:n], want)
	}
	return source
}

// The member whose socket is bound to addr.
func connGroupTestMemberAt(t *testing.T, members []*ConnGroupMember, addr *net.UDPAddr) *ConnGroupMember {
	t.Helper()
	for _, member := range members {
		if member.Conn().LocalAddr().String() == addr.String() {
			return member
		}
	}
	t.Fatalf("no socket of the group is bound to %s", addr)
	return nil
}

// Writes to one destination leave through one live socket. Once that socket
// is lost they leave through another, with no error; with no socket left the
// packet is dropped, still with no error, since a write error would end the
// connection that wrote; and once the group is closed, writes fail.
func TestUrnetworkConnGroupWritesThroughOneLiveSocketPerDestination(t *testing.T) {
	first := connGroupTestSocket(t)
	group := NewConnGroup(first.LocalAddr())
	defer group.Close()
	members := []*ConnGroupMember{}
	for _, conn := range []*net.UDPConn{first, connGroupTestSocket(t)} {
		member, err := group.Add(conn)
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, member)
	}
	peer := connGroupTestSocket(t)

	write := func(what string) {
		t.Helper()
		n, err := group.WritePacket([]byte(what), peer.LocalAddr(), nil, 0, protocol.ECNUnsupported)
		if err != nil || n != len(what) {
			t.Fatalf("writing %q: %d, %v", what, n, err)
		}
	}
	write("one")
	picked := connGroupTestReadFrom(t, peer, []byte("one"))
	write("two")
	if source := connGroupTestReadFrom(t, peer, []byte("two")); source.String() != picked.String() {
		t.Fatalf("the destination's writes left through %s, then %s", picked, source)
	}

	lost := connGroupTestMemberAt(t, members, picked)
	lost.Conn().Close()
	connGroupTestWait(t, lost.Done(), "the picked socket's end")
	write("three")
	if source := connGroupTestReadFrom(t, peer, []byte("three")); source.String() == picked.String() {
		t.Fatal("a write left through the lost socket")
	}

	for _, member := range members {
		member.Conn().Close()
		connGroupTestWait(t, member.Done(), "a socket's end")
	}
	write("dropped")

	group.Close()
	if _, err := group.WritePacket([]byte("closed"), peer.LocalAddr(), nil, 0, protocol.ECNUnsupported); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("a write to a closed group: %v", err)
	}
}

// One write a socket double saw.
type connGroupTestWrite struct {
	b             []byte
	packetInfoOOB []byte
	gsoSize       uint16
	ecn           protocol.ECN
}

// A socket double for a group: it records each write and answers it with
// writeErr, and its reads wait until it is closed.
type connGroupTestRawConn struct {
	localAddr        net.Addr
	capabilitiesOfIt connCapabilities
	writeErr         error
	closed           chan struct{}
	closeOnce        sync.Once

	mutex  sync.Mutex
	writes []connGroupTestWrite
}

var _ rawConn = &connGroupTestRawConn{}

func newConnGroupTestRawConn(capabilities connCapabilities, writeErr error) *connGroupTestRawConn {
	return &connGroupTestRawConn{
		localAddr:        &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4242},
		capabilitiesOfIt: capabilities,
		writeErr:         writeErr,
		closed:           make(chan struct{}),
	}
}

func (c *connGroupTestRawConn) ReadPacket() (receivedPacket, error) {
	<-c.closed
	return receivedPacket{}, net.ErrClosed
}

func (c *connGroupTestRawConn) WritePacket(b []byte, _ net.Addr, packetInfoOOB []byte, gsoSize uint16, ecn protocol.ECN) (int, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.writes = append(c.writes, connGroupTestWrite{
		b:             bytes.Clone(b),
		packetInfoOOB: bytes.Clone(packetInfoOOB),
		gsoSize:       gsoSize,
		ecn:           ecn,
	})
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return len(b), nil
}

func (c *connGroupTestRawConn) recordedWrites() []connGroupTestWrite {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return append([]connGroupTestWrite{}, c.writes...)
}

func (c *connGroupTestRawConn) ReadFrom([]byte) (int, net.Addr, error) {
	<-c.closed
	return 0, nil, net.ErrClosed
}

func (c *connGroupTestRawConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	return c.WritePacket(b, addr, nil, 0, protocol.ECNUnsupported)
}

func (c *connGroupTestRawConn) LocalAddr() net.Addr              { return c.localAddr }
func (c *connGroupTestRawConn) SetDeadline(time.Time) error      { return nil }
func (c *connGroupTestRawConn) SetReadDeadline(time.Time) error  { return nil }
func (c *connGroupTestRawConn) SetWriteDeadline(time.Time) error { return nil }
func (c *connGroupTestRawConn) capabilities() connCapabilities   { return c.capabilitiesOfIt }
func (c *connGroupTestRawConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

// A write passes to its socket as it was asked: the GSO segment size, the ECN
// marking and the packet info control message, which the socket's own path
// (quic-go's oob conn) turns into UDP_SEGMENT, IP_TOS and IP_PKTINFO. And the
// group reports its socket's capabilities, which a connection reads to decide
// whether to use them.
func TestUrnetworkConnGroupPassesGsoEcnAndPacketInfoToTheSocket(t *testing.T) {
	capabilities := connCapabilities{DF: true, GSO: true, ECN: true}
	socket := newConnGroupTestRawConn(capabilities, nil)
	group := NewConnGroup(socket.LocalAddr())
	defer group.Close()
	if got := group.capabilities(); got != (connCapabilities{}) {
		t.Fatalf("an empty group reports capabilities %+v", got)
	}
	if _, err := group.Add(socket); err != nil {
		t.Fatal(err)
	}
	if got := group.capabilities(); got != capabilities {
		t.Fatalf("the group reports capabilities %+v, its socket %+v", got, capabilities)
	}

	destination := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 7), Port: 443}
	packetInfoOOB := []byte{1, 2, 3, 4}
	b := bytes.Repeat([]byte{7}, 2400)
	if n, err := group.WritePacket(b, destination, packetInfoOOB, 1200, protocol.ECT1); err != nil || n != len(b) {
		t.Fatalf("the write: %d, %v", n, err)
	}
	writes := socket.recordedWrites()
	if len(writes) != 1 {
		t.Fatalf("the socket saw %d writes", len(writes))
	}
	write := writes[0]
	if !bytes.Equal(write.b, b) || !bytes.Equal(write.packetInfoOOB, packetInfoOOB) || write.gsoSize != 1200 || write.ecn != protocol.ECT1 {
		t.Fatalf("the socket saw %d bytes, oob %v, gso %d, ecn %v", len(write.b), write.packetInfoOOB, write.gsoSize, write.ecn)
	}
}

// A write that finds its socket closed under it, between the pick and the
// write, goes out through the next live socket, with no error.
func TestUrnetworkConnGroupSkipsASocketClosedUnderAWrite(t *testing.T) {
	live := connGroupTestSocket(t)
	group := NewConnGroup(live.LocalAddr())
	defer group.Close()
	if _, err := group.Add(live); err != nil {
		t.Fatal(err)
	}
	closedUnder := newConnGroupTestRawConn(group.capabilities(), net.ErrClosed)
	if _, err := group.Add(closedUnder); err != nil {
		t.Fatal(err)
	}
	writeMembers := *group.writeMembers.Load()
	if len(writeMembers) != 2 || writeMembers[1].raw != closedUnder {
		t.Fatal("the group's write set is not the two sockets in their order")
	}

	// a destination whose writes pick the closed socket first
	var peer *net.UDPConn
	for range 64 {
		candidate := connGroupTestSocket(t)
		if group.hashAddr(candidate.LocalAddr())%2 == 1 {
			peer = candidate
			break
		}
	}
	if peer == nil {
		t.Fatal("no destination picks the second socket")
	}
	if n, err := group.WritePacket([]byte("skip"), peer.LocalAddr(), nil, 0, protocol.ECNUnsupported); err != nil || n != len("skip") {
		t.Fatalf("the write: %d, %v", n, err)
	}
	if source := connGroupTestReadFrom(t, peer, []byte("skip")); source.String() != live.LocalAddr().String() {
		t.Fatalf("the write left through %s", source)
	}
	if writes := closedUnder.recordedWrites(); len(writes) != 1 {
		t.Fatalf("the closed socket saw %d writes, want the one it refused", len(writes))
	}
}

// A socket whose capabilities differ from the group's is refused, and stays
// its caller's: a connection relies on the capabilities it read when it
// started, and a GSO batch must never reach a socket without GSO.
func TestUrnetworkConnGroupRefusesASocketWithOtherCapabilities(t *testing.T) {
	first := newConnGroupTestRawConn(connCapabilities{DF: true, GSO: true, ECN: true}, nil)
	group := NewConnGroup(first.LocalAddr())
	defer group.Close()
	if _, err := group.Add(first); err != nil {
		t.Fatal(err)
	}
	other := newConnGroupTestRawConn(connCapabilities{DF: true, ECN: true}, nil)
	if _, err := group.Add(other); err == nil {
		t.Fatal("a socket without GSO joined a group with GSO")
	}
	select {
	case <-other.closed:
		t.Fatal("the group closed a socket it refused")
	default:
	}
	if writeMembers := *group.writeMembers.Load(); len(writeMembers) != 1 {
		t.Fatalf("%d sockets in the write set", len(writeMembers))
	}
}

// A Transport over the group stops its listen loop through the group's read
// deadline when it closes, as on a socket, and leaves the group's sockets
// alone: they are read on into the group, which a new reader still reads.
// Closing the group then closes its sockets, which end with no error, and its
// reads end with net.ErrClosed.
func TestUrnetworkConnGroupOutlivesTheTransportOverIt(t *testing.T) {
	socket := connGroupTestSocket(t)
	group := NewConnGroup(socket.LocalAddr())
	member, err := group.Add(socket)
	if err != nil {
		t.Fatal(err)
	}
	serverTls, _ := connGroupTestTls(t)
	server := &Transport{Conn: group}
	if _, err := server.Listen(serverTls, nil); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		server.Close()
	}()
	connGroupTestWait(t, closed, "the transport's close")
	select {
	case <-member.Done():
		t.Fatal("the transport's close ended the group's socket")
	default:
	}

	sender := connGroupTestSocket(t)
	if _, err := sender.WriteTo([]byte("after"), socket.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 64)
	n, source, err := group.ReadFrom(b)
	if err != nil || string(b[:n]) != "after" || source.String() != sender.LocalAddr().String() {
		t.Fatalf("a read after the transport's close: %q from %v, %v", b[:n], source, err)
	}

	group.Close()
	connGroupTestWait(t, member.Done(), "the socket's end")
	if err := member.Err(); err != nil {
		t.Fatalf("a socket the group closed ended with %v", err)
	}
	if _, err := group.ReadPacket(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("a read of a closed group: %v", err)
	}
	if _, err := group.Add(connGroupTestSocket(t)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("a socket joined a closed group: %v", err)
	}
}
