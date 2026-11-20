package nginx

// QUIC TLS config tests, over real QUIC handshakes on loopback through the
// vendored quic-go, as connect's public sockets serve them: alt's ALPN rule,
// which names get a certificate, and session resumption across sockets that
// share the base config (D6c).

import (
	"crypto/tls"
	"errors"
	"net"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The protocols advertised to a QUIC client are its offer intersected with
// h3, or none, alt's table (alt/alt_test.go:984-1005).
func TestQuicNegotiableNextProtos(t *testing.T) {
	cases := []struct {
		offered []string
		want    []string
	}{
		{offered: nil, want: nil},
		{offered: []string{}, want: nil},
		{offered: []string{"h3"}, want: []string{"h3"}},
		{offered: []string{"h2", "h3"}, want: []string{"h3"}},
		{offered: []string{"h2", "http/1.1"}, want: nil},
	}
	for _, c := range cases {
		got := quicNegotiableNextProtos(c.offered)
		if !slices.Equal(got, c.want) || (got == nil) != (c.want == nil) {
			t.Errorf("quicNegotiableNextProtos(%v) = %#v, want %#v", c.offered, got, c.want)
		}
	}
}

// A client offering h3 negotiates it, and raw-QUIC connect, which offers no
// protocol, still completes its handshake: a QUIC server that advertised h3
// to it would refuse it (RFC 9001). A client offering only other protocols
// is advertised none.
func TestQuicTlsConfigNegotiatesTheOfferIntersectedWithH3(t *testing.T) {
	vault := newTestTlsVault(t)
	_, quicTlsConfig := vault.newTlsConfigs(t, DefaultTlsSettings())
	listener := listenTestQuic(t, quicTlsConfig)
	cases := []struct {
		serverName    string
		offered       []string
		wantNextProto string
	}{
		{serverName: "test-api.service.example", offered: []string{"h3"}, wantNextProto: "h3"},
		{serverName: "test-api.other.example", offered: []string{"h2", "h3"}, wantNextProto: "h3"},
		{serverName: "test-connect.service.example", offered: nil, wantNextProto: ""},
		{serverName: "g1.connect.service.example", offered: nil, wantNextProto: ""},
		{serverName: "test-connect.service.example", offered: []string{"h3"}, wantNextProto: "h3"},
	}
	for _, c := range cases {
		state, err := listener.exchange(t, &tls.Config{
			ServerName: c.serverName,
			RootCAs:    vault.authority.pool(),
			NextProtos: c.offered,
		}, nil)
		if err != nil {
			t.Errorf("sni %q offering %v: %v", c.serverName, c.offered, err)
			continue
		}
		if state.TLS.NegotiatedProtocol != c.wantNextProto {
			t.Errorf("sni %q offering %v negotiated %q, want %q", c.serverName, c.offered, state.TLS.NegotiatedProtocol, c.wantNextProto)
		}
	}

	config, err := quicTlsConfig.GetConfigForClient(&tls.ClientHelloInfo{
		ServerName:      "test-api.service.example",
		SupportedProtos: []string{"h2", "http/1.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.NextProtos != nil {
		t.Fatalf("a client offering h2 and http/1.1 is advertised %v, want none", config.NextProtos)
	}
}

// Every served name completes its QUIC handshake with its certificate, so
// connect can refuse the ones it doesn't serve on QUIC with 0x1000. A
// missing, invalid or unknown SNI gets no certificate, as in alt, so its
// handshake fails.
func TestQuicTlsConfigServesEveryServedNameAndNoOther(t *testing.T) {
	vault := newTestTlsVault(t)
	_, quicTlsConfig := vault.newTlsConfigs(t, DefaultTlsSettings())
	listener := listenTestQuic(t, quicTlsConfig)
	cases := []struct {
		serverName  string
		wantDirName string
	}{
		{serverName: "test-api.service.example", wantDirName: "star.service.example"},
		{serverName: "a.g1.connect.service.example", wantDirName: "star.connect.service.example"},
		{serverName: "TEST-API.other.example", wantDirName: "star.other.example"},
		{serverName: "test-mcp.service.example", wantDirName: "star.service.example"},
		{serverName: "old-api.service.example", wantDirName: "star.service.example"},
		{serverName: "test-lb.service.example", wantDirName: "test-lb.service.example"},
		// the dialer sends no SNI for an address
		{serverName: ""},
		{serverName: "unknown.example"},
		{serverName: "x.api.service.example"},
		{serverName: "test-api..service.example"},
	}
	for _, c := range cases {
		state, err := listener.exchange(t, &tls.Config{
			ServerName:         c.serverName,
			InsecureSkipVerify: true,
			NextProtos:         []string{"h3"},
		}, nil)
		if c.wantDirName == "" {
			if err == nil {
				t.Errorf("sni %q completed a handshake with the leaf for %v, want none", c.serverName, state.TLS.PeerCertificates[0].DNSNames)
			}
			continue
		}
		if err != nil {
			t.Errorf("sni %q: %v", c.serverName, err)
			continue
		}
		if !state.TLS.PeerCertificates[0].Equal(vault.dirPairs[c.wantDirName].leaf) {
			t.Errorf("sni %q got the leaf for %v, want %s's", c.serverName, state.TLS.PeerCertificates[0].DNSNames, c.wantDirName)
		}
	}
}

// A QUIC session from one socket resumes on another socket serving the same
// base config, as on connect's reuseport sockets: quic-go uses the listener's
// config uncloned, and the per-name configs leave the ticket keys to it
// (D6c). A config built apart doesn't resume it.
func TestQuicTlsConfigResumesSessionsAcrossSocketsSharingTheBase(t *testing.T) {
	vault := newTestTlsVault(t)
	_, quicTlsConfig := vault.newTlsConfigs(t, DefaultTlsSettings())
	firstListener := listenTestQuic(t, quicTlsConfig)
	secondListener := listenTestQuic(t, quicTlsConfig)
	sessionCache := newTestNotifyingSessionCache()
	clientConfig := &tls.Config{
		ServerName:         "test-api.service.example",
		RootCAs:            vault.authority.pool(),
		NextProtos:         []string{"h3"},
		ClientSessionCache: sessionCache,
	}

	state, err := firstListener.exchange(t, clientConfig, sessionCache)
	if err != nil {
		t.Fatal(err)
	}
	if state.TLS.DidResume {
		t.Fatal("the first connection resumed")
	}

	state, err = secondListener.exchange(t, clientConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !state.TLS.DidResume {
		t.Fatal("the session didn't resume on a second socket of the same base")
	}

	_, otherQuicTlsConfig := vault.newTlsConfigs(t, DefaultTlsSettings())
	state, err = listenTestQuic(t, otherQuicTlsConfig).exchange(t, clientConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.TLS.DidResume {
		t.Fatal("the session resumed on a config built apart")
	}
}

// A connection whose only use is its local address, as the ClientHelloInfo
// quic-go builds holds the socket group's.
type testLocalAddrConn struct {
	net.Conn
	localAddr net.Addr
}

// net.Conn
func (self *testLocalAddrConn) LocalAddr() net.Addr {
	return self.localAddr
}

// The QUIC config counts each ClientHello once, never by its name: by the
// port kind its local port is among the route table's listen ports, by
// whether it offered h3, and by its name's class. A served name counts under
// its class, a served wildcard over an alt label included; the retired alt's
// alt, alt-v4 and alt-v6 on any domain as alt; another name the front
// doesn't serve as unknown, a lone alt label among them; and no name, or one
// nginx's rule refuses, as none. Only a served name gets a config.
func TestQuicTlsConfigCountsClientHellosByPortNameClassAndH3(t *testing.T) {
	vault := newTestTlsVault(t)
	metrics := newFrontMetrics(prometheus.NewRegistry())
	_, quicTlsConfig := vault.newTlsConfigsWithMetrics(t, DefaultTlsSettings(), metrics)
	listen := vault.routeTable.Listen
	// a ClientHello's connection, with the local address of a socket bound
	// to port
	udpConn := func(port int) net.Conn {
		return &testLocalAddrConn{localAddr: &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: port}}
	}
	h3 := []string{tlsNextProtoH3}
	cases := []struct {
		serverName string
		offered    []string
		conn       net.Conn
		port       string
		nameClass  string
		h3Offered  string
	}{
		{serverName: "test-api.service.example", offered: h3, conn: udpConn(listen.QuicPort), port: tlsQuicPortQuic, nameClass: "api", h3Offered: "true"},
		{serverName: "test-api.service.example", offered: []string{"h2", "h3"}, conn: udpConn(listen.WhodisPort), port: tlsQuicPortWhodis, nameClass: "api", h3Offered: "true"},
		{serverName: "a.g1.connect.service.example", offered: nil, conn: udpConn(listen.QuicPort), port: tlsQuicPortQuic, nameClass: "connect", h3Offered: "false"},
		{serverName: "alt.connect.service.example", offered: nil, conn: udpConn(listen.WhodisPort), port: tlsQuicPortWhodis, nameClass: "connect", h3Offered: "false"},
		{serverName: "test-mcp.service.example", offered: []string{"h2"}, conn: udpConn(listen.QuicPort), port: tlsQuicPortQuic, nameClass: "forwarded", h3Offered: "false"},
		{serverName: "test-lb.service.example", offered: h3, conn: udpConn(listen.QuicPort), port: tlsQuicPortQuic, nameClass: "lb", h3Offered: "true"},
		{serverName: "old-api.service.example", offered: h3, conn: udpConn(listen.QuicPort), port: tlsQuicPortQuic, nameClass: "redirect", h3Offered: "true"},
		{serverName: "alt.service.example", offered: h3, conn: udpConn(listen.QuicPort), port: tlsQuicPortQuic, nameClass: tlsQuicNameClassAlt, h3Offered: "true"},
		{serverName: "ALT-V4.other.example", offered: nil, conn: udpConn(listen.WhodisPort), port: tlsQuicPortWhodis, nameClass: tlsQuicNameClassAlt, h3Offered: "false"},
		{serverName: "alt-v6.unknown.example", offered: h3, conn: udpConn(listen.WhodisPort), port: tlsQuicPortWhodis, nameClass: tlsQuicNameClassAlt, h3Offered: "true"},
		{serverName: "alt", offered: h3, conn: udpConn(listen.QuicPort), port: tlsQuicPortQuic, nameClass: tlsQuicNameClassUnknown, h3Offered: "true"},
		{serverName: "alternative.example", offered: nil, conn: udpConn(listen.QuicPort), port: tlsQuicPortQuic, nameClass: tlsQuicNameClassUnknown, h3Offered: "false"},
		{serverName: "x.alt.example", offered: nil, conn: udpConn(listen.QuicPort), port: tlsQuicPortQuic, nameClass: tlsQuicNameClassUnknown, h3Offered: "false"},
		{serverName: "unknown.example", offered: h3, conn: udpConn(listen.HttpPort), port: tlsQuicPortOther, nameClass: tlsQuicNameClassUnknown, h3Offered: "true"},
		{serverName: "", offered: h3, conn: udpConn(listen.QuicPort), port: tlsQuicPortQuic, nameClass: tlsQuicNameClassNone, h3Offered: "true"},
		{serverName: "test-api..service.example", offered: nil, conn: udpConn(listen.WhodisPort), port: tlsQuicPortWhodis, nameClass: tlsQuicNameClassNone, h3Offered: "false"},
		// no local address to read the port from
		{serverName: "test-api.service.example", offered: h3, conn: nil, port: tlsQuicPortOther, nameClass: "api", h3Offered: "true"},
		{serverName: "test-api.service.example", offered: h3, conn: &testLocalAddrConn{localAddr: &net.TCPAddr{Port: listen.QuicPort}}, port: tlsQuicPortOther, nameClass: "api", h3Offered: "true"},
	}
	servedClasses := map[string]bool{"api": true, "connect": true, "forwarded": true, "lb": true, "redirect": true}
	wantCounts := map[[3]string]float64{}
	for _, c := range cases {
		config, err := quicTlsConfig.GetConfigForClient(&tls.ClientHelloInfo{
			ServerName:      c.serverName,
			SupportedProtos: c.offered,
			Conn:            c.conn,
		})
		if servedClasses[c.nameClass] {
			if err != nil || config == nil {
				t.Errorf("sni %q: %v, want its config", c.serverName, err)
			}
		} else if !errors.Is(err, ErrTlsNoCertificate) {
			t.Errorf("sni %q: %v, want ErrTlsNoCertificate", c.serverName, err)
		}
		wantCounts[[3]string{c.port, c.nameClass, c.h3Offered}] += 1
	}
	for labels, wantCount := range wantCounts {
		if count := testutil.ToFloat64(metrics.quicClientHellos.WithLabelValues(labels[0], labels[1], labels[2])); count != wantCount {
			t.Errorf("%v counted %v ClientHellos, want %v", labels, count, wantCount)
		}
	}
	if seriesCount := testutil.CollectAndCount(metrics.quicClientHellos); seriesCount != len(wantCounts) {
		t.Errorf("%d series, want %d", seriesCount, len(wantCounts))
	}
}

// A real QUIC handshake is counted once, as quic-go's ClientHelloInfo gives
// it: the local port of the socket that read it, here one the route table
// doesn't list, so other. A handshake that fails is counted all the same.
func TestQuicTlsConfigCountsEachRealClientHelloOnce(t *testing.T) {
	vault := newTestTlsVault(t)
	metrics := newFrontMetrics(prometheus.NewRegistry())
	_, quicTlsConfig := vault.newTlsConfigsWithMetrics(t, DefaultTlsSettings(), metrics)
	listener := listenTestQuic(t, quicTlsConfig)
	if _, err := listener.exchange(t, &tls.Config{
		ServerName: "test-api.service.example",
		RootCAs:    vault.authority.pool(),
		NextProtos: []string{"h3"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := listener.exchange(t, &tls.Config{
		ServerName:         "alt.service.example",
		InsecureSkipVerify: true,
	}, nil); err == nil {
		t.Fatal("an alt name completed its handshake")
	}
	if count := testutil.ToFloat64(metrics.quicClientHellos.WithLabelValues(tlsQuicPortOther, "api", "true")); count != 1 {
		t.Fatalf("the api handshake counted %v times", count)
	}
	if count := testutil.ToFloat64(metrics.quicClientHellos.WithLabelValues(tlsQuicPortOther, tlsQuicNameClassAlt, "false")); count != 1 {
		t.Fatalf("the alt handshake counted %v times", count)
	}
	if seriesCount := testutil.CollectAndCount(metrics.quicClientHellos); seriesCount != 2 {
		t.Fatalf("%d series, want 2", seriesCount)
	}
}
