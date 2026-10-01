package egresshealth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"
)

// net/http verifies an already-handshaken *tls.Conn returned by DialTLSContext
// and emits another start/done pair. That no-op must not replace the real TLS
// interval emitted by the provider's owned handshake.
func TestRequestTLSClockSurvivesCompletedCustomHandshake(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		custom, fail bool
		starts       int64
	}{
		{name: "standard_transport", starts: 1},
		{name: "completed_custom_handshake", custom: true, starts: 2},
		{name: "failed_custom_handshake", custom: true, fail: true, starts: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			const handshakeTime = 7 * time.Second
			var elapsed atomic.Int64
			base := time.Unix(1000, 0)
			now := func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "synthetic healthy response") }))
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { elapsed.Add(int64(handshakeTime)); return nil, nil }}
			server.StartTLS()
			defer server.Close()
			roots := x509.NewCertPool()
			if !scenario.fail {
				roots.AddCert(server.Certificate())
			}
			config := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			transport := &http.Transport{TLSClientConfig: config, DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			if scenario.custom {
				transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					raw, err := (&net.Dialer{}).DialContext(ctx, network, address)
					if err != nil {
						return nil, err
					}
					host, _, err := net.SplitHostPort(address)
					if err != nil {
						raw.Close()
						return nil, err
					}
					peerConfig := config.Clone()
					peerConfig.ServerName = host
					connection := tls.Client(raw, peerConfig)
					trace := httptrace.ContextClientTrace(ctx)
					if trace != nil && trace.TLSHandshakeStart != nil {
						trace.TLSHandshakeStart()
					}
					err = connection.HandshakeContext(ctx)
					if trace != nil && trace.TLSHandshakeDone != nil {
						trace.TLSHandshakeDone(connection.ConnectionState(), err)
					}
					if err != nil {
						raw.Close()
						return nil, err
					}
					return connection, nil
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctx, progress := traceRequestProgressAt(ctx, now)
			var starts atomic.Int64
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{TLSHandshakeStart: func() { starts.Add(1) }})
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal("synthetic request setup failed")
			}
			response, err := (&http.Client{Transport: transport}).Do(request)
			if response != nil {
				_, _ = io.Copy(io.Discard, response.Body)
				response.Body.Close()
			}
			if (err != nil) != scenario.fail {
				t.Fatalf("unexpected transport outcome: error=%t expected=%t", err != nil, scenario.fail)
			}
			if starts.Load() != scenario.starts {
				t.Fatalf("handshake trace control: starts=%d expected=%d", starts.Load(), scenario.starts)
			}
			result := CheckResult{}
			progress.recordTiming(&result, base, now())
			if result.TlsHandshakeLatency != handshakeTime {
				t.Fatalf("real handshake timing overwritten: tls=%s want=%s start_events=%d", result.TlsHandshakeLatency, handshakeTime, starts.Load())
			}
		})
	}
}
