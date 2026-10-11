package connect

// Session listener fan-out without Redis or PostgreSQL. Every recheck callback
// stands in for one authoritative lease check, a PostgreSQL read in
// production, and the injected revision read stands in for the Redis script
// that returns every live session of the network. The per-network
// notification goroutine runs in a synctest bubble, so its 60 second ticker
// and 5 second hint coalescing advance on the bubble's virtual clock.

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
)

func newSessionFanoutSubscriber(ctx context.Context, revisionReads *atomic.Int64) *keyEventSubscriber {
	return &keyEventSubscriber{
		ctx: ctx,
		sessionRevision: func(ctx context.Context, networkId server.Id) (string, int64, error) {
			revisionReads.Add(1)
			return "synthetic-generation", revisionReads.Load(), nil
		},
	}
}

// Main 2026-10-10: each new connection registered its lease and rechecked
// every other lease of the same network on the process, so a reconnect wave
// in a network with about a thousand connections per process issued about a
// thousand PostgreSQL checks per reconnect. A peer joining changes no other
// connection's authorization; only the new listener can have missed a session
// event, between its final check and this registration.
func TestSessionListenerRegistrationRechecksOnlyTheNewListener(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var revisionReads atomic.Int64
		subscriber := newSessionFanoutSubscriber(ctx, &revisionReads)

		networkId := server.NewId()
		otherNetworkId := server.NewId()
		const listenerCount = 4
		rechecks := make([]atomic.Int64, listenerCount)
		var otherRechecks atomic.Int64
		removes := []func(){
			subscriber.AddSessionListener(otherNetworkId, func() { otherRechecks.Add(1) }, nil),
		}
		for i := range listenerCount {
			removes = append(removes, subscriber.AddSessionListener(networkId, func() { rechecks[i].Add(1) }, nil))
		}
		synctest.Wait()

		for i := range listenerCount {
			if got := rechecks[i].Load(); got != 1 {
				t.Errorf("listener %d rechecked %d times after %d registrations; want 1", i, got, listenerCount)
			}
		}
		if got := otherRechecks.Load(); got != 1 {
			t.Errorf("another network's listener rechecked %d times; want 1", got)
		}

		for _, remove := range removes {
			remove()
		}
	})
}

// A resident registers a hint listener with no recheck of its own. Its
// registration used to recheck every transport lease of its network on the
// process; it must recheck none.
func TestSessionHintListenerRegistrationRechecksNoListener(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var revisionReads atomic.Int64
		subscriber := newSessionFanoutSubscriber(ctx, &revisionReads)

		networkId := server.NewId()
		var rechecks atomic.Int64
		removes := []func(){
			subscriber.AddSessionListener(networkId, func() { rechecks.Add(1) }, nil),
			subscriber.AddSessionListener(networkId, func() { rechecks.Add(1) }, nil),
		}
		synctest.Wait()
		before := rechecks.Load()

		removes = append(removes, subscriber.AddSessionListener(networkId, nil, func(*protocol.NetworkSessionsChanged) {}))
		synctest.Wait()
		if got := rechecks.Load() - before; got != 0 {
			t.Fatalf("a resident registration rechecked %d transport leases", got)
		}

		for _, remove := range removes {
			remove()
		}
	})
}

// A session event still rechecks every listener of its network, and only that
// network: the event carries no session id, so each local connection of the
// network must revalidate.
func TestSessionEventRechecksEveryListenerOfItsNetwork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var revisionReads atomic.Int64
		subscriber := newSessionFanoutSubscriber(ctx, &revisionReads)

		networkId := server.NewId()
		otherNetworkId := server.NewId()
		var rechecks atomic.Int64
		var otherRechecks atomic.Int64
		removes := []func(){
			subscriber.AddSessionListener(networkId, func() { rechecks.Add(1) }, nil),
			subscriber.AddSessionListener(networkId, func() { rechecks.Add(1) }, nil),
			subscriber.AddSessionListener(otherNetworkId, func() { otherRechecks.Add(1) }, nil),
		}
		synctest.Wait()
		before, otherBefore := rechecks.Load(), otherRechecks.Load()

		subscriber.dispatch("__keyspace@0__:{ns_"+networkId.String()+"}eid", "incr")
		synctest.Wait()

		if got := rechecks.Load() - before; got != 2 {
			t.Fatalf("session event rechecked %d of the network's 2 listeners", got)
		}
		if got := otherRechecks.Load() - otherBefore; got != 0 {
			t.Fatalf("session event rechecked %d listeners of another network", got)
		}

		for _, remove := range removes {
			remove()
		}
	})
}

// The revision read exists only to produce hints. Transport listeners register
// no hint callback, so the read returned every live session of the network to
// no consumer, and its prune could publish a session event that rechecks every
// listener of the network on every process.
func TestSessionNotificationsSkipRevisionReadWithoutHintListeners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var revisionReads atomic.Int64
		subscriber := newSessionFanoutSubscriber(ctx, &revisionReads)

		networkId := server.NewId()
		removeTransport := subscriber.AddSessionListener(networkId, func() {}, nil)
		// the registration kick and three corrective ticks
		time.Sleep(3*time.Minute + 30*time.Second)
		synctest.Wait()
		if got := revisionReads.Load(); got != 0 {
			t.Fatalf("read the session revision %d times with no hint listener", got)
		}

		// a hint listener still receives the current revision
		var hints atomic.Int64
		removeHint := subscriber.AddSessionListener(networkId, nil, func(*protocol.NetworkSessionsChanged) { hints.Add(1) })
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if revisionReads.Load() == 0 || hints.Load() == 0 {
			t.Fatalf("hint listener received %d hints from %d revision reads", hints.Load(), revisionReads.Load())
		}

		removeTransport()
		removeHint()
	})
}
