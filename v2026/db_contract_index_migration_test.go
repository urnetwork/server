package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The leaf of parentIndex attached for partition, or "".
func testContractIndexAttachedLeaf(t testing.TB, ctx context.Context, parentIndex string, partition string) string {
	return testContractPartitionScalar[string](
		t,
		ctx,
		`
		SELECT coalesce((
			SELECT leaf_class.relname::text
			FROM pg_inherits leaf
			INNER JOIN pg_class leaf_class ON leaf_class.oid = leaf.inhrelid
			INNER JOIN pg_index leaf_index ON leaf_index.indexrelid = leaf.inhrelid
			WHERE leaf.inhparent = to_regclass($1) AND leaf_index.indrelid = to_regclass($2)
		), '')
		`,
		"public."+parentIndex,
		"public."+partition,
	)
}

// The leaf names: the legacy leaf takes the cutover's suffix, a weekly leaf
// the partition's name, and a name PostgreSQL would truncate ends in a hash
// instead.
func TestContractIndexMigrationLeafNames(t *testing.T) {
	week := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	migration := newContractIndexMigration("transfer_contract", "transfer_contract_close_day", "(close_time)").contractIndex
	connect.AssertEqual(t, migration.leafName(ContractLegacyPartitionName("transfer_contract")), "transfer_contract_close_day_l")
	connect.AssertEqual(t, migration.leafName(ContractPartitionName("transfer_contract", week)), "transfer_contract_w20261026_close_day")

	unprefixed := newContractIndexMigration("contract_extender", "extender_lookup", "(extender_id)").contractIndex
	connect.AssertEqual(t, unprefixed.leafName(ContractPartitionName("contract_extender", week)), "contract_extender_w20261026_extender_lookup")

	long := newContractIndexMigration("transfer_escrow_sweep", "transfer_escrow_sweep_network_id_payment_id_payout_byte_count", "(network_id)").contractIndex
	for _, partition := range []string{ContractLegacyPartitionName("transfer_escrow_sweep"), ContractPartitionName("transfer_escrow_sweep", week)} {
		name := long.leafName(partition)
		connect.AssertEqual(t, len(name) <= pgMaxIdentifierLength, true)
		connect.AssertEqual(t, name, long.leafName(partition))
	}
	connect.AssertEqual(t, long.leafName(ContractPartitionName("transfer_escrow_sweep", week)) != long.leafName(ContractPartitionName("transfer_escrow_sweep", week.Add(contractPartitionWeek))), true)

	// only the contract tables take this migration
	connect.AssertEqual(t, HandleError(func() {
		newContractIndexMigration("network_client", "network_client_synthetic", "(client_id)")
	}) != nil, true)
}

