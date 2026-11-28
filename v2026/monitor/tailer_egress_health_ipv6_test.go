package monitor

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// A dual-stack provider's URL load adds its IPv6 legs to the record
// (qualityprobe/prober): failure_stages= counts ipv6 for a load whose IPv4
// legs passed and whose IPv6 leg failed, ipv6= sums the IPv6 HTTPS legs and
// ipv6_h3= their HTTP/3 legs, and h3= and ipv6_h3= count UDP legs a country
// disables. Each outcome the summary methods can write stays a measurement,
// built here as the prober builds its line.
func TestEgressHealthRecordsWithIpv6LegsRemainMeasurement(t *testing.T) {
	timed := func(check egresshealth.CheckResult) egresshealth.CheckResult {
		check.Name, check.Class, check.DnsMeasured, check.DnsLookupLatency = "synthetic-dual", egresshealth.ClassSite, true, time.Millisecond
		return check
	}
	passedV6 := &egresshealth.CheckResult{Ok: true}
	for _, c := range []struct {
		name   string
		check  egresshealth.CheckResult
		ipv6   string
		ipv6H3 string
		h3     string
	}{
		{"both families pass", timed(egresshealth.CheckResult{Ok: true, Ipv6Required: true, Ipv6: passedV6}), "ok:1", "", ""},
		{"IPv6 connect fails", timed(egresshealth.CheckResult{FailureStage: egresshealth.FailureStageIpv6, Ipv6Required: true,
			Ipv6: &egresshealth.CheckResult{FailureStage: "dial_tcp"}}), "dial_tcp:1", "", ""},
		{"IPv6 TLS fails", timed(egresshealth.CheckResult{FailureStage: egresshealth.FailureStageIpv6, Ipv6Required: true,
			Ipv6: &egresshealth.CheckResult{FailureStage: "tls"}}), "tls:1", "", ""},
		{"IPv6 not measured", timed(egresshealth.CheckResult{Ok: true, Ipv6Required: true,
			Ipv6: &egresshealth.CheckResult{FailureStage: "dial_dns"}}), "not_measured:1", "", ""},
		{"IPv6 skipped", timed(egresshealth.CheckResult{FailureStage: "request_timeout", Ipv6Required: true}), "skipped:1", "", ""},
		{"no AAAA URL", timed(egresshealth.CheckResult{Ok: true, Ipv6NoUrl: true}), "no_url:1", "", ""},
		{"IPv6 drops UDP", timed(egresshealth.CheckResult{FailureStage: egresshealth.FailureStageIpv6, H3Required: true, H3: &egresshealth.CheckResult{Ok: true}, Ipv6Required: true, Ipv6H3Required: true,
			Ipv6: &egresshealth.CheckResult{FailureStage: egresshealth.FailureStageH3, H3Required: true, H3: &egresshealth.CheckResult{FailureStage: egresshealth.FailureStageDialQuic}}}),
			"ok:1", "silent:1", "ok:1"},
		{"the country disables UDP", timed(egresshealth.CheckResult{Ok: true, UdpProbesDisabled: true, Ipv6Required: true, Ipv6UdpProbesDisabled: true, Ipv6: passedV6}),
			"ok:1", "country_disabled:1", "country_disabled:1"},
		// an IPv4 URL without h3 whose IPv6 legs loaded an h3 stand-in
		{"a stand-in's UDP leg", timed(egresshealth.CheckResult{Ok: true, Ipv6Required: true, Ipv6H3Required: true,
			Ipv6: &egresshealth.CheckResult{Ok: true, H3Required: true, H3: &egresshealth.CheckResult{Ok: true}}}),
			"ok:1", "ok:1", ""},
	} {
		result := &egresshealth.Result{TableTotal: 139, Checks: []egresshealth.CheckResult{c.check}}
		result.Total = 1
		summary := egresshealth.ClassSummary{Total: 1}
		if c.check.Ok {
			result.OkCount, summary.Ok = 1, 1
		}
		result.ByClass = map[egresshealth.Class]egresshealth.ClassSummary{egresshealth.ClassSite: summary}
		if got := result.Ipv6Summary(); got != c.ipv6 {
			t.Fatalf("%s: ipv6 summary = %q, want %q", c.name, got, c.ipv6)
		}
		if got := result.Ipv6H3Summary(); got != c.ipv6H3 {
			t.Fatalf("%s: ipv6_h3 summary = %q, want %q", c.name, got, c.ipv6H3)
		}
		if got := result.H3Summary(); got != c.h3 {
			t.Fatalf("%s: h3 summary = %q, want %q", c.name, got, c.h3)
		}
		line := "egress-health: provider=synthetic-provider " + result.Summary()
		if stages := result.FailureStageSummary(); stages != "" {
			line += " failure_stages=" + stages
		}
		if h3 := result.H3Summary(); h3 != "" {
			line += " h3=" + h3
		}
		if ipv6 := result.Ipv6Summary(); ipv6 != "" {
			line += " ipv6=" + ipv6
		}
		if ipv6H3 := result.Ipv6H3Summary(); ipv6H3 != "" {
			line += " ipv6_h3=" + ipv6H3
		}
		line += " " + result.TimingSummary()
		if failed := result.FailedNames(); 0 < len(failed) {
			line += " failed=" + strings.Join(failed, ",")
		}
		for _, record := range []string{line, "[synthetic-host][taskworker][g1][cid:synthetic-correlation][2026-11-11T01:00:00Z]2026/11/11 01:00:00 " + line} {
			tailer := newLogTailer("taskworker", nil)
			for occurrence := 0; occurrence < novelRateThreshold; occurrence += 1 {
				tailer.classify(record)
			}
			if novel := findingByClass(t, tailer.drainWindow(), "novel"); !novel.healthy {
				t.Fatalf("%s: complete egress producer record with IPv6 legs became novel: %s", c.name, record)
			}
		}
	}
	// an outcome the summaries never write, or the fields out of their order,
	// stays eligible
	for _, line := range []string{
		"egress-health: provider=synthetic-provider ok=0/1 site=0/1 failure_stages=ipv6:1 ipv6=unreviewed:1 failed=synthetic-dual",
		"egress-health: provider=synthetic-provider ok=0/1 site=0/1 failure_stages=ipv6:1 ipv6=ok:1 ipv6_h3=unreviewed:1 failed=synthetic-dual",
		"egress-health: provider=synthetic-provider ok=0/1 site=0/1 failure_stages=ipv6:1 ipv6_h3=silent:1 ipv6=ok:1 failed=synthetic-dual",
	} {
		tailer := newLogTailer("taskworker", nil)
		for occurrence := 0; occurrence < novelRateThreshold; occurrence += 1 {
			tailer.classify(line)
		}
		if novel := findingByClass(t, tailer.drainWindow(), "novel"); novel.healthy {
			t.Fatalf("an unreviewed IPv6 record was hidden: %s", line)
		}
	}
}
