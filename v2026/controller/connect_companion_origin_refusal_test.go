package controller

// The companion wait's final missing-origin verdict is recorded by
// CreateContract only after newContract returns (CPUOPT7-ORIGIN-DELIVERY.md
// §1.7). An origin edge that arrives in between must still close the fence
// its final read armed, or remove the refusal once stored. Synthetic
// subscription sockets carry the owner's edges, so the test decides when one
// arrives: the reader's next read is the barrier that it was handled in full.
// The origin and the companion attempts run against the test database.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type originRefusalTestEvent struct {
	event any
	// closed when the reader asks for the next event, after it handled this one
	handled chan struct{}
}

// One synthetic shard subscription socket, read by its owner's reader
// goroutine. Pings are answered, so an idle socket stays live. A pair channel
// is acknowledged at once with the socket's shard channel count, as a node
// does, and only a subscribed channel's edges are delivered.
type originRefusalTestSocket struct {
	channel   string
	events    chan originRefusalTestEvent
	closed    chan struct{}
	closeOnce sync.Once
	// the reader's own: the previous event's barrier
	handled      chan struct{}
	stateLock    sync.Mutex
	pairChannels map[string]bool
}

func (self *originRefusalTestSocket) ReceiveTimeout(ctx context.Context, timeout time.Duration) (any, error) {
	if self.handled != nil {
		close(self.handled)
		self.handled = nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-self.closed:
		return nil, errors.New("synthetic socket closed")
	case event := <-self.events:
		self.handled = event.handled
		return event.event, nil
	case <-time.After(timeout):
		return nil, context.DeadlineExceeded
	}
}

func (self *originRefusalTestSocket) SSubscribe(_ context.Context, channels ...string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, channel := range channels {
		if channel == self.channel {
			continue
		}
		self.pairChannels[channel] = true
		self.events <- originRefusalTestEvent{event: &redis.Subscription{Kind: "ssubscribe", Channel: channel, Count: 1 + len(self.pairChannels)}}
	}
	return nil
}

func (self *originRefusalTestSocket) SUnsubscribe(_ context.Context, channels ...string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, channel := range channels {
		delete(self.pairChannels, channel)
		self.events <- originRefusalTestEvent{event: &redis.Subscription{Kind: "sunsubscribe", Channel: channel, Count: 1 + len(self.pairChannels)}}
	}
	return nil
}

// Whether the node holds the channel for this socket.
func (self *originRefusalTestSocket) subscribed(channel string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return channel == self.channel || self.pairChannels[channel]
}

func (self *originRefusalTestSocket) Ping(context.Context, ...string) error {
	select {
	case self.events <- originRefusalTestEvent{event: &redis.Pong{}}:
	default:
	}
	return nil
}

func (self *originRefusalTestSocket) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}

// Queues an event and returns its barrier.
func (self *originRefusalTestSocket) feed(event any) chan struct{} {
	handled := make(chan struct{})
	self.events <- originRefusalTestEvent{event: event, handled: handled}
	return handled
}

// The sockets one owner opened, by channel.
type originRefusalTestSockets struct {
	stateLock      sync.Mutex
	channelSockets map[string]*originRefusalTestSocket
	opened         chan *originRefusalTestSocket
}

func newOriginRefusalTestSockets() *originRefusalTestSockets {
	return &originRefusalTestSockets{
		channelSockets: map[string]*originRefusalTestSocket{},
		opened:         make(chan *originRefusalTestSocket, 64),
	}
}

func (self *originRefusalTestSockets) open(_ context.Context, channel string) (server.RedisShardSubscription, error) {
	socket := &originRefusalTestSocket{
		channel:      channel,
		events:       make(chan originRefusalTestEvent, 64),
		closed:       make(chan struct{}),
		pairChannels: map[string]bool{},
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.channelSockets[channel] = socket
	}()
	self.opened <- socket
	return socket, nil
}

// The live socket subscribed to the channel: its bucket's own, or one of the
// bucket's pair channels, which share the bucket's hash tag.
func (self *originRefusalTestSockets) socket(channel string) *originRefusalTestSocket {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, socket := range self.channelSockets {
		tag := socket.channel[strings.Index(socket.channel, "{"):]
		if strings.Contains(channel, tag) && socket.subscribed(channel) {
			return socket
		}
	}
	return nil
}

