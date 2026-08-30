package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestMigrationsSignalReportsDeploymentGateWithoutFalseSchemaDrift(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if !strings.Contains(query, "migration_audit") || !strings.Contains(query, "transfer_escrow_balance_contract") {
			t.Fatalf("migration query is missing required evidence:\n%s", query)
		}
		return []Row{{"590", "t", "t", "t", "f", "f", "f", "f", "f", "f", "f"}}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one deployment gate: %+v", len(alerts), alerts)
	}
	markdown := requireAlertClass(t, alerts, "migration-behind").Markdown()
	for _, want := range []string{"migration head 590", "head 597", "dependent taskworkers", "db_version=590"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("migration-behind Markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestMigrationsSignalReportsRecordedVersionWithoutArtifact(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"597", "t", "t", "t", "t", "t", "t", "f", "t", "t", "t"}}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one schema-drift page: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "migration-schema-drift")
	if alert.Severity != SeverityPage {
		t.Fatalf("schema drift severity = %q, want page", alert.Severity)
	}
	markdown := alert.Markdown()
	for _, want := range []string{"version 597", "transfer_escrow_balance_contract@v594", "original index", "Do not edit migration_audit"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("migration-schema-drift Markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestMigrationsSignalHealthyAtCoherentHead(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"597", "t", "t", "t", "t", "t", "t", "t", "t", "t", "t"}}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("coherent migration head produced alerts: %+v", alerts)
	}
}
