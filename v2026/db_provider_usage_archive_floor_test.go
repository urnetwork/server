// The usage archive's close time floor (db_provider_usage_archive_export.go,
// usagearchive/eligibility.go): 42 days before the database's clock, rolling
// when the control sets no date, and never past a week before the window of
// the earliest SN epoch not yet recorded. These run against PostgreSQL.
package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// An SN deployment as the floor reads it: its chain sync anchor, block
// anchorBlock at anchorTime, and the windows its epochs' start blocks open,
// 12 s a block.
type testSnChain struct {
	deploymentKey string
	anchorBlock   int64
	anchorTime    time.Time
}

// The start block of a window that opens ago before the anchor.
func (self *testSnChain) startBlock(ago time.Duration) int64 {
	return self.anchorBlock - int64(ago/(12*time.Second))
}

func (self *testSnChain) anchorSql() string {
	return fmt.Sprintf(
		`INSERT INTO st_chain_sync (deployment_key, singleton_id, high_water_block, update_time, block_hash) VALUES ('%s', 1, %d, '%s', '')`,
		self.deploymentKey, self.anchorBlock, self.anchorTime.UTC().Format(partitionTimestampLayout),
	)
}

// An epoch whose window opens ago before the anchor, in a v1 status.
func (self *testSnChain) epochSql(epoch int64, ago time.Duration, status string) string {
	start := self.startBlock(ago)
	return fmt.Sprintf(
		`INSERT INTO st_epoch (deployment_key, epoch, start_block, commit_deadline_block, trails_deadline_block, finalize_block, status) VALUES ('%s', %d, %d, %d, %d, %d, '%s')`,
		self.deploymentKey, epoch, start, start+51600, start+51600, start+64800, status,
	)
}

func (self *testSnChain) activationSql(noId int64, firstEpoch int64) string {
	return fmt.Sprintf(
		`INSERT INTO st_ledger_v2_activation (deployment_key, no_id, first_epoch, vault_v2_address, settlement_v2_address, create_time) VALUES ('%s', %d, %d, '0x0000000000000000000000000000000000000001', '0x0000000000000000000000000000000000000002', now())`,
		self.deploymentKey, noId, firstEpoch,
	)
}

// A v2 root of the pool noId for epoch, by its commit seq: pending, replaced,
// deleted, or finalized, with its amounts recorded or not.
func (self *testSnChain) rootSql(noId int64, seq int64, epoch int64, status string, recorded bool) string {
	rootIndex, recordedTime := "NULL", "NULL"
	if status == "finalized" {
		rootIndex = fmt.Sprintf("%d", seq)
		if recorded {
			recordedTime = "now()"
		}
	}
	return fmt.Sprintf(
		`INSERT INTO st_ledger_v2_root (deployment_key, no_id, seq, epoch, root, total, leaf_count, ledger_hash, status, root_index, recorded_time) VALUES ('%s', %d, %d, %d, decode(repeat('ab', 32), 'hex'), 1, 1, decode(repeat('cd', 32), 'hex'), '%s', %s, %s)`,
		self.deploymentKey, noId, seq, epoch, status, rootIndex, recordedTime,
	)
}

// The rows built for a v2 epoch, with a rebuild requested of them or not.
func (self *testSnChain) rowsSql(noId int64, epoch int64, rebuildRequested bool) string {
	rebuild := "NULL"
	if rebuildRequested {
		rebuild = "'operator'"
	}
	return fmt.Sprintf(
		`INSERT INTO st_ledger_v2_epoch_rows (deployment_key, no_id, epoch, provider_chunks, rebuild_requested_by, create_time) VALUES ('%s', %d, %d, '{}', %s, now())`,
		self.deploymentKey, noId, epoch, rebuild,
	)
}

// The SN epochs' floor as the drop reads it: a timestamp's text, -infinity,
// or null.
func testSnFloor(t testing.TB, ctx context.Context) string {
	return testContractPartitionScalar[string](t, ctx, fmt.Sprintf(`SELECT coalesce((%s)::text, 'null')`, providerUsageArchiveSnFloorSql))
}

// An SN epoch's window: 50,400 blocks of 12 s.
const testSnEpoch = 7 * 24 * time.Hour

