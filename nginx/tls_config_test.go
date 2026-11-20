package nginx

// TCP TLS config tests, over real handshakes on loopback: the certificate and
// ALPN each SNI gets, the default server, the pinned suites and groups, and
// session resumption across sockets that share the base config (D6c). Each
// pin is shown to matter by a control on Go's defaults, where the same client
// gets through.

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A served SNI gets its own certificate, and ALPN by class: http/1.1 for
// connect names, nginx's h2 first for the rest. A missing, invalid or unknown
// SNI gets the default server, the first lb name's certificate with h2.
func TestTlsConfigPicksTheCertificateAndAlpnBySni(t *testing.T) {
	vault := newTestTlsVault(t)
	tlsConfig, _ := vault.newTlsConfigs(t, DefaultTlsSettings())
	listener := listenTestTls(t, tlsConfig)
	cases := []struct {
		serverName    string
		wantDirName   string
		wantNextProto string
	}{
		{serverName: "test-connect.service.example", wantDirName: "star.service.example", wantNextProto: "http/1.1"},
		{serverName: "connect.service.example", wantDirName: "star.service.example", wantNextProto: "http/1.1"},
		{serverName: "test-connect.other.example", wantDirName: "star.other.example", wantNextProto: "http/1.1"},
		// under the connect wildcard, by one label or several
		{serverName: "g1.connect.service.example", wantDirName: "star.connect.service.example", wantNextProto: "http/1.1"},
		{serverName: "a.g1.connect.service.example", wantDirName: "star.connect.service.example", wantNextProto: "http/1.1"},
		// normalized as nginx's SNI is
		{serverName: "TEST-CONNECT.Service.Example", wantDirName: "star.service.example", wantNextProto: "http/1.1"},
		{serverName: "test-api.service.example", wantDirName: "star.service.example", wantNextProto: "h2"},
		{serverName: "test-api.other.example", wantDirName: "star.other.example", wantNextProto: "h2"},
		{serverName: "test-mcp.service.example", wantDirName: "star.service.example", wantNextProto: "h2"},
		{serverName: "old-api.service.example", wantDirName: "star.service.example", wantNextProto: "h2"},
		{serverName: "test-lb.other.example", wantDirName: "star.other.example", wantNextProto: "h2"},
		{serverName: "test-lb.service.example", wantDirName: "test-lb.service.example", wantNextProto: "h2"},
		// the default server
		{serverName: "unknown.example", wantDirName: "test-lb.service.example", wantNextProto: "h2"},
		{serverName: "x.api.service.example", wantDirName: "test-lb.service.example", wantNextProto: "h2"},
		{serverName: "", wantDirName: "test-lb.service.example", wantNextProto: "h2"},
		{serverName: "test-api..service.example", wantDirName: "test-lb.service.example", wantNextProto: "h2"},
		{serverName: "test api.service.example", wantDirName: "test-lb.service.example", wantNextProto: "h2"},
	}
	for _, c := range cases {
		state, err := listener.exchange(t, &tls.Config{
			ServerName:         c.serverName,
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2", "http/1.1"},
		})
		if err != nil {
			t.Errorf("sni %q: %v", c.serverName, err)
			continue
		}
		if !state.PeerCertificates[0].Equal(vault.dirPairs[c.wantDirName].leaf) {
			t.Errorf("sni %q got the leaf for %v, want %s's", c.serverName, state.PeerCertificates[0].DNSNames, c.wantDirName)
		}
		if state.NegotiatedProtocol != c.wantNextProto {
			t.Errorf("sni %q negotiated %q, want %q", c.serverName, state.NegotiatedProtocol, c.wantNextProto)
		}
	}
}

