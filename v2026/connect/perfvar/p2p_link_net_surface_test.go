// This file verifies every UDP write and construction surface through which
// Pion can reach PERFVAR's directional outer-link scheduler.
package perfvar

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/pion/transport/v4"
)

// One delegate observation owns copies of every mutable input it received.
type p2pSurfaceWrite struct {
	method  string
	payload []byte
	oob     []byte
	address *net.UDPAddr
}

// A configurable UDP delegate records exact delayed arguments and can return
// short writes or errors without changing scheduler admission semantics.
type p2pSurfaceUDPConn struct {
	transport.UDPConn
	writes            chan p2pSurfaceWrite
	localAddress      *net.UDPAddr
	remoteAddress     *net.UDPAddr
	payloadByteOffset int
	oobByteOffset     int
	writeErr          error
	readByteCount     int
	readErr           error
	lastReadMethod    string
	readPayload       []byte
}

// The configured local tuple supports destination-scoped wrapper tests.
func (self *p2pSurfaceUDPConn) LocalAddr() net.Addr {
	return cloneP2pUDPAddress(self.localAddress)
}

// The configured remote tuple supports connected wrapper tests.
func (self *p2pSurfaceUDPConn) RemoteAddr() net.Addr {
	return cloneP2pUDPAddress(self.remoteAddress)
}

// Connected reads expose the wrapper's standard net.Conn receive surface.
func (self *p2pSurfaceUDPConn) Read(payload []byte) (int, error) {
	self.lastReadMethod = "Read"
	return self.read(payload), self.readErr
}

// Generic packet reads retain their source address.
func (self *p2pSurfaceUDPConn) ReadFrom(payload []byte) (int, net.Addr, error) {
	self.lastReadMethod = "ReadFrom"
	return self.read(payload), self.readAddress(), self.readErr
}

// Concrete UDP reads retain their source address.
func (self *p2pSurfaceUDPConn) ReadFromUDP(payload []byte) (int, *net.UDPAddr, error) {
	self.lastReadMethod = "ReadFromUDP"
	return self.read(payload), self.readAddress(), self.readErr
}

// Message reads expose one consumed payload without ancillary data.
func (self *p2pSurfaceUDPConn) ReadMsgUDP(
	payload []byte,
	oob []byte,
) (int, int, int, *net.UDPAddr, error) {
	_ = oob
	self.lastReadMethod = "ReadMsgUDP"
	return self.read(payload), 0, 0, self.readAddress(), self.readErr
}

// ICE value-address reads expose the optional concrete implementation.
func (self *p2pSurfaceUDPConn) ReadFromAddrPort(
	payload []byte,
) (int, netip.AddrPort, error) {
	self.lastReadMethod = "ReadFromAddrPort"
	return self.read(payload), self.readAddress().AddrPort(), self.readErr
}

// Concrete UDP value-address reads remain independently observable.
func (self *p2pSurfaceUDPConn) ReadFromUDPAddrPort(
	payload []byte,
) (int, netip.AddrPort, error) {
	self.lastReadMethod = "ReadFromUDPAddrPort"
	return self.read(payload), self.readAddress().AddrPort(), self.readErr
}

// The configured byte count models one delegate read result.
func (self *p2pSurfaceUDPConn) read(payload []byte) int {
	if self.readPayload != nil {
		return copy(payload, self.readPayload)
	}
	for byteIndex := 0; byteIndex < self.readByteCount && byteIndex < len(payload); byteIndex += 1 {
		payload[byteIndex] = byte(byteIndex + 1)
	}
	return self.readByteCount
}

// Every focused read uses one stable source address.
func (self *p2pSurfaceUDPConn) readAddress() *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(10, 240, 0, 8), Port: 8765}
}

// A legacy delegate intentionally omits both value-address read extensions so
// the wrapper's compatibility adapters can be checked independently.
type p2pLegacyReadUDPConn struct {
	transport.UDPConn
	readCount int
}

// Both compatibility adapters ultimately use this one concrete UDP read.
func (self *p2pLegacyReadUDPConn) ReadFromUDP(payload []byte) (int, *net.UDPAddr, error) {
	self.readCount += 1
	if 0 < len(payload) {
		payload[0] = 1
	}
	return 1, &net.UDPAddr{IP: net.IPv4(10, 240, 0, 8), Port: 8765}, nil
}

// Connected writes expose the common delayed-delivery disposition.
func (self *p2pSurfaceUDPConn) Write(payload []byte) (int, error) {
	self.record("Write", payload, nil, nil)
	return len(payload) + self.payloadByteOffset, self.writeErr
}

// Generic-address writes retain the concrete UDP destination they receive.
func (self *p2pSurfaceUDPConn) WriteTo(payload []byte, address net.Addr) (int, error) {
	udpAddress, _ := address.(*net.UDPAddr)
	self.record("WriteTo", payload, nil, udpAddress)
	return len(payload) + self.payloadByteOffset, self.writeErr
}

// UDP-address writes retain the exact delayed destination.
func (self *p2pSurfaceUDPConn) WriteToUDP(payload []byte, address *net.UDPAddr) (int, error) {
	self.record("WriteToUDP", payload, nil, address)
	return len(payload) + self.payloadByteOffset, self.writeErr
}

// Message writes expose both payload and ancillary-data ownership.
func (self *p2pSurfaceUDPConn) WriteMsgUDP(
	payload []byte,
	oob []byte,
	address *net.UDPAddr,
) (int, int, error) {
	self.record("WriteMsgUDP", payload, oob, address)
	return len(payload) + self.payloadByteOffset, len(oob) + self.oobByteOffset, self.writeErr
}

// ICE value-address writes expose the optional allocation-free surface.
func (self *p2pSurfaceUDPConn) WriteToAddrPort(
	payload []byte,
	address netip.AddrPort,
) (int, error) {
	self.record("WriteToAddrPort", payload, nil, net.UDPAddrFromAddrPort(address))
	return len(payload) + self.payloadByteOffset, self.writeErr
}

// Concrete UDP value-address writes remain distinguishable from ICE's method.
func (self *p2pSurfaceUDPConn) WriteToUDPAddrPort(
	payload []byte,
	address netip.AddrPort,
) (int, error) {
	self.record("WriteToUDPAddrPort", payload, nil, net.UDPAddrFromAddrPort(address))
	return len(payload) + self.payloadByteOffset, self.writeErr
}

// Recording copies the delegate inputs so assertions do not depend on later
// mutations by either the scheduler or the test.
func (self *p2pSurfaceUDPConn) record(
	method string,
	payload []byte,
	oob []byte,
	address *net.UDPAddr,
) {
	self.writes <- p2pSurfaceWrite{
		method:  method,
		payload: append([]byte(nil), payload...),
		oob:     append([]byte(nil), oob...),
		address: cloneP2pUDPAddress(address),
	}
}

// An explicit scheduler barrier holds the delegate call after admission and
// lets a test mutate caller-owned inputs without a timing race.
type p2pSurfaceScheduleBarrier struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

