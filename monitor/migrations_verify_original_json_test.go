// The monitor shares startup's exact historical request-reader identities.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Repair installation preserves old catalog hashes; a repaired current head
// must not be diagnosed as identity drift merely for retaining its true history.
func TestMigrationsSignalAcceptsRepairedOriginalRequestHistory(t *testing.T) {
	head := server.MigrationCount()
	rows := syntheticMigrationCatalogRows(head)
	rows[772][1] = "6166efbed5e36f5b416b2771cc96ebbc0a3aef9767a37e7e700201face01b212"
	rows[774][1] = "2780bc1901e2fafb9d1d53955ea2810597d68faddbab5db6d41141e8afe2a509"
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return rows, nil
		}
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("repaired exact original-request history was rejected: alerts=%+v err=%v", alerts, err)
	}
}

// A recognized old hash at a different slot still contradicts durable history.
func TestMigrationsSignalRejectsMisplacedOriginalRequestHistory(t *testing.T) {
	head := server.MigrationCount()
	rows := syntheticMigrationCatalogRows(head)
	rows[772][1] = "2780bc1901e2fafb9d1d53955ea2810597d68faddbab5db6d41141e8afe2a509"
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return rows, nil
		}
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if report := requireAlertClass(t, alerts, "migration-schema-drift").Markdown(); !strings.Contains(report, "migration_catalog identity[772]@v600") {
		t.Fatalf("cross-slot historical identity was not refused: %s", report)
	}
}
