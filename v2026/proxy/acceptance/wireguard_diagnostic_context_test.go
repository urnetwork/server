package acceptance

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/userwireguard/v2026/tun/tuntest"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// No packets are sent: closing a gonet UDP endpoint while its read is blocked
// must describe our cancellation, not imply an EOF from the profile's DNS server.
func TestWireGuardDNSBlockedReadPreservesContextError(t *testing.T) {
	stackCtx, stopStack := context.WithCancel(context.Background())
	stack, err := newWireGuardStack(stackCtx, netip.MustParseAddr("10.0.0.2"), 1420, tuntest.NewChannelTUN())
	if err != nil {
		stopStack()
		t.Fatal(err)
	}
	defer func() { stopStack(); stack.Close() }()
	stack.dnsServer = netip.MustParseAddr("1.1.1.1")
	for _, method := range []string{"Read", "ReadFrom"} {
		for _, deadline := range []bool{false, true} {
			name := method + "/cancel"
			if deadline {
				name = method + "/deadline"
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				want := context.Canceled
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 25*time.Millisecond)
					want = context.DeadlineExceeded
				}
				defer cancel()
				connection, err := stack.dialDNSContext(ctx, "udp", "ignored.invalid:53")
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				packet, ok := connection.(net.PacketConn)
				if !ok {
					t.Fatal("UDP DNS connection lost packet framing")
				}
				finished := make(chan error, 1)
				joined := false
				t.Cleanup(func() {
					cancel()
					connection.Close()
					if !joined {
						select {
						case <-finished:
						case <-time.After(time.Second):
							t.Error("DNS reader did not join after connection close")
						}
					}
				})
				go func() {
					buffer := make([]byte, 512)
					var err error
					if method == "Read" {
						_, err = connection.Read(buffer)
					} else {
						_, _, err = packet.ReadFrom(buffer)
					}
					finished <- err
				}()
				if !deadline {
					cancel()
				}
				select {
				case err := <-finished:
					joined = true
					if !errors.Is(err, want) {
						t.Fatalf("canceled DNS %s = %v; want %v", method, err, want)
					}
				case <-time.After(time.Second):
					t.Fatal("cancellation left DNS read blocked")
				}
			})
		}
	}
}

type wireGuardDiagnosticErrorConn struct {
	err error
}

func (c *wireGuardDiagnosticErrorConn) Read(p []byte) (int, error) {
	return min(1, len(p)), c.err
}
func (c *wireGuardDiagnosticErrorConn) Write(p []byte) (int, error) {
	return min(1, len(p)), c.err
}
func (*wireGuardDiagnosticErrorConn) Close() error                     { return nil }
func (*wireGuardDiagnosticErrorConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (*wireGuardDiagnosticErrorConn) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (*wireGuardDiagnosticErrorConn) SetDeadline(time.Time) error      { return nil }
func (*wireGuardDiagnosticErrorConn) SetReadDeadline(time.Time) error  { return nil }
func (*wireGuardDiagnosticErrorConn) SetWriteDeadline(time.Time) error { return nil }

type wireGuardDiagnosticErrorPacketConn struct{ wireGuardDiagnosticErrorConn }

func (c *wireGuardDiagnosticErrorPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.Read(p)
	return n, c.RemoteAddr(), err
}
func (c *wireGuardDiagnosticErrorPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	return c.Write(p)
}

func TestWireGuardDNSIOContextAttribution(t *testing.T) {
	remoteError := errors.New("controlled non-context I/O failure")
	for _, test := range []struct {
		name                           string
		cancel, deadline, contextClose bool
		input, want                    error
	}{
		{name: "live_eof", input: io.EOF, want: io.EOF},
		{name: "live_error", input: remoteError, want: remoteError},
		{name: "canceled_without_context_close", cancel: true, input: io.EOF, want: io.EOF},
		{name: "canceled_close", cancel: true, contextClose: true, input: io.EOF, want: context.Canceled},
		{name: "deadline_close", deadline: true, contextClose: true, input: io.EOF, want: context.DeadlineExceeded},
		{name: "successful_after_cancel", cancel: true, contextClose: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if test.deadline {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			}
			defer cancel()
			if test.cancel {
				cancel()
			}
			raw := &wireGuardDiagnosticErrorPacketConn{wireGuardDiagnosticErrorConn{err: test.input}}
			base := &wireGuardDNSConn{Conn: raw, ctx: ctx, stopCancel: func() bool { return true }}
			base.closedByContext.Store(test.contextClose)
			packet := &wireGuardDNSPacketConn{wireGuardDNSConn: base}
			for _, method := range []string{"Read", "Write", "ReadFrom", "WriteTo"} {
				var n int
				var err error
				buffer := make([]byte, 2)
				switch method {
				case "Read":
					n, err = packet.Read(buffer)
				case "Write":
					n, err = packet.Write(buffer)
				case "ReadFrom":
					n, _, err = packet.ReadFrom(buffer)
				case "WriteTo":
					n, err = packet.WriteTo(buffer, raw.RemoteAddr())
				}
				if n != 1 || !errors.Is(err, test.want) {
					t.Errorf("%s = %d, %v; want 1, %v", method, n, err, test.want)
				}
			}
			// The stream wrapper itself must not gain PacketConn: the resolver
			// uses its presence to choose DNS framing on UDP versus TCP fallback.
			if _, ok := any(base).(net.PacketConn); ok {
				t.Fatal("stream wrapper acquired datagram framing")
			}
		})
	}
}

