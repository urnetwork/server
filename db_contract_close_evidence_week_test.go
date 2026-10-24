// The close evidence partitions against PostgreSQL (PARTITION-MAINTENANCE-PLAN.md
// C5): the move guard, a report minted ahead landing in DEFAULT and later in
// its week, a week's create racing an insert into it, the drop precondition's
// reasons, a straggler keeping its week while its other rows are row-deleted,
// and a drop refused without the guard's opt-in.
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// One report as the evidence holds it.
type testCloseEvidenceReport struct {
	clientId   Id
	reportId   Id
	contractId Id
	acceptedAt time.Time
	acked      int64
}

// A report minted at mintedAt by its client's clock, for contractId, accepted
// at acceptedAt by the server's.
func testCloseEvidenceReportAt(mintedAt time.Time, contractId Id, acceptedAt time.Time) testCloseEvidenceReport {
	return testCloseEvidenceReport{
		clientId:   NewId(),
		reportId:   testContractIdAt(mintedAt),
		contractId: contractId,
		acceptedAt: acceptedAt.UTC(),
		acked:      1,
	}
}

// Inserts the report as the writer does, and returns whether it was new.
func testInsertCloseEvidenceReport(ctx context.Context, report testCloseEvidenceReport) (inserted bool) {
	Tx(ctx, func(tx PgTx) {
		tag := RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO contract_close_report_evidence (
				client_id, report_id, contract_id, party,
				acked_byte_count, unacked_byte_count, checkpoint, accepted_at
			) VALUES ($1, $2, $3, 'source', $4, 0, false, $5)
			ON CONFLICT (client_id, report_id) DO NOTHING
			`,
			report.clientId, report.reportId, report.contractId, report.acked, report.acceptedAt,
		))
		inserted = tag.RowsAffected() == 1
	})
	return
}

// The partition holding the report, or "" when no partition does.
func testCloseEvidencePartitionOf(t testing.TB, ctx context.Context, report testCloseEvidenceReport) (partition string) {
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT tableoid::regclass::text FROM contract_close_report_evidence WHERE client_id = $1 AND report_id = $2`,
			report.clientId,
			report.reportId,
		)
		WithPgResult(result, err, func() {
			if result.Next() {
				Raise(result.Scan(&partition))
			}
		})
	})
	return
}

func testCloseEvidenceExists(ctx context.Context, relation string) (exists bool) {
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+relation).Scan(&exists))
	})
	return
}

// Runs statements in one maintenance transaction and returns its failure.
func testCloseEvidenceExec(ctx context.Context, statements ...string) error {
	return contractPartitionTx(ctx, func(tx PgTx) {
		for _, statement := range statements {
			RaisePgResult(tx.Exec(ctx, statement))
		}
	})
}

func testCloseEvidenceImmutable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "original contract close reports are immutable")
}

