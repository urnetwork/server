package monitor

// The native lb status transport (lbalt_status_transport.go) against loopback
// servers that stand in for an lb interface's pin: the lb route on plain
// HTTP, each block's route over verified TLS, redirects, answers outside the
// routes' contract, connection failures, deadlines and the privacy of the
// prefix and the observer's address.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// One request the synthetic front served.
type lbAltStatusTestServed struct {
	method string
	host   string
	path   string
	// the SNI, empty on plain HTTP
	serverName string
}

// The lb status routes as the lb_alt front answers them (LBALT-IMPL.md §2.6),
// recording each request it serves. answer replaces the healthy answer.
type lbAltStatusTestFront struct {
	stateLock sync.Mutex
	served    []lbAltStatusTestServed
	answer    func(w http.ResponseWriter, r *http.Request)
}

func (self *lbAltStatusTestFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	served := lbAltStatusTestServed{method: r.Method, host: r.Host, path: r.URL.Path}
	if r.TLS != nil {
		served.serverName = r.TLS.ServerName
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.served = append(self.served, served)
	}()
	if self.answer != nil {
		self.answer(w, r)
		return
	}
	lbAltStatusTestHealthyAnswer(w, r)
}

func (self *lbAltStatusTestFront) servedRequests() []lbAltStatusTestServed {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]lbAltStatusTestServed(nil), self.served...)
}

// A healthy front: its own lb status under the prefix, and each block's
// private status through the block's hidden route, which echoes the
// observer's address as a block's status does.
func lbAltStatusTestHealthyAnswer(w http.ResponseWriter, r *http.Request) {
	prefix := "/" + lbAltStatusTestPrefix
	blockPath := strings.TrimPrefix(r.URL.Path, prefix+"/by/b/")
	blockParts := strings.Split(blockPath, "/")
	switch {
	case r.URL.Path == prefix+"/status":
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"version":"0.0.0-synthetic","status":"ok","front":"lbalt"}` + "\n"))
	case blockPath != r.URL.Path && len(blockParts) == 3 && blockParts[2] == "status":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(lbAltStatusFrontReadyHeader, "1")
		json.NewEncoder(w).Encode(map[string]string{
			"status":         "ok",
			"client_address": lbAltStatusTestClientAddress,
			"host":           "edge-synthetic",
			"service":        blockParts[0],
			"block":          blockParts[1],
		})
	default:
		http.NotFound(w, r)
	}
}

// A generated certificate for the names, and roots that trust it.
func lbAltStatusTestCertificate(t *testing.T, names ...string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		DNSNames:              names,
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(encoded)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return tls.Certificate{Certificate: [][]byte{encoded}, PrivateKey: private}, roots
}

// A loopback front for the lb route, or over TLS with a certificate for the lb
// name for the block routes. The roots trust that certificate.
func lbAltStatusTestServer(t *testing.T, front *lbAltStatusTestFront, secure bool) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	if !secure {
		server := httptest.NewServer(front)
		t.Cleanup(server.Close)
		return server, nil
	}
	certificate, roots := lbAltStatusTestCertificate(t, lbAltStatusTestServerName)
	server := httptest.NewUnstartedServer(front)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, roots
}

// Dials a loopback server in place of the pin, after checking the attempt
// asked for exactly its pin in its family. Counts each dial, and records the
// deadline the last one ran under.
type lbAltStatusTestDialer struct {
	t       *testing.T
	request LbAltStatusRequest
	// the loopback address that stands in for the pin; empty fails each dial
	// with dialErr
	target    string
	dialErr   error
	stateLock sync.Mutex
	dials     int
	deadline  time.Time
	bounded   bool
}

func (self *lbAltStatusTestDialer) dial(ctx context.Context, network string, address string) (net.Conn, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.dials += 1
		self.deadline, self.bounded = ctx.Deadline()
	}()
	wantNetwork := "tcp4"
	if self.request.Family == "ipv6" {
		wantNetwork = "tcp6"
	}
	if network != wantNetwork || address != lbAltStatusTuple(self.request) {
		self.t.Errorf("the attempt dialed %s %s, want %s %s", network, address, wantNetwork, lbAltStatusTuple(self.request))
		return nil, errors.New("synthetic dial outside the pin")
	}
	if self.target == "" {
		return nil, self.dialErr
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", self.target)
}

func (self *lbAltStatusTestDialer) dialCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.dials
}

// The deadline the last dial's context carried, and whether it had one.
func (self *lbAltStatusTestDialer) dialDeadline() (time.Time, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.deadline, self.bounded
}

// The synthetic matrix's request for one pin and route, with an attempt id.
func lbAltStatusTestRequest(t *testing.T, family string, route string, block string) LbAltStatusRequest {
	t.Helper()
	cfg := configFromSignalSettings(lbAltStatusTestSignalSettings(&syntheticSource{}))
	for _, request := range lbAltStatusTestRequests(t, cfg) {
		if request.Target == "edge-synthetic-eth1" && request.Family == family && request.Route == route && request.Block == block {
			request.AttemptId = "synthetic-attempt-" + family + "-" + route + "-" + block
			return request
		}
	}
	t.Fatalf("the synthetic matrix has no %s %s %s request", family, route, block)
	return LbAltStatusRequest{}
}

// One native attempt at a loopback server standing in for the request's pin.
func lbAltStatusTestObserve(
	t *testing.T,
	ctx context.Context,
	request LbAltStatusRequest,
	server *httptest.Server,
	roots *x509.CertPool,
	settings *LbAltStatusProbeSettings,
) (LbAltStatusObservation, error, *lbAltStatusTestDialer) {
	t.Helper()
	dialer := &lbAltStatusTestDialer{t: t, request: request, target: server.Listener.Addr().String()}
	observation, err := observeLbAltStatus(ctx, request, settings, dialer.dial, roots)
	return observation, err, dialer
}

// The lb route answers Route53's request: one fresh connection to the pin in
// its family, a GET of the prefix's status on plain HTTP with the lb name as
// Host, and a 200 with the front's status JSON, healthy in both families.
func TestLbAltStatusLbRouteAnswersOkOnPlainHttp(t *testing.T) {
	for _, family := range []string{"ipv4", "ipv6"} {
		front := &lbAltStatusTestFront{}
		server, _ := lbAltStatusTestServer(t, front, false)
		request := lbAltStatusTestRequest(t, family, lbAltStatusRouteLb, "")
		observed, err, dialer := lbAltStatusTestObserve(t, context.Background(), request, server, nil, DefaultLbAltStatusProbeSettings())
		verdict := lbAltStatusEvaluate(request, observed, err, DefaultLbAltStatusProbeSettings())
		if err != nil || !verdict.healthy || dialer.dialCount() != 1 || !observed.FreshConnection || observed.TlsVerified || !observed.LbAltFront {
			t.Fatalf("%s: the lb route was not healthy: %+v %+v err=%v", family, observed, verdict, err)
		}
		want := []lbAltStatusTestServed{{method: http.MethodGet, host: lbAltStatusTestServerName, path: "/" + lbAltStatusTestPrefix + "/status"}}
		if served := front.servedRequests(); !reflect.DeepEqual(served, want) {
			t.Fatalf("%s: the front served %+v, want %+v", family, served, want)
		}
	}
}

// Route53 counts a redirect healthy. The probe reads the redirect itself,
// never where it leads, and fails it, even when the target would answer ok.
func TestLbAltStatusRedirectIsNotFollowedAndFails(t *testing.T) {
	followed := "/" + lbAltStatusTestPrefix + "/followed"
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		front := &lbAltStatusTestFront{answer: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == followed {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"status":"ok"}`))
				return
			}
			http.Redirect(w, r, followed, code)
		}}
		server, _ := lbAltStatusTestServer(t, front, false)
		request := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteLb, "")
		observed, err, dialer := lbAltStatusTestObserve(t, context.Background(), request, server, nil, DefaultLbAltStatusProbeSettings())
		verdict := lbAltStatusEvaluate(request, observed, err, DefaultLbAltStatusProbeSettings())
		if err != nil || observed.StatusCode != code || verdict != (lbAltStatusVerdict{path: true, reason: "redirect"}) || dialer.dialCount() != 1 {
			t.Fatalf("a %d gave %+v %+v err=%v", code, observed, verdict, err)
		}
		if served := front.servedRequests(); len(served) != 1 || served[0].path == followed {
			t.Fatalf("a %d was followed: %+v", code, served)
		}
	}
}

