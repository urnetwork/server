package monitor

// SIGNALS.md §31.1 (OPS-M5-05) on loopback stand-ins and fixture answers: tlsedge's own unrecognized_name
// passes the probe.edge hello and a ServerHello from the http branch fails it; the platform name's
// verified handshake passes only on a certificate that verifies; a timeout is a visibility alert that
// holds a page open and never clears it; no finding or page names an address; and FS-50's M5 conditions
// at 02:00 on the fake clock, a broken 443 split and a registrar notice, reach the rota's sink, the
// notice paging both at once. Nothing here sleeps: sockets answer at once, and time moves by hand.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/pager"
	"gopkg.in/yaml.v3"
)

// The synthetic split's root and the platform name the http branch serves.
const (
	lbSplitTestRoot         = "root.example"
	lbSplitTestPlatformName = "api.platform.example"
)

// How long a loopback exchange may take before the test fails it; a guard, never a wait.
const lbSplitTestDeadline = 30 * time.Second

// How a stand-in interface routes a hello.
type lbSplitTestMode int

const (
	// §13.4's split: names under the root to tlsedge, every other name to the http branch
	lbSplitTestSplit lbSplitTestMode = iota
	// every name to the http branch, as on an interface whose split broke or never flipped
	lbSplitTestEverythingToHttp
	// every name to tlsedge
	lbSplitTestEverythingToTlsedge
	// nothing answers in time
	lbSplitTestSilent
)

// Two loopback servers standing in for one site's branches: tlsedge, which reads each hello with
// tlsedgeservice's reader and answers tlsedgeservice's alert, and the http branch, crypto/tls with a
// certificate for the platform name. Each stand-in interface routes to them by its mode.
type lbSplitTestSite struct {
	t       *testing.T
	tlsedge net.Listener
	http    net.Listener
	roots   *x509.CertPool

	stateLock      sync.Mutex
	addressModes   map[string]lbSplitTestMode
	tlsedgeHellos  []string
	httpHelloCount int
}

func newLbSplitTestSite(t *testing.T) *lbSplitTestSite {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               syntheticCertificateSubject("synthetic platform"),
		DNSNames:              []string{lbSplitTestPlatformName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	tlsedge, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	http, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tlsedge.Close()
		t.Fatal(err)
	}
	site := &lbSplitTestSite{t: t, tlsedge: tlsedge, http: http, roots: roots, addressModes: map[string]lbSplitTestMode{}}
	var serving sync.WaitGroup
	serve := func(listener net.Listener, handle func(conn net.Conn)) {
		serving.Add(1)
		go func() {
			defer serving.Done()
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				serving.Add(1)
				go func() {
					defer serving.Done()
					defer conn.Close()
					conn.SetDeadline(time.Now().Add(lbSplitTestDeadline))
					handle(conn)
				}()
			}
		}()
	}
	serve(tlsedge, func(conn net.Conn) {
		hello, err := tlsedgeservice.ReadClientHello(conn, 16*1024)
		if err != nil {
			return
		}
		func() {
			site.stateLock.Lock()
			defer site.stateLock.Unlock()
			site.tlsedgeHellos = append(site.tlsedgeHellos, hello.ServerName)
		}()
		tlsedgeservice.WriteTlsAlert(conn, tlsedgeservice.TlsAlertUnrecognizedName)
	})
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}
	serve(http, func(conn net.Conn) {
		func() {
			site.stateLock.Lock()
			defer site.stateLock.Unlock()
			site.httpHelloCount++
		}()
		// a probe hangs up after the ServerHello's first bytes, so the handshake's end is no concern here
		tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}}).Handshake()
	})
	t.Cleanup(func() {
		tlsedge.Close()
		http.Close()
		serving.Wait()
	})
	return site
}

// Routes address, an interface's address and port, by mode from now on.
func (self *lbSplitTestSite) route(address netip.Addr, mode lbSplitTestMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addressModes[netip.AddrPortFrom(address, lbSplitProbePort).String()] = mode
}

// The branch a hello for serverName reaches on address, or nil when nothing answers in time.
func (self *lbSplitTestSite) branch(address string, serverName string) net.Listener {
	self.stateLock.Lock()
	mode, ok := self.addressModes[address]
	self.stateLock.Unlock()
	if !ok {
		self.t.Errorf("a probe dialed an address no interface has")
		return nil
	}
	switch mode {
	case lbSplitTestEverythingToHttp:
		return self.http
	case lbSplitTestEverythingToTlsedge:
		return self.tlsedge
	case lbSplitTestSilent:
		return nil
	}
	if strings.HasSuffix(serverName, "."+lbSplitTestRoot) {
		return self.tlsedge
	}
	return self.http
}

