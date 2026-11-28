// An in-memory sharded pub/sub broker for tests of shard subscription consumers
// (server.RunRedisShardSubscription and its users). It keeps per-channel
// subscribers and per-socket channel sets, answers SSUBSCRIBE and SUNSUBSCRIBE
// with the notices and shard channel counts a Redis 8 node sends (a repeated
// subscribe or an unsubscribe of an absent channel keeps the count), delivers
// SPUBLISH only to the sockets subscribed to its channel, and injects the
// faults a cluster node can produce: refused writes, withheld
// acknowledgements, slot moves, subscriptions lost without notice, error
// replies. Each socket's events arrive in order, after the broker's latency.
// Every wait honors the caller's context and timeout and uses the runtime
// clock, so the broker runs inside testing/synctest bubbles.
//
// Safe for concurrent use: one lock guards the broker and all its sockets.
package redisshardtest

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	errSocketClosed     = errors.New("synthetic shard socket closed")
	errSubscribeRefused = errors.New("synthetic shard subscribe refused")
	errOpenRefused      = errors.New("synthetic shard socket refused")
)

type queuedEvent struct {
	event     any
	deliverAt time.Time
}

type pendingCommand struct {
	apply   func()
	applyAt time.Time
}

// The broker's state and faults. A nil fault function never applies.
type Broker struct {
	stateLock      sync.Mutex
	channelSockets map[string]map[*Socket]bool
	sockets        []*Socket
	// how long every event takes to reach its socket's reader
	latency time.Duration
	// how long a subscribe or unsubscribe written on an open socket takes to
	// apply on the node, in order
	commandDelay time.Duration
	answerPings  bool
	failOpen     bool
	// a subscribe naming a matching channel is refused as a write error
	failSubscribe func(channel string) bool
	// a matching channel is subscribed but never acknowledged
	withholdAcknowledgement func(channel string) bool
	subscribeCommandCount   int
	unsubscribeCommandCount int
	publishCount            int
}

// One socket. Its reader is a single goroutine; writes may come from others.
type Socket struct {
	broker        *Broker
	anchorChannel string
	channels      map[string]bool
	queue         []queuedEvent
	signal        chan struct{}
	// commands written and not yet applied, in order
	pendingCommands []pendingCommand
	closed          bool
	pingCount       int
	deliveryCount   int
	// messages delivered, by channel
	channelDeliveryCounts map[string]int
}

// Pings are answered and events arrive at once until set otherwise.
func NewBroker() *Broker {
	return &Broker{
		channelSockets: map[string]map[*Socket]bool{},
		answerPings:    true,
	}
}

func (self *Broker) SetLatency(latency time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.latency = latency
}

func (self *Broker) SetCommandDelay(commandDelay time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.commandDelay = commandDelay
}

func (self *Broker) SetAnswerPings(answerPings bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.answerPings = answerPings
}

func (self *Broker) SetFailOpen(failOpen bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failOpen = failOpen
}

func (self *Broker) SetFailSubscribe(failSubscribe func(channel string) bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failSubscribe = failSubscribe
}

func (self *Broker) SetWithholdAcknowledgement(withholdAcknowledgement func(channel string) bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.withholdAcknowledgement = withholdAcknowledgement
}

// Opens a socket subscribed to the channel, as the process client's
// SSubscribe does; its acknowledgement is the reader's to await.
func (self *Broker) Open(_ context.Context, channel string) (*Socket, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failOpen {
		return nil, errOpenRefused
	}
	socket := &Socket{
		broker:                self,
		anchorChannel:         channel,
		channels:              map[string]bool{},
		signal:                make(chan struct{}),
		channelDeliveryCounts: map[string]int{},
	}
	self.sockets = append(self.sockets, socket)
	self.subscribeCommandCount += 1
	if self.failSubscribe == nil || !self.failSubscribe(channel) {
		socket.subscribeWithLock(channel)
	}
	return socket, nil
}

// Delivers to every socket subscribed to the channel and returns how many
// there were, as SPUBLISH does.
func (self *Broker) Publish(channel string, payload string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.publishCount += 1
	receiverCount := 0
	for socket := range self.channelSockets[channel] {
		socket.deliveryCount += 1
		socket.channelDeliveryCounts[channel] += 1
		socket.enqueueWithLock(&redis.Message{Channel: channel, Payload: payload})
		receiverCount += 1
	}
	return receiverCount
}

