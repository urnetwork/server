// S1 (server/CONTRACT-PARTITION.md): a contract week retires at the reaper's
// horizon, and every row the contract horizon reaper keeps survives its
// retirement. The real reaper runs over two expired weeks first, then the
// partition maintenance task; the reaper's keep rules are evaluated in one
// place only, the reaper.
package model

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Every row a contract holds in the six contract tables, as text.
func weekRetirementTestRows(t testing.TB, ctx context.Context, contractId server.Id) (rows []string) {
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

// The leaves of the week starting at weekStart, in table order.
func weekRetirementTestLeaves(weekStart time.Time) (leaves []string) {
	for _, table := range server.ContractPartitionTables {
		leaves = append(leaves, server.ContractPartitionName(table, weekStart))
	}
	return
}

// Each leaf's physical relation, or "" once the leaf is gone.
func weekRetirementTestFilenodes(t testing.TB, ctx context.Context, weekStart time.Time) (filenodes []string) {
	t.Helper()
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		for _, leaf := range weekRetirementTestLeaves(weekStart) {
			var filenode string
			server.Raise(conn.QueryRow(ctx, `SELECT coalesce((SELECT pg_relation_filenode(to_regclass('public.' || $1))::text), '')`, leaf).Scan(&filenode))
			filenodes = append(filenodes, filenode)
		}
	})
	return
}