// The deadline error a socket returns, which is how a production probe's timeout arrives.
func lbSplitTestTimeout(op string) error {
	return &net.OpError{Op: op, Net: "tcp", Err: os.ErrDeadlineExceeded}
}

// A SignalSource whose raw exchanges and handshakes reach the site's stand-ins as the production runner
// reaches an interface: one connection each, the hello written, the answer read.
func (self *lbSplitTestSite) source() *syntheticSource {
	return &syntheticSource{
		tcpFn: func(network string, address string, payload []byte, responseBytes int) ([]byte, error) {
			hello, err := tlsedgeservice.ReadClientHello(bytes.NewReader(payload), 16*1024)
			if err != nil {
				self.t.Errorf("the probe sent no hello tlsedge reads: %v", err)
				return nil, err
			}
			listener := self.branch(address, hello.ServerName)
			if listener == nil {
				return nil, lbSplitTestTimeout("dial")
			}
			conn, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				return nil, err
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(lbSplitTestDeadline))
			if _, err := conn.Write(payload); err != nil {
				return nil, err
			}
			response := make([]byte, responseBytes)
			_, err = io.ReadFull(conn, response)
			return response, err
		},
		tlsFn: func(network string, address string, serverName string) (TLSCertificateObservation, error) {
			listener := self.branch(address, serverName)
			if listener == nil {
				return TLSCertificateObservation{}, lbSplitTestTimeout("dial")
			}
			// the production runner's handshake, verified against the stand-in's root
			dialer := &tls.Dialer{Config: &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}}
			ctx, cancel := context.WithTimeout(context.Background(), lbSplitTestDeadline)
			defer cancel()
			conn, err := dialer.DialContext(ctx, "tcp", listener.Addr().String())
			if err != nil {
				return TLSCertificateObservation{}, err
			}
			defer conn.Close()
			peer := conn.(*tls.Conn).ConnectionState().PeerCertificates
			observation := TLSCertificateObservation{}
			intermediates := x509.NewCertPool()
			for _, certificate := range peer {
				observation.Certificates = append(observation.Certificates, certificate.Raw)
				intermediates.AddCert(certificate)
			}
			_, observation.VerifyError = peer[0].Verify(x509.VerifyOptions{DNSName: serverName, Roots: self.roots, Intermediates: intermediates})
			return observation, nil
		},
	}
}

// The names tlsedge read, in order.
func (self *lbSplitTestSite) tlsedgeServerNames() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.tlsedgeHellos...)
}

// Two interfaces on two edge hosts, each with both families, on documentation addresses.
func lbSplitTestInterfaces() []LbSplitProbeInterface {
	return []LbSplitProbeInterface{
		{Block: "alpha-edge-0-eno1", Ipv4: netip.MustParseAddr("192.0.2.10"), Ipv6: netip.MustParseAddr("2001:db8::10")},
		{Block: "alpha-edge-1-eno1", Ipv4: netip.MustParseAddr("192.0.2.11"), Ipv6: netip.MustParseAddr("2001:db8::11")},
	}
}

// Every address the interfaces have, which no alert or page may hold.
func lbSplitTestAddresses() []string {
	addresses := []string{}
	for _, lbInterface := range lbSplitTestInterfaces() {
		addresses = append(addresses, lbInterface.Ipv4.String(), lbInterface.Ipv6.String())
	}
	return addresses
}

// The probe's settings for the two interfaces.
func lbSplitTestSettings() *LbSplitProbeSettings {
	settings := DefaultLbSplitProbeSettings()
	settings.Roots = []string{lbSplitTestRoot}
	settings.Interfaces = lbSplitTestInterfaces()
	settings.PlatformName = lbSplitTestPlatformName
	return settings
}

// Routes both families of the block's interface by mode.
func (self *lbSplitTestSite) routeBlock(block string, mode lbSplitTestMode) {
	for _, lbInterface := range lbSplitTestInterfaces() {
		if lbInterface.Block == block {
			self.route(lbInterface.Ipv4, mode)
			self.route(lbInterface.Ipv6, mode)
		}
	}
}

