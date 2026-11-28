// S1: the contract days retire at the contract horizon reaper's horizon, keep
// every row the reaper keeps, and lock nothing a vacuum or a long transaction
// holds (CONTRACT-PARTITION.md, "S1"; daily since 2026-11-17).
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/connect/v2026"
)

const testDay = 24 * time.Hour

// One maintenance transaction with options, without retries; returns what it
// raised.
func testContractTxWith(ctx context.Context, callback func(PgTx), options ...any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if recoveredErr, ok := r.(error); ok {
				err = recoveredErr
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	MaintenanceTx(ctx, callback, append([]any{OptNoRetry()}, options...)...)
	return
}

// A day expires RetireDelay (6 hours) past the reaper's horizon while ctx
// carries one (the reaper's mode is on), clamped to [14, 91] days, and at the
// 91 days of ContractRetention otherwise; never later than ContractRetention.
// The family's ranges are UTC days and its RangeExpired follows this, so a day
// 35 days and 6 hours past its end expires at a 35-day horizon, and one 18
// days and 6 hours past its end at the C1 transition's 18 days (owner,
// 2026-11-23), which the 35-day floor before held to 35 days and 6 hours.
// Before S1 the family declared no RangeExpired, and every range waited 91
// days: 13-17 TiB at Main's 140-180 GiB a day; with weeks, a range waited up
// to 7 days more.
func TestContractDayRetentionFollowsTheReaperHorizon(t *testing.T) {
	ctx := context.Background()
	settings := DefaultContractDayRetirementSettings()
	connect.AssertEqual(t, ContractDayRetention(ctx), ContractRetention)
	for horizon, want := range map[time.Duration]time.Duration{
		35 * testDay:  35 * testDay,
		20 * testDay:  20 * testDay,
		18 * testDay:  18 * testDay,
		14 * testDay:  14 * testDay,
		13 * testDay:  ContractDayRetentionFloor,
		time.Hour:     ContractDayRetentionFloor,
		60 * testDay:  60 * testDay,
		120 * testDay: ContractRetention,
	} {
		connect.AssertEqual(t, ContractDayRetention(WithContractHorizon(ctx, horizon)), want)
	}
	connect.AssertEqual(t, contractDayExpiry(WithContractHorizon(ctx, 35*testDay), settings), 35*testDay+settings.RetireDelay)
	connect.AssertEqual(t, contractDayExpiry(WithContractHorizon(ctx, 18*testDay), settings), 18*testDay+settings.RetireDelay)
	connect.AssertEqual(t, contractDayExpiry(WithContractHorizon(ctx, ContractRetention), settings), ContractRetention)
	connect.AssertEqual(t, contractDayExpiry(ctx, settings), ContractRetention)

	// a range is one UTC day, from 00:00
	day := contractPartitionFamily.RangeAt(time.Date(2026, 12, 21, 12, 0, 0, 0, time.UTC))
	connect.AssertEqual(t, day, PartitionRange{Lower: time.Date(2026, 12, 21, 0, 0, 0, 0, time.UTC), Upper: time.Date(2026, 12, 22, 0, 0, 0, 0, time.UTC)})
	connect.AssertEqual(t, settings.RetireDelay, 6*time.Hour)
	horizonCtx := WithContractHorizon(ctx, 35*testDay)
	at := day.Upper.Add(35*testDay + settings.RetireDelay)
	connect.AssertEqual(t, at, time.Date(2027, 1, 26, 6, 0, 0, 0, time.UTC))
	connect.AssertEqual(t, contractPartitionFamily.IsExpired(horizonCtx, at.Add(-time.Second), day), false)
	connect.AssertEqual(t, contractPartitionFamily.IsExpired(horizonCtx, at, day), true)
	// the next day expires a day later
	next := contractPartitionFamily.RangeAt(day.Upper)
	connect.AssertEqual(t, contractPartitionFamily.IsExpired(horizonCtx, at, next), false)
	connect.AssertEqual(t, contractPartitionFamily.IsExpired(horizonCtx, at.Add(testDay), next), true)
	// without the reaper's horizon (mode off, or the control unread): 91 days
	connect.AssertEqual(t, contractPartitionFamily.IsExpired(ctx, at, day), false)
	connect.AssertEqual(t, contractPartitionFamily.IsExpired(ctx, day.Upper.Add(ContractRetention), day), true)
	// the legacy partition retires the same way, from the cutover day
	legacy := PartitionRange{Upper: day.Lower, Legacy: true}
	connect.AssertEqual(t, contractPartitionFamily.IsExpired(horizonCtx, day.Lower.Add(35*testDay+settings.RetireDelay), legacy), true)
	// at the transition's 18 days a day expires 18 days and 6 hours past its
	// end, and so does the legacy partition past the cutover day
	transitionCtx := WithContractHorizon(ctx, 18*testDay)
	at = day.Upper.Add(18*testDay + settings.RetireDelay)
	connect.AssertEqual(t, contractPartitionFamily.IsExpired(transitionCtx, at.Add(-time.Second), day), false)
	connect.AssertEqual(t, contractPartitionFamily.IsExpired(transitionCtx, at, day), true)
	connect.AssertEqual(t, contractPartitionFamily.IsExpired(transitionCtx, day.Lower.Add(18*testDay+settings.RetireDelay-time.Second), legacy), false)
	connect.AssertEqual(t, contractPartitionFamily.IsExpired(transitionCtx, day.Lower.Add(18*testDay+settings.RetireDelay), legacy), true)
	// below the floor a day waits for the floor
	belowCtx := WithContractHorizon(ctx, 13*testDay)
	connect.AssertEqual(t, contractPartitionFamily.IsExpired(belowCtx, day.Upper.Add(13*testDay+settings.RetireDelay), day), false)
	connect.AssertEqual(t, contractPartitionFamily.IsExpired(belowCtx, day.Upper.Add(14*testDay+settings.RetireDelay), day), true)
	// the shortest retention is the reaper's floor (model's
	// TestContractDayRetentionFloorIsTheReaperFloor)
	connect.AssertEqual(t, ContractDayRetentionFloor, 14*testDay)
}

// Each lock a retirement step requests waits at most a second, and its work
// under the lock is cut off soon enough that an application statement queued
// behind it stays under PgLockTimeout. The compaction's statements take the
// leaf's lock first, then rewrite it along its key with every planner cost
// zero (CLUSTER then always scans the key, never the whole heap), then clear
// the index's clustered mark. Before S1 a drop waited up to 5 s for each of
// its locks (DropLockTimeout).
func TestContractDayRetirementLockBoundsStayBelowTheApplicationBound(t *testing.T) {
	settings := DefaultContractDayRetirementSettings()
	if PgLockTimeout/2 < settings.LockTimeout {
		t.Errorf("retirement waits up to %s for a lock; application statements queued behind it give up after %s", settings.LockTimeout, PgLockTimeout)
	}
	if PgLockTimeout <= settings.LockTimeout+settings.CompactTimeout || PgLockTimeout <= settings.LockTimeout+settings.DropTimeout {
		t.Errorf("a lock wait of %s and work of %s or %s outlast the application lock bound %s", settings.LockTimeout, settings.CompactTimeout, settings.DropTimeout, PgLockTimeout)
	}
	if PgLockTimeout/2 < contractPartitionFamily.settings().DropLockTimeout {
		t.Errorf("the family's drop waits up to %s for each lock", contractPartitionFamily.settings().DropLockTimeout)
	}
	if settings.LockSettings.MaxTransactionAge <= 0 || settings.LockSettings.MaxTransactionAge > DefaultContractPartitionLockSettings().MaxTransactionAge {
		t.Errorf("retirement admits transactions up to %s old", settings.LockSettings.MaxTransactionAge)
	}

	leaf := contractDayLeaf{name: "transfer_escrow_p20261221", primaryKey: "transfer_escrow_p20261221_pkey"}
	statements := contractCompactLeafSql(leaf, settings)
	lockAt := slices.IndexFunc(statements, func(statement string) bool { return strings.HasPrefix(statement, "LOCK TABLE ") })
	clusterAt := slices.IndexFunc(statements, func(statement string) bool { return strings.HasPrefix(statement, "CLUSTER ") })
	if lockAt < 0 || clusterAt < lockAt {
		t.Fatalf("the compaction must take its lock before CLUSTER: %v", statements)
	}
	if wait := testContractLockTimeoutOf(t, statements[0]); PgLockTimeout/2 < wait {
		t.Errorf("the compaction waits up to %s for its lock", wait)
	}
	for _, statement := range contractCompactPlannerSql {
		at := slices.Index(statements, statement)
		if at < lockAt || clusterAt < at {
			t.Errorf("%q must apply to CLUSTER", statement)
		}
	}
	for _, cost := range []string{"seq_page_cost", "random_page_cost", "cpu_tuple_cost", "cpu_index_tuple_cost", "cpu_operator_cost"} {
		if !slices.Contains(statements, fmt.Sprintf("SET LOCAL %s = 0", cost)) {
			t.Errorf("CLUSTER's choice of scan still reads %s", cost)
		}
	}
	connect.AssertEqual(t, statements[clusterAt], `CLUSTER public."transfer_escrow_p20261221" USING "transfer_escrow_p20261221_pkey"`)
	connect.AssertEqual(t, statements[len(statements)-1], `ALTER TABLE public."transfer_escrow_p20261221" SET WITHOUT CLUSTER`)
	timeouts := regexp.MustCompile(`^SET LOCAL statement_timeout = '([0-9]+)ms'$`)
	work := ""
	for _, statement := range statements[lockAt:clusterAt] {
		if groups := timeouts.FindStringSubmatch(statement); groups != nil {
			work = groups[1]
		}
	}
	connect.AssertEqual(t, work, fmt.Sprintf("%d", settings.CompactTimeout.Milliseconds()))
}

// Settings for a test day of a few rows: every leaf that holds a tuple is
// compacted, and a lock wait would last the test.
func testContractDaySettings() *ContractDayRetirementSettings {
	settings := DefaultContractDayRetirementSettings()
	settings.CompactAboveBytes = 0
	return settings
}

// A cut over test database, its days from the current one.
func testContractDays(t testing.TB, ctx context.Context) (day func(i int) PartitionRange) {
	t.Helper()
	Testing_PartitionContracts(ctx)
	partitions, err := listContractPartitions(ctx, "transfer_contract")
	if err != nil {
		t.Fatal(err)
	}
	var cutover time.Time
	for _, partition := range partitions {
		if partition.legacy {
			cutover = partition.upper
		}
	}
	if cutover.IsZero() {
		t.Fatal("no legacy partition")
	}
	return func(i int) PartitionRange {
		return contractPartitionFamily.RangeAt(cutover.AddDate(0, 0, i))
	}
}

// The physical relation of each of the day's leaves.
func testContractDayFilenodes(t testing.TB, ctx context.Context, r PartitionRange) (filenodes []string) {
	t.Helper()
	for _, leaf := range contractPartitionFamily.LeafNames(r) {
		filenodes = append(filenodes, testContractPartitionScalar[string](t, ctx, `SELECT coalesce((SELECT pg_relation_filenode(to_regclass('public.' || $1))::text), '')`, leaf))
	}
	return
}

func testContractDayExists(t testing.TB, ctx context.Context, r PartitionRange) (exists []bool) {
	t.Helper()
	for _, leaf := range contractPartitionFamily.LeafNames(r) {
		exists = append(exists, testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, leaf))
	}
	return
}

