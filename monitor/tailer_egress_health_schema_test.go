package monitor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// Unknown errors containing a tally still go through the real novel window.
func TestEgressHealthTallyDoesNotHideUnmatchedTrailingError(t *testing.T) {
	lines := []string{
		"2026/11/02 01:00:00 egress-health: provider=synthetic-provider ok=1/1 site=1/1 error=synthetic-observer-failure",
		"synthetic observer error: quoted egress-health: provider=synthetic-provider ok=1/1 site=1/1",
	}
	for index, line := range lines {
		tailer := newLogTailer("taskworker", nil)
		for _, class := range logClasses {
			applies := len(class.services) == 0
			for _, service := range class.services {
				applies = applies || service == "taskworker"
			}
			if applies && class.re.MatchString(line) && (class.match == nil || class.match(line)) {
				t.Fatalf("egress reproducer case %d unexpectedly matches known class %s", index, class.name)
			}
		}
		for occurrence := 0; occurrence < novelRateThreshold; occurrence += 1 {
			tailer.classify(line)
		}
		if novel := findingByClass(t, tailer.drainWindow(), "novel"); novel.healthy {
			t.Fatalf("egress tally substring hid unmatched error case %d", index)
		}
	}
}

// Real summary methods retain measured failures without claiming healthy egress.
func TestEgressHealthCompleteProducerRecordsRemainMeasurement(t *testing.T) {
	result := &egresshealth.Result{
		OkCount: 0, Total: 1, TableTotal: 139,
		ByClass: map[egresshealth.Class]egresshealth.ClassSummary{egresshealth.ClassSite: {Ok: 0, Total: 1}},
		Checks:  []egresshealth.CheckResult{{Name: "synthetic-site", FailureStage: "request_timeout", DnsMeasured: true, DnsLookupLatency: time.Millisecond}},
	}
	line := "egress-health: provider=synthetic-provider " + result.Summary() + " failure_stages=" + result.FailureStageSummary() + " " + result.TimingSummary() + " failed=" + strings.Join(result.FailedNames(), ",")
	lines := []string{
		line,
		"2026/11/02 01:00:00 " + line,
		"[synthetic-host][taskworker][g1][cid:synthetic-correlation][2026-11-02T01:00:00Z]2026/11/02 01:00:00 " + line,
		"egress-health: provider=synthetic-provider ok=0/0 failed=synthetic-site -- nothing measured; not submitted",
		"egress-health: provider=synthetic-provider ok=0/0 failed=synthetic-site -- quality unmeasured; publishing independent URL security evidence",
		"egress-health: provider=synthetic-provider ok=9/11 dns=3/3 connectivity=2/2 cdn=1/3 site=3/3 table=139 retried=1 not_measured=1 short=dns,site canary=0/1 exit=unobserved tls_authentication_failure=true failure_stages=unknown:4 first_byte=1 connect_ttfb_ms=1.0 ttfb_ms=2.0 failed=synthetic-site not-measured=synthetic-other canary-failed=synthetic-canary",
	}
	for index, line := range lines {
		tailer := newLogTailer("taskworker", nil)
		for occurrence := 0; occurrence < novelRateThreshold; occurrence += 1 {
			tailer.classify(line)
		}
		if novel := findingByClass(t, tailer.drainWindow(), "novel"); !novel.healthy {
			t.Fatalf("complete egress producer record case %d became novel", index)
		}
	}
}