// A client offering protocols, none of which its name serves, is refused
// with an alert, as nginx refuses it; connect names serve no h2. A client
// offering none gets HTTP/1.1.
func TestTlsConfigRefusesAnAlpnOfferWithNoOverlap(t *testing.T) {
	vault := newTestTlsVault(t)
	tlsConfig, _ := vault.newTlsConfigs(t, DefaultTlsSettings())
	listener := listenTestTls(t, tlsConfig)
	cases := []struct {
		serverName    string
		offered       []string
		wantOk        bool
		wantNextProto string
	}{
		{serverName: "test-connect.service.example", offered: []string{"h2"}, wantOk: false},
		{serverName: "test-connect.service.example", offered: []string{"spdy/3.1"}, wantOk: false},
		{serverName: "test-api.service.example", offered: []string{"spdy/3.1"}, wantOk: false},
		{serverName: "unknown.example", offered: []string{"spdy/3.1"}, wantOk: false},
		{serverName: "test-api.service.example", offered: []string{"h2"}, wantOk: true, wantNextProto: "h2"},
		{serverName: "test-api.service.example", offered: []string{"http/1.1"}, wantOk: true, wantNextProto: "http/1.1"},
		// nginx also selects these, the front only http/1.1 beside them
		// (LBALT-IMPL.md §4)
		{serverName: "test-api.service.example", offered: []string{"http/1.0"}, wantOk: false},
		{serverName: "test-api.service.example", offered: []string{"http/0.9"}, wantOk: false},
		{serverName: "test-api.service.example", offered: []string{"http/1.0", "http/1.1"}, wantOk: true, wantNextProto: "http/1.1"},
		{serverName: "test-connect.service.example", offered: []string{"http/1.1", "h2"}, wantOk: true, wantNextProto: "http/1.1"},
		{serverName: "test-connect.service.example", offered: nil, wantOk: true, wantNextProto: ""},
		{serverName: "test-api.service.example", offered: nil, wantOk: true, wantNextProto: ""},
	}
	for _, c := range cases {
		// unverified, so only the ALPN can refuse
		state, err := listener.exchange(t, &tls.Config{
			ServerName:         c.serverName,
			InsecureSkipVerify: true,
			NextProtos:         c.offered,
		})
		if !c.wantOk {
			if err == nil {
				t.Errorf("sni %q offering %v negotiated %q, want a refusal", c.serverName, c.offered, state.NegotiatedProtocol)
			}
			continue
		}
		if err != nil {
			t.Errorf("sni %q offering %v: %v", c.serverName, c.offered, err)
			continue
		}
		if state.NegotiatedProtocol != c.wantNextProto {
			t.Errorf("sni %q offering %v negotiated %q, want %q", c.serverName, c.offered, state.NegotiatedProtocol, c.wantNextProto)
		}
	}
}

