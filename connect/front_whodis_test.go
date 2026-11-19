package connect

// The whodis fan-in (front_whodis.go): one net.PacketConn over a whodis
// group's sockets, under the group's one translation (D22 (b), LBALT-IMPL.md
// §8.4).

import (
	"bytes"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"
)

// A udp socket on loopback, closed with the test.
func newTestFrontWhodisSocket(t testing.TB) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// A fan-in with the default settings, closed with the test.
func newTestFrontWhodisFanIn(t testing.TB, localAddr net.Addr) *frontWhodisFanIn {
	settings := DefaultFrontServerSettings()
	settings.SocketRestartInitialDelay = time.Millisecond
	fanIn := newFrontWhodisFanIn(localAddr, settings)
	t.Cleanup(func() { fanIn.Close() })
	return fanIn
}

// Adds packetConn to fanIn, failing the test on an error.
func testFrontWhodisAdd(t testing.TB, fanIn *frontWhodisFanIn, packetConn net.PacketConn) *frontWhodisFanInMember {
	t.Helper()
	member, err := fanIn.add(packetConn)
	if err != nil {
		t.Fatal(err)
	}
	return member
}

// Reads one datagram at peer within the step bound, and returns its source.
func testFrontWhodisReadFrom(t testing.TB, peer *net.UDPConn, want []byte) *net.UDPAddr {
	t.Helper()
	peer.SetReadDeadline(time.Now().Add(testFrontProgressTimeout))
	b := make([]byte, frontWhodisMaxDatagramByteCount)
	n, source, err := peer.ReadFromUDP(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b[:n], want) {
		t.Fatalf("read %d bytes, want %d", n, len(want))
	}
	return source
}

// Reads one datagram from the fan-in within the step bound.
func testFrontWhodisFanInRead(t testing.TB, fanIn *frontWhodisFanIn, b []byte) (int, net.Addr) {
	t.Helper()
	type read struct {
		n    int
		addr net.Addr
		err  error
	}
	reads := make(chan read, 1)
	go func() {
		n, addr, err := fanIn.ReadFrom(b)
		reads <- read{n: n, addr: addr, err: err}
	}()
	select {
	case r := <-reads:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.n, r.addr
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("the fan-in read nothing")
		return 0, nil
	}
}

// The fan-in reads every socket of its group, and writes to one destination
// through one live socket: through another once that socket is lost, with no
// error; with no socket left the datagram is dropped, still with no error,
// as the translation drops a datagram it can't write; and once the fan-in is
// closed, its writes and reads fail.
func TestFrontWhodisFanInReadsEverySocketAndWritesThroughOnePerDestination(t *testing.T) {
	sockets := []*net.UDPConn{newTestFrontWhodisSocket(t), newTestFrontWhodisSocket(t)}
	fanIn := newTestFrontWhodisFanIn(t, sockets[0].LocalAddr())
	members := []*frontWhodisFanInMember{}
	for _, socket := range sockets {
		members = append(members, testFrontWhodisAdd(t, fanIn, socket))
	}

	sender := newTestFrontWhodisSocket(t)
	for _, socket := range sockets {
		what := "to " + socket.LocalAddr().String()
		if _, err := sender.WriteTo([]byte(what), socket.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 2048)
		n, addr := testFrontWhodisFanInRead(t, fanIn, b)
		if string(b[:n]) != what || addr.String() != sender.LocalAddr().String() {
			t.Fatalf("read %q from %s, want %q from %s", b[:n], addr, what, sender.LocalAddr())
		}
	}

	peer := newTestFrontWhodisSocket(t)
	write := func(what string) {
		t.Helper()
		if n, err := fanIn.WriteTo([]byte(what), peer.LocalAddr()); err != nil || n != len(what) {
			t.Fatalf("writing %q: %d, %v", what, n, err)
		}
	}
	write("one")
	picked := testFrontWhodisReadFrom(t, peer, []byte("one"))
	write("two")
	if source := testFrontWhodisReadFrom(t, peer, []byte("two")); source.String() != picked.String() {
		t.Fatalf("the destination's writes left through %s, then %s", picked, source)
	}

	lostIndex := slices.IndexFunc(sockets, func(socket *net.UDPConn) bool {
		return socket.LocalAddr().String() == picked.String()
	})
	sockets[lostIndex].Close()
	testFrontWait(t, members[lostIndex].Done(), "the picked socket's end")
	if members[lostIndex].Err() == nil {
		t.Fatal("the lost socket reports no error")
	}
	if slices.Contains(*fanIn.writeMembers.Load(), members[lostIndex]) {
		t.Fatal("writes may still pick the lost socket")
	}
	write("three")
	if source := testFrontWhodisReadFrom(t, peer, []byte("three")); source.String() == picked.String() {
		t.Fatal("a write left through the lost socket")
	}

	for i, socket := range sockets {
		socket.Close()
		testFrontWait(t, members[i].Done(), "a socket's end")
	}
	write("dropped")

	fanIn.Close()
	if _, err := fanIn.WriteTo([]byte("closed"), peer.LocalAddr()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("a write to a closed fan-in: %v", err)
	}
	if _, _, err := fanIn.ReadFrom(make([]byte, 64)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("a read of a closed fan-in: %v", err)
	}
	if _, err := fanIn.add(newTestFrontWhodisSocket(t)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("a socket joined a closed fan-in: %v", err)
	}
}

