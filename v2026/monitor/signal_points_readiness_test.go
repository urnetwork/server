package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
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
			for _, want := range []string{"latest.epoch_metrics_available", "network_points_leaderboard AS ranked", "count(ranked.network_id)", "clock_timestamp() AT TIME ZONE 'UTC'"} {
				if !strings.Contains(query, want) {
					t.Fatalf("snapshot query is missing %q: %s", want, query)
				}
			}
			return snapshot, nil
		case strings.Contains(query, "FROM st_epoch"):
			for _, want := range []string{"clock_timestamp() AT TIME ZONE 'UTC'", "DISTINCT ON (deployment_key)", "latest_finalized.finalized_time IS NOT NULL", "status = 'finalized'", "GROUP BY deployment_key", "LIMIT 1001"} {
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

func syntheticPointsSnapshot(
	ageSeconds int64,
	latestEpoch uint64,
	totalRanked uint64,
	epochAvailable bool,
	rowCount uint64,
	positiveBlocks uint64,
	positiveStreak uint64,
	positiveLongest uint64,
) []Row {
	return []Row{{
		fmt.Sprint(ageSeconds), fmt.Sprint(latestEpoch), fmt.Sprint(totalRanked),
		fmt.Sprint(epochAvailable), fmt.Sprint(rowCount), fmt.Sprint(positiveBlocks),
		fmt.Sprint(positiveStreak), fmt.Sprint(positiveLongest),
	}}
}

func syntheticPointsEpoch(
	deployment string,
	finalized uint64,
	latest uint64,
	ageKnown bool,
	ageSeconds int64,
) Row {
	return Row{
		deployment, fmt.Sprint(finalized), fmt.Sprint(latest),
		fmt.Sprint(ageKnown), fmt.Sprint(ageSeconds),
	}
}

func requireNoPointsAlertClass(t testing.TB, alerts Alerts, class string) {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == class {
			t.Fatalf("unexpected alert class %q: %s", class, alert.Markdown())
		}
	}
}

func TestPointsReadinessSignalDiagnosesUnavailableEpochMetrics(t *testing.T) {
	settings := syntheticPointsSettings()
	settings.VerificationEnabled = false
	settings.STDeploymentKey = ""
	alerts := runPointsReadinessFixture(t, settings,
		syntheticPointsSnapshot(300, 0, 25708, false, 25708, 0, 0, 0),
		nil,
	)
	alert := requireAlertClass(t, alerts, "points-epoch-metrics-unavailable")
	for _, want := range []string{
		"st_enabled=false",
		"total_ranked=25708",
		"snapshot_epoch_metrics_available=false",
		"positive_blocks=0",
		"already implemented and deployed",
		"obtain the first legitimate finalized epoch",
		"Do not add or hand-edit the field",
		"SIGNALS.md §17.6",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("unavailable alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	for _, stale := range []string{
		"serializes unavailable values and ranks as numeric zero",
		"Add an explicit epoch-metrics availability",
	} {
		if strings.Contains(alert.Markdown(), stale) {
			t.Fatalf("unavailable alert retains stale software guidance %q:\n%s", stale, alert.Markdown())
		}
	}
}

func TestPointsReadinessSignalTreatsFinalizedEpochZeroAsAvailable(t *testing.T) {
	settings := syntheticPointsSettings()
	alerts := runPointsReadinessFixture(t, settings,
		syntheticPointsSnapshot(120, 0, 10, true, 10, 0, 0, 0),
		[]Row{syntheticPointsEpoch(syntheticPointsDeployment, 1, 0, true, 120)},
	)
	if len(alerts) != 0 {
		t.Fatalf("finalized epoch zero produced alerts: %+v", alerts)
	}
}

func TestPointsReadinessSignalDoesNotTreatAnOpenEpochAsAvailable(t *testing.T) {
	settings := syntheticPointsSettings()
	alerts := runPointsReadinessFixture(t, settings,
		syntheticPointsSnapshot(120, 0, 10, false, 10, 0, 0, 0),
		[]Row{syntheticPointsEpoch(syntheticPointsDeployment, 0, 0, false, 0)},
	)
	requireAlertClass(t, alerts, "points-epoch-metrics-unavailable")
}

func TestPointsReadinessSignalPagesOnPersistedAvailabilityContradiction(t *testing.T) {
	settings := syntheticPointsSettings()
	settings.VerificationEnabled = false
	settings.STDeploymentKey = ""
	alerts := runPointsReadinessFixture(t, settings,
		syntheticPointsSnapshot(120, 0, 10, true, 10, 0, 0, 0),
		nil,
	)
	alert := requireAlertClass(t, alerts, "points-epoch-availability-drift")
	if alert.Severity != SeverityPage {
		t.Fatalf("availability contradiction severity = %s, want PAGE", alert.Severity)
	}
	for _, want := range []string{
		"without finalized windows",
		"snapshot_epoch_metrics_available=true",
		"already implemented",
		"normal transactional leaderboard rebuild",
		"Do not edit the availability bit",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("availability contradiction missing %q:\n%s", want, alert.Markdown())
		}
	}
	requireAlertOmits(t, alert, syntheticPointsDeployment)
}

