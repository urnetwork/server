package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestSelectionFreshnessSignalSyntheticSelectionStaleness(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{{"6000"}}, nil }}
	alerts, err := NewSelectionFreshnessSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "selection-stale")
}

func TestSelectionFreshnessSignalExplainsLiveReclaimedExport(t *testing.T) {
	taskID := "01a05555-97e8-e794-e009-04721c586db9"
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) { return []Row{{"5886"}}, nil },
		localFn: func(name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			for _, want := range []string{"logs synthetic taskworker", "--since=5m", "--limit=1000", "--query=UpdateClientScores", "--utc"} {
				if name != "warpctl" || !strings.Contains(joined, want) {
					t.Fatalf("score lifecycle lookup lost %q: %s %s", want, name, joined)
				}
			}
			return "[edge-0][taskworker][g1][cid:scoreworker][I][2026-08-29T12:00:00Z][task.go:1938][" + taskID + "]eval active(1458.47s) github.com/urnetwork/server/taskworker/work.UpdateClientScores({})", nil
		},
	}

	alerts, err := NewSelectionFreshnessSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "selection-stale").Markdown()
	for _, want := range []string{
		"actively rebuilding full-fleet export rather than a parked lease",
		"active_duration_s=1458",
		"active_task_id=" + taskID,
		"active_host=edge-0 active_generation=g1 active_container=scoreworker",
		"streaming, bounded-batch score exporter",
		"Do not restart this worker",
		"SIGNALS.md §2.8, §2.12, and §2.13",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("live score-recovery diagnosis lost %q:\n%s", want, markdown)
		}
	}
}
