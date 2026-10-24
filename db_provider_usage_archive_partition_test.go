// The usage archive partitioned by contract week: its registry declaration,
// the cutover migration (exact copy, concurrent archives, restart), the drop
// policy, and the contract archive step writing into the partitioned shape.
// The declaration, the expiry rule, the cutover's statement order and the
// fence's range check are pure; the rest runs against PostgreSQL.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/connect"
)

const testArchiveTable = "st_provider_usage_archive"

// The archive is a weekly id family on the contract weeks: keyed by
// contract_id through contract_id_floor, four weeks ahead, each week kept 52
// weeks, with a DEFAULT partition, the retention opt-in and its fence, and out
// of the REINDEX rotation.
func TestProviderUsageArchivePartitionFamilyDeclaration(t *testing.T) {
	family := providerUsageArchivePartitionFamily
	if GetPartitionFamily(testArchiveTable) != family {
		t.Fatal("the archive family is not registered under its table's name")
	}
	connect.AssertEqual(t, validatePartitionFamily(family), nil)
	connect.AssertEqual(t, family.Tables, []string{testArchiveTable})
	connect.AssertEqual(t, family.KeyColumn, "contract_id")
	connect.AssertEqual(t, family.KeyKind, PartitionKeyId)
	connect.AssertEqual(t, family.Granularity, PartitionGranularityWeekly)
	connect.AssertEqual(t, family.AheadRanges, ContractPartitionAheadWeeks)
	connect.AssertEqual(t, family.Retention, 52*7*24*time.Hour)
	connect.AssertEqual(t, family.DefaultPartition, true)
	connect.AssertEqual(t, family.KeepLegacyPartition, false)
	connect.AssertEqual(t, family.DropGuardSetting, "urnetwork.usage_archive_retention")
	connect.AssertEqual(t, family.ReindexExempt, true)
	connect.AssertEqual(t, family.DropFence != nil, true)
	connect.AssertEqual(t, family.DropPrecondition == nil, true)
	for _, name := range []string{testArchiveTable, "st_provider_usage_archive_w20260601", "st_provider_usage_archive_default"} {
		if PartitionFamilyOfTable(name) != family {
			t.Fatalf("%s is not the archive family's", name)
		}
		connect.AssertEqual(t, PartitionRegistryReindexExempt(name, true), true)
	}
	// a Tuesday is no week of the archive
	if PartitionFamilyOfTable("st_provider_usage_archive_w20260602") != nil {
		t.Fatal("a name off the Monday bounds belongs to the archive family")
	}

	// an archive week is the week of its contracts, under the contract
	// partitions' naming
	for at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC); at.Before(time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC)); at = at.Add(11 * time.Hour) {
		r := family.RangeAt(at)
		weekStart := ContractPartitionWeekStart(at)
		if !r.Lower.Equal(weekStart) || family.LeafName(testArchiveTable, r) != ContractPartitionName(testArchiveTable, weekStart) {
			t.Fatalf("the archive week of %s is %+v, not the contract week %s", at, r, weekStart)
		}
	}
	// the cutover's first possible week is a Monday, bounded by contract_id_floor
	first := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	connect.AssertEqual(t, ContractPartitionWeekStart(first).Equal(first), true)
	connect.AssertEqual(t, family.LeafName(testArchiveTable, family.RangeAt(first)), "st_provider_usage_archive_w20260601")
	connect.AssertEqual(t, family.KeyKind.BoundSql(first), contractIdLiteral(ContractIdFloor(first)))
}

// A week expires once its upper bound is 52 weeks past, not a nanosecond
// before; the DEFAULT partition never does.
func TestProviderUsageArchiveWeekExpiresAfterRetention(t *testing.T) {
	ctx := context.Background()
	family := providerUsageArchivePartitionFamily
	week := family.RangeAt(time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC))
	horizon := week.Upper.Add(providerUsageArchiveRetention)
	if !horizon.Equal(time.Date(2027, 6, 7, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the week of 2026-06-01 expires at %s", horizon)
	}
	connect.AssertEqual(t, family.IsExpired(ctx, horizon.Add(-time.Nanosecond), week), false)
	connect.AssertEqual(t, family.IsExpired(ctx, horizon, week), true)
	connect.AssertEqual(t, family.IsExpired(ctx, horizon.AddDate(10, 0, 0), PartitionRange{Default: true}), false)
}