// The installed hook is nil outside the focused test link.
func newP2pSurfaceScheduleBarrier(link *directionalLink) *p2pSurfaceScheduleBarrier {
	barrier := &p2pSurfaceScheduleBarrier{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	link.setAfterPacketScheduledForTest(func(linkScheduleObservation) {
		barrier.enteredOnce.Do(func() { close(barrier.entered) })
		<-barrier.release
	})
	return barrier
}

// A context is only a liveness bound for the positive scheduler edge.
func (self *p2pSurfaceScheduleBarrier) wait(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for P2P surface scheduler: %v", ctx.Err())
	case <-self.entered:
	}
}

// Release is idempotent so cleanup can unblock a failed assertion safely.
func (self *p2pSurfaceScheduleBarrier) open() {
	self.releaseOnce.Do(func() { close(self.release) })
}

// One normalized result lets homogeneous disposition tests cover every write
// surface without hiding their different public signatures.
type p2pSurfaceWriteResult struct {
	payloadByteCount int
	oobByteCount     int
	err              error
}

// The selected wrapper method receives identical mutable test inputs.
func invokeP2pSurfaceWrite(
	connection *p2pLinkUDPConn,
	method string,
	payload []byte,
	oob []byte,
	address *net.UDPAddr,
) p2pSurfaceWriteResult {
	result := p2pSurfaceWriteResult{}
	switch method {
	case "Write":
		result.payloadByteCount, result.err = connection.Write(payload)
	case "WriteTo":
		result.payloadByteCount, result.err = connection.WriteTo(payload, address)
	case "WriteToUDP":
		result.payloadByteCount, result.err = connection.WriteToUDP(payload, address)
	case "WriteMsgUDP":
		result.payloadByteCount, result.oobByteCount, result.err = connection.WriteMsgUDP(
			payload,
			oob,
			address,
		)
	case "WriteToAddrPort":
		result.payloadByteCount, result.err = connection.WriteToAddrPort(payload, address.AddrPort())
	case "WriteToUDPAddrPort":
		result.payloadByteCount, result.err = connection.WriteToUDPAddrPort(payload, address.AddrPort())
	default:
		result.err = errors.New("unknown P2P surface method")
	}
	return result
}

// One normalized call covers every transport.UDPConn receive surface without
// hiding the wrapper method that the delegate observed.
func invokeP2pSurfaceRead(connection *p2pLinkUDPConn, method string) (int, error) {
	payload := make([]byte, 16)
	switch method {
	case "Read":
		return connection.Read(payload)
	case "ReadFrom":
		readByteCount, _, err := connection.ReadFrom(payload)
		return readByteCount, err
	case "ReadFromUDP":
		readByteCount, _, err := connection.ReadFromUDP(payload)
		return readByteCount, err
	case "ReadMsgUDP":
		readByteCount, _, _, _, err := connection.ReadMsgUDP(payload, make([]byte, 16))
		return readByteCount, err
	case "ReadFromAddrPort":
		readByteCount, _, err := connection.ReadFromAddrPort(payload)
		return readByteCount, err
	case "ReadFromUDPAddrPort":
		readByteCount, _, err := connection.ReadFromUDPAddrPort(payload)
		return readByteCount, err
	default:
		return 0, errors.New("unknown P2P read surface method")
	}
}

// Every public UDP read method releases one and only one receive reservation.
func TestP2pLinkUDPConnReadSurfacesReleaseExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	methods := []string{
		"Read",
		"ReadFrom",
		"ReadFromUDP",
		"ReadMsgUDP",
		"ReadFromAddrPort",
		"ReadFromUDPAddrPort",
	}
	for _, method := range methods {
		credits := newP2pReceiveCredits(1)
		if !credits.acquire(ctx) {
			t.Fatalf("%s receive credit was not admitted", method)
		}
		delegate := &p2pSurfaceUDPConn{readByteCount: 1}
		connection := &p2pLinkUDPConn{UDPConn: delegate, inboundCredits: credits}
		readByteCount, err := invokeP2pSurfaceRead(connection, method)
		if err != nil || readByteCount != 1 {
			t.Fatalf("%s read bytes=%d err=%v", method, readByteCount, err)
		}
		if delegate.lastReadMethod != method {
			t.Fatalf("%s invoked delegate %s", method, delegate.lastReadMethod)
		}
		snapshot := credits.snapshot()
		if snapshot.AdmittedPacketCount != 1 || snapshot.ReadPacketCount != 1 ||
			snapshot.OutstandingPacketCount != 0 ||
			snapshot.InvalidReleasePacketCount != 0 || !snapshot.isExactLiveTerminal() {
			t.Fatalf("%s receive-credit snapshot=%+v", method, snapshot)
		}
		credits.close()
	}
}

// Every UDP read surface removes the private vnet generation header while
// retiring exactly the reservation that supplied that frame.
func TestP2pLinkUDPConnReadSurfacesStripGenerationFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	methods := []string{
		"Read",
		"ReadFrom",
		"ReadFromUDP",
		"ReadMsgUDP",
		"ReadFromAddrPort",
		"ReadFromUDPAddrPort",
	}
	expectedPayload := []byte("generation-framed-read")
	for methodIndex, method := range methods {
		credits := newP2pVnetReceiveCredits(1)
		localAddress := &net.UDPAddr{
			IP:   net.IPv4(10, 240, 0, 2),
			Port: 5600 + methodIndex,
		}
		remoteAddress := &net.UDPAddr{
			IP:   net.IPv4(10, 240, 0, 1),
			Port: 5700 + methodIndex,
		}
		socket := credits.registerSocket(localAddress, remoteAddress, nil)
		reservation, hasDestination, admitted := credits.reserveForTransfer(
			ctx,
			remoteAddress,
			localAddress,
		)
		if !hasDestination || !admitted || reservation == nil {
			credits.close()
			t.Fatalf("%s generation-frame reservation was not admitted", method)
		}
		outerPacket := make([]byte, p2pIPv4UDPHeaderByteCount+len(expectedPayload))
		copy(outerPacket[p2pIPv4UDPHeaderByteCount:], expectedPayload)
		framedPayload, framed := reservation.framePayload(outerPacket)
		if !framed {
			credits.close()
			t.Fatalf("%s generation frame was not created", method)
		}
		credits.completeRouterPayload(socket.generation)
		delegate := &p2pSurfaceUDPConn{
			localAddress:  localAddress,
			remoteAddress: remoteAddress,
			readPayload:   append([]byte(nil), framedPayload...),
		}
		connection := &p2pLinkUDPConn{
			UDPConn:        delegate,
			inboundCredits: credits,
			receiveSocket:  socket,
		}
		payload := make([]byte, len(expectedPayload))
		var readByteCount int
		var err error
		switch method {
		case "Read":
			readByteCount, err = connection.Read(payload)
		case "ReadFrom":
			readByteCount, _, err = connection.ReadFrom(payload)
		case "ReadFromUDP":
			readByteCount, _, err = connection.ReadFromUDP(payload)
		case "ReadMsgUDP":
			readByteCount, _, _, _, err = connection.ReadMsgUDP(payload, make([]byte, 16))
		case "ReadFromAddrPort":
			readByteCount, _, err = connection.ReadFromAddrPort(payload)
		case "ReadFromUDPAddrPort":
			readByteCount, _, err = connection.ReadFromUDPAddrPort(payload)
		default:
			err = errors.New("unknown generation-framed read surface")
		}
		if err != nil || readByteCount != len(expectedPayload) ||
			string(payload) != string(expectedPayload) {
			credits.close()
			t.Fatalf(
				"%s generation-framed read bytes=%d payload=%q err=%v",
				method,
				readByteCount,
				payload,
				err,
			)
		}
		snapshot := credits.snapshot()
		if snapshot.AdmittedPacketCount != 1 || snapshot.ReadPacketCount != 1 ||
			snapshot.OutstandingPacketCount != 0 || snapshot.TrackedReservationCount != 0 ||
			snapshot.StaleGenerationDropCount != 0 || !snapshot.isExactLiveTerminal() {
			credits.close()
			t.Fatalf("%s generation-framed credits=%+v", method, snapshot)
		}
		socket.close()
		credits.close()
	}
}

