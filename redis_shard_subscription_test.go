package server

// Synthetic sockets replay what a cluster node sends a shard subscriber around a
// slot move (see TestRedisShardSubscriptionFollowsSlotMigration for the same
// sequence on a real cluster). All timing runs on the synctest clock, without
// Redis or wall-clock races.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/redis/go-redis/v9"
)

const testRedisShardChannel = "synthetic-change:{g7}"

// A synthetic shard socket. The test feeds its events; an error event is
// returned as a read error.
type testRedisShardSocket struct {
	channel      string
	events       chan any
	closed       chan struct{}
	closeOnce    sync.Once
	pingResponse func() any
	pings        atomic.Int32
	reasserts    atomic.Int32
}

func (self *testRedisShardSocket) ReceiveTimeout(ctx context.Context, timeout time.Duration) (any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-self.closed:
		return nil, errors.New("synthetic socket closed")
	case event := <-self.events:
		if err, ok := event.(error); ok {
			return nil, err
		}
		return event, nil
	case <-time.After(timeout):
		return nil, context.DeadlineExceeded
	}
}

// The test answers a re-assertion itself, after it observes the count.
func (self *testRedisShardSocket) SSubscribe(context.Context, ...string) error {
	self.reasserts.Add(1)
	return nil
}

func (self *testRedisShardSocket) Ping(context.Context, ...string) error {
	self.pings.Add(1)
	if self.pingResponse != nil {
		self.events <- self.pingResponse()
	}
	return nil
}

func (self *testRedisShardSocket) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}

func (self *testRedisShardSocket) isClosed() bool {
	select {
	case <-self.closed:
		return true
	default:
		return false
	}
}

// Opens synthetic sockets whose pings the owner answers, and counts open
// attempts and slot map reloads.
type testRedisShardSource struct {
	opened        chan *testRedisShardSocket
	failOpen      atomic.Bool
	stateLock     sync.Mutex
	openTimes     []time.Time
	reloadCount   atomic.Int32
	answeredPings bool
}

func newTestRedisShardSource(answeredPings bool) *testRedisShardSource {
	return &testRedisShardSource{
		opened:        make(chan *testRedisShardSocket, 64),
		answeredPings: answeredPings,
	}
}

func (self *testRedisShardSource) source() *RedisShardSubscriptionSource {
	return &RedisShardSubscriptionSource{
		Open: func(ctx context.Context, channel string) (RedisShardSubscription, error) {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.openTimes = append(self.openTimes, time.Now())
			}()
			if self.failOpen.Load() {
				return nil, errors.New("synthetic cluster unavailable")
			}
			socket := &testRedisShardSocket{
				channel: channel,
				events:  make(chan any, 64),
				closed:  make(chan struct{}),
			}
			if self.answeredPings {
				socket.pingResponse = func() any { return &redis.Pong{} }
			}
			self.opened <- socket
			return socket, nil
		},
		ReloadRoutes: func(context.Context) {
			self.reloadCount.Add(1)
		},
	}
}

func (self *testRedisShardSource) attemptTimes() []time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.openTimes)
}

func (self *testRedisShardSource) requireOpened(t *testing.T, message string) *testRedisShardSocket {
	select {
	case socket := <-self.opened:
		return socket
	default:
		t.Fatal(message)
		return nil
	}
}

func (self *testRedisShardSource) requireNotOpened(t *testing.T, message string) {
	select {
	case <-self.opened:
		t.Fatal(message)
	default:
	}
}

// Records what the subscription reported to its consumer.
type testRedisShardConsumer struct {
	stateLock       sync.Mutex
	resubscribeds   []bool
	payloads        []string
	lossCount       int
	invalidPayloads map[string]bool
}

func (self *testRedisShardConsumer) handler() *RedisShardSubscriptionHandler {
	return &RedisShardSubscriptionHandler{
		Subscribed: func(resubscribed bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.resubscribeds = append(self.resubscribeds, resubscribed)
		},
		Message: func(message RedisMessage) bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if message.Channel != testRedisShardChannel || self.invalidPayloads[message.Payload] {
				return false
			}
			self.payloads = append(self.payloads, message.Payload)
			return true
		},
		Lost: func(error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.lossCount += 1
		},
	}
}

