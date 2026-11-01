package monitor

import (
	"strings"
	"testing"
)

func TestPoolTimeoutOriginalContextDoesNotLeak(t *testing.T) {
	tailer := newLogTailer("synthetic-service", nil)
	for range 10 {
		tailer.classify("synthetic-secret synthetic-customer 192.0.2.88:6379 redis: connection pool timeout")
	}
	f := findingByClass(t, tailer.drainWindow(), "pool-timeout")
	if f.healthy || f.frame != "" {
		t.Fatal("private synthetic context changed the positive count or supplied a frame")
	}
	a := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", f)
	for _, private := range []string{"synthetic-secret", "synthetic-customer", "192.0.2.88"} {
		requireAlertOmits(t, a, private)
	}
	if !strings.Contains(a.Markdown(), "redis: connection pool timeout (original context omitted)") {
		t.Fatal("fixed diagnostic exemplar missing")
	}
}