// Both value-address fallbacks release one reservation without recursively
// applying receive accounting a second time.
func TestP2pLinkUDPConnValueReadFallbacksReleaseExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	methods := []string{"ReadFromAddrPort", "ReadFromUDPAddrPort"}
	for _, method := range methods {
		credits := newP2pReceiveCredits(1)
		if !credits.acquire(ctx) {
			t.Fatalf("%s fallback receive credit was not admitted", method)
		}
		delegate := &p2pLegacyReadUDPConn{}
		connection := &p2pLinkUDPConn{UDPConn: delegate, inboundCredits: credits}
		readByteCount, err := invokeP2pSurfaceRead(connection, method)
		if err != nil || readByteCount != 1 || delegate.readCount != 1 {
			t.Fatalf(
				"%s fallback bytes=%d delegate reads=%d err=%v",
				method,
				readByteCount,
				delegate.readCount,
				err,
			)
		}
		snapshot := credits.snapshot()
		if snapshot.ReadPacketCount != 1 || snapshot.OutstandingPacketCount != 0 ||
			snapshot.InvalidReleasePacketCount != 0 || !snapshot.isExactLiveTerminal() {
			t.Fatalf("%s fallback receive-credit snapshot=%+v", method, snapshot)
		}
		credits.close()
	}
}

// A short-buffer result still consumed one UDP datagram and must make room for
// the next vnet write even when the caller supplied a zero-length buffer.
func TestP2pLinkUDPConnShortBufferReadReleasesReceiveCredit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	credits := newP2pReceiveCredits(1)
	defer credits.close()
	if !credits.acquire(ctx) {
		t.Fatal("short-buffer receive credit was not admitted")
	}
	delegate := &p2pSurfaceUDPConn{readErr: io.ErrShortBuffer}
	connection := &p2pLinkUDPConn{UDPConn: delegate, inboundCredits: credits}
	readByteCount, err := connection.Read(nil)
	if readByteCount != 0 || !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("short-buffer read bytes=%d err=%v", readByteCount, err)
	}
	snapshot := credits.snapshot()
	if snapshot.ReadPacketCount != 1 || snapshot.OutstandingPacketCount != 0 ||
		!snapshot.isExactLiveTerminal() {
		t.Fatalf("short-buffer receive-credit snapshot=%+v", snapshot)
	}
}

// A zero-length caller buffer still has room for the private frame header in
// the pooled delegate read. The short-buffer result consumes one framed UDP
// datagram and releases its exact reservation.
func TestP2pLinkUDPConnFramedZeroBufferReadReleasesReceiveCredit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credits := newP2pVnetReceiveCredits(1)
	defer credits.close()
	localAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 5751}
	remoteAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 5752}
	socket := credits.registerSocket(localAddress, remoteAddress, nil)
	defer socket.close()
	reservation, hasDestination, admitted := credits.reserveForTransfer(
		ctx,
		remoteAddress,
		localAddress,
	)
	if !hasDestination || !admitted || reservation == nil {
		t.Fatal("framed short-buffer reservation was not admitted")
	}
	expectedPayload := []byte("does-not-fit")
	outerPacket := make([]byte, p2pIPv4UDPHeaderByteCount+len(expectedPayload))
	copy(outerPacket[p2pIPv4UDPHeaderByteCount:], expectedPayload)
	framedPayload, framed := reservation.framePayload(outerPacket)
	if !framed {
		t.Fatal("short-buffer generation frame was not created")
	}
	credits.completeRouterPayload(socket.generation)
	delegate := &p2pSurfaceUDPConn{
		localAddress:  localAddress,
		remoteAddress: remoteAddress,
		readPayload:   append([]byte(nil), framedPayload...),
		readErr:       io.ErrShortBuffer,
	}
	connection := &p2pLinkUDPConn{
		UDPConn:        delegate,
		inboundCredits: credits,
		receiveSocket:  socket,
	}
	readByteCount, err := connection.Read(nil)
	if readByteCount != 0 || !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("framed short-buffer read bytes=%d err=%v", readByteCount, err)
	}
	snapshot := credits.snapshot()
	if snapshot.ReadPacketCount != 1 || snapshot.OutstandingPacketCount != 0 ||
		snapshot.TrackedReservationCount != 0 || snapshot.RouterPendingPacketCount != 0 ||
		!snapshot.isExactLiveTerminal() {
		t.Fatalf("framed short-buffer credits=%+v", snapshot)
	}
}

// A timeout or other empty read error consumes no datagram and leaves the
// exact sender reservation outstanding for a later successful read.
func TestP2pLinkUDPConnEmptyReadErrorRetainsReceiveCredit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	credits := newP2pReceiveCredits(1)
	defer credits.close()
	if !credits.acquire(ctx) {
		t.Fatal("errored-read receive credit was not admitted")
	}
	readErr := errors.New("empty read error")
	delegate := &p2pSurfaceUDPConn{readErr: readErr}
	connection := &p2pLinkUDPConn{UDPConn: delegate, inboundCredits: credits}
	readByteCount, err := connection.Read(make([]byte, 16))
	if readByteCount != 0 || !errors.Is(err, readErr) {
		t.Fatalf("empty errored read bytes=%d err=%v", readByteCount, err)
	}
	snapshot := credits.snapshot()
	if snapshot.ReadPacketCount != 0 || snapshot.OutstandingPacketCount != 1 ||
		snapshot.InvalidReleasePacketCount != 0 {
		t.Fatalf("empty errored-read receive-credit snapshot=%+v", snapshot)
	}
	credits.cancelAdmission()
}

