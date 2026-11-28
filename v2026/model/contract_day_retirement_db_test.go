// S1 (server/CONTRACT-PARTITION.md): a contract day retires at the reaper's
// horizon, and every row the contract horizon reaper keeps survives its
// retirement. The real reaper runs over two expired days first, then the
// partition maintenance task; the reaper's keep rules are evaluated in one
// place only, the reaper.
package model

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Every row a contract holds in the six contract tables, as text.
func dayRetirementTestRows(t testing.TB, ctx context.Context, contractId server.Id) (rows []string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		for _, table := range server.ContractPartitionTables {
			var row string
			server.Raise(conn.QueryRow(ctx, fmt.Sprintf(
				`SELECT coalesce(string_agg(row_to_json(row)::text, ',' ORDER BY row_to_json(row)::text), '') FROM public.%s AS row WHERE contract_id = $1`,
				table,
			), contractId).Scan(&row))
			rows = append(rows, row)
		}
	})
	return
}

// The leaves of the day starting at dayStart, in table order.
func dayRetirementTestLeaves(dayStart time.Time) (leaves []string) {
	for _, table := range server.ContractPartitionTables {
		leaves = append(leaves, server.ContractPartitionName(table, dayStart))
	}
	return
}

// Each leaf's physical relation, or "" once the leaf is gone.
func dayRetirementTestFilenodes(t testing.TB, ctx context.Context, dayStart time.Time) (filenodes []string) {
	t.Helper()
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		for _, leaf := range dayRetirementTestLeaves(dayStart) {
			var filenode string
			server.Raise(conn.QueryRow(ctx, `SELECT coalesce((SELECT pg_relation_filenode(to_regclass('public.' || $1))::text), '')`, leaf).Scan(&filenode))
			filenodes = append(filenodes, filenode)
		}
	})
	return
}

// Waits until every row deleted before the call is dead to every snapshot, so
// a compaction copies none of them (server.Testing_WaitForSnapshotHorizon). A
// wait that read only this database's backends, as this test's did before
// 2026-11-17, returned while another database of the shared local server held
// a transaction open across the deletes; CLUSTER then copied the deleted rows
// and the days stayed ("transfer_contract_w20261123 stayed once empty").
func waitDayRetirementTestHorizon(t testing.TB, ctx context.Context) {
	t.Helper()
	if err := server.Testing_WaitForSnapshotHorizon(ctx, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
}

// VACUUM of each leaf of the day, as autovacuum runs after the reaper's
// deletes, once every snapshot sees them. TRUNCATE false, as on Main
// (vacuum_truncate off).
func vacuumDayRetirementTestDay(t testing.TB, ctx context.Context, dayStart time.Time) {
	t.Helper()
	waitDayRetirementTestHorizon(t, ctx)
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		for _, leaf := range dayRetirementTestLeaves(dayStart) {
			server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`VACUUM (ANALYZE, TRUNCATE false) public.%s`, leaf)))
		}
	})
}

// Deletes a contract and every dependent row, dependents first, as the reaper
// does once its keep rules admit it.
func deleteDayRetirementTestContract(t testing.TB, ctx context.Context, contractId server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, table := range []string{"contract_close_report", "contract_close", "transfer_escrow", "transfer_escrow_sweep", "contract_extender", "transfer_contract"} {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`DELETE FROM public.%s WHERE contract_id = $1`, table), contractId))
		}
	}, server.TxReadCommitted, server.OptNoRetry())
}