// A day the reaper emptied is compacted and dropped once its upper bound is
// the reaper's horizon (here its floor, 14 days) and 6 hours past, while the
// reaper's mode is on: not before, and not at that age without the horizon,
// when it waits for 91 days. The build before S1 kept every range 91 days.
func TestContractDayRetiresAtTheReaperHorizon(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := testContractDays(t, ctx)
		reaped := day(1)
		contractIds := []Id{testContractIdAt(reaped.Lower.Add(time.Hour)), testContractIdAt(reaped.Lower.Add(2 * time.Hour))}
		for _, contractId := range contractIds {
			testInsertContractRows(t, ctx, contractId, true)
			testReapContract(t, ctx, contractId)
		}
		testVacuumContractDay(t, ctx, reaped)
		horizonCtx := WithContractHorizon(ctx, ContractDayRetentionFloor)
		settings := DefaultContractDayRetirementSettings()
		expiry := reaped.Upper.Add(ContractDayRetentionFloor + settings.RetireDelay)

		for _, early := range []struct {
			ctx context.Context
			now time.Time
		}{
			{horizonCtx, expiry.Add(-time.Minute)},
			{ctx, expiry.Add(time.Minute)},
		} {
			report, _ := MaintainContractPartitions(early.ctx, early.now, ContractRetentionCursor{}, time.Minute)
			connect.AssertEqual(t, len(report.Compacted), 0)
			connect.AssertEqual(t, slices.Contains(report.Dropped, ContractPartitionName("transfer_contract", reaped.Lower)), false)
			connect.AssertEqual(t, testContractDayExists(t, ctx, reaped), []bool{true, true, true, true, true, true})
		}

		report, _ := MaintainContractPartitions(horizonCtx, expiry.Add(time.Minute), ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, len(report.Blocked), 0)
		for _, leaf := range contractPartitionFamily.LeafNames(reaped) {
			connect.AssertEqual(t, slices.Contains(report.Compacted, leaf), true)
			connect.AssertEqual(t, slices.Contains(report.Dropped, leaf), true)
		}
		connect.AssertEqual(t, testContractDayExists(t, ctx, reaped), []bool{false, false, false, false, false, false})
		// the next day is not expired yet
		connect.AssertEqual(t, testContractDayExists(t, ctx, day(2)), []bool{true, true, true, true, true, true})
		// the reaper archived, retirement did not delete
		for _, contractId := range contractIds {
			connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM st_provider_usage_archive WHERE contract_id = $1)`, contractId), true)
		}
	})
}

// Compaction keeps every row version the leaf holds, with its transaction ids
// (CLUSTER): rows the reaper keeps, unchanged, and rows it deleted that an
// older snapshot can still see. A REPEATABLE READ transaction whose snapshot
// predates both the reaper's delete and the compaction reads the same rows
// after it as before, the deleted contract included, and a later pass, once
// that transaction ends, compacts the deleted rows away. Pre-S1, a range past
// 91 days lost every resolved contract in it to the archive step.
func TestContractDayCompactionKeepsEveryRowVersion(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := testContractDays(t, ctx)
		r := day(1)
		keptIds := []Id{}
		reapedIds := []Id{}
		for i := range 40 {
			contractId := testContractIdAt(r.Lower.Add(time.Duration(i+1) * time.Minute))
			testInsertContractRows(t, ctx, contractId, true)
			if i%10 == 0 {
				keptIds = append(keptIds, contractId)
			} else {
				reapedIds = append(reapedIds, contractId)
			}
		}
		// one more the reaper deletes after the snapshot below
		lateId := testContractIdAt(r.Lower.Add(time.Hour))
		testInsertContractRows(t, ctx, lateId, true)
		for _, contractId := range reapedIds {
			testReapContract(t, ctx, contractId)
		}
		testVacuumContractDay(t, ctx, r)
		before := map[Id][]string{}
		for _, contractId := range append(slices.Clone(keptIds), lateId) {
			before[contractId] = testContractRows(t, ctx, contractId)
		}
		xminsSql := fmt.Sprintf(`SELECT string_agg(contract_id::text || ':' || xmin::text, ',' ORDER BY contract_id) FROM public.%s WHERE contract_id = ANY($1::uuid[])`, quoteIdentifier(ContractPartitionName("transfer_contract", r.Lower)))
		xmins := testContractPartitionScalar[string](t, ctx, xminsSql, keptIds)

		// a REPEATABLE READ snapshot that predates the reaper's next delete and
		// the compaction, held by its own session until the end
		readRequests := make(chan Id)
		readResults := make(chan []string)
		snapshotTaken := make(chan struct{})
		snapshotDone := make(chan error, 1)
		go func() {
			snapshotDone <- testContractTxWith(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `SELECT 1`))
				close(snapshotTaken)
				for contractId := range readRequests {
					rows := []string{}
					for _, table := range ContractPartitionTables {
						var row string
						Raise(tx.QueryRow(ctx, fmt.Sprintf(`SELECT coalesce(string_agg(row_to_json(row)::text, ',' ORDER BY row_to_json(row)::text), '') FROM public.%s AS row WHERE contract_id = $1`, quoteIdentifier(table)), contractId).Scan(&row))
						rows = append(rows, row)
					}
					readResults <- rows
				}
			}, pgx.RepeatableRead, pgx.ReadOnly)
		}()
		select {
		case <-snapshotTaken:
		case err := <-snapshotDone:
			t.Fatalf("the snapshot holder failed: %v", err)
		}
		snapshotOpen := true
		closeSnapshot := func() {
			if snapshotOpen {
				snapshotOpen = false
				close(readRequests)
				if err := <-snapshotDone; err != nil {
					t.Fatalf("the snapshot holder failed: %v", err)
				}
			}
		}
		defer closeSnapshot()
		testReapContract(t, ctx, lateId)

		filenodes := testContractDayFilenodes(t, ctx, r)
		horizonCtx := WithContractHorizon(ctx, ContractDayRetentionFloor)
		now := r.Upper.Add(ContractRetention + time.Hour)
		report, _ := maintainContractPartitions(horizonCtx, now, ContractRetentionCursor{}, time.Minute, testContractDaySettings())
		connect.AssertEqual(t, len(report.Blocked), 0)
		for _, leaf := range contractPartitionFamily.LeafNames(r) {
			connect.AssertEqual(t, slices.Contains(report.Compacted, leaf), true)
		}
		connect.AssertEqual(t, slices.Contains(report.Dropped, ContractPartitionName("transfer_contract", r.Lower)), false)
		after := testContractDayFilenodes(t, ctx, r)
		for i := range filenodes {
			if filenodes[i] == after[i] {
				t.Errorf("%s was not rewritten", contractPartitionFamily.LeafNames(r)[i])
			}
		}
		// every kept row is exactly as it was, with its transaction ids
		for _, contractId := range keptIds {
			connect.AssertEqual(t, testContractRows(t, ctx, contractId), before[contractId])
		}
		connect.AssertEqual(t, testContractPartitionScalar[string](t, ctx, xminsSql, keptIds), xmins)
		connect.AssertEqual(t, report.Kept[ContractPartitionName("transfer_contract", r.Lower)], int64(len(keptIds)*len(ContractPartitionTables)))
		// the older snapshot still reads the contract the reaper deleted
		// after it, and every kept one
		for _, contractId := range append(slices.Clone(keptIds), lateId) {
			readRequests <- contractId
			connect.AssertEqual(t, <-readResults, before[contractId])
		}
		connect.AssertEqual(t, testContractRows(t, ctx, lateId), []string{"", "", "", "", "", ""})
		closeSnapshot()

		// with that snapshot gone, the reaper's last delete goes too, and once
		// the reaper deletes the rest the day drops
		for _, contractId := range keptIds {
			testReapContract(t, ctx, contractId)
		}
		testWaitForContractHorizon(t, ctx)
		report, _ = maintainContractPartitions(horizonCtx, now, ContractRetentionCursor{}, time.Minute, testContractDaySettings())
		connect.AssertEqual(t, len(report.Blocked), 0)
		connect.AssertEqual(t, slices.Contains(report.Dropped, ContractPartitionName("transfer_contract", r.Lower)), true)
		connect.AssertEqual(t, testContractDayExists(t, ctx, r), []bool{false, false, false, false, false, false})
	})
}

// Another database on the test's server, dropped when the test ends, and a
// connection to it. Named as the test environment names its databases, so an
// interrupted test's database is swept as an orphan.
func testContractOtherDatabase(t testing.TB, ctx context.Context) (name string, conn *pgx.Conn) {
	t.Helper()
	name = fmt.Sprintf("test_%d_%s", NowUtc().UnixMilli(), strings.ReplaceAll(NewId().String(), "-", ""))
	Db(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE template0`, quoteIdentifier(name))))
	}, OptReadWrite())
	conn = acquireTestPgDbLease(ctx, name)
	t.Cleanup(func() {
		closeCtx := context.WithoutCancel(ctx)
		conn.Close(closeCtx)
		Db(closeCtx, func(conn PgConn) {
			RaisePgResult(conn.Exec(closeCtx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, quoteIdentifier(name))))
		}, OptReadWrite())
	})
	return
}

