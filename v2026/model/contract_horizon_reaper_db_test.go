// The contract horizon reaper against the migrated schema: it deletes exactly
// the contracts past the 91-day horizon that every check admits, with all of
// their dependent rows, in bounded committed batches that resume from their
// cursor, and its runtime control stops it.
package model

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server/v2026"
)

// One fixture contract. Every one gets two closes, an escrow, an extender
// party and two close reports; a sweep unless unswept.
type horizonTestContract struct {
	contractId server.Id
	createTime time.Time
	closeTime  *time.Time
	// "" is a NULL outcome
	outcome string
	dispute bool
	settled bool
	unswept bool
	// the sweep's payment; nil is unpaid
	paymentId *server.Id
	// the sweep's settlement; nil settles at the close, or at creation for a
	// contract without one
	sweepTime *time.Time
}

// A contract id minted at the millisecond of at, with a random nonzero tail.
func horizonTestContractId(at time.Time) server.Id {
	floor := server.ContractIdFloor(at)
	for {
		id := server.NewId()
		copy(id[:6], floor[:6])
		if id != floor {
			return id
		}
	}
}

// A resolved, settled contract minted at createTime and closed an hour later.
func newHorizonTestContract(createTime time.Time, outcome string) *horizonTestContract {
	closeTime := createTime.Add(time.Hour)
	return &horizonTestContract{
		contractId: horizonTestContractId(createTime),
		createTime: createTime,
		closeTime:  &closeTime,
		outcome:    outcome,
		settled:    true,
		unswept:    outcome == "canceled",
	}
}

func insertHorizonTestContracts(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, contracts ...*horizonTestContract) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, contract := range contracts {
			var outcome *string
			var usage *contractUsageSnapshot
			if contract.outcome != "" {
				outcome = &contract.outcome
				if contract.outcome != "canceled" {
					usage = &contractUsageSnapshot{Version: 1, ByteCount: 1024, Providers: []contractProviderUsage{
						{ClientId: f.destinationId, NetworkId: f.destinationNetworkId, ByteCount: 1024},
					}}
				}
			}
			var closeTime *time.Time
			if contract.closeTime != nil {
				utc := contract.closeTime.UTC()
				closeTime = &utc
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,
				 transfer_byte_count,create_time,close_time,outcome,dispute,provider_usage,usage_origin_is_source)
				VALUES($1,$2,$3,$4,$5,$2,1024,$6,$7,$8,$9,$10,true)`,
				contract.contractId, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId,
				contract.createTime.UTC(), closeTime, outcome, contract.dispute, usage))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count)
				VALUES($1,'source',0),($1,'destination',0)`, contract.contractId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count,settled)
				VALUES($1,$2,0,$3)`, contract.contractId, f.balanceId, contract.settled))
			if !contract.unswept {
				sweepTime := contract.createTime.UTC()
				if contract.sweepTime != nil {
					sweepTime = contract.sweepTime.UTC()
				} else if closeTime != nil {
					sweepTime = *closeTime
				}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
					(contract_id,balance_id,network_id,payout_byte_count,payout_net_revenue_nano_cents,payment_id,sweep_time)
					VALUES($1,$2,$3,0,0,$4,$5)`, contract.contractId, f.balanceId, f.destinationNetworkId, contract.paymentId, sweepTime))
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_extender(contract_id,extender_id,party,client_id,network_id)
				VALUES($1,$2,'source',$3,$4)`, contract.contractId, server.NewId(), f.sourceId, f.sourceNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close_report
				(contract_id,party,report_id,used_transfer_byte_count,checkpoint)
				VALUES($1,'source',$2,0,true),($1,'destination',$3,0,false)`, contract.contractId, server.NewId(), server.NewId()))
		}
	}, server.TxReadCommitted, server.OptNoRetry())
}

