package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
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
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Close the seam between the encrypted acceptance DNS tests and the proxy's
// UDP socket/NAT tests: these are genuine net.Resolver DNS messages, carried
// by gVisor and the real ProxyDevice owned-send/return-demultiplexing methods.
// The provider response is synthetic; no host socket, DNS or service is used.
func TestProxyDeviceWireGuardDNSExchange(t *testing.T) {
	t.Run("A answer", func(t *testing.T) {
		h := newProxyDNSHarness(t)
		query := h.query()
		h.deliver(wgNatUDPReply(query, proxyDNSAnswer(t, query, false)), true)
		h.wantAnswer()
	})
	for _, order := range []string{"before read", "read blocked"} {
		t.Run("ICMP refusal "+order, func(t *testing.T) {
			h := newProxyDNSHarness(t, order)
			// net.Resolver may retry a refused exchange using another configured
			// resolver. Every Dial still goes solely to this isolated UDP socket.
			// The bound is a safety guard, not a test-controlled retry policy.
			for queries := 0; queries < 32; queries++ {
				select {
				case result := <-h.result:
					var dnsErr *net.DNSError
					if !errors.As(result.err, &dnsErr) || dnsErr.IsTimeout || !strings.Contains(dnsErr.Err, (&tcpip.ErrConnectionRefused{}).String()) || len(result.ips) != 0 {
						t.Fatalf("refused DNS exchange was not reported as a non-timeout refusal: %v", result.err)
					}
					return
				case packet := <-h.tun.Outbound:
					query := h.send(packet)
					var gate proxyDNSReadGate
					select {
					case gate = <-h.readGates:
					case <-h.ctx.Done():
						t.Fatal("DNS read did not reach its controlled boundary")
					}
					h.deliver(wgNatUDPTeardown(query), true)
					if gate.endpoint.Readiness(waiter.EventErr)&waiter.EventErr == 0 {
						t.Fatal("valid rewritten ICMP did not reach the connected UDP endpoint")
					}
					t.Logf("ICMP accepted by UDP: read_order=%q wait_mask=%#x error_mask=%#x", order, gate.queue.Events(), waiter.EventErr)
					close(gate.release)
				case <-h.ctx.Done():
					t.Fatal("DNS refusal did not finish before the safety deadline")
				}
			}
			t.Fatal("unbounded DNS refusal exchanges")
		})
	}
	t.Run("canceled without response", func(t *testing.T) {
		h := newProxyDNSHarness(t)
		_ = h.query() // the exchange really reached the provider boundary
		h.cancelLookup()
		result := h.wait()
		if !errors.Is(result.err, context.Canceled) || len(result.ips) != 0 {
			t.Fatalf("canceled DNS exchange = %v, want context cancellation", result.err)
		}
	})
	for _, name := range []string{"foreign destination", "wrong quoted owner", "short ICMP quote", "wrong DNS transaction", "invalid UDP checksum"} {
		t.Run(name, func(t *testing.T) {
			h := newProxyDNSHarness(t)
			query := h.query()
			packet := wgNatUDPReply(query, proxyDNSAnswer(t, query, false))
			wantHandoff := false
			switch name {
			case "foreign destination":
				if !connect.RewriteIpv4Destination(packet, netip.MustParseAddr("192.0.2.77")) {
					t.Fatal("construct foreign-address response")
				}
			case "wrong quoted owner":
				packet = wgNatUDPTeardown(query)
				copy(packet[40:44], []byte{192, 0, 2, 77})
				binary.BigEndian.PutUint16(packet[38:40], 0)
				binary.BigEndian.PutUint16(packet[38:40], natTestChecksum(packet[28:48]))
				binary.BigEndian.PutUint16(packet[22:24], 0)
				binary.BigEndian.PutUint16(packet[22:24], natTestChecksum(packet[20:]))
			case "short ICMP quote":
				packet = wgNatUDPTeardown(query)[:52]
				binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
				binary.BigEndian.PutUint16(packet[10:12], 0)
				binary.BigEndian.PutUint16(packet[10:12], natTestChecksum(packet[:20]))
				binary.BigEndian.PutUint16(packet[22:24], 0)
				binary.BigEndian.PutUint16(packet[22:24], natTestChecksum(packet[20:]))
			case "wrong DNS transaction":
				packet = wgNatUDPReply(query, proxyDNSAnswer(t, query, true))
				wantHandoff = true // the DNS parser, not the proxy, rejects it
			case "invalid UDP checksum":
				packet[len(packet)-1] ^= 1
				wantHandoff = true // the UDP stack, not the proxy, rejects it
			}
			h.deliver(packet, wantHandoff)
			// A valid response for the same pending query must still win. No
			// sleep or "not done yet" timing assertion is needed for rejection.
			h.deliver(wgNatUDPReply(query, proxyDNSAnswer(t, query, false)), true)
			h.wantAnswer()
		})
	}
}