// Only a 200 whose Content-Type values are all JSON, with one JSON object
// whose status is "ok", is healthy. Every other answer is a path failure with
// its fixed reason.
func TestLbAltStatusLbRouteRequiresOkStatusJson(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		contentTypes []string
		body         string
		reason       string
		state        LbAltStatusState
	}{
		{name: "nginx's answer", status: 200, contentTypes: []string{"application/json", "application/json"}, body: `{"version":"0.0.0-synthetic","status":"ok"}`, reason: "healthy", state: LbAltStatusStateOk},
		{name: "a charset", status: 200, contentTypes: []string{"application/json; charset=utf-8"}, body: `{"status":"ok"}`, reason: "healthy", state: LbAltStatusStateOk},
		{name: "a text answer", status: 200, contentTypes: []string{"text/plain"}, body: `{"status":"ok"}`, reason: "content-type", state: LbAltStatusStateOk},
		{name: "a json and a text type", status: 200, contentTypes: []string{"application/json", "text/html"}, body: `{"status":"ok"}`, reason: "content-type", state: LbAltStatusStateOk},
		{name: "no content type", status: 200, contentTypes: nil, body: `{"status":"ok"}`, reason: "content-type", state: LbAltStatusStateOk},
		{name: "a draining answer", status: 200, contentTypes: []string{"application/json"}, body: `{"status":"draining"}`, reason: "status-not-ok", state: LbAltStatusStateDraining},
		{name: "an error answer", status: 200, contentTypes: []string{"application/json"}, body: `{"status":"error not ready: synthetic"}`, reason: "status-not-ok", state: LbAltStatusStateError},
		{name: "another status", status: 200, contentTypes: []string{"application/json"}, body: `{"status":"OK"}`, reason: "status-not-ok", state: LbAltStatusStateOther},
		{name: "no status", status: 200, contentTypes: []string{"application/json"}, body: `{"version":"0.0.0-synthetic"}`, reason: "status-not-ok", state: LbAltStatusStateAbsent},
		{name: "a status that is no string", status: 200, contentTypes: []string{"application/json"}, body: `{"status":1}`, reason: "body-invalid"},
		{name: "a json array", status: 200, contentTypes: []string{"application/json"}, body: `["ok"]`, reason: "body-invalid"},
		{name: "a json null", status: 200, contentTypes: []string{"application/json"}, body: `null`, reason: "body-invalid"},
		{name: "trailing data", status: 200, contentTypes: []string{"application/json"}, body: `{"status":"ok"} {}`, reason: "body-invalid"},
		{name: "an unavailable route", status: 503, contentTypes: []string{"application/json"}, body: `{"status":"ok"}`, reason: "http-status", state: LbAltStatusStateOk},
		{name: "a missing route", status: 404, contentTypes: []string{"text/plain"}, body: "404 page not found", reason: "http-status"},
	}
	for _, c := range cases {
		front := &lbAltStatusTestFront{answer: func(w http.ResponseWriter, r *http.Request) {
			for _, contentType := range c.contentTypes {
				w.Header().Add("Content-Type", contentType)
			}
			if c.contentTypes == nil {
				// net/http sniffs a type for a body written with none
				w.Header()["Content-Type"] = nil
			}
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		}}
		server, _ := lbAltStatusTestServer(t, front, false)
		request := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteLb, "")
		observed, err, _ := lbAltStatusTestObserve(t, context.Background(), request, server, nil, DefaultLbAltStatusProbeSettings())
		verdict := lbAltStatusEvaluate(request, observed, err, DefaultLbAltStatusProbeSettings())
		want := lbAltStatusVerdict{healthy: c.reason == "healthy", path: c.reason != "healthy", reason: c.reason}
		if err != nil || verdict != want || observed.StatusCode != c.status || observed.Status != c.state {
			t.Errorf("%s: %+v gave %+v, want %+v (err=%v)", c.name, observed, verdict, want, err)
		}
	}
}