// The cutover's statements keep their safe order: read committed and the write
// fence before anything reads, the copy before the staged parent has a guard,
// the check before the guards and the swap, and ACCESS EXCLUSIVE before the
// plain table goes. It makes the weeks the family keeps ahead.
func TestProviderUsageArchivePartitionSqlOrder(t *testing.T) {
	sql := providerUsageArchivePartitionSql
	position := func(statement string) int {
		if count := strings.Count(sql, statement); count != 1 {
			t.Fatalf("%q appears %d times in the cutover", statement, count)
		}
		return strings.Index(sql, statement)
	}
	order := []string{
		"SET TRANSACTION ISOLATION LEVEL READ COMMITTED;",
		"SET LOCAL lock_timeout = '5s';",
		"SET LOCAL transaction_timeout = 0;",
		"LOCK TABLE st_provider_usage_archive IN EXCLUSIVE MODE;",
		"CREATE TABLE st_provider_usage_archive_partitioned (",
		"DO $archive_grants$",
		"DO $archive_weeks$",
		"PARTITION OF st_provider_usage_archive_partitioned DEFAULT;",
		"INSERT INTO st_provider_usage_archive_partitioned (",
		"DO $archive_copy_check$",
		"CREATE TRIGGER st_provider_usage_archive_guard\n",
		"CREATE TRIGGER st_provider_usage_archive_truncate_guard\n",
		"DO $archive_leaf_guards$",
		"LOCK TABLE st_provider_usage_archive IN ACCESS EXCLUSIVE MODE;",
		"DROP TABLE st_provider_usage_archive;",
		"ALTER TABLE st_provider_usage_archive_partitioned RENAME TO st_provider_usage_archive;",
		"ALTER INDEX st_provider_usage_archive_partitioned_pkey RENAME TO st_provider_usage_archive_pkey;",
		"ALTER INDEX st_provider_usage_archive_partitioned_close_time RENAME TO st_provider_usage_archive_close_time;",
	}
	for i := 1; i < len(order); i += 1 {
		if position(order[i]) <= position(order[i-1]) {
			t.Fatalf("%q runs before %q", order[i], order[i-1])
		}
	}
	// nothing reads or writes a table before the fence
	fence := position("LOCK TABLE st_provider_usage_archive IN EXCLUSIVE MODE;")
	for _, statement := range []string{"SELECT", "INSERT", "CREATE", "DO ", "DROP", "ALTER"} {
		if index := strings.Index(sql, statement); index < fence {
			t.Fatalf("%q runs before the write fence", statement)
		}
	}
	if !strings.Contains(sql, fmt.Sprintf("current_week + interval '%d weeks'", ContractPartitionAheadWeeks)) {
		t.Fatal("the cutover does not create the weeks the family keeps ahead")
	}
	if !strings.Contains(sql, "generate_series(timestamp '2026-06-01', current_week") {
		t.Fatal("the cutover's weeks do not start at the oldest contract's week on main")
	}
}

// The cutover is listed once, after contract_id_floor, which its bounds call,
// and after the archive it partitions; its version is its place in the list.
func TestProviderUsageArchivePartitionMigrationVersion(t *testing.T) {
	version := ProviderUsageArchivePartitionMigrationVersion()
	if migrations[version-1] != any(providerUsageArchivePartitionMigration) {
		t.Fatalf("version %d is not the cutover", version)
	}
	listed := 0
	for _, migration := range migrations {
		if migration == any(providerUsageArchivePartitionMigration) {
			listed += 1
		}
	}
	connect.AssertEqual(t, listed, 1)
	connect.AssertEqual(t, testMigrationIndexOfSql(t, contractIdFloorSql) < version-1, true)
	connect.AssertEqual(t, testMigrationIndexOfSql(t, providerUsageArchiveSchemaSql) < version-1, true)
	identity, err := MigrationIdentity(version - 1)
	connect.AssertEqual(t, err, nil)
	expected, err := migrationIdentity(&SqlMigration{sql: providerUsageArchivePartitionSql})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, identity, expected)
	// a migration outside the list has no version
	if testContractPartitionRecover(func() { migrationVersion(newSqlMigration(`SELECT 1`)) }) == nil {
		t.Fatal("an unlisted migration has a version")
	}
}

// Before it reads anything, the fence refuses a range that is not one
// canonical week: the DEFAULT partition, an open bound, the legacy partition,
// two weeks, or a week that does not start on a Monday.
func TestProviderUsageArchiveDropFenceRefusesPartialWeeks(t *testing.T) {
	ctx := context.Background()
	week := providerUsageArchivePartitionFamily.RangeAt(time.Date(2025, 1, 8, 0, 0, 0, 0, time.UTC))
	for _, r := range []PartitionRange{
		{Default: true},
		{Lower: week.Lower},
		{Upper: week.Upper, Legacy: true},
		{Lower: week.Lower, Upper: week.Upper.Add(contractPartitionWeek)},
		{Lower: week.Lower.Add(24 * time.Hour), Upper: week.Upper.Add(24 * time.Hour)},
	} {
		failure := testContractPartitionRecover(func() {
			providerUsageArchiveDropFence(ctx, nil, r)
		})
		if failure == nil || !strings.Contains(fmt.Sprint(failure), "whole weeks only") {
			t.Fatalf("the fence admitted %+v: %v", r, failure)
		}
	}
}

// The index of the SQL migration whose statements are sql: applying migrations
// up to it leaves that migration unapplied.
func testMigrationIndexOfSql(t testing.TB, sql string) int {
	for i, migration := range migrations {
		if sqlMigration, ok := migration.(*SqlMigration); ok && sqlMigration.sql == sql {
			return i
		}
	}
	t.Fatalf("no migration runs %.60q", sql)
	return 0
}

// A database migrated just short of the cutover, so the archive is plain.
func testArchivePlainEnv() *TestEnv {
	environment := DefaultTestEnv()
	environment.ApplyDbMigrations = false
	environment.RerunCount = 0
	return environment
}

