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
	keyEventsDispatchedClientClose = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_key_events_dispatched_client_close_total",
		Help: "client close-request key events dispatched to registered listeners",
	})
	keyEventsDispatchedClientRemoved = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_key_events_dispatched_client_removed_total",
		Help: "client removed key events dispatched to the exchange",
	})
	keyEventsDispatchedClientRecycle = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_key_events_dispatched_client_recycle_total",
		Help: "stuck provider recycle flag key events dispatched to the exchange",
	})
	keyEventsDispatchedSidecarPush = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_key_events_dispatched_sidecar_push_total",
		Help: "sidecar hint key events (an INCR on {scp_<C>}n) dispatched to resident publishers",
	})
)

func init() {
	prometheus.MustRegister(
		keyEventsDispatchedPeers,
		keyEventsDispatchedHops,
		keyEventResubscribes,
		keyEventResyncs,
		keyEventsDispatchedClientClose,
		keyEventsDispatchedClientRemoved,
		keyEventsDispatchedClientRecycle,
		keyEventsDispatchedSidecarPush,
	)
}

// A resident publisher (sidecar_publisher.go), registered under its client's id
// and kicked by the INCR on its {scp_<C>}n, §6.8's one trigger: HSET and XADD
// emit nothing under production's classes, and EXPIRE NX's g, an expiry and a
// del are no hint.
type sidecarPushListener interface {
	// Never blocks: the dispatch calls it on the subscription's goroutine.
	Kick()
}

// keyEventSubscriber is the exchange's shared consumer of redis keyspace
// notifications for the peers + stream-hops listeners (PEERSSTREAMS2.md).
// ONE subscriber per exchange process: it holds the per-master psubscribe
// connections (via server.SubscribeKeyEvents) and demuxes key events to the
// in-process listeners registered by residents. The demux only routes — it
// performs no registry I/O and never blocks: peer deltas defer one shared
// registry read to the listener workers, and all listener inputs are
// non-blocking and degrade to a resync under pressure. Delivery reaches only
// explicitly registered listeners; there is no broadcast path.
//
// Every (re)established subscription triggers a trickled resync of all
// registrations — anything published while unsubscribed is gone forever.
type keyEventSubscriber struct {
	ctx    context.Context
	cancel context.CancelFunc

	settings *KeyEventDeliverySettings

	stateLock        sync.Mutex
	nextListenerId   int64
	sessionListeners map[server.Id]*sessionNotificationGroup
	peerListeners    map[server.Id]map[int64]*model.NetworkPeerListener
	hopListeners     map[server.Id]map[int64]*model.StreamHopListener
	closeListeners   map[server.Id]map[int64]*clientCloseListener
	// resident publishers by client (sidecar_publisher.go)
	sidecarPushListeners map[server.Id]map[int64]sidecarPushListener
	// closes what this process holds of a client removed through the api
	// (CLIENTCLOSE.md); it must not block
	clientRemoved func(server.Id)
	// recycles a stuck provider the api reported, if its resident is here
	// (client_recycle.go); it must not block. Nil ignores the flags.
	clientRecycle func(server.Id)
	// whether the subscription includes the recycle flags' channel. Every
	// keyspace notification on every master is matched against every
	// pattern, so only an exchange whose recycle ceiling is on subscribes it
	clientRecyclePattern bool
	// whether the subscription includes every client's {scp_<C>}n, for the
	// same reason: only an exchange with EnableSidecarPush subscribes it
	sidecarPushPattern bool
	newPeerDelta       func(context.Context, server.Id, server.Id, string) *model.NetworkPeerDelta
	// the session revision read behind hints; session.NetworkSessionRevision
	// when unset
	sessionRevision func(context.Context, server.Id) (string, int64, error)

	resyncLock   sync.Mutex
	resyncCancel context.CancelFunc
}

