// The close evidence custody in both layouts (PARTITION-MAINTENANCE-PLAN.md
// C5): the plain table as migrated, and after its owner-run cutover a parent
// partitioned by report_id (server.Testing_PartitionCloseEvidence). The move
// guard's artifact reads present in both, and each fault of the partitioned
// layout reaches it.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The move guard's contract is the version the migration list gives its SQL,
// it supersedes 857, and it is the only contract at that version.
func TestMigrationsCloseEvidenceMoveGuardContract(t *testing.T) {
	version := server.ContractCloseEvidenceMoveGuardMigrationVersion()
	if version == 0 || version != contractCloseEvidenceMoveGuardMigrationVersion {
		t.Fatalf("move guard version %d, contract version %d", version, contractCloseEvidenceMoveGuardMigrationVersion)
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.query != contractCloseEvidenceMoveArtifactQuery || contract.artifact.removedVersion != 0 || contract.artifact.rowColumn != version-589 {
		t.Fatalf("move guard contract %+v", contract.artifact)
	}
	if retention := migrationCurrentSuffixTestContract(t, 857); retention.artifact.removedVersion != version {
		t.Fatalf("857 is removed at %d, want %d", retention.artifact.removedVersion, version)
	}
	for _, want := range []string{
		"relation.relkind='r' OR (relation.relkind='p'",
		server.ContractCloseEvidenceMoveGuardBodySql()[:40],
		"regexp_replace(pg_get_indexdef(i.indexrelid), '^(CREATE (UNIQUE )?INDEX \\S+ ON )ONLY ', '\\1')='CREATE INDEX contract_close_original_census ON public.contract_close_report_evidence USING btree (contract_id, client_id, report_id)'",
		"attached.inhparent=to_regclass('public.contract_close_report_evidence_pkey')",
	} {
		if !strings.Contains(contractCloseEvidenceMoveArtifactQuery, want) {
			t.Errorf("move guard artifact lacks %q", want)
		}
	}
}

// After the close evidence cutover every published artifact still reads
// present, including the move guard's custody read through the parent. Each
// real fault of the partitioned layout still reaches it: a TRUNCATE guard
// missing or disabled on one partition, a row guard disabled on one, a foreign
// key on the legacy partition, a unique index on DEFAULT that is not the key's
// copy, and a guard function that refuses moves again.
func TestMigrationsCloseEvidencePartitionedCatalog(t *testing.T) {
	migrationContractTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrations(ctx)
		week := server.PartitionGranularityWeekly.RangeStart(server.NowUtc())
		server.Testing_PartitionCloseEvidence(ctx, week)
		if !server.IsCloseEvidencePartitioned(ctx) {
			t.Fatal("the cutover left the close evidence plain")
		}
		row, drift := migrationPingDatabaseCheck(t, ctx)
		if drift != "" {
			t.Fatalf("partitioned close evidence reports drift at the head: %s", drift)
		}
		head := server.MigrationCount()
		for _, artifact := range migrationArtifacts {
			if artifact.removedVersion != 0 && artifact.removedVersion <= head {
				continue
			}
			if !migrationBool(row[artifact.rowColumn]) {
				t.Fatalf("%s@v%d reads absent on the partitioned close evidence", artifact.name, artifact.requiredVersion)
			}
		}

		family := server.GetPartitionFamily("close_evidence")
		weekLeaf := family.LeafName("contract_close_report_evidence", family.RangeAt(week))
		artifact := fmt.Sprintf("%s@v%d", migrationCurrentSuffixTestContract(t, contractCloseEvidenceMoveGuardMigrationVersion).artifact.name, contractCloseEvidenceMoveGuardMigrationVersion)
		for _, fault := range []struct {
			name string
			sql  string
		}{
			{name: "week TRUNCATE guard dropped", sql: fmt.Sprintf(`DROP TRIGGER contract_close_report_evidence_truncate_guard ON %s`, weekLeaf)},
			{name: "week TRUNCATE guard disabled", sql: fmt.Sprintf(`ALTER TABLE %s DISABLE TRIGGER contract_close_report_evidence_truncate_guard`, weekLeaf)},
			{name: "DEFAULT row guard disabled", sql: `ALTER TABLE contract_close_report_evidence_default DISABLE TRIGGER contract_close_report_evidence_guard`},
			{name: "legacy TRUNCATE guard dropped", sql: `DROP TRIGGER contract_close_report_evidence_truncate_guard ON contract_close_report_evidence_legacy`},
			{name: "legacy foreign key", sql: `ALTER TABLE contract_close_report_evidence_legacy ADD CONSTRAINT synthetic_close_contract_fk FOREIGN KEY (contract_id) REFERENCES transfer_contract(contract_id) NOT VALID`},
			{name: "DEFAULT unique index", sql: `CREATE UNIQUE INDEX synthetic_global_close_report ON contract_close_report_evidence_default (report_id)`},
			{name: "parent check dropped", sql: `ALTER TABLE contract_close_report_evidence DROP CONSTRAINT contract_close_inventory_requires_report`},
			{name: "guard refuses moves again", sql: `CREATE OR REPLACE FUNCTION contract_close_report_evidence_guard() RETURNS trigger
				LANGUAGE plpgsql AS 'BEGIN RAISE EXCEPTION ''original contract close reports are immutable''; END'`},
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				drift, err := migrationContractTxDrift(ctx, tx)
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				if !strings.Contains(drift, artifact) {
					t.Fatalf("%s did not reach %s: %s", fault.name, artifact, drift)
				}
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
			t.Fatalf("rolled-back partitioned faults left drift: %s", drift)
		}
	})
}
