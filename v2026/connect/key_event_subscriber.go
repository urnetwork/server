package connect

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
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

	resyncLock   sync.Mutex
	resyncCancel context.CancelFunc
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
	peerListeners := map[server.Id][]*model.NetworkPeerListener{}
	hopListeners := []*model.StreamHopListener{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for networkId, listeners := range self.peerListeners {
			for _, listener := range listeners {
				peerListeners[networkId] = append(peerListeners[networkId], listener)
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

	self.resyncLock.Lock()
	if self.resyncCancel != nil {
		self.resyncCancel()
	}
	resyncCtx, resyncCancel := context.WithCancel(self.ctx)
	self.resyncCancel = resyncCancel
	self.resyncLock.Unlock()

	go server.HandleError(func() {
		for networkId, listeners := range peerListeners {
			// One authoritative read per network/process, then distribute the
			// immutable snapshot to every resident listener. This prevents a
			// normal network mutation or reconnect from becoming N duplicate
			// full reads and N copies of the peer list.
			var eventId int64
			var peers []*model.NetworkPeer
			if r := server.HandleError(func() {
				eventId, peers = model.GetNetworkPeers(resyncCtx, networkId)
			}); r != nil {
				// Do not fan a failed shared read back into one full read per
				// listener: that recreates the incident-time N-way Redis
				// stampede this process-wide reconcile exists to avoid. The
				// next corrective epoch retries the one shared read.
			} else {
				snapshot := model.PrepareNetworkPeerSnapshot(eventId, peers)
				for _, listener := range listeners {
					listener.ApplySnapshot(snapshot)
					keyEventResyncs.Inc()
				}
			}
			select {
			case <-resyncCtx.Done():
				return
			case <-time.After(spacing):
			}
		}
		for _, listener := range hopListeners {
			listener.Reconcile()
			keyEventResyncs.Inc()
			select {
			case <-resyncCtx.Done():
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
			switch event {
			case "del", "expired", "evicted", "unlink":
				// Expiry/removal does not increment the stream event id.
				// Force a state-based full reconcile rather than only checking
				// the unchanged counter.
				listener.Reconcile()
			default:
				listener.Kick()
			}
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
			correctivePollInterval := self.settings.CorrectivePollInterval
			if correctivePollInterval <= 0 {
				// Defensive fallback for partially populated settings. Avoid
				// a process panic from time.NewTicker while preserving the
				// production default convergence bound.
				correctivePollInterval = defaultKeyEventCorrectivePollInterval
			}
			correctiveTicker := time.NewTicker(correctivePollInterval)
			defer correctiveTicker.Stop()
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
				case <-correctiveTicker.C:
					// Correct missed expiry/events even when the registry
					// version did not move. resyncAll cancels an older wave
					// before starting this generation.
					self.resyncAll()
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
	self.resyncLock.Lock()
	if self.resyncCancel != nil {
		self.resyncCancel()
	}
	self.resyncLock.Unlock()
	self.cancel()
}
