package acceptance

import (
	"context"
	"errors"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestWireGuardOriginTraceUsesInnerPeer(t *testing.T) {
	transport, _, _, _ := acceptanceDNSWireGuardPair(t, false, false)
	trace := &httpsRequestTrace{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, httpsRequestTraceContextKey{}, trace)
	connection, err := transport.stack.DialContext(ctx, "tcp", "acceptance-dns.invalid:80")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if trace.originAddress != netip.MustParseAddrPort("1.1.1.1:80") {
		t.Fatalf("inner origin = %s; want 1.1.1.1:80, not the UDP proxy endpoint", trace.originAddress)
	}
	local := netip.MustParseAddrPort(connection.LocalAddr().String())
	if trace.originLocalPort == 0 || trace.originLocalPort != local.Port() {
		t.Fatalf("inner source port = %d; want connected TCP port %d", trace.originLocalPort, local.Port())
	}
}

// Another protocol or later cleanup can change the aggregate route before
// terminal diagnostics run. Only RPCs entirely inside this request's lifetime
// qualify, and multiple local window channels remain destination candidates.
func TestHostedDeviceRequestRoutesRespectFailureBoundary(t *testing.T) {
	started := time.Date(2026, 9, 24, 20, 11, 25, 0, time.UTC)
	tracker := &sdkHostedDeviceTracker{}
	tracker.recordRequestRouteSample(started.Add(-time.Second), started.Add(-time.Millisecond),
		"destinations=[142.251.218.195->before(1)]")
	tracker.recordRequestRouteSample(started.Add(time.Millisecond), started.Add(2*time.Millisecond),
		"destinations=[142.251.218.195->p29(1),142.251.218.195->p45(2),142.251.218.196->unrelated(1)]")
	tracker.recordRequestRouteSample(started.Add(19*time.Millisecond), started.Add(21*time.Millisecond),
		"destinations=[142.251.218.195->straddled(1)]")
	tracker.recordRequestRouteSample(started.Add(21*time.Millisecond), started.Add(22*time.Millisecond),
		"destinations=[142.251.218.195->after(1)]")
	cause := errors.New("TLS verification failed")
	request := &httpsRequestFailure{
		started: started, finished: started.Add(20 * time.Millisecond),
		originAddress: netip.MustParseAddrPort("142.251.218.195:443"), cause: cause,
	}
	wrapped := withHostedDeviceDiagnostics(request, tracker)
	if !errors.Is(wrapped, cause) {
		t.Fatalf("request route lost underlying rejection: %v", wrapped)
	}
	for _, want := range []string{
		"origin=142.251.218.195:443 scope=destination-aggregate",
		"candidate_kind=local-channel",
		"2026-09-24T20:11:25.001Z/2026-09-24T20:11:25.002Z candidates=p29(1),p45(2)",
	} {
		if !strings.Contains(wrapped.Error(), want) {
			t.Errorf("request route lacks %q: %v", want, wrapped)
		}
	}
	for _, outside := range []string{"before(1)", "after(1)", "straddled(1)", "unrelated(1)"} {
		if strings.Contains(wrapped.Error(), outside) {
			t.Errorf("request route borrowed %q: %v", outside, wrapped)
		}
	}
	request.started = started.Add(30 * time.Millisecond)
	request.finished = started.Add(40 * time.Millisecond)
	if got := tracker.RequestDiagnostic(request); !strings.Contains(got, "samples=none") {
		t.Fatalf("missing sample borrowed an earlier route: %q", got)
	}
}

func TestHTTPSProxyListenerDoesNotBecomeOriginRoute(t *testing.T) {
	started := time.Now()
	trace := &httpsRequestTrace{started: started}
	callback := trace.clientTrace()
	callback.ConnectStart("tcp", "192.0.2.1:7130")
	callback.ConnectDone("tcp", "192.0.2.1:7130", nil)
	callback.GotConn(httptrace.GotConnInfo{})
	err := trace.wrap(errors.New("request failed"), started.Add(time.Millisecond))
	if !strings.Contains(err.Error(), "origin unavailable") {
		t.Fatalf("proxy listener mistaken for origin: %v", err)
	}
	tracker := &sdkHostedDeviceTracker{}
	tracker.recordRequestRouteSample(started, started, "destinations=[192.0.2.1->p29(1)]")
	wrapped := withHostedDeviceDiagnostics(err, tracker)
	if !strings.Contains(wrapped.Error(), "request route: {origin=unavailable candidates=unavailable reason=origin-not-observed}") {
		t.Fatalf("proxy listener selected an origin route: %v", wrapped)
	}
}
