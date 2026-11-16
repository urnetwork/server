package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	wgproxy "github.com/urnetwork/proxy/v2026"
	"github.com/urnetwork/userwireguard/v2026/conn"
	uwgdevice "github.com/urnetwork/userwireguard/v2026/device"
	"github.com/urnetwork/userwireguard/v2026/logger"
	"github.com/urnetwork/userwireguard/v2026/tun/tuntest"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// These payload lengths match the sequence holes in the retained MAIN failures:
// the ClientHello was cumulatively ACKed, but the later FIN was 5178/3875 bytes
// beyond the next expected return byte. They are synthetic packet bytes, not a
// TLS implementation or evidence about which hosted boundary lost those bytes.
// Starting at the provider callback, pin the actual NAT, shared return ownership,
// WgProxy read, encryption, loopback UDP receive and decryption boundaries.
func TestProxyDeviceWireGuardTLSReturnBulk(t *testing.T) {
	for _, test := range []struct {
		name       string
		payload    int
		timestamps bool
	}{
		{"timestamp_options", 5178, true},
		{"plain_tcp", 3875, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			beforeTaken, beforeReturned, _ := connect.MessagePoolCounts()
			h := newWgReturnHarness(t)
			defer func() {
				h.close()
				afterTaken, afterReturned, _ := connect.MessagePoolCounts()
				if afterTaken-beforeTaken != afterReturned-beforeReturned {
					t.Errorf("pooled return ownership: taken=%d returned=%d", afterTaken-beforeTaken, afterReturned-beforeReturned)
				}
			}()
			packets := wgReturnFlight(t, h.device.natAddr, test.payload, test.timestamps)
			expected := wgReturnFlight(t, h.peer, test.payload, test.timestamps)
			beforeOuter := h.bind.dataPackets.Load()
			beforeLarge := h.bind.largePackets.Load()
			for index, packet := range packets {
				packets[index] = connect.MessagePoolCopy(packet)
			}
			h.device.deliverReturnPackets(packets)
			for _, packet := range packets {
				connect.MessagePoolReturn(packet) // callback's original owner
			}
			payloadBytes := 0
			for index, want := range expected {
				select {
				case got := <-h.clientTun.Inbound:
					if !bytes.Equal(got, want) {
						t.Fatalf("return packet %d changed or reordered: got=%dB want=%dB", index, len(got), len(want))
					}
					assertWgChecksums(t, got)
					payloadBytes += len(got) - 20 - int(got[32]>>4)*4
				case <-h.ctx.Done():
					t.Fatalf("missing return packet %d/%d: %v", index, len(expected), h.ctx.Err())
				}
			}
			if payloadBytes != test.payload || h.bind.dataPackets.Load()-beforeOuter < uint64(len(expected)) || h.bind.largePackets.Load()-beforeLarge != uint64(len(expected)-2) {
				t.Fatalf("return lineage incomplete: payload=%d outer=%d bulk_outer=%d", payloadBytes, h.bind.dataPackets.Load()-beforeOuter, h.bind.largePackets.Load()-beforeLarge)
			}
			if h.bind.receiveErrors.Load() != 0 || h.wg.RuntimeStats() != (wgproxy.WgRuntimeStats{}) {
				t.Fatal("unloaded WireGuard path had a socket failure or receive refusal")
			}
			t.Logf("provider_callback=%d packets NAT_restored=%dB bulk_outer=%d client_payload=%dB FIN_gap=%dB exact=true", len(expected), test.payload, h.bind.largePackets.Load()-beforeLarge, payloadBytes, test.payload)
		})
	}
}