// Each address-bearing method must copy its payload, address, and ancillary
// bytes before returning admission to its caller.
func verifyP2pLinkUDPConnWriteOwnership(t *testing.T, method string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link := newDirectionalLink(ctx, testP2pLinkProfile(1500, oversizeModeDrop), 7401, nil)
	barrier := newP2pSurfaceScheduleBarrier(link)
	t.Cleanup(func() {
		barrier.open()
		link.close()
	})
	delegate := &p2pSurfaceUDPConn{writes: make(chan p2pSurfaceWrite, 1)}
	connection := &p2pLinkUDPConn{UDPConn: delegate, link: link}
	payload := []byte("caller-owned-payload")
	expectedPayload := append([]byte(nil), payload...)
	oob := []byte("caller-owned-oob")
	expectedOob := append([]byte(nil), oob...)
	address := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 9), Port: 4321, Zone: "original"}
	expectedAddress := cloneP2pUDPAddress(address)
	if method == "WriteToAddrPort" || method == "WriteToUDPAddrPort" {
		expectedAddress = net.UDPAddrFromAddrPort(address.AddrPort())
	}

	result := invokeP2pSurfaceWrite(connection, method, payload, oob, address)
	if result.err != nil || result.payloadByteCount != len(payload) {
		t.Fatalf("%s admission bytes=%d err=%v", method, result.payloadByteCount, result.err)
	}
	if method == "WriteMsgUDP" && result.oobByteCount != len(oob) {
		t.Fatalf("%s admission oob bytes=%d", method, result.oobByteCount)
	}
	barrier.wait(t, ctx)
	for index := range payload {
		payload[index] = 0
	}
	for index := range oob {
		oob[index] = 0
	}
	address.IP[0] = 127
	address.Port = 9999
	address.Zone = "mutated"
	barrier.open()
	if !link.waitIdle(ctx) {
		t.Fatalf("%s link did not become idle: %v", method, ctx.Err())
	}
	var write p2pSurfaceWrite
	select {
	case <-ctx.Done():
		t.Fatalf("wait for %s delegate: %v", method, ctx.Err())
	case write = <-delegate.writes:
	}
	if write.method != method || string(write.payload) != string(expectedPayload) {
		t.Fatalf("%s delegate write=%+v", method, write)
	}
	if write.address == nil || write.address.String() != expectedAddress.String() ||
		write.address.Zone != expectedAddress.Zone {
		t.Fatalf("%s delegate address=%v want=%v", method, write.address, expectedAddress)
	}
	if method == "WriteMsgUDP" && string(write.oob) != string(expectedOob) {
		t.Fatalf("%s delegate oob=%q want=%q", method, write.oob, expectedOob)
	}
	if snapshot := link.snapshot(); snapshot.DeliveredPacketCount != 1 ||
		snapshot.ReceiverDropPacketCount != 0 {
		t.Fatalf("%s disposition=%+v", method, snapshot)
	}
}

// Generic-address delayed writes own their mutable inputs.
func TestP2pLinkUDPConnWriteToOwnsPayloadAndAddress(t *testing.T) {
	verifyP2pLinkUDPConnWriteOwnership(t, "WriteTo")
}

// UDP-address delayed writes own their mutable inputs.
func TestP2pLinkUDPConnWriteToUDPOwnsPayloadAndAddress(t *testing.T) {
	verifyP2pLinkUDPConnWriteOwnership(t, "WriteToUDP")
}

// Message writes own payload, ancillary bytes, and destination together.
func TestP2pLinkUDPConnWriteMsgUDPOwnsEveryMutableInput(t *testing.T) {
	verifyP2pLinkUDPConnWriteOwnership(t, "WriteMsgUDP")
}

// A connected WriteMsgUDP call uses RemoteAddr only for receive admission and
// preserves the required nil destination passed to the connected delegate.
func TestP2pLinkUDPConnConnectedWriteMsgUsesRemoteReservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	link := newDirectionalLink(ctx, profile, 7451, nil)
	defer link.close()
	credits := newP2pDestinationReceiveCredits(1)
	defer credits.close()
	localAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 5741}
	remoteAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 5742}
	receiveSocket := credits.registerSocket(remoteAddress, localAddress, nil)
	defer receiveSocket.close()
	delegate := &p2pSurfaceUDPConn{
		writes:        make(chan p2pSurfaceWrite, 1),
		localAddress:  localAddress,
		remoteAddress: remoteAddress,
	}
	connection := &p2pLinkUDPConn{
		UDPConn:         delegate,
		link:            link,
		outboundCredits: credits,
	}
	payload := []byte("connected-message")
	oob := []byte("connected-oob")
	writtenByteCount, writtenOobByteCount, err := connection.WriteMsgUDP(payload, oob, nil)
	if err != nil || writtenByteCount != len(payload) || writtenOobByteCount != len(oob) {
		t.Fatalf(
			"connected WriteMsgUDP bytes=%d oob=%d err=%v",
			writtenByteCount,
			writtenOobByteCount,
			err,
		)
	}
	if !link.waitIdle(ctx) {
		t.Fatalf("join connected WriteMsgUDP: %v", ctx.Err())
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for connected WriteMsgUDP delegate: %v", ctx.Err())
	case write := <-delegate.writes:
		if write.method != "WriteMsgUDP" || write.address != nil ||
			string(write.payload) != string(payload) || string(write.oob) != string(oob) {
			t.Fatalf("connected WriteMsgUDP delegate=%+v", write)
		}
	}
	heldSnapshot := credits.snapshot()
	if heldSnapshot.AdmittedPacketCount != 1 || heldSnapshot.OutstandingPacketCount != 1 ||
		heldSnapshot.TrackedReservationCount != 1 {
		t.Fatalf("connected WriteMsgUDP held credits=%+v", heldSnapshot)
	}
	receiveSocket.recordRead(len(payload), nil)
	snapshot := credits.snapshot()
	if snapshot.ReadPacketCount != 1 || snapshot.OutstandingPacketCount != 0 ||
		snapshot.TrackedReservationCount != 0 || !snapshot.isExactLiveTerminal() {
		t.Fatalf("connected WriteMsgUDP terminal credits=%+v", snapshot)
	}
}

// ICE value-address writes own payloads and retain their immutable destination.
func TestP2pLinkUDPConnWriteToAddrPortOwnsPayload(t *testing.T) {
	verifyP2pLinkUDPConnWriteOwnership(t, "WriteToAddrPort")
}

// Concrete UDP value-address writes use the same delayed ownership path.
func TestP2pLinkUDPConnWriteToUDPAddrPortOwnsPayload(t *testing.T) {
	verifyP2pLinkUDPConnWriteOwnership(t, "WriteToUDPAddrPort")
}

// An optional value-address reader records which concrete method delegated.
type p2pSurfaceAddrPortReadUDPConn struct {
	transport.UDPConn
	methods chan string
	address netip.AddrPort
}

// ICE reads return one fixed payload and address.
func (self *p2pSurfaceAddrPortReadUDPConn) ReadFromAddrPort(
	payload []byte,
) (int, netip.AddrPort, error) {
	self.methods <- "ReadFromAddrPort"
	return copy(payload, "ice-read"), self.address, nil
}

// Concrete UDP reads return a distinguishable fixed payload and address.
func (self *p2pSurfaceAddrPortReadUDPConn) ReadFromUDPAddrPort(
	payload []byte,
) (int, netip.AddrPort, error) {
	self.methods <- "ReadFromUDPAddrPort"
	return copy(payload, "udp-read"), self.address, nil
}

// A legacy reader exposes only transport.UDPConn's pointer-address method.
type p2pSurfaceLegacyReadUDPConn struct {
	transport.UDPConn
	address *net.UDPAddr
}

// Pointer-address fallback returns one fixed payload and address.
func (self *p2pSurfaceLegacyReadUDPConn) ReadFromUDP(
	payload []byte,
) (int, *net.UDPAddr, error) {
	return copy(payload, "legacy-read"), cloneP2pUDPAddress(self.address), nil
}

