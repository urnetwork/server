package connect

// This file verifies the exchange outbound-dial seam at its production owner,
// without database state or the performance harness.

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// closeObservedExchangeConn records the exchange connection's ownership
// release without changing stream behavior.
type closeObservedExchangeConn struct {
	net.Conn
	closeOnce sync.Once
	closed    chan struct{}
}

// Wraps a stream and records its first close.
func newCloseObservedExchangeConn(conn net.Conn) *closeObservedExchangeConn {
	return &closeObservedExchangeConn{
		Conn:   conn,
		closed: make(chan struct{}),
	}
}

// Closing remains idempotent while notifying the owner test.
func (self *closeObservedExchangeConn) Close() error {
	var err error
	self.closeOnce.Do(func() {
		err = self.Conn.Close()
		close(self.closed)
	})
	return err
}

// Echoes the exchange header, then drains client writes until ownership closes
// the stream. This is the minimum real resident-side handshake.
func serveExchangeHeaderEcho(conn net.Conn, settings *ExchangeSettings) error {
	defer conn.Close()
	buffer := NewDefaultExchangeBuffer(settings)
	header, err := buffer.ReadHeader(context.Background(), conn)
	if err != nil {
		return err
	}
	if err := buffer.WriteHeader(context.Background(), conn, header); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, conn)
	return err
}

// Accepts the platform-specific close shapes emitted after a completed fixture
// handshake. TCP uses linger zero and can therefore surface a reset.
func isExpectedExchangeFixtureCloseError(err error) bool {
	return err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)
}

// The callback receives the routed authority and owns the returned connection
// only until NewExchangeConnection succeeds; the connection then belongs to
// the ExchangeConnection and closes with it.
func TestExchangeConnectionUsesAndOwnsInjectedDialContext(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	observedConn := newCloseObservedExchangeConn(clientConn)
	serverDone := make(chan error, 1)
	settings := DefaultExchangeSettings()
	settings.ExchangePingTimeout = time.Hour
	go func() {
		serverDone <- serveExchangeHeaderEcho(serverConn, settings)
	}()

	type dialCall struct {
		ctx       context.Context
		network   string
		authority string
	}
	dialCalls := make(chan dialCall, 1)
	settings.DialContext = func(ctx context.Context, network string, authority string) (net.Conn, error) {
		dialCalls <- dialCall{ctx: ctx, network: network, authority: authority}
		return observedConn, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	header := ExchangeHeader{
		Version:    1,
		ClientId:   server.NewId(),
		ResidentId: server.NewId(),
		Op:         ExchangeOpForward,
	}
	connection, err := NewExchangeConnection(
		ctx,
		header,
		"edge-a",
		18443,
		map[string]string{"edge-a": "198.51.100.8"},
		settings,
	)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case call := <-dialCalls:
		if call.ctx != ctx {
			t.Fatal("exchange did not pass through the caller context")
		}
		if call.network != "tcp" || call.authority != "198.51.100.8:18443" {
			t.Fatalf("dial = %s %s, expected tcp 198.51.100.8:18443", call.network, call.authority)
		}
	default:
		t.Fatal("exchange did not use the injected dial context")
	}
	connection.Close()
	select {
	case <-observedConn.closed:
	case <-time.After(time.Second):
		t.Fatal("exchange did not close the injected connection")
	}
	select {
	case serverErr := <-serverDone:
		if !isExpectedExchangeFixtureCloseError(serverErr) {
			t.Fatal(serverErr)
		}
	case <-time.After(time.Second):
		t.Fatal("resident-side fixture did not observe exchange close")
	}
}

// Callback failures return directly and cannot fall through to a host dial.
func TestExchangeConnectionInjectedDialFailureIsReturned(t *testing.T) {
	sentinel := errors.New("injected exchange dial failure")
	settings := DefaultExchangeSettings()
	var dialCount int
	settings.DialContext = func(ctx context.Context, network string, authority string) (net.Conn, error) {
		dialCount++
		return nil, sentinel
	}
	connection, err := NewExchangeConnection(
		context.Background(),
		ExchangeHeader{
			Version:    1,
			ClientId:   server.NewId(),
			ResidentId: server.NewId(),
			Op:         ExchangeOpForward,
		},
		"edge-a",
		18443,
		map[string]string{"edge-a": "192.0.2.99"},
		settings,
	)
	if connection != nil {
		connection.Close()
		t.Fatal("failed injected dial returned a connection")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("dial error = %v, expected sentinel", err)
	}
	if dialCount != 1 {
		t.Fatalf("injected dial called %d times, expected one", dialCount)
	}
}

// Parent cancellation closes a connected socket that is still waiting for
// its header echo rather than retaining it until the handshake deadline.
func TestExchangeConnectionCancellationInterruptsHeaderRead(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	observedConn := newCloseObservedExchangeConn(clientConn)
	settings := DefaultExchangeSettings()
	settings.ExchangeReadHeaderTimeout = time.Hour
	settings.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return observedConn, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	headerRead := make(chan error, 1)
	go func() {
		buffer := NewReceiveOnlyExchangeBuffer(settings)
		_, err := buffer.ReadHeader(context.Background(), serverConn)
		headerRead <- err
	}()
	type connectionResult struct {
		connection *ExchangeConnection
		err        error
	}
	result := make(chan connectionResult, 1)
	go func() {
		connection, err := NewExchangeConnection(
			ctx,
			ExchangeHeader{
				Version:    1,
				ClientId:   server.NewId(),
				ResidentId: server.NewId(),
				Op:         ExchangeOpForward,
			},
			"edge-a",
			18443,
			map[string]string{"edge-a": "198.51.100.8"},
			settings,
		)
		result <- connectionResult{connection: connection, err: err}
	}()
	select {
	case err := <-headerRead:
		if err != nil {
			t.Fatalf("read outbound exchange header: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("outbound exchange did not send its header")
	}
	cancel()
	select {
	case connectionResult := <-result:
		if connectionResult.connection != nil {
			connectionResult.connection.Close()
			t.Fatal("canceled header read returned a live connection")
		}
		if connectionResult.err == nil {
			t.Fatal("canceled header read returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled header read retained the outbound socket")
	}
	select {
	case <-observedConn.closed:
	case <-time.After(time.Second):
		t.Fatal("canceled header read did not close the outbound socket")
	}
}

// A nil callback retains the host TCP dial path and completes the production
// exchange header handshake against a real local listener.
func TestExchangeConnectionNilDialContextUsesHostNetwork(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	settings := DefaultExchangeSettings()
	settings.ExchangePingTimeout = time.Hour
	if settings.DialContext != nil {
		t.Fatal("default exchange settings unexpectedly inject a dial context")
	}
	serverDone := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		serverDone <- serveExchangeHeaderEcho(conn, settings)
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	connection, err := NewExchangeConnection(
		context.Background(),
		ExchangeHeader{
			Version:    1,
			ClientId:   server.NewId(),
			ResidentId: server.NewId(),
			Op:         ExchangeOpForward,
		},
		"edge-local",
		port,
		map[string]string{"edge-local": "127.0.0.1"},
		settings,
	)
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	select {
	case serverErr := <-serverDone:
		if !isExpectedExchangeFixtureCloseError(serverErr) {
			t.Fatal(serverErr)
		}
	case <-time.After(time.Second):
		t.Fatal("host-network fixture did not observe exchange close")
	}
}
