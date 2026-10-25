package providertunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

func urlPhaseTestContext(t *testing.T, ctx context.Context, host string) context.Context {
	t.Helper()
	target, err := url.Parse("https://" + host + "/")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = (&providerHttpTransport{}).ProviderUrlProbeContext(ctx, target, false)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func waitUrlPhase(ctx context.Context, delay time.Duration) error {
	if delay == 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-time.After(delay):
		return nil
	}
}

// A single DNS budget includes route waiting and all retries. The standalone
// path retains its former healthy 35-second recovery allowance.
func TestUrlProbeDnsPhaseBudget(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		scoped                       bool
		route, answer, owner, cancel time.Duration
		want                         time.Duration
		ok                           bool
	}{
		{name: "url_answer_4", scoped: true, answer: 4 * time.Second, owner: time.Minute, want: 4 * time.Second, ok: true},
		{name: "url_late_6", scoped: true, answer: 6 * time.Second, owner: time.Minute, want: 5 * time.Second},
		{name: "url_never", scoped: true, answer: time.Hour, owner: time.Minute, want: 5 * time.Second},
		{name: "route_and_dns_share_budget", scoped: true, route: 4 * time.Second, answer: 2 * time.Second, owner: time.Minute, want: 5 * time.Second},
		{name: "short_parent", scoped: true, answer: time.Hour, owner: 3 * time.Second},
		{name: "no_parent_deadline", scoped: true, answer: time.Hour, want: 5 * time.Second},
		{name: "canceled", scoped: true, answer: time.Hour, owner: time.Minute, cancel: 2 * time.Second, want: 2 * time.Second},
		{name: "non_url_35", answer: 35 * time.Second, owner: time.Minute, want: 35 * time.Second, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.owner > 0 {
					var end context.CancelFunc
					ctx, end = context.WithTimeout(ctx, tc.owner)
					defer end()
				}
				if tc.scoped {
					ctx = urlPhaseTestContext(t, ctx, "phase.example")
				}
				if tc.cancel > 0 {
					timer := time.AfterFunc(tc.cancel, cancel)
					defer timer.Stop()
				}
				start := time.Now()
				ready := start.Add(tc.route)
				answer := ready.Add(tc.answer)
				dials := 0
				stop := errors.New("resolved socket boundary")
				resolver := &providerUrlResolver{
					waitRoute: func(ctx context.Context) error { return waitUrlPhase(ctx, max(0, time.Until(ready))) },
					query: func(ctx context.Context, _, _ string) ([]netip.Addr, bool) {
						if waitUrlPhase(ctx, max(0, time.Until(answer))) != nil {
							return nil, false
						}
						return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, true
					},
					dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
						dials++
						return nil, stop
					},
				}
				ctx, trace := traceProviderHttpDial(ctx, "tcp", "phase.example:443")
				_, err := resolver.dialContext(ctx, "tcp", "phase.example:443", trace)
				stage := trace.finish(err)
				if (tc.want > 0 && time.Since(start) != tc.want) || (tc.want == 0 && time.Since(start) > tc.owner) || errors.Is(err, stop) != tc.ok || (dials == 1) != tc.ok {
					t.Fatalf("elapsed=%s want=%s dials=%d err=%v", time.Since(start), tc.want, dials, err)
				}
				if !tc.ok && stage != "dial_dns" {
					t.Fatalf("DNS failure changed class: %s", stage)
				}
				if !tc.ok && tc.cancel == 0 && tc.want > 0 {
					var timeout interface{ Timeout() bool }
					if !errors.As(err, &timeout) || !timeout.Timeout() {
						t.Fatalf("DNS budget lost timeout classification: %v", err)
					}
				}
			})
		})
	}
}

