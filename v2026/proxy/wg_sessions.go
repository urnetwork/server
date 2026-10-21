package proxy

// WireGuard session metrics.
//
// WireGuard has no session teardown. A peer is in session while it keeps
// completing handshakes: a client with traffic re-handshakes about every two
// minutes (REKEY_AFTER_TIME), and one that has gone silent stops, after which
// its keys expire (REJECT_AFTER_TIME, three minutes). So a session here is
// what the handshake timeline shows. It starts at a peer's first handshake
// inside the window after having none, stays open while the peer's newest
// handshake is inside the window, and ends -- with a duration of first
// handshake to last -- once a whole window has passed without a new one, or
// the peer has left the table.
//
// Sampled from the device's peer table. Identity-free at the metrics
// boundary: open sessions are keyed by client address in memory only, and
// what is published is counts and durations.

import (
	"net/netip"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/proxy/v2026"
)

type wgSessionMetrics struct {
	active  prometheus.Gauge
	started prometheus.Counter
	ended   prometheus.Counter
	seconds prometheus.Summary

	stateLock sync.Mutex
	sessions  map[netip.Addr]wgSession
}

// One open session: the handshake that began it and the newest one.
type wgSession struct {
	firstHandshake time.Time
	lastHandshake  time.Time
}

// newWgSessionMetrics constructs one independently registerable family.
func newWgSessionMetrics(registerer prometheus.Registerer) *wgSessionMetrics {
	metrics := &wgSessionMetrics{
		active: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "urnetwork", Subsystem: "proxy", Name: "wg_sessions_active",
			Help: "WireGuard peers whose newest completed handshake is inside the session window",
		}),
		started: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "urnetwork", Subsystem: "proxy", Name: "wg_sessions_started_total",
			Help: "WireGuard sessions begun: a peer's first handshake inside the window after having none",
		}),
		ended: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "urnetwork", Subsystem: "proxy", Name: "wg_sessions_ended_total",
			Help: "WireGuard sessions ended: a whole window passed with no new handshake, or the peer left the table",
		}),
		seconds: prometheus.NewSummary(prometheus.SummaryOpts{
			Namespace: "urnetwork", Subsystem: "proxy", Name: "wg_session_duration_seconds",
			Help:       "Ended WireGuard session duration, first completed handshake to last",
			Objectives: nil,
		}),
		sessions: map[netip.Addr]wgSession{},
	}
	registerer.MustRegister(metrics.active, metrics.started, metrics.ended, metrics.seconds)
	return metrics
}

var defaultWgSessionMetrics = newWgSessionMetrics(prometheus.DefaultRegisterer)

// sample applies one reading of the peer table taken at `now`. A peer with a
// handshake inside the window opens or extends its session; an open session
// whose peer has fallen outside the window, or is no longer in the table,
// ends. A peer that has never completed a handshake is not in session.
func (self *wgSessionMetrics) sample(
	statuses map[netip.Addr]*proxy.WgPeerStatus,
	now time.Time,
	window time.Duration,
) {
	windowStart := now.Add(-window)
	inWindow := func(status *proxy.WgPeerStatus) bool {
		return status != nil && !status.LastHandshake.IsZero() && !status.LastHandshake.Before(windowStart)
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	for clientIp, status := range statuses {
		if !inWindow(status) {
			continue
		}
		session, ok := self.sessions[clientIp]
		if !ok {
			self.sessions[clientIp] = wgSession{
				firstHandshake: status.LastHandshake,
				lastHandshake:  status.LastHandshake,
			}
			self.started.Inc()
			continue
		}
		if session.lastHandshake.Before(status.LastHandshake) {
			session.lastHandshake = status.LastHandshake
			self.sessions[clientIp] = session
		}
	}
	for clientIp, session := range self.sessions {
		if inWindow(statuses[clientIp]) {
			continue
		}
		delete(self.sessions, clientIp)
		self.ended.Inc()
		self.seconds.Observe(max(0, session.lastHandshake.Sub(session.firstHandshake)).Seconds())
	}
	self.active.Set(float64(len(self.sessions)))
}

// runSessionMetrics samples the peer table on the session cadence while the
// server lives. A read that fails leaves the open sessions as they are rather
// than ending them: the failure says nothing about the peers.
func (self *wgServer) runSessionMetrics() {
	if self.settings.WgSessionSampleTimeout <= 0 || self.settings.WgSessionWindow <= 0 {
		return
	}
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.WgSessionSampleTimeout):
		}
		statuses, err := self.wgProxy.PeerStatuses()
		if err != nil {
			continue
		}
		defaultWgSessionMetrics.sample(statuses, time.Now(), self.settings.WgSessionWindow)
	}
}