// The body is read through its bound: past it the answer is a fixed failure,
// and the count stays bounded.
func TestLbAltStatusBodyPastItsBoundFails(t *testing.T) {
	front := &lbAltStatusTestFront{answer: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","version":"` + strings.Repeat("9", 256) + `"}`))
	}}
	server, _ := lbAltStatusTestServer(t, front, false)
	settings := DefaultLbAltStatusProbeSettings()
	settings.ResponseBodyByteLimit = 64
	request := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteLb, "")
	observed, err, _ := lbAltStatusTestObserve(t, context.Background(), request, server, nil, settings)
	verdict := lbAltStatusEvaluate(request, observed, err, settings)
	if err != nil || observed.Failure != LbAltStatusFailureBudget || observed.ResponseBytes != 64 || observed.BodyJson ||
		verdict != (lbAltStatusVerdict{path: true, reason: "response-budget-exhausted"}) {
		t.Fatalf("a body past its bound gave %+v %+v err=%v", observed, verdict, err)
	}
}

// The response header is read through its bound too: past it the exchange is
// a path failure with fixed text, while the same answer under the default
// bound is healthy, so only the bound refused it. No header text reaches a
// diagnostic.
func TestLbAltStatusResponseHeaderPastItsBoundFails(t *testing.T) {
	const marker = "synthetic-header-marker"
	front := &lbAltStatusTestFront{answer: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Synthetic-Padding", marker+strings.Repeat("p", 4*1024))
		lbAltStatusTestHealthyAnswer(w, r)
	}}
	server, _ := lbAltStatusTestServer(t, front, false)
	request := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteLb, "")
	bounded := DefaultLbAltStatusProbeSettings()
	bounded.ResponseHeaderByteLimit = 1024
	observed, err, dialer := lbAltStatusTestObserve(t, context.Background(), request, server, nil, bounded)
	verdict := lbAltStatusEvaluate(request, observed, err, bounded)
	if err == nil || observed.Failure != LbAltStatusFailureRequest || observed.StatusCode != 0 || !observed.FreshConnection ||
		dialer.dialCount() != 1 || verdict != (lbAltStatusVerdict{path: true, reason: "request-failed"}) {
		t.Fatalf("a header past its bound gave %+v %+v err=%v", observed, verdict, err)
	}
	for _, text := range []string{fmt.Sprintf("%+v", observed), fmt.Sprintf("%+v", verdict), err.Error()} {
		if strings.Contains(text, marker) || strings.Contains(text, lbAltStatusTestPrefix) {
			t.Fatalf("a diagnostic carried the header or the prefix: %s", text)
		}
	}
	observed, err, _ = lbAltStatusTestObserve(t, context.Background(), request, server, nil, DefaultLbAltStatusProbeSettings())
	if verdict := lbAltStatusEvaluate(request, observed, err, DefaultLbAltStatusProbeSettings()); err != nil || !verdict.healthy {
		t.Fatalf("the same answer under the default bound gave %+v %+v err=%v", observed, verdict, err)
	}
}

