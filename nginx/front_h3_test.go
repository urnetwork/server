package nginx

// The front's api handler over a real http3 server, for what depends on
// URnetwork's http3 (../quic-go): the forwarder's per-read body
// timeout reaches an h3 request's stream through http.ResponseController.

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/quic-go"
	"github.com/urnetwork/quic-go/http3"
)

// On h3 the forwarder bounds each read of a request body by
// client_body_timeout, as on h1 and h2: the ResponseWriter of the quic-go
// fork's http3 takes a read deadline through http.ResponseController, which
// sets it on the request's stream, so a body that stalls past the timeout is
// a 408, not a wait for the request's context.
func TestNewFrontH3BoundsEachBodyReadByTheClientBodyTimeout(t *testing.T) {
	const clientBodyTimeout = 200 * time.Millisecond
	vault := newTestTlsVault(t)
	settings := testFrontSettings()
	settings.Body.ClientBodyTimeout = clientBodyTimeout
	front := newTestCloseFront(t, vault.routeTable, vault.load(t), settings)

	// quic-go warns once per process when it can't raise the socket buffers
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	transport := &quic.Transport{
		Conn: udpConn,
	}
	listener, err := transport.ListenEarly(front.QuicTlsConfig(), &quic.Config{})
	if err != nil {
		udpConn.Close()
		t.Fatal(err)
	}
	server := &http3.Server{
		Handler: front.H3ApiHandler("eth1"),
	}
	go server.ServeListener(listener)
	t.Cleanup(func() {
		server.Close()
		listener.Close()
		transport.Close()
		udpConn.Close()
	})

	roundTripper := &http3.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "api.service.example",
		},
		Dial: func(ctx context.Context, _ string, tlsConfig *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
			return quic.DialAddr(ctx, udpConn.LocalAddr().String(), tlsConfig, quicConfig)
		},
	}
	t.Cleanup(func() {
		roundTripper.Close()
	})
	client := &http.Client{
		Transport: roundTripper,
		Timeout:   testTlsExchangeTimeout,
	}

	bodyReader, bodyWriter := io.Pipe()
	defer bodyWriter.Close()
	// part of a declared body, then nothing more
	go io.WriteString(bodyWriter, "0123456789")
	r, err := http.NewRequestWithContext(t.Context(), "POST", "https://api.service.example/v1", bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	r.ContentLength = 100
	requestStart := time.Now()
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.ProtoMajor != 3 || response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("a stalled h3 body = HTTP/%d %d, want 408", response.ProtoMajor, response.StatusCode)
	}
	if elapsed := time.Since(requestStart); elapsed < clientBodyTimeout {
		t.Fatalf("the 408 came after %s, before client_body_timeout %s", elapsed, clientBodyTimeout)
	}
}
