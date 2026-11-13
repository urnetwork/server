// Regressions for maintenance on partitioned contract tables
// (CONTRACT-PARTITION.md). After the cutover each priority index is a
// partitioned index and each contract table a partitioned parent, so the
// measurement that required an ordinary table skipped every priority index,
// and the rotation's exact-name skip list let the parents and every partition
// into REINDEX TABLE CONCURRENTLY. The fixtures build the shape the cutover
// leaves: a legacy partition with the renamed _l leaf indexes and a weekly
// partition with the leaves PostgreSQL names on attach.
package server

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The cutover week of the synthetic partitioned fixtures.
var testDbMaintenanceCutoverWeek = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

// The bounds clause of the cutover week's partition and of the legacy
// partition before it.
func testDbMaintenanceWeekBounds(weekStart time.Time) string {
	return fmt.Sprintf(
		"FOR VALUES FROM (%s) TO (%s)",
		contractIdLiteral(ContractIdFloor(weekStart)),
		contractIdLiteral(ContractIdFloor(weekStart.Add(contractPartitionWeek))),
	)
}

func testDbMaintenanceLegacyBounds(cutoverWeek time.Time) string {
	return fmt.Sprintf("FOR VALUES FROM (MINVALUE) TO (%s)", contractIdLiteral(ContractIdFloor(cutoverWeek)))
}

