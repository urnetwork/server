// The contract horizon reaper's pass at its horizon: it never moves its
// cursor past a contract that is about to qualify, and a pass that began while
// reads still reached the horizon itself starts over, so the contracts it moved
// past are read again (Main, 2026-11-12 on).
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A caught-up pass never moves its cursor past a contract that is about to
// qualify. At the 35-day horizon the newest contracts below it were minted
// minutes before it and close 12 minutes after they were minted (the quiet
// close), after it. A pass that read up to the horizon itself read them, kept
// them as recent_close and moved past them, and nothing read them again until
// the next rescan, weeks later: on Main from 2026-11-13, 99% of what the pass
// read. Now the first run reads none of them and stops before them, and a run
// a day later, with their closes past the horizon, deletes every one and
// keeps none.
func TestContractHorizonCaughtUpPassNeverMovesPastContractsAboutToQualify(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		now := server.NowUtc().Truncate(time.Millisecond)
		horizonTime := now.Add(-ContractHorizonFloor)
		settings := horizonTestSettings()
		settings.Horizon = ContractHorizonFloor

		// past the horizon long ago, so the pass catches up behind it
		old := newHorizonTestContract(now.Add(-40*day), "canceled")
		aboutToQualify := []*horizonTestContract{}
		for i := range 3 {
			contract := newHorizonTestContract(horizonTime.Add(-5*time.Minute+time.Duration(i)*time.Second), "settled")
			contract.closeTime = horizonTestPointer(contract.createTime.Add(12 * time.Minute))
			aboutToQualify = append(aboutToQualify, contract)
		}
		insertHorizonTestContracts(t, ctx, f, append([]*horizonTestContract{old}, aboutToQualify...)...)
		before := map[server.Id][6]int{}
		for _, contract := range aboutToQualify {
			before[contract.contractId], _ = countHorizonTestRows(t, ctx, contract.contractId)
		}

		report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil {
			t.Fatal(err)
		}
		if !report.CaughtUp || report.Eligible != 1 || report.Protected[contractHorizonProtectedRecentClose] != 0 {
			t.Fatalf("the caught-up pass read contracts about to qualify and kept them as recent_close: %+v", report)
		}
		for _, contract := range aboutToQualify {
			rows, _ := countHorizonTestRows(t, ctx, contract.contractId)
			if !next.ContractId.Less(contract.contractId) || rows != before[contract.contractId] {
				t.Fatalf("the caught-up pass moved its cursor past a contract minted %s before the horizon and closing %s after it: cursor at %s, rows %v",
					horizonTime.Sub(contract.createTime), contract.closeTime.Sub(horizonTime), server.ContractIdTime(next.ContractId), rows)
			}
		}

		// a day later their closes are past the horizon
		report, _, err = RemoveContractsPastHorizon(ctx, now.Add(day), next, settings)
		if err != nil {
			t.Fatal(err)
		}
		for _, contract := range aboutToQualify {
			if rows, archived := countHorizonTestRows(t, ctx, contract.contractId); rows != [6]int{} || archived != 1 {
				t.Fatalf("a contract that closed before the horizon was never read again: rows %v (contract, close, escrow, sweep, extender, report), archived %d, %+v",
					rows, archived, report)
			}
		}
		if report.Eligible != 3 || report.Protected[contractHorizonProtectedRecentClose] != 0 || !report.CaughtUp {
			t.Fatalf("wrong report a day later: %+v", report)
		}
	})
}