// One run of the signal at now against source.
func runLbSplitProbe(t *testing.T, source SignalSource, settings *LbSplitProbeSettings, now time.Time) Alerts {
	t.Helper()
	signalSettings := syntheticSettings(source)
	signalSettings.Now = func() time.Time { return now }
	signalSettings.LbSplitProbe = settings
	alerts, err := NewLbSplitProbeSignal().Run(context.Background(), signalSettings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

// The alerts of class for target.
func lbSplitAlertsOf(alerts Alerts, class string, target string) Alerts {
	found := Alerts{}
	for _, alert := range alerts {
		if alert.Class == class && alert.Target == target {
			found = append(found, alert)
		}
	}
	return found
}

// The hello is crypto/tls's own, for probe.edge.<root> with no ALPN, and tlsedge's reader takes it.
func TestLbSplitProbeHelloIsAVisitorsHelloForTheProbeName(t *testing.T) {
	hello, err := lbSplitProbeClientHello(lbSplitProbeServerName(lbSplitTestRoot + "."))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := tlsedgeservice.ReadClientHello(bytes.NewReader(hello), 16*1024)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ServerName != "probe.edge."+lbSplitTestRoot || len(parsed.AlpnIds) != 0 || parsed.OffersRouteProbe() {
		t.Fatalf("hello %+v", parsed)
	}
	if !bytes.Equal(parsed.RawBytes, hello) {
		t.Fatal("tlsedge read more or less than the hello")
	}
}

// Over loopback: an interface whose split works passes both halves on both families, with tlsedge seeing
// only the probe name; one whose split sends user names to the http branch pages, naming each failing
// half by family and probe name, and nothing in its alert names an address.
func TestLbSplitProbeOverLoopbackPassesTlsedgeAndFailsTheHttpBranch(t *testing.T) {
	site := newLbSplitTestSite(t)
	site.routeBlock("alpha-edge-0-eno1", lbSplitTestSplit)
	site.routeBlock("alpha-edge-1-eno1", lbSplitTestEverythingToHttp)
	now := time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)
	alerts := runLbSplitProbe(t, site.source(), lbSplitTestSettings(), now)

	if len(alerts) != 1 {
		t.Fatalf("alerts %+v", alerts)
	}
	alert := requireAlertClass(t, alerts, lbSplitProbeClassFailing)
	if alert.Target != "alpha-edge-1-eno1" || alert.Severity != SeverityPage || alert.Sustain != 2 || alert.unobservable {
		t.Fatalf("alert %+v", alert)
	}
	for _, half := range []string{
		"ipv4 probe.edge.root.example: the http branch answered (a ServerHello)",
		"ipv6 probe.edge.root.example: the http branch answered (a ServerHello)",
	} {
		if !strings.Contains(alert.Observed, half) {
			t.Errorf("observed %q lacks %q", alert.Observed, half)
		}
	}
	if strings.Contains(alert.Observed, "platform") {
		t.Errorf("the platform half failed on the http branch: %q", alert.Observed)
	}
	requireAlertOmits(t, alert, lbSplitTestAddresses()...)
	if row, paged := pageClassOf(alert.Class); !paged || row.action != "that interface's prior lb version" {
		t.Fatalf("row %+v", row)
	}
	// both families of the working interface reached tlsedge, with the probe name and nothing else
	if got := site.tlsedgeServerNames(); !reflect.DeepEqual(got, []string{"probe.edge.root.example", "probe.edge.root.example"}) {
		t.Fatalf("tlsedge read %v", got)
	}
}

// A split that sends platform names to tlsedge fails the platform half on tlsedge's alert, even though the
// probe name passes there.
func TestLbSplitProbeFailsAPlatformNameReachingTlsedge(t *testing.T) {
	site := newLbSplitTestSite(t)
	site.routeBlock("alpha-edge-0-eno1", lbSplitTestEverythingToTlsedge)
	site.routeBlock("alpha-edge-1-eno1", lbSplitTestSplit)
	alerts := runLbSplitProbe(t, site.source(), lbSplitTestSettings(), time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC))
	if len(alerts) != 1 {
		t.Fatalf("alerts %+v", alerts)
	}
	alert := requireAlertClass(t, alerts, lbSplitProbeClassFailing)
	if alert.Target != "alpha-edge-0-eno1" ||
		!strings.Contains(alert.Observed, "ipv4 platform: an alert answered the platform name") ||
		strings.Contains(alert.Observed, "probe.edge") {
		t.Fatalf("alert %+v", alert)
	}
	requireAlertOmits(t, alert, lbSplitTestAddresses()...)
}