// The floor stays a week before the window of the earliest epoch an epoch
// read can still read: a v1 epoch not finalized; from the v2 first epoch on,
// one whose pool has no finalized root with its amounts recorded, or a root
// still pending, or a rebuild requested; every deployment's latest epoch. A
// window it cannot place (no chain sync anchor, or v2 records of an epoch
// st_epoch lacks) holds the floor at -infinity. No epoch at all sets none.
// Main on 2026-11-21: v1 epochs 0-3 finalized; v2 from epoch 4, epochs 4 and
// 5 recorded although epoch 5's v1 mirror stays open; epoch 6's root pending;
// epoch 7 open. Its floor is a week before epoch 6's window.
func TestProviderUsageArchiveSnFloor(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		anchor := time.Date(2026, 11, 21, 22, 0, 0, 0, time.UTC)
		chain := &testSnChain{deploymentKey: "964:0xtest", anchorBlock: 9_557_954, anchorTime: anchor}
		other := &testSnChain{deploymentKey: "945:0xtest", anchorBlock: 5_000_000, anchorTime: anchor}
		// epoch e of the chain opens (7 - e) weeks before the anchor: epoch 7
		// is the open one
		ago := func(epoch int64) time.Duration {
			return time.Duration(7-epoch) * testSnEpoch
		}
		floorOf := func(opens time.Duration) string {
			return anchor.Add(-opens).Add(-providerUsageArchiveSnFloorMargin).Format(partitionTimestampLayout)
		}
		reset := func() {
			t.Helper()
			connect.AssertEqual(t, testArchiveExec(ctx,
				`DELETE FROM st_epoch`, `DELETE FROM st_chain_sync`, `DELETE FROM st_ledger_v2_activation`,
				`DELETE FROM st_ledger_v2_root`, `DELETE FROM st_ledger_v2_epoch_rows`,
			), nil)
		}
		// Main's shape, with the given changes to epoch 5
		main := func(epoch5 ...string) []string {
			statements := []string{chain.anchorSql(), chain.activationSql(1, 4)}
			for epoch := int64(0); epoch <= 3; epoch++ {
				statements = append(statements, chain.epochSql(epoch, ago(epoch), "finalized"))
			}
			statements = append(statements,
				chain.epochSql(4, ago(4), "finalized"),
				chain.epochSql(5, ago(5), "open"),
				chain.epochSql(6, ago(6), "open"),
				chain.epochSql(7, ago(7), "open"),
				chain.rootSql(1, 1, 4, "finalized", true),
				chain.rootSql(1, 3, 6, "pending", false),
				chain.rowsSql(1, 4, false),
				chain.rowsSql(1, 6, false),
			)
			return append(statements, epoch5...)
		}
		for _, test := range []struct {
			name       string
			statements []string
			floor      string
		}{
			{"no epoch", nil, "null"},
			{"Main", main(chain.rootSql(1, 2, 5, "finalized", true), chain.rowsSql(1, 5, false)), floorOf(ago(6))},
			{"epoch 5's amounts not recorded", main(chain.rootSql(1, 2, 5, "finalized", false), chain.rowsSql(1, 5, false)), floorOf(ago(5))},
			{"epoch 5 with no root", main(chain.rowsSql(1, 5, false)), floorOf(ago(5))},
			{"epoch 5 committed again", main(chain.rootSql(1, 2, 5, "finalized", true), chain.rootSql(1, 4, 5, "pending", false), chain.rowsSql(1, 5, false)), floorOf(ago(5))},
			{"a rebuild of epoch 5 requested", main(chain.rootSql(1, 2, 5, "finalized", true), chain.rowsSql(1, 5, true)), floorOf(ago(5))},
			{"epoch 5 finalized for another pool", main(chain.rootSql(2, 2, 5, "finalized", true), chain.rowsSql(1, 5, false)), floorOf(ago(5))},
			{"epoch 5's replaced and deleted roots", main(
				chain.rootSql(1, 10, 5, "replaced", false), chain.rootSql(1, 11, 5, "deleted", false),
				chain.rootSql(1, 2, 5, "finalized", true), chain.rowsSql(1, 5, false),
			), floorOf(ago(6))},
			{"v2 records of an epoch st_epoch lacks", main(chain.rootSql(1, 2, 5, "finalized", true), chain.rowsSql(1, 5, false), chain.rootSql(1, 5, 9, "pending", false)), "-infinity"},
			{"a v1 epoch not finalized", []string{
				chain.anchorSql(),
				chain.epochSql(1, ago(1), "finalized"), chain.epochSql(2, ago(2), "committed"),
				chain.epochSql(3, ago(3), "finalized"), chain.epochSql(4, ago(4), "open"),
			}, floorOf(ago(2))},
			{"every epoch finalized: the latest counts", []string{
				chain.anchorSql(),
				chain.epochSql(1, ago(1), "finalized"), chain.epochSql(2, ago(2), "finalized"),
			}, floorOf(ago(2))},
			{"no chain sync anchor", []string{
				chain.epochSql(1, ago(1), "finalized"), chain.epochSql(2, ago(2), "open"),
			}, "-infinity"},
			{"the earliest of two deployments", append(
				main(chain.rootSql(1, 2, 5, "finalized", true), chain.rowsSql(1, 5, false)),
				other.anchorSql(), other.epochSql(30, 6*testSnEpoch, "closed"), other.epochSql(31, 5*testSnEpoch, "open"),
			), floorOf(6 * testSnEpoch)},
		} {
			reset()
			connect.AssertEqual(t, testArchiveExec(ctx, test.statements...), nil)
			if floor := testSnFloor(t, ctx); floor != test.floor {
				t.Fatalf("%s: the SN floor is %s, not %s", test.name, floor, test.floor)
			}
		}
	})
}

