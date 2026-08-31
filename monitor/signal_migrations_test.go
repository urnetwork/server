package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func TestMigrationsSignalReportsDeploymentGateWithoutFalseSchemaDrift(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if !strings.Contains(query, "migration_audit") ||
			!strings.Contains(query, "transfer_escrow_balance_contract") ||
			!strings.Contains(query, "transfer_escrow_unsettled_balance_contract") {
			t.Fatalf("migration query is missing required evidence:\n%s", query)
		}
		return []Row{{"590", "t", "t", "t", "f", "f", "f", "f", "f", "f", "f", "f", "f"}}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one deployment gate: %+v", len(alerts), alerts)
	}
	markdown := requireAlertClass(t, alerts, "migration-behind").Markdown()
	for _, want := range []string{"migration head 590", fmt.Sprintf("head %d", server.MigrationCount()), "dependent taskworkers", "db_version=590"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("migration-behind Markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestMigrationsSignalReportsRecordedVersionWithoutArtifact(t *testing.T) {
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return []Row{{fmt.Sprint(head), "0", fmt.Sprint(head - 1)}}, nil
		}
		return []Row{{fmt.Sprint(head), "t", "t", "t", "t", "t", "t", "f", "t", "t", "t", "t", "t"}}, nil
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
	for _, want := range []string{fmt.Sprintf("version %d", head), "transfer_escrow_balance_contract@v594", "original index", "Do not edit migration_audit"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("migration-schema-drift Markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestMigrationsSignalHealthyAtCoherentHead(t *testing.T) {
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return []Row{{fmt.Sprint(head), "0", fmt.Sprint(head - 1)}}, nil
		}
		return []Row{{fmt.Sprint(head), "t", "t", "t", "t", "t", "t", "t", "t", "t", "t", "t", "t"}}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("coherent migration head produced alerts: %+v", alerts)
	}
}

func TestMigrationsSignalUsesCurrentServerMigrationCount(t *testing.T) {
	head := server.MigrationCount()
	if head <= 597 {
		t.Fatalf("test requires migrations newer than the former hard-coded head, got %d", head)
	}
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{fmt.Sprint(head - 1), "t", "t", "t", "t", "t", "t", "t", "t", "t", "t", "t", "f"}}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "migration-behind").Markdown()
	for _, want := range []string{
		fmt.Sprintf("head %d", head-1),
		fmt.Sprintf("code-required head %d", head),
		"lag=1",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("dynamic migration-head alert missing %q:\n%s", want, markdown)
		}
	}
}

func TestMigrationsSignalRejectsIncompleteIdentityCatalog(t *testing.T) {
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return []Row{{fmt.Sprint(head - 1), "0", fmt.Sprint(head - 2)}}, nil
		}
		return []Row{{fmt.Sprint(head), "t", "t", "t", "t", "t", "t", "t", "t", "t", "t", "t", "t"}}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "migration-schema-drift").Markdown()
	if !strings.Contains(markdown, "migration_catalog identities@v600") {
		t.Fatalf("incomplete durable migration catalog was not diagnosed:\n%s", markdown)
	}
}

func TestMigrationsSignalReportsMissingUnsettledEscrowIndex(t *testing.T) {
	head := server.MigrationCount()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return []Row{{fmt.Sprint(head), "0", fmt.Sprint(head - 1)}}, nil
		}
		return []Row{{fmt.Sprint(head), "t", "t", "t", "t", "t", "t", "t", "t", "t", "t", "t", "f"}}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "migration-schema-drift").Markdown()
	for _, want := range []string{
		"transfer_escrow_unsettled_balance_contract@v601",
		fmt.Sprintf("db_version=%d", head),
		"Stop dependent service activation",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("missing unsettled-index alert detail %q:\n%s", want, markdown)
		}
	}
}
