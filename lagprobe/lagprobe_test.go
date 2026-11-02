package lagprobe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests of the measurement api probe (lag.dev/PLAN.md A4) against in-process
// servers: one that answers as an extender's measurement api does (M1, M2),
// and one for each way an extender can fail a page.

// The measurement name of these tests.
const testName = "0199ac10-0000-7000-8000-000000000001.extender.lag-test.example"

// A certificate authority of the test's own, standing in for Let's Encrypt.
type testCa struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCa(t testing.TB) *testCa {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "lag test root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCa{cert: cert, key: key, pool: pool}
}

// A leaf for name, as the CA would issue a measurement name's (C5).
func (self *testCa) issue(t testing.TB, name string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, self.cert, &key.PublicKey, self.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// Listens on a loopback address of the family, or skips the test when this
// host has none.
func listenTest(t testing.TB, ipVersion int) net.Listener {
	t.Helper()
	address := "127.0.0.1:0"
	if ipVersion == 6 {
		address = "[::1]:0"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		if ipVersion == 6 {
			t.Skipf("no IPv6 loopback: %s", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Close()
	})
	return listener
}

func listenerAddr(t testing.TB, listener net.Listener) netip.AddrPort {
	t.Helper()
	addr, err := netip.ParseAddrPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

// The measurement api's answer to the probe (M2): every response varies by
// origin and echoes an allowed one, and a get of the session route that is no
// websocket upgrade is refused 400 before any ticket is read.
func extenderHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Origin")
	if origin := r.Header.Get("Origin"); origin == "https://lag.dev" || origin == "https://ur.io" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
	if r.URL.Path != "/lag/v1/session" || r.Method != http.MethodGet {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusBadRequest)
}

// Serves handler over tls with the certificate and protocols on a loopback
// address of the family.
func serveTest(t testing.TB, ipVersion int, cert tls.Certificate, nextProtos []string, handler http.HandlerFunc) netip.AddrPort {
	t.Helper()
	listener := listenTest(t, ipVersion)
	tlsListener := tls.NewListener(listener, &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   nextProtos,
	})
	httpServer := &http.Server{
		Handler:      handler,
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		// the failures are the clients' to report
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go httpServer.Serve(tlsListener)
	t.Cleanup(func() {
		httpServer.Close()
	})
	return listenerAddr(t, listener)
}

// Accepts connections and passes each to handle, on a loopback address of
// the family. Each connection is closed when the test ends.
func serveRawTest(t testing.TB, ipVersion int, handle func(conn net.Conn)) netip.AddrPort {
	t.Helper()
	listener := listenTest(t, ipVersion)
	connsLock := sync.Mutex{}
	conns := []net.Conn{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connsLock.Lock()
			conns = append(conns, conn)
			connsLock.Unlock()
			go handle(conn)
		}
	}()
	t.Cleanup(func() {
		connsLock.Lock()
		defer connsLock.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
	})
	return listenerAddr(t, listener)
}

// An address of the family where nothing listens.
func closedTestAddr(t testing.TB, ipVersion int) netip.AddrPort {
	t.Helper()
	listener := listenTest(t, ipVersion)
	addr := listenerAddr(t, listener)
	listener.Close()
	return addr
}

// The plan's probe with short timeouts and the test CA.
func testSettings(ca *testCa) Settings {
	settings := DefaultSettings()
	settings.HandshakeTimeout = 500 * time.Millisecond
	settings.ResponseTimeout = 500 * time.Millisecond
	settings.RootCAs = ca.pool
	return settings
}

// Every address has a route, unless a test takes a family away.
func useRoutes(t testing.TB, route func(addr netip.Addr) bool) {
	t.Helper()
	previous := hasRoute
	hasRoute = route
	t.Cleanup(func() {
		hasRoute = previous
	})
}

// An extender that answers as the measurement api does passes, on either
// family.
func TestProbeOk(t *testing.T) {
	ca := newTestCa(t)
	settings := testSettings(ca)
	for _, ipVersion := range []int{4, 6} {
		addr := serveTest(t, ipVersion, ca.issue(t, testName), []string{"http/1.1"}, extenderHandler)
		outcome, err := Probe(context.Background(), Target{Name: testName, Addr: addr}, settings)
		if outcome != OutcomeOk || err != nil {
			t.Fatalf("ipv%d: %s, %v, want ok", ipVersion, outcome, err)
		}
	}
}

// Each way an extender can fail a page has its outcome, named by the first
// step that failed.
func TestProbeClassifiesFailures(t *testing.T) {
	ca := newTestCa(t)
	otherCa := newTestCa(t)
	settings := testSettings(ca)
	answer := func(status int, allowOrigin string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if allowOrigin != "" {
				w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
			}
			w.WriteHeader(status)
		}
	}
	// a tls server that finishes the handshake, then does what after says
	tlsThen := func(after func(conn *tls.Conn)) func(net.Conn) {
		return func(conn net.Conn) {
			tlsConn := tls.Server(conn, &tls.Config{
				Certificates: []tls.Certificate{ca.issue(t, testName)},
				NextProtos:   []string{"http/1.1"},
			})
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			after(tlsConn)
		}
	}

	for _, test := range []struct {
		name    string
		addr    func() netip.AddrPort
		outcome Outcome
		message string
	}{
		{
			name: "nothing listens",
			addr: func() netip.AddrPort {
				return closedTestAddr(t, 4)
			},
			outcome: OutcomeDial,
		},
		{
			name: "a certificate for another name",
			addr: func() netip.AddrPort {
				return serveTest(t, 4, ca.issue(t, "other.extender.lag-test.example"), []string{"http/1.1"}, extenderHandler)
			},
			outcome: OutcomeTls,
			message: "certificate",
		},
		{
			name: "a certificate the roots do not trust",
			addr: func() netip.AddrPort {
				return serveTest(t, 4, otherCa.issue(t, testName), []string{"http/1.1"}, extenderHandler)
			},
			outcome: OutcomeTls,
			message: "certificate",
		},
		{
			name: "no http/1.1 to agree on",
			addr: func() netip.AddrPort {
				return serveTest(t, 4, ca.issue(t, testName), []string{"h2"}, extenderHandler)
			},
			outcome: OutcomeTls,
		},
		{
			name: "no protocol agreed",
			addr: func() netip.AddrPort {
				return serveTest(t, 4, ca.issue(t, testName), nil, extenderHandler)
			},
			outcome: OutcomeTls,
			message: "rather than http/1.1",
		},
		{
			// the stall of lag.dev/PLAN.md amendment 26: tcp is accepted and
			// tls never answered
			name: "tcp accepted and the handshake never answered",
			addr: func() netip.AddrPort {
				return serveRawTest(t, 4, func(conn net.Conn) {})
			},
			outcome: OutcomeTlsTimeout,
		},
		{
			name: "the handshake and then nothing",
			addr: func() netip.AddrPort {
				return serveRawTest(t, 4, tlsThen(func(conn *tls.Conn) {
					buffer := make([]byte, 4096)
					for {
						if _, err := conn.Read(buffer); err != nil {
							return
						}
					}
				}))
			},
			outcome: OutcomeHttpTimeout,
		},
		{
			name: "the handshake and then a close",
			addr: func() netip.AddrPort {
				return serveRawTest(t, 4, tlsThen(func(conn *tls.Conn) {
					conn.Close()
				}))
			},
			outcome: OutcomeHttp,
		},
		{
			name: "the handshake and then bytes that are not http",
			addr: func() netip.AddrPort {
				return serveRawTest(t, 4, tlsThen(func(conn *tls.Conn) {
					conn.Write([]byte("SSH-2.0-OpenSSH_9.9\r\n\r\n"))
					conn.Close()
				}))
			},
			outcome: OutcomeHttp,
		},
		{
			// a name withdrawn since the certificate was installed (C9)
			name: "misdirected",
			addr: func() netip.AddrPort {
				return serveTest(t, 4, ca.issue(t, testName), []string{"http/1.1"}, answer(http.StatusMisdirectedRequest, "https://lag.dev"))
			},
			outcome: OutcomeStatus,
			message: "421",
		},
		{
			name: "a success where a refusal belongs",
			addr: func() netip.AddrPort {
				return serveTest(t, 4, ca.issue(t, testName), []string{"http/1.1"}, answer(http.StatusOK, "https://lag.dev"))
			},
			outcome: OutcomeStatus,
			message: "200",
		},
		{
			name: "the refusal without the origin",
			addr: func() netip.AddrPort {
				return serveTest(t, 4, ca.issue(t, testName), []string{"http/1.1"}, answer(http.StatusBadRequest, ""))
			},
			outcome: OutcomeCors,
		},
		{
			name: "the refusal allowing another origin",
			addr: func() netip.AddrPort {
				return serveTest(t, 4, ca.issue(t, testName), []string{"http/1.1"}, answer(http.StatusBadRequest, "https://ur.io"))
			},
			outcome: OutcomeCors,
		},
		{
			name: "the refusal allowing every origin",
			addr: func() netip.AddrPort {
				return serveTest(t, 4, ca.issue(t, testName), []string{"http/1.1"}, answer(http.StatusBadRequest, "*"))
			},
			outcome: OutcomeCors,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			start := time.Now()
			outcome, err := Probe(context.Background(), Target{Name: testName, Addr: test.addr()}, settings)
			t.Logf("%s: %v", outcome, err)
			if outcome != test.outcome {
				t.Fatalf("%s (%v), want %s", outcome, err, test.outcome)
			}
			if err == nil {
				t.Fatalf("%s without an error", outcome)
			}
			if test.message != "" && !strings.Contains(err.Error(), test.message) {
				t.Fatalf("%s: %q does not say %q", outcome, err, test.message)
			}
			if !outcome.Failed() {
				t.Fatalf("%s is not a failure", outcome)
			}
			// no probe waits past its two timeouts
			if limit := settings.HandshakeTimeout + settings.ResponseTimeout + time.Second; limit < time.Since(start) {
				t.Fatalf("took %s", time.Since(start))
			}
		})
	}
}

