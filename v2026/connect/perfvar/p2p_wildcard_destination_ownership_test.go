// These tests pin wildcard vnet destination ownership without attributing the
// historical nine-hop join failure to its aggregate cancellation counters.
package perfvar

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/pion/transport/v4"
)

// One real composite-node wrapper owns both the scheduler and Pion UDP path.
// Existing topology construction supplies its synthetic endpoint addresses.
func newP2pWildcardOwnershipFixture(
	t *testing.T,
) (context.Context, *streamP2pNetwork, transport.UDPConn, net.IP) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	network, err := newStreamP2pNetwork(networkProfile{
		Name:    "wildcard-destination-ownership",
		Seed:    8401,
		Forward: profile,
		Reverse: profile,
	}, 1)
	if err != nil {
		cancel()
		t.Fatalf("construct wildcard ownership fixture: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		network.close()
	})
	left := network.nets[0].(*streamP2pNodeNet).endpoints[0].address
	right := network.nets[1].(*streamP2pNodeNet).endpoints[0].address
	sender, err := network.nets[0].ListenUDP("udp4", &net.UDPAddr{IP: net.IP(left.AsSlice())})
	if err != nil {
		t.Fatalf("listen wildcard ownership sender: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	return ctx, network, sender, net.IP(right.AsSlice())
}

// A same-port wildcard on another endpoint cannot turn a rejected topology
// packet into a receive or router-completion obligation.
func TestP2pWildcardDestinationOwnershipForeign(t *testing.T) {
	ctx, network, sender, _ := newP2pWildcardOwnershipFixture(t)
	receiver, err := network.nets[1].ListenUDP("udp4", &net.UDPAddr{Port: 56300})
	if err != nil {
		t.Fatalf("listen wildcard destination: %v", err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	destinations := []net.IP{
		net.ParseIP("203.0.113.29"),
		net.IPv4zero,
		nil,
		net.ParseIP("2001:db8::29"),
		net.IPv6unspecified,
		net.IPv6loopback,
		net.ParseIP("::ffff:203.0.113.29"),
	}
	for _, ip := range destinations {
		payload := []byte("foreign-wildcard-destination")
		n, err := sender.WriteTo(payload, &net.UDPAddr{IP: ip, Port: 56300})
		if err != nil || n != len(payload) {
			t.Fatalf("submit foreign destination %v: bytes=%d err=%v", ip, n, err)
		}
	}
	// Scheduler completion proves every destination lookup and delegate result
	// finished; no timing-based negative read or router polling is needed.
	if !network.hopForwardLinks[0].waitIdle(ctx) {
		t.Fatalf("join foreign destination scheduler: %v", ctx.Err())
	}
	if err := receiver.Close(); err != nil {
		t.Fatalf("close foreign destination listener: %v", err)
	}
	credits := network.hopForwardReceiveCredits[0].snapshot()
	if credits.AdmittedPacketCount != 0 || credits.CanceledPacketCount != 0 ||
		credits.OutstandingPacketCount != 0 || credits.TrackedReservationCount != 0 ||
		credits.RouterPendingPacketCount != 0 || !credits.isExactLiveTerminal() {
		t.Fatalf("foreign wildcard created phantom ownership: %+v", credits)
	}
	link := network.hopForwardLinks[0].snapshot()
	if link.DeliveredPacketCount != 0 || link.ReceiverDropPacketCount != uint64(len(destinations)) {
		t.Fatalf("foreign wildcard link disposition: %+v", link)
	}
	if link.LossDropPacketCount != 0 || link.MtuDropPacketCount != 0 ||
		link.QueueDropPacketCount != 0 || link.OutageDropPacketCount != 0 ||
		link.CanceledDropPacketCount != 0 {
		t.Fatalf("foreign wildcard used an incidental drop: %+v", link)
	}
	nonAdjacent := network.nonAdjacent.snapshot()
	if nonAdjacent.DataPacketDropCount != uint64(len(destinations)) ||
		nonAdjacent.StunPacketDropCount != 0 {
		t.Fatalf("foreign wildcard topology disposition: %+v", nonAdjacent)
	}
	if !network.waitForTerminalIdle(ctx) {
		t.Fatalf("join foreign destination carrier: %v", ctx.Err())
	}
}

// Nil and explicit IPv4 wildcard binds retain normal local delivery, including
// an IPv4-mapped destination presented through the generic address surface.
func TestP2pWildcardDestinationOwnershipLocal(t *testing.T) {
	ctx, network, sender, destinationIp := newP2pWildcardOwnershipFixture(t)
	for index, bindIp := range []net.IP{nil, net.IPv4zero} {
		receiver, err := network.nets[1].ListenUDP("udp4", &net.UDPAddr{IP: bindIp, Port: 56301})
		if err != nil {
			t.Fatalf("listen local wildcard %d: %v", index, err)
		}
		t.Cleanup(func() { _ = receiver.Close() })
		deadline, _ := ctx.Deadline()
		if err := receiver.SetReadDeadline(deadline); err != nil {
			t.Fatalf("set local wildcard read guard: %v", err)
		}
		payload := []byte{byte(index), 7, 9, 11}
		destination := &net.UDPAddr{IP: net.ParseIP(destinationIp.String()), Port: 56301}
		if n, err := sender.WriteTo(payload, destination); err != nil || n != len(payload) {
			t.Fatalf("write local wildcard %d: bytes=%d err=%v", index, n, err)
		}
		readPayload := make([]byte, len(payload))
		if n, _, err := receiver.ReadFrom(readPayload); err != nil || n != len(payload) ||
			!bytes.Equal(readPayload, payload) {
			t.Fatalf("read local wildcard %d: bytes=%d payload=%v err=%v", index, n, readPayload, err)
		}
		if err := receiver.Close(); err != nil {
			t.Fatalf("close local wildcard %d: %v", index, err)
		}
	}
	if !network.waitForTerminalIdle(ctx) {
		t.Fatalf("join local wildcard carrier: %v", ctx.Err())
	}
	credits := network.hopForwardReceiveCredits[0].snapshot()
	if credits.AdmittedPacketCount != 2 || credits.ReadPacketCount != 2 ||
		credits.CanceledPacketCount != 0 || !credits.isExactLiveTerminal() {
		t.Fatalf("local wildcard ownership: %+v", credits)
	}
	if nonAdjacent := network.nonAdjacent.snapshot(); nonAdjacent.DataPacketDropCount != 0 ||
		nonAdjacent.StunPacketDropCount != 0 {
		t.Fatalf("local wildcard classified non-adjacent: %+v", nonAdjacent)
	}
}

// Pion vnet aliases either unspecified bind family to the same UDP wildcard.
// Preserve that delegate behavior while still requiring the actual endpoint IP.
func TestP2pWildcardDestinationOwnershipIpv6WildcardAlias(t *testing.T) {
	ctx, network, sender, destinationIp := newP2pWildcardOwnershipFixture(t)
	receiver, err := network.nets[1].ListenUDP("udp", &net.UDPAddr{IP: net.IPv6unspecified, Port: 56302})
	if err != nil {
		t.Fatalf("listen IPv6 wildcard control: %v", err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	deadline, _ := ctx.Deadline()
	if err := receiver.SetReadDeadline(deadline); err != nil {
		t.Fatalf("set IPv6 wildcard alias read guard: %v", err)
	}
	payload := []byte("local-ipv4-enters-vnet-wildcard-alias")
	if n, err := sender.WriteTo(payload, &net.UDPAddr{IP: destinationIp, Port: 56302}); err != nil ||
		n != len(payload) {
		t.Fatalf("write IPv6 wildcard control: bytes=%d err=%v", n, err)
	}
	readPayload := make([]byte, len(payload))
	if n, _, err := receiver.ReadFrom(readPayload); err != nil || n != len(payload) ||
		!bytes.Equal(readPayload, payload) {
		t.Fatalf("read IPv6 wildcard alias: bytes=%d payload=%v err=%v", n, readPayload, err)
	}
	if !network.hopForwardLinks[0].waitIdle(ctx) {
		t.Fatalf("join IPv6 wildcard scheduler: %v", ctx.Err())
	}
	if err := receiver.Close(); err != nil {
		t.Fatalf("close IPv6 wildcard control: %v", err)
	}
	credits := network.hopForwardReceiveCredits[0].snapshot()
	if credits.AdmittedPacketCount != 1 || credits.ReadPacketCount != 1 || credits.CanceledPacketCount != 0 ||
		credits.RouterPendingPacketCount != 0 || !credits.isExactLiveTerminal() {
		t.Fatalf("IPv6 wildcard alias changed local ownership: %+v", credits)
	}
	if !network.waitForTerminalIdle(ctx) {
		t.Fatalf("join IPv6 wildcard carrier: %v", ctx.Err())
	}
}

// Framed wildcard ownership is fail-closed when construction omitted its
// endpoint identity. Credit-only delegate fixtures keep their existing policy.
func TestP2pWildcardDestinationOwnershipUnknownOwner(t *testing.T) {
	for _, owner := range []*p2pLinkNet{nil, {}} {
		credits := newP2pVnetReceiveCredits(1)
		socket := credits.registerSocket(&net.UDPAddr{Port: 56303}, nil, owner)
		reservation, found, admitted := credits.reserveForTransfer(t.Context(),
			&net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 56304},
			&net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 56303},
		)
		reservation.cancel()
		socket.close()
		snapshot := credits.snapshot()
		credits.close()
		if found || !admitted || snapshot.AdmittedPacketCount != 0 ||
			snapshot.CanceledPacketCount != 0 || !snapshot.isExactLiveTerminal() {
			t.Fatalf("unknown wildcard endpoint admitted destination: found=%t admitted=%t snapshot=%+v",
				found, admitted, snapshot)
		}
	}
	credits := newP2pDestinationReceiveCredits(1)
	socket := credits.registerSocket(&net.UDPAddr{Port: 56303}, nil, nil)
	reservation, found, admitted := credits.reserveForTransfer(t.Context(),
		&net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 56304},
		&net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 56303},
	)
	reservation.cancel()
	socket.close()
	snapshot := credits.snapshot()
	credits.close()
	if !found || !admitted || snapshot.AdmittedPacketCount != 1 ||
		snapshot.CanceledPacketCount != 1 || !snapshot.isExactLiveTerminal() {
		t.Fatalf("credit-only wildcard compatibility changed: found=%t admitted=%t snapshot=%+v",
			found, admitted, snapshot)
	}
}

// Cancellation returns receive capacity but not a real frame's router owner.
// An unrelated reservation on the shared hop survives the closed socket read.
func TestP2pWildcardDestinationOwnershipCanceledInflight(t *testing.T) {
	ctx, network, sender, destinationIp := newP2pWildcardOwnershipFixture(t)
	credits := network.hopForwardReceiveCredits[0]
	filterEntered := make(chan struct{})
	releaseFilter := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFilter) }) }
	var workers sync.WaitGroup
	t.Cleanup(func() {
		release()
		workers.Wait()
	})
	credits.beforeRouterCompletionForTest = func([]byte) {
		close(filterEntered)
		select {
		case <-releaseFilter:
		case <-ctx.Done():
		}
	}
	routerWaitEntered := make(chan struct{})
	var routerWaitOnce sync.Once
	credits.routerPendingObservedForTest = func() {
		routerWaitOnce.Do(func() { close(routerWaitEntered) })
	}
	receiver, err := network.nets[1].ListenUDP("udp4", &net.UDPAddr{Port: 56305})
	if err != nil {
		t.Fatalf("listen inflight wildcard: %v", err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	other, err := network.nets[1].ListenUDP("udp4", &net.UDPAddr{IP: destinationIp, Port: 56306})
	if err != nil {
		t.Fatalf("listen unrelated shared-hop destination: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	payload := []byte("held-inflight-wildcard")
	if n, err := sender.WriteTo(payload, &net.UDPAddr{IP: destinationIp, Port: 56305}); err != nil ||
		n != len(payload) {
		t.Fatalf("write inflight wildcard: bytes=%d err=%v", n, err)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("wait for inflight router gate: %v", ctx.Err())
	case <-filterEntered:
	}
	if err := receiver.Close(); err != nil {
		t.Fatalf("close inflight wildcard: %v", err)
	}
	otherReservation, found, admitted := credits.reserveForTransfer(ctx,
		sender.LocalAddr(), &net.UDPAddr{IP: destinationIp, Port: 56306},
	)
	if !found || !admitted || otherReservation == nil {
		t.Fatalf("reserve unrelated shared-hop credit: found=%t admitted=%t", found, admitted)
	}
	t.Cleanup(otherReservation.cancel)
	if n, _, err := receiver.ReadFrom(make([]byte, len(payload))); err == nil || n != 0 {
		t.Fatalf("closed wildcard read: bytes=%d err=%v", n, err)
	}
	held := credits.snapshot()
	if held.CanceledPacketCount != 1 || held.OutstandingPacketCount != 1 ||
		held.TrackedReservationCount != 1 || held.RouterPendingPacketCount != 1 ||
		held.InvalidReleasePacketCount != 0 || len(credits.available) != held.CapacityPacketCount-1 {
		t.Fatalf("closed wildcard consumed unrelated shared-hop owner: %+v", held)
	}
	pending := snapshotP2pRouterPending([]*p2pReceiveCredits{credits})
	if len(pending) != 1 || len(pending[0].Owners) != 1 || pending[0].Owners[0].Active ||
		!pending[0].Owners[0].Closing || !pending[0].Owners[0].Closed ||
		pending[0].Owners[0].Generation != receiver.(*p2pLinkUDPConn).receiveSocket.generation {
		t.Fatalf("pending owner did not identify the canceled inflight socket: %+v", pending)
	}
	joined := make(chan bool, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		joined <- waitForP2pCarrierQuiescent(ctx, network.directionalLinks(), network.receiveCreditPools(), nil)
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for router join ownership barrier: %v", ctx.Err())
	case <-routerWaitEntered:
	}
	select {
	case result := <-joined:
		t.Fatalf("join crossed a held router owner: result=%t", result)
	default:
	}
	release()
	select {
	case <-ctx.Done():
		t.Fatalf("join released inflight router owner: %v", ctx.Err())
	case result := <-joined:
		if !result {
			t.Fatal("released router owner did not permit quiescence")
		}
	}
	workers.Wait()
	released := credits.snapshot()
	if released.RouterPendingPacketCount != 0 || released.StaleGenerationDropCount != 1 ||
		released.OutstandingPacketCount != 1 || released.TrackedReservationCount != 1 ||
		released.CanceledPacketCount != 1 || released.InvalidReleasePacketCount != 0 {
		t.Fatalf("router completion consumed unrelated shared-hop credit: %+v", released)
	}
	otherReservation.cancel()
	if !network.waitForTerminalIdle(ctx) {
		t.Fatalf("join canceled shared-hop reservations: %v", ctx.Err())
	}
	if terminal := credits.snapshot(); terminal.AdmittedPacketCount != 2 ||
		terminal.CanceledPacketCount != 2 || !terminal.isExactLiveTerminal() {
		t.Fatalf("canceled inflight terminal ownership: %+v", terminal)
	}
}

// A fixture-only delegate holds a successfully dequeued real vnet frame before
// its wrapper can account for the read. It owns no borrowed data after return.
type p2pWildcardHeldRead struct {
	transport.UDPConn
	ctx         context.Context
	readEntered chan struct{}
	releaseRead <-chan struct{}
}

// The worker retains the caller-owned read buffer only for this call.
func (self *p2pWildcardHeldRead) ReadFrom(payload []byte) (int, net.Addr, error) {
	n, address, err := self.UDPConn.ReadFrom(payload)
	close(self.readEntered)
	select {
	case <-self.releaseRead:
	case <-self.ctx.Done():
	}
	return n, address, err
}

// Closing after dequeue and before wrapper accounting retires that old owner
// once; the late successful read must not consume a different socket's credit.
func TestP2pWildcardDestinationOwnershipLateRead(t *testing.T) {
	ctx, network, sender, destinationIp := newP2pWildcardOwnershipFixture(t)
	receiver, err := network.nets[1].ListenUDP("udp4", &net.UDPAddr{Port: 56307})
	if err != nil {
		t.Fatalf("listen late-read wildcard: %v", err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	wrapped := receiver.(*p2pLinkUDPConn)
	readEntered := make(chan struct{})
	releaseRead := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRead) }) }
	var workers sync.WaitGroup
	t.Cleanup(func() {
		release()
		_ = receiver.Close()
		workers.Wait()
	})
	wrapped.UDPConn = &p2pWildcardHeldRead{
		UDPConn:     wrapped.UDPConn,
		ctx:         ctx,
		readEntered: readEntered,
		releaseRead: releaseRead,
	}
	deadline, _ := ctx.Deadline()
	if err := receiver.SetReadDeadline(deadline); err != nil {
		t.Fatalf("set late-read guard: %v", err)
	}
	payload := []byte("dequeued-before-wildcard-close")
	if n, err := sender.WriteTo(payload, &net.UDPAddr{IP: destinationIp, Port: 56307}); err != nil ||
		n != len(payload) {
		t.Fatalf("write late-read wildcard: bytes=%d err=%v", n, err)
	}
	type readResult struct {
		n       int
		payload []byte
		err     error
	}
	readDone := make(chan readResult, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		buffer := make([]byte, len(payload))
		n, _, err := receiver.ReadFrom(buffer)
		readDone <- readResult{n: n, payload: buffer, err: err}
	}()
	select {
	case <-ctx.Done():
		t.Fatalf("wait for dequeued wildcard frame: %v", ctx.Err())
	case <-readEntered:
	}
	if err := receiver.Close(); err != nil {
		t.Fatalf("close dequeued wildcard: %v", err)
	}
	other, err := network.nets[1].ListenUDP("udp4", &net.UDPAddr{IP: destinationIp, Port: 56308})
	if err != nil {
		t.Fatalf("listen late-read unrelated destination: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	credits := network.hopForwardReceiveCredits[0]
	otherReservation, found, admitted := credits.reserveForTransfer(ctx,
		sender.LocalAddr(), &net.UDPAddr{IP: destinationIp, Port: 56308},
	)
	if !found || !admitted || otherReservation == nil {
		t.Fatalf("reserve late-read unrelated credit: found=%t admitted=%t", found, admitted)
	}
	t.Cleanup(otherReservation.cancel)
	release()
	select {
	case <-ctx.Done():
		t.Fatalf("join late successful wildcard read: %v", ctx.Err())
	case result := <-readDone:
		if result.err != nil || result.n != len(payload) || !bytes.Equal(result.payload, payload) {
			t.Fatalf("late wildcard read result=%+v", result)
		}
	}
	workers.Wait()
	if held := credits.snapshot(); held.CanceledPacketCount != 1 || held.ReadPacketCount != 0 ||
		held.OutstandingPacketCount != 1 || held.TrackedReservationCount != 1 ||
		held.InvalidReleasePacketCount != 0 || held.RouterPendingPacketCount != 0 {
		t.Fatalf("late read consumed a different socket's credit: %+v", held)
	}
	otherReservation.cancel()
	if !network.waitForTerminalIdle(ctx) {
		t.Fatalf("join late-read ownership: %v", ctx.Err())
	}
}

// Failure evidence is bounded even when several reservations share a socket;
// each sampled row preserves that exact owner's state without guessing joins.
func TestP2pWildcardDestinationOwnershipPendingSnapshot(t *testing.T) {
	credits := newP2pVnetReceiveCredits(5)
	defer credits.close()
	owner := &p2pLinkNet{sourceIPv4: netip.MustParseAddr("192.0.2.2")}
	socket := credits.registerSocket(&net.UDPAddr{Port: 56309}, nil, owner)
	for range 5 {
		reservation, found, admitted := credits.reserveForTransfer(t.Context(),
			&net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 56310},
			&net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 56309},
		)
		if !found || !admitted || reservation == nil {
			t.Fatalf("reserve pending snapshot owner: found=%t admitted=%t", found, admitted)
		}
		if _, framed := reservation.framePayload(make([]byte, p2pIPv4UDPHeaderByteCount+1)); !framed {
			t.Fatal("frame pending snapshot owner")
		}
	}
	socket.close()
	snapshot := snapshotP2pRouterPending([]*p2pReceiveCredits{nil, credits})
	if len(snapshot) != 1 || snapshot[0].PoolIndex != 1 || snapshot[0].PendingPacketCount != 5 ||
		len(snapshot[0].Owners) != 4 || snapshot[0].OmittedOwnerCount != 1 {
		t.Fatalf("unbounded or incomplete pending snapshot: %+v", snapshot)
	}
	for _, pending := range snapshot[0].Owners {
		if pending.Generation != socket.generation || pending.Address != socket.addressKey ||
			pending.Active || !pending.Closing || !pending.Closed {
			t.Fatalf("pending snapshot invented ownership: %+v", pending)
		}
	}
	for range 5 {
		credits.completeRouterPayload(socket.generation)
	}
	if snapshot := snapshotP2pRouterPending([]*p2pReceiveCredits{credits}); len(snapshot) != 0 {
		t.Fatalf("completed owners retained in failure snapshot: %+v", snapshot)
	}
	if terminal := credits.snapshot(); terminal.CanceledPacketCount != 5 || !terminal.isExactLiveTerminal() {
		t.Fatalf("snapshot changed router/receive ownership: %+v", terminal)
	}
}
