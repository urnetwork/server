// Snapshot-only availability contradictions survive a later unknown source.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A valid same-snapshot contradiction pages even when the later source fails.
func TestPointsPartialRetainsSnapshotContradictionsAcrossSourceFailures(t *testing.T) {
	for _, age := range []int64{60, 7201} {
		for _, snapshot := range [][]Row{
			syntheticPointsSnapshot(age, 1, false, 10, 1),
			syntheticPointsSnapshot(age, 1, false, 10, 0),
			{{"60", "0", "10", "false", "10", "1", "0", "0"}},
			{{"60", "0", "10", "false", "10", "0", "1", "0"}},
			{{"60", "0", "10", "false", "10", "0", "0", "1"}},
			syntheticPointsSnapshot(age, 0, true, 10, 0),
		} {
			if age == 7201 {
				snapshot[0][0] = "7201"
			}
			for _, control := range []struct {
				name string
				rows []Row
				err  error
			}{
				{name: "read", err: errors.New("synthetic-private-contradiction")},
				{name: "empty"},
				{name: "shape", rows: []Row{{"12", "60", "0", "0", "0", "synthetic-private-contradiction"}}},
				{name: "value", rows: []Row{{"12", "60", "synthetic-private-contradiction", "0", "0"}}},
				{name: "inconsistent", rows: []Row{{"12", "60", "0", "0", "1"}}},
				{name: "local-cancel", err: context.Canceled},
				{name: "local-deadline", err: context.DeadlineExceeded},
			} {
				settings, stages := pointsPartialSettings(t, snapshot, []Row{{"true"}}, nil, control.rows, control.err, nil)
				alerts, err := NewPointsReadinessSignal().Run(context.Background(), settings)
				wantCount := 2
				if age == 7201 {
					wantCount++
				}
				if err != nil || len(alerts) != wantCount || strings.Join(*stages, ",") != "snapshot,schema,source" {
					t.Fatalf("age=%d case=%s: independent PAGE lost or dependency boundary changed", age, control.name)
				}
				page := requireAlertClass(t, alerts, "points-epoch-availability-drift")
				unknown := requireAlertClass(t, alerts, "cannot-observe")
				if page.Severity != SeverityPage || page.Sustain != 1 || page.unobservable || unknown.Severity != SeverityWarn || unknown.Sustain != 2 || !unknown.unobservable || unknown.Target != "points-readiness/operator-source" {
					t.Fatal("independent PAGE or source-unknown contract changed")
				}
				if age == 7201 {
					requireAlertClass(t, alerts, "points-leaderboard-stale")
				}
				for _, inferred := range []string{"completed_operator_blocks=", "latest_close_age_seconds=", "missing_payment_points=", "pending_rollup_points="} {
					if strings.Contains(page.Observed, inferred) {
						t.Fatal("unavailable operator source was rendered as a numeric observation")
					}
				}
				if !strings.Contains(page.Evidence, "same immutable snapshot") || strings.Contains(page.Evidence, "Three bounded") {
					t.Fatal("snapshot-only PAGE claimed an unread or invalid operator source")
				}
				encoded, marshalErr := json.Marshal(alerts)
				if marshalErr != nil || strings.Contains(string(encoded)+alerts.Markdown(), "synthetic-private-contradiction") {
					t.Fatal("private source error or malformed cell escaped public rendering")
				}
			}
		}
	}
}

// Fresh normal snapshots and calendar-dependent candidates need a valid source.
func TestPointsPartialSourceFailureWithoutSnapshotContradictionRemainsFatal(t *testing.T) {
	readErr := errors.New("synthetic-private-contradiction")
	for _, block := range []uint64{12, 13} {
		for _, control := range []struct {
			rows []Row
			err  error
		}{
			{err: readErr},
			{rows: []Row{{"12", "60", "synthetic-private-contradiction", "0", "0"}}},
		} {
			settings, stages := pointsPartialSettings(t, syntheticPointsSnapshot(60, block, true, 10, 0), []Row{{"true"}}, nil, control.rows, control.err, nil)
			alerts, err := NewPointsReadinessSignal().Run(context.Background(), settings)
			if err == nil || len(alerts) != 0 || strings.Join(*stages, ",") != "snapshot,schema,source" {
				t.Fatal("ordinary fresh failure or unobserved calendar became a product conclusion")
			}
			if control.err != nil && !errors.Is(err, readErr) {
				t.Fatal("ordinary source read error identity was lost")
			}
			if control.err == nil && strings.Contains(err.Error(), "synthetic-private-contradiction") {
				t.Fatal("malformed source leaked private cell text")
			}
		}
	}
}

