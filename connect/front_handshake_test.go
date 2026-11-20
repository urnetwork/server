package connect

// The front's handshake accounting (LBALTOPT1.md R4, R12): TLS handshakes on
// TCP 443 that complete, by version, resumption and application protocol;
// the handshakes of accepted QUIC connections, completed by QUIC version and
// resumption, failed by reason; and the nginx front's count of QUIC
// ClientHellos by the class of their name. Classifier tables, and real
// handshakes against the front's public sockets.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"

	"github.com/urnetwork/server/nginx"
)

// The reason of a failed QUIC handshake comes from quic-go's typed errors and
// the front's context, never from an error's text, and every reason is one of
// the closed set. A CRYPTO_ERROR is the client's alert when quic-go marks it
// remote, and a certificate alert counts only as the client's: the front's
// own is other.
func TestFrontQuicHandshakeReasonReadsQuicGosTypedErrors(t *testing.T) {
	// a CRYPTO_ERROR carrying a TLS alert (RFC 9001 §4.8)
	cryptoError := func(alert tls.AlertError, remote bool) error {
		return &quic.TransportError{
			Remote:    remote,
			ErrorCode: quic.TransportErrorCode(0x100 + uint64(alert)),
		}
	}
	cases := []struct {
		what   string
		err    error
		reason string
	}{
		{what: "the handshake idle timeout", err: &quic.IdleTimeoutError{}, reason: frontQuicHandshakeReasonIdleTimeout},
		{what: "a wrapped handshake idle timeout", err: fmt.Errorf("await: %w", &quic.IdleTimeoutError{}), reason: frontQuicHandshakeReasonIdleTimeout},
		{what: "the handshake timeout", err: &quic.HandshakeTimeoutError{}, reason: frontQuicHandshakeReasonHandshakeTimeout},
		{what: "the client's bad_certificate", err: cryptoError(42, true), reason: frontQuicHandshakeReasonCertificate},
		{what: "the client's unsupported_certificate", err: cryptoError(43, true), reason: frontQuicHandshakeReasonCertificate},
		{what: "the client's certificate_revoked", err: cryptoError(44, true), reason: frontQuicHandshakeReasonCertificate},
		{what: "the client's certificate_expired", err: cryptoError(45, true), reason: frontQuicHandshakeReasonCertificate},
		{what: "the client's certificate_unknown", err: cryptoError(46, true), reason: frontQuicHandshakeReasonCertificate},
		{what: "the client's unknown_ca", err: cryptoError(48, true), reason: frontQuicHandshakeReasonCertificate},
		{what: "the client's certificate_unobtainable", err: cryptoError(111, true), reason: frontQuicHandshakeReasonCertificate},
		{what: "the client's bad_certificate_status_response", err: cryptoError(113, true), reason: frontQuicHandshakeReasonCertificate},
		{what: "the client's bad_certificate_hash_value", err: cryptoError(114, true), reason: frontQuicHandshakeReasonCertificate},
		{what: "the client's certificate_required", err: cryptoError(116, true), reason: frontQuicHandshakeReasonCertificate},
		// the front's own certificate problem is not the client refusing it
		{what: "the front's bad_certificate", err: cryptoError(42, false), reason: frontQuicHandshakeReasonOther},
		{what: "the client's no_application_protocol", err: cryptoError(120, true), reason: frontQuicHandshakeReasonNoAlpn},
		{what: "the front's no_application_protocol", err: cryptoError(120, false), reason: frontQuicHandshakeReasonNoAlpn},
		{what: "the client's protocol_version", err: cryptoError(70, true), reason: frontQuicHandshakeReasonVersion},
		{what: "the front's protocol_version", err: cryptoError(70, false), reason: frontQuicHandshakeReasonVersion},
		{what: "a failed version negotiation", err: &quic.VersionNegotiationError{}, reason: frontQuicHandshakeReasonVersion},
		// the shape quic-go gives the front's refusal, a local internal_error
		// that wraps it (TestFrontQuicNoCertificateSurfacesAsUnknownNameBeforeAnyAccept
		// reads the real one)
		{what: "the front's no-certificate refusal", err: errors.Join(cryptoError(80, false), nginx.ErrTlsNoCertificate), reason: frontQuicHandshakeReasonUnknownName},
		{what: "the front's other internal_error", err: cryptoError(80, false), reason: frontQuicHandshakeReasonOther},
		{what: "the client's handshake_failure", err: cryptoError(40, true), reason: frontQuicHandshakeReasonPeerClosed},
		{what: "the client's internal_error", err: cryptoError(80, true), reason: frontQuicHandshakeReasonPeerClosed},
		{what: "the client's user_canceled", err: cryptoError(90, true), reason: frontQuicHandshakeReasonPeerClosed},
		{what: "the client's NO_ERROR close", err: &quic.TransportError{Remote: true, ErrorCode: quic.NoError}, reason: frontQuicHandshakeReasonPeerClosed},
		{what: "the client's application close before its handshake completed", err: &quic.TransportError{Remote: true, ErrorCode: quic.ApplicationErrorErrorCode}, reason: frontQuicHandshakeReasonPeerClosed},
		{what: "the client's PROTOCOL_VIOLATION", err: &quic.TransportError{Remote: true, ErrorCode: quic.ProtocolViolation}, reason: frontQuicHandshakeReasonPeerClosed},
		{what: "the client's application close", err: &quic.ApplicationError{Remote: true, ErrorCode: 0x1}, reason: frontQuicHandshakeReasonPeerClosed},
		{what: "a stateless reset", err: &quic.StatelessResetError{}, reason: frontQuicHandshakeReasonPeerClosed},
		{what: "the front's PROTOCOL_VIOLATION", err: &quic.TransportError{ErrorCode: quic.ProtocolViolation}, reason: frontQuicHandshakeReasonOther},
		{what: "the front's INTERNAL_ERROR", err: &quic.TransportError{ErrorCode: quic.InternalError}, reason: frontQuicHandshakeReasonOther},
		{what: "the front's application close", err: &quic.ApplicationError{ErrorCode: 0x1}, reason: frontQuicHandshakeReasonOther},
		{what: "the front's context ending", err: context.Canceled, reason: frontQuicHandshakeReasonCanceled},
		{what: "a deadline on the wait", err: context.DeadlineExceeded, reason: frontQuicHandshakeReasonCanceled},
		{what: "the connection's transport closing", err: quic.ErrTransportClosed, reason: frontQuicHandshakeReasonCanceled},
		{what: "a wrapped transport close", err: fmt.Errorf("serve: %w", quic.ErrTransportClosed), reason: frontQuicHandshakeReasonCanceled},
		{what: "an error quic-go doesn't type", err: errors.New("synthetic"), reason: frontQuicHandshakeReasonOther},
	}
	reasons := map[string]bool{
		frontQuicHandshakeReasonIdleTimeout:      true,
		frontQuicHandshakeReasonHandshakeTimeout: true,
		frontQuicHandshakeReasonCertificate:      true,
		frontQuicHandshakeReasonNoAlpn:           true,
		frontQuicHandshakeReasonUnknownName:      true,
		frontQuicHandshakeReasonVersion:          true,
		frontQuicHandshakeReasonPeerClosed:       true,
		frontQuicHandshakeReasonCanceled:         true,
		frontQuicHandshakeReasonOther:            true,
	}
	seenReasons := map[string]bool{}
	for _, c := range cases {
		reason := frontQuicHandshakeReason(c.err)
		if reason != c.reason {
			t.Errorf("%s (%v) has reason %s, want %s", c.what, c.err, reason, c.reason)
		}
		if !reasons[reason] {
			t.Errorf("%s has reason %s, outside the closed set", c.what, reason)
		}
		seenReasons[reason] = true
	}
	for reason := range reasons {
		if !seenReasons[reason] {
			t.Errorf("no case has reason %s", reason)
		}
	}
}