// A week that ended more than 42 days ago drops once its export holds it,
// under a dated floor past it and under rolling, which sets no date; the 70
// days before 2026-11-21 kept such a week another four weeks. A week that
// ended within 42 days stays, rolling or not.
func TestProviderUsageArchiveExportDropsWeeksPast42Days(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := providerUsageArchivePartitionFamily
		now := NowUtc()
		// ended 56 to 63 days ago, 49 to 56, and 28 to 35
		older, old, recent := family.RangeAt(now.Add(-9*testArchiveWeek)), family.RangeAt(now.Add(-8*testArchiveWeek)), family.RangeAt(now.Add(-5*testArchiveWeek))
		dir := t.TempDir()
		clock := testExportClock()
		for _, r := range []PartitionRange{older, old, recent} {
			_, err := family.EnsureRangeFor(ctx, r.Lower)
			connect.AssertEqual(t, err, nil)
			closeTime := r.Lower.Add(time.Hour)
			testExportArchiveContract(t, ctx, r.Lower.Add(time.Hour), &closeTime, testArchiveUsage(1))
			_, manifestPath := testExportWeek(t, ctx, family.LeafName(testArchiveTable, r), dir, clock)
			testRegisterExport(t, ctx, manifestPath)
		}
		leafExists := func(r PartitionRange) bool {
			return testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, family.LeafName(testArchiveTable, r))
		}
		settings := DefaultProviderUsageArchiveExportSettings()
		pass := func(fields map[string]string) *PartitionRetireReport {
			control := NarrowProviderUsageArchiveExportControl(fields, settings)
			connect.AssertEqual(t, control.Invalid, "")
			report, _ := family.Retire(WithProviderUsageArchiveExportControlValue(ctx, control), NowUtc(), PartitionFamilyState{}, time.Now().Add(time.Minute))
			return report
		}

		// a dated floor of tomorrow: 42 days before now binds it
		report := pass(map[string]string{"mode": "on", "closeTimeFloor": now.Add(24 * time.Hour).Format(time.DateOnly), "maxWeeksPerPass": "1"})
		connect.AssertEqual(t, report.Dropped, []string{family.LeafName(testArchiveTable, older)})
		connect.AssertEqual(t, leafExists(older), false)
		connect.AssertEqual(t, leafExists(old), true)

		// rolling: the next week past 42 days drops; the recent one stays
		report = pass(map[string]string{"mode": "on", "closeTimeFloor": "rolling", "maxWeeksPerPass": "4"})
		connect.AssertEqual(t, report.Dropped, []string{family.LeafName(testArchiveTable, old)})
		if blocker := report.Blocked[family.LeafName(testArchiveTable, recent)]; blocker != "" {
			t.Fatalf("the recent week was nominated, and stays for %q", blocker)
		}
		connect.AssertEqual(t, leafExists(old), false)
		connect.AssertEqual(t, leafExists(recent), true)

		// the fence refuses the recent week outright, rolling or not
		rolling := NarrowProviderUsageArchiveExportControl(map[string]string{"mode": "on", "closeTimeFloor": "rolling"}, settings)
		_, err := family.DropRange(WithProviderUsageArchiveExportControlValue(ctx, rolling), recent)
		if err == nil || !strings.Contains(err.Error(), "ends after the export's close time floor") {
			t.Fatalf("the fence admitted a week within 42 days: %v", err)
		}
		connect.AssertEqual(t, leafExists(recent), true)
		connect.AssertEqual(t, testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM st_provider_usage_archive_export WHERE dropped_time IS NOT NULL`), int64(2))
	})
}

// The floor never passes a week before the window of an SN epoch not yet
// recorded, however long its recording takes, whether the control sets a date
// or rolling: the week stays and says why, and the fence refuses it. Before
// this, only whoever moved the date checked the epochs, and a week past 70
// days dropped whatever an epoch still had to read. Once the epoch is
// recorded, and the open epoch's window is recent, the week drops.
func TestProviderUsageArchiveExportHoldsTheFloorBeforeUnrecordedSnEpochs(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := providerUsageArchivePartitionFamily
		now := NowUtc()
		// ended 70 to 77 days ago: past 42 days, and past the 70 before them
		week := family.RangeAt(now.Add(-11 * testArchiveWeek))
		_, err := family.EnsureRangeFor(ctx, week.Lower)
		connect.AssertEqual(t, err, nil)
		leaf := family.LeafName(testArchiveTable, week)
		closeTime := week.Lower.Add(time.Hour)
		testExportArchiveContract(t, ctx, week.Lower.Add(time.Hour), &closeTime, testArchiveUsage(1))
		_, manifestPath := testExportWeek(t, ctx, leaf, t.TempDir(), testExportClock())
		testRegisterExport(t, ctx, manifestPath)

		// epoch 20's window opened 12 weeks ago and it is still not recorded:
		// its root is pending; epoch 21 is open
		chain := &testSnChain{deploymentKey: "964:0xtest", anchorBlock: 9_557_954, anchorTime: now}
		connect.AssertEqual(t, testArchiveExec(ctx,
			chain.anchorSql(), chain.activationSql(1, 4),
			chain.epochSql(20, 12*testSnEpoch, "open"), chain.epochSql(21, 11*testSnEpoch, "open"),
			chain.rootSql(1, 1, 20, "pending", false), chain.rowsSql(1, 20, false),
		), nil)

		settings := DefaultProviderUsageArchiveExportSettings()
		dated := map[string]string{"mode": "on", "closeTimeFloor": now.Add(24 * time.Hour).Format(time.DateOnly), "maxWeeksPerPass": "4"}
		rolling := map[string]string{"mode": "on", "closeTimeFloor": "rolling", "maxWeeksPerPass": "4"}
		pass := func(fields map[string]string) *PartitionRetireReport {
			control := NarrowProviderUsageArchiveExportControl(fields, settings)
			report, _ := family.Retire(WithProviderUsageArchiveExportControlValue(ctx, control), NowUtc(), PartitionFamilyState{}, time.Now().Add(time.Minute))
			return report
		}
		leafExists := func() bool {
			return testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, leaf)
		}

		for _, fields := range []map[string]string{dated, rolling} {
			report := pass(fields)
			connect.AssertEqual(t, len(report.Dropped), 0)
			if blocker := report.Blocked[leaf]; !strings.Contains(blocker, "the week ends after the close time floor") || !strings.Contains(blocker, "SN epoch not yet recorded") {
				t.Fatalf("under %v the week stays for %q", fields, blocker)
			}
			_, err = family.DropRange(WithProviderUsageArchiveExportControlValue(ctx, NarrowProviderUsageArchiveExportControl(fields, settings)), week)
			if err == nil || !strings.Contains(err.Error(), "ends after the export's close time floor") {
				t.Fatalf("under %v the fence admitted a week an SN epoch can still read: %v", fields, err)
			}
			connect.AssertEqual(t, leafExists(), true)
		}

		// epoch 20 recorded, 21 finalized too, and 22 open since last week:
		// the week drops
		connect.AssertEqual(t, testArchiveExec(ctx,
			`UPDATE st_ledger_v2_root SET status = 'finalized', root_index = 1, recorded_time = now() WHERE epoch = 20`,
			chain.rootSql(1, 2, 21, "finalized", true), chain.rowsSql(1, 21, false),
			chain.epochSql(22, testSnEpoch, "open"),
		), nil)
		report := pass(rolling)
		connect.AssertEqual(t, report.Dropped, []string{leaf})
		connect.AssertEqual(t, leafExists(), false)
	})
}