// A full queue must not turn an otherwise complete bulk response into a
// control-only stream. Force the first handoff to wait, then drain the exact
// finite batch. No elapsed-time assertion or scheduler sleep selects the race.
func TestProxyDeviceWireGuardTLSReturnBatchBackpressure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer := natPeerAddr(t)
	device := natTestDevice(t, true, peer)
	device.ctx = ctx
	receive := make(chan []byte, 1)
	device.SetReceiveForAddress(peer, receive)
	receive <- nil // owns the only queue slot until the explicit barrier
	reached := make(chan struct{})
	var once sync.Once
	device.receiveBackpressureForTest = func() { once.Do(func() { close(reached) }) }
	packets := wgReturnFlight(t, device.natAddr, 5178, true)
	want := wgReturnFlight(t, peer, 5178, true)
	beforeTaken, beforeReturned, _ := connect.MessagePoolCounts()
	for index, packet := range packets {
		packets[index] = connect.MessagePoolCopy(packet)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		device.deliverReturnPackets(packets)
		for _, packet := range packets {
			connect.MessagePoolReturn(packet)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		for len(receive) != 0 {
			connect.MessagePoolReturn(<-receive)
		}
	})
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal("return did not reach the full-queue boundary")
	}
	<-receive
	for index, expected := range want {
		select {
		case packet := <-receive:
			matched := bytes.Equal(packet, expected)
			connect.MessagePoolReturn(packet)
			if !matched {
				t.Fatalf("backpressured packet %d changed or reordered", index)
			}
		case <-ctx.Done():
			t.Fatalf("backpressure lost bulk packet %d", index)
		}
	}
	<-done
	afterTaken, afterReturned, _ := connect.MessagePoolCounts()
	if afterTaken-beforeTaken != afterReturned-beforeReturned {
		t.Fatal("backpressured batch leaked a callback or shared owner")
	}
}

type wgReturnHarness struct {
	ctx       context.Context
	peer      netip.Addr
	device    *ProxyDevice
	wg        *wgproxy.WgProxy
	clientTun *tuntest.ChannelTUN
	bind      *wgReturnTrackingBind
	provider  <-chan []byte
	close     func()
}

