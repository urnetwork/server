package connect

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The wakes of an exchange process's relays of webrtc extender carrier offers
// (connect/EXTENDER.md S8, model/extender_webrtc_relay_model.go). The prober
// adds an offer to the client's offer set and then publishes the client id on
// the sharded channel of the exchange process its client's resident
// registration names; the process subscribes to its own channel alone and
// kicks that client's relay listener. So no process matches a pattern against
// the keyspace notification of every set on Main to find its residents'
// offers.
//
// A wake is advisory. One published while the process had no subscription is
// gone, so every start of the subscription kicks every listener, trickled over
// the resync spread, and every listener takes once as its resident registers;
// an offer nobody took expires with its prober's wait.
//
// The subscription starts with the first listener, so a process that holds no
// resident opens no socket for it.
type extenderWebRtcOfferWakes struct {
	ctx     context.Context
	channel string
	// the shard subscription's liveness bounds and its socket source, a
	// synthetic one in a test
	subscriptionSettings *server.RedisShardSubscriptionSettings
	subscriptionSource   *server.RedisShardSubscriptionSource
	// the spread a start of the subscription kicks every listener over
	resyncSpreadTimeout time.Duration

	stateLock      sync.Mutex
	nextListenerId int64
	listeners      map[server.Id]map[int64]*extenderWebRtcRelayListener
	startOnce      sync.Once
}

func newExtenderWebRtcOfferWakes(
	ctx context.Context,
	channel string,
	subscriptionSettings *server.RedisShardSubscriptionSettings,
	subscriptionSource *server.RedisShardSubscriptionSource,
	resyncSpreadTimeout time.Duration,
) *extenderWebRtcOfferWakes {
	return &extenderWebRtcOfferWakes{
		ctx:                  ctx,
		channel:              channel,
		subscriptionSettings: subscriptionSettings,
		subscriptionSource:   subscriptionSource,
		resyncSpreadTimeout:  resyncSpreadTimeout,
		listeners:            map[server.Id]map[int64]*extenderWebRtcRelayListener{},
	}
}

// AddListener registers a resident client's relay for the wakes of its
// client's offers, starting the subscription with the first one.
func (self *extenderWebRtcOfferWakes) AddListener(clientId server.Id, listener *extenderWebRtcRelayListener) (remove func()) {
	listenerId := func() int64 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		listenerId := self.nextListenerId
		self.nextListenerId += 1
		clientListeners, ok := self.listeners[clientId]
		if !ok {
			clientListeners = map[int64]*extenderWebRtcRelayListener{}
			self.listeners[clientId] = clientListeners
		}
		clientListeners[listenerId] = listener
		return listenerId
	}()
	self.startOnce.Do(func() {
		go server.HandleError(self.run)
	})
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		clientListeners := self.listeners[clientId]
		delete(clientListeners, listenerId)
		if len(clientListeners) == 0 {
			delete(self.listeners, clientId)
		}
	}
}

// Keeps the subscription to the process's channel until the exchange ends
// (server.RunRedisShardSubscription). Its handler never blocks: a kick never
// waits, and a resync trickles from a goroutine of its own.
func (self *extenderWebRtcOfferWakes) run() {
	server.RunRedisShardSubscription(
		self.ctx,
		self.channel,
		self.subscriptionSettings,
		self.subscriptionSource,
		&server.RedisShardSubscriptionHandler{
			Subscribed: func(resubscribed bool) {
				self.resync()
			},
			Message: func(message server.RedisMessage) bool {
				if message.Channel != self.channel {
					return false
				}
				clientId, ok := model.ParseExtenderWebRtcOfferWake(message.Payload)
				if !ok {
					return false
				}
				self.kick(clientId)
				return true
			},
		},
	)
}

// Kicks the relay listeners of one client.
func (self *extenderWebRtcOfferWakes) kick(clientId server.Id) {
	listeners := func() []*extenderWebRtcRelayListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		listeners := []*extenderWebRtcRelayListener{}
		for _, listener := range self.listeners[clientId] {
			listeners = append(listeners, listener)
		}
		return listeners
	}()
	for _, listener := range listeners {
		listener.Kick()
	}
}

// Kicks every listener, trickled over the resync spread: an offer added while
// the process had no subscription has no wake of its own. An offer lives
// seconds, so most such kicks find nothing; a kick starts a worker only for
// that pass (extenderWebRtcRelayListener).
func (self *extenderWebRtcOfferWakes) resync() {
	listeners := func() []*extenderWebRtcRelayListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		listeners := []*extenderWebRtcRelayListener{}
		for _, clientListeners := range self.listeners {
			for _, listener := range clientListeners {
				listeners = append(listeners, listener)
			}
		}
		return listeners
	}()
	if len(listeners) == 0 {
		return
	}
	spacing := self.resyncSpreadTimeout / time.Duration(len(listeners))
	go server.HandleError(func() {
		for _, listener := range listeners {
			listener.Kick()
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(spacing):
			}
		}
	})
}