// The move guard admits the delete half of a move only once an identical copy
// is where the row moves to: from the plain table, the staged parent; from
// DEFAULT, the weekly partition of its report id's week. A copy that differs,
// a copy in another week, or a missing destination is refused, as are an
// update and a truncate under 'move', and retention keeps its 91-day rule.
func TestCloseEvidenceMoveGuard(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		insert := func(table string, report testCloseEvidenceReport) {
			testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, fmt.Sprintf(
				`
				INSERT INTO public.%s (client_id, report_id, contract_id, party, acked_byte_count, unacked_byte_count, checkpoint, accepted_at)
				VALUES ('%s', '%s', '%s', 'source', %d, 0, false, '%s')
				`,
				quoteIdentifier(table), report.clientId, report.reportId, report.contractId, report.acked, report.acceptedAt.Format(partitionTimestampMicrosecondLayout),
			)))
		}
		deleteAs := func(setting string, table string, report testCloseEvidenceReport) error {
			return testCloseEvidenceExec(
				ctx,
				fmt.Sprintf("SET LOCAL urnetwork.close_evidence_retention = '%s'", setting),
				fmt.Sprintf("DELETE FROM public.%s WHERE client_id = '%s' AND report_id = '%s'", quoteIdentifier(table), report.clientId, report.reportId),
			)
		}

		// out of the plain table: no staged parent, then a different copy,
		// then the identical copy
		plain := testCloseEvidenceReportAt(now, NewId(), now)
		insert(closeEvidenceTable, plain)
		connect.AssertEqual(t, testCloseEvidenceImmutable(deleteAs("move", closeEvidenceTable, plain)), true)
		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, `CREATE TABLE public.contract_close_report_evidence_p (LIKE public.contract_close_report_evidence INCLUDING DEFAULTS)`))
		different := plain
		different.acked = 2
		insert(closeEvidenceStagedParentName(), different)
		connect.AssertEqual(t, testCloseEvidenceImmutable(deleteAs("move", closeEvidenceTable, plain)), true)
		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, `DELETE FROM public.contract_close_report_evidence_p`))
		insert(closeEvidenceStagedParentName(), plain)
		// retention and no opt-in still refuse a recent row with its copy in place
		connect.AssertEqual(t, testCloseEvidenceImmutable(deleteAs("on", closeEvidenceTable, plain)), true)
		connect.AssertEqual(t, testCloseEvidenceImmutable(deleteAs("", closeEvidenceTable, plain)), true)
		// update and truncate stay refused under the move opt-in
		connect.AssertEqual(t, testCloseEvidenceImmutable(testCloseEvidenceExec(
			ctx,
			`SET LOCAL urnetwork.close_evidence_retention = 'move'`,
			`UPDATE contract_close_report_evidence SET checkpoint = true`,
		)), true)
		connect.AssertEqual(t, testCloseEvidenceImmutable(testCloseEvidenceExec(
			ctx,
			`SET LOCAL urnetwork.close_evidence_retention = 'move'`,
			`TRUNCATE contract_close_report_evidence`,
		)), true)
		connect.AssertEqual(t, deleteAs("move", closeEvidenceTable, plain), nil)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, plain), "")
		connect.AssertEqual(t, testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM public.contract_close_report_evidence_p WHERE client_id = $1`, plain.clientId), int64(1))
		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, `DROP TABLE public.contract_close_report_evidence_p`))

		// out of DEFAULT, a stand-in with the guard: the copy must be in the
		// weekly partition of the report id's week
		testCloseEvidenceNoError(t, testCloseEvidenceExec(
			ctx,
			`CREATE TABLE public.contract_close_report_evidence_default (LIKE public.contract_close_report_evidence INCLUDING DEFAULTS)`,
			`CREATE TRIGGER contract_close_report_evidence_guard BEFORE DELETE OR UPDATE ON public.contract_close_report_evidence_default FOR EACH ROW EXECUTE FUNCTION contract_close_report_evidence_guard()`,
		))
		week := closeEvidencePartitionFamily.RangeAt(now.Add(6 * 7 * 24 * time.Hour))
		weekLeaf := closeEvidencePartitionFamily.LeafName(closeEvidenceTable, week)
		nextLeaf := closeEvidencePartitionFamily.LeafName(closeEvidenceTable, closeEvidencePartitionFamily.RangeAt(week.Upper))
		// minted in the last millisecond of its week
		ahead := testCloseEvidenceReportAt(week.Upper.Add(-time.Millisecond), NewId(), now)
		insert(closeEvidenceDefaultLeafName(), ahead)
		connect.AssertEqual(t, testCloseEvidenceImmutable(deleteAs("move", closeEvidenceDefaultLeafName(), ahead)), true)
		for _, leaf := range []string{weekLeaf, nextLeaf} {
			testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, fmt.Sprintf(`CREATE TABLE public.%s (LIKE public.contract_close_report_evidence INCLUDING DEFAULTS)`, quoteIdentifier(leaf))))
		}
		insert(nextLeaf, ahead)
		connect.AssertEqual(t, testCloseEvidenceImmutable(deleteAs("move", closeEvidenceDefaultLeafName(), ahead)), true)
		insert(weekLeaf, ahead)
		connect.AssertEqual(t, deleteAs("move", closeEvidenceDefaultLeafName(), ahead), nil)

		// retention still deletes only rows past 91 days
		old := testCloseEvidenceReportAt(now.Add(-100*24*time.Hour), NewId(), now.Add(-100*24*time.Hour))
		insert(closeEvidenceTable, old)
		connect.AssertEqual(t, testCloseEvidenceImmutable(deleteAs("move", closeEvidenceTable, old)), true)
		connect.AssertEqual(t, deleteAs("on", closeEvidenceTable, old), nil)
	})
}

// Fails the test on a non-nil error.
func testCloseEvidenceNoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A report minted ahead of every created week is not refused: it lands in
// DEFAULT, where its identity stays unique, and moves into its week when the
// runway creates that week. Its exact retry still conflicts afterwards.
func TestCloseEvidenceAheadReportLandsInDefaultThenItsWeek(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		currentWeek := PartitionGranularityWeekly.RangeStart(now)
		Testing_PartitionCloseEvidence(ctx, currentWeek)
		connect.AssertEqual(t, IsCloseEvidencePartitioned(ctx), true)

		// six weeks ahead, past the runway of four
		mintedAt := currentWeek.Add(6*7*24*time.Hour + time.Hour)
		ahead := testCloseEvidenceReportAt(mintedAt, NewId(), now)
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, ahead), true)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, ahead), closeEvidenceDefaultLeafName())
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, ahead), false)
		current := testCloseEvidenceReportAt(now, NewId(), now)
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, current), true)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, current), closeEvidencePartitionFamily.LeafName(closeEvidenceTable, closeEvidencePartitionFamily.RangeAt(now)))

		// two weeks later the runway reaches the report's week
		report := closeEvidencePartitionFamily.EnsureRunway(ctx, now.Add(2*7*24*time.Hour))
		connect.AssertEqual(t, len(report.Errors), 0)
		weekLeaf := closeEvidencePartitionFamily.LeafName(closeEvidenceTable, closeEvidencePartitionFamily.RangeAt(mintedAt))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, ahead), weekLeaf)
		connect.AssertEqual(t, testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM contract_close_report_evidence_default`), int64(0))
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, ahead), false)
		// the moved row is the inserted row, and the week indexes its late reports
		connect.AssertEqual(t, testContractPartitionScalar[bool](
			t, ctx,
			`SELECT accepted_at = $3 AND acked_byte_count = 1 FROM contract_close_report_evidence WHERE client_id = $1 AND report_id = $2`,
			ahead.clientId, ahead.reportId, ahead.acceptedAt,
		), true)
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, closeEvidenceLateIndexName(weekLeaf)), true)
		// the week carries both guards
		connect.AssertEqual(t, testCloseEvidenceImmutable(testCloseEvidenceExec(ctx, fmt.Sprintf(`DELETE FROM public.%s`, quoteIdentifier(weekLeaf)))), true)
		connect.AssertEqual(t, testCloseEvidenceImmutable(testCloseEvidenceExec(ctx, fmt.Sprintf(`TRUNCATE public.%s`, quoteIdentifier(weekLeaf)))), true)
	})
}

