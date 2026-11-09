package connect

// The shared key-event subscriber's half of §6.8's resident publishers (PROTO-M1-09): the {scp_*}n
// pattern only under EnableSidecarPush, an INCR on a client's n kicking that client's publishers and no
// other event kicking anyone, and a (re)subscribe kicking every publisher. Pure: no Redis, no
// PostgreSQL.

import (
	"context"
	"fmt"
	"path"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Counts its kicks on a channel.
type testSidecarPushListener struct {
	kicks chan struct{}
}

func newTestSidecarPushListener() *testSidecarPushListener {
	return &testSidecarPushListener{kicks: make(chan struct{}, 64)}
}

func (self *testSidecarPushListener) Kick() {
	select {
	case self.kicks <- struct{}{}:
	default:
	}
}

func (self *testSidecarPushListener) kickCount() int {
	count := 0
	for {
		select {
		case <-self.kicks:
			count += 1
		default:
			return count
		}
	}
}

// A subscriber with no run loop: only what dispatch, registration and Close read.
func newTestSidecarPushSubscriber(ctx context.Context, sidecarPushPattern bool) *keyEventSubscriber {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &keyEventSubscriber{
		ctx:                  cancelCtx,
		cancel:               cancel,
		settings:             &KeyEventDeliverySettings{ResyncSpreadTimeout: 30 * time.Millisecond},
		peerListeners:        map[server.Id]map[int64]*model.NetworkPeerListener{},
		hopListeners:         map[server.Id]map[int64]*model.StreamHopListener{},
		closeListeners:       map[server.Id]map[int64]*clientCloseListener{},
		sidecarPushListeners: map[server.Id]map[int64]sidecarPushListener{},
		sidecarPushPattern:   sidecarPushPattern,
		newPeerDelta:         model.NewNetworkPeerDelta,
	}
}

// A client's n as its keyspace channel names it on db.
func testScpChannel(db int, clientId server.Id) string {
	return fmt.Sprintf("__keyspace@%d__:%s", db, pushservice.HintSeqKey(connect.Id(clientId)))
}

// Every keyspace notification on every master is matched against every pattern, so the {scp_*}n
// pattern is subscribed only under EnableSidecarPush, as the recycle flags' is under theirs.
func TestKeyEventSubscriberSubscribesScpOnlyUnderEnableSidecarPush(t *testing.T) {
	pattern := model.ScpHintKeyEventPattern(3)
	if pattern != "__keyspace@3__:{scp_*}n" {
		t.Fatalf("pattern %q", pattern)
	}
	off := (&keyEventSubscriber{sidecarPushPattern: false}).patterns(3)
	on := (&keyEventSubscriber{sidecarPushPattern: true}).patterns(3)
	if slices.Contains(off, pattern) || !slices.Contains(on, pattern) || len(on) != len(off)+1 {
		t.Errorf("off %v, on %v", off, on)
	}
	if DefaultExchangeSettings().EnableSidecarPush {
		t.Error("the exchange's default subscribes {scp_*}n")
	}
}

// A client's n, its keyspace channel and the subscribed pattern agree: the channel parses back to its
// client and matches the pattern, while the slot's other keys and other families do neither, and an n
// whose tag holds no id names no client.
func TestScpHintKeyEventRoundTrips(t *testing.T) {
	pattern := model.ScpHintKeyEventPattern(0)
	for range 16 {
		clientId := server.NewId()
		channel := testScpChannel(0, clientId)
		parsed, ok := model.ParseScpHintKeyEvent(channel)
		if !ok || server.Id(parsed) != clientId {
			t.Fatalf("%s parses to %s, %t", channel, parsed, ok)
		}
		if matched, err := path.Match(pattern, channel); err != nil || !matched {
			t.Errorf("%s does not match %s", channel, pattern)
		}
		for _, other := range []string{
			fmt.Sprintf("__keyspace@0__:%s", pushservice.HintStreamKey(connect.Id(clientId))),
			fmt.Sprintf("__keyspace@0__:%s", pushservice.HintVersionsKey(connect.Id(clientId))),
			fmt.Sprintf("__keyspace@0__:{%s}n", clientId),
		} {
			if _, ok := model.ParseScpHintKeyEvent(other); ok {
				t.Errorf("%s parsed as a hint", other)
			}
			if matched, _ := path.Match(pattern, other); matched {
				t.Errorf("%s matches %s", other, pattern)
			}
		}
	}
	if _, ok := model.ParseScpHintKeyEvent("__keyspace@0__:{scp_not-an-id}n"); ok {
		t.Error("an n with no id in its tag parsed")
	}
}

// Only n's INCR (incrby) kicks, and only the client it names: EXPIRE NX's expire, an expiry's expired,
// a del, and HSET's and XADD's events, were they ever on, kick nobody. With the pattern off, the
// subscriber reads no {scp_} event at all.
func TestKeyEventSubscriberKicksAPublisherOnItsIncrAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscriber := newTestSidecarPushSubscriber(ctx, true)
	clientIdA, clientIdB := server.NewId(), server.NewId()
	listenerA, listenerB := newTestSidecarPushListener(), newTestSidecarPushListener()
	removeA := subscriber.AddSidecarPushListener(clientIdA, listenerA)
	defer removeA()
	removeB := subscriber.AddSidecarPushListener(clientIdB, listenerB)
	defer removeB()

	for _, event := range []string{"expire", "expired", "del", "hset", "xadd", "set", "evicted"} {
		subscriber.dispatch(testScpChannel(0, clientIdA), event)
	}
	if count := listenerA.kickCount(); count != 0 {
		t.Errorf("events that are no hint kicked %d times", count)
	}
	subscriber.dispatch(testScpChannel(0, clientIdA), "incrby")
	if listenerA.kickCount() != 1 || listenerB.kickCount() != 0 {
		t.Error("an INCR on a's n did not kick a alone")
	}

	// a second publisher of one client, a resident moving say, is kicked too, and a removed one is not
	listenerA2 := newTestSidecarPushListener()
	removeA2 := subscriber.AddSidecarPushListener(clientIdA, listenerA2)
	subscriber.dispatch(testScpChannel(0, clientIdA), "incrby")
	if listenerA.kickCount() != 1 || listenerA2.kickCount() != 1 {
		t.Error("an INCR did not kick both of a's publishers")
	}
	removeA2()
	subscriber.dispatch(testScpChannel(0, clientIdA), "incrby")
	if listenerA.kickCount() != 1 || listenerA2.kickCount() != 0 {
		t.Error("a removed publisher was kicked")
	}

	off := newTestSidecarPushSubscriber(ctx, false)
	listenerOff := newTestSidecarPushListener()
	removeOff := off.AddSidecarPushListener(clientIdA, listenerOff)
	defer removeOff()
	off.dispatch(testScpChannel(0, clientIdA), "incrby")
	if listenerOff.kickCount() != 0 {
		t.Error("a subscriber without the pattern read an {scp_} event")
	}
}