// Two expired days after the cutover, 21 days past the later one, with the
// reaper's mode on at its 14-day floor (owner, 2026-11-23; the C1 transition
// sets 18). Before the floor moved, 14 counted as 35, and nothing in these
// days was deleted or retired. Day A holds a contract for each keep
// rule the partition drop never checked before S1 (paid under attribution
// review, paid by a payment with no row, paid within the discharge time,
// closed inside the horizon, an unpaid sweep in an operator block still to be
// awarded), and day B one for each it did (open, disputed, an unsettled
// escrow, a pending payment); each day also holds contracts the reaper
// deletes. The reaper runs, vacuum runs, then the partition maintenance task:
// every kept contract keeps every row in all six tables, unchanged, with no
// usage archived; the deleted ones are gone and archived; both days stay.
// Once the reaper has deleted the rest, both days drop. Before S1 the task
// archived and dropped day A's range whole, losing every contract the reaper
// kept there.
func TestContractDayRetirementKeepsWhatTheReaperKeeps(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		const day = 24 * time.Hour
		server.Testing_PartitionContracts(ctx)
		cutoverDay := server.ContractPartitionDayStart(server.NowUtc())
		dayA := cutoverDay.Add(day)
		dayB := cutoverDay.Add(2 * day)
		// the storage precision, so archived times compare exactly
		now := dayB.Add(day + 21*day).Truncate(time.Microsecond)
		cutoff := now.Add(-70 * day).Truncate(time.Second)
		usePayoutTransitionAt(t, cutoff)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		at := func(dayStart time.Time, i int) time.Time {
			return dayStart.Add(time.Hour + time.Duration(i)*time.Minute)
		}

		// completed after both days, and longer ago than
		// CompletedContractExpiration
		completedOldPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-16 * day))})
		completedRecentPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-2 * day))})
		reviewedPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-16 * day)), attributionReview: true})
		pendingPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{})
		missingPaymentId := server.NewId()
		openBlock := pointsBlockOf(now)
		openBlockStart, _, ok := OperatorBlockPointsWindow(openBlock, cutoff)
		if !ok {
			t.Fatal("the open block has no traffic past the cutoff")
		}

		// day A: kept by the rules the drop never checked before S1
		review := newHorizonTestContract(at(dayA, 0), "settled")
		review.paymentId = &reviewedPaymentId
		incomplete := newHorizonTestContract(at(dayA, 1), "settled")
		incomplete.paymentId = &missingPaymentId
		recentPayment := newHorizonTestContract(at(dayA, 2), "settled")
		recentPayment.paymentId = &completedRecentPaymentId
		recentClose := newHorizonTestContract(at(dayA, 3), "settled")
		recentClose.closeTime = horizonTestPointer(now.Add(-10 * day))
		recentClose.paymentId = &completedOldPaymentId
		blockPoints := newHorizonTestContract(at(dayA, 4), "settled")
		blockPoints.sweepTime = horizonTestPointer(openBlockStart.Add(now.Sub(openBlockStart) / 2))
		// day B: kept by the rules the drop checked
		open := newHorizonTestContract(at(dayB, 0), "")
		open.closeTime = nil
		open.settled = false
		disputed := newHorizonTestContract(at(dayB, 1), "")
		disputed.dispute = true
		unsettled := newHorizonTestContract(at(dayB, 2), "settled")
		unsettled.settled = false
		unsettled.paymentId = &completedOldPaymentId
		pending := newHorizonTestContract(at(dayB, 3), "settled")
		pending.paymentId = &pendingPaymentId
		kept := map[string]*horizonTestContract{
			contractHorizonProtectedPaymentReview:     review,
			contractHorizonProtectedPaymentIncomplete: incomplete,
			contractHorizonProtectedPaymentRecent:     recentPayment,
			contractHorizonProtectedRecentClose:       recentClose,
			contractHorizonProtectedBlockPoints:       blockPoints,
			contractHorizonProtectedOpen:              open,
			contractHorizonProtectedUnresolved:        disputed,
			contractHorizonProtectedUnsettled:         unsettled,
			contractHorizonProtectedPaymentHold:       pending,
		}
		// in both days: contracts the reaper deletes
		deleted := []*horizonTestContract{}
		for _, dayStart := range []time.Time{dayA, dayB} {
			for i := 10; i < 13; i += 1 {
				paid := newHorizonTestContract(at(dayStart, i), "settled")
				paid.paymentId = &completedOldPaymentId
				deleted = append(deleted, paid)
			}
			deleted = append(deleted, newHorizonTestContract(at(dayStart, 13), "canceled"))
		}
		all := slices.Clone(deleted)
		for _, contract := range kept {
			all = append(all, contract)
		}
		insertHorizonTestContracts(t, ctx, f, all...)
		for _, contract := range all {
			if partition := server.ContractPartitionDayStart(contract.createTime); !partition.Equal(dayA) && !partition.Equal(dayB) {
				t.Fatalf("a fixture contract fell outside days A and B: %s", contract.createTime)
			}
		}
		before := map[server.Id][]string{}
		for _, contract := range kept {
			before[contract.contractId] = dayRetirementTestRows(t, ctx, contract.contractId)
		}

		setHorizonTestControl(t, ctx, map[string]string{ContractHorizonControlFieldMode: "on", horizonTestDaysField: "14"})
		report, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, horizonTestSettings())
		if err != nil {
			t.Fatal(err)
		}
		for reason := range kept {
			if report.Protected[reason] != 1 {
				t.Fatalf("the reaper did not keep the %s contract: %+v", reason, report)
			}
		}
		if report.Eligible != int64(len(deleted)) {
			t.Fatalf("the reaper deleted %d contracts, want %d: %+v", report.Eligible, len(deleted), report)
		}
		vacuumDayRetirementTestDay(t, ctx, dayA)
		vacuumDayRetirementTestDay(t, ctx, dayB)

		taskCtx := WithContractHorizonControl(ctx)
		if retention := server.ContractDayRetention(taskCtx); retention != 14*day {
			t.Fatalf("the task reads a retention of %s from the reaper's control", retention)
		}
		server.MaintainContractPartitions(taskCtx, now, server.ContractRetentionCursor{}, time.Minute)
		for reason, contract := range kept {
			if rows := dayRetirementTestRows(t, ctx, contract.contractId); !slices.Equal(rows, before[contract.contractId]) {
				t.Errorf("retirement changed the %s contract's rows: %v, were %v", reason, rows, before[contract.contractId])
			}
			if _, archived := countHorizonTestRows(t, ctx, contract.contractId); archived != 0 {
				t.Errorf("the %s contract was archived", reason)
			}
		}
		for _, contract := range deleted {
			rows, archived := countHorizonTestRows(t, ctx, contract.contractId)
			wantArchived := 1
			if contract.outcome == "canceled" {
				wantArchived = 0
			}
			if rows != [6]int{} || archived != wantArchived {
				t.Errorf("a %s contract the reaper deleted has rows %v, archived %d", contract.outcome, rows, archived)
			}
		}
		// both days stay, attached; leaves this small are compact already
		// (a large one is rewritten: server's TestContractDayCompactionKeepsEveryRowVersion)
		for _, dayStart := range []time.Time{dayA, dayB} {
			for i, filenode := range dayRetirementTestFilenodes(t, ctx, dayStart) {
				if filenode == "" {
					t.Errorf("%s was dropped while the reaper keeps rows in it", dayRetirementTestLeaves(dayStart)[i])
				}
			}
		}

		// once the reaper has deleted every kept contract, and every snapshot
		// sees it, both days drop
		for _, contract := range kept {
			deleteDayRetirementTestContract(t, ctx, contract.contractId)
		}
		waitDayRetirementTestHorizon(t, ctx)
		retirement, _ := server.MaintainContractPartitions(taskCtx, now, server.ContractRetentionCursor{}, time.Minute)
		for _, dayStart := range []time.Time{dayA, dayB} {
			for i, filenode := range dayRetirementTestFilenodes(t, ctx, dayStart) {
				if filenode != "" {
					t.Errorf("%s stayed once empty: %+v", dayRetirementTestLeaves(dayStart)[i], retirement.Blocked)
				}
			}
		}
	})
}
