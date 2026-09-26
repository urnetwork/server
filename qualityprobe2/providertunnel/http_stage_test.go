package providertunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// Reads only the closed diagnostic enum, never a destination or free-form error.
func requireProbeHttpStage(t *testing.T, err error, want string) {
	t.Helper()
	var staged interface{ ProviderHttpStage() string }
	if !errors.As(err, &staged) || staged.ProviderHttpStage() != want {
		t.Fatalf("missing expected fixed stage %q (error type %T)", want, err)
	}
}

// The actual HTTP owner preserves the timeout identity while identifying its
// custom dial boundary; that boundary includes both DNS and socket races.
func TestProbeHttpStageDial(t *testing.T) {
	client := httpClientOverDialerWithHosts(func(context.Context, string, string) (net.Conn, error) {
		return nil, os.ErrDeadlineExceeded
	}, nil, []string{"echo.example"}, time.Minute)
	_, err := client.Get("https://echo.example/my-ip-info")
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("dial error identity lost")
	}
	if timeout, ok := err.(interface{ Timeout() bool }); !ok || !timeout.Timeout() {
		t.Fatal("HTTP caller lost timeout classification")
	}
	requireProbeHttpStage(t, err, "dial_dns_or_socket")
}

// A deterministic rejected TLS write is not misreported as a dial failure.
func TestProbeHttpStageTls(t *testing.T) {
	conn := &probeStageFailConn{}
	client := httpClientOverDialerWithHosts(func(context.Context, string, string) (net.Conn, error) {
		return conn, nil
	}, nil, []string{"echo.example"}, time.Minute)
	_, err := client.Get("https://echo.example/my-ip-info")
	if !errors.Is(err, io.ErrUnexpectedEOF) || !conn.closed.Load() {
		t.Fatal("TLS failure lost cause or owned connection cleanup")
	}
	requireProbeHttpStage(t, err, "tls")
}

// The probe owns DialTLSContext, so net/http cannot publish the custom dial
// and handshake phases for request-progress diagnostics on its behalf.
func TestProbeHttpCustomDialPublishesTracePhases(t *testing.T) {
	var dialStart, dialDone, tlsStart, tlsDone atomic.Int32
	client := httpClientOverDialerWithHosts(func(context.Context, string, string) (net.Conn, error) {
		return &probeStageFailConn{}, nil
	}, nil, []string{"echo.example"}, time.Minute)
	trace := &httptrace.ClientTrace{
		ConnectStart: func(string, string) { dialStart.Add(1) },
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				dialDone.Add(1)
			}
		},
		TLSHandshakeStart: func() { tlsStart.Add(1) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err != nil {
				tlsDone.Add(1)
			}
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, "https://echo.example/my-ip-info", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(req); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("synthetic TLS failure lost its cause: %v", err)
	}
	if dialStart.Load() != 1 || dialDone.Load() != 1 || tlsStart.Load() != 1 || tlsDone.Load() != 1 {
		t.Fatalf("custom dial trace phases = dial %d/%d tls %d/%d, want 1/1 each",
			dialStart.Load(), dialDone.Load(), tlsStart.Load(), tlsDone.Load())
	}
}

// Policy remains enforced before touching the provider dialer.
func TestProbeHttpStagePolicy(t *testing.T) {
	var dialed atomic.Bool
	client := httpClientOverDialerWithHosts(func(context.Context, string, string) (net.Conn, error) {
		dialed.Store(true)
		return nil, context.Canceled
	}, nil, []string{"echo.example"}, time.Minute)
	_, err := client.Get("https://other.example/my-ip-info")
	if !errors.Is(err, ErrPinHostUnknown) || dialed.Load() {
		t.Fatal("allowlist refusal changed")
	}
	requireProbeHttpStage(t, err, "policy")
}

// Metadata must not turn ordinary caller cancellation into a timeout.
func TestProbeHttpStageCancellationIdentity(t *testing.T) {
	client := httpClientOverDialerWithHosts(func(context.Context, string, string) (net.Conn, error) {
		return nil, context.Canceled
	}, nil, []string{"echo.example"}, time.Minute)
	_, err := client.Get("https://echo.example/my-ip-info")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("caller cancellation identity lost")
	}
	if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
		t.Fatal("cancellation promoted to timeout")
	}
}

// Existing ownership and protocol policy are unchanged by diagnostics.
func TestProbeHttpStageTransportPolicy(t *testing.T) {
	client := httpClientOverDialerWithHosts(func(context.Context, string, string) (net.Conn, error) {
		return nil, context.Canceled
	}, nil, []string{"echo.example"}, time.Minute)
	transport, ok := client.Transport.(*providerHttpTransport)
	if !ok || !transport.DisableKeepAlives || transport.TLSNextProto == nil || len(transport.TLSNextProto) != 0 || client.Timeout != time.Minute {
		t.Fatal("diagnostics changed pooling, protocol or owner timeout")
	}
}

// A synthetic connection fails synchronously, without timers or remote I/O.
type probeStageFailConn struct{ closed atomic.Bool }

func (self *probeStageFailConn) Read([]byte) (int, error)         { return 0, io.ErrUnexpectedEOF }
func (self *probeStageFailConn) Write([]byte) (int, error)        { return 0, io.ErrUnexpectedEOF }
func (self *probeStageFailConn) Close() error                     { self.closed.Store(true); return nil }
func (self *probeStageFailConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (self *probeStageFailConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (self *probeStageFailConn) SetDeadline(time.Time) error      { return nil }
func (self *probeStageFailConn) SetReadDeadline(time.Time) error  { return nil }
func (self *probeStageFailConn) SetWriteDeadline(time.Time) error { return nil }
