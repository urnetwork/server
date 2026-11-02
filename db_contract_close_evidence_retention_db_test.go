// The close evidence row retention against PostgreSQL and Redis: its brake,
// its bounded batch, its delete by ctid, where its passes start, and the
// retention it applies when the guard is missing or still the 91-day one.
package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func testSetCloseEvidenceRetentionControl(t testing.TB, fields map[string]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	testCloseEvidenceNoError(t, RemoveCloseEvidenceRetentionControl(ctx))
	testCloseEvidenceNoError(t, SetCloseEvidenceRetentionControl(ctx, fields))
}

func testRemoveCloseEvidenceRetentionControl(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	testCloseEvidenceNoError(t, RemoveCloseEvidenceRetentionControl(ctx))
}

// The brake is read before every batch: off, on at a rate of zero, or at a
// rate that does not parse, nothing goes and the run says why; without the
// brake the code's default rate deletes what expired, and the run reports the
// retention it applied.
func TestCloseEvidenceRetentionBrakeStopsTheDeletes(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer testRemoveCloseEvidenceRetentionControl(t)
		now := NowUtc()
		const day = 24 * time.Hour
		horizon := WithContractHorizon(ctx, 45*day)
		resolvedId := testContractIdAt(now.Add(-50 * day))
		testInsertContractRows(t, ctx, resolvedId, true)
		reports := []testCloseEvidenceReport{
			testCloseEvidenceReportAt(now.Add(-50*day), resolvedId, now.Add(-50*day)),
			testCloseEvidenceReportAt(now.Add(-50*day), resolvedId, now.Add(-50*day)),
			// a contract the reaper deleted
			testCloseEvidenceReportAt(now.Add(-49*day), testContractIdAt(now.Add(-49*day)), now.Add(-49*day)),
		}
		for _, report := range reports {
			connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
		}
		remaining := func() (count int) {
			for _, report := range reports {
				if testCloseEvidencePartitionOf(t, ctx, report) != "" {
					count += 1
				}
			}
			return
		}

		for _, fields := range []map[string]string{
			{CloseEvidenceRetentionControlFieldMode: "off"},
			{CloseEvidenceRetentionControlFieldMode: "off", CloseEvidenceRetentionControlFieldMaxRowsPerSecond: "1000"},
			{CloseEvidenceRetentionControlFieldMode: "on", CloseEvidenceRetentionControlFieldMaxRowsPerSecond: "0"},
			{CloseEvidenceRetentionControlFieldMaxRowsPerSecond: "fast"},
		} {
			testSetCloseEvidenceRetentionControl(t, fields)
			report, cursor, more := RemoveExpiredCloseEvidence(horizon, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
			connect.AssertEqual(t, report.Removed, int64(0))
			connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopBrakeOff)
			connect.AssertEqual(t, more, false)
			// the pass waits where it stood for the brake to lift
			connect.AssertEqual(t, cursor.InProgress(), true)
			connect.AssertEqual(t, remaining(), 3)
		}

		// on at a rate
		testSetCloseEvidenceRetentionControl(t, map[string]string{CloseEvidenceRetentionControlFieldMode: "on", CloseEvidenceRetentionControlFieldMaxRowsPerSecond: "500"})
		report, _, _ := RemoveExpiredCloseEvidence(horizon, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, report.Removed, int64(3))
		connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopDone)
		connect.AssertEqual(t, report.Retention, 45*day)
		connect.AssertEqual(t, remaining(), 0)

		// without the brake, at the code's default rate
		testRemoveCloseEvidenceRetentionControl(t)
		another := testCloseEvidenceReportAt(now.Add(-48*day), testContractIdAt(now.Add(-48*day)), now.Add(-48*day))
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, another), true)
		report, _, _ = RemoveExpiredCloseEvidence(horizon, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, report.Removed, int64(1))
		connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopDone)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, another), "")
	})
}

// A batch waits at most its lock timeout for a row another transaction holds,
// and fails instead of queueing behind it; the next run deletes the row.
func TestCloseEvidenceRetentionBatchBoundsItsLockWait(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		horizon := WithContractHorizon(ctx, 45*day)
		report := testCloseEvidenceReportAt(now.Add(-50*day), testContractIdAt(now.Add(-50*day)), now.Add(-50*day))
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)

		locked := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			MaintenanceTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(
					ctx,
					`SELECT 1 FROM contract_close_report_evidence WHERE client_id = $1 AND report_id = $2 FOR UPDATE`,
					report.clientId,
					report.reportId,
				))
				close(locked)
				select {
				case <-release:
				case <-time.After(30 * time.Second):
				}
			}, TxReadCommitted, OptNoRetry())
		}()
		<-locked
		start := time.Now()
		removal, _, more := RemoveExpiredCloseEvidence(horizon, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
		waited := time.Since(start)
		close(release)
		<-done
		if 15*time.Second < waited {
			t.Fatalf("the batch waited %s for a held row", waited)
		}
		connect.AssertEqual(t, removal.Stop, CloseEvidenceRemovalStopError)
		connect.AssertEqual(t, removal.Removed, int64(0))
		connect.AssertEqual(t, more, false)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, report) != "", true)

		removal, _, _ = RemoveExpiredCloseEvidence(horizon, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, removal.Removed, int64(1))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, report), "")
	})
}