func testArchiveUsage(byteCount int64) *string {
	usage := fmt.Sprintf(`{"version":1,"byte_count":%d,"providers":[]}`, byteCount)
	return &usage
}

// A terminal contract with only its transfer_contract row, whose delete the
// capture trigger archives. Before the usage guard a terminal contract may
// lack its proof and its close time, as historical rows on main do.
func testArchiveInsertContract(t testing.TB, ctx context.Context, contractId Id, outcome string, closeTime *time.Time, usage *string) {
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, close_time, outcome, provider_usage, usage_origin_is_source
			) VALUES ($1, $2, $3, $4, $5, 1024, $6, $7, $8, true)
			`,
			contractId, NewId(), NewId(), NewId(), NewId(), closeTime, outcome, usage,
		))
	})
}

// Deletes one contract, whose capture archives it when it is terminal, and
// returns what the transaction raised.
func testArchiveDeleteContractErr(ctx context.Context, contractId Id) error {
	return contractPartitionTx(ctx, func(tx PgTx) {
		if deleted := RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id = $1`, contractId)).RowsAffected(); deleted != 1 {
			panic(fmt.Errorf("deleted %d contracts %s", deleted, contractId))
		}
	})
}

func testArchiveDeleteContract(t testing.TB, ctx context.Context, contractId Id) {
	if err := testArchiveDeleteContractErr(ctx, contractId); err != nil {
		t.Fatal(err)
	}
}

// A live contract's proof as PostgreSQL prints it, which the archive copies.
func testArchiveSourceUsage(t testing.TB, ctx context.Context, contractId Id) string {
	return testContractPartitionScalar[string](t, ctx, `SELECT provider_usage::text FROM transfer_contract WHERE contract_id = $1`, contractId)
}

// Runs statements in one transaction and returns what it raised.
func testArchiveExec(ctx context.Context, statements ...string) error {
	return contractPartitionExec(ctx, statements)
}

// One archived row as stored, and the partition that holds it.
type testArchiveRow struct {
	outcome       string
	closeTime     *time.Time
	providerUsage *string
	partition     string
}

