// The per-cycle Pro grant contract (migration_pro_grant_cycle.go): registered at
// its migration's version, and its predicate admits exactly the schema the
// migration publishes. Its predicate is evaluated directly rather than through
// the migrations signal, which reads a head beyond a gap in the catalog's
// contracts as cannot-observe.
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The contract follows its migration's position in the server's list, and pins
// its version's row column with no retirement.
func TestMigrationsProGrantCycleContractFollowsItsVersion(t *testing.T) {
	version := server.ProGrantCycleVersion()
	head := server.MigrationCount()
	if version < 1 || head < version {
		t.Fatalf("Pro grant cycle migration is not published: version=%d head=%d", version, head)
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatalf("Pro grant cycle contract is %+v", contract.artifact)
	}
}

// One version behind, the column and the table are absent and the predicate
// refuses the schema; at its version the real schema is admitted; and each
// rolled-back fault of what it publishes is refused. Every credit path writes
// the paid end and only a hold leaves it out, so a default would read every hold
// as paid; one grant per network and cycle rests on the key; a clawback rests on
// ended_time; the sweep scans running cycles by their end.
func TestMigrationsProGrantCycleAdmitsThePublishedSchema(t *testing.T) {
	version := server.ProGrantCycleVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	faults := []struct{ name, sql string }{
		{"renewal paid end absent", `ALTER TABLE subscription_renewal DROP COLUMN paid_end_time`},
		{"renewal paid end defaults to paid", `ALTER TABLE subscription_renewal ALTER COLUMN paid_end_time SET DEFAULT now()`},
		{"renewal paid end required", `ALTER TABLE subscription_renewal ALTER COLUMN paid_end_time SET NOT NULL`},
		{"cycles absent", `DROP TABLE pro_grant_cycle`},
		{"cycles unkeyed", `ALTER TABLE pro_grant_cycle DROP CONSTRAINT pro_grant_cycle_pkey`},
		{"cycle clawback absent", `ALTER TABLE pro_grant_cycle DROP COLUMN ended_time`},
		{"cycle grant required", `ALTER TABLE pro_grant_cycle ALTER COLUMN balance_id SET NOT NULL`},
		{"cycle end index absent", `DROP INDEX pro_grant_cycle_cycle_end_time`},
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
