// This file turns the nil-by-default P2P route callback into an exact,
// nonblocking integration-test event stream for multihop readiness.
package perfvar

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// One local route direction is identified by its adjacent peer, stream,
// endpoint role, route-manager owner, and send/receive role. A count is kept
// for this identity because replacement transports may overlap while an old
// generation retires.
type p2pRouteStateKey struct {
	peerId          clientconnect.Id
	streamId        clientconnect.Id
	peerType        clientconnect.PeerType
	routeManagerTag string
	send            bool
}

// One immutable callback edge is retained until the single trace consumer
// applies it. The Treiber stack makes producer admission lock-free.
type p2pRouteStateTraceEvent struct {
	generation uint64
	state      clientconnect.P2pRouteState
	next       *p2pRouteStateTraceEvent
}

// A snapshot reports the exact applied generation and current directional
// route counts. One setup goroutine owns Snapshot and WaitForMinimumRoutes;
// transport callbacks may call Observe concurrently and never block.
type p2pRouteStateTraceSnapshot struct {
	Generation                  uint64
	ActiveSendRoutes            int
	ActiveReceiveRoutes         int
	ConnectedTransitionCount    int
	DisconnectedTransitionCount int
}

// The pending stack retains every edge while a capacity-one notification may
// safely coalesce wakeups. The single consumer sorts admitted generations
// before updating current keyed state.
type p2pRouteStateTrace struct {
	nextGeneration              atomic.Uint64
	pending                     atomic.Pointer[p2pRouteStateTraceEvent]
	changed                     chan struct{}
	active                      map[p2pRouteStateKey]int
	keyGenerations              map[p2pRouteStateKey]uint64
	generation                  uint64
	connectedTransitionCount    int
	disconnectedTransitionCount int
}

// Construction allocates test state once; production settings leave the
// callback nil and never allocate this trace.
func newP2pRouteStateTrace() *p2pRouteStateTrace {
	return &p2pRouteStateTrace{
		changed:        make(chan struct{}, 1),
		active:         map[p2pRouteStateKey]int{},
		keyGenerations: map[p2pRouteStateKey]uint64{},
	}
}

// Observe admits one event through atomics and a nonblocking coalesced wake.
func (self *p2pRouteStateTrace) Observe(state clientconnect.P2pRouteState) {
	event := &p2pRouteStateTraceEvent{state: state}
	for {
		previous := self.pending.Load()
		event.next = previous
		event.generation = self.nextGeneration.Add(1)
		if self.pending.CompareAndSwap(previous, event) {
			break
		}
	}
	select {
	case self.changed <- struct{}{}:
	default:
	}
}

// Snapshot drains every admitted edge, applies it in publication order, and
// returns the resulting exact keyed level.
func (self *p2pRouteStateTrace) Snapshot() p2pRouteStateTraceSnapshot {
	events := []*p2pRouteStateTraceEvent{}
	for event := self.pending.Swap(nil); event != nil; event = event.next {
		events = append(events, event)
	}
	sort.Slice(events, func(i int, j int) bool {
		return events[i].generation < events[j].generation
	})
	for _, event := range events {
		key := p2pRouteStateKey{
			peerId:          event.state.PeerId,
			streamId:        event.state.StreamId,
			peerType:        event.state.PeerType,
			routeManagerTag: event.state.RouteManagerTag,
			send:            event.state.Send,
		}
		if event.generation <= self.keyGenerations[key] {
			continue
		}
		self.keyGenerations[key] = event.generation
		if event.state.Connected {
			self.connectedTransitionCount += 1
			self.active[key] += 1
		} else {
			if 0 < self.active[key] {
				self.disconnectedTransitionCount += 1
				self.active[key] -= 1
			}
			if self.active[key] == 0 {
				delete(self.active, key)
			}
		}
		self.generation = max(self.generation, event.generation)
	}
	snapshot := p2pRouteStateTraceSnapshot{
		Generation:                  self.generation,
		ConnectedTransitionCount:    self.connectedTransitionCount,
		DisconnectedTransitionCount: self.disconnectedTransitionCount,
	}
	for key, activeRouteCount := range self.active {
		if key.send {
			snapshot.ActiveSendRoutes += activeRouteCount
		} else {
			snapshot.ActiveReceiveRoutes += activeRouteCount
		}
	}
	return snapshot
}