// A probe that cannot run is a visibility alert on its interface, never a page and never healthy.
func TestLbSplitProbeTimeoutIsAVisibilityAlert(t *testing.T) {
	site := newLbSplitTestSite(t)
	site.routeBlock("alpha-edge-0-eno1", lbSplitTestSilent)
	site.routeBlock("alpha-edge-1-eno1", lbSplitTestSplit)
	alerts := runLbSplitProbe(t, site.source(), lbSplitTestSettings(), time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC))
	if len(alerts) != 1 {
		t.Fatalf("alerts %+v", alerts)
	}
	alert := requireAlertClass(t, alerts, lbSplitProbeClassUnobservable)
	if alert.Target != "alpha-edge-0-eno1" || !alert.unobservable || alert.Severity != SeverityWarn {
		t.Fatalf("alert %+v", alert)
	}
	if !strings.Contains(alert.Observed, "ipv4 probe.edge.root.example: no answer in time") ||
		!strings.Contains(alert.Observed, "ipv6 platform: no answer in time") {
		t.Fatalf("observed %q", alert.Observed)
	}
	if _, paged := pageClassOf(alert.Class); paged {
		t.Fatal("a probe that could not run pages")
	}
	requireAlertOmits(t, alert, lbSplitTestAddresses()...)
}

// Each answer the probe.edge hello can draw, tlsedge's own alert bytes among them: only tlsedge's
// unrecognized_name passes; bytes that arrived outrank an error; a timeout or a connection that never
// opened is blind.
func TestLbSplitUserAnswerPassesOnlyTlsedgesAlert(t *testing.T) {
	tlsedgeAlert := func(code tlsedgeservice.TlsAlertCode) []byte {
		var written bytes.Buffer
		if err := tlsedgeservice.WriteTlsAlert(&written, code); err != nil {
			t.Fatal(err)
		}
		return written.Bytes()
	}
	serverHello := func() []byte {
		site := newLbSplitTestSite(t)
		hello, err := lbSplitProbeClientHello(lbSplitProbeServerName(lbSplitTestRoot))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.Dial("tcp", site.http.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(lbSplitTestDeadline))
		if _, err := conn.Write(hello); err != nil {
			t.Fatal(err)
		}
		response := make([]byte, lbSplitTlsAlertRecordByteCount)
		if _, err := io.ReadFull(conn, response); err != nil {
			t.Fatal(err)
		}
		return response
	}()
	partial := func(data []byte, count int) []byte {
		response := make([]byte, lbSplitTlsAlertRecordByteCount)
		copy(response, data[:count])
		return response
	}
	for _, c := range []struct {
		name     string
		response []byte
		err      error
		want     lbSplitVerdict
	}{
		{name: "tlsedge's unrecognized_name", response: tlsedgeAlert(tlsedgeservice.TlsAlertUnrecognizedName), want: lbSplitPassed},
		{name: "tlsedge's internal_error", response: tlsedgeAlert(tlsedgeservice.TlsAlertInternalError), want: lbSplitFailed},
		{name: "tlsedge's decode_error", response: tlsedgeAlert(tlsedgeservice.TlsAlertDecodeError), want: lbSplitFailed},
		{name: "a warning, not fatal", response: []byte{21, 3, 3, 0, 2, 1, 112}, want: lbSplitFailed},
		{name: "an alert of another length", response: []byte{21, 3, 3, 0, 3, 2, 112}, want: lbSplitFailed},
		{name: "the http branch's ServerHello", response: serverHello, want: lbSplitFailed},
		{name: "a ServerHello's first byte, then a timeout", response: partial(serverHello, 1), err: lbSplitTestTimeout("read"), want: lbSplitFailed},
		{name: "half an alert, then a close", response: partial(tlsedgeAlert(tlsedgeservice.TlsAlertUnrecognizedName), 5), err: io.ErrUnexpectedEOF, want: lbSplitFailed},
		{name: "plaintext http", response: []byte("HTTP/1"), want: lbSplitFailed},
		{name: "a close with no answer", response: make([]byte, lbSplitTlsAlertRecordByteCount), err: io.EOF, want: lbSplitFailed},
		{name: "a reset with no answer", response: make([]byte, lbSplitTlsAlertRecordByteCount), err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, want: lbSplitFailed},
		{name: "a read timeout", response: make([]byte, lbSplitTlsAlertRecordByteCount), err: lbSplitTestTimeout("read"), want: lbSplitBlind},
		{name: "a dial timeout", err: lbSplitTestTimeout("dial"), want: lbSplitBlind},
		{name: "a refused dial", err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, want: lbSplitBlind},
		{name: "a failed write", err: &net.OpError{Op: "write", Net: "tcp", Err: errors.New("broken pipe")}, want: lbSplitBlind},
		{name: "the context's end", err: context.DeadlineExceeded, want: lbSplitBlind},
		{name: "an error of no known kind", err: errors.New("synthetic"), want: lbSplitBlind},
	} {
		outcome := lbSplitUserAnswer(c.response, c.err)
		if outcome.verdict != c.want {
			t.Errorf("%s: verdict %d (%q), want %d", c.name, outcome.verdict, outcome.reason, c.want)
		}
		if outcome.verdict != lbSplitPassed && outcome.reason == "" {
			t.Errorf("%s: no reason", c.name)
		}
	}
}