// The labels of a completed handshake are closed sets: a TLS version, a QUIC
// version or a protocol the front doesn't negotiate by default is other, and
// a client that offered no protocol is none.
func TestFrontHandshakeLabelsAreClosedSets(t *testing.T) {
	tlsVersionCases := []struct {
		version uint16
		label   string
	}{
		{version: tls.VersionTLS12, label: frontTlsVersionTls12},
		{version: tls.VersionTLS13, label: frontTlsVersionTls13},
		{version: tls.VersionTLS11, label: frontTlsVersionOther},
		{version: tls.VersionTLS10, label: frontTlsVersionOther},
		{version: 0, label: frontTlsVersionOther},
	}
	for _, c := range tlsVersionCases {
		if label := frontTlsVersionLabel(c.version); label != c.label {
			t.Errorf("TLS version %#x is labeled %s, want %s", c.version, label, c.label)
		}
	}
	alpnCases := []struct {
		negotiatedProtocol string
		label              string
	}{
		{negotiatedProtocol: "h2", label: frontTlsAlpnH2},
		{negotiatedProtocol: "http/1.1", label: frontTlsAlpnHttp1},
		{negotiatedProtocol: "", label: frontTlsAlpnNone},
		{negotiatedProtocol: "acme-tls/1", label: frontTlsAlpnOther},
	}
	for _, c := range alpnCases {
		if label := frontTlsAlpnLabel(c.negotiatedProtocol); label != c.label {
			t.Errorf("protocol %q is labeled %s, want %s", c.negotiatedProtocol, label, c.label)
		}
	}
	quicVersionCases := []struct {
		version quic.Version
		label   string
	}{
		{version: quic.Version1, label: frontQuicVersionV1},
		{version: quic.Version2, label: frontQuicVersionV2},
		{version: quic.Version(0x1a2a3a4a), label: frontQuicVersionOther},
		{version: 0, label: frontQuicVersionOther},
	}
	for _, c := range quicVersionCases {
		if label := frontQuicVersionLabel(c.version); label != c.label {
			t.Errorf("QUIC version %#x is labeled %s, want %s", uint32(c.version), label, c.label)
		}
	}
}

