// Mainnet custody upgrades extend the published production catalog exactly.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
)

// Main 4479188a publishes 741 entries, including URL/ARIN probing and grant
// selection indexes. Its complete prefix precedes the independent hardening.
func TestMainnetMergePreservesMainPublishedCatalog(t *testing.T) {
	const publishedHead = 741
	const publishedPrefix = "f92ed04cf4abebc7d840dcbbe1cc8ba7ec1cae1771d6e491160a6ea82e3c56f4"
	prefix, err := migrationPrefixIdentity(publishedHead)
	if err != nil || prefix != publishedPrefix {
		t.Fatalf("published main head changed: identity=%s error=%v", prefix, err)
	}
	if len(migrations) < 749 {
		t.Fatal("merged catalog lost a hardening append")
	}
	for offset, expected := range mainnetHardeningMigrationIdentities() {
		identity, err := MigrationIdentity(publishedHead + offset)
		if err != nil || identity != expected {
			t.Fatalf("hardening append %d changed: identity=%s error=%v", offset, identity, err)
		}
	}
}

// Independent constants captured from hardening 898dc8f3 keep all eight SQL
// payloads exact while their unpublished numeric positions move past main.
func mainnetHardeningMigrationIdentities() []string {
	return []string{
		"1f29bf5b246588066c9f00fd31d5952ffb8bdec3f11b2bea5dba87a4e1cba7b7",
		"0d7f8f261411b3ed484f837222d4e15dfb3d739177cffdf11dad37919b135db9",
		"1f2b9e4c0694f1b29f31064170fe86be9811b841a86ab1197683587e79fc1396",
		"8e29ae3c76768003adfe9dadc19558bc19c2c229d33544b6a53cce94ec432294",
		"e959f97afcc5edaabbda36dd23d493d43a1afdadf7a7adc4004a4c1bd411c720",
		"77b2f1e2b81fe31be8b7e2930b991d04817025a68c9d3a8d5e265a1b1364b953",
		"e6988779d0ea648ade962a89e08c817fb47b72eb5aae07271a4aa9595d163902",
		"294b099185f27c514ea4f7b9fb7467e2098d2f4620a57bbadf12d8af57f70282",
	}
}

// A retained pre-merge branch database has different SQL recorded at 721.
// It must be refused rather than silently renamed to main's ARIN migration.
func TestMainnetMergeRefusesConflictingHardeningCatalog(t *testing.T) {
	for version := 722; version <= 729; version++ {
		entries := testMigrationCatalogEntries(t, version)
		for offset, identity := range mainnetHardeningMigrationIdentities()[:version-721] {
			entries[721+offset].Identity = identity
		}
		err := validateMigrationCatalog(version, entries)
		if err == nil || !strings.Contains(err.Error(), "migration 721 identity differs from durable catalog") {
			t.Fatalf("conflicting branch head %d was not refused: %v", version, err)
		}
	}
}

// A production main database upgrades through every relocated append without
// rewriting a catalog row, and a second startup remains an audit no-op.
func TestMainnetMergeUpgradesPublishedMainWithoutReplay(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		for version := 741; version <= 749; version++ {
			ApplyDbMigrationsUpTo(ctx, version)
			if current := DbVersion(ctx); current != version {
				t.Fatalf("merged upgrade version=%d, want %d", current, version)
			}
			verifyMigrationCatalog(ctx, version)
		}
		auditCount := func() int {
			var count int
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `SELECT count(*) FROM migration_audit`).Scan(&count))
			}, OptReadOnly(), OptNoRetry())
			return count
		}
		before := auditCount()
		ApplyDbMigrationsUpTo(ctx, 749)
		if after := auditCount(); after != before {
			t.Fatalf("merged restart replayed migration audit: %d -> %d", before, after)
		}
	})
}

// The integration base af17d1d2 publishes head 721. Its existing identities
// must survive the usage, policy namespace, guard, and archive additions.
func TestMainnetUsageMigrationsPreservePublishedCatalog(t *testing.T) {
	const publishedHead = 721
	const publishedPrefix = "8c563020311a2df64905e48646e46e0641cccf8067fa110d60fdc725b377e7c2"
	prefix, err := migrationPrefixIdentity(publishedHead)
	if err != nil || prefix != publishedPrefix {
		t.Fatalf("published mainnet head changed: identity=%s error=%v", prefix, err)
	}
	if index := sqlMigrationIndex(t, "ADD COLUMN usage_origin_is_source boolean NULL"); index != 741 {
		t.Fatalf("provider usage migration index=%d, want append after main's published head 741", index)
	}
	for index, expected := range map[int]string{
		743: clientKeyPolicyHistorySchemaSql,
		744: contractUsageGuardSchemaSql,
		745: providerUsageArchiveSchemaSql,
		746: netEscrowRevisionSchemaSql,
		748: clientRegistrationSchemaSql,
	} {
		migration, ok := migrations[index].(*SqlMigration)
		if !ok || migration.sql != expected {
			t.Fatalf("mainnet custody migration %d changed identity or order", index)
		}
	}
}

// Preserve the exact branch SQL identities and order after moving its series
// past main's published prefix. This projection does not authorize a database
// with the old branch indices: the normal catalog verifier still rejects it.
func TestMainnetUsageTimeMigrationPreservesPublishedCatalog(t *testing.T) {
	const publishedHead = 727
	const publishedPrefix = "1a0031f08dff4648fc4526aa87ebc658d2b4a1b4f89089e137f9526c0ea77eb9"
	digest := sha256.New()
	for index := range publishedHead {
		mergedIndex := index
		if index >= 721 {
			mergedIndex += 20
		}
		identity, err := migrationIdentity(migrations[mergedIndex])
		if err != nil {
			t.Fatal(err)
		}
		writeMigrationIdentityPart(digest, strconv.Itoa(index))
		writeMigrationIdentityPart(digest, identity)
	}
	if prefix := hex.EncodeToString(digest.Sum(nil)); prefix != publishedPrefix {
		t.Fatalf("retained custody SQL changed during relocation: identity=%s", prefix)
	}
	index := migrationIndex(t, "transfer_contract_usage_missing_time")
	if index != publishedHead+20 {
		t.Fatalf("missing-time migration index=%d, want %d", index, publishedHead+20)
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
		migration := migrations[747].(*OnlineSqlMigration)
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
