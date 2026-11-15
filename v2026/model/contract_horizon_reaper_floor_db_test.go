// The contract horizon reaper at the horizon the runtime control's horizonDays
// sets, down to the 35-day floor: what the shorter horizon reaches, exactly
// where it stops, that the control applies from the next batch, that every
// protection still holds there, and that operator block points keep the
// sweeps they still read.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
)

// The control field as an operator sets it:
// HSET {contract_horizon}control horizonDays 35
const horizonTestDaysField = "horizonDays"

// The value of the unlabeled gauge the process exports as name, as a scrape
// reads it. False when there is none, or it has labels.
func horizonTestGauge(name string) (float64, bool) {
	// a collector that fails elsewhere still leaves this family in the result
	families, _ := prometheus.DefaultGatherer.Gather()
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		metrics := family.GetMetric()
		if len(metrics) != 1 || len(metrics[0].GetLabel()) != 0 || metrics[0].GetGauge() == nil {
			return 0, false
		}
		return metrics[0].GetGauge().GetValue(), true
	}
	return 0, false
}

// The run's horizon as the reaper exports it: the horizon's length and its time.
func checkHorizonTestExported(t testing.TB, now time.Time, report *ContractHorizonReport) {
	t.Helper()
	if exported, ok := horizonTestGauge("urnetwork_contract_horizon_horizon_seconds"); !ok || exported != now.Sub(report.HorizonTime).Seconds() {
		t.Errorf("exported horizon %v s (found %t), reported %s", exported, ok, now.Sub(report.HorizonTime))
	}
	if exported, ok := horizonTestGauge("urnetwork_contract_horizon_horizon_timestamp_seconds"); !ok || exported != float64(report.HorizonTime.Unix()) {
		t.Errorf("exported horizon time %v (found %t), reported %s", exported, ok, report.HorizonTime)
	}
}

