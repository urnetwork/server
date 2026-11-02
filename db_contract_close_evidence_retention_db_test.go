// The close evidence row retention against PostgreSQL and Redis: its brake,
// its bounded batch, its delete by ctid, where its passes start, and the
// retention it applies when the guard is missing or still the 91-day one.
package server

import (
	"context"
	"encoding/json"
	"errors"
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
		connect.AssertEqual(t, removal.Stop, CloseEvidenceRemovalStopLockTimeout)
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

		// the day's first pass, stopped after its first batch by its pace: at a
		// row every ten seconds, the next batch would start past the run's
		// 5-second deadline
		slow := *settings
		slow.DefaultRowsPerSecond = 0.1
		report, cursor, more := removeExpiredCloseEvidenceWith(horizon, now, CloseEvidenceRemovalCursor{}, time.Now().Add(5*time.Second), &slow)
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

// The steps of a run that read its targets just before the cutover: the table
// by its plain name, now the partitioned parent's.
type testCloseEvidenceTargetsBeforeCutover struct {
	closeEvidenceRemovalStore
}

func (testCloseEvidenceTargetsBeforeCutover) Targets(ctx context.Context, now time.Time) ([]string, error) {
	return []string{closeEvidenceTable}, nil
}

// A cutover during a run leaves a batch naming the partitioned parent, where
// each leaf's first row has the same ctid. The batch reads nothing there and
// deletes nothing, so a report of an unresolved contract stays beside an
// expired report at the same ctid; each leaf then behaves as the plain table
// did.
func TestCloseEvidenceRetentionBatchOnAPartitionedParentReadsNothing(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		testCloseEvidencePartitionedInThePast(t, ctx, now)
		weeksAgo := func(weeks int) time.Time {
			return PartitionGranularityWeekly.RangeStart(now).Add(-time.Duration(weeks)*7*day + day)
		}
		_, unresolvedLeaf := testCloseEvidenceCreateWeek(t, ctx, weeksAgo(8))
		_, expiredLeaf := testCloseEvidenceCreateWeek(t, ctx, weeksAgo(7))
		unresolvedId := testContractIdAt(now.Add(-60 * day))
		testInsertContractRows(t, ctx, unresolvedId, false)
		unresolved := testCloseEvidenceReportAt(weeksAgo(8), unresolvedId, now.Add(-50*day))
		expired := testCloseEvidenceReportAt(weeksAgo(7), testContractIdAt(now.Add(-60*day)), now.Add(-50*day))
		for _, report := range []testCloseEvidenceReport{unresolved, expired} {
			connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
		}
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, unresolved), unresolvedLeaf)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, expired), expiredLeaf)
		ctidOf := func(report testCloseEvidenceReport) (ctid string) {
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(
					ctx,
					`SELECT ctid::text FROM contract_close_report_evidence WHERE client_id = $1 AND report_id = $2`,
					report.clientId,
					report.reportId,
				).Scan(&ctid))
			})
			return
		}
		connect.AssertEqual(t, ctidOf(unresolved), ctidOf(expired))

		settings := DefaultCloseEvidenceRetentionSettings()
		horizon := WithContractHorizon(ctx, 45*day)

		// a run that read its targets before the cutover ends its pass there,
		// with nothing deleted
		removal := &closeEvidenceRemoval{
			settings: settings,
			steps:    testCloseEvidenceTargetsBeforeCutover{},
			pace:     newCloseEvidenceRetentionPace(),
			deadline: time.Now().Add(time.Minute),
		}
		report, cursor, more := removal.run(horizon, now, CloseEvidenceRemovalCursor{})
		connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopDone)
		connect.AssertEqual(t, report.Removed, int64(0))
		connect.AssertEqual(t, more, false)
		connect.AssertEqual(t, *cursor.LowWater, ContractIdFloor(now.Add(-45*day)))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, unresolved), unresolvedLeaf)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, expired), expiredLeaf)

		argsFor := func(leaf string) closeEvidenceBatchArgs {
			return closeEvidenceBatchArgs{
				leaf:      leaf,
				upper:     ContractIdFloor(now.Add(-45 * day)),
				retention: 45 * day,
				size:      settings.BatchSize,
			}
		}
		batch, err := removeCloseEvidenceBatch(ctx, argsFor(closeEvidenceTable), settings)
		testCloseEvidenceNoError(t, err)
		connect.AssertEqual(t, batch.removed, int64(0))
		connect.AssertEqual(t, batch.scanned, int64(0))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, unresolved), unresolvedLeaf)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, expired), expiredLeaf)

		batch, err = removeCloseEvidenceBatch(ctx, argsFor(unresolvedLeaf), settings)
		testCloseEvidenceNoError(t, err)
		connect.AssertEqual(t, batch.removed, int64(0))
		connect.AssertEqual(t, batch.scanned, int64(1))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, unresolved), unresolvedLeaf)

		batch, err = removeCloseEvidenceBatch(ctx, argsFor(expiredLeaf), settings)
		testCloseEvidenceNoError(t, err)
		connect.AssertEqual(t, batch.removed, int64(1))
		connect.AssertEqual(t, batch.scanned, int64(1))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, expired), "")
	})
}

