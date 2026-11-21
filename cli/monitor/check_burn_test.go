package main

// check-burn's contract with warp_rollout_gate: exit 0 only for clear; burning, unknown, settings that do
// not load and a check that fails are each an error, so the step stops.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/urnetwork/sidecar/dnsservice"

	servermonitor "github.com/urnetwork/server/monitor"
)

// A check answering state with lines, or err.
func fixedCheckBurn(state string, err error, lines ...string) checkBurnFunction {
	return func(ctx context.Context, settings servermonitor.SignalSettings, service string) (*servermonitor.SloBurnCheck, error) {
		if err != nil {
			return nil, err
		}
		return &servermonitor.SloBurnCheck{Service: service, State: state, Lines: lines}, nil
	}
}

// Settings that load.
func loadedCheckBurnSettings() (servermonitor.SignalSettings, error) {
	return servermonitor.SignalSettings{Environment: "synthetic"}, nil
}

// Clear exits 0 with each objective's line; burning and unknown are errors.
func TestCheckBurnExitsZeroOnlyWhenClear(t *testing.T) {
	stdout := &bytes.Buffer{}
	if err := runCheckBurnCommand(t.Context(), []string{"--service=evs"}, stdout, loadedCheckBurnSettings, fixedCheckBurn(servermonitor.SloBurnClear, nil, "availability: clear")); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if want := "evs availability: clear\nevs: clear\n"; stdout.String() != want {
		t.Errorf("output %q, want %q", stdout, want)
	}
	for _, state := range []string{servermonitor.SloBurnBurning, servermonitor.SloBurnUnknown} {
		stdout := &bytes.Buffer{}
		err := runCheckBurnCommand(t.Context(), []string{"--service=evs"}, stdout, loadedCheckBurnSettings, fixedCheckBurn(state, nil))
		if err == nil || !strings.Contains(err.Error(), state) || !strings.Contains(stdout.String(), "evs: "+state) {
			t.Errorf("%s: %v, %q", state, err, stdout)
		}
	}
}

// Settings that do not load, a failed check and a wrong command line are each an error.
func TestCheckBurnFailsClosed(t *testing.T) {
	broken := func() (servermonitor.SignalSettings, error) {
		return servermonitor.SignalSettings{}, errors.New("no monitor.yml")
	}
	if err := runCheckBurnCommand(t.Context(), []string{"--service=evs"}, &bytes.Buffer{}, broken, fixedCheckBurn(servermonitor.SloBurnClear, nil)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("settings that do not load: %v", err)
	}
	if err := runCheckBurnCommand(t.Context(), []string{"--service=evs"}, &bytes.Buffer{}, loadedCheckBurnSettings, fixedCheckBurn("", errors.New("no env"))); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("a failed check: %v", err)
	}
	for _, args := range [][]string{{}, {"--service="}, {"--service=evs", "extra"}, {"--nope"}} {
		if err := runCheckBurnCommand(t.Context(), args, &bytes.Buffer{}, loadedCheckBurnSettings, fixedCheckBurn(servermonitor.SloBurnClear, nil)); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%q: %v", args, err)
		}
	}
}