// The node gives up the channels' slot: each subscriber loses them with a
// sunsubscribe notice, and keeps its socket.
func (self *Broker) Migrate(channels ...string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, channel := range channels {
		for socket := range self.channelSockets[channel] {
			socket.unsubscribeWithLock(channel, true)
		}
	}
}

// The node drops the socket's subscription to the channel without a notice.
func (self *Broker) DropSilently(socket *Socket, channel string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	socket.unsubscribeWithLock(channel, false)
}

// Queues an event, or an error the read returns, on the socket.
func (self *Broker) Inject(socket *Socket, event any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	socket.enqueueWithLock(event)
}

// SHARDNUMSUB for one channel.
func (self *Broker) Subscribers(channel string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.channelSockets[channel])
}

// Shard channels subscribed across every live socket, as the node's
// pubsubshard_channels counts them per subscriber.
func (self *Broker) SubscriptionCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, sockets := range self.channelSockets {
		count += len(sockets)
	}
	return count
}

func (self *Broker) SubscribeCommandCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.subscribeCommandCount
}

func (self *Broker) UnsubscribeCommandCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.unsubscribeCommandCount
}

// Every socket opened, in order.
func (self *Broker) Sockets() []*Socket {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*Socket(nil), self.sockets...)
}

// The open sockets whose anchor is the channel.
func (self *Broker) LiveSockets(anchorChannel string) []*Socket {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var sockets []*Socket
	for _, socket := range self.sockets {
		if !socket.closed && socket.anchorChannel == anchorChannel {
			sockets = append(sockets, socket)
		}
	}
	return sockets
}

func (self *Socket) AnchorChannel() string {
	return self.anchorChannel
}

func (self *Socket) Closed() bool {
	self.broker.stateLock.Lock()
	defer self.broker.stateLock.Unlock()
	return self.closed
}

// The channels the node holds for the socket.
func (self *Socket) Channels() []string {
	self.broker.stateLock.Lock()
	defer self.broker.stateLock.Unlock()
	var channels []string
	for channel := range self.channels {
		channels = append(channels, channel)
	}
	return channels
}

func (self *Socket) Subscribed(channel string) bool {
	self.broker.stateLock.Lock()
	defer self.broker.stateLock.Unlock()
	return self.channels[channel]
}

// Messages delivered to the socket, in all or on one channel.
func (self *Socket) DeliveryCount() int {
	self.broker.stateLock.Lock()
	defer self.broker.stateLock.Unlock()
	return self.deliveryCount
}

func (self *Socket) ChannelDeliveryCount(channel string) int {
	self.broker.stateLock.Lock()
	defer self.broker.stateLock.Unlock()
	return self.channelDeliveryCounts[channel]
}

func (self *Socket) PingCount() int {
	self.broker.stateLock.Lock()
	defer self.broker.stateLock.Unlock()
	return self.pingCount
}

func (self *Socket) SSubscribe(_ context.Context, channels ...string) error {
	self.broker.stateLock.Lock()
	defer self.broker.stateLock.Unlock()
	if self.closed {
		return errSocketClosed
	}
	if failSubscribe := self.broker.failSubscribe; failSubscribe != nil {
		for _, channel := range channels {
			if failSubscribe(channel) {
				return errSubscribeRefused
			}
		}
	}
	self.broker.subscribeCommandCount += 1
	self.commandWithLock(func() {
		for _, channel := range channels {
			self.subscribeWithLock(channel)
		}
	})
	return nil
}

func (self *Socket) SUnsubscribe(_ context.Context, channels ...string) error {
	self.broker.stateLock.Lock()
	defer self.broker.stateLock.Unlock()
	if self.closed {
		return errSocketClosed
	}
	self.broker.unsubscribeCommandCount += 1
	self.commandWithLock(func() {
		for _, channel := range channels {
			self.unsubscribeWithLock(channel, true)
		}
	})
	return nil
}

