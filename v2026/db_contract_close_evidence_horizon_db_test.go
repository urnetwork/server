// Close evidence retention at its own window against PostgreSQL: the row
// deletes and the weekly drops follow the window in their context, down to the
// guard's 7 days, and keep what the readers need: the evidence of a contract
// that is not resolved, and every report accepted inside the window. The
// contract horizon does not move them (2026-11-23).
package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// On the plain table: without a window nothing younger than 91 days goes, and
// the contract horizon alone, at its 14-day floor, moves nothing, where it
// moved the evidence to 35 days before. At a 45-day window the reports of a
// resolved contract and of a deleted one go once accepted more than 45 days
// ago, while the reports of an unresolved contract, a late report of an old
// contract and every younger report stay. At the owner's 7 days the late
// report, the 37-day one and the 10-day one go and the 5-day one stays, and a
// window below the floor deletes down to 7 days and no further, which the
// guard admits.
func TestCloseEvidenceRowRetentionFollowsItsWindow(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		at := func(age time.Duration) time.Time {
			return now.Add(-age)
		}

		resolvedId := testContractIdAt(at(50 * day))
		testInsertContractRows(t, ctx, resolvedId, true)
		unresolvedId := testContractIdAt(at(50*day - time.Minute))
		testInsertContractRows(t, ctx, unresolvedId, false)
		// minted, reported and then deleted, as the horizon reaper deletes:
		// no contract row is left
		goneId := testContractIdAt(at(50*day - 2*time.Minute))
		lateId := testContractIdAt(at(50*day - 3*time.Minute))
		agedId := testContractIdAt(at(38 * day))
		tenDayId := testContractIdAt(at(11 * day))
		youngId := testContractIdAt(at(6 * day))

		resolved := testCloseEvidenceReportAt(at(49*day), resolvedId, at(49*day))
		unresolved := testCloseEvidenceReportAt(at(49*day), unresolvedId, at(49*day))
		gone := testCloseEvidenceReportAt(at(49*day), goneId, at(49*day))
		// an old contract's report accepted 30 days ago
		late := testCloseEvidenceReportAt(at(30*day), lateId, at(30*day))
		aged := testCloseEvidenceReportAt(at(37*day), agedId, at(37*day))
		tenDay := testCloseEvidenceReportAt(at(10*day), tenDayId, at(10*day))
		young := testCloseEvidenceReportAt(at(5*day), youngId, at(5*day))
		reports := []testCloseEvidenceReport{resolved, unresolved, gone, late, aged, tenDay, young}
		for _, report := range reports {
			connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
		}
		present := func(report testCloseEvidenceReport) bool {
			return testCloseEvidencePartitionOf(t, ctx, report) != ""
		}
		remove := func(ctx context.Context) int64 {
			report, cursor, more := RemoveExpiredCloseEvidence(ctx, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
			connect.AssertEqual(t, more, false)
			connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopDone)
			connect.AssertEqual(t, cursor.InProgress(), false)
			return report.Removed
		}

		// without a window, at the contract horizon alone, and at a window
		// longer than every report's age
		connect.AssertEqual(t, remove(ctx), int64(0))
		connect.AssertEqual(t, remove(WithContractHorizon(ctx, 14*day)), int64(0))
		connect.AssertEqual(t, remove(WithCloseEvidenceWindow(ctx, 60*day)), int64(0))
		for _, report := range reports {
			connect.AssertEqual(t, present(report), true)
		}

		connect.AssertEqual(t, remove(WithCloseEvidenceWindow(ctx, 45*day)), int64(2))
		connect.AssertEqual(t, present(resolved), false)
		connect.AssertEqual(t, present(gone), false)
		for _, report := range []testCloseEvidenceReport{unresolved, late, aged, tenDay, young} {
			connect.AssertEqual(t, present(report), true)
		}

		// the owner's window: 7 days
		connect.AssertEqual(t, remove(WithCloseEvidenceWindow(ctx, CloseEvidenceRetentionWindow)), int64(3))
		for _, report := range []testCloseEvidenceReport{late, aged, tenDay} {
			connect.AssertEqual(t, present(report), false)
		}
		connect.AssertEqual(t, present(unresolved), true)
		connect.AssertEqual(t, present(young), true)

		// below the floor the retention is 7 days: the 5-day report stays
		connect.AssertEqual(t, remove(WithCloseEvidenceWindow(ctx, time.Hour)), int64(0))
		connect.AssertEqual(t, present(young), true)

		// once its contract resolves, the unresolved contract's report goes
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET outcome = 'canceled', close_time = $2 WHERE contract_id = $1`, unresolvedId, NowUtc()))
		})
		connect.AssertEqual(t, remove(WithCloseEvidenceWindow(ctx, CloseEvidenceRetentionWindow)), int64(1))
		connect.AssertEqual(t, present(unresolved), false)
		connect.AssertEqual(t, present(young), true)
	})
}

// Partitioned: a week whose every row qualifies drops once its upper bound is
// past the window, and not before; the fence refuses a week inside it, and
// one the contract horizon alone would reach; and a week a straggler keeps
// stays, while its qualifying rows are row-deleted.
func TestCloseEvidenceWeekDropsAtTheWindow(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		testCloseEvidencePartitionedInThePast(t, ctx, now)
		weekStart := func(weeks int) time.Time {
			return PartitionGranularityWeekly.RangeStart(now).Add(-time.Duration(weeks) * 7 * day)
		}
		window := func(window time.Duration) context.Context {
			return WithCloseEvidenceWindow(ctx, window)
		}

		// upper bounds 14 to 21 days past, 21 to 28, and 28 to 35
		cleanWeek, cleanLeaf := testCloseEvidenceCreateWeek(t, ctx, weekStart(3).Add(day))
		stragglerWeek, stragglerLeaf := testCloseEvidenceCreateWeek(t, ctx, weekStart(4).Add(day))
		unresolvedWeek, unresolvedLeaf := testCloseEvidenceCreateWeek(t, ctx, weekStart(5).Add(day))
		// upper bound 0 to 7 days past
		youngWeek, youngLeaf := testCloseEvidenceCreateWeek(t, ctx, weekStart(1).Add(day))

		clean := testCloseEvidenceReportAt(cleanWeek.Lower.Add(day), testContractIdAt(cleanWeek.Lower), cleanWeek.Lower.Add(day))
		qualifying := testCloseEvidenceReportAt(stragglerWeek.Lower.Add(day), testContractIdAt(stragglerWeek.Lower), stragglerWeek.Lower.Add(day))
		straggler := testCloseEvidenceReportAt(stragglerWeek.Lower.Add(day), testContractIdAt(stragglerWeek.Lower), now.Add(-3*day))
		young := testCloseEvidenceReportAt(youngWeek.Lower.Add(day), testContractIdAt(youngWeek.Lower), youngWeek.Lower.Add(day))
		// a report of a contract that is still open, in a week otherwise past
		// the window
		unresolvedContractId := testContractIdAt(unresolvedWeek.Lower)
		testInsertContractRows(t, ctx, unresolvedContractId, false)
		unresolved := testCloseEvidenceReportAt(unresolvedWeek.Lower.Add(day), unresolvedContractId, unresolvedWeek.Lower.Add(day))
		for _, report := range []testCloseEvidenceReport{clean, qualifying, straggler, young, unresolved} {
			connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
		}
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, clean), cleanLeaf)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, straggler), stragglerLeaf)

		// without a window, at the contract horizon alone, and at a window
		// longer than the weeks' age, nothing expires, and the fence refuses
		// the drop
		for _, ctx := range []context.Context{ctx, WithContractHorizon(ctx, 14*day), window(70 * day)} {
			report, _ := closeEvidencePartitionFamily.Retire(ctx, now, PartitionFamilyState{}, time.Now().Add(time.Minute))
			connect.AssertEqual(t, len(report.Dropped), 0)
			connect.AssertEqual(t, len(report.Blocked), 0)
		}
		for _, ctx := range []context.Context{ctx, WithContractHorizon(ctx, 14*day)} {
			if _, err := closeEvidencePartitionFamily.DropRange(ctx, cleanWeek); err == nil || !strings.Contains(err.Error(), "is not past its retention of 91 days") {
				t.Fatalf("a drop inside 91 days: %v", err)
			}
		}
		if _, err := closeEvidencePartitionFamily.DropRange(window(7*day), youngWeek); err == nil || !strings.Contains(err.Error(), "is not past its retention of 7 days") {
			t.Fatalf("a drop inside 7 days: %v", err)
		}

		// at 7 days the row deletes take the straggler week's qualifying row
		// and leave the clean week to the drop
		removal, _, _ := RemoveExpiredCloseEvidence(window(7*day), now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, removal.Removed, int64(1))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, qualifying), "")
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, clean), cleanLeaf)

		report, _ := closeEvidencePartitionFamily.Retire(window(7*day), now, PartitionFamilyState{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, report.Dropped, []string{cleanLeaf})
		connect.AssertEqual(t, report.Blocked[stragglerLeaf], closeEvidenceBlockerRecentReport)
		connect.AssertEqual(t, report.Blocked[unresolvedLeaf], closeEvidenceBlockerUnresolved)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, unresolved), unresolvedLeaf)
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, cleanLeaf), false)
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, youngLeaf), true)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, straggler), stragglerLeaf)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, young), youngLeaf)
	})
}

// A release with the window keeps 35 days while the installed guard is the
// horizon guard (migration 913), as on Main until the window guard migration
// applies, and 91 days while it admits only 91, as before 913 or after an
// operator restores the move guard to stop the deletes: the row pass deletes
// only rows past the guard's age, without failing on the younger ones the
// guard refuses, and the drop fence refuses a week inside it. Once the window
// guard is back, the window applies again.
func TestCloseEvidenceRetentionWaitsForTheWindowGuard(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		window := WithCloseEvidenceWindow(ctx, CloseEvidenceRetentionWindow)
		guardFloor := func() (floor time.Duration) {
			MaintenanceDb(ctx, func(conn PgConn) {
				floor = closeEvidenceGuardFloor(ctx, conn)
			})
			return
		}
		connect.AssertEqual(t, guardFloor(), CloseEvidenceRetentionFloor)

		// the horizon guard's 35 days, as Main has before the migration
		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, contractCloseReportEvidenceHorizonGuardSql))
		connect.AssertEqual(t, guardFloor(), 35*day)

		aged := testCloseEvidenceReportAt(now.Add(-92*day), testContractIdAt(now.Add(-93*day)), now.Add(-92*day))
		inside := testCloseEvidenceReportAt(now.Add(-50*day), testContractIdAt(now.Add(-51*day)), now.Add(-50*day))
		month := testCloseEvidenceReportAt(now.Add(-30*day), testContractIdAt(now.Add(-31*day)), now.Add(-30*day))
		tenDay := testCloseEvidenceReportAt(now.Add(-10*day), testContractIdAt(now.Add(-11*day)), now.Add(-10*day))
		for _, report := range []testCloseEvidenceReport{aged, inside, month, tenDay} {
			connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
		}

		// the move guard's 91 days: only the 92-day report goes
		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, contractCloseReportEvidenceMoveGuardSql))
		connect.AssertEqual(t, guardFloor(), ContractRetention)
		removal, _, _ := RemoveExpiredCloseEvidence(window, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, removal.Removed, int64(1))
		connect.AssertEqual(t, removal.Retention, ContractRetention)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, aged), "")
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, inside) != "", true)

		// the horizon guard: down to 35 days
		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, contractCloseReportEvidenceHorizonGuardSql))
		removal, _, _ = RemoveExpiredCloseEvidence(window, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, removal.Removed, int64(1))
		connect.AssertEqual(t, removal.Retention, 35*day)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, inside), "")
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, month) != "", true)

		// the window guard back: the window applies
		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, contractCloseReportEvidenceWindowGuardSql))
		connect.AssertEqual(t, guardFloor(), CloseEvidenceRetentionFloor)
		removal, _, _ = RemoveExpiredCloseEvidence(window, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, removal.Removed, int64(2))
		connect.AssertEqual(t, removal.Retention, 7*day)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, month), "")
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, tenDay), "")

		// partitioned, under the horizon guard again: a week 14 to 21 days
		// past stays at the 7-day window
		testCloseEvidencePartitionedInThePast(t, ctx, now)
		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, contractCloseReportEvidenceHorizonGuardSql))
		weekStart := PartitionGranularityWeekly.RangeStart(now).Add(-3 * 7 * day)
		week, leaf := testCloseEvidenceCreateWeek(t, ctx, weekStart.Add(day))
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, testCloseEvidenceReportAt(week.Lower.Add(day), testContractIdAt(week.Lower), week.Lower.Add(day))), true)
		if _, err := closeEvidencePartitionFamily.DropRange(window, week); err == nil || !strings.Contains(err.Error(), "is not past its retention of 35 days") {
			t.Fatalf("a drop the installed guard does not admit: %v", err)
		}
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, leaf), true)
		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, contractCloseReportEvidenceWindowGuardSql))
		dropped, err := closeEvidencePartitionFamily.DropRange(window, week)
		testCloseEvidenceNoError(t, err)
		connect.AssertEqual(t, dropped, []string{leaf})
	})
}

// The evidence tasks carry the window only while the brake is on: absent or
// on, 7 days; off, or at a rate of zero, nothing, so the evidence keeps 91 days
// and the row deletes and the weekly drops stop below 91 days.
func TestCloseEvidenceRetentionControlCarriesTheWindow(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const day = 24 * time.Hour
		controlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		defer func() {
			removeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			Raise(RemoveCloseEvidenceRetentionControl(removeCtx))
		}()
		for _, test := range []struct {
			fields    map[string]string
			retention time.Duration
		}{
			{nil, 7 * day},
			{map[string]string{CloseEvidenceRetentionControlFieldMode: "on", CloseEvidenceRetentionControlFieldMaxRowsPerSecond: "5000"}, 7 * day},
			{map[string]string{CloseEvidenceRetentionControlFieldMode: "off"}, ContractRetention},
			{map[string]string{CloseEvidenceRetentionControlFieldMode: "on", CloseEvidenceRetentionControlFieldMaxRowsPerSecond: "0"}, ContractRetention},
		} {
			Raise(RemoveCloseEvidenceRetentionControl(controlCtx))
			Raise(SetCloseEvidenceRetentionControl(controlCtx, test.fields))
			if retention := CloseEvidenceRetention(WithCloseEvidenceRetentionControl(ctx)); retention != test.retention {
				t.Errorf("brake %v carries a retention of %s, want %s", test.fields, retention, test.retention)
			}
		}
	})
}
