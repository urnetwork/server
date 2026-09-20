// Verifies that teardown cannot mutate a routing identity still visible to callbacks.
package proxy

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect"
)

// An address cannot be reused while a callback can still complete its return path.
func TestProxyDeviceCloseRetainsNatUntilDeviceJoin(t *testing.T) {
	natAddr, ok := connect.TakeLocalIpv4Address()
	if !ok {
		t.Fatal("no local nat address available")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	joinEntered := make(chan struct{})
	release := make(chan struct{})
	device := &ProxyDevice{
		ctx:           ctx,
		cancel:        cancel,
		natAddr:       natAddr,
		natClientAddr: netip.MustParseAddr("192.0.2.55"),
		closeDeviceLocalForTest: func() {
			close(joinEntered)
			<-release
		},
	}
	closed := make(chan error, 1)
	go func() { closed <- device.Close() }()
	<-joinEntered
	defer func() {
		close(release)
		if err := <-closed; err != nil {
			t.Error(err)
		}
	}()
	if ctx.Err() == nil {
		t.Error("device join began before cancellation stopped new traffic")
	}
	latePacket := []byte("late-packet")
	if device.Send(latePacket) {
		t.Error("canceled device accepted a new packet")
	}
	if n := device.SendBorrowedBatch([][]byte{latePacket}, 0); n != 0 {
		t.Errorf("canceled device accepted %d new batch packets", n)
	}
	// The nil Tun makes any late return-path admission observable immediately.
	device.deliverReturnPackets([][]byte{latePacket})
	if !device.stateLock.TryLock() {
		t.Fatal("device join holds the receive state lock")
	}
	device.stateLock.Unlock()
	if got, _, active := device.natAddrs(); got != natAddr || !active {
		t.Errorf("nat lease changed before device join: address = %v, active = %v", got, active)
	}
	otherAddr, ok := connect.TakeLocalIpv4Address()
	if !ok {
		t.Fatal("no address available for another device")
	}
	defer connect.ReturnLocalIpv4Address(otherAddr)
	if otherAddr == natAddr {
		t.Error("another device reused the address before callbacks joined")
	}
}

// Concurrent and repeated callers share one join and one return to the address pool.
func TestProxyDeviceCloseReturnsNatExactlyOnce(t *testing.T) {
	natAddr, ok := connect.TakeLocalIpv4Address()
	if !ok {
		t.Fatal("no local nat address available")
	}
	var joins atomic.Int32
	device := &ProxyDevice{
		natAddr: natAddr,
		closeDeviceLocalForTest: func() {
			joins.Add(1)
		},
	}
	start := make(chan struct{})
	closed := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			closed <- device.Close()
		}()
	}
	close(start)
	for range 2 {
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	}
	firstAddr, ok := connect.TakeLocalIpv4Address()
	if !ok {
		t.Fatal("no first address available after close")
	}
	defer connect.ReturnLocalIpv4Address(firstAddr)
	if firstAddr != natAddr {
		t.Fatalf("close did not return its nat lease: got %v, want %v", firstAddr, natAddr)
	}
	if err := device.Close(); err != nil {
		t.Fatal(err)
	}
	secondAddr, ok := connect.TakeLocalIpv4Address()
	if !ok {
		t.Fatal("no second address available after close")
	}
	defer connect.ReturnLocalIpv4Address(secondAddr)
	if firstAddr == secondAddr {
		t.Fatal("repeated close returned an address now owned by another device")
	}
	if got := joins.Load(); got != 1 {
		t.Fatalf("device workers joined %d times, want once", got)
	}
}

// The cancellation barrier leaves reads and teardown unordered until Close joins.
// A race build catches the old write regardless of which side runs first.
func TestProxyDeviceCloseSynchronizesNatReaders(t *testing.T) {
	natAddr, ok := connect.TakeLocalIpv4Address()
	if !ok {
		t.Fatal("no local nat address available")
	}
	peerAddr := netip.MustParseAddr("192.0.2.55")
	cancelEntered := make(chan struct{})
	release := make(chan struct{})
	device := &ProxyDevice{
		natAddr:       natAddr,
		natClientAddr: peerAddr,
		receiveAddr:   peerAddr,
		cancel: func() {
			close(cancelEntered)
			<-release
		},
	}
	closed := make(chan error, 1)
	go func() { closed <- device.Close() }()
	<-cancelEntered
	close(release)
	gotNatAddr, gotPeerAddr, active := device.natAddrs()
	_, matchAddr, returnAddr, _ := device.receiveWithNotifyNat()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if gotNatAddr != natAddr || gotPeerAddr != peerAddr || !active {
		t.Fatalf("routing identity changed during teardown: nat = %v, peer = %v, active = %v", gotNatAddr, gotPeerAddr, active)
	}
	if matchAddr != natAddr || returnAddr != peerAddr {
		t.Fatalf("return routing changed during teardown: match = %v, peer = %v", matchAddr, returnAddr)
	}
}
