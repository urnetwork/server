package server

import (
	"context"
	"testing"
)

func testMigrationCatalogEntries(t testing.TB, count int) []migrationCatalogEntry {
	t.Helper()
	entries := make([]migrationCatalogEntry, count)
	for index := 0; index < count; index += 1 {
		identity, err := migrationIdentity(migrations[index])
		if err != nil {
			t.Fatal(err)
		}
		entries[index] = migrationCatalogEntry{Index: index, Identity: identity}
	}
	return entries
}

func assertAppendedMigrationArtifacts(t testing.TB, ctx context.Context) {
	t.Helper()
	if err := verifyAppendedMigrationArtifacts(ctx); err != nil {
		t.Fatal(err)
	}
	var catalogCount int
	var firstIndex int
	var lastIndex int
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `
			SELECT count(*), min(migration_index), max(migration_index)
			FROM migration_catalog
		`).Scan(&catalogCount, &firstIndex, &lastIndex))
	}, OptReadOnly(), OptNoRetry())
	if catalogCount != len(migrations) || firstIndex != 0 || lastIndex != len(migrations)-1 {
		t.Fatalf(
			"migration catalog count/range = %d/%d..%d, want %d/0..%d",
			catalogCount,
			firstIndex,
			lastIndex,
			len(migrations),
			len(migrations)-1,
		)
	}
	if version := DbVersion(ctx); version != len(migrations) {
		t.Fatalf("DB version = %d, want %d", version, len(migrations))
	}
}

func TestMigrationCatalogAcceptsExactHistory(t *testing.T) {
	entries := testMigrationCatalogEntries(t, len(migrations))
	if err := validateMigrationCatalog(len(migrations), entries); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationCatalogRejectsMissingHistory(t *testing.T) {
	entries := testMigrationCatalogEntries(t, len(migrations))
	entries = entries[:len(entries)-1]
	if err := validateMigrationCatalog(len(migrations), entries); err == nil {
		t.Fatal("migration catalog accepted missing durable history")
	}
}

func TestMigrationCatalogRejectsReorderedHistory(t *testing.T) {
	entries := testMigrationCatalogEntries(t, len(migrations))
	entries[0], entries[1] = entries[1], entries[0]
	if err := validateMigrationCatalog(len(migrations), entries); err == nil {
		t.Fatal("migration catalog accepted reordered durable history")
	}
}

func TestMigrationCatalogRejectsChangedMigration(t *testing.T) {
	entries := testMigrationCatalogEntries(t, len(migrations))
	entries[len(entries)-1].Identity = entries[0].Identity
	if err := validateMigrationCatalog(len(migrations), entries); err == nil {
		t.Fatal("migration catalog accepted changed migration SQL or identity")
	}
}

func TestPublishedMigrationHistoryUpgradesWithoutReplay(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		ApplyDbMigrationsUpTo(ctx, publishedMigrationPrefixCount)
		if version := DbVersion(ctx); version != publishedMigrationPrefixCount {
			t.Fatalf("published DB version = %d, want %d", version, publishedMigrationPrefixCount)
		}
		if err := verifyAppendedMigrationArtifacts(ctx); err == nil {
			t.Fatal("post-prefix migration artifacts existed before their migrations")
		}

		ApplyDbMigrations(ctx)
		assertAppendedMigrationArtifacts(t, ctx)

		// A normal restart must verify the durable catalog and remain a no-op.
		ApplyDbMigrations(ctx)
		assertAppendedMigrationArtifacts(t, ctx)
	})
}

func TestShiftedMigrationHistoryRepairsSkippedFirstAppend(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		ApplyDbMigrationsUpTo(ctx, publishedMigrationPrefixCount)

		// The faulty release replayed the old competition-workload migration at
		// numeric index 593 and recorded version 594 without creating the new
		// transfer_escrow index that belongs at the restored index 593.
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `
				INSERT INTO migration_audit (start_version_number, status)
				VALUES ($1, 'start')
			`, publishedMigrationPrefixCount))
			RaisePgResult(tx.Exec(ctx, `
				INSERT INTO migration_audit (
					start_version_number,
					end_version_number,
					status
				) VALUES ($1, $2, 'success')
			`, publishedMigrationPrefixCount, publishedMigrationPrefixCount+1))
		})
		if err := verifyAppendedMigrationArtifacts(ctx); err == nil {
			t.Fatal("shifted history unexpectedly contained all appended artifacts")
		}

		ApplyDbMigrations(ctx)
		assertAppendedMigrationArtifacts(t, ctx)
	})
}

func TestMigrationArtifactVerificationRejectsIdentityConfusion(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		ApplyDbMigrationsUpTo(ctx, publishedMigrationPrefixCount)
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `
				ALTER TABLE account_payment
					ADD COLUMN contract_retention_cursor uuid NULL,
					ADD COLUMN contract_retention_pending bool NOT NULL DEFAULT false;
				CREATE INDEX transfer_escrow_balance_contract
				ON transfer_escrow (contract_id, balance_id);
				CREATE INDEX account_payment_contract_retention_pending
				ON account_payment (payment_id, complete_time)
				WHERE contract_retention_pending;
				CREATE INDEX transfer_escrow_sweep_payment_contract
				ON transfer_escrow_sweep (contract_id, payment_id);
			`))
		})
		if err := verifyAppendedMigrationArtifacts(ctx); err == nil {
			t.Fatal("migration artifact verification accepted same-name indexes with confused identities")
		}
	})
}

func TestMigrationCatalogTableCommitBeforeAuditRecovers(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		ApplyDbMigrationsUpTo(ctx, migrationCatalogTableIndex)
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, migrationCatalogSchemaSQL))
		})
		if version := DbVersion(ctx); version != migrationCatalogTableIndex {
			t.Fatalf("DB version after catalog DDL-only commit = %d, want %d", version, migrationCatalogTableIndex)
		}

		ApplyDbMigrations(ctx)
		assertAppendedMigrationArtifacts(t, ctx)
	})
}

func TestMigrationCatalogBaselineCommitBeforeAuditRecovers(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		ApplyDbMigrationsUpTo(ctx, migrationCatalogBaselineIndex)
		migrationInstallCatalog(ctx)
		if version := DbVersion(ctx); version != migrationCatalogBaselineIndex {
			t.Fatalf("DB version after catalog population-only commit = %d, want %d", version, migrationCatalogBaselineIndex)
		}

		ApplyDbMigrations(ctx)
		assertAppendedMigrationArtifacts(t, ctx)
	})
}