// The root cause of the flake of the model's
// TestContractWeekRetirementKeepsWhatTheReaperKeeps on the shared local
// server (2026-11-17, "transfer_contract_w20261123 stayed once empty"). Once
// no backend of this database predates the reaper's last deletes of a day,
// they are dead to every snapshot of it; but CLUSTER takes its horizon from
// its own snapshot too, whose xmin is the oldest transaction running anywhere
// on the server. While a transaction of another database that took its
// transaction id before the deletes runs, a compaction copies the deleted rows
// as recently dead, and the day stays. The wait the S1 tests used read only
// this database's backends, so it returned here and the drop the test then
// expected did not happen. The run reports the day waiting for those rows,
// not as rows the reaper keeps; Testing_WaitForSnapshotHorizon names the other
// database's transaction and waits for it, and once it ends the next run drops
// the day.
func TestContractDayCompactionSeesTransactionsOfOtherDatabases(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := testContractDays(t, ctx)
		r := day(1)
		contractId := testContractIdAt(r.Lower.Add(time.Hour))
		testInsertContractRows(t, ctx, contractId, true)
		dayName := ContractPartitionName("transfer_contract", r.Lower)

		// a transaction of another database, with a transaction id from before
		// the reaper's delete
		other, otherConn := testContractOtherDatabase(t, ctx)
		otherTx, err := otherConn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer otherTx.Rollback(context.WithoutCancel(ctx))
		// as pg_stat_activity shows it: the 32 bits of the full id
		var otherXid string
		Raise(otherTx.QueryRow(ctx, `SELECT (pg_current_xact_id()::text::numeric % 4294967296)::bigint::text`).Scan(&otherXid))
		testReapContract(t, ctx, contractId)

		// the S1 tests' wait sees no backend of this database from before the
		// delete and returns at once
		connect.AssertEqual(t, testWaitForThisDatabaseHorizon(t, ctx, time.Minute), "")
		// the wait that reads the server's horizon does not
		err = Testing_WaitForSnapshotHorizon(ctx, time.Second)
		if err == nil || !strings.Contains(err.Error(), "database "+other) || !strings.Contains(err.Error(), "transaction id "+otherXid) {
			t.Fatalf("the horizon wait did not name the other database's transaction %s: %v", otherXid, err)
		}

		horizonCtx := WithContractHorizon(ctx, ContractDayRetentionFloor)
		now := r.Upper.Add(ContractRetention + time.Hour)
		report, _ := maintainContractPartitions(horizonCtx, now, ContractRetentionCursor{}, time.Minute, testContractDaySettings())
		for _, leaf := range contractPartitionFamily.LeafNames(r) {
			connect.AssertEqual(t, slices.Contains(report.Compacted, leaf), true)
		}
		// CLUSTER copied the deleted rows: the day stays, reported waiting for
		// them, not kept for the reaper
		connect.AssertEqual(t, testContractDayExists(t, ctx, r), []bool{true, true, true, true, true, true})
		connect.AssertEqual(t, 0 < testContractPartitionScalar[int64](t, ctx, `SELECT pg_relation_size(to_regclass('public.' || $1))`, dayName), true)
		_, kept := report.Kept[dayName]
		connect.AssertEqual(t, kept, false)
		if !strings.Contains(report.Blocked[dayName], "only rows deleted after the oldest open transaction") {
			t.Fatalf("the day was not reported waiting for its deleted rows: %+v", report.Blocked)
		}
		connect.AssertEqual(t, slices.Contains(report.Dropped, dayName), false)

		// once the other database's transaction ends, the wait returns, and
		// the next run empties the leaves and drops the day
		if err := otherTx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		testWaitForContractHorizon(t, ctx)
		report, _ = maintainContractPartitions(horizonCtx, now, ContractRetentionCursor{}, time.Minute, testContractDaySettings())
		connect.AssertEqual(t, len(report.Blocked), 0)
		connect.AssertEqual(t, slices.Contains(report.Dropped, dayName), true)
		connect.AssertEqual(t, testContractDayExists(t, ctx, r), []bool{false, false, false, false, false, false})
	})
}