// A future index migration on a contract table, run in either layout. On the
// plain table it runs its published CREATE INDEX CONCURRENTLY IF NOT EXISTS
// after dropping an invalid leftover that IF NOT EXISTS would keep. After the
// cutover, where that statement is refused on the partitioned parent, it
// builds the shell ON ONLY the parent and a leaf per partition, concurrently,
// attaching each, so the published name ends as a valid partitioned index;
// an interrupted leaf build is rebuilt, a rerun changes nothing, and a later
// partition builds its own leaf on attach. Without URNETWORK_TEST_CONTRACT_PARTITIONS
// the test checks the plain table, then cuts over and checks the partitions.
func TestContractIndexMigrationBuildsInEitherLayout(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		const table = "contract_extender"
		const index = "contract_extender_synthetic_network_create_time"
		const shape = "(network_id, create_time) WHERE client_id IS NOT NULL"
		const definition = "CREATE INDEX contract_extender_synthetic_network_create_time ON public.contract_extender USING btree (network_id, create_time) WHERE (client_id IS NOT NULL)"
		migration := newContractIndexMigration(table, index, shape)
		connect.AssertEqual(t, migration.sql, "CREATE INDEX CONCURRENTLY IF NOT EXISTS "+index+" ON "+table+" "+shape)
		connect.AssertEqual(t, migration.auditSql, "CREATE INDEX IF NOT EXISTS "+index+" ON "+table+" "+shape)
		// a contract from before the current week, which is where the test
		// cutover puts its legacy bound
		testInsertContractRows(t, ctx, testContractIdAt(ContractPartitionWeekStart(NowUtc()).Add(-time.Hour)), true)

		if !IsContractPartitioned(ctx) {
			testDbMaintenanceInterruptIndexBuild(t, ctx, table, "CREATE INDEX CONCURRENTLY "+index+" ON "+table+" "+shape)
			relkind, valid, _ := testContractIndexState(t, ctx, index)
			connect.AssertEqual(t, relkind, "i")
			connect.AssertEqual(t, valid, false)

			connect.AssertEqual(t, testExecuteOnlineMigration(ctx, migration), nil)
			relkind, valid, actual := testContractIndexState(t, ctx, index)
			connect.AssertEqual(t, relkind, "i")
			connect.AssertEqual(t, valid, true)
			connect.AssertEqual(t, actual, definition)
			connect.AssertEqual(t, testExecuteOnlineMigration(ctx, migration), nil)

			MaintenanceDb(ctx, func(conn PgConn) {
				RaisePgResult(conn.Exec(ctx, "DROP INDEX "+index))
			})
			cutoverStart := time.Now()
			Testing_PartitionContracts(ctx)
			t.Logf("cut over in %s", time.Since(cutoverStart))
		}

		// the published statement is refused on the partitioned parent
		connect.AssertEqual(t, HandleError(func() {
			MaintenanceDb(ctx, func(conn PgConn) {
				RaisePgResult(conn.Exec(ctx, migration.sql))
			}, OptReadWrite(), OptNoRetry())
		}) != nil, true)

		// a run interrupted after the shell, during the legacy leaf's build
		legacy := ContractLegacyPartitionName(table)
		legacyLeaf := index + contractLegacyIndexSuffix
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, fmt.Sprintf("CREATE INDEX %s ON ONLY public.%s %s", index, table, shape)))
		})
		testDbMaintenanceInterruptIndexBuild(t, ctx, legacy, fmt.Sprintf("CREATE INDEX CONCURRENTLY %s ON public.%s %s", legacyLeaf, legacy, shape))
		_, valid, _ := testContractIndexState(t, ctx, legacyLeaf)
		connect.AssertEqual(t, valid, false)

		connect.AssertEqual(t, testExecuteOnlineMigration(ctx, migration), nil)
		relkind, valid, actual := testContractIndexState(t, ctx, index)
		connect.AssertEqual(t, relkind, "I")
		connect.AssertEqual(t, valid, true)
		connect.AssertEqual(t, normalizeIndexDef(actual), definition)
		partitions, err := listContractPartitions(ctx, table)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, 2 <= len(partitions), true)
		leaves := map[string]string{}
		for _, partition := range partitions {
			leaf := testContractIndexAttachedLeaf(t, ctx, index, partition.name)
			connect.AssertEqual(t, leaf, migration.contractIndex.leafName(partition.name))
			_, leafValid, _ := testContractIndexState(t, ctx, leaf)
			connect.AssertEqual(t, leafValid, true)
			leaves[partition.name] = leaf
		}
		connect.AssertEqual(t, leaves[legacy], legacyLeaf)

		// a rerun changes nothing
		before := testDbMaintenanceIndexOid(ctx, legacyLeaf)
		connect.AssertEqual(t, testExecuteOnlineMigration(ctx, migration), nil)
		connect.AssertEqual(t, testDbMaintenanceIndexOid(ctx, legacyLeaf), before)

		// a week created later builds its own leaf when it is attached
		lastWeek := partitions[len(partitions)-1].weekStart
		created, _, err := ensureContractWeekPartitions(ctx, lastWeek.Add(contractPartitionWeek))
		connect.AssertEqual(t, err, nil)
		next := ContractPartitionName(table, lastWeek.Add(contractPartitionWeek))
		found := false
		for _, name := range created {
			if name == next {
				found = true
			}
		}
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, testContractIndexAttachedLeaf(t, ctx, index, next) != "", true)
		_, valid, _ = testContractIndexState(t, ctx, index)
		connect.AssertEqual(t, valid, true)
	})
}
