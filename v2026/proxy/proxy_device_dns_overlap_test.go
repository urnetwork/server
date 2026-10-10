package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/proxy/internal/dnsudp"
	"github.com/urnetwork/userwireguard/v2026/tun/tuntest"
	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Reproduce the observed packet shape without assuming its hosted origin:
// a real DNS socket is blocked when an unrelated, valid ICMP3/3 arrives in the
// same provider callback as a real HTTP response for the private Tun. The
// provider reply is controlled; no host DNS, sockets, services or sleeps are
// used. Passing proves local NAT/demux/socket behavior, not hosted delivery.
func TestProxyDeviceWireGuardDNSWithConcurrentTunReturn(t *testing.T) {
	for _, name := range []string{"retired DNS socket", "previous WireGuard stack", "wrong resolver", "matched refusal", "no DNS answer"} {
		t.Run(name, func(t *testing.T) {
			var retainedDevice *ProxyDevice
			var previousQuery []byte
			if name == "previous WireGuard stack" {
				retainedDevice = natTestDevice(t, true, natPeerAddr(t))
				previousQuery = proxyDNSRetiredStackQuery(t, retainedDevice)
			}
			h := newProxyDNSHarnessForDevice(t, retainedDevice, "read blocked")
			query := h.query()
			gate := proxyDNSWaitReadGate(t, h)
			quote := query
			switch name {
			case "previous WireGuard stack":
				quote = previousQuery
				if binary.BigEndian.Uint16(quote[20:22]) == binary.BigEndian.Uint16(query[20:22]) ||
					!bytes.Equal(quote[12:20], query[12:20]) || !bytes.Equal(quote[22:24], query[22:24]) {
					t.Fatal("the two peer generations must differ only in their UDP source port")
				}
			case "retired DNS socket", "no DNS answer":
				// Obtain a different, genuinely connected source port, then close
				// it before its error arrives. The pending DNS socket stays live.
				remote := tcpip.FullAddress{NIC: h.client.nicId, Addr: tcpip.AddrFrom4([4]byte{1, 1, 1, 1}), Port: 53}
				connection, err := dnsudp.Dial(h.client.stack, remote)
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				if _, err := connection.Write(query[28:]); err != nil {
					t.Fatal(err)
				}
				quote = h.query()
				if binary.BigEndian.Uint16(quote[20:22]) == binary.BigEndian.Uint16(query[20:22]) {
					t.Fatal("retired exchange reused the active DNS source port")
				}
				if err := connection.Close(); err != nil {
					t.Fatal(err)
				}
			case "wrong resolver":
				quote = bytes.Clone(query)
				if !connect.RewriteIpv4Destination(quote, netip.MustParseAddr("9.9.9.9")) {
					t.Fatal("construct different remote DNS tuple")
				}
			}

			httpPath := newProxyDNSHTTPHarness(t, h)
			var httpReturn []byte
			select {
			case httpReturn = <-httpPath.firstReturn:
			case <-h.ctx.Done():
				t.Fatal("HTTP response did not reach the controlled provider boundary")
			}
			if packetDestination(httpReturn) == h.device.natAddr {
				t.Fatal("private Tun and WireGuard unexpectedly share a return address")
			}
			returned := h.deliverBatch([][]byte{wgNatUDPTeardown(quote), httpReturn}, 1)[0]
			// Observe the real post-NAT packet, not only the synthetic input:
			// local envelope, 56-byte ICMP3/3, valid UDP quotation/checksums,
			// and an exact tuple comparison against the still-active socket.
			local, localErr := gate.endpoint.GetLocalAddress()
			remote, remoteErr := gate.endpoint.GetRemoteAddress()
			if localErr != nil || remoteErr != nil {
				t.Fatal("blocked DNS endpoint lost its addresses")
			}
			if len(returned) != 56 || returned[9] != 1 || returned[20] != 3 || returned[21] != 3 || returned[37] != 17 ||
				!bytes.Equal(returned[16:20], local.Addr.AsSlice()) || natTestChecksum(returned[:20]) != 0 ||
				natTestChecksum(returned[20:]) != 0 || natTestChecksum(returned[28:48]) != 0 {
				t.Fatal("return did not reproduce valid local ICMP3/3 with a UDP-header quote")
			}
			matched := bytes.Equal(returned[40:44], local.Addr.AsSlice()) && bytes.Equal(returned[44:48], remote.Addr.AsSlice()) &&
				binary.BigEndian.Uint16(returned[48:50]) == local.Port && binary.BigEndian.Uint16(returned[50:52]) == remote.Port
			if name == "previous WireGuard stack" &&
				(!bytes.Equal(returned[40:44], local.Addr.AsSlice()) || !bytes.Equal(returned[44:48], remote.Addr.AsSlice()) ||
					binary.BigEndian.Uint16(returned[48:50]) != binary.BigEndian.Uint16(previousQuery[20:22]) ||
					binary.BigEndian.Uint16(returned[50:52]) != remote.Port) {
				t.Fatal("previous-stack teardown did not preserve the exact source-port-only mismatch")
			}
			if matched != (name == "matched refusal") {
				t.Fatalf("observed active DNS tuple match = %t for %s", matched, name)
			}
			refused := gate.endpoint.Readiness(waiter.EventErr)&waiter.EventErr != 0
			if refused != (name == "matched refusal") {
				t.Fatalf("ICMP socket error = %t for %s", refused, name)
			}
			if gate.endpoint.Readiness(waiter.ReadableEvents)&waiter.ReadableEvents != 0 {
				t.Fatal("ICMP was incorrectly delivered as DNS datagram data")
			}
			close(httpPath.releaseReturn)
			httpPath.wantFirstResponse(t) // HTTP succeeds while DNS is still held.

			switch name {
			case "matched refusal":
				close(gate.release)
				proxyDNSWantRefused(t, h)
			case "no DNS answer":
				close(gate.release)
				h.cancelLookup()
				result := h.wait()
				if !errors.Is(result.err, context.Canceled) || len(result.ips) != 0 {
					t.Fatalf("unrelated ICMP invented a DNS outcome: %v", result.err)
				}
			default:
				h.deliver(wgNatUDPReply(query, proxyDNSAnswer(t, query, false)), true)
				close(gate.release)
				h.wantAnswer()
			}
			if err := httpPath.request(); err != nil {
				t.Fatalf("HTTP stopped after DNS completed: %v", err)
			}
		})
	}
}

