package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/proxy/internal/dnsudp"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Join the existing encrypted return and resolver/NAT controls. A real DNS
// query traverses client TUN, WireGuard UDP, owned provider submission, return
// NAT, WireGuard UDP and the original DNS socket. Only the provider is scripted.
// Read barriers select the ordering; no hosted service or scheduler sleep does.
func TestProxyDeviceWireGuardEncryptedDNSExchange(t *testing.T) {
	for _, test := range []struct {
		name, readOrder string
		answer          bool
	}{
		{"answer before read", "before read", true},
		{"delayed answer after read blocks", "read blocked", true},
		{"absent answer until canceled", "read blocked", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newWgReturnHarness(t)
			client, err := newWgClientStack(h.ctx, h.peer, 1420, h.clientTun)
			if err != nil {
				t.Fatal(err)
			}
			lookupCtx, cancelLookup := context.WithCancel(h.ctx)
			gates := make(chan proxyDNSReadGate, 1)
			result := make(chan proxyDNSResult, 1)
			done := make(chan struct{})
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
				var queue waiter.Queue
				endpoint, endpointErr := client.stack.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &queue)
				if endpointErr != nil {
					return nil, errors.New(endpointErr.String())
				}
				remote := tcpip.FullAddress{NIC: client.nicId, Addr: tcpip.AddrFrom4([4]byte{1, 1, 1, 1}), Port: 53}
				if endpointErr := endpoint.Connect(remote); endpointErr != nil {
					endpoint.Close()
					return nil, errors.New(endpointErr.String())
				}
				endpoint = &proxyDNSBarrierEndpoint{Endpoint: endpoint, ctx: ctx, order: test.readOrder, queue: &queue, gates: gates}
				connection := dnsudp.NewConn(&queue, endpoint)
				sockets.Add(1)
				wrapped := &proxyDNSConn{Conn: connection, ctx: ctx, joined: sockets.Done}
				wrapped.stop = context.AfterFunc(ctx, func() { connection.Close() })
				return wrapped, nil
			}}
			t.Cleanup(func() {
				socketLock.Lock()
				closed = true
				cancelLookup()
				socketLock.Unlock()
				h.close()
				client.CloseAndWait()
				<-done
				sockets.Wait()
			})
			beforeOuter := h.bind.payloadPackets.Load()
			go func() {
				defer close(done)
				ips, err := resolver.LookupIP(lookupCtx, "ip4", "wireguard-dns.invalid.")
				result <- proxyDNSResult{ips, err}
			}()
			var query []byte
			select {
			case query = <-h.provider:
			case <-h.ctx.Done():
				t.Fatal("encrypted DNS query did not reach provider submission")
			}
			if len(query) < 40 || query[9] != 17 || packetSource(query) != h.device.natAddr ||
				packetDestination(query).String() != "1.1.1.1" || binary.BigEndian.Uint16(query[22:24]) != 53 {
				t.Fatal("provider did not receive the NAT-owned UDP53 query")
			}
			answer := proxyDNSAnswer(t, query, false) // also validates the real A query
			var gate proxyDNSReadGate
			select {
			case gate = <-gates:
			case <-h.ctx.Done():
				t.Fatal("DNS socket did not reach its read barrier")
			}
			local, localErr := gate.endpoint.GetLocalAddress()
			if localErr != nil || local.Addr != tcpip.AddrFromSlice(h.peer.AsSlice()) || local.Port != binary.BigEndian.Uint16(query[20:22]) {
				t.Fatal("provider query lost the originating socket's port or source NAT")
			}
			if test.answer {
				// Observe endpoint admission independently of resolver completion.
				// This forces the reply through real encryption/decryption before
				// releasing either the first read or the already-registered waiter.
				entry, readable := waiter.NewChannelEntry(waiter.ReadableEvents)
				gate.queue.EventRegister(&entry)
				defer gate.queue.EventUnregister(&entry)
				packet := connect.MessagePoolCopy(wgNatUDPReply(query, answer))
				h.device.deliverReturnPackets([][]byte{packet})
				connect.MessagePoolReturn(packet)
				select {
				case <-readable:
				case <-h.ctx.Done():
					t.Fatal("provider answer did not reach the DNS endpoint through encrypted WireGuard")
				}
				if gate.endpoint.Readiness(waiter.ReadableEvents)&waiter.ReadableEvents == 0 {
					t.Fatal("DNS endpoint notification had no readable datagram")
				}
			} else {
				cancelLookup()
			}
			close(gate.release)
			var lookup proxyDNSResult
			select {
			case lookup = <-result:
			case <-h.ctx.Done():
				t.Fatal("DNS lookup did not finish after the controlled provider outcome")
			}
			if test.answer {
				if lookup.err != nil || len(lookup.ips) != 1 || !lookup.ips[0].Equal(net.IPv4(192, 0, 2, 9)) {
					t.Fatalf("encrypted provider answer was not resolved: %v", lookup.err)
				}
				if received := h.bind.payloadPackets.Load() - beforeOuter; received != 1 {
					t.Fatalf("encrypted answer datagrams = %d, want 1", received)
				}
			} else if !errors.Is(lookup.err, context.Canceled) || len(lookup.ips) != 0 || h.bind.payloadPackets.Load() != beforeOuter {
				t.Fatalf("provider silence invented a DNS reply or changed cancellation: %v", lookup.err)
			}
			if h.bind.receiveErrors.Load() != 0 {
				t.Fatal("local WireGuard UDP receive failed")
			}
			t.Logf("provider_query=%dB encrypted_return=%d DNS_answer=%t read_order=%q", len(query), h.bind.payloadPackets.Load()-beforeOuter, test.answer, test.readOrder)
		})
	}
}
