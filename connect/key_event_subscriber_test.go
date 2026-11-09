// Key-event subscriber tests cover process-wide routing without external
// Redis or PostgreSQL dependencies.
package connect

import (
	"context"
	"fmt"
	"path"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
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

// A network's {acl_}v as its keyspace channel names it on db.
func testAclVersionChannel(db int, networkId server.Id) string {
	return fmt.Sprintf("__keyspace@%d__:%s", db, sidecar.AclVersionKey(sidecar.Id(networkId)))
}

// Every keyspace notification on every master is matched against every pattern, so the {acl_*}v pattern is
// subscribed only where the process hosts §8.9's gate cache (EnableAclGate, off by default), and it matches the
// channel api's INCR names, which parses back to its network.
func TestKeyEventSubscriberSubscribesAclVersionOnlyWhereAGateIs(t *testing.T) {
	pattern := model.AclVersionKeyEventPattern(3)
	if pattern != "__keyspace@3__:{acl_*}v" {
		t.Fatalf("pattern %q", pattern)
	}
	off := (&keyEventSubscriber{}).patterns(3)
	on := (&keyEventSubscriber{aclGatePattern: true}).patterns(3)
	if slices.Contains(off, pattern) || !slices.Contains(on, pattern) || len(on) != len(off)+1 {
		t.Errorf("off %v, on %v", off, on)
	}
	if DefaultExchangeSettings().EnableAclGate {
		t.Error("an exchange hosts the gate cache by default")
	}
	networkId := server.NewId()
	channel := testAclVersionChannel(3, networkId)
	if matched, err := path.Match(pattern, channel); err != nil || !matched {
		t.Errorf("%q does not match %q: %v", pattern, channel, err)
	}
	if parsed, ok := model.ParseAclVersionKeyEvent(channel); !ok || parsed != networkId {
		t.Errorf("parsed %v, %v", parsed, ok)
	}
	for _, other := range []string{
		testScpChannel(3, networkId),
		fmt.Sprintf("__keyspace@3__:{acl_%s}e", networkId),
		fmt.Sprintf("__keyspace@3__:{acl_%s}q", networkId),
		"__keyspace@3__:{acl_not-an-id}v",
		"{acl_" + networkId.String() + "}v",
	} {
		if _, ok := model.ParseAclVersionKeyEvent(other); ok {
			t.Errorf("%q parsed", other)
		}
	}
}

// api's INCR of a network's {acl_}v reaches the gate cache's Changed with that network, and nothing else does: not
// the ttl's expire, an expiry, a del or a set, nor any event at a subscriber without the pattern; every
// (re)subscription runs the cache's Resync.
func TestKeyEventSubscriberRoutesAclVersionToTheGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan server.Id, 16)
	resyncs := atomic.Int32{}
	subscriber := newTestSidecarPushSubscriber(ctx, false)
	subscriber.aclGatePattern = true
	subscriber.SetAclGate(func(networkId server.Id) { changed <- networkId }, func() { resyncs.Add(1) })
	drained := func() []server.Id {
		ids := []server.Id{}
		for {
			select {
			case networkId := <-changed:
				ids = append(ids, networkId)
			default:
				return ids
			}
		}
	}

	networkId := server.NewId()
	subscriber.dispatch(testAclVersionChannel(0, networkId), "incrby")
	if ids := drained(); len(ids) != 1 || ids[0] != networkId {
		t.Fatalf("the INCR: %v", ids)
	}
	for _, event := range []string{"expire", "expired", "del", "set"} {
		subscriber.dispatch(testAclVersionChannel(0, networkId), event)
	}
	if ids := drained(); len(ids) != 0 {
		t.Errorf("another event reached the gate: %v", ids)
	}

	without := newTestSidecarPushSubscriber(ctx, false)
	without.SetAclGate(func(networkId server.Id) { changed <- networkId }, func() { resyncs.Add(1) })
	without.dispatch(testAclVersionChannel(0, networkId), "incrby")
	if ids := drained(); len(ids) != 0 {
		t.Errorf("a subscriber without the pattern routed it: %v", ids)
	}

	subscriber.resyncAclGate()
	if resyncs.Load() != 1 {
		t.Errorf("%d resyncs", resyncs.Load())
	}
}
