// SIGNALS.md §1.5: timestamp-free Warpctl pre-cursor summaries describe live
// responses. Their recurrence must survive source-record replay suppression.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Identical service/count bytes can describe separate responses in one minute
// or successive minutes inside the four-minute source replay retention.
func TestStandingTailPreCursorCountsEveryLiveOccurrence(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC)
	tailer := newLogTailer("fixture-service", nil)
	tailer.clock = func() time.Time { return now }
	line := "[warpctl][loki-tail-pre-cursor-entries] service=fixture-service count=17"

	for window, occurrences := range []int{1, 2, 1} {
		for range occurrences {
			tailer.ingestStanding(line, true, true)
		}
		findings := tailer.drainWindow()
		finding := findingByClass(t, findings, "loki-tail-pre-cursor-entries")
		if finding.healthy {
			t.Fatalf("live summary recurrence disappeared in window %d: %d distinct responses became healthy", window+1, occurrences)
		}
		if want := fmt.Sprintf("rate=%d/min ", occurrences); !strings.Contains(finding.observed, want) {
			t.Fatalf("window %d counted records instead of live summaries: got %q, want %q", window+1, finding.observed, want)
		}
		if finding.probeId != "logs/loki-tail-pre-cursor-entries" || finding.target != "fixture-service" || finding.frame != "" || finding.tier != tierWarn || finding.sustain != 1 {
			t.Fatalf("window %d changed the immediate warning or identity: %+v", window+1, finding)
		}
		if !strings.Contains(finding.context, "not a current failure") || !strings.Contains(finding.action, "do not replay the suppressed contents") {
			t.Fatal("pre-cursor observations lost their current-product-failure boundary")
		}
		for _, class := range []string{"panic", "loki-tail-dropped-entries", "loki-tail-dropped-streams"} {
			if other := findingByClass(t, findings, class); !other.healthy {
				t.Fatalf("pre-cursor summary manufactured %s", class)
			}
		}
		now = now.Add(time.Minute)
	}
}

// Counting each response must not carry an earlier summary into an empty
// window. Ordinary current-source input remains only a liveness observation.
func TestStandingTailPreCursorQuietWindowDoesNotRepeat(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC)
	tailer := newLogTailer("fixture-service", nil)
	tailer.clock = func() time.Time { return now }
	tailer.ingestStanding("[warpctl][loki-tail-pre-cursor-entries] service=fixture-service count=1", true, true)
	if finding := findingByClass(t, tailer.drainWindow(), "loki-tail-pre-cursor-entries"); finding.healthy {
		t.Fatal("one positive summary did not reach the unchanged warning threshold")
	}
	now = now.Add(time.Minute)
	tailer.ingestStanding("[fixture-host.example][fixture-service][fixture-block][cid:fixture-process]["+now.Format(time.RFC3339)+"] ordinary current record", true, true)
	if finding := findingByClass(t, tailer.drainWindow(), "loki-tail-pre-cursor-entries"); !finding.healthy {
		t.Fatalf("an empty summary window retained the previous occurrence: %+v", finding)
	}
	lastLine, _, _ := tailer.healthSnapshot()
	if !lastLine.Equal(now) {
		t.Fatal("ordinary current source input did not refresh live-tail liveness")
	}
}

