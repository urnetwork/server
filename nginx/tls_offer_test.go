package nginx

// Tests of the ClientHello offer the TCP config reads for the handshake
// counters (LBALTOPT2 R22 (b)): the classification of synthetic ClientHellos,
// GREASE included, and the offers of real ClientHellos, read through the
// handshake's context on a front TLS listener.

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

// GREASE values are the sixteen 0x?A?A with equal bytes.
func TestTlsGrease(t *testing.T) {
	greaseCount := 0
	for value := range 1 << 16 {
		if tlsGrease(uint16(value)) {
			greaseCount += 1
			if value&0xff != 0x0a|value&0xf0 || value>>8 != value&0xff {
				t.Errorf("%#04x is not GREASE", value)
			}
		}
	}
	if greaseCount != 16 {
		t.Errorf("%d GREASE values, want 16", greaseCount)
	}
	for _, value := range []uint16{0x0a0a, 0x1a1a, 0xfafa} {
		if !tlsGrease(value) {
			t.Errorf("%#04x is GREASE", value)
		}
	}
	for _, value := range []uint16{tls.VersionTLS13, tls.VersionTLS12, 0x0a1a, 0x7f1c} {
		if tlsGrease(value) {
			t.Errorf("%#04x is not GREASE", value)
		}
	}
}

// Each ClientHello is classified by what it offered to resume with, the PSK
// first, and by the shape of its supported_versions, a GREASE value marked;
// without the extension it is legacy, whatever crypto/tls extrapolates from
// the legacy version.
func TestReadTlsClientOfferClassifiesTheClientHello(t *testing.T) {
	const grease uint16 = 0x3a3a
	for _, c := range []struct {
		what              string
		extensions        []uint16
		supportedVersions []uint16
		want              TlsClientOffer
	}{
		{
			what:              "a browser's ClientHello with a ticket",
			extensions:        []uint16{grease, 0, 23, 65281, 10, 11, 35, 16, 5, 13, 18, 51, 45, 43, 27, 17513, grease, 41},
			supportedVersions: []uint16{grease, tls.VersionTLS13, tls.VersionTLS12},
			want:              TlsClientOffer{NameClass: "api", Resumption: "psk", Versions: "tls13_tls12_grease"},
		},
		{
			what:              "a Go client's that keeps tickets and holds none",
			extensions:        []uint16{0, 5, 10, 11, 13, 65281, 23, 35, 18, 43, 51, 45},
			supportedVersions: []uint16{tls.VersionTLS13, tls.VersionTLS12},
			want:              TlsClientOffer{NameClass: "api", Resumption: "psk_modes", Versions: "tls13_tls12"},
		},
		{
			what:              "a TLS 1.3-only client that keeps no tickets",
			extensions:        []uint16{0, 10, 13, 43, 51},
			supportedVersions: []uint16{tls.VersionTLS13},
			want:              TlsClientOffer{NameClass: "api", Resumption: "none", Versions: "tls13"},
		},
		{
			what:              "a TLS 1.3-only client listing GREASE",
			extensions:        []uint16{0, 10, 13, 43, 51},
			supportedVersions: []uint16{grease, tls.VersionTLS13},
			want:              TlsClientOffer{NameClass: "api", Resumption: "none", Versions: "tls13_grease"},
		},
		{
			what:              "a TLS 1.2 client listing its version",
			extensions:        []uint16{0, 10, 11, 13, 35, 43},
			supportedVersions: []uint16{tls.VersionTLS12},
			want:              TlsClientOffer{NameClass: "api", Resumption: "ticket", Versions: "tls12"},
		},
		{
			what:              "a client of TLS 1.2 or older without supported_versions",
			extensions:        []uint16{0, 10, 11, 13, 35},
			supportedVersions: []uint16{tls.VersionTLS12, tls.VersionTLS11, tls.VersionTLS10},
			want:              TlsClientOffer{NameClass: "api", Resumption: "ticket", Versions: "legacy"},
		},
		{
			what:              "a probe with no extension",
			extensions:        nil,
			supportedVersions: []uint16{tls.VersionTLS12, tls.VersionTLS11, tls.VersionTLS10},
			want:              TlsClientOffer{NameClass: "api", Resumption: "none", Versions: "legacy"},
		},
		{
			what:              "a client listing only draft versions",
			extensions:        []uint16{43},
			supportedVersions: []uint16{0x7f1c},
			want:              TlsClientOffer{NameClass: "api", Resumption: "none", Versions: "other"},
		},
	} {
		clientHello := &tls.ClientHelloInfo{
			ServerName:        "api.service.example",
			Extensions:        c.extensions,
			SupportedVersions: c.supportedVersions,
		}
		if offer := readTlsClientOffer(clientHello, "api"); offer != c.want {
			t.Errorf("%s: %+v, want %+v", c.what, offer, c.want)
		}
	}

	// an offer never read labels as such
	nameClass, resumption, versions := TlsClientOffer{}.Labels()
	if nameClass != tlsClientOfferUnread || resumption != tlsClientOfferUnread || versions != tlsClientOfferUnread {
		t.Errorf("the zero offer labels as %s %s %s", nameClass, resumption, versions)
	}
	// a ClientHello whose context carries no slot, or that has no context, as
	// a test's, records nothing and fails nothing
	recordTlsClientOffer(&tls.ClientHelloInfo{}, "none")
}