func TestPointsReadinessSignalPagesWhenUnavailableSnapshotCarriesEpochPayload(t *testing.T) {
	settings := syntheticPointsSettings()
	settings.VerificationEnabled = false
	settings.STDeploymentKey = ""
	alerts := runPointsReadinessFixture(t, settings,
		syntheticPointsSnapshot(120, 7, 10, false, 10, 3, 2, 1),
		nil,
	)
	alert := requireAlertClass(t, alerts, "points-epoch-availability-drift")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Mechanism, "snapshot payload and its availability bit") {
		t.Fatalf("unavailable payload contradiction = %+v", alert)
	}
	for _, want := range []string{"Do not edit the availability bit", "semantically unavailable"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("unavailable payload contradiction missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestPointsReadinessSignalClassifiesFinalizeToRebuildBoundary(t *testing.T) {
	tests := []struct {
		name              string
		snapshotAge       int64
		snapshotAvailable bool
		snapshotLatest    uint64
		finalized         uint64
		activeLatest      uint64
		finalizedAge      int64
		class             string
		severity          Severity
		provenance        string
		mechanism         string
	}{
		{
			name:        "first finalization is newer and within budget",
			snapshotAge: 600, snapshotAvailable: false, snapshotLatest: 0,
			finalized: 1, activeLatest: 0, finalizedAge: 60,
			class: "points-epoch-rebuild-pending", severity: SeverityWarn,
			provenance: "provenance=source-newer", mechanism: "separate transactions",
		},
		{
			name:        "snapshot postdates first finalization within budget",
			snapshotAge: 60, snapshotAvailable: false, snapshotLatest: 0,
			finalized: 1, activeLatest: 0, finalizedAge: 600,
			class: "points-epoch-rebuild-pending", severity: SeverityWarn,
			provenance: "provenance=snapshot-newer", mechanism: "separate transactions",
		},
		{
			name:        "first finalization is overdue",
			snapshotAge: 60, snapshotAvailable: false, snapshotLatest: 0,
			finalized: 1, activeLatest: 0, finalizedAge: 7300,
			class: "points-epoch-availability-drift", severity: SeverityPage,
			provenance: "provenance=snapshot-newer", mechanism: "older than the two-hour",
		},
		{
			name:        "later finalization is newer and within budget",
			snapshotAge: 600, snapshotAvailable: true, snapshotLatest: 8,
			finalized: 9, activeLatest: 9, finalizedAge: 60,
			class: "points-epoch-rebuild-pending", severity: SeverityWarn,
			provenance: "provenance=source-newer", mechanism: "separate transactions",
		},
		{
			name:        "snapshot postdates later finalization within budget",
			snapshotAge: 60, snapshotAvailable: true, snapshotLatest: 8,
			finalized: 9, activeLatest: 9, finalizedAge: 600,
			class: "points-epoch-rebuild-pending", severity: SeverityWarn,
			provenance: "provenance=snapshot-newer", mechanism: "separate transactions",
		},
		{
			name:        "later finalization at exact budget",
			snapshotAge: 60, snapshotAvailable: true, snapshotLatest: 8,
			finalized: 9, activeLatest: 9, finalizedAge: int64(pointsSnapshotMaxAge / time.Second),
			class: "points-epoch-rebuild-pending", severity: SeverityWarn,
			provenance: "provenance=snapshot-newer", mechanism: "separate transactions",
		},
		{
			name:        "later finalization is overdue",
			snapshotAge: 8000, snapshotAvailable: true, snapshotLatest: 8,
			finalized: 9, activeLatest: 9, finalizedAge: 7300,
			class: "points-epoch-snapshot-drift", severity: SeverityPage,
			provenance: "provenance=source-newer", mechanism: "older than the two-hour",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alerts := runPointsReadinessFixture(t, syntheticPointsSettings(),
				syntheticPointsSnapshot(
					test.snapshotAge, test.snapshotLatest, 10, test.snapshotAvailable,
					10, 0, 0, 0,
				),
				[]Row{syntheticPointsEpoch(
					syntheticPointsDeployment, test.finalized, test.activeLatest,
					true, test.finalizedAge,
				)},
			)
			alert := requireAlertClass(t, alerts, test.class)
			if alert.Severity != test.severity {
				t.Fatalf("severity = %s, want %s", alert.Severity, test.severity)
			}
			if test.severity == SeverityWarn {
				requireNoPointsAlertClass(t, alerts, "points-epoch-availability-drift")
				requireNoPointsAlertClass(t, alerts, "points-epoch-snapshot-drift")
			}
			for _, want := range []string{test.provenance, test.mechanism, "SIGNALS.md §17.6"} {
				if !strings.Contains(alert.Markdown(), want) {
					t.Fatalf("alert missing %q:\n%s", want, alert.Markdown())
				}
			}
			requireAlertOmits(t, alert, syntheticPointsDeployment)
		})
	}
}