// Produce real DNS egress through the same ProxyDevice/NAT, then close and
// join the entire old netstack before the fresh peer stack is constructed.
// Connect's fake-time reaper control separately exercises the real 120-second
// expiration/builder; here its delayed quotation traverses the real NAT,
// return demultiplexer and new gVisor socket alongside HTTP.
func proxyDNSRetiredStackQuery(t *testing.T, device *ProxyDevice) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tun := tuntest.NewChannelTUN()
	old, err := newWgClientStack(ctx, natPeerAddr(t), 1420, tun)
	if err != nil {
		t.Fatal(err)
	}
	defer old.CloseAndWait()
	var queue waiter.Queue
	endpoint, epErr := old.stack.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &queue)
	if epErr != nil {
		t.Fatal(epErr)
	}
	defer endpoint.Close()
	// Outside gVisor's ephemeral allocation range: a later automatically bound
	// socket cannot accidentally reuse this fixture's port.
	if err := endpoint.Bind(tcpip.FullAddress{Port: 1024}); err != nil {
		t.Fatal(err)
	}
	remote := tcpip.FullAddress{NIC: old.nicId, Addr: tcpip.AddrFrom4([4]byte{1, 1, 1, 1}), Port: 53}
	if err := endpoint.Connect(remote); err != nil {
		t.Fatal(err)
	}
	connection := dnsudp.NewConn(&queue, endpoint)
	defer connection.Close()
	question := dnsmessage.Message{Header: dnsmessage.Header{ID: 1, RecursionDesired: true}, Questions: []dnsmessage.Question{{
		Name: dnsmessage.MustNewName("previous-peer.invalid."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET,
	}}}
	query, err := question.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write(query); err != nil {
		t.Fatal(err)
	}
	device.ctx = ctx
	device.SetReceiveForAddress(natPeerAddr(t), make(chan []byte, 1))
	select {
	case packet := <-tun.Outbound:
		return (&proxyDNSHarness{t: t, device: device}).send(packet)
	case <-ctx.Done():
		t.Fatal("previous stack never emitted its DNS query")
		return nil
	}
}

func proxyDNSWaitReadGate(t *testing.T, h *proxyDNSHarness) proxyDNSReadGate {
	t.Helper()
	select {
	case gate := <-h.readGates:
		return gate
	case <-h.ctx.Done():
		t.Fatal("DNS read did not reach its registered-waiter boundary")
		return proxyDNSReadGate{}
	}
}