// One completed TLS handshake on a public TCP socket, as the front counted
// it.
type testFrontTlsHandshake struct {
	version string
	resumed string
	alpn    string
}

// The completed TLS handshakes the env's TCP sockets count.
func observeTestFrontTlsHandshakes(env *testFrontEnv) chan testFrontTlsHandshake {
	handshakes := make(chan testFrontTlsHandshake, 64)
	env.settings.tlsHandshakeObserverForTest = func(version string, resumed string, alpn string) {
		select {
		case handshakes <- testFrontTlsHandshake{version: version, resumed: resumed, alpn: alpn}:
		default:
		}
	}
	return handshakes
}

// One completed QUIC handshake of an accepted connection, as the front
// counted it.
type testFrontQuicHandshake struct {
	portKind string
	version  string
	resumed  string
}

// The completed QUIC handshakes the env's accepted connections count.
func observeTestFrontQuicHandshakes(env *testFrontEnv) chan testFrontQuicHandshake {
	handshakes := make(chan testFrontQuicHandshake, 64)
	env.settings.quicHandshakeObserverForTest = func(portKind string, version string, resumed string) {
		select {
		case handshakes <- testFrontQuicHandshake{portKind: portKind, version: version, resumed: resumed}:
		default:
		}
	}
	return handshakes
}

// The next value a test observed, which must come.
func nextTestFrontObserved[T any](t testing.TB, observed <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-observed:
		return value
	case <-time.After(testFrontProgressTimeout):
		t.Fatalf("%s was not counted", what)
		var zero T
		return zero
	}
}

// A new client session cache that signals each ticket it stores.
func newTestFrontSessionCache() *testFrontSessionCache {
	return &testFrontSessionCache{
		ClientSessionCache: tls.NewLRUClientSessionCache(4),
		stored:             make(chan struct{}, 4),
	}
}

// Completes a TLS handshake with the https socket on address and has the
// server answer, so a TLS 1.3 client takes the session ticket that came with
// the server's flight: an h2 client reads the server's SETTINGS, any other
// sends a GET for authority and reads the response's first bytes. The
// client's view of the handshake.
func (self *testFrontEnv) completeTlsHandshake(address netip.Addr, tlsConfig *tls.Config, authority string) tls.ConnectionState {
	self.t.Helper()
	dialer := &net.Dialer{Timeout: testFrontProgressTimeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", self.bindAddress(address, frontPortHttps), tlsConfig)
	if err != nil {
		self.t.Fatal(err)
	}
	defer conn.Close()
	// bounds a broken test, never a passing one
	conn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
	connectionState := conn.ConnectionState()
	if connectionState.NegotiatedProtocol == http2.NextProtoTLS {
		if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
			self.t.Fatal(err)
		}
		if err := http2.NewFramer(conn, conn).WriteSettings(); err != nil {
			self.t.Fatal(err)
		}
	} else {
		if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", authority); err != nil {
			self.t.Fatal(err)
		}
	}
	if _, err := io.ReadAtLeast(conn, make([]byte, 1), 1); err != nil {
		self.t.Fatal(err)
	}
	return connectionState
}