// The pass that ran when reads stopped short of the horizon began on
// 2026-11-01 and, caught up at the horizon, moved past what it read minutes
// before it as recent_close. Its first batch with the mode on starts a new
// pass at the oldest contract, which deletes those contracts now that their
// closes are past the horizon. Before, the run found nothing past the cursor
// and kept the pass until its rescan, 30 days after it began. A pass begun
// since keeps its cursor, also as a build from before hands it on with only
// its id, start and batch size: the contract behind it waits for a later pass.
func TestContractHorizonPassFromBeforeSettledReadsStartsOver(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		now := server.NowUtc().Truncate(time.Millisecond)
		horizonTime := now.Add(-ContractHorizonFloor)
		// no rescan is due in this test, before the change or after it
		settings := horizonTestSettings()
		settings.Horizon = ContractHorizonFloor

		// minutes before the horizon three days ago, when the pass read them,
		// and closed 12 minutes after they were minted
		skipped := []*horizonTestContract{}
		for i := range 3 {
			contract := newHorizonTestContract(horizonTime.Add(-3*day+time.Duration(i)*time.Second), "settled")
			contract.closeTime = horizonTestPointer(contract.createTime.Add(12 * time.Minute))
			skipped = append(skipped, contract)
		}
		insertHorizonTestContracts(t, ctx, f, skipped...)
		// Main's cursor on 2026-11-15: the pass of 2026-11-01 at the horizon of
		// its run five minutes before
		passStartTime := time.Date(2026, time.November, 1, 15, 30, 51, 0, time.UTC)
		cursor := ContractHorizonCursor{
			ContractId:    server.ContractIdFloor(horizonTime.Add(-5 * time.Minute)),
			PassStartTime: passStartTime,
			BatchSize:     2000,
		}

		report, next, err := RemoveContractsPastHorizon(ctx, now, cursor, settings)
		if err != nil {
			t.Fatal(err)
		}
		for _, contract := range skipped {
			if rows, archived := countHorizonTestRows(t, ctx, contract.contractId); rows != [6]int{} || archived != 1 {
				t.Fatalf("the contracts the pass of %s moved past were kept: rows %v (contract, close, escrow, sweep, extender, report), archived %d, %+v, cursor %+v",
					passStartTime.Format(time.RFC3339), rows, archived, report, next)
			}
		}
		if !next.PassStartTime.Equal(now) || next.ContractId != skipped[2].contractId || next.BatchSize != 2000 || report.Eligible != 3 || !report.CaughtUp {
			t.Fatalf("no new pass started at the oldest contract: %+v, cursor %+v", report, next)
		}

		behind := newHorizonTestContract(horizonTime.Add(-4*day), "canceled")
		insertHorizonTestContracts(t, ctx, f, behind)
		handedOn := ContractHorizonCursor{
			ContractId:    next.ContractId,
			PassStartTime: next.PassStartTime,
			BatchSize:     next.BatchSize,
		}
		report, kept, err := RemoveContractsPastHorizon(ctx, now.Add(time.Hour), handedOn, settings)
		if rows, _ := countHorizonTestRows(t, ctx, behind.contractId); err != nil || rows[0] != 1 || report.Scanned != 0 ||
			!kept.PassStartTime.Equal(next.PassStartTime) || kept.ContractId != next.ContractId {
			t.Fatalf("a pass begun since started over: rows %v, %+v, cursor %+v, %v", rows, report, kept, err)
		}
	})
}

// A run whose clock is before reads stopped short of the horizon, as a test
// fixture's can be, never starts its pass over: the pass it began would begin
// before that time too and start over at every batch, rereading the contracts
// it keeps until its budget ran out. It reads its kept contracts once and
// reaches the horizon.
func TestContractHorizonRunBeforeSettledReadsNeverStartsOver(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const day = 24 * time.Hour
		now := contractHorizonSettledPassTime.Add(-day)
		settings := horizonTestSettings()
		settings.Horizon = ContractHorizonFloor
		// a pass that starts over at every batch stops here instead of in 10 minutes
		settings.RunBudget = 10 * time.Second
		kept := []*horizonTestContract{}
		for i := range 3 {
			contract := newHorizonTestContract(now.Add(-40*day+time.Duration(i)*time.Second), "canceled")
			contract.settled = false
			kept = append(kept, contract)
		}
		insertHorizonTestContracts(t, ctx, f, kept...)

		report, next, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, settings)
		if err != nil || !report.CaughtUp || report.Batches != 2 || report.Scanned != 3 || report.Protected[contractHorizonProtectedUnsettled] != 3 ||
			next.ContractId != kept[2].contractId || !next.PassStartTime.Equal(now) {
			t.Fatalf("a run before reads were settled started its pass over: %+v, cursor %+v, %v", report, next, err)
		}
	})
}