// The payment states a sweep can belong to.
type horizonTestPayment struct {
	completed          bool
	completeTime       *time.Time
	canceled           bool
	retentionPending   bool
	paymentRecord      *string
	circleIdempotentId *server.Id
	// completed under attribution review, which completePaymentInTx leaves
	// without retention pending
	attributionReview bool
}

func insertHorizonTestPayment(t testing.TB, ctx context.Context, networkId server.Id, payment horizonTestPayment) server.Id {
	t.Helper()
	paymentId := server.NewId()
	var completeTime *time.Time
	if payment.completeTime != nil {
		utc := payment.completeTime.UTC()
		completeTime = &utc
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment
			(payment_id,payment_plan_id,network_id,payout_byte_count,payout_nano_cents,subsidy_payout_nano_cents,min_sweep_time,
			 completed,complete_time,canceled,contract_retention_pending,payment_record,circle_idempotency_key,attribution_review_required)
			VALUES($1,$2,$3,0,0,0,$4,$5,$6,$7,$8,$9,$10,$11)`,
			paymentId, server.NewId(), networkId, server.NowUtc(),
			payment.completed, completeTime, payment.canceled, payment.retentionPending, payment.paymentRecord, payment.circleIdempotentId,
			payment.attributionReview))
	}, server.TxReadCommitted, server.OptNoRetry())
	return paymentId
}

func horizonTestPointer[T any](value T) *T {
	return &value
}

// Rows of one contract (contract, close, escrow, sweep, extender party, close
// report) and its archived usage.
func countHorizonTestRows(t testing.TB, ctx context.Context, contractId server.Id) (rows [6]int, archived int) {
	t.Helper()
	rows = countReaperDependentTestRows(t, ctx, contractId)
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM st_provider_usage_archive WHERE contract_id=$1`, contractId).Scan(&archived))
	})
	return
}

// Settings for a test: on without the control, unpaced, rescanning only when
// a test asks.
func horizonTestSettings() *ContractHorizonSettings {
	settings := DefaultContractHorizonSettings()
	settings.DefaultMode = ContractHorizonModeOn
	settings.DefaultMaxRowsPerSecond = 1000000
	settings.MaxRowsPerSecond = 1000000
	settings.RescanInterval = 365 * 24 * time.Hour
	return settings
}

func removeHorizonTestControl(t testing.TB, ctx context.Context) {
	t.Helper()
	controlCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	server.Raise(RemoveContractHorizonControl(controlCtx))
}

func setHorizonTestControl(t testing.TB, ctx context.Context, fields map[string]string) {
	t.Helper()
	controlCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	server.Raise(SetContractHorizonControl(controlCtx, fields))
}

// Installs the batch observer for one test.
func observeHorizonTestBatches(t testing.TB, observe func(batch int, last server.Id)) {
	t.Helper()
	contractHorizonBatchCommitted = observe
	t.Cleanup(func() {
		contractHorizonBatchCommitted = nil
	})
}