func testArchiveRows(t testing.TB, ctx context.Context) map[Id]testArchiveRow {
	rows := map[Id]testArchiveRow{}
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT contract_id, outcome, close_time, provider_usage::text, tableoid::regclass::text FROM st_provider_usage_archive`,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var contractId Id
				var row testArchiveRow
				Raise(result.Scan(&contractId, &row.outcome, &row.closeTime, &row.providerUsage, &row.partition))
				rows[contractId] = row
			}
		})
	})
	return rows
}

// The proof fields of rows, without the partition that holds them.
func testArchiveProofs(rows map[Id]testArchiveRow) map[Id]testArchiveRow {
	proofs := map[Id]testArchiveRow{}
	for contractId, row := range rows {
		row.partition = ""
		proofs[contractId] = row
	}
	return proofs
}

// Rows, rows without proof and usage bytes in one contract week.
type testArchiveWeekTotal struct {
	rows      int
	missing   int
	byteCount int64
}

func testArchiveWeekTotals(t testing.TB, rows map[Id]testArchiveRow) map[time.Time]testArchiveWeekTotal {
	totals := map[time.Time]testArchiveWeekTotal{}
	for contractId, row := range rows {
		weekStart := ContractPartitionWeekStart(ContractIdTime(contractId))
		total := totals[weekStart]
		total.rows += 1
		if row.providerUsage == nil {
			total.missing += 1
		} else {
			var usage struct {
				ByteCount int64 `json:"byte_count"`
			}
			if err := json.Unmarshal([]byte(*row.providerUsage), &usage); err != nil {
				t.Fatalf("archived proof of %s: %s", contractId, err)
			}
			total.byteCount += usage.ByteCount
		}
		totals[weekStart] = total
	}
	return totals
}

// The family's leaves of the archive: the weekly ones by week start, and
// whether the DEFAULT partition is attached. Every leaf is the registry's own.
func testArchiveLeaves(t testing.TB, ctx context.Context) (weekStarts []time.Time, defaultPartition bool) {
	var leaves []PartitionLeaf
	var err error
	MaintenanceDb(ctx, func(conn PgConn) {
		leaves, err = providerUsageArchivePartitionFamily.ListLeaves(ctx, conn, testArchiveTable)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, leaf := range leaves {
		if !leaf.Canonical {
			t.Fatalf("%s is not a leaf the registry names: %+v", leaf.Name, leaf.Range)
		}
		if leaf.Range.Default {
			defaultPartition = true
			continue
		}
		weekStarts = append(weekStarts, leaf.Range.Lower)
	}
	slices.SortFunc(weekStarts, func(a time.Time, b time.Time) int {
		return a.Compare(b)
	})
	return
}

// The consecutive weeks from first through last.
func testArchiveWeeks(first time.Time, last time.Time) (weekStarts []time.Time) {
	for weekStart := first; !last.Before(weekStart); weekStart = weekStart.Add(contractPartitionWeek) {
		weekStarts = append(weekStarts, weekStart)
	}
	return
}

func testArchiveWeeksEqual(a []time.Time, b []time.Time) bool {
	return slices.EqualFunc(a, b, func(x time.Time, y time.Time) bool {
		return x.Equal(y)
	})
}

// Waits until a session waits for mode on the plain archive table: the
// cutover at its fence or at its swap, or a capture at the fence. It only
// synchronizes the test with the server; the outcomes are the proof.
func testArchiveAwaitLockWait(t testing.TB, ctx context.Context, mode string) {
	t.Helper()
	for range 3000 {
		var waiting bool
		Db(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(
				ctx,
				`
				SELECT EXISTS (
					SELECT 1 FROM pg_locks
					WHERE locktype = 'relation' AND relation = to_regclass('public.st_provider_usage_archive')
						AND mode = $1 AND NOT granted
				)
				`,
				mode,
			).Scan(&waiting))
		}, OptNoRetry())
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the test stopped before a session waited for %s on the archive", mode)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("no session waited for %s on the archive", mode)
}

// The cutover copies every archived row byte for byte, including rows whose
// contracts are gone and a historical row without proof or close time, each
// into the partition of its contract week from 2026-06-01 through four weeks
// ahead, and an id outside those weeks into the DEFAULT partition. Rows, rows
// without proof and usage bytes match week by week before and after. The
// guards hold on the partitioned shape, and a contract deleted after the
// cutover is archived into its week.
func TestProviderUsageArchivePartitionMigrationCopiesEveryRow(t *testing.T) {
	testArchivePlainEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := providerUsageArchivePartitionFamily
		now := NowUtc()
		currentWeek := ContractPartitionWeekStart(now)

		// before the usage guard a terminal contract could lack its proof
		ApplyDbMigrationsUpTo(ctx, testMigrationIndexOfSql(t, contractUsageGuardSchemaSql))
		missingProofId := testContractIdAt(now.Add(-2 * contractPartitionWeek))
		testArchiveInsertContract(t, ctx, missingProofId, "settled", nil, nil)

		version := ProviderUsageArchivePartitionMigrationVersion()
		ApplyDbMigrationsUpTo(ctx, version-1)
		connect.AssertEqual(t, family.IsPartitioned(ctx), false)

		// rows whose contracts are gone: every credit outcome over several
		// weeks, an id before the first archive week and one past the weeks
		// ahead
		closeTime := now
		goneIds := map[Id]string{
			testContractIdAt(now.Add(-3 * contractPartitionWeek)):                      "settled",
			testContractIdAt(now.Add(-contractPartitionWeek)):                          "dispute_resolved_to_source",
			testContractIdAt(now):                                                      "dispute_resolved_to_destination",
			testContractIdAt(now.Add(time.Minute)):                                     "settled",
			testContractIdAt(now.Add(contractPartitionWeek)):                           "settled",
			testContractIdAt(time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)):             "settled",
			testContractIdAt(currentWeek.Add(10*contractPartitionWeek + time.Hour)):    "dispute_resolved_to_source",
			testContractIdAt(currentWeek.Add(-12*contractPartitionWeek + time.Second)): "settled",
		}
		byteCount := int64(100)
		for contractId, outcome := range goneIds {
			testArchiveInsertContract(t, ctx, contractId, outcome, &closeTime, testArchiveUsage(byteCount))
			testArchiveDeleteContract(t, ctx, contractId)
			byteCount += 100
		}
		testArchiveDeleteContract(t, ctx, missingProofId)
		// a canceled contract leaves no proof
		canceledId := testContractIdAt(now)
		testArchiveInsertContract(t, ctx, canceledId, "canceled", &closeTime, nil)
		testArchiveDeleteContract(t, ctx, canceledId)
		// a terminal contract still live at the cutover
		liveId := testContractIdAt(now.Add(-contractPartitionWeek + time.Minute))
		testArchiveInsertContract(t, ctx, liveId, "settled", &closeTime, testArchiveUsage(7))
		liveUsage := testArchiveSourceUsage(t, ctx, liveId)

		// a read grant made outside the migrations
		connect.AssertEqual(t, testArchiveExec(ctx, `GRANT SELECT ON st_provider_usage_archive TO PUBLIC`), nil)

		before := testArchiveRows(t, ctx)
		connect.AssertEqual(t, len(before), len(goneIds)+1)
		connect.AssertEqual(t, before[missingProofId].providerUsage == nil && before[missingProofId].closeTime == nil, true)
		for _, row := range before {
			connect.AssertEqual(t, row.partition, testArchiveTable)
		}
		beforeTotals := testArchiveWeekTotals(t, before)

		ApplyDbMigrationsUpTo(ctx, version)
		connect.AssertEqual(t, DbVersion(ctx), version)
		connect.AssertEqual(t, family.IsPartitioned(ctx), true)

		after := testArchiveRows(t, ctx)
		connect.AssertEqual(t, testArchiveProofs(after), testArchiveProofs(before))
		connect.AssertEqual(t, testArchiveWeekTotals(t, after), beforeTotals)
		connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT has_table_privilege('public', 'public.st_provider_usage_archive', 'SELECT')`), true)

		// the oldest id is before 2026-06-01, so the weeks start there
		firstWeek := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		lastWeek := currentWeek.Add(ContractPartitionAheadWeeks * contractPartitionWeek)
		weekStarts, defaultPartition := testArchiveLeaves(t, ctx)
		connect.AssertEqual(t, defaultPartition, true)
		if !testArchiveWeeksEqual(weekStarts, testArchiveWeeks(firstWeek, lastWeek)) {
			t.Fatalf("the cutover made weeks %v, want %s through %s", weekStarts, firstWeek, lastWeek)
		}
		for contractId, row := range after {
			week := family.RangeAt(ContractIdTime(contractId))
			want := PartitionDefaultLeafName(testArchiveTable)
			if !week.Lower.Before(firstWeek) && !lastWeek.Before(week.Lower) {
				want = family.LeafName(testArchiveTable, week)
			}
			if row.partition != want {
				t.Fatalf("%s is in %s, want %s", contractId, row.partition, want)
			}
		}

		// the guards hold on the parent and on every partition
		goneId := testContractIdAt(now)
		for _, statement := range []string{
			`UPDATE st_provider_usage_archive SET provider_usage = NULL`,
			`UPDATE st_provider_usage_archive SET close_time = close_time + interval '1 hour'`,
			`DELETE FROM st_provider_usage_archive`,
			`TRUNCATE st_provider_usage_archive`,
			fmt.Sprintf(`TRUNCATE public.%s`, quoteIdentifier(family.LeafName(testArchiveTable, family.RangeAt(now)))),
			`TRUNCATE st_provider_usage_archive_default`,
			fmt.Sprintf(`INSERT INTO st_provider_usage_archive (contract_id, outcome, close_time, provider_usage) VALUES ('%s', 'settled', now(), '{}')`, goneId),
		} {
			err := testArchiveExec(ctx, statement)
			if err == nil || !(strings.Contains(err.Error(), "archived provider usage is append-only") || strings.Contains(err.Error(), "differs from its terminal source")) {
				t.Fatalf("the partitioned archive admitted %s: %v", statement, err)
			}
		}

		// a contract deleted after the cutover lands in its own week
		testArchiveDeleteContract(t, ctx, liveId)
		archived := testArchiveRows(t, ctx)[liveId]
		connect.AssertEqual(t, archived.partition, family.LeafName(testArchiveTable, family.RangeAt(ContractIdTime(liveId))))
		connect.AssertEqual(t, *archived.providerUsage, liveUsage)

		// the rest of the migrations apply over the partitioned archive
		ApplyDbMigrations(ctx)
		connect.AssertEqual(t, DbVersion(ctx), MigrationCount())
	})
}

