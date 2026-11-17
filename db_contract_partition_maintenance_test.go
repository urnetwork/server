package server

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
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

// Waits until no other backend of this database holds a snapshot or a
// transaction id from before now, so a row deleted before the call is dead to
// every snapshot: vacuum removes it, and CLUSTER copies it no more. A shared
// test server's autovacuum workers can hold one for seconds.
func testWaitForContractHorizon(t testing.TB, ctx context.Context) {
	t.Helper()
	since := testContractPartitionScalar[time.Time](t, ctx, `SELECT clock_timestamp()`)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		holders := testContractPartitionScalar[string](t, ctx, `
			SELECT coalesce(string_agg(pid::text || ' ' || coalesce(backend_type, ''), ', '), '')
			FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
				AND (backend_xmin IS NOT NULL OR backend_xid IS NOT NULL) AND xact_start <= $1`, since)
		if holders == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backends of this database still hold snapshots from before %s: %s", since, holders)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// VACUUM (ANALYZE) of each leaf of the week r, as autovacuum runs after the
// reaper's deletes: it removes the deleted rows from the heaps and keys, and
// sets the row estimates. TRUNCATE false, as on Main (vacuum_truncate off).
// It first waits until every snapshot sees those deletes.
func testVacuumContractWeek(t testing.TB, ctx context.Context, r PartitionRange) {
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

// Exercises a maintenance pass on partitioned contract tables 20 weeks after
// the cutover (S1). Retirement deletes no row: the reaper alone does. A week
// the reaper emptied is compacted and dropped in one pass; weeks whose rows
// the reaper keeps (a resolved contract in week 0, an unresolved one in week
// 2, and one with an unsettled escrow in the legacy partition) are reported
// kept, every row in place and none archived. The weeks ahead are created,
// closed weeks and the parents analyzed, and expired close evidence is deleted
// unless its contract is unresolved. Once the reaper deletes week 0's
// contract, week 0 drops too.
func TestContractPartitionMaintenance(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()

		// before the cutover week, so in the legacy partition
		legacyContractId := testContractIdAt(ContractPartitionWeekStart(now).Add(-24 * time.Hour))
		testInsertContractRows(t, ctx, legacyContractId, true)
		// the legacy partition keeps an unsettled escrow
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET settled = false WHERE contract_id = $1`, legacyContractId))
		})

		Testing_PartitionContracts(ctx)
		cutoverWeek := ContractPartitionWeekStart(now)
		week := func(i int) time.Time {
			return cutoverWeek.Add(time.Duration(i) * contractPartitionWeek)
		}

		// week 0: a contract the reaper keeps
		keptContractId := testContractIdAt(week(0).Add(time.Hour))
		testInsertContractRows(t, ctx, keptContractId, true)
		// weeks 1 and 3: contracts the reaper deleted
		reapedContractIds := []Id{
			testContractIdAt(week(1).Add(time.Hour)),
			testContractIdAt(week(1).Add(2 * time.Hour)),
			testContractIdAt(week(3).Add(time.Hour)),
		}
		for _, contractId := range reapedContractIds {
			testInsertContractRows(t, ctx, contractId, true)
			testReapContract(t, ctx, contractId)
		}
		// week 2: an unresolved contract
		unresolvedContractId := testContractIdAt(week(2).Add(time.Hour))
		testInsertContractRows(t, ctx, unresolvedContractId, false)
		for _, i := range []int{0, 1, 2, 3} {
			testVacuumContractWeek(t, ctx, contractPartitionFamily.RangeAt(week(i)))
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

		later := week(20).Add(time.Hour)
		report, cursor := MaintainContractPartitions(ctx, later, ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, report.Partitioned, true)
		// the weeks ahead exist
		connect.AssertEqual(t, report.FutureWeeks, ContractPartitionAheadWeeks)
		for i := 20; i <= 20+ContractPartitionAheadWeeks; i += 1 {
			for _, table := range ContractPartitionTables {
				connect.AssertEqual(t, slices.Contains(report.Created, ContractPartitionName(table, week(i))), true)
			}
		}
		// the reaped weeks were emptied by compaction and dropped
		for _, i := range []int{1, 3} {
			for _, table := range ContractPartitionTables {
				leaf := ContractPartitionName(table, week(i))
				connect.AssertEqual(t, slices.Contains(report.Compacted, leaf), true)
				connect.AssertEqual(t, slices.Contains(report.Dropped, leaf), true)
				connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.' || $1) IS NULL`, leaf), true)
			}
		}
		// week 4 never held a row and drops as it is
		connect.AssertEqual(t, slices.Contains(report.Dropped, ContractPartitionName("transfer_contract", week(4))), true)
		connect.AssertEqual(t, slices.Contains(report.Compacted, ContractPartitionName("transfer_contract", week(4))), false)
		connect.AssertEqual(t, len(report.Blocked), 0)
		// the weeks the reaper keeps rows of stay, every row in place
		connect.AssertEqual(t, report.Kept[ContractLegacyPartitionName("transfer_contract")], int64(len(ContractPartitionTables)))
		connect.AssertEqual(t, report.Kept[ContractPartitionName("transfer_contract", week(0))], int64(len(ContractPartitionTables)))
		connect.AssertEqual(t, report.Kept[ContractPartitionName("transfer_contract", week(2))], int64(len(ContractPartitionTables)))
		for contractId, rows := range keptBefore {
			connect.AssertEqual(t, testContractRows(t, ctx, contractId), rows)
			connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM st_provider_usage_archive WHERE contract_id = $1)`, contractId), false)
		}
		// only the reaper's deletes archived
		for _, contractId := range reapedContractIds {
			connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM st_provider_usage_archive WHERE contract_id = $1)`, contractId), true)
		}
		// the closed weeks and the parents were analyzed
		connect.AssertEqual(t, slices.Contains(report.Analyzed, ContractPartitionName("transfer_contract", week(0))), true)
		connect.AssertEqual(t, slices.Contains(report.Analyzed, "transfer_contract"), true)
		connect.AssertEqual(t, cursor.AnalyzedWeek, ContractPartitionWeekStart(later).Format(time.DateOnly))

		connect.AssertEqual(t, evidenceExists(expiredClientId, expiredReportId), false)
		connect.AssertEqual(t, evidenceExists(openClientId, openReportId), true)
		connect.AssertEqual(t, evidenceExists(recentClientId, recentReportId), true)
		connect.AssertEqual(t, report.EvidenceRemoved, int64(1))

		// a later pass is idempotent: nothing more to create, analyze,
		// compact or drop
		report, _ = MaintainContractPartitions(ctx, later, cursor, time.Minute)
		connect.AssertEqual(t, len(report.Created), 0)
		connect.AssertEqual(t, len(report.Compacted), 0)
		connect.AssertEqual(t, len(report.Dropped), 0)
		connect.AssertEqual(t, len(report.Kept), 3)
		connect.AssertEqual(t, slices.Contains(report.Analyzed, "transfer_contract"), false)

		// once the reaper deletes week 0's contract, week 0 is emptied and
		// drops
		testReapContract(t, ctx, keptContractId)
		testWaitForContractHorizon(t, ctx)
		report, _ = MaintainContractPartitions(ctx, later, cursor, time.Minute)
		connect.AssertEqual(t, slices.Contains(report.Compacted, ContractPartitionName("transfer_contract", week(0))), true)
		connect.AssertEqual(t, slices.Contains(report.Dropped, ContractPartitionName("transfer_contract", week(0))), true)
		connect.AssertEqual(t, len(report.Kept), 2)
	})
}

