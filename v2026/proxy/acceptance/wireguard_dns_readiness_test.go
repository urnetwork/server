package acceptance

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func dnsMetadataMustBePrivate(t *testing.T, text string) {
	t.Helper()
	for _, secret := range []string{"10.42.13.7", "192.0.2.91", "203.0.113.19", "54321", "54322", "SECRET-DNS", "PRIVATE-ERROR"} {
		if strings.Contains(text, secret) {
			t.Fatalf("metadata contains forbidden value %q", secret)
		}
	}
}

func TestWireGuardDNSQuotedTupleMismatchFields(t *testing.T) {
	for _, test := range []struct {
		name, fields string
		mutate       func([]byte)
	}{
		{"matched", "none", func([]byte) {}},
		{"source address", "source_address", func(p []byte) { p[40]++ }},
		{"source port", "source_port", func(p []byte) { binary.BigEndian.PutUint16(p[48:50], 54322) }},
		{"remote address", "remote_address", func(p []byte) { p[44]++ }},
		{"remote port", "remote_port", func(p []byte) { binary.BigEndian.PutUint16(p[50:52], 54) }},
		{"two fields", "remote_address+remote_port", func(p []byte) { p[44]++; binary.BigEndian.PutUint16(p[50:52], 54) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := dnsTraceStack()
			r := s.startDNSRequest()
			r.exchange(&dnsTraceConn{port: 54321}, false)
			packet := dnsTraceICMP(54321, 3, 3)
			test.mutate(packet)
			s.observePacket(packet, false)
			text := r.freeze()
			dnsTraceContains(t, text, "proto=1 bytes=56 icmp=3/3", "local_envelope=true closest_active_fields="+test.fields, "closed_current_tuple=false prior_request_tuple=not_observed", "generation=unproven")
			if (r.packets[0].match != 0) != (test.name == "matched") {
				t.Fatal("field mismatch changed exact active-tuple attribution")
			}
			dnsMetadataMustBePrivate(t, text)
		})
	}
}

type dnsOtherRemoteConn struct{ dnsTraceConn }

func (c *dnsOtherRemoteConn) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(192, 0, 2, 92), Port: 53}
}

func TestWireGuardDNSQuotedTupleAmbiguityAndKnownClosed(t *testing.T) {
	s := dnsTraceStack()
	r := s.startDNSRequest()
	first := r.exchange(&dnsTraceConn{port: 54321}, false)
	r.exchange(&dnsOtherRemoteConn{dnsTraceConn{port: 54322}}, false)
	packet := dnsTraceICMP(54321, 3, 3)
	packet[47]++
	s.observePacket(packet, false)
	if got := dnsTupleAlternatives(r.packets[0].closestActive); got != "source_port|remote_address" {
		t.Fatalf("equally close sockets lost their alternatives: %s", got)
	}
	first.close()
	s.observePacket(dnsTraceICMP(54321, 3, 3), false)
	if !r.packets[1].closedCurrent || r.packets[1].match != 0 {
		t.Fatal("known closed tuple was promoted to active ownership")
	}
	// Reuse is explicitly both active and known closed; neither match proves
	// which generation produced the packet.
	r.exchange(&dnsTraceConn{port: 54321}, false)
	s.observePacket(dnsTraceICMP(54321, 3, 3), false)
	if !r.packets[2].closedCurrent || r.packets[2].match != 1 {
		t.Fatal("tuple reuse concealed the closed generation")
	}
	text := r.freeze()
	dnsTraceContains(t, text, "closest_active_fields=source_port|remote_address", "closed_current_tuple=true", "generation=unproven")
	dnsMetadataMustBePrivate(t, text)
}

func TestWireGuardDNSReadinessDistinguishesPriorCloseFromRequestFreeze(t *testing.T) {
	s, window := dnsTraceStack(), new(wireGuardDNSReadiness)
	first := s.startDNSRequestInWindow(window)
	x := first.exchange(&dnsTraceConn{port: 54321}, false)
	first.freeze() // HTTP request ended, but its resolver socket is still open.
	second := s.startDNSRequestInWindow(window)
	second.exchange(&dnsTraceConn{port: 54322}, false)
	s.observePacket(dnsTraceICMP(54321, 3, 3), false)
	if second.packets[0].prior != 1 || second.packets[0].match != 0 {
		t.Fatal("request completion falsely retired an open prior socket")
	}
	x.close()
	s.observePacket(dnsTraceICMP(54321, 3, 3), false)
	if second.packets[1].prior != 2 {
		t.Fatal("actual close of a frozen request was not retained")
	}
	third := s.startDNSRequestInWindow(window)
	third.exchange(&dnsTraceConn{port: 54321}, false)
	s.observePacket(dnsTraceICMP(54321, 3, 3), false)
	if second.packets[2].prior != 3 {
		t.Fatal("prior active/closed tuple reuse lost generation ambiguity")
	}
	dnsTraceContains(t, second.freeze(), "prior_request_tuple=active", "prior_request_tuple=closed", "prior_request_tuple=active_and_closed")
	third.freeze()
	text := window.freeze()
	if window.flowN != 0 || window.flows != [wireGuardDNSHistoryCap]wireGuardDNSHistory{} {
		t.Fatal("readiness completion retained raw tuples")
	}
	late := s.startDNSRequestInWindow(window)
	late.exchange(&dnsTraceConn{port: 54321}, false).close()
	late.freeze()
	if window.freeze() != text || window.flowN != 0 {
		t.Fatal("late request rewrote frozen readiness evidence")
	}
	dnsMetadataMustBePrivate(t, text)
}