type proxyDNSResult struct {
	ips []net.IP
	err error
}

type proxyDNSHarness struct {
	t            *testing.T
	ctx          context.Context
	cancelLookup context.CancelFunc
	client       *wgClientStack
	tun          *tuntest.ChannelTUN
	device       *ProxyDevice
	receive      chan []byte
	result       chan proxyDNSResult
	readGates    chan proxyDNSReadGate
}

func newProxyDNSHarness(t *testing.T, readOrder ...string) *proxyDNSHarness {
	t.Helper()
	return newProxyDNSHarnessForDevice(t, nil, readOrder...)
}

func newProxyDNSHarnessForDevice(t *testing.T, device *ProxyDevice, readOrder ...string) *proxyDNSHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	peer := natPeerAddr(t)
	tun := tuntest.NewChannelTUN()
	client, err := newWgClientStack(ctx, peer, 1420, tun)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if device == nil {
		device = natTestDevice(t, true, peer)
	}
	device.ctx = ctx
	device.tun, err = connect.CreateTunWithDefaults(ctx)
	if err != nil {
		cancel()
		client.CloseAndWait()
		t.Fatal(err)
	}
	receive := make(chan []byte, 1)
	device.SetReceiveForAddress(peer, receive)
	lookupCtx, cancelLookup := context.WithCancel(ctx)
	h := &proxyDNSHarness{t: t, ctx: ctx, cancelLookup: cancelLookup, client: client, tun: tun,
		device: device, receive: receive, result: make(chan proxyDNSResult, 1), readGates: make(chan proxyDNSReadGate, 1)}
	var sockets sync.WaitGroup
	var socketLock sync.Mutex
	closed := false
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		socketLock.Lock()
		defer socketLock.Unlock()
		if closed || ctx.Err() != nil {
			return nil, context.Canceled
		}
		if network != "udp" && network != "udp4" {
			return nil, fmt.Errorf("unexpected DNS transport %s", network)
		}
		remote := tcpip.FullAddress{NIC: client.nicId, Addr: tcpip.AddrFrom4([4]byte{1, 1, 1, 1}), Port: 53}
		var queue waiter.Queue
		endpoint, endpointErr := client.stack.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &queue)
		if endpointErr != nil {
			return nil, errors.New(endpointErr.String())
		}
		if endpointErr := endpoint.Connect(remote); endpointErr != nil {
			endpoint.Close()
			return nil, errors.New(endpointErr.String())
		}
		if len(readOrder) != 0 {
			endpoint = &proxyDNSBarrierEndpoint{Endpoint: endpoint, ctx: ctx, order: readOrder[0], queue: &queue, gates: h.readGates}
		}
		connection := dnsudp.NewConn(&queue, endpoint)
		sockets.Add(1)
		wrapped := &proxyDNSConn{Conn: connection, ctx: ctx, joined: sockets.Done}
		wrapped.stop = context.AfterFunc(ctx, func() { connection.Close() })
		return wrapped, nil
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ips, err := resolver.LookupIP(lookupCtx, "ip4", "wireguard-dns.invalid.")
		h.result <- proxyDNSResult{ips, err}
	}()
	t.Cleanup(func() {
		socketLock.Lock()
		closed = true
		cancelLookup()
		cancel()
		socketLock.Unlock()
		client.CloseAndWait()
		device.tun.Close()
		<-done
		sockets.Wait() // Close from the DNS exchange joins each accepted Dial
	})
	return h
}

type proxyDNSReadGate struct {
	endpoint tcpip.Endpoint
	queue    *waiter.Queue
	release  chan struct{}
}

type proxyDNSBarrierEndpoint struct {
	tcpip.Endpoint
	ctx   context.Context
	order string
	queue *waiter.Queue
	gates chan<- proxyDNSReadGate
	reads int
}

func (e *proxyDNSBarrierEndpoint) pause() {
	gate := proxyDNSReadGate{e.Endpoint, e.queue, make(chan struct{})}
	select {
	case e.gates <- gate:
	case <-e.ctx.Done():
		return
	}
	select {
	case <-gate.release:
	case <-e.ctx.Done():
	}
}

func (e *proxyDNSBarrierEndpoint) Read(dst io.Writer, opts tcpip.ReadOptions) (tcpip.ReadResult, tcpip.Error) {
	e.reads++
	if e.order == "before read" && e.reads == 1 {
		e.pause()
	}
	result, err := e.Endpoint.Read(dst, opts)
	if _, blocked := err.(*tcpip.ErrWouldBlock); blocked && e.order == "read blocked" && e.reads == 2 {
		// commonRead has registered its waiter and repeated Read. Hold its
		// return so the error must arrive after that read but before waiting.
		e.pause()
	}
	return result, err
}