// clientRecyclePattern subscribes the recycle flags' channel; an exchange
// passes its EnableStuckProviderRecycle. sidecarPushPattern subscribes every
// client's {scp_<C>}n; an exchange passes its EnableSidecarPush.
func newKeyEventSubscriber(ctx context.Context, settings *KeyEventDeliverySettings, clientRecyclePattern bool, sidecarPushPattern bool) *keyEventSubscriber {
	cancelCtx, cancel := context.WithCancel(ctx)
	s := &keyEventSubscriber{
		ctx:                  cancelCtx,
		cancel:               cancel,
		settings:             settings,
		peerListeners:        map[server.Id]map[int64]*model.NetworkPeerListener{},
		hopListeners:         map[server.Id]map[int64]*model.StreamHopListener{},
		closeListeners:       map[server.Id]map[int64]*clientCloseListener{},
		sidecarPushListeners: map[server.Id]map[int64]sidecarPushListener{},
		clientRecyclePattern: clientRecyclePattern,
		sidecarPushPattern:   sidecarPushPattern,
		newPeerDelta:         model.NewNetworkPeerDelta,
	}
	go server.HandleError(s.run)
	return s
}

// The psubscribe patterns of every (re)subscription on db.
func (self *keyEventSubscriber) patterns(db int) []string {
	patterns := []string{
		model.NetworkPeerKeyEventPattern(db),
		sessionEventPattern(db),
		model.StreamHopsKeyEventPattern(db),
		model.ClientRemovedKeyEventPattern(db),
		model.ClientCloseKeyEventPattern(db),
	}
	if self.clientRecyclePattern {
		patterns = append(patterns, model.ClientRecycleKeyEventPattern(db))
	}
	if self.sidecarPushPattern {
		patterns = append(patterns, model.ScpHintKeyEventPattern(db))
	}
	return patterns
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

// AddCloseListener registers a resident client's close-request listener for
// key-event kicks (CLIENTCLOSE.md).
func (self *keyEventSubscriber) AddCloseListener(clientId server.Id, listener *clientCloseListener) (remove func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	listenerId := self.nextListenerId
	self.nextListenerId += 1
	listeners, ok := self.closeListeners[clientId]
	if !ok {
		listeners = map[int64]*clientCloseListener{}
		self.closeListeners[clientId] = listeners
	}
	listeners[listenerId] = listener
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(listeners, listenerId)
		if len(listeners) == 0 {
			delete(self.closeListeners, clientId)
		}
	}
}

// Registers a resident publisher for its client's {scp_<C>}n events, which
// kick it, as AddCloseListener does a close listener (sidecar_publisher.go).
func (self *keyEventSubscriber) AddSidecarPushListener(clientId server.Id, listener sidecarPushListener) (remove func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	listenerId := self.nextListenerId
	self.nextListenerId += 1
	listeners, ok := self.sidecarPushListeners[clientId]
	if !ok {
		listeners = map[int64]sidecarPushListener{}
		self.sidecarPushListeners[clientId] = listeners
	}
	listeners[listenerId] = listener
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(listeners, listenerId)
		if len(listeners) == 0 {
			delete(self.sidecarPushListeners, clientId)
		}
	}
}

// SetClientRemoved sets what a client's removed event runs. It must not block.
func (self *keyEventSubscriber) SetClientRemoved(clientRemoved func(server.Id)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clientRemoved = clientRemoved
}

// Sets what a client's recycle flag event runs. It must not block.
func (self *keyEventSubscriber) SetClientRecycle(clientRecycle func(server.Id)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clientRecycle = clientRecycle
}

// Kicks every close listener, trickled over the resync spread. Requests added
// while unsubscribed have no event of their own, and otherwise wait for the
// client's next request or expire. Only a (re)subscribe can lose an event, so
// the corrective poll does not run this.
func (self *keyEventSubscriber) resyncCloseListeners() {
	listeners := []*clientCloseListener{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, clientListeners := range self.closeListeners {
			for _, listener := range clientListeners {
				listeners = append(listeners, listener)
			}
		}
	}()
	if len(listeners) == 0 {
		return
	}
	spacing := self.settings.ResyncSpreadTimeout / time.Duration(len(listeners))
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

// Kicks every publisher, trickled over the resync spread, so a hint published
// while unsubscribed costs each one XRANGE: an event lost in the gap has no
// other way back before the client's hourly resync. As with close listeners,
// only a (re)subscribe can lose an event, so the corrective poll does not run
// this.
func (self *keyEventSubscriber) resyncSidecarPushListeners() {
	listeners := []sidecarPushListener{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, clientListeners := range self.sidecarPushListeners {
			for _, listener := range clientListeners {
				listeners = append(listeners, listener)
			}
		}
	}()
	if len(listeners) == 0 {
		return
	}
	spacing := self.settings.ResyncSpreadTimeout / time.Duration(len(listeners))
	go server.HandleError(func() {
		for _, listener := range listeners {
			listener.Kick()
			keyEventResyncs.Inc()
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(spacing):
			}
		}
	})
}

