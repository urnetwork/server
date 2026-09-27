package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
)

// Tests of the pin set: fetching and validating it, refreshing it without
// losing it, and a wrong served pin failing closed end to end.

// The hosts the test server serves pins for: the operator's
// echo host, whose answer places the provider, and a pooled destination.
var pinnedHosts = []string{"api.example.net", "pooled.example"}

// What a healthy server answers with: a usable pin for
// every host it observes.
func completeServedSet() map[string]ingest.GeolocationPin {
	served := map[string]ingest.GeolocationPin{}
	for _, host := range pinnedHosts {
		served[host] = ingest.GeolocationPin{Leaf: "leaf-" + host, Intermediate: "int-" + host}
	}
	return served
}

// Every host the server serves a complete
// pin for is kept, whatever the host -- no host is required any more, and none
// is dropped here; each probe cuts the set to the hosts it dials.
func TestFetchPinsKeepsEveryCompletePin(t *testing.T) {
	endpoint := &pinEndpoint{served: completeServedSet()}
	srv := httptest.NewServer(http.HandlerFunc(endpoint.serve))
	defer srv.Close()

	pins, err := fetchPins(context.Background(), &ingest.Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"})
	if err != nil {
		t.Fatalf("fetchPins: %s", err)
	}
	for _, host := range pinnedHosts {
		if got := pins[host]; len(got) != 2 || got[0] != "leaf-"+host || got[1] != "int-"+host {
			t.Errorf("host %q pins = %v", host, got)
		}
	}
}

// Half a pin is not a pin. An empty string matches no certificate, so a host
// served with one is not pinned in any useful sense -- and the server never
// writes one (its observation job errors rather than storing a chain it could
// not take an issuer from), so this shape means something is wrong upstream,
// and the whole set is refused.
func TestFetchPinsRejectsAHalfPin(t *testing.T) {
	host := pinnedHosts[0]
	for _, broken := range []ingest.GeolocationPin{
		{Leaf: "", Intermediate: "int"},
		{Leaf: "leaf", Intermediate: ""},
		{},
	} {
		served := completeServedSet()
		served[host] = broken
		endpoint := &pinEndpoint{served: served}
		srv := httptest.NewServer(http.HandlerFunc(endpoint.serve))
		pins, err := fetchPins(context.Background(), &ingest.Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"})
		srv.Close()
		if err == nil {
			t.Errorf("a served pin %+v for %q was accepted: %v", broken, host, pins)
		}
	}
}

// An empty answer is valid now: no host is required to carry a pin, and a
// host without one is verified by WebPKI. A tunnel opens without pins too.
func TestAnEmptyPinSetIsValid(t *testing.T) {
	endpoint := &pinEndpoint{served: map[string]ingest.GeolocationPin{}}
	srv := httptest.NewServer(http.HandlerFunc(endpoint.serve))
	defer srv.Close()
	pins, err := fetchPins(context.Background(), &ingest.Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"})
	if err != nil || len(pins) != 0 {
		t.Fatalf("an empty served set = %v, %v; want a valid empty map", pins, err)
	}
	tun, err := providertunnel.Open(context.Background(), providertunnel.Config{
		ApiUrl:      "http://127.0.0.1:0",
		PlatformUrl: "http://127.0.0.1:0",
		ByJwt:       "test-jwt",
		ClientId:    connect.NewId(),
	}, connect.NewId())
	if err != nil {
		t.Fatalf("providertunnel.Open with no pins: %v", err)
	}
	if err := tun.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// Serves a pin set that the test can swap or break at will,
// counting requests.
type pinEndpoint struct {
	stateLock sync.Mutex
	served    map[string]ingest.GeolocationPin
	status    int
	calls     int
}

// An http.HandlerFunc serving the current set, or the status it was broken
// with.
func (self *pinEndpoint) serve(w http.ResponseWriter, r *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls++
	if self.status != 0 && self.status != http.StatusOK {
		http.Error(w, "nope", self.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(self.served)
}

// Makes the endpoint answer status from now on; http.StatusOK mends it.
func (self *pinEndpoint) breakWith(status int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.status = status
}

// Once the prober has a good set, a
// server that stops answering must not be able to change what the prober
// trusts. It keeps the last good set and logs -- it does not blank it (an empty
// map would stop probing entirely on a transient blip) and above all does not
// unpin.
//
// Keeping a stale set is safe for a specific reason, not just convenient: the
// pins were observed by the server on a direct WebPKI-validated connection, so
// an old one still rejects a provider substituting its own certificate. What it
// stops doing eventually is matching the legitimate host after a CA change --
// which fails closed, loudly, exactly where a fresh set would.
func TestPinRefreshFailureKeepsThePreviousSet(t *testing.T) {
	endpoint := &pinEndpoint{served: completeServedSet()}
	srv := httptest.NewServer(http.HandlerFunc(endpoint.serve))
	defer srv.Close()

	client := &ingest.Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"}
	pins := &pinSet{}

	initial, err := fetchPins(context.Background(), client)
	if err != nil {
		t.Fatalf("startup fetch: %s", err)
	}
	pins.set(initial)
	before := pins.get()

	// the server breaks; refresh runs (interval 0 forces it) and must not
	// disturb the set
	endpoint.breakWith(http.StatusInternalServerError)
	refreshPins(context.Background(), client, pins, 0)

	after := pins.get()
	if len(after) == 0 {
		t.Fatal("a failed refresh blanked the pin set; the prober would stop probing on a transient server blip -- or, worse, a caller reading an empty map as 'unpinned' would probe unpinned")
	}
	if len(after) != len(before) {
		t.Fatalf("pin set changed on a failed refresh: %v -> %v", before, after)
	}
	for host, want := range before {
		got := after[host]
		if len(got) != len(want) {
			t.Fatalf("host %q pins changed on a failed refresh: %v -> %v", host, want, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("host %q pin %d changed on a failed refresh: %q -> %q", host, i, want[i], got[i])
			}
		}
	}

	// and a working server updates it again
	endpoint.breakWith(http.StatusOK)
	endpoint.stateLock.Lock()
	rotated := completeServedSet()
	for host := range rotated {
		rotated[host] = ingest.GeolocationPin{Leaf: "rotated-leaf", Intermediate: "rotated-int"}
	}
	endpoint.served = rotated
	endpoint.stateLock.Unlock()

	refreshPins(context.Background(), client, pins, 0)
	for host, got := range pins.get() {
		if got[0] != "rotated-leaf" {
			t.Errorf("host %q was not refreshed after the server recovered: %v", host, got)
		}
	}
}

// A refresh must not run before its interval has elapsed: the server observes
// every 6h and this is a control-plane call per pass otherwise.
func TestPinRefreshWaitsForTheInterval(t *testing.T) {
	endpoint := &pinEndpoint{served: completeServedSet()}
	srv := httptest.NewServer(http.HandlerFunc(endpoint.serve))
	defer srv.Close()

	client := &ingest.Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"}
	pins := &pinSet{}
	initial, err := fetchPins(context.Background(), client)
	if err != nil {
		t.Fatalf("startup fetch: %s", err)
	}
	pins.set(initial)

	endpoint.stateLock.Lock()
	callsAfterStartup := endpoint.calls
	endpoint.stateLock.Unlock()

	refreshPins(context.Background(), client, pins, time.Hour)

	endpoint.stateLock.Lock()
	defer endpoint.stateLock.Unlock()
	if endpoint.calls != callsAfterStartup {
		t.Errorf("refresh called the server %d extra time(s) inside the interval", endpoint.calls-callsAfterStartup)
	}
}

// pinSet.get must hand out a copy: a refresh that mutated the map a tunnel is
// already verifying against would change the pins mid-probe.
func TestPinSetGetReturnsACopy(t *testing.T) {
	pins := &pinSet{}
	pins.set(map[string][]string{"pinned.example": {"leaf", "int"}})

	got := pins.get()
	got["pinned.example"][0] = "tampered"
	got["evil.example"] = []string{"x"}

	again := pins.get()
	if again["pinned.example"][0] != "leaf" {
		t.Error("mutating the returned map changed the stored pin")
	}
	if _, ok := again["evil.example"]; ok {
		t.Error("adding to the returned map added a host to the stored set")
	}
}

// ---------------------------------------------------------------------------
// Teeth-check: a wrong served pin must fail closed, not fall through to an
// unpinned connection.
// ---------------------------------------------------------------------------

// A throwaway CA plus a leaf valid for every
// pinned host, so a local listener can present itself AS one of them under
// ordinary chain verification. That is what makes this a real test
// of the pin rather than of hostname verification: the certificate is
// chain-valid and correctly named for the host, exactly as a mis-issued or
// substituted certificate would be. Only the pin can tell them apart.
type testCertificateAuthority struct {
	pool *x509.CertPool
	cert tls.Certificate
	leaf *x509.Certificate
}

// The end-to-end
// teeth-check for the whole change.
//
// It runs the real chain: a server serves a pin set over the real endpoint
// shape -> the real ingest client fetches it -> the real validation gate turns
// it into the map providertunnel takes -> the real pin verifier runs against a
// real TLS handshake with a chain-valid certificate correctly named for the
// host -- here the operator's echo host, whose answer places the provider. The only link that is simulated is the provider's multiclient tunnel
// itself, which cannot be stood up in a unit test; the pin check is identical
// either way, because it is the same providertunnel code on the same
// tls.Config.
//
// The assertion that matters is not just "an error came back". It is that the
// request never reached the far side: a pin failure that still delivered the
// request would be a pin that decorates an unpinned probe.
func TestAWrongServedPinFailsClosedRatherThanProbingUnpinned(t *testing.T) {
	hosts := pinnedHosts
	host := hosts[0]
	// A throwaway CA and a leaf valid for every pinned host (see
	// testCertificateAuthority).
	newTestCa := func(hosts []string) *testCertificateAuthority {
		caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("ca key: %s", err)
		}
		caTemplate := &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               pkix.Name{CommonName: "egress-prober test ca"},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			IsCA:                  true,
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
			BasicConstraintsValid: true,
		}
		caDer, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
		if err != nil {
			t.Fatalf("ca cert: %s", err)
		}
		caCert, err := x509.ParseCertificate(caDer)
		if err != nil {
			t.Fatalf("parse ca: %s", err)
		}

		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("leaf key: %s", err)
		}
		leafTemplate := &x509.Certificate{
			SerialNumber: big.NewInt(2),
			Subject:      pkix.Name{CommonName: hosts[0]},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			DNSNames:     hosts,
			IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		}
		leafDer, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
		if err != nil {
			t.Fatalf("leaf cert: %s", err)
		}
		leaf, err := x509.ParseCertificate(leafDer)
		if err != nil {
			t.Fatalf("parse leaf: %s", err)
		}

		pool := x509.NewCertPool()
		pool.AddCert(caCert)

		return &testCertificateAuthority{
			pool: pool,
			cert: tls.Certificate{Certificate: [][]byte{leafDer, caDer}, PrivateKey: leafKey, Leaf: leaf},
			leaf: leaf,
		}
	}
	// A pin for a key that appears nowhere in the served chain:
	// the "wrong pin" a stale or mistaken server entry amounts to.
	unrelatedSpki := func() string {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("key: %s", err)
		}
		spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatalf("marshal spki: %s", err)
		}
		sum := sha256.Sum256(spki)
		return base64.StdEncoding.EncodeToString(sum[:])
	}
	ca := newTestCa(hosts)

	var stateLock sync.Mutex
	reached := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stateLock.Lock()
		reached++
		stateLock.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.cert}}
	srv.StartTLS()
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	timesReached := func() int {
		stateLock.Lock()
		defer stateLock.Unlock()
		return reached
	}

	// what the server observed: the true pin of the certificate the host
	// presents (leaf plus its issuer, exactly what the observation job records)
	truePins := map[string]ingest.GeolocationPin{}
	for _, h := range hosts {
		truePins[h] = ingest.GeolocationPin{
			Leaf:         providertunnel.SpkiPin(ca.leaf),
			Intermediate: providertunnel.SpkiPin(ca.leaf), // any cert on the verified path would do
		}
	}

	endpoint := &pinEndpoint{served: truePins}
	pinSrv := httptest.NewServer(http.HandlerFunc(endpoint.serve))
	defer pinSrv.Close()
	client := &ingest.Client{ServerUrl: pinSrv.URL, OperatorSecret: "s3cret"}

	// get returns an http.Client that reaches the local listener while
	// believing it is talking to `host`, pinned exactly as providertunnel pins
	// a host the server serves a pin for
	get := func(pins map[string][]string) error {
		cfg := providertunnel.PinnedTlsConfigForHost(pins, host)
		cfg.RootCAs = ca.pool
		httpClient := &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
					if err != nil {
						return nil, err
					}
					tlsConn := tls.Client(conn, cfg)
					if err := tlsConn.HandshakeContext(ctx); err != nil {
						conn.Close()
						return nil, err
					}
					return tlsConn, nil
				},
			},
		}
		resp, err := httpClient.Get("https://" + host + "/json")
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}

	// Before: the served pin is correct -- the probe works, and the request
	// arrives. Without this half, "it failed" would prove nothing.
	goodPins, err := fetchPins(context.Background(), client)
	if err != nil {
		t.Fatalf("fetch with the correct pin: %s", err)
	}
	if err := get(goodPins); err != nil {
		t.Fatalf("a correct served pin failed the handshake: %s", err)
	}
	if timesReached() != 1 {
		t.Fatalf("the request did not reach the source with a correct pin (reached=%d); the rest of this test would prove nothing", timesReached())
	}
	t.Logf("BEFORE (server serves the observed pin for %s): request completed, source reached %d time(s)", host, timesReached())

	// After: the server serves a wrong pin for this host. The certificate is
	// still chain-valid and still correctly named, so nothing but the pin
	// rejects it.
	wrong := unrelatedSpki()
	endpoint.stateLock.Lock()
	broken := map[string]ingest.GeolocationPin{}
	for h, p := range truePins {
		broken[h] = p
	}
	broken[host] = ingest.GeolocationPin{Leaf: wrong, Intermediate: wrong}
	endpoint.served = broken
	endpoint.stateLock.Unlock()

	badPins, err := fetchPins(context.Background(), client)
	if err != nil {
		// a wrong pin is still a well-formed set: it must reach the tunnel and
		// be rejected there, not be filtered out earlier, or this would be
		// testing validation instead of enforcement
		t.Fatalf("a well-formed set with a wrong pin was rejected by validation: %s", err)
	}
	if len(badPins[host]) == 0 {
		t.Fatalf("the wrong pin did not survive validation, so nothing would be enforced for %q", host)
	}

	err = get(badPins)
	if err == nil {
		t.Fatal("a WRONG served pin still completed the request: the probe proceeded as if unpinned, which is exactly what lets the provider under test forge the echo's answer")
	}
	if !errors.Is(err, providertunnel.ErrPinMismatch) {
		t.Errorf("the wrong pin failed with %v, want providertunnel.ErrPinMismatch (a failure for some other reason would not prove the pin is what stopped it)", err)
	}
	if timesReached() != 1 {
		t.Errorf("the source was reached %d time(s); with a wrong pin the request must never arrive", timesReached()-1)
	}
	t.Logf("AFTER  (server serves a WRONG pin for %s):    request refused: %v", host, err)
	t.Logf("AFTER  source reached %d time(s) in total -- unchanged, so the wrong-pin request never arrived; it did not proceed unpinned", timesReached())
}