// A URL that declares HTTP/3 adds its leg to the record (qualityprobe/prober):
// failure_stages= counts h3 for a load whose HTTPS leg passed and whose HTTP/3
// leg heard nothing, and h3= sums every leg. Each outcome the summary methods
// can write stays a measurement, built here as the prober builds its line.
func TestEgressHealthRecordsWithHttp3LegsRemainMeasurement(t *testing.T) {
	timed := func(check egresshealth.CheckResult) egresshealth.CheckResult {
		check.Name, check.Class, check.DnsMeasured, check.DnsLookupLatency = "synthetic-h3", egresshealth.ClassSite, true, time.Millisecond
		check.H3Required = true
		return check
	}
	for _, c := range []struct {
		name  string
		check egresshealth.CheckResult
		h3    string
	}{
		{"silent", timed(egresshealth.CheckResult{FailureStage: egresshealth.FailureStageH3, H3: &egresshealth.CheckResult{FailureStage: egresshealth.FailureStageDialQuic}}), "silent:1"},
		{"passed", timed(egresshealth.CheckResult{Ok: true, H3: &egresshealth.CheckResult{Ok: true}}), "ok:1"},
		{"answered with a TLS alert", timed(egresshealth.CheckResult{Ok: true, H3: &egresshealth.CheckResult{FailureStage: "tls"}}), "tls:1"},
		{"answered and reset", timed(egresshealth.CheckResult{Ok: true, H3: &egresshealth.CheckResult{FailureStage: egresshealth.FailureStageQuic}}), "quic:1"},
		{"answered and stalled", timed(egresshealth.CheckResult{Ok: true, H3: &egresshealth.CheckResult{FailureStage: "request_tls_timeout"}}), "request_tls_timeout:1"},
		{"skipped", timed(egresshealth.CheckResult{FailureStage: "request_timeout"}), "skipped:1"},
		{"not measured", timed(egresshealth.CheckResult{NotMeasured: true, FailureStage: egresshealth.FailureStageH3, H3: &egresshealth.CheckResult{NotMeasured: true, FailureStage: egresshealth.FailureStageH3Unavailable}}), "not_measured:1"},
	} {
		result := &egresshealth.Result{TableTotal: 139, Checks: []egresshealth.CheckResult{c.check}}
		if !c.check.NotMeasured {
			result.Total = 1
			summary := egresshealth.ClassSummary{Total: 1}
			if c.check.Ok {
				result.OkCount, summary.Ok = 1, 1
			}
			result.ByClass = map[egresshealth.Class]egresshealth.ClassSummary{egresshealth.ClassSite: summary}
		} else {
			result.NotMeasured = 1
		}
		if got := result.H3Summary(); got != c.h3 {
			t.Fatalf("%s: h3 summary = %q, want %q", c.name, got, c.h3)
		}
		line := "egress-health: provider=synthetic-provider " + result.Summary()
		if stages := result.FailureStageSummary(); stages != "" {
			line += " failure_stages=" + stages
		}
		line += " h3=" + result.H3Summary() + " " + result.TimingSummary()
		if failed := result.FailedNames(); 0 < len(failed) {
			line += " failed=" + strings.Join(failed, ",")
		}
		if unmeasured := result.NotMeasuredNames(); 0 < len(unmeasured) {
			line += " not-measured=" + strings.Join(unmeasured, ",")
		}
		if result.Total == 0 {
			line += " -- nothing measured; not submitted"
		}
		for _, record := range []string{line, "[synthetic-host][taskworker][g1][cid:synthetic-correlation][2026-11-02T01:00:00Z]2026/11/02 01:00:00 " + line} {
			tailer := newLogTailer("taskworker", nil)
			for occurrence := 0; occurrence < novelRateThreshold; occurrence += 1 {
				tailer.classify(record)
			}
			if novel := findingByClass(t, tailer.drainWindow(), "novel"); !novel.healthy {
				t.Fatalf("%s: complete egress producer record with an HTTP/3 leg became novel: %s", c.name, record)
			}
		}
	}
	// an outcome the summary never writes stays eligible
	const line = "egress-health: provider=synthetic-provider ok=0/1 site=0/1 failure_stages=h3:1 h3=unreviewed:1 failed=synthetic-h3"
	tailer := newLogTailer("taskworker", nil)
	for occurrence := 0; occurrence < novelRateThreshold; occurrence += 1 {
		tailer.classify(line)
	}
	if novel := findingByClass(t, tailer.drainWindow(), "novel"); novel.healthy {
		t.Fatal("an unreviewed HTTP/3 outcome was hidden")
	}
}

