// The contract horizon reaper's rescan at the production interval, a week
// (owner, 2026-11-16): a pass that reached its horizon starts over a week
// after it began, and not before, so a contract it kept for a payment that had
// completed days earlier is deleted within about a week rather than a month. A
// pass still behind its horizon when the week ends reads on from its cursor
// and starts over once it reaches the horizon. The interval is not in the
// cursor, so a build with another interval that runs the chain during a roll
// hands the cursor on unchanged.
package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The cursor as the next run's task args carry it.
func horizonTestHandOn(t testing.TB, cursor ContractHorizonCursor) ContractHorizonCursor {
	t.Helper()
	cursorJson, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	var handedOn ContractHorizonCursor
	if err := json.Unmarshal(cursorJson, &handedOn); err != nil {
		t.Fatal(err)
	}
	return handedOn
}

// A pass that read a contract kept for a recent payment, then reached the
// horizon, reads nothing behind its cursor until it starts over. With the
// production interval it starts over exactly a week after it began: a run a
// millisecond before keeps the cursor, the first run at the week starts a new
// pass, and that pass deletes the contract, whose payment completed more than
// CompletedContractExpiration ago by then. A run of a build with the 30-day
// interval, as during a roll, keeps the pass and hands its cursor on
// unchanged. Before, with the 30-day interval, the run at the week kept the
// pass, and the contract waited for a rescan three weeks later.
func TestContractHorizonCaughtUpPassStartsOverAfterAWeek(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		const week = 7 * day
		now := server.NowUtc().Truncate(time.Millisecond)
		settings := horizonTestSettings()
		// production's interval, which the test settings set aside
		settings.RescanInterval = DefaultContractHorizonSettings().RescanInterval

		// completed two days ago, so it keeps the contract as payment_recent
		// for five more days
		paymentId := insertHorizonTestPayment(t, ctx, f.destinationNetworkId, horizonTestPayment{
			completed: true, completeTime: horizonTestPointer(now.Add(-2 * day))})
		paidRecently := newHorizonTestContract(now.Add(-120*day), "settled")
		paidRecently.paymentId = &paymentId
		insertHorizonTestContracts(t, ctx, f, paidRecently)
		before, _ := countHorizonTestRows(t, ctx, paidRecently.contractId)

		report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || !report.CaughtUp || report.Protected[contractHorizonProtectedPaymentRecent] != 1 || report.Eligible != 0 ||
			next.ContractId != paidRecently.contractId || !next.PassStartTime.Equal(now) {
			t.Fatalf("the pass did not keep the contract of the recent payment and reach the horizon: %+v, cursor %+v, %v", report, next, err)
		}

		// the payment is discharged from five days on, but the caught-up pass
		// reads nothing behind its cursor
		report, kept, err := RemoveContractsPastHorizon(ctx, now.Add(week-time.Millisecond), horizonTestHandOn(t, next), settings)
		if rows, _ := countHorizonTestRows(t, ctx, paidRecently.contractId); err != nil || report.Scanned != 0 || !report.CaughtUp ||
			kept.ContractId != next.ContractId || !kept.PassStartTime.Equal(now) || rows != before {
			t.Fatalf("a pass that began less than a week ago started over: rows %v, %+v, cursor %+v, %v", rows, report, kept, err)
		}

		// a build with the 30-day interval, still running during a roll
		before30 := *settings
		before30.RescanInterval = 30 * day
		report, handedOn, err := RemoveContractsPastHorizon(ctx, now.Add(week), horizonTestHandOn(t, kept), &before30)
		if rows, _ := countHorizonTestRows(t, ctx, paidRecently.contractId); err != nil || report.Scanned != 0 || !report.CaughtUp ||
			handedOn.ContractId != next.ContractId || !handedOn.PassStartTime.Equal(now) || rows != before {
			t.Fatalf("a run with the 30-day interval changed the pass: rows %v, %+v, cursor %+v, %v", rows, report, handedOn, err)
		}

		report, restarted, err := RemoveContractsPastHorizon(ctx, now.Add(week), horizonTestHandOn(t, handedOn), settings)
		if rows, _ := countHorizonTestRows(t, ctx, paidRecently.contractId); err != nil || !report.CaughtUp ||
			restarted.ContractId != (server.Id{}) || !restarted.PassStartTime.Equal(now.Add(week)) || len(restarted.SkippedReasonCounts) != 0 || rows != before {
			t.Fatalf("a caught-up pass that began a week ago did not start over: rows %v, %+v, cursor %+v, %v", rows, report, restarted, err)
		}

		report, _, err = RemoveContractsPastHorizon(ctx, now.Add(week), horizonTestHandOn(t, restarted), settings)
		if rows, archived := countHorizonTestRows(t, ctx, paidRecently.contractId); err != nil || rows != [6]int{} || archived != 1 ||
			report.Eligible != 1 || report.Protected[contractHorizonProtectedPaymentRecent] != 0 || !report.CaughtUp {
			t.Fatalf("the new pass did not delete the contract of the discharged payment: rows %v (contract, close, escrow, sweep, extender, report), archived %d, %+v, %v",
				rows, archived, report, err)
		}
	})
}

