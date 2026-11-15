package monitor

import (
	"fmt"
	"strings"
	"testing"
)

// The prober's per-provider egress-health result record names the
// destinations one provider failed in its measurement (failed=...). That is
// the measurement working, judged by §2.19b and §2.23, not an error of the
// emitting Taskworker. On 2026-10-18 these records were the top novel shape on
// Main (46/min of 178/min), drowning the unclassified errors beside them.
func TestEgressHealthResultRecordsStayOutOfNovel(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	for i := 0; i < novelRateThreshold*3; i += 1 {
		tailer.classify(fmt.Sprintf(
			"[edge-private][taskworker][g1][cid:private-correlation][2026-10-18T17:37:12-05:00]"+
				"2026/10/18 22:37:12 egress-health: provider=synthetic-provider-%d ok=0/1 site=0/1 table=121 "+
				"failure_stages=dial_dns:1 dns_timed=1 dns_ms=5010.5 failed=npm-registry",
			i,
		))
		tailer.classify(fmt.Sprintf(
			"2026/10/18 22:37:13 egress-health: provider=synthetic-provider-%d ok=48/50 dns=6/6 connectivity=8/8 "+
				"cdn=9/10 site=25/26 table=139 retried=3 failed=cachefly,reddit",
			i,
		))
	}
	if novel := findingByClass(t, tailer.drainWindow(), "novel"); !novel.healthy {
		t.Fatalf("egress-health measurement records became a novel error: %+v", novel)
	}
}

// The prober's structural failure lines stay eligible: a check that did not
// run is not a measurement, and at rate it is an unclassified error.
func TestEgressHealthCheckThatDidNotRunRemainsNovel(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	for i := 0; i < novelRateThreshold; i += 1 {
		tailer.classify(fmt.Sprintf(
			"2026/10/18 22:37:12 egress-health: provider=synthetic-provider-%d did not run: tunnel setup failed: synthetic", i,
		))
	}
	novel := findingByClass(t, tailer.drainWindow(), "novel")
	if novel.healthy {
		t.Fatal("egress-health checks that did not run were hidden from the novel class")
	}
	if !strings.Contains(novel.evidence, "did not run") {
		t.Fatalf("novel evidence lacks the failing shape: %q", novel.evidence)
	}
}
