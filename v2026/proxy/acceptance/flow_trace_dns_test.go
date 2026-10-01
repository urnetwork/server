package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/proxy/flowtrace"
)

func TestFlowTraceDNSRequiresPositiveServerCapability(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var start flowtrace.StartRequest
			if json.NewDecoder(r.Body).Decode(&start) != nil || !start.IncludeDns || start.Port != 443 || r.Header.Get("Authorization") != "Bearer test-only" {
				t.Error("explicit DNS request or authentication missing")
			}
			_ = json.NewEncoder(w).Encode(flowtrace.Snapshot{Session: "test-session", DnsEnabled: enabled})
		}))
		client := &flowTraceClient{url: api.URL, token: "test-only", client: api.Client()}
		err := client.start(context.Background(), 443)
		api.Close()
		if enabled != (err == nil) || client.dnsEnabled != enabled || (!enabled && client.session != "") {
			t.Fatalf("legacy server claimed DNS capability: enabled=%t client=%+v err=%v", enabled, client, err)
		}
	}
}

func TestFlowTraceDNSDisarmsExactGeneration(t *testing.T) {
	requests := 0
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodDelete || r.Header.Get("Authorization") != "Bearer test-only" || r.Header.Get("X-UR-Flow-Trace") != "test-session" {
			t.Errorf("disarm request method=%s authorization=%q generation=%q", r.Method, r.Header.Get("Authorization"), r.Header.Get("X-UR-Flow-Trace"))
		}
		_ = json.NewEncoder(w).Encode(flowtrace.Snapshot{Session: "test-session"})
	}))
	defer api.Close()
	client := &flowTraceClient{url: api.URL, token: "test-only", session: "test-session", client: api.Client()}
	if err := client.stop(); err != nil || requests != 1 {
		t.Fatalf("disarm err=%v requests=%d", err, requests)
	}
}

func TestFlowTraceDNSRequestKeysFreezeWithoutResurrectingClosedSocket(t *testing.T) {
	r := &wireGuardDNSRequest{stack: &wireGuardStack{}, providerMetadata: true, flowN: 1}
	r.flows[0] = wireGuardDNSFlow{local: netip.MustParseAddrPort("192.0.2.2:45001"), remote: netip.MustParseAddrPort("198.51.100.53:53"), active: true}
	x := &wireGuardDNSExchange{request: r, index: 0}
	query := []byte{0, 7, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	x.wroteQuery(query, len(query), nil)
	clear(query)
	x.close()
	r.freeze()
	keys := r.providerQueries()
	if len(keys) != 1 || keys[0].Id != 7 || keys[0].Active || keys[0].LocalPort != 45001 {
		t.Fatalf("request-local metadata lost ownership boundary: %+v", keys)
	}
	x.wroteQuery([]byte{0, 9, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 12, nil)
	if r.providerQueries()[0].Id != 7 {
		t.Fatal("late write mutated frozen query")
	}
}

// Exercise real resolver writes through an encrypted loopback WireGuard pair,
// not only synthetic metadata calls. The opt-in wrapper must reach the DNS
// socket and publish its frozen keys even when resolution fails.
func TestFlowTraceDNSRealWireGuardRequestCorrelation(t *testing.T) {
	for _, test := range []struct {
		name             string
		enabled, missing bool
	}{
		{"disabled", false, false}, {"answer", true, false}, {"nxdomain", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			hostLookups := failAcceptanceHostDNS(t)
			transport, udp, _, origin := acceptanceDNSWireGuardPair(t, false, test.missing)
			trace := &httpsRequestTrace{}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctx = context.WithValue(ctx, httpsRequestTraceContextKey{}, trace)
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://acceptance-dns.invalid/", nil)
			if err != nil {
				t.Fatal(err)
			}
			wrapper := flowTraceTransport{protocol: "wireguard", transport: transport, trace: &flowTraceClient{dnsEnabled: test.enabled}}
			response, err := wrapper.RoundTrip(request)
			if response != nil {
				response.Body.Close()
			}
			if (err != nil) != test.missing || udp.Load() == 0 || hostLookups.Load() != 0 || (!test.missing && origin.Load() != 1) {
				t.Fatalf("changed real tunnel outcome: err=%v UDP=%d host=%d origin=%d", err, udp.Load(), hostLookups.Load(), origin.Load())
			}
			trace.stateLock.Lock()
			queries := append([]flowtrace.DnsQuery(nil), trace.dnsQueries...)
			trace.stateLock.Unlock()
			if test.enabled != (len(queries) > 0) {
				t.Fatalf("request opt-in=%t produced queries=%+v", test.enabled, queries)
			}
			for _, query := range queries {
				if query.LocalPort == 0 || query.Resolver != netip.MustParseAddrPort("1.1.1.1:53") || query.Active {
					t.Fatalf("real resolver key missing or resurrected after close: %+v", query)
				}
			}
		})
	}
}

func TestFlowTraceDNSIncompleteAndRepeatedLocalQueriesFailClosed(t *testing.T) {
	for _, kind := range []string{"disabled", "missing", "partial", "repeated", "tcp", "limited"} {
		r := &wireGuardDNSRequest{stack: &wireGuardStack{}, providerMetadata: kind != "disabled", flowN: 1}
		r.flows[0] = wireGuardDNSFlow{local: netip.MustParseAddrPort("192.0.2.2:45001"), remote: netip.MustParseAddrPort("198.51.100.53:53"), active: true, tcp: kind == "tcp"}
		x := &wireGuardDNSExchange{request: r, index: 0}
		query := make([]byte, 12)
		if kind != "missing" {
			n := len(query)
			if kind == "partial" {
				n--
			}
			x.wroteQuery(query, n, nil)
		}
		if kind == "repeated" {
			x.wroteQuery(query, len(query), nil)
		}
		r.limited = kind == "limited"
		r.freeze()
		if keys := r.providerQueries(); len(keys) != 0 {
			t.Errorf("%s claimed query metadata: %+v", kind, keys)
		}
	}
}

func TestFlowTraceDNSFailurePreservesCauseAndLabelsUnprovenICMP(t *testing.T) {
	now := time.Now()
	flow := flowtrace.Flow{Local: netip.MustParseAddrPort("192.0.2.2:45001"), Origin: netip.MustParseAddrPort("198.51.100.53:53")}
	snapshot := flowtrace.Snapshot{DnsEnabled: true, Now: now, Until: now.Add(time.Minute), Events: []flowtrace.Event{
		{Kind: "dns_egress", Flow: flow, Dns: &flowtrace.DnsMetadata{Id: 7, IdKnown: true, Admission: "accepted"}},
		{Kind: "dns_icmp", Flow: flow, Dns: &flowtrace.DnsMetadata{IcmpType: 3, IcmpCode: 3}},
	}}
	err := withDnsFlowEvidence(context.DeadlineExceeded, snapshot, []flowtrace.DnsQuery{{LocalPort: 45001, Resolver: flow.Origin, Id: 7}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("diagnostic changed deadline cause")
	}
	for _, want := range []string{"replies=0", "socket_active_at_freeze=false", "tuple_only_icmp_generation_unproven=1", "admission_not_upstream_write=true", "correlation=tuple+dns-id-not-authenticated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("DNS evidence lacks %q: %v", want, err)
		}
	}
}
