// Recovery metadata is nullable historical evidence; the emitted migration
// probe must reject schema that loses or invents that authority.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Execute the complete probe on the published schema and rolled-back faults.
// A numeric current head alone must not conceal an unobserved appended column.
func TestMigrationsCurrentSuffixRecoveryMetadataFaultsReachSignal(t *testing.T) {
	faults := []struct {
		name    string
		version int
		sql     string
	}{
		{name: "missing registration session", version: 840, sql: `ALTER TABLE network_client_registration DROP COLUMN session_id`},
		{name: "wrong registration session type", version: 840, sql: `ALTER TABLE network_client_registration ALTER COLUMN session_id TYPE text`},
		{name: "required historical registration session", version: 840, sql: `ALTER TABLE network_client_registration ALTER COLUMN session_id SET NOT NULL`},
		{name: "invented historical registration session", version: 840, sql: `ALTER TABLE network_client_registration ALTER COLUMN session_id SET DEFAULT '00000000-0000-0000-0000-000000000000'::uuid`},
		{name: "missing registration session create time", version: 840, sql: `ALTER TABLE network_client_registration DROP COLUMN session_create_time`},
		{name: "wrong registration session create time type", version: 840, sql: `ALTER TABLE network_client_registration ALTER COLUMN session_create_time TYPE date`},
		{name: "required historical registration create time", version: 840, sql: `ALTER TABLE network_client_registration ALTER COLUMN session_create_time SET NOT NULL`},
		{name: "invented historical registration create time", version: 840, sql: `ALTER TABLE network_client_registration ALTER COLUMN session_create_time SET DEFAULT now()`},
		{name: "missing registration session acceptance bound", version: 840, sql: `ALTER TABLE network_client_registration DROP COLUMN session_accept_until`},
		{name: "wrong registration session acceptance type", version: 840, sql: `ALTER TABLE network_client_registration ALTER COLUMN session_accept_until TYPE date`},
		{name: "required historical registration acceptance bound", version: 840, sql: `ALTER TABLE network_client_registration ALTER COLUMN session_accept_until SET NOT NULL`},
		{name: "invented historical registration acceptance bound", version: 840, sql: `ALTER TABLE network_client_registration ALTER COLUMN session_accept_until SET DEFAULT now()`},
		{name: "missing retained seed window", version: 841, sql: `ALTER TABLE verify_original_transition DROP COLUMN head_window`},
		{name: "widened retained seed window", version: 841, sql: `ALTER TABLE verify_original_transition ALTER COLUMN head_window TYPE varchar(33)`},
		{name: "required historical seed window", version: 841, sql: `ALTER TABLE verify_original_transition ALTER COLUMN head_window SET NOT NULL`},
		{name: "invented historical seed window", version: 841, sql: `ALTER TABLE verify_original_transition ALTER COLUMN head_window SET DEFAULT 'synthetic-window'`},
	}
	var observed [][]Alert
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("published recovery metadata reports schema drift: %s", drift)
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
				observed = append(observed, alerts)
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back recovery metadata faults left schema drift: %s", drift)
		}
	})
	if len(observed) != len(faults) {
		t.Fatalf("observed %d recovery metadata faults, want %d", len(observed), len(faults))
	}
	for index, alerts := range observed {
		fault := faults[index]
		want := fmt.Sprintf("@v%d", fault.version)
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("%s lost its owning migration %s: %s", fault.name, want, alert.Markdown())
		}
	}
}
