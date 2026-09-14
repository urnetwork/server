package monitor

import (
	"bytes"
	"encoding/json"
	"reflect"
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

func TestWriteAlertsJSONLIsDeterministicAndMachineReadable(t *testing.T) {
	page := Alert{
		SignalNumber: "1.1", SignalKey: "alpha", SignalID: "probe/alpha", SignalName: "Alpha",
		Severity: SeverityPage, Class: "broken", Target: "alpha-1", Environment: "synthetic",
		ObservedAt: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC), Symptom: "alpha < baseline",
	}
	warn := Alert{
		SignalNumber: "1.2", SignalKey: "beta", SignalID: "probe/beta", SignalName: "Beta",
		Severity: SeverityWarn, Class: "slow", Target: "beta-1", Environment: "synthetic",
		ObservedAt: time.Date(2026, 9, 7, 12, 1, 0, 0, time.UTC), Symptom: "beta slow",
	}
	laterWarn := Alert{
		SignalNumber: "1.3", SignalKey: "gamma", SignalID: "probe/gamma", SignalName: "Gamma",
		Severity: SeverityWarn, Class: "slow", Target: "gamma-1", Environment: "synthetic",
		ObservedAt: time.Date(2026, 9, 7, 12, 2, 0, 0, time.UTC), Symptom: "gamma slow",
	}

	var first bytes.Buffer
	if err := WriteAlertsJSONL(&first, []Alert{laterWarn, warn, page}); err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := (Alerts{page, warn, laterWarn}).WriteJSONL(&second); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatalf("JSONL depends on input order:\nfirst:  %s\nsecond: %s", first.String(), second.String())
	}
	if strings.Contains(first.String(), `\u003c`) {
		t.Fatalf("JSONL rewrote evidence text with HTML escaping: %s", first.String())
	}

	lines := strings.Split(strings.TrimSuffix(first.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("JSONL line count = %d, want 3: %q", len(lines), first.String())
	}
	decoded := make([]Alert, 0, len(lines))
	for _, line := range lines {
		var alert Alert
		if err := json.Unmarshal([]byte(line), &alert); err != nil {
			t.Fatalf("decode JSONL line %q: %v", line, err)
		}
		decoded = append(decoded, alert)
	}
	if want := []Alert{page, warn, laterWarn}; !reflect.DeepEqual(decoded, want) {
		t.Fatalf("decoded alerts = %#v, want %#v", decoded, want)
	}
}

func TestWriteAlertsJSONLEmitsNothingForNoAlerts(t *testing.T) {
	var output bytes.Buffer
	if err := WriteAlertsJSONL(&output, nil); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("empty JSONL output = %q", output.String())
	}
}