// A cutover prepared and not finalized 6 hours before its week is abandoned,
// so its legacy bound never refuses a contract.
func TestContractPartitionAbandonsUnfinalizedCutover(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if IsContractPartitioned(ctx) {
			t.Skip("the cutover starts from plain contract tables")
		}
		cutoverWeek := ContractPartitionWeekStart(NowUtc()).Add(2 * contractPartitionWeek)
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverWeek: cutoverWeek, Logf: testContractPartitionLogf(t)}), nil)

		report, _ := MaintainContractPartitions(ctx, cutoverWeek.Add(-contractCutoverAbandonMargin-time.Minute), ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, report.AbandonedCutover, false)
		_, prepared := preparedContractCutoverWeek(ctx)
		connect.AssertEqual(t, prepared, true)

		report, _ = MaintainContractPartitions(ctx, cutoverWeek.Add(-contractCutoverAbandonMargin+time.Minute), ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, report.AbandonedCutover, true)
		connect.AssertEqual(t, report.Partitioned, false)
		_, prepared = preparedContractCutoverWeek(ctx)
		connect.AssertEqual(t, prepared, false)
		// a contract of the cutover week is admitted again
		testInsertContractRows(t, ctx, testContractIdAt(cutoverWeek.Add(time.Hour)), false)
	})
}