// Every block is probed through its own hidden route on 443, over TLS
// verified for the lb name, which is also the SNI and the Host, and each
// block's own answer is healthy.
func TestLbAltStatusBlockRoutesProbeEveryBlockOverVerifiedTls(t *testing.T) {
	for _, family := range []string{"ipv4", "ipv6"} {
		front := &lbAltStatusTestFront{}
		server, roots := lbAltStatusTestServer(t, front, true)
		want := []lbAltStatusTestServed{}
		for _, block := range lbAltStatusTestBlocks {
			request := lbAltStatusTestRequest(t, family, lbAltStatusRouteBlock, block)
			observed, err, dialer := lbAltStatusTestObserve(t, context.Background(), request, server, roots, DefaultLbAltStatusProbeSettings())
			verdict := lbAltStatusEvaluate(request, observed, err, DefaultLbAltStatusProbeSettings())
			if err != nil || !verdict.healthy || !observed.TlsVerified || !observed.BlockMatched || dialer.dialCount() != 1 ||
				observed.FrontReadiness != LbAltStatusFrontReadinessReady {
				t.Fatalf("%s %s: the block route was not healthy: %+v %+v err=%v", family, block, observed, verdict, err)
			}
			want = append(want, lbAltStatusTestServed{
				method:     http.MethodGet,
				host:       lbAltStatusTestServerName,
				path:       "/" + lbAltStatusTestPrefix + "/by/b/connect/" + block + "/status",
				serverName: lbAltStatusTestServerName,
			})
		}
		if served := front.servedRequests(); !reflect.DeepEqual(served, want) {
			t.Fatalf("%s: the front served %+v, want %+v", family, served, want)
		}
	}
}

// A block route is healthy only when the answer names the requested service
// and block, its status is "ok", and the front, when it says, is ready.
func TestLbAltStatusBlockRouteRequiresTheNamedBlockAndAReadyFront(t *testing.T) {
	cases := []struct {
		name       string
		frontReady []string
		body       map[string]string
		reason     string
	}{
		{name: "a ready front", frontReady: []string{"1"}, body: map[string]string{"status": "ok", "service": "connect", "block": "g1"}, reason: "healthy"},
		{name: "no front header", frontReady: nil, body: map[string]string{"status": "ok", "service": "connect", "block": "g1"}, reason: "healthy"},
		{name: "another block", frontReady: []string{"1"}, body: map[string]string{"status": "ok", "service": "connect", "block": "g2"}, reason: "block-mismatch"},
		{name: "another service", frontReady: []string{"1"}, body: map[string]string{"status": "ok", "service": "api", "block": "g1"}, reason: "block-mismatch"},
		{name: "no block", frontReady: []string{"1"}, body: map[string]string{"status": "ok", "service": "connect"}, reason: "block-mismatch"},
		{name: "a front not ready", frontReady: []string{"0"}, body: map[string]string{"status": "ok", "service": "connect", "block": "g1"}, reason: "front-not-ready"},
		{name: "a front header of no value", frontReady: []string{"yes"}, body: map[string]string{"status": "ok", "service": "connect", "block": "g1"}, reason: "front-readiness-invalid"},
		{name: "a repeated front header", frontReady: []string{"1", "1"}, body: map[string]string{"status": "ok", "service": "connect", "block": "g1"}, reason: "front-readiness-invalid"},
		{name: "a draining block", frontReady: []string{"0"}, body: map[string]string{"status": "draining", "service": "connect", "block": "g1"}, reason: "status-not-ok"},
	}
	for _, c := range cases {
		front := &lbAltStatusTestFront{answer: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			for _, value := range c.frontReady {
				w.Header().Add(lbAltStatusFrontReadyHeader, value)
			}
			json.NewEncoder(w).Encode(c.body)
		}}
		server, roots := lbAltStatusTestServer(t, front, true)
		request := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteBlock, "g1")
		observed, err, _ := lbAltStatusTestObserve(t, context.Background(), request, server, roots, DefaultLbAltStatusProbeSettings())
		verdict := lbAltStatusEvaluate(request, observed, err, DefaultLbAltStatusProbeSettings())
		want := lbAltStatusVerdict{healthy: c.reason == "healthy", path: c.reason != "healthy", reason: c.reason}
		if err != nil || verdict != want {
			t.Errorf("%s: %+v gave %+v, want %+v (err=%v)", c.name, observed, verdict, want, err)
		}
	}
}

