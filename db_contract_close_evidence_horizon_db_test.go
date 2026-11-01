// Close evidence retention at the contract horizon against PostgreSQL: the row
// deletes and the weekly drops follow the horizon in their context, down to the
// guard's 35 days, and keep what the readers need: the evidence of a contract
// that is not resolved, and every report accepted inside the horizon.
package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// On the plain table: without a horizon nothing younger than 91 days goes. At
// a 45-day horizon the reports of a resolved contract and of a deleted one go
// once accepted more than 45 days ago, while the reports of an unresolved
// contract, a late report of an old contract and every younger report stay. A
// horizon below the floor deletes down to 35 days and no further, which the
// guard admits.
func TestCloseEvidenceRowRetentionFollowsTheHorizon(t *testing.T) {
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
		floorAgedId := testContractIdAt(at(38 * day))
		youngId := testContractIdAt(at(30 * day))

		resolved := testCloseEvidenceReportAt(at(49*day), resolvedId, at(49*day))
		unresolved := testCloseEvidenceReportAt(at(49*day), unresolvedId, at(49*day))
		gone := testCloseEvidenceReportAt(at(49*day), goneId, at(49*day))
		// an old contract's report accepted 30 days ago
		late := testCloseEvidenceReportAt(at(30*day), lateId, at(30*day))
		floorAged := testCloseEvidenceReportAt(at(37*day), floorAgedId, at(37*day))
		young := testCloseEvidenceReportAt(at(30*day), youngId, at(30*day))
		reports := []testCloseEvidenceReport{resolved, unresolved, gone, late, floorAged, young}
		for _, report := range reports {
			connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
		}
		present := func(report testCloseEvidenceReport) bool {
			return testCloseEvidencePartitionOf(t, ctx, report) != ""
		}
		remove := func(ctx context.Context) int64 {
			removed, cursor, more := RemoveExpiredCloseEvidence(ctx, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
			connect.AssertEqual(t, more, false)
			connect.AssertEqual(t, cursor, CloseEvidenceRemovalCursor{})
			return removed
		}

		// without a horizon, and at one longer than every report's age
		connect.AssertEqual(t, remove(ctx), int64(0))
		connect.AssertEqual(t, remove(WithContractHorizon(ctx, 60*day)), int64(0))
		for _, report := range reports {
			connect.AssertEqual(t, present(report), true)
		}

		connect.AssertEqual(t, remove(WithContractHorizon(ctx, 45*day)), int64(2))
		connect.AssertEqual(t, present(resolved), false)
		connect.AssertEqual(t, present(gone), false)
		for _, report := range []testCloseEvidenceReport{unresolved, late, floorAged, young} {
			connect.AssertEqual(t, present(report), true)
		}

		// below the floor the retention is 35 days: the 37-day report goes,
		// the 30-day reports stay
		connect.AssertEqual(t, remove(WithContractHorizon(ctx, 20*day)), int64(1))
		connect.AssertEqual(t, present(floorAged), false)
		for _, report := range []testCloseEvidenceReport{unresolved, late, young} {
			connect.AssertEqual(t, present(report), true)
		}

		// once its contract resolves, the unresolved contract's report goes
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET outcome = 'canceled', close_time = $2 WHERE contract_id = $1`, unresolvedId, NowUtc()))
		})
		connect.AssertEqual(t, remove(WithContractHorizon(ctx, 45*day)), int64(1))
		connect.AssertEqual(t, present(unresolved), false)
		connect.AssertEqual(t, present(late), true)
		connect.AssertEqual(t, present(young), true)
	})
}

// Partitioned: a week whose every row qualifies drops once its upper bound is
// past the horizon, and not before; the fence refuses a week inside it; and a
// week a straggler keeps stays, while its qualifying rows are row-deleted.
func TestCloseEvidenceWeekDropsAtTheHorizon(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		testCloseEvidencePartitionedInThePast(t, ctx, now)
		weekStart := func(weeks int) time.Time {
			return PartitionGranularityWeekly.RangeStart(now).Add(-time.Duration(weeks) * 7 * day)
		}
		horizon := func(horizon time.Duration) context.Context {
			return WithContractHorizon(ctx, horizon)
		}

		// upper bounds 42 to 49 days past, and 49 to 56
		cleanWeek, cleanLeaf := testCloseEvidenceCreateWeek(t, ctx, weekStart(7).Add(day))
		stragglerWeek, stragglerLeaf := testCloseEvidenceCreateWeek(t, ctx, weekStart(8).Add(day))
		// upper bound 28 to 35 days past
		youngWeek, youngLeaf := testCloseEvidenceCreateWeek(t, ctx, weekStart(5).Add(day))

		clean := testCloseEvidenceReportAt(cleanWeek.Lower.Add(day), testContractIdAt(cleanWeek.Lower), cleanWeek.Lower.Add(day))
		qualifying := testCloseEvidenceReportAt(stragglerWeek.Lower.Add(day), testContractIdAt(stragglerWeek.Lower), stragglerWeek.Lower.Add(day))
		straggler := testCloseEvidenceReportAt(stragglerWeek.Lower.Add(day), testContractIdAt(stragglerWeek.Lower), now.Add(-20*day))
		young := testCloseEvidenceReportAt(youngWeek.Lower.Add(day), testContractIdAt(youngWeek.Lower), youngWeek.Lower.Add(day))
		for _, report := range []testCloseEvidenceReport{clean, qualifying, straggler, young} {
			connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
		}
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, clean), cleanLeaf)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, straggler), stragglerLeaf)

		// without a horizon, and at one longer than the weeks' age, nothing
		// expires, and the fence refuses the drop
		for _, ctx := range []context.Context{ctx, horizon(60 * day)} {
			report, _ := closeEvidencePartitionFamily.Retire(ctx, now, PartitionFamilyState{}, time.Now().Add(time.Minute))
			connect.AssertEqual(t, len(report.Dropped), 0)
			connect.AssertEqual(t, len(report.Blocked), 0)
		}
		if _, err := closeEvidencePartitionFamily.DropRange(ctx, cleanWeek); err == nil || !strings.Contains(err.Error(), "is not past its retention of 91 days") {
			t.Fatalf("a drop inside 91 days: %v", err)
		}
		if _, err := closeEvidencePartitionFamily.DropRange(horizon(35*day), youngWeek); err == nil || !strings.Contains(err.Error(), "is not past its retention of 35 days") {
			t.Fatalf("a drop inside 35 days: %v", err)
		}

		// at 35 days the row deletes take the straggler week's qualifying row
		// and leave the clean week to the drop
		removed, _, _ := RemoveExpiredCloseEvidence(horizon(35*day), now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, removed, int64(1))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, qualifying), "")
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, clean), cleanLeaf)

		report, _ := closeEvidencePartitionFamily.Retire(horizon(35*day), now, PartitionFamilyState{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, report.Dropped, []string{cleanLeaf})
		connect.AssertEqual(t, report.Blocked[stragglerLeaf], closeEvidenceBlockerRecentReport)
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, cleanLeaf), false)
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, youngLeaf), true)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, straggler), stragglerLeaf)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, young), youngLeaf)
	})
}
