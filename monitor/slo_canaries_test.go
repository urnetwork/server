package monitor

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/urnetwork/sidecar/dnsservice"
)

func TestSloBurnWarmTlsComparesTwoVerifiedWarmPaths(t *testing.T) {
	target := &SloWarmTlsSettings{ServerName: "published.example", Edge: "192.0.2.1:443", Direct: "192.0.2.2:443"}
	settings := loadSidecarSloSettings(&sidecarServicesMonitorYaml{Services: []string{"tlsedge"}, WarmTls: target})
	if settings.ConfigError != "" || settings.WarmTls == nil {
		t.Fatalf("warm TLS config: %+v", settings)
	}
	for _, failAt := range []int{-1, 0, 1, 2, 3} {
		now := time.Unix(0, 0)
		var addresses []string
		handshake := func(ctx context.Context, network, address, name string) (TLSCertificateObservation, error) {
			if network != "tcp" || name != target.ServerName {
				t.Fatalf("unexpected handshake identity %s %s", network, name)
			}
			i := len(addresses)
			addresses = append(addresses, address)
			now = now.Add([]time.Duration{time.Second, 2 * time.Second, 400 * time.Millisecond, 700 * time.Millisecond}[i])
			peer := TLSCertificateObservation{Certificates: [][]byte{{1}}}
			if i == failAt {
				peer.VerifyError = errors.New("unverified peer")
			}
			return peer, nil
		}
		overhead, ok := target.measure(t.Context(), handshake, func() time.Time { return now })
		if failAt < 0 {
			if !ok || overhead != 300*time.Millisecond || !slices.Equal(addresses, []string{target.Direct, target.Edge, target.Direct, target.Edge}) {
				t.Fatalf("warm measurement: %v %t %v", overhead, ok, addresses)
			}
		} else if ok {
			t.Fatalf("accepted unverified handshake %d", failAt)
		}
	}
	target.Direct = target.Edge
	if target.valid() {
		t.Fatal("the same path was accepted as the direct control")
	}
	if _, ok := target.measure(t.Context(), nil, time.Now); ok {
		t.Fatal("an invalid programmatic target produced an observation")
	}
}

func TestSloBurnDnsPropagationObservesTheCommittedValueAtBothAuthorities(t *testing.T) {
	check, _ := newDnsCanaryTestCheck(t, dnsservice.DnsCanarySourceSite)
	settings := check.settings
	// Fake authority reads never dial. On success both need to see this exact nonce.
	root, value := settings.Dns.Roots[0], "1 site"
	owner := dnsservice.DnsCanaryOwner(root)
	reply := &dns.Msg{MsgHdr: dns.MsgHdr{Authoritative: true}, Answer: []dns.RR{
		&dns.TXT{Hdr: dns.RR_Header{Name: owner}, Txt: []string{"0 site"}},
	}}
	if authoritativeCanaryPresent(reply, owner, value) {
		t.Fatal("the previous write counted as serving this one")
	}
	reply.Answer = append(reply.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: owner}, Txt: []string{"1 ", "site"}})
	if !authoritativeCanaryPresent(reply, owner, value) {
		t.Fatal("the matching authoritative TXT was missed")
	}
	reply.Authoritative = false
	if authoritativeCanaryPresent(reply, owner, value) {
		t.Fatal("a cached answer counted as authoritative")
	}
	seen := []netip.AddrPort{}
	read := func(ctx context.Context, gotRoot string, addresses []netip.AddrPort, gotValue string) bool {
		if root != gotRoot || value != gotValue {
			t.Fatalf("read another write: %s %s", gotRoot, gotValue)
		}
		seen = append(seen, addresses[0])
		return true
	}
	if got := observeDnsPropagation(t.Context(), time.Now(), settings, root, value, read); got != "on_time" || len(seen) != 2 || seen[0] == seen[1] {
		t.Fatalf("immediate answer %s at %v", got, seen)
	}
	if got := observeDnsPropagation(t.Context(), time.Now().Add(-2*settings.PropagationTarget), settings, root, value, read); got != "late" || len(seen) != 2 {
		t.Fatalf("write past the deadline: %s", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	misses := 0
	partial := func(ctx context.Context, _ string, _ []netip.AddrPort, _ string) bool {
		misses++
		if misses == 1 {
			return true
		}
		cancel()
		return false
	}
	if got := observeDnsPropagation(ctx, time.Now(), settings, root, value, partial); got != "" || misses != 2 {
		t.Fatalf("one authority and an interrupted monitor reported %s", got)
	}
}
