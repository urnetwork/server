// The close evidence cutover against PostgreSQL (PARTITION-MAINTENANCE-PLAN.md
// C5): its lifecycle with dry runs, rollback and abort, inserts arriving at
// each step with barriers, a restart after every interrupted step, and the
// maintenance task abandoning a prepare that outlives its week's margin.
package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Fast retries for tests; nothing waits on a real busy table.
func testCloseEvidenceCutoverSettings() *CloseEvidencePartitionSettings {
	settings := DefaultCloseEvidencePartitionSettings()
	settings.PrepareLeadTime = 0
	settings.RetryInterval = 10 * time.Millisecond
	return settings
}

func testCloseEvidenceMigrate(t testing.TB, ctx context.Context, settings *CloseEvidencePartitionSettings, options CloseEvidencePartitionMigrateOptions) error {
	options.Settings = settings
	if options.Logf == nil {
		options.Logf = func(format string, args ...any) {
			t.Logf(format, args...)
		}
	}
	return MigrateCloseEvidenceToPartitions(ctx, options)
}

// The count and a digest of every evidence row, wherever it is: the table (or
// the parent's partitions) and, while a cutover is prepared, the staged parent.
func testCloseEvidenceChecksum(t testing.TB, ctx context.Context) (checksum string) {
	staged := ""
	if testCloseEvidenceExists(ctx, closeEvidenceStagedParentName()) {
		staged = ` UNION ALL SELECT * FROM public.contract_close_report_evidence_p`
	}
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(
			ctx,
			`
			SELECT count(*) || ':' || coalesce(md5(string_agg(evidence::text, ',' ORDER BY client_id, report_id)), '')
			FROM (SELECT * FROM public.contract_close_report_evidence`+staged+`) evidence
			`,
		).Scan(&checksum))
	})
	return
}

// The number of evidence rows, wherever they are (testCloseEvidenceChecksum).
func testCloseEvidenceCount(t testing.TB, ctx context.Context) int64 {
	staged := ""
	if testCloseEvidenceExists(ctx, closeEvidenceStagedParentName()) {
		staged = ` UNION ALL SELECT client_id FROM public.contract_close_report_evidence_p`
	}
	return testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM (SELECT client_id FROM public.contract_close_report_evidence`+staged+`) evidence`)
}

// The rows of the plain table itself, not the staged parent's.
func testCloseEvidencePlainCount(t testing.TB, ctx context.Context) int64 {
	return testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM ONLY public.contract_close_report_evidence`)
}