// A probe ended by its caller has no outcome: the prober went away, which
// says nothing about the extender.
func TestProbeCutShortHasNoOutcome(t *testing.T) {
	ca := newTestCa(t)
	settings := testSettings(ca)
	settings.HandshakeTimeout = 10 * time.Second
	settings.ResponseTimeout = 10 * time.Second
	stalled := serveRawTest(t, 4, func(conn net.Conn) {})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	outcome, err := Probe(ctx, Target{Name: testName, Addr: stalled}, settings)
	if outcome != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%q, %v, want no outcome", outcome, err)
	}

	// nor does the pass keep one, for a probe under way or one yet to start
	settings.Concurrency = 1
	passCtx, passCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer passCancel()
	pass := ProbeAll(passCtx, []Target{
		{Name: testName, Addr: stalled},
		{Name: testName, Addr: stalled},
	}, nil, settings)
	if pass.Results[0] != nil || pass.Results[1] != nil {
		t.Fatalf("probes cut short have %+v and %+v", pass.Results[0], pass.Results[1])
	}
}

// ProbeAll answers each target in its place, and records unprobed the
// addresses this host has no route to rather than probing them.
func TestProbeAllRecordsAnAddressWithoutARouteUnprobed(t *testing.T) {
	ca := newTestCa(t)
	settings := testSettings(ca)
	ok := serveTest(t, 4, ca.issue(t, testName), []string{"http/1.1"}, extenderHandler)
	refused := closedTestAddr(t, 4)
	unrouted := netip.AddrPortFrom(netip.MustParseAddr("2001:db8::1"), 443)
	useRoutes(t, func(addr netip.Addr) bool {
		return addr.Is4()
	})

	pass := ProbeAll(context.Background(), []Target{
		{Name: testName, Addr: unrouted},
		{Name: testName, Addr: ok},
		{Name: testName, Addr: refused},
	}, nil, settings)
	outcomes := []Outcome{}
	for _, result := range pass.Results {
		outcomes = append(outcomes, result.Outcome)
	}
	if !slices.Equal(outcomes, []Outcome{OutcomeUnprobed, OutcomeOk, OutcomeDial}) {
		t.Fatalf("outcomes %v", outcomes)
	}
	if !errors.Is(pass.Results[0].Err, errNoRoute) {
		t.Fatalf("unprobed for %v", pass.Results[0].Err)
	}
	if len(pass.UnreachableIpVersions) != 0 {
		t.Fatalf("unreachable families %v", pass.UnreachableIpVersions)
	}
}

