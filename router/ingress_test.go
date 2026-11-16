package router

// The ingress of a front with no lb in front of it (ingress.go): what a
// client of such a front can send, a forged X-UR-Forwarded-For and the
// standard forwarding headers, against what reaches the handler, and the
// count of the headers it overwrote.

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/session"
)

func ingressOverwrittenCounts() map[string]float64 {
	counts := map[string]float64{}
	for _, ingressHeader := range ingressHeaders {
		counts[ingressHeader.label] = testutil.ToFloat64(ingressOverwrittenHeadersCounter.WithLabelValues(ingressHeader.label))
	}
	return counts
}

func ingressCountDelta(before map[string]float64) map[string]float64 {
	delta := map[string]float64{}
	for label, count := range ingressOverwrittenCounts() {
		if count != before[label] {
			delta[label] = count - before[label]
		}
	}
	return delta
}

// What the handler behind the ingress saw.
type ingressObservation struct {
	clientAddress  string
	urForwardedFor []string
	others         []string
}

func serveThroughIngress(t *testing.T, remoteAddr string, headers map[string][]string) ingressObservation {
	t.Helper()
	observation := ingressObservation{}
	served := 0
	handler := NewIngressHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served += 1
		observation.urForwardedFor = r.Header.Values(urForwardedForHeader)
		for _, header := range []string{forwardedForHeader, forwardedSourcePortHeader, RequestBufferingHeader} {
			observation.others = append(observation.others, r.Header.Values(header)...)
		}
		if clientSession, err := session.NewClientSessionFromRequest(r); err == nil {
			observation.clientAddress = clientSession.ClientAddress
		}
	}))
	r := httptest.NewRequest(http.MethodGet, "/hello", nil)
	r.RemoteAddr = remoteAddr
	for name, values := range headers {
		for _, value := range values {
			r.Header.Add(name, value)
		}
	}
	handler.ServeHTTP(httptest.NewRecorder(), r)
	if served != 1 {
		t.Fatalf("the ingress served %d requests", served)
	}
	return observation
}

// Each forwarding header a client sent is replaced or removed, and counted
// once for the request however many values it carried. A header the client
// did not send is not counted, and X-UR-Forwarded-For the ingress set itself
// is not a header the client sent.
func TestIngressCountsEachForwardingHeaderTheClientSent(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string][]string
		want    map[string]float64
	}{
		{
			name:    "no forwarding header",
			headers: map[string][]string{},
			want:    map[string]float64{},
		},
		{
			name:    "a forged X-UR-Forwarded-For",
			headers: map[string][]string{urForwardedForHeader: {"203.0.113.7:443"}},
			want:    map[string]float64{"x_ur_forwarded_for": 1},
		},
		{
			name:    "a repeated X-UR-Forwarded-For",
			headers: map[string][]string{urForwardedForHeader: {"203.0.113.7:443", "203.0.113.9:443"}},
			want:    map[string]float64{"x_ur_forwarded_for": 1},
		},
		{
			name:    "the client's own address",
			headers: map[string][]string{urForwardedForHeader: {"198.51.100.50:55000"}},
			want:    map[string]float64{"x_ur_forwarded_for": 1},
		},
		{
			name: "every forwarding header",
			headers: map[string][]string{
				urForwardedForHeader:      {"203.0.113.7:443"},
				forwardedForHeader:        {"203.0.113.8"},
				forwardedSourcePortHeader: {"41001"},
				RequestBufferingHeader:    {RequestBufferingOff},
			},
			want: map[string]float64{
				"x_ur_forwarded_for":      1,
				"x_forwarded_for":         1,
				"x_forwarded_source_port": 1,
				"x_ur_request_buffering":  1,
			},
		},
		{
			name:    "the standard header alone",
			headers: map[string][]string{forwardedForHeader: {"203.0.113.8, 203.0.113.10"}},
			want:    map[string]float64{"x_forwarded_for": 1},
		},
	}
	for _, c := range cases {
		before := ingressOverwrittenCounts()
		observation := serveThroughIngress(t, "198.51.100.50:55000", c.headers)
		if delta := ingressCountDelta(before); !mapsEqual(delta, c.want) {
			t.Errorf("%s: counted %v, want %v", c.name, delta, c.want)
		}
		if observation.clientAddress != "198.51.100.50:55000" {
			t.Errorf("%s: the handler resolved client %q, want the peer", c.name, observation.clientAddress)
		}
		if !slices.Equal(observation.urForwardedFor, []string{"198.51.100.50:55000"}) {
			t.Errorf("%s: X-UR-Forwarded-For reached the handler as %q", c.name, observation.urForwardedFor)
		}
		if len(observation.others) != 0 {
			t.Errorf("%s: forwarding headers reached the handler: %q", c.name, observation.others)
		}
	}
}

// A peer the ingress cannot read leaves no X-UR-Forwarded-For, so the session
// refuses rather than acting for the address the client named; the header is
// still counted.
func TestIngressDropsAClientForwardedForWhenThePeerIsUnreadable(t *testing.T) {
	before := ingressOverwrittenCounts()
	observation := serveThroughIngress(t, "not an address", map[string][]string{urForwardedForHeader: {"203.0.113.7:443"}})
	if len(observation.urForwardedFor) != 0 {
		t.Fatalf("X-UR-Forwarded-For reached the handler as %q", observation.urForwardedFor)
	}
	if observation.clientAddress != "" {
		t.Fatalf("the session resolved %q for an unreadable peer", observation.clientAddress)
	}
	if delta := ingressCountDelta(before); !mapsEqual(delta, map[string]float64{"x_ur_forwarded_for": 1}) {
		t.Fatalf("counted %v", delta)
	}
}

// The ingress puts the peer in nginx's form: an ipv6 peer bracketed, an
// ipv4-mapped peer as ipv4.
func TestIngressRendersThePeerAsNginxDoes(t *testing.T) {
	for remoteAddr, want := range map[string]string{
		"198.51.100.51:55001":          "198.51.100.51:55001",
		"[2001:db8::7]:55002":          "[2001:db8::7]:55002",
		"[::ffff:198.51.100.52]:55003": "198.51.100.52:55003",
	} {
		observation := serveThroughIngress(t, remoteAddr, map[string][]string{urForwardedForHeader: {"[2001:db8::99]:443"}})
		if !slices.Equal(observation.urForwardedFor, []string{want}) {
			t.Errorf("peer %s: X-UR-Forwarded-For = %q, want [%q]", remoteAddr, observation.urForwardedFor, want)
		}
	}
}

func mapsEqual(a map[string]float64, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
