// Exercises keyspace subscription readiness with synthetic sockets: each
// master's confirmations, messages and failure boundaries are controlled.
package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/redis/go-redis/v9"
)

// A synthetic master whose subscription replies are held until the test sends them.
type redisKeyEventTestNode struct {
	stateLock            sync.Mutex
	connections          []net.Conn
	closed               bool
	stop                 chan struct{}
	workers              sync.WaitGroup
	subscriptionRequests chan *redisKeyEventTestSubscription
	afterHelloReadGate   <-chan struct{}
	helloDone            chan struct{}
	helloOnce            sync.Once
}

// One subscription socket, with serialized replies and observable closure.
type redisKeyEventTestSubscription struct {
	conn      net.Conn
	stateLock sync.Mutex
	patterns  []string
	done      chan struct{}
}

// Allocates an isolated master and joins its sockets when the test ends.
func newRedisKeyEventTestNode(t *testing.T) *redisKeyEventTestNode {
	t.Helper()
	node := &redisKeyEventTestNode{
		stop:                 make(chan struct{}),
		subscriptionRequests: make(chan *redisKeyEventTestSubscription, 16),
	}
	t.Cleanup(node.close)
	return node
}

// Uses only in-memory pipes, including the real go-redis handshake and parser.
func (self *redisKeyEventTestNode) dial(context.Context, string, string) (net.Conn, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, net.ErrClosed
	}
	peer, client := net.Pipe()
	self.connections = append(self.connections, peer, client)
	self.workers.Add(1)
	go func() {
		defer self.workers.Done()
		defer peer.Close()
		subscription := &redisKeyEventTestSubscription{conn: peer, done: make(chan struct{})}
		defer close(subscription.done)
		reader := bufio.NewReader(peer)
		for {
			args, err := redisDialTestReadCommand(reader)
			if err != nil {
				return
			}
			var reply string
			switch strings.ToLower(args[0]) {
			case "hello":
				reply = "*0\r\n"
			case "psubscribe":
				subscription.patterns = slices.Clone(args[1:])
				select {
				case self.subscriptionRequests <- subscription:
				case <-self.stop:
					return
				}
				continue
			case "ping":
				reply = "*2\r\n$4\r\npong\r\n$0\r\n\r\n"
			default:
				reply = "+OK\r\n"
			}
			if err := subscription.write(reply); err != nil {
				return
			}
			if strings.ToLower(args[0]) == "hello" && self.afterHelloReadGate != nil {
				self.helloOnce.Do(func() { close(self.helloDone) })
				select {
				case <-self.afterHelloReadGate:
				case <-self.stop:
					return
				}
			}
		}
	}()
	return client, nil
}

// Cancels all synthetic socket readers and joins them.
func (self *redisKeyEventTestNode) close() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.closed {
			self.closed = true
			close(self.stop)
			for _, conn := range self.connections {
				_ = conn.Close()
			}
		}
	}()
	self.workers.Wait()
}

// Hands the test a command whose acknowledgements have not been sent.
func (self *redisKeyEventTestNode) subscription(t *testing.T) *redisKeyEventTestSubscription {
	t.Helper()
	select {
	case subscription := <-self.subscriptionRequests:
		return subscription
	case <-time.After(time.Second):
		t.Fatal("subscription command did not reach synthetic master")
		return nil
	}
}

// Keeps concurrent synthetic health-check replies from interleaving frames.
func (self *redisKeyEventTestSubscription) write(reply string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	_, err := io.WriteString(self.conn, reply)
	return err
}

// Sends one pattern's subscription confirmation using the Redis wire format.
func (self *redisKeyEventTestSubscription) acknowledge(t *testing.T, pattern string, count int) {
	t.Helper()
	if err := self.write(fmt.Sprintf("*3\r\n$10\r\npsubscribe\r\n$%d\r\n%s\r\n:%d\r\n", len(pattern), pattern, count)); err != nil {
		t.Fatal(err)
	}
}