func TestUrlProbeTcpBudgetStartsAfterDns(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		dns, tcp, owner, want, cancel time.Duration
		literal, scoped, success      bool
	}{
		{name: "separate_budgets", dns: 4 * time.Second, tcp: 2 * time.Second, owner: time.Minute, want: 6 * time.Second, scoped: true, success: true},
		{name: "tcp_timeout", dns: 4 * time.Second, tcp: 4 * time.Second, owner: time.Minute, want: 7 * time.Second, scoped: true},
		{name: "short_owner", dns: 4 * time.Second, tcp: 4 * time.Second, owner: 6 * time.Second, want: 6 * time.Second, scoped: true},
		{name: "literal", tcp: 20 * time.Second, owner: time.Minute, want: 3 * time.Second, literal: true, scoped: true},
		{name: "canceled_tcp", dns: 4 * time.Second, tcp: time.Minute, owner: time.Minute, cancel: 5 * time.Second, want: 5 * time.Second, scoped: true},
		{name: "non_url", tcp: 20 * time.Second, owner: time.Minute, want: 20 * time.Second, success: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), tc.owner)
				defer cancel()
				if tc.cancel > 0 {
					timer := time.AfterFunc(tc.cancel, cancel)
					defer timer.Stop()
				}
				host := "phase.example"
				if tc.literal {
					host = "8.8.8.8"
				}
				if tc.scoped {
					ctx = urlPhaseTestContext(t, ctx, host)
				}
				stop := errors.New("socket connected")
				answerAt := time.Now().Add(tc.dns)
				resolver := &providerUrlResolver{
					query: func(ctx context.Context, _, _ string) ([]netip.Addr, bool) {
						if tc.literal {
							t.Error("literal address invoked DNS")
						}
						if waitUrlPhase(ctx, max(0, time.Until(answerAt))) != nil {
							return nil, false
						}
						return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, true
					},
					dial: func(ctx context.Context, _, _ string, _ []netip.Addr) (net.Conn, error) {
						if err := waitUrlPhase(ctx, tc.tcp); err != nil {
							return nil, err
						}
						return nil, stop
					},
				}
				ctx, trace := traceProviderHttpDial(ctx, "tcp", host+":443")
				start := time.Now()
				_, err := resolver.dialContext(ctx, "tcp", host+":443", trace)
				trace.finish(err)
				if time.Since(start) != tc.want || errors.Is(err, stop) != tc.success {
					t.Fatalf("elapsed=%s want=%s err=%v", time.Since(start), tc.want, err)
				}
				if tc.cancel > 0 && !errors.Is(err, context.Canceled) {
					t.Fatalf("TCP cancellation lost: %v", err)
				}
			})
		})
	}
}

type urlPhaseFixture struct {
	dns, tcp, handshake, headers, bodyGap time.Duration
	body                                  string
	chunkSize                             int
	connections                           atomic.Int64
	workers                               sync.WaitGroup
	lifetime                              context.Context
	stop                                  context.CancelFunc
}

// Real WebPKI verification, TLS, HTTP/1, request ownership and URL result
// classification run on net.Pipe. No host socket, provider or Main dependency.
func (self *urlPhaseFixture) client(t *testing.T) *http.Client {
	t.Helper()
	requireInjectedSystemTrust(t)
	self.lifetime, self.stop = context.WithCancel(context.Background())
	leaf, key := issueLeaf(t, "phase.example")
	certificate := tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}
	resolver := &providerUrlResolver{
		query: func(ctx context.Context, _, _ string) ([]netip.Addr, bool) {
			if waitUrlPhase(ctx, self.dns) != nil {
				return nil, false
			}
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, true
		},
		dial: func(ctx context.Context, _, _ string, _ []netip.Addr) (net.Conn, error) {
			if err := waitUrlPhase(ctx, self.tcp); err != nil {
				return nil, err
			}
			left, right := net.Pipe()
			self.connections.Add(1)
			self.workers.Add(1)
			go func() {
				defer self.workers.Done()
				defer self.connections.Add(-1)
				defer right.Close()
				stop := context.AfterFunc(self.lifetime, func() { right.Close() })
				defer stop()
				if waitUrlPhase(self.lifetime, self.handshake) != nil {
					return
				}
				conn := tls.Server(right, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
				if conn.HandshakeContext(self.lifetime) != nil {
					return
				}
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					return
				}
				if waitUrlPhase(self.lifetime, self.headers) != nil {
					return
				}
				if _, err := fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(self.body)); err != nil {
					return
				}
				chunkSize := self.chunkSize
				if chunkSize <= 0 {
					chunkSize = max(1, len(self.body))
				}
				for remaining := self.body; len(remaining) > 0; {
					if waitUrlPhase(self.lifetime, self.bodyGap) != nil {
						return
					}
					n := min(chunkSize, len(remaining))
					if _, err := io.WriteString(conn, remaining[:n]); err != nil {
						return
					}
					remaining = remaining[n:]
				}
			}()
			return left, nil
		},
	}
	client := httpClientOverDialerWithResolver(nil, resolver, map[string][]string{"phase.example": {SpkiPin(leaf)}}, nil, time.Minute)
	t.Cleanup(func() {
		client.CloseIdleConnections()
		self.stop()
		self.workers.Wait()
		synctest.Wait()
		if self.connections.Load() != 0 {
			t.Error("fixture connections survived terminal cleanup")
		}
	})
	return client
}