// Waits until no other backend of this database holds a snapshot or a
// transaction id from before now, so a row deleted before the call is dead to
// every snapshot. A shared test server's autovacuum workers can hold one for
// seconds.
func waitWeekRetirementTestHorizon(t testing.TB, ctx context.Context) {
	t.Helper()
	var since time.Time
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&since))
	})
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var holders string
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT coalesce(string_agg(pid::text || ' ' || coalesce(backend_type, ''), ', '), '')
				FROM pg_stat_activity
				WHERE datname = current_database() AND pid <> pg_backend_pid()
					AND (backend_xmin IS NOT NULL OR backend_xid IS NOT NULL) AND xact_start <= $1`, since).Scan(&holders))
		})
		if holders == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backends of this database still hold snapshots from before %s: %s", since, holders)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// VACUUM of each leaf of the week, as autovacuum runs after the reaper's
// deletes, once every snapshot sees them. TRUNCATE false, as on Main
// (vacuum_truncate off).
func vacuumWeekRetirementTestWeek(t testing.TB, ctx context.Context, weekStart time.Time) {
	t.Helper()
	waitWeekRetirementTestHorizon(t, ctx)
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		for _, leaf := range weekRetirementTestLeaves(weekStart) {
			server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`VACUUM (ANALYZE, TRUNCATE false) public.%s`, leaf)))
		}
	})
}

// Deletes a contract and every dependent row, dependents first, as the reaper
// does once its keep rules admit it.
func deleteWeekRetirementTestContract(t testing.TB, ctx context.Context, contractId server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, table := range []string{"contract_close_report", "contract_close", "transfer_escrow", "transfer_escrow_sweep", "contract_extender", "transfer_contract"} {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`DELETE FROM public.%s WHERE contract_id = $1`, table), contractId))
		}
	}, server.TxReadCommitted, server.OptNoRetry())
}

// Two expired weeks after the cutover, 92 days past the later one, with the
// reaper's mode on at a 35-day horizon. Week A holds a contract for each keep
// rule the partition drop never checked before S1 (paid under attribution
// review, paid by a payment with no row, paid within the discharge time,
// closed inside the horizon, an unpaid sweep in an operator block still to be
// awarded), and week B one for each it did (open, disputed, an unsettled
// escrow, a pending payment); each week also holds contracts the reaper
// deletes. The reaper runs, vacuum runs, then the partition maintenance task:
// every kept contract keeps every row in all six tables, unchanged, with no
// usage archived; the deleted ones are gone and archived; both weeks stay.
// Once the reaper has deleted the rest, both weeks drop. Before S1 the task archived and dropped week A
// whole, losing every contract the reaper kept there.
func TestContractWeekRetirementKeepsWhatTheReaperKeeps(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		const day = 24 * time.Hour
		server.Testing_PartitionContracts(ctx)
		cutoverWeek := server.ContractPartitionWeekStart(server.NowUtc())
		weekA := cutoverWeek.Add(7 * day)
		weekB := cutoverWeek.Add(14 * day)
		// the storage precision, so archived times compare exactly
		now := weekB.Add(7*day + 92*day).Truncate(time.Microsecond)
		cutoff := now.Add(-70 * day).Truncate(time.Second)
		usePayoutTransitionAt(t, cutoff)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		at := func(weekStart time.Time, i int) time.Time {
			return weekStart.Add(time.Hour + time.Duration(i)*time.Minute)
		}

		completedOldPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-30 * day))})
		completedRecentPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-2 * day))})
		reviewedPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-30 * day)), attributionReview: true})
		pendingPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{})
		missingPaymentId := server.NewId()
		openBlock := pointsBlockOf(now)
		openBlockStart, _, ok := OperatorBlockPointsWindow(openBlock, cutoff)
		if !ok {
			t.Fatal("the open block has no traffic past the cutoff")
		}

		// week A: kept by the rules the drop never checked before S1
		review := newHorizonTestContract(at(weekA, 0), "settled")
		review.paymentId = &reviewedPaymentId
		incomplete := newHorizonTestContract(at(weekA, 1), "settled")
		incomplete.paymentId = &missingPaymentId
		recentPayment := newHorizonTestContract(at(weekA, 2), "settled")
		recentPayment.paymentId = &completedRecentPaymentId
		recentClose := newHorizonTestContract(at(weekA, 3), "settled")
		recentClose.closeTime = horizonTestPointer(now.Add(-10 * day))
		recentClose.paymentId = &completedOldPaymentId
		blockPoints := newHorizonTestContract(at(weekA, 4), "settled")
		blockPoints.sweepTime = horizonTestPointer(openBlockStart.Add(now.Sub(openBlockStart) / 2))
		// week B: kept by the rules the drop checked
		open := newHorizonTestContract(at(weekB, 0), "")
		open.closeTime = nil
		open.settled = false
		disputed := newHorizonTestContract(at(weekB, 1), "")
		disputed.dispute = true
		unsettled := newHorizonTestContract(at(weekB, 2), "settled")
		unsettled.settled = false
		unsettled.paymentId = &completedOldPaymentId
		pending := newHorizonTestContract(at(weekB, 3), "settled")
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
		// in both weeks: contracts the reaper deletes
		deleted := []*horizonTestContract{}
		for _, weekStart := range []time.Time{weekA, weekB} {
			for i := 10; i < 13; i += 1 {
				paid := newHorizonTestContract(at(weekStart, i), "settled")
				paid.paymentId = &completedOldPaymentId
				deleted = append(deleted, paid)
			}
			deleted = append(deleted, newHorizonTestContract(at(weekStart, 13), "canceled"))
		}
		all := slices.Clone(deleted)
		for _, contract := range kept {
			all = append(all, contract)
		}
		insertHorizonTestContracts(t, ctx, f, all...)
		for _, contract := range all {
			if partition := server.ContractPartitionWeekStart(contract.createTime); !partition.Equal(weekA) && !partition.Equal(weekB) {
				t.Fatalf("a fixture contract fell outside weeks A and B: %s", contract.createTime)
			}
		}
		before := map[server.Id][]string{}
		for _, contract := range kept {
			before[contract.contractId] = weekRetirementTestRows(t, ctx, contract.contractId)
		}

		setHorizonTestControl(t, ctx, map[string]string{ContractHorizonControlFieldMode: "on", horizonTestDaysField: "35"})
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
		vacuumWeekRetirementTestWeek(t, ctx, weekA)
		vacuumWeekRetirementTestWeek(t, ctx, weekB)

		taskCtx := WithContractHorizonControl(ctx)
		if retention := server.ContractWeekRetention(taskCtx); retention != 35*day {
			t.Fatalf("the task reads a retention of %s from the reaper's control", retention)
		}
		server.MaintainContractPartitions(taskCtx, now, server.ContractRetentionCursor{}, time.Minute)
		for reason, contract := range kept {
			if rows := weekRetirementTestRows(t, ctx, contract.contractId); !slices.Equal(rows, before[contract.contractId]) {
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
		// both weeks stay, attached; leaves this small are compact already
		// (a large one is rewritten: server's TestContractWeekCompactionKeepsEveryRowVersion)
		for _, weekStart := range []time.Time{weekA, weekB} {
			for i, filenode := range weekRetirementTestFilenodes(t, ctx, weekStart) {
				if filenode == "" {
					t.Errorf("%s was dropped while the reaper keeps rows in it", weekRetirementTestLeaves(weekStart)[i])
				}
			}
		}

		// once the reaper has deleted every kept contract, both weeks drop
		for _, contract := range kept {
			deleteWeekRetirementTestContract(t, ctx, contract.contractId)
		}
		waitWeekRetirementTestHorizon(t, ctx)
		server.MaintainContractPartitions(taskCtx, now, server.ContractRetentionCursor{}, time.Minute)
		for _, weekStart := range []time.Time{weekA, weekB} {
			for i, filenode := range weekRetirementTestFilenodes(t, ctx, weekStart) {
				if filenode != "" {
					t.Errorf("%s stayed once empty", weekRetirementTestLeaves(weekStart)[i])
				}
			}
		}
	})
}