// horizonDays shortens the horizon to as few as 35 days. At 45 days a
// contract 60 days old goes, into the archive since it bears credit; at 35 a
// contract 40 days old goes too, and contracts 34 and 30 days old stay. The
// boundary is exactly 35 days, for the mint time in the id and for the close
// time, and a value below the floor reaches no closer. Without the field, or
// with one that does not parse or is past 91 days, the horizon is the code's
// 91 days. Each run reports and exports the horizon it applied.
func TestContractHorizonControlShortensHorizonToFloor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		// a whole millisecond, so a mint time and a close time can sit exactly
		// on the horizon
		now := server.NowUtc().Truncate(time.Millisecond)
		floor := now.Add(-35 * day)

		// credit-bearing and unswept, so nothing but the horizon keeps it
		aged60 := newHorizonTestContract(now.Add(-60*day), "settled")
		aged60.unswept = true
		aged40 := newHorizonTestContract(now.Add(-40*day), "canceled")
		aged34 := newHorizonTestContract(now.Add(-34*day), "canceled")
		aged30 := newHorizonTestContract(now.Add(-30*day), "canceled")
		// closed before it was minted, so only the id decides
		mintBoundary := newHorizonTestContract(floor, "canceled")
		mintBoundary.closeTime = horizonTestPointer(floor.Add(-time.Hour))
		closeBoundary := newHorizonTestContract(now.Add(-40*day), "canceled")
		closeBoundary.closeTime = horizonTestPointer(floor)
		all := []*horizonTestContract{aged60, aged40, aged34, aged30, mintBoundary, closeBoundary}
		insertHorizonTestContracts(t, ctx, f, all...)

		present := func(contract *horizonTestContract) bool {
			rows, _ := countHorizonTestRows(t, ctx, contract.contractId)
			return rows[0] == 1
		}
		// "" runs without the control: the code default, mode on
		run := func(now time.Time, horizonDays string) *ContractHorizonReport {
			if horizonDays == "" {
				removeHorizonTestControl(t, ctx)
			} else {
				setHorizonTestControl(t, ctx, map[string]string{horizonTestDaysField: horizonDays})
			}
			report, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, horizonTestSettings())
			if err != nil {
				t.Fatal(err)
			}
			checkHorizonTestExported(t, now, report)
			return report
		}

		for _, horizonDays := range []string{"", "soon", "35.5", "400"} {
			if report := run(now, horizonDays); !report.HorizonTime.Equal(now.Add(-91*day)) || report.Scanned != 0 {
				t.Fatalf("horizonDays %q is not the code's 91 days: %s, %+v", horizonDays, now.Sub(report.HorizonTime), report)
			}
		}
		for _, contract := range all {
			if !present(contract) {
				t.Fatal("the code's horizon removed a contract inside 91 days", contract.createTime)
			}
		}

		report := run(now, "45")
		if rows, archived := countHorizonTestRows(t, ctx, aged60.contractId); rows != [6]int{} || archived != 1 {
			t.Fatalf("a 45-day horizon kept a contract 60 days old: rows %v (contract, close, escrow, sweep, extender, report), archived %d, horizon %s",
				rows, archived, now.Sub(report.HorizonTime))
		}
		if !present(aged40) || !present(aged34) || !present(mintBoundary) || !present(closeBoundary) {
			t.Fatal("a 45-day horizon removed a contract inside it")
		}

		report = run(now, "35")
		if present(aged40) {
			t.Fatalf("a 35-day horizon kept a contract 40 days old: horizon %s, %+v", now.Sub(report.HorizonTime), report)
		}
		if !present(aged34) || !present(aged30) || !present(mintBoundary) || !present(closeBoundary) {
			t.Fatal("a 35-day horizon removed a contract on it or inside it")
		}
		if !report.HorizonTime.Equal(floor) || report.Scanned != 2 || report.Eligible != 1 || report.Archived != 0 ||
			report.Protected[contractHorizonProtectedRecentClose] != 1 || report.Deleted["transfer_contract"] != 1 {
			t.Fatalf("wrong report at 35 days: %+v", report)
		}

		// a microsecond later the close boundary is past; the mint boundary's
		// millisecond is not
		if run(now.Add(time.Microsecond), "35"); present(closeBoundary) || !present(mintBoundary) {
			t.Fatal("the 35-day horizon is not exact for the close time")
		}
		// a millisecond later the mint boundary is past, and a horizon below the
		// floor still keeps the contract 34 days old
		report = run(now.Add(time.Millisecond), "1")
		if present(mintBoundary) || !present(aged34) || !present(aged30) || !report.HorizonTime.Equal(floor.Add(time.Millisecond)) {
			t.Fatalf("a horizon below the floor is not exactly 35 days for the mint time: %s, %+v", now.Add(time.Millisecond).Sub(report.HorizonTime), report)
		}
	})
}

// The control's horizon is read before every batch, as its other fields are:
// raising it back to 91 days during a pass at 45 stops the pass before its
// next batch, with nothing more inside 91 days deleted, and lowering it again
// resumes the pass at its cursor.
func TestContractHorizonControlHorizonAppliesFromNextBatch(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		now := server.NowUtc()
		contracts := []*horizonTestContract{}
		for i := range 30 {
			contracts = append(contracts, newHorizonTestContract(now.Add(-60*day+time.Duration(i)*time.Second), "canceled"))
		}
		insertHorizonTestContracts(t, ctx, f, contracts...)
		present := func() (count int) {
			for _, contract := range contracts {
				if rows, _ := countHorizonTestRows(t, ctx, contract.contractId); rows[0] == 1 {
					count += 1
				}
			}
			return
		}
		settings := horizonTestSettings()
		settings.BatchSize = 10
		settings.MinBatchSize = 10

		setHorizonTestControl(t, ctx, map[string]string{horizonTestDaysField: "45"})
		observeHorizonTestBatches(t, func(batch int, last server.Id) {
			if batch == 1 {
				setHorizonTestControl(t, ctx, map[string]string{horizonTestDaysField: "91"})
			}
		})
		report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || present() != 20 || report.Deleted["transfer_contract"] != 10 || !report.CaughtUp || report.Stop != contractHorizonStopHorizon ||
			!report.HorizonTime.Equal(now.Add(-91*day)) || next.ContractId != contracts[9].contractId {
			t.Fatalf("raising the horizon did not stop the pass before its next batch: %+v %v, %d present", report, err, present())
		}

		observeHorizonTestBatches(t, nil)
		setHorizonTestControl(t, ctx, map[string]string{horizonTestDaysField: "45"})
		report, _, err = RemoveContractsPastHorizon(ctx, now, next, settings)
		if err != nil || present() != 0 || report.Scanned != 20 || !report.HorizonTime.Equal(now.Add(-45*day)) {
			t.Fatalf("lowering the horizon again did not resume at the cursor: %+v %v, %d present", report, err, present())
		}
	})
}