// The fan-in reads a datagram whole, larger than quic-go's 1452-byte packet
// buffer and than the translation's own buffer, so the translation reads what
// it read from its socket before: all of it, or what its buffer holds, cut as
// a socket's read cuts it.
func TestFrontWhodisFanInReadsAWholeDatagram(t *testing.T) {
	socket := newTestFrontWhodisSocket(t)
	fanIn := newTestFrontWhodisFanIn(t, socket.LocalAddr())
	testFrontWhodisAdd(t, fanIn, socket)
	sender := newTestFrontWhodisSocket(t)
	datagram := make([]byte, 3000)
	for i := range datagram {
		datagram[i] = byte(i)
	}
	for _, bufferByteCount := range []int{4 * 1024, 2 * 1024} {
		if _, err := sender.WriteTo(datagram, socket.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, bufferByteCount)
		n, _ := testFrontWhodisFanInRead(t, fanIn, b)
		want := datagram[:min(len(datagram), bufferByteCount)]
		if !bytes.Equal(b[:n], want) {
			t.Fatalf("a %d-byte read of a %d-byte datagram got %d bytes, want %d", bufferByteCount, len(datagram), n, len(want))
		}
	}
}

// A socket for the fan-in that records the buffer sizes it was given, and
// whose reads and writes a test can fail.
type testFrontWhodisPacketConn struct {
	net.PacketConn

	stateLock         sync.Mutex
	readBufferSizes   []int
	writeBufferSizes  []int
	readErrs          []error
	writeErr          error
	readErrsDelivered chan struct{}
}

// Wraps packetConn.
func newTestFrontWhodisPacketConn(packetConn net.PacketConn) *testFrontWhodisPacketConn {
	return &testFrontWhodisPacketConn{
		PacketConn:        packetConn,
		readErrsDelivered: make(chan struct{}, 16),
	}
}

func (self *testFrontWhodisPacketConn) SetReadBuffer(byteCount int) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.readBufferSizes = append(self.readBufferSizes, byteCount)
	return nil
}

func (self *testFrontWhodisPacketConn) SetWriteBuffer(byteCount int) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writeBufferSizes = append(self.writeBufferSizes, byteCount)
	return nil
}

// The read and write buffer sizes the socket was given.
func (self *testFrontWhodisPacketConn) bufferSizes() ([]int, []int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.readBufferSizes), slices.Clone(self.writeBufferSizes)
}

// Answers the next reads with errs, then reads again.
func (self *testFrontWhodisPacketConn) failReads(errs ...error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.readErrs = append(self.readErrs, errs...)
	}()
	// wakes a read waiting now
	self.PacketConn.SetReadDeadline(time.Now())
}

func (self *testFrontWhodisPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := self.PacketConn.ReadFrom(p)
		readErr := func() error {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if len(self.readErrs) == 0 {
				return nil
			}
			readErr := self.readErrs[0]
			self.readErrs = self.readErrs[1:]
			return readErr
		}()
		if readErr != nil {
			self.PacketConn.SetReadDeadline(time.Time{})
			self.readErrsDelivered <- struct{}{}
			return 0, nil, readErr
		}
		var netErr net.Error
		if err != nil && errors.As(err, &netErr) && netErr.Timeout() {
			// the wake of failReads, past
			continue
		}
		return n, addr, err
	}
}

func (self *testFrontWhodisPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	writeErr := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.writeErr
	}()
	if writeErr != nil {
		return 0, writeErr
	}
	return self.PacketConn.WriteTo(p, addr)
}

