package monitor

import (
	"strings"
	"testing"
)

func TestPoolTimeoutLiteralRetainsUnknownAuthority(t *testing.T) {
	tailer := newLogTailer("synthetic-service", nil)
	for range 10 {
		tailer.classify("redis: connection pool timeout")
	}
	f := findingByClass(t, tailer.drainWindow(), "pool-timeout")
	if f.healthy || f.tier != tierWarn || f.probeId != "logs/pool-timeout" || f.target != "synthetic-service" || f.frame != "" {
		t.Fatal("positive literal lost its warning identity or acquired an unproved pool frame")
	}
	a := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", f)
	for _, want := range []string{"actual pool, borrower outcome and cause are unknown", "typed wait stage/result", "independent borrower terminal outcome", "rate=10/min class=pool-timeout"} {
		if !strings.Contains(a.Markdown(), want) {
			t.Errorf("missing bounded authority qualifier %q", want)
		}
	}
	if strings.Contains(a.Markdown(), "local pool exhausted") || strings.Contains(a.Markdown(), "volume is retry amplification") {
		t.Fatal("literal log count acquired unproved native exhaustion or retry authority")
	}
}

func TestPoolTimeoutThresholdPreservesPositiveBand(t *testing.T) {
	for _, count := range []int{0, 9, 10, 11} {
		tailer := newLogTailer("synthetic-service", nil)
		for range count {
			tailer.classify("redis: connection pool timeout")
		}
		f := findingByClass(t, tailer.drainWindow(), "pool-timeout")
		if f.healthy != (count < 10) {
			t.Errorf("count %d changed the diagnostic threshold", count)
		}
		if !f.healthy && f.tier != tierWarn {
			t.Errorf("count %d changed warning severity", count)
		}
	}
}

func TestPoolTimeoutHealthyNonmatchingInput(t *testing.T) {
	tailer := newLogTailer("synthetic-service", nil)
	for range 11 {
		tailer.classify("synthetic operation completed successfully")
	}
	if !findingByClass(t, tailer.drainWindow(), "pool-timeout").healthy {
		t.Fatal("healthy nonmatching input produced a pool-timeout diagnostic")
	}
}

func TestPoolTimeoutRelayedDiagnosticKeepsCauseUnknown(t *testing.T) {
	tailer := newLogTailer("synthetic-service", nil)
	for range 10 {
		tailer.classify("synthetic relay quoted diagnostic: redis: connection pool timeout")
	}
	f := findingByClass(t, tailer.drainWindow(), "pool-timeout")
	if f.healthy || !strings.Contains(f.context, "quoted diagnostics") || !strings.Contains(f.context, "Missing original lifecycle evidence remains unknown") {
		t.Fatal("relayed text was suppressed or missing context acquired causal authority")
	}
}
