package providertunnel

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Tests of the tunnel client: the dial path, the https-only allowlist and the
// pin check through real handshakes, the loss signal, the lifecycle of Open
// and Close, the in-tunnel-only resolver, and one connection per request.

// A self-signed test CA, testCaCert and testCaKey, installed as a
// trusted root for this test binary only (see TestMain). The two TLS
// end-to-end tests below issue leaf certificates signed by this CA, so that
// normal certificate-chain verification succeeds and the pin check --
// PinnedTlsConfigForHost's VerifyPeerCertificate -- is what actually
// isolates pass/fail. A bare self-signed leaf (no CA in the chain) would
// always be rejected at the chain-trust step, regardless of whether the pin
// matched, and would not prove pinning does anything.
var (
	testCaCert *x509.Certificate
	testCaKey  *ecdsa.PrivateKey

	// A second CA, attackerCaCert and attackerCaKey, that is also in the
	// trust store -- exactly like the real world, where the system trust
	// store contains hundreds of independent, unrelated CAs, and a leaf
	// issued by any of them chains successfully. It exists only for
	// TestCheckPinBypassViaDeadWeightIntermediate and
	// TestCheckPinBypassViaDeadWeightIntermediate_FixedRejects below: those
	// tests hold this "attacker" CA (standing in for some other publicly
	// trusted CA an attacker can obtain a certificate from) and show that,
	// under the pre-fix rawCerts-based check, it is sufficient to forge a
	// pinned host's identity by padding the wire chain with an unrelated,
	// legitimately pinned certificate that was never actually part of the
	// validated path.
	attackerCaCert *x509.Certificate
	attackerCaKey  *ecdsa.PrivateKey
)

// Installs testCaCert as a trusted root for this process by
// pointing SSL_CERT_FILE at it before any test runs. crypto/x509's Linux
// loader (root_unix.go) honors SSL_CERT_FILE to build the system root pool,
// and that pool is loaded lazily and cached for the life of the process, so
// this must happen before the first TLS handshake -- which is exactly what
// TestMain guarantees relative to m.Run(). This lets the TLS regression
// tests below exercise real chain verification plus the pin check, without
// touching pinning.go's contract (pins are additive to normal verification,
// not a replacement for it) and without relying on any real, publicly
// trusted certificate for a fake test host.
func TestMain(m *testing.M) {
	cert, key, err := generateTestCa()
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate test CA:", err)
		os.Exit(1)
	}
	testCaCert, testCaKey = cert, key

	attCert, attKey, err := generateTestCa()
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate attacker test CA:", err)
		os.Exit(1)
	}
	attackerCaCert, attackerCaKey = attCert, attKey

	dir, err := os.MkdirTemp("", "providertunnel-test-ca")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkdir temp:", err)
		os.Exit(1)
	}
	caPath := filepath.Join(dir, "ca.pem")
	// Both CAs are concatenated into one PEM file: crypto/x509's SSL_CERT_FILE
	// loader (root_unix.go) accepts a file containing multiple concatenated
	// PEM blocks via CertPool.AppendCertsFromPEM, so this installs both as
	// independently trusted roots for this test binary -- modelling a real
	// trust store, which trusts many unrelated CAs at once, not just the one
	// this package's pins are meant to track.
	var pemBytes []byte
	pemBytes = append(pemBytes, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	pemBytes = append(pemBytes, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: attCert.Raw})...)
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write ca file:", err)
		os.Exit(1)
	}
	os.Setenv("SSL_CERT_FILE", caPath)

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// Skips a test that needs testCaCert to be a
// trusted system root. TestMain installs it via SSL_CERT_FILE, which
// crypto/x509 honors only on Linux (root_unix.go); Windows and macOS use
// their platform verifiers, so there every handshake against the test CA
// fails with "certificate signed by unknown authority" -- an environmental
// failure indistinguishable from a real fail-closed regression in this
// package. Skipping keeps the two from being confused; CI runs Linux and
// still exercises these end to end.
func requireInjectedSystemTrust(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("SSL_CERT_FILE trust injection is Linux-only (GOOS=%s); the platform verifier ignores it and every handshake fails environmentally", runtime.GOOS)
	}
}