// The platform half passes only a handshake whose certificate verifies for the name.
func TestLbSplitPlatformAnswerPassesOnlyAVerifiedCertificate(t *testing.T) {
	for _, c := range []struct {
		name        string
		observation TLSCertificateObservation
		err         error
		want        lbSplitVerdict
	}{
		{name: "verified", observation: TLSCertificateObservation{Certificates: [][]byte{{1}}}, want: lbSplitPassed},
		{name: "not verified", observation: TLSCertificateObservation{Certificates: [][]byte{{1}}, VerifyError: errors.New("x509: certificate is valid for another name")}, want: lbSplitFailed},
		{name: "tlsedge's alert", err: &net.OpError{Op: "remote error", Err: errors.New("tls: unrecognized name")}, want: lbSplitFailed},
		{name: "no tls", err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, want: lbSplitFailed},
		{name: "a close", err: io.EOF, want: lbSplitFailed},
		{name: "a reset", err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, want: lbSplitFailed},
		{name: "a timeout", err: lbSplitTestTimeout("read"), want: lbSplitBlind},
		{name: "a refused dial", err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, want: lbSplitBlind},
		{name: "an error of no known kind", err: errors.New("synthetic"), want: lbSplitBlind},
	} {
		if outcome := lbSplitPlatformAnswer(c.observation, c.err); outcome.verdict != c.want {
			t.Errorf("%s: verdict %d (%q), want %d", c.name, outcome.verdict, outcome.reason, c.want)
		}
	}
}

// With no platform name the platform half is blind on every interface, and the probe.edge half still
// pages a broken split.
func TestLbSplitProbeWithoutAPlatformNameStillPagesTheUserHalf(t *testing.T) {
	site := newLbSplitTestSite(t)
	site.routeBlock("alpha-edge-0-eno1", lbSplitTestSplit)
	site.routeBlock("alpha-edge-1-eno1", lbSplitTestEverythingToHttp)
	settings := lbSplitTestSettings()
	settings.PlatformName = ""
	alerts := runLbSplitProbe(t, site.source(), settings, time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC))
	if len(lbSplitAlertsOf(alerts, lbSplitProbeClassFailing, "alpha-edge-1-eno1")) != 1 ||
		len(lbSplitAlertsOf(alerts, lbSplitProbeClassUnobservable, "alpha-edge-0-eno1")) != 1 || len(alerts) != 2 {
		t.Fatalf("alerts %+v", alerts)
	}
}

