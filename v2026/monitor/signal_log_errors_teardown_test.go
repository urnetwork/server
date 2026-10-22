package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

const peerTeardownStallFixture = "[synthetic-taskworker.example][taskworker][synthetic-generation][I][2000-01-01T00:00:00Z][transport_p2p_webrtc.go:42][peerconn]teardown stalled for 5s at starting s(synthetic-private-id) <>synthetic-peer"

// One watchdog header is one diagnostic stall; a whole-process stack dump
// must not turn thousands of records into thousands of incidents.
func TestPeerConnectionTeardownStallIgnoresStackFanout(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	tailer.classify(peerTeardownStallFixture)
	for range 1000 {
		tailer.classify("[synthetic-taskworker.example][taskworker][I][2000-01-01T00:00:00Z][transport_p2p_webrtc.go:42][peerconn]teardown goroutine 7/70000 at starting s(synthetic-private-id):\nsynthetic.example/worker.wait()")
	}
	finding := findingByClass(t, tailer.drainWindow(), "peerconn-teardown-stalled")
	if finding.healthy || finding.tier != tierWarn || finding.frame != "stage=starting" || !strings.Contains(finding.observed, "rate=1/min") {
		t.Fatalf("watchdog header was not counted independently of stack fanout: %+v", finding)
	}
	markdown := alertFromFinding(SignalSettings{
		Environment: "synthetic",
		Now:         func() time.Time { return time.Date(2000, 1, 1, 0, 1, 0, 0, time.UTC) },
	}, "1.5", "log-errors", "Log error-class rates", finding).Markdown()
	for _, private := range []string{"synthetic-taskworker.example", "synthetic-private-id", "synthetic-peer"} {
		if strings.Contains(markdown, private) {
			t.Fatalf("private source context leaked into Markdown: %q", private)
		}
	}
	if !strings.Contains(markdown, "not the exact blocked primitive") || !strings.Contains(markdown, "incomplete Loki reconciliation") {
		t.Fatal("Markdown lost the causal and visibility qualifiers")
	}
}

// A missing overlap cannot silently close a previous stall, even when the
// standing stream itself remains connected and emits no new header.
func TestPeerConnectionTeardownStallRecoveryRequiresCompleteOverlap(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	now := time.Date(2000, 1, 1, 0, 1, 0, 0, time.UTC)
	tailer.clock = func() time.Time { return now }
	tailer.reconcile = func(context.Context, time.Time, []string) (string, error) { return "", nil }
	hasHealthy := func() bool {
		for _, finding := range tailer.drainWindow() {
			if finding.class == "peerconn-teardown-stalled" && finding.healthy {
				return true
			}
		}
		return false
	}
	if hasHealthy() {
		t.Fatal("unreconciled stream cleared teardown-stall ticket")
	}
	tailer.recordReconcile(now.Add(-time.Minute), nil)
	if hasHealthy() {
		t.Fatal("one overlap cleared teardown-stall ticket")
	}
	now = now.Add(logReconcileInterval)
	tailer.recordReconcile(now.Add(-time.Minute), nil)
	if !hasHealthy() {
		t.Fatal("two advancing complete overlaps did not permit recovery")
	}
	tailer.recordReconcile(now.Add(-time.Minute), errLogReconcileIncomplete)
	if hasHealthy() {
		t.Fatal("failed overlap cleared teardown-stall ticket")
	}
}

// Five independent watchdog headers at the same finite stage cross the page
// band, while a malformed or unrelated line does not acquire this class.
func TestPeerConnectionTeardownStallPageAndUnknownControls(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	for range 5 {
		tailer.classify(peerTeardownStallFixture)
	}
	page := findingByClass(t, tailer.drainWindow(), "peerconn-teardown-stalled")
	if page.healthy || page.tier != tierPage || !strings.Contains(page.observed, "rate=5/min") {
		t.Fatalf("five watchdog headers did not page: %+v", page)
	}
	for _, line := range []string{
		strings.Replace(peerTeardownStallFixture, "starting", "synthetic-unknown-stage", 1),
		strings.Replace(peerTeardownStallFixture, "teardown stalled", "teardown completed", 1),
	} {
		tailer.classify(line)
	}
	if quiet := findingByClass(t, tailer.drainWindow(), "peerconn-teardown-stalled"); !quiet.healthy {
		t.Fatal("near miss inherited watchdog incidence")
	}
}
