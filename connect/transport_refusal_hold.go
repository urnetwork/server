// A WebSocket refused for its credential is held open for a while before its
// authorization close, when its client cannot read that close. Every build
// before connect 2026-10-09 reads a refusal after its 101 as a dropped
// connection and redials it within five seconds, forever: on Main on
// 2026-10-11 about 13k such transports cost about 4.5k TLS handshakes, JWT
// verifications and rate limit round trips a second. Held for the hold
// timeout, each redials about once a minute instead.
//
// A client that can read the close declares it with stream lease version 1,
// which came with the authorization closes in connect 2026-10-09; it gets the
// close at once and stops. So does a frame-auth client, which gives up waiting
// for its auth echo within seconds, so a hold would keep only this end of a
// socket the client already closed. H1+ and H3 refuse at once with a terminal
// status or close, which every client of theirs reads.
//
// A held socket is never read. An empty binary message, the platform's
// keepalive, goes out every keepalive interval, inside the 30 second read
// deadline those clients keep, so a client waits on the held socket instead of
// timing it out and redialing. At the hold timeout the authorization close
// goes out (transport_credential_refusal.go) and the socket closes. A write
// that fails ends its hold at once.
//
// The hold is bounded: past the max count a refusal closes at once, as before.
// One goroutine serves every hold on a shared tick, and holds release in the
// order they started, since each lasts the same time. A drain of the exchange
// or a close of the handler releases every hold and closes later refusals at
// once.
//
// hold is safe for concurrent use. Only the run goroutine writes to or closes
// a held socket, and only it reads or clears a held refusal's conn.
package connect

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"

	clientconnect "github.com/urnetwork/connect"
)

const (
	defaultRefusalHoldTimeout           = 60 * time.Second
	defaultRefusalHoldKeepaliveInterval = 15 * time.Second
	defaultRefusalHoldMaxCount          = 16 * 1024
	// the shared tick, which bounds how late a release or keepalive goes out
	refusalHoldTickInterval = time.Second
	// a held socket is never read and carries a few bytes at a time, so a
	// write that stalls longer has a dead peer
	refusalHoldWriteTimeout = time.Second
)

var connectRefusalHolds = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork", Subsystem: "connect", Name: "refusal_holds_total",
	Help: "WebSocket credential refusals offered to the hold, by outcome: held, over_cap (closed at once past the bound) or closing (closed at once during a drain or close).",
}, []string{"outcome"})

var connectRefusalHoldReleases = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork", Subsystem: "connect", Name: "refusal_hold_releases_total",
	Help: "Held WebSocket credential refusals released, by cause: timeout (closed with the authorization close), drain (closed early for a drain or close) or write_error (the peer was gone).",
}, []string{"cause"})

var connectRefusalHoldsActive = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork", Subsystem: "connect", Name: "refusal_holds_active",
	Help: "WebSocket credential refusals held open before their authorization close.",
})

func init() {
	prometheus.MustRegister(connectRefusalHolds, connectRefusalHoldReleases, connectRefusalHoldsActive)
}

type connectHeldRefusal struct {
	// nil once released early; owned by the run goroutine
	conn        clientconnect.H1MessageConn
	closeCode   int
	releaseTime time.Time
}

type connectRefusalHold struct {
	ctx               context.Context
	draining          func() bool
	timeout           time.Duration
	keepaliveInterval time.Duration
	maxCount          int
	now               func() time.Time
	tick              <-chan time.Time
	// nil outside package tests
	heldForTest   func(outcome string)
	tickedForTest func()

	stateLock sync.Mutex
	// in hold order, which is release order
	refusals []*connectHeldRefusal
	// the refusals still held, which excludes the ones released early
	heldCount int
	// set once every hold was released for a drain or close
	released      bool
	keepaliveTime time.Time
}

func newConnectRefusalHold(ctx context.Context, draining func() bool, settings *ConnectHandlerSettings) *connectRefusalHold {
	now := settings.refusalHoldNowForTest
	if now == nil {
		now = time.Now
	}
	return &connectRefusalHold{
		ctx:               ctx,
		draining:          draining,
		timeout:           settings.RefusalHoldTimeout,
		keepaliveInterval: settings.RefusalHoldKeepaliveInterval,
		maxCount:          settings.RefusalHoldMaxCount,
		now:               now,
		tick:              settings.refusalHoldTickForTest,
		heldForTest:       settings.refusalHoldHeldForTest,
		tickedForTest:     settings.refusalHoldTickedForTest,
		keepaliveTime:     now(),
	}
}