// Every TLS handshake that completes on a public TCP socket is counted once,
// by version, whether it resumed a session and the protocol the nginx front's
// configs negotiated: h2 for an api name, http/1.1 alone for a connect name,
// none for a client that offers no protocol. A ticket from the first
// handshake resumes on the next, on TLS 1.3 and on TLS 1.2, since every
// socket shares the front's one base config (D6c).
func TestFrontHttpsCountsCompletedHandshakesByVersionResumptionAndAlpn(t *testing.T) {
	var handshakes chan testFrontTlsHandshake
	connectHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	env := newTestNginxFrontEnv(t, func(env *testFrontEnv) {
		handshakes = observeTestFrontTlsHandshakes(env)
	}, connectHandler)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)

	cases := []struct {
		what       string
		serverName string
		nextProtos []string
		maxVersion uint16
		version    string
		alpn       string
	}{
		{
			what:       "an api client of TLS 1.3 offering h2",
			serverName: testFrontApiName,
			nextProtos: []string{"h2", "http/1.1"},
			maxVersion: tls.VersionTLS13,
			version:    frontTlsVersionTls13,
			alpn:       frontTlsAlpnH2,
		},
		{
			what:       "a connect client of TLS 1.3 offering h2",
			serverName: testFrontConnectName,
			nextProtos: []string{"h2", "http/1.1"},
			maxVersion: tls.VersionTLS13,
			version:    frontTlsVersionTls13,
			alpn:       frontTlsAlpnHttp1,
		},
		{
			what:       "an api client of TLS 1.3 offering no protocol",
			serverName: testFrontApiName,
			nextProtos: nil,
			maxVersion: tls.VersionTLS13,
			version:    frontTlsVersionTls13,
			alpn:       frontTlsAlpnNone,
		},
		{
			what:       "an api client of TLS 1.2 offering h2",
			serverName: testFrontApiName,
			nextProtos: []string{"h2", "http/1.1"},
			maxVersion: tls.VersionTLS12,
			version:    frontTlsVersionTls12,
			alpn:       frontTlsAlpnH2,
		},
	}
	for _, c := range cases {
		sessionCache := newTestFrontSessionCache()
		for _, resumed := range []bool{false, true} {
			what := fmt.Sprintf("%s, resumed %t", c.what, resumed)
			want := testFrontTlsHandshake{version: c.version, resumed: strconv.FormatBool(resumed), alpn: c.alpn}
			counter := frontTlsHandshakesCounter.WithLabelValues(want.version, want.resumed, want.alpn)
			before := testutil.ToFloat64(counter)

			tlsConfig := env.clientTlsConfig(c.serverName, c.nextProtos)
			tlsConfig.MaxVersion = c.maxVersion
			tlsConfig.ClientSessionCache = sessionCache
			clientState := env.completeTlsHandshake(address, tlsConfig, testFrontConnectName)
			if clientState.DidResume != resumed {
				t.Fatalf("%s: the client resumed %t", what, clientState.DidResume)
			}
			if handshake := nextTestFrontObserved(t, handshakes, what); handshake != want {
				t.Fatalf("%s was counted as %+v, want %+v", what, handshake, want)
			}
			if after := testutil.ToFloat64(counter); after < before+1 {
				t.Fatalf("%s counted %v times", what, after-before)
			}
			if !resumed {
				testFrontWait(t, sessionCache.stored, what+"'s session ticket")
			}
		}
	}
}

// A handshake that fails is counted only by its reason, never as completed:
// a TLS 1.3 client that refuses the front's certificate, which fails the
// handshake after the front sent its Finished, and so after crypto/tls ran
// VerifyConnection, which a TLS 1.3 server runs as soon as its flight is
// out; and a client of a protocol the front doesn't speak, which fails it
// before.
func TestFrontHttpsCountsNoFailedHandshakeAsCompleted(t *testing.T) {
	var handshakes chan testFrontTlsHandshake
	var httpErrors chan testFrontHttpError
	env := newTestNginxFrontEnv(t, func(env *testFrontEnv) {
		handshakes = observeTestFrontTlsHandshakes(env)
		httpErrors = observeTestFrontHttpErrors(env)
	}, http.NotFoundHandler())
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)

	refusingTlsConfig := env.clientTlsConfig(testFrontApiName, []string{"h2", "http/1.1"})
	refusingTlsConfig.MinVersion = tls.VersionTLS13
	refusingTlsConfig.RootCAs = x509.NewCertPool()
	cases := []struct {
		what      string
		tlsConfig *tls.Config
		reason    string
	}{
		{
			what:      "a TLS 1.3 client that refuses the front's certificate",
			tlsConfig: refusingTlsConfig,
			reason:    frontTlsHandshakeReasonCertificate,
		},
		{
			what:      "a client of an application protocol the front doesn't speak",
			tlsConfig: env.clientTlsConfig(testFrontApiName, []string{"acme-tls/1"}),
			reason:    frontTlsHandshakeReasonAlpn,
		},
	}
	for _, c := range cases {
		dialer := &net.Dialer{Timeout: testFrontProgressTimeout}
		if conn, err := tls.DialWithDialer(dialer, "tcp", env.bindAddress(address, frontPortHttps), c.tlsConfig); err == nil {
			conn.Close()
			t.Fatalf("%s: the handshake succeeded", c.what)
		}
		httpError := nextTestFrontHttpError(t, httpErrors, c.what)
		if want := (testFrontHttpError{portKind: frontPortHttps, kind: frontHttpErrorTlsHandshake, reason: c.reason}); httpError != want {
			t.Fatalf("%s was %+v, want %+v", c.what, httpError, want)
		}
		// the server's goroutine logs a failure only once its handshake
		// returned, after which nothing counts it as completed
		select {
		case handshake := <-handshakes:
			t.Fatalf("%s was counted as a completed handshake: %+v", c.what, handshake)
		default:
		}
	}
}