// No archive write races the cutover unseen. A contract deleted while the
// cutover waits at its fence is copied, and one deleted while the cutover
// holds the fence waits for the swap and lands in the partitioned table, its
// capture resolving the name again and passing the guard there.
func TestProviderUsageArchivePartitionMigrationKeepsConcurrentArchives(t *testing.T) {
	testArchivePlainEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := providerUsageArchivePartitionFamily
		version := ProviderUsageArchivePartitionMigrationVersion()
		ApplyDbMigrationsUpTo(ctx, version-1)
		now := NowUtc()
		beforeFenceId := testContractIdAt(now.Add(-contractPartitionWeek))
		duringFenceId := testContractIdAt(now)
		sourceUsages := map[Id]string{}
		for _, contractId := range []Id{beforeFenceId, duringFenceId} {
			testArchiveInsertContract(t, ctx, contractId, "settled", &now, testArchiveUsage(11))
			sourceUsages[contractId] = testArchiveSourceUsage(t, ctx, contractId)
		}

		// a delete whose capture holds the archive until it commits
		deleter := RaisePgResult(AcquireMaintenanceDbConn(ctx))
		defer deleter.Release()
		deleteTx := RaisePgResult(deleter.Begin(ctx))
		defer deleteTx.Rollback(context.WithoutCancel(ctx))
		RaisePgResult(deleteTx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id = $1`, beforeFenceId))
		// a reader that keeps the swap waiting
		reader := RaisePgResult(AcquireMaintenanceDbConn(ctx))
		defer reader.Release()
		readTx := RaisePgResult(reader.Begin(ctx))
		defer readTx.Rollback(context.WithoutCancel(ctx))
		var archivedBefore int
		Raise(readTx.QueryRow(ctx, `SELECT count(*) FROM st_provider_usage_archive`).Scan(&archivedBefore))
		connect.AssertEqual(t, archivedBefore, 0)

		migrated := make(chan any, 1)
		go func() {
			migrated <- testContractPartitionRecover(func() {
				ApplyDbMigrationsUpTo(ctx, version)
			})
		}()
		testArchiveAwaitLockWait(t, ctx, "ExclusiveLock")
		Raise(deleteTx.Commit(ctx))
		// the copy is done and the swap waits for the reader
		testArchiveAwaitLockWait(t, ctx, "AccessExclusiveLock")

		archived := make(chan error, 1)
		go func() {
			archived <- testArchiveDeleteContractErr(ctx, duringFenceId)
		}()
		testArchiveAwaitLockWait(t, ctx, "RowExclusiveLock")
		Raise(readTx.Commit(ctx))

		connect.AssertEqual(t, <-migrated, nil)
		connect.AssertEqual(t, <-archived, nil)
		connect.AssertEqual(t, DbVersion(ctx), version)
		connect.AssertEqual(t, family.IsPartitioned(ctx), true)
		rows := testArchiveRows(t, ctx)
		connect.AssertEqual(t, len(rows), 2)
		for _, contractId := range []Id{beforeFenceId, duringFenceId} {
			row, ok := rows[contractId]
			connect.AssertEqual(t, ok, true)
			connect.AssertEqual(t, row.partition, family.LeafName(testArchiveTable, family.RangeAt(ContractIdTime(contractId))))
			connect.AssertEqual(t, *row.providerUsage, sourceUsages[contractId])
			connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM transfer_contract WHERE contract_id = $1)`, contractId), false)
		}
	})
}

