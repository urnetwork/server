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

	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
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

// A session revocation event rechecks every listener of its network, and only
// that network: the event carries no session id, so each local connection of
// the network must revalidate. Only the increment is a report; the event
// key's ttl and expiry are not.
func TestSessionRevocationEventRechecksEveryListenerOfItsNetwork(t *testing.T) {
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

		channel := "__keyspace@0__:{ns_" + networkId.String() + "}rid"
		for _, event := range []string{"expire", "expired", "del"} {
			subscriber.dispatch(channel, event)
		}
		synctest.Wait()
		if got := rechecks.Load() - before; got != 0 {
			t.Fatalf("the revocation event key's ttl and expiry rechecked %d listeners", got)
		}

		subscriber.dispatch(channel, "incrby")
		synctest.Wait()
		if got := rechecks.Load() - before; got != 2 {
			t.Fatalf("session revocation event rechecked %d of the network's 2 listeners", got)
		}
		if got := otherRechecks.Load() - otherBefore; got != 0 {
			t.Fatalf("session revocation event rechecked %d listeners of another network", got)
		}

		for _, remove := range removes {
			remove()
		}
	})
}

// S1 W8, S2 N7: the inventory revision (eid) changes on every new session,
// and its event rechecked every lease of the network on every process. With
// creation on, each sign-in, code login, hosted proxy bootstrap or API-key
// client would have cost one authoritative check per connection of its
// network. A change of the inventory refreshes the hints and rechecks no
// lease: only a revocation (TestSessionRevocationEventRechecksEveryListenerOfItsNetwork)
// can end a transport's authority.
func TestSessionInventoryEventRechecksNoLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var revisionReads atomic.Int64
		subscriber := newSessionFanoutSubscriber(ctx, &revisionReads)

		networkId := server.NewId()
		var rechecks atomic.Int64
		var hints atomic.Int64
		removes := []func(){
			subscriber.AddSessionListener(networkId, func() { rechecks.Add(1) }, nil),
			subscriber.AddSessionListener(networkId, func() { rechecks.Add(1) }, nil),
			subscriber.AddSessionListener(networkId, nil, func(*protocol.NetworkSessionsChanged) { hints.Add(1) }),
		}
		// past the registration hint's coalescing window
		time.Sleep(10 * time.Second)
		synctest.Wait()
		rechecksBefore, hintsBefore := rechecks.Load(), hints.Load()

		// a session created, then the initialization of a missing revision
		channel := "__keyspace@0__:{ns_" + networkId.String() + "}eid"
		subscriber.dispatch(channel, "incrby")
		subscriber.dispatch(channel, "set")
		time.Sleep(10 * time.Second)
		synctest.Wait()

		if got := rechecks.Load() - rechecksBefore; got != 0 {
			t.Fatalf("an inventory change rechecked %d transport leases", got)
		}
		if got := hints.Load() - hintsBefore; got == 0 {
			t.Fatal("an inventory change refreshed no hint")
		}

		for _, remove := range removes {
			remove()
		}
	})
}