// Runs one handshake through a front TLS listener over tlsConfig, as
// net/http runs it, and returns the offer the listener reported, or false
// when the handshake failed.
func exchangeTestTlsOffer(t *testing.T, tlsConfig *tls.Config, clientConfig *tls.Config) (TlsClientOffer, tls.ConnectionState, bool) {
	t.Helper()
	tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer tcpListener.Close()
	offers := make(chan TlsClientOffer, 1)
	listener := NewTlsListener(tcpListener, tlsConfig, 0, func(connectionState tls.ConnectionState, clientOffer TlsClientOffer) {
		offers <- clientOffer
	})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		tcpListener.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
		if err := conn.(interface {
			HandshakeContext(ctx context.Context) error
		}).HandshakeContext(context.Background()); err != nil {
			return
		}
		if _, err := conn.Write([]byte{1}); err != nil {
			return
		}
		io.Copy(io.Discard, conn)
	}()
	defer func() {
		<-serverDone
	}()

	ctx, cancel := context.WithTimeout(context.Background(), testTlsExchangeTimeout)
	defer cancel()
	dialer := &tls.Dialer{
		Config: clientConfig,
	}
	conn, err := dialer.DialContext(ctx, "tcp", tcpListener.Addr().String())
	if err != nil {
		return TlsClientOffer{}, tls.ConnectionState{}, false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		return TlsClientOffer{}, tls.ConnectionState{}, false
	}
	select {
	case offer := <-offers:
		return offer, conn.(*tls.Conn).ConnectionState(), true
	case <-time.After(testTlsExchangeTimeout):
		t.Fatal("the handshake wasn't reported")
		return TlsClientOffer{}, tls.ConnectionState{}, false
	}
}

// The TCP config reads the offer of real ClientHellos: the class of a served
// name, none without a name and unknown for another one, which the default
// server answers; what a Go client offers to resume with, by its cache and
// its versions; and its supported_versions. A client offering no protocol
// completes with none, as the no-ALPN handshakes the counters single out.
func TestTlsConfigReadsTheOfferOfRealClientHellos(t *testing.T) {
	vault := newTestTlsVault(t)
	tlsConfig, _ := vault.newTlsConfigs(t, DefaultTlsSettings())
	for _, c := range []struct {
		what   string
		client *tls.Config
		want   TlsClientOffer
	}{
		{
			what: "a client with no name, no protocol and no ticket cache",
			client: &tls.Config{
				InsecureSkipVerify: true,
			},
			want: TlsClientOffer{NameClass: "none", Resumption: "none", Versions: "tls13_tls12"},
		},
		{
			what: "a client of a name the front doesn't serve",
			client: &tls.Config{
				ServerName:         "nope.example",
				InsecureSkipVerify: true,
				ClientSessionCache: tls.NewLRUClientSessionCache(0),
			},
			want: TlsClientOffer{NameClass: "unknown", Resumption: "psk_modes", Versions: "tls13_tls12"},
		},
		{
			what: "a TLS 1.2 client of a connect name",
			client: &tls.Config{
				ServerName:         "connect.service.example",
				RootCAs:            vault.authority.pool(),
				NextProtos:         []string{"http/1.1"},
				ClientSessionCache: tls.NewLRUClientSessionCache(0),
				MaxVersion:         tls.VersionTLS12,
			},
			want: TlsClientOffer{NameClass: "connect", Resumption: "ticket", Versions: "tls12"},
		},
		{
			what: "a TLS 1.3-only client of the lb name",
			client: &tls.Config{
				ServerName: "test-lb.service.example",
				RootCAs:    vault.authority.pool(),
				NextProtos: []string{"h2"},
				MinVersion: tls.VersionTLS13,
			},
			want: TlsClientOffer{NameClass: "lb", Resumption: "none", Versions: "tls13"},
		},
	} {
		offer, state, ok := exchangeTestTlsOffer(t, tlsConfig, c.client)
		if !ok {
			t.Fatalf("%s: the handshake failed", c.what)
		}
		if offer != c.want {
			t.Errorf("%s: %+v, want %+v", c.what, offer, c.want)
		}
		if c.client.NextProtos == nil && state.NegotiatedProtocol != "" {
			t.Errorf("%s negotiated %q", c.what, state.NegotiatedProtocol)
		}
	}
}