// A certificate the roots do not trust, or one for another name, leaves the
// block route unknown: an unauthenticated answer is no evidence either way.
func TestLbAltStatusUnverifiedCertificateIsUnknown(t *testing.T) {
	for _, untrusted := range []bool{true, false} {
		front := &lbAltStatusTestFront{}
		server := httptest.NewUnstartedServer(front)
		certificate, roots := lbAltStatusTestCertificate(t, lbAltStatusTestServerName)
		if untrusted {
			roots = x509.NewCertPool()
		} else {
			certificate, roots = lbAltStatusTestCertificate(t, "synthetic-other.edge.example")
		}
		server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
		server.StartTLS()
		request := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteBlock, "g1")
		observed, err, _ := lbAltStatusTestObserve(t, context.Background(), request, server, roots, DefaultLbAltStatusProbeSettings())
		server.Close()
		verdict := lbAltStatusEvaluate(request, observed, err, DefaultLbAltStatusProbeSettings())
		if err == nil || observed.Failure != LbAltStatusFailureTls || observed.TlsVerified || len(front.servedRequests()) != 0 ||
			verdict != (lbAltStatusVerdict{reason: "tls-authentication-unverified"}) {
			t.Fatalf("untrusted=%t: %+v gave %+v err=%v", untrusted, observed, verdict, err)
		}
	}
}

// A refused connection is a path failure, and so is a host a router reports
// unreachable, as for a down edge interface; a missing route, address or
// family on the monitor host is unknown; an exchange the front breaks after
// accepting is a path failure.
func TestLbAltStatusConnectionFailuresAreClassified(t *testing.T) {
	cases := []struct {
		name    string
		dialErr error
		verdict lbAltStatusVerdict
	}{
		{name: "a refused connection", dialErr: &net.OpError{Op: "dial", Net: "tcp4", Err: syscall.ECONNREFUSED}, verdict: lbAltStatusVerdict{path: true, reason: "connect-failed"}},
		{
			name:    "an unreachable host",
			dialErr: &net.OpError{Op: "dial", Net: "tcp6", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)},
			verdict: lbAltStatusVerdict{path: true, reason: "connect-failed"},
		},
		{name: "no route", dialErr: &net.OpError{Op: "dial", Net: "tcp6", Err: syscall.ENETUNREACH}, verdict: lbAltStatusVerdict{reason: "observer-route-unavailable"}},
		{name: "no address", dialErr: &net.OpError{Op: "dial", Net: "tcp6", Err: syscall.EADDRNOTAVAIL}, verdict: lbAltStatusVerdict{reason: "observer-route-unavailable"}},
		{
			name:    "no family",
			dialErr: &net.OpError{Op: "dial", Net: "tcp6", Err: os.NewSyscallError("socket", syscall.EAFNOSUPPORT)},
			verdict: lbAltStatusVerdict{reason: "observer-route-unavailable"},
		},
	}
	for _, c := range cases {
		request := lbAltStatusTestRequest(t, "ipv6", lbAltStatusRouteLb, "")
		dialer := &lbAltStatusTestDialer{t: t, request: request, dialErr: c.dialErr}
		observed, err := observeLbAltStatus(context.Background(), request, DefaultLbAltStatusProbeSettings(), dialer.dial, nil)
		verdict := lbAltStatusEvaluate(request, observed, err, DefaultLbAltStatusProbeSettings())
		if err == nil || verdict != c.verdict || !observed.Dialed || observed.FreshConnection || dialer.dialCount() != 1 {
			t.Errorf("%s: %+v gave %+v err=%v", c.name, observed, verdict, err)
		}
	}
	front := &lbAltStatusTestFront{answer: func(w http.ResponseWriter, r *http.Request) {
		connection, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			connection.Close()
		}
	}}
	server, _ := lbAltStatusTestServer(t, front, false)
	request := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteLb, "")
	observed, err, _ := lbAltStatusTestObserve(t, context.Background(), request, server, nil, DefaultLbAltStatusProbeSettings())
	verdict := lbAltStatusEvaluate(request, observed, err, DefaultLbAltStatusProbeSettings())
	if err == nil || !observed.FreshConnection || verdict != (lbAltStatusVerdict{path: true, reason: "request-failed"}) {
		t.Fatalf("a broken exchange gave %+v %+v err=%v", observed, verdict, err)
	}
}

