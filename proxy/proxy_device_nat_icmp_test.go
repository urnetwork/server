package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/userwireguard/tun/tuntest"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// Exercise the real ProxyDevice return demultiplexer/NAT and the same gVisor
// stack that consumes decrypted WireGuard packets. No remote DNS, crypto timer,
// provider, or sleeps are needed. Inbound injection completes synchronously;
// read deadlines only bound a broken implementation, not the test ordering.
func TestProxyDeviceWireGuardUDPTeardownReachesSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer := netip.MustParseAddr("10.55.12.34")
	tunDevice := tuntest.NewChannelTUN()
	client, err := newWgClientStack(ctx, peer, 1420, tunDevice)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseAndWait()
	remote := tcpip.FullAddress{NIC: client.nicId, Addr: tcpip.AddrFrom4([4]byte{1, 1, 1, 1}), Port: 53}
	connection, err := gonet.DialUDP(client.stack, nil, &remote, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	device := natTestDevice(t, true, peer)
	device.ctx = ctx
	receive := make(chan []byte, 1)
	device.SetReceiveForAddress(peer, receive)
	var providerPacket []byte
	device.sendOwnedPacketForTest = func(packet []byte) bool {
		providerPacket = bytes.Clone(packet)
		if !connect.MessagePoolReturn(packet) {
			t.Fatal("egress packet did not transfer its single pool owner")
		}
		return true
	}
	query := []byte("bounded DNS socket probe")
	if _, err := connection.Write(query); err != nil {
		t.Fatal(err)
	}
	original := <-tunDevice.Outbound
	before := bytes.Clone(original)
	if !device.Send(original) || !bytes.Equal(original, before) {
		t.Fatal("WireGuard send failed or mutated its borrowed packet")
	}
	if packetSource(providerPacket) != device.natAddr {
		t.Fatal("provider packet did not pass through source NAT")
	}

	deliver := func(packet []byte) []byte {
		t.Helper()
		owned := connect.MessagePoolCopy(packet)
		device.deliverReturnPackets([][]byte{owned})
		select {
		case shared := <-receive:
			copy := bytes.Clone(shared)
			if connect.MessagePoolReturn(shared) || !connect.MessagePoolReturn(owned) {
				t.Fatal("return handoff lost callback/shared ownership")
			}
			inbound := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(copy)})
			client.endpoint.InjectInbound(header.IPv4ProtocolNumber, inbound)
			inbound.DecRef()
			return copy
		default:
			connect.MessagePoolReturn(owned)
			t.Fatal("NAT-addressed return missed WireGuard")
			return nil
		}
	}
	// A normal DNS datagram still reaches this socket without an error.
	answer := []byte("healthy DNS answer")
	deliver(wgNatUDPReply(providerPacket, answer))
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 256)
	if n, err := connection.Read(buffer); err != nil || !bytes.Equal(buffer[:n], answer) {
		t.Fatalf("healthy DNS response = %q, %v", buffer[:n], err)
	}

	returned := deliver(wgNatUDPTeardown(providerPacket))
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	_, err = connection.Read(buffer)
	var opError *net.OpError
	if !errors.As(err, &opError) || opError.Timeout() || opError.Err.Error() != (&tcpip.ErrConnectionRefused{}).String() {
		t.Fatalf("UDP teardown read = %v, want immediate ECONNREFUSED; quoted source = %s", err, net.IP(returned[40:44]))
	}
	// The quotation is the real original socket packet's IP header and
	// first eight transport bytes, including its repaired UDP checksum.
	if !bytes.Equal(returned[28:], original[:28]) {
		t.Fatal("ICMP quotation does not exactly recover the original socket packet")
	}
	if natTestChecksum(returned[:20]) != 0 || natTestChecksum(returned[20:]) != 0 || natTestChecksum(returned[28:48]) != 0 {
		t.Fatal("repaired outer IP, outer ICMP, or quoted IP checksum is invalid")
	}
}

// Build the same RFC 792 quotation emitted by Connect's UDP teardown: a full
// IPv4 header and eight UDP bytes. Leave the quoted original total length and
// UDP checksum intact even though the rest of the datagram is not quoted.
func wgNatUDPTeardown(query []byte) []byte {
	headerSize := int(query[0]&15) * 4
	packet := make([]byte, 28+headerSize+8)
	packet[0], packet[8], packet[9] = 0x45, 64, 1
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	copy(packet[12:16], query[16:20])
	copy(packet[16:20], query[12:16])
	packet[20], packet[21] = 3, 3
	copy(packet[28:], query[:headerSize+8])
	binary.BigEndian.PutUint16(packet[10:12], natTestChecksum(packet[:20]))
	binary.BigEndian.PutUint16(packet[22:24], natTestChecksum(packet[20:]))
	return packet
}

func wgNatUDPReply(query, payload []byte) []byte {
	headerSize := int(query[0]&15) * 4
	packet := make([]byte, 28+len(payload))
	packet[0], packet[8], packet[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	copy(packet[12:16], query[16:20])
	copy(packet[16:20], query[12:16])
	copy(packet[20:22], query[headerSize+2:headerSize+4])
	copy(packet[22:24], query[headerSize:headerSize+2])
	binary.BigEndian.PutUint16(packet[24:26], uint16(8+len(payload)))
	copy(packet[28:], payload)
	pseudo := make([]byte, 12+8+len(payload))
	copy(pseudo[:8], packet[12:20])
	pseudo[9] = 17
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(8+len(payload)))
	copy(pseudo[12:], packet[20:])
	udpChecksum := natTestChecksum(pseudo)
	if udpChecksum == 0 {
		udpChecksum = 0xffff
	}
	binary.BigEndian.PutUint16(packet[26:28], udpChecksum)
	binary.BigEndian.PutUint16(packet[10:12], natTestChecksum(packet[:20]))
	return packet
}