// A (re)subscribe kicks every publisher, trickled, so whatever was hinted while unsubscribed costs each
// one read; a publisher removed before it is not kicked.
func TestKeyEventSubscriberResubscribeKicksEveryPublisher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscriber := newTestSidecarPushSubscriber(ctx, true)
	listeners := []*testSidecarPushListener{}
	for range 3 {
		listener := newTestSidecarPushListener()
		remove := subscriber.AddSidecarPushListener(server.NewId(), listener)
		defer remove()
		listeners = append(listeners, listener)
	}
	removed := newTestSidecarPushListener()
	subscriber.AddSidecarPushListener(server.NewId(), removed)()

	subscriber.resyncSidecarPushListeners()
	for index, listener := range listeners {
		select {
		case <-listener.kicks:
		case <-time.After(10 * time.Second):
			t.Fatalf("publisher %d was not kicked", index)
		}
	}
	if removed.kickCount() != 0 {
		t.Error("a removed publisher was kicked")
	}
}

// The connect service takes EnableSidecarPush from sidecar.yml's push switch as it reads at start:
// absent or off is off.
func TestExchangeSettingsForRunFollowTheSidecarPushSwitch(t *testing.T) {
	for _, c := range []struct {
		config string
		want   bool
	}{
		{config: "", want: false},
		{config: "sidecar:\n  push: {enabled: false}\n", want: false},
		{config: "sidecar:\n  push: {enabled: true}\n", want: true},
	} {
		func() {
			defer server.Testing_PushSidecarConfig(c.config)()
			settings := exchangeSettingsForRun(RunOptions{Port: 443})
			if settings.EnableSidecarPush != c.want {
				t.Errorf("%q: EnableSidecarPush %t", c.config, settings.EnableSidecarPush)
			}
		}()
	}
}
