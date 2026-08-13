// These tests pin the copy boundary between userwireguard's borrowed buffers
// and DeviceLocal's asynchronous pooled-buffer ownership.
package proxy

import (
	"bytes"
	"context"
	"testing"

	"github.com/urnetwork/connect"
)

// A singular borrowed packet must remain valid after userwireguard reuses its
// source buffer, and its eventual sender must own the independent pooled copy.
func TestProxyDeviceSendCopiesBorrowedWireGuardPacket(t *testing.T) {
	packet := []byte("wireguard borrowed packet")
	wantPacket := bytes.Clone(packet)
	var ownedPacket []byte
	proxyDevice := &ProxyDevice{
		ctx: context.Background(),
		sendOwnedPacketForTest: func(packet []byte) bool {
			ownedPacket = packet
			return true
		},
	}

	if !proxyDevice.Send(packet) {
		t.Fatal("borrowed packet was not admitted")
	}
	clear(packet)
	if !bytes.Equal(ownedPacket, wantPacket) {
		t.Fatalf("owned packet changed after source reuse: got %q want %q", ownedPacket, wantPacket)
	}
	if !connect.MessagePoolReturn(ownedPacket) {
		t.Fatal("singular WireGuard copy was not owned by the message pool")
	}
}

// A batch copies every offset payload before admission; mutation of the
// userwireguard buffers after return cannot alter asynchronous DeviceLocal work.
func TestProxyDeviceSendBorrowedBatchCopiesBeforeReturn(t *testing.T) {
	const offset = 8
	packets := [][]byte{
		append(make([]byte, offset), []byte("first")...),
		append(make([]byte, offset), []byte("second")...),
		append(make([]byte, offset), []byte("third")...),
	}
	wantPackets := [][]byte{[]byte("first"), []byte("second"), []byte("third")}
	var ownedPackets [][]byte
	proxyDevice := &ProxyDevice{
		ctx: context.Background(),
		sendOwnedPacketsForTest: func(packets [][]byte) int {
			ownedPackets = packets
			return len(packets)
		},
	}

	if sentPacketCount := proxyDevice.SendBorrowedBatch(packets, offset); sentPacketCount != len(packets) {
		t.Fatalf("sent packets=%d, want %d", sentPacketCount, len(packets))
	}
	for _, packet := range packets {
		clear(packet)
	}
	for packetIndex, ownedPacket := range ownedPackets {
		if !bytes.Equal(ownedPacket, wantPackets[packetIndex]) {
			t.Errorf("owned packet %d=%q, want %q", packetIndex, ownedPacket, wantPackets[packetIndex])
		}
		if !connect.MessagePoolReturn(ownedPacket) {
			t.Errorf("batch WireGuard copy %d was not owned by the message pool", packetIndex)
		}
	}
}

// A partial injected admission keeps only the accepted prefix; ProxyDevice
// returns every rejected copied suffix owner before returning to userwireguard.
func TestProxyDeviceSendBorrowedBatchReturnsRejectedCopies(t *testing.T) {
	beforeTakenCount, beforeReturnedCount, _ := connect.MessagePoolCounts()
	var acceptedPacket []byte
	proxyDevice := &ProxyDevice{
		ctx: context.Background(),
		sendOwnedPacketsForTest: func(packets [][]byte) int {
			acceptedPacket = packets[0]
			return 1
		},
	}
	packets := [][]byte{[]byte("accepted"), []byte("rejected-1"), []byte("rejected-2")}
	if sentPacketCount := proxyDevice.SendBorrowedBatch(packets, 0); sentPacketCount != 1 {
		t.Fatalf("sent packets=%d, want 1", sentPacketCount)
	}
	if !connect.MessagePoolReturn(acceptedPacket) {
		t.Fatal("accepted WireGuard copy was not owned by the message pool")
	}
	afterTakenCount, afterReturnedCount, _ := connect.MessagePoolCounts()
	if afterTakenCount-beforeTakenCount != afterReturnedCount-beforeReturnedCount {
		t.Fatalf(
			"message pool delta taken=%d returned=%d",
			afterTakenCount-beforeTakenCount,
			afterReturnedCount-beforeReturnedCount,
		)
	}
}
