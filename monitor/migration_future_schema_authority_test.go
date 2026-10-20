// Unknown appended migrations remove live-schema authority from an older
// monitor without removing its immutable-prefix and catalog integrity checks.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The published runtime-blob successor changed one conjunct of this older
// aggregate, demonstrating why a later head can legitimately fail it.
const migrationFutureSchemaArtifact = "isolated automation staging and current lifecycle guards"

// Exercise the public signal with its actual artifact and identity query
// dispatch. The fixture supplies only rows, never the expected alert verdict.
func migrationFutureSchemaAlerts(t *testing.T, signal Signal, name string, artifactRow Row, catalogRows []Row) Alerts {
	t.Helper()
	artifactReads, catalogReads := 0, 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			catalogReads++
			return catalogRows, nil
		}
		if !strings.Contains(query, "WITH version AS") || !strings.Contains(query, "FROM migration_audit") {
			t.Fatalf("%s: unexpected migration source query", name)
		}
		artifactReads++
		return []Row{artifactRow}, nil
	}}
	alerts, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatalf("%s: fixture did not reach the public migration verdict", name)
	}
	wantCatalogReads := 1
	if artifactRow[11] == "f" {
		wantCatalogReads = 0
	}
	if artifactReads != 1 || catalogReads != wantCatalogReads {
		t.Fatalf("%s: source reads artifact=%d catalog=%d, want 1/%d", name, artifactReads, catalogReads, wantCatalogReads)
	}
	return alerts
}

// Unknown suffix identities have no source-side comparison authority; the
// complete numeric range and every known immutable identity still do.
func migrationFutureSchemaCatalogRows() []Row {
	head := server.MigrationCount()
	return append(syntheticMigrationCatalogRows(head), Row{fmt.Sprint(head), strings.Repeat("f", 64)})
}

// One failed predicate is actionable at a known head and ambiguous above it.
// Passing old predicates likewise cannot certify an unknown appended schema.
func TestMigrationsForwardSchemaAuthorityBoundary(t *testing.T) {
	head := server.MigrationCount()
	cases := []struct {
		name            string
		head            int
		missingArtifact bool
		wantClass       string
		wantSeverity    Severity
		wantSustain     int
		wantUnqualified int
	}{
		{name: "known coherent head", head: head},
		{name: "known corrupt artifact", head: head, missingArtifact: true, wantClass: "migration-schema-drift", wantSeverity: SeverityPage, wantSustain: 1},
		{name: "unknown successor changes old artifact", head: head + 1, missingArtifact: true, wantClass: "cannot-observe", wantSeverity: SeverityWarn, wantSustain: 2, wantUnqualified: 1},
		{name: "unknown successor passes old artifacts", head: head + 1, wantClass: "cannot-observe", wantSeverity: SeverityWarn, wantSustain: 2},
	}
	for _, test := range cases {
		row := syntheticMigrationArtifactRow(test.head)
		if test.missingArtifact {
			row = syntheticMigrationMissingArtifactRow(t, test.head, migrationFutureSchemaArtifact)
		}
		catalogRows := syntheticMigrationCatalogRows(head)
		if head < test.head {
			catalogRows = migrationFutureSchemaCatalogRows()
		}
		alerts := migrationFutureSchemaAlerts(t, NewMigrationsSignal(), test.name, row, catalogRows)
		if test.wantClass == "" {
			if len(alerts) != 0 {
				t.Fatalf("%s: coherent current schema produced %d alerts", test.name, len(alerts))
			}
			continue
		}
		if len(alerts) != 1 || alerts[0].Class != test.wantClass {
			t.Fatalf("%s: public verdict must contain only %s", test.name, test.wantClass)
		}
		alert := requireAlertClass(t, alerts, test.wantClass)
		if alert.Severity != test.wantSeverity || alert.Sustain != test.wantSustain || alert.unobservable != (test.wantClass == "cannot-observe") {
			t.Fatalf("%s: severity, sustain or visibility authority changed", test.name)
		}
		if test.wantClass == "cannot-observe" {
			want := fmt.Sprintf("db_version=%d code_required_version=%d unqualified_artifact_predicates=%d", test.head, head, test.wantUnqualified)
			if alert.Observed != want {
				t.Fatalf("%s: missing bounded head and unqualified-predicate evidence", test.name)
			}
			for _, text := range []string{"newer than this monitor", "passing older predicates cannot validate", "Unknown cannot clear", "SIGNALS.md §8.9"} {
				if !strings.Contains(alert.Markdown(), text) {
					t.Fatalf("%s: missing authority guidance %q", test.name, text)
				}
			}
		} else if !strings.Contains(alert.Observed, migrationFutureSchemaArtifact+"@v847") {
			t.Fatalf("%s: known corrupt artifact lost its owning diagnosis", test.name)
		}
		requireAlertOmits(t, alert, catalogRows[0][1], strings.Repeat("f", 64))
	}
}