// Generates a self-signed CA certificate and its key, valid for an hour
// around now.
func generateTestCa() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "providertunnel test root CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// Mints a certificate for host, signed by testCaCert/testCaKey,
// so it chains to a trusted root under the SSL_CERT_FILE override installed
// by TestMain.
func issueLeaf(t *testing.T, host string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, testCaCert, &key.PublicKey, testCaKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// Mints a certificate for host, signed directly by the
// given CA cert/key, generalizing issueLeaf (which always signs with
// testCaCert) to any CA -- used by the C1 bypass tests below to mint a leaf
// signed by attackerCaCert instead.
func issueLeafSignedBy(t *testing.T, host string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// Mints an intermediate CA certificate signed by
// the given parent CA cert/key. Used by the C1 bypass tests to build a
// "legit" intermediate (signed by testCaCert) that plays the role of a
// real, publicly downloadable intermediate like a Let's Encrypt issuer:
// something an attacker can freely obtain and append to a Certificate
// message without ever holding its private key.
func issueIntermediateSignedBy(t *testing.T, cn string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// httpClientOverDialer is the contract Tunnel.HttpClient must satisfy: every
// request is dialed through the supplied dialer (the tunnel), never the host
// network. https is the only scheme the client will carry (see
// TestHttpClientRefusesPlainHttp), so the wiring is proven by watching the
// dialer get invoked for an allowlisted https host; the handshake then
// failing against the plaintext stub is expected and irrelevant to the
// property -- what matters is that the connection attempt rode the supplied
// dialer at all. This keeps the test free of the trust-injection harness,
// so it runs on every platform.
func TestHttpClientUsesSuppliedDialer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		})}
		_ = srv.Serve(ln)
	}()

	dialed := 0
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed++
		// ignore the requested address; always reach the stub listener,
		// modelling "the tunnel decides where bytes actually go"
		return net.Dial("tcp", ln.Addr().String())
	}

	client := httpClientOverDialer(dial, map[string][]string{"geolocation.example": {"sha256/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}, 5*time.Second)
	resp, err := client.Get("https://geolocation.example/json")
	if err == nil {
		resp.Body.Close()
		t.Fatal("TLS handshake against a plaintext stub unexpectedly succeeded")
	}
	if dialed == 0 {
		t.Fatal("request did not go through the supplied dialer")
	}
}

// The allowlist and the pins are enforced
// in DialTLSContext, which only https traffic reaches. A plain http:// URL
// would otherwise ride the raw tunnel dialer in cleartext -- unpinned,
// un-allowlisted, and forgeable by the provider being measured -- so the
// transport must refuse the scheme outright, before any bytes traverse the
// tunnel. Even an allowlisted host is refused: the allowlist grants pinned
// https, not the host.
func TestHttpClientRefusesPlainHttp(t *testing.T) {
	dialed := 0
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed++
		return nil, context.Canceled
	}

	client := httpClientOverDialer(dial, map[string][]string{"geolocation.example": {"sha256/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}, 5*time.Second)
	resp, err := client.Get("http://geolocation.example/json")
	if err == nil {
		resp.Body.Close()
		t.Fatal("plain http through the tunnel succeeded; it must be refused")
	}
	if !errors.Is(err, ErrPlainHttpRefused) {
		t.Fatalf("err = %v, want it to wrap ErrPlainHttpRefused", err)
	}
	if dialed != 0 {
		t.Fatalf("the tunnel dialer was invoked %d time(s) for a plain-http request; the refusal must happen before any bytes traverse the tunnel", dialed)
	}
}

// Asserts the transport is wired to
// dial through the tunnel and to build its TLS config per host (via
// DialTLSContext), rather than sharing one mutated *tls.Config template
// across hosts -- the latter is exactly the fail-open shape Task 1's
// security review found (see ErrPinHostUnknown doc in pinning.go) and
// PinnedTlsConfigForHost exists to avoid.
func TestHttpClientAppliesPinnedTlsPerHost(t *testing.T) {
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, context.Canceled // never actually connects
	}
	client := httpClientOverDialer(dial, map[string][]string{"pinned.example": {"pin"}}, time.Second)
	tr, ok := client.Transport.(*providerHttpTransport)
	if !ok {
		t.Fatalf("transport type = %T, want *providerHttpTransport", client.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("transport must dial through the tunnel")
	}
	if tr.DialTLSContext == nil {
		t.Fatal("transport must build a per-host pinned tls config via DialTLSContext")
	}
}

// Serves plain 200 responses over TLS using cert/key,
// on a random loopback port, and returns the raw (non-TLS-wrapping)
// net.Listener address to dial. The server owns the listener and is torn
// down by the returned cleanup func.
func startTlsTestServer(t *testing.T, cert *x509.Certificate, key *ecdsa.PrivateKey) (addr string, cleanup func()) {
	t.Helper()
	tlsCert := tls.Certificate{
		Certificate: [][]byte{cert.Raw},
		PrivateKey:  key,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})}
	go func() {
		_ = srv.Serve(ln)
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// Like startTlsTestServer, generalized to present an
// arbitrary, caller-controlled Certificate message: chain[0] is the leaf
// (served with leafKey) and any further entries are sent as-is, in order,
// exactly as crypto/tls sends whatever is in tls.Certificate.Certificate on
// the wire without validating that they actually form a path. This is what
// lets the C1 bypass test below construct a real handshake where the
// server pads its Certificate message with a certificate that plays no
// role in the validated path -- modelling an attacker appending a
// legitimate, publicly obtainable certificate as inert dead weight.
func startTlsTestServerChain(t *testing.T, chain []*x509.Certificate, leafKey *ecdsa.PrivateKey) (addr string, cleanup func()) {
	t.Helper()
	raw := make([][]byte, len(chain))
	for i, c := range chain {
		raw[i] = c.Raw
	}
	tlsCert := tls.Certificate{
		Certificate: raw,
		PrivateKey:  leafKey,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})}
	go func() {
		_ = srv.Serve(ln)
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// Returns a dial func that ignores the requested host:port and
// always connects to addr, modelling "the tunnel decides where bytes
// actually go" -- exactly like the stub dialer in
// TestHttpClientUsesSuppliedDialer, but reused for the TLS tests below.
func dialerToAddr(addr string) dialContextFunc {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return net.Dial("tcp", addr)
	}
}

// The pinning regression
// test at the http.Client layer: Task 1's own tests only prove the verifier
// rejects a mismatched pin in isolation (calling VerifyPeerCertificate
// directly). This proves the same thing through the actual client
// httpClientOverDialer builds -- a real TLS handshake, dialed through the
// tunnel's dial func, against a server presenting a certificate whose key
// does not match the configured pin for a pinned host, must fail the
// request. This is the layer a wiring mistake (e.g. accidentally using an
// unpinned config, or the clone-and-mutate bug Task 1's review caught)
// would actually bite: a malicious provider MITMing a pinned host with a
// chain-valid certificate from another issuer to forge its answer.
func TestHttpClientRejectsWrongKeyCertForPinnedHost(t *testing.T) {
	requireInjectedSystemTrust(t)
	const pinnedHost = "pinned.example"

	// The server presents a CA-signed cert for pinnedHost -- chain
	// verification succeeds under the SSL_CERT_FILE override from
	// TestMain, isolating the pin check as the thing under test.
	serverCert, serverKey := issueLeaf(t, pinnedHost)
	// The pin set trusts a different key for the same host -- simulating a
	// MITM presenting a chain-valid certificate the pin set does not trust
	// (e.g. issued by a different, also-trusted CA for a key it controls).
	trustedCert, _ := issueLeaf(t, pinnedHost)

	addr, cleanup := startTlsTestServer(t, serverCert, serverKey)
	defer cleanup()

	client := httpClientOverDialer(dialerToAddr(addr), map[string][]string{
		pinnedHost: {SpkiPin(trustedCert)},
	}, 5*time.Second)

	resp, err := client.Get("https://" + pinnedHost + "/json")
	if err == nil {
		resp.Body.Close()
		t.Fatal("FAIL-OPEN: request to a pinned host succeeded despite a wrong-key certificate")
	}
	if !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("err = %v, want it to wrap ErrPinMismatch", err)
	}
}

// The positive counterpart: with
// the correct pin configured, the same real TLS handshake through the same
// client construction succeeds. Without this, a bug that made pinning
// reject everything (fail-closed but broken, e.g. wrong host normalization)
// would not be caught by the rejection test above.
func TestHttpClientAcceptsMatchingPinnedCert(t *testing.T) {
	requireInjectedSystemTrust(t)
	const pinnedHost = "pinned.example"

	serverCert, serverKey := issueLeaf(t, pinnedHost)
	addr, cleanup := startTlsTestServer(t, serverCert, serverKey)
	defer cleanup()

	client := httpClientOverDialer(dialerToAddr(addr), map[string][]string{
		pinnedHost: {SpkiPin(serverCert)},
	}, 5*time.Second)

	resp, err := client.Get("https://" + pinnedHost + "/json")
	if err != nil {
		t.Fatalf("get err = %v, want success with a matching pin", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// The fix 1 regression test: the
// set of hosts a probe dials is closed and known, so an https host outside it
// -- no entry in the pin map and not among the client's extra hosts -- must be
// a loud, refused connection, never a silent pass. Each case here reproduces a
// way a wiring mistake could leave a host reachable that nothing asked for: a
// nil map, an empty map, a typo'd host key, and a map that only pins some
// other host. Before fix 1 these all connected with
// pinning silently disabled (checkPin's "absent host passes" contract,
// which is correct for pinning.go's general callers, was being reached
// directly by an untrusted dial path that never should have allowed it).
// The dial func here fails the test if it is ever invoked, proving the
// allowlist rejects the host before any TCP connection is attempted, not
// merely at the TLS verification step.
func TestHttpClientRefusesUnknownHostAllowlist(t *testing.T) {
	cases := []struct {
		name string
		pins map[string][]string
		host string
	}{
		{name: "nil pin map", pins: nil, host: "api.pinned.example"},
		{name: "empty pin map", pins: map[string][]string{}, host: "api.pinned.example"},
		{
			name: "typo'd host key",
			pins: map[string][]string{"pinned.example": {"somepin"}},
			host: "api.pinned.example",
		},
		{
			name: "host absent from a populated map",
			pins: map[string][]string{"other.example": {"somepin"}},
			host: "unlisted.example",
		},
	}

	for _, tc := range cases {
		dialed := false
		dial := func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = true
			return nil, fmt.Errorf("dial must not be reached for an unlisted host")
		}

		client := httpClientOverDialer(dial, tc.pins, 5*time.Second)
		resp, err := client.Get("https://" + tc.host + "/json")
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s: FAIL-OPEN: request to a host absent from the pin map succeeded", tc.name)
		}
		if !errors.Is(err, ErrPinHostUnknown) {
			t.Fatalf("%s: err = %v, want it to wrap ErrPinHostUnknown", tc.name, err)
		}
		if dialed {
			t.Fatalf("%s: dial must not be reached: an unlisted host must be refused before the TCP dial", tc.name)
		}
	}
}

// The fix 1 regression test
// for the "key mistakenly includes a port" case the reviewer found (e.g.
// Pins["api.example:443"] instead of Pins["api.example"]). Lookup normalization
// strips a :port suffix from both the dialed host and pin-map keys, so a
// port-suffixed key matches the real (portless) dialed host and pinning
// actually applies to it -- it neither silently misses (the pre-fix-1 bug)
// nor spuriously fails closed for a merely-oddly-formatted, otherwise
// correct key.
func TestHttpClientPortSuffixedPinKeyIsNormalized(t *testing.T) {
	requireInjectedSystemTrust(t)
	const pinnedHost = "pinned.example"
	serverCert, serverKey := issueLeaf(t, pinnedHost)
	addr, cleanup := startTlsTestServer(t, serverCert, serverKey)
	defer cleanup()

	// The correct pin under a port-suffixed key connects.
	client := httpClientOverDialer(dialerToAddr(addr), map[string][]string{
		pinnedHost + ":443": {SpkiPin(serverCert)},
	}, 5*time.Second)
	resp, err := client.Get("https://" + pinnedHost + "/json")
	if err != nil {
		t.Fatalf("get err = %v, want success: a port-suffixed key must normalize to match the dialed host", err)
	}
	resp.Body.Close()

	// A wrong pin under a port-suffixed key is refused, not silently bypassed.
	wrongCert, _ := issueLeaf(t, pinnedHost)
	client = httpClientOverDialer(dialerToAddr(addr), map[string][]string{
		pinnedHost + ":443": {SpkiPin(wrongCert)},
	}, 5*time.Second)
	resp, err = client.Get("https://" + pinnedHost + "/json")
	if err == nil {
		resp.Body.Close()
		t.Fatal("FAIL-OPEN: a port-suffixed key must not let an unmatched pin through")
	}
}

// The C1 teeth-check: it
// reproduces, with a real TLS handshake, the exact bypass a reviewer
// demonstrated -- a rawCerts-based pin check (matching against whatever the
// peer sent) can be defeated by an attacker who holds a leaf for the
// pinned host issued by any CA in the trust store, and who simply appends
// the real, publicly downloadable pinned intermediate to the Certificate
// message as inert dead weight.
//
// Setup, mirroring the reviewer's report:
//   - attackerCaCert is a second CA independently trusted alongside
//     testCaCert (installed by TestMain), standing in for "any CA in the
//     system trust store" -- the attacker does not need to compromise the
//     CA this host's real certificate happens to chain to, just obtain a
//     certificate from some publicly trusted CA.
//   - legitIntermediate is signed by testCaCert and its SPKI is the only
//     configured pin for pinnedHost -- modelling pinning a real issuing
//     intermediate, as a pin the server serves for a host would.
//   - The attacker never holds legitIntermediate's private key and never
//     needs to: their leaf is signed directly by attackerCaCert, so the
//     TLS handshake presents [attackerLeaf, legitIntermediate] and
//     verifies successfully via the attacker's own path (attackerLeaf ->
//     attackerCaCert), with legitIntermediate contributing nothing to that
//     path -- exactly the "dead weight" the reviewer's report describes.
//
// A pin check that scans rawCerts (the wire message the peer controls)
// finds legitIntermediate's SPKI sitting there, unused, and wrongly
// accepts. A pin check that scans verifiedChains (what crypto/tls actually
// validated: attackerLeaf + attackerCaCert) never sees legitIntermediate
// at all and correctly rejects. This test must fail against the old
// rawCerts-based checkPin and pass against the verifiedChains-based fix --
// see the task report for the before/after run showing exactly that.
func TestCheckPinBypassViaDeadWeightIntermediate(t *testing.T) {
	requireInjectedSystemTrust(t)
	const pinnedHost = "pinned.example"

	legitIntermediate, _ := issueIntermediateSignedBy(t, "legit test intermediate CA", testCaCert, testCaKey)
	attackerLeaf, attackerLeafKey := issueLeafSignedBy(t, pinnedHost, attackerCaCert, attackerCaKey)

	// Wire order matches a real handshake: leaf first, then whatever else
	// the attacker chooses to append.
	addr, cleanup := startTlsTestServerChain(t, []*x509.Certificate{attackerLeaf, legitIntermediate}, attackerLeafKey)
	defer cleanup()

	client := httpClientOverDialer(dialerToAddr(addr), map[string][]string{
		pinnedHost: {SpkiPin(legitIntermediate)}, // Only the legit intermediate is pinned
	}, 5*time.Second)

	resp, err := client.Get("https://" + pinnedHost + "/json")
	if err == nil {
		resp.Body.Close()
		t.Fatal("BYPASS: attacker-signed leaf + dead-weight legit intermediate was accepted for a pinned host; " +
			"pin check must match against the VERIFIED chain, not whatever the peer merely sent in rawCerts")
	}
	if !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("err = %v, want it to wrap ErrPinMismatch", err)
	}
}

// The positive
// counterpart to the bypass test above, exercised through the same real
// handshake path (not just a direct checkPin/VerifyPeerCertificate call):
// the leaf has rotated (its own pin is absent from the allowed set, as
// happens routinely -- Let's Encrypt roughly every 90 days) but it is
// properly, honestly chained through the pinned intermediate to a trusted
// root. This must be accepted. Without this test, a fix that makes
// checkPin match only chain[0] (the leaf) of each verified chain -- rather
// than every certificate in it -- would pass the bypass test above but
// silently break the documented intermediate-rotation contract each time a
// real leaf rotates, and nothing here would catch it.
func TestCheckPinAcceptsRotatedLeafThroughRealHandshake(t *testing.T) {
	requireInjectedSystemTrust(t)
	const pinnedHost = "pinned.example"

	legitIntermediate, legitIntermediateKey := issueIntermediateSignedBy(t, "legit test intermediate CA", testCaCert, testCaKey)
	rotatedLeaf, rotatedLeafKey := issueLeafSignedBy(t, pinnedHost, legitIntermediate, legitIntermediateKey)

	// A real handshake presents the leaf and the intermediate that issued
	// it, honestly chaining to testCaCert (trusted via SSL_CERT_FILE).
	addr, cleanup := startTlsTestServerChain(t, []*x509.Certificate{rotatedLeaf, legitIntermediate}, rotatedLeafKey)
	defer cleanup()

	client := httpClientOverDialer(dialerToAddr(addr), map[string][]string{
		pinnedHost: {SpkiPin(legitIntermediate)}, // only the intermediate is pinned; the new leaf's pin is absent
	}, 5*time.Second)

	resp, err := client.Get("https://" + pinnedHost + "/json")
	if err != nil {
		t.Fatalf("get err = %v, want success: a properly chained rotated leaf under a pinned intermediate must be accepted", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// Pins are optional now that no host is required to
// carry one -- a host without a pin is verified by WebPKI -- so a tunnel opens
// without any. What stays closed is the allowlist: with no pins and no extra
// hosts, its client can reach nothing at all.
func TestOpenAcceptsNoPins(t *testing.T) {
	for _, pins := range []map[string][]string{nil, {}} {
		cfg := dummyOpenConfig()
		cfg.Pins = pins
		tun, err := Open(context.Background(), cfg, connect.NewId())
		if err != nil {
			t.Fatalf("Open with pins %v: %v", pins, err)
		}
		resp, err := tun.HttpClientForHosts(time.Second, nil).Get("https://unlisted.example/json")
		if err == nil {
			resp.Body.Close()
			t.Error("a client with no pins and no extra hosts reached a host")
		} else if !errors.Is(err, ErrPinHostUnknown) {
			t.Errorf("err = %v, want the allowlist refusal", err)
		}
		if err := tun.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

// Lost is live while the tunnel is open, and a
// deliberate Close ends it with ErrTunnelClosed -- never ErrTunnelLost, which
// would read as the provider going away.
func TestLostEndsOnCloseAsClosed(t *testing.T) {
	tun, err := Open(context.Background(), dummyOpenConfig(), connect.NewId())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := tun.Lost().Err(); err != nil {
		t.Fatalf("Lost is already done on a fresh tunnel: %v", context.Cause(tun.Lost()))
	}
	if err := tun.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-tun.Lost().Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not end Lost")
	}
	cause := context.Cause(tun.Lost())
	if !errors.Is(cause, ErrTunnelClosed) || errors.Is(cause, ErrTunnelLost) {
		t.Fatalf("Lost's cause after Close = %v, want ErrTunnelClosed and not ErrTunnelLost", cause)
	}
}

// A connect.MultiClientMonitor the test drives by hand.
type fakeMonitor struct {
	stateLock sync.Mutex
	events    map[connect.Id]*connect.ProviderEvent
	callbacks []connect.MonitorEventFunction
}

// Returns a monitor with no providers and no callbacks.
func newFakeMonitor() *fakeMonitor {
	return &fakeMonitor{events: map[connect.Id]*connect.ProviderEvent{}}
}

// Implements connect.MultiClientMonitor.
func (self *fakeMonitor) AddMonitorEventCallback(callback connect.MonitorEventFunction) func() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.callbacks = append(self.callbacks, callback)
	index := len(self.callbacks) - 1
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.callbacks[index] = nil
	}
}

// Implements connect.MultiClientMonitor.
func (self *fakeMonitor) Events() (*connect.WindowExpandEvent, map[connect.Id]*connect.ProviderEvent) {
	return &connect.WindowExpandEvent{}, self.ProviderEvents()
}

// Implements connect.MultiClientMonitor.
func (self *fakeMonitor) WindowExpandEvent() *connect.WindowExpandEvent {
	return &connect.WindowExpandEvent{}
}

// Implements connect.MultiClientMonitor.
func (self *fakeMonitor) ProviderEvents() map[connect.Id]*connect.ProviderEvent {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	out := map[connect.Id]*connect.ProviderEvent{}
	for id, event := range self.events {
		out[id] = event
	}
	return out
}

// Records a provider state the way the monitor does -- a terminal state
// leaves the current set -- and delivers it.
func (self *fakeMonitor) emit(clientId connect.Id, state connect.ProviderState) {
	event := &connect.ProviderEvent{ClientId: clientId, State: state}
	var callbacks []connect.MonitorEventFunction
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if state.IsTerminal() {
			delete(self.events, clientId)
		} else {
			self.events[clientId] = event
		}
		callbacks = append([]connect.MonitorEventFunction(nil), self.callbacks...)
	}()
	// Delivered with no lock held, as the monitor does.
	for _, callback := range callbacks {
		if callback != nil {
			callback(&connect.WindowExpandEvent{}, map[connect.Id]*connect.ProviderEvent{clientId: event}, false)
		}
	}
}

// The path is lost
// when the provider, once added, is no longer added at all -- and not before.
// A provider still in evaluation, or one that never made it in, is a tunnel
// that never had a path, not one that lost it; and an ordinary rotation, which
// adds the replacement before the old client leaves, never empties the set.
func TestWatchProviderPathLosesOnlyAfterTheProviderWasAdded(t *testing.T) {
	lostCtx, lose := context.WithCancelCause(context.Background())
	monitor := newFakeMonitor()
	unwatch := watchProviderPath(monitor, lose)
	defer unwatch()

	first, second, third := connect.NewId(), connect.NewId(), connect.NewId()
	monitor.emit(first, connect.ProviderStateInEvaluation)
	monitor.emit(first, connect.ProviderStateEvaluationFailed)
	if lostCtx.Err() != nil {
		t.Fatal("a provider that never got added counted as a lost path")
	}

	monitor.emit(second, connect.ProviderStateInEvaluation)
	monitor.emit(second, connect.ProviderStateAdded)
	// rotation: the replacement lands before the old client leaves
	monitor.emit(third, connect.ProviderStateAdded)
	monitor.emit(second, connect.ProviderStateRemoved)
	if lostCtx.Err() != nil {
		t.Fatal("a make-before-break rotation counted as a lost path")
	}

	monitor.emit(third, connect.ProviderStateRemoved)
	if lostCtx.Err() == nil {
		t.Fatal("the provider left with nothing added in its place, and the path was not lost")
	}
	if cause := context.Cause(lostCtx); !errors.Is(cause, ErrTunnelLost) {
		t.Fatalf("cause = %v, want ErrTunnelLost", cause)
	}
}

// A tun read failing under a live tunnel
// is the path going away; the same failure after Close canceled the data path
// is teardown.
func TestPumpStoppedIsALossOnlyWhileLive(t *testing.T) {
	readErr := errors.New("tun read: endpoint closed")

	live, lose := context.WithCancelCause(context.Background())
	pumpStopped(context.Background(), readErr, lose)
	if cause := context.Cause(live); !errors.Is(cause, ErrTunnelLost) || !errors.Is(cause, readErr) {
		t.Fatalf("cause = %v, want ErrTunnelLost carrying the read error", cause)
	}

	notLost, lose := context.WithCancelCause(context.Background())
	dataCtx, cancel := context.WithCancel(context.Background())
	cancel()
	pumpStopped(dataCtx, readErr, lose)
	if notLost.Err() != nil {
		t.Fatalf("a read failing after the data path was canceled counted as a loss: %v", context.Cause(notLost))
	}
}

// A Config that lets Open build a real tunnel entirely
// offline: every construction step it drives (CreateTunWithResolver,
// NewApiMultiClientGenerator, NewRemoteUserNatMultiClient) only
// allocates in-process state (a private gvisor stack, in-memory structs) --
// none of it dials out or blocks on network I/O, so no live provider or
// server is needed to open and close a tunnel.
func dummyOpenConfig() Config {
	return Config{
		ApiUrl:            "http://127.0.0.1:0",
		PlatformUrl:       "http://127.0.0.1:0",
		ByJwt:             "test-jwt",
		ClientId:          connect.NewId(),
		Pins:              map[string][]string{"pinned.example": {"testpin"}},
		DeviceDescription: "test",
		DeviceSpec:        "test",
		Version:           "0.0.0-test",
	}
}

// A provider tunnel is already owned by the outer full or blackhole probe. The
// general multi-client defaults enable their own provider-qualification sweep,
// which made every short-lived outer probe start a redundant inner probe and
// emit misleading [rel] failure/transition lines. The tunnel keeps every other
// default but must turn that second authority off before construction.
func TestProviderTunnelMultiClientSettingsDisableNestedProviderProbe(t *testing.T) {
	expected := connect.DefaultMultiClientSettings()
	if !expected.ProviderProbe {
		t.Fatal("test precondition: shared defaults no longer enable ProviderProbe")
	}
	expected.ProviderProbe = false

	settings := providerTunnelMultiClientSettings()
	if settings.SecurityPolicyGenerator == nil || expected.SecurityPolicyGenerator == nil {
		t.Fatal("provider tunnel settings lost the default security policy generator")
	}
	if reflect.ValueOf(settings.SecurityPolicyGenerator).Pointer() != reflect.ValueOf(expected.SecurityPolicyGenerator).Pointer() {
		t.Fatal("provider tunnel settings changed the default security policy generator")
	}
	// DeepEqual intentionally reports non-nil functions as unequal, even when
	// both values name the same function. Compare that hook by code identity
	// above, then clear it from value copies so every remaining field is still
	// checked structurally.
	actualComparable := *settings
	expectedComparable := *expected
	actualComparable.SecurityPolicyGenerator = nil
	expectedComparable.SecurityPolicyGenerator = nil
	if !reflect.DeepEqual(actualComparable, expectedComparable) {
		t.Fatal("provider tunnel settings must preserve every default except ProviderProbe=false")
	}
	settings.ProviderProbe = true
	if !connect.DefaultMultiClientSettings().ProviderProbe {
		t.Fatal("provider tunnel settings mutated the shared defaults")
	}
}

// Closing a tunnel cancels its context before closing the tun. The resulting
// terminal read error is ordinary teardown and must not be reported as a live
// proxy failure.
func TestReportTunReadErrorSuppressesCanceledTunnel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	reportTunReadError(ctx, errors.New("Done"), func(...any) {
		called = true
	})
	if called {
		t.Fatal("canceled tunnel read error was reported")
	}
}

// A read error before lifecycle cancellation means the pump has died while
// its tunnel still appears live, so the original diagnostic must remain.
func TestReportTunReadErrorPreservesLiveTunnelFailure(t *testing.T) {
	wantErr := errors.New("synthetic live read failure")
	var gotArgs []any
	reportTunReadError(context.Background(), wantErr, func(args ...any) {
		gotArgs = append([]any(nil), args...)
	})
	if len(gotArgs) != 2 {
		t.Fatalf("log args = %v, want message and error", gotArgs)
	}
	if gotArgs[0] != "providertunnel: tun read error:" {
		t.Fatalf("log message = %v", gotArgs[0])
	}
	if gotArgs[1] != wantErr {
		t.Fatalf("logged error = %v, want original error", gotArgs[1])
	}
}

// The fix 2 regression test: Open/Close
// had zero coverage on the theory that exercising them needs a live
// provider. They do not -- every step Open drives is local construction
// (see dummyOpenConfig), so this runs offline in well under a second. It
// asserts the goroutine count returns to its pre-Open baseline after
// Close(), which would catch a missing cancel(), a leaked tun on an error
// path, or the packet-pump goroutine (fix 5) failing to exit. It also
// asserts a second Close() is safe, since Tunnel.Close is documented as
// idempotent.
func TestOpenCloseGoroutineLifecycle(t *testing.T) {
	// Warm up connect's process-wide local IPv4 address allocator
	// (tun.go's defaultLocalIpv4AddressAllocator, a sync.OnceValue) before
	// measuring the baseline. It spawns one AddrGenerator goroutine
	// (net_util.go) on its first-ever use in the process and, by design,
	// never tears it down -- it is a shared pool meant to outlive any single
	// Tun, not a per-tunnel resource. Without this warm-up cycle, whichever
	// of Open's two calls in this test happened to be first would appear to
	// "leak" that one-time goroutine, which is a false positive unrelated to
	// Tunnel's own teardown.
	warmup, err := Open(context.Background(), dummyOpenConfig(), connect.NewId())
	if err != nil {
		t.Fatalf("warm-up Open err = %v", err)
	}
	if err := warmup.Close(); err != nil {
		t.Fatalf("warm-up Close err = %v", err)
	}
	// Let the warm-up tunnel's own goroutines exit before sampling the
	// baseline; only the permanent singleton goroutine(s) should remain.
	time.Sleep(200 * time.Millisecond)

	baseline := runtime.NumGoroutine()

	tun, err := Open(context.Background(), dummyOpenConfig(), connect.NewId())
	if err != nil {
		t.Fatalf("Open err = %v", err)
	}

	if err := tun.Close(); err != nil {
		t.Fatalf("Close err = %v", err)
	}
	// Double-close must be safe and must not hang or panic.
	if err := tun.Close(); err != nil {
		t.Fatalf("second Close err = %v, want nil (idempotent)", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		current := runtime.NumGoroutine()
		if current <= baseline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak after Close: baseline=%d current=%d", baseline, current)
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
}

// A close owner that records the order it was closed in, and fails with err
// and whatever check reports.
type recordingCloseWaiter struct {
	name  string
	order *[]string
	err   error
	check func() error
}

// Implements closeAndWaiter.
func (self *recordingCloseWaiter) CloseAndWait(context.Context) error {
	*self.order = append(*self.order, self.name)
	if self.check != nil {
		return errors.Join(self.err, self.check())
	}
	return self.err
}

// A RemoteUserNatMultiClient does not own the generated transport clients; its
// ApiMultiClientGenerator does. The old Tunnel.Close canceled the shared
// context and closed only the multi-client/Tun, so every short-lived probe
// returned before generator-owned Client and OOB retirement was joined. A
// fleet sweep then accumulated teardown tails faster than they retired.
func TestCloseTunnelPartsJoinsGeneratorOwnership(t *testing.T) {
	order := []string{}
	pumpDone := make(chan struct{})
	close(pumpDone)
	multiClient := &recordingCloseWaiter{name: "multi-client", order: &order}
	lifecycleCtx, cancelLifecycle := context.WithCancel(t.Context())
	generator := &recordingCloseWaiter{
		name:  "generator",
		order: &order,
		check: func() error {
			if err := lifecycleCtx.Err(); err != nil {
				return fmt.Errorf("generator lifecycle canceled before retirement: %w", err)
			}
			return nil
		},
	}

	err := closeTunnelParts(
		t.Context(),
		func() { order = append(order, "cancel-data") },
		func() error { order = append(order, "tun"); return nil },
		pumpDone,
		multiClient,
		generator,
		func() { order = append(order, "client-strategy") },
		func() {
			order = append(order, "cancel-lifecycle")
			cancelLifecycle()
		},
	)
	if err != nil {
		t.Fatalf("closeTunnelParts: %v", err)
	}
	if want := []string{"cancel-data", "tun", "multi-client", "generator", "client-strategy", "cancel-lifecycle"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("close order = %v, want %v", order, want)
	}
	if lifecycleCtx.Err() == nil {
		t.Fatal("tunnel lifecycle remained live after every owner retired")
	}
}

// A failure in one owner must not skip the later owners. Teardown is a drain,
// not a short-circuit: returning the first error while leaving the generator
// alive would recreate the leak precisely on the unhealthy paths that sweep
// most often.
func TestCloseTunnelPartsAttemptsEveryOwnerAfterError(t *testing.T) {
	order := []string{}
	pumpDone := make(chan struct{})
	close(pumpDone)
	multiClient := &recordingCloseWaiter{name: "multi-client", order: &order, err: errors.New("multi-client close failed")}
	generator := &recordingCloseWaiter{name: "generator", order: &order}

	err := closeTunnelParts(
		t.Context(),
		func() { order = append(order, "cancel-data") },
		func() error { order = append(order, "tun"); return errors.New("tun close failed") },
		pumpDone,
		multiClient,
		generator,
		func() { order = append(order, "client-strategy") },
		func() { order = append(order, "cancel-lifecycle") },
	)
	if err == nil {
		t.Fatal("closeTunnelParts returned nil despite close failures")
	}
	if want := []string{"cancel-data", "tun", "multi-client", "generator", "client-strategy", "cancel-lifecycle"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("close order = %v, want %v", order, want)
	}
}

// Asserts that s permits exactly one resolution
// path -- remote DoH, dialed through the tun -- and no host-side or cleartext
// path of any kind.
//
// It walks the struct with reflection rather than checking the four toggles by
// name on purpose: connect owns DnsResolverSettings, and a future sibling
// toggle added there (a new Enable*, or a new Local* server list) would default
// to the zero value in the literal this package builds but would otherwise go
// unasserted. Reflecting over every field makes "everything off except
// EnableRemoteDoh" the property under test, not a snapshot of today's fields.
func assertInTunnelOnlyResolver(t *testing.T, s *connect.DnsResolverSettings) {
	t.Helper()
	if s == nil {
		t.Fatal("resolver settings are nil, so connect's defaults apply: EnableLocalDns is true there, which resolves the probe's destination hostnames off-tunnel in plaintext from the operator's own IP")
	}

	v := reflect.ValueOf(*s)
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		switch {
		case field.Type.Kind() == reflect.Bool:
			// EnableRemoteDoh is the single permitted path: encrypted, and
			// dialed through the tun. Every other toggle (EnableLocalDoh,
			// EnableLocalDns, EnableRemoteDns, or any future sibling) either
			// uses the host's dialer or sends the query in the clear.
			want := field.Name == "EnableRemoteDoh"
			if got := v.Field(i).Bool(); got != want {
				t.Errorf("%s = %v, want %v: only in-tunnel encrypted resolution may be enabled", field.Name, got, want)
			}
		case field.Type.Kind() == reflect.Slice && strings.HasPrefix(field.Name, "Local"):
			// Belt and braces: with no host-side servers configured, even a
			// toggle flipped by a future connect default has nothing to dial.
			if n := v.Field(i).Len(); n != 0 {
				t.Errorf("%s has %d entry/entries, want none: no host-side resolver may be reachable from this tunnel", field.Name, n)
			}
		}
	}

	if len(s.RemoteDohUrlsIpv4) == 0 {
		t.Error("RemoteDohUrlsIpv4 is empty: in-tunnel DoH would have no server to query, so no name could ever resolve")
	}
	if len(s.RemoteDnsIpv4) == 0 {
		t.Error("RemoteDnsIpv4 is empty: a hostname-form DoH server name could not be resolved through the tunnel")
	}
}

// Pins the resolver literal itself.
func TestInTunnelOnlyDnsResolverSettings(t *testing.T) {
	assertInTunnelOnlyResolver(t, inTunnelOnlyDnsResolverSettings())
}

// The regression test for the
// off-tunnel plaintext DNS leak: Open used to call
// connect.CreateTunWithDefaults, which inherits EnableLocalDns: true, so a
// failed in-tunnel DoH lookup silently fell back to a cleartext port-53 query
// for a probe destination issued from the operator's own IP. The TCP that
// followed still went through the tunnel, so no existing test could see it --
// the location stayed correct and nothing was logged.
//
// This asserts on the real Open path, not just on the settings constructor: if
// Open is ever changed back to CreateTunWithDefaults/CreateTun (or to any
// other constructor that does not go through the createTun seam), nothing is
// captured here and the test fails.
func TestOpenUsesInTunnelOnlyDnsResolution(t *testing.T) {
	var captured *connect.DnsResolverSettings
	var called bool
	var strategyCalled bool
	orig := createTun
	origStrategy := newControlplaneClientStrategy
	createTun = func(ctx context.Context, resolver *connect.DnsResolverSettings) (*connect.Tun, error) {
		called = true
		captured = resolver
		return orig(ctx, resolver)
	}
	newControlplaneClientStrategy = func(ctx context.Context) *connect.ClientStrategy {
		strategyCalled = true
		return origStrategy(ctx)
	}
	defer func() {
		createTun = orig
		newControlplaneClientStrategy = origStrategy
	}()

	tunnel, err := Open(context.Background(), dummyOpenConfig(), connect.NewId())
	if err != nil {
		t.Fatalf("Open err = %v", err)
	}
	defer tunnel.Close()

	if !called {
		t.Fatal("Open built its tun without going through createTun; it can no longer be proven that off-tunnel DNS resolution is disabled")
	}
	if !strategyCalled {
		t.Fatal("Open built its API/Connect client without the IPv4-only control-plane strategy")
	}
	assertInTunnelOnlyResolver(t, captured)
}

// Main's inner data-center path cannot carry IPv6 yet. The probe's private
// gVisor TUN must not inherit Connect's dual-stack default even though its
// separately owned control-plane strategy is already IPv4-only.
func TestOpenUsesIpv4OnlyInnerTun(t *testing.T) {
	tunnel, err := Open(context.Background(), dummyOpenConfig(), connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()
	if tunnel.tun.Ipv6Enabled() {
		t.Fatal("provider probe opened a dual-stack inner TUN")
	}
	for _, address := range tunnel.tun.LocalAddresses() {
		if !address.Is4() {
			t.Fatalf("inner TUN assigned non-IPv4 address %s", address)
		}
	}
	if _, err := tunnel.tun.DialContext(context.Background(), "tcp6", "[2001:db8::1]:443"); err == nil {
		t.Fatal("provider probe accepted an inner IPv6 dial")
	}
}

// Mints a leaf that chains to nothing in the trust store:
// it is its own issuer, and neither test CA signed it. Used below to prove that
// an allowed-but-unpinned host still gets full WebPKI chain verification -- the
// property the whole "unpinned is safe enough for a health check" argument
// rests on.
func issueSelfSignedLeaf(t *testing.T, host string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// Without this the egress-health
// probe dies at the tunnel's allowlist with ErrPinHostUnknown for every
// destination, and nothing but a live probe against a real provider would
// reveal it -- every package-level test would still be green while the feature
// was completely dead in production.
func TestHttpClientForHostsAllowsTheExtraHosts(t *testing.T) {
	requireInjectedSystemTrust(t)
	const healthHost = "health.example"

	serverCert, serverKey := issueLeaf(t, healthHost)
	addr, cleanup := startTlsTestServer(t, serverCert, serverKey)
	defer cleanup()

	client := httpClientOverDialerWithHosts(
		dialerToAddr(addr),
		map[string][]string{"pinned.example": {"some-pin"}},
		[]string{healthHost},
		5*time.Second,
	)

	resp, err := client.Get("https://" + healthHost + "/robots.txt")
	if err != nil {
		t.Fatalf("request to an explicitly allowed unpinned host failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// The allowlist is widened, not opened: a host in neither set is still
	// refused before a single byte is dialed.
	if _, err := client.Get("https://not-in-any-list.example/"); !errors.Is(err, ErrPinHostUnknown) {
		t.Fatalf("err = %v, want ErrPinHostUnknown; the allowlist must still be closed", err)
	}
}

// "allowed without a pin" must
// mean ordinary WebPKI verification, not no verification. If this failed, a
// provider on the path could forge a healthy-looking response for every
// egress-health destination with a self-signed certificate, and the probe would
// certify a blackholing provider as healthy -- the precise inversion of its
// purpose.
func TestHttpClientForHostsStillVerifiesTheChain(t *testing.T) {
	const healthHost = "health.example"

	serverCert, serverKey := issueSelfSignedLeaf(t, healthHost)
	addr, cleanup := startTlsTestServer(t, serverCert, serverKey)
	defer cleanup()

	client := httpClientOverDialerWithHosts(
		dialerToAddr(addr),
		map[string][]string{"pinned.example": {"some-pin"}},
		[]string{healthHost},
		5*time.Second,
	)

	resp, err := client.Get("https://" + healthHost + "/robots.txt")
	if err == nil {
		resp.Body.Close()
		t.Fatal("FAIL-OPEN: an unpinned allowed host was accepted with an untrusted self-signed certificate")
	}
	var unknownAuthority x509.UnknownAuthorityError
	if !errors.As(err, &unknownAuthority) && !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("err = %v, want a certificate verification failure", err)
	}
}

// Naming a host in the extra
// list must never remove its pin. Otherwise a destination in the health table
// or the server's pool that happens to share a host the server pins -- the
// operator's own /ip echo, say, whose answer places the provider -- would
// silently drop the pin it was given.
func TestHttpClientForHostsDoesNotUnpinAPinnedHost(t *testing.T) {
	requireInjectedSystemTrust(t)
	const pinnedHost = "pinned.example"

	serverCert, serverKey := issueLeaf(t, pinnedHost)
	trustedCert, _ := issueLeaf(t, pinnedHost) // a different key: the pin must not match

	addr, cleanup := startTlsTestServer(t, serverCert, serverKey)
	defer cleanup()

	client := httpClientOverDialerWithHosts(
		dialerToAddr(addr),
		map[string][]string{pinnedHost: {SpkiPin(trustedCert)}},
		[]string{pinnedHost, "PINNED.example"}, // also exercises normalization
		5*time.Second,
	)

	resp, err := client.Get("https://" + pinnedHost + "/json")
	if err == nil {
		resp.Body.Close()
		t.Fatal("FAIL-OPEN: listing a pinned host as an extra host unpinned it")
	}
	if !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("err = %v, want it to wrap ErrPinMismatch", err)
	}
}

// Counts every connection the server accepts. Connections,
// not requests: the thing the bandwidth probe depends on is that N concurrent
// requests are N transport connections, and a request counter cannot tell that
// apart from N HTTP/2 streams sharing one.
type countingListener struct {
	net.Listener
	conns atomic.Int64
}

// Implements net.Listener, counting every accepted connection.
func (self *countingListener) Accept() (net.Conn, error) {
	c, err := self.Listener.Accept()
	if err == nil {
		self.conns.Add(1)
	}
	return c, err
}

// The regression test for the whole point of the parallel bandwidth probe.
//
// bandwidth.measure opens bandwidth.StreamCount requests at once because one
// TCP flow cannot exceed (connect's 1 MiB window / RTT), and N flows get N
// windows. If those requests were multiplexed over a single connection --
// HTTP/2 does exactly this -- they would share one window and the probe would
// go straight back to reporting 1 MiB / RTT for every provider on the fleet,
// with every test in the bandwidth package still passing, because the requests
// really are all being made.
//
// So this asserts on accepted connections, and the test server advertises h2
// first in ALPN: if this client ever starts offering ALPN protocols, the
// server selects h2, the connection count collapses to 1, and this fails.
func TestHttpClientForHostsOpensOneConnectionPerConcurrentRequest(t *testing.T) {
	requireInjectedSystemTrust(t)
	const bandwidthHost = "bandwidth.example"
	const streams = 8

	serverCert, serverKey := issueLeaf(t, bandwidthHost)
	raw, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{serverCert.Raw},
			PrivateKey:  serverKey,
		}},
		// h2 offered first, so a client that negotiates ALPN at all will end
		// up multiplexing. This is the trap the assertion below is set for.
		NextProtos: []string{"h2", "http/1.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ln := &countingListener{Listener: raw}

	// The handler holds every request open until all of them have arrived, so
	// they are unambiguously simultaneous. Without this the client could serve
	// them one after another on one reused connection and still be correct.
	var arrived atomic.Int64
	allArrived := make(chan struct{})
	var once sync.Once
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(arrived.Add(1)) == streams {
			once.Do(func() { close(allArrived) })
		}
		select {
		case <-allArrived:
		case <-time.After(10 * time.Second):
			t.Errorf("only %d of %d requests were in flight at once", arrived.Load(), streams)
		}
		_, _ = w.Write([]byte("ok"))
	})}
	go func() {
		_ = srv.Serve(ln)
	}()
	defer func() { _ = raw.Close() }()

	client := httpClientOverDialerWithHosts(
		dialerToAddr(raw.Addr().String()),
		map[string][]string{"pinned.example": {"some-pin"}},
		[]string{bandwidthHost},
		30*time.Second,
	)

	var wg sync.WaitGroup
	errs := make([]error, streams)
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.Get("https://" + bandwidthHost + "/stream")
			if err != nil {
				errs[i] = err
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("stream %d failed: %v", i, err)
		}
	}
	if got := ln.conns.Load(); got != streams {
		t.Fatalf("the server accepted %d connections for %d concurrent requests, want %d. "+
			"Multiplexed requests share one congestion window, so the bandwidth probe would "+
			"measure one window over the RTT no matter how many streams it opens",
			got, streams, streams)
	}
}

// Open must not alias the caller's map. The cmd
// layer refreshes pins on a timer, so a caller mutating its own map after
// Open would race the per-dial reads in DialTLSContext -- and the copy had no
// coverage at all: reverting it left the whole suite green.
func TestOpenCopiesThePinMap(t *testing.T) {
	cfg := dummyOpenConfig()
	pins := map[string][]string{"pinned.example": {"originalpin"}}
	cfg.Pins = pins

	tun, err := Open(context.Background(), cfg, connect.NewId())
	if err != nil {
		t.Fatalf("Open: %s", err)
	}
	defer tun.Close()

	// The refresh the cmd layer performs, done the unsafe way.
	pins["pinned.example"] = []string{"rotatedpin"}
	pins["added.example"] = []string{"newpin"}

	if got := tun.pins["pinned.example"]; len(got) != 1 || got[0] != "originalpin" {
		t.Errorf("tunnel pins for pinned.example = %v after the caller mutated its map, want [originalpin]: Open aliased the caller's map", got)
	}
	if _, added := tun.pins["added.example"]; added {
		t.Error("a host added to the caller's map after Open appeared in the tunnel's allowlist")
	}
}