func (self *testRedisShardConsumer) requireSubscriptions(t *testing.T, resubscribeds ...bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !slices.Equal(self.resubscribeds, resubscribeds) {
		t.Fatalf("subscriptions reported=%v, want %v", self.resubscribeds, resubscribeds)
	}
}

func (self *testRedisShardConsumer) requirePayloads(t *testing.T, payloads ...string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !slices.Equal(self.payloads, payloads) {
		t.Fatalf("payloads delivered=%v, want %v", self.payloads, payloads)
	}
}

// Runs the subscription until the test ends, then joins it.
func runTestRedisShardSubscription(
	t *testing.T,
	settings *RedisShardSubscriptionSettings,
	source *testRedisShardSource,
	consumer *testRedisShardConsumer,
) {
	ctx, cancel := context.WithCancel(t.Context())
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		RunRedisShardSubscription(ctx, testRedisShardChannel, settings, source.source(), consumer.handler())
	}()
	t.Cleanup(func() {
		cancel()
		<-returned
	})
}

func testRedisShardAcknowledgement(channel string) *redis.Subscription {
	return &redis.Subscription{Kind: "ssubscribe", Channel: channel, Count: 1}
}

// The root cause of the 2026-10-12 proxy outage: the node that gives up a slot
// sends sunsubscribe and keeps the socket open. The subscriber must reload the
// slot map, open a subscription on the new owner after the backoff floor, and
// receive later messages there.
func TestRedisShardSubscriptionResubscribesAfterSlotMove(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultRedisShardSubscriptionSettings()
		source := newTestRedisShardSource(true)
		consumer := &testRedisShardConsumer{}
		runTestRedisShardSubscription(t, settings, source, consumer)
		synctest.Wait()

		first := source.requireOpened(t, "no subscription opened")
		first.events <- testRedisShardAcknowledgement(testRedisShardChannel)
		first.events <- &redis.Message{Channel: testRedisShardChannel, Payload: "before-move"}
		synctest.Wait()
		consumer.requireSubscriptions(t, false)

		// the old owner drops the subscription but keeps the socket
		first.events <- &redis.Subscription{Kind: "sunsubscribe", Channel: testRedisShardChannel, Count: 0}
		synctest.Wait()
		if !first.isClosed() {
			t.Fatal("the dropped subscription kept its socket to the old owner")
		}
		if reloads := source.reloadCount.Load(); reloads != 1 {
			t.Fatalf("slot map reloads=%d, want 1 before resubscribing", reloads)
		}
		source.requireNotOpened(t, "resubscribed before the backoff floor")

		time.Sleep(settings.MinReconnectTimeout)
		synctest.Wait()
		second := source.requireOpened(t, "the moved slot was never subscribed again")
		if second.channel != testRedisShardChannel {
			t.Fatalf("resubscribed channel=%s, want %s", second.channel, testRedisShardChannel)
		}
		second.events <- testRedisShardAcknowledgement(testRedisShardChannel)
		second.events <- &redis.Message{Channel: testRedisShardChannel, Payload: "after-move"}
		synctest.Wait()
		// the new subscription is reported as one that follows a gap
		consumer.requireSubscriptions(t, false, true)
		consumer.requirePayloads(t, "before-move", "after-move")
		if second.isClosed() {
			t.Fatal("the new owner's subscription was closed")
		}
	})
}

// A subscription opened by a stale slot map reaches the old owner, which
// answers MOVED and never acknowledges. The slot map is reloaded and the next
// open reaches the owner.
func TestRedisShardSubscriptionReloadsRoutesAfterMovedReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultRedisShardSubscriptionSettings()
		source := newTestRedisShardSource(true)
		consumer := &testRedisShardConsumer{}
		runTestRedisShardSubscription(t, settings, source, consumer)
		synctest.Wait()

		stale := source.requireOpened(t, "no subscription opened")
		stale.events <- errors.New("MOVED 1730 192.0.2.10:6379")
		synctest.Wait()
		if !stale.isClosed() {
			t.Fatal("a MOVED reply kept the socket to the old owner")
		}
		if reloads := source.reloadCount.Load(); reloads != 1 {
			t.Fatalf("slot map reloads=%d, want 1", reloads)
		}
		consumer.requireSubscriptions(t)

		time.Sleep(settings.MinReconnectTimeout)
		synctest.Wait()
		owner := source.requireOpened(t, "a MOVED reply was never retried")
		owner.events <- testRedisShardAcknowledgement(testRedisShardChannel)
		synctest.Wait()
		// no subscription existed before, so this one follows no gap
		consumer.requireSubscriptions(t, false)
	})
}