func TestUrlProbeTlsPhaseBudget(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		scoped               bool
		delay, owner, cancel time.Duration
		want                 time.Duration
		success              bool
	}{
		{name: "url_healthy", scoped: true, delay: 2 * time.Second, owner: time.Minute, want: 2 * time.Second, success: true},
		{name: "url_late", scoped: true, delay: 4 * time.Second, owner: time.Minute, want: 3 * time.Second},
		{name: "short_owner", scoped: true, delay: 20 * time.Second, owner: 2 * time.Second, want: 2 * time.Second},
		{name: "canceled_tls", scoped: true, delay: time.Minute, owner: time.Minute, cancel: time.Second, want: time.Second},
		{name: "non_url", delay: 20 * time.Second, owner: time.Minute, want: 20 * time.Second, success: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := &urlPhaseFixture{handshake: tc.delay, body: "ok"}
				client := fixture.client(t)
				ctx, cancel := context.WithTimeout(context.Background(), tc.owner)
				defer cancel()
				if tc.cancel > 0 {
					timer := time.AfterFunc(tc.cancel, cancel)
					defer timer.Stop()
				}
				if tc.scoped {
					ctx = urlPhaseTestContext(t, ctx, "phase.example")
				}
				request, _ := http.NewRequestWithContext(ctx, "GET", "https://phase.example/", nil)
				start := time.Now()
				response, err := client.Do(request)
				elapsed := time.Since(start)
				if response != nil {
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if response.TLS == nil || !response.TLS.HandshakeComplete || len(response.TLS.VerifiedChains) == 0 {
						t.Error("TLS connection state lost")
					}
				}
				if elapsed != tc.want || (err == nil) != tc.success {
					t.Fatalf("elapsed=%s want=%s err=%v", elapsed, tc.want, err)
				}
				if !tc.success && tc.owner > 5*time.Second && tc.cancel == 0 {
					requireProbeHttpStage(t, err, "tls")
				}
				if tc.cancel > 0 && !errors.Is(err, context.Canceled) {
					t.Fatalf("TLS cancellation lost: %v", err)
				}
			})
		})
	}
}

func TestUrlProbeReadIdleBudgetAndTotalOwner(t *testing.T) {
	for _, tc := range []struct {
		name        string
		scoped      bool
		fixture     *urlPhaseFixture
		owner, want time.Duration
		success     bool
	}{
		{name: "header_stall", scoped: true, fixture: &urlPhaseFixture{headers: time.Minute, body: "ok"}, owner: time.Minute, want: 5 * time.Second},
		{name: "body_stall", scoped: true, fixture: &urlPhaseFixture{bodyGap: time.Minute, body: "ok"}, owner: time.Minute, want: 5 * time.Second},
		{name: "progress_refreshes_read", scoped: true, fixture: &urlPhaseFixture{bodyGap: 4 * time.Second, body: "four", chunkSize: 1}, owner: time.Minute, want: 16 * time.Second, success: true},
		{name: "short_owner", scoped: true, fixture: &urlPhaseFixture{bodyGap: time.Minute, body: "ok"}, owner: 3 * time.Second, want: 3 * time.Second},
		{name: "all_phases_progress", scoped: true, fixture: &urlPhaseFixture{dns: 4 * time.Second, tcp: 2 * time.Second, handshake: 2 * time.Second, headers: 4 * time.Second, bodyGap: 4 * time.Second, body: "ok"}, owner: 20 * time.Second, want: 16 * time.Second, success: true},
		{name: "header_budget_after_setup", scoped: true, fixture: &urlPhaseFixture{dns: 4 * time.Second, tcp: 2 * time.Second, handshake: 2 * time.Second, headers: time.Minute, body: "ok"}, owner: time.Minute, want: 13 * time.Second},
		{name: "owner_clips_header_after_setup", scoped: true, fixture: &urlPhaseFixture{dns: time.Second, tcp: 2 * time.Second, handshake: 2 * time.Second, headers: time.Minute, body: "ok"}, owner: 7 * time.Second, want: 7 * time.Second},
		{name: "cumulative_owner", scoped: true, fixture: &urlPhaseFixture{dns: 4 * time.Second, tcp: 2 * time.Second, handshake: 2 * time.Second, headers: 4 * time.Second, bodyGap: 4 * time.Second, body: "sixteen-byte-body", chunkSize: 1}, owner: time.Minute, want: time.Minute},
		{name: "non_url_read", fixture: &urlPhaseFixture{headers: 20 * time.Second, body: "ok"}, owner: time.Minute, want: 20 * time.Second, success: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := tc.fixture.client(t)
				ctx, cancel := context.WithTimeout(context.Background(), tc.owner)
				defer cancel()
				if tc.scoped {
					ctx = urlPhaseTestContext(t, ctx, "phase.example")
				}
				request, _ := http.NewRequestWithContext(ctx, "GET", "https://phase.example/", nil)
				start := time.Now()
				response, err := client.Do(request)
				if response != nil {
					_, err = io.ReadAll(response.Body)
					response.Body.Close()
					if response.TLS == nil || !response.TLS.HandshakeComplete || len(response.TLS.VerifiedChains) == 0 {
						t.Error("TLS connection state lost")
					}
				}
				if time.Since(start) != tc.want || (err == nil) != tc.success {
					t.Fatalf("elapsed=%s want=%s err=%v", time.Since(start), tc.want, err)
				}
				if !tc.success {
					var timeout interface{ Timeout() bool }
					if !errors.As(err, &timeout) || !timeout.Timeout() {
						t.Fatalf("read lost timeout classification: %v", err)
					}
				}
			})
		})
	}
}

