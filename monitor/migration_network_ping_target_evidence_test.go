// The ping target evidence contract (migration_network_ping_target_evidence.go):
// registered at its migration's version, and its predicate admits exactly the
// column the migration publishes. Its predicate is evaluated directly as well
// as through the migrations signal (migration_current_suffix_test.go).
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// The contract follows its migration's position in the server's list, and
// pins its version's row column with no retirement.
func TestMigrationsNetworkPingTargetEvidenceContractFollowsItsVersion(t *testing.T) {
	version := server.NetworkPingTargetEvidenceSchemaMigrationVersion()
	head := server.MigrationCount()
	if version < 1 || head < version {
		t.Fatalf("ping target evidence migration is not published: version=%d head=%d", version, head)
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatalf("ping target evidence contract is %+v", contract.artifact)
	}
}

// One version behind, the column is absent and the predicate refuses the
// schema; at its version the real schema is admitted; and each rolled-back
// shape fault of the published column is refused. A release from before the
// column inserts without it, so a required column would refuse its pings, and
// a default would read the evidence of every row it stores as none.
func TestMigrationsNetworkPingTargetEvidenceAdmitsThePublishedColumn(t *testing.T) {
	version := server.NetworkPingTargetEvidenceSchemaMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	faults := []struct{ name, sql string }{
		{"evidence count absent", `ALTER TABLE network_ping_target_hour_tally DROP COLUMN evidence_rejection_count`},
		{"evidence count narrowed", `ALTER TABLE network_ping_target_hour_tally ALTER COLUMN evidence_rejection_count TYPE integer`},
		{"evidence count required", `ALTER TABLE network_ping_target_hour_tally ALTER COLUMN evidence_rejection_count SET NOT NULL`},
		{"evidence count defaults to none", `ALTER TABLE network_ping_target_hour_tally ALTER COLUMN evidence_rejection_count SET DEFAULT 0`},
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
