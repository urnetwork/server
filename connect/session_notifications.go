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
}

func sessionEventPattern(db int) string { return fmt.Sprintf("__keyspace@%d__:{ns_*}eid", db) }
func parseSessionEvent(channel string) (server.Id, bool) {
	at := strings.Index(channel, ":{ns_")
	if at < 0 || !strings.HasSuffix(channel, "}eid") {
		return server.Id{}, false
	}
	id, err := server.ParseId(channel[at+5 : len(channel)-4])
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
		group = &sessionNotificationGroup{ctx: ctx, cancel: cancel, kick: make(chan struct{}, 1), listeners: map[int64]sessionNotificationListener{}}
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
		if len(group.listeners) == 0 && self.sessionListeners[networkId] == group {
			delete(self.sessionListeners, networkId)
			group.cancel()
		}
	}
}
func (self *keyEventSubscriber) kickSession(networkId server.Id) {
	self.stateLock.Lock()
	group := self.sessionListeners[networkId]
	listeners := []sessionNotificationListener{}
	if group != nil {
		for _, listener := range group.listeners {
			listeners = append(listeners, listener)
		}
		select {
		case group.kick <- struct{}{}:
		default:
		}
	}
	self.stateLock.Unlock()
	for _, listener := range listeners {
		if listener.recheck != nil {
			listener.recheck()
		}
	}
}
func (self *keyEventSubscriber) resyncSessions() {
	self.stateLock.Lock()
	ids := []server.Id{}
	for id := range self.sessionListeners {
		ids = append(ids, id)
	}
	self.stateLock.Unlock()
	for _, id := range ids {
		self.kickSession(id)
	}
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
			self.kickSession(networkId)
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
			revision = session.NetworkSessionRevision
		}
		ctx, cancel := context.WithTimeout(group.ctx, 100*time.Millisecond)
		generation, eventId, err := revision(ctx, networkId)
		cancel()
		if err != nil {
			continue
		}
		hint := &protocol.NetworkSessionsChanged{Generation: generation, EventId: eventId}
		self.stateLock.Lock()
		listeners := []sessionNotificationListener{}
		for _, listener := range group.listeners {
			listeners = append(listeners, listener)
		}
		self.stateLock.Unlock()
		last = time.Now()
		for _, listener := range listeners {
			if listener.hint != nil {
				listener.hint(hint)
			}
		}
	}
}

func writeConnectAuthorizationClose(conn clientconnect.H1MessageConn, code int) {
	reason := uint32(code - 4000)
	message := []byte{clientconnect.TransportControlClose, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(message[1:], reason)
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = conn.WriteMessage(websocket.BinaryMessage, message)
	if ws, ok := conn.(*websocket.Conn); ok {
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, "authorization ended"), time.Now().Add(time.Second))
	}
}