// A family more than half of whose probes failed, at least the quorum of them,
// is taken to be one this host cannot reach, and its failures are recorded
// unprobed. Fewer failures, or as many successes, leave them standing.
func TestProbeAllTakesAFamilyThatMostlyFailedAsUnreachable(t *testing.T) {
	ca := newTestCa(t)
	settings := testSettings(ca)
	ok := serveTest(t, 4, ca.issue(t, testName), []string{"http/1.1"}, extenderHandler)
	refused := func() Target {
		return Target{Name: testName, Addr: closedTestAddr(t, 4)}
	}
	outcomes := func(pass *Pass) []Outcome {
		outcomes := []Outcome{}
		for _, result := range pass.Results {
			outcomes = append(outcomes, result.Outcome)
		}
		return outcomes
	}

	// three failures of three
	pass := ProbeAll(context.Background(), []Target{refused(), refused(), refused()}, nil, settings)
	if got := outcomes(pass); !slices.Equal(got, []Outcome{OutcomeUnprobed, OutcomeUnprobed, OutcomeUnprobed}) {
		t.Fatalf("three of three failed: %v", got)
	}
	if !slices.Equal(pass.UnreachableIpVersions, []int{4}) {
		t.Fatalf("unreachable families %v", pass.UnreachableIpVersions)
	}
	if !strings.Contains(pass.Results[0].Err.Error(), "dial") {
		t.Fatalf("the unprobed result loses its failure: %v", pass.Results[0].Err)
	}

	// two failures of two are under the quorum
	pass = ProbeAll(context.Background(), []Target{refused(), refused()}, nil, settings)
	if got := outcomes(pass); !slices.Equal(got, []Outcome{OutcomeDial, OutcomeDial}) {
		t.Fatalf("two of two failed: %v", got)
	}

	// three failures beside a success, the shape of a moment's outage of this
	// host's network: the probes that connected first succeeded
	okTarget := Target{Name: testName, Addr: ok}
	pass = ProbeAll(context.Background(), []Target{refused(), okTarget, refused(), refused()}, nil, settings)
	if got := outcomes(pass); !slices.Equal(got, []Outcome{OutcomeUnprobed, OutcomeOk, OutcomeUnprobed, OutcomeUnprobed}) {
		t.Fatalf("three of four failed: %v", got)
	}
	if !slices.Equal(pass.UnreachableIpVersions, []int{4}) {
		t.Fatalf("unreachable families %v", pass.UnreachableIpVersions)
	}

	// three failures beside three successes are not more than half
	pass = ProbeAll(context.Background(), []Target{refused(), okTarget, refused(), okTarget, refused(), okTarget}, nil, settings)
	if got := outcomes(pass); !slices.Equal(got, []Outcome{OutcomeDial, OutcomeOk, OutcomeDial, OutcomeOk, OutcomeDial, OutcomeOk}) {
		t.Fatalf("three of six failed: %v", got)
	}
	if len(pass.UnreachableIpVersions) != 0 {
		t.Fatalf("unreachable families %v", pass.UnreachableIpVersions)
	}

	// one extender stalling among healthy ones, which is what the rule is for
	pass = ProbeAll(context.Background(), []Target{okTarget, refused(), okTarget, okTarget}, nil, settings)
	if got := outcomes(pass); !slices.Equal(got, []Outcome{OutcomeOk, OutcomeDial, OutcomeOk, OutcomeOk}) {
		t.Fatalf("one of four failed: %v", got)
	}
}

