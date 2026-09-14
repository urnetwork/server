package connect

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

type ppScriptedPacket struct {
	payload []byte
	addr    net.Addr
	err     error
}

type ppScriptedPacketConn struct {
	packets   []ppScriptedPacket
	readCount int
}

func (self *ppScriptedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if len(self.packets) <= self.readCount {
		return 0, nil, io.EOF
	}
	packet := self.packets[self.readCount]
	self.readCount += 1
	if packet.err != nil {
		return 0, packet.addr, packet.err
	}
	return copy(p, packet.payload), packet.addr, nil
}

func (self *ppScriptedPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	return len(p), nil
}

func (self *ppScriptedPacketConn) Close() error                     { return nil }
func (self *ppScriptedPacketConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (self *ppScriptedPacketConn) SetDeadline(time.Time) error      { return nil }
func (self *ppScriptedPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (self *ppScriptedPacketConn) SetWriteDeadline(time.Time) error { return nil }

func ppV2Ipv4Header(protocol byte) []byte {
	return []byte{
		0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51,
		0x55, 0x49, 0x54, 0x0A, 0x21, protocol, 0x00, 0x0C,
		127, 0, 0, 1, 127, 0, 0, 1, 0xCA, 0x2B, 0x04, 0x01,
	}
}

// TestParsePpHeaderPacketV1 is the platform-independent proxy-protocol
// regression. The nginx end-to-end fixture uses Unix process groups and is
// build-tagged accordingly; header decoding must remain compiled and tested on
// Windows too.
func TestParsePpHeaderPacketV1(t *testing.T) {
	const header = "PROXY TCP4 192.0.2.10 198.51.100.20 12345 443\r\n"
	payload := []byte(header + "payload")

	headerByteCount, parsed, err := parsePpHeaderPacket(payload)
	if err != nil {
		t.Fatal(err)
	}
	if headerByteCount != len(header) {
		t.Fatalf("header byte count = %d, want %d", headerByteCount, len(header))
	}
	if parsed == nil {
		t.Fatal("proxy header was not detected")
	}
	source, ok := parsed.Source.(*net.TCPAddr)
	if !ok {
		t.Fatalf("source type = %T, want *net.TCPAddr", parsed.Source)
	}
	if source.IP.String() != "192.0.2.10" || source.Port != 12345 {
		t.Fatalf("source = %s, want 192.0.2.10:12345", source)
	}
}

// Packet validation failures must remain local to PpPacketConn. In
// particular, more than the historical ten-discard budget cannot leak an
// error into quic-go and terminate the listener shared by every client.
func TestPpPacketConnDropsInvalidPacketsUntilValidUdp(t *testing.T) {
	proxyAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40100}
	packets := make([]ppScriptedPacket, 0, 20)
	for range 12 {
		packets = append(packets, ppScriptedPacket{
			payload: []byte("headerless-initial"),
			addr:    proxyAddr,
		})
	}
	packets = append(packets,
		ppScriptedPacket{
			payload: append(append([]byte{}, V2Identifier...), 0x21),
			addr:    proxyAddr,
		},
		ppScriptedPacket{
			payload: append(ppV2Ipv4Header(0x11), []byte("tcp-family")...),
			addr:    proxyAddr,
		},
		ppScriptedPacket{
			payload: []byte("wrong-proxy-address-family"),
			addr:    &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40100},
		},
	)
	const firstPayload = "valid-initial"
	packets = append(packets,
		ppScriptedPacket{
			payload: append(ppV2Ipv4Header(0x12), []byte(firstPayload)...),
			addr:    proxyAddr,
		},
		ppScriptedPacket{
			payload: []byte("valid-followup"),
			addr:    proxyAddr,
		},
	)

	underlying := &ppScriptedPacketConn{packets: packets}
	drops := map[string]int{}
	settings := DefaultWarpPpSettings()
	settings.DropObserver = func(reason string) {
		drops[reason] += 1
	}
	conn := NewPpPacketConn(underlying, settings)
	buffer := make([]byte, 1500)

	n, addr, err := conn.ReadFrom(buffer)
	if err != nil {
		t.Fatalf("invalid packet escaped from PpPacketConn: %v", err)
	}
	if !bytes.Equal(buffer[:n], []byte(firstPayload)) {
		t.Fatalf("payload=%q want=%q", buffer[:n], firstPayload)
	}
	realAddr, ok := addr.(*net.UDPAddr)
	if !ok || realAddr.Port != 51755 || !realAddr.IP.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("real address=%v", addr)
	}
	if drops[ppDropMissingHeader] != 12 ||
		drops[ppDropMalformedHeader] != 1 ||
		drops[ppDropTransportFamily] != 1 ||
		drops[ppDropProxyAddressFamily] != 1 {
		t.Fatalf("drop reasons=%v", drops)
	}

	n, followupAddr, err := conn.ReadFrom(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if string(buffer[:n]) != "valid-followup" || followupAddr.String() != realAddr.String() {
		t.Fatalf("followup payload/address=%q/%v want valid-followup/%v", buffer[:n], followupAddr, realAddr)
	}
}

func TestPpPacketConnReturnsUnderlyingSocketError(t *testing.T) {
	underlying := &ppScriptedPacketConn{packets: []ppScriptedPacket{{err: io.ErrClosedPipe}}}
	conn := NewPpPacketConn(underlying, DefaultWarpPpSettings())
	if _, _, err := conn.ReadFrom(make([]byte, 1500)); err != io.ErrClosedPipe {
		t.Fatalf("underlying error=%v want=%v", err, io.ErrClosedPipe)
	}
}
