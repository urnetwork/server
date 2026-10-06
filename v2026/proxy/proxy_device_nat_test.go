package proxy

// WireGuard source NAT (threat model §9.4).
//
// A wg peer's tunnel address is allocated from a durable pool and written into
// its config, so without the rewrite it is stable across sessions AND identical
// at every provider in the peer's window -- which is exactly what lets
// colluding providers tell that those flows belong to one client. These tests
// pin the property that the peer's address never reaches a provider, and that
// the return path is restored exactly.

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// natTestChecksum is the standard one's-complement sum. Kept local so this
// package needs no packet library: the incremental-repair arithmetic is
// connect's contract and is tested there against full recomputation; what this
// file tests is the NAT policy, plus a verify-to-zero sanity check that is
// independent of how the repair was computed.
func natTestChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for 0xffff < sum {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}

func natPeerAddr(t *testing.T) netip.Addr {
	t.Helper()
	addr, err := netip.ParseAddr("10.55.12.34")
	if err != nil {
		t.Fatalf("parse: %s", err)
	}
	return addr
}

// buildWgTestPacket hand-builds a valid IPv4/TCP packet with both checksums
// computed, so the tests start from a packet a real stack would accept.
func buildWgTestPacket(t *testing.T, source string, destination string, payload string) []byte {
	t.Helper()
	src, err := netip.ParseAddr(source)
	if err != nil {
		t.Fatalf("parse %s: %s", source, err)
	}
	dst, err := netip.ParseAddr(destination)
	if err != nil {
		t.Fatalf("parse %s: %s", destination, err)
	}
	body := []byte(payload)
	const headerSize = 20
	const tcpSize = 20
	packet := make([]byte, headerSize+tcpSize+len(body))

	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	binary.BigEndian.PutUint16(packet[4:6], 0x1234)
	packet[8] = 64
	packet[9] = 6 // tcp
	s4, d4 := src.As4(), dst.As4()
	copy(packet[12:16], s4[:])
	copy(packet[16:20], d4[:])
	binary.BigEndian.PutUint16(packet[10:12], natTestChecksum(packet[0:headerSize]))

	tcp := packet[headerSize:]
	binary.BigEndian.PutUint16(tcp[0:2], 51423)
	binary.BigEndian.PutUint16(tcp[2:4], 443)
	binary.BigEndian.PutUint32(tcp[4:8], 0x1000)
	tcp[12] = 5 << 4
	tcp[13] = 0x18 // psh|ack
	binary.BigEndian.PutUint16(tcp[14:16], 502)
	copy(tcp[tcpSize:], body)
	binary.BigEndian.PutUint16(tcp[16:18], tcpPseudoChecksum(packet))
	return packet
}

// tcpPseudoChecksum computes the TCP checksum over the pseudo-header plus the
// segment, with the checksum field already zero.
func tcpPseudoChecksum(packet []byte) uint16 {
	headerSize := int(packet[0]&0x0f) * 4
	tcp := packet[headerSize:]
	pseudo := make([]byte, 12+len(tcp))
	copy(pseudo[0:4], packet[12:16])
	copy(pseudo[4:8], packet[16:20])
	pseudo[9] = 6
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(tcp)))
	copy(pseudo[12:], tcp)
	binary.BigEndian.PutUint16(pseudo[12+16:12+18], 0)
	return natTestChecksum(pseudo)
}

func packetSource(packet []byte) netip.Addr {
	addr, _ := netip.AddrFromSlice(packet[12:16])
	return addr
}

func packetDestination(packet []byte) netip.Addr {
	addr, _ := netip.AddrFromSlice(packet[16:20])
	return addr
}

