package monitor

import (
	"context"
	"strings"
	"testing"
)

// A false-only generated-column sample must retain the structural migration
// and bounded sampling target as the durable correction.
func TestPlannerFlipsSignalSyntheticStatsLandmine(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		for _, want := range []string{
			"transfer_contract_outcome_null",
			"pg_get_indexdef",
			"pg_get_expr",
			"index_predicate ILIKE '%CASE%'",
			"(source_id, destination_id, create_time) INCLUDE (contract_id, companion_contract_id, transfer_byte_count, priority)",
			"(destination_id, source_id, create_time) INCLUDE (contract_id, companion_contract_id, transfer_byte_count, priority)",
			"(payer_network_id) INCLUDE (transfer_byte_count)",
			"source_id IS NOT NULL",
			"destination_id IS NOT NULL",
			"payer_network_id IS NOT NULL",
			"attstattarget",
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("stats-landmine query omits structural check %q:\n%s", want, query)
			}
		}
		return []Row{{"1", "0", "0", "10000"}}, nil
	}}
	alerts, err := NewPlannerFlipsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "stats-landmine")
	markdown := alerts.Markdown()
	if !strings.Contains(markdown, "target 10000") ||
		!strings.Contains(markdown, "valid structural index shapes=0") ||
		!strings.Contains(markdown, "target at 300") {
		t.Fatalf("stats-landmine Markdown lacks root-cause guidance:\n%s", markdown)
	}
}

// Once all three structural families exist, false-zero legacy estimates are no
// longer an availability boundary.
func TestPlannerFlipsSignalSyntheticStructuralBoundaryToleratesEmptyLegacyStats(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{{"1", "2", "3", "300"}}, nil }}
	alerts, err := NewPlannerFlipsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("isolated structural plans emitted a legacy-statistics alert:\n%s", alerts.Markdown())
	}
}

// Healthy statistics without all three access families are not a durable
// state; a later false-only sample would re-expose deployed pair readers.
func TestPlannerFlipsSignalSyntheticMissingStructuralIndex(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{{"2", "0", "2", "300"}}, nil }}
	alerts, err := NewPlannerFlipsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "stats-landmine")
}

// Both catalog visibility and the structural access families must clear.
func TestPlannerFlipsSignalSyntheticHealthyStructuralPlan(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{{"2", "0", "3", "300"}}, nil }}
	alerts, err := NewPlannerFlipsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy structural plan emitted alerts:\n%s", alerts.Markdown())
	}
}

func TestPlannerFlipsSignalSyntheticRejectsUnboundedAnalyzeTarget(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{{"2", "0", "3", "10000"}}, nil }}
	alerts, err := NewPlannerFlipsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "stats-landmine").Markdown()
	for _, want := range []string{"statistics target=10000", "target to 300", "ANALYZE transfer_contract (open)"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("unbounded target alert missing %q:\n%s", want, markdown)
		}
	}
}
