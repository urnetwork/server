// The contract horizon reaper's zero-byte pass against the migrated schema: it
// deletes exactly the zero-byte contracts past the 8-day zero-byte horizon,
// with their rows, keeps every other contract for the horizon, follows its
// control, skips what another transaction holds, and resumes at its cursor.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// The rows of a zero-byte fixture contract: the contract, two closes, an
// escrow, no sweep, no extender party and one close report.
var zeroByteTestRows = [6]int{1, 2, 1, 0, 0, 1}

// A zero-byte contract, settled by both parties with no bytes, its escrow
// settled with nothing paid out, no sweep, no extender party and a usage
// snapshot of zero bytes, goes with its rows once it is 8 days old, and leaves
// its zero-byte usage in the archive. Every contract beside it keeps every row
// for the horizon: one with bytes in a close, a sweep or its usage; the
// zero-byte sweep of a payment under attribution review;
// one open, in review or resolved by a dispute; one closed by one party only;
// one with an extender party; one whose escrow is unsettled or paid out; one
// closed inside 8 days; and one minted inside 8 days. A second pass deletes
// nothing more. Before the zero-byte horizon, the horizon (35 days on Main then)
// kept every one of them.
func TestContractHorizonRemovesZeroByteContractsPastEightDays(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		// the storage precision, so archived times compare exactly
		now := server.NowUtc().Truncate(time.Microsecond)
		zeroByte, kept, young := newZeroByteTestScenario(t, ctx, f, now)
		before := map[server.Id][6]int{}
		for _, contracts := range kept {
			for _, contract := range contracts {
				before[contract.contractId], _ = countHorizonTestRows(t, ctx, contract.contractId)
			}
		}
		before[young.contractId], _ = countHorizonTestRows(t, ctx, young.contractId)
		for _, contract := range zeroByte {
			if rows, _ := countHorizonTestRows(t, ctx, contract.contractId); rows != zeroByteTestRows {
				t.Fatalf("the zero-byte fixture has rows %v, want %v", rows, zeroByteTestRows)
			}
		}

		settings := horizonTestSettings()
		// the horizon at its floor, 14 days, past the zero-byte horizon
		settings.Horizon = ContractHorizonFloor
		if _, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings); err != nil {
			t.Fatal(err)
		}
		for _, contract := range zeroByte {
			rows, archived := countHorizonTestRows(t, ctx, contract.contractId)
			if rows != [6]int{} || archived != 1 {
				t.Fatalf("a zero-byte contract past 8 days kept rows %v (contract, close, escrow, sweep, extender, report) or archived %d", rows, archived)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var outcome string
				var closeTime time.Time
				var usage []byte
				server.Raise(conn.QueryRow(ctx, `SELECT outcome,close_time,provider_usage FROM st_provider_usage_archive WHERE contract_id=$1`,
					contract.contractId).Scan(&outcome, &closeTime, &usage))
				snapshot, err := decodeContractUsageSnapshot(usage)
				if err != nil || outcome != "settled" || !closeTime.Equal(*contract.closeTime) || snapshot.ByteCount != 0 {
					t.Fatal("the archive does not hold the zero-byte contract's exact usage", outcome, closeTime, snapshot, err)
				}
			})
		}
		unchanged := func(when string) {
			for reason, contracts := range kept {
				for _, contract := range contracts {
					if rows, archived := countHorizonTestRows(t, ctx, contract.contractId); rows != before[contract.contractId] || archived != 0 {
						t.Fatalf("%s: a %s contract changed: rows %v, was %v, archived %d", when, reason, rows, before[contract.contractId], archived)
					}
				}
			}
			if rows, _ := countHorizonTestRows(t, ctx, young.contractId); rows != before[young.contractId] {
				t.Fatalf("%s: a zero-byte contract minted inside 8 days changed: %v", when, rows)
			}
		}
		unchanged("first pass")

		if _, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings); err != nil {
			t.Fatal(err)
		}
		unchanged("second pass")
	})
}