func TestPointsReadinessSignalAcceptsCompletedLaterEpochRebuild(t *testing.T) {
	alerts := runPointsReadinessFixture(t, syntheticPointsSettings(),
		syntheticPointsSnapshot(60, 9, 10, true, 10, 4, 3, 2),
		[]Row{syntheticPointsEpoch(syntheticPointsDeployment, 10, 9, true, 120)},
	)
	if len(alerts) != 0 {
		t.Fatalf("completed later-epoch rebuild produced alerts: %+v", alerts)
	}
}

func TestPointsReadinessSignalKeepsAmbiguousFinalizeOrderingPending(t *testing.T) {
	tests := []struct {
		name         string
		snapshotAge  int64
		ageKnown     bool
		finalizedAge int64
		wantStale    bool
	}{
		{name: "nullable finalized time", snapshotAge: 600, ageKnown: false},
		{name: "future finalized time", snapshotAge: 600, ageKnown: true, finalizedAge: -1},
		{name: "future snapshot time", snapshotAge: -1, ageKnown: true, finalizedAge: 60, wantStale: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alerts := runPointsReadinessFixture(t, syntheticPointsSettings(),
				syntheticPointsSnapshot(test.snapshotAge, 8, 10, true, 10, 0, 0, 0),
				[]Row{syntheticPointsEpoch(
					syntheticPointsDeployment, 9, 9, test.ageKnown, test.finalizedAge,
				)},
			)
			alert := requireAlertClass(t, alerts, "points-epoch-rebuild-pending")
			if alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, "provenance=ambiguous") {
				t.Fatalf("ambiguous boundary alert = %+v", alert)
			}
			requireNoPointsAlertClass(t, alerts, "points-epoch-snapshot-drift")
			requireNoPointsAlertClass(t, alerts, "points-epoch-availability-drift")
			if test.wantStale {
				requireAlertClass(t, alerts, "points-leaderboard-stale")
			} else {
				requireNoPointsAlertClass(t, alerts, "points-leaderboard-stale")
			}
			requireAlertOmits(t, alert, syntheticPointsDeployment)
		})
	}
}

