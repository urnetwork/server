// audit_device_event's cutover and retention against PostgreSQL with the
// migrated schema (db_audit_device_event_partition.go): the operator's prepare
// refuses an incomplete device day rollup, drops the unique event id index,
// stages and validates; finalize keeps every event in the legacy partition and
// sends events from the cutover day to its partition; rollback copies them
// back; an unfinalized prepare is abandoned before its day; a day lands at its
// bounds and drops once 7 days past, the legacy partition with it.
package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// One attempt per step with short waits, so a busy step fails at once.
func testAuditDeviceEventPartitionSettings() *AuditDeviceEventPartitionSettings {
	settings := DefaultAuditDeviceEventPartitionSettings()
	settings.CutoverAttempts = 3
	settings.CutoverRetryInterval = 10 * time.Millisecond
	return settings
}

func testAuditDeviceEventMigrate(t testing.TB, ctx context.Context, options AuditDeviceEventPartitionMigrateOptions) error {
	t.Helper()
	options.Logf = t.Logf
	if options.Settings == nil {
		options.Settings = testAuditDeviceEventPartitionSettings()
	}
	return MigrateAuditDeviceEventsToPartitions(ctx, options)
}

func testAuditDeviceEventInsert(t testing.TB, ctx context.Context, eventTime time.Time) (eventId Id) {
	t.Helper()
	eventId = NewId()
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_device_event (event_id, event_time, network_id, device_id, event_type, event_details)
			VALUES ($1, $2, $3, $4, 'device_added', 'synthetic sweep')`, eventId, eventTime, NewId(), NewId()))
	})
	return
}

// The relation holding the event.
func testAuditDeviceEventRelation(t testing.TB, ctx context.Context, eventId Id) string {
	t.Helper()
	return testContractPartitionScalar[string](t, ctx, `SELECT tableoid::regclass::text FROM audit_device_event WHERE event_id = $1`, eventId)
}

// The rollup complete for the export's window.
func testAuditDeviceEventCompleteRollup(t testing.TB, ctx context.Context) {
	t.Helper()
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_device_day (day, device_count, complete, update_time)
			SELECT day::date, 1, true, now() FROM generate_series((now() AT TIME ZONE 'UTC')::date - 100, (now() AT TIME ZONE 'UTC')::date, interval '1 day') day
			ON CONFLICT (day) DO UPDATE SET complete = true`))
	})
}

func testAuditDeviceEventLeaves(t testing.TB, ctx context.Context) (names []string) {
	t.Helper()
	MaintenanceDb(ctx, func(conn PgConn) {
		leaves, err := auditDeviceEventDays.ListLeaves(ctx, conn, auditDeviceEventTable)
		Raise(err)
		for _, leaf := range leaves {
			names = append(names, leaf.Name)
		}
	})
	return
}