func TestUrlProbePhaseTimeoutMeasuredClassification(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		fixture     *urlPhaseFixture
		elapsed     time.Duration
	}{
		{name: "late_dns", fixture: &urlPhaseFixture{dns: 6 * time.Second}, stage: "dial_dns", elapsed: 5 * time.Second},
		// This custom dialer has no Tun progress callback; retain the combined class.
		{name: "late_tcp", fixture: &urlPhaseFixture{tcp: 4 * time.Second}, stage: "dial_dns_or_socket", elapsed: 3 * time.Second},
		{name: "late_tls", fixture: &urlPhaseFixture{handshake: 4 * time.Second}, stage: "tls", elapsed: 3 * time.Second},
		{name: "headers", fixture: &urlPhaseFixture{headers: time.Minute, body: "ok"}, stage: "request_response_timeout", elapsed: 5 * time.Second},
		{name: "body", fixture: &urlPhaseFixture{bodyGap: time.Minute, body: "ok"}, stage: "response_body", elapsed: 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := tc.fixture.client(t)
				started := time.Now()
				result, err := egresshealth.Check(context.Background(), client, egresshealth.Options{
					UrlProbe: true, LoadAttempts: 1, Concurrency: 1, PerRequestTimeout: 10 * time.Second,
					ColdStartTimeout: time.Minute, Budget: time.Minute,
					Destinations: []egresshealth.Destination{{Name: "synthetic", Class: egresshealth.ClassSite, Url: "https://phase.example/"}},
				})
				if err != nil || result == nil || result.Total != 1 || result.OkCount != 0 || result.NotMeasured != 0 || len(result.Checks) != 1 {
					t.Fatalf("lost one measured negative: result=%+v err=%v", result, err)
				}
				if elapsed := time.Since(started); elapsed != tc.elapsed {
					t.Fatalf("measured phase elapsed=%s want=%s", elapsed, tc.elapsed)
				}
				if result.Checks[0].FailureStage != tc.stage || result.TlsAuthenticationFailure {
					t.Fatalf("stage=%s tls_failure=%t error=%s", result.Checks[0].FailureStage, result.TlsAuthenticationFailure, result.Checks[0].Err)
				}
				if err := result.UrlProbeEvidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
					t.Fatalf("negative evidence rejected: %v", err)
				}
			})
		})
	}
}

// A buffered loopback TCP connection lets a rejected TLS peer finish sending
// its certificate flight while the client returns its fatal alert. net.Pipe's
// unbuffered writes cannot model that simultaneous exchange.
func TestUrlProbePhaseTimeoutPreservesTlsAuthentication(t *testing.T) {
	requireInjectedSystemTrust(t)
	leaf, key := issueLeaf(t, "phase.example")
	address, closeServer := startTlsTestServer(t, leaf, key)
	defer closeServer()
	resolver := &providerUrlResolver{
		query: func(context.Context, string, string) ([]netip.Addr, bool) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, true
		},
		dial: func(ctx context.Context, _, _ string, _ []netip.Addr) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", address)
		},
	}
	client := httpClientOverDialerWithResolver(nil, resolver, map[string][]string{"phase.example": {"deliberately-wrong-pin"}}, nil, time.Minute)
	defer client.CloseIdleConnections()
	result, err := egresshealth.Check(context.Background(), client, egresshealth.Options{
		UrlProbe: true, LoadAttempts: 1, Concurrency: 1, Budget: time.Minute,
		Destinations: []egresshealth.Destination{{Name: "synthetic", Class: egresshealth.ClassSite, Url: "https://phase.example/"}},
	})
	if err != nil || result == nil || result.Total != 1 || result.OkCount != 0 || result.NotMeasured != 0 || !result.TlsAuthenticationFailure {
		t.Fatalf("TLS authentication failure lost: result=%+v err=%v", result, err)
	}
	if err := result.UrlProbeEvidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
		t.Fatal(err)
	}
}