func testCloseEvidenceBoundValidated(t testing.TB, ctx context.Context) (exists bool, validated bool) {
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT convalidated FROM pg_constraint WHERE conname = 'contract_close_report_evidence_legacy_bound' AND conrelid = to_regclass('public.contract_close_report_evidence')`,
		)
		WithPgResult(result, err, func() {
			if result.Next() {
				exists = true
				Raise(result.Scan(&validated))
			}
		})
	})
	return
}

func testCloseEvidenceMoved(t testing.TB, ctx context.Context) (moved bool) {
	Tx(ctx, func(tx PgTx) {
		var err error
		moved, err = CloseEvidenceMovedOutsideTableInTx(ctx, tx)
		Raise(err)
	}, TxRepeatableRead)
	return
}

// The reports a cutover test starts with: one now, one accepted 100 days ago,
// one minted two weeks past the cutover week and one thirty weeks past it.
func testCloseEvidenceCutoverRows(ctx context.Context, now time.Time, cutoverWeek time.Time) (current, old, ahead, farAhead testCloseEvidenceReport) {
	current = testCloseEvidenceReportAt(now, NewId(), now)
	old = testCloseEvidenceReportAt(now.Add(-100*24*time.Hour), testContractIdAt(now.Add(-100*24*time.Hour)), now.Add(-100*24*time.Hour))
	ahead = testCloseEvidenceReportAt(cutoverWeek.Add(2*7*24*time.Hour+time.Hour), NewId(), now)
	farAhead = testCloseEvidenceReportAt(cutoverWeek.Add(30*7*24*time.Hour), NewId(), now)
	for _, report := range []testCloseEvidenceReport{current, old, ahead, farAhead} {
		if !testInsertCloseEvidenceReport(ctx, report) {
			panic(errors.New("a seed report conflicted"))
		}
	}
	return
}

// The cutover end to end on the migrated schema: dry runs change nothing;
// prepare moves the rows past the bound into the staged DEFAULT partition,
// refuses a new one until the swap and resumes as a no-op; the swap keeps every
// row and both guards on every partition, routes reports by id and creates the
// weeks ahead, moving the week's DEFAULT rows; rollback copies the rows written
// outside the legacy partition back, and refuses when they are too many; abort
// returns the moved rows to the table.
func TestCloseEvidencePartitionCutover(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		cutoverWeek := PartitionGranularityWeekly.RangeStart(now).Add(7 * 24 * time.Hour)
		settings := testCloseEvidenceCutoverSettings()
		current, old, ahead, farAhead := testCloseEvidenceCutoverRows(ctx, now, cutoverWeek)
		seeded := testCloseEvidenceChecksum(t, ctx)

		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{CutoverWeek: cutoverWeek, DryRun: true}))
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, closeEvidenceStagedParentName()), false)
		exists, _ := testCloseEvidenceBoundValidated(t, ctx)
		connect.AssertEqual(t, exists, false)

		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{CutoverWeek: cutoverWeek}))
		exists, validated := testCloseEvidenceBoundValidated(t, ctx)
		connect.AssertEqual(t, exists && validated, true)
		connect.AssertEqual(t, closeEvidenceStagedDefaultAttached(ctx), true)
		connect.AssertEqual(t, testCloseEvidencePlainCount(t, ctx), int64(2))
		connect.AssertEqual(t, testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM public.contract_close_report_evidence_default`), int64(2))
		connect.AssertEqual(t, testCloseEvidenceChecksum(t, ctx), seeded)
		connect.AssertEqual(t, testCloseEvidenceMoved(t, ctx), true)
		// resumes as a no-op
		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{CutoverWeek: cutoverWeek}))
		connect.AssertEqual(t, testCloseEvidenceChecksum(t, ctx), seeded)
		// another week is refused until this prepare is aborted
		if err := testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{CutoverWeek: cutoverWeek.Add(7 * 24 * time.Hour)}); err == nil {
			t.Fatal("a second cutover week was prepared")
		}

		// until the swap, a report past the bound is refused, and one before it
		// is admitted into the table
		pastBound := testCloseEvidenceReportAt(cutoverWeek.Add(time.Hour), NewId(), now)
		err := partitionRegistryRun(func() {
			testInsertCloseEvidenceReport(ctx, pastBound)
		})
		if err == nil || !strings.Contains(err.Error(), "contract_close_report_evidence_legacy_bound") {
			t.Fatalf("a report past the bound before the swap: %v", err)
		}
		admitted := testCloseEvidenceReportAt(now, NewId(), now)
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, admitted), true)

		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{Finalize: true, DryRun: true}))
		connect.AssertEqual(t, IsCloseEvidencePartitioned(ctx), false)
		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{Finalize: true}))
		connect.AssertEqual(t, IsCloseEvidencePartitioned(ctx), true)
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, closeEvidenceStagedParentName()), false)
		connect.AssertEqual(t, testCloseEvidenceMoved(t, ctx), false)
		if err := testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{Finalize: true}); err == nil {
			t.Fatal("a second swap was accepted")
		}

		// every row is where its id puts it
		weekOf := func(report testCloseEvidenceReport) string {
			return closeEvidencePartitionFamily.LeafName(closeEvidenceTable, closeEvidencePartitionFamily.RangeAt(ContractIdTime(report.reportId)))
		}
		for _, report := range []testCloseEvidenceReport{current, old, admitted} {
			connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, report), closeEvidenceLegacyLeafName())
		}
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, ahead), weekOf(ahead))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, farAhead), closeEvidenceDefaultLeafName())
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, pastBound), true)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, pastBound), weekOf(pastBound))
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, ahead), false)
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, old), false)
		// both guards hold on the parent and every partition
		for _, statement := range []string{
			`DELETE FROM contract_close_report_evidence`,
			`UPDATE contract_close_report_evidence SET checkpoint = true`,
			`TRUNCATE contract_close_report_evidence`,
			`DELETE FROM contract_close_report_evidence_legacy`,
			`TRUNCATE contract_close_report_evidence_legacy`,
			`TRUNCATE contract_close_report_evidence_default`,
		} {
			connect.AssertEqual(t, testCloseEvidenceImmutable(testCloseEvidenceExec(ctx, statement)), true)
		}
		swapped := testCloseEvidenceChecksum(t, ctx)

		// rollback refuses when more rows than its bound are outside the
		// legacy partition, and otherwise copies them back
		few := testCloseEvidenceCutoverSettings()
		few.RollbackMaxRows = 2
		if err := testCloseEvidenceMigrate(t, ctx, few, CloseEvidencePartitionMigrateOptions{Rollback: true}); err == nil {
			t.Fatal("a rollback of more rows than its bound was accepted")
		}
		connect.AssertEqual(t, IsCloseEvidencePartitioned(ctx), true)
		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{Rollback: true, DryRun: true}))
		connect.AssertEqual(t, IsCloseEvidencePartitioned(ctx), true)
		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{Rollback: true}))
		connect.AssertEqual(t, IsCloseEvidencePartitioned(ctx), false)
		connect.AssertEqual(t, testCloseEvidenceChecksum(t, ctx), swapped)
		exists, _ = testCloseEvidenceBoundValidated(t, ctx)
		connect.AssertEqual(t, exists, false)
		connect.AssertEqual(t, testCloseEvidenceImmutable(testCloseEvidenceExec(ctx, `DELETE FROM contract_close_report_evidence`)), true)
		connect.AssertEqual(t, testCloseEvidenceImmutable(testCloseEvidenceExec(ctx, `TRUNCATE contract_close_report_evidence`)), true)
		for _, index := range []string{"contract_close_report_evidence_pkey", "contract_close_original_census"} {
			connect.AssertEqual(t, testCloseEvidenceExists(ctx, index), true)
		}

		// prepared again and aborted, the moved rows return to the table
		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{CutoverWeek: cutoverWeek}))
		connect.AssertEqual(t, testCloseEvidenceMoved(t, ctx), true)
		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{Abort: true, DryRun: true}))
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, closeEvidenceStagedParentName()), true)
		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{Abort: true}))
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, closeEvidenceStagedParentName()), false)
		exists, _ = testCloseEvidenceBoundValidated(t, ctx)
		connect.AssertEqual(t, exists, false)
		connect.AssertEqual(t, testCloseEvidenceChecksum(t, ctx), swapped)
		connect.AssertEqual(t, testCloseEvidencePlainCount(t, ctx), int64(6))
	})
}

