package monitor

import (
	"strings"
	"testing"
	"time"
)

func TestAlertMarkdownIsDetailedAndHumanReadable(t *testing.T) {
	alert := Alert{
		SignalNumber: "1.4", SignalKey: "redis-cluster", SignalID: "redis/node-unreachable", SignalName: "Redis liveness",
		Severity: SeverityPage, Class: "node-unreachable", Target: "redis-1", Frame: "6380",
		Environment: "synthetic", ObservedAt: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC),
		Symptom: "Redis PING timed out", Mechanism: "The event loop is wedged", Baseline: "PING < 100ms",
		Observed: "timeout after 2s", Evidence: "port 6380 backlog 511", Action: "Inspect the node process",
		Verify: "PING returns PONG", Playbook: "SIGNALS.md §5.2",
	}
	markdown := alert.Markdown()
	for _, want := range []string{"[PAGE]", "SIGNALS.md §1.4 (`redis-cluster`)", "### Mechanism", "### Evidence", "### Action", "### Verify"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("Markdown missing %q:\n%s", want, markdown)
		}
	}
}