// At the 35-day floor every protection the reaper applies still holds. In a
// cohort minted 60 days ago, past the floor and inside 91 days, a contract
// keeps every row when it is open, disputed or unsettled; when a sweep of it is
// held by a pending or externally attempted payment, of a missing payment, or
// paid too recently; when it closed inside the horizon; or when another
// transaction holds it or its sweep. The earning cutoff falls 70 days back, as
// production's falls inside 91 days. The rest goes, so the horizon was 35
// days: a paid contract, one whose completed payment still carries a former
// release's retention stamping flag, a canceled one, an unpaid one past the
// cutoff, whose provider earns through the subnet pool, a paid one before the
// cutoff, one of a safely canceled payment, and one minted before the cutoff,
// 80 days ago, with an unpaid legacy sweep. USDC payouts ended, so the last two
// are forfeited (owner decision 2026-11-13). Once the other transaction ends,
// the held contracts go too, and a fresh pass deletes nothing more.
func TestContractHorizonFloorKeepsProtectedContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		const day = 24 * time.Hour
		// the storage precision, so archived times compare exactly
		now := server.NowUtc().Truncate(time.Microsecond)
		usePayoutTransitionAt(t, now.Add(-70*day).Truncate(time.Second))
		f := newNetEscrowOrderingTestFixture(t, ctx)
		at := func(i int) time.Time { return now.Add(-60*day + time.Duration(i)*time.Second) }
		legacyAt := func(i int) time.Time { return now.Add(-80*day + time.Duration(i)*time.Second) }

		completedOldPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-30 * day))})
		completedRecentPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-2 * day))})
		retentionPendingPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-30 * day)), retentionPending: true})
		pendingPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{})
		canceledRecordedPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			canceled: true, paymentRecord: horizonTestPointer("synthetic-record")})
		canceledSafePaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{canceled: true})
		missingPaymentId := server.NewId()

		paid := newHorizonTestContract(at(0), "settled")
		paid.paymentId = &completedOldPaymentId
		canceled := newHorizonTestContract(at(1), "canceled")
		unpaidAfterCutoff := newHorizonTestContract(at(2), "dispute_resolved_to_destination")
		paidLegacy := newHorizonTestContract(legacyAt(0), "settled")
		paidLegacy.paymentId = &completedOldPaymentId
		retentionPending := newHorizonTestContract(at(7), "settled")
		retentionPending.paymentId = &retentionPendingPaymentId
		eligible := []*horizonTestContract{paid, canceled, unpaidAfterCutoff, paidLegacy, retentionPending}

		open := newHorizonTestContract(at(3), "")
		open.closeTime = nil
		open.settled = false
		disputed := newHorizonTestContract(at(4), "")
		disputed.dispute = true
		unsettled := newHorizonTestContract(at(5), "settled")
		unsettled.settled = false
		unsettled.paymentId = &completedOldPaymentId
		pending := newHorizonTestContract(at(6), "settled")
		pending.paymentId = &pendingPaymentId
		canceledRecorded := newHorizonTestContract(at(8), "settled")
		canceledRecorded.paymentId = &canceledRecordedPaymentId
		canceledSafe := newHorizonTestContract(at(9), "settled")
		canceledSafe.paymentId = &canceledSafePaymentId
		missingPayment := newHorizonTestContract(at(10), "settled")
		missingPayment.paymentId = &missingPaymentId
		recentPayment := newHorizonTestContract(at(11), "settled")
		recentPayment.paymentId = &completedRecentPaymentId
		recentClose := newHorizonTestContract(at(13), "settled")
		recentClose.closeTime = horizonTestPointer(now.Add(-10 * day))
		recentClose.paymentId = &completedOldPaymentId
		heldContract := newHorizonTestContract(at(14), "canceled")
		heldSweep := newHorizonTestContract(at(15), "settled")
		heldSweep.paymentId = &completedOldPaymentId
		unpaidLegacy := newHorizonTestContract(legacyAt(1), "settled")
		eligible = append(eligible, canceledSafe, unpaidLegacy)
		protected := map[string][]*horizonTestContract{
			contractHorizonProtectedOpen:              {open},
			contractHorizonProtectedUnresolved:        {disputed},
			contractHorizonProtectedUnsettled:         {unsettled},
			contractHorizonProtectedPaymentHold:       {pending, canceledRecorded},
			contractHorizonProtectedPaymentIncomplete: {missingPayment},
			contractHorizonProtectedPaymentRecent:     {recentPayment},
			contractHorizonProtectedRecentClose:       {recentClose},
			contractHorizonProtectedBusy:              {heldContract, heldSweep},
		}
		young := newHorizonTestContract(now.Add(-30*day), "canceled")
		all := append([]*horizonTestContract{young}, eligible...)
		for _, contracts := range protected {
			all = append(all, contracts...)
		}
		insertHorizonTestContracts(t, ctx, f, all...)
		before := map[server.Id][6]int{}
		for _, contract := range all {
			before[contract.contractId], _ = countHorizonTestRows(t, ctx, contract.contractId)
		}

		// another transaction holds one contract, and the sweep of another, as
		// a closer and a settlement do
		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		held, err := holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer held.Rollback(context.WithoutCancel(ctx))
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, heldContract.contractId))
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1 FOR UPDATE`, heldSweep.contractId))

		setHorizonTestControl(t, ctx, map[string]string{horizonTestDaysField: "35"})
		settings := horizonTestSettings()
		settings.LockTimeout = 200 * time.Millisecond
		report, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil {
			t.Fatal(err)
		}
		for _, contract := range eligible {
			rows, archived := countHorizonTestRows(t, ctx, contract.contractId)
			wantArchived := 1
			if contract.outcome == "canceled" {
				wantArchived = 0
			}
			if rows != [6]int{} || archived != wantArchived {
				t.Fatalf("a 35-day horizon kept an eligible %s contract minted %s ago: rows %v (contract, close, escrow, sweep, extender, report), archived %d, horizon %s",
					contract.outcome, now.Sub(contract.createTime), rows, archived, now.Sub(report.HorizonTime))
			}
		}
		for reason, contracts := range protected {
			for _, contract := range contracts {
				if rows, archived := countHorizonTestRows(t, ctx, contract.contractId); rows != before[contract.contractId] || archived != 0 {
					t.Fatalf("a 35-day horizon changed a %s contract: %v, was %v, archived %d", reason, rows, before[contract.contractId], archived)
				}
			}
			if report.Protected[reason] != int64(len(contracts)) {
				t.Errorf("protected %s = %d, want %d", reason, report.Protected[reason], len(contracts))
			}
		}
		if rows, _ := countHorizonTestRows(t, ctx, young.contractId); rows != before[young.contractId] {
			t.Fatal("a contract minted inside the 35-day horizon changed", rows)
		}
		wantDeleted := map[string]int64{
			"transfer_contract":     7,
			"contract_close":        14,
			"transfer_escrow":       7,
			"transfer_escrow_sweep": 6,
			"contract_extender":     7,
			"contract_close_report": 14,
		}
		for table, count := range wantDeleted {
			if report.Deleted[table] != count {
				t.Errorf("deleted %s = %d, want %d", table, report.Deleted[table], count)
			}
		}
		if !report.HorizonTime.Equal(now.Add(-35*day)) || report.Scanned != int64(len(all)-1) || report.Eligible != 7 || report.Archived != 6 ||
			!report.CaughtUp || report.Stop != contractHorizonStopHorizon || report.Mode != ContractHorizonModeOn {
			t.Fatalf("wrong report: %+v", report)
		}

		server.Raise(held.Rollback(ctx))
		report, _, err = RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || report.Eligible != 2 || report.Archived != 1 || report.Protected[contractHorizonProtectedBusy] != 0 || report.Scanned != int64(len(all)-1-7) {
			t.Fatalf("a later pass did not take the released contracts: %+v %v", report, err)
		}
		for _, contract := range []*horizonTestContract{heldContract, heldSweep} {
			if rows, _ := countHorizonTestRows(t, ctx, contract.contractId); rows != [6]int{} {
				t.Fatal("a released contract was kept", rows)
			}
		}
		report, _, err = RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || report.Eligible != 0 || report.Deleted["transfer_contract"] != 0 || report.Scanned != int64(len(all)-1-9) {
			t.Fatalf("a fresh pass deleted again or missed protected contracts: %+v %v", report, err)
		}
	})
}

// Marks the operator block awarded, as CommitOperatorBlockPoints records it.
func awardHorizonTestBlock(t testing.TB, ctx context.Context, block int64) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO payment_report
			(payment_plan_id,total_payout_nano_cents,total_nano_points,point_scale_factor,time_scale_factor,payout_points_per_payout)
			VALUES($1,0,0,0,0,0)`, OperatorBlockPointsPlanId(block)))
	}, server.TxReadCommitted, server.OptNoRetry())
}