// The batch deletes the rows it read by their ctid, a TID scan, not by
// probing the primary key for each one. The test's table is a page or two,
// where a sequential scan costs the same, so the plan is read with sequential
// scans off: the delete's only other access is then by an index.
func TestCloseEvidenceRetentionBatchDeletesByCtid(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		for range 20 {
			report := testCloseEvidenceReportAt(now.Add(-50*day), testContractIdAt(now.Add(-50*day)), now.Add(-50*day))
			connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `ANALYZE contract_close_report_evidence`))
		})
		var plan []string
		testCloseEvidenceNoError(t, contractPartitionTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
			result, err := tx.Query(
				ctx,
				"EXPLAIN "+fmt.Sprintf(closeEvidenceRetentionBatchSql, quoteIdentifier(closeEvidenceTable)),
				Id{},
				Id{},
				Id{},
				ContractIdFloor(now.Add(-45*day)),
				5000,
				float64(45*24*60*60),
			)
			WithPgResult(result, err, func() {
				for result.Next() {
					var line string
					Raise(result.Scan(&line))
					plan = append(plan, line)
				}
			})
		}))
		text := strings.Join(plan, "\n")
		if !strings.Contains(text, "Tid Scan on contract_close_report_evidence") {
			t.Fatalf("the batch's delete is not a TID scan:\n%s", text)
		}
	})
}