// The zero-byte pass reports what it read, deleted and kept, each contract
// under the first reason that kept it, apart from the horizon's pass; counts
// its deletes in its own metric; and hands on its cursor, so the next run
// reads nothing before it.
func TestContractHorizonZeroBytePassReports(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Microsecond)
		zeroByte, kept, _ := newZeroByteTestScenario(t, ctx, f, now)
		settings := horizonTestSettings()
		settings.Horizon = ContractHorizonFloor
		deletedBefore := testutil.ToFloat64(contractHorizonZeroByteDeletedCounter)
		rowsBefore := testutil.ToFloat64(contractHorizonDeletedRowsCounter.WithLabelValues("transfer_contract"))

		report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil {
			t.Fatal(err)
		}
		read := int64(len(zeroByte))
		var last server.Id
		for reason, contracts := range kept {
			read += int64(len(contracts))
			if report.ZeroByteKept[reason] != int64(len(contracts)) {
				t.Errorf("kept %s = %d, want %d", reason, report.ZeroByteKept[reason], len(contracts))
			}
			for _, contract := range contracts {
				if last.Less(contract.contractId) {
					last = contract.contractId
				}
			}
		}
		if report.ZeroByteKept[contractHorizonZeroByteKeptBusy] != 0 {
			t.Errorf("kept busy = %d", report.ZeroByteKept[contractHorizonZeroByteKeptBusy])
		}
		wantDeleted := map[string]int64{"transfer_contract": 2, "contract_close": 4, "transfer_escrow": 2, "contract_close_report": 2}
		for table, count := range wantDeleted {
			if report.ZeroByteDeleted[table] != count {
				t.Errorf("zero-byte deleted %s = %d, want %d", table, report.ZeroByteDeleted[table], count)
			}
		}
		if report.ZeroByteScanned != read || report.ZeroByteEligible != 2 || report.ZeroByteArchived != 2 ||
			report.ZeroByteMode != ContractHorizonModeOn || !report.ZeroByteHorizonTime.Equal(now.Add(-ContractHorizonZeroByteFloor)) ||
			!report.ZeroByteCaughtUp || report.ZeroByteBehind || !report.CaughtUp || report.Stop != contractHorizonStopHorizon {
			t.Fatalf("wrong zero-byte report: %+v", report)
		}
		// the horizon's pass read nothing: every contract is inside its horizon
		if report.Scanned != 0 || report.Eligible != 0 || len(report.Deleted) != 0 || report.Protected[contractHorizonProtectedRecentClose] != 0 {
			t.Fatalf("the horizon's pass counted zero-byte work: %+v", report)
		}
		if deleted := testutil.ToFloat64(contractHorizonZeroByteDeletedCounter) - deletedBefore; deleted != 2 {
			t.Errorf("the zero-byte deleted metric rose %v, want 2", deleted)
		}
		if rows := testutil.ToFloat64(contractHorizonDeletedRowsCounter.WithLabelValues("transfer_contract")) - rowsBefore; rows != 2 {
			t.Errorf("the reaper's deleted contract rows rose %v, want 2", rows)
		}
		if next.ZeroByteContractId != last || next.ContractId != (server.Id{}) {
			t.Fatalf("the zero-byte cursor is not at the last contract it read: %+v", next)
		}

		again, againNext, err := RemoveContractsPastHorizon(ctx, now, next, settings)
		if err != nil || again.ZeroByteScanned != 0 || !again.ZeroByteCaughtUp || againNext.ZeroByteContractId != last {
			t.Fatalf("a second run did not find the zero-byte pass complete: %+v %v", again, err)
		}
	})
}

