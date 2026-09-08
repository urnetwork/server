package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const syntheticPointsDeployment = "synthetic:deployment-alpha"

func runPointsReadinessFixture(
	t testing.TB,
	settings SignalSettings,
	snapshot []Row,
	epochs []Row,
) Alerts {
	t.Helper()
	queries := 0
	settings.Source = &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		queries++
		switch {
		case strings.Contains(query, "FROM network_points_leaderboard_snapshot"):
			for _, want := range []string{"network_points_leaderboard AS ranked", "count(ranked.network_id)", "clock_timestamp()"} {
				if !strings.Contains(query, want) {
					t.Fatalf("snapshot query is missing %q: %s", want, query)
				}
			}
			return snapshot, nil
		case strings.Contains(query, "FROM st_epoch"):
			for _, want := range []string{"status = 'finalized'", "GROUP BY deployment_key", "LIMIT 1001"} {
				if !strings.Contains(query, want) {
					t.Fatalf("epoch census query is missing %q: %s", want, query)
				}
			}
			return epochs, nil
		default:
			t.Fatalf("unexpected points-readiness query: %s", query)
			return nil, nil
		}
	}}
	alerts, err := NewPointsReadinessSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if queries == 0 || 2 < queries {
		t.Fatalf("points-readiness query count = %d", queries)
	}
	return alerts
}

func syntheticPointsSettings() SignalSettings {
	settings := syntheticSettings(nil)
	settings.VerificationEnabled = true
	settings.STDeploymentKey = syntheticPointsDeployment
	return settings
}

func TestPointsReadinessSignalDiagnosesUnavailableEpochMetrics(t *testing.T) {
	settings := syntheticPointsSettings()
	settings.VerificationEnabled = false
	settings.STDeploymentKey = ""
	alerts := runPointsReadinessFixture(t, settings,
		[]Row{{"300", "0", "25708", "25708", "0", "0", "0"}},
		nil,
	)
	alert := requireAlertClass(t, alerts, "points-epoch-metrics-unavailable")
	for _, want := range []string{
		"st_enabled=false",
		"total_ranked=25708",
		"positive_blocks=0",
		"serializes unavailable values and ranks as numeric zero",
		"API, SDK, and apps",
		"Do not substitute legacy payout periods or open epochs",
		"SIGNALS.md §17.6",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("unavailable alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestPointsReadinessSignalTreatsFinalizedEpochZeroAsAvailable(t *testing.T) {
	settings := syntheticPointsSettings()
	alerts := runPointsReadinessFixture(t, settings,
		[]Row{{"120", "0", "10", "10", "0", "0", "0"}},
		[]Row{{syntheticPointsDeployment, "1", "0"}},
	)
	if len(alerts) != 0 {
		t.Fatalf("finalized epoch zero produced alerts: %+v", alerts)
	}
}

func TestPointsReadinessSignalDoesNotTreatAnOpenEpochAsAvailable(t *testing.T) {
	settings := syntheticPointsSettings()
	alerts := runPointsReadinessFixture(t, settings,
		[]Row{{"120", "0", "10", "10", "0", "0", "0"}},
		[]Row{{syntheticPointsDeployment, "0", "0"}},
	)
	requireAlertClass(t, alerts, "points-epoch-metrics-unavailable")
}

func TestPointsReadinessSignalRejectsWrongDeploymentEvidenceWithoutLeakingIt(t *testing.T) {
	settings := syntheticPointsSettings()
	wrongDeployment := "synthetic:deployment-retired"
	alerts := runPointsReadinessFixture(t, settings,
		[]Row{{"120", "9", "10", "10", "8", "7", "8"}},
		[]Row{{wrongDeployment, "10", "9"}},
	)
	alert := requireAlertClass(t, alerts, "points-epoch-metrics-unavailable")
	if !strings.Contains(alert.Observed, "active_deployment_present=false") {
		t.Fatalf("wrong deployment alert = %+v", alert)
	}
	requireAlertOmits(t, alert, syntheticPointsDeployment, wrongDeployment)
}

func TestPointsReadinessSignalDetectsSnapshotEpochDrift(t *testing.T) {
	settings := syntheticPointsSettings()
	alerts := runPointsReadinessFixture(t, settings,
		[]Row{{"120", "8", "10", "10", "8", "7", "8"}},
		[]Row{{syntheticPointsDeployment, "10", "9"}},
	)
	alert := requireAlertClass(t, alerts, "points-epoch-snapshot-drift")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "active_latest_epoch=9 snapshot_latest_epoch=8") {
		t.Fatalf("snapshot drift alert = %+v", alert)
	}
	requireAlertOmits(t, alert, syntheticPointsDeployment)
}

func TestPointsReadinessSignalDetectsIncompleteAndMissingSnapshots(t *testing.T) {
	settings := syntheticPointsSettings()
	tests := []struct {
		name     string
		snapshot []Row
		class    string
	}{
		{name: "missing", snapshot: nil, class: "points-leaderboard-unavailable"},
		{name: "incomplete", snapshot: []Row{{"60", "3", "10", "9", "2", "1", "2"}}, class: "points-leaderboard-incomplete"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alerts := runPointsReadinessFixture(t, settings, test.snapshot, nil)
			requireAlertClass(t, alerts, test.class)
		})
	}
}

func TestPointsReadinessSignalReportsStaleSnapshotAlongsideHealthyEpochSource(t *testing.T) {
	settings := syntheticPointsSettings()
	alerts := runPointsReadinessFixture(t, settings,
		[]Row{{fmt.Sprint(int64(pointsSnapshotMaxAge.Seconds()) + 1), "4", "10", "10", "2", "1", "2"}},
		[]Row{{syntheticPointsDeployment, "5", "4"}},
	)
	alert := requireAlertClass(t, alerts, "points-leaderboard-stale")
	if alert.Sustain != 2 {
		t.Fatalf("stale snapshot sustain = %d", alert.Sustain)
	}
}

func TestPointsReadinessSignalRejectsTruncatedDeploymentCensus(t *testing.T) {
	settings := syntheticPointsSettings()
	epochs := make([]Row, pointsEpochCensusMax+1)
	for index := range epochs {
		epochs[index] = Row{fmt.Sprintf("synthetic:deployment-%d", index), "1", "0"}
	}
	settings.Source = &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM network_points_leaderboard_snapshot") {
			return []Row{{"120", "0", "10", "10", "0", "0", "0"}}, nil
		}
		return epochs, nil
	}}
	_, err := NewPointsReadinessSignal().Run(context.Background(), settings)
	if err == nil || !strings.Contains(err.Error(), "completeness cap") {
		t.Fatalf("truncated deployment census error = %v", err)
	}
}