// natTestDevice is a ProxyDevice with only the NAT state populated. The rewrite
// helpers touch nothing else, so this avoids standing up a whole device to test
// address substitution.
func natTestDevice(t *testing.T, withNat bool, peer netip.Addr) *ProxyDevice {
	t.Helper()
	device := &ProxyDevice{receiveMonitor: connect.NewMonitor()}
	if withNat {
		natAddr, ok := connect.TakeLocalIpv4Address()
		if !ok {
			t.Fatal("no local nat address available")
		}
		device.natAddr = natAddr
		t.Cleanup(func() { connect.ReturnLocalIpv4Address(natAddr) })
	}
	if peer.IsValid() {
		device.natClientAddr = peer
	}
	return device
}

// The property the whole change exists for.
func TestWgNatReplacesThePeerAddressOnEgress(t *testing.T) {
	peer := natPeerAddr(t)
	device := natTestDevice(t, true, peer)

	packet := buildWgTestPacket(t, peer.String(), "93.184.216.34", "to-the-provider")
	if !device.natRewriteEgress(packet) {
		t.Fatal("rewrite refused")
	}
	if got := packetSource(packet); got == peer {
		t.Fatal("the peer's tunnel address reached the provider")
	}
	if got := packetSource(packet); got != device.natAddr {
		t.Fatalf("source = %s, want the nat address %s", got, device.natAddr)
	}
	// the NAT address must come from the same pool the app path's tun uses
	if !device.natAddr.IsValid() || !netip.MustParsePrefix("169.254.0.0/16").Contains(device.natAddr) {
		t.Fatalf("nat address %s is outside the local pool", device.natAddr)
	}
	if got := packetDestination(packet); got.String() != "93.184.216.34" {
		t.Fatalf("destination changed to %s", got)
	}
	assertWgChecksums(t, packet)
}

func TestWgNatRestoresThePeerAddressOnReturn(t *testing.T) {
	peer := natPeerAddr(t)
	device := natTestDevice(t, true, peer)

	packet := buildWgTestPacket(t, "93.184.216.34", device.natAddr.String(), "from-the-provider")
	if !device.natRewriteReturn(packet, peer) {
		t.Fatal("return rewrite refused")
	}
	if got := packetDestination(packet); got != peer {
		t.Fatalf("destination = %s, want the peer %s", got, peer)
	}
	assertWgChecksums(t, packet)
}

// Out and back must reproduce the peer's own bytes exactly, or the peer's TCP
// stack sees a corrupted stream.
func TestWgNatRoundTripIsExact(t *testing.T) {
	peer := natPeerAddr(t)
	device := natTestDevice(t, true, peer)

	// what the peer would receive if nothing were rewritten
	expected := buildWgTestPacket(t, "93.184.216.34", peer.String(), "round-trip")
	// what the provider actually returns, addressed to the nat address
	returned := buildWgTestPacket(t, "93.184.216.34", device.natAddr.String(), "round-trip")

	if !device.natRewriteReturn(returned, peer) {
		t.Fatal("return rewrite refused")
	}
	if string(returned) != string(expected) {
		t.Fatal("restored packet differs from the one the peer would have received")
	}
}

// Only the attached peer's own traffic is rewritten. A device also serving HTTP
// or SOCKS carries packets with other sources on the same Tun, and touching
// them would corrupt unrelated flows.
func TestWgNatLeavesOtherSourcesAlone(t *testing.T) {
	peer := natPeerAddr(t)
	device := natTestDevice(t, true, peer)

	packet := buildWgTestPacket(t, "10.99.99.99", "93.184.216.34", "not-the-peer")
	original := append([]byte{}, packet...)
	if !device.natRewriteEgress(packet) {
		t.Fatal("rewrite refused")
	}
	if string(packet) != string(original) {
		t.Fatal("a packet from another source was rewritten")
	}
}

// HTTP and SOCKS attach no wg address, so the rewrite must not engage for them.
func TestWgNatInertWithoutAnAttachedPeer(t *testing.T) {
	device := natTestDevice(t, true, netip.Addr{})

	packet := buildWgTestPacket(t, "10.55.12.34", "93.184.216.34", "http-or-socks")
	original := append([]byte{}, packet...)
	if !device.natRewriteEgress(packet) {
		t.Fatal("rewrite refused")
	}
	if string(packet) != string(original) {
		t.Fatal("rewrote a packet with no peer attached")
	}
}