// One family only, when the block says so, and an interface with no address of it is blind.
func TestLbSplitProbeKeepsToTheFamiliesItIsGiven(t *testing.T) {
	dialed := []string{}
	var dialedLock sync.Mutex
	source := &syntheticSource{
		tcpFn: func(network string, _ string, _ []byte, _ int) ([]byte, error) {
			dialedLock.Lock()
			defer dialedLock.Unlock()
			dialed = append(dialed, network)
			return []byte{21, 3, 3, 0, 2, 2, 112}, nil
		},
		tlsFn: func(network string, _ string, _ string) (TLSCertificateObservation, error) {
			dialedLock.Lock()
			defer dialedLock.Unlock()
			dialed = append(dialed, network)
			return TLSCertificateObservation{Certificates: [][]byte{{1}}}, nil
		},
	}
	settings := lbSplitTestSettings()
	settings.Families = []string{lbSplitFamilyIpv6}
	settings.Interfaces = append(settings.Interfaces, LbSplitProbeInterface{Block: "alpha-edge-2-eno1", Ipv4: netip.MustParseAddr("192.0.2.12")})
	alerts := runLbSplitProbe(t, source, settings, time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC))
	if len(alerts) != 1 || len(lbSplitAlertsOf(alerts, lbSplitProbeClassUnobservable, "alpha-edge-2-eno1")) != 1 {
		t.Fatalf("alerts %+v", alerts)
	}
	for _, network := range dialed {
		if network != "tcp6" {
			t.Fatalf("dialed %v", dialed)
		}
	}
	if len(dialed) != 4 {
		t.Fatalf("dialed %v", dialed)
	}
}

// No block is the probe configured off, a standing state and no incident; a block that does not read is a
// visibility alert, so a probe asked for and not running never reads as healthy.
func TestLbSplitProbeOffAndUnreadableSettings(t *testing.T) {
	alerts := runLbSplitProbe(t, &syntheticSource{}, nil, time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC))
	if len(alerts) != 1 || !alerts[0].configuredOff || alerts[0].Class != lbSplitProbeClassUnobservable {
		t.Fatalf("no block: alerts %+v", alerts)
	}
	alerts = runLbSplitProbe(t, &syntheticSource{}, &LbSplitProbeSettings{ConfigError: "synthetic reason"}, time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC))
	if len(alerts) != 1 || alerts[0].configuredOff || !alerts[0].unobservable || alerts[0].Observed != "synthetic reason" {
		t.Fatalf("unreadable: alerts %+v", alerts)
	}
	// settings with nothing to probe, however they were built, are blind rather than quietly healthy
	noRoots := lbSplitTestSettings()
	noRoots.Roots = nil
	noInterfaces := lbSplitTestSettings()
	noInterfaces.Interfaces = nil
	for _, settings := range []*LbSplitProbeSettings{noRoots, noInterfaces} {
		alerts = runLbSplitProbe(t, &syntheticSource{}, settings, time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC))
		if len(alerts) != 1 || alerts[0].configuredOff || !alerts[0].unobservable || alerts[0].Target != lbSplitProbeTarget {
			t.Fatalf("nothing to probe: alerts %+v", alerts)
		}
	}
}

// A synthetic services.yml with the split and three edge interfaces, one IPv4 only, beside a transparent
// host and a historical version.
const lbSplitTestServicesYaml = `
domain: synthetic.example
expose_aliases:
    - manager.synthetic.example
versions:
-   lb:
        tls_split:
            roots: [Root.Example.]
        interfaces:
            alpha-edge-0.synthetic.example:
                eno1:
                    ipv4: 192.0.2.10
                    ipv6: "2001:db8::10"
            alpha-edge-1.synthetic.example:
                eno1:
                    ipv4: 192.0.2.11
                    ipv6: "2001:db8::11"
                eno2:
                    ipv4: 192.0.2.12
            relay.synthetic.example:
                eno1:
                    transparent: true
                    ipv4: 198.51.100.5
-   lb:
        interfaces:
            alpha-edge-0.synthetic.example:
                old0:
                    ipv4: 203.0.113.200
`

func lbSplitTestSettingsFrom(t *testing.T, block *lbSplitProbeYaml, servicesYamlText string) *LbSplitProbeSettings {
	t.Helper()
	var services servicesYaml
	if err := yaml.Unmarshal([]byte(servicesYamlText), &services); err != nil {
		t.Fatal(err)
	}
	return lbSplitProbeSettingsFrom(block, []byte(servicesYamlText), services, activeManagerHostnameFromServices(services))
}

