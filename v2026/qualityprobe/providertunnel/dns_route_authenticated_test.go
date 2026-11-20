package providertunnel

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
	"golang.org/x/net/http2"
)

// Real certificate verification, TLS, HTTP/2, the DoH cache and RFC8484 parsing
// run on in-memory sockets. Route admission is a real monitor event. The Tun
// stack, TCP packets and production scheduling are deliberately not modeled.
func TestProviderDnsWaitsForAuthenticatedRoute(t *testing.T) {
	trusted, roots := routeWaitCertificate(t)
	untrusted, _ := routeWaitCertificate(t)
	cases := []struct {
		name       string
		admitAfter time.Duration
		cancelAt   time.Duration
		mode       string
		wantAnswer bool
	}{
		{name: "warm", wantAnswer: true},
		{name: "early_cold", admitAfter: 5 * time.Second, wantAnswer: true},
		{name: "second_wave", admitAfter: 16 * time.Second, wantAnswer: true},
		{name: "healthy_35_seconds", admitAfter: 35 * time.Second, wantAnswer: true},
		{name: "partial_resolvers", admitAfter: 5 * time.Second, mode: "partial", wantAnswer: true},
		{name: "partial_resolvers_35_seconds", admitAfter: 35 * time.Second, mode: "partial", wantAnswer: true},
		{name: "partial_empty_and_stalled", admitAfter: 5 * time.Second, mode: "partial_empty"},
		{name: "authoritative_empty", admitAfter: 5 * time.Second, mode: "empty"},
		{name: "authentication_failure", admitAfter: 5 * time.Second, mode: "bad_tls"},
		{name: "never_ready", admitAfter: -1},
		{name: "after_allowance", admitAfter: 50 * time.Second},
		{name: "cancel_forming", admitAfter: -1, cancelAt: 2 * time.Second},
	}
	for _, candidate := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("candidate_%t/%s", candidate, tc.name), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					harnessWorkers := routeTestBubbleWorkers(t)
					start := time.Now()
					ctx, cancel := context.WithDeadline(t.Context(), start.Add(time.Minute))
					defer cancel()
					lifetime, stop := context.WithCancel(t.Context())
					defer stop()
					monitor := connect.NewRemoteUserNatMultiClientMonitorWithDefaults()
					ready := make(chan struct{})
					admit := func() {
						id := connect.NewId()
						monitor.AddProviderEvent(id, connect.ProviderStateAdded, id, nil, connect.IpFamilyV4Only)
						close(ready)
					}
					if tc.admitAfter == 0 {
						admit()
					} else if tc.admitAfter > 0 {
						go func() {
							select {
							case <-lifetime.Done():
							case <-time.After(tc.admitAfter):
								admit()
							}
						}()
					}
					if tc.cancelAt > 0 {
						go func() {
							select {
							case <-lifetime.Done():
							case <-time.After(tc.cancelAt):
								cancel()
							}
						}()
					}

					var dials, preAdmissionDials, tlsDone, wireRequests, authenticatedAnswers atomic.Int64
					var workers sync.WaitGroup
					fixtureErrors := make(chan error, 32)
					httpServer := &http.Server{}
					h2Server := &http2.Server{}
					// ConfigureServer owns the error-channel pool per server; the bare
					// ServeConn fallback pool cannot cross independent synctest bubbles.
					if err := http2.ConfigureServer(httpServer, h2Server); err != nil {
						t.Fatal(err)
					}
					settings := connect.DefaultDohSettings()
					settings.Log = connect.NewNoopLogger()
					settings.IpVersion = 4
					settings.DnsResolverSettings = &connect.DnsResolverSettings{
						EnableRemoteDoh: true,
						RemoteDohUrlsIpv4: []string{
							"https://192.0.2.1/dns-query", "https://192.0.2.2/dns-query",
							"https://192.0.2.3/dns-query", "https://192.0.2.4/dns-query",
						},
						TlsConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
					}
					settings.DialContextSettings = &connect.DialContextSettings{
						DialContext: func(dialCtx context.Context, network, address string) (net.Conn, error) {
							dials.Add(1)
							select {
							case <-ready:
							default:
								preAdmissionDials.Add(1)
							}
							select {
							case <-dialCtx.Done():
								return nil, dialCtx.Err()
							case <-lifetime.Done():
								return nil, context.Canceled
							case <-ready:
							}
							operator := -1
							for i := 0; i < 4; i++ {
								if address == fmt.Sprintf("192.0.2.%d:443", i+1) {
									operator = i
								}
							}
							if operator < 0 || network != "tcp" && network != "tcp4" {
								return nil, errors.New("unexpected synthetic DoH transport")
							}
							local, remote := net.Pipe()
							workers.Add(1)
							go func() {
								defer workers.Done()
								defer remote.Close()
								stopClose := context.AfterFunc(lifetime, func() { remote.Close() })
								defer stopClose()
								certificate := trusted
								if tc.mode == "bad_tls" || (tc.mode == "partial" || tc.mode == "partial_empty") && operator == 0 {
									certificate = untrusted
								}
								server := tls.Server(remote, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}})
								if err := server.HandshakeContext(lifetime); err != nil {
									// Authentication rejection, abandoned detached dials and
									// cleanup cancellation are expected transport outcomes.
									return
								}
								tlsDone.Add(1)
								if server.ConnectionState().NegotiatedProtocol != "h2" {
									fixtureErrors <- errors.New("synthetic server did not negotiate HTTP/2")
									return
								}
								h2Server.ServeConn(server, &http2.ServeConnOpts{
									BaseConfig: httpServer,
									Context:    lifetime,
									Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
										wireRequests.Add(1)
										if tc.mode == "partial" && operator == 2 || tc.mode == "partial_empty" && operator >= 2 {
											select {
											case <-request.Context().Done():
											case <-lifetime.Done():
											}
											return
										}
										empty := tc.mode == "empty" || (tc.mode == "partial" || tc.mode == "partial_empty") && operator == 1
										body, err := routeWaitDnsResponse(request, empty)
										if err != nil {
											fixtureErrors <- err
											return
										}
										select {
										case <-request.Context().Done():
											return
										case <-lifetime.Done():
											return
										case <-time.After(100 * time.Millisecond):
										}
										w.Header().Set("Content-Type", "application/dns-message")
										if _, err := w.Write(body); err == nil && !empty {
											authenticatedAnswers.Add(1)
										}
									}),
								})
							}()
							return local, nil
						},
					}
					cache := connect.NewDohCache(settings)
					defer func() {
						stop()
						cache.Close()
						workers.Wait()
						drainRouteTestHttpTimers(t, settings.RequestTimeout, harnessWorkers)
					}()
					observations := &DnsObservations{}
					queries, targetDials := 0, 0
					socketStop := errors.New("synthetic website socket reached")
					resolver := &providerUrlResolver{
						query: func(queryCtx context.Context, recordType, host string) ([]netip.Addr, bool) {
							queries++
							return cache.QueryResult(queryCtx, recordType, host)
						},
						dial: func(dialCtx context.Context, network, address string, addrs []netip.Addr) (net.Conn, error) {
							targetDials++
							deadline, ok := dialCtx.Deadline()
							if !ok || !deadline.Equal(start.Add(time.Minute)) || dialCtx.Err() != nil || len(addrs) != 1 || addrs[0] != netip.MustParseAddr("198.51.100.91") {
								return nil, errors.New("website deadline or authenticated DNS answer changed")
							}
							return nil, socketStop
						},
						observations: observations,
						routeState: func() dnsRouteSnapshot {
							window, providers := monitor.Events()
							return dnsRouteFromMonitor(nil, window, providers)
						},
					}
					if candidate {
						resolver.waitRoute = func(ctx context.Context) error { return waitProviderRoute(ctx, nil, monitor) }
					}
					requestCtx, trace := traceProviderHttpDial(ctx, "tcp", "sample.example:443")
					_, err := resolver.dialContext(requestCtx, "tcp", "sample.example:443", trace)
					trace.finish(err)
					elapsed := time.Since(start)
					stop()
					cache.Close()
					workers.Wait()
					select {
					case fixtureErr := <-fixtureErrors:
						t.Fatalf("wire fixture failed: %v", fixtureErr)
					default:
					}
					if tc.wantAnswer {
						if !errors.Is(err, socketStop) || targetDials != 1 || authenticatedAnswers.Load() < 1 || tlsDone.Load() < 1 {
							t.Fatalf("authenticated healthy route failed: elapsed=%s queries=%d error=%v", elapsed, queries, err)
						}
						if elapsed < tc.admitAfter || elapsed > tc.admitAfter+time.Second {
							t.Fatalf("healthy recovery changed: elapsed=%s admission=%s", elapsed, tc.admitAfter)
						}
					} else if targetDials != 0 {
						t.Fatal("unresolved or unauthenticated DNS reached website")
					} else if tc.cancelAt > 0 {
						if !errors.Is(err, context.Canceled) || elapsed != tc.cancelAt {
							t.Fatalf("request cancellation changed: elapsed=%s error=%v", elapsed, err)
						}
					} else {
						var dnsErr *net.DNSError
						if !errors.As(err, &dnsErr) || dnsErr.IsNotFound != (tc.mode == "empty") {
							t.Fatalf("DNS failure authority changed: %v", err)
						}
						if tc.mode == "empty" {
							if elapsed < tc.admitAfter || elapsed > tc.admitAfter+time.Second {
								t.Fatalf("complete resolver response changed: elapsed=%s", elapsed)
							}
						} else if tc.mode == "bad_tls" {
							// TLS rejection over zero-buffer pipes need not complete a
							// response exchange. Both modes must keep the original DNS
							// allowance and expose no HTTP bytes or website dial.
							if ctx.Err() != nil || elapsed < tc.admitAfter || elapsed > 46*time.Second {
								t.Fatalf("untrusted TLS exceeded DNS allowance: elapsed=%s", elapsed)
							}
						} else if ctx.Err() != nil || elapsed < 45*time.Second || elapsed > 46*time.Second {
							t.Fatalf("original DNS allowance changed: elapsed=%s request_error=%v", elapsed, ctx.Err())
						}
					}
					if tc.mode == "bad_tls" && wireRequests.Load() != 0 {
						t.Fatal("untrusted TLS exposed an HTTP request")
					}
					var waves uint64
					var waveSeconds float64
					for _, value := range observations.RouteTimingSnapshot() {
						waves += value.Count
						waveSeconds += value.BeforeCurrentAdmissionSeconds + value.AfterCurrentAdmissionSeconds + value.UnattributedSeconds
					}
					if waveSeconds > elapsed.Seconds()+0.001 || elapsed.Seconds()-waveSeconds > 0.401 {
						t.Fatalf("waiting disappeared from DNS clock: elapsed=%s waves=%v", elapsed, waveSeconds)
					}
					t.Logf("candidate=%t elapsed_ms=%d dns_waves=%d resolver_queries=%d transport_dials=%d pre_admission_dials=%d tls_completed=%d wire_requests=%d target_dials=%d", candidate, elapsed.Milliseconds(), waves, queries, dials.Load(), preAdmissionDials.Load(), tlsDone.Load(), wireRequests.Load(), targetDials)
					// The resource boundary is transport creation before admission.
					// Neither its count nor synthetic time is a Main capacity estimate.
					if candidate && preAdmissionDials.Load() != 0 {
						t.Errorf("DNS transport allocated before any admitted route: %d", preAdmissionDials.Load())
					}
					if !candidate && tc.admitAfter != 0 && preAdmissionDials.Load() == 0 {
						t.Error("baseline cold transport work was not exercised")
					}
				})
			})
		}
	}
}

func routeWaitCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Unix(0, 0), NotAfter: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for i := 1; i <= 4; i++ {
		template.IPAddresses = append(template.IPAddresses, net.ParseIP(fmt.Sprintf("192.0.2.%d", i)))
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, roots
}
