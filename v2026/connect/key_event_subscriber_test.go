// Key-event subscriber tests cover process-wide routing without external
// Redis or PostgreSQL dependencies.
package connect

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
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