// Waits until another transaction waits for a lock on relation: the state an
// insert reaches once it queues behind a held ACCESS EXCLUSIVE lock.
func testCloseEvidenceAwaitLockWaiter(t testing.TB, ctx context.Context, relation string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		var waiting bool
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(
				ctx,
				`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE relation = to_regclass($1) AND NOT granted)`,
				"public."+relation,
			).Scan(&waiting))
		})
		if waiting {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("no transaction came to wait for %s", relation)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Inserts the report in another goroutine; the result arrives when the
// insert finishes.
func testInsertCloseEvidenceReportAsync(ctx context.Context, report testCloseEvidenceReport) chan error {
	done := make(chan error, 1)
	go func() {
		err := partitionRegistryRun(func() {
			if !testInsertCloseEvidenceReport(ctx, report) {
				panic(errors.New("the report conflicted"))
			}
		})
		done <- err
	}()
	return done
}

// A week's create holds the parent while it moves the week's rows out of
// DEFAULT. An insert into the new week that arrives meanwhile waits on the
// parent, and once the create commits it routes into the new week: not to
// DEFAULT, and without failing a partition check. An insert for a week that
// still has no partition lands in DEFAULT.
func TestCloseEvidenceDefaultMoveRacesInsert(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		currentWeek := PartitionGranularityWeekly.RangeStart(now)
		Testing_PartitionCloseEvidence(ctx, currentWeek)

		week := closeEvidencePartitionFamily.RangeAt(currentWeek.Add(6 * 7 * 24 * time.Hour))
		later := closeEvidencePartitionFamily.RangeAt(week.Upper)
		moved := testCloseEvidenceReportAt(week.Lower.Add(time.Hour), NewId(), now)
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, moved), true)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, moved), closeEvidenceDefaultLeafName())

		racing := testCloseEvidenceReportAt(week.Lower.Add(2*time.Hour), NewId(), now)
		elsewhere := testCloseEvidenceReportAt(later.Lower.Add(time.Hour), NewId(), now)
		var racingDone chan error
		var elsewhereDone chan error
		settings := DefaultCloseEvidencePartitionSettings()
		settings.afterCreateLockForTest = func() {
			// both inserts queue behind the create's lock on the parent
			racingDone = testInsertCloseEvidenceReportAsync(ctx, racing)
			testCloseEvidenceAwaitLockWaiter(t, ctx, closeEvidenceTable)
			elsewhereDone = testInsertCloseEvidenceReportAsync(ctx, elsewhere)
			// both wait; the second waiter is visible as another ungranted row
			deadline := time.Now().Add(time.Minute)
			for testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM pg_locks WHERE relation = 'public.contract_close_report_evidence'::regclass AND NOT granted`) < 2 {
				if !time.Now().Before(deadline) {
					t.Fatal("the second insert did not come to wait")
				}
				<-time.After(10 * time.Millisecond)
			}
		}
		testCloseEvidenceNoError(t, createCloseEvidenceLeaf(ctx, closeEvidenceTable, week, settings))
		connect.AssertEqual(t, <-racingDone, nil)
		connect.AssertEqual(t, <-elsewhereDone, nil)

		weekLeaf := closeEvidencePartitionFamily.LeafName(closeEvidenceTable, week)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, moved), weekLeaf)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, racing), weekLeaf)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, elsewhere), closeEvidenceDefaultLeafName())
		// a second create of the same week finds it and changes nothing
		testCloseEvidenceNoError(t, createCloseEvidenceLeaf(ctx, closeEvidenceTable, week, DefaultCloseEvidencePartitionSettings()))
		connect.AssertEqual(t, testContractPartitionScalar[int64](t, ctx, fmt.Sprintf(`SELECT count(*) FROM public.%s`, quoteIdentifier(weekLeaf))), int64(2))
	})
}

// Each reason a week stays, read by the precondition from the week's own
// indexes: within a day of its retention, without its late index, holding a
// report accepted within 91 days, holding a report of an unresolved contract;
// and none once every row qualifies.
func TestCloseEvidenceWeekBlockerReasons(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		// a stand-in leaf for a week whose upper bound is r.Upper; the
		// precondition reads it by name
		standIn := func(r PartitionRange, lateIndex bool) string {
			leaf := closeEvidencePartitionFamily.LeafName(closeEvidenceTable, r)
			testCloseEvidenceNoError(t, testCloseEvidenceExec(
				ctx,
				fmt.Sprintf(`DROP TABLE IF EXISTS public.%s`, quoteIdentifier(leaf)),
				fmt.Sprintf(`CREATE TABLE public.%s (LIKE public.contract_close_report_evidence INCLUDING DEFAULTS INCLUDING INDEXES)`, quoteIdentifier(leaf)),
			))
			if lateIndex {
				testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, closeEvidenceLateIndexSql(leaf, r)))
			}
			return leaf
		}
		insertInto := func(leaf string, report testCloseEvidenceReport) {
			testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, fmt.Sprintf(
				`
				INSERT INTO public.%s (client_id, report_id, contract_id, party, acked_byte_count, unacked_byte_count, checkpoint, accepted_at)
				VALUES ('%s', '%s', '%s', 'source', 1, 0, false, '%s')
				`,
				quoteIdentifier(leaf), report.clientId, report.reportId, report.contractId, report.acceptedAt.Format(partitionTimestampMicrosecondLayout),
			)))
		}
		blocker := func(r PartitionRange) (blocker string) {
			MaintenanceDb(ctx, func(conn PgConn) {
				blocker = closeEvidenceWeekBlocker(ctx, conn, r)
			})
			return
		}

		// half a day past its retention
		recent := PartitionRange{Lower: now.Add(-98*24*time.Hour + 12*time.Hour), Upper: now.Add(-91*24*time.Hour + 12*time.Hour)}
		standIn(recent, true)
		connect.AssertEqual(t, blocker(recent), closeEvidenceBlockerRetentionDay)

		expired := PartitionRange{Lower: now.Add(-102 * 24 * time.Hour), Upper: now.Add(-95 * 24 * time.Hour)}
		standIn(expired, false)
		connect.AssertEqual(t, blocker(expired), closeEvidenceBlockerNoLateIndex)
		leaf := standIn(expired, true)
		connect.AssertEqual(t, blocker(expired), "")

		// a report accepted within the week, and one a day after it: neither
		// is late enough to block
		gone := testCloseEvidenceReportAt(expired.Lower.Add(time.Hour), testContractIdAt(expired.Lower), expired.Lower.Add(2*time.Hour))
		insertInto(leaf, gone)
		connect.AssertEqual(t, blocker(expired), "")
		// a straggler accepted ten days ago
		straggler := testCloseEvidenceReportAt(expired.Lower.Add(time.Hour), testContractIdAt(expired.Lower), now.Add(-10*24*time.Hour))
		insertInto(leaf, straggler)
		connect.AssertEqual(t, blocker(expired), closeEvidenceBlockerRecentReport)
		// the stand-in has no guard
		testCloseEvidenceNoError(t, testCloseEvidenceExec(ctx, fmt.Sprintf(`DELETE FROM public.%s WHERE client_id = '%s'`, quoteIdentifier(leaf), straggler.clientId)))
		// a late report older than 91 days does not block
		late := testCloseEvidenceReportAt(expired.Lower.Add(time.Hour), testContractIdAt(expired.Lower), now.Add(-93*24*time.Hour))
		insertInto(leaf, late)
		connect.AssertEqual(t, blocker(expired), "")

		// a report of an unresolved contract
		unresolvedContractId := testContractIdAt(expired.Lower)
		testInsertContractRows(t, ctx, unresolvedContractId, false)
		insertInto(leaf, testCloseEvidenceReportAt(expired.Lower.Add(time.Hour), unresolvedContractId, expired.Lower.Add(2*time.Hour)))
		connect.AssertEqual(t, blocker(expired), closeEvidenceBlockerUnresolved)
		Tx(ctx, func(tx PgTx) {
			// a canceled contract is resolved without usage credit
			RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET outcome = 'canceled', close_time = $2 WHERE contract_id = $1`, unresolvedContractId, NowUtc()))
		})
		connect.AssertEqual(t, blocker(expired), "")
	})
}