// The zero-byte horizon is exactly 8 days before now for the close time, and
// the zero-byte pass reads a contract once its mint time in the contract id is
// exactly the close settle time before that: a zero-byte contract on either
// boundary stays, one a microsecond of close time, or a millisecond of mint
// time, past it goes.
func TestContractHorizonZeroByteBoundaryIsExact(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		mint := server.NowUtc().Add(-20 * day).Truncate(time.Millisecond)
		// closed before it was minted, so only the id decides
		mintBoundary := newZeroByteTestContract(mint)
		mintBoundary.closeTime = horizonTestPointer(mint.Add(-time.Hour))
		closed := mint.Add(-5 * day).Truncate(time.Microsecond)
		closeBoundary := newZeroByteTestContract(mint.Add(-10 * day))
		closeBoundary.closeTime = &closed
		insertZeroByteTestContracts(t, ctx, f, mintBoundary, closeBoundary)
		settings := horizonTestSettings()
		run := func(now time.Time) *ContractHorizonReport {
			report, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
			if err != nil {
				t.Fatal(err)
			}
			return report
		}
		present := func(contract *zeroByteTestContract) bool {
			rows, _ := countHorizonTestRows(t, ctx, contract.contractId)
			return rows[0] == 1
		}
		zeroByteHorizon := ContractHorizonZeroByteFloor

		if report := run(closed.Add(zeroByteHorizon)); !present(closeBoundary) || report.ZeroByteKept[contractHorizonZeroByteKeptRecentClose] != 1 {
			t.Fatalf("a zero-byte contract closed exactly at the zero-byte horizon was removed: %+v", report)
		}
		if run(closed.Add(zeroByteHorizon).Add(time.Microsecond)); present(closeBoundary) {
			t.Fatal("a zero-byte contract closed a microsecond before the zero-byte horizon was kept")
		}
		if report := run(mint.Add(zeroByteHorizon)); !present(mintBoundary) || report.ZeroByteScanned != 0 {
			t.Fatalf("a zero-byte contract minted exactly at the zero-byte horizon was read or removed: %+v", report)
		}
		settle := settings.CloseSettleTime
		if report := run(mint.Add(zeroByteHorizon + settle)); !present(mintBoundary) || report.ZeroByteScanned != 0 {
			t.Fatalf("a zero-byte contract minted exactly the close settle time before the zero-byte horizon was read or removed: %+v", report)
		}
		if run(mint.Add(zeroByteHorizon + settle).Add(time.Millisecond)); present(mintBoundary) {
			t.Fatal("a zero-byte contract minted a millisecond more than the close settle time before the zero-byte horizon was kept")
		}
	})
}

// The zero-byte pass follows its control: off keeps every zero-byte contract
// for the horizon; a dry run deletes nothing, counts what it would delete and
// leaves its cursor; a longer zero-byte horizon keeps the contracts inside it;
// a zero-byte horizon at the horizon turns it off; the reaper's mode off stops
// it; and without its fields it runs at 8 days.
func TestContractHorizonZeroByteFollowsItsControl(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		now := server.NowUtc()
		tenDays := newZeroByteTestContract(now.Add(-10 * day))
		twelveDays := newZeroByteTestContract(now.Add(-12 * day))
		insertZeroByteTestContracts(t, ctx, f, tenDays, twelveDays)
		settings := horizonTestSettings()
		present := func(contract *zeroByteTestContract) bool {
			rows, _ := countHorizonTestRows(t, ctx, contract.contractId)
			return rows[0] == 1
		}
		run := func(fields map[string]string) (*ContractHorizonReport, ContractHorizonCursor) {
			removeHorizonTestControl(t, ctx)
			if 0 < len(fields) {
				setHorizonTestControl(t, ctx, fields)
			}
			report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
			if err != nil {
				t.Fatal(err)
			}
			return report, next
		}
		t.Cleanup(func() { removeHorizonTestControl(t, ctx) })

		if report, _ := run(map[string]string{ContractHorizonControlFieldZeroByteMode: "off"}); !present(tenDays) || !present(twelveDays) ||
			report.ZeroByteMode != ContractHorizonModeOff || report.ZeroByteScanned != 0 || report.Stop != contractHorizonStopHorizon {
			t.Fatalf("the zero-byte pass ran while off: %+v", report)
		}
		if report, next := run(map[string]string{ContractHorizonControlFieldZeroByteMode: "dry_run"}); !present(tenDays) || !present(twelveDays) ||
			report.ZeroByteMode != ContractHorizonModeDryRun || report.ZeroByteEligible != 2 || report.ZeroByteScanned != 2 ||
			report.ZeroByteBehind || next.ZeroByteContractId != (server.Id{}) {
			t.Fatalf("a dry run deleted, miscounted or moved the zero-byte cursor: %+v %+v", report, next)
		}
		if report, _ := run(map[string]string{ContractHorizonControlFieldMode: "off"}); !present(tenDays) || !present(twelveDays) ||
			report.ZeroByteMode != ContractHorizonModeOff || report.Stop != contractHorizonStopOff {
			t.Fatalf("the zero-byte pass ran with the reaper off: %+v", report)
		}
		if report, _ := run(map[string]string{ContractHorizonControlFieldHorizonDays: "35", ContractHorizonControlFieldZeroByteHorizonDays: "35"}); !present(tenDays) || !present(twelveDays) ||
			report.ZeroByteMode != ContractHorizonModeOff || report.ZeroByteScanned != 0 {
			t.Fatalf("a zero-byte horizon at the horizon did not turn the zero-byte pass off: %+v", report)
		}
		if report, _ := run(map[string]string{ContractHorizonControlFieldZeroByteHorizonDays: "11"}); !present(tenDays) || present(twelveDays) ||
			report.ZeroByteEligible != 1 || !report.ZeroByteHorizonTime.Equal(now.Add(-11*day)) {
			t.Fatalf("an 11-day zero-byte horizon did not keep the contract inside it and delete the one past it: %+v", report)
		}
		if report, _ := run(nil); present(tenDays) || report.ZeroByteEligible != 1 || report.ZeroByteMode != ContractHorizonModeOn {
			t.Fatalf("the zero-byte pass did not run at 8 days without its fields: %+v", report)
		}
	})
}

