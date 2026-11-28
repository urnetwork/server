package connect

// The front's readiness on connect's private /status (LBALT-IMPL.md §2.4).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/router"
)

// The private /status of a connect whose trusted listeners are all up, with
// the front's headers.
func (self *testFrontEnv) privateStatus(warpStatus *router.WarpStatusState) *httptest.ResponseRecorder {
	self.t.Setenv("WARP_SERVICE", "connect")
	self.t.Setenv("WARP_BLOCK", "g1")
	self.t.Setenv("WARP_HOST", "edge-a.example")
	connectRouter := &ConnectRouter{
		connectHandler: &ConnectHandler{
			listenerStates: map[connectListenerKey]bool{
				{transport: connectListenerTransportH3, port: 443}:     true,
				{transport: connectListenerTransportH3Dns, port: 4053}: true,
			},
		},
	}
	recorder := httptest.NewRecorder()
	connectRouter.statusWithWarpStatus(recorder, httptest.NewRequest(http.MethodGet, "/status", nil), warpStatus, self.frontServer)
	return recorder
}

// Every group of the route table at up/expected.
func (self *testFrontEnv) socketsHeader(up int) string {
	entries := []string{}
	routeListen := self.routeTable.Listen
	for _, routeInterface := range self.routeTable.Interfaces {
		for _, address := range []string{routeInterface.Ipv4, routeInterface.Ipv6} {
			if address == "" {
				continue
			}
			for _, group := range []string{
				fmt.Sprintf("tcp/%s/%d", address, routeListen.HttpPort),
				fmt.Sprintf("tcp/%s/%d", address, routeListen.HttpsPort),
				fmt.Sprintf("udp/%s/%d", address, routeListen.QuicPort),
				fmt.Sprintf("udp/%s/%d", address, routeListen.WhodisPort),
			} {
				entries = append(entries, fmt.Sprintf("%s=%d/%d", group, up, self.settings.SocketsPerPort))
			}
		}
	}
	slices.Sort(entries)
	return strings.Join(entries, ",")
}

// A ready block's private /status names its generation, its route file's
// revision and its earliest certificate expiry, counts every socket, and is
// ready; the nginx path's headers keep their meaning.
func TestFrontReadinessHeadersReportAReadyFront(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	warpStatus := &router.WarpStatusState{}
	warpStatus.SetReady()
	recorder := env.privateStatus(warpStatus)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d", recorder.Code)
	}
	header := recorder.Header()
	wants := map[string]string{
		frontGenerationHeader:          env.routeTable.Generation,
		frontRouteRevisionHeader:       env.revision,
		frontReadyHeader:               "1",
		frontSocketsHeader:             env.socketsHeader(env.settings.SocketsPerPort),
		frontCertExpiryHeader:          "2027-01-15T12:30:00Z",
		"X-UR-Connect-Listeners-Ready": "1",
		"X-UR-Connect-UDP-Listeners":   "443,4053",
	}
	for name, want := range wants {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	result := &router.WarpStatusResult{}
	if err := json.Unmarshal(recorder.Body.Bytes(), result); err != nil || result.Status != "ok" {
		t.Fatalf("status body %q %v", recorder.Body.String(), err)
	}
}

// Draining is never ready: not when the latch reads draining with every
// socket up, and not when the front's drain began under an ok latch.
func TestFrontReadinessIsNeverReadyWhileDraining(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	warpStatus := &router.WarpStatusState{}
	warpStatus.SetReady()
	warpStatus.SetDrainingIfReady()
	header := env.privateStatus(warpStatus).Header()
	if got := header.Get(frontReadyHeader); got != "0" {
		t.Fatalf("%s = %q under a draining latch", frontReadyHeader, got)
	}
	if got := header.Get(frontSocketsHeader); got != env.socketsHeader(env.settings.SocketsPerPort) {
		t.Fatalf("%s = %q, want every socket up", frontSocketsHeader, got)
	}

	readyStatus := &router.WarpStatusState{}
	readyStatus.SetReady()
	env.frontServer.beginDrain()
	if got := env.privateStatus(readyStatus).Header().Get(frontReadyHeader); got != "0" {
		t.Fatalf("%s = %q with the front draining", frontReadyHeader, got)
	}
}

// With a trusted QUIC listener down the private /status answers 503, and
// the front's headers say not ready too.
func TestFrontReadinessFollowsTheTrustedListeners(t *testing.T) {
	env := newTestFrontEnv(t, nil)
	t.Setenv("WARP_SERVICE", "connect")
	t.Setenv("WARP_BLOCK", "g1")
	warpStatus := &router.WarpStatusState{}
	warpStatus.SetReady()
	connectRouter := &ConnectRouter{
		connectHandler: &ConnectHandler{
			listenerStates: map[connectListenerKey]bool{
				{transport: connectListenerTransportH3, port: 443}: false,
			},
		},
	}
	recorder := httptest.NewRecorder()
	connectRouter.statusWithWarpStatus(recorder, httptest.NewRequest(http.MethodGet, "/status", nil), warpStatus, env.frontServer)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d with a trusted listener down", recorder.Code)
	}
	if got := recorder.Header().Get(frontReadyHeader); got != "0" {
		t.Fatalf("%s = %q with a trusted listener down", frontReadyHeader, got)
	}
}

// A connect that loaded its route file but failed its startup readiness
// still names the front on /status, never ready, with no socket up.
func TestFrontReadinessOfAStartThatNeverServed(t *testing.T) {
	env := newUnstartedTestFrontEnv(t, nil)
	t.Setenv("WARP_SERVICE", "connect")
	t.Setenv("WARP_BLOCK", "g1")
	warpStatus := &router.WarpStatusState{}
	warpStatus.SetNotReady(fmt.Errorf("the test's readiness failure"))
	recorder := httptest.NewRecorder()
	frontStatusHandler(env.frontServer, warpStatus)(recorder, httptest.NewRequest(http.MethodGet, "/status", nil))
	header := recorder.Header()
	if got := header.Get(frontReadyHeader); got != "0" {
		t.Fatalf("%s = %q before the front served", frontReadyHeader, got)
	}
	if got := header.Get(frontGenerationHeader); got != env.routeTable.Generation {
		t.Fatalf("%s = %q", frontGenerationHeader, got)
	}
	if got := header.Get(frontSocketsHeader); got != env.socketsHeader(0) {
		t.Fatalf("%s = %q, want no socket up", frontSocketsHeader, got)
	}

	// even an ok latch can't make a front that never started ready
	okStatus := &router.WarpStatusState{}
	okStatus.SetReady()
	header = http.Header{}
	env.frontServer.writeStatusHeaders(header, okStatus.Status(), true)
	if got := header.Get(frontReadyHeader); got != "0" {
		t.Fatalf("%s = %q for a front that never started", frontReadyHeader, got)
	}
}
