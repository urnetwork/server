// The contract horizon reaper at the horizon the runtime control's horizonDays
// sets, down to the 45-day floor: what the shorter horizon reaches, exactly
// where it stops, that the control applies from the next batch, and that every
// protection still holds there.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

// The control field as an operator sets it:
// HSET {contract_horizon}control horizonDays 45
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

// horizonDays shortens the horizon to as few as 45 days. At 45 days a contract
// 60 days old goes, into the archive since it bears credit, and contracts 44
// and 40 days old stay. The boundary is exactly 45 days, for the mint time in
// the id and for the close time, and a value below the floor reaches no
// closer. Without the field, or with one that does not parse or is past 91
// days, the horizon is the code's 91 days. Each run reports and exports the
// horizon it applied.
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
		floor := now.Add(-45 * day)

		// credit-bearing and unswept, so nothing but the horizon keeps it
		aged60 := newHorizonTestContract(now.Add(-60*day), "settled")
		aged60.unswept = true
		aged44 := newHorizonTestContract(now.Add(-44*day), "canceled")
		aged40 := newHorizonTestContract(now.Add(-40*day), "canceled")
		// closed before it was minted, so only the id decides
		mintBoundary := newHorizonTestContract(floor, "canceled")
		mintBoundary.closeTime = horizonTestPointer(floor.Add(-time.Hour))
		closeBoundary := newHorizonTestContract(now.Add(-50*day), "canceled")
		closeBoundary.closeTime = horizonTestPointer(floor)
		all := []*horizonTestContract{aged60, aged44, aged40, mintBoundary, closeBoundary}
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

		for _, horizonDays := range []string{"", "soon", "45.5", "400"} {
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
		if !present(aged44) || !present(aged40) || !present(mintBoundary) || !present(closeBoundary) {
			t.Fatal("a 45-day horizon removed a contract on it or inside it")
		}
		if !report.HorizonTime.Equal(floor) || report.Scanned != 2 || report.Eligible != 1 || report.Archived != 1 ||
			report.Protected[contractHorizonProtectedRecentClose] != 1 || report.Deleted["transfer_contract"] != 1 {
			t.Fatalf("wrong report at 45 days: %+v", report)
		}

		// a microsecond later the close boundary is past; the mint boundary's
		// millisecond is not
		if run(now.Add(time.Microsecond), "45"); present(closeBoundary) || !present(mintBoundary) {
			t.Fatal("the 45-day horizon is not exact for the close time")
		}
		// a millisecond later the mint boundary is past, and a horizon below the
		// floor still keeps the contract 44 days old
		report = run(now.Add(time.Millisecond), "1")
		if present(mintBoundary) || !present(aged44) || !present(aged40) || !report.HorizonTime.Equal(floor.Add(time.Millisecond)) {
			t.Fatalf("a horizon below the floor is not exactly 45 days for the mint time: %s, %+v", now.Add(time.Millisecond).Sub(report.HorizonTime), report)
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

// At the 45-day floor every protection the reaper applies still holds. In a
// cohort minted 60 days ago, past the floor and inside 91 days, a contract
// keeps every row when it is open, disputed or unsettled; when a sweep of it is
// held by a pending, retention-stamping or externally attempted payment,
// re-plannable through a safely canceled or missing payment, paid too
// recently, or staged in a pending plan; when it closed inside the horizon; or
// when another transaction holds it or its sweep. The earning cutoff falls 70
// days back, as production's falls inside 91 days, and a contract minted
// before it, 80 days ago, keeps its unpaid sweep for the legacy planner. The
// rest goes, so the horizon was 45 days: a paid contract, a canceled one, an
// unpaid one past the cutoff, whose provider earns through the subnet pool,
// and a paid one before the cutoff. Once the other transaction ends, the held
// contracts go too, and a fresh pass deletes nothing more.
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
		eligible := []*horizonTestContract{paid, canceled, unpaidAfterCutoff, paidLegacy}

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
		retentionPending := newHorizonTestContract(at(7), "settled")
		retentionPending.paymentId = &retentionPendingPaymentId
		canceledRecorded := newHorizonTestContract(at(8), "settled")
		canceledRecorded.paymentId = &canceledRecordedPaymentId
		canceledSafe := newHorizonTestContract(at(9), "settled")
		canceledSafe.paymentId = &canceledSafePaymentId
		missingPayment := newHorizonTestContract(at(10), "settled")
		missingPayment.paymentId = &missingPaymentId
		recentPayment := newHorizonTestContract(at(11), "settled")
		recentPayment.paymentId = &completedRecentPaymentId
		staged := newHorizonTestContract(at(12), "settled")
		recentClose := newHorizonTestContract(at(13), "settled")
		recentClose.closeTime = horizonTestPointer(now.Add(-10 * day))
		recentClose.paymentId = &completedOldPaymentId
		heldContract := newHorizonTestContract(at(14), "canceled")
		heldSweep := newHorizonTestContract(at(15), "settled")
		heldSweep.paymentId = &completedOldPaymentId
		unpaidLegacy := newHorizonTestContract(legacyAt(1), "settled")
		protected := map[string][]*horizonTestContract{
			contractHorizonProtectedOpen:              {open},
			contractHorizonProtectedUnresolved:        {disputed},
			contractHorizonProtectedUnsettled:         {unsettled},
			contractHorizonProtectedPaymentHold:       {pending, retentionPending, canceledRecorded},
			contractHorizonProtectedPaymentIncomplete: {canceledSafe, missingPayment},
			contractHorizonProtectedPaymentRecent:     {recentPayment},
			contractHorizonProtectedPlanStaged:        {staged},
			contractHorizonProtectedRecentClose:       {recentClose},
			contractHorizonProtectedUnpaidLegacy:      {unpaidLegacy},
			contractHorizonProtectedBusy:              {heldContract, heldSweep},
		}
		young := newHorizonTestContract(now.Add(-40*day), "canceled")
		all := append([]*horizonTestContract{young}, eligible...)
		for _, contracts := range protected {
			all = append(all, contracts...)
		}
		insertHorizonTestContracts(t, ctx, f, all...)
		server.Tx(ctx, func(tx server.PgTx) {
			paymentPlanId := server.NewId()
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO payment_plan_assignment(payment_plan_id,create_time,sweep_count)
				VALUES($1,$2,1)`, paymentPlanId, now))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO payment_plan_sweep
				(contract_id,balance_id,network_id,payment_plan_id,previous_payment_id,payment_id) VALUES($1,$2,$3,$4,NULL,$5)`,
				staged.contractId, f.balanceId, f.destinationNetworkId, paymentPlanId, pendingPaymentId))
		}, server.TxReadCommitted, server.OptNoRetry())
		before := map[server.Id][6]int{}
		for _, contract := range all {
			before[contract.contractId], _ = countHorizonTestRows(t, ctx, contract.contractId)
		}

		// another transaction holds one contract, and the sweep of another, as
		// a closer and the payout planner do
		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		held, err := holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer held.Rollback(context.WithoutCancel(ctx))
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, heldContract.contractId))
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1 FOR UPDATE`, heldSweep.contractId))

		setHorizonTestControl(t, ctx, map[string]string{horizonTestDaysField: "45"})
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
				t.Fatalf("a 45-day horizon kept an eligible %s contract minted %s ago: rows %v (contract, close, escrow, sweep, extender, report), archived %d, horizon %s",
					contract.outcome, now.Sub(contract.createTime), rows, archived, now.Sub(report.HorizonTime))
			}
		}
		for reason, contracts := range protected {
			for _, contract := range contracts {
				if rows, archived := countHorizonTestRows(t, ctx, contract.contractId); rows != before[contract.contractId] || archived != 0 {
					t.Fatalf("a 45-day horizon changed a %s contract: %v, was %v, archived %d", reason, rows, before[contract.contractId], archived)
				}
			}
			if report.Protected[reason] != int64(len(contracts)) {
				t.Errorf("protected %s = %d, want %d", reason, report.Protected[reason], len(contracts))
			}
		}
		if rows, _ := countHorizonTestRows(t, ctx, young.contractId); rows != before[young.contractId] {
			t.Fatal("a contract minted inside the 45-day horizon changed", rows)
		}
		wantDeleted := map[string]int64{
			"transfer_contract":     4,
			"contract_close":        8,
			"transfer_escrow":       4,
			"transfer_escrow_sweep": 3,
			"contract_extender":     4,
			"contract_close_report": 8,
		}
		for table, count := range wantDeleted {
			if report.Deleted[table] != count {
				t.Errorf("deleted %s = %d, want %d", table, report.Deleted[table], count)
			}
		}
		if !report.HorizonTime.Equal(now.Add(-45*day)) || report.Scanned != int64(len(all)-1) || report.Eligible != 4 || report.Archived != 3 ||
			!report.CaughtUp || report.Stop != contractHorizonStopHorizon || report.Mode != ContractHorizonModeOn {
			t.Fatalf("wrong report: %+v", report)
		}

		server.Raise(held.Rollback(ctx))
		report, _, err = RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || report.Eligible != 2 || report.Archived != 1 || report.Protected[contractHorizonProtectedBusy] != 0 || report.Scanned != int64(len(all)-1-4) {
			t.Fatalf("a later pass did not take the released contracts: %+v %v", report, err)
		}
		for _, contract := range []*horizonTestContract{heldContract, heldSweep} {
			if rows, _ := countHorizonTestRows(t, ctx, contract.contractId); rows != [6]int{} {
				t.Fatal("a released contract was kept", rows)
			}
		}
		report, _, err = RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || report.Eligible != 0 || report.Deleted["transfer_contract"] != 0 || report.Scanned != int64(len(all)-1-6) {
			t.Fatalf("a fresh pass deleted again or missed protected contracts: %+v %v", report, err)
		}
	})
}