// "*" takes the inventory's edge interfaces by block, the roots come from tls_split, and the platform name
// defaults to the manager hostname.
func TestLbSplitProbeSettingsReadTheInventoryAndTheSplitsRoots(t *testing.T) {
	settings := lbSplitTestSettingsFrom(t, &lbSplitProbeYaml{Interfaces: []string{"*"}}, lbSplitTestServicesYaml)
	if settings.ConfigError != "" {
		t.Fatal(settings.ConfigError)
	}
	want := []LbSplitProbeInterface{
		{Block: "alpha-edge-0-eno1", Ipv4: netip.MustParseAddr("192.0.2.10"), Ipv6: netip.MustParseAddr("2001:db8::10")},
		{Block: "alpha-edge-1-eno1", Ipv4: netip.MustParseAddr("192.0.2.11"), Ipv6: netip.MustParseAddr("2001:db8::11")},
		{Block: "alpha-edge-1-eno2", Ipv4: netip.MustParseAddr("192.0.2.12")},
	}
	if !reflect.DeepEqual(settings.Interfaces, want) || !reflect.DeepEqual(settings.Roots, []string{"root.example"}) ||
		settings.PlatformName != "manager.synthetic.example" || !reflect.DeepEqual(settings.Families, []string{"ipv4", "ipv6"}) {
		t.Fatalf("settings %+v", settings)
	}

	settings = lbSplitTestSettingsFrom(t, &lbSplitProbeYaml{
		Interfaces:   []string{" alpha-edge-1-eno2 ", "alpha-edge-0-eno1", "alpha-edge-0-eno1"},
		PlatformName: "API.Platform.Example",
		Families:     []string{"IPv4"},
	}, lbSplitTestServicesYaml)
	if settings.ConfigError != "" || len(settings.Interfaces) != 2 || settings.Interfaces[0].Block != "alpha-edge-0-eno1" ||
		settings.Interfaces[1].Block != "alpha-edge-1-eno2" || settings.PlatformName != "api.platform.example" ||
		!reflect.DeepEqual(settings.Families, []string{"ipv4"}) {
		t.Fatalf("settings %+v", settings)
	}
}

// Each settings refusal is blind, and none repeats an address or an operator's name.
func TestLbSplitProbeSettingsRefuseWhatDoesNotRead(t *testing.T) {
	noSplit := strings.Replace(lbSplitTestServicesYaml, "        tls_split:\n            roots: [Root.Example.]\n", "", 1)
	for _, c := range []struct {
		name     string
		block    *lbSplitProbeYaml
		services string
	}{
		{name: "no split", block: &lbSplitProbeYaml{Interfaces: []string{"*"}}, services: noSplit},
		{name: "no interfaces", block: &lbSplitProbeYaml{}, services: lbSplitTestServicesYaml},
		{name: "an unknown interface", block: &lbSplitProbeYaml{Interfaces: []string{"192.0.2.99"}}, services: lbSplitTestServicesYaml},
		{name: "star beside a name", block: &lbSplitProbeYaml{Interfaces: []string{"*", "alpha-edge-0-eno1"}}, services: lbSplitTestServicesYaml},
		{name: "a bad family", block: &lbSplitProbeYaml{Interfaces: []string{"*"}, Families: []string{"ipv5"}}, services: lbSplitTestServicesYaml},
		{name: "an address that does not parse", block: &lbSplitProbeYaml{Interfaces: []string{"*"}}, services: strings.Replace(lbSplitTestServicesYaml, "192.0.2.12", "192.0.2.300", 1)},
	} {
		settings := lbSplitTestSettingsFrom(t, c.block, c.services)
		if settings.ConfigError == "" || len(settings.Interfaces) != 0 {
			t.Errorf("%s: settings %+v", c.name, settings)
			continue
		}
		for _, forbidden := range []string{"192.0.2", "2001:db8"} {
			if strings.Contains(settings.ConfigError, forbidden) {
				t.Errorf("%s: the refusal holds an address: %q", c.name, settings.ConfigError)
			}
		}
	}
}

// A page that opens stays open through runs that cannot see the interface, and resolves only after
// complete runs show it passing: a probe that cannot run never clears.
func TestLbSplitProbeBlindRunsHoldThePageOpen(t *testing.T) {
	site := newLbSplitTestSite(t)
	site.routeBlock("alpha-edge-0-eno1", lbSplitTestSplit)
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)
	run := func(mode lbSplitTestMode) scriptedExecution {
		site.routeBlock("alpha-edge-1-eno1", mode)
		return scriptedExecution{alerts: runLbSplitProbe(t, site.source(), lbSplitTestSettings(), harness.clock.Now())}
	}

	broken := run(lbSplitTestEverythingToHttp)
	runScriptedExecutions(t, []scriptedExecution{broken, broken}, notifier.Handle)
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
	openKeys := harness.sink.OpenKeys()
	if len(openKeys) != 1 {
		t.Fatalf("open keys %v", openKeys)
	}

	blind := run(lbSplitTestSilent)
	runScriptedExecutions(t, []scriptedExecution{broken, broken, blind, blind, blind, blind}, notifier.Handle)
	flushPager(t, notifier)
	if got := harness.sink.OpenKeys(); !reflect.DeepEqual(got, openKeys) {
		t.Fatalf("blind runs changed the open pages: %v", got)
	}

	healthy := run(lbSplitTestSplit)
	if len(healthy.alerts) != 0 {
		t.Fatalf("a working split alerted: %+v", healthy.alerts)
	}
	runScriptedExecutions(t, []scriptedExecution{broken, broken, healthy, healthy}, notifier.Handle)
	flushPager(t, notifier)
	if got := harness.sink.OpenKeys(); len(got) != 0 {
		t.Fatalf("open keys %v after the split passed", got)
	}
}

