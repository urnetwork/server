package nginx

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

type testHeaderDeadlineConn struct {
	net.Conn
	deadlines []time.Time
}

func (self *testHeaderDeadlineConn) SetReadDeadline(deadline time.Time) error {
	self.deadlines = append(self.deadlines, deadline)
	return nil
}

// Progress across a HEADERS/CONTINUATION pair shares an absolute deadline.
// Its final payload byte releases the timer even with a DATA frame in flight.
func TestH2HeaderDeadlineFollowsFrameBoundaries(t *testing.T) {
	raw := &testHeaderDeadlineConn{}
	conn := &frontTlsConn{Conn: tls.Server(raw, &tls.Config{}), headerTimeout: time.Minute, prefaceLeft: len(http2.ClientPreface)}
	var frames bytes.Buffer
	writer := http2.NewFramer(&frames, nil)
	if err := writer.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: []byte{0x82}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteContinuation(1, true, []byte{0x87, 0x84}); err != nil {
		t.Fatal(err)
	}
	headEnd := frames.Len()
	if err := writer.WriteData(1, true, []byte("body")); err != nil {
		t.Fatal(err)
	}
	conn.observeFrames([]byte(http2.ClientPreface))
	if len(raw.deadlines) != 0 {
		t.Fatalf("preface set deadlines %v", raw.deadlines)
	}
	conn.observeFrames(frames.Next(2))
	if len(raw.deadlines) != 1 || raw.deadlines[0].IsZero() {
		t.Fatalf("initial prefix set deadlines %v", raw.deadlines)
	}
	conn.observeFrames(frames.Next(headEnd - 3))
	if len(raw.deadlines) != 1 {
		t.Fatalf("fragment progress changed deadlines %v", raw.deadlines)
	}
	conn.observeFrames(frames.Next(1 + len(conn.frameHeader)))
	if len(raw.deadlines) != 2 || !raw.deadlines[1].IsZero() {
		t.Fatalf("end of headers and start of DATA set deadlines %v", raw.deadlines)
	}
	conn.observeFrames(frames.Bytes())
	if len(raw.deadlines) != 2 {
		t.Fatalf("body changed deadlines %v", raw.deadlines)
	}
}

// One handshake the listener reported.
type testTlsObservedHandshake struct {
	connectionState tls.ConnectionState
	clientOffer     TlsClientOffer
}