// ---------------------------------------------------------------------------
// Startup: no pin set, no probing.
// ---------------------------------------------------------------------------

// A syntactically valid jwt carrying a parseable client_id. The
// prober parses it unverified (the server that issued it is the authority), so
// the signature can be anything; what matters is that startup gets past
// parseByJwtClientId and reaches the pin fetch, which is the thing under test.
func testByJwt(t *testing.T) string {
	t.Helper()
	enc := func(v any) string {
		buf, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt part: %s", err)
		}
		return base64.RawURLEncoding.EncodeToString(buf)
	}
	return enc(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." +
		enc(map[string]string{"client_id": "00000000-0000-0000-0000-000000000001"}) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("not-a-real-signature"))
}

// Like runProber, with a jwt the binary can actually parse, so
// startup proceeds past parseByJwtClientId.
func runProberWithJwt(t *testing.T, byJwt string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(buildProber(t), args...)
	cmd.Env = append(os.Environ(),
		"UR_PROBER_BY_JWT="+byJwt,
		"UR_OPERATOR_SECRET="+testOperatorSecret,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	default:
		t.Fatalf("running the prober: %s", err)
		return "", -1
	}
}

// Drives the real binary,
// because the property is about what the process does, not what a function
// returns: with a pin set it could not fetch it must not begin a pass at all.
// (An empty set is a different thing now -- a valid answer, see
// TestProberStartsWithAnEmptyPinSet.)
//
// The stub server is deliberately healthy in every other respect -- it answers
// the due list with a provider, so a prober that shrugged off the pin failure
// would have something to probe and would say so. The assertion is therefore
// not only the exit code (which a prober that probed and failed would also
// produce) but that the due endpoint was never called and no pass line was ever
// printed. Nothing was attempted.
func TestProberDoesNotProbeWhenTheStartupPinFetchFails(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pinStatus   int
		unreachable bool
	}{
		{name: "server unreachable", unreachable: true},
		{name: "pin endpoint 500", pinStatus: http.StatusInternalServerError},
		{name: "pin endpoint 404 (server too old)", pinStatus: http.StatusNotFound},
		{name: "pin endpoint 401 (wrong operator secret)", pinStatus: http.StatusUnauthorized},
	} {
		var stateLock sync.Mutex
		dueCalls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/network/geolocation-source-pins":
				http.Error(w, "nope", tc.pinStatus)
			case "/network/provider-egress-due":
				stateLock.Lock()
				dueCalls++
				stateLock.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"client_ids":["00000000-0000-0000-0000-000000000001"]}`))
			default:
				http.NotFound(w, r)
			}
		}))
		apiUrl := srv.URL
		if tc.unreachable {
			apiUrl = "http://127.0.0.1:1"
		}

		out, code := runProberWithJwt(t, testByJwt(t),
			"-api-url", apiUrl,
			"-platform-url", "ws://127.0.0.1:1",
			"-interval", "0",
			"-skip-confinement-check",
			"-skip-bandwidth",
		)
		srv.Close()

		if code == 0 {
			t.Errorf("%s: exited 0 with no pin set.\n--- output ---\n%s", tc.name, out)
		}
		if strings.Contains(out, "pass: ") {
			t.Errorf("%s: a pass ran without a pin set; the prober must not begin probing.\n--- output ---\n%s", tc.name, out)
		}
		stateLock.Lock()
		calls := dueCalls
		stateLock.Unlock()
		if calls != 0 {
			t.Errorf("%s: the prober asked for %d due batch(es) without a pin set; it must stop before scheduling anything", tc.name, calls)
		}
		if !strings.Contains(out, "refusing to start") {
			t.Errorf("%s: the prober did not say why it stopped; an operator reading journald has to be able to tell this from a crash.\n--- output ---\n%s", tc.name, out)
		}
		assertNoSecrets(t, "the startup pin failure: "+tc.name, out)
	}
}

// No host is required to be pinned any
// more, so a server with nothing observed answers 200 {} and the prober gets on
// with the pass -- fetching the destination pool (the built-in table when the
// server has none) and asking what is due.
func TestProberStartsWithAnEmptyPinSet(t *testing.T) {
	var stateLock sync.Mutex
	dueCalls, poolCalls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/network/geolocation-source-pins":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		case "/network/provider-egress-destinations":
			stateLock.Lock()
			poolCalls++
			stateLock.Unlock()
			http.NotFound(w, r)
		case "/network/provider-egress-due":
			stateLock.Lock()
			dueCalls++
			stateLock.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"client_ids":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	out, code := runProberWithJwt(t, testByJwt(t),
		"-api-url", srv.URL,
		"-platform-url", "ws://127.0.0.1:1",
		"-interval", "0",
		"-skip-confinement-check",
		"-skip-bandwidth",
	)
	if code != 0 {
		t.Errorf("exited %d with an empty pin set and nothing due.\n--- output ---\n%s", code, out)
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	if poolCalls != 1 || dueCalls != 1 {
		t.Errorf("pool fetched %d time(s), due asked %d time(s); want one pass of each.\n--- output ---\n%s", poolCalls, dueCalls, out)
	}
	if !strings.Contains(out, "pass: ") || !strings.Contains(out, "built-in destination table") {
		t.Errorf("the pass did not run on the built-in table.\n--- output ---\n%s", out)
	}
	assertNoSecrets(t, "a start on an empty pin set", out)
}
