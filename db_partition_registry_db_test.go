// The partition registry against the migrated test database, on synthetic
// families whose tables each test makes: create-ahead and its catch-up after
// downtime, canonical bounds and leaves, the drop's precondition rechecked in
// the transaction that detaches, the guard setting, parent and leaf
// statistics, and the resume of an interrupted archive step. Fixed clocks and
// barriers; no sleep orders anything.
package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A daily family on synthetic_registry_event, which the test creates
// partitioned, with a TRUNCATE guard to repeat on every leaf.
func testRegistryEventFamily(t testing.TB, ctx context.Context) *PartitionFamily {
	t.Helper()
	testRegistryExec(t, ctx, `
		CREATE TABLE synthetic_registry_event (
			event_id uuid NOT NULL,
			observed_at timestamp NOT NULL,
			held boolean NOT NULL DEFAULT false,
			CHECK (event_id IS NOT NULL),
			PRIMARY KEY (event_id, observed_at)
		) PARTITION BY RANGE (observed_at);
		CREATE FUNCTION synthetic_registry_event_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
		BEGIN RAISE EXCEPTION 'synthetic events are immutable'; END $guard$;
		CREATE TRIGGER synthetic_registry_event_truncate_guard BEFORE TRUNCATE ON synthetic_registry_event
			FOR EACH STATEMENT EXECUTE FUNCTION synthetic_registry_event_guard();
	`)
	return &PartitionFamily{
		Name:           "synthetic_registry_event",
		Description:    "a synthetic daily family",
		Tables:         []string{"synthetic_registry_event"},
		KeyColumn:      "observed_at",
		KeyKind:        PartitionKeyTimestamp,
		Granularity:    PartitionGranularityDaily,
		AheadRanges:    3,
		Retention:      7 * 24 * time.Hour,
		LeafReloptions: []string{"autovacuum_vacuum_scale_factor=0"},
	}
}

func testRegistryExec(t testing.TB, ctx context.Context, sql string, args ...any) {
	t.Helper()
	MaintenanceDb(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, sql, args...))
	}, OptReadWrite(), OptNoRetry())
}

func testRegistryScalar[T any](t testing.TB, ctx context.Context, sql string, args ...any) (value T) {
	t.Helper()
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, sql, args...).Scan(&value))
	})
	return
}

func testRegistryLeafNames(t testing.TB, ctx context.Context, family *PartitionFamily, table string) (names []string) {
	t.Helper()
	var leaves []PartitionLeaf
	var err error
	MaintenanceDb(ctx, func(conn PgConn) {
		leaves, err = family.ListLeaves(ctx, conn, table)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, leaf := range leaves {
		names = append(names, leaf.Name)
	}
	return
}

// The days from the one holding from through count days after it, by name.
func testRegistryDayNames(family *PartitionFamily, table string, from time.Time, count int) (names []string) {
	start := family.Granularity.RangeStart(from)
	for i := 0; i <= count; i += 1 {
		names = append(names, family.LeafName(table, family.Granularity.RangeOf(family.Granularity.AddRanges(start, i))))
	}
	return
}

// The runway creates today and the days ahead, a repeat creates nothing, and
// after ten days of downtime one pass creates every missing day from the
// current one, so an insert at the new clock lands at once. The creation
// window starts at CreateFrom for late writes.
func TestPartitionRegistryRunwayCatchesUpAfterDowntime(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		family := testRegistryEventFamily(t, ctx)
		// a synthetic month out, so a real midnight moves nothing
		now := PartitionGranularityDaily.RangeStart(NowUtc().AddDate(0, 1, 0)).Add(13 * time.Hour)

		report := family.EnsureRunway(ctx, now)
		connect.AssertEqual(t, report.Partitioned, true)
		connect.AssertEqual(t, len(report.Errors), 0)
		connect.AssertEqual(t, report.Created, testRegistryDayNames(family, "synthetic_registry_event", now, 3))
		connect.AssertEqual(t, report.CurrentCovered, true)
		connect.AssertEqual(t, report.FutureRanges, 3)

		report = family.EnsureRunway(ctx, now.Add(time.Hour))
		connect.AssertEqual(t, len(report.Created), 0)

		later := now.AddDate(0, 0, 10)
		report = family.EnsureRunway(ctx, later)
		connect.AssertEqual(t, report.Created, testRegistryDayNames(family, "synthetic_registry_event", later, 3))
		connect.AssertEqual(t, report.CurrentCovered, true)
		testRegistryExec(t, ctx, `INSERT INTO synthetic_registry_event (event_id, observed_at) VALUES ($1, $2)`, NewId(), later.Add(time.Hour))

		// late writes: the window starts two days back
		late := *family
		late.CreateFrom = func(ctx context.Context, now time.Time) time.Time {
			return now.AddDate(0, 0, -2)
		}
		report = late.EnsureRunway(ctx, later.AddDate(0, 0, 1))
		connect.AssertEqual(t, report.Created, []string{
			family.LeafName("synthetic_registry_event", family.RangeAt(later.AddDate(0, 0, -1))),
			family.LeafName("synthetic_registry_event", family.RangeAt(later.AddDate(0, 0, 4))),
		})
		report = late.EnsureRunway(ctx, later.AddDate(0, 0, 6))
		connect.AssertEqual(t, slices.Contains(report.Created, family.LeafName("synthetic_registry_event", family.RangeAt(later.AddDate(0, 0, 4)))), false)
		connect.AssertEqual(t, slices.Contains(report.Created, family.LeafName("synthetic_registry_event", family.RangeAt(later.AddDate(0, 0, 5)))), true)

		// a writer's own create for a day the runway does not keep
		far := later.AddDate(0, 0, 30)
		created, err := family.EnsureRangeFor(ctx, far)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, created, []string{family.LeafName("synthetic_registry_event", family.RangeAt(far))})
		created, err = family.EnsureRangeFor(ctx, far.Add(time.Hour))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(created), 0)
	})
}