// An open that is never acknowledged is replaced at the acknowledgement
// timeout; other incoming traffic does not stand in for the acknowledgement.
func TestRedisShardSubscriptionRequiresAcknowledgement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultRedisShardSubscriptionSettings()
		source := newTestRedisShardSource(true)
		consumer := &testRedisShardConsumer{}
		runTestRedisShardSubscription(t, settings, source, consumer)
		synctest.Wait()

		unacknowledged := source.requireOpened(t, "no subscription opened")
		time.Sleep(settings.AcknowledgementTimeout / 2)
		unacknowledged.events <- &redis.Pong{}
		unacknowledged.events <- &redis.Message{Channel: testRedisShardChannel, Payload: "unacknowledged"}
		synctest.Wait()
		if unacknowledged.isClosed() {
			t.Fatal("replaced before the acknowledgement timeout")
		}
		time.Sleep(settings.AcknowledgementTimeout / 2)
		synctest.Wait()
		if !unacknowledged.isClosed() {
			t.Fatal("incoming traffic hid a missing acknowledgement")
		}
		consumer.requireSubscriptions(t)
		time.Sleep(settings.MinReconnectTimeout)
		synctest.Wait()
		source.requireOpened(t, "a missing acknowledgement was never retried")
	})
}

// A live subscription is re-asserted on its socket. The owner's acknowledgement
// keeps it without a resync; a node that lost the slot answers MOVED, and the
// subscription moves.
func TestRedisShardSubscriptionReassertsLiveSubscription(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultRedisShardSubscriptionSettings()
		source := newTestRedisShardSource(true)
		consumer := &testRedisShardConsumer{}
		runTestRedisShardSubscription(t, settings, source, consumer)
		synctest.Wait()

		first := source.requireOpened(t, "no subscription opened")
		first.events <- testRedisShardAcknowledgement(testRedisShardChannel)
		synctest.Wait()

		time.Sleep(settings.ReassertTimeout)
		synctest.Wait()
		if reasserts := first.reasserts.Load(); reasserts != 1 {
			t.Fatalf("re-assertions=%d, want 1", reasserts)
		}
		first.events <- testRedisShardAcknowledgement(testRedisShardChannel)
		synctest.Wait()
		if first.isClosed() {
			t.Fatal("an acknowledged re-assertion replaced the socket")
		}
		source.requireNotOpened(t, "an acknowledged re-assertion opened another socket")
		consumer.requireSubscriptions(t, false)

		time.Sleep(settings.ReassertTimeout)
		synctest.Wait()
		if reasserts := first.reasserts.Load(); reasserts != 2 {
			t.Fatalf("re-assertions=%d, want 2", reasserts)
		}
		// the node no longer owns the slot, and no sunsubscribe was seen
		first.events <- errors.New("MOVED 1730 192.0.2.11:6379")
		synctest.Wait()
		if !first.isClosed() {
			t.Fatal("a MOVED re-assertion kept the socket")
		}
		time.Sleep(settings.MinReconnectTimeout)
		synctest.Wait()
		second := source.requireOpened(t, "a MOVED re-assertion never resubscribed")
		second.events <- testRedisShardAcknowledgement(testRedisShardChannel)
		synctest.Wait()
		consumer.requireSubscriptions(t, false, true)
	})
}

// A re-assertion that is never acknowledged replaces the socket, even while
// pings are answered.
func TestRedisShardSubscriptionReplacesUnacknowledgedReassertion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultRedisShardSubscriptionSettings()
		source := newTestRedisShardSource(true)
		consumer := &testRedisShardConsumer{}
		runTestRedisShardSubscription(t, settings, source, consumer)
		synctest.Wait()

		first := source.requireOpened(t, "no subscription opened")
		first.events <- testRedisShardAcknowledgement(testRedisShardChannel)
		synctest.Wait()

		time.Sleep(settings.ReassertTimeout)
		synctest.Wait()
		if reasserts := first.reasserts.Load(); reasserts != 1 {
			t.Fatalf("re-assertions=%d, want 1", reasserts)
		}
		time.Sleep(settings.AcknowledgementTimeout)
		synctest.Wait()
		if !first.isClosed() {
			t.Fatal("pongs hid an unacknowledged re-assertion")
		}
		time.Sleep(settings.MinReconnectTimeout)
		synctest.Wait()
		source.requireOpened(t, "an unacknowledged re-assertion never resubscribed")
	})
}