// Optional value-address reads preserve the exact delegate method and adapt a
// legacy pointer-address socket without allocating on supported sockets.
func TestP2pLinkUDPConnPreservesAddrPortReadSurfaces(t *testing.T) {
	address := netip.MustParseAddrPort("10.240.0.4:4444")
	methods := make(chan string, 2)
	connection := &p2pLinkUDPConn{UDPConn: &p2pSurfaceAddrPortReadUDPConn{
		methods: methods,
		address: address,
	}}
	payload := make([]byte, 32)
	readByteCount, readAddress, err := connection.ReadFromAddrPort(payload)
	if err != nil || string(payload[:readByteCount]) != "ice-read" || readAddress != address {
		t.Fatalf("ICE AddrPort read bytes=%q address=%v err=%v", payload[:readByteCount], readAddress, err)
	}
	if method := <-methods; method != "ReadFromAddrPort" {
		t.Fatalf("ICE AddrPort delegate method=%q", method)
	}
	readByteCount, readAddress, err = connection.ReadFromUDPAddrPort(payload)
	if err != nil || string(payload[:readByteCount]) != "udp-read" || readAddress != address {
		t.Fatalf("UDP AddrPort read bytes=%q address=%v err=%v", payload[:readByteCount], readAddress, err)
	}
	if method := <-methods; method != "ReadFromUDPAddrPort" {
		t.Fatalf("UDP AddrPort delegate method=%q", method)
	}

	legacyAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 3), Port: 3333}
	legacyConnection := &p2pLinkUDPConn{UDPConn: &p2pSurfaceLegacyReadUDPConn{
		address: legacyAddress,
	}}
	readByteCount, readAddress, err = legacyConnection.ReadFromAddrPort(payload)
	if err != nil || string(payload[:readByteCount]) != "legacy-read" ||
		readAddress != legacyAddress.AddrPort() {
		t.Fatalf("legacy AddrPort read bytes=%q address=%v err=%v", payload[:readByteCount], readAddress, err)
	}
}

// Once the wrapper admits a datagram, a later delegate short write or error is
// one receiver drop; it cannot retroactively change the caller's return value.
func TestP2pLinkUDPConnDelegateFailuresBecomeReceiverDrops(t *testing.T) {
	cases := []struct {
		name              string
		method            string
		payloadByteOffset int
		oobByteOffset     int
		writeErr          error
	}{
		{name: "connected short payload", method: "Write", payloadByteOffset: -1},
		{name: "generic address error", method: "WriteTo", writeErr: errors.New("delegate error")},
		{name: "UDP address short payload", method: "WriteToUDP", payloadByteOffset: -1},
		{name: "message short oob", method: "WriteMsgUDP", oobByteOffset: -1},
		{name: "ICE address short payload", method: "WriteToAddrPort", payloadByteOffset: -1},
		{name: "concrete value address error", method: "WriteToUDPAddrPort", writeErr: errors.New("value delegate error")},
	}
	for caseIndex, testCase := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		credits := newP2pReceiveCredits(1)
		link := newDirectionalLink(
			ctx,
			testP2pLinkProfile(1500, oversizeModeDrop),
			7500+int64(caseIndex),
			nil,
		)
		delegate := &p2pSurfaceUDPConn{
			writes:            make(chan p2pSurfaceWrite, 1),
			payloadByteOffset: testCase.payloadByteOffset,
			oobByteOffset:     testCase.oobByteOffset,
			writeErr:          testCase.writeErr,
		}
		connection := &p2pLinkUDPConn{
			UDPConn:         delegate,
			link:            link,
			outboundCredits: credits,
		}
		payload := []byte("accepted-before-delegate")
		oob := []byte("oob")
		address := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 8), Port: 1234}
		result := invokeP2pSurfaceWrite(connection, testCase.method, payload, oob, address)
		if result.err != nil || result.payloadByteCount != len(payload) ||
			(testCase.method == "WriteMsgUDP" && result.oobByteCount != len(oob)) {
			link.close()
			cancel()
			t.Fatalf(
				"%s admission bytes=(%d,%d) err=%v",
				testCase.name,
				result.payloadByteCount,
				result.oobByteCount,
				result.err,
			)
		}
		if !link.waitIdle(ctx) {
			link.close()
			cancel()
			t.Fatalf("%s link did not become idle: %v", testCase.name, ctx.Err())
		}
		snapshot := link.snapshot()
		creditSnapshot := credits.snapshot()
		credits.close()
		link.close()
		cancel()
		if snapshot.DeliveredPacketCount != 0 || snapshot.ReceiverDropPacketCount != 1 ||
			snapshot.QueuedPacketCount != 0 {
			t.Fatalf("%s disposition=%+v", testCase.name, snapshot)
		}
		if creditSnapshot.AdmittedPacketCount != 1 ||
			creditSnapshot.CanceledPacketCount != 1 ||
			creditSnapshot.OutstandingPacketCount != 0 ||
			creditSnapshot.InvalidReleasePacketCount != 0 ||
			!creditSnapshot.isExactLiveTerminal() {
			t.Fatalf("%s receive-credit disposition=%+v", testCase.name, creditSnapshot)
		}
	}
}

// Every write signature translates closed-link and MTU dispositions back to
// its own caller-return shape without invoking the delegate.
func TestP2pLinkUDPConnSurfacesPreserveClosedAndMtuDispositions(t *testing.T) {
	methods := []string{
		"Write",
		"WriteTo",
		"WriteToUDP",
		"WriteMsgUDP",
		"WriteToAddrPort",
		"WriteToUDPAddrPort",
	}
	payload := []byte("surface-admission-disposition")
	oob := []byte("oob")
	address := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 6), Port: 6666}
	outerMtu := len(payload) + p2pIPv4UDPHeaderByteCount - 1
	for methodIndex, method := range methods {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		errorLink := newDirectionalLink(
			ctx,
			testP2pLinkProfile(outerMtu, oversizeModeError),
			7800+int64(3*methodIndex),
			nil,
		)
		errorDelegate := &p2pSurfaceUDPConn{writes: make(chan p2pSurfaceWrite, 1)}
		errorConnection := &p2pLinkUDPConn{UDPConn: errorDelegate, link: errorLink}
		result := invokeP2pSurfaceWrite(errorConnection, method, payload, oob, address)
		var tooLarge *packetTooLargeError
		if result.payloadByteCount != 0 || result.oobByteCount != 0 ||
			!errors.As(result.err, &tooLarge) {
			errorLink.close()
			cancel()
			t.Fatalf("%s MTU error result=%+v", method, result)
		}
		if snapshot := errorLink.snapshot(); snapshot.WireByteCount != 0 ||
			snapshot.MtuDropPacketCount != 0 {
			errorLink.close()
			cancel()
			t.Fatalf("%s MTU error disposition=%+v", method, snapshot)
		}
		errorLink.close()

		dropLink := newDirectionalLink(
			ctx,
			testP2pLinkProfile(outerMtu, oversizeModeDrop),
			7801+int64(3*methodIndex),
			nil,
		)
		dropDelegate := &p2pSurfaceUDPConn{writes: make(chan p2pSurfaceWrite, 1)}
		dropConnection := &p2pLinkUDPConn{UDPConn: dropDelegate, link: dropLink}
		result = invokeP2pSurfaceWrite(dropConnection, method, payload, oob, address)
		if result.err != nil || result.payloadByteCount != len(payload) ||
			(method == "WriteMsgUDP" && result.oobByteCount != len(oob)) {
			dropLink.close()
			cancel()
			t.Fatalf("%s silent MTU result=%+v", method, result)
		}
		if snapshot := dropLink.snapshot(); snapshot.WireByteCount != 0 ||
			snapshot.MtuDropPacketCount != 1 || snapshot.AdmittedPacketCount != 0 {
			dropLink.close()
			cancel()
			t.Fatalf("%s silent MTU disposition=%+v", method, snapshot)
		}
		dropLink.close()

		closedLink := newDirectionalLink(
			ctx,
			testP2pLinkProfile(1500, oversizeModeDrop),
			7802+int64(3*methodIndex),
			nil,
		)
		closedLink.close()
		closedDelegate := &p2pSurfaceUDPConn{writes: make(chan p2pSurfaceWrite, 1)}
		closedConnection := &p2pLinkUDPConn{UDPConn: closedDelegate, link: closedLink}
		result = invokeP2pSurfaceWrite(closedConnection, method, payload, oob, address)
		cancel()
		if result.payloadByteCount != 0 || result.oobByteCount != 0 ||
			!errors.Is(result.err, errLinkClosed) {
			t.Fatalf("%s closed result=%+v", method, result)
		}
	}
}