func TestPointsReadinessSignalPagesWhenSnapshotEpochIsAheadOfSource(t *testing.T) {
	alerts := runPointsReadinessFixture(t, syntheticPointsSettings(),
		syntheticPointsSnapshot(600, 10, 10, true, 10, 0, 0, 0),
		[]Row{syntheticPointsEpoch(syntheticPointsDeployment, 10, 9, true, 60)},
	)
	alert := requireAlertClass(t, alerts, "points-epoch-snapshot-drift")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Mechanism, "cannot publish an epoch") {
		t.Fatalf("snapshot-ahead alert = %+v", alert)
	}
	requireAlertOmits(t, alert, syntheticPointsDeployment)
}

func TestPointsReadinessQueriesUseUtcDatabaseClock(t *testing.T) {
	for name, query := range map[string]string{
		"snapshot": pointsSnapshotQuery,
		"epoch":    pointsEpochCensusQuery,
	} {
		if !strings.Contains(query, "clock_timestamp() AT TIME ZONE 'UTC'") {
			t.Fatalf("%s query does not normalize the database clock to UTC: %s", name, query)
		}
	}
}

func TestParsePointsEpochCensusRejectsMalformedAgeEvidence(t *testing.T) {
	tests := []struct {
		name string
		row  pgRow
		want string
	}{
		{
			name: "nonnumeric age",
			row:  pgRow{syntheticPointsDeployment, "1", "0", "true", "not-an-age"},
			want: "invalid latest-finalization age",
		},
		{
			name: "age without timestamp",
			row:  pgRow{syntheticPointsDeployment, "1", "0", "false", "1"},
			want: "age without timestamp provenance",
		},
		{
			name: "empty source with provenance",
			row:  pgRow{syntheticPointsDeployment, "0", "0", "true", "0"},
			want: "contradictory empty-finalization evidence",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parsePointsEpochCensus([]pgRow{test.row})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParsePointsSnapshotRejectsInvalidAvailability(t *testing.T) {
	_, err := parsePointsSnapshot([]pgRow{{"120", "0", "10", "not-a-boolean", "10", "0", "0", "0"}})
	if err == nil || !strings.Contains(err.Error(), "invalid epoch-metrics availability") {
		t.Fatalf("invalid availability error = %v", err)
	}
}

func TestPointsReadinessSignalRejectsWrongDeploymentEvidenceWithoutLeakingIt(t *testing.T) {
	settings := syntheticPointsSettings()
	wrongDeployment := "synthetic:deployment-retired"
	alerts := runPointsReadinessFixture(t, settings,
		syntheticPointsSnapshot(120, 0, 10, false, 10, 0, 0, 0),
		[]Row{syntheticPointsEpoch(wrongDeployment, 10, 9, true, 60)},
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
		syntheticPointsSnapshot(120, 8, 10, true, 10, 8, 7, 8),
		[]Row{syntheticPointsEpoch(syntheticPointsDeployment, 10, 9, true, 3*60*60)},
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
		{name: "incomplete", snapshot: syntheticPointsSnapshot(60, 3, 10, true, 9, 2, 1, 2), class: "points-leaderboard-incomplete"},
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
		syntheticPointsSnapshot(int64(pointsSnapshotMaxAge.Seconds())+1, 4, 10, true, 10, 2, 1, 2),
		[]Row{syntheticPointsEpoch(syntheticPointsDeployment, 5, 4, true, 60)},
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
		epochs[index] = syntheticPointsEpoch(fmt.Sprintf("synthetic:deployment-%d", index), 1, 0, true, 60)
	}
	settings.Source = &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM network_points_leaderboard_snapshot") {
			return syntheticPointsSnapshot(120, 0, 10, true, 10, 0, 0, 0), nil
		}
		return epochs, nil
	}}
	_, err := NewPointsReadinessSignal().Run(context.Background(), settings)
	if err == nil || !strings.Contains(err.Error(), "completeness cap") {
		t.Fatalf("truncated deployment census error = %v", err)
	}
}
