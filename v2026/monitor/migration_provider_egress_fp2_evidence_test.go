// The FP2 URL evidence contract (migration_provider_egress_fp2_evidence.go):
// registered at its migration's version, and its predicate admits exactly the
// two columns the migration publishes. Its predicate is evaluated directly,
// as the onboarding distribution contract's is.
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The contract follows its migration's position in the server's list, and
// pins its version's row column with no retirement.
func TestMigrationsProviderEgressFp2EvidenceContractFollowsItsVersion(t *testing.T) {
	version := server.ProviderEgressFp2EvidenceMigrationVersion()
	head := server.MigrationCount()
	if version < 1 || head < version {
		t.Fatalf("FP2 URL evidence migration is not published: version=%d head=%d", version, head)
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatalf("FP2 URL evidence contract is %+v", contract.artifact)
	}
}

// One version behind the columns are absent and the predicate refuses them; at
// its version the real schema is admitted; and each rolled-back shape fault of
// either published column is refused.
func TestMigrationsProviderEgressFp2EvidenceAdmitsThePublishedColumns(t *testing.T) {
	version := server.ProviderEgressFp2EvidenceMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	faults := []struct{ name, sql string }{
		{"failed later legs absent", `ALTER TABLE provider_egress_health_history DROP COLUMN failed_later_legs`},
		{"failed later legs widened", `ALTER TABLE provider_egress_health_history ALTER COLUMN failed_later_legs TYPE integer`},
		{"failed later legs nullable", `ALTER TABLE provider_egress_health_history ALTER COLUMN failed_later_legs DROP NOT NULL`},
		{"failed later legs without its default", `ALTER TABLE provider_egress_health_history ALTER COLUMN failed_later_legs DROP DEFAULT`},
		{"failed later legs with another default", `ALTER TABLE provider_egress_health_history ALTER COLUMN failed_later_legs SET DEFAULT 7`},
		{"post-quantum outcome absent", `ALTER TABLE provider_egress_health_history DROP COLUMN pq_outcome`},
		{"post-quantum outcome widened", `ALTER TABLE provider_egress_health_history ALTER COLUMN pq_outcome TYPE integer`},
		{"post-quantum outcome nullable", `ALTER TABLE provider_egress_health_history ALTER COLUMN pq_outcome DROP NOT NULL`},
		{"post-quantum outcome without its default", `ALTER TABLE provider_egress_health_history ALTER COLUMN pq_outcome DROP DEFAULT`},
		{"post-quantum outcome defaults to positive", `ALTER TABLE provider_egress_health_history ALTER COLUMN pq_outcome SET DEFAULT 1`},
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