// The attempt's deadline ends an exchange the front holds open: a path
// failure, returned at once with the deadline's own error. The front's entry
// is the barrier, and the deadline is an explicit state transition.
func TestLbAltStatusDeadlineDuringTheExchangeIsATimeout(t *testing.T) {
	attemptCtx := &publicUdpTestDeadlineContext{Context: context.Background(), expired: make(chan struct{})}
	entered := make(chan struct{})
	var enterOnce sync.Once
	front := &lbAltStatusTestFront{answer: func(w http.ResponseWriter, r *http.Request) {
		enterOnce.Do(func() { close(entered) })
		<-r.Context().Done()
	}}
	server, _ := lbAltStatusTestServer(t, front, false)
	request := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteLb, "")
	type attempt struct {
		observed LbAltStatusObservation
		err      error
	}
	done := make(chan attempt, 1)
	go func() {
		observed, err, _ := lbAltStatusTestObserve(t, attemptCtx, request, server, nil, DefaultLbAltStatusProbeSettings())
		done <- attempt{observed: observed, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the front")
	}
	close(attemptCtx.expired)
	select {
	case result := <-done:
		verdict := lbAltStatusEvaluate(request, result.observed, result.err, DefaultLbAltStatusProbeSettings())
		if !errors.Is(result.err, context.DeadlineExceeded) || result.observed.Failure != LbAltStatusFailureTimeout ||
			verdict != (lbAltStatusVerdict{path: true, reason: "timeout"}) {
			t.Fatalf("an expired exchange gave %+v %+v err=%v", result.observed, verdict, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the expired attempt did not return")
	}
}

// The attempt's deadline is the settings' attempt timeout, whatever its
// caller's context allows: a raised timeout and the default reach the attempt
// although the caller set no deadline, a lowered timeout and the default hold
// under a longer caller deadline, and a shorter caller deadline, as the
// runner's command timeout is, still holds. The dial runs under the request's
// own context, so the deadline it records is the one the request carries, on
// the lb route and on a block route.
func TestLbAltStatusTransportBoundsTheAttemptByItsSettings(t *testing.T) {
	raised := DefaultLbAltStatusProbeSettings()
	raised.AttemptTimeout = time.Hour
	lowered := DefaultLbAltStatusProbeSettings()
	lowered.AttemptTimeout = time.Second
	cases := []struct {
		name           string
		settings       *LbAltStatusProbeSettings
		callerDeadline time.Duration
		// how long after its start the attempt's deadline falls
		bound time.Duration
	}{
		{name: "a raised timeout", settings: raised, callerDeadline: 0, bound: time.Hour},
		{name: "a lowered timeout", settings: lowered, callerDeadline: 2 * time.Hour, bound: time.Second},
		{name: "the default", settings: DefaultLbAltStatusProbeSettings(), callerDeadline: 0, bound: 5 * time.Second},
		{name: "the default under a longer caller deadline", settings: DefaultLbAltStatusProbeSettings(), callerDeadline: 2 * time.Hour, bound: 5 * time.Second},
		{name: "a shorter caller deadline", settings: raised, callerDeadline: time.Minute, bound: time.Minute},
	}
	for _, c := range cases {
		for _, request := range []LbAltStatusRequest{
			lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteLb, ""),
			lbAltStatusTestRequest(t, "ipv6", lbAltStatusRouteBlock, "g1"),
		} {
			started := time.Now()
			ctx, cancel := context.Background(), context.CancelFunc(func() {})
			if 0 < c.callerDeadline {
				ctx, cancel = context.WithTimeout(ctx, c.callerDeadline)
			}
			dialer := &lbAltStatusTestDialer{t: t, request: request, dialErr: errors.New("synthetic-private-dial-error")}
			observed, err := observeLbAltStatus(ctx, request, c.settings, dialer.dial, nil)
			finished := time.Now()
			cancel()
			if err == nil || dialer.dialCount() != 1 || observed.Failure != LbAltStatusFailureConnect || !observed.Dialed {
				t.Fatalf("%s, %s route: the attempt did not reach its dial: %+v err=%v", c.name, request.Route, observed, err)
			}
			deadline, bounded := dialer.dialDeadline()
			if !bounded || deadline.Before(started.Add(c.bound)) || deadline.After(finished.Add(c.bound)) {
				t.Errorf("%s, %s route: the dial ran under a deadline %s after the start (bounded=%t), want %s", c.name, request.Route, deadline.Sub(started), bounded, c.bound)
			}
		}
	}
}

// No diagnostic carries the hidden prefix or an observer address the front
// echoes: not the error, the observation or the verdict, whatever the
// attempt meets. net/http's own errors quote the request URL, prefix and all.
func TestLbAltStatusDiagnosticsKeepThePrefixAndClientAddressPrivate(t *testing.T) {
	echo := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"status":"error ` + r.URL.Path + `","client_address":"` + lbAltStatusTestClientAddress + `"}`))
	}
	attempts := []func() (LbAltStatusRequest, LbAltStatusObservation, error){
		func() (LbAltStatusRequest, LbAltStatusObservation, error) {
			request := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteLb, "")
			dialer := &lbAltStatusTestDialer{t: t, request: request, dialErr: &net.OpError{Op: "dial", Net: "tcp4", Err: syscall.ECONNREFUSED}}
			observed, err := observeLbAltStatus(context.Background(), request, DefaultLbAltStatusProbeSettings(), dialer.dial, nil)
			return request, observed, err
		},
		func() (LbAltStatusRequest, LbAltStatusObservation, error) {
			server, _ := lbAltStatusTestServer(t, &lbAltStatusTestFront{answer: echo}, true)
			request := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteBlock, "g1")
			observed, err, _ := lbAltStatusTestObserve(t, context.Background(), request, server, x509.NewCertPool(), DefaultLbAltStatusProbeSettings())
			return request, observed, err
		},
		func() (LbAltStatusRequest, LbAltStatusObservation, error) {
			server, roots := lbAltStatusTestServer(t, &lbAltStatusTestFront{answer: echo}, true)
			request := lbAltStatusTestRequest(t, "ipv6", lbAltStatusRouteBlock, "g2")
			observed, err, _ := lbAltStatusTestObserve(t, context.Background(), request, server, roots, DefaultLbAltStatusProbeSettings())
			return request, observed, err
		},
		func() (LbAltStatusRequest, LbAltStatusObservation, error) {
			server, _ := lbAltStatusTestServer(t, &lbAltStatusTestFront{answer: func(w http.ResponseWriter, r *http.Request) {
				connection, _, err := http.NewResponseController(w).Hijack()
				if err == nil {
					connection.Close()
				}
			}}, false)
			request := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteLb, "")
			observed, err, _ := lbAltStatusTestObserve(t, context.Background(), request, server, nil, DefaultLbAltStatusProbeSettings())
			return request, observed, err
		},
		func() (LbAltStatusRequest, LbAltStatusObservation, error) {
			server, _ := lbAltStatusTestServer(t, &lbAltStatusTestFront{answer: echo}, false)
			request := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteLb, "")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			observed, err, _ := lbAltStatusTestObserve(t, ctx, request, server, nil, DefaultLbAltStatusProbeSettings())
			return request, observed, err
		},
	}
	for index, attempt := range attempts {
		request, observed, err := attempt()
		verdict := lbAltStatusEvaluate(request, observed, err, DefaultLbAltStatusProbeSettings())
		texts := []string{fmt.Sprintf("%+v", observed), fmt.Sprintf("%+v", verdict)}
		if err != nil {
			texts = append(texts, err.Error())
		}
		for _, text := range texts {
			if strings.Contains(text, lbAltStatusTestPrefix) || strings.Contains(text, "203.0.113.77") {
				t.Errorf("attempt %d: a diagnostic carried the prefix or the observer address: %s", index, text)
			}
		}
	}
}

// A request outside the transport's authority never dials: a route on the
// other port, a path that is not its route's, a family its address lacks, a
// missing attempt id, an unbounded body or an attempt with no positive bound.
func TestLbAltStatusTransportRefusesARequestOutsideItsAuthority(t *testing.T) {
	lbRequest := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteLb, "")
	blockRequest := lbAltStatusTestRequest(t, "ipv6", lbAltStatusRouteBlock, "g1")
	bounded := DefaultLbAltStatusProbeSettings()
	unbounded := DefaultLbAltStatusProbeSettings()
	unbounded.ResponseBodyByteLimit = 0
	untimed := DefaultLbAltStatusProbeSettings()
	untimed.AttemptTimeout = 0
	negativelyTimed := DefaultLbAltStatusProbeSettings()
	negativelyTimed.AttemptTimeout = -time.Second
	cases := []struct {
		name     string
		request  LbAltStatusRequest
		mutate   func(*LbAltStatusRequest)
		settings *LbAltStatusProbeSettings
	}{
		{name: "the lb route on 443", request: lbRequest, mutate: func(request *LbAltStatusRequest) { request.Port = 443 }, settings: bounded},
		{name: "a block route on 80", request: blockRequest, mutate: func(request *LbAltStatusRequest) { request.Port = 80 }, settings: bounded},
		{name: "an unknown route", request: lbRequest, mutate: func(request *LbAltStatusRequest) { request.Route = "web" }, settings: bounded},
		{name: "the lb route naming a block", request: lbRequest, mutate: func(request *LbAltStatusRequest) { request.Block = "g1" }, settings: bounded},
		{name: "a block route with the lb path", request: blockRequest, mutate: func(request *LbAltStatusRequest) { request.Path = lbRequest.Path }, settings: bounded},
		{name: "another block's path", request: blockRequest, mutate: func(request *LbAltStatusRequest) { request.Block = "g2" }, settings: bounded},
		{name: "a status with no prefix", request: lbRequest, mutate: func(request *LbAltStatusRequest) { request.Path = "/status" }, settings: bounded},
		{
			name: "a prefix with a query", request: lbRequest,
			mutate:   func(request *LbAltStatusRequest) { request.Path = "/" + lbAltStatusTestPrefix + "?x/status" },
			settings: bounded,
		},
		{name: "an address of the other family", request: lbRequest, mutate: func(request *LbAltStatusRequest) { request.Address = "2001:db8:10::10" }, settings: bounded},
		{name: "an uppercase lb name", request: lbRequest, mutate: func(request *LbAltStatusRequest) { request.ServerName = "Synthetic-lb.edge.example" }, settings: bounded},
		{name: "no attempt id", request: lbRequest, mutate: func(request *LbAltStatusRequest) { request.AttemptId = "" }, settings: bounded},
		{name: "an unbounded body", request: lbRequest, mutate: func(*LbAltStatusRequest) {}, settings: unbounded},
		{name: "no attempt timeout", request: blockRequest, mutate: func(*LbAltStatusRequest) {}, settings: untimed},
		{name: "a negative attempt timeout", request: lbRequest, mutate: func(*LbAltStatusRequest) {}, settings: negativelyTimed},
		{name: "no settings", request: lbRequest, mutate: func(*LbAltStatusRequest) {}, settings: nil},
	}
	for _, c := range cases {
		request := c.request
		c.mutate(&request)
		dials := 0
		observed, err := observeLbAltStatus(context.Background(), request, c.settings, func(context.Context, string, string) (net.Conn, error) {
			dials++
			return nil, errors.New("synthetic forbidden dial")
		}, nil)
		if err == nil || dials != 0 || observed.Failure != LbAltStatusFailureConfiguration || observed.Dialed {
			t.Errorf("%s: dials=%d failure=%s err=%v", c.name, dials, observed.Failure, err)
		}
	}
}

// The native adapter refuses settings with no positive attempt timeout, and
// missing settings, as configuration before any dial. Such a timeout expires
// the attempt's context at its start, which is no timeout of the path.
func TestLbAltStatusNativeAdapterRefusesAnAttemptWithNoPositiveBound(t *testing.T) {
	untimed := DefaultLbAltStatusProbeSettings()
	untimed.AttemptTimeout = 0
	negativelyTimed := DefaultLbAltStatusProbeSettings()
	negativelyTimed.AttemptTimeout = -time.Second
	native := &runner{cfg: &monitorConfig{commandTimeout: time.Minute}}
	for index, settings := range []*LbAltStatusProbeSettings{untimed, negativelyTimed, nil} {
		request := lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteLb, "")
		observed, err := native.lbAltStatus(context.Background(), request, settings)
		verdict := lbAltStatusEvaluate(request, observed, err, DefaultLbAltStatusProbeSettings())
		if err == nil || observed.Failure != LbAltStatusFailureConfiguration || observed.Dialed ||
			verdict != (lbAltStatusVerdict{reason: "configuration-invalid"}) {
			t.Errorf("settings %d: %+v gave %+v err=%v", index, observed, verdict, err)
		}
	}
}

// A native attempt runs within the runner's command timeout and under the
// settings' own bound, so the shorter applies. A raised attempt timeout
// reaches the attempt up to the command timeout, and in full when no positive
// command timeout caps it; a lowered one and the default hold under the
// monitor's default command timeout. The dial runs under the request's
// context, so the deadline it records is the attempt's, on the lb route and
// on a block route.
func TestLbAltStatusNativeAttemptIsBoundedWithinTheCommandTimeout(t *testing.T) {
	defaultCommandTimeout := (SignalSettings{}).withDefaults().CommandTimeout
	raised := DefaultLbAltStatusProbeSettings()
	raised.AttemptTimeout = time.Hour
	lowered := DefaultLbAltStatusProbeSettings()
	lowered.AttemptTimeout = time.Second
	cases := []struct {
		name           string
		settings       *LbAltStatusProbeSettings
		commandTimeout time.Duration
		// how long after its start the attempt's deadline falls
		bound time.Duration
	}{
		{name: "a raised timeout under a shorter command timeout", settings: raised, commandTimeout: time.Minute, bound: time.Minute},
		{name: "a raised timeout within a longer command timeout", settings: raised, commandTimeout: 2 * time.Hour, bound: time.Hour},
		{name: "a raised timeout and no command timeout", settings: raised, commandTimeout: 0, bound: time.Hour},
		{name: "a raised timeout and a negative command timeout", settings: raised, commandTimeout: -time.Second, bound: time.Hour},
		{name: "a lowered timeout", settings: lowered, commandTimeout: defaultCommandTimeout, bound: time.Second},
		{name: "the default under the default command timeout", settings: DefaultLbAltStatusProbeSettings(), commandTimeout: defaultCommandTimeout, bound: 5 * time.Second},
		{name: "the default and no command timeout", settings: DefaultLbAltStatusProbeSettings(), commandTimeout: 0, bound: 5 * time.Second},
	}
	for _, c := range cases {
		for _, request := range []LbAltStatusRequest{
			lbAltStatusTestRequest(t, "ipv4", lbAltStatusRouteLb, ""),
			lbAltStatusTestRequest(t, "ipv6", lbAltStatusRouteBlock, "g1"),
		} {
			dialer := &lbAltStatusTestDialer{t: t, request: request, dialErr: errors.New("synthetic-private-dial-error")}
			started := time.Now()
			observed, err := observeLbAltStatusWithin(context.Background(), c.commandTimeout, request, c.settings, dialer.dial, nil)
			finished := time.Now()
			if err == nil || dialer.dialCount() != 1 || observed.Failure != LbAltStatusFailureConnect || !observed.Dialed {
				t.Fatalf("%s, %s route: the attempt did not reach its dial: %+v err=%v", c.name, request.Route, observed, err)
			}
			deadline, bounded := dialer.dialDeadline()
			if !bounded || deadline.Before(started.Add(c.bound)) || deadline.After(finished.Add(c.bound)) {
				t.Errorf("%s, %s route: the dial ran under a deadline %s after the start (bounded=%t), want %s", c.name, request.Route, deadline.Sub(started), bounded, c.bound)
			}
		}
	}
}