// An SNI ending in a dot fails the handshake with decode_error: crypto/tls
// refuses the ClientHello before GetConfigForClient runs, where nginx drops
// the dot and serves the name (LBALT-IMPL.md §4; RFC 6066 forbids the dot).
// Go's client strips the dot, so the ClientHello is a Go client's for a name
// of the same length, the name replaced in its bytes. The same name with a
// letter for the dot, unknown, gets the default server's ServerHello.
func TestTlsConfigRefusesAnSniEndingInADot(t *testing.T) {
	const (
		tlsRecordTypeAlert     = 21
		tlsRecordTypeHandshake = 22
		tlsAlertDecodeError    = 50
	)
	vault := newTestTlsVault(t)
	tlsConfig, _ := vault.newTlsConfigs(t, DefaultTlsSettings())
	placeholder := "test-api.service.exampleZ"
	clientHello := func() []byte {
		clientSide, serverSide := net.Pipe()
		defer clientSide.Close()
		defer serverSide.Close()
		go tls.Client(clientSide, &tls.Config{
			ServerName:         placeholder,
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2", "http/1.1"},
		}).Handshake()
		serverSide.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
		header := make([]byte, 5)
		if _, err := io.ReadFull(serverSide, header); err != nil {
			t.Fatal(err)
		}
		body := make([]byte, int(header[3])*256+int(header[4]))
		if _, err := io.ReadFull(serverSide, body); err != nil {
			t.Fatal(err)
		}
		return append(header, body...)
	}()
	if bytes.Count(clientHello, []byte(placeholder)) != 1 {
		t.Fatalf("the ClientHello names the placeholder %d times", bytes.Count(clientHello, []byte(placeholder)))
	}
	for _, c := range []struct {
		serverName     string
		wantRecordType byte
		wantAlert      byte
	}{
		{serverName: placeholder, wantRecordType: tlsRecordTypeHandshake},
		{serverName: "test-api.service.example.", wantRecordType: tlsRecordTypeAlert, wantAlert: tlsAlertDecodeError},
	} {
		listener := listenTestTls(t, tlsConfig)
		go func() {
			listener.tcpListener.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
			conn, err := listener.tlsListener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
			conn.(*tls.Conn).Handshake()
		}()
		conn, err := net.Dial("tcp", listener.tcpListener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
		if _, err := conn.Write(bytes.Replace(clientHello, []byte(placeholder), []byte(c.serverName), 1)); err != nil {
			t.Fatal(err)
		}
		record := make([]byte, 7)
		if _, err := io.ReadFull(conn, record[:5]); err != nil {
			t.Fatalf("sni %q: no answer: %v", c.serverName, err)
		}
		if record[0] == tlsRecordTypeAlert {
			io.ReadFull(conn, record[5:])
		}
		conn.Close()
		if record[0] != c.wantRecordType || (c.wantRecordType == tlsRecordTypeAlert && record[6] != c.wantAlert) {
			t.Errorf("sni %q: record type %d, alert %d; want %d, %d", c.serverName, record[0], record[6], c.wantRecordType, c.wantAlert)
		}
	}
}

// A TLS 1.2 client offering only CBC suites is refused, as nginx's
// EECDH+AESGCM refuses it, while one offering AES-GCM gets through. Go's
// default suites would admit the CBC client.
func TestTlsConfigRefusesATls12ClientOfferingOnlyCbcSuites(t *testing.T) {
	vault := newTestTlsVault(t)
	newClientConfig := func(cipherSuites ...uint16) *tls.Config {
		return &tls.Config{
			ServerName:   "test-api.service.example",
			RootCAs:      vault.authority.pool(),
			MaxVersion:   tls.VersionTLS12,
			CipherSuites: cipherSuites,
		}
	}
	cbcClientConfig := newClientConfig(tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA, tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA)

	tlsConfig, _ := vault.newTlsConfigs(t, DefaultTlsSettings())
	listener := listenTestTls(t, tlsConfig)
	if state, err := listener.exchange(t, cbcClientConfig); err == nil {
		t.Fatalf("a CBC-only TLS 1.2 client negotiated %s", tls.CipherSuiteName(state.CipherSuite))
	}
	for _, cipherSuite := range []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384} {
		state, err := listener.exchange(t, newClientConfig(cipherSuite))
		if err != nil {
			t.Fatalf("%s: %v", tls.CipherSuiteName(cipherSuite), err)
		}
		if state.Version != tls.VersionTLS12 || state.CipherSuite != cipherSuite {
			t.Fatalf("negotiated %x with %s, want TLS 1.2 with %s", state.Version, tls.CipherSuiteName(state.CipherSuite), tls.CipherSuiteName(cipherSuite))
		}
	}

	unpinnedSettings := DefaultTlsSettings()
	unpinnedSettings.CipherSuites = nil
	unpinnedTlsConfig, _ := vault.newTlsConfigs(t, unpinnedSettings)
	if _, err := listenTestTls(t, unpinnedTlsConfig).exchange(t, cbcClientConfig); err != nil {
		t.Fatalf("Go's default suites refused the CBC client too, so the test shows nothing: %v", err)
	}
}

