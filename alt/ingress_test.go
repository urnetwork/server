package alt

// The ingress headers of the alt fronts (ingress.go). Behind the load balancer
// nginx owns X-UR-Forwarded-For and the session trusts it; alt has no lb in
// front of it, so alt must set it from the connection. These requests are
// what a client of alt can send: a forged X-UR-Forwarded-For and the
// standard forwarding headers, over a QUIC peer the handshake proved.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// What the api router behind the front saw for one request: the client
// address its session resolved and the ingress headers as they reached it.
type testIngressObservation struct {
	clientAddress       string
	sessionErr          error
	urForwardedFor      []string
	forwardedFor        []string
	forwardedSourcePort []string
	requestBuffering    []string
	servedRequestCount  int
}

// The api front exactly as NewAlt mounts it, over a router stand-in that
// records what it saw.
func testApiFront(t testing.TB) (http.Handler, *testIngressObservation) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	observation := &testIngressObservation{}
	front := newApiFrontHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			observation.servedRequestCount += 1
			observation.urForwardedFor = r.Header.Values("X-UR-Forwarded-For")
			observation.forwardedFor = r.Header.Values("X-Forwarded-For")
			observation.forwardedSourcePort = r.Header.Values("X-Forwarded-Source-Port")
			observation.requestBuffering = r.Header.Values(router.RequestBufferingHeader)
			clientSession, err := session.NewClientSessionFromRequest(r)
			observation.sessionErr = err
			if err == nil {
				observation.clientAddress = clientSession.ClientAddress
			}
			w.WriteHeader(http.StatusOK)
		}),
		NewLimits(ctx, testLimitsSettings(t)),
	)
	return front, observation
}

// The review repro (fix/extender-activation-budget, connect#230): a QUIC peer
// at 198.51.100.1:51000 named 203.0.113.7:443 in X-UR-Forwarded-For, and the
// api acted for 203.0.113.7 -- the address the extender activation probe
// dials and every per-address limit is keyed by. The front must resolve the
// peer.
func TestApiFrontResolvesTheQuicPeerNotAClientForwardedFor(t *testing.T) {
	front, observation := testApiFront(t)
	r := httptest.NewRequest(http.MethodPost, "/network/extender-activate", nil)
	r.RemoteAddr = "198.51.100.1:51000"
	r.Header.Set("X-UR-Forwarded-For", "203.0.113.7:443")
	w := httptest.NewRecorder()
	front.ServeHTTP(w, r)

	if observation.servedRequestCount != 1 {
		t.Fatalf("the router served %d requests, want 1 (status %d)", observation.servedRequestCount, w.Code)
	}
	if observation.sessionErr != nil {
		t.Fatal(observation.sessionErr)
	}
	if observation.clientAddress != "198.51.100.1:51000" {
		t.Fatalf("the quic peer 198.51.100.1:51000 resolved as client %q", observation.clientAddress)
	}
}