// Unsupported or malformed records stay eligible rather than being accepted
// because one prefix resembles a measurement.
func TestEgressHealthUnknownProducerRecordsRemainNovel(t *testing.T) {
	const line = "2026/11/02 01:00:00 egress-health: provider=synthetic-provider ok=0/1 site=0/1 failed=synthetic-site"
	lines := []string{
		line + " context canceled",
		line + " context deadline exceeded",
		line + " error=synthetic-reader-failure",
		line + " -- unknown suffix",
		strings.Replace(line, "ok=0/1", "ok=2/1", 1),
		line + " -- nothing measured; not submitted",
		strings.Replace(line, " failed=", " short=site,site failed=", 1),
		strings.Replace(line, " failed=", " failure_stages=unknown:1,unknown:1 failed=", 1),
		strings.Replace(line, "ok=0/1", "ok=unknown/1", 1),
		strings.Replace(line, "ok=0/1", "ok=-1/1", 1),
		strings.Replace(line, "ok=0/1", "ok=00/1", 1),
		strings.Replace(line, "ok=0/1", "ok=9223372036854775808/1", 1),
		strings.Replace(line, "2026/11/02", "2026/99/02", 1),
		"[invalid-time]" + line,
		strings.Replace(line, " site=0/1", " site=0/1 site=0/1", 1),
		strings.Replace(line, " site=0/1", " newclass=0/1", 1),
		strings.Replace(line, " failed=", " failure_stages=unreviewed:1 failed=", 1),
		strings.Replace(line, "failed=synthetic-site", "failed=synthetic site", 1),
		"egress-health: provider=synthetic-provider did not run: synthetic reader failure",
		"egress-health: provider=synthetic-provider skipped: synthetic error",
	}
	for index, line := range lines {
		tailer := newLogTailer("taskworker", nil)
		for occurrence := 0; occurrence < novelRateThreshold; occurrence += 1 {
			tailer.classify(line)
		}
		if novel := findingByClass(t, tailer.drainWindow(), "novel"); novel.healthy {
			t.Fatalf("unknown egress record case %d was hidden", index)
		}
	}
}

// Known classes still win, preserving owner projection and replay/liveness.
func TestEgressHealthExceptionPreservesKnownOwnerAndPrivacy(t *testing.T) {
	fixedNow := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	const summary = "egress-health: provider=synthetic-provider ok=0/1 site=0/1 failed=synthetic-site"
	const panicLine = `Unexpected error: {"error":"*errors.errorString=synthetic private failure","stack":["goroutine 7 [running]:","github.com/urnetwork/server/model.ReadPayerDueContracts()","/synthetic/subscription_payer_due_close.go:127 +0x1"]}`
	tailer := newLogTailer("taskworker", nil)
	tailer.clock = func() time.Time { return fixedNow }
	tailer.ingestStanding(strings.Replace(panicLine, "synthetic private failure", "synthetic private failure "+summary, 1), true, true)
	tailer.ingestStanding(strings.Replace(panicLine, "synthetic private failure", "synthetic private failure "+summary, 1), true, true)
	if tailer.classCounts["panic"] != 1 || tailer.classCounts["panic\x00server/model.ReadPayerDueContracts"] != 1 {
		t.Fatal("egress exception changed known panic owner or replay counts")
	}
	if sample := tailer.classSamples["panic"]; strings.Contains(sample, "synthetic private failure") || !strings.Contains(sample, "server/model.ReadPayerDueContracts") {
		t.Fatalf("egress exception changed fixed panic projection: %q", sample)
	}
	for occurrence := 0; occurrence < novelRateThreshold; occurrence += 1 {
		tailer.classify(fmt.Sprintf("[synthetic-host][taskworker][g1][cid:private-correlation]egress-health: provider=11111111-1111-1111-1111-111111111111 ok=0/1 site=0/1 failed=synthetic-site error=synthetic attempt %d", occurrence))
	}
	novel := findingByClass(t, tailer.drainWindow(), "novel")
	if novel.healthy {
		t.Fatal("adjacent unknown egress error lost novelty")
	}
	if strings.Contains(novel.evidence, "private-correlation") || strings.Contains(novel.evidence, "11111111-1111-1111-1111-111111111111") {
		t.Fatal("adjacent unknown egress error lost privacy")
	}
	lastLine, _, _ := tailer.healthSnapshot()
	if !lastLine.Equal(fixedNow) {
		t.Fatal("egress exception changed live-tail liveness accounting")
	}
}