// The default create names each leaf for its range and bounds it on 00:00 UTC
// days (or Monday 00:00 UTC weeks of ids), with the parent's checks, the
// leaf storage settings and the parent's TRUNCATE guard repeated on it; the
// catalog reads it back as the registry's own; a DEFAULT partition is made
// when declared.
func TestPartitionRegistryCreatesCanonicalLeaves(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		family := testRegistryEventFamily(t, ctx)
		day := PartitionGranularityDaily.RangeStart(NowUtc().AddDate(0, 1, 0))
		connect.AssertEqual(t, len(family.EnsureRunway(ctx, day).Errors), 0)
		leaf := family.LeafName("synthetic_registry_event", family.RangeAt(day))
		connect.AssertEqual(t, leaf, "synthetic_registry_event_p"+day.Format("20060102"))
		bound := testRegistryScalar[string](t, ctx, `SELECT pg_get_expr(relpartbound, oid) FROM pg_class WHERE oid = to_regclass($1)`, "public."+leaf)
		connect.AssertEqual(t, bound, fmt.Sprintf("FOR VALUES FROM ('%s') TO ('%s')", day.Format("2006-01-02 15:04:05"), day.AddDate(0, 0, 1).Format("2006-01-02 15:04:05")))
		var leaves []PartitionLeaf
		MaintenanceDb(ctx, func(conn PgConn) {
			var err error
			leaves, err = family.ListLeaves(ctx, conn, "synthetic_registry_event")
			Raise(err)
		})
		for _, listed := range leaves {
			connect.AssertEqual(t, listed.Canonical, true)
		}
		connect.AssertEqual(t, testRegistryScalar[bool](t, ctx, `SELECT 'autovacuum_vacuum_scale_factor=0' = ANY(reloptions) FROM pg_class WHERE oid = to_regclass($1)`, "public."+leaf), true)
		connect.AssertEqual(t, testRegistryScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = to_regclass($1) AND tgname = 'synthetic_registry_event_truncate_guard')`, "public."+leaf), true)
		connect.AssertEqual(t, testRegistryScalar[int](t, ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid = to_regclass($1) AND contype = 'c'`, "public."+leaf), 1)
		// a TRUNCATE naming the leaf meets the guard
		err := partitionRegistryRun(func() {
			testRegistryExec(t, ctx, fmt.Sprintf(`TRUNCATE %s`, leaf))
		})
		connect.AssertEqual(t, err != nil && strings.Contains(err.Error(), "immutable"), true)

		// weekly ids with a DEFAULT partition
		testRegistryExec(t, ctx, `CREATE TABLE synthetic_registry_week (report_id uuid NOT NULL PRIMARY KEY) PARTITION BY RANGE (report_id)`)
		weekly := &PartitionFamily{
			Name: "synthetic_registry_week", Tables: []string{"synthetic_registry_week"}, KeyColumn: "report_id",
			KeyKind: PartitionKeyId, Granularity: PartitionGranularityWeekly, AheadRanges: 2, DefaultPartition: true,
		}
		report := weekly.EnsureRunway(ctx, day)
		connect.AssertEqual(t, len(report.Errors), 0)
		connect.AssertEqual(t, slices.Contains(report.Created, "synthetic_registry_week_default"), true)
		week := weekly.RangeAt(day)
		weekLeaf := weekly.LeafName("synthetic_registry_week", week)
		bound = testRegistryScalar[string](t, ctx, `SELECT pg_get_expr(relpartbound, oid) FROM pg_class WHERE oid = to_regclass($1)`, "public."+weekLeaf)
		connect.AssertEqual(t, bound, fmt.Sprintf("FOR VALUES FROM ('%s') TO ('%s')", ContractIdFloor(week.Lower), ContractIdFloor(week.Upper)))
		connect.AssertEqual(t, week.Lower.Weekday(), time.Monday)
		// an id outside every week lands in the DEFAULT partition
		testRegistryExec(t, ctx, `INSERT INTO synthetic_registry_week (report_id) VALUES ($1)`, ContractIdFloor(day.AddDate(1, 0, 0)))
		connect.AssertEqual(t, testRegistryScalar[string](t, ctx, `SELECT tableoid::regclass::text FROM synthetic_registry_week`), "synthetic_registry_week_default")
		expired, err := weekly.ExpiredRanges(ctx, day.AddDate(5, 0, 0))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(expired), 0)
	})
}