// The first pass of the day reads from the first key and leaves its low water:
// the lowest contract of a row it kept for being accepted inside the
// retention. Later passes that day start there, so a contract below it that
// resolved since keeps its evidence until the next day's first pass takes it.
// A run that ends inside a contract's rows resumes at the next row.
func TestCloseEvidenceRetentionStartsAtTheLowWater(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		horizon := WithContractHorizon(ctx, 45*day)
		settings := DefaultCloseEvidenceRetentionSettings()
		settings.BatchSize = 2

		unresolvedId := testContractIdAt(now.Add(-60 * day))
		testInsertContractRows(t, ctx, unresolvedId, false)
		resolvedId := testContractIdAt(now.Add(-50 * day))
		testInsertContractRows(t, ctx, resolvedId, true)
		lateId := testContractIdAt(now.Add(-46 * day))
		testInsertContractRows(t, ctx, lateId, true)
		youngId := testContractIdAt(now.Add(-30 * day))
		testInsertContractRows(t, ctx, youngId, true)
		unresolved := testCloseEvidenceReportAt(now.Add(-60*day), unresolvedId, now.Add(-60*day))
		resolvedSource := testCloseEvidenceReportAt(now.Add(-50*day), resolvedId, now.Add(-50*day))
		resolvedDestination := testCloseEvidenceReportAt(now.Add(-50*day), resolvedId, now.Add(-50*day))
		// accepted two days after its contract's mint, inside the retention
		late := testCloseEvidenceReportAt(now.Add(-44*day), lateId, now.Add(-44*day))
		young := testCloseEvidenceReportAt(now.Add(-30*day), youngId, now.Add(-30*day))
		for _, report := range []testCloseEvidenceReport{unresolved, resolvedSource, resolvedDestination, late, young} {
			connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
		}
		present := func(report testCloseEvidenceReport) bool {
			return testCloseEvidencePartitionOf(t, ctx, report) != ""
		}
		today := now.UTC().Format(time.DateOnly)

		// the day's first pass, stopped after its first batch by its deadline
		report, cursor, more := removeExpiredCloseEvidenceWith(horizon, now, CloseEvidenceRemovalCursor{}, time.Now(), settings)
		connect.AssertEqual(t, report.FullPass, true)
		connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopBudget)
		connect.AssertEqual(t, more, true)
		connect.AssertEqual(t, report.Removed, int64(1))
		connect.AssertEqual(t, cursor.InProgress(), true)
		connect.AssertEqual(t, *cursor.ContractId, resolvedId)
		// it resumes at the resolved contract's other report
		report, cursor, more = removeExpiredCloseEvidenceWith(horizon, now, cursor, time.Now().Add(time.Minute), settings)
		connect.AssertEqual(t, report.FullPass, false)
		connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopDone)
		connect.AssertEqual(t, more, false)
		connect.AssertEqual(t, report.Removed, int64(1))
		connect.AssertEqual(t, present(resolvedSource) || present(resolvedDestination), false)
		connect.AssertEqual(t, present(unresolved), true)
		connect.AssertEqual(t, present(late), true)
		connect.AssertEqual(t, present(young), true)
		connect.AssertEqual(t, cursor.InProgress(), false)
		connect.AssertEqual(t, *cursor.LowWater, lateId)
		connect.AssertEqual(t, cursor.FullPassDay, today)

		// the unresolved contract resolves; the day's next pass starts at the
		// low water, above it
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_contract SET outcome = 'settled', close_time = $2, provider_usage = '{"version":1,"byte_count":1,"providers":[]}' WHERE contract_id = $1`,
				unresolvedId,
				now,
			))
		}, TxReadCommitted, OptNoRetry())
		report, cursor, _ = removeExpiredCloseEvidenceWith(horizon, now, cursor, time.Now().Add(time.Minute), settings)
		connect.AssertEqual(t, report.FullPass, false)
		connect.AssertEqual(t, report.Removed, int64(0))
		connect.AssertEqual(t, present(unresolved), true)
		connect.AssertEqual(t, *cursor.LowWater, lateId)

		// the next day's first pass takes it, and keeps the late report
		tomorrow := now.Add(day)
		report, cursor, _ = removeExpiredCloseEvidenceWith(horizon, tomorrow, cursor, time.Now().Add(time.Minute), settings)
		connect.AssertEqual(t, report.FullPass, true)
		connect.AssertEqual(t, report.Removed, int64(1))
		connect.AssertEqual(t, present(unresolved), false)
		connect.AssertEqual(t, present(late), true)
		connect.AssertEqual(t, present(young), true)
		connect.AssertEqual(t, cursor.FullPassDay, tomorrow.UTC().Format(time.DateOnly))
	})
}

// A guard function that cannot be found keeps 91 days, whatever the horizon,
// and the horizon applies again once it is back.
func TestCloseEvidenceRetentionKeeps91DaysWithoutTheGuard(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		horizon := WithContractHorizon(ctx, 35*day)
		report := testCloseEvidenceReportAt(now.Add(-50*day), testContractIdAt(now.Add(-50*day)), now.Add(-50*day))
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)

		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, `ALTER FUNCTION contract_close_report_evidence_guard() RENAME TO contract_close_report_evidence_guard_away`))
		floor, horizonGuard := GetCloseEvidenceRetentionGuardFloor(ctx)
		connect.AssertEqual(t, floor, ContractRetention)
		connect.AssertEqual(t, horizonGuard, false)
		removal, _, _ := RemoveExpiredCloseEvidence(horizon, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, removal.Retention, ContractRetention)
		connect.AssertEqual(t, removal.Removed, int64(0))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, report) != "", true)

		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, `ALTER FUNCTION contract_close_report_evidence_guard_away() RENAME TO contract_close_report_evidence_guard`))
		floor, horizonGuard = GetCloseEvidenceRetentionGuardFloor(ctx)
		connect.AssertEqual(t, floor, CloseEvidenceRetentionFloor)
		connect.AssertEqual(t, horizonGuard, true)
		removal, _, _ = RemoveExpiredCloseEvidence(horizon, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, removal.Retention, 35*day)
		connect.AssertEqual(t, removal.Removed, int64(1))
	})
}

// The registry's retire pass under the 91-day guard, as before the horizon
// guard migration or after an operator restored it: a week 49 to 56 days past,
// expired at a 45-day horizon, stays, blocked by its precondition at the
// installed guard's 91 days rather than failing in its drop; with the horizon
// guard back, the same pass drops it.
func TestCloseEvidenceWeekRetireWaitsForTheHorizonGuard(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		horizon := WithContractHorizon(ctx, 45*day)
		testCloseEvidencePartitionedInThePast(t, ctx, now)
		week, leaf := testCloseEvidenceCreateWeek(t, ctx, PartitionGranularityWeekly.RangeStart(now).Add(-8*7*day+day))
		// a report of a contract the reaper deleted
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, testCloseEvidenceReportAt(week.Lower.Add(day), testContractIdAt(week.Lower), week.Lower.Add(day))), true)

		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, contractCloseReportEvidenceMoveGuardSql))
		report, _ := closeEvidencePartitionFamily.Retire(horizon, now, PartitionFamilyState{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, len(report.Dropped), 0)
		connect.AssertEqual(t, report.Blocked[leaf], closeEvidenceBlockerRetentionDay)
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, leaf), true)

		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, contractCloseReportEvidenceHorizonGuardSql))
		report, _ = closeEvidencePartitionFamily.Retire(horizon, now, PartitionFamilyState{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, report.Dropped, []string{leaf})
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, leaf), false)
	})
}