// Takes ownership of conn and its authorization close code when the hold is
// on and has room. False leaves conn with the caller, which closes at once. A
// handler a fixture built without its constructor has no hold.
func (self *connectRefusalHold) hold(conn clientconnect.H1MessageConn, closeCode int) bool {
	if self == nil || self.timeout <= 0 {
		return false
	}
	outcome := func() string {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.released || self.ctx.Err() != nil || self.draining() {
			return "closing"
		}
		if self.maxCount <= self.heldCount {
			return "over_cap"
		}
		self.refusals = append(self.refusals, &connectHeldRefusal{
			conn:        conn,
			closeCode:   closeCode,
			releaseTime: self.now().Add(self.timeout),
		})
		self.heldCount += 1
		connectRefusalHoldsActive.Inc()
		return "held"
	}()
	connectRefusalHolds.WithLabelValues(outcome).Inc()
	if self.heldForTest != nil {
		self.heldForTest(outcome)
	}
	return outcome == "held"
}

func (self *connectRefusalHold) run() {
	// a panic still releases every hold
	defer self.releaseAll()
	tick := self.tick
	if tick == nil {
		ticker := time.NewTicker(refusalHoldTickInterval)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-tick:
		}
		if self.draining() {
			self.releaseAll()
		} else {
			self.releaseDue()
			self.keepalive()
		}
		if self.tickedForTest != nil {
			self.tickedForTest()
		}
	}
}

// Closes every hold whose timeout passed with its authorization close.
func (self *connectRefusalHold) releaseDue() {
	now := self.now()
	dueRefusals := func() []*connectHeldRefusal {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		i := 0
		for i < len(self.refusals) && !now.Before(self.refusals[i].releaseTime) {
			i += 1
		}
		return self.popWithLock(i)
	}()
	self.close(dueRefusals, "timeout")
}

// Releases every hold with its authorization close, and closes later
// refusals at once.
func (self *connectRefusalHold) releaseAll() {
	refusals := func() []*connectHeldRefusal {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.released = true
		return self.popWithLock(len(self.refusals))
	}()
	self.close(refusals, "drain")
}

// Removes the first count refusals from the queue, clearing their slots so the
// queue keeps no closed socket alive.
func (self *connectRefusalHold) popWithLock(count int) []*connectHeldRefusal {
	refusals := slices.Clone(self.refusals[:count])
	clear(self.refusals[:count])
	self.refusals = self.refusals[count:]
	for _, refusal := range refusals {
		if refusal.conn != nil {
			self.heldCount -= 1
			connectRefusalHoldsActive.Dec()
		}
	}
	return refusals
}

func (self *connectRefusalHold) close(refusals []*connectHeldRefusal, cause string) {
	for _, refusal := range refusals {
		if refusal.conn == nil {
			continue
		}
		writeConnectAuthorizationClose(refusal.conn, refusal.closeCode)
		refusal.conn.Close()
		refusal.conn = nil
		connectRefusalHoldReleases.WithLabelValues(cause).Inc()
	}
}

// Sends the keepalive to every held socket once per keepalive interval. A
// write that fails ends that hold at once.
func (self *connectRefusalHold) keepalive() {
	now := self.now()
	heldRefusals := func() []*connectHeldRefusal {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if now.Before(self.keepaliveTime.Add(self.keepaliveInterval)) {
			return nil
		}
		self.keepaliveTime = now
		return slices.Clone(self.refusals)
	}()
	for _, refusal := range heldRefusals {
		if refusal.conn == nil {
			continue
		}
		err := refusal.conn.SetWriteDeadline(time.Now().Add(refusalHoldWriteTimeout))
		if err == nil {
			err = refusal.conn.WriteMessage(websocket.BinaryMessage, []byte{})
		}
		if err == nil {
			continue
		}
		refusal.conn.Close()
		refusal.conn = nil
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.heldCount -= 1
		}()
		connectRefusalHoldsActive.Dec()
		connectRefusalHoldReleases.WithLabelValues("write_error").Inc()
	}
}

// The refusals still held, for tests.
func (self *connectRefusalHold) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.heldCount
}