// A transaction left open on another connection, to hold a writer's lock
// while a cutover step runs.
type testCloseEvidenceOpenWriter struct {
	conn PgConn
	tx   PgTx
}

// Inserts the report in a transaction it leaves open.
func testOpenCloseEvidenceWriter(t testing.TB, ctx context.Context, report testCloseEvidenceReport) *testCloseEvidenceOpenWriter {
	conn, err := AcquireMaintenanceDbConn(ctx)
	testCloseEvidenceNoError(t, err)
	tx, err := conn.Begin(ctx)
	testCloseEvidenceNoError(t, err)
	_, err = tx.Exec(
		ctx,
		`
		INSERT INTO contract_close_report_evidence (
			client_id, report_id, contract_id, party,
			acked_byte_count, unacked_byte_count, checkpoint, accepted_at
		) VALUES ($1, $2, $3, 'source', $4, 0, false, $5)
		`,
		report.clientId, report.reportId, report.contractId, report.acked, report.acceptedAt,
	)
	testCloseEvidenceNoError(t, err)
	return &testCloseEvidenceOpenWriter{conn: conn, tx: tx}
}

func (self *testCloseEvidenceOpenWriter) commit(t testing.TB, ctx context.Context) {
	defer self.conn.Release()
	testCloseEvidenceNoError(t, self.tx.Commit(ctx))
}

