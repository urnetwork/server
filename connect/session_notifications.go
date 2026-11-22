package connect

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/gorilla/websocket"
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"strings"
	"time"
)

type sessionNotificationListener struct {
	recheck func()
	hint    func(*protocol.NetworkSessionsChanged)
}
type sessionNotificationGroup struct {
	ctx       context.Context
	cancel    context.CancelFunc
	kick      chan struct{}
	listeners map[int64]sessionNotificationListener
	// the revision each hint listener last received, so an unchanged
	// revision is not sent again (S1 W11)
	delivered map[int64]string
}

// The inventory revision (eid) changes on every session created, revoked or
// expired; its events refresh the hints. The revocation event (rid) is written
// after every revocation record of the network refuses the revoked sessions
// (session/session_revocation_event.go); only its events recheck the leases.
func sessionEventPattern(db int) string { return fmt.Sprintf("__keyspace@%d__:{ns_*}eid", db) }
func sessionRevocationEventPattern(db int) string {
	return fmt.Sprintf("__keyspace@%d__:{ns_*}rid", db)
}
func parseSessionEvent(channel string) (server.Id, bool) {
	return parseSessionKeyEvent(channel, "}eid")
}
func parseSessionRevocationEvent(channel string) (server.Id, bool) {
	return parseSessionKeyEvent(channel, "}rid")
}
func parseSessionKeyEvent(channel string, suffix string) (server.Id, bool) {
	at := strings.Index(channel, ":{ns_")
	if at < 0 || !strings.HasSuffix(channel, suffix) {
		return server.Id{}, false
	}
	id, err := server.ParseId(channel[at+5 : len(channel)-len(suffix)])
	return id, err == nil
}

// One group per network/process does one bounded revision read, coalescing
// outbound hints for five seconds. Local authorization kicks do not wait on it.
//
// Registration rechecks only the new listener. A session event that landed
// between its final authorization check and this registration reached no
// listener of its own, so the new listener revalidates once. A peer joining
// changes no other listener's authorization: rechecking the whole network
// here made every connection cost one authoritative PostgreSQL check per
// connection of its network on the process, quadratic in a reconnect wave.
func (self *keyEventSubscriber) AddSessionListener(networkId server.Id, recheck func(), hint func(*protocol.NetworkSessionsChanged)) func() {
	self.stateLock.Lock()
	if self.sessionListeners == nil {
		self.sessionListeners = map[server.Id]*sessionNotificationGroup{}
	}
	group := self.sessionListeners[networkId]
	if group == nil {
		ctx, cancel := context.WithCancel(self.ctx)
		group = &sessionNotificationGroup{ctx: ctx, cancel: cancel, kick: make(chan struct{}, 1), listeners: map[int64]sessionNotificationListener{}, delivered: map[int64]string{}}
		self.sessionListeners[networkId] = group
		go self.runSessionNotifications(networkId, group)
	}
	id := self.nextListenerId
	self.nextListenerId++
	group.listeners[id] = sessionNotificationListener{recheck, hint}
	// the coalesced revision read delivers the new listener its first hint
	select {
	case group.kick <- struct{}{}:
	default:
	}
	self.stateLock.Unlock()
	if recheck != nil {
		recheck()
	}
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(group.listeners, id)
		delete(group.delivered, id)
		if len(group.listeners) == 0 && self.sessionListeners[networkId] == group {
			delete(self.sessionListeners, networkId)
			group.cancel()
		}
	}
}

// A change of the network's session inventory refreshes its hints. It
// rechecks no lease: a new, extended or expired session ends no transport's
// authority.
func (self *keyEventSubscriber) kickSessionHints(networkId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if group := self.sessionListeners[networkId]; group != nil {
		select {
		case group.kick <- struct{}{}:
		default:
		}
	}
}

// A revocation rechecks every lease of the network at once. The records
// already refuse the revoked sessions when the event is written.
func (self *keyEventSubscriber) recheckSessionListeners(networkId server.Id) {
	self.stateLock.Lock()
	group := self.sessionListeners[networkId]
	listeners := []sessionNotificationListener{}
	if group != nil {
		for _, listener := range group.listeners {
			listeners = append(listeners, listener)
		}
	}
	self.stateLock.Unlock()
	for _, listener := range listeners {
		if listener.recheck != nil {
			listener.recheck()
		}
	}
}

// One recheck per session listener of every network, for a resync to trickle
// with the other registrations over its spread window. Each group's revision
// read is nudged now, so hint listeners converge without waiting.
func (self *keyEventSubscriber) resyncSessionRechecks() []func() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	rechecks := []func(){}
	for _, group := range self.sessionListeners {
		for _, listener := range group.listeners {
			if listener.recheck != nil {
				rechecks = append(rechecks, listener.recheck)
			}
		}
		select {
		case group.kick <- struct{}{}:
		default:
		}
	}
	return rechecks
}
func (self *keyEventSubscriber) runSessionNotifications(networkId server.Id, group *sessionNotificationGroup) {
	var last time.Time
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-group.ctx.Done():
			return
		case <-group.kick:
		case <-ticker.C:
			// The corrective pass refreshes hints only. Each lease rechecks
			// on its own timer at most a minute apart, and session events and
			// resubscribe resyncs still recheck at once; rechecking every
			// lease here checked each connection twice a minute.
		}
		if delay := time.Until(last.Add(5 * time.Second)); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-group.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		select {
		case <-group.kick:
		default:
		}
		// The revision read only produces hints, and it returns every live
		// session of the network while its prune can publish a session event
		// that rechecks every listener of the network on every process. Skip
		// it while no listener of the group consumes hints.
		hinted := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			for _, listener := range group.listeners {
				if listener.hint != nil {
					return true
				}
			}
			return false
		}()
		if !hinted {
			continue
		}
		revision := self.sessionRevision
		if revision == nil {
			revision = session.ReadNetworkSessionRevision
		}
		ctx, cancel := context.WithTimeout(group.ctx, 100*time.Millisecond)
		generation, eventId, err := revision(ctx, networkId)
		cancel()
		if err != nil {
			continue
		}
		hint := &protocol.NetworkSessionsChanged{Generation: generation, EventId: eventId}
		// Each hint listener gets a revision once. Every resident used to be
		// sent the network's revision every minute whether or not it changed;
		// a new listener, or a change, still gets it at once.
		key := fmt.Sprintf("%s/%d", generation, eventId)
		self.stateLock.Lock()
		hints := []func(*protocol.NetworkSessionsChanged){}
		for id, listener := range group.listeners {
			if listener.hint != nil && group.delivered[id] != key {
				group.delivered[id] = key
				hints = append(hints, listener.hint)
			}
		}
		self.stateLock.Unlock()
		last = time.Now()
		for _, deliver := range hints {
			deliver(hint)
		}
	}
}

func writeConnectAuthorizationClose(conn clientconnect.H1MessageConn, code int) {
	reason := uint32(code - 4000)
	message := []byte{clientconnect.TransportControlClose, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(message[1:], reason)
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = conn.WriteMessage(websocket.BinaryMessage, message)
	if ws, ok := clientconnect.H1WebSocketConn(conn); ok {
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, "authorization ended"), time.Now().Add(time.Second))
	}
}