func TestWireGuardDialAttemptKeepsLateTrafficSeparate(t *testing.T) {
	clientIP := netip.MustParseAddr("10.0.0.2")
	targetIP := netip.MustParseAddr("192.0.2.1")
	stack := &wireGuardStack{clientIPv4: clientIP}
	stack.stats.DialAddr, stack.stats.DialPort = targetIP, 443
	stack.stats.DialAttemptSequence, stack.stats.ResolvedDialAttemptSequence = 1, 1
	before := stack.packetStats()
	_, err := stack.DialContext(context.Background(), "tcp", "unconfigured.invalid:443")
	if err == nil {
		t.Fatal("unconfigured resolver unexpectedly succeeded")
	}
	// The older active flow remains accounted for even after the new DNS
	// failure; its bounded TCP history is explicitly not the new attempt's.
	stack.observePacket(acceptanceTCPPacket(clientIP, targetIP, header.TCPFlagAck, 24), true)
	after := stack.packetStats()
	if after.Outbound.Packets != 1 || after.Outbound.TCPPayloadBytes != 24 || after.DialAddr != targetIP || after.EventSequence != 1 {
		t.Fatalf("active flow accounting lost: %+v", after)
	}
	detail := wireGuardPacketStatsDelta(before, after, time.Now())
	if !strings.Contains(detail, "target=unavailable packet_scope=transport") || strings.Contains(detail, "tcp_recent=") {
		t.Fatalf("late previous-flow packet attributed to unresolved attempt: %s", detail)
	}
	// Stored events from an earlier resolved generation are excluded. This is
	// not full TCP tuple isolation: later packets to the same remote target can
	// still be observed under the newer resolved generation (see next test).
	after.ResolvedDialAttemptSequence = after.DialAttemptSequence
	after.EventSequence = 2
	after.RecentTCPEvents[1] = after.RecentTCPEvents[0]
	after.RecentTCPEvents[1].EventSequence = 2
	after.RecentTCPEvents[1].DialAttemptSequence = after.DialAttemptSequence
	after.RecentTCPEvents[1].TCPSequence = 4242
	detail = wireGuardPacketStatsDelta(before, after, time.Now())
	if !strings.Contains(detail, "seq=4242") || strings.Contains(detail, "seq=1000") {
		t.Fatalf("TCP generations mixed: %s", detail)
	}
}

func TestWireGuardTCPHistoryStatesRemoteTargetScope(t *testing.T) {
	clientIP, targetIP := netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("192.0.2.1")
	stack := &wireGuardStack{clientIPv4: clientIP}
	stack.stats.DialAddr, stack.stats.DialPort = targetIP, 443
	stack.stats.DialAttemptSequence, stack.stats.ResolvedDialAttemptSequence = 2, 2
	stack.stats.DialFlow.SourcePort = 51002
	before := stack.packetStats()
	for _, port := range []uint16{51001, 51002} {
		stack.observePacket(acceptanceTCPPacketWithFields(targetIP, clientIP, 443, port, 1000, 9000, header.TCPFlagAck, 0, true), false)
	}
	detail := wireGuardPacketStatsDelta(before, stack.packetStats(), time.Now())
	for _, want := range []string{"packet_scope=transport", "tcp_recent_scope=resolved_remote_target", "443->51001", "443->51002"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("trace %q missing scope/port %q", detail, want)
		}
	}
}