// The operator's cutover end to end: prepare refuses until the rollup covers
// the export's window; it drops the unique event id index and stages; an event
// written between prepare and finalize stays in the table; finalize keeps every
// event in the legacy partition under the published names, and an event of
// the cutover day lands in its day; rollback copies it back and leaves a plain
// table without the unique index. Dry runs change nothing.
func TestAuditDeviceEventCutoverKeepsEveryEventAndDropsTheUniqueIndex(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// the constraint as Main held it before its prepare; the record
		// migration drops it from fresh databases
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `ALTER TABLE audit_device_event ADD CONSTRAINT audit_device_event_event_id_key UNIQUE (event_id)`))
		})
		cutoverDay := PartitionGranularityDaily.AddRanges(PartitionGranularityDaily.RangeStart(NowUtc()), 2)
		before := []Id{}
		for _, age := range []time.Duration{time.Hour, 3 * 24 * time.Hour, 200 * 24 * time.Hour} {
			before = append(before, testAuditDeviceEventInsert(t, ctx, NowUtc().Add(-age)))
		}

		err := testAuditDeviceEventMigrate(t, ctx, AuditDeviceEventPartitionMigrateOptions{CutoverDay: cutoverDay})
		if err == nil || !strings.Contains(err.Error(), "problem") {
			t.Fatal("prepare ran without the rollup", err)
		}
		testAuditDeviceEventCompleteRollup(t, ctx)
		if err := testAuditDeviceEventMigrate(t, ctx, AuditDeviceEventPartitionMigrateOptions{CutoverDay: cutoverDay, DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.audit_device_event_staged') IS NOT NULL OR NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'audit_device_event_event_id_key')`) {
			t.Fatal("the prepare's dry run changed the table")
		}
		if err := testAuditDeviceEventMigrate(t, ctx, AuditDeviceEventPartitionMigrateOptions{CutoverDay: cutoverDay}); err != nil {
			t.Fatal(err)
		}
		if testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'public.audit_device_event'::regclass AND contype = 'u')`) {
			t.Fatal("the unique event id index is still there")
		}
		if !testContractPartitionScalar[bool](t, ctx, `SELECT convalidated FROM pg_constraint WHERE conname = 'audit_device_event_legacy_bound'`) {
			t.Fatal("the legacy bound is not validated")
		}
		between := testAuditDeviceEventInsert(t, ctx, NowUtc())
		// a prepare run again resumes, changing nothing
		if err := testAuditDeviceEventMigrate(t, ctx, AuditDeviceEventPartitionMigrateOptions{CutoverDay: cutoverDay}); err != nil {
			t.Fatal(err)
		}

		if err := testAuditDeviceEventMigrate(t, ctx, AuditDeviceEventPartitionMigrateOptions{Finalize: true, DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if IsAuditDeviceEventPartitioned(ctx) {
			t.Fatal("the finalize's dry run swapped")
		}
		if err := testAuditDeviceEventMigrate(t, ctx, AuditDeviceEventPartitionMigrateOptions{Finalize: true}); err != nil {
			t.Fatal(err)
		}
		if !IsAuditDeviceEventPartitioned(ctx) {
			t.Fatal("the finalize did not swap")
		}
		for _, eventId := range append(slices.Clone(before), between) {
			if relation := testAuditDeviceEventRelation(t, ctx, eventId); relation != "audit_device_event_legacy" {
				t.Fatalf("event %s is in %s, not the legacy partition", eventId, relation)
			}
		}
		for _, index := range []string{"audit_device_event_pkey", "audit_device_event_device_id_event_time"} {
			if testContractPartitionScalar[string](t, ctx, `SELECT indrelid::regclass::text FROM pg_index WHERE indexrelid = to_regclass($1)`, "public."+index) != "audit_device_event" {
				t.Fatalf("%s is not the parent's index", index)
			}
		}
		cutoverEvent := testAuditDeviceEventInsert(t, ctx, cutoverDay.Add(time.Hour))
		if relation := testAuditDeviceEventRelation(t, ctx, cutoverEvent); relation != auditDeviceEventDays.LeafName(auditDeviceEventTable, PartitionGranularityDaily.RangeOf(cutoverDay)) {
			t.Fatalf("the cutover day's event is in %s", relation)
		}
		if leaves := testAuditDeviceEventLeaves(t, ctx); !slices.Contains(leaves, "audit_device_event_legacy") || len(leaves) < 1+settingsAheadDays() {
			t.Fatalf("the parent's partitions: %v", leaves)
		}

		if err := testAuditDeviceEventMigrate(t, ctx, AuditDeviceEventPartitionMigrateOptions{Rollback: true, DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if err := testAuditDeviceEventMigrate(t, ctx, AuditDeviceEventPartitionMigrateOptions{Rollback: true}); err != nil {
			t.Fatal(err)
		}
		if IsAuditDeviceEventPartitioned(ctx) {
			t.Fatal("the rollback left the table partitioned")
		}
		for _, eventId := range append(append(slices.Clone(before), between), cutoverEvent) {
			if relation := testAuditDeviceEventRelation(t, ctx, eventId); relation != "audit_device_event" {
				t.Fatalf("event %s is in %s after the rollback", eventId, relation)
			}
		}
		if testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'public.audit_device_event'::regclass AND contype = 'u')`) {
			t.Fatal("the rollback restored the unique index")
		}
		if !testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.audit_device_event_pkey') IS NOT NULL AND to_regclass('public.audit_device_event_device_id_event_time') IS NOT NULL`) {
			t.Fatal("the rollback did not restore the published index names")
		}
	})
}

