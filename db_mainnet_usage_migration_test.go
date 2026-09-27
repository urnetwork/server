// Mainnet custody upgrades extend the published production catalog exactly.
package server

import (
	"context"
	"strings"
	"testing"
)

// The integration base af17d1d2 publishes head 721. Its existing identities
// must survive the usage, policy namespace, guard, and archive additions.
func TestMainnetUsageMigrationsPreservePublishedCatalog(t *testing.T) {
	const publishedHead = 721
	const publishedPrefix = "8c563020311a2df64905e48646e46e0641cccf8067fa110d60fdc725b377e7c2"
	prefix, err := migrationPrefixIdentity(publishedHead)
	if err != nil || prefix != publishedPrefix {
		t.Fatalf("published mainnet head changed: identity=%s error=%v", prefix, err)
	}
	if index := sqlMigrationIndex(t, "ADD COLUMN usage_origin_is_source boolean NULL"); index != publishedHead {
		t.Fatalf("provider usage migration index=%d, want append at %d", index, publishedHead)
	}
	for index, expected := range map[int]string{
		723: clientKeyPolicyHistorySchemaSql,
		724: contractUsageGuardSchemaSql,
		725: providerUsageArchiveSchemaSql,
	} {
		migration, ok := migrations[index].(*SqlMigration)
		if !ok || migration.sql != expected {
			t.Fatalf("mainnet custody migration %d changed identity or order", index)
		}
	}
}

// Pin the independently observed v11 catalog, including the reservation
// revision migration. The timestamp lookup may only append after that history.
func TestMainnetUsageTimeMigrationPreservesPublishedCatalog(t *testing.T) {
	const publishedHead = 727
	const publishedPrefix = "1a0031f08dff4648fc4526aa87ebc658d2b4a1b4f89089e137f9526c0ea77eb9"
	prefix, err := migrationPrefixIdentity(publishedHead)
	if err != nil || prefix != publishedPrefix {
		t.Fatalf("published custody catalog changed: identity=%s error=%v", prefix, err)
	}
	index := migrationIndex(t, "transfer_contract_usage_missing_time")
	if index != publishedHead {
		t.Fatalf("missing-time migration index=%d, want %d", index, publishedHead)
	}
	migration, ok := migrations[index].(*OnlineSqlMigration)
	if !ok || len(migration.productionSqlSteps()) != 2 || !strings.Contains(migration.recoverySql, "DROP INDEX CONCURRENTLY IF EXISTS") {
		t.Fatalf("missing-time migration has no independently committed index recovery: %T", migrations[index])
	}
	if strings.Contains(migration.auditSql, "CONCURRENTLY") || !strings.Contains(migration.auditSql, "DROP INDEX IF EXISTS") {
		t.Fatalf("transactional audit cannot reproduce missing-time index recovery: %s", migration.auditSql)
	}
}

// An interrupted concurrent index build can leave a named invalid relation.
// Replaying the production executor must repair it, and must also tolerate a
// crash after successful creation but before recording migration success.
func TestMainnetUsageTimeMigrationRecoversInvalidIndex(t *testing.T) {
	testEnv := DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		migration := migrations[727].(*OnlineSqlMigration)
		Db(ctx, func(conn PgConn) {
			for range 2 {
				RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,outcome)
					VALUES($1,$2,$3,$4,$5,1000,'canceled')`, NewId(), NewId(), NewId(), NewId(), NewId()))
			}
			RaisePgResult(conn.Exec(ctx, `DROP INDEX transfer_contract_usage_missing_time`))
			// A duplicate-key failure deterministically leaves real invalid
			// concurrent-build residue without editing PostgreSQL catalogs.
			if _, err := conn.Exec(ctx, `CREATE UNIQUE INDEX CONCURRENTLY transfer_contract_usage_missing_time ON transfer_contract (outcome)`); err == nil {
				t.Fatal("synthetic concurrent uniqueness failure did not occur")
			}
			var invalid bool
			Raise(conn.QueryRow(ctx, `SELECT NOT indisvalid FROM pg_index WHERE indexrelid='transfer_contract_usage_missing_time'::regclass`).Scan(&invalid))
			if !invalid {
				t.Fatal("failed concurrent build did not retain invalid residue")
			}
			for range 2 {
				Raise(executeOnlineSqlMigration(ctx, migration, func(ctx context.Context, sql string) error {
					_, err := conn.Exec(ctx, sql)
					return err
				}))
				var valid, ready bool
				Raise(conn.QueryRow(ctx, `SELECT indisvalid,indisready FROM pg_index WHERE indexrelid='transfer_contract_usage_missing_time'::regclass`).Scan(&valid, &ready))
				if !valid || !ready {
					t.Fatalf("replayed index remains invalid/not ready: %t/%t", valid, ready)
				}
			}
		})
	})
}