// Exercise the actual range adapter: timestamp-free summaries cannot enter
// reconciliation, while its overlapping source records remain idempotent.
func TestStandingTailPreCursorKeepsReconciliationReplayDedup(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC)
	tailer := newLogTailer("fixture-service", nil)
	tailer.clock = func() time.Time { return now }
	tailer.startedAt = now.Add(-time.Minute)
	tailer.lastLineTime = now
	source := "[fixture-host.example][fixture-service][fixture-block][cid:fixture-process][" + now.Format(time.RFC3339) + "] OOM command not allowed synthetic source failure"
	summary := "[warpctl][loki-tail-pre-cursor-entries] service=fixture-service count=1"
	tailer.reconcile = func(context.Context, time.Time, []string) (string, error) {
		return source + "\n" + summary + "\n", nil
	}

	tailer.ingestStanding(source, true, true)
	tailer.reconcileOnce(context.Background())
	first := tailer.drainWindow()
	if finding := findingByClass(t, first, "oom-writes"); finding.healthy || !strings.Contains(finding.observed, "rate=1/min ") {
		t.Fatalf("live/range overlap changed the source occurrence count: %+v", finding)
	}
	if finding := findingByClass(t, first, "loki-tail-pre-cursor-entries"); !finding.healthy {
		t.Fatal("timestamp-free local summary entered source-time reconciliation")
	}
	priorLiveness, _, _ := tailer.healthSnapshot()
	now = now.Add(time.Minute)
	tailer.reconcileOnce(context.Background())
	replayed := tailer.drainWindow()
	for _, class := range []string{"oom-writes", "loki-tail-pre-cursor-entries"} {
		if finding := findingByClass(t, replayed, class); !finding.healthy {
			t.Fatalf("overlapping range replay manufactured %s", class)
		}
	}
	lastLine, _, _ := tailer.healthSnapshot()
	if !lastLine.Equal(priorLiveness) {
		t.Fatal("range reconciliation refreshed standing-tail liveness")
	}
	tailer.ingestStanding(source, true, true)
	if finding := findingByClass(t, tailer.drainWindow(), "oom-writes"); !finding.healthy {
		t.Fatal("exact source-record replay became a new live product failure")
	}
}

// The exception applies to the local summary, never to the historical source
// bytes it describes. Stale source records still cannot page a product now.
func TestStandingTailPreCursorKeepsStaleSourceGuard(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC)
	tailer := newLogTailer("fixture-service", nil)
	tailer.clock = func() time.Time { return now }
	source := "[fixture-host.example][fixture-service][fixture-block][cid:fixture-process][" + now.Add(-time.Hour).Format(time.RFC3339) + "] OOM command not allowed synthetic historical failure"
	tailer.ingestStanding(source, true, true)
	tailer.ingestStanding("[warpctl][loki-tail-pre-cursor-entries] service=fixture-service count=1", true, true)
	first := tailer.drainWindow()
	if finding := findingByClass(t, first, "oom-writes"); !finding.healthy {
		t.Fatal("historical source record became a current product failure")
	}
	if finding := findingByClass(t, first, "tailer-stale-arrival"); finding.healthy || !strings.Contains(finding.observed, "stale_arrivals=1 ") {
		t.Fatalf("stale source observation was lost: %+v", finding)
	}
	if finding := findingByClass(t, first, "loki-tail-pre-cursor-entries"); finding.healthy {
		t.Fatal("separate local summary was lost beside the stale source record")
	}
	now = now.Add(time.Minute)
	tailer.ingestStanding(source, true, true)
	for _, finding := range tailer.drainWindow() {
		if !finding.healthy {
			t.Fatalf("exact stale source replay manufactured %s", finding.class)
		}
	}
}

// Missing or malformed finite fields never gain the live-summary exception.
func TestStandingTailPreCursorRejectsSummaryLookalikes(t *testing.T) {
	for _, line := range []string{
		"",
		"[warpctl][loki-tail-pre-cursor-entries] service=fixture-service",
		"[warpctl][loki-tail-pre-cursor-entries] count=1",
		"[warpctl][loki-tail-pre-cursor-entries] service=fixture-service count=0",
		"[warpctl][loki-tail-pre-cursor-entries] service=fixture-service count=unknown",
		"[warpctl][loki-tail-pre-cursor-entries] service=fixture-service count=1 trailing=synthetic",
		"[2030-01-02T03:04:00Z][warpctl][loki-tail-pre-cursor-entries] service=fixture-service count=1",
	} {
		tailer := newLogTailer("fixture-service", nil)
		for range 2 {
			tailer.ingestStanding(line, true, true)
		}
		if finding := findingByClass(t, tailer.drainWindow(), "loki-tail-pre-cursor-entries"); !finding.healthy {
			t.Fatalf("unproven pre-cursor summary gained trusted occurrence semantics: %q", line)
		}
	}
}