// Every accepted QUIC connection whose handshake completes is counted once,
// on the quic and whodis ports alike, by QUIC version and whether it resumed
// a session: a ticket from a first connection resumes on the next, since
// every socket shares the nginx front's one QUIC TLS config (D6c), and a
// client of QUIC v2 is counted as one.
func TestFrontQuicCountsCompletedHandshakesByVersionAndResumption(t *testing.T) {
	var handshakes chan testFrontQuicHandshake
	env := newTestNginxFrontEnv(t, func(env *testFrontEnv) {
		handshakes = observeTestFrontQuicHandshakes(env)
	}, http.NotFoundHandler())
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)

	// a connect connection to portKind's socket, counted as want; the
	// client's connection, open
	handshake := func(what string, portKind string, tlsConfig *tls.Config, quicConfig *quic.Config, want testFrontQuicHandshake) *quic.Conn {
		t.Helper()
		counter := frontQuicHandshakesCounter.WithLabelValues(want.portKind, want.version, want.resumed)
		before := testutil.ToFloat64(counter)
		target := net.UDPAddrFromAddrPort(netip.MustParseAddrPort(env.bindAddress(address, portKind)))
		conn, err := env.dialQuicToWithConfig(env.clientPacketConn(address), target, portKind, tlsConfig, quicConfig)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got := nextTestFrontObserved(t, handshakes, what); got != want {
			t.Fatalf("%s was counted as %+v, want %+v", what, got, want)
		}
		if after := testutil.ToFloat64(counter); after < before+1 {
			t.Fatalf("%s counted %v times", what, after-before)
		}
		env.nextHandled()
		return conn
	}

	for _, portKind := range []string{frontPortQuic, frontPortWhodis} {
		sessionCache := newTestFrontSessionCache()
		for _, resumed := range []bool{false, true} {
			what := fmt.Sprintf("a %s connection, resumed %t", portKind, resumed)
			tlsConfig := env.clientTlsConfig(testFrontConnectName, nil)
			tlsConfig.ClientSessionCache = sessionCache
			want := testFrontQuicHandshake{portKind: portKind, version: frontQuicVersionV1, resumed: strconv.FormatBool(resumed)}
			conn := handshake(what, portKind, tlsConfig, testFrontClientQuicConfig(), want)
			if conn.ConnectionState().TLS.DidResume != resumed {
				t.Fatalf("%s: the client resumed %t", what, conn.ConnectionState().TLS.DidResume)
			}
			if !resumed {
				// the front sends its ticket once the handshake is confirmed
				testFrontWait(t, sessionCache.stored, what+"'s session ticket")
			}
			conn.CloseWithError(0, "")
		}
	}

	quicConfig := testFrontClientQuicConfig()
	quicConfig.Versions = []quic.Version{quic.Version2}
	want := testFrontQuicHandshake{portKind: frontPortQuic, version: frontQuicVersionV2, resumed: "false"}
	conn := handshake("a client of QUIC v2", frontPortQuic, env.clientTlsConfig(testFrontConnectName, nil), quicConfig, want)
	if version := conn.ConnectionState().Version; version != quic.Version2 {
		t.Fatalf("the client of QUIC v2 negotiated %#x", uint32(version))
	}
	conn.CloseWithError(0, "")
}

// A client socket whose reads wait until release, so the front's first
// flight waits in the socket's buffer and the client can't answer it before
// the test lets it. Closing it, or a read deadline already passed, which
// quic-go's Transport.Close sets to stop its reads, ends the wait too.
type testFrontHeldSource struct {
	net.PacketConn
	released    chan struct{}
	releaseOnce sync.Once
}