// A pass still behind its horizon when the week ends is not started over: its
// next run reads on from its cursor, past the contracts it read before, and
// starts the new pass only once it reaches the horizon. The new pass then
// reads again what the old one kept.
func TestContractHorizonPassBehindItsHorizonReadsOnPastTheWeek(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		now := server.NowUtc().Truncate(time.Millisecond)
		settings := horizonTestSettings()
		settings.RescanInterval = DefaultContractHorizonSettings().RescanInterval
		// two contracts a batch, so the pass takes more than one
		settings.BatchSize = 2
		settings.MinBatchSize = 2
		settings.TargetBatchDuration = time.Hour

		unsettledAt := func(createTime time.Time) *horizonTestContract {
			contract := newHorizonTestContract(createTime, "canceled")
			contract.settled = false
			return contract
		}
		keptFirst := unsettledAt(now.Add(-120 * day))
		eligibleFirst := newHorizonTestContract(now.Add(-119*day), "canceled")
		eligibleLast := newHorizonTestContract(now.Add(-118*day), "canceled")
		keptLast := unsettledAt(now.Add(-117 * day))
		insertHorizonTestContracts(t, ctx, f, keptFirst, eligibleFirst, eligibleLast, keptLast)
		present := func(contract *horizonTestContract) bool {
			rows, _ := countHorizonTestRows(t, ctx, contract.contractId)
			return rows[0] == 1
		}

		// the run stops after its first batch, short of the horizon
		runCtx, cancelRun := context.WithCancel(ctx)
		defer cancelRun()
		observeHorizonTestBatches(t, func(batch int, last server.Id) {
			if batch == 1 {
				cancelRun()
			}
		})
		report, next, err := RemoveContractsPastHorizon(runCtx, now, ContractHorizonCursor{}, settings)
		if err != nil || report.Stop != contractHorizonStopCanceled || report.CaughtUp || report.Scanned != 2 ||
			next.ContractId != eligibleFirst.contractId || !next.PassStartTime.Equal(now) || present(eligibleFirst) || !present(keptFirst) {
			t.Fatalf("the first run did not stop after one batch, behind the horizon: %+v, cursor %+v, %v", report, next, err)
		}
		observeHorizonTestBatches(t, nil)

		// eight days later the pass is past the week and still behind its horizon
		later := now.Add(8 * day)
		report, restarted, err := RemoveContractsPastHorizon(ctx, later, horizonTestHandOn(t, next), settings)
		if err != nil || !report.CaughtUp || report.Scanned != 2 || report.Eligible != 1 || report.Protected[contractHorizonProtectedUnsettled] != 1 {
			t.Fatalf("the pass did not read on from its cursor to the horizon: %+v, cursor %+v, %v", report, restarted, err)
		}
		if present(eligibleLast) || !present(keptFirst) || !present(keptLast) {
			t.Fatal("the run past the week deleted the wrong contracts")
		}
		if restarted.ContractId != (server.Id{}) || !restarted.PassStartTime.Equal(later) {
			t.Fatalf("the pass that reached its horizon past the week did not start over: cursor %+v", restarted)
		}

		report, _, err = RemoveContractsPastHorizon(ctx, later, horizonTestHandOn(t, restarted), settings)
		if err != nil || !report.CaughtUp || report.Scanned != 2 || report.Protected[contractHorizonProtectedUnsettled] != 2 {
			t.Fatalf("the new pass did not read again what the old one kept: %+v, %v", report, err)
		}
	})
}