// Exhausting the pool disables the rewrite rather than dropping traffic: the
// fallback is the pre-NAT behavior, not an outage.
func TestWgNatWithoutAnAddressIsAPassThrough(t *testing.T) {
	peer := natPeerAddr(t)
	device := natTestDevice(t, false, peer)

	packet := buildWgTestPacket(t, peer.String(), "93.184.216.34", "no-nat-address")
	original := append([]byte{}, packet...)
	if !device.natRewriteEgress(packet) {
		t.Fatal("rewrite refused")
	}
	if string(packet) != string(original) {
		t.Fatal("rewrote without a nat address")
	}
}

// Return packets arrive addressed to the NAT address, so that is what the
// partition must match on -- matching the peer's address would send every
// return packet to the Tun instead of to WireGuard.
func TestWgNatReturnIsMatchedOnTheNatAddress(t *testing.T) {
	peer := natPeerAddr(t)
	device := natTestDevice(t, true, peer)
	device.receiveAddr = peer
	device.receive = make(chan []byte, 1)

	_, matchAddr, clientAddr, _ := device.receiveWithNotifyNat()
	if matchAddr != device.natAddr {
		t.Fatalf("match address = %s, want the nat address %s", matchAddr, device.natAddr)
	}
	if clientAddr != peer {
		t.Fatalf("client address = %s, want the peer %s", clientAddr, peer)
	}

	returned := buildWgTestPacket(t, "93.184.216.34", device.natAddr.String(), "matched")
	if !proxyPacketMatchesReceiveAddress(returned, matchAddr) {
		t.Fatal("a return packet addressed to the nat address did not match")
	}
	// and the peer's own address must no longer match, since nothing is
	// addressed to it on the wire any more
	peerAddressed := buildWgTestPacket(t, "93.184.216.34", peer.String(), "stale")
	if proxyPacketMatchesReceiveAddress(peerAddressed, matchAddr) {
		t.Fatal("a packet addressed to the peer matched the nat address")
	}
}

// Without a NAT address the device must keep its original behavior of matching
// the peer's own address.
func TestWgNatReturnFallsBackToThePeerAddress(t *testing.T) {
	peer := natPeerAddr(t)
	device := natTestDevice(t, false, peer)
	device.receiveAddr = peer
	device.receive = make(chan []byte, 1)

	_, matchAddr, clientAddr, _ := device.receiveWithNotifyNat()
	if matchAddr != peer {
		t.Fatalf("match address = %s, want the peer %s", matchAddr, peer)
	}
	if clientAddr.IsValid() {
		t.Fatal("a client address was reported with no nat address")
	}
}

// Two devices must not share a NAT address, or their peers would be
// indistinguishable at the provider -- the property this replaces.
func TestWgNatAddressesAreDistinctPerDevice(t *testing.T) {
	peer := natPeerAddr(t)
	seen := map[netip.Addr]bool{}
	for range 16 {
		device := natTestDevice(t, true, peer)
		if seen[device.natAddr] {
			t.Fatalf("nat address %s was handed out twice", device.natAddr)
		}
		seen[device.natAddr] = true
	}
}

// A correct checksum makes its covered region sum to zero, which checks the
// repair without reproducing how it was computed.
func assertWgChecksums(t *testing.T, packet []byte) {
	t.Helper()
	headerSize := int(packet[0]&0x0f) * 4
	if got := natTestChecksum(packet[0:headerSize]); got != 0 {
		t.Errorf("ipv4 header checksum does not verify (%#x)", got)
	}
	tcp := packet[headerSize:]
	pseudo := make([]byte, 12+len(tcp))
	copy(pseudo[0:4], packet[12:16])
	copy(pseudo[4:8], packet[16:20])
	pseudo[9] = 6
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(tcp)))
	copy(pseudo[12:], tcp)
	if got := natTestChecksum(pseudo); got != 0 {
		t.Errorf("tcp checksum does not verify (%#x)", got)
	}
}