// Notices for other channels on the socket change nothing.
func TestRedisShardSubscriptionIgnoresOtherChannelNotices(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultRedisShardSubscriptionSettings()
		source := newTestRedisShardSource(true)
		consumer := &testRedisShardConsumer{}
		runTestRedisShardSubscription(t, settings, source, consumer)
		synctest.Wait()

		socket := source.requireOpened(t, "no subscription opened")
		socket.events <- testRedisShardAcknowledgement(testRedisShardChannel)
		socket.events <- &redis.Subscription{Kind: "sunsubscribe", Channel: "synthetic-other:{g8}", Count: 0}
		socket.events <- testRedisShardAcknowledgement("synthetic-other:{g8}")
		synctest.Wait()
		if socket.isClosed() {
			t.Fatal("another channel's sunsubscribe closed the socket")
		}
		consumer.requireSubscriptions(t, false)
	})
}

// Consecutive failures back off from the minimum, doubling to the maximum; an
// acknowledged subscription resets the backoff.
func TestRedisShardSubscriptionBacksOffRepeatedFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultRedisShardSubscriptionSettings()
		source := newTestRedisShardSource(true)
		source.failOpen.Store(true)
		consumer := &testRedisShardConsumer{}
		startTime := time.Now()
		runTestRedisShardSubscription(t, settings, source, consumer)

		time.Sleep(45 * time.Second)
		synctest.Wait()
		source.failOpen.Store(false)
		var attemptOffsets []time.Duration
		for _, attemptTime := range source.attemptTimes() {
			attemptOffsets = append(attemptOffsets, attemptTime.Sub(startTime))
		}
		wantOffsets := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second, 15 * time.Second, 30 * time.Second, 45 * time.Second}
		if !slices.Equal(attemptOffsets, wantOffsets) {
			t.Fatalf("attempt offsets=%v, want %v", attemptOffsets, wantOffsets)
		}

		// the attempt at 45s failed; the next one waits the 15s maximum
		time.Sleep(settings.MaxReconnectTimeout)
		synctest.Wait()
		socket := source.requireOpened(t, "recovery never opened a subscription")
		socket.events <- testRedisShardAcknowledgement(testRedisShardChannel)
		synctest.Wait()
		socket.events <- &redis.Subscription{Kind: "sunsubscribe", Channel: testRedisShardChannel, Count: 0}
		synctest.Wait()
		time.Sleep(settings.MinReconnectTimeout)
		synctest.Wait()
		source.requireOpened(t, "an acknowledged subscription did not reset the backoff")
	})
}

// Silence past the incoming timeout replaces the socket even though the ping
// writes succeed; valid messages and pongs keep it.
func TestRedisShardSubscriptionReplacesSilentSocket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultRedisShardSubscriptionSettings()
		source := newTestRedisShardSource(false)
		consumer := &testRedisShardConsumer{invalidPayloads: map[string]bool{"synthetic-invalid": true}}
		runTestRedisShardSubscription(t, settings, source, consumer)
		synctest.Wait()

		socket := source.requireOpened(t, "no subscription opened")
		socket.events <- testRedisShardAcknowledgement(testRedisShardChannel)
		synctest.Wait()
		// an invalid message is not evidence that the subscription is live
		socket.events <- &redis.Message{Channel: testRedisShardChannel, Payload: "synthetic-invalid"}
		synctest.Wait()
		time.Sleep(settings.IncomingTimeout)
		synctest.Wait()
		if socket.pings.Load() == 0 {
			t.Fatal("fixture never exercised a ping")
		}
		if !socket.isClosed() {
			t.Fatal("ping writes hid a silent socket")
		}
		consumer.requirePayloads(t)
	})
}