// Writes keep arriving through the cutover, each step held at a barrier: a
// report before the bound is admitted and one past it refused once the bound
// is in place; a writer left open across VALIDATE neither blocks it nor is
// lost; the swap waits out a writer that holds the table (its lock wait times
// out and the retry succeeds once the writer commits); and inserts that queue
// behind the swap's lock route into the new layout once it commits, a report
// for the cutover week into its week.
func TestCloseEvidenceCutoverWithInsertsArriving(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		cutoverWeek := PartitionGranularityWeekly.RangeStart(now).Add(7 * 24 * time.Hour)
		testCloseEvidenceCutoverRows(ctx, now, cutoverWeek)

		var arrived []testCloseEvidenceReport
		var heldAcrossValidate *testCloseEvidenceOpenWriter
		settings := testCloseEvidenceCutoverSettings()
		settings.beforeCutoverStepForTest = func(step string) error {
			switch step {
			case "move":
				before := testCloseEvidenceReportAt(now, NewId(), now)
				if !testInsertCloseEvidenceReport(ctx, before) {
					t.Fatal("a report before the bound was not admitted")
				}
				arrived = append(arrived, before)
				err := partitionRegistryRun(func() {
					testInsertCloseEvidenceReport(ctx, testCloseEvidenceReportAt(cutoverWeek.Add(time.Hour), NewId(), now))
				})
				if err == nil {
					t.Fatal("a report past the bound was admitted before the swap")
				}
			case "validate":
				held := testCloseEvidenceReportAt(now, NewId(), now)
				heldAcrossValidate = testOpenCloseEvidenceWriter(t, ctx, held)
				arrived = append(arrived, held)
			}
			return nil
		}
		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{CutoverWeek: cutoverWeek}))
		exists, validated := testCloseEvidenceBoundValidated(t, ctx)
		connect.AssertEqual(t, exists && validated, true)
		heldAcrossValidate.commit(t, ctx)
		prepared := testCloseEvidenceCount(t, ctx)

		// a writer holds the table when the swap starts
		heldAcrossSwap := testCloseEvidenceReportAt(now, NewId(), now)
		var openWriter *testCloseEvidenceOpenWriter
		var releaseOnce sync.Once
		queuedCurrent := testCloseEvidenceReportAt(now, NewId(), now)
		queuedWeek := testCloseEvidenceReportAt(cutoverWeek.Add(24*time.Hour), NewId(), now)
		var queuedCurrentDone chan error
		var queuedWeekDone chan error
		finalize := testCloseEvidenceCutoverSettings()
		finalize.LockTimeout = 200 * time.Millisecond
		finalize.beforeCutoverStepForTest = func(step string) error {
			if step == "swap" {
				openWriter = testOpenCloseEvidenceWriter(t, ctx, heldAcrossSwap)
			}
			return nil
		}
		finalize.afterSwapLockForTest = func() {
			queuedCurrentDone = testInsertCloseEvidenceReportAsync(ctx, queuedCurrent)
			queuedWeekDone = testInsertCloseEvidenceReportAsync(ctx, queuedWeek)
			deadline := time.Now().Add(time.Minute)
			for testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM pg_locks WHERE relation = 'public.contract_close_report_evidence'::regclass AND NOT granted`) < 2 {
				if !time.Now().Before(deadline) {
					t.Fatal("the inserts did not queue behind the swap")
				}
				<-time.After(10 * time.Millisecond)
			}
		}
		err := testCloseEvidenceMigrate(t, ctx, finalize, CloseEvidencePartitionMigrateOptions{
			Finalize: true,
			Logf: func(format string, args ...any) {
				t.Logf(format, args...)
				// the swap's lock wait timed out behind the open writer
				if strings.HasPrefix(format, "lock busy") {
					releaseOnce.Do(func() {
						openWriter.commit(t, ctx)
					})
				}
			},
		})
		testCloseEvidenceNoError(t, err)
		connect.AssertEqual(t, <-queuedCurrentDone, nil)
		connect.AssertEqual(t, <-queuedWeekDone, nil)
		connect.AssertEqual(t, IsCloseEvidencePartitioned(ctx), true)

		for _, report := range append(arrived, heldAcrossSwap, queuedCurrent) {
			connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, report), closeEvidenceLegacyLeafName())
		}
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, queuedWeek), closeEvidencePartitionFamily.LeafName(closeEvidenceTable, closeEvidencePartitionFamily.RangeAt(cutoverWeek)))
		// every row from before the swap is still there, with the three
		// written around it
		connect.AssertEqual(t, testCloseEvidenceCount(t, ctx), prepared+3)
	})
}

var errTestCloseEvidenceInterrupted = errors.New("synthetic interruption")

// Each prepare step interrupted (including partway through the move) and run
// again completes the same prepared state with every row kept; each finalize
// step interrupted, including inside the swap's transaction, leaves the table
// prepared, and finalize run again completes.
func TestCloseEvidenceInterruptedCutoverRestarts(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		cutoverWeek := PartitionGranularityWeekly.RangeStart(now).Add(7 * 24 * time.Hour)
		testCloseEvidenceCutoverRows(ctx, now, cutoverWeek)
		// a third row past the bound, so the move takes several batches
		connect.AssertEqual(t, testInsertCloseEvidenceReport(ctx, testCloseEvidenceReportAt(cutoverWeek.Add(3*24*time.Hour), NewId(), now)), true)
		seeded := testCloseEvidenceChecksum(t, ctx)

		for _, step := range []string{"stage", "default", "bound", "move", "move batch", "validate"} {
			settings := testCloseEvidenceCutoverSettings()
			settings.MoveBatchSize = 1
			interrupted := false
			batches := 0
			settings.beforeCutoverStepForTest = func(at string) error {
				if at == "move batch" {
					batches += 1
				}
				// the move is interrupted after its first batch committed
				if at == step && !interrupted && (step != "move batch" || batches == 2) {
					interrupted = true
					return errTestCloseEvidenceInterrupted
				}
				return nil
			}
			err := testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{CutoverWeek: cutoverWeek})
			if !errors.Is(err, errTestCloseEvidenceInterrupted) {
				t.Fatalf("prepare interrupted at %s: %v", step, err)
			}
			connect.AssertEqual(t, testCloseEvidenceChecksum(t, ctx), seeded)
			testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, testCloseEvidenceCutoverSettings(), CloseEvidencePartitionMigrateOptions{CutoverWeek: cutoverWeek}))
			exists, validated := testCloseEvidenceBoundValidated(t, ctx)
			if !exists || !validated || !closeEvidenceStagedDefaultAttached(ctx) || testCloseEvidencePlainCount(t, ctx) != 2 {
				t.Fatalf("prepare run again after %s did not complete", step)
			}
			connect.AssertEqual(t, testCloseEvidenceChecksum(t, ctx), seeded)
			testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, testCloseEvidenceCutoverSettings(), CloseEvidencePartitionMigrateOptions{Abort: true}))
			connect.AssertEqual(t, testCloseEvidencePlainCount(t, ctx), int64(5))
		}

		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, testCloseEvidenceCutoverSettings(), CloseEvidencePartitionMigrateOptions{CutoverWeek: cutoverWeek}))
		for _, step := range []string{"probe", "swap", "inside the swap"} {
			settings := testCloseEvidenceCutoverSettings()
			if step == "inside the swap" {
				settings.afterSwapLockForTest = func() {
					panic(errTestCloseEvidenceInterrupted)
				}
			} else {
				settings.beforeCutoverStepForTest = func(at string) error {
					if at == step {
						return errTestCloseEvidenceInterrupted
					}
					return nil
				}
			}
			err := testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{Finalize: true})
			if !errors.Is(err, errTestCloseEvidenceInterrupted) {
				t.Fatalf("finalize interrupted at %s: %v", step, err)
			}
			connect.AssertEqual(t, IsCloseEvidencePartitioned(ctx), false)
			exists, validated := testCloseEvidenceBoundValidated(t, ctx)
			connect.AssertEqual(t, exists && validated, true)
			connect.AssertEqual(t, testCloseEvidenceChecksum(t, ctx), seeded)
		}
		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, testCloseEvidenceCutoverSettings(), CloseEvidencePartitionMigrateOptions{Finalize: true}))
		connect.AssertEqual(t, IsCloseEvidencePartitioned(ctx), true)
		connect.AssertEqual(t, testCloseEvidenceChecksum(t, ctx), seeded)
	})
}

// A prepare not finalized by AbandonMargin before its week is abandoned by
// the maintenance task, before its bound can refuse every report of that
// week; its moved rows return to the table. Earlier it is left alone.
func TestCloseEvidenceCutoverAbandonedBeforeItsWeek(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		cutoverWeek := PartitionGranularityWeekly.RangeStart(now).Add(7 * 24 * time.Hour)
		_, _, ahead, farAhead := testCloseEvidenceCutoverRows(ctx, now, cutoverWeek)
		seeded := testCloseEvidenceChecksum(t, ctx)
		settings := testCloseEvidenceCutoverSettings()
		logf := func(format string, args ...any) {
			t.Logf(format, args...)
		}

		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{CutoverWeek: cutoverWeek}))
		connect.AssertEqual(t, abandonStaleCloseEvidenceCutover(ctx, cutoverWeek.Add(-settings.AbandonMargin-time.Minute), settings, logf), false)
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, closeEvidenceStagedParentName()), true)
		connect.AssertEqual(t, abandonStaleCloseEvidenceCutover(ctx, cutoverWeek.Add(-settings.AbandonMargin), settings, logf), true)
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, closeEvidenceStagedParentName()), false)
		exists, _ := testCloseEvidenceBoundValidated(t, ctx)
		connect.AssertEqual(t, exists, false)
		connect.AssertEqual(t, testCloseEvidenceChecksum(t, ctx), seeded)
		connect.AssertEqual(t, testCloseEvidencePlainCount(t, ctx), int64(4))

		// the maintenance task does the same on its run, and still deletes the
		// expired evidence (the report accepted 100 days ago)
		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{CutoverWeek: cutoverWeek}))
		report, _ := MaintainContractPartitions(ctx, cutoverWeek.Add(-time.Hour), ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, report.AbandonedEvidenceCutover, true)
		connect.AssertEqual(t, report.EvidenceRemoved, int64(1))
		connect.AssertEqual(t, testCloseEvidenceExists(ctx, closeEvidenceStagedParentName()), false)
		connect.AssertEqual(t, testCloseEvidencePlainCount(t, ctx), int64(3))
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, ahead), closeEvidenceTable)
		connect.AssertEqual(t, testCloseEvidencePartitionOf(t, ctx, farAhead), closeEvidenceTable)
		// a finalized cutover is never abandoned
		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{CutoverWeek: cutoverWeek}))
		testCloseEvidenceNoError(t, testCloseEvidenceMigrate(t, ctx, settings, CloseEvidencePartitionMigrateOptions{Finalize: true}))
		connect.AssertEqual(t, abandonStaleCloseEvidenceCutover(ctx, cutoverWeek, settings, logf), false)
		connect.AssertEqual(t, IsCloseEvidencePartitioned(ctx), true)
	})
}
