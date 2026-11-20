// The provider intent contracts (migrations 787 and 788) reject each change
// that would break the probe priority or the provider install category: a
// dropped table, a nullable column and a dropped primary key.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestMigrationsProviderIntentCatalogFaultsReachSignal(t *testing.T) {
	faults := []struct {
		name    string
		version int
		sql     string
	}{
		{name: "missing probe priority", version: 787, sql: `DROP TABLE provider_intent_probe_priority`},
		{name: "nullable probe priority start", version: 787, sql: `ALTER TABLE provider_intent_probe_priority ALTER COLUMN priority_since DROP NOT NULL`},
		{name: "missing probe priority key", version: 787, sql: snMainnetMigrationDropConstraint("provider_intent_probe_priority", "p", "PRIMARY KEY (client_id)")},
		{name: "missing provider install category", version: 788, sql: `DROP TABLE network_client_provider_intent`},
		{name: "nullable provider install time", version: 788, sql: `ALTER TABLE network_client_provider_intent ALTER COLUMN create_time DROP NOT NULL`},
		{name: "missing provider install key", version: 788, sql: snMainnetMigrationDropConstraint("network_client_provider_intent", "p", "PRIMARY KEY (client_id)")},
	}
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				contract := snMainnetMigrationTestContract(t, fault.version)
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				observed = append(observed, snMainnetMigrationAlertObservation{alerts: alerts, want: fmt.Sprintf("%s@v%d", contract.artifact.name, fault.version)})
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back provider intent faults left catalog drift: %s", drift)
		}
	})
	if len(observed) != len(faults) {
		t.Fatalf("observed %d provider intent faults, want %d", len(observed), len(faults))
	}
	for _, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("provider intent drift lost its owning version: %s", alert.Markdown())
		}
	}
}

// Each contract reads absent on a database stopped before its version and
// present from it on.
func TestMigrationsProviderIntentContractsFollowTheirVersions(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		read := func(version int) bool {
			contract := snMainnetMigrationTestContract(t, version)
			var present bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, "SELECT "+contract.query).Scan(&present))
			})
			return present
		}
		server.ApplyDbMigrationsUpTo(ctx, 786)
		if read(787) || read(788) {
			t.Fatal("a provider intent contract reads present before its version")
		}
		server.ApplyDbMigrationsUpTo(ctx, 787)
		if !read(787) || read(788) {
			t.Fatal("the provider intent contracts do not follow version 787")
		}
		server.ApplyDbMigrationsUpTo(ctx, 788)
		if !read(787) || !read(788) {
			t.Fatal("a provider intent contract reads absent after its version")
		}
	})
}
