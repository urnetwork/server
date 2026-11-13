// The usage archive's catalog contracts in both layouts: plain until its
// cutover migration, partitioned by contract week from it on.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func migrationUsageArchivePartitionArtifactName() string {
	return fmt.Sprintf("st_provider_usage_archive weekly contract-week partitions@v%d", server.ProviderUsageArchivePartitionMigrationVersion())
}

const migrationUsageArchiveCustodyArtifactName = "st_provider_usage_archive exact atomic copy and append-only custody@v746"

// The cutover's contract sits at the server's version of the cutover, in its
// positional column, and the v746 custody contract reads the partitioned
// layout: the archive's guards on every partition and its close-time index in
// either form.
func TestMigrationsUsageArchivePartitionContract(t *testing.T) {
	version := server.ProviderUsageArchivePartitionMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 || contract.query != providerUsageArchivePartitionArtifactQuery {
		t.Fatalf("the cutover's contract is %+v", contract.artifact)
	}
	for _, predicate := range []string{
		migrationContractLeafTriggers("st_provider_usage_archive", "st_provider_usage_archive_guard", "st_provider_usage_archive_truncate_guard"),
		"'CREATE INDEX st_provider_usage_archive_close_time ON public.st_provider_usage_archive USING btree (close_time, contract_id)'",
		"'CREATE INDEX st_provider_usage_archive_close_time ON ONLY public.st_provider_usage_archive USING btree (close_time, contract_id)'",
	} {
		if !strings.Contains(providerUsageArchiveArtifactQuery, predicate) {
			t.Fatalf("v746 custody does not read the partitioned archive: missing %s", predicate)
		}
	}
}

// The coherence query emits the suffix contracts in list order, one column per
// version, so the list runs 794, 795, ... without a gap: a contract whose
// version comes from the server's list (the cutover's) still lands in its own
// column after a merge appends other migrations.
func TestMigrationsCurrentSuffixContractsFollowTheMigrationList(t *testing.T) {
	for i, contract := range migrationCurrentSuffixContracts {
		version := 794 + i
		if contract.artifact.requiredVersion != version || contract.artifact.rowColumn != version-589 {
			t.Fatalf("suffix contract %d is %+v, want version %d in column %d", i, contract.artifact, version, version-589)
		}
	}
	last := migrationCurrentSuffixContracts[len(migrationCurrentSuffixContracts)-1]
	if last.artifact.requiredVersion != server.MigrationCount() {
		t.Fatalf("the suffix ends at version %d, the migration list at %d", last.artifact.requiredVersion, server.MigrationCount())
	}
}

// Before the cutover's version a plain archive is only migration lag; from it
// on, an archive that is not partitioned is schema drift.
func TestMigrationsSignalUsageArchivePartitionLifetime(t *testing.T) {
	version := server.ProviderUsageArchivePartitionMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	for _, head := range []int{version - 1, version} {
		row := syntheticMigrationArtifactRow(head)
		row[contract.artifact.rowColumn] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(head), nil
			}
			if !strings.Contains(query, providerUsageArchivePartitionArtifactQuery) {
				t.Fatal("migration admission omitted the archive's partitions")
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if head < version {
			requireAlertClass(t, alerts, "migration-behind")
			if requireAlertClassCount(alerts, "migration-schema-drift") != 0 {
				t.Fatalf("a plain archive before the cutover reported drift: %+v", alerts)
			}
		} else {
			alert := requireAlertClass(t, alerts, "migration-schema-drift")
			if !strings.Contains(alert.Markdown(), migrationUsageArchivePartitionArtifactName()) {
				t.Fatalf("an unpartitioned archive at the cutover's version is not identified: %+v", alerts)
			}
		}
	}
}

// On a database migrated to the head the archive is partitioned and every
// published artifact reads present. Each fault of the partitioned layout
// reaches its owning artifact: a partition without its TRUNCATE guard or with
// its row guard disabled (v746), and the DEFAULT partition or a week detached
// by hand (the cutover's contract).
func TestMigrationsUsageArchivePartitionedCatalog(t *testing.T) {
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		row, drift := migrationPingDatabaseCheck(t, ctx)
		if drift != "" {
			t.Fatalf("the partitioned archive reports drift at the head: %s", drift)
		}
		contract := migrationCurrentSuffixTestContract(t, server.ProviderUsageArchivePartitionMigrationVersion())
		if !migrationBool(row[contract.artifact.rowColumn]) || !migrationBool(row[157]) {
			t.Fatal("the archive's partition or custody contract reads absent at the head")
		}

		week := server.ContractPartitionWeekStart(server.NowUtc())
		leaf := server.ContractPartitionName("st_provider_usage_archive", week)
		for _, fault := range []struct {
			name     string
			sql      string
			artifact string
		}{
			{
				name:     "partition TRUNCATE guard dropped",
				sql:      fmt.Sprintf(`DROP TRIGGER st_provider_usage_archive_truncate_guard ON %s`, leaf),
				artifact: migrationUsageArchiveCustodyArtifactName,
			},
			{
				name:     "DEFAULT partition TRUNCATE guard disabled",
				sql:      `ALTER TABLE st_provider_usage_archive_default DISABLE TRIGGER st_provider_usage_archive_truncate_guard`,
				artifact: migrationUsageArchiveCustodyArtifactName,
			},
			{
				name:     "partition row guard disabled",
				sql:      fmt.Sprintf(`ALTER TABLE %s DISABLE TRIGGER st_provider_usage_archive_guard`, leaf),
				artifact: migrationUsageArchiveCustodyArtifactName,
			},
			{
				name:     "parent close-time index dropped",
				sql:      `DROP INDEX st_provider_usage_archive_close_time`,
				artifact: migrationUsageArchiveCustodyArtifactName,
			},
			{
				name:     "DEFAULT partition detached",
				sql:      `ALTER TABLE st_provider_usage_archive DETACH PARTITION st_provider_usage_archive_default`,
				artifact: migrationUsageArchivePartitionArtifactName(),
			},
			{
				name:     "week detached by hand",
				sql:      fmt.Sprintf(`ALTER TABLE st_provider_usage_archive DETACH PARTITION %s`, leaf),
				artifact: migrationUsageArchivePartitionArtifactName(),
			},
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				drift, err := migrationContractTxDrift(ctx, tx)
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				if !strings.Contains(drift, fault.artifact) {
					t.Fatalf("%s did not reach %s: %s", fault.name, fault.artifact, drift)
				}
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
			t.Fatalf("rolled-back archive faults left drift: %s", drift)
		}
	})
}