// A second look at a pass's failures probes only them, so the family rule counts
// what succeeded in the pass beside them. Only failures that are still most of
// their family are taken for this host's own: a stall or two among healthy
// extenders stands, however many of the probes made now failed.
func TestProbeAllCountsThePassSuccessesWhenProbingAgain(t *testing.T) {
	ca := newTestCa(t)
	settings := testSettings(ca)
	refused := func(count int) []Target {
		targets := []Target{}
		for range count {
			targets = append(targets, Target{Name: testName, Addr: closedTestAddr(t, 4)})
		}
		return targets
	}
	outcomes := func(pass *Pass) []Outcome {
		outcomes := []Outcome{}
		for _, result := range pass.Results {
			outcomes = append(outcomes, result.Outcome)
		}
		return outcomes
	}
	dials := []Outcome{OutcomeDial, OutcomeDial, OutcomeDial}
	unprobed := []Outcome{OutcomeUnprobed, OutcomeUnprobed, OutcomeUnprobed}

	for _, test := range []struct {
		name             string
		alreadySucceeded map[int]int
		want             []Outcome
		unreachable      []int
	}{
		{name: "nothing else in the family", want: unprobed, unreachable: []int{4}},
		{name: "fewer successes than failures", alreadySucceeded: map[int]int{4: 2}, want: unprobed, unreachable: []int{4}},
		{name: "as many successes as failures", alreadySucceeded: map[int]int{4: 3}, want: dials, unreachable: []int{}},
		{name: "many successes", alreadySucceeded: map[int]int{4: 20}, want: dials, unreachable: []int{}},
		{name: "the other family's successes do not count", alreadySucceeded: map[int]int{6: 20}, want: unprobed, unreachable: []int{4}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pass := ProbeAll(context.Background(), refused(3), test.alreadySucceeded, settings)
			if got := outcomes(pass); !slices.Equal(got, test.want) {
				t.Fatalf("outcomes %v, want %v", got, test.want)
			}
			if !slices.Equal(pass.UnreachableIpVersions, test.unreachable) {
				t.Fatalf("unreachable families %v, want %v", pass.UnreachableIpVersions, test.unreachable)
			}
		})
	}

	// one address still failing is under the quorum, whatever it is beside
	pass := ProbeAll(context.Background(), refused(1), map[int]int{}, settings)
	if got := outcomes(pass); !slices.Equal(got, []Outcome{OutcomeDial}) {
		t.Fatalf("one still failing: %v", got)
	}
}

