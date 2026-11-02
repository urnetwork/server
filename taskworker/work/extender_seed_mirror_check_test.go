package work

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/controller"
)

// The mirror check against local TLS mirrors: no public site is contacted.
// httptest's certificate names example.com and the loopback addresses, so a
// route to 127.0.0.1 is the DNS route of a url naming the address, and a route
// whose url names example.com with a dial address of 127.0.0.1 is the
// DNS-independent route: the configured address, with the url's host still
// the server name the certificate is verified for.

type testExtenderSeedMirror struct {
	server   *httptest.Server
	rootKeys *connect.ExtenderRootKeySet
	now      time.Time
	sequence uint64
}

func newTestExtenderSeedMirror(t *testing.T) *testExtenderSeedMirror {
	t.Helper()
	ctx := context.Background()

	// a current bundle, an expired one and a forged one, from the publisher
	operator := newTestExtenderSeedOperator(t)
	fleet := newTestExtenderSeedFleet()
	for range 12 {
		fleet.add(t, connect.ExtenderDirectoryTierOpen, "", 4, 6)
	}
	now := testExtenderSeedEpochStart.Add(time.Hour)
	expiredAt := now.Add(-2 * controller.ExtenderRecordExpireTimeout)
	if result := operator.publisher(t, fleet).publish(ctx, expiredAt); result.outcome != extenderSeedOutcomePublished {
		t.Fatalf("expired bundle: %s", result.outcome)
	}
	_, expired := operator.stored(t, expiredAt)
	result := operator.publisher(t, fleet).publish(ctx, now)
	if result.outcome != extenderSeedOutcomePublished {
		t.Fatalf("current bundle: %s", result.outcome)
	}
	_, current := operator.stored(t, now)
	forger := newTestExtenderSeedOperator(t)
	if result := forger.publisher(t, fleet).publish(ctx, now); result.outcome != extenderSeedOutcomePublished {
		t.Fatalf("forged bundle: %s", result.outcome)
	}
	_, forged := forger.stored(t, now)

	mux := http.NewServeMux()
	serve := func(artifact []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", controller.ExtenderSeedContentType)
			w.Write(artifact)
		}
	}
	mux.HandleFunc(controller.ExtenderSeedPath, serve(current))
	mux.HandleFunc("/expired", serve(expired))
	mux.HandleFunc("/forged", serve(forged))
	mux.HandleFunc("/large", serve(make([]byte, connect.ExtenderSeedMaxEnvelopeByteCount+1)))
	mux.HandleFunc("/garbage", serve([]byte("<html>not a seed</html>")))
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, controller.ExtenderSeedPath, http.StatusFound)
	})
	mux.HandleFunc("/error", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	previousRoots := extenderSeedMirrorRootCAs
	extenderSeedMirrorRootCAs = func() *x509.CertPool {
		roots := x509.NewCertPool()
		roots.AddCert(server.Certificate())
		return roots
	}
	t.Cleanup(func() { extenderSeedMirrorRootCAs = previousRoots })

	return &testExtenderSeedMirror{
		server:   server,
		rootKeys: operator.rootKeys,
		now:      now,
		sequence: result.stored.Sequence,
	}
}

