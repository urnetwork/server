package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Pure: the record's contract is the suffix contract of its migration's
// position and pins the constraint and the index name.
func TestAuditDeviceEventUniqueRecordContractFollowsItsMigration(t *testing.T) {
	version := server.AuditDeviceEventUniqueRecordVersion()
	if version <= 0 || server.MigrationCount() < version {
		t.Fatalf("the record migration is at %d of %d", version, server.MigrationCount())
	}
	artifact := auditDeviceEventUniqueRecordMigrationContract.artifact
	if artifact.requiredVersion != version || artifact.rowColumn != version-589 {
		t.Fatalf("%s at version %d column %d, want version %d", artifact.name, artifact.requiredVersion, artifact.rowColumn, version)
	}
	if migrationCurrentSuffixTestContract(t, version).query != auditDeviceEventUniqueRecordMigrationContract.query {
		t.Fatal("the record's contract is not the suffix contract of its version")
	}
	for _, pin := range []string{
		"constraint_name = 'audit_device_event_event_id_key'",
		"index_name = 'audit_device_event_event_id_key'",
	} {
		if !strings.Contains(auditDeviceEventUniqueRecordMigrationContract.query, pin) {
			t.Fatalf("the record's contract does not pin %s", pin)
		}
	}
}

// DB-backed: the constraint or a unique index of its name, rebuilt, reaches
// the signal as drift of the record's migration.
func TestAuditDeviceEventUniqueRecordFaultsReachSignal(t *testing.T) {
	version := server.AuditDeviceEventUniqueRecordVersion()
	faults := []struct {
		name string
		sql  string
	}{
		{"unique event id constraint rebuilt", `ALTER TABLE audit_device_event ADD CONSTRAINT audit_device_event_event_id_key UNIQUE (event_id)`},
		{"unique event id index rebuilt", `CREATE UNIQUE INDEX audit_device_event_event_id_key ON audit_device_event (event_id)`},
	}
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("the healthy catalog reports drift: %s", drift)
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				artifact := migrationCurrentSuffixTestContract(t, version).artifact
				observed = append(observed, snMainnetMigrationAlertObservation{
					alerts: alerts, want: fmt.Sprintf("%s@v%d", artifact.name, version),
				})
			})
		}
	})
	if len(observed) != len(faults) {
		t.Fatalf("observed %d schema faults, want %d", len(observed), len(faults))
	}
	for index, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("%s lost its owning migration: %s", faults[index].name, alert.Markdown())
		}
	}
}