func newWgReturnHarness(t *testing.T) *wgReturnHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	peer := natPeerAddr(t)
	device := natTestDevice(t, true, peer)
	device.ctx, device.cancel = ctx, cancel
	device.deviceState = newProxyDeviceReadinessTestSource(true)
	provider := make(chan []byte, 1)
	device.sendOwnedPacketForTest = func(packet []byte) bool {
		select {
		case provider <- bytes.Clone(packet):
			connect.MessagePoolReturn(packet)
			return true
		case <-ctx.Done():
			return false
		}
	}
	device.sendOwnedPacketsForTest = func(packets [][]byte) int {
		for index, packet := range packets {
			if !device.sendOwnedPacketForTest(packet) {
				return index
			}
		}
		return len(packets)
	}
	serverPrivate, serverPublic, err := wgproxy.WgGenKeyPairStrings()
	if err != nil {
		t.Fatal(err)
	}
	clientPrivate, clientPublic, err := wgproxy.WgGenKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	settings := wgproxy.DefaultWgProxySettings()
	settings.PrivateKey, settings.Log = serverPrivate, connect.NewNoopLogger()
	wg := wgproxy.NewWgProxy(ctx, settings)
	t.Cleanup(func() { _ = wg.Close() })
	if err := wg.SetClients(map[netip.Addr]*wgproxy.WgClient{
		peer: {PublicKey: clientPublic.String(), ClientIpv4: peer, Tun: func() (wgproxy.WgTun, error) { return device, nil }},
	}); err != nil {
		t.Fatal(err)
	}
	// WgProxy's public listener does not expose an ephemeral bound port. Reserve
	// one locally, release it, and fail if ListenAndServe cannot acquire it.
	reservation, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.LocalAddr().(*net.UDPAddr).Port
	_ = reservation.Close()
	serverDone := make(chan struct{})
	var serverErr error // published by closing serverDone
	go func() {
		serverErr = wg.ListenAndServe("127.0.0.1", "::1", port)
		close(serverDone)
	}()
	var joinOnce sync.Once
	joinServer := func() {
		joinOnce.Do(func() {
			_ = wg.Close()
			<-serverDone
			if serverErr != nil {
				t.Errorf("WG listener shutdown: %v", serverErr)
			}
		})
	}
	t.Cleanup(joinServer)
	clientTun := tuntest.NewChannelTUN()
	bind := &wgReturnTrackingBind{Bind: conn.NewDefaultBind()}
	clientDevice := uwgdevice.NewDevice(clientTun.TUN(), bind, logger.NewLogger(logger.LogLevelSilent, ""))
	t.Cleanup(clientDevice.Close)
	serverKey, err := wgtypes.ParseKey(serverPublic)
	if err != nil {
		t.Fatal(err)
	}
	zeroPort := 0
	if err := clientDevice.IpcSet(&wgtypes.Config{
		PrivateKey: &clientPrivate, ListenPort: &zeroPort, ReplacePeers: true,
		Peers: []wgtypes.PeerConfig{{PublicKey: serverKey,
			Endpoint: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}, ReplaceAllowedIPs: true,
			AllowedIPs: []net.IPNet{{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := clientDevice.Up(); err != nil {
		t.Fatal(err)
	}
	primer := buildWgTestPacket(t, peer.String(), "192.0.2.1", "client-owned primer")
	select {
	case clientTun.Outbound <- primer:
	case <-serverDone:
		t.Fatalf("WG listener failed: %v", serverErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case packet := <-provider:
		if packetSource(packet) != device.natAddr || !connect.RewriteIpv4Source(packet, peer) || !bytes.Equal(packet, primer) {
			t.Fatal("primer did not establish the real WG/NAT attachment")
		}
	case <-serverDone:
		t.Fatalf("WG listener failed: %v", serverErr)
	case <-ctx.Done():
		t.Fatal("WG primer did not reach provider boundary")
	}
	var closeOnce sync.Once
	closeHarness := func() {
		closeOnce.Do(func() {
			cancel()
			clientDevice.Close()
			joinServer()
		})
	}
	t.Cleanup(closeHarness)
	return &wgReturnHarness{ctx, peer, device, wg, clientTun, bind, provider, closeHarness}
}

// Count datagrams at the actual local Bind receive boundary, before decrypt or
// stack injection. Fixed atomics retain neither payloads nor peer identities.
type wgReturnTrackingBind struct {
	conn.Bind
	dataPackets    atomic.Uint64
	payloadPackets atomic.Uint64
	largePackets   atomic.Uint64
	receiveErrors  atomic.Uint64
}

func (b *wgReturnTrackingBind) Open(ip4, ip6 string, port uint16) ([]conn.ReceiveFunc, uint16, error) {
	receivers, actual, err := b.Bind.Open(ip4, ip6, port)
	for index, receive := range receivers {
		receivers[index] = func(packets [][]byte, sizes []int, endpoints []conn.Endpoint) (int, error) {
			n, err := receive(packets, sizes, endpoints)
			if err != nil && !errors.Is(err, net.ErrClosed) {
				b.receiveErrors.Add(1)
			}
			for index := range n {
				if sizes[index] >= 4 && binary.LittleEndian.Uint32(packets[index][:4]) == uwgdevice.MessageTransportType {
					b.dataPackets.Add(1)
					if sizes[index] > uwgdevice.MessageKeepaliveSize {
						b.payloadPackets.Add(1)
					}
					if sizes[index] > 200 {
						b.largePackets.Add(1)
					}
				}
			}
			return n, err
		}
	}
	return receivers, actual, err
}

func wgReturnFlight(t *testing.T, destination netip.Addr, payload int, timestamps bool) [][]byte {
	t.Helper()
	const firstSequence = uint32(0x10000)
	const ackedClientHello = uint32(0x20000 + 1521)
	tcpSize := 20
	if timestamps {
		tcpSize += 12
	}
	packet := func(sequence uint32, body []byte, flags byte) []byte {
		p := buildWgTestPacket(t, "192.0.2.1", destination.String(), string(make([]byte, tcpSize-20+len(body))))
		tcp := p[20:]
		binary.BigEndian.PutUint16(tcp[0:2], 443)
		binary.BigEndian.PutUint16(tcp[2:4], 23500)
		binary.BigEndian.PutUint32(tcp[4:8], sequence)
		binary.BigEndian.PutUint32(tcp[8:12], ackedClientHello)
		tcp[12], tcp[13] = byte(tcpSize/4)<<4, flags
		if timestamps {
			copy(tcp[20:32], []byte{1, 1, 8, 10, 0, 0, 0, 7, 0, 0, 0, 9})
		}
		copy(tcp[tcpSize:], body)
		binary.BigEndian.PutUint16(tcp[16:18], tcpPseudoChecksum(p))
		return p
	}
	packets := [][]byte{packet(firstSequence, nil, 0x10)}
	for offset := 0; offset < payload; {
		n := min(1420-20-tcpSize, payload-offset)
		body := make([]byte, n)
		for index := range body {
			body[index] = byte((offset + index) % 251)
		}
		packets = append(packets, packet(firstSequence+uint32(offset), body, 0x18))
		offset += n
	}
	return append(packets, packet(firstSequence+uint32(payload), nil, 0x11))
}
