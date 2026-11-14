package acceptance

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	proxylib "github.com/urnetwork/proxy/v2026"
)

func waitHTTPConnectBoundary(t *testing.T, boundary <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-boundary:
	case <-time.After(5 * time.Second):
		t.Fatalf("did not reach %s", name)
	}
}

// The real proxy withholds CONNECT's 200 until its upstream dial succeeds.
// Pair a blocked upstream dial with a successful dial whose origin never
// answers TLS. No DNS lookup or target connection leaves this local fixture.
func TestHTTPConnectTraceDistinguishesProxyResponseAndOriginTLSStalls(t *testing.T) {
	for _, tlsStall := range []bool{false, true} {
		name, phase, status := "connect_response", "tcp_connected", "pending"
		if tlsStall {
			name, phase, status = "origin_tls", "tls_handshake", "200"
		}
		t.Run(name, func(t *testing.T) {
			boundary := make(chan struct{})
			dialCanceled := make(chan struct{})
			fixtureDone := make(chan struct{})
			defer close(fixtureDone)
			var dials atomic.Int32
			proxy := proxylib.NewHttpProxyWithDefaults()
			proxy.ConnectDialContextWithRequest = func(ctx context.Context, _ *http.Request, _, _ string) (net.Conn, error) {
				dials.Add(1)
				if !tlsStall {
					close(boundary)
					select {
					case <-ctx.Done():
						close(dialCanceled)
					case <-fixtureDone:
					}
					return nil, context.Canceled
				}
				upstream, origin := net.Pipe()
				go func() {
					<-fixtureDone
					_ = origin.Close()
				}()
				go func() {
					defer origin.Close()
					var tlsRecordHeader [5]byte
					if _, err := io.ReadFull(origin, tlsRecordHeader[:]); err != nil {
						return
					}
					close(boundary)
					_, _ = io.Copy(io.Discard, origin)
				}()
				return upstream, nil
			}
			server := httptest.NewServer(proxy)
			defer server.Close()
			proxyURL, _ := url.Parse(server.URL)
			proxyURL.User = url.UserPassword("proxy-authorization-secret", "password-secret")
			transport := newHTTPConnectTransport(proxyURL)
			ctx, cancel := context.WithCancel(context.Background())
			defer func() {
				cancel()
				transport.CloseIdleConnections()
			}()
			done := make(chan error, 1)
			go func() {
				done <- probeHTTPSRequest(ctx, &http.Client{Transport: transport}, "https://origin.invalid/")
			}()
			waitHTTPConnectBoundary(t, boundary, name)
			cancel()
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not cancel")
			}
			// net/http detaches an in-progress dial for possible connection reuse.
			// Production closes the transport when the campaign exits; that step
			// must also close the socket and cancel the proxy's blocked dial.
			transport.CloseIdleConnections()
			if !tlsStall {
				waitHTTPConnectBoundary(t, dialCanceled, "canceled upstream dial")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("request error = %v, want context.Canceled", err)
			}
			for _, want := range []string{"phase " + phase + ";", "proxy_connect_status " + status, "connection not_established", "origin unavailable"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("trace lacks %q: %v", want, err)
				}
			}
			for _, secret := range []string{"proxy-authorization-secret", "password-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("trace leaked proxy credentials: %v", err)
				}
			}
			if got := dials.Load(); got != 1 {
				t.Errorf("upstream dial count = %d, want one", got)
			}
		})
	}
}

func TestHTTPConnectTraceRetainsRejectedStatusWithoutResponseContent(t *testing.T) {
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodConnect {
			t.Errorf("proxy method = %s, want CONNECT", r.Method)
		}
		w.Header().Set("X-Private-Header", "response-header-secret")
		w.WriteHeader(http.StatusProxyAuthRequired)
		_, _ = io.WriteString(w, "response-body-secret")
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	transport := newHTTPConnectTransport(proxyURL)
	defer transport.CloseIdleConnections()
	err := probeHTTPSRequest(context.Background(), &http.Client{Transport: transport, Timeout: 5 * time.Second}, "https://origin.invalid/")
	if err == nil {
		t.Fatal("rejected CONNECT passed")
	}
	for _, want := range []string{"phase proxy_connect_rejected;", "proxy_connect_status 407", "connection not_established"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("trace lacks %q: %v", want, err)
		}
	}
	for _, forbidden := range []string{"response-header-secret", "response-body-secret", "tls_handshake"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("rejection trace contains %q: %v", forbidden, err)
		}
	}
	if requests.Load() != 1 {
		t.Errorf("CONNECT requests = %d, want one", requests.Load())
	}
}
