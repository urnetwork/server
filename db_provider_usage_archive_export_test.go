// The usage archive's export (db_provider_usage_archive_export.go,
// usagearchive): the control, the export and its verification against the
// rows PostgreSQL holds, the fence that drops a week before its 52 weeks only
// against a verified copy of exactly its rows, the brake, and the restore of
// a dropped week from its copy. The control's narrowing is pure; the rest runs
// against PostgreSQL.
package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/usagearchive"
)

// A field that does not parse turns the mode off; the brake never passes the
// code's bound; without the hash nothing drops early.
func TestProviderUsageArchiveExportControlNarrows(t *testing.T) {
	settings := DefaultProviderUsageArchiveExportSettings()
	absent := NarrowProviderUsageArchiveExportControl(map[string]string{}, settings)
	connect.AssertEqual(t, absent.Present, false)
	connect.AssertEqual(t, absent.Mode, ProviderUsageArchiveExportModeOff)
	connect.AssertEqual(t, absent.CloseTimeFloor, usagearchive.DefaultCloseTimeFloor)
	connect.AssertEqual(t, absent.MaxWeeksPerPass, 1)

	on := NarrowProviderUsageArchiveExportControl(map[string]string{"mode": "on", "closeTimeFloor": "2026-08-15", "maxWeeksPerPass": "9"}, settings)
	connect.AssertEqual(t, on.Mode, ProviderUsageArchiveExportModeOn)
	connect.AssertEqual(t, on.CloseTimeFloor, time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC))
	connect.AssertEqual(t, on.MaxWeeksPerPass, settings.MaxWeeksPerPass)

	dryRun := NarrowProviderUsageArchiveExportControl(map[string]string{"mode": " dry_run ", "closeTimeFloor": "2026-08-15T12:00:00Z"}, settings)
	connect.AssertEqual(t, dryRun.Mode, ProviderUsageArchiveExportModeDryRun)
	connect.AssertEqual(t, dryRun.CloseTimeFloor, time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC))

	for _, fields := range []map[string]string{
		{"mode": "ON"},
		{"mode": "on", "closeTimeFloor": "next week"},
		{"mode": "on", "maxWeeksPerPass": "0"},
		{"mode": "on", "maxWeeksPerPass": "-1"},
		{"mode": "on", "maxWeeksPerPass": "two"},
	} {
		if control := NarrowProviderUsageArchiveExportControl(fields, settings); control.Mode != ProviderUsageArchiveExportModeOff {
			t.Fatalf("%v narrowed to mode %s", fields, control.Mode)
		}
	}

	// whatever the floor, no row closed within 70 days of now leaves
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	late := NarrowProviderUsageArchiveExportControl(map[string]string{"mode": "on", "closeTimeFloor": "2027-01-01"}, settings)
	connect.AssertEqual(t, late.EffectiveFloor(now), now.Add(-70*24*time.Hour))
	connect.AssertEqual(t, on.EffectiveFloor(now), time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC))

	// the brake hands out its weeks once
	connect.AssertEqual(t, absent.takeWeek(), true)
	connect.AssertEqual(t, absent.takeWeek(), false)
	connect.AssertEqual(t, absent.weeksLeft(), false)
}

// Without an active control a week expires at its 52 weeks only; with one, a
// week expires once it ends by the effective floor.
func TestProviderUsageArchiveExportRangeExpiry(t *testing.T) {
	family := providerUsageArchivePartitionFamily
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	june := family.RangeAt(time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC))
	august := family.RangeAt(time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC))
	september := family.RangeAt(time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	settings := DefaultProviderUsageArchiveExportSettings()
	ctxOf := func(fields map[string]string) context.Context {
		return WithProviderUsageArchiveExportControlValue(context.Background(), NarrowProviderUsageArchiveExportControl(fields, settings))
	}
	for _, ctx := range []context.Context{context.Background(), ctxOf(map[string]string{}), ctxOf(map[string]string{"mode": "off"})} {
		connect.AssertEqual(t, family.IsExpired(ctx, now, june), false)
	}
	on := ctxOf(map[string]string{"mode": "on"})
	connect.AssertEqual(t, family.IsExpired(on, now, june), true)
	// the week of 2026-08-24 ends 08-31, by the default floor of 09-01
	connect.AssertEqual(t, family.IsExpired(on, now, august), true)
	connect.AssertEqual(t, family.IsExpired(on, now, september), false)
	connect.AssertEqual(t, family.IsExpired(ctxOf(map[string]string{"mode": "dry_run"}), now, june), true)
	// 70 days before now caps any floor: on 11-03 the week ending 08-31 is
	// within them, whatever the floor
	earlier := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	connect.AssertEqual(t, family.IsExpired(on, earlier, august), false)
	connect.AssertEqual(t, family.IsExpired(ctxOf(map[string]string{"mode": "on", "closeTimeFloor": "2027-01-01"}), earlier, august), false)
	connect.AssertEqual(t, family.IsExpired(ctxOf(map[string]string{"mode": "on", "closeTimeFloor": "2027-01-01"}), now, september), true)
	connect.AssertEqual(t, family.IsExpired(on, now, PartitionRange{Default: true}), false)
}