// The deadline only bounds a broken fixture.
func awaitOriginRefusalTestHandled(t testing.TB, handled chan struct{}) bool {
	t.Helper()
	select {
	case <-handled:
		return true
	case <-time.After(time.Minute):
		t.Error("the synthetic socket's reader did not handle the event")
		return false
	}
}

// A companion request that beats its origin waits, finds none at its final
// read and returns the miss. The window's new forward contract commits, and
// its edge arrives, after newContract returned and before CreateContract
// records the verdict. The edge must keep the refusal out of the ring, so the
// provider's next return request rides the new origin instead of a refusal
// replayed for up to 60s.
func TestCreateContractCompanionRefusalKeepsItsWatchUntilRecorded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sockets := newOriginRefusalTestSockets()
		publishedEdges := make(chan chan struct{}, 64)
		owner := model.Testing_NewContractOriginNotifications(ctx, model.DefaultContractOriginNotificationSettings(), sockets.open,
			func(_ context.Context, channel string, payload string) error {
				// as SPUBLISH, nothing reaches a channel nobody subscribed to
				if socket := sockets.socket(channel); socket != nil {
					publishedEdges <- socket.feed(&redis.Message{Channel: channel, Payload: payload})
				}
				return nil
			})
		defer owner.Close()
		ownerCtx := model.WithContractOriginNotifications(ctx, owner)
		sourceNetworkId, sourceId, destinationNetworkId, destinationId := companionOriginTestSetup(ownerCtx, t)
		// a window advertises only Stream, for its return traffic
		model.SetProvide(ownerCtx, destinationId, map[model.ProvideMode][]byte{
			model.ProvideModeStream: []byte("synthetic-window-stream-key-0003"),
		})

		// The pair's subscription is acknowledged before the request, so only
		// an origin edge can close the verdict's fence.
		bucketWatch := owner.Watch(destinationId, sourceId)
		var socket *originRefusalTestSocket
		select {
		case socket = <-sockets.opened:
		case <-time.After(time.Minute):
			t.Fatal("the pair's subscription was not opened")
		}
		if !awaitOriginRefusalTestHandled(t, socket.feed(&redis.Subscription{Kind: "ssubscribe", Channel: socket.channel, Count: 1})) {
			t.FailNow()
		}
		bucketWatch.Close()

		previousRecord := recordContractRouteRejection
		defer func() { recordContractRouteRejection = previousRecord }()
		injected := false
		recordContractRouteRejection = func(
			notifications *model.ContractOriginNotifications,
			recordSourceId server.Id,
			recordDestinationId server.Id,
			companion bool,
			rejection model.ContractRouteRejection,
			fence <-chan struct{},
		) bool {
			if !injected && rejection.Cause == model.ContractRouteMissingOrigin {
				injected = true
				// the window's new forward contract, through this owner
				origin, err := model.CreateTransferEscrow(ownerCtx, destinationNetworkId, destinationId, sourceNetworkId, sourceId, model.ByteCount(1024*1024))
				if err != nil || origin == nil {
					t.Errorf("origin creation failed: %v", err)
				}
				select {
				case handled := <-publishedEdges:
					awaitOriginRefusalTestHandled(t, handled)
				case <-time.After(time.Minute):
					t.Error("the origin edge was not published")
				}
			}
			return previousRecord(notifications, recordSourceId, recordDestinationId, companion, rejection, fence)
		}

		if result := createRouteRejectionContract(t, ownerCtx, sourceId, destinationId, true); !isRouteReliabilityRefusal(result) {
			t.Fatalf("a companion request without an origin was not refused as an unreliable route: %+v", result)
		}
		if !injected {
			t.Fatal("no missing-origin verdict reached the record")
		}
		if _, ok := owner.RouteRejection(sourceId, destinationId, true); ok {
			t.Fatal("the refusal was stored although its origin's edge arrived before the record: the wait's watch let go of the pair before its verdict was recorded")
		}
		if result := createRouteRejectionContract(t, ownerCtx, sourceId, destinationId, true); result.Contract == nil {
			t.Fatalf("the request after its origin committed was refused: %+v", result)
		}
	})
}