// The drop rechecks its precondition in the transaction that detaches: a hold
// committed after the check before the drop, while the drop waits at its
// fence, is seen once the leaf is detached, and the whole drop rolls back with
// the leaf attached and its rows intact. The guard setting is on inside the
// drop transaction only. With the hold cleared the range drops.
func TestPartitionRegistryDropRechecksInTheDetachTransaction(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		family := testRegistryEventFamily(t, ctx)
		day := PartitionGranularityDaily.RangeStart(NowUtc().AddDate(0, 1, 0))
		connect.AssertEqual(t, len(family.EnsureRunway(ctx, day).Errors), 0)
		r := family.RangeAt(day)
		leaf := family.LeafName("synthetic_registry_event", r)
		testRegistryExec(t, ctx, `INSERT INTO synthetic_registry_event (event_id, observed_at) VALUES ($1, $2)`, NewId(), day.Add(time.Hour))

		guarded := *family
		guarded.DropGuardSetting = "urnetwork.synthetic_registry_retention"
		settings := []string{}
		guarded.DropPrecondition = func(ctx context.Context, conn PgQuerier, r PartitionRange) string {
			var setting string
			Raise(conn.QueryRow(ctx, `SELECT COALESCE(current_setting('urnetwork.synthetic_registry_retention', true), '')`).Scan(&setting))
			settings = append(settings, setting)
			var held bool
			Raise(conn.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM public.%s WHERE held)`, quoteIdentifier(guarded.LeafName("synthetic_registry_event", r)))).Scan(&held))
			if held {
				return "a held event"
			}
			return ""
		}
		fenced := make(chan struct{})
		release := make(chan struct{})
		guarded.DropFence = func(ctx context.Context, tx PgTx, r PartitionRange) {
			close(fenced)
			<-release
		}
		// the precondition passes before the drop
		MaintenanceDb(ctx, func(conn PgConn) {
			connect.AssertEqual(t, guarded.DropPrecondition(ctx, conn, r), "")
		})
		connect.AssertEqual(t, settings, []string{""})

		dropped := make(chan error, 1)
		go func() {
			_, err := guarded.DropRange(ctx, r)
			dropped <- err
		}()
		<-fenced
		// the hold commits while the drop waits at its fence, before any detach
		testRegistryExec(t, ctx, `INSERT INTO synthetic_registry_event (event_id, observed_at, held) VALUES ($1, $2, true)`, NewId(), day.Add(2*time.Hour))
		close(release)
		err := <-dropped
		var blocked *PartitionDropBlockedError
		connect.AssertEqual(t, errors.As(err, &blocked), true)
		connect.AssertEqual(t, blocked.Blocker, "a held event")
		connect.AssertEqual(t, settings, []string{"", "on"})
		connect.AssertEqual(t, slices.Contains(testRegistryLeafNames(t, ctx, family, "synthetic_registry_event"), leaf), true)
		connect.AssertEqual(t, testRegistryScalar[int](t, ctx, `SELECT count(*) FROM synthetic_registry_event WHERE $1 <= observed_at AND observed_at < $2`, r.Lower, r.Upper), 2)
		connect.AssertEqual(t, testRegistryScalar[string](t, ctx, `SELECT COALESCE(current_setting('urnetwork.synthetic_registry_retention', true), '')`), "")

		testRegistryExec(t, ctx, `UPDATE synthetic_registry_event SET held = false`)
		guarded.DropFence = nil
		names, err := guarded.DropRange(ctx, r)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, names, []string{leaf})
		connect.AssertEqual(t, slices.Contains(testRegistryLeafNames(t, ctx, family, "synthetic_registry_event"), leaf), false)
		connect.AssertEqual(t, testRegistryScalar[bool](t, ctx, `SELECT to_regclass($1) IS NULL`, "public."+leaf), true)
	})
}

// The retirement pass drops a range once its upper bound is the retention
// past, the bound itself included, and not before; a kept legacy partition and
// the DEFAULT partition stay.
func TestPartitionRegistryRetireDropsAtTheRetention(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		family := testRegistryEventFamily(t, ctx)
		day := PartitionGranularityDaily.RangeStart(NowUtc().AddDate(0, 1, 0))
		connect.AssertEqual(t, len(family.EnsureRunway(ctx, day).Errors), 0)
		r := family.RangeAt(day)
		leaf := family.LeafName("synthetic_registry_event", r)
		deadline := time.Now().Add(time.Minute)

		report, _ := family.Retire(ctx, r.Upper.Add(family.Retention).Add(-time.Microsecond), PartitionFamilyState{}, deadline)
		connect.AssertEqual(t, len(report.Dropped), 0)
		report, _ = family.Retire(ctx, r.Upper.Add(family.Retention), PartitionFamilyState{}, deadline)
		connect.AssertEqual(t, report.Dropped, []string{leaf})
		connect.AssertEqual(t, len(report.Blocked), 0)
	})
}

// Parents are analyzed once per range period, so their statistics exist
// although autovacuum never analyzes a partitioned parent; a closed leaf is
// analyzed once its delay passes, and not again.
func TestPartitionRegistryAnalyzesParentsAndClosedLeaves(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		family := testRegistryEventFamily(t, ctx)
		family.LeafAnalyzeDelay = time.Hour
		// autovacuum must not analyze the leaves behind the test's back
		family.LeafReloptions = []string{"autovacuum_enabled=false"}
		// a day in the past, so the statistics clock of an analyze, which is
		// real, is after the day's settled time
		day := PartitionGranularityDaily.RangeStart(NowUtc().AddDate(0, 0, -3))
		connect.AssertEqual(t, len(family.EnsureRunway(ctx, day).Errors), 0)
		for i := range 100 {
			testRegistryExec(t, ctx, `INSERT INTO synthetic_registry_event (event_id, observed_at) VALUES ($1, $2)`, NewId(), day.Add(time.Duration(i)*time.Minute))
		}
		deadline := time.Now().Add(time.Minute)
		connect.AssertEqual(t, testRegistryScalar[int](t, ctx, `SELECT count(*) FROM pg_stats WHERE tablename = 'synthetic_registry_event' AND inherited`), 0)

		analyzed, period := family.AnalyzeParents(ctx, day.Add(time.Hour), "", deadline)
		connect.AssertEqual(t, analyzed, []string{"synthetic_registry_event"})
		connect.AssertEqual(t, period, day.Format(time.DateOnly))
		connect.AssertEqual(t, 0 < testRegistryScalar[int](t, ctx, `SELECT count(*) FROM pg_stats WHERE tablename = 'synthetic_registry_event' AND inherited`), true)
		analyzed, next := family.AnalyzeParents(ctx, day.Add(2*time.Hour), period, deadline)
		connect.AssertEqual(t, len(analyzed), 0)
		connect.AssertEqual(t, next, period)
		analyzed, next = family.AnalyzeParents(ctx, day.AddDate(0, 0, 1), period, deadline)
		connect.AssertEqual(t, analyzed, []string{"synthetic_registry_event"})
		connect.AssertEqual(t, next, day.AddDate(0, 0, 1).Format(time.DateOnly))
		// out of time, nothing is analyzed and the period stays
		analyzed, next = family.AnalyzeParents(ctx, day.AddDate(0, 0, 2), next, time.Now())
		connect.AssertEqual(t, len(analyzed), 0)
		connect.AssertEqual(t, next, day.AddDate(0, 0, 1).Format(time.DateOnly))

		leaf := family.LeafName("synthetic_registry_event", family.RangeAt(day))
		connect.AssertEqual(t, len(family.AnalyzeClosedLeaves(ctx, day.AddDate(0, 0, 1), deadline)), 0)
		connect.AssertEqual(t, family.AnalyzeClosedLeaves(ctx, day.AddDate(0, 0, 1).Add(time.Hour), deadline), []string{leaf})
		connect.AssertEqual(t, len(family.AnalyzeClosedLeaves(ctx, day.AddDate(0, 0, 1).Add(2*time.Hour), deadline)), 0)
	})
}

// An archive step cut short leaves its cursor in the state, and the next pass
// resumes it from there, then drops the range; the cursor is cleared.
func TestPartitionRegistryRetireResumesItsArchive(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		family := testRegistryEventFamily(t, ctx)
		day := PartitionGranularityDaily.RangeStart(NowUtc().AddDate(0, 1, 0))
		connect.AssertEqual(t, len(family.EnsureRunway(ctx, day).Errors), 0)
		testRegistryExec(t, ctx, `CREATE TABLE synthetic_registry_archive (event_id uuid PRIMARY KEY)`)
		for i := range 5 {
			testRegistryExec(t, ctx, `INSERT INTO synthetic_registry_event (event_id, observed_at) VALUES ($1, $2)`, NewId(), day.Add(time.Duration(i)*time.Hour))
		}
		r := family.RangeAt(day)
		leaf := family.LeafName("synthetic_registry_event", r)
		cursors := []string{}
		archiving := *family
		// archives two events per step, in event order, from the cursor
		archiving.Archive = func(ctx context.Context, r PartitionRange, cursor string, deadline time.Time) (string, bool, error) {
			cursors = append(cursors, cursor)
			var last string
			var count int
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `
					WITH batch AS (
						SELECT event_id FROM synthetic_registry_event
						WHERE $1 <= observed_at AND observed_at < $2 AND event_id::text > $3
						ORDER BY event_id LIMIT 2
					), archived AS (
						INSERT INTO synthetic_registry_archive SELECT event_id FROM batch RETURNING event_id
					)
					SELECT COALESCE(max(event_id::text), ''), count(*) FROM archived
				`, r.Lower, r.Upper, cursor).Scan(&last, &count))
			})
			if count < 2 {
				return "", true, nil
			}
			return last, false, nil
		}
		now := r.Upper.Add(family.Retention)
		deadline := time.Now().Add(time.Minute)

		report, state := archiving.Retire(ctx, now, PartitionFamilyState{}, deadline)
		connect.AssertEqual(t, report.More, true)
		connect.AssertEqual(t, len(report.Dropped), 0)
		connect.AssertEqual(t, state.ArchiveLeaf, leaf)
		connect.AssertEqual(t, state.ArchiveCursor != "", true)
		first := state.ArchiveCursor

		report, state = archiving.Retire(ctx, now, state, deadline)
		connect.AssertEqual(t, report.More, true)
		connect.AssertEqual(t, cursors[1], first)

		report, state = archiving.Retire(ctx, now, state, deadline)
		connect.AssertEqual(t, report.Archived, []string{leaf})
		connect.AssertEqual(t, report.Dropped, []string{leaf})
		connect.AssertEqual(t, state.ArchiveLeaf, "")
		connect.AssertEqual(t, state.ArchiveCursor, "")
		connect.AssertEqual(t, testRegistryScalar[int](t, ctx, `SELECT count(*) FROM synthetic_registry_archive`), 5)
		connect.AssertEqual(t, len(cursors), 3)
	})
}