// A terminal contract of the week holding at, closed at closeTime, archived by
// its delete.
func testExportArchiveContract(t testing.TB, ctx context.Context, at time.Time, closeTime *time.Time, usage *string) Id {
	contractId := testContractIdAt(at)
	testArchiveInsertContract(t, ctx, contractId, "settled", closeTime, usage)
	testArchiveDeleteContract(t, ctx, contractId)
	return contractId
}

// An archived contract without a close time, as historical rows on main are:
// the usage guard is set aside for its insert.
func testExportArchiveUndatedContract(t testing.TB, ctx context.Context, at time.Time) Id {
	contractId := testContractIdAt(at)
	err := contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_contract DISABLE TRIGGER transfer_contract_usage_guard`))
		RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, close_time, outcome, provider_usage, usage_origin_is_source
			) VALUES ($1, $2, $3, $4, $5, 1024, NULL, 'settled', NULL, true)
			`,
			contractId, NewId(), NewId(), NewId(), NewId(),
		))
		RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_contract ENABLE TRIGGER transfer_contract_usage_guard`))
	})
	if err != nil {
		t.Fatal(err)
	}
	testArchiveDeleteContract(t, ctx, contractId)
	return contractId
}

// Exports the leaf to dir with a clock that names each export distinctly.
func testExportWeek(t testing.TB, ctx context.Context, leafName string, dir string, clock func() time.Time) (*usagearchive.Manifest, string) {
	var manifest *usagearchive.Manifest
	var err error
	MaintenanceDb(ctx, func(conn PgConn) {
		var leaves []usagearchive.WeekLeaf
		leaves, err = usagearchive.ListWeekLeaves(ctx, conn.Conn())
		if err != nil {
			return
		}
		index := slices.IndexFunc(leaves, func(leaf usagearchive.WeekLeaf) bool { return leaf.Leaf == leafName })
		if index < 0 {
			err = fmt.Errorf("no week leaf %s", leafName)
			return
		}
		manifest, err = usagearchive.ExportWeek(ctx, conn.Conn(), leaves[index], &usagearchive.ExportSettings{Dir: dir, Host: "test", Clock: clock})
	})
	if err != nil {
		t.Fatal(err)
	}
	return manifest, filepath.Join(dir, strings.TrimSuffix(manifest.File, ".copy.zst")+".manifest.json")
}

func testRegisterExport(t testing.TB, ctx context.Context, manifestPath string) {
	var registered bool
	var err error
	MaintenanceDb(ctx, func(conn PgConn) {
		_, registered, err = usagearchive.Register(ctx, conn.Conn(), manifestPath, "test:"+filepath.Dir(manifestPath))
	})
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, registered, true)
}

// A clock that advances a second on every read, from a fixed start.
func testExportClock() func() time.Time {
	at := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		at = at.Add(time.Second)
		return at
	}
}

// The export reads the week's rows and their figures in one snapshot, and the
// COPY stream and the stored file match the row digest PostgreSQL computed:
// rows whose proof holds backslashes, quotes, escaped control characters and
// non-ASCII text, and a row without a close time. The stored copy is read
// back, and a copy that differs in one byte, or is cut short, fails.
func TestProviderUsageArchiveExportMatchesTheRows(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := providerUsageArchivePartitionFamily
		week := family.RangeAt(NowUtc().Add(-20 * contractPartitionWeek))
		_, err := family.EnsureRangeFor(ctx, week.Lower)
		connect.AssertEqual(t, err, nil)
		leaf := family.LeafName(testArchiveTable, week)
		usages := []string{
			`{"version":1,"byte_count":3,"providers":[]}`,
			`{"version":1,"byte_count":5,"providers":[],"note":"back\\slash \"quoted\" tab\there line\nbreak \u0001 \u00fcber \u2603"}`,
			`{"version":1,"byte_count":7,"providers":[],"path":"C:\\\\dir\\\\file"}`,
		}
		closeTime := week.Lower.Add(90 * time.Minute).Add(123456 * time.Microsecond)
		for i, usage := range usages {
			testExportArchiveContract(t, ctx, week.Lower.Add(time.Duration(i+1)*time.Hour), &closeTime, &usage)
		}
		testExportArchiveUndatedContract(t, ctx, week.Lower.Add(5*time.Hour))

		dir := t.TempDir()
		manifest, manifestPath := testExportWeek(t, ctx, leaf, dir, testExportClock())
		connect.AssertEqual(t, manifest.Rows, int64(4))
		connect.AssertEqual(t, manifest.NullCloseRows, int64(1))
		connect.AssertEqual(t, manifest.Leaf, leaf)
		connect.AssertEqual(t, manifest.WeekStart, week.Lower.Format(time.DateOnly))
		connect.AssertEqual(t, manifest.LowerContractId, ContractIdFloor(week.Lower).String())
		connect.AssertEqual(t, manifest.UpperContractId, ContractIdFloor(week.Upper).String())
		connect.AssertEqual(t, manifest.MaxCloseTime, closeTime.Format("2006-01-02 15:04:05.999999"))
		if manifest.VerifiedTime == "" {
			t.Fatal("the export was not verified")
		}
		// the digest PostgreSQL computes over the leaf is the manifest's
		var rows int64
		var digest string
		MaintenanceDb(ctx, func(conn PgConn) {
			rows, digest, err = providerUsageArchiveLeafDigest(ctx, conn, leaf)
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, rows, int64(4))
		connect.AssertEqual(t, digest, manifest.RowDigest)
		reread, err := usagearchive.VerifyManifest(manifestPath)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, reread.FileSha256, manifest.FileSha256)

		// one byte changed, then the file cut short
		copyPath := filepath.Join(dir, manifest.File)
		original, err := os.ReadFile(copyPath)
		connect.AssertEqual(t, err, nil)
		changed := slices.Clone(original)
		changed[len(changed)/2] ^= 0x01
		connect.AssertEqual(t, os.WriteFile(copyPath, changed, 0o640), nil)
		if _, err := usagearchive.VerifyManifest(manifestPath); err == nil {
			t.Fatal("a copy that differs in one byte verified")
		}
		connect.AssertEqual(t, os.WriteFile(copyPath, original[:len(original)-8], 0o640), nil)
		if _, err := usagearchive.VerifyManifest(manifestPath); err == nil {
			t.Fatal("a copy cut short verified")
		}
		// a copy that does not verify is never registered
		MaintenanceDb(ctx, func(conn PgConn) {
			_, _, err = usagearchive.Register(ctx, conn.Conn(), manifestPath, "test:"+dir)
		})
		if err == nil {
			t.Fatal("a copy that does not verify was registered")
		}
		connect.AssertEqual(t, testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM st_provider_usage_archive_export`), int64(0))
	})
}