// Operator block points read a block's unpaid sweeps by settlement time until
// the block is awarded or its reliability passes retention, and a contract
// can settle long after it closed: a dispute resolved late, say. Among
// contracts minted and closed 40 days ago, past the 35-day floor, the reaper
// keeps the ones settled with an unpaid sweep in the open block and in the
// last completed block, which has no award yet. It deletes the ones settled in
// an awarded block, in a block whose reliability is past retention (an award
// of it fails closed), and at their close, and one settled in the open block
// whose sweep is paid. Once the completed block is awarded, its contract goes
// too. The reason is "block_points", as the protected counter labels it.
func TestContractHorizonKeepsSweepsOperatorBlockPointsStillRead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		const day = 24 * time.Hour
		// the storage precision, so sweep times compare exactly
		now := server.NowUtc().Truncate(time.Microsecond)
		cutoff := now.Add(-70 * day).Truncate(time.Second)
		usePayoutTransitionAt(t, cutoff)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		completedOldPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-30 * day))})

		openBlock := pointsBlockOf(now)
		blockStart := func(block int64) time.Time {
			start, _, ok := OperatorBlockPointsWindow(block, cutoff)
			if !ok {
				t.Fatal("block has no traffic past the cutoff", block)
			}
			return start
		}
		minted := now.Add(-40 * day)
		// minted and closed 40 days ago, settled at sweepTime with an unpaid
		// sweep; nil settles at the close
		settled := func(sweepTime *time.Time) *horizonTestContract {
			contract := newHorizonTestContract(minted, "settled")
			contract.sweepTime = sweepTime
			minted = minted.Add(time.Second)
			return contract
		}
		inOpen := settled(horizonTestPointer(blockStart(openBlock).Add(now.Sub(blockStart(openBlock)) / 2)))
		inLast := settled(horizonTestPointer(blockStart(openBlock - 1).Add(time.Hour)))
		inAwarded := settled(horizonTestPointer(blockStart(openBlock - 2).Add(time.Hour)))
		// its window opened more than 28 days ago, past client_reliability
		// retention
		inExpired := settled(horizonTestPointer(blockStart(openBlock - 4).Add(time.Hour)))
		atClose := settled(nil)
		paidInOpen := settled(inOpen.sweepTime)
		paidInOpen.paymentId = &completedOldPaymentId
		all := []*horizonTestContract{inOpen, inLast, inAwarded, inExpired, atClose, paidInOpen}
		insertHorizonTestContracts(t, ctx, f, all...)
		awardHorizonTestBlock(t, ctx, openBlock-2)
		before := map[server.Id][6]int{}
		for _, contract := range all {
			before[contract.contractId], _ = countHorizonTestRows(t, ctx, contract.contractId)
		}

		setHorizonTestControl(t, ctx, map[string]string{horizonTestDaysField: "35"})
		report, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, horizonTestSettings())
		if err != nil {
			t.Fatal(err)
		}
		for name, contract := range map[string]*horizonTestContract{
			"settled in an awarded block":       inAwarded,
			"settled in a block past retention": inExpired,
			"settled at its close":              atClose,
			"paid, settled in the open block":   paidInOpen,
		} {
			if rows, archived := countHorizonTestRows(t, ctx, contract.contractId); rows != [6]int{} || archived != 1 {
				t.Fatalf("a 35-day horizon kept a contract %s that block points no longer read: rows %v, archived %d, horizon %s, %+v",
					name, rows, archived, now.Sub(report.HorizonTime), report)
			}
		}
		for name, contract := range map[string]*horizonTestContract{
			"settled in the open block":                  inOpen,
			"settled in the last block, not awarded yet": inLast,
		} {
			if rows, archived := countHorizonTestRows(t, ctx, contract.contractId); rows != before[contract.contractId] || archived != 0 {
				t.Fatalf("a 35-day horizon removed a contract %s, whose unpaid sweep block points still read: rows %v, was %v, archived %d",
					name, rows, before[contract.contractId], archived)
			}
		}
		if report.Scanned != 6 || report.Eligible != 4 || report.Archived != 4 || report.Protected["block_points"] != 2 {
			t.Fatalf("wrong report: %+v", report)
		}

		awardHorizonTestBlock(t, ctx, openBlock-1)
		report, _, err = RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, horizonTestSettings())
		if err != nil {
			t.Fatal(err)
		}
		if rows, _ := countHorizonTestRows(t, ctx, inLast.contractId); rows != [6]int{} {
			t.Fatal("the contract of a block awarded since was kept", rows)
		}
		if rows, _ := countHorizonTestRows(t, ctx, inOpen.contractId); rows != before[inOpen.contractId] {
			t.Fatal("the open block's contract was removed", rows)
		}
		if report.Scanned != 2 || report.Eligible != 1 || report.Protected["block_points"] != 1 {
			t.Fatalf("wrong report after the award: %+v", report)
		}
	})
}