// Applies a written command now, or after the broker's command delay behind
// the commands written before it.
func (self *Socket) commandWithLock(apply func()) {
	if self.broker.commandDelay <= 0 && len(self.pendingCommands) == 0 {
		apply()
		return
	}
	self.pendingCommands = append(self.pendingCommands, pendingCommand{apply: apply, applyAt: time.Now().Add(self.broker.commandDelay)})
	time.AfterFunc(self.broker.commandDelay, func() {
		self.broker.stateLock.Lock()
		defer self.broker.stateLock.Unlock()
		for 0 < len(self.pendingCommands) && !time.Now().Before(self.pendingCommands[0].applyAt) {
			command := self.pendingCommands[0]
			self.pendingCommands = self.pendingCommands[1:]
			if !self.closed {
				command.apply()
			}
		}
	})
}

func (self *Socket) Ping(context.Context, ...string) error {
	self.broker.stateLock.Lock()
	defer self.broker.stateLock.Unlock()
	if self.closed {
		return errSocketClosed
	}
	self.pingCount += 1
	if self.broker.answerPings {
		self.enqueueWithLock(&redis.Pong{})
	}
	return nil
}

// Returns the next event that has arrived, or a timeout error once the
// timeout passes with none.
func (self *Socket) ReceiveTimeout(ctx context.Context, timeout time.Duration) (any, error) {
	deadline := time.Now().Add(timeout)
	for {
		var event queuedEvent
		ready := false
		wait := time.Until(deadline)
		var signal chan struct{}
		closed := false
		func() {
			self.broker.stateLock.Lock()
			defer self.broker.stateLock.Unlock()
			if self.closed {
				closed = true
				return
			}
			if 0 < len(self.queue) {
				if !time.Now().Before(self.queue[0].deliverAt) {
					event = self.queue[0]
					self.queue = self.queue[1:]
					ready = true
					return
				}
				wait = min(wait, time.Until(self.queue[0].deliverAt))
			}
			signal = self.signal
		}()
		if closed {
			return nil, errSocketClosed
		}
		if ready {
			if err, ok := event.event.(error); ok {
				return nil, err
			}
			return event.event, nil
		}
		if !time.Now().Before(deadline) {
			return nil, context.DeadlineExceeded
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-signal:
		case <-time.After(wait):
		}
	}
}

// The node drops every subscription of a closed socket.
func (self *Socket) Close() error {
	self.broker.stateLock.Lock()
	defer self.broker.stateLock.Unlock()
	if self.closed {
		return nil
	}
	for channel := range self.channels {
		self.unsubscribeWithLock(channel, false)
	}
	self.closed = true
	self.signalWithLock()
	return nil
}

func (self *Socket) subscribeWithLock(channel string) {
	if !self.channels[channel] {
		self.channels[channel] = true
		sockets := self.broker.channelSockets[channel]
		if sockets == nil {
			sockets = map[*Socket]bool{}
			self.broker.channelSockets[channel] = sockets
		}
		sockets[self] = true
	}
	if withhold := self.broker.withholdAcknowledgement; withhold == nil || !withhold(channel) {
		self.enqueueWithLock(&redis.Subscription{Kind: "ssubscribe", Channel: channel, Count: len(self.channels)})
	}
}

func (self *Socket) unsubscribeWithLock(channel string, notice bool) {
	if self.channels[channel] {
		delete(self.channels, channel)
		sockets := self.broker.channelSockets[channel]
		delete(sockets, self)
		if len(sockets) == 0 {
			delete(self.broker.channelSockets, channel)
		}
	}
	if notice {
		self.enqueueWithLock(&redis.Subscription{Kind: "sunsubscribe", Channel: channel, Count: len(self.channels)})
	}
}

// Events arrive in order: none before the one queued ahead of it.
func (self *Socket) enqueueWithLock(event any) {
	if self.closed {
		return
	}
	deliverAt := time.Now().Add(self.broker.latency)
	if count := len(self.queue); 0 < count && deliverAt.Before(self.queue[count-1].deliverAt) {
		deliverAt = self.queue[count-1].deliverAt
	}
	self.queue = append(self.queue, queuedEvent{event: event, deliverAt: deliverAt})
	self.signalWithLock()
}

func (self *Socket) signalWithLock() {
	close(self.signal)
	self.signal = make(chan struct{})
}
