package nginx

// Regression tests: a client that resets its connection or its stream while
// the front reads its body went away, so the body is refused as canceled,
// with no response, whatever the request's context says yet. Before, only an
// ended context or a timeout was the client leaving, so a reset that failed
// the read before the context ended, or without ending it, was counted as
// incomplete and answered 400: always for an h3 client that resets only its
// request's side, and whenever Go's h2 server failed a body for a lost
// connection before it canceled the stream's context.
//
// The errors here are the ones real servers return: Go's own TCP, h1 and h2
// stacks and URnetwork's http3 (../quic-go), each read through a
// handler while a raw client resets at a barrier, the handler's first read
// of the body. Each is then classified with a context that never ends, so
// the test shows what the error alone says.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/quic-go/v2026"
	"github.com/urnetwork/quic-go/v2026/http3"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// The read error of a TCP connection its peer reset: the peer closes with
// SO_LINGER 0, which sends RST in place of FIN.
func testConnectionResetReadError(t *testing.T) error {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	serverConn, err := listener.Accept()
	if err != nil {
		clientConn.Close()
		t.Fatal(err)
	}
	defer serverConn.Close()
	if err := clientConn.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	clientConn.Close()
	// bounds a broken test, never a passing one
	serverConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, err = serverConn.Read(make([]byte, 1))
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("a read of a reset connection returned %v, want ECONNRESET", err)
	}
	return err
}

// A handler that reads a request's body until it fails, signals its first
// successful read, and hands on the error that ended the body.
func testBodyErrorHandler(firstReads chan<- struct{}, bodyErrs chan<- error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buffer := make([]byte, 4096)
		first := true
		for {
			n, err := r.Body.Read(buffer)
			if 0 < n && first {
				first = false
				firstReads <- struct{}{}
			}
			if err != nil {
				bodyErrs <- err
				return
			}
		}
	})
}

// The body read errors of real h2 streams, each ended by its client once
// the handler has read its first bytes: by end, after a POST declaring 100
// bytes and sending 10.
func testHttp2BodyErrors(t *testing.T, ends map[string]func(conn *tls.Conn, framer *http2.Framer)) map[string]error {
	t.Helper()
	firstReads := make(chan struct{}, 1)
	bodyErrs := make(chan error, 1)
	server := httptest.NewUnstartedServer(testBodyErrorHandler(firstReads, bodyErrs))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	errs := map[string]error{}
	for name, end := range ends {
		conn, err := tls.Dial("tcp", server.Listener.Addr().String(), &tls.Config{
			// the httptest server's certificate names no served name
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2"},
		})
		if err != nil {
			t.Fatal(err)
		}
		// bounds a broken test, never a passing one
		conn.SetDeadline(time.Now().Add(30 * time.Second))
		if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
			t.Fatal(err)
		}
		framer := http2.NewFramer(conn, conn)
		framer.WriteSettings()
		var headerBlock bytes.Buffer
		encoder := hpack.NewEncoder(&headerBlock)
		for _, field := range [][2]string{
			{":method", "POST"},
			{":scheme", "https"},
			{":authority", "api.service.example"},
			{":path", "/body"},
			{"content-length", "100"},
		} {
			encoder.WriteField(hpack.HeaderField{Name: field[0], Value: field[1]})
		}
		framer.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      1,
			BlockFragment: headerBlock.Bytes(),
			EndHeaders:    true,
		})
		framer.WriteData(1, false, []byte("0123456789"))
		receiveBounded(t, firstReads, name+": the handler's first read")
		end(conn, framer)
		errs[name] = receiveBounded(t, bodyErrs, name+": the body's error")
		conn.Close()
	}
	return errs
}