// The cutover is one transaction. Interrupted after its copy (a reader keeps
// the swap's lock past lock_timeout), it leaves the version, the plain table,
// its rows and its guards as they were and nothing staged; the next migration
// run starts it over and completes it, with weeks from the oldest contract's.
func TestProviderUsageArchivePartitionMigrationRestarts(t *testing.T) {
	testArchivePlainEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := providerUsageArchivePartitionFamily
		version := ProviderUsageArchivePartitionMigrationVersion()
		ApplyDbMigrationsUpTo(ctx, version-1)
		now := NowUtc()
		oldestId := testContractIdAt(now.Add(-2 * contractPartitionWeek))
		for i, contractId := range []Id{oldestId, testContractIdAt(now)} {
			testArchiveInsertContract(t, ctx, contractId, "settled", &now, testArchiveUsage(int64(10+i)))
			testArchiveDeleteContract(t, ctx, contractId)
		}
		before := testArchiveRows(t, ctx)

		reader := RaisePgResult(AcquireMaintenanceDbConn(ctx))
		defer reader.Release()
		readTx := RaisePgResult(reader.Begin(ctx))
		defer readTx.Rollback(context.WithoutCancel(ctx))
		var archived int
		Raise(readTx.QueryRow(ctx, `SELECT count(*) FROM st_provider_usage_archive`).Scan(&archived))
		connect.AssertEqual(t, archived, len(before))

		failure := testContractPartitionRecover(func() {
			ApplyDbMigrationsUpTo(ctx, version)
		})
		var pgErr *pgconn.PgError
		if err, _ := failure.(error); !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("the cutover did not stop at the swap's lock: %v", failure)
		}
		Raise(readTx.Rollback(ctx))

		connect.AssertEqual(t, DbVersion(ctx), version-1)
		connect.AssertEqual(t, family.IsPartitioned(ctx), false)
		connect.AssertEqual(t, testArchiveRows(t, ctx), before)
		connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `
			SELECT to_regclass('public.st_provider_usage_archive_partitioned') IS NULL
				AND NOT EXISTS (
					SELECT 1 FROM pg_class
					WHERE relkind IN ('r', 'p') AND relname LIKE 'st\_provider\_usage\_archive\_%'
				)
		`), true)
		connect.AssertEqual(t, testContractPartitionScalar[int](t, ctx, `
			SELECT count(*) FROM pg_trigger
			WHERE tgrelid = to_regclass('public.st_provider_usage_archive')
				AND tgname IN ('st_provider_usage_archive_guard', 'st_provider_usage_archive_truncate_guard')
				AND tgenabled = 'O'
		`), 2)
		// the interrupted start has no success, so the cutover runs again
		connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `
			SELECT EXISTS (SELECT 1 FROM migration_audit WHERE start_version_number = $1 AND status = 'start')
				AND NOT EXISTS (SELECT 1 FROM migration_audit WHERE start_version_number = $1 AND status = 'success')
		`, version-1), true)

		ApplyDbMigrationsUpTo(ctx, version)
		connect.AssertEqual(t, DbVersion(ctx), version)
		connect.AssertEqual(t, family.IsPartitioned(ctx), true)
		after := testArchiveRows(t, ctx)
		connect.AssertEqual(t, testArchiveProofs(after), testArchiveProofs(before))
		weekStarts, defaultPartition := testArchiveLeaves(t, ctx)
		connect.AssertEqual(t, defaultPartition, true)
		firstWeek := ContractPartitionWeekStart(ContractIdTime(oldestId))
		lastWeek := ContractPartitionWeekStart(now).Add(ContractPartitionAheadWeeks * contractPartitionWeek)
		if !testArchiveWeeksEqual(weekStarts, testArchiveWeeks(firstWeek, lastWeek)) {
			t.Fatalf("the rerun made weeks %v, want %s through %s", weekStarts, firstWeek, lastWeek)
		}
	})
}