// The leaf of parentIndex attached for partition, by name.
func testDbMaintenanceAttachedLeafIndex(t testing.TB, ctx context.Context, parentIndex string, partition string) (leafName string) {
	Db(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT leaf_class.relname
			FROM pg_inherits leaf
			INNER JOIN pg_class leaf_class ON leaf_class.oid = leaf.inhrelid
			INNER JOIN pg_index leaf_index ON leaf_index.indexrelid = leaf.inhrelid
			WHERE leaf.inhparent = to_regclass($1) AND leaf_index.indrelid = to_regclass($2)
			`,
			pgx.Identifier{"public", parentIndex}.Sanitize(),
			pgx.Identifier{"public", partition}.Sanitize(),
		)
		WithPgResult(result, err, func() {
			if result.Next() {
				Raise(result.Scan(&leafName))
			}
		})
	})
	if leafName == "" {
		t.Fatalf("no leaf of %s is attached for %s", parentIndex, partition)
	}
	return
}

// Whether the index exists.
func testDbMaintenanceIndexExists(ctx context.Context, indexName string) (exists bool) {
	Db(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, pgx.Identifier{"public", indexName}.Sanitize()).Scan(&exists))
	})
	return
}

// Whether the index is valid; false when it does not exist.
func testDbMaintenanceIndexValid(ctx context.Context, indexName string) (valid bool) {
	Db(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(
			ctx,
			`SELECT coalesce((SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass($1)), false)`,
			pgx.Identifier{"public", indexName}.Sanitize(),
		).Scan(&valid))
	})
	return
}

// A transfer_escrow partitioned the way the cutover leaves it, carrying the
// production unsettled partial index: its legacy leaf keeps the renamed _l
// index, and the cutover week's leaf gets the one PostgreSQL builds on
// attach. The legacy escrows churn the way contracts settle, all but one in
// fifty leaving the predicate, so the legacy leaf is bloated; the weekly
// escrows stay unsettled, so its leaf is compact.
func testDbMaintenanceCreatePartitionedEscrowFixture(ctx context.Context, cutoverWeek time.Time) {
	Db(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, fmt.Sprintf(
			`
			CREATE TABLE transfer_escrow (
				contract_id uuid NOT NULL,
				balance_id uuid NOT NULL,
				balance_byte_count bigint NOT NULL,
				settled boolean NOT NULL DEFAULT false,
				PRIMARY KEY (contract_id, balance_id)
			) PARTITION BY RANGE (contract_id);
			CREATE TABLE transfer_escrow_legacy PARTITION OF transfer_escrow %s WITH (autovacuum_enabled = false);
			CREATE TABLE %s PARTITION OF transfer_escrow %s WITH (autovacuum_enabled = false);
			CREATE INDEX transfer_escrow_unsettled_balance_contract_l
				ON transfer_escrow_legacy (balance_id, contract_id)
				INCLUDE (balance_byte_count)
				WHERE settled = false;
			CREATE INDEX transfer_escrow_unsettled_balance_contract
				ON transfer_escrow (balance_id, contract_id)
				INCLUDE (balance_byte_count)
				WHERE settled = false
			`,
			testDbMaintenanceLegacyBounds(cutoverWeek),
			quoteIdentifier(ContractPartitionName("transfer_escrow", cutoverWeek)),
			testDbMaintenanceWeekBounds(cutoverWeek),
		)))
		// legacy contract ids lead with millisecond zero; sixteen balances
		RaisePgResult(conn.Exec(ctx, `
			INSERT INTO transfer_escrow (contract_id, balance_id, balance_byte_count)
			SELECT
				lpad(to_hex(n), 32, '0')::uuid,
				('b' || lpad(to_hex(n % 16), 31, '0'))::uuid,
				n
			FROM generate_series(1, $1::bigint) n
		`, testDbMaintenanceChurnRowCount))
		RaisePgResult(conn.Exec(ctx, `UPDATE transfer_escrow SET settled = true WHERE balance_byte_count % 50 <> 0`))
		// cutover week contract ids lead with the week's first millisecond
		RaisePgResult(conn.Exec(ctx, `
			INSERT INTO transfer_escrow (contract_id, balance_id, balance_byte_count)
			SELECT
				(lpad(to_hex($1::bigint), 12, '0') || lpad(to_hex(n), 20, '0'))::uuid,
				('b' || lpad(to_hex(n % 16), 31, '0'))::uuid,
				n
			FROM generate_series(1, 1000) n
		`, cutoverWeek.UnixMilli()))
	})
	testDbMaintenanceVacuumAnalyze(ctx, "transfer_escrow")
}

// A partitioned priority index is measured and rebuilt one leaf at a time:
// the bloated legacy leaf alone is rebuilt with REINDEX INDEX CONCURRENTLY and
// stays attached under the published index, while the compact weekly leaf,
// the partitioned index and the tables are left alone. Before the fix the
// measurement required an ordinary table, so no priority index of a
// partitioned table was ever measured or rebuilt.
func TestDbMaintenanceRebuildsBloatedLeafOfPartitionedPriorityIndex(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const indexName = "transfer_escrow_unsettled_balance_contract"
		legacyLeafName := indexName + contractLegacyIndexSuffix
		weekly := ContractPartitionName("transfer_escrow", testDbMaintenanceCutoverWeek)
		testDbMaintenanceCreatePartitionedEscrowFixture(ctx, testDbMaintenanceCutoverWeek)
		if attached := testDbMaintenanceAttachedLeafIndex(t, ctx, indexName, "transfer_escrow_legacy"); attached != legacyLeafName {
			t.Fatalf("legacy leaf = %s, want %s", attached, legacyLeafName)
		}
		weeklyLeafName := testDbMaintenanceAttachedLeafIndex(t, ctx, indexName, weekly)

		measured, err := dbMaintenancePriorityIndexSizes(ctx, []string{indexName})
		if err != nil || len(measured) != 2 {
			t.Fatalf("measured %+v (err %v), want the two leaves", measured, err)
		}
		leaves := map[string]dbMaintenancePriorityIndex{}
		for _, leaf := range measured {
			leaves[leaf.indexName] = leaf
		}
		legacyLeaf, weeklyLeaf := leaves[legacyLeafName], leaves[weeklyLeafName]
		if legacyLeaf.tableName != "transfer_escrow_legacy" || !legacyLeaf.bloated() {
			t.Fatalf("legacy leaf measurement %+v, want a bloated leaf on transfer_escrow_legacy", legacyLeaf)
		}
		if weeklyLeaf.tableName != weekly || weeklyLeaf.bloated() {
			t.Fatalf("weekly leaf measurement %+v, want a compact leaf on %s", weeklyLeaf, weekly)
		}
		// a leaf named on its own measures that leaf alone
		if alone, err := dbMaintenancePriorityIndexSizes(ctx, []string{legacyLeafName}); err != nil || len(alone) != 1 || alone[0].indexName != legacyLeafName {
			t.Fatalf("leaf measured alone = %+v (err %v)", alone, err)
		}

		parentOid := testDbMaintenanceIndexOid(ctx, indexName)
		legacyOid := testDbMaintenanceIndexOid(ctx, legacyLeafName)
		weeklyOid := testDbMaintenanceIndexOid(ctx, weeklyLeafName)
		legacyPkeyOid := testDbMaintenanceIndexOid(ctx, "transfer_escrow_legacy_pkey")
		beforeBytes := testDbMaintenanceRelationBytes(ctx, legacyLeafName)

		outcome := DbMaintenance(ctx, 0, &DbMaintenanceOptions{
			Reindex:         true,
			Cleanup:         true,
			PriorityReindex: true,
		})
		if outcome.Stopped != "" || !slices.Equal(outcome.DoneIndexes, []string{legacyLeafName}) {
			t.Fatalf("outcome = %+v, want finished with only %s rebuilt", *outcome, legacyLeafName)
		}
		if testDbMaintenanceIndexOid(ctx, legacyLeafName) == legacyOid {
			t.Fatalf("the bloated %s was not rebuilt", legacyLeafName)
		}
		if afterBytes := testDbMaintenanceRelationBytes(ctx, legacyLeafName); beforeBytes < 4*afterBytes {
			t.Fatalf("%s is %d bytes after its rebuild, was %d", legacyLeafName, afterBytes, beforeBytes)
		}
		if testDbMaintenanceIndexOid(ctx, weeklyLeafName) != weeklyOid {
			t.Fatalf("the compact %s was rebuilt", weeklyLeafName)
		}
		if testDbMaintenanceIndexOid(ctx, indexName) != parentOid || !testDbMaintenanceIndexValid(ctx, indexName) {
			t.Fatalf("the partitioned %s was replaced or left invalid", indexName)
		}
		if testDbMaintenanceIndexOid(ctx, "transfer_escrow_legacy_pkey") != legacyPkeyOid {
			t.Fatal("transfer_escrow_legacy was rebuilt as a whole")
		}
		// the rebuilt leaf took the old one's place under the published index
		if attached := testDbMaintenanceAttachedLeafIndex(t, ctx, indexName, "transfer_escrow_legacy"); attached != legacyLeafName {
			t.Fatalf("legacy leaf after the rebuild = %s, want %s", attached, legacyLeafName)
		}
		for _, table := range []string{"transfer_escrow_legacy", weekly} {
			if _, invalid := testDbMaintenanceIndexes(ctx, table); len(invalid) != 0 {
				t.Fatalf("%s has invalid indexes after the rebuild: %v", table, invalid)
			}
		}
	})
}

// Over a whole cycle a partitioned contract table, its partitions and a
// prepared cutover's staged parent get cleanup and are recorded done, but are
// never rebuilt as a whole, while a contract table that is still plain is
// rebuilt in its epoch as before. Before the fix REINDEX TABLE CONCURRENTLY
// ran on the partitioned contract_extender, which rebuilds every partition,
// and on each partition by name.
func TestDbMaintenanceRotationLeavesPartitionedContractTablesToCleanup(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		week := testDbMaintenanceCutoverWeek
		extenderWeekly := ContractPartitionName("contract_extender", week)
		stagedWeekly := ContractPartitionName("transfer_escrow", week.Add(contractPartitionWeek))
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, fmt.Sprintf(
				`
				CREATE TABLE contract_extender (
					contract_id uuid NOT NULL,
					extender_id uuid NOT NULL,
					create_time timestamp NOT NULL DEFAULT now(),
					PRIMARY KEY (contract_id, extender_id)
				) PARTITION BY RANGE (contract_id);
				CREATE TABLE contract_extender_legacy PARTITION OF contract_extender %s;
				CREATE TABLE %s PARTITION OF contract_extender %s;
				CREATE INDEX contract_extender_create_time_contract_id ON contract_extender (create_time, contract_id);
				CREATE TABLE contract_close_report (
					contract_id uuid NOT NULL,
					report_id uuid NOT NULL,
					PRIMARY KEY (contract_id, report_id)
				);
				CREATE TABLE transfer_escrow_p (
					contract_id uuid NOT NULL,
					balance_id uuid NOT NULL,
					PRIMARY KEY (contract_id, balance_id)
				) PARTITION BY RANGE (contract_id);
				CREATE TABLE %s PARTITION OF transfer_escrow_p %s
				`,
				testDbMaintenanceLegacyBounds(week),
				quoteIdentifier(extenderWeekly),
				testDbMaintenanceWeekBounds(week),
				quoteIdentifier(stagedWeekly),
				testDbMaintenanceWeekBounds(week.Add(contractPartitionWeek)),
			)))
		})
		partitionedNames := []string{"contract_extender", "contract_extender_legacy", extenderWeekly, "transfer_escrow_p", stagedWeekly}
		leafIndexOids := map[string]uint32{}
		for _, table := range []string{"contract_extender_legacy", extenderWeekly, stagedWeekly} {
			valid, _ := testDbMaintenanceIndexes(ctx, table)
			for _, indexName := range valid {
				leafIndexOids[indexName] = testDbMaintenanceIndexOid(ctx, indexName)
			}
		}
		if len(leafIndexOids) != 5 {
			t.Fatalf("fixture leaf indexes = %v", leafIndexOids)
		}
		plainPkeyOid := testDbMaintenanceIndexOid(ctx, "contract_close_report_pkey")

		doneTables := []string{}
		for epoch := uint64(0); epoch < DbReindexEpochs; epoch += 1 {
			outcome := DbMaintenance(ctx, epoch, &DbMaintenanceOptions{
				Reindex: true,
				Cleanup: true,
			})
			if outcome.Stopped != "" {
				t.Fatalf("epoch %d outcome = %+v, want finished", epoch, *outcome)
			}
			doneTables = append(doneTables, outcome.DoneTables...)
		}
		for _, name := range append(partitionedNames, "contract_close_report") {
			if !slices.Contains(doneTables, name) {
				t.Errorf("%s was not cleaned in its epoch (done %v)", name, doneTables)
			}
		}
		for indexName, oid := range leafIndexOids {
			if testDbMaintenanceIndexOid(ctx, indexName) != oid {
				t.Errorf("leaf index %s was rebuilt", indexName)
			}
		}
		if testDbMaintenanceIndexOid(ctx, "contract_close_report_pkey") == plainPkeyOid {
			t.Error("the plain contract_close_report was not rebuilt in its epoch")
		}
	})
}

// Cleanup never takes a partitioned index for debris. One is invalid while an
// index migration builds it a partition at a time (here its parent shell with
// only the legacy leaf attached), REINDEX CONCURRENTLY never copies one, and
// DROP INDEX CONCURRENTLY cannot drop one: before the fix a suffixed shell
// identical to a valid partitioned index was listed, its drop failed, and the
// table's cleanup never finished, which also held back its rebuilds. A
// genuine copy of a leaf index is still dropped.
func TestDbMaintenanceCleanupKeepsPartitionedIndexes(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		week := testDbMaintenanceCutoverWeek
		weekly := ContractPartitionName("contract_extender", week)
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, fmt.Sprintf(
				`
				CREATE TABLE contract_extender (
					contract_id uuid NOT NULL,
					extender_id uuid NOT NULL,
					create_time timestamp NOT NULL DEFAULT now(),
					PRIMARY KEY (contract_id, extender_id)
				) PARTITION BY RANGE (contract_id);
				CREATE TABLE contract_extender_legacy PARTITION OF contract_extender %s;
				CREATE TABLE %s PARTITION OF contract_extender %s;
				CREATE INDEX contract_extender_create_time_contract_id_l ON contract_extender_legacy (create_time, contract_id);
				CREATE INDEX contract_extender_create_time_contract_id ON contract_extender (create_time, contract_id);
				INSERT INTO contract_extender (contract_id, extender_id)
				SELECT lpad(to_hex(n), 32, '0')::uuid, lpad(to_hex(n), 32, '0')::uuid FROM generate_series(1, 100) n;
				-- an index migration halfway: the shell and the legacy leaf
				CREATE INDEX contract_extender_extender_id ON ONLY contract_extender (extender_id);
				CREATE INDEX contract_extender_extender_id_l ON contract_extender_legacy (extender_id);
				ALTER INDEX contract_extender_extender_id ATTACH PARTITION contract_extender_extender_id_l;
				-- a shell named like a copy of the valid partitioned index
				CREATE INDEX contract_extender_create_time_contract_id_ccnew ON ONLY contract_extender (create_time, contract_id)
				`,
				testDbMaintenanceLegacyBounds(week),
				quoteIdentifier(weekly),
				testDbMaintenanceWeekBounds(week),
			)))
		})
		// a genuine copy of the legacy leaf
		testDbMaintenanceInterruptIndexBuild(t, ctx, "contract_extender_legacy", `REINDEX INDEX CONCURRENTLY contract_extender_create_time_contract_id_l`)
		const leafCopy = "contract_extender_create_time_contract_id_l_ccnew"
		if _, invalid := testDbMaintenanceIndexes(ctx, "contract_extender_legacy"); !slices.Equal(invalid, []string{leafCopy}) {
			t.Fatalf("fixture leaf copies = %v, want %s", invalid, leafCopy)
		}
		for _, indexName := range []string{"contract_extender_extender_id", "contract_extender_create_time_contract_id_ccnew"} {
			if !testDbMaintenanceIndexExists(ctx, indexName) || testDbMaintenanceIndexValid(ctx, indexName) {
				t.Fatalf("fixture %s is not an invalid partitioned index", indexName)
			}
		}

		var debris []dbMaintenanceReindexDebris
		var kept []string
		MaintenanceDb(ctx, func(conn PgConn) {
			var err error
			debris, kept, err = listDbMaintenanceReindexDebris(ctx, conn, "contract_extender", 0)
			Raise(err)
		})
		if len(debris) != 1 || debris[0].indexName != leafCopy || debris[0].tableName != "contract_extender_legacy" || len(kept) != 0 {
			t.Fatalf("debris = %+v kept = %v, want only the leaf copy %s", debris, kept, leafCopy)
		}
		remaining, err := dbMaintenanceDropReindexDebris(ctx, "contract_extender", time.Minute)
		if err != nil || remaining != 0 {
			t.Fatalf("cleanup left %d (err %v), want none", remaining, err)
		}
		if testDbMaintenanceIndexExists(ctx, leafCopy) {
			t.Fatalf("the leaf copy %s survived cleanup", leafCopy)
		}
		for _, indexName := range []string{"contract_extender_extender_id", "contract_extender_extender_id_l", "contract_extender_create_time_contract_id_ccnew"} {
			if !testDbMaintenanceIndexExists(ctx, indexName) {
				t.Fatalf("cleanup dropped %s", indexName)
			}
		}
		if !testDbMaintenanceIndexValid(ctx, "contract_extender_create_time_contract_id") {
			t.Fatal("the valid partitioned index was lost")
		}
	})
}