// Holds packetConn's reads.
func newTestFrontHeldSource(packetConn net.PacketConn) *testFrontHeldSource {
	return &testFrontHeldSource{
		PacketConn: packetConn,
		released:   make(chan struct{}),
	}
}

// Ends the wait.
func (self *testFrontHeldSource) release() {
	self.releaseOnce.Do(func() {
		close(self.released)
	})
}

// Reads once released.
func (self *testFrontHeldSource) ReadFrom(p []byte) (int, net.Addr, error) {
	<-self.released
	return self.PacketConn.ReadFrom(p)
}

// Ends the wait for a deadline already passed.
func (self *testFrontHeldSource) SetReadDeadline(deadline time.Time) error {
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		self.release()
	}
	return self.PacketConn.SetReadDeadline(deadline)
}

// Ends the wait.
func (self *testFrontHeldSource) Close() error {
	self.release()
	return self.PacketConn.Close()
}

// Real clients that fail an accepted connection's handshake are counted
// under the reason their failure gives: a client that refuses the front's
// certificate, on a connect name and an api one, and a client of an
// application protocol the front doesn't speak, whose no_application_protocol
// alert reads as no_alpn. Each client reads nothing until the front has
// dispatched its connection, so the front accepted it before the client
// failed.
func TestFrontQuicLabelsRealFailedHandshakesByReason(t *testing.T) {
	var results chan testFrontConnectionResult
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		results = observeTestFrontConnectionResults(env)
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)

	refusingConnectTlsConfig := env.clientTlsConfig(testFrontConnectName, nil)
	refusingConnectTlsConfig.RootCAs = x509.NewCertPool()
	refusingApiTlsConfig := env.clientTlsConfig(testFrontApiName, []string{http3.NextProtoH3})
	refusingApiTlsConfig.RootCAs = x509.NewCertPool()
	cases := []struct {
		what      string
		tlsConfig *tls.Config
		nameClass nginx.NameClass
		reason    string
	}{
		{
			what:      "a connect client that refuses the front's certificate",
			tlsConfig: refusingConnectTlsConfig,
			nameClass: nginx.NameClassConnect,
			reason:    frontQuicHandshakeReasonCertificate,
		},
		{
			what:      "an api client that refuses the front's certificate",
			tlsConfig: refusingApiTlsConfig,
			nameClass: nginx.NameClassApi,
			reason:    frontQuicHandshakeReasonCertificate,
		},
		{
			what:      "a client of an application protocol the front doesn't speak",
			tlsConfig: env.clientTlsConfig(testFrontConnectName, []string{"acme-tls/1"}),
			nameClass: nginx.NameClassConnect,
			reason:    frontQuicHandshakeReasonNoAlpn,
		},
	}
	for _, c := range cases {
		source := newTestFrontHeldSource(env.clientPacketConn(address))
		dialed := make(chan error, 1)
		go func() {
			conn, err := env.dialQuicFrom(source, address, frontPortQuic, c.tlsConfig)
			if err == nil {
				conn.CloseWithError(0, "")
			}
			dialed <- err
		}()
		if dispatch := env.nextDispatch(); dispatch.nameClass != c.nameClass {
			t.Fatalf("%s: dispatch = %+v", c.what, dispatch)
		}
		source.release()
		select {
		case err := <-dialed:
			if err == nil {
				t.Fatalf("%s: the handshake completed", c.what)
			}
		case <-time.After(testFrontProgressTimeout):
			t.Fatalf("%s: the dial never ended", c.what)
		}
		want := testFrontConnectionResult{portKind: frontPortQuic, result: frontQuicResultHandshakeFailed, reason: c.reason}
		if result := nextTestFrontConnectionResult(t, results); result != want {
			t.Fatalf("%s was counted as %+v, want %+v", c.what, result, want)
		}
	}
}

// A client socket that sends only its first datagram, its Initial with the
// whole ClientHello, and drops every later one: a client gone silent.
type testFrontFirstDatagramSource struct {
	net.PacketConn
	written atomic.Bool
}

// Sends the first datagram and drops the rest.
func (self *testFrontFirstDatagramSource) WriteTo(p []byte, addr net.Addr) (int, error) {
	if self.written.Swap(true) {
		return len(p), nil
	}
	return self.PacketConn.WriteTo(p, addr)
}