// The families are judged apart: IPv6 failing everywhere leaves IPv4's
// outcomes as they are, and addresses already unprobed count for neither.
func TestApplyFamilyQuorumJudgesEachFamily(t *testing.T) {
	target := func(ip string) Target {
		return Target{Name: testName, Addr: netip.AddrPortFrom(netip.MustParseAddr(ip), 443)}
	}
	targets := []Target{
		target("192.0.2.1"),
		target("192.0.2.2"),
		target("2001:db8::1"),
		target("2001:db8::2"),
		target("2001:db8::3"),
		target("2001:db8::4"),
		target("::ffff:192.0.2.3"),
	}
	results := []*Result{
		{Outcome: OutcomeOk},
		{Outcome: OutcomeTlsTimeout, Err: errors.New("stalled")},
		{Outcome: OutcomeDial, Err: errors.New("refused")},
		{Outcome: OutcomeTlsTimeout, Err: errors.New("stalled")},
		{Outcome: OutcomeUnprobed, Err: errNoRoute},
		{Outcome: OutcomeCors, Err: errors.New("cors")},
		// a mapped address is IPv4's
		{Outcome: OutcomeStatus, Err: errors.New("421")},
	}
	unreachable := applyFamilyQuorum(targets, results, nil, 3)
	if !slices.Equal(unreachable, []int{6}) {
		t.Fatalf("unreachable families %v", unreachable)
	}
	outcomes := []Outcome{}
	for _, result := range results {
		outcomes = append(outcomes, result.Outcome)
	}
	want := []Outcome{OutcomeOk, OutcomeTlsTimeout, OutcomeUnprobed, OutcomeUnprobed, OutcomeUnprobed, OutcomeUnprobed, OutcomeStatus}
	if !slices.Equal(outcomes, want) {
		t.Fatalf("outcomes %v, want %v", outcomes, want)
	}

	// a result missing, the budget's or the pass's end, counts for neither side
	results = []*Result{
		{Outcome: OutcomeDial, Err: errors.New("refused")},
		{Outcome: OutcomeDial, Err: errors.New("refused")},
		nil,
		nil,
		nil,
		nil,
		nil,
	}
	if unreachable := applyFamilyQuorum(targets, results, nil, 3); len(unreachable) != 0 {
		t.Fatalf("under the quorum: unreachable families %v", unreachable)
	}
	if results[0].Outcome != OutcomeDial {
		t.Fatalf("under the quorum: %s", results[0].Outcome)
	}
}

