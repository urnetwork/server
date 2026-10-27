// Exercise source authority through the registered planner signal, without SQL or host contact.
package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// One native aggregate always returns seven columns, including zero plain-table partition counts.
func TestPlannerFlipsSourceShapeHealthy(t *testing.T) {
	calls := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		calls++
		if query != pgStatsLandmineSQL {
			t.Fatal("planner control did not use the owning aggregate query")
		}
		return []Row{{"2", "0", "3", "300", "0", "0", ""}}, nil
	}}
	alerts, err := NewPlannerFlipsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 || calls != 1 {
		t.Fatalf("complete healthy source: calls=%d alerts=%d error=%v", calls, len(alerts), err)
	}
}

// Missing data must not become either an index-defect finding or an implicit healthy observation.
func TestPlannerFlipsSourceShapeRejectsUnavailableRows(t *testing.T) {
	healthy := Row{"2", "0", "3", "300", "0", "0", ""}
	for _, control := range []struct {
		name string
		rows []Row
	}{
		{name: "empty", rows: nil},
		{name: "truncated", rows: []Row{{"2", "0", "3", "300"}}},
		{name: "extra-column", rows: []Row{{"2", "0", "3", "300", "0", "0", "", "unexpected"}}},
		{name: "duplicate", rows: []Row{healthy, healthy}},
	} {
		calls := 0
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			calls++
			return control.rows, nil
		}}
		alerts, err := NewPlannerFlipsSignal().Run(context.Background(), syntheticSettings(source))
		if err == nil || len(alerts) != 0 || calls != 1 {
			t.Errorf("%s: unavailable aggregate gained a verdict: calls=%d alerts=%d error=%v", control.name, calls, len(alerts), err)
		}
	}
}

// Counts come from count(*) and cannot be silently rounded, defaulted, negative or contradictory.
func TestPlannerFlipsSourceShapeRejectsMalformedFields(t *testing.T) {
	const privateMarker = "synthetic-private-source-field"
	for _, control := range []struct {
		name string
		row  Row
	}{
		{name: "bad-legacy-count", row: Row{"2", privateMarker, "3", "300", "0", "0", ""}},
		{name: "rounded-shape-count", row: Row{"2", "0", "3.5", "300", "0", "0", ""}},
		{name: "bad-leaf-count", row: Row{"2", "0", "3", "300", privateMarker, "0", ""}},
		{name: "bad-mismatch-count", row: Row{"2", "0", "3", "300", "2", privateMarker, ""}},
		{name: "negative-count", row: Row{"2", "-1", "3", "300", "0", "0", ""}},
		{name: "mismatch-exceeds-leaves", row: Row{"2", "0", "3", "300", "1", "2", "synthetic_leaf=100"}},
		{name: "too-many-structural-shapes", row: Row{"2", "0", "4", "300", "0", "0", ""}},
		{name: "bad-legacy-statistic", row: Row{privateMarker, "0", "3", "300", "0", "0", ""}},
		{name: "nonfinite-legacy-statistic", row: Row{"NaN", "0", "3", "300", "0", "0", ""}},
		{name: "bad-statistics-target", row: Row{"2", "0", "3", privateMarker, "0", "0", ""}},
		{name: "missing-partition-witness", row: Row{"2", "0", "3", "300", "2", "1", ""}},
		{name: "unexpected-partition-witness", row: Row{"2", "0", "3", "300", "0", "0", "synthetic_leaf=100"}},
	} {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			return []Row{control.row}, nil
		}}
		alerts, err := NewPlannerFlipsSignal().Run(context.Background(), syntheticSettings(source))
		if err == nil || len(alerts) != 0 {
			t.Errorf("%s: malformed evidence gained a verdict: alerts=%d error=%v", control.name, len(alerts), err)
		}
		if err != nil && strings.Contains(err.Error(), privateMarker) {
			t.Errorf("%s: invalid private source entered public error", control.name)
		}
	}
}

// The valid missing-statistic sentinel is not an unavailable aggregate or a structural failure.
func TestPlannerFlipsSourceShapeMissingLegacyStatsRemainHealthy(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"missing", "0", "3", "300", "0", "0", ""}}, nil
	}}
	alerts, err := NewPlannerFlipsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("complete structural source depended on legacy statistics: alerts=%d error=%v", len(alerts), err)
	}
}

// A complete violated source retains the genuine structural warning and its bounded action.
func TestPlannerFlipsSourceShapeViolatedSourceStillWarns(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"2", "0", "2", "300", "0", "0", ""}}, nil
	}}
	alerts, err := NewPlannerFlipsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 1 {
		t.Fatalf("valid structural defect disappeared: alerts=%d error=%v", len(alerts), err)
	}
	alert := requireAlertClass(t, alerts, "stats-landmine")
	if alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, "valid_structural_index_shapes=2") {
		t.Fatal("valid structural defect lost its original severity or observed count")
	}
}

// The parent remains authoritative even when source bytes arrive after cancellation.
func TestPlannerFlipsSourceShapeCancellationPublishesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		cancel()
		return nil, nil
	}}
	alerts, err := NewPlannerFlipsSignal().Run(ctx, syntheticSettings(source))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("ended parent gained a source verdict: alerts=%d error=%v", len(alerts), err)
	}
}