// Standalone and overlap campaigns create separate netstacks and readiness
// histories, but use the same provisioned peer address. A late teardown from
// standalone therefore has the hosted source-port-only signature even though
// no component has changed a port. "Not observed" cannot mean "not old".
func TestWireGuardDNSReadinessPreviousStackIsOutsideHistory(t *testing.T) {
	oldStack, oldWindow := dnsTraceStack(), new(wireGuardDNSReadiness)
	oldRequest := oldStack.startDNSRequestInWindow(oldWindow)
	oldSocket := oldRequest.exchange(&dnsTraceConn{port: 54321}, false)
	oldSocket.close()
	oldRequest.freeze()
	oldWindow.freeze()
	if oldWindow.flowN != 0 {
		t.Fatal("previous campaign retained raw socket tuples")
	}
	freshStack, freshWindow := dnsTraceStack(), new(wireGuardDNSReadiness)
	request := freshStack.startDNSRequestInWindow(freshWindow)
	request.exchange(&dnsTraceConn{port: 54322}, false)
	freshStack.observePacket(dnsTraceICMP(54321, 3, 3), false)
	packet := request.packets[0]
	if !packet.localEnvelope || packet.match != 0 || packet.closedCurrent || packet.prior != 0 || dnsTupleAlternatives(packet.closestActive) != "source_port" {
		t.Fatal("a prior-stack quote was mistaken for an active, current-closed or observed prior-request tuple")
	}
	text := request.freeze()
	dnsTraceContains(t, text, "proto=1 bytes=56 icmp=3/3", "active_dns_tuple=unmatched", "closest_active_fields=source_port", "closed_current_tuple=false prior_request_tuple=not_observed")
	dnsMetadataMustBePrivate(t, text)
	text = freshWindow.freeze()
	dnsTraceContains(t, text, "sampled_packets=1 matched=0 closed_current=0 prior_active=0 prior_closed=0 closest_active_fields=[source_port:1]")
	dnsMetadataMustBePrivate(t, text)
}

func TestWireGuardDNSReadinessBoundsAndOmissions(t *testing.T) {
	s, window := dnsTraceStack(), new(wireGuardDNSReadiness)
	for i := range 20 {
		r := s.startDNSRequestInWindow(window)
		for j := range wireGuardDNSCap {
			r.exchange(&dnsTraceConn{port: 54321 + j}, false)
		}
		r.lookupEvent(true, nil)
		s.observePacket(dnsTraceICMP(54321, 3, 3), false)
		r.freeze()
		if i == 19 && !window.flowLimited {
			t.Fatal("history overflow was not explicit")
		}
	}
	text := window.freeze()
	dnsTraceContains(t, text, "started=20 frozen_attempts=20 omitted_samples=12 flow_limit=true text_limit=false", "exchanges=160 sampled_packets=20", "a1{", "a4{", "a17{", "a20{")
	if strings.Contains(text, "a5{") || len(text) > wireGuardDNSReadinessTextCap {
		t.Fatal("readiness sample retention was not first four/last four and bounded")
	}
	dnsMetadataMustBePrivate(t, text)

	// Stress the renderer's byte bound independently of realistic counters.
	stress := new(wireGuardDNSReadiness)
	for i := range stress.counts.mismatch {
		stress.counts.mismatch[i] = ^uint64(0)
	}
	stress.records, stress.requests, stress.sampleN = 8, 8, 8
	for i := range stress.samples {
		stress.samples[i] = wireGuardDNSReadinessSample{request: uint64(i + 1), counts: stress.counts}
	}
	text = stress.freeze()
	if len(text) > wireGuardDNSReadinessTextCap || !strings.Contains(text, "text_limit=true") || strings.Contains(text, "omitted_samples=0") {
		t.Fatal("verbose classes were not dropped as whole, explicitly omitted samples")
	}
}