// Every contract past the horizon that each check admits goes with all of its
// dependent rows, and a credit-bearing one leaves its usage in the archive,
// including one of a completed payment whose retention stamping a former
// release queued: the flag holds nothing since USDC completions ended. A
// contract of a safely canceled payment and one unpaid before the earning
// cutoff go too: USDC payouts ended, so both are forfeited (owner decision
// 2026-11-13); before, each was kept. Every other contract keeps every row:
// open, disputed, unsettled, held by a pending or externally attempted
// payment, of a missing payment, paid too recently, closed inside the horizon,
// or minted inside it. A second run finds nothing.
func TestContractHorizonRemovesOnlyEligibleContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		usePayoutTransition(t)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		// the storage precision, so archived times compare exactly
		now := server.NowUtc().Truncate(time.Microsecond)
		old := now.Add(-120 * 24 * time.Hour)
		at := func(i int) time.Time { return old.Add(time.Duration(i) * time.Second) }

		completedOldPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-30 * 24 * time.Hour))})
		completedRecentPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-2 * 24 * time.Hour))})
		retentionPendingPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-30 * 24 * time.Hour)), retentionPending: true})
		pendingPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{})
		canceledRecordedPaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			canceled: true, paymentRecord: horizonTestPointer("synthetic-record")})
		canceledSafePaymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{canceled: true})
		missingPaymentId := server.NewId()

		paid := newHorizonTestContract(at(0), "settled")
		paid.paymentId = &completedOldPaymentId
		canceled := newHorizonTestContract(at(1), "canceled")
		unpaidAfterCutoff := newHorizonTestContract(at(2), "dispute_resolved_to_destination")
		retentionPending := newHorizonTestContract(at(7), "settled")
		retentionPending.paymentId = &retentionPendingPaymentId
		eligible := []*horizonTestContract{paid, canceled, unpaidAfterCutoff, retentionPending}

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
		recentClose.closeTime = horizonTestPointer(now.Add(-10 * 24 * time.Hour))
		recentClose.paymentId = &completedOldPaymentId
		// minted and closed before the synthetic cutoff, never paid
		unpaidLegacy := newHorizonTestContract(payoutTestCutoff.Add(-7*24*time.Hour), "settled")
		eligible = append(eligible, canceledSafe, unpaidLegacy)
		young := newHorizonTestContract(now.Add(-30*24*time.Hour), "canceled")
		protected := map[string][]*horizonTestContract{
			contractHorizonProtectedOpen:              {open},
			contractHorizonProtectedUnresolved:        {disputed},
			contractHorizonProtectedUnsettled:         {unsettled},
			contractHorizonProtectedPaymentHold:       {pending, canceledRecorded},
			contractHorizonProtectedPaymentIncomplete: {missingPayment},
			contractHorizonProtectedPaymentRecent:     {recentPayment},
			contractHorizonProtectedRecentClose:       {recentClose},
		}
		all := append(slices.Clone(eligible), young)
		for _, contracts := range protected {
			all = append(all, contracts...)
		}
		insertHorizonTestContracts(t, ctx, f, all...)
		before := map[server.Id][6]int{}
		for _, contract := range all {
			before[contract.contractId], _ = countHorizonTestRows(t, ctx, contract.contractId)
		}

		report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, horizonTestSettings())
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
				t.Fatalf("eligible %s contract kept rows %v (contract, close, escrow, sweep, extender, report) or archived %d", contract.outcome, rows, archived)
			}
		}
		for reason, contracts := range protected {
			for _, contract := range contracts {
				if rows, archived := countHorizonTestRows(t, ctx, contract.contractId); rows != before[contract.contractId] || archived != 0 {
					t.Fatalf("%s contract changed: %v, was %v, archived %d", reason, rows, before[contract.contractId], archived)
				}
			}
			if report.Protected[reason] != int64(len(contracts)) {
				t.Errorf("protected %s = %d, want %d", reason, report.Protected[reason], len(contracts))
			}
		}
		if rows, _ := countHorizonTestRows(t, ctx, young.contractId); rows != before[young.contractId] {
			t.Fatal("a contract minted inside the horizon changed", rows)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var outcome string
			var closeTime time.Time
			var usage []byte
			server.Raise(conn.QueryRow(ctx, `SELECT outcome,close_time,provider_usage FROM st_provider_usage_archive WHERE contract_id=$1`,
				paid.contractId).Scan(&outcome, &closeTime, &usage))
			snapshot, err := decodeContractUsageSnapshot(usage)
			if err != nil || outcome != "settled" || !closeTime.Equal(*paid.closeTime) || snapshot.ByteCount != 1024 {
				t.Fatal("archive does not hold the deleted contract's exact usage", outcome, closeTime, snapshot, err)
			}
		})
		wantDeleted := map[string]int64{
			"transfer_contract":     6,
			"contract_close":        12,
			"transfer_escrow":       6,
			"transfer_escrow_sweep": 5,
			"contract_extender":     6,
			"contract_close_report": 12,
		}
		if report.Scanned != int64(len(all)-1) || report.Eligible != 6 || report.Archived != 5 || report.Protected[contractHorizonProtectedBusy] != 0 ||
			!report.CaughtUp || report.Stop != contractHorizonStopHorizon || report.Mode != ContractHorizonModeOn {
			t.Fatalf("wrong report: %+v", report)
		}
		for table, count := range wantDeleted {
			if report.Deleted[table] != count {
				t.Errorf("deleted %s = %d, want %d", table, report.Deleted[table], count)
			}
		}
		if next.ContractId != recentClose.contractId || next.PassStartTime.IsZero() {
			t.Fatal("cursor is not at the last contract the pass read", next)
		}

		again, againNext, err := RemoveContractsPastHorizon(ctx, now, next, horizonTestSettings())
		if err != nil || again.Scanned != 0 || again.Eligible != 0 || !again.CaughtUp || againNext.ContractId != next.ContractId {
			t.Fatalf("a second run did not find the pass complete: %+v %v", again, err)
		}
		fresh, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, horizonTestSettings())
		if err != nil || fresh.Eligible != 0 || fresh.Deleted["transfer_contract"] != 0 || fresh.Scanned != report.Scanned-6 {
			t.Fatalf("a fresh pass deleted again or missed protected contracts: %+v %v", fresh, err)
		}
	})
}