// The front sets the headers nginx sets for the lb-fronted api, over whatever
// the client sent: X-UR-Forwarded-For is the one peer ip:port, in nginx's
// bracketed form for ipv6 and as ipv4 for an ipv4-mapped peer, and the
// standard X-Forwarded-For and X-Forwarded-Source-Port and the lb's streaming
// mark are gone.
func TestApiFrontSetsTheIngressHeadersFromThePeer(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string][]string
		want       string
	}{
		{
			name:       "forged forwarding headers",
			remoteAddr: "198.51.100.2:51001",
			headers: map[string][]string{
				"X-UR-Forwarded-For":          {"203.0.113.7:443"},
				"X-Forwarded-For":             {"203.0.113.8"},
				"X-Forwarded-Source-Port":     {"41001"},
				router.RequestBufferingHeader: {router.RequestBufferingOff},
			},
			want: "198.51.100.2:51001",
		},
		{
			name:       "a repeated forged header",
			remoteAddr: "198.51.100.3:51002",
			headers: map[string][]string{
				"X-UR-Forwarded-For": {"203.0.113.7:443", "203.0.113.9:443"},
			},
			want: "198.51.100.3:51002",
		},
		{
			name:       "no forwarding header",
			remoteAddr: "198.51.100.4:51003",
			headers:    map[string][]string{},
			want:       "198.51.100.4:51003",
		},
		{
			name:       "an ipv6 peer",
			remoteAddr: "[2001:db8::7]:51004",
			headers: map[string][]string{
				"X-UR-Forwarded-For": {"[2001:db8::99]:443"},
			},
			want: "[2001:db8::7]:51004",
		},
		{
			name:       "an ipv4-mapped peer",
			remoteAddr: "[::ffff:198.51.100.5]:51005",
			headers: map[string][]string{
				"X-UR-Forwarded-For": {"203.0.113.7:443"},
			},
			want: "198.51.100.5:51005",
		},
	}
	for _, c := range cases {
		front, observation := testApiFront(t)
		r := httptest.NewRequest(http.MethodGet, "/hello", nil)
		r.RemoteAddr = c.remoteAddr
		for name, values := range c.headers {
			for _, value := range values {
				r.Header.Add(name, value)
			}
		}
		w := httptest.NewRecorder()
		front.ServeHTTP(w, r)

		if observation.servedRequestCount != 1 {
			t.Fatalf("%s: the router served %d requests (status %d)", c.name, observation.servedRequestCount, w.Code)
		}
		if observation.sessionErr != nil {
			t.Fatalf("%s: %v", c.name, observation.sessionErr)
		}
		if !slices.Equal(observation.urForwardedFor, []string{c.want}) {
			t.Fatalf("%s: X-UR-Forwarded-For = %q, want [%q]", c.name, observation.urForwardedFor, c.want)
		}
		if observation.clientAddress != c.want {
			t.Fatalf("%s: resolved client %q, want %q", c.name, observation.clientAddress, c.want)
		}
		for name, values := range map[string][]string{
			"X-Forwarded-For":             observation.forwardedFor,
			"X-Forwarded-Source-Port":     observation.forwardedSourcePort,
			router.RequestBufferingHeader: observation.requestBuffering,
		} {
			if len(values) != 0 {
				t.Fatalf("%s: %s reached the router as %q", c.name, name, values)
			}
		}
	}
}

// A peer the front cannot read never lets the client's own value through: the
// header is removed, so the session falls back to the same unreadable peer
// and refuses, rather than acting for the address the client named.
func TestApiFrontDropsAClientForwardedForWhenThePeerIsUnreadable(t *testing.T) {
	front, observation := testApiFront(t)
	r := httptest.NewRequest(http.MethodGet, "/hello", nil)
	r.RemoteAddr = "not an address"
	r.Header.Set("X-UR-Forwarded-For", "203.0.113.7:443")
	w := httptest.NewRecorder()
	front.ServeHTTP(w, r)

	if observation.servedRequestCount != 1 {
		t.Fatalf("the router served %d requests (status %d)", observation.servedRequestCount, w.Code)
	}
	if len(observation.urForwardedFor) != 0 {
		t.Fatalf("X-UR-Forwarded-For reached the router as %q", observation.urForwardedFor)
	}
	if observation.sessionErr == nil {
		t.Fatalf("the session resolved %q for an unreadable peer", observation.clientAddress)
	}
}

// The status listener is alt's other http ingress: it echoes the client
// address its session resolved, which is the peer's whatever the client
// claims.
func TestStatusHandlerResolvesThePeer(t *testing.T) {
	t.Setenv("WARP_HOST", "host0")
	t.Setenv("WARP_SERVICE", "alt")
	t.Setenv("WARP_BLOCK", "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	altServer := &Alt{
		listenerStates: map[altListenerKey]bool{
			{transport: ListenerTransportH3, address: "127.0.0.1:443"}: true,
		},
	}
	handler := newStatusHandler(ctx, altServer)

	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.RemoteAddr = "198.51.100.6:51006"
	r.Header.Set("X-UR-Forwarded-For", "203.0.113.7:443")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %q", w.Code, w.Body.String())
	}
	result := &router.WarpStatusResult{}
	if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
		t.Fatal(err)
	}
	if result.ClientAddress != "198.51.100.6:51006" {
		t.Fatalf("status client address = %q, want the peer", result.ClientAddress)
	}
}