// A zero-byte contract another transaction holds is skipped as busy, never
// waited on, and the rest of the batch goes. The zero-byte pass does not read
// it again; the horizon's pass deletes it at the horizon.
func TestContractHorizonZeroByteSkipsBusyContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		now := server.NowUtc()
		held := newZeroByteTestContract(now.Add(-10 * day))
		free := newZeroByteTestContract(now.Add(-10*day + time.Second))
		insertZeroByteTestContracts(t, ctx, f, held, free)

		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		tx, err := holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer tx.Rollback(context.WithoutCancel(ctx))
		server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, held.contractId))

		settings := horizonTestSettings()
		settings.Horizon = ContractHorizonFloor
		settings.LockTimeout = 200 * time.Millisecond
		report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || report.ZeroByteKept[contractHorizonZeroByteKeptBusy] != 1 || report.ZeroByteEligible != 1 {
			t.Fatalf("a held zero-byte contract was waited on or deleted: %+v %v", report, err)
		}
		if rows, _ := countHorizonTestRows(t, ctx, held.contractId); rows != zeroByteTestRows {
			t.Fatal("the held contract lost rows", rows)
		}
		if rows, _ := countHorizonTestRows(t, ctx, free.contractId); rows != [6]int{} {
			t.Fatal("the free zero-byte contract was kept", rows)
		}

		server.Raise(tx.Rollback(ctx))
		again, next, err := RemoveContractsPastHorizon(ctx, now, next, settings)
		if rows, _ := countHorizonTestRows(t, ctx, held.contractId); err != nil || again.ZeroByteScanned != 0 || rows != zeroByteTestRows {
			t.Fatalf("the zero-byte pass read a contract behind its cursor again: %+v %v", again, err)
		}
		// past the horizon, 36 days after it was minted
		later, _, err := RemoveContractsPastHorizon(ctx, now.Add(26*day), next, settings)
		if rows, _ := countHorizonTestRows(t, ctx, held.contractId); err != nil || later.Eligible != 1 || rows != [6]int{} {
			t.Fatalf("the horizon's pass did not delete the contract the zero-byte pass skipped: %+v %v", later, err)
		}
	})
}