// WaitForMinimumRoutes joins exact callback edges until both directional
// levels are live. Context cancellation is only the liveness bound.
func (self *p2pRouteStateTrace) WaitForMinimumRoutes(
	ctx context.Context,
	minimumSendRoutes int,
	minimumReceiveRoutes int,
) (p2pRouteStateTraceSnapshot, error) {
	for {
		snapshot := self.Snapshot()
		if minimumSendRoutes <= snapshot.ActiveSendRoutes &&
			minimumReceiveRoutes <= snapshot.ActiveReceiveRoutes {
			return snapshot, nil
		}
		select {
		case <-ctx.Done():
			return snapshot, fmt.Errorf(
				"P2P routes send=%d/%d receive=%d/%d generation=%d: %w",
				snapshot.ActiveSendRoutes,
				minimumSendRoutes,
				snapshot.ActiveReceiveRoutes,
				minimumReceiveRoutes,
				snapshot.Generation,
				ctx.Err(),
			)
		case <-self.changed:
		}
	}
}

// A fast disconnect/reconnect is retained even when the consumer drains only
// after both callbacks; the final keyed level and generation are exact.
func TestP2pRouteStateTraceRetainsFastReconnect(t *testing.T) {
	trace := newP2pRouteStateTrace()
	peerId := clientconnect.NewId()
	streamId := clientconnect.NewId()
	state := clientconnect.P2pRouteState{
		PeerId:    peerId,
		StreamId:  streamId,
		Send:      true,
		Connected: true,
	}
	trace.Observe(state)
	state.Connected = false
	trace.Observe(state)
	state.Connected = true
	trace.Observe(state)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	snapshot, err := trace.WaitForMinimumRoutes(ctx, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation != 3 || snapshot.ActiveSendRoutes != 1 ||
		snapshot.ActiveReceiveRoutes != 0 ||
		snapshot.ConnectedTransitionCount != 2 ||
		snapshot.DisconnectedTransitionCount != 1 {
		t.Fatalf("fast reconnect snapshot=%+v", snapshot)
	}
}

// A replacement may connect before the old transport disconnects. The old
// disconnect must retire only its own contribution and leave the replacement
// route active until the replacement also disconnects.
func TestP2pRouteStateTraceRetainsOverlappingReplacement(t *testing.T) {
	trace := newP2pRouteStateTrace()
	state := clientconnect.P2pRouteState{
		PeerId:          clientconnect.NewId(),
		StreamId:        clientconnect.NewId(),
		PeerType:        clientconnect.PeerTypeSource,
		RouteManagerTag: "device-send",
		Send:            true,
		Connected:       true,
	}
	trace.Observe(state)
	trace.Observe(state)
	state.Connected = false
	trace.Observe(state)

	snapshot := trace.Snapshot()
	if snapshot.Generation != 3 || snapshot.ActiveSendRoutes != 1 ||
		snapshot.ConnectedTransitionCount != 2 ||
		snapshot.DisconnectedTransitionCount != 1 {
		t.Fatalf("overlapping replacement snapshot=%+v", snapshot)
	}

	trace.Observe(state)
	snapshot = trace.Snapshot()
	if snapshot.Generation != 4 || snapshot.ActiveSendRoutes != 0 ||
		snapshot.ConnectedTransitionCount != 2 ||
		snapshot.DisconnectedTransitionCount != 2 {
		t.Fatalf("retired replacement snapshot=%+v", snapshot)
	}
}

// Independent send and receive keys survive concurrent callback admission
// without a blocking callback or a dropped readiness edge.
func TestP2pRouteStateTraceRetainsConcurrentDirections(t *testing.T) {
	trace := newP2pRouteStateTrace()
	peerId := clientconnect.NewId()
	streamId := clientconnect.NewId()
	release := make(chan struct{})
	done := make(chan struct{}, 2)
	for _, send := range []bool{false, true} {
		go func(send bool) {
			<-release
			trace.Observe(clientconnect.P2pRouteState{
				PeerId:    peerId,
				StreamId:  streamId,
				Send:      send,
				Connected: true,
			})
			done <- struct{}{}
		}(send)
	}
	close(release)
	<-done
	<-done

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	snapshot, err := trace.WaitForMinimumRoutes(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation < 2 || snapshot.ActiveSendRoutes != 1 ||
		snapshot.ActiveReceiveRoutes != 1 {
		t.Fatalf("concurrent route snapshot=%+v", snapshot)
	}
}