// A future live-schema ambiguity must not downgrade independently provable
// immutable-prefix, missing-range, duplicate-index or absent-catalog faults.
func TestMigrationsFutureHeadPreservesCatalogFaults(t *testing.T) {
	head := server.MigrationCount()
	for _, fault := range []string{"known identity differs", "incomplete catalog", "duplicate index", "catalog absent"} {
		row := syntheticMigrationMissingArtifactRow(t, head+1, migrationFutureSchemaArtifact)
		catalogRows := migrationFutureSchemaCatalogRows()
		want := "migration_catalog identities@v600"
		switch fault {
		case "known identity differs":
			catalogRows[head-1][1] = strings.Repeat("0", 64)
			want = fmt.Sprintf("migration_catalog identity[%d]@v600", head-1)
		case "incomplete catalog":
			catalogRows = catalogRows[:head]
		case "duplicate index":
			catalogRows[head][0] = "0"
		case "catalog absent":
			row[11] = "f"
		}
		alerts := migrationFutureSchemaAlerts(t, NewMigrationsSignal(), fault, row, catalogRows)
		if len(alerts) != 2 {
			t.Fatalf("%s: need both the independent fault and future-schema unknown", fault)
		}
		page := requireAlertClass(t, alerts, "migration-schema-drift")
		if page.Severity != SeverityPage || page.Sustain != 1 || page.unobservable || !strings.Contains(page.Observed, want) {
			t.Fatalf("%s: independent catalog fault lost its immediate page", fault)
		}
		if strings.Contains(page.Observed, migrationFutureSchemaArtifact) {
			t.Fatalf("%s: unqualified live artifact was promoted into the independent fault", fault)
		}
		unknown := requireAlertClass(t, alerts, "cannot-observe")
		if unknown.Severity != SeverityWarn || !unknown.unobservable || unknown.Sustain != 2 {
			t.Fatalf("%s: future live-schema authority was lost", fault)
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, strings.Repeat("0", 64), strings.Repeat("f", 64), catalogRows[0][1])
		}
	}
}

// The real cadence gate and notifier reducer must retain an open page across
// both held and emitted unknowns, then require two complete known-head runs.
func TestMigrationsFutureHeadCannotClearKnownSchemaIncident(t *testing.T) {
	head := server.MigrationCount()
	signal := NewMigrationsSignal()
	gate := newCadenceAlertGate()
	consumer := &SlackAlertNotifier{
		settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	for run := 0; run < 5; run++ {
		row := syntheticMigrationArtifactRow(head)
		catalogRows := syntheticMigrationCatalogRows(head)
		if run == 0 {
			row = syntheticMigrationMissingArtifactRow(t, head, migrationFutureSchemaArtifact)
		} else if run == 1 || run == 2 {
			row = syntheticMigrationArtifactRow(head + 1)
			if run == 1 {
				row = syntheticMigrationMissingArtifactRow(t, head+1, migrationFutureSchemaArtifact)
			}
			catalogRows = migrationFutureSchemaCatalogRows()
		}
		alerts := migrationFutureSchemaAlerts(t, signal, fmt.Sprintf("consumer run %d", run), row, catalogRows)
		ready, held := gate.split(signal, alerts)
		if run == 1 && (len(ready) != 0 || len(held) != 1 || held[0].Class != "cannot-observe") {
			t.Fatal("first future-head unknown must be held without complete-observation credit")
		}
		if run == 2 && (len(ready) != 1 || ready[0].Class != "cannot-observe" || len(held) != 0) {
			t.Fatal("second future-head unknown must emit despite passing older artifact predicates")
		}
		now := syntheticSettings(nil).Now().Add(time.Duration(run) * time.Minute)
		transitions := consumer.observeWithLock(now, signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
		raised, cleared := 0, 0
		for _, transition := range transitions {
			if transition.alert.Class != "migration-schema-drift" {
				continue
			}
			if transition.kind == slackRaised {
				raised++
			}
			if transition.kind == slackCleared {
				cleared++
			}
		}
		if run == 0 {
			if raised != 1 || cleared != 0 {
				t.Fatal("known corrupt schema did not open exactly one page incident")
			}
		} else if run < 4 {
			if raised != 0 || cleared != 0 {
				t.Fatalf("run %d: ambiguous source or first complete recovery changed the incident", run)
			}
		} else if raised != 0 || cleared != 1 {
			t.Fatal("two complete known-head recovery observations did not clear the incident exactly once")
		}
	}
}
