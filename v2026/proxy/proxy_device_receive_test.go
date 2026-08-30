package proxy

import (
	"bytes"
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func ipv4PacketForDestination(destination netip.Addr) []byte {
	packet := make([]byte, 20)
	packet[0] = 4 << 4
	copy(packet[16:20], destination.AsSlice())
	return packet
}

func ipv6PacketForDestination(destination netip.Addr) []byte {
	packet := make([]byte, 40)
	packet[0] = 6 << 4
	copy(packet[24:40], destination.AsSlice())
	return packet
}

// One DeviceLocal backs all three public proxy protocols. Return routing must
// therefore be a per-packet decision, not mutable device-wide mode: otherwise
// an HTTP/SOCKS dial concurrent with WireGuard sends one path's packets into the
// other's stack and both appear healthy while requests time out.
func TestProxyPacketMatchesReceiveAddress(t *testing.T) {
	wg4 := netip.MustParseAddr("10.23.45.67")
	tun4 := netip.MustParseAddr("169.254.9.10")
	wg6 := netip.MustParseAddr("2001:db8::67")
	tun6 := netip.MustParseAddr("fd00::10")

	tests := []struct {
		name    string
		packet  []byte
		address netip.Addr
		want    bool
	}{
		{name: "wireguard ipv4", packet: ipv4PacketForDestination(wg4), address: wg4, want: true},
		{name: "tun ipv4", packet: ipv4PacketForDestination(tun4), address: wg4, want: false},
		{name: "wireguard ipv6", packet: ipv6PacketForDestination(wg6), address: wg6, want: true},
		{name: "tun ipv6", packet: ipv6PacketForDestination(tun6), address: wg6, want: false},
		{name: "wrong family", packet: ipv6PacketForDestination(wg6), address: wg4, want: false},
		{name: "short ipv4", packet: []byte{4 << 4}, address: wg4, want: false},
		{name: "short ipv6", packet: []byte{6 << 4}, address: wg6, want: false},
		{name: "unknown version", packet: []byte{7 << 4}, address: wg4, want: false},
		{name: "legacy all packets", packet: ipv4PacketForDestination(tun4), want: true},
	}
	for _, test := range tests {
		if got := proxyPacketMatchesReceiveAddress(test.packet, test.address); got != test.want {
			t.Errorf("%s: proxyPacketMatchesReceiveAddress() = %t, want %t", test.name, got, test.want)
		}
	}
}

// A full process WireGuard queue used to make the callback silently discard an
// inner TCP segment. Provider NAT cannot replay that device-side segment, so a
// single full-queue instant permanently stalled the flow behind the sequence
// hole even after the queue drained.
func TestProxyDeviceWireGuardReturnWaitsForQueueCapacity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receive := make(chan []byte, 1)
	receive <- []byte("occupying packet")
	receiveMonitor := connect.NewMonitor()
	proxyDevice := &ProxyDevice{
		ctx:            ctx,
		receiveMonitor: receiveMonitor,
	}
	proxyDevice.SetReceiveForAddress(netip.MustParseAddr("10.33.44.55"), receive)
	_, _, receiveNotify := proxyDevice.receiveWithNotify()

	backpressureReached := make(chan struct{})
	releaseBackpressure := make(chan struct{})
	proxyDevice.receiveBackpressureForTest = func() {
		close(backpressureReached)
		<-releaseBackpressure
	}
	packet := connect.MessagePoolCopy([]byte("lossless inner TCP return"))
	deliveryDone := make(chan bool, 1)
	go func() {
		deliveryDone <- proxyDevice.deliverWireGuardReturn(receive, receiveNotify, packet)
	}()

	<-backpressureReached
	<-receive
	close(releaseBackpressure)
	if delivered := <-deliveryDone; !delivered {
		t.Fatal("full-queue return was discarded instead of delivered after capacity returned")
	}
	deliveredPacket := <-receive
	if !bytes.Equal(deliveredPacket, packet) {
		t.Fatalf("delivered packet = %q, want %q", deliveredPacket, packet)
	}
	if connect.MessagePoolReturn(deliveredPacket) {
		t.Fatal("returning the shared delivery released the callback's original owner")
	}
	if !connect.MessagePoolReturn(packet) {
		t.Fatal("callback packet did not retain its original message-pool owner")
	}
}

func TestProxyDeviceWireGuardBackpressureEndsWithAttachment(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receive := make(chan []byte, 1)
	receive <- []byte("occupying packet")
	proxyDevice := &ProxyDevice{
		ctx:            ctx,
		receiveMonitor: connect.NewMonitor(),
	}
	address := netip.MustParseAddr("10.33.44.55")
	proxyDevice.SetReceiveForAddress(address, receive)
	_, _, receiveNotify := proxyDevice.receiveWithNotify()

	backpressureReached := make(chan struct{})
	releaseBackpressure := make(chan struct{})
	proxyDevice.receiveBackpressureForTest = func() {
		close(backpressureReached)
		<-releaseBackpressure
	}
	packet := connect.MessagePoolCopy([]byte("removed peer return"))
	deliveryDone := make(chan bool, 1)
	go func() {
		deliveryDone <- proxyDevice.deliverWireGuardReturn(receive, receiveNotify, packet)
	}()

	<-backpressureReached
	proxyDevice.SetReceiveForAddress(address, nil)
	close(releaseBackpressure)
	if delivered := <-deliveryDone; delivered {
		t.Fatal("return was delivered after the WireGuard attachment was removed")
	}
	if !connect.MessagePoolReturn(packet) {
		t.Fatal("callback packet did not retain its original message-pool owner")
	}
}

// This forces the production overlap ordering: WireGuard attaches its return
// channel, then an HTTP/SOCKS connection starts on the same DeviceLocal, then a
// WireGuard response arrives. The old DialContext called SetReceive(nil), so
// the response was injected into the private Tun and WireGuard retried forever
// despite both the hosted window and the process remaining ready.
func TestProxyDeviceTunDialDoesNotStealWireGuardReturn(t *testing.T) {
	receive := make(chan []byte, 1)
	address := netip.MustParseAddr("10.33.44.55")
	tunCtx, closeTun := context.WithCancel(context.Background())
	defer closeTun()
	tun, err := connect.CreateTunWithDefaults(tunCtx)
	if err != nil {
		t.Fatalf("create Tun: %v", err)
	}
	defer tun.Close()
	pd := &ProxyDevice{
		ctx:            tunCtx,
		tun:            tun,
		receiveMonitor: connect.NewMonitor(),
	}

	pd.SetReceiveForAddress(address, receive)
	dialCtx, cancelDial := context.WithCancel(context.Background())
	cancelDial()
	_, _ = pd.DialContext(dialCtx, "tcp", "192.0.2.1:443")

	packet := connect.MessagePoolCopy(ipv4PacketForDestination(address))
	pd.deliverReturnPackets([][]byte{packet})
	select {
	case deliveredPacket := <-receive:
		if !bytes.Equal(deliveredPacket, packet) {
			t.Fatalf("delivered packet = %x, want %x", deliveredPacket, packet)
		}
		if connect.MessagePoolReturn(deliveredPacket) {
			t.Fatal("returning the shared delivery released the callback's original owner")
		}
	default:
		t.Fatal("TUN dial stole the WireGuard return packet")
	}
	if !connect.MessagePoolReturn(packet) {
		t.Fatal("callback packet did not retain its original message-pool owner")
	}
}