// Main 2026-10-26: each network's corrective 60 second pass rechecked every
// lease of the network, while each lease also rechecks on its own 60 second
// timer, so every connection cost two authoritative PostgreSQL checks a
// minute (connect's untagged state queries ran at 7.9k/s for 203k
// connections). The pass refreshes hints; the lease timers keep the minute
// bound, and session revocation events still recheck at once.
func TestSessionCorrectivePassRefreshesHintsWithoutRechecks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var revisionReads atomic.Int64
		subscriber := newSessionFanoutSubscriber(ctx, &revisionReads)

		networkId := server.NewId()
		const listenerCount = 3
		rechecks := make([]atomic.Int64, listenerCount)
		var hints atomic.Int64
		removes := []func(){
			subscriber.AddSessionListener(networkId, nil, func(*protocol.NetworkSessionsChanged) { hints.Add(1) }),
		}
		for i := range listenerCount {
			removes = append(removes, subscriber.AddSessionListener(networkId, func() { rechecks[i].Add(1) }, nil))
		}
		synctest.Wait()
		hintsBefore := hints.Load()

		// three corrective passes
		time.Sleep(3*time.Minute + 30*time.Second)
		synctest.Wait()

		for i := range listenerCount {
			if got := rechecks[i].Load(); got != 1 {
				t.Errorf("listener %d rechecked %d times over three corrective passes; want only its registration recheck", i, got)
			}
		}
		if got := hints.Load() - hintsBefore; got < 3 {
			t.Errorf("hint listener received %d hints over three corrective passes; want at least 3", got)
		}

		// a session revocation event still rechecks each listener of the
		// network once
		subscriber.dispatch("__keyspace@0__:{ns_"+networkId.String()+"}rid", "incrby")
		synctest.Wait()
		for i := range listenerCount {
			if got := rechecks[i].Load(); got != 2 {
				t.Errorf("listener %d rechecked %d times after a session revocation event; want 2", i, got)
			}
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

// A (re)subscribe and every corrective poll resync all registrations, and the
// settings promise those resyncs trickle over ResyncSpreadTimeout. Session
// listeners were instead all rechecked at once, so every resubscribe made each
// lease of the process start a PostgreSQL check in the same instant.
func TestKeyEventResyncSpreadsSessionRechecks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var revisionReads atomic.Int64
		subscriber := newSessionFanoutSubscriber(ctx, &revisionReads)
		subscriber.settings = &KeyEventDeliverySettings{ResyncSpreadTimeout: 30 * time.Second}

		const listenerCount = 6
		networkIds := []server.Id{server.NewId(), server.NewId()}
		var rechecks atomic.Int64
		removes := []func(){}
		for i := range listenerCount {
			removes = append(removes, subscriber.AddSessionListener(networkIds[i%len(networkIds)], func() { rechecks.Add(1) }, nil))
		}
		synctest.Wait()
		registered := rechecks.Load()

		subscriber.resyncAll()
		synctest.Wait()
		if immediate := rechecks.Load() - registered; 1 < immediate {
			t.Fatalf("resync rechecked %d of %d listeners at once", immediate, listenerCount)
		}

		time.Sleep(30 * time.Second)
		synctest.Wait()
		if resynced := rechecks.Load() - registered; resynced != listenerCount {
			t.Fatalf("resync rechecked %d of %d listeners within its spread window", resynced, listenerCount)
		}

		for _, remove := range removes {
			remove()
		}
	})
}

// S1 W11: every resident was sent its network's revision every minute,
// whether or not it changed. A hint listener now gets each revision once: at
// registration, and when the revision changes. A listener joining later still
// gets the current one.
func TestSessionHintsSkipAnUnchangedRevision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var eventId atomic.Int64
		eventId.Store(7)
		subscriber := &keyEventSubscriber{
			ctx: ctx,
			sessionRevision: func(ctx context.Context, networkId server.Id) (string, int64, error) {
				return "synthetic-generation", eventId.Load(), nil
			},
		}
		networkId := server.NewId()
		var firstHints, secondHints atomic.Int64
		removeFirst := subscriber.AddSessionListener(networkId, nil, func(*protocol.NetworkSessionsChanged) { firstHints.Add(1) })
		defer removeFirst()
		// the registration's hint, then three corrective passes
		time.Sleep(3*time.Minute + 30*time.Second)
		synctest.Wait()
		if got := firstHints.Load(); got != 1 {
			t.Fatalf("an unchanged revision was hinted %d times, want once", got)
		}

		eventId.Store(8)
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := firstHints.Load(); got != 2 {
			t.Fatalf("a changed revision brought the listener to %d hints, want 2", got)
		}

		removeSecond := subscriber.AddSessionListener(networkId, nil, func(*protocol.NetworkSessionsChanged) { secondHints.Add(1) })
		defer removeSecond()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if firstHints.Load() != 2 || secondHints.Load() != 1 {
			t.Fatalf("a new listener got %d hints and the first %d, want 1 and still 2", secondHints.Load(), firstHints.Load())
		}
	})
}