// The body read errors of real h3 request streams on the quic-go fork's
// http3, each ended by its client once the handler has read its first
// bytes: by end, after a POST declaring declaredLength bytes and sending
// body. The certificate is any the server may present.
func testHttp3BodyErrors(t *testing.T, certificates []tls.Certificate, declaredLength int64, body []byte, ends map[string]func(quicConn *quic.Conn, str *http3.RequestStream)) map[string]error {
	t.Helper()
	// quic-go warns once per process when it can't raise the socket buffers
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	firstReads := make(chan struct{}, 1)
	bodyErrs := make(chan error, 1)
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	quicTransport := &quic.Transport{
		Conn: udpConn,
	}
	listener, err := quicTransport.ListenEarly(&tls.Config{
		Certificates: certificates,
		NextProtos:   []string{http3.NextProtoH3},
	}, &quic.Config{})
	if err != nil {
		udpConn.Close()
		t.Fatal(err)
	}
	h3Server := &http3.Server{
		Handler: testBodyErrorHandler(firstReads, bodyErrs),
	}
	go h3Server.ServeListener(listener)
	defer func() {
		h3Server.Close()
		listener.Close()
		quicTransport.Close()
		udpConn.Close()
	}()

	errs := map[string]error{}
	for name, end := range ends {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		quicConn, err := quic.DialAddr(ctx, udpConn.LocalAddr().String(), &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{http3.NextProtoH3},
		}, &quic.Config{})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		str, err := (&http3.Transport{}).NewClientConn(quicConn).OpenRequestStream(ctx)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		request, err := http.NewRequest("POST", "https://api.service.example/body", http.NoBody)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		// the declared length goes in the head; the body goes as DATA frames
		request.ContentLength = declaredLength
		if err := str.SendRequestHeader(request); err != nil {
			cancel()
			t.Fatal(err)
		}
		if _, err := str.Write(body); err != nil {
			cancel()
			t.Fatal(err)
		}
		receiveBounded(t, firstReads, name+": the handler's first read")
		end(quicConn, str)
		errs[name] = receiveBounded(t, bodyErrs, name+": the body's error")
		quicConn.CloseWithError(0, "")
		cancel()
	}
	return errs
}