// The zero-byte pass commits batch by batch and hands on its cursor: a run
// stopped after its first committed batch resumes after it, and reads only the
// contracts past it.
func TestContractHorizonZeroBytePassResumesAtItsCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		now := server.NowUtc()
		contracts := make([]*zeroByteTestContract, 5)
		for i := range contracts {
			contracts[i] = newZeroByteTestContract(now.Add(-10*day + time.Duration(i)*time.Second))
		}
		insertZeroByteTestContracts(t, ctx, f, contracts...)
		settings := horizonTestSettings()
		settings.MinBatchSize = 1
		setHorizonTestControl(t, ctx, map[string]string{ContractHorizonControlFieldBatchSize: "2"})
		t.Cleanup(func() { removeHorizonTestControl(t, ctx) })

		stopped, stop := context.WithCancel(ctx)
		defer stop()
		committed := []server.Id{}
		contractHorizonZeroByteBatchCommitted = func(batch int, last server.Id) {
			committed = append(committed, last)
			stop()
		}
		t.Cleanup(func() { contractHorizonZeroByteBatchCommitted = nil })
		report, next, err := RemoveContractsPastHorizon(stopped, now, ContractHorizonCursor{}, settings)
		if err != nil || report.Stop != contractHorizonStopCanceled || len(committed) != 1 || committed[0] != contracts[1].contractId ||
			next.ZeroByteContractId != contracts[1].contractId || !report.ZeroByteBehind {
			t.Fatalf("the stopped run did not hand on its first batch's cursor: %+v %+v %v %v", report, next, committed, err)
		}
		for i, contract := range contracts {
			if rows, _ := countHorizonTestRows(t, ctx, contract.contractId); (i < 2) != (rows == [6]int{}) {
				t.Fatalf("contract %d has rows %v after the first batch", i, rows)
			}
		}

		contractHorizonZeroByteBatchCommitted = nil
		resumed, _, err := RemoveContractsPastHorizon(ctx, now, next, settings)
		if err != nil || resumed.ZeroByteScanned != 3 || resumed.ZeroByteEligible != 3 || !resumed.ZeroByteCaughtUp {
			t.Fatalf("the resumed run did not read only the contracts past the cursor: %+v %v", resumed, err)
		}
		for _, contract := range contracts {
			if rows, _ := countHorizonTestRows(t, ctx, contract.contractId); rows != [6]int{} {
				t.Fatal("a zero-byte contract was kept", rows)
			}
		}
	})
}

// Both passes in one run: the horizon's pass deletes what is past the horizon
// by its own rules, zero-byte or not, and counts only its own; the zero-byte
// pass deletes only the zero-byte contracts between the two horizons.
func TestContractHorizonZeroBytePassBesideTheHorizon(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		now := server.NowUtc()
		paymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-30 * day))})
		oldZeroByte := newZeroByteTestContract(now.Add(-40 * day))
		oldBytes := newZeroByteTestContract(now.Add(-40*day + time.Second))
		oldBytes.closes = map[ContractParty]ByteCount{ContractPartySource: 100, ContractPartyDestination: 100}
		oldBytes.usageBytes = 100
		oldBytes.sweep = true
		oldBytes.paymentId = &paymentId
		zeroByte := newZeroByteTestContract(now.Add(-10 * day))
		withBytes := newZeroByteTestContract(now.Add(-10*day + time.Second))
		withBytes.closes = oldBytes.closes
		withBytes.usageBytes = 100
		withBytes.sweep = true
		withBytes.paymentId = &paymentId
		insertZeroByteTestContracts(t, ctx, f, oldZeroByte, oldBytes, zeroByte, withBytes)

		settings := horizonTestSettings()
		settings.Horizon = ContractHorizonFloor
		report, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil {
			t.Fatal(err)
		}
		for _, contract := range []*zeroByteTestContract{oldZeroByte, oldBytes, zeroByte} {
			if rows, _ := countHorizonTestRows(t, ctx, contract.contractId); rows != [6]int{} {
				t.Fatal("a contract past its horizon was kept", rows)
			}
		}
		if rows, _ := countHorizonTestRows(t, ctx, withBytes.contractId); rows[0] != 1 {
			t.Fatal("a contract with bytes inside the horizon was deleted", rows)
		}
		if report.Scanned != 2 || report.Eligible != 2 || report.Deleted["transfer_contract"] != 2 || report.Deleted["transfer_escrow_sweep"] != 1 ||
			report.ZeroByteScanned != 2 || report.ZeroByteEligible != 1 || report.ZeroByteDeleted["transfer_contract"] != 1 ||
			report.ZeroByteKept[contractHorizonZeroByteKeptBytes] != 1 || report.Stop != contractHorizonStopHorizon {
			t.Fatalf("the passes did not each count their own contracts: %+v", report)
		}
	})
}
