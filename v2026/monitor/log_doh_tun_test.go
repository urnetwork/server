// A Proxy device's tun path resolves through the customer's selected provider.
// Its DoH leg timeouts are provider-path evidence, not a platform DNS page;
// platform-owned paths keep the page. Every line here is synthetic.
package monitor

import (
	"strings"
	"testing"
)

// A device pinned to an offline or retired provider times out every tun leg,
// and the per-process, per-target throttle keeps one such device above the
// page threshold. On Main on 2026-10-12 every Proxy process logged ~20/min
// such legs while logical tun resolver calls had zero timeouts and the host
// path answered every call. The Proxy tun path must warn, not page.
func TestProxyTunDohDialTimeoutIsProviderPathEvidence(t *testing.T) {
	tailer := newLogTailer("proxy", nil)
	line := "[synthetic-host.example][proxy][synthetic-block][cid:private-fixture] " +
		"[family]dial tag=doh observable=v1 path=tun attempted_family=4 result=timeout resolver_owner=tun resolver_scope=address resolver_outcome=canceled err=i/o timeout"
	for range 20 {
		tailer.classify(line)
	}
	findings := tailer.drainWindow()
	for _, finding := range findings {
		if finding.class == "doh-dial-timeout" && !finding.healthy {
			t.Fatal("a Proxy tun-path leg timeout paged as a platform DoH failure")
		}
	}
	finding := findingByClass(t, findings, "doh-tun-dial-timeout")
	if finding.healthy || finding.tier != tierWarn || finding.frame != "doh-tun-attempt" {
		t.Fatalf("tun-path legs lost their bounded warning: healthy=%t tier=%s frame=%s", finding.healthy, finding.tier, finding.frame)
	}
	// a measured timeout must not hide other log incidents' recovery
	alert := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", finding)
	if alert.Severity != SeverityWarn || isVisibilityAlert(alert) {
		t.Fatalf("measured tun timeout must remain an observable WARN: severity=%s visibility=%t", alert.Severity, isVisibilityAlert(alert))
	}
	for _, want := range []string{"path=tun", "resolver_outcome=canceled"} {
		if !strings.Contains(finding.evidence, want) {
			t.Errorf("bounded sample omitted %s: %s", want, finding.evidence)
		}
	}
	for _, private := range []string{"synthetic-host.example", "synthetic-block", "private-fixture", "cid:"} {
		if strings.Contains(finding.evidence, private) {
			t.Fatal("outer identity entered bounded DoH evidence")
		}
	}
}

// Platform-owned Proxy paths keep the unchanged page.
func TestProxyHostDohDialTimeoutStillPages(t *testing.T) {
	for _, path := range []string{"host", "caller", "unknown"} {
		tailer := newLogTailer("proxy", nil)
		for range 10 {
			tailer.classify("[family]dial tag=doh observable=v1 path=" + path + " attempted_family=4 result=timeout resolver_owner=" + path + " resolver_scope=address resolver_outcome=pending err=i/o timeout")
		}
		findings := tailer.drainWindow()
		finding := findingByClass(t, findings, "doh-dial-timeout")
		if finding.healthy || finding.tier != tierPage || finding.frame != "doh-attempt" {
			t.Fatalf("path=%s lost its page: healthy=%t tier=%s frame=%s", path, finding.healthy, finding.tier, finding.frame)
		}
		for _, other := range findings {
			if other.class == "doh-tun-dial-timeout" && !other.healthy {
				t.Fatalf("path=%s was classified as provider-path evidence", path)
			}
		}
	}
}
