// Key-event subscriber tests cover process-wide routing without external
// Redis or PostgreSQL dependencies.
package connect

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// TestKeyEventSubscriberBuildsOnePeerDeltaPerEvent reproduces a network with
// multiple resident listeners and proves the process-wide subscriber fans one
// shared lazy delta out, rather than constructing one Redis read per listener.
func TestKeyEventSubscriberBuildsOnePeerDeltaPerEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const listenerCount = 8
	networkId := server.NewId()
	clientId := server.NewId()
	peerListeners := map[int64]*model.NetworkPeerListener{}
	for i := range listenerCount {
		listener := model.NewNetworkPeerListener(
			ctx,
			networkId,
			func(*model.NetworkPeerEvent) {},
			10*time.Minute,
			0,
			nil,
		)
		// Stop the worker before dispatch so this routing test cannot perform
		// registry I/O. The model-layer test separately proves that listeners
		// consuming the shared object execute its lazy loader exactly once.
		listener.CloseAndWait()
		peerListeners[int64(i)] = listener
	}

	var deltaCount atomic.Int64
	subscriber := &keyEventSubscriber{
		ctx: ctx,
		peerListeners: map[server.Id]map[int64]*model.NetworkPeerListener{
			networkId: peerListeners,
		},
		hopListeners: map[server.Id]map[int64]*model.StreamHopListener{},
		newPeerDelta: func(
			ctx context.Context,
			networkId server.Id,
			clientId server.Id,
			event string,
		) *model.NetworkPeerDelta {
			deltaCount.Add(1)
			return model.NewNetworkPeerDelta(ctx, networkId, clientId, event)
		},
	}
	channel := fmt.Sprintf(
		"__keyspace@0__:{np_%s}p:%s",
		networkId,
		clientId,
	)
	subscriber.dispatch(channel, "set")

	if got := deltaCount.Load(); got != 1 {
		t.Fatalf("subscriber built %d deltas for one event; want 1", got)
	}
}

// Close listeners whose kicks are recorded at their bubble time. A listener
// has no worker: in production each kick is one Redis take.
type closeResyncTestListeners struct {
	stateLock sync.Mutex
	// each listener's kick times, by its index
	kickTimes [][]time.Time
}

func newCloseResyncTestListeners(ctx context.Context, subscriber *keyEventSubscriber, count int) *closeResyncTestListeners {
	testListeners := &closeResyncTestListeners{kickTimes: make([][]time.Time, count)}
	for i := range count {
		listener := &clientCloseListener{kick: make(chan struct{}, 1)}
		subscriber.AddCloseListener(server.NewId(), listener)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-listener.kick:
					testListeners.stateLock.Lock()
					testListeners.kickTimes[i] = append(testListeners.kickTimes[i], time.Now())
					testListeners.stateLock.Unlock()
				}
			}
		}()
	}
	return testListeners
}

// Each listener's kick count in [start, end].
func (self *closeResyncTestListeners) kickCounts(start time.Time, end time.Time) []int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	counts := make([]int, len(self.kickTimes))
	for i, kickTimes := range self.kickTimes {
		for _, kickTime := range kickTimes {
			if !kickTime.Before(start) && !kickTime.After(end) {
				counts[i] += 1
			}
		}
	}
	return counts
}

func newCloseResyncTestSubscriber(ctx context.Context) *keyEventSubscriber {
	return &keyEventSubscriber{
		ctx:            ctx,
		settings:       &KeyEventDeliverySettings{ResyncSpreadTimeout: 30 * time.Second},
		closeListeners: map[server.Id]map[int64]*clientCloseListener{},
	}
}

// Every close-listener wave runs to the end, so each listener is kicked within
// the spread of every resubscribe. Requests added while unsubscribed have no
// other backstop: the corrective poll does not kick close listeners. A newer
// wave that canceled an older one, as resyncAll's do, would leave the
// listeners late in the order unkicked while resubscribes came faster than
// the spread, as they did every ~8.3 s on 2026-11-15 (CPUOPT6, rejected there
// as a loosened delivery bound).
func TestCloseListenerResyncKicksEveryListenerWithinEachSpread(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		subscriber := newCloseResyncTestSubscriber(ctx)
		const listenerCount = 30
		listeners := newCloseResyncTestListeners(ctx, subscriber, listenerCount)
		spread := subscriber.settings.ResyncSpreadTimeout

		start := time.Now()
		// resubscribes every 8.25 s, off the 1 s kick spacing
		resyncOffsets := []time.Duration{0, 8250 * time.Millisecond, 16500 * time.Millisecond, 24750 * time.Millisecond}
		for _, offset := range resyncOffsets {
			time.Sleep(start.Add(offset).Sub(time.Now()))
			subscriber.resyncCloseListeners()
			synctest.Wait()
		}
		time.Sleep(start.Add(resyncOffsets[len(resyncOffsets)-1]).Add(2 * spread).Sub(time.Now()))
		synctest.Wait()

		for _, offset := range resyncOffsets {
			resync := start.Add(offset)
			for i, count := range listeners.kickCounts(resync, resync.Add(spread)) {
				if count == 0 {
					t.Errorf("listener %d was not kicked within %s of the resubscribe at +%s", i, spread, offset)
				}
			}
		}
	})
}