// A payment that completed under attribution review queues no retention
// stamping for its contracts (completePaymentInTx), so the payment completion
// path keeps them until the straggler horizon while the review is open. Past
// the horizon the reaper keeps a contract that payment paid, and still deletes
// one paid as long ago by an ordinary payment.
func TestContractHorizonKeepsContractsUnderAttributionReview(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		const day = 24 * time.Hour
		now := server.NowUtc().Truncate(time.Microsecond)
		usePayoutTransitionAt(t, now.Add(-70*day).Truncate(time.Second))
		f := newNetEscrowOrderingTestFixture(t, ctx)
		reviewedPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-30 * day)), attributionReview: true})
		paidPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-30 * day))})
		underReview := newHorizonTestContract(now.Add(-60*day), "settled")
		underReview.paymentId = &reviewedPaymentId
		paid := newHorizonTestContract(now.Add(-60*day+time.Second), "settled")
		paid.paymentId = &paidPaymentId
		insertHorizonTestContracts(t, ctx, f, underReview, paid)
		before, _ := countHorizonTestRows(t, ctx, underReview.contractId)

		setHorizonTestControl(t, ctx, map[string]string{horizonTestDaysField: "35"})
		report, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, horizonTestSettings())
		if err != nil {
			t.Fatal(err)
		}
		if rows, archived := countHorizonTestRows(t, ctx, underReview.contractId); rows != before || archived != 0 {
			t.Fatalf("a contract paid under attribution review was removed: rows %v, was %v, archived %d, %+v", rows, before, archived, report)
		}
		if rows, archived := countHorizonTestRows(t, ctx, paid.contractId); rows != [6]int{} || archived != 1 {
			t.Fatalf("a contract paid as long ago without review was kept: rows %v, archived %d", rows, archived)
		}
		if report.Protected["payment_review"] != 1 || report.Eligible != 1 {
			t.Fatalf("wrong report: %+v", report)
		}
	})
}