// A held first write occupies the hard queue for every wrapper method, so a
// second write receives one exact silent ingress-drop disposition.
func TestP2pLinkUDPConnSurfacesShareHardQueueAdmission(t *testing.T) {
	methods := []string{
		"Write",
		"WriteTo",
		"WriteToUDP",
		"WriteMsgUDP",
		"WriteToAddrPort",
		"WriteToUDPAddrPort",
	}
	payload := []byte("surface-hard-queue")
	oob := []byte("oob")
	address := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 5), Port: 5555}
	for methodIndex, method := range methods {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		profile := testP2pLinkProfile(1500, oversizeModeDrop)
		profile.QueuePacketCount = 1
		profile.QueueByteCount = 1500
		link := newDirectionalLink(ctx, profile, 7900+int64(methodIndex), nil)
		barrier := newP2pSurfaceScheduleBarrier(link)
		delegate := &p2pSurfaceUDPConn{writes: make(chan p2pSurfaceWrite, 1)}
		connection := &p2pLinkUDPConn{UDPConn: delegate, link: link}
		first := invokeP2pSurfaceWrite(connection, method, payload, oob, address)
		if first.err != nil || first.payloadByteCount != len(payload) {
			barrier.open()
			link.close()
			cancel()
			t.Fatalf("%s first queue result=%+v", method, first)
		}
		barrier.wait(t, ctx)
		second := invokeP2pSurfaceWrite(connection, method, payload, oob, address)
		if second.err != nil || second.payloadByteCount != len(payload) {
			barrier.open()
			link.close()
			cancel()
			t.Fatalf("%s second queue result=%+v", method, second)
		}
		snapshot := link.snapshot()
		barrier.open()
		link.close()
		cancel()
		if snapshot.AdmittedPacketCount != 1 || snapshot.QueueDropPacketCount != 1 ||
			snapshot.QueuedPacketCount != 1 ||
			snapshot.WireByteCount != uint64(len(payload)+p2pIPv4UDPHeaderByteCount) {
			t.Fatalf("%s hard queue disposition=%+v", method, snapshot)
		}
	}
}

// A PacketConn-only delegate proves alternate listener paths cannot bypass
// delayed admission merely because the socket lacks transport.UDPConn methods.
type p2pSurfacePacketConn struct {
	net.PacketConn
	writes        chan p2pSurfaceWrite
	readByteCount int
	readPayload   []byte
	readCount     int
}

// Generic packet reads expose one consumed datagram to the fallback wrapper.
func (self *p2pSurfacePacketConn) ReadFrom(payload []byte) (int, net.Addr, error) {
	self.readCount += 1
	if self.readPayload != nil {
		return copy(payload, self.readPayload), &net.UDPAddr{
			IP:   net.IPv4(10, 240, 0, 8),
			Port: 8765,
		}, nil
	}
	if 0 < len(payload) {
		payload[0] = 1
	}
	return self.readByteCount, &net.UDPAddr{IP: net.IPv4(10, 240, 0, 8), Port: 8765}, nil
}

// Generic packet writes record the delayed payload and UDP destination.
func (self *p2pSurfacePacketConn) WriteTo(payload []byte, address net.Addr) (int, error) {
	udpAddress, _ := address.(*net.UDPAddr)
	self.writes <- p2pSurfaceWrite{
		method:  "PacketConn.WriteTo",
		payload: append([]byte(nil), payload...),
		address: cloneP2pUDPAddress(udpAddress),
	}
	return len(payload), nil
}

// PacketConn fallback writes traverse the same outer accounting and receiver
// disposition as transport.UDPConn writes.
func TestP2pLinkNetPacketConnFallbackUsesDirectionalLink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link := newDirectionalLink(ctx, testP2pLinkProfile(1500, oversizeModeDrop), 7601, nil)
	barrier := newP2pSurfaceScheduleBarrier(link)
	defer func() {
		barrier.open()
		link.close()
	}()
	delegate := &p2pSurfacePacketConn{writes: make(chan p2pSurfaceWrite, 1)}
	delegateNet := &p2pSurfaceDelegateNet{packetConnection: delegate}
	network := newP2pLinkNet(delegateNet, link)
	connection, err := network.ListenPacket("udp", "10.240.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := connection.(*p2pLinkPacketConn); !ok {
		t.Fatalf("PacketConn fallback type=%T", connection)
	}
	payload := []byte("packet-conn-fallback")
	expectedPayload := append([]byte(nil), payload...)
	address := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 7), Port: 7777}
	expectedAddress := cloneP2pUDPAddress(address)
	writtenByteCount, err := connection.WriteTo(payload, address)
	if err != nil || writtenByteCount != len(payload) {
		t.Fatalf("PacketConn fallback bytes=%d err=%v", writtenByteCount, err)
	}
	barrier.wait(t, ctx)
	for index := range payload {
		payload[index] = 0
	}
	address.IP[0] = 127
	address.Port = 9999
	barrier.open()
	if !link.waitIdle(ctx) {
		t.Fatalf("PacketConn fallback did not become idle: %v", ctx.Err())
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for PacketConn fallback delegate: %v", ctx.Err())
	case write := <-delegate.writes:
		if string(write.payload) != string(expectedPayload) ||
			write.address.String() != expectedAddress.String() {
			t.Fatalf("PacketConn fallback write=%+v", write)
		}
	}
	if snapshot := link.snapshot(); snapshot.DeliveredPacketCount != 1 ||
		snapshot.WireByteCount != uint64(len(expectedPayload)+p2pIPv4UDPHeaderByteCount) {
		t.Fatalf("PacketConn fallback disposition=%+v", snapshot)
	}
}