// Without an earning cutoff no operator block can be awarded, so a contract
// past the horizon with an unpaid sweep goes, as an unswept one does. USDC
// payouts ended, so an unpaid sweep is forfeited (owner decision 2026-11-13);
// before, such a contract stayed as a legacy obligation.
func TestContractHorizonRemovesUnpaidSweepsWithoutCutoff(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		unpaid := newHorizonTestContract(now.Add(-120*24*time.Hour), "settled")
		unswept := newHorizonTestContract(now.Add(-119*24*time.Hour), "settled")
		unswept.unswept = true
		insertHorizonTestContracts(t, ctx, f, unpaid, unswept)

		report, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, horizonTestSettings())
		if err != nil {
			t.Fatal(err)
		}
		for _, contract := range []*horizonTestContract{unpaid, unswept} {
			if rows, archived := countHorizonTestRows(t, ctx, contract.contractId); rows != [6]int{} || archived != 1 {
				t.Fatal("a contract past the horizon was kept", rows, archived)
			}
		}
		if report.Eligible != 2 {
			t.Fatalf("wrong report: %+v", report)
		}
	})
}

// The horizon is exactly 91 days before now, for both the mint time in the
// contract id and the close time: a contract on the boundary stays, one a
// millisecond (or a microsecond of close time) past it goes. A horizon setting
// below the 45-day floor does not move the boundary inside the floor.
func TestContractHorizonBoundaryIsExact(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		mint := server.NowUtc().Add(-100 * 24 * time.Hour).Truncate(time.Millisecond)
		// closed before it was minted, so only the id decides
		mintBoundary := newHorizonTestContract(mint, "canceled")
		mintBoundary.closeTime = horizonTestPointer(mint.Add(-time.Hour))
		closed := mint.Add(-5 * 24 * time.Hour).Truncate(time.Microsecond)
		closeBoundary := newHorizonTestContract(mint.Add(-10*24*time.Hour), "canceled")
		closeBoundary.closeTime = &closed
		insertHorizonTestContracts(t, ctx, f, mintBoundary, closeBoundary)
		retention := 91 * 24 * time.Hour
		run := func(now time.Time, settings *ContractHorizonSettings) *ContractHorizonReport {
			report, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
			if err != nil {
				t.Fatal(err)
			}
			return report
		}
		present := func(contract *horizonTestContract) bool {
			rows, _ := countHorizonTestRows(t, ctx, contract.contractId)
			return rows[0] == 1
		}

		if report := run(closed.Add(retention), horizonTestSettings()); !present(closeBoundary) || report.Protected[contractHorizonProtectedRecentClose] != 1 {
			t.Fatalf("a contract closed exactly at the horizon was removed: %+v", report)
		}
		if run(closed.Add(retention).Add(time.Microsecond), horizonTestSettings()); present(closeBoundary) {
			t.Fatal("a contract closed a microsecond before the horizon was kept")
		}
		shorter := horizonTestSettings()
		shorter.Horizon = 30 * 24 * time.Hour
		if run(mint.Add(31*24*time.Hour), shorter); !present(mintBoundary) {
			t.Fatal("a 30-day horizon setting removed a contract inside the 45-day floor")
		}
		if report := run(mint.Add(retention), horizonTestSettings()); !present(mintBoundary) || report.Scanned != 0 {
			t.Fatalf("a contract minted exactly at the horizon was read or removed: %+v", report)
		}
		if run(mint.Add(retention).Add(time.Millisecond), horizonTestSettings()); present(mintBoundary) {
			t.Fatal("a contract minted a millisecond before the horizon was kept")
		}
	})
}