func settingsAheadDays() int {
	return DefaultAuditDeviceEventPartitionSettings().AheadDays
}

// A prepare not finalized CutoverAbandonMargin before its day is abandoned by
// the runway pass's hook, and not earlier; abort removes a prepare too.
func TestAuditDeviceEventCutoverAbandonedBeforeItsDay(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testAuditDeviceEventPartitionSettings()
		cutoverDay := PartitionGranularityDaily.AddRanges(PartitionGranularityDaily.RangeStart(NowUtc()), 2)
		prepared := func() bool {
			return testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.audit_device_event_staged') IS NOT NULL
				AND EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'audit_device_event_legacy_bound')`)
		}
		Raise(prepareAuditDeviceEventCutover(ctx, cutoverDay, false, false, t.Logf, settings))
		if !prepared() {
			t.Fatal("not prepared")
		}
		if err := abandonStaleAuditDeviceEventCutover(ctx, cutoverDay.Add(-settings.CutoverAbandonMargin-time.Minute)); err != nil || !prepared() {
			t.Fatal("abandoned before the margin", err)
		}
		if err := abandonStaleAuditDeviceEventCutover(ctx, cutoverDay.Add(-settings.CutoverAbandonMargin)); err != nil || prepared() {
			t.Fatal("not abandoned at the margin", err)
		}
		if err := auditDeviceEventPartitionFamily.BeforeCutover(ctx, NowUtc()); err != nil {
			t.Fatal("the hook failed with nothing prepared", err)
		}

		Raise(prepareAuditDeviceEventCutover(ctx, cutoverDay, false, false, t.Logf, settings))
		if err := testAuditDeviceEventMigrate(t, ctx, AuditDeviceEventPartitionMigrateOptions{Abort: true}); err != nil || prepared() {
			t.Fatal("abort left the prepare", err)
		}
	})
}

// Once partitioned, an event at a day's first microsecond lands in that day,
// one a microsecond earlier in the day before; the retention drops each day,
// and the legacy partition, once its upper bound is 7 days past, and keeps
// the rest.
func TestAuditDeviceEventDaysLandAtTheirBoundsAndDropAtSevenDays(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		today := PartitionGranularityDaily.RangeStart(NowUtc())
		cutoverDay := PartitionGranularityDaily.AddRanges(today, -10)
		Testing_PartitionAuditDeviceEvents(ctx, cutoverDay)
		dayBefore := PartitionGranularityDaily.AddRanges(today, -2)
		dayOf := PartitionGranularityDaily.AddRanges(today, -1)
		first := testAuditDeviceEventInsert(t, ctx, dayOf)
		last := testAuditDeviceEventInsert(t, ctx, dayOf.Add(-time.Microsecond))
		if relation := testAuditDeviceEventRelation(t, ctx, first); relation != auditDeviceEventDays.LeafName(auditDeviceEventTable, PartitionGranularityDaily.RangeOf(dayOf)) {
			t.Fatalf("the day's first microsecond landed in %s", relation)
		}
		if relation := testAuditDeviceEventRelation(t, ctx, last); relation != auditDeviceEventDays.LeafName(auditDeviceEventTable, PartitionGranularityDaily.RangeOf(dayBefore)) {
			t.Fatalf("the day before's last microsecond landed in %s", relation)
		}
		old := testAuditDeviceEventInsert(t, ctx, PartitionGranularityDaily.AddRanges(today, -9).Add(time.Hour))
		kept := testAuditDeviceEventInsert(t, ctx, PartitionGranularityDaily.AddRanges(today, -6).Add(time.Hour))

		report, _ := auditDeviceEventPartitionFamily.Retire(ctx, NowUtc(), PartitionFamilyState{}, time.Now().Add(time.Minute))
		if 0 < len(report.Blocked) {
			t.Fatalf("blocked: %v", report.Blocked)
		}
		leaves := testAuditDeviceEventLeaves(t, ctx)
		for days := 10; 8 <= days; days -= 1 {
			if leaf := auditDeviceEventDays.LeafName(auditDeviceEventTable, PartitionGranularityDaily.RangeOf(PartitionGranularityDaily.AddRanges(today, -days))); slices.Contains(leaves, leaf) {
				t.Fatalf("%s, 7 days past, was not dropped: %v", leaf, leaves)
			}
		}
		if slices.Contains(leaves, "audit_device_event_legacy") {
			t.Fatal("the legacy partition, 10 days past, was not dropped")
		}
		for days := 7; 0 <= days; days -= 1 {
			if leaf := auditDeviceEventDays.LeafName(auditDeviceEventTable, PartitionGranularityDaily.RangeOf(PartitionGranularityDaily.AddRanges(today, -days))); !slices.Contains(leaves, leaf) {
				t.Fatalf("%s, inside 7 days, was dropped: %v", leaf, leaves)
			}
		}
		if testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM audit_device_event WHERE event_id = $1`, old) != 0 ||
			testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM audit_device_event WHERE event_id = $1`, kept) != 1 {
			t.Fatal("the drop took the wrong days")
		}
		runway := auditDeviceEventPartitionFamily.EnsureRunway(ctx, NowUtc())
		if leaf := auditDeviceEventDays.LeafName(auditDeviceEventTable, PartitionGranularityDaily.RangeOf(PartitionGranularityDaily.AddRanges(today, 3))); !slices.Contains(testAuditDeviceEventLeaves(t, ctx), leaf) {
			t.Fatalf("the runway did not keep %s ahead: %+v", leaf, runway)
		}
	})
}

// The swap waits for a sweep's open insert without queueing behind it for
// longer than its lock wait, and gives up within its attempts, leaving the
// table plain; with the insert committed it swaps, and the inserted event is
// in the legacy partition.
func TestAuditDeviceEventSwapWaitsOutAnOpenInsert(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testAuditDeviceEventPartitionSettings()
		settings.CutoverAttempts = 1
		settings.ExclusiveLockTimeout = 200 * time.Millisecond
		// the open insert is the barrier: the quiet wait must not defer to it
		settings.MaxTransactionAge = time.Hour
		cutoverDay := PartitionGranularityDaily.AddRanges(PartitionGranularityDaily.RangeStart(NowUtc()), 2)
		Raise(prepareAuditDeviceEventCutover(ctx, cutoverDay, false, false, t.Logf, settings))

		sweep, err := AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer sweep.Release()
		RaisePgResult(sweep.Exec(ctx, `BEGIN`))
		var eventId Id
		Raise(sweep.QueryRow(ctx, `INSERT INTO audit_device_event (event_id, event_time, network_id, device_id, event_type, event_details)
			VALUES (gen_random_uuid(), now() AT TIME ZONE 'UTC', gen_random_uuid(), gen_random_uuid(), 'device_added', 'synthetic sweep') RETURNING event_id`).Scan(&eventId))

		err = testAuditDeviceEventMigrate(t, ctx, AuditDeviceEventPartitionMigrateOptions{Finalize: true, Settings: settings})
		if err == nil || !strings.Contains(err.Error(), "lock timeout") {
			t.Fatal("the swap did not give up behind the open insert", err)
		}
		if IsAuditDeviceEventPartitioned(ctx) {
			t.Fatal("the swap that gave up changed the table")
		}
		RaisePgResult(sweep.Exec(ctx, `COMMIT`))
		settings.CutoverAttempts = 3
		if err := testAuditDeviceEventMigrate(t, ctx, AuditDeviceEventPartitionMigrateOptions{Finalize: true, Settings: settings}); err != nil {
			t.Fatal(err)
		}
		if relation := testAuditDeviceEventRelation(t, ctx, eventId); relation != "audit_device_event_legacy" {
			t.Fatalf("the sweep's event is in %s", relation)
		}
	})
}