// Archives a terminal contract of the week holding at, and returns its id.
func testArchiveContractIn(t testing.TB, ctx context.Context, at time.Time, byteCount int64) Id {
	contractId := testContractIdAt(at)
	closeTime := NowUtc()
	testArchiveInsertContract(t, ctx, contractId, "settled", &closeTime, testArchiveUsage(byteCount))
	testArchiveDeleteContract(t, ctx, contractId)
	return contractId
}

// Retention keeps a week until its upper bound is 52 weeks past and drops it
// then, under the registry's guarded drop, with its rows. A pass whose clock
// already puts a younger week past the horizon cannot drop it before the
// database's clock does: the fence keeps it and the pass reports it.
func TestProviderUsageArchiveWeekDropsAtRetention(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := providerUsageArchivePartitionFamily
		now := NowUtc()
		old := family.RangeAt(now.Add(-60 * contractPartitionWeek))
		young := family.RangeAt(now.Add(-50 * contractPartitionWeek))
		for _, r := range []PartitionRange{old, young} {
			created, err := family.EnsureRangeFor(ctx, r.Lower)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, created, []string{family.LeafName(testArchiveTable, r)})
		}
		oldId := testArchiveContractIn(t, ctx, old.Lower.Add(time.Hour), 3)
		youngId := testArchiveContractIn(t, ctx, young.Lower.Add(time.Hour), 5)
		rows := testArchiveRows(t, ctx)
		connect.AssertEqual(t, rows[oldId].partition, family.LeafName(testArchiveTable, old))
		connect.AssertEqual(t, rows[youngId].partition, family.LeafName(testArchiveTable, young))
		leafExists := func(r PartitionRange) bool {
			return testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, family.LeafName(testArchiveTable, r))
		}

		deadline := time.Now().Add(time.Minute)
		report, state := family.Retire(ctx, old.Upper.Add(providerUsageArchiveRetention-time.Nanosecond), PartitionFamilyState{}, deadline)
		connect.AssertEqual(t, len(report.Dropped), 0)
		connect.AssertEqual(t, len(report.Blocked), 0)
		connect.AssertEqual(t, leafExists(old), true)

		report, state = family.Retire(ctx, old.Upper.Add(providerUsageArchiveRetention), state, deadline)
		connect.AssertEqual(t, report.Dropped, []string{family.LeafName(testArchiveTable, old)})
		connect.AssertEqual(t, len(report.Blocked), 0)
		connect.AssertEqual(t, leafExists(old), false)
		rows = testArchiveRows(t, ctx)
		_, kept := rows[oldId]
		connect.AssertEqual(t, kept, false)
		connect.AssertEqual(t, rows[youngId].partition, family.LeafName(testArchiveTable, young))

		// the database's clock keeps the young week however late the caller's is
		youngLeaf := family.LeafName(testArchiveTable, young)
		report, _ = family.Retire(ctx, young.Upper.Add(providerUsageArchiveRetention), state, deadline)
		connect.AssertEqual(t, len(report.Dropped), 0)
		if blocker := report.Blocked[youngLeaf]; !strings.Contains(blocker, "is kept until") {
			t.Fatalf("the young week was not kept by the database's clock: %q", blocker)
		}
		connect.AssertEqual(t, leafExists(young), true)
		connect.AssertEqual(t, testArchiveRows(t, ctx)[youngId].partition, youngLeaf)
		// the current week and the DEFAULT partition stay
		connect.AssertEqual(t, leafExists(family.RangeAt(now)), true)
		connect.AssertEqual(t, leafExists(PartitionRange{Default: true}), true)
	})
}

// Only the guarded drop drops a week. The fence refuses, before anything is
// detached, a drop transaction without the retention opt-in, a range that is
// not one week and a week within the horizon; the DEFAULT partition is never
// dropped. With the opt-in an expired week drops.
func TestProviderUsageArchiveDropRefusedWithoutOptIn(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := providerUsageArchivePartitionFamily
		now := NowUtc()
		old := family.RangeAt(now.Add(-60 * contractPartitionWeek))
		_, err := family.EnsureRangeFor(ctx, old.Lower)
		connect.AssertEqual(t, err, nil)
		oldId := testArchiveContractIn(t, ctx, old.Lower.Add(time.Hour), 3)
		oldLeaf := family.LeafName(testArchiveTable, old)
		attached := func(name string) bool {
			return testContractPartitionScalar[bool](t, ctx, `
				SELECT EXISTS (
					SELECT 1 FROM pg_inherits
					WHERE inhparent = to_regclass('public.st_provider_usage_archive') AND inhrelid = to_regclass('public.' || $1)
				)
			`, name)
		}
		refused := func(err error, reason string) {
			t.Helper()
			if err == nil || !strings.Contains(err.Error(), reason) {
				t.Fatalf("the drop was not refused for %q: %v", reason, err)
			}
			connect.AssertEqual(t, attached(oldLeaf), true)
			connect.AssertEqual(t, testArchiveRows(t, ctx)[oldId].partition, oldLeaf)
		}

		// the registry's drop transaction without its opt-in
		withoutOptIn := *family
		withoutOptIn.DropGuardSetting = ""
		_, err = withoutOptIn.DropRange(ctx, old)
		refused(err, "only under the retention opt-in")
		// another drop transaction that runs the fence without the opt-in
		refused(contractPartitionTx(ctx, func(tx PgTx) {
			providerUsageArchiveDropFence(ctx, tx, old)
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("ALTER TABLE public.st_provider_usage_archive DETACH PARTITION public.%s", quoteIdentifier(oldLeaf))))
		}), "only under the retention opt-in")
		// two weeks at once
		_, err = family.DropRange(ctx, PartitionRange{Lower: old.Lower, Upper: old.Upper.Add(contractPartitionWeek)})
		refused(err, "whole weeks only")
		// a week within the horizon, by the database's clock
		current := family.RangeAt(now)
		_, err = family.DropRange(ctx, current)
		refused(err, "is kept until")
		connect.AssertEqual(t, attached(family.LeafName(testArchiveTable, current)), true)
		// the DEFAULT partition
		_, err = family.DropRange(ctx, PartitionRange{Default: true})
		refused(err, "never dropped")
		connect.AssertEqual(t, attached(PartitionDefaultLeafName(testArchiveTable)), true)

		dropped, err := family.DropRange(ctx, old)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, dropped, []string{oldLeaf})
		connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.' || $1) IS NULL`, oldLeaf), true)
	})
}