// No batch reads more contracts than the batch size, the control can lower
// it, and each batch commits on its own: the observer sees every batch's
// deletes from another session.
func TestContractHorizonBatchesStayBounded(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		contracts := []*horizonTestContract{}
		for i := range 34 {
			contracts = append(contracts, newHorizonTestContract(now.Add(-120*24*time.Hour+time.Duration(i)*time.Second), "canceled"))
		}
		insertHorizonTestContracts(t, ctx, f, contracts...)
		remaining := func() (count int) {
			ids := make([]server.Id, len(contracts))
			for i, contract := range contracts {
				ids[i] = contract.contractId
			}
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1)`, ids).Scan(&count))
			})
			return
		}
		settings := horizonTestSettings()
		settings.BatchSize = 10
		settings.MinBatchSize = 1
		settings.TargetBatchDuration = time.Hour
		var counts []int
		observeHorizonTestBatches(t, func(batch int, last server.Id) {
			counts = append(counts, remaining())
		})

		setHorizonTestControl(t, ctx, map[string]string{ContractHorizonControlFieldBatchSize: "25"})
		report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || !slices.Equal(counts, []int{24, 14, 4, 0}) || report.Batches != 5 || report.Deleted["transfer_contract"] != 34 {
			t.Fatalf("batches over the size of 10: remaining after each %v, report %+v, err %v", counts, report, err)
		}
		if next.BatchSize != 10 {
			t.Fatal("the carried batch size passed the code size", next.BatchSize)
		}

		more := []*horizonTestContract{}
		for i := range 9 {
			more = append(more, newHorizonTestContract(now.Add(-110*24*time.Hour+time.Duration(i)*time.Second), "canceled"))
		}
		insertHorizonTestContracts(t, ctx, f, more...)
		contracts = more
		counts = nil
		setHorizonTestControl(t, ctx, map[string]string{ContractHorizonControlFieldBatchSize: "4"})
		report, _, err = RemoveContractsPastHorizon(ctx, now, next, settings)
		if err != nil || !slices.Equal(counts, []int{5, 1, 0}) || report.Deleted["transfer_contract"] != 9 {
			t.Fatalf("batches over the control's size of 4: remaining after each %v, report %+v, err %v", counts, report, err)
		}
	})
}

var errHorizonTestCrash = errors.New("synthetic crash after a committed batch")

// A run that dies after committing batches leaves its task's cursor where the
// run started; the rerun finds the committed batches gone and finishes the
// rest. A run whose batch fails returns the cursor of its last committed
// batch, and a rerun from there finishes without reading those batches again.
func TestContractHorizonResumesAfterCrash(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		newContracts := func(start time.Time) []*horizonTestContract {
			contracts := []*horizonTestContract{}
			for i := range 30 {
				contracts = append(contracts, newHorizonTestContract(start.Add(time.Duration(i)*time.Second), "canceled"))
			}
			insertHorizonTestContracts(t, ctx, f, contracts...)
			return contracts
		}
		present := func(contracts []*horizonTestContract) (count int) {
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

		crashed := newContracts(now.Add(-130 * 24 * time.Hour))
		observeHorizonTestBatches(t, func(batch int, last server.Id) {
			if batch == 2 {
				panic(errHorizonTestCrash)
			}
		})
		if r := server.HandleError(func() {
			RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		}); r == nil || !errors.Is(r.(error), errHorizonTestCrash) {
			t.Fatal("the synthetic crash did not stop the run", r)
		}
		if present(crashed) != 10 {
			t.Fatal("the crashed run did not commit exactly its two batches", present(crashed))
		}
		observeHorizonTestBatches(t, nil)
		report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || present(crashed) != 0 || report.Scanned != 10 || report.Deleted["transfer_contract"] != 10 || !report.CaughtUp {
			t.Fatalf("the rerun did not finish the crashed pass: %+v %v", report, err)
		}

		failed := newContracts(now.Add(-125 * 24 * time.Hour))
		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		var held pgx.Tx
		observeHorizonTestBatches(t, func(batch int, last server.Id) {
			if batch == 1 {
				var err error
				held, err = holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				server.Raise(err)
				server.RaisePgResult(held.Exec(ctx, `LOCK TABLE contract_extender IN ACCESS EXCLUSIVE MODE`))
			}
		})
		settings.LockTimeout = 200 * time.Millisecond
		report, failedNext, err := RemoveContractsPastHorizon(ctx, now, next, settings)
		if held != nil {
			server.Raise(held.Rollback(ctx))
		}
		if err == nil || !server.IsLockTimeout(err) || report.Stop != contractHorizonStopError || present(failed) != 20 {
			t.Fatalf("the blocked batch did not fail alone: %+v %v, %d present", report, err, present(failed))
		}
		if failedNext.ContractId != failed[9].contractId {
			t.Fatal("the failed run did not return its last committed batch's cursor", failedNext)
		}
		observeHorizonTestBatches(t, nil)
		report, _, err = RemoveContractsPastHorizon(ctx, now, failedNext, settings)
		if err != nil || present(failed) != 0 || report.Scanned != 20 || report.Deleted["transfer_contract"] != 20 {
			t.Fatalf("the rerun from the checkpoint did not finish the rest: %+v %v", report, err)
		}
	})
}

// Turning the control off stops a running pass before its next batch and
// keeps the reaper idle; a dry run deletes nothing and leaves the cursor; the
// code switch overrides the control; turning it on resumes at the cursor.
func TestContractHorizonKillSwitchStopsRun(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer removeHorizonTestControl(t, ctx)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		contracts := []*horizonTestContract{}
		for i := range 30 {
			contracts = append(contracts, newHorizonTestContract(now.Add(-120*24*time.Hour+time.Duration(i)*time.Second), "canceled"))
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
		// production's code default: off until the control turns it on
		settings := horizonTestSettings()
		settings.DefaultMode = ContractHorizonModeOff
		settings.BatchSize = 10
		settings.MinBatchSize = 10

		if report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings); err != nil ||
			report.Stop != contractHorizonStopOff || !report.Idle() || report.Batches != 0 || present() != 30 || next.ContractId != (server.Id{}) {
			t.Fatalf("the code default deleted: %+v %v", report, err)
		}

		setHorizonTestControl(t, ctx, map[string]string{ContractHorizonControlFieldMode: "on"})
		observeHorizonTestBatches(t, func(batch int, last server.Id) {
			if batch == 1 {
				setHorizonTestControl(t, ctx, map[string]string{ContractHorizonControlFieldMode: "off"})
			}
		})
		report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || report.Stop != contractHorizonStopOff || report.Batches != 1 || present() != 20 || next.ContractId != contracts[9].contractId {
			t.Fatalf("the kill switch did not stop the pass after one batch: %+v %v, %d present", report, err, present())
		}
		observeHorizonTestBatches(t, nil)
		if again, againNext, err := RemoveContractsPastHorizon(ctx, now, next, settings); err != nil ||
			again.Stop != contractHorizonStopOff || present() != 20 || againNext.ContractId != next.ContractId {
			t.Fatalf("an off reaper ran: %+v %v", again, err)
		}

		setHorizonTestControl(t, ctx, map[string]string{ContractHorizonControlFieldMode: "dry_run"})
		if dryRun, dryRunNext, err := RemoveContractsPastHorizon(ctx, now, next, settings); err != nil ||
			dryRun.Mode != ContractHorizonModeDryRun || dryRun.Eligible != 20 || dryRun.Deleted["transfer_contract"] != 0 ||
			present() != 20 || dryRunNext.ContractId != next.ContractId || !dryRun.CaughtUp {
			t.Fatalf("a dry run deleted or moved the cursor: %+v %v", dryRun, err)
		}

		setHorizonTestControl(t, ctx, map[string]string{ContractHorizonControlFieldMode: "on"})
		disabled := *settings
		disabled.Enable = false
		if report, _, err := RemoveContractsPastHorizon(ctx, now, next, &disabled); err != nil || report.Stop != contractHorizonStopOff || present() != 20 {
			t.Fatalf("the code switch did not override the control: %+v %v", report, err)
		}

		report, _, err = RemoveContractsPastHorizon(ctx, now, next, settings)
		if err != nil || present() != 0 || report.Scanned != 20 || !report.CaughtUp {
			t.Fatalf("turning the control on did not resume at the cursor: %+v %v", report, err)
		}
	})
}

// A pass that reached the horizon keeps its cursor until it is RescanInterval
// old; then it starts over and deletes a contract that became eligible after
// the pass first read it.
func TestContractHorizonRescanRevisitsProtected(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		eligible := newHorizonTestContract(now.Add(-120*24*time.Hour), "canceled")
		unsettled := newHorizonTestContract(now.Add(-119*24*time.Hour), "canceled")
		unsettled.settled = false
		insertHorizonTestContracts(t, ctx, f, eligible, unsettled)
		settings := horizonTestSettings()
		settings.RescanInterval = 7 * 24 * time.Hour

		report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || report.Eligible != 1 || report.Protected[contractHorizonProtectedUnsettled] != 1 || next.ContractId != unsettled.contractId {
			t.Fatalf("first pass: %+v %v", report, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET settled=true WHERE contract_id=$1`, unsettled.contractId))
		}, server.TxReadCommitted, server.OptNoRetry())

		report, kept, err := RemoveContractsPastHorizon(ctx, now.Add(24*time.Hour), next, settings)
		if rows, _ := countHorizonTestRows(t, ctx, unsettled.contractId); err != nil || report.Scanned != 0 || kept.ContractId != next.ContractId || rows[0] != 1 {
			t.Fatalf("a young pass restarted: %+v %v", report, err)
		}
		report, restarted, err := RemoveContractsPastHorizon(ctx, now.Add(8*24*time.Hour), kept, settings)
		if err != nil || !report.CaughtUp || restarted.ContractId != (server.Id{}) || !restarted.PassStartTime.Equal(now.Add(8*24*time.Hour)) {
			t.Fatalf("an old pass did not restart: %+v %v %+v", report, err, restarted)
		}
		report, _, err = RemoveContractsPastHorizon(ctx, now.Add(8*24*time.Hour), restarted, settings)
		if rows, _ := countHorizonTestRows(t, ctx, unsettled.contractId); err != nil || report.Eligible != 1 || rows != [6]int{} {
			t.Fatalf("the rescan did not remove the newly eligible contract: %+v %v %v", report, err, rows)
		}
	})
}
