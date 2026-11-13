package server

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// The operator drops a large index by hand, with DROP INDEX CONCURRENTLY on a
// direct session, before the migration that records the drop runs. Until then
// the database's version still expects the index, so the audit must not report
// it missing, and its fix must not rebuild it with a plain CREATE INDEX, which
// would block the table's writes for the whole build. An index dropped without
// a pending migration that drops it is still missing.
func TestAuditSchemaAcceptsIndexDroppedAheadOfItsMigration(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		// created at v354, and dropped by a plain migration before v377 rebuilt it
		const pendingDrop = "transfer_contract_open_payer_network_id_transfer_byte_count"
		// created at v56 and never dropped
		const noPendingDrop = "transfer_contract_open_source_id"
		dropVersion := auditTestIndexDropVersion(t, pendingDrop)
		ApplyDbMigrationsUpTo(ctx, dropVersion-1)

		missingBefore := []string{}
		MaintenanceDb(ctx, func(conn PgConn) {
			for _, name := range []string{pendingDrop, noPendingDrop} {
				var exists bool
				Raise(conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists))
				if !exists {
					missingBefore = append(missingBefore, name)
				}
			}
		}, OptReadOnly())
		if len(missingBefore) != 0 {
			t.Fatalf("%v do not exist at version %d", missingBefore, dropVersion-1)
		}
		// the operator's statement, outside a transaction
		MaintenanceDb(ctx, func(conn PgConn) {
			for _, name := range []string{pendingDrop, noPendingDrop} {
				RaisePgResult(conn.Exec(ctx, `DROP INDEX CONCURRENTLY `+name))
			}
		}, OptReadWrite(), OptNoRetry())

		result := AuditSchema(ctx)
		report := result.Diff.Report()
		if contains(result.Diff.missingIdx, pendingDrop) {
			t.Fatalf("the audit reports %s missing ahead of its drop at %d:\n%s", pendingDrop, dropVersion, report)
		}
		rebuildsControl := false
		for _, statement := range result.Diff.FixStatements() {
			if strings.Contains(statement, pendingDrop) {
				t.Fatalf("the audit's fix would rebuild %s: %s", pendingDrop, statement)
			}
			if strings.Contains(statement, noPendingDrop) {
				rebuildsControl = true
			}
		}
		if !strings.Contains(report, pendingDrop) {
			t.Fatalf("the report does not name %s:\n%s", pendingDrop, report)
		}
		if !contains(result.Diff.missingIdx, noPendingDrop) || !rebuildsControl {
			t.Fatalf("the audit accepted %s, which no pending migration drops:\n%s", noPendingDrop, report)
		}
	})
}

// The version of the first migration whose whole statement drops the named
// index.
func auditTestIndexDropVersion(t testing.TB, name string) int {
	pattern := regexp.MustCompile(`(?i)^\s*DROP\s+INDEX\s+(?:IF\s+EXISTS\s+)?` + name + `\s*;?\s*$`)
	for i, migration := range migrations {
		if m, ok := migration.(*SqlMigration); ok && pattern.MatchString(m.sql) {
			return i + 1
		}
	}
	t.Fatalf("no migration drops %s", name)
	return 0
}
