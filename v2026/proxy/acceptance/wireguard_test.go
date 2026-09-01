package acceptance

import (
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func acceptanceTCPPacket(source, destination netip.Addr, flags header.TCPFlags, payloadBytes int) []byte {
	return acceptanceTCPPacketWithFields(source, destination, 40000, 443, 1000, 9000, flags, payloadBytes, true)
}

func acceptanceTCPPacketWithFields(
	source netip.Addr,
	destination netip.Addr,
	sourcePort uint16,
	destinationPort uint16,
	sequence uint32,
	acknowledgment uint32,
	flags header.TCPFlags,
	payloadBytes int,
	validChecksum bool,
) []byte {
	packet := make([]byte, 40+payloadBytes)
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	packet[9] = uint8(header.TCPProtocolNumber)
	copy(packet[12:16], source.AsSlice())
	copy(packet[16:20], destination.AsSlice())
	tcpHeader := header.TCP(packet[20:])
	tcpHeader.Encode(&header.TCPFields{
		SrcPort:    sourcePort,
		DstPort:    destinationPort,
		SeqNum:     sequence,
		AckNum:     acknowledgment,
		DataOffset: header.TCPMinimumSize,
		Flags:      flags,
		WindowSize: 65535,
	})
	pseudoHeaderChecksum := header.PseudoHeaderChecksum(
		header.TCPProtocolNumber,
		tcpip.AddrFrom4Slice(source.AsSlice()),
		tcpip.AddrFrom4Slice(destination.AsSlice()),
		uint16(len(packet)-20),
	)
	tcpHeader.SetChecksum(^checksum.Checksum(tcpHeader, pseudoHeaderChecksum))
	if !validChecksum {
		tcpHeader.SetChecksum(tcpHeader.Checksum() ^ 1)
	}
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

func TestWireGuardPacketTraceValidatesResetForActiveDial(t *testing.T) {
	clientIP := netip.MustParseAddr("10.0.0.2")
	targetIP := netip.MustParseAddr("65.49.70.85")
	clientPort := uint16(51001)
	targetPort := uint16(443)
	stack := &wireGuardStack{clientIPv4: clientIP}
	stack.stats.DialAddr = targetIP
	stack.stats.DialPort = targetPort
	before := stack.packetStats()

	stack.observePacket(acceptanceTCPPacketWithFields(clientIP, targetIP, clientPort, targetPort, 100, 0, header.TCPFlagSyn, 0, true), true)
	stack.observePacket(acceptanceTCPPacketWithFields(targetIP, clientIP, targetPort, clientPort, 9000, 101, header.TCPFlagSyn|header.TCPFlagAck, 0, true), false)
	stack.observePacket(acceptanceTCPPacketWithFields(clientIP, targetIP, clientPort, targetPort, 101, 9001, header.TCPFlagAck|header.TCPFlagPsh, 517, true), true)
	stack.observePacket(acceptanceTCPPacketWithFields(targetIP, clientIP, targetPort, clientPort, 9001, 0, header.TCPFlagRst, 0, true), false)
	// A reset addressed to the WireGuard client but for a different socket
	// must not be conflated with the failed HTTPS dial. Corrupt its checksum so
	// both diagnostic dimensions are pinned by one deterministic trace.
	stack.observePacket(acceptanceTCPPacketWithFields(targetIP, clientIP, 8443, clientPort+1, 77, 0, header.TCPFlagRst, 0, false), false)

	after := stack.packetStats()
	in := subtractWireGuardDirection(before.Inbound, after.Inbound)
	if in.Rst != 2 || in.RstForDial != 1 || in.RstOther != 1 || in.RstSequenceMatch != 1 || in.RstSequenceMismatch != 0 {
		t.Fatalf("inbound reset trace = %+v", in)
	}
	if in.TCPChecksumValid != 2 || in.TCPChecksumInvalid != 1 {
		t.Fatalf("inbound checksum trace = %+v", in)
	}
	detail := wireGuardPacketStatsDelta(before, after, time.Now())
	for _, want := range []string{
		"target=65.49.70.85:443",
		"rst_dial=1 rst_other=1",
		"rst_seq=1/0(match/mismatch)",
		"65.49.70.85:443->10.0.0.2:51001 seq=9001 ack=0 flags=RST checksum=true expected_seq=9001",
		"65.49.70.85:8443->10.0.0.2:51002 seq=77 ack=0 flags=RST checksum=false",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("packet trace %q missing %q", detail, want)
		}
	}
}

type acceptanceRoundTripper func(*http.Request) (*http.Response, error)

func (f acceptanceRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type acceptanceCloseRecorder struct {
	closed bool
}

type acceptanceReadErrorBody struct {
	err error
}

func (b *acceptanceReadErrorBody) Read(p []byte) (int, error) {
	copy(p, "partial")
	return len("partial"), b.err
}

func (*acceptanceReadErrorBody) Close() error {
	return nil
}

type acceptanceObservedBody struct {
	read func()
}

func (b *acceptanceObservedBody) Read([]byte) (int, error) {
	b.read()
	return 0, io.EOF
}

func (*acceptanceObservedBody) Close() error {
	return nil
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

func TestWireGuardTransportTracesResponseBodyFailure(t *testing.T) {
	clientIP := netip.MustParseAddr("10.0.0.2")
	targetIP := netip.MustParseAddr("65.49.70.82")
	stack := &wireGuardStack{clientIPv4: clientIP}
	bodyErr := errors.New("response body stalled")
	transport := &wireGuardDiagnosticTransport{
		stack: stack,
		roundTripper: acceptanceRoundTripper(func(*http.Request) (*http.Response, error) {
			stack.observePacket(acceptanceTCPPacket(clientIP, targetIP, header.TCPFlagAck, 517), true)
			stack.observePacket(acceptanceTCPPacket(targetIP, clientIP, header.TCPFlagAck, 12), false)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &acceptanceReadErrorBody{err: bodyErr},
			}, nil
		}),
	}
	request, err := http.NewRequest(http.MethodGet, "https://ur.io/ip", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip failed before body read: %v", err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	if !errors.Is(err, bodyErr) {
		t.Fatalf("body error = %v, want wrapped %v", err, bodyErr)
	}
	for _, want := range []string{
		"WireGuard inner packet trace",
		"out{packets=1",
		"payload=1/517B",
		"in{packets=1",
		"payload=1/12B",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("body error %q missing %q", err, want)
		}
	}
}

func TestWireGuardTransportRejectsForeignTrafficObservedWhileReadingBody(t *testing.T) {
	clientIP := netip.MustParseAddr("10.0.0.2")
	targetIP := netip.MustParseAddr("65.49.70.82")
	tunClientIP := netip.MustParseAddr("169.254.2.2")
	stack := &wireGuardStack{clientIPv4: clientIP}
	transport := &wireGuardDiagnosticTransport{
		stack: stack,
		roundTripper: acceptanceRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: &acceptanceObservedBody{read: func() {
					stack.observePacket(acceptanceTCPPacket(targetIP, tunClientIP, header.TCPFlagRst, 0), false)
				}},
			}, nil
		}),
	}
	request, err := http.NewRequest(http.MethodGet, "https://ur.io/ip", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip failed before body read: %v", err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	if err == nil || !strings.Contains(err.Error(), "packet for another proxy protocol") {
		t.Fatalf("body foreign-return error = %v", err)
	}
}
