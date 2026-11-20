package nginx

// QUIC TLS config tests, over real QUIC handshakes on loopback through the
// vendored quic-go, as connect's public sockets serve them: alt's ALPN rule,
// which names get a certificate, and session resumption across sockets that
// share the base config (D6c).

import (
	"crypto/tls"
	"slices"
	"testing"
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