// Key exchange uses only X25519MLKEM768 and nginx's X25519, P-256 and P-384,
// the hybrid first. Go's default groups would admit P-521.
func TestTlsConfigUsesOnlyThePinnedGroups(t *testing.T) {
	vault := newTestTlsVault(t)
	newClientConfig := func(curveIds ...tls.CurveID) *tls.Config {
		return &tls.Config{
			ServerName:       "test-api.service.example",
			RootCAs:          vault.authority.pool(),
			CurvePreferences: curveIds,
		}
	}
	p521ClientConfig := newClientConfig(tls.CurveP521)

	tlsConfig, _ := vault.newTlsConfigs(t, DefaultTlsSettings())
	listener := listenTestTls(t, tlsConfig)
	if state, err := listener.exchange(t, p521ClientConfig); err == nil {
		t.Fatalf("a P-521-only client negotiated %s", state.CurveID)
	}
	cases := []struct {
		clientConfig *tls.Config
		wantCurveId  tls.CurveID
	}{
		// Go's own client offers the hybrid, which the front prefers
		{clientConfig: newClientConfig(), wantCurveId: tls.X25519MLKEM768},
		{clientConfig: newClientConfig(tls.X25519), wantCurveId: tls.X25519},
		{clientConfig: newClientConfig(tls.CurveP256), wantCurveId: tls.CurveP256},
		{clientConfig: newClientConfig(tls.CurveP384), wantCurveId: tls.CurveP384},
		{clientConfig: newClientConfig(tls.CurveP384, tls.X25519), wantCurveId: tls.X25519},
	}
	for _, c := range cases {
		state, err := listener.exchange(t, c.clientConfig)
		if err != nil {
			t.Fatalf("client offering %v: %v", c.clientConfig.CurvePreferences, err)
		}
		if state.CurveID != c.wantCurveId {
			t.Errorf("client offering %v negotiated %s, want %s", c.clientConfig.CurvePreferences, state.CurveID, c.wantCurveId)
		}
	}

	unpinnedSettings := DefaultTlsSettings()
	unpinnedSettings.CurvePreferences = nil
	unpinnedTlsConfig, _ := vault.newTlsConfigs(t, unpinnedSettings)
	if _, err := listenTestTls(t, unpinnedTlsConfig).exchange(t, p521ClientConfig); err != nil {
		t.Fatalf("Go's default groups refused the P-521 client too, so the test shows nothing: %v", err)
	}
}

// A session from one socket resumes on another socket serving the same base
// config, since every config a handshake gets leaves the ticket keys to the
// base (D6c). A config built apart has keys of its own, as another process
// does, so the session doesn't resume there. TLS 1.3 and TLS 1.2 alike.
func TestTlsConfigResumesSessionsAcrossSocketsSharingTheBase(t *testing.T) {
	vault := newTestTlsVault(t)
	for _, version := range []uint16{tls.VersionTLS13, tls.VersionTLS12} {
		tlsConfig, _ := vault.newTlsConfigs(t, DefaultTlsSettings())
		firstListener := listenTestTls(t, tlsConfig)
		secondListener := listenTestTls(t, tlsConfig)
		sessionCache := newTestNotifyingSessionCache()
		clientConfig := &tls.Config{
			ServerName:         "test-api.service.example",
			RootCAs:            vault.authority.pool(),
			ClientSessionCache: sessionCache,
			MinVersion:         version,
			MaxVersion:         version,
		}

		state, err := firstListener.exchange(t, clientConfig)
		if err != nil {
			t.Fatal(err)
		}
		if state.DidResume {
			t.Fatalf("version %x: the first connection resumed", version)
		}
		sessionCache.awaitSession(t)

		state, err = secondListener.exchange(t, clientConfig)
		if err != nil {
			t.Fatal(err)
		}
		if !state.DidResume {
			t.Fatalf("version %x: the session didn't resume on a second socket of the same base", version)
		}

		otherTlsConfig, _ := vault.newTlsConfigs(t, DefaultTlsSettings())
		state, err = listenTestTls(t, otherTlsConfig).exchange(t, clientConfig)
		if err != nil {
			t.Fatal(err)
		}
		if state.DidResume {
			t.Fatalf("version %x: the session resumed on a config built apart", version)
		}
	}
}