func TestCheckBurnRequiresTheReadyFirstEdgeAtTheReleaseVersionThroughoutTheCanary(t *testing.T) {
	const ready = `{"service":"dnsedge-1","block":"g1","version":"new","status":"ok"}`
	const root = "zone.example."
	type rolloutCase struct {
		label, start, end    string
		oldAtStart, oldAtEnd string
		peerUdpBehind        bool
		oldCanaryUdp         bool
		oldIpv6Udp           bool
		changedSignature     bool
		staleCanary          bool
		wantChecks           int
		clear                bool
	}
	var active atomic.Pointer[rolloutCase]
	var checks atomic.Int32
	var receipts, seenChallenges sync.Map
	authority := func(ns string) dns.Handler {
		return dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
			tc := active.Load()
			transport := w.LocalAddr().Network()
			serial := uint32(7)
			if ns == "ns2" && tc.peerUdpBehind && transport == "udp" {
				serial--
			}
			reply := new(dns.Msg)
			reply.SetReply(q)
			reply.Authoritative = true
			reply.Compress = true
			soa := &dns.SOA{Hdr: dns.RR_Header{Name: root, Rrtype: dns.TypeSOA, Class: dns.ClassINET}, Ns: "ns1." + root, Mbox: "hostmaster." + root, Serial: serial}
			if ns == "ns1" {
				owner := q.Question[0].Name
				nonce, _, _ := strings.Cut(owner, ".")
				hash := sha256.Sum256([]byte(owner))
				sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: owner, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET}, SignerName: root, Signature: base64.StdEncoding.EncodeToString(hash[:])}
				var data dns.RR
				old := tc.oldAtStart
				if checks.Load() != 0 {
					old = tc.oldAtEnd
				}
				if tc.oldIpv6Udp && netip.MustParseAddrPort(w.LocalAddr().String()).Addr().Is6() {
					old = "udp"
				}
				if q.Question[0].Qtype == dns.TypeTXT {
					sig.TypeCovered = dns.TypeTXT
					written := time.Now()
					if tc.staleCanary {
						written = written.Add(-24 * time.Hour)
					}
					data = &dns.TXT{Hdr: dns.RR_Header{Name: owner, Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{strconv.FormatInt(written.UnixMilli(), 10) + " synthetic"}}
					reply.Answer = []dns.RR{data, sig}
					if tc.oldCanaryUdp {
						old = "udp"
					}
				} else {
					if _, reused := seenChallenges.LoadOrStore(nonce, true); reused {
						t.Error("rollout reused a public challenge")
					}
					sig.TypeCovered = dns.TypeNSEC
					data = &dns.NSEC{Hdr: dns.RR_Header{Name: owner, Rrtype: dns.TypeNSEC, Class: dns.ClassINET}, NextDomain: "\\000." + owner, TypeBitMap: []uint16{dns.TypeRRSIG, dns.TypeNSEC, dns.TypeNXNAME}}
					reply.Ns = []dns.RR{soa, data, sig}
				}
				if !strings.Contains(old, transport) {
					receipts.Store(nonce+" "+root, strconv.FormatUint(uint64(serial), 10)+" "+dnsservice.DnsRolloutRecordProof(data, sig))
				}
				if tc.changedSignature && sig.TypeCovered == dns.TypeTXT && transport == "udp" {
					sig.OrigTtl++
				}
			} else {
				reply.Answer = []dns.RR{soa}
			}
			if err := w.WriteMsg(reply); err != nil {
				t.Error(err)
			}
		})
	}
	// Each address can route the two transports independently while private HTTP is ready.
	start := func(ns, family, address string) netip.AddrPort {
		tcp, err := net.Listen("tcp"+family, address)
		if err != nil {
			t.Fatal(err)
		}
		udp, err := net.ListenPacket("udp"+family, tcp.Addr().String())
		if err != nil {
			tcp.Close()
			t.Fatal(err)
		}
		handler := authority(ns)
		for _, srv := range []*dns.Server{{Listener: tcp, Handler: handler}, {PacketConn: udp, Handler: handler}} {
			ready := make(chan struct{})
			srv.NotifyStartedFunc = func() { close(ready) }
			done := make(chan error, 1)
			go func() { done <- srv.ActivateAndServe() }()
			select {
			case <-ready:
			case err := <-done:
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := srv.Shutdown(); err != nil {
					t.Error(err)
				}
				if err := <-done; err != nil {
					t.Error(err)
				}
			})
		}
		return tcp.Addr().(*net.TCPAddr).AddrPort()
	}
	settings := servermonitor.DefaultDnsCanarySettings()
	settings.Dns = dnsservice.DefaultDnsSettings()
	settings.Dns.Roots = []string{root}
	settings.NsAddresses = map[string][]netip.AddrPort{
		"ns1": {start("ns1", "4", "127.0.0.1:0"), start("ns1", "6", "[::1]:0")},
		"ns2": {start("ns2", "4", "127.0.0.1:0")},
	}
	load := func() (servermonitor.SignalSettings, error) {
		return servermonitor.SignalSettings{Environment: "synthetic", DnsCanary: settings}, nil
	}
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		if value, ok := receipts.LoadAndDelete(r.Header.Get(dnsservice.DnsRolloutProofHeader)); ok {
			w.Header().Set(dnsservice.DnsRolloutProofHeader, value.(string))
		}
		tc := active.Load()
		if checks.Load() == 0 {
			fmt.Fprint(w, tc.start)
		} else {
			fmt.Fprint(w, tc.end)
		}
	}))
	defer private.Close()
	for _, tc := range []rolloutCase{
		{label: "ready", start: ready, end: ready, wantChecks: 1, clear: true},
		{label: "old selector", start: strings.Replace(ready, "new", "old", 1), end: ready},
		{label: "starting", start: strings.Replace(ready, "ok", "error not ready", 1), end: ready},
		{label: "wrong authority", start: strings.Replace(ready, "dnsedge-1", "dnsedge-2", 1), end: ready},
		{label: "old public DNS", start: ready, end: ready, oldAtStart: "tcp udp"},
		{label: "old public UDP", start: ready, end: ready, oldAtStart: "udp"},
		{label: "only IPv6 UDP is old", start: ready, end: ready, oldIpv6Udp: true},
		{label: "old public TCP", start: ready, end: ready, oldAtStart: "tcp"},
		{label: "UDP moved during canary", start: ready, end: ready, oldAtEnd: "udp", wantChecks: 1},
		{label: "challenge reached new edge but UDP TXT came from another signer", start: ready, end: ready, oldCanaryUdp: true, wantChecks: 1},
		{label: "new edge serves stale TXT", start: ready, end: ready, staleCanary: true, wantChecks: 1},
		{label: "public UDP TXT has different signature parameters", start: ready, end: ready, changedSignature: true, wantChecks: 1},
		{label: "peer UDP serial differs", start: ready, end: ready, peerUdpBehind: true},
		{label: "rolled back", start: ready, end: strings.Replace(ready, "new", "old", 1), wantChecks: 1},
		{label: "drained", start: ready, end: strings.Replace(ready, "ok", "draining", 1), wantChecks: 1},
	} {
		checks.Store(0)
		active.Store(&tc)
		check := func(ctx context.Context, settings servermonitor.SignalSettings, service string) (*servermonitor.SloBurnCheck, error) {
			checks.Add(1)
			return fixedCheckBurn(servermonitor.SloBurnClear, nil)(ctx, settings, service)
		}
		var output bytes.Buffer
		err := runCheckBurnCommand(t.Context(), []string{"--service=dnsedge-1", "--expected-version=new", "--status-url=" + private.URL + "/status"}, &output, load, check)
		if (err == nil) != tc.clear || int(checks.Load()) != tc.wantChecks || strings.Contains(output.String(), "dnsedge-1/g1 serving new tcp+udp dns-canary clear\n") != tc.clear {
			t.Errorf("%s: result %v, canary ran %d times: %q", tc.label, err, checks.Load(), output.String())
		}
	}
}
