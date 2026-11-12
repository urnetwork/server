// The capture the flag award network contract (migration_ctf_award_network.go):
// registered at its migration's version, and its predicate admits exactly the
// column and the index the migration publishes. Its predicate is evaluated
// directly here, and through the migrations signal as well
// (migration_current_suffix_test.go).
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// The contract follows its migration's position in the server's list, and
// pins its version's row column with no retirement.
func TestMigrationsCtfAwardNetworkContractFollowsItsVersion(t *testing.T) {
	version := server.CtfAwardNetworkMigrationVersion()
	head := server.MigrationCount()
	if version < 1 || head < version {
		t.Fatalf("ctf award network migration is not published: version=%d head=%d", version, head)
	}
	if schema := server.CtfSchemaMigrationVersion(); version <= schema {
		t.Fatalf("ctf award network migration %d is not after the ctf tables' %d", version, schema)
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatalf("ctf award network contract is %+v", contract.artifact)
	}
}

// One version behind, the column and its index are absent and the predicate
// refuses the schema; at its version the real schema is admitted; and each
// rolled-back shape fault is refused. A release from before the column
// inserts without it, so a required column would refuse its awards, and a
// default would give every award it writes a network.
func TestMigrationsCtfAwardNetworkAdmitsThePublishedColumnAndIndex(t *testing.T) {
	version := server.CtfAwardNetworkMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	faults := []struct{ name, sql string }{
		{"network absent", `ALTER TABLE ctf_award DROP COLUMN network_id`},
		{"network required", `ALTER TABLE ctf_award ALTER COLUMN network_id SET NOT NULL`},
		{"network with a default", `ALTER TABLE ctf_award ALTER COLUMN network_id SET DEFAULT gen_random_uuid()`},
		{"network index absent", `DROP INDEX ctf_award_network`},
		{"network index without the epoch", `DROP INDEX ctf_award_network; CREATE INDEX ctf_award_network ON ctf_award (deployment_key, no_id, network_id)`},
	}
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.ApplyDbMigrationsUpTo(t.Context(), version-1)
		snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
			snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
		})
		server.ApplyDbMigrationsUpTo(t.Context(), version)
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
			})
		}
	})
}