// This extension does not invent a compatibility policy without schema readiness.
func TestPointsPartialSnapshotContradictionStillRequiresSchema(t *testing.T) {
	for _, snapshot := range [][]Row{
		syntheticPointsSnapshot(60, 1, false, 10, 1),
		syntheticPointsSnapshot(60, 0, true, 10, 0),
	} {
		for _, control := range []struct {
			rows       []Row
			err        error
			knownFalse bool
		}{
			{rows: []Row{{"false"}}, knownFalse: true},
			{},
			{rows: []Row{{"synthetic-private-contradiction"}}},
			{err: errors.New("synthetic-private-contradiction")},
		} {
			settings, stages := pointsPartialSettings(t, snapshot, control.rows, control.err, nil, errors.New("synthetic source must not be read"), nil)
			alerts, err := NewPointsReadinessSignal().Run(context.Background(), settings)
			if strings.Join(*stages, ",") != "snapshot,schema" {
				t.Fatal("unknown schema permitted a dependent source read")
			}
			if control.knownFalse {
				if err != nil || len(alerts) != 1 {
					t.Fatal("schema absence compatibility policy changed")
				}
				if requireAlertClass(t, alerts, "points-operator-schema-unavailable").Severity != SeverityWarn {
					t.Fatal("schema absence automatically paged")
				}
			} else if err == nil || len(alerts) != 0 {
				t.Fatal("unproved schema authorized snapshot availability PAGE")
			}
		}
	}
}

// Complete-source controls preserve real contradictions and legitimate handoffs.
func TestPointsPartialSnapshotContradictionCompleteSourceControls(t *testing.T) {
	for _, control := range []struct {
		snapshot []Row
		source   []Row
		class    string
		severity Severity
	}{
		{snapshot: syntheticPointsSnapshot(60, 1, false, 10, 1), source: syntheticPointsOperatorSource(12, 60, 0, 0, 0), class: "points-epoch-availability-drift", severity: SeverityPage},
		{snapshot: syntheticPointsSnapshot(60, 0, true, 10, 0), source: syntheticPointsOperatorSource(12, 60, 0, 0, 0), class: "points-epoch-availability-drift", severity: SeverityPage},
		{snapshot: syntheticPointsSnapshot(60, 12, true, 10, 0), source: syntheticPointsOperatorSource(12, 60, 0, 0, 0)},
		{snapshot: syntheticPointsSnapshot(60, 0, false, 10, 0), source: syntheticPointsOperatorSource(0, 0, 0, 0, 0)},
		{snapshot: syntheticPointsSnapshot(60, 0, false, 10, 0), source: syntheticPointsOperatorSource(12, 60, 0, 1, 30), class: "points-epoch-metrics-unavailable", severity: SeverityWarn},
		// a pending rollup publishes payout weeks: the current snapshot is valid
		{snapshot: syntheticPointsSnapshot(60, 12, true, 10, 1), source: syntheticPointsOperatorSource(12, 60, 0, 1, 30)},
		{snapshot: syntheticPointsSnapshot(60, 12, true, 10, 1), source: syntheticPointsOperatorSource(12, 60, 1, 0, 0), class: "points-payment-link-missing", severity: SeverityWarn},
		{snapshot: syntheticPointsSnapshot(60, 0, false, 10, 0), source: syntheticPointsOperatorSource(12, 60, 0, 0, 0), class: "points-epoch-metrics-unavailable", severity: SeverityWarn},
		{snapshot: syntheticPointsSnapshot(60, 11, true, 10, 0), source: syntheticPointsOperatorSource(12, 60, 0, 1, 30), class: "points-epoch-rebuild-pending", severity: SeverityWarn},
		{snapshot: syntheticPointsSnapshot(60, 13, true, 10, 0), source: syntheticPointsOperatorSource(12, 60, 0, 0, 0), class: "points-epoch-snapshot-drift", severity: SeverityPage},
	} {
		settings, stages := pointsPartialSettings(t, control.snapshot, []Row{{"true"}}, nil, control.source, nil, nil)
		alerts, err := NewPointsReadinessSignal().Run(context.Background(), settings)
		if err != nil || strings.Join(*stages, ",") != "snapshot,schema,source" {
			t.Fatal("valid source control failed")
		}
		if control.class == "" {
			if len(alerts) != 0 {
				t.Fatal("measured zero or open period became a finding")
			}
		} else if len(alerts) != 1 || requireAlertClass(t, alerts, control.class).Severity != control.severity {
			t.Fatal("complete source control changed class or severity")
		}
	}
}

// Actual owner cancellation discards both independent and unknown conclusions.
func TestPointsPartialSnapshotContradictionCanceledOwnerDiscardsFindings(t *testing.T) {
	for _, snapshot := range [][]Row{
		syntheticPointsSnapshot(60, 1, false, 10, 1),
		syntheticPointsSnapshot(60, 0, true, 10, 0),
	} {
		for _, sourceErr := range []error{nil, errors.New("synthetic-private-contradiction")} {
			ctx, cancel := context.WithCancel(context.Background())
			settings, stages := pointsPartialSettings(t, snapshot, []Row{{"true"}}, nil, syntheticPointsOperatorSource(12, 60, 0, 0, 0), sourceErr, func(stage int) {
				if stage == 3 {
					cancel()
				}
			})
			alerts, err := NewPointsReadinessSignal().Run(ctx, settings)
			cancel()
			if !errors.Is(err, context.Canceled) || len(alerts) != 0 || strings.Join(*stages, ",") != "snapshot,schema,source" {
				t.Fatal("ended parent published an independent PAGE or partial observation")
			}
		}
	}
}