// A batch gives up at its statement timeout instead of holding the transaction
// horizon: the slow statement rolls back, the run says why, and the next run
// deletes the row.
func TestCloseEvidenceRetentionBatchBoundsItsStatement(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		horizon := WithContractHorizon(ctx, 45*day)
		report := testCloseEvidenceReportAt(now.Add(-50*day), testContractIdAt(now.Add(-50*day)), now.Add(-50*day))
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
		testCloseEvidenceNoError(t, testCloseEvidenceExec(
			ctx,
			`CREATE FUNCTION test_close_evidence_slow_delete() RETURNS trigger LANGUAGE plpgsql AS $slow$ BEGIN PERFORM pg_sleep(20); RETURN OLD; END $slow$`,
			`CREATE TRIGGER test_close_evidence_slow_delete BEFORE DELETE ON contract_close_report_evidence FOR EACH ROW EXECUTE FUNCTION test_close_evidence_slow_delete()`,
		))

		settings := DefaultCloseEvidenceRetentionSettings()
		settings.StatementTimeout = time.Second
		start := time.Now()
		removal, _, more := removeExpiredCloseEvidenceWith(horizon, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute), settings)
		if waited := time.Since(start); 15*time.Second < waited {
			t.Fatalf("the batch ran %s past its statement timeout", waited)
		}
		connect.AssertEqual(t, removal.Stop, CloseEvidenceRemovalStopStatementTimeout)
		connect.AssertEqual(t, removal.Removed, int64(0))
		connect.AssertEqual(t, more, false)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, report) != "", true)

		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, `DROP TRIGGER test_close_evidence_slow_delete ON contract_close_report_evidence`))
		removal, _, _ = removeExpiredCloseEvidenceWith(horizon, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute), settings)
		connect.AssertEqual(t, removal.Removed, int64(1))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, report), "")
	})
}

// Waits until a batch's statement waits for a lock.
func testCloseEvidenceAwaitBatchLockWait(t testing.TB, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		var waiting bool
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(
				ctx,
				`SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%scanned AS MATERIALIZED%'
				)`,
			).Scan(&waiting))
		})
		if waiting {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatal("no batch came to wait for a lock")
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A row the batch read changes under its delete by ctid: another transaction
// is deleting it when the delete reaches it. Once that commits the batch skips
// the row and deletes only the others, and once it rolls back the batch deletes
// the row as well. Either way the statement reads what it read and no other row
// goes.
func TestCloseEvidenceRetentionBatchFollowsARowChangingUnderIt(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		settings := DefaultCloseEvidenceRetentionSettings()
		settings.LockTimeout = time.Minute
		// a report inside the retention, which stays through both
		young := testCloseEvidenceReportAt(now.Add(-30*day), testContractIdAt(now.Add(-30*day)), now.Add(-30*day))
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, young), true)

		for _, test := range []struct {
			name    string
			commit  bool
			removed int64
		}{
			{name: "the other delete commits", commit: true, removed: 2},
			{name: "the other delete rolls back", commit: false, removed: 3},
		} {
			reports := []testCloseEvidenceReport{}
			for range 3 {
				report := testCloseEvidenceReportAt(now.Add(-50*day), testContractIdAt(now.Add(-50*day)), now.Add(-50*day))
				connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
				reports = append(reports, report)
			}
			changing := reports[1]

			deleting := make(chan struct{})
			release := make(chan struct{})
			otherDone := make(chan error, 1)
			go func() {
				otherDone <- contractPartitionTx(ctx, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, `SET LOCAL urnetwork.close_evidence_retention = 'on'`))
					RaisePgResult(tx.Exec(
						ctx,
						`DELETE FROM contract_close_report_evidence WHERE client_id = $1 AND report_id = $2`,
						changing.clientId,
						changing.reportId,
					))
					close(deleting)
					<-release
					if !test.commit {
						panic(errors.New("roll back"))
					}
				})
			}()
			<-deleting

			type outcome struct {
				batch closeEvidenceBatch
				err   error
			}
			batchDone := make(chan outcome, 1)
			go func() {
				batch, err := removeCloseEvidenceBatch(ctx, closeEvidenceBatchArgs{
					leaf:      closeEvidenceTable,
					upper:     ContractIdFloor(now.Add(-45 * day)),
					retention: 45 * day,
					size:      settings.BatchSize,
				}, settings)
				batchDone <- outcome{batch: batch, err: err}
			}()
			testCloseEvidenceAwaitBatchLockWait(t, ctx)
			close(release)
			otherErr := <-otherDone
			if test.commit {
				testCloseEvidenceNoError(t, otherErr)
			} else if otherErr == nil {
				t.Fatalf("%s: the other delete did not roll back", test.name)
			}
			done := <-batchDone
			testCloseEvidenceNoError(t, done.err)
			if done.batch.removed != test.removed || done.batch.scanned != 3 {
				t.Fatalf("%s: removed %d of %d rows read, want %d of 3", test.name, done.batch.removed, done.batch.scanned, test.removed)
			}
			for _, report := range reports {
				connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, report), "")
			}
			connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, young) != "", true)
		}
	})
}