func TestWireGuardDNSReadinessConcurrentFreezeAndClose(t *testing.T) {
	s, window := dnsTraceStack(), new(wireGuardDNSReadiness)
	r := s.startDNSRequestInWindow(window)
	x := r.exchange(&dnsTraceConn{port: 54321}, false)
	var workers sync.WaitGroup
	for i := range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 50 {
				switch i {
				case 0:
					s.observePacket(dnsTraceICMP(54321, 3, 3), false)
				case 1:
					x.close()
				case 2:
					r.freeze()
				case 3:
					window.freeze()
				}
			}
		}()
	}
	workers.Wait()
	if window.flowN != 0 || len(window.freeze()) > wireGuardDNSReadinessTextCap {
		t.Fatal("concurrent freeze leaked tuple history or text")
	}
}

func TestProbeHTTPSDNSReadinessRetainsEarlierAttempts(t *testing.T) {
	s := dnsTraceStack()
	sentinel := errors.New("controlled DNS deadline")
	requests, waits := 0, 0
	var window *wireGuardDNSReadiness
	transport := &wireGuardDiagnosticTransport{stack: s, roundTripper: acceptanceRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests++
		window, _ = request.Context().Value(wireGuardDNSReadinessKey{}).(*wireGuardDNSReadiness)
		if window == nil {
			t.Fatal("readiness request has no bounded observer")
		}
		r := request.Context().Value(wireGuardDNSContextKey{}).(*wireGuardDNSRequest)
		x := r.exchange(&dnsTraceConn{port: 54320 + requests}, false)
		r.lookupEvent(true, nil)
		x.ioEvent(58, nil, true)
		packet := dnsTraceICMP(54321, 3, 3)
		if requests == 1 {
			packet[40]++ // first attempt: un-restored-looking quoted source
		}
		s.observePacket(packet, false)
		x.ioEvent(0, context.DeadlineExceeded, false)
		r.lookupEvent(false, context.DeadlineExceeded)
		x.close()
		return nil, sentinel
	})}
	count, err := probeHTTPSCampaign(context.Background(), "WireGuard", "https://synthetic.invalid/", transport, 2*time.Minute, 0, time.Second,
		func(_ context.Context, duration time.Duration) error {
			waits++
			if duration != readinessRetryInterval {
				t.Fatal("diagnostics changed readiness retry policy")
			}
			if waits == 2 {
				return context.DeadlineExceeded // deterministic end, no wall-clock sleep
			}
			return nil
		}, nil, nil)
	if count != 0 || requests != 2 || waits != 2 || !errors.Is(err, sentinel) {
		t.Fatalf("readiness semantics changed: successes=%d requests=%d waits=%d err=%v", count, requests, waits, err)
	}
	text := err.Error()
	dnsTraceContains(t, text, "within 2m0s", "prior_request_tuple=closed", "started=2 frozen_attempts=2 omitted_samples=0", "source_address:1", "source_port:1", "a1{", "a2{")
	dnsMetadataMustBePrivate(t, text)
	if window.flowN != 0 || !window.frozen {
		t.Fatal("failed readiness retained tuple history")
	}
	path := filepath.Join(t.TempDir(), "results.tsv")
	if err := WriteResults(path, []Result{{Case: "wireguard", Status: "FAIL", Detail: text}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dnsTraceContains(t, string(data), "source_address:1", "source_port:1", "a1{", "a2{")
}

func TestProbeHTTPSDNSReadinessDoesNotRetainThroughSoak(t *testing.T) {
	s, requests := dnsTraceStack(), 0
	var window *wireGuardDNSReadiness
	transport := &wireGuardDiagnosticTransport{stack: s, roundTripper: acceptanceRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests++
		current, _ := request.Context().Value(wireGuardDNSReadinessKey{}).(*wireGuardDNSReadiness)
		if requests == 1 {
			window = current
			r := request.Context().Value(wireGuardDNSContextKey{}).(*wireGuardDNSRequest)
			r.exchange(&dnsTraceConn{port: 54321}, false)
		} else if current != nil || window == nil || !window.frozen || window.flowN != 0 {
			t.Fatal("successful readiness leaked tuple retention into sustained requests")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})}
	count, err := probeHTTPSCampaign(context.Background(), "WireGuard", "https://synthetic.invalid/", transport, 2*time.Minute, time.Second, time.Second,
		func(context.Context, time.Duration) error { return nil }, nil, nil)
	if err != nil || count != 2 || requests != 2 {
		t.Fatalf("successful readiness/soak changed: count=%d requests=%d err=%v", count, requests, err)
	}
}