// A route to the test mirror: by address when the host is an address, or the
// DNS-independent route of a named host to the loopback address.
func (self *testExtenderSeedMirror) route(t *testing.T, host string, path string) *controller.ExtenderSeedMirrorRoute {
	t.Helper()
	serverUrl, err := url.Parse(self.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	routeUrl := &url.URL{Scheme: "https", Host: host + ":" + serverUrl.Port(), Path: path}
	route := &controller.ExtenderSeedMirrorRoute{Mirror: "test", Route: controller.ExtenderSeedRouteDns, Url: routeUrl}
	if _, err := netip.ParseAddr(host); err != nil {
		route.Route = controller.ExtenderSeedRouteIpv4
		route.DialAddress = netip.MustParseAddr(serverUrl.Hostname())
	}
	return route
}

func (self *testExtenderSeedMirror) check(t *testing.T, route *controller.ExtenderSeedMirrorRoute) *extenderSeedMirrorResult {
	t.Helper()
	return checkExtenderSeedMirrorRoute(context.Background(), route, self.rootKeys, []string{testExtenderSeedHost}, self.now)
}

func TestExtenderSeedMirrorCheckVerifiesTheServedBundle(t *testing.T) {
	mirror := newTestExtenderSeedMirror(t)
	serverUrl, err := url.Parse(mirror.server.URL)
	if err != nil {
		t.Fatal(err)
	}

	for _, route := range []*controller.ExtenderSeedMirrorRoute{
		mirror.route(t, serverUrl.Hostname(), controller.ExtenderSeedPath),
		// the DNS-independent route: example.com is never resolved
		mirror.route(t, "example.com", controller.ExtenderSeedPath),
	} {
		result := mirror.check(t, route)
		if result.outcome != extenderSeedMirrorOutcomeOk || !result.observation.up || !result.observation.verified {
			t.Fatalf("route %s: %s", route.Route, result.outcome)
		}
		if result.observation.sequence != mirror.sequence {
			t.Fatalf("route %s: sequence %d, want %d", route.Route, result.observation.sequence, mirror.sequence)
		}
		if !result.observation.expireTime.After(mirror.now) {
			t.Fatalf("route %s: a current bundle reported expired", route.Route)
		}
	}
}

func TestExtenderSeedMirrorCheckOutcomes(t *testing.T) {
	mirror := newTestExtenderSeedMirror(t)
	for _, c := range []struct {
		host     string
		path     string
		outcome  string
		verified bool
	}{
		// an expired bundle reports what it is, and is not up
		{"example.com", "/expired", connect.ExtenderSeedCodeExpired, true},
		{"example.com", "/forged", connect.ExtenderSeedCodeSignature, false},
		{"example.com", "/large", extenderSeedMirrorOutcomeTooLarge, false},
		{"example.com", "/garbage", extenderSeedMirrorOutcomeMalformed, false},
		{"example.com", "/redirect", extenderSeedMirrorOutcomeRedirect, false},
		{"example.com", "/error", extenderSeedMirrorOutcomeHttpStatus, false},
		// the configured address answers with a certificate for another name
		{"wrong.example", controller.ExtenderSeedPath, extenderSeedMirrorOutcomeTls, false},
	} {
		result := mirror.check(t, mirror.route(t, c.host, c.path))
		if result.outcome != c.outcome || result.observation.up || result.observation.verified != c.verified {
			t.Errorf("%s%s: %s up=%t verified=%t, want %s verified=%t", c.host, c.path, result.outcome, result.observation.up, result.observation.verified, c.outcome, c.verified)
		}
	}

	// a closed port
	closed := mirror.route(t, "example.com", controller.ExtenderSeedPath)
	closed.Url.Host = "example.com:1"
	if result := mirror.check(t, closed); result.outcome != extenderSeedMirrorOutcomeUnreachable {
		t.Errorf("closed port: %s", result.outcome)
	}

	// another network's keys verify nothing
	other, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	result := checkExtenderSeedMirrorRoute(context.Background(), mirror.route(t, "example.com", controller.ExtenderSeedPath), connect.NewExtenderRootKeySet(other), []string{testExtenderSeedHost}, mirror.now)
	if result.outcome != connect.ExtenderSeedCodeSignature {
		t.Errorf("foreign keys: %s", result.outcome)
	}
	result = checkExtenderSeedMirrorRoute(context.Background(), mirror.route(t, "example.com", controller.ExtenderSeedPath), mirror.rootKeys, []string{"other.example"}, mirror.now)
	if result.outcome != connect.ExtenderSeedCodeForeignNetwork {
		t.Errorf("foreign host: %s", result.outcome)
	}
}

// Every route's result is counted and observed under its configured name and
// route, and nothing else.
func TestExtenderSeedMirrorCheckRecordsBoundedLabels(t *testing.T) {
	mirror := newTestExtenderSeedMirror(t)
	routes := []*controller.ExtenderSeedMirrorRoute{
		mirror.route(t, "example.com", controller.ExtenderSeedPath),
		mirror.route(t, "example.com", "/forged"),
	}
	routes[1].Mirror = "second"
	results := checkExtenderSeedMirrors(context.Background(), routes, mirror.rootKeys, []string{testExtenderSeedHost}, mirror.now)
	if len(results) != 2 || results[0].outcome != extenderSeedMirrorOutcomeOk || results[1].outcome != connect.ExtenderSeedCodeSignature {
		t.Fatalf("results: %+v", results)
	}
	for _, labels := range [][]string{{"test", "ipv4", "ok"}, {"second", "ipv4", "signature"}} {
		metric := &dto.Metric{}
		if err := extenderSeedMirrorChecksCounter.WithLabelValues(labels...).Write(metric); err != nil || metric.Counter.GetValue() < 1 {
			t.Fatalf("no count for %v", labels)
		}
	}
}

// The collector exports an observation only while it is fresh, and an off
// seed exports only that it is off.
func TestExtenderSeedCollectorExportsOnlyFreshObservations(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	collector := &extenderSeedCollector{
		mirrors:    map[extenderSeedMirrorKey]*extenderSeedMirrorObservation{},
		nowForTest: func() time.Time { return now },
	}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	names := func() map[string]int {
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for _, family := range families {
			counts[family.GetName()] = len(family.Metric)
			for _, metric := range family.Metric {
				for _, label := range metric.Label {
					switch label.GetName() {
					case "mirror", "route", "ip_version":
					default:
						t.Fatalf("%s carries the label %s", family.GetName(), label.GetName())
					}
				}
			}
		}
		return counts
	}

	collector.observeEnabled(true, now)
	collector.observeMirror("origin", controller.ExtenderSeedRouteDns, &extenderSeedMirrorObservation{
		observedAt: now, up: true, verified: true, sequence: 7, issueTime: now, expireTime: now.Add(24 * time.Hour),
	})
	collector.observeMirror("origin", controller.ExtenderSeedRouteIpv4, &extenderSeedMirrorObservation{observedAt: now})
	counts := names()
	if counts["urnetwork_taskworker_extender_seed_enabled"] != 1 ||
		counts["urnetwork_taskworker_extender_seed_mirror_up"] != 2 ||
		counts["urnetwork_taskworker_extender_seed_mirror_sequence"] != 1 {
		t.Fatalf("fresh: %v", counts)
	}

	now = now.Add(extenderSeedObservationTtl)
	if counts := names(); len(counts) != 0 {
		t.Fatalf("stale observations are still exported: %v", counts)
	}

	collector.observeEnabled(false, now)
	if counts := names(); len(counts) != 1 || counts["urnetwork_taskworker_extender_seed_enabled"] != 1 {
		t.Fatalf("off: %v", counts)
	}
}