// The contract tables' archive step deletes a retired week's contracts
// through the capture trigger, and each one's proof passes the guard and lands
// in its own contract week of the partitioned archive: a week from before the
// contract cutover, archived 23 weeks later from the legacy partition, and a
// week after the cutover. Those archive weeks exist before the contracts are
// written, and the runway keeps the archive four weeks ahead at any later
// time.
func TestProviderUsageArchiveContractRetentionWritesHistoricalWeek(t *testing.T) {
	testArchivePlainEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := providerUsageArchivePartitionFamily
		now := NowUtc()
		cutoverWeek := ContractPartitionWeekStart(now)
		week := func(i int) time.Time {
			return cutoverWeek.Add(time.Duration(i) * contractPartitionWeek)
		}

		// a contract three weeks before the cutover is the oldest, so the
		// archive's weeks start with its week
		ApplyDbMigrationsUpTo(ctx, ProviderUsageArchivePartitionMigrationVersion()-1)
		historicalId := testContractIdAt(week(-3).Add(time.Hour))
		testInsertContractRows(t, ctx, historicalId, true)
		ApplyDbMigrations(ctx)
		Testing_PartitionContracts(ctx)
		laterId := testContractIdAt(week(1).Add(time.Hour))
		testInsertContractRows(t, ctx, laterId, true)
		sourceUsages := map[Id]string{
			historicalId: testArchiveSourceUsage(t, ctx, historicalId),
			laterId:      testArchiveSourceUsage(t, ctx, laterId),
		}
		connect.AssertEqual(t, testContractPartitionOf(t, ctx, "transfer_contract", historicalId), ContractLegacyPartitionName("transfer_contract"))
		connect.AssertEqual(t, testContractPartitionOf(t, ctx, "transfer_contract", laterId), ContractPartitionName("transfer_contract", week(1)))

		weekStarts, defaultPartition := testArchiveLeaves(t, ctx)
		connect.AssertEqual(t, defaultPartition, true)
		if !testArchiveWeeksEqual(weekStarts, testArchiveWeeks(week(-3), week(ContractPartitionAheadWeeks))) {
			t.Fatalf("the archive has weeks %v before retention writes them", weekStarts)
		}
		// at the cutover the archive's runway already covers the contract weeks
		runway := family.EnsureRunway(ctx, now)
		connect.AssertEqual(t, runway.Created, []string(nil))
		connect.AssertEqual(t, runway.CurrentCovered, true)
		connect.AssertEqual(t, runway.FutureRanges, ContractPartitionAheadWeeks)

		later := week(20).Add(time.Hour)
		report, _ := MaintainContractPartitions(ctx, later, ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, len(report.Blocked), 0)
		connect.AssertEqual(t, report.Archived, int64(2))
		connect.AssertEqual(t, slices.Contains(report.Dropped, ContractLegacyPartitionName("transfer_contract")), true)
		connect.AssertEqual(t, slices.Contains(report.Dropped, ContractPartitionName("transfer_contract", week(1))), true)

		rows := testArchiveRows(t, ctx)
		connect.AssertEqual(t, len(rows), 2)
		for contractId, weekStart := range map[Id]time.Time{historicalId: week(-3), laterId: week(1)} {
			connect.AssertEqual(t, rows[contractId].partition, family.LeafName(testArchiveTable, family.RangeAt(weekStart)))
			connect.AssertEqual(t, *rows[contractId].providerUsage, sourceUsages[contractId])
			connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM transfer_contract WHERE contract_id = $1)`, contractId), false)
		}

		// at that later time the runway creates the archive's weeks ahead again
		runway = family.EnsureRunway(ctx, later)
		connect.AssertEqual(t, len(runway.Errors), 0)
		connect.AssertEqual(t, runway.CurrentCovered, true)
		connect.AssertEqual(t, runway.FutureRanges, ContractPartitionAheadWeeks)
		connect.AssertEqual(t, len(runway.Created), ContractPartitionAheadWeeks+1)
	})
}