// The retention lengthens during a pass: a row the longer retention keeps
// stays, and when it shortens again the same day, the next pass deletes it.
func TestCloseEvidenceRetentionLengthenedMidPass(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		short := WithContractHorizon(ctx, 45*day)
		long := WithContractHorizon(ctx, 60*day)
		settings := DefaultCloseEvidenceRetentionSettings()
		settings.BatchSize = 1
		slow := *settings
		slow.DefaultRowsPerSecond = 0.1

		first := testCloseEvidenceReportAt(now.Add(-50*day), testContractIdAt(now.Add(-50*day)), now.Add(-50*day))
		second := testCloseEvidenceReportAt(now.Add(-49*day), testContractIdAt(now.Add(-49*day)), now.Add(-49*day))
		for _, report := range []testCloseEvidenceReport{first, second} {
			connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
		}

		// stopped after the first row by its pace
		report, cursor, more := removeExpiredCloseEvidenceWith(short, now, CloseEvidenceRemovalCursor{}, time.Now().Add(5*time.Second), &slow)
		connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopBudget)
		connect.AssertEqual(t, report.Removed, int64(1))
		connect.AssertEqual(t, more, true)
		connect.AssertEqual(t, cursor.InProgress(), true)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, first), "")

		// 60 days keeps the other
		report, cursor, more = removeExpiredCloseEvidenceWith(long, now, cursor, time.Now().Add(time.Minute), settings)
		connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopDone)
		connect.AssertEqual(t, report.Retention, 60*day)
		connect.AssertEqual(t, report.Removed, int64(0))
		connect.AssertEqual(t, more, false)
		connect.AssertEqual(t, *cursor.LowWater, ContractIdFloor(now.Add(-60*day)))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, second) != "", true)

		// back to 45
		report, _, _ = removeExpiredCloseEvidenceWith(short, now, cursor, time.Now().Add(time.Minute), settings)
		connect.AssertEqual(t, report.Stop, CloseEvidenceRemovalStopDone)
		connect.AssertEqual(t, report.Removed, int64(1))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, second), "")
	})
}

// The contract task's arguments carry the cursor from run to run, through JSON
// as the task stores them. Arguments from before 2026-11-02 resume at their
// contract, above an older row; a run with no time left keeps the pass in the
// new field; and the next day's first pass takes the older row.
func TestMaintainContractPartitionsCarriesTheEvidenceCursor(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		const day = 24 * time.Hour
		horizon := WithContractHorizon(ctx, 45*day)
		older := testCloseEvidenceReportAt(now.Add(-60*day), testContractIdAt(now.Add(-60*day)), now.Add(-60*day))
		newer := testCloseEvidenceReportAt(now.Add(-50*day), testContractIdAt(now.Add(-50*day)), now.Add(-50*day))
		for _, report := range []testCloseEvidenceReport{older, newer} {
			connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
		}
		throughTheTask := func(cursor ContractRetentionCursor) (stored ContractRetentionCursor) {
			encoded, err := json.Marshal(cursor)
			testCloseEvidenceNoError(t, err)
			testCloseEvidenceNoError(t, json.Unmarshal(encoded, &stored))
			return
		}

		legacy := ContractRetentionCursor{EvidenceCursor: &newer.contractId}
		report, next := MaintainContractPartitions(horizon, now, throughTheTask(legacy), 0)
		connect.AssertEqual(t, report.EvidenceStop, CloseEvidenceRemovalStopBudget)
		connect.AssertEqual(t, report.EvidenceRemoved, int64(0))
		connect.AssertEqual(t, report.More, true)
		connect.AssertEqual(t, next.EvidenceCursor == nil, true)
		connect.AssertEqual(t, next.EvidenceLeaf, "")
		connect.AssertEqual(t, next.EvidenceRemoval.InProgress(), true)
		connect.AssertEqual(t, *next.EvidenceRemoval.ContractId, newer.contractId)

		report, next = MaintainContractPartitions(horizon, now, throughTheTask(next), time.Minute)
		connect.AssertEqual(t, report.EvidenceStop, CloseEvidenceRemovalStopDone)
		connect.AssertEqual(t, report.EvidenceRemoved, int64(1))
		connect.AssertEqual(t, next.EvidenceRemoval.InProgress(), false)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, newer), "")
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, older) != "", true)

		report, _ = MaintainContractPartitions(horizon, now, throughTheTask(next), time.Minute)
		connect.AssertEqual(t, report.EvidenceRemoved, int64(1))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, older), "")
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