// The generic PacketConn receive fallback releases the same directional
// reservation as every concrete transport.UDPConn read surface.
func TestP2pLinkPacketConnReadFromReleasesExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	credits := newP2pReceiveCredits(1)
	defer credits.close()
	if !credits.acquire(ctx) {
		t.Fatal("PacketConn receive credit was not admitted")
	}
	delegate := &p2pSurfacePacketConn{readByteCount: 1}
	connection := &p2pLinkPacketConn{PacketConn: delegate, inboundCredits: credits}
	readByteCount, _, err := connection.ReadFrom(make([]byte, 16))
	if err != nil || readByteCount != 1 || delegate.readCount != 1 {
		t.Fatalf(
			"PacketConn read bytes=%d delegate reads=%d err=%v",
			readByteCount,
			delegate.readCount,
			err,
		)
	}
	snapshot := credits.snapshot()
	if snapshot.ReadPacketCount != 1 || snapshot.OutstandingPacketCount != 0 ||
		snapshot.InvalidReleasePacketCount != 0 || !snapshot.isExactLiveTerminal() {
		t.Fatalf("PacketConn receive-credit snapshot=%+v", snapshot)
	}
}

// The generic PacketConn fallback strips the same private generation frame as
// transport.UDPConn and retires its exact destination reservation once.
func TestP2pLinkPacketConnReadFromStripsGenerationFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	credits := newP2pVnetReceiveCredits(1)
	defer credits.close()
	localAddress := &net.UDPAddr{IP: net.IPv4zero, Port: 5761}
	remoteAddress := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 5762}
	socket := credits.registerSocket(localAddress, nil, nil)
	defer socket.close()
	reservation, hasDestination, admitted := credits.reserveForTransfer(
		ctx,
		remoteAddress,
		&net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: localAddress.Port},
	)
	if !hasDestination || !admitted || reservation == nil {
		t.Fatal("PacketConn generation-frame reservation was not admitted")
	}
	expectedPayload := []byte("packet-conn-generation")
	outerPacket := make([]byte, p2pIPv4UDPHeaderByteCount+len(expectedPayload))
	copy(outerPacket[p2pIPv4UDPHeaderByteCount:], expectedPayload)
	framedPayload, framed := reservation.framePayload(outerPacket)
	if !framed {
		t.Fatal("PacketConn generation frame was not created")
	}
	credits.completeRouterPayload(socket.generation)
	delegate := &p2pSurfacePacketConn{readPayload: append([]byte(nil), framedPayload...)}
	connection := &p2pLinkPacketConn{
		PacketConn:     delegate,
		inboundCredits: credits,
		receiveSocket:  socket,
	}
	payload := make([]byte, len(expectedPayload))
	readByteCount, _, err := connection.ReadFrom(payload)
	if err != nil || readByteCount != len(expectedPayload) ||
		string(payload) != string(expectedPayload) {
		t.Fatalf(
			"PacketConn generation read bytes=%d payload=%q err=%v",
			readByteCount,
			payload,
			err,
		)
	}
	snapshot := credits.snapshot()
	if snapshot.ReadPacketCount != 1 || snapshot.OutstandingPacketCount != 0 ||
		snapshot.TrackedReservationCount != 0 || !snapshot.isExactLiveTerminal() {
		t.Fatalf("PacketConn generation credits=%+v", snapshot)
	}
}

// One delegate Net returns fixed identities so wrapper and passthrough
// constructor decisions are directly observable.
type p2pSurfaceDelegateNet struct {
	transport.Net
	packetConnection net.PacketConn
	udpConnection    transport.UDPConn
	streamConnection net.Conn
	dialConnection   net.Conn
	listenPacketErr  error
	listenUDPErr     error
	dialErr          error
	dialUDPErr       error
	configuredDialer transport.Dialer
	configuredListen transport.ListenConfig
}

// Configured dialing returns the delegate selected by the test fixture.
func (self *p2pSurfaceDelegateNet) CreateDialer(dialer *net.Dialer) transport.Dialer {
	_ = dialer
	return self.configuredDialer
}

// Configured listening returns the delegate selected by the test fixture.
func (self *p2pSurfaceDelegateNet) CreateListenConfig(config *net.ListenConfig) transport.ListenConfig {
	_ = config
	return self.configuredListen
}

// Packet listener construction returns the configured delegate identity.
func (self *p2pSurfaceDelegateNet) ListenPacket(network string, address string) (net.PacketConn, error) {
	_ = network
	_ = address
	return self.packetConnection, self.listenPacketErr
}

// UDP listener construction returns the configured delegate identity.
func (self *p2pSurfaceDelegateNet) ListenUDP(
	network string,
	address *net.UDPAddr,
) (transport.UDPConn, error) {
	_ = network
	_ = address
	return self.udpConnection, self.listenUDPErr
}

// Generic dialing returns either the configured UDP or stream identity.
func (self *p2pSurfaceDelegateNet) Dial(network string, address string) (net.Conn, error) {
	_ = address
	if self.dialErr != nil {
		return nil, self.dialErr
	}
	if self.dialConnection != nil {
		return self.dialConnection, nil
	}
	if p2pUDPNetwork(network) {
		return self.udpConnection, nil
	}
	return self.streamConnection, nil
}

// Explicit UDP dialing returns the configured delegate identity.
func (self *p2pSurfaceDelegateNet) DialUDP(
	network string,
	localAddress *net.UDPAddr,
	remoteAddress *net.UDPAddr,
) (transport.UDPConn, error) {
	_ = network
	_ = localAddress
	_ = remoteAddress
	return self.udpConnection, self.dialUDPErr
}

