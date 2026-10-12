package proxy

// The change channel wakes of a proxy block across a cluster slot move. The
// watches run against synthetic shard sockets and a change fixture, and the
// timed poll is pushed out of the test's reach, so every poll observed here was
// woken by the subscription. All timing runs on the synctest clock.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A synthetic shard socket. The test feeds its events, and its owner answers
// every ping.
type testProxyChangeSocket struct {
	channel   string
	events    chan any
	closed    chan struct{}
	closeOnce sync.Once
}

func (self *testProxyChangeSocket) ReceiveTimeout(ctx context.Context, timeout time.Duration) (any, error) {
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

func (self *testProxyChangeSocket) SSubscribe(context.Context, ...string) error {
	return nil
}

func (self *testProxyChangeSocket) Ping(context.Context, ...string) error {
	self.events <- &redis.Pong{}
	return nil
}

func (self *testProxyChangeSocket) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}

func (self *testProxyChangeSocket) isClosed() bool {
	select {
	case <-self.closed:
		return true
	default:
		return false
	}
}

// Counts the change polls per host.
type testProxyChangePolls struct {
	stateLock  sync.Mutex
	hostCounts map[string]int
}

func (self *testProxyChangePolls) getProxyClientsSince(
	ctx context.Context,
	proxyHost string,
	block string,
	changeId int64,
) (map[server.Id]*model.ProxyClient, int64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.hostCounts[proxyHost] += 1
	return map[server.Id]*model.ProxyClient{}, changeId, nil
}

func (self *testProxyChangePolls) count(proxyHost string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.hostCounts[proxyHost]
}

// A slot move drops the block's change subscription with a sunsubscribe on a
// socket that stays healthy. The watch must subscribe again, and the change
// published after the move must still wake a poll. Before the shared shard
// subscription, the watch kept the dead socket and only the timed poll found
// new proxy clients.
func TestProxyClientNotificationResubscribesAfterSlotMove(t *testing.T) {
	t.Setenv("WARP_HOST", "synthetic-proxy")
	t.Setenv("WARP_BLOCK", "g7")
	t.Setenv("WARP_DOMAIN", "example")
	synctest.Test(t, func(t *testing.T) {
		host := "synthetic-proxy"
		fqHost := "synthetic-proxy.example"
		channel := model.ProxyClientChannel(host, "g7")
		fqChannel := model.ProxyClientChannel(fqHost, "g7")

		settings := DefaultProxySettings()
		settings.NotificationTimeout = time.Hour
		settings.ReconcileTimeout = 0
		subscriptionSettings := server.DefaultRedisShardSubscriptionSettings()

		opened := make(chan *testProxyChangeSocket, 16)
		subscriptionSource := &server.RedisShardSubscriptionSource{
			Open: func(ctx context.Context, channel string) (server.RedisShardSubscription, error) {
				socket := &testProxyChangeSocket{channel: channel, events: make(chan any, 64), closed: make(chan struct{})}
				opened <- socket
				return socket, nil
			},
		}
		polls := &testProxyChangePolls{hostCounts: map[string]int{}}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		notification := newProxyClientNotification(ctx, settings, subscriptionSource, polls.getProxyClientsSince)
		synctest.Wait()
		select {
		case <-notification.InitialSyncDone():
		default:
			t.Fatal("the initial poll did not complete the initial sync")
		}

		requireSocket := func(message string) *testProxyChangeSocket {
			select {
			case socket := <-opened:
				return socket
			default:
				t.Fatal(message)
				return nil
			}
		}
		channelSockets := map[string]*testProxyChangeSocket{}
		for range 2 {
			socket := requireSocket("a watch opened no change subscription")
			channelSockets[socket.channel] = socket
		}
		first, fqSocket := channelSockets[channel], channelSockets[fqChannel]
		if first == nil || fqSocket == nil {
			t.Fatalf("subscribed channels=%v, want %s and %s", channelSockets, channel, fqChannel)
		}
		requirePolls := func(want int, message string) {
			if count := polls.count(host); count != want {
				t.Fatalf("%s: polls=%d, want %d", message, count, want)
			}
		}
		requirePolls(1, "initial poll")

		// a new subscription wakes a poll for the wakes it may have missed
		first.events <- &redis.Subscription{Kind: "ssubscribe", Channel: channel, Count: 1}
		fqSocket.events <- &redis.Subscription{Kind: "ssubscribe", Channel: fqChannel, Count: 1}
		synctest.Wait()
		time.Sleep(settings.EventRateLimitTimeout)
		synctest.Wait()
		requirePolls(2, "subscription wake")

		first.events <- &redis.Message{Channel: channel, Payload: server.NewId().String()}
		synctest.Wait()
		time.Sleep(settings.EventRateLimitTimeout)
		synctest.Wait()
		requirePolls(3, "change wake before the move")

		// the old owner gives up the slot and keeps the socket
		first.events <- &redis.Subscription{Kind: "sunsubscribe", Channel: channel, Count: 0}
		synctest.Wait()
		if !first.isClosed() {
			t.Fatal("the dropped subscription kept its socket to the old owner")
		}
		time.Sleep(subscriptionSettings.MinReconnectTimeout)
		synctest.Wait()
		second := requireSocket("the moved change channel was never subscribed again")
		if second.channel != channel {
			t.Fatalf("resubscribed channel=%s, want %s", second.channel, channel)
		}

		second.events <- &redis.Subscription{Kind: "ssubscribe", Channel: channel, Count: 1}
		synctest.Wait()
		time.Sleep(settings.EventRateLimitTimeout)
		synctest.Wait()
		requirePolls(4, "resubscription wake")

		// a change published on the new owner wakes the poll
		second.events <- &redis.Message{Channel: channel, Payload: server.NewId().String()}
		synctest.Wait()
		time.Sleep(settings.EventRateLimitTimeout)
		synctest.Wait()
		requirePolls(5, "change wake after the move")

		if fqSocket.isClosed() {
			t.Fatal("the move of one channel replaced the other watch's subscription")
		}
	})
}