// The listener reports each handshake that completes, once, with the state
// net/http then serves and the offer the front's config read from its
// ClientHello, however often HandshakeContext runs, with the h2 header bound
// and without it, full and resumed. A handshake that fails is never reported,
// a TLS 1.3 client's that refuses the certificate after the server's flight
// was out included.
func TestTlsListenerObservesEachCompletedHandshakeOnce(t *testing.T) {
	vault := newTestTlsVault(t)
	tlsConfig, _ := vault.newTlsConfigs(t, DefaultTlsSettings())
	for _, headerTimeout := range []time.Duration{0, time.Minute} {
		tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			tcpListener.Close()
		})
		observed := make(chan testTlsObservedHandshake, 8)
		listener := NewTlsListener(tcpListener, tlsConfig, headerTimeout, func(connectionState tls.ConnectionState, clientOffer TlsClientOffer) {
			observed <- testTlsObservedHandshake{
				connectionState: connectionState,
				clientOffer:     clientOffer,
			}
		})
		// serves one connection as net/http does, the handshake first, here
		// twice; the client reads a byte, after any session ticket. The
		// client's view of the handshake, or its error
		exchange := func(clientConfig *tls.Config) (tls.ConnectionState, error) {
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
				// what net/http asks of a TLS connection (net/http server.go)
				handshaker, ok := conn.(interface {
					HandshakeContext(ctx context.Context) error
					ConnectionState() tls.ConnectionState
				})
				if !ok {
					t.Errorf("the accepted %T is no TLS connection to net/http", conn)
					return
				}
				for range 2 {
					if err := handshaker.HandshakeContext(context.Background()); err != nil {
						return
					}
				}
				if _, err := conn.Write([]byte{1}); err != nil {
					return
				}
				// until the client closes, so no reset races its read
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
				return tls.ConnectionState{}, err
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(testTlsExchangeTimeout))
			if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
				return tls.ConnectionState{}, err
			}
			return conn.(*tls.Conn).ConnectionState(), nil
		}
		// the one state reported since the last call, nil for none; the
		// exchange has waited for the server, which reports before its
		// handshake returns
		reported := func() *testTlsObservedHandshake {
			select {
			case handshake := <-observed:
				select {
				case <-observed:
					t.Fatalf("header timeout %s: one handshake was reported twice", headerTimeout)
				default:
				}
				return &handshake
			default:
				return nil
			}
		}

		sessionCache := newTestNotifyingSessionCache()
		for _, resumed := range []bool{false, true} {
			clientState, err := exchange(&tls.Config{
				ServerName:         "test-api.service.example",
				RootCAs:            vault.authority.pool(),
				NextProtos:         []string{"h2", "http/1.1"},
				ClientSessionCache: sessionCache,
			})
			if err != nil {
				t.Fatalf("header timeout %s, resumed %t: %v", headerTimeout, resumed, err)
			}
			if clientState.DidResume != resumed {
				t.Fatalf("header timeout %s: the client resumed %t", headerTimeout, clientState.DidResume)
			}
			handshake := reported()
			if handshake == nil {
				t.Fatalf("header timeout %s, resumed %t: the handshake wasn't reported", headerTimeout, resumed)
			}
			serverState := handshake.connectionState
			if serverState.Version != tls.VersionTLS13 || serverState.DidResume != resumed || serverState.NegotiatedProtocol != "h2" || !serverState.HandshakeComplete {
				t.Fatalf("header timeout %s, resumed %t: reported %#x resumed %t %q complete %t", headerTimeout, resumed, serverState.Version, serverState.DidResume, serverState.NegotiatedProtocol, serverState.HandshakeComplete)
			}
			// a Go client with a session cache offers PSK modes, and its ticket
			// once it holds one
			wantOffer := TlsClientOffer{
				NameClass:  "api",
				Resumption: tlsClientOfferResumptionPskModes,
				Versions:   tlsClientOfferVersionsTls13Tls12,
			}
			if resumed {
				wantOffer.Resumption = tlsClientOfferResumptionPsk
			}
			if handshake.clientOffer != wantOffer {
				t.Fatalf("header timeout %s, resumed %t: reported the offer %+v, want %+v", headerTimeout, resumed, handshake.clientOffer, wantOffer)
			}
			if !resumed {
				sessionCache.awaitSession(t)
			}
		}

		if _, err := exchange(&tls.Config{
			ServerName: "test-api.service.example",
			RootCAs:    x509.NewCertPool(),
			MinVersion: tls.VersionTLS13,
		}); err == nil {
			t.Fatalf("header timeout %s: the client accepted a certificate it can't verify", headerTimeout)
		}
		if serverState := reported(); serverState != nil {
			t.Fatalf("header timeout %s: a handshake the client refused was reported", headerTimeout)
		}
	}
}

// Why the listener reports at HandshakeContext's return rather than from
// crypto/tls's VerifyConnection: a TLS 1.3 server calls VerifyConnection
// once its own flight is out, before it reads anything of the client's
// (go1.27 handshake_server_tls13.go, readClientCertificate), so it runs for a
// handshake the client then fails by refusing the certificate. Pins the order
// the listener's doc relies on.
func TestTls13ServerVerifyConnectionRunsBeforeTheClientRefuses(t *testing.T) {
	vault := newTestTlsVault(t)
	pair := vault.dirPairs["star.service.example"]
	certificate, err := tls.X509KeyPair(pair.pemBytes, pair.keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	var verifyCount atomic.Int64
	// a config of the test's own, so no shared base gets the hook
	listener := listenTestTls(t, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS13,
		VerifyConnection: func(tls.ConnectionState) error {
			verifyCount.Add(1)
			return nil
		},
	})
	if _, err := listener.exchange(t, &tls.Config{
		ServerName: "test-api.service.example",
		RootCAs:    x509.NewCertPool(),
	}); err == nil {
		t.Fatal("the client accepted a certificate it can't verify")
	}
	if count := verifyCount.Load(); count != 1 {
		t.Fatalf("VerifyConnection ran %d times for a handshake the client refused, want 1", count)
	}
}