// Sends one key event while the real subscription pumps are running.
func (self *redisKeyEventTestSubscription) message(t *testing.T, pattern, channel, payload string) {
	t.Helper()
	if err := self.write(fmt.Sprintf("*4\r\n$8\r\npmessage\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n", len(pattern), pattern, len(channel), channel, len(payload), payload)); err != nil {
		t.Fatal(err)
	}
}

// Substitutes only this test's process Redis client; no external service is used.
func installRedisKeyEventTestClient(t *testing.T, client RedisClient) {
	t.Helper()
	previous := safeClient
	safeClient = &safeRedisClient{client: client}
	t.Cleanup(func() {
		_ = client.Close()
		safeClient = previous
	})
}

// Creates the standalone client with a finite synthetic dial and socket policy.
func redisKeyEventTestClient(node *redisKeyEventTestNode) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:            "192.0.2.1:6379",
		Protocol:        2,
		DisableIdentity: true,
		MaxRetries:      -1,
		DialerRetries:   1,
		Dialer:          node.dial,
		ReadTimeout:     time.Second,
		WriteTimeout:    time.Second,
	})
}

// Gives each synthetic master a disjoint slot range without network discovery.
func redisKeyEventTestCluster(nodes ...*redisKeyEventTestNode) *redis.ClusterClient {
	nodeAddrNodeKVs := map[string]*redisKeyEventTestNode{}
	slots := []redis.ClusterSlot{}
	for i, node := range nodes {
		addr := fmt.Sprintf("192.0.2.%d:6379", i+1)
		nodeAddrNodeKVs[addr] = node
		slots = append(slots, redis.ClusterSlot{
			Start: i * 16384 / len(nodes),
			End:   (i+1)*16384/len(nodes) - 1,
			Nodes: []redis.ClusterNode{{Addr: addr}},
		})
	}
	return redis.NewClusterClient(&redis.ClusterOptions{
		Protocol:        2,
		DisableIdentity: true,
		MaxRetries:      -1,
		DialerRetries:   1,
		ReadTimeout:     time.Second,
		WriteTimeout:    time.Second,
		ClusterSlots: func(context.Context) ([]redis.ClusterSlot, error) {
			return slots, nil
		},
		Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return nodeAddrNodeKVs[addr].dial(ctx, network, addr)
		},
	})
}

// An in-flight production subscription whose completion is an explicit barrier.
type redisKeyEventTestStart struct {
	cancel   context.CancelFunc
	returned chan struct{}
	messages <-chan RedisMessage
	done     <-chan struct{}
	unsub    func()
	err      error
}

// Starts the public subscription boundary and owns its cancellation and teardown.
func startRedisKeyEventTest(t *testing.T, settings *RedisKeyEventSubscriptionSettings, patterns ...string) *redisKeyEventTestStart {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	start := &redisKeyEventTestStart{cancel: cancel, returned: make(chan struct{})}
	go func() {
		defer close(start.returned)
		start.messages, start.done, start.unsub, start.err = SubscribeKeyEventsWithSettings(ctx, settings, patterns...)
	}()
	t.Cleanup(func() {
		cancel()
		<-start.returned
		if start.unsub != nil {
			start.unsub()
		}
	})
	return start
}

// Quiescence proves startup remains blocked on the withheld acknowledgement.
func (self *redisKeyEventTestStart) requireWaiting(t *testing.T) {
	t.Helper()
	synctest.Wait()
	select {
	case <-self.returned:
		t.Fatalf("subscription returned before every acknowledgement: %v", self.err)
	default:
	}
}

// Joins startup and verifies that failure did not leave a callable subscription.
func (self *redisKeyEventTestStart) requireFailure(t *testing.T, cause error) {
	t.Helper()
	<-self.returned
	if self.err == nil || (cause != nil && !errors.Is(self.err, cause)) || self.messages != nil || self.done != nil || self.unsub != nil {
		t.Fatalf("failed subscription retained state or lost its cause: %v", self.err)
	}
}