func TestWireGuardRequestTraceDoesNotBorrowAnotherDial(t *testing.T) {
	stack := &wireGuardStack{}
	transport := &wireGuardDiagnosticTransport{stack: stack}
	transport.roundTripper = acceptanceRoundTripper(func(request *http.Request) (*http.Response, error) {
		_, err := stack.DialContext(request.Context(), "tcp", "unconfigured.invalid:443")
		if err == nil {
			t.Fatal("expected controlled unconfigured resolver")
		}
		// A second request completes a dial before the first RoundTrip returns.
		stack.statsLock.Lock()
		stack.stats.DialAttemptSequence++
		stack.stats.ResolvedDialAttemptSequence = stack.stats.DialAttemptSequence
		stack.stats.DialAddr, stack.stats.DialPort = netip.MustParseAddr("192.0.2.2"), 443
		stack.statsLock.Unlock()
		return nil, err
	})
	request, err := http.NewRequest(http.MethodGet, "https://unconfigured.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = transport.RoundTrip(request)
	if err == nil || !strings.Contains(err.Error(), "target=unavailable") || strings.Contains(err.Error(), "target=192.0.2.2:443") {
		t.Fatalf("request borrowed a subsequent dial: %v", err)
	}
	// A body diagnostic can also be delayed until a different request dialed.
	attempt := new(atomic.Uint64)
	attempt.Store(1)
	bodyError := errors.New("controlled delayed body error")
	body := &wireGuardDiagnosticBody{ReadCloser: &acceptanceReadErrorBody{err: bodyError}, stack: stack, dialAttempt: attempt}
	_, err = body.Read(make([]byte, 1))
	if !errors.Is(err, bodyError) || !strings.Contains(err.Error(), "target=unavailable") {
		t.Fatalf("body borrowed a subsequent dial: %v", err)
	}
}

func TestWireGuardDNSExchangeDeadlineIsClassifiedAsTimeout(t *testing.T) {
	stackCtx, stopStack := context.WithCancel(context.Background())
	stack, err := newWireGuardStack(stackCtx, netip.MustParseAddr("10.0.0.2"), 1420, tuntest.NewChannelTUN())
	if err != nil {
		stopStack()
		t.Fatal(err)
	}
	defer func() { stopStack(); stack.Close() }()
	stack.dnsServer = netip.MustParseAddr("1.1.1.1")
	stack.resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		// The per-exchange deadline expires while the overall lookup remains
		// live, exactly the distinction that allowed raw EOF in the campaign.
		exchangeCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
		t.Cleanup(cancel)
		return stack.dialDNSContext(exchangeCtx, network, address)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = stack.DialContext(ctx, "tcp", "dropped-dns.invalid:443")
	var dnsError *net.DNSError
	if !errors.As(err, &dnsError) || !dnsError.IsTimeout || !errors.Is(err, context.DeadlineExceeded) || dnsError.Server != "1.1.1.1:53" {
		t.Fatalf("exchange deadline not classified as profile DNS timeout: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("overall lookup deadline fired instead of exchange deadline: %v", ctx.Err())
	}
}

// A successful literal-IP request leaves a TCP diagnostic target. The next
// request fails during controlled tunnel DNS resolution, before any TCP dial.
// Its error must not present the earlier origin as the current request target.
func TestWireGuardDNSFailureDoesNotReusePreviousRequestTarget(t *testing.T) {
	hostLookups := failAcceptanceHostDNS(t)
	transport, dnsUDP, _, originRequests := acceptanceDNSWireGuardPair(t, false, true)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, err := http.NewRequest(http.MethodGet, "http://1.1.1.1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Close = true
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", response.StatusCode)
	}
	before := transport.stack.packetStats()
	if before.DialAddr != netip.MustParseAddr("1.1.1.1") || before.DialPort != 80 {
		t.Fatal("positive control did not establish the old diagnostic target")
	}
	response, err = client.Get("http://acceptance-dns.invalid/")
	if response != nil {
		response.Body.Close()
	}
	var dnsError *net.DNSError
	if !errors.As(err, &dnsError) || !dnsError.IsNotFound {
		t.Fatalf("expected controlled NXDOMAIN, got %v", err)
	}
	if !strings.Contains(err.Error(), "target=unavailable") || strings.Contains(err.Error(), "target=1.1.1.1:80") {
		t.Fatalf("DNS-failed request retained previous origin: %v", err)
	}
	if strings.Contains(err.Error(), "tcp_recent=[") {
		t.Fatalf("DNS-failed request retained previous TCP history: %v", err)
	}
	after := transport.stack.packetStats()
	if after.DialAddr != before.DialAddr || after.DialPort != before.DialPort ||
		after.ResolvedDialAttemptSequence != before.ResolvedDialAttemptSequence ||
		after.DialAttemptSequence <= before.DialAttemptSequence {
		t.Fatal("DNS failure discarded active-flow accounting or did not begin a new diagnostic attempt")
	}
	if after.Outbound.Packets <= before.Outbound.Packets || after.Inbound.Packets <= before.Inbound.Packets {
		t.Fatal("request freshness discarded aggregate tunnel packet counts")
	}
	if hostLookups.Load() != 0 || dnsUDP.Load() == 0 || originRequests.Load() != 1 {
		t.Fatalf("host DNS=%d tunnel DNS=%d origin=%d", hostLookups.Load(), dnsUDP.Load(), originRequests.Load())
	}
}