// An accepted connection whose client goes silent after its ClientHello is
// counted as an idle timeout: with nothing more from the client, quic-go's
// handshake idle timeout passes, before its handshake timeout, which is
// twice as long from the connection's start.
func TestFrontQuicCountsAClientGoneSilentAsAnIdleTimeout(t *testing.T) {
	var results chan testFrontConnectionResult
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		results = observeTestFrontConnectionResults(env)
		env.connectQuic.handshakeIdleTimeout = time.Second
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	source := &testFrontFirstDatagramSource{
		PacketConn: env.clientPacketConn(address),
	}
	tlsConfig := env.clientTlsConfig(testFrontConnectName, nil)
	// a ClientHello that fits the first datagram: no ML-KEM key share
	tlsConfig.CurvePreferences = []tls.CurveID{tls.X25519}
	dialed := make(chan struct{})
	go func() {
		defer close(dialed)
		// the client completes its side, and its Finished never arrives
		conn, err := env.dialQuicFrom(source, address, frontPortQuic, tlsConfig)
		if err == nil {
			conn.CloseWithError(0, "")
		}
	}()
	if dispatch := env.nextDispatch(); dispatch.nameClass != nginx.NameClassConnect {
		t.Fatalf("dispatch = %+v", dispatch)
	}
	want := testFrontConnectionResult{portKind: frontPortQuic, result: frontQuicResultHandshakeFailed, reason: frontQuicHandshakeReasonIdleTimeout}
	if result := nextTestFrontConnectionResult(t, results); result != want {
		t.Fatalf("a client gone silent was counted as %+v, want %+v", result, want)
	}
	testFrontWait(t, dialed, "the silent client's dial")
}

// An accepted connection whose handshake is under way when the front closes
// is counted as canceled: the front stopped waiting for it.
func TestFrontQuicCountsAHandshakeTheFrontStoppedAsCanceled(t *testing.T) {
	var results chan testFrontConnectionResult
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		results = observeTestFrontConnectionResults(env)
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	// the client's dial ends with the test's cleanup, which closes its
	// transport
	source := newTestFrontHeldSource(env.clientPacketConn(address))
	go func() {
		conn, err := env.dialQuicFrom(source, address, frontPortQuic, env.clientTlsConfig(testFrontConnectName, nil))
		if err == nil {
			conn.CloseWithError(0, "")
		}
	}()
	if dispatch := env.nextDispatch(); dispatch.nameClass != nginx.NameClassConnect {
		t.Fatalf("dispatch = %+v", dispatch)
	}
	env.frontServer.close()
	want := testFrontConnectionResult{portKind: frontPortQuic, result: frontQuicResultHandshakeFailed, reason: frontQuicHandshakeReasonCanceled}
	if result := nextTestFrontConnectionResult(t, results); result != want {
		t.Fatalf("a handshake the front stopped was counted as %+v, want %+v", result, want)
	}
}

// How the front's refusal of a name it doesn't serve surfaces through
// quic-go: crypto/tls fails the ClientHello at GetConfigForClient, and quic-go
// closes the connection with a local CRYPTO_ERROR for internal_error, 0x150,
// whose error wraps nginx.ErrTlsNoCertificate, which the QUIC handshake
// reasons read as unknown_name. The connection is never accepted: quic-go
// accepts one once it has read the client's transport parameters, which come
// after GetConfigForClient, so the dispatch counts nothing, and the nginx
// front's ClientHello count is where such names show.
func TestFrontQuicNoCertificateSurfacesAsUnknownNameBeforeAnyAccept(t *testing.T) {
	connContexts := make(chan context.Context, 16)
	env := newTestNginxFrontEnv(t, func(env *testFrontEnv) {
		env.connectQuic.connContexts = connContexts
	}, http.NotFoundHandler())
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	for _, serverName := range []string{testFrontUnknownName, "alt.service.example", ""} {
		tlsConfig := env.clientTlsConfig(serverName, []string{http3.NextProtoH3})
		// nothing could verify a name the front has no certificate for
		tlsConfig.InsecureSkipVerify = true
		if conn, err := env.dialQuicFrom(env.clientPacketConn(address), address, frontPortQuic, tlsConfig); err == nil {
			conn.CloseWithError(0, "")
			t.Fatalf("%q: the handshake completed", serverName)
		}
		connCtx := nextTestFrontObserved(t, connContexts, fmt.Sprintf("the connection of %q", serverName))
		testFrontWait(t, connCtx.Done(), fmt.Sprintf("the close of %q's connection", serverName))
		cause := context.Cause(connCtx)
		var transportErr *quic.TransportError
		// CRYPTO_ERROR for internal_error (80)
		if !errors.As(cause, &transportErr) || transportErr.Remote || transportErr.ErrorCode != 0x150 {
			t.Fatalf("%q: the connection closed with %v (%T), want the front's local CRYPTO_ERROR 0x150", serverName, cause, cause)
		}
		if !errors.Is(cause, nginx.ErrTlsNoCertificate) {
			t.Fatalf("%q: the close cause %v doesn't wrap the front's refusal", serverName, cause)
		}
		if reason := frontQuicHandshakeReason(cause); reason != frontQuicHandshakeReasonUnknownName {
			t.Fatalf("%q: the close cause has reason %s, want %s", serverName, reason, frontQuicHandshakeReasonUnknownName)
		}
	}
	select {
	case dispatch := <-env.dispatched:
		t.Fatalf("a refused name was dispatched: %+v", dispatch)
	default:
	}
}

