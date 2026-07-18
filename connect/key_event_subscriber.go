package connect

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// key-event transport signals (PEERSSTREAMS2.md §9, monitor/SIGNALS.md):
// dispatched should track registry churn; resubscribes should be rare (conn
// death, topology change); resyncs spike with resubscribes and registrations.
var (
	keyEventsDispatchedPeers = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_key_events_dispatched_peers_total",
		Help: "peer key events dispatched to registered listeners",
	})
	keyEventsDispatchedHops = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_key_events_dispatched_hops_total",
		Help: "stream-hop key events dispatched to registered listeners",
	})
	keyEventResubscribes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_key_event_resubscribes_total",
		Help: "key-event subscription (re)establishments (conn death, topology change, startup)",
	})
	keyEventResyncs = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_key_event_resyncs_total",
		Help: "listener resyncs forced by the key-event subscriber",
	})
)

func init() {
	prometheus.MustRegister(
		keyEventsDispatchedPeers,
		keyEventsDispatchedHops,
		keyEventResubscribes,
		keyEventResyncs,
	)
}

// keyEventSubscriber is the exchange's shared consumer of redis keyspace
// notifications for the peers + stream-hops listeners (PEERSSTREAMS2.md).
// ONE subscriber per exchange process: it holds the per-master psubscribe
// connections (via server.SubscribeKeyEvents) and demuxes key events to the
// in-process listeners registered by residents. The demux only routes — it
// reads no registry data and never blocks: the listener inputs
// (NetworkPeerListener.Delta/Resync, StreamHopListener.Kick/Resync) are
// non-blocking and degrade to a resync under pressure. Delivery reaches only
// explicitly registered listeners; there is no broadcast path.
//
// Every (re)established subscription triggers a trickled resync of all
// registrations — anything published while unsubscribed is gone forever.
type keyEventSubscriber struct {
	ctx    context.Context
	cancel context.CancelFunc

	settings *KeyEventDeliverySettings

	stateLock      sync.Mutex
	nextListenerId int64
	peerListeners  map[server.Id]map[int64]*model.NetworkPeerListener
	hopListeners   map[server.Id]map[int64]*model.StreamHopListener
}

func newKeyEventSubscriber(ctx context.Context, settings *KeyEventDeliverySettings) *keyEventSubscriber {
	cancelCtx, cancel := context.WithCancel(ctx)
	s := &keyEventSubscriber{
		ctx:           cancelCtx,
		cancel:        cancel,
		settings:      settings,
		peerListeners: map[server.Id]map[int64]*model.NetworkPeerListener{},
		hopListeners:  map[server.Id]map[int64]*model.StreamHopListener{},
	}
	go server.HandleError(s.run)
	return s
}

// AddPeerListener registers a network's peer listener for key-event deltas.
// The listener's own first sync covers anything before registration.
func (self *keyEventSubscriber) AddPeerListener(networkId server.Id, listener *model.NetworkPeerListener) (remove func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	listenerId := self.nextListenerId
	self.nextListenerId += 1
	listeners, ok := self.peerListeners[networkId]
	if !ok {
		listeners = map[int64]*model.NetworkPeerListener{}
		self.peerListeners[networkId] = listeners
	}
	listeners[listenerId] = listener
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(listeners, listenerId)
		if len(listeners) == 0 {
			delete(self.peerListeners, networkId)
		}
	}
}

// AddHopListener registers a client's stream-hop listener for key-event kicks.
func (self *keyEventSubscriber) AddHopListener(clientId server.Id, listener *model.StreamHopListener) (remove func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	listenerId := self.nextListenerId
	self.nextListenerId += 1
	listeners, ok := self.hopListeners[clientId]
	if !ok {
		listeners = map[int64]*model.StreamHopListener{}
		self.hopListeners[clientId] = listeners
	}
	listeners[listenerId] = listener
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(listeners, listenerId)
		if len(listeners) == 0 {
			delete(self.hopListeners, clientId)
		}
	}
}

// resyncAll trickles a forced resync across every registration, spread over
// the resync spread timeout so a resubscribe does not stampede the registry
// with simultaneous full reads.
func (self *keyEventSubscriber) resyncAll() {
	peerListeners := []*model.NetworkPeerListener{}
	hopListeners := []*model.StreamHopListener{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, listeners := range self.peerListeners {
			for _, listener := range listeners {
				peerListeners = append(peerListeners, listener)
			}
		}
		for _, listeners := range self.hopListeners {
			for _, listener := range listeners {
				hopListeners = append(hopListeners, listener)
			}
		}
	}()
	count := len(peerListeners) + len(hopListeners)
	if count == 0 {
		return
	}
	spacing := self.settings.ResyncSpreadTimeout / time.Duration(count)
	go server.HandleError(func() {
		for _, listener := range peerListeners {
			listener.Resync()
			keyEventResyncs.Inc()
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(spacing):
			}
		}
		for _, listener := range hopListeners {
			listener.Resync()
			keyEventResyncs.Inc()
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(spacing):
			}
		}
	})
}

// dispatch routes one keyspace notification. `channel` names the key;
// `event` is the command class name (the message payload).
func (self *keyEventSubscriber) dispatch(channel string, event string) {
	if event == "expire" {
		// a ttl refresh (heartbeat), not a visible change
		return
	}
	if networkId, clientId, ok := model.ParseNetworkPeerKeyEvent(channel); ok {
		switch event {
		case "set", "del", "expired":
			listeners := []*model.NetworkPeerListener{}
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				for _, listener := range self.peerListeners[networkId] {
					listeners = append(listeners, listener)
				}
			}()
			for _, listener := range listeners {
				listener.Delta(clientId, event)
				keyEventsDispatchedPeers.Inc()
			}
		}
		return
	}
	if clientId, ok := model.ParseStreamHopsKeyEvent(channel); ok {
		listeners := []*model.StreamHopListener{}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			for _, listener := range self.hopListeners[clientId] {
				listeners = append(listeners, listener)
			}
		}()
		for _, listener := range listeners {
			listener.Kick()
			keyEventsDispatchedHops.Inc()
		}
	}
}

// run supervises the subscription set: (re)subscribe, resync everything the
// gap may have missed, drain until the subscription dies or the topology
// changes, repeat. Errors back off; the loop only exits with the context.
func (self *keyEventSubscriber) run() {
	defer self.cancel()

	db := server.RedisDb()
	patterns := []string{
		model.NetworkPeerKeyEventPattern(db),
		model.StreamHopsKeyEventPattern(db),
	}

	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		messages, done, unsub, err := server.SubscribeKeyEvents(
			self.ctx,
			self.settings.TopologyRefreshInterval,
			patterns...,
		)
		if err != nil {
			glog.Infof("[kes]subscribe error = %s\n", err)
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(self.settings.ResubscribeTimeout):
			}
			continue
		}

		// events before this point (including any prior gap) are lost:
		// every registration full-reads
		keyEventResubscribes.Inc()
		self.resyncAll()

		drain := func() {
			defer unsub()
			for {
				select {
				case <-self.ctx.Done():
					return
				case <-done:
					return
				case message, ok := <-messages:
					if !ok {
						return
					}
					self.dispatch(message.Channel, message.Payload)
				}
			}
		}
		drain()

		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.ResubscribeTimeout):
		}
	}
}

func (self *keyEventSubscriber) Close() {
	self.cancel()
}