// The retention pass under an export control, with the floor set after the
// week. Nothing drops before a verified copy of exactly the week's rows is in
// the ledger and PostgreSQL's digest of the week equals it; a dry run
// verifies and keeps the week; a row archived after the export keeps it until
// a new export holds that row too. Then the week drops, the ledger row records
// the drop, and the week restores from its copy to exactly the rows it held.
func TestProviderUsageArchiveExportedWeekDropsOnlyWhenVerified(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := providerUsageArchivePartitionFamily
		now := NowUtc()
		week := family.RangeAt(now.Add(-20 * contractPartitionWeek))
		_, err := family.EnsureRangeFor(ctx, week.Lower)
		connect.AssertEqual(t, err, nil)
		leaf := family.LeafName(testArchiveTable, week)
		closeTime := week.Lower.Add(2 * time.Hour)
		for i := range 3 {
			testExportArchiveContract(t, ctx, week.Lower.Add(time.Duration(i+1)*time.Hour), &closeTime, testArchiveUsage(int64(i+1)))
		}
		before := testArchiveProofs(testArchiveRows(t, ctx))
		connect.AssertEqual(t, len(before), 3)
		leafExists := func() bool {
			return testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, leaf)
		}
		settings := DefaultProviderUsageArchiveExportSettings()
		floor := now.Add(-10 * contractPartitionWeek).Format(time.RFC3339)
		pass := func(mode string) *PartitionRetireReport {
			control := NarrowProviderUsageArchiveExportControl(map[string]string{"mode": mode, "closeTimeFloor": floor}, settings)
			report, _ := family.Retire(WithProviderUsageArchiveExportControlValue(ctx, control), NowUtc(), PartitionFamilyState{}, time.Now().Add(time.Minute))
			return report
		}
		stays := func(report *PartitionRetireReport, reason string) {
			t.Helper()
			connect.AssertEqual(t, len(report.Dropped), 0)
			if blocker := report.Blocked[leaf]; !strings.Contains(blocker, reason) {
				t.Fatalf("the week stays for %q, not %q", blocker, reason)
			}
			connect.AssertEqual(t, leafExists(), true)
		}

		// without the control, or with it off, the week is not due
		report, _ := family.Retire(ctx, now, PartitionFamilyState{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, len(report.Dropped)+len(report.Blocked), 0)
		report = pass("off")
		connect.AssertEqual(t, len(report.Dropped)+len(report.Blocked), 0)
		// on, but no copy is in the ledger
		stays(pass("on"), "no verified export holds the week's 3 rows")

		dir := t.TempDir()
		clock := testExportClock()
		_, firstManifest := testExportWeek(t, ctx, leaf, dir, clock)
		// a copy on disk that is not in the ledger drops nothing
		stays(pass("on"), "no verified export holds the week's 3 rows")
		testRegisterExport(t, ctx, firstManifest)

		// a ledger digest that differs from the rows keeps the week
		connect.AssertEqual(t, testArchiveExec(ctx, `UPDATE st_provider_usage_archive_export SET row_digest = row_digest + 1`), nil)
		stays(pass("on"), "have digest")
		connect.AssertEqual(t, testArchiveExec(ctx, `UPDATE st_provider_usage_archive_export SET row_digest = row_digest - 1`), nil)

		// a dry run verifies and keeps it
		stays(pass("dry_run"), "dry run: would drop")

		// a row archived after the export keeps the week until a copy holds it
		testExportArchiveContract(t, ctx, week.Lower.Add(4*time.Hour), &closeTime, testArchiveUsage(4))
		stays(pass("on"), "no verified export holds the week's 4 rows")
		_, secondManifest := testExportWeek(t, ctx, leaf, dir, clock)
		testRegisterExport(t, ctx, secondManifest)
		before = testArchiveProofs(testArchiveRows(t, ctx))
		connect.AssertEqual(t, len(before), 4)

		// a week the reaper has not passed stays: two of its six newest
		// contracts are live
		live := []Id{}
		for i := range 2 {
			contractId := testContractIdAt(week.Lower.Add(time.Duration(6+i) * time.Hour))
			testArchiveInsertContract(t, ctx, contractId, "settled", &closeTime, testArchiveUsage(9))
			live = append(live, contractId)
		}
		stays(pass("on"), "the contract reaper has not passed the week")
		// the reaper's delete would archive them; deleting them without the
		// capture keeps the week's rows as the copy holds them
		connect.AssertEqual(t, testArchiveExec(ctx,
			`ALTER TABLE transfer_contract DISABLE TRIGGER transfer_contract_usage_archive_capture`,
			fmt.Sprintf(`DELETE FROM transfer_contract WHERE contract_id IN ('%s', '%s')`, live[0], live[1]),
			`ALTER TABLE transfer_contract ENABLE TRIGGER transfer_contract_usage_archive_capture`,
		), nil)

		// on: the week drops, and the ledger row of the copy records it
		report = pass("on")
		connect.AssertEqual(t, report.Dropped, []string{leaf})
		connect.AssertEqual(t, leafExists(), false)
		after := testArchiveRows(t, ctx)
		connect.AssertEqual(t, len(after), 0)
		connect.AssertEqual(t, testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM st_provider_usage_archive_export WHERE dropped_time IS NOT NULL AND row_count = 4`), int64(1))
		connect.AssertEqual(t, testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM st_provider_usage_archive_export WHERE dropped_time IS NOT NULL`), int64(1))

		// the copy restores to exactly the rows the week held, and attaches
		// again as the week's partition
		var restored *usagearchive.RestoreReport
		MaintenanceDb(ctx, func(conn PgConn) {
			restored, err = usagearchive.Restore(ctx, conn.Conn(), secondManifest, "st_provider_usage_archive_restore_test")
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, restored.Rows, int64(4))
		connect.AssertEqual(t, restored.DefaultRowsInWeek, int64(0))
		manifest, err := usagearchive.ReadManifest(secondManifest)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, testArchiveExec(ctx, usagearchive.AttachSql(manifest, "st_provider_usage_archive_restore_test")...), nil)
		connect.AssertEqual(t, testArchiveProofs(testArchiveRows(t, ctx)), before)
		for contractId := range before {
			connect.AssertEqual(t, testArchiveRows(t, ctx)[contractId].partition, "st_provider_usage_archive_restore_test")
		}
		// the restored week keeps the archive's guards
		if err := testArchiveExec(ctx, `DELETE FROM st_provider_usage_archive WHERE contract_id IN (SELECT contract_id FROM st_provider_usage_archive_restore_test)`); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("the restored week admitted a delete: %v", err)
		}
		if err := testArchiveExec(ctx, `TRUNCATE st_provider_usage_archive_restore_test`); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("the restored week admitted a truncate: %v", err)
		}
	})
}

