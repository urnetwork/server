// Tests of the rotation's rebuild bound and skip list
// (PARTITION-MAINTENANCE-PLAN.md A3): the bound counts what a rebuild reads and
// writes, the heap and toast plus each index's compact estimate, so a table
// whose indexes are bloated stays eligible; network_client_connection is back
// in the rotation.
package server

import (
	"slices"
	"testing"
)

// The rotation's bound counts an index at its compact estimate, and one
// without an estimate at its size on disk.
func TestDbMaintenanceRebuildBytesCountsCompactIndexes(t *testing.T) {
	const gb = int64(1000 * 1000 * 1000)
	// pending_task on 2026-10-21: about 200 MB of rows under 94 GB of indexes
	indexes := []dbMaintenancePriorityIndex{
		{indexName: "synthetic_poll_index", indexBytes: 94 * gb, compactBytes: gb / 10},
		{indexName: "synthetic_gin_index", indexBytes: gb / 2},
	}
	if bytes := dbMaintenanceRebuildBytes(gb/5, indexes); bytes != gb/5+gb/10+gb/2 {
		t.Fatalf("rebuild bytes = %d", bytes)
	}
	if DefaultDbMaintenanceMaxReindexTableBytes < dbMaintenanceRebuildBytes(gb/5, indexes) {
		t.Fatal("a table whose indexes are bloated would leave the rotation")
	}
}

// network_client_connection is back in the rotation; the partitioned families
// and the tables Parts B and D still have to shrink stay out.
func TestDbMaintenanceSkipListAdmitsTheConnectionTable(t *testing.T) {
	if !dbMaintenanceShouldReindexTable("network_client_connection") || !dbMaintenanceShouldReindexRelation("network_client_connection", false) {
		t.Fatal("network_client_connection is still excluded from the rotation")
	}
	for _, tableName := range []string{
		"client_reliability",
		"client_reliability_p20261024",
		"contract_participant",
		"contract_close",
		"transfer_contract",
		"transfer_escrow",
		"transfer_escrow_sweep",
		"network_client",
		"network_client_location_reliability",
	} {
		if dbMaintenanceShouldReindexTable(tableName) {
			t.Errorf("%s joined the rotation", tableName)
		}
	}
}

// The rotation's bound measures what a rebuild reads and writes, not the
// index bytes it throws away: a table whose heap vacuum emptied but whose
// indexes kept every page stays eligible and is rebuilt, while a table whose
// heap alone passes the bound is left to cleanup. Uses only DbMaintenance,
// its options and SQL, so it runs unchanged on the code before the fix, which
// measured the indexes on disk and left the bloated table to cleanup.
func TestDbMaintenanceBoundKeepsABloatedTableEligible(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_bound_bloated (id bigint PRIMARY KEY, value bigint) WITH (autovacuum_enabled = false, vacuum_truncate = true);
				CREATE INDEX synthetic_bound_bloated_value ON synthetic_bound_bloated (value);
				INSERT INTO synthetic_bound_bloated SELECT n, n FROM generate_series(1, 100000) n;
				DELETE FROM synthetic_bound_bloated WHERE 300 < id;
				CREATE TABLE synthetic_bound_large (id bigint PRIMARY KEY, value bigint) WITH (autovacuum_enabled = false);
				INSERT INTO synthetic_bound_large SELECT n, n FROM generate_series(1, 100000) n
			`))
		})
		// vacuum truncates the emptied heap; btree files never shrink
		testDbMaintenanceVacuumAnalyze(ctx, "synthetic_bound_bloated")
		testDbMaintenanceVacuumAnalyze(ctx, "synthetic_bound_large")
		// above the bloated table's heap and compact indexes, below its
		// indexes on disk and the large table's heap
		const maxBytes = 1024 * 1024
		if indexBytes := testDbMaintenanceRelationBytes(ctx, "synthetic_bound_bloated_pkey"); indexBytes <= maxBytes {
			t.Fatalf("fixture index is %d bytes", indexBytes)
		}

		bloatedBefore := testDbMaintenanceIndexOid(ctx, "synthetic_bound_bloated_pkey")
		outcome := DbMaintenance(ctx, testDbMaintenanceEpoch("synthetic_bound_bloated"), &DbMaintenanceOptions{
			Reindex:              true,
			Cleanup:              true,
			MaxReindexTableBytes: maxBytes,
		})
		if outcome.Stopped != "" || !slices.Contains(outcome.DoneTables, "synthetic_bound_bloated") {
			t.Fatalf("outcome = %+v", *outcome)
		}
		if testDbMaintenanceIndexOid(ctx, "synthetic_bound_bloated_pkey") == bloatedBefore {
			t.Fatal("the table whose indexes are bloated was left to cleanup")
		}

		largeBefore := testDbMaintenanceIndexOid(ctx, "synthetic_bound_large_pkey")
		outcome = DbMaintenance(ctx, testDbMaintenanceEpoch("synthetic_bound_large"), &DbMaintenanceOptions{
			Reindex:              true,
			Cleanup:              true,
			MaxReindexTableBytes: maxBytes,
		})
		if outcome.Stopped != "" || !slices.Contains(outcome.DoneTables, "synthetic_bound_large") {
			t.Fatalf("outcome = %+v", *outcome)
		}
		if testDbMaintenanceIndexOid(ctx, "synthetic_bound_large_pkey") != largeBefore {
			t.Fatal("a table whose heap passes the bound was rebuilt")
		}
	})
}
