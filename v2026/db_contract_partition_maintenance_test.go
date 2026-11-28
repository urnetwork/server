package server

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Inserts close evidence for contractId accepted at acceptedAt.
func testInsertCloseEvidence(t testing.TB, ctx context.Context, contractId Id, acceptedAt time.Time) (clientId Id, reportId Id) {
	clientId = NewId()
	reportId = NewId()
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO contract_close_report_evidence (
				client_id, report_id, contract_id, party,
				acked_byte_count, unacked_byte_count, checkpoint, accepted_at
			) VALUES ($1, $2, $3, 'source', 1, 0, false, $4)
			`,
			clientId, reportId, contractId, acceptedAt,
		))
	})
	return
}

// Deletes a contract with its closes, escrows, sweeps, extender parties and
// close reports through the parents, dependents first, in one transaction, as
// the contract horizon reaper does once its keep rules admit the contract. The
// capture trigger archives a credit-bearing contract's usage.
func testReapContract(t testing.TB, ctx context.Context, contractId Id) {
	t.Helper()
	Tx(ctx, func(tx PgTx) {
		for _, table := range []string{"contract_close_report", "contract_close", "transfer_escrow", "transfer_escrow_sweep", "contract_extender", "transfer_contract"} {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`DELETE FROM public.%s WHERE contract_id = $1`, quoteIdentifier(table)), contractId))
		}
	})
}

// Waits until a row deleted before the call is dead to every snapshot: vacuum
// removes it, and CLUSTER copies it no more (Testing_WaitForSnapshotHorizon).
// CLUSTER's horizon is the oldest transaction running anywhere on the server,
// and the shared local test server runs other packages' tests in other
// databases, which can hold a transaction open for seconds.
func testWaitForContractHorizon(t testing.TB, ctx context.Context) {
	t.Helper()
	if err := Testing_WaitForSnapshotHorizon(ctx, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
}

// The wait the S1 tests used before 2026-11-17: until no other backend of this
// database holds a snapshot or a transaction id from a transaction that began
// before the call. It returns while a transaction in another database, older
// than a delete, keeps the deleted row recently dead to CLUSTER
// (TestContractDayCompactionSeesTransactionsOfOtherDatabases); kept to show it.
func testWaitForThisDatabaseHorizon(t testing.TB, ctx context.Context, timeout time.Duration) (holders string) {
	t.Helper()
	since := testContractPartitionScalar[time.Time](t, ctx, `SELECT clock_timestamp()`)
	deadline := time.Now().Add(timeout)
	for {
		holders = testContractPartitionScalar[string](t, ctx, `
			SELECT coalesce(string_agg(pid::text || ' ' || coalesce(backend_type, ''), ', '), '')
			FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
				AND (backend_xmin IS NOT NULL OR backend_xid IS NOT NULL) AND xact_start <= $1`, since)
		if holders == "" || time.Now().After(deadline) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// VACUUM (ANALYZE) of each leaf of the day r, as autovacuum runs after the
// reaper's deletes: it removes the deleted rows from the heaps and keys, and
// sets the row estimates. TRUNCATE false, as on Main (vacuum_truncate off).
// It first waits until every snapshot sees those deletes.
func testVacuumContractDay(t testing.TB, ctx context.Context, r PartitionRange) {
	t.Helper()
	testWaitForContractHorizon(t, ctx)
	MaintenanceDb(ctx, func(conn PgConn) {
		for _, leaf := range contractPartitionFamily.LeafNames(r) {
			RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`VACUUM (ANALYZE, TRUNCATE false) public.%s`, quoteIdentifier(leaf))))
		}
	})
}

// Every row a contract holds in the six tables, as text.
func testContractRows(t testing.TB, ctx context.Context, contractId Id) (rows []string) {
	t.Helper()
	for _, table := range ContractPartitionTables {
		rows = append(rows, testContractPartitionScalar[string](
			t,
			ctx,
			fmt.Sprintf(`SELECT coalesce(string_agg(row_to_json(row)::text, ',' ORDER BY row_to_json(row)::text), '') FROM public.%s AS row WHERE contract_id = $1`, quoteIdentifier(table)),
			contractId,
		))
	}
	return
}

// Exercises a maintenance pass on partitioned contract tables 140 days after
// the cutover (S1). Retirement deletes no row: the reaper alone does. A day
// the reaper emptied is compacted and dropped in one pass, and a day that
// never held a row is dropped as it is; days whose rows the reaper keeps (a
// resolved contract in day 0, an unresolved one in day 2, and one with an
// unsettled escrow in the legacy partition) are reported kept, every row in
// place and none archived. The days ahead are created, closed days and the
// parents analyzed, and expired close evidence is deleted unless its contract
// is unresolved. Once the reaper deletes day 0's contract, day 0 drops too.
func TestContractPartitionMaintenance(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()

		// before the cutover day, so in the legacy partition
		legacyContractId := testContractIdAt(ContractPartitionDayStart(now).Add(-24 * time.Hour))
		testInsertContractRows(t, ctx, legacyContractId, true)
		// the legacy partition keeps an unsettled escrow
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET settled = false WHERE contract_id = $1`, legacyContractId))
		})

		Testing_PartitionContracts(ctx)
		cutoverDay := ContractPartitionDayStart(now)
		day := func(i int) time.Time {
			return cutoverDay.AddDate(0, 0, i)
		}
		// the cutover made days 0 to 3; day 4 is made as the runway makes it
		created, err := contractPartitionFamily.EnsureRangeFor(ctx, day(4))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(created), len(ContractPartitionTables))

		// day 0: a contract the reaper keeps
		keptContractId := testContractIdAt(day(0).Add(time.Hour))
		testInsertContractRows(t, ctx, keptContractId, true)
		// days 1 and 3: contracts the reaper deleted
		reapedContractIds := []Id{
			testContractIdAt(day(1).Add(time.Hour)),
			testContractIdAt(day(1).Add(2 * time.Hour)),
			testContractIdAt(day(3).Add(time.Hour)),
		}
		for _, contractId := range reapedContractIds {
			testInsertContractRows(t, ctx, contractId, true)
			testReapContract(t, ctx, contractId)
		}
		// day 2: an unresolved contract
		unresolvedContractId := testContractIdAt(day(2).Add(time.Hour))
		testInsertContractRows(t, ctx, unresolvedContractId, false)
		for _, i := range []int{0, 1, 2, 3} {
			testVacuumContractDay(t, ctx, contractPartitionFamily.RangeAt(day(i)))
		}

		// close evidence: expired and resolved, expired but unresolved, recent
		expiredContractId := testContractIdAt(now.Add(-100 * 24 * time.Hour))
		expiredClientId, expiredReportId := testInsertCloseEvidence(t, ctx, expiredContractId, now.Add(-100*24*time.Hour))
		openClientId, openReportId := testInsertCloseEvidence(t, ctx, unresolvedContractId, now.Add(-100*24*time.Hour))
		recentClientId, recentReportId := testInsertCloseEvidence(t, ctx, NewId(), now.Add(-30*24*time.Hour))
		evidenceExists := func(clientId Id, reportId Id) bool {
			return testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM contract_close_report_evidence WHERE client_id = $1 AND report_id = $2)`, clientId, reportId)
		}
		keptBefore := map[Id][]string{}
		for _, contractId := range []Id{legacyContractId, keptContractId, unresolvedContractId} {
			keptBefore[contractId] = testContractRows(t, ctx, contractId)
		}

		later := day(140).Add(time.Hour)
		report, cursor := MaintainContractPartitions(ctx, later, ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, report.Partitioned, true)
		// the days ahead exist
		connect.AssertEqual(t, report.FutureDays, ContractPartitionAheadDays)
		for i := 140; i <= 140+ContractPartitionAheadDays; i += 1 {
			for _, table := range ContractPartitionTables {
				connect.AssertEqual(t, slices.Contains(report.Created, ContractPartitionName(table, day(i))), true)
			}
		}
		connect.AssertEqual(t, len(report.Created), (ContractPartitionAheadDays+1)*len(ContractPartitionTables))
		// the reaped days were emptied by compaction and dropped
		for _, i := range []int{1, 3} {
			for _, table := range ContractPartitionTables {
				leaf := ContractPartitionName(table, day(i))
				connect.AssertEqual(t, slices.Contains(report.Compacted, leaf), true)
				connect.AssertEqual(t, slices.Contains(report.Dropped, leaf), true)
				connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.' || $1) IS NULL`, leaf), true)
			}
		}
		// day 4 never held a row and drops as it is
		connect.AssertEqual(t, slices.Contains(report.Dropped, ContractPartitionName("transfer_contract", day(4))), true)
		connect.AssertEqual(t, slices.Contains(report.Compacted, ContractPartitionName("transfer_contract", day(4))), false)
		connect.AssertEqual(t, len(report.Blocked), 0)
		// the days the reaper keeps rows of stay, every row in place
		connect.AssertEqual(t, report.Kept[ContractLegacyPartitionName("transfer_contract")], int64(len(ContractPartitionTables)))
		connect.AssertEqual(t, report.Kept[ContractPartitionName("transfer_contract", day(0))], int64(len(ContractPartitionTables)))
		connect.AssertEqual(t, report.Kept[ContractPartitionName("transfer_contract", day(2))], int64(len(ContractPartitionTables)))
		for contractId, rows := range keptBefore {
			connect.AssertEqual(t, testContractRows(t, ctx, contractId), rows)
			connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM st_provider_usage_archive WHERE contract_id = $1)`, contractId), false)
		}
		// only the reaper's deletes archived
		for _, contractId := range reapedContractIds {
			connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM st_provider_usage_archive WHERE contract_id = $1)`, contractId), true)
		}
		// the closed days and the parents were analyzed
		connect.AssertEqual(t, slices.Contains(report.Analyzed, ContractPartitionName("transfer_contract", day(0))), true)
		connect.AssertEqual(t, slices.Contains(report.Analyzed, "transfer_contract"), true)
		connect.AssertEqual(t, cursor.AnalyzedDay, ContractPartitionDayStart(later).Format(time.DateOnly))

		// the close evidence is its own task's since 2026-11-23: this one
		// leaves it, and that one deletes the expired report alone
		connect.AssertEqual(t, evidenceExists(expiredClientId, expiredReportId), true)
		removal, _, _ := RemoveExpiredCloseEvidence(ctx, later, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, removal.Removed, int64(1))
		connect.AssertEqual(t, evidenceExists(expiredClientId, expiredReportId), false)
		connect.AssertEqual(t, evidenceExists(openClientId, openReportId), true)
		connect.AssertEqual(t, evidenceExists(recentClientId, recentReportId), true)

		// a later pass is idempotent: nothing more to create, analyze,
		// compact or drop
		report, _ = MaintainContractPartitions(ctx, later, cursor, time.Minute)
		connect.AssertEqual(t, len(report.Created), 0)
		connect.AssertEqual(t, len(report.Compacted), 0)
		connect.AssertEqual(t, len(report.Dropped), 0)
		connect.AssertEqual(t, len(report.Kept), 3)
		connect.AssertEqual(t, slices.Contains(report.Analyzed, "transfer_contract"), false)

		// once the reaper deletes day 0's contract, day 0 is emptied and
		// drops
		testReapContract(t, ctx, keptContractId)
		testWaitForContractHorizon(t, ctx)
		report, _ = MaintainContractPartitions(ctx, later, cursor, time.Minute)
		connect.AssertEqual(t, slices.Contains(report.Compacted, ContractPartitionName("transfer_contract", day(0))), true)
		connect.AssertEqual(t, slices.Contains(report.Dropped, ContractPartitionName("transfer_contract", day(0))), true)
		connect.AssertEqual(t, len(report.Kept), 2)
	})
}

// A cutover prepared and not finalized 6 hours before its day is abandoned,
// so its legacy bound never refuses a contract.
func TestContractPartitionAbandonsUnfinalizedCutover(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if IsContractPartitioned(ctx) {
			t.Skip("the cutover starts from plain contract tables")
		}
		cutoverDay := ContractPartitionDayStart(NowUtc()).AddDate(0, 0, 2)
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverDay: cutoverDay, Logf: testContractPartitionLogf(t)}), nil)

		report, _ := MaintainContractPartitions(ctx, cutoverDay.Add(-contractCutoverAbandonMargin-time.Minute), ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, report.AbandonedCutover, false)
		_, prepared := preparedContractCutoverDay(ctx)
		connect.AssertEqual(t, prepared, true)

		report, _ = MaintainContractPartitions(ctx, cutoverDay.Add(-contractCutoverAbandonMargin+time.Minute), ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, report.AbandonedCutover, true)
		connect.AssertEqual(t, report.Partitioned, false)
		_, prepared = preparedContractCutoverDay(ctx)
		connect.AssertEqual(t, prepared, false)
		// a contract of the cutover day is admitted again
		testInsertContractRows(t, ctx, testContractIdAt(cutoverDay.Add(time.Hour)), false)
	})
}
