package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func selectionFreshnessAggregateFixture() requiredAggregateFixture {
	return requiredAggregateFixture{NewSelectionFreshnessSignal, "FROM finished_task WHERE function_name LIKE '%UpdateClientScores%'", Row{"0"}}
}

func TestSelectionFreshnessAggregateShape(t *testing.T) {
	testRequiredAggregateShape(t, selectionFreshnessAggregateFixture())
}

func TestSelectionFreshnessAggregateRunLoop(t *testing.T) {
	testRequiredAggregateRunLoop(t, selectionFreshnessAggregateFixture())
}

func TestSelectionFreshnessAggregateSentinelAndBands(t *testing.T) {
	for _, test := range []struct {
		gap      string
		severity Severity
	}{
		{gap: "0"},
		{gap: "5400"},
		{gap: "5401", severity: SeverityWarn},
		{gap: "10800", severity: SeverityWarn},
		{gap: "10801", severity: SeverityPage},
		{gap: "2147483647", severity: SeverityPage},
		{gap: "-1", severity: SeverityPage},
	} {
		t.Run(test.gap, func(t *testing.T) {
			requiredReads, lifecycleReads := 0, 0
			source := &syntheticSource{
				postgresFn: func(string) ([]Row, error) { requiredReads++; return []Row{{test.gap}}, nil },
				localFn:    func(string, ...string) (string, error) { lifecycleReads++; return "", nil },
			}
			signal := NewSelectionFreshnessSignal()
			alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(context.Background())
			if err != nil || requiredReads != 1 || signal.Cadence() != 5*time.Minute {
				t.Fatal("valid signed selection scalar changed its observation contract")
			}
			if test.severity == "" {
				if len(alerts) != 0 || lifecycleReads != 0 {
					t.Error("healthy selection boundary emitted an alert or read lifecycle logs")
				}
				return
			}
			if len(alerts) != 1 || lifecycleReads != 1 {
				t.Fatal("selection violation or no-completion sentinel did not retain one finding and one optional read")
			}
			alert := requireAlertClass(t, alerts, "selection-stale")
			if alert.SignalID != signal.ID() || alert.Target != "pg-1" || alert.Severity != test.severity || alert.Sustain != 1 ||
				!strings.Contains(alert.Observed, "completion_gap_s="+test.gap+" ") ||
				strings.Contains(alert.Observed, "active_duration_s=") {
				t.Error("selection signed sentinel, severity or empty-lifecycle meaning changed")
			}
		})
	}
}

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
		"active_attempt_correlated=true",
		"active_host=edge-0 active_generation=g1 active_container=scoreworker",
		"Retain the streaming, bounded-batch score exporter",
		"roll it out only where version or code evidence says it is absent",
		"full-map load and caller-location fan-out",
		"Do not restart this worker",
		"SIGNALS.md §2.8, §2.12, and §2.13",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("live score-recovery diagnosis lost %q:\n%s", want, markdown)
		}
	}
	requireAlertOmits(t, requireAlertClass(t, alerts, "selection-stale"), taskID)
	if strings.Contains(markdown, "roll out the streaming, bounded-batch score exporter on every taskworker generation") {
		t.Fatalf("live score-recovery diagnosis retained a stale rollout prescription:\n%s", markdown)
	}
}

// Invalid ages are unavailable before optional lifecycle evidence is consulted.
func TestSelectionFreshnessAuthorityRejectsMalformedAge(t *testing.T) {
	for _, test := range []struct{ name, value string }{
		{"private text", aggregatePrivateCell}, {"null", ""}, {"blank", " "},
		{"fraction", "0.9"}, {"integral decimal", "5401.0"}, {"negative nonsentinel", "-2"},
		{"not a number", "NaN"}, {"infinity", "Inf"}, {"negative infinity", "-Inf"},
		{"sql integer overflow", "2147483648"}, {"integer overflow", "9223372036854775808"},
		{"exponent", "6e3"}, {"leading plus", "+5401"}, {"hexadecimal", "0x1519"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requiredReads, lifecycleReads := 0, 0
			source := &syntheticSource{
				postgresFn: func(query string) ([]Row, error) {
					if !strings.Contains(query, selectionFreshnessAggregateFixture().queryMarker) ||
						!strings.Contains(query, "coalesce(round(extract(epoch FROM now()-max(run_end_time)))::int, -1)") {
						t.Fatal("age authority control did not reach its actual signed SQL aggregate")
					}
					requiredReads++
					return []Row{{test.value}}, nil
				},
				localFn:       func(string, ...string) (string, error) { lifecycleReads++; return "", nil },
				hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) { lifecycleReads++; return "", nil },
			}
			signal := NewSelectionFreshnessSignal()
			alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(context.Background())
			if err == nil {
				t.Error("malformed selection age established a product judgment")
			}
			requireAggregateVisibility(t, signal, alerts, observationErrorClassInvalidResponse)
			if requiredReads != 1 || lifecycleReads != 0 {
				t.Error("malformed required age triggered optional lifecycle observation")
			}
		})
	}
}
