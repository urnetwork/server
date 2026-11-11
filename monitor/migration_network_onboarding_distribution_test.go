// The onboarding distribution contract (migration_network_onboarding_distribution.go):
// registered at its migration's version, and its predicate admits exactly the
// column the migration publishes. Its predicate is evaluated directly rather
// than through the migrations signal, which reads a head beyond a gap in the
// catalog's contracts as cannot-observe.
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// The contract follows its migration's position in the server's list, and
// pins its version's row column with no retirement.
func TestMigrationsNetworkOnboardingDistributionContractFollowsItsVersion(t *testing.T) {
	version := server.NetworkOnboardingDistributionMigrationVersion()
	head := server.MigrationCount()
	if version < 1 || head < version {
		t.Fatalf("onboarding distribution migration is not published: version=%d head=%d", version, head)
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatalf("onboarding distribution contract is %+v", contract.artifact)
	}
}

// One version behind the column is absent and the predicate refuses it; at its
// version the real schema is admitted; and each rolled-back shape fault of the
// published column is refused.
func TestMigrationsNetworkOnboardingDistributionAdmitsThePublishedColumn(t *testing.T) {
	version := server.NetworkOnboardingDistributionMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	faults := []struct{ name, sql string }{
		{"distribution absent", `ALTER TABLE network_onboarding DROP COLUMN distribution`},
		{"distribution widened", `ALTER TABLE network_onboarding ALTER COLUMN distribution TYPE varchar(64)`},
		{"distribution as text", `ALTER TABLE network_onboarding ALTER COLUMN distribution TYPE text`},
		{"distribution nullable", `ALTER TABLE network_onboarding ALTER COLUMN distribution DROP NOT NULL`},
		{"distribution without its default", `ALTER TABLE network_onboarding ALTER COLUMN distribution DROP DEFAULT`},
		{"invented distribution default", `ALTER TABLE network_onboarding ALTER COLUMN distribution SET DEFAULT 'app_store'`},
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
