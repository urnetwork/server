// The Bitcoin scanner's kept-chain contract (migration_crypto_scan_chain.go):
// registered at its migration's version, and its predicate admits exactly the
// column the migration publishes. Its predicate is evaluated directly rather
// than through the migrations signal, which reads a head beyond a gap in the
// catalog's contracts as cannot-observe.
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The contract follows its migration's position in the server's list, and
// pins its version's row column with no retirement.
func TestMigrationsCryptoScanChainContractFollowsItsVersion(t *testing.T) {
	version := server.CryptoScanChainVersion()
	head := server.MigrationCount()
	if version < 1 || head < version {
		t.Fatalf("kept-chain migration is not published: version=%d head=%d", version, head)
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatalf("kept-chain contract is %+v", contract.artifact)
	}
}

// One version behind the column is absent and the predicate refuses it; at its
// version the real schema is admitted; and each rolled-back shape fault of the
// published column is refused.
func TestMigrationsCryptoScanChainAdmitsThePublishedColumn(t *testing.T) {
	version := server.CryptoScanChainVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	faults := []struct{ name, sql string }{
		{"recent_blocks absent", `ALTER TABLE crypto_scan_cursor DROP COLUMN recent_blocks`},
		{"recent_blocks as text", `ALTER TABLE crypto_scan_cursor ALTER COLUMN recent_blocks TYPE text`},
		{"recent_blocks required", `ALTER TABLE crypto_scan_cursor ALTER COLUMN recent_blocks SET NOT NULL`},
		{"invented recent_blocks default", `ALTER TABLE crypto_scan_cursor ALTER COLUMN recent_blocks SET DEFAULT '{}'::jsonb`},
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