// Retirement never queues a lock request behind a lock held on a day's leaf,
// here a vacuum's SHARE UPDATE EXCLUSIVE held by a transaction older than the
// admission's age: it leaves that leaf, its ANALYZE and the day's drop for a
// later run, while it compacts the day's other leaves. Queued, the request
// would stall every statement on the leaf (and, for the drop, every contract
// statement) for its lock timeout, or cancel a routine autovacuum after
// deadlock_timeout. The build before S1 read only the parents' locks and
// transactions, and its drop's DETACH waited up to 5 s behind the leaf.
func TestContractDayRetirementDoesNotQueueBehindALeafLock(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		day := testContractDays(t, ctx)
		r := day(1)
		contractId := testContractIdAt(r.Lower.Add(time.Hour))
		testInsertContractRows(t, ctx, contractId, true)
		testReapContract(t, ctx, contractId)
		testVacuumContractDay(t, ctx, r)
		held := ContractPartitionName("transfer_escrow", r.Lower)

		holderPid := make(chan uint32, 1)
		release := make(chan struct{})
		holderDone := make(chan error, 1)
		go func() {
			holderDone <- contractPartitionTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`LOCK TABLE public.%s IN SHARE UPDATE EXCLUSIVE MODE`, quoteIdentifier(held))))
				holderPid <- tx.Conn().PgConn().PID()
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}()
		var pid uint32
		select {
		case pid = <-holderPid:
		case err := <-holderDone:
			t.Fatalf("the leaf lock holder failed: %v", err)
		}
		releaseHolder := func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}
		defer func() {
			releaseHolder()
			<-holderDone
		}()

		// a vacuum-like holder, older than the admission's transaction age by
		// the database clock
		age := DefaultContractPartitionLockSettings().MaxTransactionAge + time.Second
		MaintenanceDb(ctx, func(conn PgConn) {
			for {
				var old bool
				Raise(conn.QueryRow(ctx, `SELECT clock_timestamp() - xact_start > make_interval(secs => $2) FROM pg_stat_activity WHERE pid = $1`, pid, age.Seconds()).Scan(&old))
				if old {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
		settings := testContractDaySettings()
		// a lock wait would last the test
		settings.LockTimeout = time.Minute
		now := r.Upper.Add(ContractRetention + time.Hour)
		reportDone := make(chan *ContractPartitionReport, 1)
		go func() {
			report, _ := maintainContractPartitions(WithContractHorizon(ctx, ContractDayRetentionFloor), now, ContractRetentionCursor{}, time.Minute, settings)
			reportDone <- report
		}()

		queued := false
		var report *ContractPartitionReport
		MaintenanceDb(ctx, func(conn PgConn) {
			for {
				select {
				case report = <-reportDone:
					return
				default:
				}
				var waiting bool
				Raise(conn.QueryRow(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_locks AS waiting
					WHERE NOT waiting.granted AND $1::int = ANY(pg_blocking_pids(waiting.pid)))`, pid).Scan(&waiting))
				if waiting {
					queued = true
					releaseHolder()
					report = <-reportDone
					return
				}
				select {
				case report = <-reportDone:
					return
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
		})
		if queued {
			t.Fatalf("retirement queued a lock request behind the lock on %s (report %+v)", held, report)
		}
		if !strings.Contains(report.Blocked[held], fmt.Sprintf("pid %d", pid)) {
			t.Fatalf("the held leaf was not reported waiting for its holder: %+v", report.Blocked)
		}
		for _, leaf := range contractPartitionFamily.LeafNames(r) {
			connect.AssertEqual(t, slices.Contains(report.Compacted, leaf), leaf != held)
			connect.AssertEqual(t, slices.Contains(report.Dropped, leaf), false)
		}
		// the closed leaf's ANALYZE waited for a later run too
		connect.AssertEqual(t, slices.Contains(report.Analyzed, held), false)
		connect.AssertEqual(t, testContractDayExists(t, ctx, r), []bool{true, true, true, true, true, true})

		// once the holder is gone, the next run finishes the day
		releaseHolder()
		if err := <-holderDone; err != nil {
			t.Fatalf("the leaf lock holder failed: %v", err)
		}
		holderDone <- nil
		report, _ = maintainContractPartitions(WithContractHorizon(ctx, ContractDayRetentionFloor), now, ContractRetentionCursor{}, time.Minute, testContractDaySettings())
		connect.AssertEqual(t, len(report.Blocked), 0)
		connect.AssertEqual(t, testContractDayExists(t, ctx, r), []bool{false, false, false, false, false, false})
	})
}

// An autovacuum worker on a day's leaf, whatever its age, keeps both the
// compaction of that leaf and the day's drop from requesting a lock: the
// worker's pass would be cancelled after deadlock_timeout, or, anti-wraparound,
// would hold every queued statement for the request's lock timeout. No test
// session can be an autovacuum worker, so the admission's read is shown one.
func TestContractDayRetirementWaitsOutAnAutovacuum(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := testContractDays(t, ctx)
		r := day(1)
		contractId := testContractIdAt(r.Lower.Add(time.Hour))
		testInsertContractRows(t, ctx, contractId, true)
		testReapContract(t, ctx, contractId)
		testVacuumContractDay(t, ctx, r)
		vacuumed := ContractPartitionName("contract_close", r.Lower)

		scopes := 0
		contractScopeLocksObserved = func(scope *contractLockScope, locks []contractScopeLock) []contractScopeLock {
			if slices.Contains(scope.relations, "public."+vacuumed) {
				scopes += 1
				locks = append(locks, contractScopeLock{
					pid:            4242,
					backendType:    "autovacuum worker",
					relation:       vacuumed,
					mode:           "ShareUpdateExclusiveLock",
					granted:        true,
					transactionAge: 0,
				})
			}
			return locks
		}
		defer func() {
			contractScopeLocksObserved = nil
		}()

		filenodes := testContractDayFilenodes(t, ctx, r)
		now := r.Upper.Add(ContractRetention + time.Hour)
		report, _ := maintainContractPartitions(WithContractHorizon(ctx, ContractDayRetentionFloor), now, ContractRetentionCursor{}, time.Minute, testContractDaySettings())
		if !strings.Contains(report.Blocked[vacuumed], "autovacuum worker") {
			t.Fatalf("the vacuumed leaf was not reported waiting for the autovacuum: %+v", report.Blocked)
		}
		connect.AssertEqual(t, 1 <= scopes, true)
		after := testContractDayFilenodes(t, ctx, r)
		for i, leaf := range contractPartitionFamily.LeafNames(r) {
			connect.AssertEqual(t, after[i] == filenodes[i], leaf == vacuumed)
		}
		connect.AssertEqual(t, testContractDayExists(t, ctx, r), []bool{true, true, true, true, true, true})

		// with the leaf compacted, the day's drop also waits for the worker
		contractScopeLocksObserved = func(scope *contractLockScope, locks []contractScopeLock) []contractScopeLock {
			if slices.Contains(scope.relations, "public."+vacuumed) && 0 < len(scope.only) {
				locks = append(locks, contractScopeLock{pid: 4242, backendType: "autovacuum worker", relation: vacuumed, mode: "ShareUpdateExclusiveLock", granted: true})
			}
			return locks
		}
		report, _ = maintainContractPartitions(WithContractHorizon(ctx, ContractDayRetentionFloor), now, ContractRetentionCursor{}, time.Minute, testContractDaySettings())
		if !strings.Contains(report.Blocked[ContractPartitionName("transfer_contract", r.Lower)], "autovacuum worker") {
			t.Fatalf("the drop was not reported waiting for the autovacuum: %+v", report.Blocked)
		}
		connect.AssertEqual(t, testContractDayExists(t, ctx, r), []bool{true, true, true, true, true, true})

		contractScopeLocksObserved = nil
		report, _ = maintainContractPartitions(WithContractHorizon(ctx, ContractDayRetentionFloor), now, ContractRetentionCursor{}, time.Minute, testContractDaySettings())
		connect.AssertEqual(t, len(report.Blocked), 0)
		connect.AssertEqual(t, testContractDayExists(t, ctx, r), []bool{false, false, false, false, false, false})
	})
}

// A day that holds more live rows than a compaction may move under its lock,
// or that the statistics say the reaper has not emptied, is reported and left
// as it is: no lock, no rewrite, no drop.
func TestContractDayRetirementBoundsItsWork(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := testContractDays(t, ctx)
		r := day(1)
		for i := range 3 {
			testInsertContractRows(t, ctx, testContractIdAt(r.Lower.Add(time.Duration(i+1)*time.Hour)), true)
		}
		testVacuumContractDay(t, ctx, r)
		filenodes := testContractDayFilenodes(t, ctx, r)
		horizonCtx := WithContractHorizon(ctx, ContractDayRetentionFloor)
		now := r.Upper.Add(ContractRetention + time.Hour)

		settings := testContractDaySettings()
		settings.MaxCompactRows = 2
		report, _ := maintainContractPartitions(horizonCtx, now, ContractRetentionCursor{}, time.Minute, settings)
		for _, leaf := range contractPartitionFamily.LeafNames(r) {
			connect.AssertEqual(t, strings.Contains(report.Blocked[leaf], "more than 2 live rows"), true)
		}
		connect.AssertEqual(t, len(report.Compacted), 0)
		connect.AssertEqual(t, testContractDayFilenodes(t, ctx, r), filenodes)

		settings = testContractDaySettings()
		settings.ProbeGateRows = 2
		report, _ = maintainContractPartitions(horizonCtx, now, ContractRetentionCursor{}, time.Minute, settings)
		for _, leaf := range contractPartitionFamily.LeafNames(r) {
			connect.AssertEqual(t, strings.Contains(report.Blocked[leaf], "by the statistics"), true)
		}
		connect.AssertEqual(t, len(report.Compacted), 0)
		connect.AssertEqual(t, testContractDayFilenodes(t, ctx, r), filenodes)

		// within the bounds the day is compacted and kept
		report, _ = maintainContractPartitions(horizonCtx, now, ContractRetentionCursor{}, time.Minute, testContractDaySettings())
		connect.AssertEqual(t, len(report.Blocked), 0)
		connect.AssertEqual(t, report.Kept[ContractPartitionName("transfer_contract", r.Lower)], int64(3*len(ContractPartitionTables)))
	})
}

// On plain contract tables retirement does nothing: no lock, no rewrite, no
// drop, whatever the horizon and the rows' age.
func TestContractDayRetirementIsANoOpOnPlainTables(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if IsContractPartitioned(ctx) {
			t.Skip("plain contract tables only")
		}
		now := NowUtc()
		old := []Id{testContractIdAt(now.Add(-100 * testDay)), testContractIdAt(now.Add(-60 * testDay))}
		for _, contractId := range old {
			testInsertContractRows(t, ctx, contractId, true)
		}
		testReapContract(t, ctx, old[0])
		filenodes := []string{}
		for _, table := range ContractPartitionTables {
			filenodes = append(filenodes, testContractPartitionScalar[string](t, ctx, `SELECT pg_relation_filenode(to_regclass('public.' || $1))::text`, table))
		}
		before := testContractRows(t, ctx, old[1])

		report, _ := maintainContractPartitions(WithContractHorizon(ctx, ContractDayRetentionFloor), now.Add(200*testDay), ContractRetentionCursor{}, time.Minute, testContractDaySettings())
		connect.AssertEqual(t, report.Partitioned, false)
		connect.AssertEqual(t, len(report.Compacted), 0)
		connect.AssertEqual(t, len(report.Dropped), 0)
		connect.AssertEqual(t, len(report.Kept), 0)
		connect.AssertEqual(t, len(report.Blocked), 0)
		for i, table := range ContractPartitionTables {
			connect.AssertEqual(t, testContractPartitionScalar[string](t, ctx, `SELECT relkind::text FROM pg_class WHERE oid = to_regclass('public.' || $1)`, table), "r")
			connect.AssertEqual(t, testContractPartitionScalar[string](t, ctx, `SELECT pg_relation_filenode(to_regclass('public.' || $1))::text`, table), filenodes[i])
		}
		connect.AssertEqual(t, testContractRows(t, ctx, old[1]), before)
	})
}

// A task cursor written before S1 still carries the archive step's position,
// and one written by the weekly build the week whose parents it analyzed. It
// decodes, keeps its other fields, and the next cursor carries neither:
// retirement no longer deletes contracts, so there is nothing to resume, and
// the parents are analyzed once more, for the day.
func TestContractRetentionCursorIgnoresTheArchiveStep(t *testing.T) {
	var cursor ContractRetentionCursor
	if err := json.Unmarshal([]byte(`{"archive_partition":"transfer_contract_w20261221","archive_cursor":"01a1e8d6-df9f-4dc9-5869-68d7a787f914","analyzed_week":"2026-12-21","evidence_leaf":"contract_close_report_evidence_w20261221"}`), &cursor); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, cursor.AnalyzedDay, "")
	connect.AssertEqual(t, cursor.EvidenceLeaf, "contract_close_report_evidence_w20261221")
	encoded, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"archive_partition"`, `"archive_cursor"`, `"analyzed_week"`} {
		if strings.Contains(string(encoded), field) {
			t.Errorf("the next cursor keeps %s: %s", field, encoded)
		}
	}
	if err := json.Unmarshal([]byte(`{"analyzed_day":"2026-12-21"}`), &cursor); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, cursor.AnalyzedDay, "2026-12-21")
}

// A day the reaper has emptied retires even when vacuum has not run since:
// Main's leaves inherit vacuum thresholds of 5M to 25M dead rows, so the
// reaper's last deletes of a day can stay unvacuumed for good. The gate reads
// the live rows the statistics count, which follow the deletes, and the
// count's walk marks the deleted rows' key entries dead for CLUSTER. Gating on
// dead rows, or on the row estimate the last vacuum or analyze left, would
// keep such a day forever.
func TestContractDayRetiresWithoutVacuum(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := testContractDays(t, ctx)
		r := day(1)
		for i := range 200 {
			contractId := testContractIdAt(r.Lower.Add(time.Duration(i+1) * time.Minute))
			testInsertContractRows(t, ctx, contractId, true)
			testReapContract(t, ctx, contractId)
		}
		// no vacuum; the statistics count the deletes and every snapshot sees them
		leaf := ContractPartitionName("transfer_contract", r.Lower)
		deadline := time.Now().Add(time.Minute)
		for testContractPartitionScalar[int64](t, ctx, `SELECT coalesce((SELECT n_live_tup FROM pg_stat_user_tables WHERE relid = to_regclass('public.' || $1)), -1)`, leaf) != 0 {
			if time.Now().After(deadline) {
				t.Fatal("the statistics never counted the deletes")
			}
			time.Sleep(200 * time.Millisecond)
		}
		testWaitForContractHorizon(t, ctx)
		connect.AssertEqual(t, 0 < testContractPartitionScalar[int64](t, ctx, `SELECT pg_relation_size(to_regclass('public.' || $1))`, leaf), true)

		settings := testContractDaySettings()
		// fewer rows than the day's deleted ones
		settings.ProbeGateRows = 10
		now := r.Upper.Add(ContractRetention + time.Hour)
		report, _ := maintainContractPartitions(WithContractHorizon(ctx, ContractDayRetentionFloor), now, ContractRetentionCursor{}, time.Minute, settings)
		connect.AssertEqual(t, len(report.Blocked), 0)
		connect.AssertEqual(t, slices.Contains(report.Compacted, leaf), true)
		connect.AssertEqual(t, slices.Contains(report.Dropped, leaf), true)
		connect.AssertEqual(t, testContractDayExists(t, ctx, r), []bool{false, false, false, false, false, false})
	})
}
