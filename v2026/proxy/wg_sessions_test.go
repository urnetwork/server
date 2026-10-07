package proxy

import (
	"net/netip"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	proxyconnect "github.com/urnetwork/proxy/v2026"
)

// A session family on its own registry, so a test reads exactly what it fed.
func newTestWgSessionMetrics() (*wgSessionMetrics, *prometheus.Registry) {
	registry := prometheus.NewRegistry()
	return newWgSessionMetrics(registry), registry
}

// The ended-session summary's sum and count, read back through the registry.
func wgSessionDurationTotals(t *testing.T, registry *prometheus.Registry) (sum float64, count uint64) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_proxy_wg_session_duration_seconds" {
			continue
		}
		summary := family.GetMetric()[0].GetSummary()
		return summary.GetSampleSum(), summary.GetSampleCount()
	}
	return 0, 0
}

func wgSessionCounts(m *wgSessionMetrics) (active float64, started float64, ended float64) {
	return testutil.ToFloat64(m.active), testutil.ToFloat64(m.started), testutil.ToFloat64(m.ended)
}

// A session follows the handshake timeline: it opens on the first handshake,
// is extended by every one inside the window, ends once a window passes with
// none -- measured first to last -- and a later handshake is a new session.
func TestWgSessionMetricsFollowTheHandshakeTimeline(t *testing.T) {
	m, registry := newTestWgSessionMetrics()
	window := 3 * time.Minute
	t0 := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	clientIp := netip.MustParseAddr("10.0.0.2")
	peer := func(lastHandshake time.Time) map[netip.Addr]*proxyconnect.WgPeerStatus {
		return map[netip.Addr]*proxyconnect.WgPeerStatus{
			clientIp: {ClientIpv4: clientIp, LastHandshake: lastHandshake},
		}
	}
	assertCounts := func(step string, active float64, started float64, ended float64) {
		t.Helper()
		if a, s, e := wgSessionCounts(m); a != active || s != started || e != ended {
			t.Fatalf("%s: active=%v started=%v ended=%v, expected %v/%v/%v", step, a, s, e, active, started, ended)
		}
	}

	// a registered peer that never completed a handshake is not in session
	m.sample(peer(time.Time{}), t0, window)
	assertCounts("never handshook", 0, 0, 0)

	// the first handshake opens one
	m.sample(peer(t0), t0, window)
	assertCounts("first handshake", 1, 1, 0)

	// a re-handshake inside the window extends it, and a repeat sample of
	// the same handshake changes nothing
	m.sample(peer(t0.Add(2*time.Minute)), t0.Add(2*time.Minute+time.Second), window)
	m.sample(peer(t0.Add(2*time.Minute)), t0.Add(2*time.Minute+2*time.Second), window)
	assertCounts("extended", 1, 1, 0)

	// still inside the window just before it closes
	m.sample(peer(t0.Add(2*time.Minute)), t0.Add(2*time.Minute).Add(window), window)
	assertCounts("edge of the window", 1, 1, 0)

	// a whole window of silence ends it, measured first handshake to last
	m.sample(peer(t0.Add(2*time.Minute)), t0.Add(2*time.Minute).Add(window).Add(time.Second), window)
	assertCounts("ended", 0, 1, 1)
	if sum, count := wgSessionDurationTotals(t, registry); count != 1 || sum != 120 {
		t.Fatalf("ended session duration sum=%v count=%d, expected 120s once", sum, count)
	}

	// a later handshake is a new session, and the peer leaving the table
	// ends it
	m.sample(peer(t0.Add(10*time.Minute)), t0.Add(10*time.Minute), window)
	assertCounts("new session", 1, 2, 1)
	m.sample(map[netip.Addr]*proxyconnect.WgPeerStatus{}, t0.Add(10*time.Minute+time.Second), window)
	assertCounts("peer removed", 0, 2, 2)
	if sum, count := wgSessionDurationTotals(t, registry); count != 2 || sum != 120 {
		t.Fatalf("durations after a one-handshake session sum=%v count=%d, expected 120s over two", sum, count)
	}
}

// Every peer is judged on its own handshake, and a peer whose handshake
// regressed to zero -- a device restart -- is no longer in session.
func TestWgSessionMetricsCountEachPeerOnItsOwnHandshake(t *testing.T) {
	m, _ := newTestWgSessionMetrics()
	window := 3 * time.Minute
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	statuses := map[netip.Addr]*proxyconnect.WgPeerStatus{}
	for i, age := range []time.Duration{0, time.Minute, 2 * time.Minute, 4 * time.Minute} {
		clientIp := netip.AddrFrom4([4]byte{10, 0, 0, byte(10 + i)})
		statuses[clientIp] = &proxyconnect.WgPeerStatus{ClientIpv4: clientIp, LastHandshake: now.Add(-age)}
	}
	// a nil status is tolerated
	statuses[netip.MustParseAddr("10.0.0.99")] = nil

	m.sample(statuses, now, window)
	if active, started, ended := wgSessionCounts(m); active != 3 || started != 3 || ended != 0 {
		t.Fatalf("active=%v started=%v ended=%v, expected three of four in session", active, started, ended)
	}
	// the device restarted: handshakes read as zero until peers re-establish
	for _, status := range statuses {
		if status != nil {
			status.LastHandshake = time.Time{}
		}
	}
	m.sample(statuses, now.Add(time.Second), window)
	if active, _, ended := wgSessionCounts(m); active != 0 || ended != 3 {
		t.Fatalf("after a restart active=%v ended=%v, expected every session ended", active, ended)
	}
}

func TestWgSessionDefaultsFollowTheKeyLifetime(t *testing.T) {
	settings := DefaultProxySettings()
	if settings.WgSessionWindow != 3*time.Minute {
		t.Fatalf("window = %s, expected the three minute key lifetime", settings.WgSessionWindow)
	}
	if settings.WgSessionSampleTimeout <= 0 || settings.WgSessionWindow < settings.WgSessionSampleTimeout {
		t.Fatalf("sample timeout = %s does not fit the window", settings.WgSessionSampleTimeout)
	}
}