// Cuts over with the cutover week twenty weeks back, so past weeks can be
// created, expired and dropped against the database's own clock.
func testCloseEvidencePartitionedInThePast(t testing.TB, ctx context.Context, now time.Time) {
	Testing_PartitionCloseEvidence(ctx, PartitionGranularityWeekly.RangeStart(now).Add(-20*7*24*time.Hour))
	if !IsCloseEvidencePartitioned(ctx) {
		t.Fatal("the close evidence is not partitioned")
	}
}

// Creates the week holding at, through the family.
func testCloseEvidenceCreateWeek(t testing.TB, ctx context.Context, at time.Time) (PartitionRange, string) {
	r := closeEvidencePartitionFamily.RangeAt(at)
	testCloseEvidenceNoError(t, closeEvidencePartitionFamily.CreateRange(ctx, closeEvidenceTable, r))
	return r, closeEvidencePartitionFamily.LeafName(closeEvidenceTable, r)
}

// A week holding a report of an unresolved contract, or a straggler accepted
// within 91 days, stays, and its qualifying rows are row-deleted as before. A
// week whose every row qualifies is left to the drop, which takes it; a week
// not yet expired is left alone; the legacy and DEFAULT partitions are
// row-deleted. Once the unresolved contract resolves, its week drops too.
func TestCloseEvidenceStragglerWeekStaysAndItsRowsAreDeleted(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		testCloseEvidencePartitionedInThePast(t, ctx, now)
		weeksAgo := func(weeks int) time.Time {
			return PartitionGranularityWeekly.RangeStart(now).Add(-time.Duration(weeks)*7*24*time.Hour + 24*time.Hour)
		}
		acceptedOld := now.Add(-100 * 24 * time.Hour)

		unresolvedWeek, unresolvedLeaf := testCloseEvidenceCreateWeek(t, ctx, weeksAgo(16))
		recentWeek, recentLeaf := testCloseEvidenceCreateWeek(t, ctx, weeksAgo(15))
		_, cleanLeaf := testCloseEvidenceCreateWeek(t, ctx, weeksAgo(17))
		_, youngLeaf := testCloseEvidenceCreateWeek(t, ctx, weeksAgo(5))

		// week 16: one unresolved contract, one resolved, one gone
		unresolvedContractId := testContractIdAt(unresolvedWeek.Lower)
		testInsertContractRows(t, ctx, unresolvedContractId, false)
		resolvedContractId := testContractIdAt(unresolvedWeek.Lower.Add(time.Minute))
		testInsertContractRows(t, ctx, resolvedContractId, true)
		unresolved := testCloseEvidenceReportAt(weeksAgo(16), unresolvedContractId, acceptedOld)
		resolved := testCloseEvidenceReportAt(weeksAgo(16), resolvedContractId, acceptedOld)
		gone := testCloseEvidenceReportAt(weeksAgo(16), testContractIdAt(unresolvedWeek.Lower.Add(2*time.Minute)), acceptedOld)
		// week 15: a straggler accepted ten days ago, and a qualifying row
		straggler := testCloseEvidenceReportAt(weeksAgo(15), testContractIdAt(recentWeek.Lower), now.Add(-10*24*time.Hour))
		recentGone := testCloseEvidenceReportAt(weeksAgo(15), testContractIdAt(recentWeek.Lower.Add(time.Minute)), acceptedOld)
		// week 17: every row qualifies
		clean := testCloseEvidenceReportAt(weeksAgo(17), testContractIdAt(weeksAgo(17)), acceptedOld)
		// week 5 is young, even with a synthetic row older than 91 days
		young := testCloseEvidenceReportAt(weeksAgo(5), testContractIdAt(weeksAgo(17)), acceptedOld)
		// the legacy partition and DEFAULT (a week that was never created)
		legacy := testCloseEvidenceReportAt(weeksAgo(25), testContractIdAt(weeksAgo(25)), acceptedOld)
		uncreated := testCloseEvidenceReportAt(weeksAgo(10), testContractIdAt(weeksAgo(18)), acceptedOld)
		for _, report := range []testCloseEvidenceReport{unresolved, resolved, gone, straggler, recentGone, clean, young, legacy, uncreated} {
			connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, report), true)
		}
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, legacy), closeEvidenceLegacyLeafName())
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, uncreated), closeEvidenceDefaultLeafName())

		// the row deletes take the stragglers' weeks, DEFAULT and the legacy
		// partition, and leave the clean and young weeks to the drop
		removed, cursor, more := RemoveExpiredCloseEvidence(ctx, now, CloseEvidenceRemovalCursor{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, removed, int64(5))
		connect.AssertEqual(t, more, false)
		connect.AssertEqual(t, cursor, CloseEvidenceRemovalCursor{})
		for _, report := range []testCloseEvidenceReport{resolved, gone, recentGone, legacy, uncreated} {
			connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, report), "")
		}
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, unresolved), unresolvedLeaf)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, straggler), recentLeaf)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, clean), cleanLeaf)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, young), youngLeaf)

		// the drop takes the clean week only, and reports why the others stay
		report, state := closeEvidencePartitionFamily.Retire(ctx, now, PartitionFamilyState{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, report.Dropped, []string{cleanLeaf})
		connect.AssertEqual(t, report.Blocked[unresolvedLeaf], closeEvidenceBlockerUnresolved)
		connect.AssertEqual(t, report.Blocked[recentLeaf], closeEvidenceBlockerRecentReport)
		connect.AssertEqual(t, state, PartitionFamilyState{})
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, cleanLeaf), false)
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, youngLeaf), true)

		// once its contract resolves, the unresolved week drops
		Tx(ctx, func(tx PgTx) {
			// a canceled contract is resolved without usage credit
			RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET outcome = 'canceled', close_time = $2 WHERE contract_id = $1`, unresolvedContractId, NowUtc()))
		})
		report, _ = closeEvidencePartitionFamily.Retire(ctx, now, PartitionFamilyState{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, report.Dropped, []string{unresolvedLeaf})
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, unresolvedLeaf), false)
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, recentLeaf), true)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, straggler), recentLeaf)
	})
}

// A drop bypasses the evidence guard, so only the retention task's drop, under
// the guard's opt-in and for a week 91 days past, gets through the fence: the
// same drop without the opt-in, the fence alone without it, and a drop of a
// week not yet expired are refused and leave the week attached.
func TestCloseEvidenceDropWithoutOptInIsRefused(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		testCloseEvidencePartitionedInThePast(t, ctx, now)
		at := PartitionGranularityWeekly.RangeStart(now).Add(-16*7*24*time.Hour + 24*time.Hour)
		week, leaf := testCloseEvidenceCreateWeek(t, ctx, at)
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, testCloseEvidenceReportAt(at, testContractIdAt(at), now.Add(-100*24*time.Hour))), true)
		attached := func() bool {
			return testContractPartitionScalar[bool](
				t, ctx,
				`SELECT EXISTS (SELECT 1 FROM pg_inherits WHERE inhrelid = to_regclass($1) AND inhparent = to_regclass('public.contract_close_report_evidence'))`,
				"public."+leaf,
			)
		}

		unguarded := newCloseEvidencePartitionFamily()
		unguarded.Name = "synthetic_unguarded_close_evidence"
		unguarded.DropGuardSetting = ""
		_, err := unguarded.DropRange(ctx, week)
		if err == nil || !strings.Contains(err.Error(), "only by the retention task") {
			t.Fatalf("a drop without the opt-in: %v", err)
		}
		connect.AssertEqual(t, attached(), true)

		err = contractPartitionTx(ctx, func(tx PgTx) {
			fenceCloseEvidenceDrop(ctx, tx, week, DefaultCloseEvidencePartitionSettings())
		})
		if err == nil || !strings.Contains(err.Error(), "only by the retention task") {
			t.Fatalf("the fence without the opt-in: %v", err)
		}
		youngAt := PartitionGranularityWeekly.RangeStart(now).Add(-5*7*24*time.Hour + 24*time.Hour)
		young, youngLeaf := testCloseEvidenceCreateWeek(t, ctx, youngAt)
		_, err = closeEvidencePartitionFamily.DropRange(ctx, young)
		if err == nil || !strings.Contains(err.Error(), "is not 91 days past") {
			t.Fatalf("a drop of a young week: %v", err)
		}
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, youngLeaf), true)
		for _, r := range []PartitionRange{{Default: true}, {Legacy: true, Upper: week.Lower}} {
			if _, err := closeEvidencePartitionFamily.DropRange(ctx, r); err == nil {
				t.Fatalf("a drop of %+v was accepted", r)
			}
		}

		dropped, err := closeEvidencePartitionFamily.DropRange(ctx, week)
		testCloseEvidenceNoError(t, err)
		connect.AssertEqual(t, dropped, []string{leaf})
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, leaf), false)
	})
}