// The recovery read cannot run before the last pattern's acknowledgement.
func TestRedisKeyEventSubscriptionWaitsForEveryPattern(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newRedisKeyEventTestNode(t)
		installRedisKeyEventTestClient(t, redisKeyEventTestClient(node))
		settings := DefaultRedisKeyEventSubscriptionSettings()
		start := startRedisKeyEventTest(t, settings, "removed:*", "close:*")
		subscription := node.subscription(t)
		start.requireWaiting(t)
		subscription.acknowledge(t, "removed:*", 1)
		start.requireWaiting(t)
		subscription.acknowledge(t, "close:*", 2)
		<-start.returned
		if start.err != nil {
			t.Fatal(start.err)
		}
		subscription.message(t, "removed:*", "removed:synthetic", "set")
		message := <-start.messages
		if message.Channel != "removed:synthetic" || message.Payload != "set" {
			t.Fatal("first post-readiness event was lost", message)
		}
		time.Sleep(2 * settings.AcknowledgementTimeout)
		synctest.Wait()
		select {
		case <-start.done:
			t.Fatal("startup timeout canceled an established subscription")
		default:
		}
	})
}

// An early master's events remain buffered while another master acknowledges.
func TestRedisKeyEventSubscriptionWaitsForEveryMaster(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := newRedisKeyEventTestNode(t), newRedisKeyEventTestNode(t)
		installRedisKeyEventTestClient(t, redisKeyEventTestCluster(first, second))
		start := startRedisKeyEventTest(t, DefaultRedisKeyEventSubscriptionSettings(), "removed:*")
		firstSubscription, secondSubscription := first.subscription(t), second.subscription(t)
		firstSubscription.acknowledge(t, "removed:*", 1)
		start.requireWaiting(t)
		firstSubscription.message(t, "removed:*", "removed:synthetic", "set")
		start.requireWaiting(t)
		secondSubscription.acknowledge(t, "removed:*", 1)
		<-start.returned
		if start.err != nil {
			t.Fatal(start.err)
		}
		if message := <-start.messages; message.Channel != "removed:synthetic" {
			t.Fatal("startup did not preserve an early master's event", message)
		}
	})
}

// A silent master consumes only the owner's finite startup allowance.
func TestRedisKeyEventSubscriptionAcknowledgementTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newRedisKeyEventTestNode(t)
		installRedisKeyEventTestClient(t, redisKeyEventTestClient(node))
		settings := DefaultRedisKeyEventSubscriptionSettings()
		settings.AcknowledgementTimeout = 100 * time.Millisecond
		start := startRedisKeyEventTest(t, settings, "removed:*")
		subscription := node.subscription(t)
		start.requireWaiting(t)
		time.Sleep(settings.AcknowledgementTimeout)
		start.requireFailure(t, context.DeadlineExceeded)
		<-subscription.done
	})
}

// Cancellation while awaiting readiness closes the socket without a new event.
func TestRedisKeyEventSubscriptionStartupCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newRedisKeyEventTestNode(t)
		installRedisKeyEventTestClient(t, redisKeyEventTestClient(node))
		start := startRedisKeyEventTest(t, DefaultRedisKeyEventSubscriptionSettings(), "removed:*")
		subscription := node.subscription(t)
		start.requireWaiting(t)
		start.cancel()
		start.requireFailure(t, context.Canceled)
		<-subscription.done
	})
}

// A failed socket open returns its cause instead of starting a silent epoch.
func TestRedisKeyEventSubscriptionStartupDialFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cause := errors.New("synthetic subscription dial failure")
		client := redis.NewClient(&redis.Options{
			Addr:          "192.0.2.1:6379",
			MaxRetries:    -1,
			DialerRetries: 1,
			Dialer: func(context.Context, string, string) (net.Conn, error) {
				return nil, cause
			},
		})
		installRedisKeyEventTestClient(t, client)
		start := startRedisKeyEventTest(t, DefaultRedisKeyEventSubscriptionSettings(), "removed:*")
		start.requireFailure(t, cause)
	})
}