func proxyDNSWantRefused(t *testing.T, h *proxyDNSHarness) {
	t.Helper()
	// Go may consult another configured resolver after a refusal. Every
	// exchange still goes through the same isolated profile DNS endpoint.
	for exchanges := 0; exchanges < 32; exchanges++ {
		select {
		case result := <-h.result:
			var dnsErr *net.DNSError
			if !errors.As(result.err, &dnsErr) || dnsErr.IsTimeout || !strings.Contains(dnsErr.Err, (&tcpip.ErrConnectionRefused{}).String()) || len(result.ips) != 0 {
				t.Fatalf("matched ICMP did not wake DNS with a non-timeout refusal: %v", result.err)
			}
			return
		case packet := <-h.tun.Outbound:
			query := h.send(packet)
			gate := proxyDNSWaitReadGate(t, h)
			h.deliver(wgNatUDPTeardown(query), true)
			close(gate.release)
		case <-h.ctx.Done():
			t.Fatal("matched ICMP left DNS blocked")
		}
	}
	t.Fatal("unbounded DNS refusal exchanges")
}

type proxyDNSHTTPHarness struct {
	ctx           context.Context
	client        *http.Client
	firstReturn   chan []byte
	releaseReturn chan struct{}
	firstResult   chan error
}

func newProxyDNSHTTPHarness(t *testing.T, dns *proxyDNSHarness) *proxyDNSHTTPHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(dns.ctx)
	originTun := tuntest.NewChannelTUN()
	origin, err := newWgClientStack(ctx, netip.MustParseAddr("192.0.2.9"), 1420, originTun)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	listener, err := gonet.ListenTCP(origin.stack, tcpip.FullAddress{
		NIC: origin.nicId, Addr: tcpip.AddrFrom4([4]byte{192, 0, 2, 9}), Port: 80,
	}, ipv4.ProtocolNumber)
	if err != nil {
		cancel()
		origin.CloseAndWait()
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: dns.device.DialContext}
	h := &proxyDNSHTTPHarness{ctx: ctx, client: &http.Client{Transport: transport},
		firstReturn: make(chan []byte, 1), releaseReturn: make(chan struct{}), firstResult: make(chan error, 1)}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "isolated HTTP survived DNS")
	})}
	var workers sync.WaitGroup
	workers.Add(4)
	go func() {
		defer workers.Done()
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("isolated HTTP server: %v", err)
		}
	}()
	go func() {
		defer workers.Done()
		packets := make([][]byte, 8)
		for {
			n, err := dns.device.tun.ReadBatch(packets)
			if err != nil {
				return
			}
			for _, packet := range packets[:n] {
				inbound := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(bytes.Clone(packet))})
				if !connect.MessagePoolReturn(packet) {
					t.Error("private Tun egress lost its pool owner")
				}
				origin.endpoint.InjectInbound(header.IPv4ProtocolNumber, inbound)
				inbound.DecRef()
			}
		}
	}()
	go func() {
		defer workers.Done()
		held := false
		for {
			select {
			case <-ctx.Done():
				return
			case packet := <-originTun.Outbound:
				ipSize := int(packet[0]&15) * 4
				if !held && packet[9] == 6 && len(packet) > ipSize+int(packet[ipSize+12]>>4)*4 {
					held = true
					h.firstReturn <- packet
					select {
					case <-h.releaseReturn:
					case <-ctx.Done():
						return
					}
					continue // the test injects this held packet in its mixed batch
				}
				owned := connect.MessagePoolCopy(packet)
				dns.device.deliverReturnPackets([][]byte{owned})
				if !connect.MessagePoolReturn(owned) {
					t.Error("private Tun return lost its pool owner")
				}
			}
		}
	}()
	go func() { defer workers.Done(); h.firstResult <- h.request() }()
	t.Cleanup(func() {
		cancel()
		transport.CloseIdleConnections()
		server.Close()
		dns.device.tun.Close() // release the ReadBatch bridge before joining
		origin.CloseAndWait()
		workers.Wait()
	})
	return h
}

func (h *proxyDNSHTTPHarness) request() error {
	request, err := http.NewRequestWithContext(h.ctx, http.MethodGet, "http://192.0.2.9/", nil)
	if err != nil {
		return err
	}
	response, err := h.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || string(body) != "isolated HTTP survived DNS" {
		return fmt.Errorf("HTTP status/body mismatch: %d %q", response.StatusCode, body)
	}
	return nil
}

func (h *proxyDNSHTTPHarness) wantFirstResponse(t *testing.T) {
	t.Helper()
	select {
	case err := <-h.firstResult:
		if err != nil {
			t.Fatalf("concurrent HTTP response failed: %v", err)
		}
	case <-h.ctx.Done():
		t.Fatal("concurrent HTTP response remained blocked")
	}
}