// An embedded stream connection provides identity without implementing I/O.
type p2pSurfaceStreamConn struct {
	net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

// Close publishes exact cleanup when a UDP dial returns an incompatible type.
func (self *p2pSurfaceStreamConn) Close() error {
	if self.closed != nil {
		self.closeOnce.Do(func() { close(self.closed) })
	}
	return nil
}

// Every direct constructor wraps IPv4 UDP and preserves nonmodeled network
// identities unchanged.
func TestP2pLinkNetConstructorsCoverUDPAndPreserveNonUDP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link := newDirectionalLink(ctx, testP2pLinkProfile(1500, oversizeModeDrop), 7701, nil)
	defer link.close()
	udpConnection := &p2pSurfaceUDPConn{writes: make(chan p2pSurfaceWrite, 1)}
	streamConnection := &p2pSurfaceStreamConn{}
	delegate := &p2pSurfaceDelegateNet{
		packetConnection: udpConnection,
		udpConnection:    udpConnection,
		streamConnection: streamConnection,
	}
	network := newP2pLinkNet(delegate, link)

	packetConnection, err := network.ListenPacket("udp4", "10.240.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := packetConnection.(*p2pLinkUDPConn); !ok {
		t.Fatalf("ListenPacket udp4 type=%T", packetConnection)
	}
	packetConnection, err = network.ListenPacket("udp6", "[::1]:0")
	if err != nil || packetConnection != udpConnection {
		t.Fatalf("ListenPacket udp6 connection=%T err=%v", packetConnection, err)
	}
	listenedUDP, err := network.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := listenedUDP.(*p2pLinkUDPConn); !ok {
		t.Fatalf("ListenUDP udp4 type=%T", listenedUDP)
	}
	listenedUDP, err = network.ListenUDP("udp6", &net.UDPAddr{})
	if err != nil || listenedUDP != udpConnection {
		t.Fatalf("ListenUDP udp6 connection=%T err=%v", listenedUDP, err)
	}
	dialed, err := network.Dial("udp4", "10.240.0.2:9")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dialed.(*p2pLinkUDPConn); !ok {
		t.Fatalf("Dial udp4 type=%T", dialed)
	}
	dialed, err = network.Dial("tcp4", "10.240.0.2:9")
	if err != nil || dialed != streamConnection {
		t.Fatalf("Dial tcp4 connection=%T err=%v", dialed, err)
	}
	dialedUDP, err := network.DialUDP("udp4", nil, &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dialedUDP.(*p2pLinkUDPConn); !ok {
		t.Fatalf("DialUDP udp4 type=%T", dialedUDP)
	}
	dialedUDP, err = network.DialUDP("udp6", nil, &net.UDPAddr{})
	if err != nil || dialedUDP != udpConnection {
		t.Fatalf("DialUDP udp6 connection=%T err=%v", dialedUDP, err)
	}
}

// Direct constructor errors remain unchanged, and a generic UDP dial closes
// an incompatible net.Conn before reporting transport.ErrNotSupported.
func TestP2pLinkNetConstructorsPreserveErrorsAndCloseWrongDialType(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link := newDirectionalLink(ctx, testP2pLinkProfile(1500, oversizeModeDrop), 7702, nil)
	defer link.close()
	sentinel := errors.New("delegate constructor error")
	delegate := &p2pSurfaceDelegateNet{
		listenPacketErr: sentinel,
		listenUDPErr:    sentinel,
		dialErr:         sentinel,
		dialUDPErr:      sentinel,
	}
	network := newP2pLinkNet(delegate, link)
	if _, err := network.ListenPacket("udp", "10.240.0.1:0"); !errors.Is(err, sentinel) {
		t.Fatalf("ListenPacket error=%v", err)
	}
	if _, err := network.ListenUDP("udp", &net.UDPAddr{}); !errors.Is(err, sentinel) {
		t.Fatalf("ListenUDP error=%v", err)
	}
	if _, err := network.Dial("udp", "10.240.0.2:9"); !errors.Is(err, sentinel) {
		t.Fatalf("Dial error=%v", err)
	}
	if _, err := network.DialUDP("udp", nil, &net.UDPAddr{}); !errors.Is(err, sentinel) {
		t.Fatalf("DialUDP error=%v", err)
	}

	wrongConnection := &p2pSurfaceStreamConn{closed: make(chan struct{})}
	wrongNetwork := newP2pLinkNet(
		&p2pSurfaceDelegateNet{dialConnection: wrongConnection},
		link,
	)
	connection, err := wrongNetwork.Dial("udp", "10.240.0.2:9")
	if connection != nil || !errors.Is(err, transport.ErrNotSupported) {
		t.Fatalf("wrong UDP dial connection=%T err=%v", connection, err)
	}
	select {
	case <-wrongConnection.closed:
	default:
		t.Fatal("wrong UDP dial connection was not closed")
	}
}

// A configured dialer exposes exact delegate errors and connection identities.
type p2pSurfaceDialer struct {
	connection net.Conn
	err        error
}

// Dial returns the configured result without interpreting its network.
func (self *p2pSurfaceDialer) Dial(network string, address string) (net.Conn, error) {
	_ = network
	_ = address
	return self.connection, self.err
}

// A configured listener exposes stream and packet results independently.
type p2pSurfaceListenConfig struct {
	listener    net.Listener
	listenerErr error
	packet      net.PacketConn
	packetErr   error
}

// Stream listening returns the exact delegate result.
func (self *p2pSurfaceListenConfig) Listen(
	ctx context.Context,
	network string,
	address string,
) (net.Listener, error) {
	_ = ctx
	_ = network
	_ = address
	return self.listener, self.listenerErr
}

// Packet listening returns the exact delegate result.
func (self *p2pSurfaceListenConfig) ListenPacket(
	ctx context.Context,
	network string,
	address string,
) (net.PacketConn, error) {
	_ = ctx
	_ = network
	_ = address
	return self.packet, self.packetErr
}

// Configured constructors preserve errors and non-UDP identities, and close
// incompatible UDP dial results just like direct dialing.
func TestP2pLinkNetConfiguredConstructorsPreserveFailuresAndPassthrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link := newDirectionalLink(ctx, testP2pLinkProfile(1500, oversizeModeDrop), 7703, nil)
	defer link.close()
	sentinel := errors.New("configured delegate error")
	errorNetwork := newP2pLinkNet(
		&p2pSurfaceDelegateNet{
			configuredDialer: &p2pSurfaceDialer{err: sentinel},
			configuredListen: &p2pSurfaceListenConfig{
				listenerErr: sentinel,
				packetErr:   sentinel,
			},
		},
		link,
	)
	errorDialer := errorNetwork.CreateDialer(&net.Dialer{})
	if _, err := errorDialer.Dial("udp", "10.240.0.2:9"); !errors.Is(err, sentinel) {
		t.Fatalf("configured Dial error=%v", err)
	}
	errorListen := errorNetwork.CreateListenConfig(&net.ListenConfig{})
	if _, err := errorListen.Listen(ctx, "tcp", "10.240.0.1:0"); !errors.Is(err, sentinel) {
		t.Fatalf("configured Listen error=%v", err)
	}
	if _, err := errorListen.ListenPacket(ctx, "udp", "10.240.0.1:0"); !errors.Is(err, sentinel) {
		t.Fatalf("configured ListenPacket error=%v", err)
	}

	wrongConnection := &p2pSurfaceStreamConn{closed: make(chan struct{})}
	wrongNetwork := newP2pLinkNet(
		&p2pSurfaceDelegateNet{
			configuredDialer: &p2pSurfaceDialer{connection: wrongConnection},
		},
		link,
	)
	connection, err := wrongNetwork.CreateDialer(&net.Dialer{}).Dial("udp", "10.240.0.2:9")
	if connection != nil || !errors.Is(err, transport.ErrNotSupported) {
		t.Fatalf("configured wrong UDP connection=%T err=%v", connection, err)
	}
	select {
	case <-wrongConnection.closed:
	default:
		t.Fatal("configured wrong UDP connection was not closed")
	}

	streamConnection := &p2pSurfaceStreamConn{}
	packetConnection := &p2pSurfacePacketConn{writes: make(chan p2pSurfaceWrite, 1)}
	passthroughNetwork := newP2pLinkNet(
		&p2pSurfaceDelegateNet{
			configuredDialer: &p2pSurfaceDialer{connection: streamConnection},
			configuredListen: &p2pSurfaceListenConfig{packet: packetConnection},
		},
		link,
	)
	connection, err = passthroughNetwork.CreateDialer(&net.Dialer{}).Dial("tcp", "10.240.0.2:9")
	if err != nil || connection != streamConnection {
		t.Fatalf("configured TCP connection=%T err=%v", connection, err)
	}
	packet, err := passthroughNetwork.CreateListenConfig(&net.ListenConfig{}).
		ListenPacket(ctx, "udp6", "[::1]:0")
	if err != nil || packet != packetConnection {
		t.Fatalf("configured udp6 PacketConn=%T err=%v", packet, err)
	}
}