// Preserve net.PacketConn for the resolver's datagram framing. Cancellation
// mirrors acceptance's DNS adapter; the production adapter has separate tests.
type proxyDNSConn struct {
	*dnsudp.Conn
	ctx    context.Context
	stop   func() bool
	joined func()
	once   sync.Once
}

func (c *proxyDNSConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil && c.ctx.Err() != nil {
		err = c.ctx.Err()
	}
	return n, err
}

func (c *proxyDNSConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.stop(); c.joined() })
	return err
}

func (h *proxyDNSHarness) query() []byte {
	h.t.Helper()
	select {
	case packet := <-h.tun.Outbound:
		return h.send(packet)
	case result := <-h.result:
		h.t.Fatalf("lookup ended before provider received query: %v", result.err)
	case <-h.ctx.Done():
		h.t.Fatal("no DNS query before safety deadline")
	}
	return nil
}

func (h *proxyDNSHarness) send(packet []byte) []byte {
	h.t.Helper()
	before := bytes.Clone(packet)
	var providerPacket []byte
	h.device.sendOwnedPacketForTest = func(owned []byte) bool {
		providerPacket = bytes.Clone(owned)
		if !connect.MessagePoolReturn(owned) {
			h.t.Fatal("provider did not receive a single packet owner")
		}
		return true
	}
	if !h.device.Send(packet) || !bytes.Equal(packet, before) || packetSource(providerPacket) != h.device.natAddr {
		h.t.Fatal("DNS query lost borrowed ownership or source NAT")
	}
	if len(providerPacket) < 40 || providerPacket[9] != 17 || binary.BigEndian.Uint16(providerPacket[22:24]) != 53 {
		h.t.Fatal("resolver did not produce a UDP53 query")
	}
	return providerPacket
}

func (h *proxyDNSHarness) deliver(packet []byte, wantHandoff bool) {
	h.t.Helper()
	want := 0
	if wantHandoff {
		want = 1
	}
	h.deliverBatch([][]byte{packet}, want)
}

func (h *proxyDNSHarness) deliverBatch(packets [][]byte, wantHandoffs int) [][]byte {
	h.t.Helper()
	owned := make([][]byte, len(packets))
	for i, packet := range packets {
		owned[i] = connect.MessagePoolCopy(packet)
	}
	defer func() {
		for _, packet := range owned {
			if !connect.MessagePoolReturn(packet) {
				h.t.Error("return callback lost its final packet owner")
			}
		}
	}()
	h.device.deliverReturnPackets(owned)
	handed := 0
	var delivered [][]byte
	for {
		select {
		case shared := <-h.receive:
			handed++
			copy := bytes.Clone(shared)
			delivered = append(delivered, copy)
			if connect.MessagePoolReturn(shared) {
				h.t.Fatal("WireGuard handoff released the callback's owner")
			}
			inbound := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(copy)})
			h.client.endpoint.InjectInbound(header.IPv4ProtocolNumber, inbound)
			inbound.DecRef()
		default:
			if handed != wantHandoffs {
				h.t.Fatalf("WireGuard return handoffs = %d, want %d", handed, wantHandoffs)
			}
			return delivered
		}
	}
}

func (h *proxyDNSHarness) wait() proxyDNSResult {
	h.t.Helper()
	select {
	case result := <-h.result:
		return result
	case <-h.ctx.Done():
		h.t.Fatal("DNS exchange did not finish before safety deadline")
	}
	return proxyDNSResult{}
}

func (h *proxyDNSHarness) wantAnswer() {
	h.t.Helper()
	result := h.wait()
	if result.err != nil || len(result.ips) != 1 || !result.ips[0].Equal(net.IPv4(192, 0, 2, 9)) {
		h.t.Fatalf("valid DNS reply was not accepted: %v", result.err)
	}
}

func proxyDNSAnswer(t *testing.T, query []byte, wrongID bool) []byte {
	t.Helper()
	var message dnsmessage.Message
	if err := message.Unpack(query[28:]); err != nil || len(message.Questions) != 1 || message.Questions[0].Type != dnsmessage.TypeA {
		t.Fatal("provider did not receive a genuine DNS A query")
	}
	message.Header = dnsmessage.Header{ID: message.ID, Response: true, RecursionDesired: true, RecursionAvailable: true}
	if wrongID {
		message.ID++
	}
	message.Additionals = nil
	message.Answers = []dnsmessage.Resource{{
		Header: dnsmessage.ResourceHeader{Name: message.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
		Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 9}},
	}}
	answer, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return answer
}