// Rows that any SN epoch could read keep their week: a row without a close
// time, and a row closed after the floor. The fence refuses a direct drop of
// an exported week without the control on, after the floor, or past the
// brake; the brake lets one week through per pass.
func TestProviderUsageArchiveExportKeepsWhatReadersNeed(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := providerUsageArchivePartitionFamily
		now := NowUtc()
		weeks := []PartitionRange{}
		for _, ago := range []int{24, 23, 22, 21} {
			r := family.RangeAt(now.Add(-time.Duration(ago) * contractPartitionWeek))
			_, err := family.EnsureRangeFor(ctx, r.Lower)
			connect.AssertEqual(t, err, nil)
			weeks = append(weeks, r)
		}
		undated, lateClosed, first, second := weeks[0], weeks[1], weeks[2], weeks[3]
		for _, r := range []PartitionRange{undated, lateClosed, first, second} {
			closeTime := r.Lower.Add(time.Hour)
			testExportArchiveContract(t, ctx, r.Lower.Add(time.Hour), &closeTime, testArchiveUsage(1))
		}
		testExportArchiveUndatedContract(t, ctx, undated.Lower.Add(2*time.Hour))
		// closed after the floor (10 weeks ago): an epoch window may read it
		lateClose := now.Add(-5 * contractPartitionWeek)
		testExportArchiveContract(t, ctx, lateClosed.Lower.Add(2*time.Hour), &lateClose, testArchiveUsage(2))

		dir := t.TempDir()
		clock := testExportClock()
		for _, r := range weeks {
			_, manifestPath := testExportWeek(t, ctx, family.LeafName(testArchiveTable, r), dir, clock)
			testRegisterExport(t, ctx, manifestPath)
		}
		settings := DefaultProviderUsageArchiveExportSettings()
		control := func(mode string) *ProviderUsageArchiveExportControl {
			return NarrowProviderUsageArchiveExportControl(map[string]string{"mode": mode, "closeTimeFloor": now.Add(-10 * contractPartitionWeek).Format(time.RFC3339)}, settings)
		}
		leafExists := func(r PartitionRange) bool {
			return testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, family.LeafName(testArchiveTable, r))
		}

		// the fence alone, before the precondition: no control, a dry run
		for _, ctx := range []context.Context{ctx, WithProviderUsageArchiveExportControlValue(ctx, control("dry_run"))} {
			_, err := family.DropRange(ctx, first)
			if err == nil || !strings.Contains(err.Error(), "is kept until") {
				t.Fatalf("the fence admitted an early drop without the control on: %v", err)
			}
			connect.AssertEqual(t, leafExists(first), true)
		}
		// a floor before the week's end
		early := NarrowProviderUsageArchiveExportControl(map[string]string{"mode": "on", "closeTimeFloor": first.Lower.Format(time.RFC3339)}, settings)
		_, err := family.DropRange(WithProviderUsageArchiveExportControlValue(ctx, early), first)
		if err == nil || !strings.Contains(err.Error(), "ends after the export's close time floor") {
			t.Fatalf("the fence admitted a week after the floor: %v", err)
		}
		connect.AssertEqual(t, leafExists(first), true)

		// one pass, one week: the oldest eligible drops, the next waits
		report, _ := family.Retire(WithProviderUsageArchiveExportControlValue(ctx, control("on")), NowUtc(), PartitionFamilyState{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, report.Dropped, []string{family.LeafName(testArchiveTable, first)})
		if blocker := report.Blocked[family.LeafName(testArchiveTable, undated)]; blocker != "a row has no close time" {
			t.Fatalf("the undated week stays for %q", blocker)
		}
		if blocker := report.Blocked[family.LeafName(testArchiveTable, lateClosed)]; !strings.Contains(blocker, "not before the close time floor") {
			t.Fatalf("the late-closed week stays for %q", blocker)
		}
		if blocker := report.Blocked[family.LeafName(testArchiveTable, second)]; !strings.Contains(blocker, "brake") {
			t.Fatalf("the second week stays for %q", blocker)
		}
		connect.AssertEqual(t, leafExists(undated), true)
		connect.AssertEqual(t, leafExists(lateClosed), true)
		connect.AssertEqual(t, leafExists(second), true)

		// the next pass takes the next week; the rows readers need stay
		report, _ = family.Retire(WithProviderUsageArchiveExportControlValue(ctx, control("on")), NowUtc(), PartitionFamilyState{}, time.Now().Add(time.Minute))
		connect.AssertEqual(t, report.Dropped, []string{family.LeafName(testArchiveTable, second)})
		connect.AssertEqual(t, leafExists(undated), true)
		connect.AssertEqual(t, leafExists(lateClosed), true)
		connect.AssertEqual(t, testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM st_provider_usage_archive WHERE close_time IS NULL`), int64(1))
	})
}

// The control round-trips through Redis; a run reads it once into its
// context.
func TestProviderUsageArchiveExportControlReadsRedis(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		connect.AssertEqual(t, ProviderUsageArchiveExportActive(WithProviderUsageArchiveExportControl(ctx)), false)
		connect.AssertEqual(t, SetProviderUsageArchiveExportControl(ctx, map[string]string{"mode": "dry_run", "closeTimeFloor": "2026-08-01"}), nil)
		read := providerUsageArchiveExportControlIn(WithProviderUsageArchiveExportControl(ctx))
		connect.AssertEqual(t, read.Present, true)
		connect.AssertEqual(t, read.Mode, ProviderUsageArchiveExportModeDryRun)
		connect.AssertEqual(t, read.CloseTimeFloor, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
		connect.AssertEqual(t, RemoveProviderUsageArchiveExportControl(ctx), nil)
		connect.AssertEqual(t, ProviderUsageArchiveExportActive(WithProviderUsageArchiveExportControl(ctx)), false)
	})
}

// A copy verified before the ledger existed, or whose registration failed, is
// recorded by the next run, once; a copy that no longer verifies is not.
func TestProviderUsageArchiveExportRegistersStoredCopies(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := providerUsageArchivePartitionFamily
		week := family.RangeAt(NowUtc().Add(-20 * contractPartitionWeek))
		_, err := family.EnsureRangeFor(ctx, week.Lower)
		connect.AssertEqual(t, err, nil)
		leaf := family.LeafName(testArchiveTable, week)
		closeTime := week.Lower.Add(time.Hour)
		dir := t.TempDir()
		clock := testExportClock()
		files := []string{}
		for i := range 3 {
			testExportArchiveContract(t, ctx, week.Lower.Add(time.Duration(i+1)*time.Hour), &closeTime, testArchiveUsage(int64(i+1)))
			manifest, _ := testExportWeek(t, ctx, leaf, dir, clock)
			files = append(files, manifest.File)
		}
		register := func() ([]string, error) {
			var written []string
			var err error
			MaintenanceDb(ctx, func(conn PgConn) {
				written, err = usagearchive.RegisterStoredCopies(ctx, conn.Conn(), dir, "test:"+dir)
			})
			return written, err
		}
		ledgerRows := func() int64 {
			return testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM st_provider_usage_archive_export`)
		}
		// the newest copy no longer verifies: the two before it are recorded
		copyPath := filepath.Join(dir, files[2])
		data, err := os.ReadFile(copyPath)
		connect.AssertEqual(t, err, nil)
		data[len(data)/2] ^= 0x01
		connect.AssertEqual(t, os.WriteFile(copyPath, data, 0o640), nil)
		written, err := register()
		if err == nil || !strings.Contains(err.Error(), files[2]) {
			t.Fatalf("a copy that does not verify was registered: %v", err)
		}
		connect.AssertEqual(t, written, files[:2])
		connect.AssertEqual(t, ledgerRows(), int64(2))
		// once recorded, a copy is not recorded again
		data[len(data)/2] ^= 0x01
		connect.AssertEqual(t, os.WriteFile(copyPath, data, 0o640), nil)
		written, err = register()
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, written, files[2:])
		written, err = register()
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(written), 0)
		connect.AssertEqual(t, ledgerRows(), int64(3))
	})
}
