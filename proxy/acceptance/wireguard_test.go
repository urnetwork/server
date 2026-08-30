package acceptance

import (
	"encoding/binary"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func acceptanceTCPPacket(source, destination netip.Addr, flags header.TCPFlags, payloadBytes int) []byte {
	packet := make([]byte, 40+payloadBytes)
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	packet[9] = uint8(header.TCPProtocolNumber)
	copy(packet[12:16], source.AsSlice())
	copy(packet[16:20], destination.AsSlice())
	packet[20+12] = 5 << 4
	packet[20+13] = byte(flags)
	return packet
}

func TestWireGuardPacketTraceDistinguishesTLSResponseLoss(t *testing.T) {
	clientIP := netip.MustParseAddr("10.0.0.2")
	targetIP := netip.MustParseAddr("65.49.70.82")
	stack := &wireGuardStack{clientIPv4: clientIP}
	before := stack.packetStats()
	stack.observePacket(acceptanceTCPPacket(clientIP, targetIP, header.TCPFlagSyn, 0), true)
	stack.observePacket(acceptanceTCPPacket(targetIP, clientIP, header.TCPFlagSyn|header.TCPFlagAck, 0), false)
	stack.observePacket(acceptanceTCPPacket(clientIP, targetIP, header.TCPFlagAck, 517), true) // TLS ClientHello
	stack.observePacket(acceptanceTCPPacket(targetIP, clientIP, header.TCPFlagAck, 0), false)  // origin ACK, no ServerHello
	after := stack.packetStats()

	out := subtractWireGuardDirection(before.Outbound, after.Outbound)
	in := subtractWireGuardDirection(before.Inbound, after.Inbound)
	if out.Packets != 2 || out.LocalAddrPackets != 2 || out.ForeignAddrPackets != 0 || out.Syn != 1 || out.TCPPayloadPackets != 1 || out.TCPPayloadBytes != 517 {
		t.Fatalf("outbound trace = %+v", out)
	}
	if in.Packets != 2 || in.LocalAddrPackets != 2 || in.ForeignAddrPackets != 0 || in.SynAck != 1 || in.TCPPayloadPackets != 0 || in.TCPPayloadBytes != 0 {
		t.Fatalf("inbound trace = %+v", in)
	}
	detail := wireGuardPacketStatsDelta(before, after, time.Now())
	for _, want := range []string{"out{packets=2", "local=2 foreign=0", "payload=1/517B", "in{packets=2", "payload=0/0B", "synack=1"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("packet trace %q missing %q", detail, want)
		}
	}
}

func TestWireGuardPacketTraceRejectsMalformedTCPHeaders(t *testing.T) {
	stack := &wireGuardStack{}
	stack.observePacket([]byte{4 << 4}, true)
	stack.observePacket(acceptanceTCPPacket(netip.IPv4Unspecified(), netip.IPv4Unspecified(), header.TCPFlagAck, 10)[:25], false)
	stats := stack.packetStats()
	if stats.Outbound.Packets != 1 || stats.Outbound.TCPPackets != 0 {
		t.Fatalf("short IPv4 trace = %+v", stats.Outbound)
	}
	if stats.Inbound.Packets != 1 || stats.Inbound.TCPPackets != 0 {
		t.Fatalf("truncated TCP trace = %+v", stats.Inbound)
	}
}

func TestWireGuardPacketTraceIdentifiesForeignReturnTraffic(t *testing.T) {
	clientIP := netip.MustParseAddr("10.0.0.2")
	targetIP := netip.MustParseAddr("65.49.70.82")
	tunClientIP := netip.MustParseAddr("169.254.2.2")
	stack := &wireGuardStack{clientIPv4: clientIP}
	stack.observePacket(acceptanceTCPPacket(targetIP, clientIP, header.TCPFlagAck, 10), false)
	stack.observePacket(acceptanceTCPPacket(targetIP, tunClientIP, header.TCPFlagRst, 0), false)

	stats := stack.packetStats().Inbound
	if stats.LocalAddrPackets != 1 || stats.ForeignAddrPackets != 1 || stats.Rst != 1 {
		t.Fatalf("foreign return trace = %+v", stats)
	}
}

type acceptanceRoundTripper func(*http.Request) (*http.Response, error)

func (f acceptanceRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type acceptanceCloseRecorder struct {
	closed bool
}

func (self *acceptanceCloseRecorder) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (self *acceptanceCloseRecorder) Close() error {
	self.closed = true
	return nil
}

func TestWireGuardTransportRejectsForeignReturnTraffic(t *testing.T) {
	clientIP := netip.MustParseAddr("10.0.0.2")
	targetIP := netip.MustParseAddr("65.49.70.82")
	tunClientIP := netip.MustParseAddr("169.254.2.2")
	stack := &wireGuardStack{clientIPv4: clientIP}
	body := &acceptanceCloseRecorder{}
	transport := &wireGuardDiagnosticTransport{
		stack: stack,
		roundTripper: acceptanceRoundTripper(func(*http.Request) (*http.Response, error) {
			stack.observePacket(acceptanceTCPPacket(targetIP, tunClientIP, header.TCPFlagRst, 0), false)
			return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
		}),
	}
	request, err := http.NewRequest(http.MethodGet, "https://api.bringyour.com/hello", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := transport.RoundTrip(request)
	if err == nil || !strings.Contains(err.Error(), "packet for another proxy protocol") {
		t.Fatalf("foreign return error = %v", err)
	}
	if response != nil {
		t.Fatalf("foreign return response = %#v, want nil", response)
	}
	if !body.closed {
		t.Fatal("foreign return response body was not closed")
	}
}