// Every config a handshake can get pins the settings' version, suites and
// groups, as do the bases, each from its own copy; and each is built once,
// so a name gets the same config on every handshake.
func TestNewTlsConfigsPinsTheSettingsInConfigsBuiltOnce(t *testing.T) {
	vault := newTestTlsVault(t)
	settings := DefaultTlsSettings()
	tlsConfig, quicTlsConfig := vault.newTlsConfigs(t, settings)
	wantMinVersion := settings.MinVersion
	wantCipherSuites := slices.Clone(settings.CipherSuites)
	wantCurvePreferences := slices.Clone(settings.CurvePreferences)
	// the configs keep copies, so settings changed later change nothing
	settings.MinVersion = tls.VersionTLS13
	settings.CipherSuites[0] = tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA
	settings.CurvePreferences[0] = tls.CurveP521

	clientHellos := []*tls.ClientHelloInfo{
		{ServerName: "test-api.service.example"},
		{ServerName: "test-connect.service.example"},
		{ServerName: "unknown.example"},
		{ServerName: ""},
	}
	quicClientHellos := []*tls.ClientHelloInfo{
		{ServerName: "test-api.service.example", SupportedProtos: []string{"h3"}},
		{ServerName: "test-connect.service.example"},
	}
	type namedConfig struct {
		description string
		config      *tls.Config
	}
	namedConfigs := []namedConfig{
		{description: "the tcp base", config: tlsConfig},
		{description: "the quic base", config: quicTlsConfig},
	}
	getConfigs := func(baseConfig *tls.Config, clientHellos []*tls.ClientHelloInfo, description string) {
		for _, clientHello := range clientHellos {
			config, err := baseConfig.GetConfigForClient(clientHello)
			if err != nil {
				t.Fatalf("%s sni %q: %v", description, clientHello.ServerName, err)
			}
			if again, _ := baseConfig.GetConfigForClient(clientHello); again != config {
				t.Errorf("%s sni %q got a new config on a second handshake", description, clientHello.ServerName)
			}
			// tickets on, with no keys of its own, so the base's apply
			if config.SessionTicketsDisabled || config.SessionTicketKey != [32]byte{} {
				t.Errorf("%s sni %q got a config that changes the ticket keys", description, clientHello.ServerName)
			}
			namedConfigs = append(namedConfigs, namedConfig{
				description: description + " sni " + clientHello.ServerName,
				config:      config,
			})
		}
	}
	getConfigs(tlsConfig, clientHellos, "tcp")
	getConfigs(quicTlsConfig, quicClientHellos, "quic")
	for _, namedConfig := range namedConfigs {
		config := namedConfig.config
		if config.MinVersion != wantMinVersion {
			t.Errorf("%s: MinVersion %x, want %x", namedConfig.description, config.MinVersion, wantMinVersion)
		}
		if !slices.Equal(config.CipherSuites, wantCipherSuites) {
			t.Errorf("%s: CipherSuites %v, want %v", namedConfig.description, config.CipherSuites, wantCipherSuites)
		}
		if !slices.Equal(config.CurvePreferences, wantCurvePreferences) {
			t.Errorf("%s: CurvePreferences %v, want %v", namedConfig.description, config.CurvePreferences, wantCurvePreferences)
		}
	}
}

// A served name the store has no certificate for, from a store of another
// table, is served as an unknown name: the default server on TCP, nothing on
// QUIC, whose ClientHello count calls it unknown. Without the default
// server's certificate, TCP has nothing either.
func TestNewTlsConfigsServesANameTheStoreLacksAsUnknown(t *testing.T) {
	vault := newTestTlsVault(t)
	certificateStore := vault.load(t)
	// a name under service.example, which no served wildcard covers
	tree := routeFixtureTree(t)
	names := routeTreeValue(t, tree, "services", 0, "names").([]any)
	setRouteTreeValue(t, tree, append(names, "extra-api.service.example"), "services", 0, "names")
	matcherTable, err := ParseRouteTable(encodeRouteTree(t, tree))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := certificateStore.certificate("extra-api.service.example"); ok {
		t.Fatal("the store covers extra-api.service.example, so the test shows nothing")
	}
	metrics := newFrontMetrics(prometheus.NewRegistry())
	tlsConfig, quicTlsConfig := newTlsConfigs(certificateStore, newNameMatcher(matcherTable), matcherTable.Lb.Names[0], matcherTable.Listen, DefaultTlsSettings(), metrics, time.Now)

	config, err := tlsConfig.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "extra-api.service.example"})
	if err != nil {
		t.Fatal(err)
	}
	defaultConfig, err := tlsConfig.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "unknown.example"})
	if err != nil {
		t.Fatal(err)
	}
	if config != defaultConfig {
		t.Fatalf("a name without a certificate didn't get the default server")
	}
	if _, err := quicTlsConfig.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "extra-api.service.example", SupportedProtos: []string{"h3"}}); err == nil {
		t.Fatalf("quic served a name without a certificate")
	}
	if count := testutil.ToFloat64(metrics.quicClientHellos.WithLabelValues(tlsQuicPortOther, tlsQuicNameClassUnknown, "true")); count != 1 {
		t.Fatalf("the ClientHello of a name without a certificate counted %v times as unknown", count)
	}

	noDefaultTlsConfig, _ := newTlsConfigs(certificateStore, newNameMatcher(vault.routeTable), "absent.example", vault.routeTable.Listen, DefaultTlsSettings(), newFrontMetrics(prometheus.NewRegistry()), time.Now)
	if _, err := noDefaultTlsConfig.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "unknown.example"}); err == nil {
		t.Fatalf("an unknown name got a config with no default certificate")
	}
	if _, err := noDefaultTlsConfig.GetConfigForClient(&tls.ClientHelloInfo{ServerName: "test-api.service.example"}); err != nil {
		t.Fatalf("a served name lost its config with the default server: %v", err)
	}
}