// The buffer sizes quic-go asks of the translation reach every socket of the
// fan-in: those in it then, and each that joins later, as they reached the
// translation's one socket before.
func TestFrontWhodisFanInSizesTheBuffersOfEverySocket(t *testing.T) {
	first := newTestFrontWhodisPacketConn(newTestFrontWhodisSocket(t))
	fanIn := newTestFrontWhodisFanIn(t, first.LocalAddr())
	testFrontWhodisAdd(t, fanIn, first)
	const readBufferByteCount = 7 * 1024 * 1024
	const writeBufferByteCount = 6 * 1024 * 1024
	if err := fanIn.SetReadBuffer(readBufferByteCount); err != nil {
		t.Fatal(err)
	}
	if err := fanIn.SetWriteBuffer(writeBufferByteCount); err != nil {
		t.Fatal(err)
	}
	later := newTestFrontWhodisPacketConn(newTestFrontWhodisSocket(t))
	testFrontWhodisAdd(t, fanIn, later)
	for _, socket := range []*testFrontWhodisPacketConn{first, later} {
		readSizes, writeSizes := socket.bufferSizes()
		if !slices.Equal(readSizes, []int{readBufferByteCount}) || !slices.Equal(writeSizes, []int{writeBufferByteCount}) {
			t.Fatalf("a socket was sized %v and %v, want %d and %d", readSizes, writeSizes, readBufferByteCount, writeBufferByteCount)
		}
	}
}

// A temporary read error, one net/http and quic-go wait out, is waited out
// with the socket kept in the fan-in; any other read error is the socket's
// loss, which its member reports.
func TestFrontWhodisFanInWaitsOutATemporaryReadError(t *testing.T) {
	socket := newTestFrontWhodisPacketConn(newTestFrontWhodisSocket(t))
	fanIn := newTestFrontWhodisFanIn(t, socket.LocalAddr())
	member := testFrontWhodisAdd(t, fanIn, socket)
	sender := newTestFrontWhodisSocket(t)

	socket.failReads(testFrontWhodisTemporaryErr{})
	testFrontWait(t, socket.readErrsDelivered, "the temporary read error")
	if _, err := sender.WriteTo([]byte("after"), socket.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 64)
	if n, _ := testFrontWhodisFanInRead(t, fanIn, b); string(b[:n]) != "after" {
		t.Fatalf("read %q after the temporary error", b[:n])
	}
	select {
	case <-member.Done():
		t.Fatal("a temporary read error ended the socket")
	default:
	}

	lossErr := errors.New("the test's socket failed")
	socket.failReads(lossErr)
	testFrontWait(t, member.Done(), "the socket's loss")
	if err := member.Err(); !errors.Is(err, lossErr) {
		t.Fatalf("the lost socket reports %v", err)
	}
}

// A read error net/http and quic-go read as temporary.
type testFrontWhodisTemporaryErr struct{}

func (testFrontWhodisTemporaryErr) Error() string   { return "the test's temporary read error" }
func (testFrontWhodisTemporaryErr) Timeout() bool   { return false }
func (testFrontWhodisTemporaryErr) Temporary() bool { return true }

// A write that finds its socket closed under it, between the pick and the
// write, goes out through the next live socket, with no error.
func TestFrontWhodisFanInSkipsASocketClosedUnderAWrite(t *testing.T) {
	live := newTestFrontWhodisSocket(t)
	fanIn := newTestFrontWhodisFanIn(t, live.LocalAddr())
	testFrontWhodisAdd(t, fanIn, live)
	closedUnder := newTestFrontWhodisPacketConn(newTestFrontWhodisSocket(t))
	closedUnder.writeErr = net.ErrClosed
	testFrontWhodisAdd(t, fanIn, closedUnder)

	// a destination whose writes pick the closed socket first
	var peer *net.UDPConn
	for range 64 {
		candidate := newTestFrontWhodisSocket(t)
		if fanIn.hashAddr(candidate.LocalAddr())%2 == 1 {
			peer = candidate
			break
		}
	}
	if peer == nil {
		t.Fatal("no destination picks the second socket")
	}
	if n, err := fanIn.WriteTo([]byte("skip"), peer.LocalAddr()); err != nil || n != len("skip") {
		t.Fatalf("the write: %d, %v", n, err)
	}
	if source := testFrontWhodisReadFrom(t, peer, []byte("skip")); source.String() != live.LocalAddr().String() {
		t.Fatalf("the write left through %s", source)
	}
}