// Each failed body read a real client's reset or loss causes is the client
// gone, whatever the request's context says: a TCP reset, an h2 stream the
// client canceled, an h2 connection that ended, an h3 request stream the
// client reset, with or without stopping the response, and an h3 connection
// it closed. A body that broke its framing or its length is not.
func TestBodyReadClientGoneOfRealClientResets(t *testing.T) {
	type classified struct {
		err  error
		gone bool
	}
	cases := map[string]classified{
		"tcp: reset": {err: testConnectionResetReadError(t), gone: true},
	}

	http2Errs := testHttp2BodyErrors(t, map[string]func(conn *tls.Conn, framer *http2.Framer){
		"h2: the client cancels its stream": func(conn *tls.Conn, framer *http2.Framer) {
			framer.WriteRSTStream(1, http2.ErrCodeCancel)
		},
		"h2: the client resets its connection": func(conn *tls.Conn, framer *http2.Framer) {
			conn.NetConn().(*net.TCPConn).SetLinger(0)
			conn.NetConn().Close()
		},
		"h2: the client closes its connection": func(conn *tls.Conn, framer *http2.Framer) {
			conn.Close()
		},
		"h2: the stream ends short of its declared length": func(conn *tls.Conn, framer *http2.Framer) {
			framer.WriteData(1, true, nil)
		},
		"h2: the client resets its stream for a protocol error": func(conn *tls.Conn, framer *http2.Framer) {
			framer.WriteRSTStream(1, http2.ErrCodeProtocol)
		},
	})
	for name, err := range http2Errs {
		cases[name] = classified{
			err: err,
			// a reset of another code than CANCEL counts as canceled through
			// the context Go's server cancels first; the error alone is not
			// the client gone
			gone: name != "h2: the stream ends short of its declared length" && name != "h2: the client resets its stream for a protocol error",
		}
	}

	// any certificate the client can complete a handshake with
	certificateServer := httptest.NewUnstartedServer(http.NotFoundHandler())
	certificateServer.StartTLS()
	certificates := certificateServer.TLS.Certificates
	certificateServer.Close()
	http3Errs := testHttp3BodyErrors(t, certificates, 100, []byte("0123456789"), map[string]func(quicConn *quic.Conn, str *http3.RequestStream){
		"h3: the client resets its request's side": func(quicConn *quic.Conn, str *http3.RequestStream) {
			str.CancelWrite(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
		},
		"h3: the client resets both sides": func(quicConn *quic.Conn, str *http3.RequestStream) {
			str.CancelWrite(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
			str.CancelRead(quic.StreamErrorCode(http3.ErrCodeRequestCanceled))
		},
		"h3: the client closes its connection": func(quicConn *quic.Conn, str *http3.RequestStream) {
			quicConn.CloseWithError(quic.ApplicationErrorCode(http3.ErrCodeNoError), "")
		},
	})
	// more DATA than the declared length, refused by the quic-go fork's http3
	for name, err := range testHttp3BodyErrors(t, certificates, 5, []byte("0123456789"), map[string]func(quicConn *quic.Conn, str *http3.RequestStream){
		"h3: the body runs past its declared length": func(quicConn *quic.Conn, str *http3.RequestStream) {},
	}) {
		http3Errs[name] = err
	}
	for name, err := range http3Errs {
		cases[name] = classified{
			err:  err,
			gone: name != "h3: the body runs past its declared length",
		}
	}

	for name, c := range cases {
		if gone := bodyReadClientGone(c.err); gone != c.gone {
			t.Errorf("%s: %T %q is the client gone: %t, want %t", name, c.err, c.err, gone, c.gone)
		}
		// a context that never ends: the error alone decides
		wantReason := bodyRefusalIncomplete
		if c.gone {
			wantReason = bodyRefusalCanceled
		}
		if refusal := bodyReadRefusal(context.Background(), c.err); refusal.reason != wantReason {
			t.Errorf("%s: %T %q is refused as %s (%d), want %s", name, c.err, c.err, refusal.reason, refusal.status, wantReason)
		}
	}
}

// The QUIC failures no test client can make at will, as URnetwork's quic-go
// and its http3 report them: a stateless reset, and a remote transport,
// application or stream error, each the client's QUIC gone, wrapped or not;
// the same errors from the front's own side are not.
func TestBodyReadClientGoneOfQuicErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		gone bool
	}{
		{err: &quic.StatelessResetError{}, gone: true},
		{err: &quic.TransportError{Remote: true, ErrorCode: quic.NoError}, gone: true},
		{err: &quic.TransportError{Remote: false, ErrorCode: quic.InternalError}, gone: false},
		{err: &quic.ApplicationError{Remote: true, ErrorCode: quic.ApplicationErrorCode(http3.ErrCodeNoError)}, gone: true},
		{err: &quic.ApplicationError{Remote: false, ErrorCode: quic.ApplicationErrorCode(http3.ErrCodeNoError)}, gone: false},
		{err: &quic.StreamError{Remote: true, ErrorCode: quic.StreamErrorCode(http3.ErrCodeRequestCanceled)}, gone: true},
		{err: &quic.StreamError{Remote: false, ErrorCode: quic.StreamErrorCode(http3.ErrCodeRequestCanceled)}, gone: false},
		{err: &http3.Error{Remote: true, ErrorCode: http3.ErrCodeRequestCanceled}, gone: true},
		{err: &http3.Error{Remote: false, ErrorCode: http3.ErrCodeRequestCanceled}, gone: false},
		{err: fmt.Errorf("read: %w", &http3.Error{Remote: true, ErrorCode: http3.ErrCodeRequestCanceled}), gone: true},
		{err: io.ErrUnexpectedEOF, gone: false},
		{err: errors.New("http: invalid byte in chunk length"), gone: false},
	} {
		if gone := bodyReadClientGone(c.err); gone != c.gone {
			t.Errorf("%T %q is the client gone: %t, want %t", c.err, c.err, gone, c.gone)
		}
	}
}