// A blocked subscription write carries the same startup deadline as its replies.
func TestRedisKeyEventSubscriptionStartupWriteTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newRedisKeyEventTestNode(t)
		node.afterHelloReadGate = make(chan struct{})
		node.helloDone = make(chan struct{})
		installRedisKeyEventTestClient(t, redisKeyEventTestClient(node))
		settings := DefaultRedisKeyEventSubscriptionSettings()
		settings.AcknowledgementTimeout = 100 * time.Millisecond
		startedAt := time.Now()
		start := startRedisKeyEventTest(t, settings, "removed:*")
		<-node.helloDone
		start.requireWaiting(t)
		time.Sleep(settings.AcknowledgementTimeout)
		start.requireFailure(t, nil)
		if elapsed := time.Since(startedAt); elapsed != settings.AcknowledgementTimeout {
			t.Fatalf("subscription write exceeded startup allowance: %s", elapsed)
		}
	})
}

// Duplicate caller patterns have one confirmation and do not resemble reconnects.
func TestRedisKeyEventSubscriptionDuplicatePatterns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newRedisKeyEventTestNode(t)
		installRedisKeyEventTestClient(t, redisKeyEventTestClient(node))
		start := startRedisKeyEventTest(t, DefaultRedisKeyEventSubscriptionSettings(), "removed:*", "removed:*")
		subscription := node.subscription(t)
		if !slices.Equal(subscription.patterns, []string{"removed:*"}) {
			t.Fatal("duplicate patterns reached Redis", subscription.patterns)
		}
		subscription.acknowledge(t, "removed:*", 1)
		<-start.returned
		if start.err != nil {
			t.Fatal(start.err)
		}
		synctest.Wait()
		select {
		case <-start.done:
			t.Fatal("duplicate patterns ended a healthy subscription")
		default:
		}
	})
}

// Pressure before all masters are ready fails startup instead of blocking drains.
func TestRedisKeyEventSubscriptionStartupOverflow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := newRedisKeyEventTestNode(t), newRedisKeyEventTestNode(t)
		installRedisKeyEventTestClient(t, redisKeyEventTestCluster(first, second))
		settings := DefaultRedisKeyEventSubscriptionSettings()
		settings.MessageBufferSize = 1
		start := startRedisKeyEventTest(t, settings, "removed:*")
		firstSubscription, secondSubscription := first.subscription(t), second.subscription(t)
		firstSubscription.acknowledge(t, "removed:*", 1)
		firstSubscription.message(t, "removed:*", "removed:first", "set")
		start.requireWaiting(t)
		firstSubscription.message(t, "removed:*", "removed:second", "set")
		start.requireFailure(t, nil)
		<-firstSubscription.done
		<-secondSubscription.done
	})
}

// A later confirmation ends the generation so callers repair the reconnect gap.
func TestRedisKeyEventSubscriptionReconnectEndsGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newRedisKeyEventTestNode(t)
		installRedisKeyEventTestClient(t, redisKeyEventTestClient(node))
		start := startRedisKeyEventTest(t, DefaultRedisKeyEventSubscriptionSettings(), "removed:*")
		subscription := node.subscription(t)
		subscription.acknowledge(t, "removed:*", 1)
		<-start.returned
		if start.err != nil {
			t.Fatal(start.err)
		}
		subscription.acknowledge(t, "removed:*", 1)
		<-start.done
	})
}

// No empty pattern set is allowed to wait forever for an impossible confirmation.
func TestRedisKeyEventSubscriptionRequiresPatterns(t *testing.T) {
	messages, done, unsub, err := SubscribeKeyEventsWithSettings(t.Context(), DefaultRedisKeyEventSubscriptionSettings())
	if err == nil || messages != nil || done != nil || unsub != nil {
		t.Fatal("empty pattern set was admitted", err)
	}
}

// An already-canceled owner gets an ordinary error before Redis is acquired.
func TestRedisKeyEventSubscriptionCanceledBeforeStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	messages, done, unsub, err := SubscribeKeyEventsWithSettings(ctx, DefaultRedisKeyEventSubscriptionSettings(), "removed:*")
	if !errors.Is(err, context.Canceled) || messages != nil || done != nil || unsub != nil {
		t.Fatal("canceled startup was admitted", err)
	}
}