// FS-50's M5 conditions at 02:00 on the fake clock (D-OPS-9, R-OPS-11): a broken 443 split, run by the
// signal through the run loop's gate and the pager notifier, pages the first person at once and the
// second at 30 minutes unacknowledged; a registrar notice, paged by taskworker's path on the same sink,
// pages both at once. DNS-M5-17 runs §19.8's two dns rows on this sink in its own tests.
func TestFs50M5ConditionsAt0200ReachTheRota(t *testing.T) {
	site := newLbSplitTestSite(t)
	site.routeBlock("alpha-edge-0-eno1", lbSplitTestSplit)
	site.routeBlock("alpha-edge-1-eno1", lbSplitTestEverythingToHttp)
	harness := newPagerHarness()
	notifier := NewPagerAlertNotifier(harness.settings())
	defer closePager(t, notifier)
	at0200 := time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)
	if !harness.clock.Now().Equal(at0200) {
		t.Fatalf("the clock reads %s", harness.clock.Now())
	}

	broken := scriptedExecution{alerts: runLbSplitProbe(t, site.source(), lbSplitTestSettings(), harness.clock.Now())}
	// one run stays below the page's sustain; the second pages
	runScriptedExecutions(t, []scriptedExecution{broken}, notifier.Handle)
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink)
	runScriptedExecutions(t, []scriptedExecution{broken, broken}, notifier.Handle)
	flushPager(t, notifier)
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
	splitPage := harness.sink.Notifications()[0]
	if splitPage.Page.Class != lbSplitProbeClassFailing || splitPage.Page.BothAtOnce || !splitPage.Time.Equal(at0200) {
		t.Fatalf("paged %+v", splitPage)
	}
	for _, address := range lbSplitTestAddresses() {
		if strings.Contains(splitPage.Page.Summary, address) || strings.Contains(splitPage.Page.DedupKey, address) {
			t.Fatal("the page holds an address")
		}
	}
	if strings.Contains(splitPage.Page.Summary, "alpha-edge") {
		t.Fatalf("the page names its target: %q", splitPage.Page.Summary)
	}

	caseId := server.NewId()
	if err := pager.PageNotice(context.Background(), harness.sink, pager.Notice{
		Kind:        pager.NoticeKindRegistrar,
		CaseId:      caseId,
		Time:        harness.clock.Now(),
		Environment: "synthetic",
	}); err != nil {
		t.Fatal(err)
	}
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson, pager.FakeFirstPerson, pager.FakeSecondPerson)
	for _, notification := range harness.sink.Notifications()[1:] {
		if notification.Page.Class != pager.NoticePageClass || !notification.Page.BothAtOnce || !notification.Time.Equal(at0200) ||
			notification.Page.DedupKey != pager.NoticeDedupKey(caseId) {
			t.Fatalf("notice paged %+v", notification)
		}
	}

	harness.clock.Advance(30*time.Minute - time.Second)
	harness.sink.Tick()
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson, pager.FakeFirstPerson, pager.FakeSecondPerson)
	harness.clock.Advance(time.Second)
	harness.sink.Tick()
	requirePagedPeople(t, harness.sink, pager.FakeFirstPerson, pager.FakeFirstPerson, pager.FakeSecondPerson, pager.FakeSecondPerson)
	last := harness.sink.Notifications()[3]
	if last.Page.Class != lbSplitProbeClassFailing || !last.Time.Equal(at0200.Add(30*time.Minute)) {
		t.Fatalf("the second person was paged with %+v", last)
	}
}
