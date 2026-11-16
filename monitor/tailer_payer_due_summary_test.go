package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPayerDueCloseZeroFailureSummaryDoesNotBecomeNovel(t *testing.T) {
	const summary = "[payer-due-close]11111111-1111-1111-1111-111111111111 due=0 closed=0 already_closed=0 busy=0 failed=0 more=false"
	lines := []string{
		summary,
		"[I][2026-11-02T01:00:00Z][subscription_payer_due_close.go:467]" + summary,
		"[synthetic-host][taskworker][g1][cid:synthetic-correlation][I][2026-11-02T01:00:00.123456Z][subscription_payer_due_close.go:467]" + summary,
		strings.Replace(summary, "due=0 closed=0", "due=3 closed=3", 1),
		strings.Replace(summary, "due=0 closed=0 already_closed=0 busy=0", "due=3 closed=1 already_closed=1 busy=1", 1),
		strings.Replace(summary, "more=false", "more=true", 1),
	}
	for index, line := range lines {
		tailer := newLogTailer("taskworker", nil)
		emitter := &recordingEmitter{}
		manager := newTicketManager("test", emitter)
		for window := 0; window < 2; window += 1 {
			for occurrence := 0; occurrence < novelRateThreshold; occurrence += 1 {
				tailer.classify(line)
			}
			findings := tailer.drainWindow()
			if novel := findingByClass(t, findings, "novel"); !novel.healthy {
				t.Fatalf("zero-failure payer summary case %d manufactured a novel finding", index)
			}
			manager.ingest(context.Background(), findings)
		}
		for _, event := range emitter.events {
			if event.kind == ticketOpen && event.t.probeId == "logs/novel" {
				t.Fatalf("zero-failure payer summary case %d opened a novel ticket", index)
			}
		}
	}
}

func TestPayerDueCloseNovelExceptionRetainsFailuresAndUnknownFormats(t *testing.T) {
	const summary = "[payer-due-close]11111111-1111-1111-1111-111111111111 due=0 closed=0 already_closed=0 busy=0 failed=0 more=false"
	lines := []string{
		strings.Replace(summary, "failed=0", "failed=1", 1),
		strings.Replace(summary, "failed=0", "failed=unknown", 1),
		strings.Replace(summary, "failed=0", "failed=-1", 1),
		strings.Replace(summary, "due=0", "due=-1", 1),
		strings.Replace(summary, "due=0", "due=9223372036854775808", 1),
		strings.Replace(summary, "due=0", "due=00", 1),
		strings.Replace(summary, "closed=0 ", "", 1),
		strings.Replace(summary, "busy=0 ", "busy=0 busy=0 ", 1),
		strings.Replace(summary, "more=false", "more=unknown", 1),
		strings.Replace(summary, " already_closed=0 busy=0", " busy=0 already_closed=0", 1),
		summary + " error=synthetic-reader-failure",
		summary + " context canceled",
		summary + " context deadline exceeded",
		"error: " + summary,
		"[E][2026-11-02T01:00:00Z][subscription_payer_due_close.go:467]" + summary,
		"[I][invalid-time][subscription_payer_due_close.go:467]" + summary,
		"[I][2026-11-02T01:00:00Z][different_source.go:467]" + summary,
		strings.Replace(summary, "11111111-1111-1111-1111-111111111111", "malformed-payer", 1),
		"[payer-due-close]11111111-1111-1111-1111-111111111111 closed nothing, 0 failed: context canceled",
		"[payer-due-close]11111111-1111-1111-1111-111111111111 closed nothing, 1 failed: synthetic-reader-failure",
		"[payer-due-close]11111111-1111-1111-1111-111111111111 closed nothing, 0 failed: context deadline exceeded",
		"[unrelated] completed failed=0",
	}
	for index, line := range lines {
		tailer := newLogTailer("taskworker", nil)
		for occurrence := 0; occurrence < novelRateThreshold; occurrence += 1 {
			tailer.classify(line)
		}
		if novel := findingByClass(t, tailer.drainWindow(), "novel"); novel.healthy {
			t.Fatalf("failure or unknown payer summary case %d was hidden", index)
		}
	}
}

func TestPayerDueCloseSummaryPreservesPanicOwnerCountsAndReplay(t *testing.T) {
	fixedNow := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	const summary = "[I][2026-11-02T01:00:00Z][subscription_payer_due_close.go:467][payer-due-close]11111111-1111-1111-1111-111111111111 due=0 closed=0 already_closed=0 busy=0 failed=0 more=false"
	const panicLine = `Unexpected error: {"error":"*errors.errorString=synthetic payer read failure","stack":["goroutine 7 [running]:","github.com/urnetwork/server/model.ReadPayerDueContracts()","/synthetic/subscription_payer_due_close.go:127 +0x1"]}`
	tailer := newLogTailer("taskworker", nil)
	tailer.clock = func() time.Time { return fixedNow }
	for occurrence := 0; occurrence < novelRateThreshold; occurrence += 1 {
		tailer.ingestStanding(summary, true, true)
	}
	tailer.ingestStanding(panicLine, true, true)
	tailer.ingestStanding(panicLine, true, true)
	if got := tailer.classCounts["panic"]; got != 1 {
		t.Fatalf("panic aggregate count = %d, want one replay-deduplicated diagnostic", got)
	}
	if got := tailer.classCounts["panic\x00server/model.ReadPayerDueContracts"]; got != 1 {
		t.Fatalf("payer read owner count = %d, want one overlapping owner diagnostic", got)
	}
	if sample := tailer.classSamples["panic"]; strings.Contains(sample, "synthetic payer read failure") || !strings.Contains(sample, "server/model.ReadPayerDueContracts") {
		t.Fatalf("panic sample lost its bounded owner projection: %q", sample)
	}
	// One ordinary reader failure still reaches novelty independently of the
	// known panic. These are line counts, not payers, tasks or outcomes.
	for occurrence := 0; occurrence < novelRateThreshold; occurrence += 1 {
		tailer.classify(fmt.Sprintf("payer reader failure: synthetic attempt %d", occurrence))
	}
	if novel := findingByClass(t, tailer.drainWindow(), "novel"); novel.healthy {
		t.Fatal("independent reader failure was hidden by a neighboring payer summary")
	}
	lastLine, _, _ := tailer.healthSnapshot()
	if !lastLine.Equal(fixedNow) {
		t.Fatal("summary or panic classification changed live-tail liveness accounting")
	}
}