// At most Concurrency probes run at once, and none starts after the budget.
func TestProbeAllBoundsConcurrencyAndBudget(t *testing.T) {
	ca := newTestCa(t)
	settings := testSettings(ca)
	settings.HandshakeTimeout = 400 * time.Millisecond
	settings.Concurrency = 2
	settings.FamilyQuorum = 100

	var open atomic.Int64
	var mostOpen atomic.Int64
	stalled := serveRawTest(t, 4, func(conn net.Conn) {
		count := open.Add(1)
		for {
			most := mostOpen.Load()
			if count <= most || mostOpen.CompareAndSwap(most, count) {
				break
			}
		}
		buffer := make([]byte, 1024)
		for {
			if _, err := conn.Read(buffer); err != nil {
				break
			}
		}
		open.Add(-1)
	})
	targets := []Target{}
	for range 6 {
		targets = append(targets, Target{Name: testName, Addr: stalled})
	}
	start := time.Now()
	pass := ProbeAll(context.Background(), targets, nil, settings)
	elapsed := time.Since(start)
	for i, result := range pass.Results {
		if result == nil || result.Outcome != OutcomeTlsTimeout {
			t.Fatalf("target %d: %+v", i, result)
		}
	}
	// six stalls two at a time take three handshake timeouts, where six at
	// once would take one; the probes did overlap
	if elapsed < 3*settings.HandshakeTimeout-100*time.Millisecond || 10*time.Second < elapsed {
		t.Fatalf("six stalls two at a time took %s", elapsed)
	}
	if mostOpen.Load() < 2 {
		t.Fatalf("at most %d probes at once, want 2", mostOpen.Load())
	}

	// two at a time for 400 ms each: within a 600 ms budget the first four
	// start, and the last two do not
	settings.Budget = 600 * time.Millisecond
	pass = ProbeAll(context.Background(), targets, nil, settings)
	probed := 0
	for _, result := range pass.Results {
		if result != nil {
			probed += 1
		}
	}
	if probed != 4 || pass.Results[4] != nil || pass.Results[5] != nil {
		t.Fatalf("probed %d within the budget, want the first 4", probed)
	}
}