// The value of one series of a counter in the default registry, where the
// nginx front's metrics are, named by all its labels; zero before the series
// exists.
func testFrontDefaultCounterValue(t testing.TB, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			labelPairs := metric.GetLabel()
			matched := len(labelPairs) == len(labels)
			for _, labelPair := range labelPairs {
				if value, ok := labels[labelPair.GetName()]; !ok || value != labelPair.GetValue() {
					matched = false
				}
			}
			if matched {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// The nginx front counts every QUIC ClientHello once, on the quic and whodis
// ports, by its port kind, the class of its name and whether it offered h3,
// never by the name: the retired alt's names on any domain as alt, another
// name the front doesn't serve as unknown, a missing or invalid name as none,
// and a served name by its class, a served wildcard over an alt label
// included. Only a served name completes its handshake.
func TestFrontQuicCountsClientHellosByNameClass(t *testing.T) {
	env := newTestNginxFrontEnv(t, nil, http.NotFoundHandler())
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	h3 := []string{http3.NextProtoH3}
	cases := []struct {
		portKind   string
		serverName string
		nextProtos []string
		nameClass  string
		served     bool
	}{
		{portKind: frontPortQuic, serverName: "alt.service.example", nextProtos: h3, nameClass: "alt"},
		{portKind: frontPortQuic, serverName: "alt-v4.other.example", nextProtos: nil, nameClass: "alt"},
		{portKind: frontPortWhodis, serverName: "alt-v6.service.example", nextProtos: h3, nameClass: "alt"},
		{portKind: frontPortWhodis, serverName: testFrontUnknownName, nextProtos: nil, nameClass: "unknown"},
		// no name: the client sends no SNI for the address it dials
		{portKind: frontPortQuic, serverName: "", nextProtos: h3, nameClass: "none"},
		{portKind: frontPortWhodis, serverName: "invalid..name.example", nextProtos: nil, nameClass: "none"},
		{portKind: frontPortQuic, serverName: testFrontConnectName, nextProtos: nil, nameClass: "connect", served: true},
		{portKind: frontPortWhodis, serverName: "alt.connect.service.example", nextProtos: nil, nameClass: "connect", served: true},
		{portKind: frontPortQuic, serverName: testFrontApiName, nextProtos: h3, nameClass: "api", served: true},
		{portKind: frontPortWhodis, serverName: testFrontForwardedName, nextProtos: h3, nameClass: "forwarded", served: true},
	}
	for _, c := range cases {
		labels := map[string]string{
			"port":       c.portKind,
			"name_class": c.nameClass,
			"h3_offered": strconv.FormatBool(slices.Contains(c.nextProtos, http3.NextProtoH3)),
		}
		what := fmt.Sprintf("%s %q offering %v", c.portKind, c.serverName, c.nextProtos)
		before := testFrontDefaultCounterValue(t, "urnetwork_front_quic_client_hellos_total", labels)
		tlsConfig := env.clientTlsConfig(c.serverName, c.nextProtos)
		tlsConfig.InsecureSkipVerify = !c.served
		conn, err := env.dialQuicFrom(env.clientPacketConn(address), address, c.portKind, tlsConfig)
		if c.served {
			// a forwarded name's 0x1000 can end the connection as the dial
			// returns; any served name is dispatched
			if err == nil {
				conn.CloseWithError(0, "")
			}
			env.nextDispatch()
		} else if err == nil {
			conn.CloseWithError(0, "")
			t.Fatalf("%s: the handshake completed", what)
		}
		if after := testFrontDefaultCounterValue(t, "urnetwork_front_quic_client_hellos_total", labels); after != before+1 {
			t.Fatalf("%s counted %v times under %v", what, after-before, labels)
		}
	}
}