// A service that isn't exposed is served as unknown names, even when the
// vault holds certificates for them (LBALT-IMPL.md §7): its names and
// redirect aliases get the default server's certificate on TCP and fail the
// handshake on QUIC, and a name under its wildcard gets the exposed connect
// wildcard's certificate instead.
func TestTlsConfigsServeAnUnexposedServiceAsUnknown(t *testing.T) {
	vault := newTestTlsVault(t)
	for dirName, dnsName := range map[string]string{
		"mcp.hidden.example":              "mcp.hidden.example",
		"old-mcp.hidden.example":          "old-mcp.hidden.example",
		"star.g1.connect.service.example": "*.g1.connect.service.example",
	} {
		pair := vault.authority.issueValid(t, 30*24*time.Hour, dnsName)
		vault.dirPairs[dirName] = pair
		writeTestTlsPair(t, vault.envHome(), testTlsVersion, dirName, pair)
	}
	vault.routeTable = newTestUnexposedRouteTable(t)
	tlsConfig, quicTlsConfig := vault.newTlsConfigs(t, DefaultTlsSettings())
	tlsListener := listenTestTls(t, tlsConfig)
	quicListener := listenTestQuic(t, quicTlsConfig)

	cases := []struct {
		serverName  string
		wantDirName string
		// whether a QUIC handshake completes, with the same certificate
		quicServed bool
	}{
		{serverName: "mcp.hidden.example", wantDirName: "test-lb.service.example", quicServed: false},
		{serverName: "old-mcp.hidden.example", wantDirName: "test-lb.service.example", quicServed: false},
		{serverName: "x.g1.connect.service.example", wantDirName: "star.connect.service.example", quicServed: true},
	}
	for _, c := range cases {
		state, err := tlsListener.exchange(t, &tls.Config{
			ServerName:         c.serverName,
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2", "http/1.1"},
		})
		if err != nil {
			t.Errorf("tcp sni %q: %v", c.serverName, err)
		} else if !state.PeerCertificates[0].Equal(vault.dirPairs[c.wantDirName].leaf) {
			t.Errorf("tcp sni %q got the leaf for %v, want %s's", c.serverName, state.PeerCertificates[0].DNSNames, c.wantDirName)
		}

		quicState, err := quicListener.exchange(t, &tls.Config{
			ServerName:         c.serverName,
			InsecureSkipVerify: true,
			NextProtos:         []string{"h3"},
		}, nil)
		switch {
		case !c.quicServed && err == nil:
			t.Errorf("quic sni %q completed a handshake with the leaf for %v, want none", c.serverName, quicState.TLS.PeerCertificates[0].DNSNames)
		case c.quicServed && err != nil:
			t.Errorf("quic sni %q: %v", c.serverName, err)
		case c.quicServed && !quicState.TLS.PeerCertificates[0].Equal(vault.dirPairs[c.wantDirName].leaf):
			t.Errorf("quic sni %q got the leaf for %v, want %s's", c.serverName, quicState.TLS.PeerCertificates[0].DNSNames, c.wantDirName)
		}
	}
}