// resyncAll trickles a forced resync across every registration, spread over
// the resync spread timeout so a resubscribe does not stampede the registry
// with simultaneous full reads. Session listeners trickle too: each recheck
// is an authoritative PostgreSQL read, and a resubscribe or corrective poll
// used to start one for every lease of the process in the same instant. Each
// lease's own recheck timer still bounds its staleness meanwhile.
func (self *keyEventSubscriber) resyncAll() {
	sessionRechecks := self.resyncSessionRechecks()
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
	count := len(sessionRechecks) + len(peerListeners) + len(hopListeners)
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
		for _, recheck := range sessionRechecks {
			recheck()
			select {
			case <-resyncCtx.Done():
				return
			case <-time.After(spacing):
			}
		}
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
	if self.sidecarPushPattern {
		if clientId, ok := model.ParseScpHintKeyEvent(channel); ok {
			self.dispatchSidecarPush(server.Id(clientId), event)
			return
		}
	}
	if networkId, ok := parseSessionEvent(channel); ok {
		self.kickSession(networkId)
		return
	}
	if clientId, ok := model.ParseClientRemovedKeyEvent(channel); ok {
		if event == "set" {
			var clientRemoved func(server.Id)
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				clientRemoved = self.clientRemoved
			}()
			if clientRemoved != nil {
				clientRemoved(clientId)
				keyEventsDispatchedClientRemoved.Inc()
			}
		}
		return
	}
	if clientId, ok := model.ParseClientRecycleKeyEvent(channel); ok {
		// a set only if absent emits set only when it sets; the ttl's expire
		// and expired are not reports
		if event == "set" {
			var clientRecycle func(server.Id)
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				clientRecycle = self.clientRecycle
			}()
			if clientRecycle != nil {
				clientRecycle(clientId)
				keyEventsDispatchedClientRecycle.Inc()
			}
		}
		return
	}
	if clientId, ok := model.ParseClientCloseKeyEvent(channel); ok {
		// a take (spop) or the set emptying (del) is the listener's own work
		if event == "sadd" {
			listeners := []*clientCloseListener{}
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				for _, listener := range self.closeListeners[clientId] {
					listeners = append(listeners, listener)
				}
			}()
			for _, listener := range listeners {
				listener.Kick()
				keyEventsDispatchedClientClose.Inc()
			}
		}
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
			if len(listeners) == 0 {
				return
			}
			// Construction is lazy: dispatch remains nonblocking, while the
			// first listener worker performs one metadata/version read shared
			// by every resident listener in this process.
			delta := self.newPeerDelta(self.ctx, networkId, clientId, event)
			for _, listener := range listeners {
				listener.ApplyDelta(delta)
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

// Kicks a client's publishers on its n's INCR (D-PROTO-12), the one event that
// is a hint: EXPIRE NX's g events, an expiry's expired and a del are none.
func (self *keyEventSubscriber) dispatchSidecarPush(clientId server.Id, event string) {
	if event != "incrby" {
		return
	}
	listeners := []sidecarPushListener{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, listener := range self.sidecarPushListeners[clientId] {
			listeners = append(listeners, listener)
		}
	}()
	for _, listener := range listeners {
		listener.Kick()
		keyEventsDispatchedSidecarPush.Inc()
	}
}

// run supervises the subscription set: (re)subscribe, resync everything the
// gap may have missed, drain until the subscription dies or the topology
// changes, repeat. Errors back off; the loop only exits with the context.
func (self *keyEventSubscriber) run() {
	defer self.cancel()

	patterns := self.patterns(server.RedisDb())

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
		self.resyncCloseListeners()
		self.resyncSidecarPushListeners()

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
