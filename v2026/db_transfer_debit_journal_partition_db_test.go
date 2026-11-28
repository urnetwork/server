// transfer_debit_journal's daily partitions against PostgreSQL with the
// migrated schema (db_transfer_debit_journal_partition.go,
// db_transfer_debit_journal_cutover.go): debits route by contract day, late
// and far-off ones to DEFAULT; the settlement statements apply and delete
// across leaves and the pending debit guard sees every leaf; a day drops only
// once two days past and empty, by the guard and the registry's recheck; a
// create moves stragglers out of DEFAULT; the rotation's locks are brief under
// concurrent settlement, which loses and repeats no debit; and the cutover
// prepares, aborts, finalizes, rolls back and abandons as designed.
package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A fresh balance. Its debits carry testDebitJournalShard, which the table's
// shard check requires.
func testDebitJournalBalance() Id {
	return NewId()
}

func testDebitJournalShard(balanceId Id) int {
	return int(balanceId[15]) % 16
}

func testDebitJournalInsert(t testing.TB, ctx context.Context, contractId Id, balanceId Id) {
	t.Helper()
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO transfer_debit_journal (contract_id, balance_id, shard, debit_byte_count) VALUES ($1, $2, $3, 1024)`,
			contractId,
			balanceId,
			testDebitJournalShard(balanceId),
		))
	}, TxReadCommitted)
}

// The relation that holds the debit, or "" when it is gone.
func testDebitJournalRelation(t testing.TB, ctx context.Context, contractId Id, balanceId Id) (relation string) {
	t.Helper()
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, `SELECT tableoid::regclass::text FROM transfer_debit_journal WHERE contract_id = $1 AND balance_id = $2`, contractId, balanceId)
		WithPgResult(result, err, func() {
			if result.Next() {
				Raise(result.Scan(&relation))
			}
		})
	})
	return
}

func testDebitJournalLeaf(day time.Time) string {
	return transferDebitJournalPartitionFamily.LeafName(transferDebitJournalTable, PartitionGranularityDaily.RangeOf(day))
}

func testDebitJournalExists(t testing.TB, ctx context.Context, relation string) bool {
	t.Helper()
	return testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+relation)
}

// Settlement in the flush's shape (model/subscription_transfer_debit.go): lock
// the balance's debits, mark them applied, then delete them, each in a read
// committed transaction with the flush's statement and lock bounds.
func testDebitJournalSettle(ctx context.Context, balanceId Id, contractIds []Id) (applied int64, deleted int64, err error) {
	err = partitionRegistryRun(func() {
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='250ms'`))
			rows, queryErr := tx.Query(ctx, `SELECT contract_id FROM transfer_debit_journal WHERE balance_id=$1 ORDER BY contract_id LIMIT 512 FOR UPDATE SKIP LOCKED`, balanceId)
			WithPgResult(rows, queryErr, func() {
				for rows.Next() {
				}
			})
			applied = RaisePgResult(tx.Exec(ctx, `UPDATE transfer_debit_journal SET applied=true WHERE balance_id=$1 AND contract_id=ANY($2) AND NOT applied`, balanceId, contractIds)).RowsAffected()
		}, TxReadCommitted, OptNoRetry())
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout='250ms'`))
			deleted = RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_debit_journal WHERE balance_id=$1 AND contract_id=ANY($2) AND applied`, balanceId, contractIds)).RowsAffected()
		}, TxReadCommitted, OptNoRetry())
	})
	return
}

// The family's declaration: daily by contract id, three days ahead, a DEFAULT
// partition, the legacy partition retiring like a day, two days of retention
// matching the guard's, the published storage settings on every leaf, lock
// waits short enough that a settlement statement queued behind one, then
// behind its hold, stays within the flush's 250 ms, and out of the REINDEX
// rotation.
func TestTransferDebitJournalFamilyDeclaration(t *testing.T) {
	family := transferDebitJournalPartitionFamily
	if family.Name != "transfer_debit_journal" || !slices.Equal(family.Tables, []string{"transfer_debit_journal"}) || family.KeyColumn != "contract_id" ||
		family.KeyKind != PartitionKeyId || family.Granularity != PartitionGranularityDaily || family.AheadRanges != 3 {
		t.Fatalf("unexpected family: %+v", family)
	}
	if family.Retention != 48*time.Hour || !strings.Contains(transferDebitJournalDropGuardSql, "interval '2 days'") {
		t.Fatal("the family's retention and the guard's disagree")
	}
	if !family.DefaultPartition || family.KeepLegacyPartition || !family.ReindexExempt || family.DropGuardSetting != transferDebitJournalRetentionSetting {
		t.Fatalf("unexpected family rules: %+v", family)
	}
	if !slices.Contains(family.LeafReloptions, "fillfactor=70") {
		t.Fatal("leaves would lose the published fillfactor")
	}
	// the holds measured below are 24–38 ms for an attach, about 30 ms for a
	// drop's remainder and about 100 ms for the swap
	settings := DefaultTransferDebitJournalPartitionSettings()
	const holdBudget = 120 * time.Millisecond
	for _, wait := range []time.Duration{settings.LockTimeout, family.settings().DropLockTimeout, family.settings().CreateLockTimeout} {
		if 250*time.Millisecond < wait+holdBudget {
			t.Fatalf("a %s lock wait plus a hold reaches the flush's 250 ms bound", wait)
		}
	}
	if !dbMaintenanceShouldReindexTable("transfer_debit_journal") || dbMaintenanceShouldReindexRelation("transfer_debit_journal", true) {
		t.Fatal("the REINDEX rotation would rebuild the partitioned journal")
	}
	if name := family.LeafName(transferDebitJournalTable, PartitionGranularityDaily.RangeOf(time.Date(2026, 11, 21, 5, 0, 0, 0, time.UTC))); name != "transfer_debit_journal_p20261121" {
		t.Fatal(name)
	}
	if r := family.RangeAt(time.Date(2026, 11, 21, 5, 0, 0, 0, time.UTC)); PartitionKeyId.BoundSql(r.Lower) != contractIdLiteral(ContractIdFloor(time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC))) {
		t.Fatal(PartitionKeyId.BoundSql(r.Lower))
	}
}

// After the cutover a debit lands in its contract's day, a debit of a contract
// before the cutover day in the legacy partition, and one of a contract far
// ahead in DEFAULT. The flush's statements apply and delete them in every
// leaf, and the pending debit guard on transfer_balance sees a debit in any
// leaf.
func TestTransferDebitJournalRoutesDebitsByContractDay(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		today := PartitionGranularityDaily.RangeStart(now)
		cutoverDay := PartitionGranularityDaily.AddRanges(today, -3)
		balanceId := testDebitJournalBalance()
		// a debit of an old contract written before the cutover
		before := testContractIdAt(PartitionGranularityDaily.AddRanges(cutoverDay, -1).Add(time.Hour))
		testDebitJournalInsert(t, ctx, before, balanceId)
		Testing_PartitionTransferDebitJournal(ctx, cutoverDay)
		if !IsTransferDebitJournalPartitioned(ctx) {
			t.Fatal("the journal is not partitioned")
		}

		current := testContractIdAt(now)
		late := testContractIdAt(PartitionGranularityDaily.AddRanges(cutoverDay, -2))
		ahead := testContractIdAt(now.Add(30 * 24 * time.Hour))
		for _, contractId := range []Id{current, late, ahead} {
			testDebitJournalInsert(t, ctx, contractId, balanceId)
		}
		for contractId, want := range map[Id]string{
			before:  transferDebitJournalLegacyLeafName(),
			late:    transferDebitJournalLegacyLeafName(),
			current: testDebitJournalLeaf(now),
			ahead:   transferDebitJournalDefaultLeafName(),
		} {
			if relation := testDebitJournalRelation(t, ctx, contractId, balanceId); relation != want {
				t.Fatalf("debit of %s in %q, want %s", ContractIdTime(contractId).Format(time.RFC3339), relation, want)
			}
		}

		// the pending debit guard reads the parent, so every leaf
		var guardErr error
		guardErr = partitionRegistryRun(func() {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance (balance_id, network_id, start_time, end_time, start_balance_byte_count, net_revenue_nano_cents, balance_byte_count) VALUES ($1, $2, now(), now() + interval '1 day', 1, 0, 1)`, balanceId, NewId()))
			}, TxReadCommitted, OptNoRetry())
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id = $1`, balanceId))
			}, TxReadCommitted, OptNoRetry())
		})
		if guardErr == nil || !strings.Contains(guardErr.Error(), "pending asynchronous consumption") {
			t.Fatal("the pending debit guard let a balance with debits in day leaves go", guardErr)
		}

		contractIds := []Id{before, late, current, ahead}
		applied, deleted, err := testDebitJournalSettle(ctx, balanceId, contractIds)
		if err != nil || applied != 4 || deleted != 4 {
			t.Fatalf("settled applied=%d deleted=%d: %v", applied, deleted, err)
		}
		for _, contractId := range contractIds {
			if relation := testDebitJournalRelation(t, ctx, contractId, balanceId); relation != "" {
				t.Fatalf("a settled debit stays in %s", relation)
			}
		}
	})
}

// A day drops only once its upper bound is two days past and it holds no row:
// the registry's retirement drops the empty old days and the drained legacy
// partition, keeps a day holding a debit (pending or applied) and the days
// inside two days, and drops the kept day once its debit is gone. The guard
// refuses on its own a drop without the opt-in, of a day inside two days, of a
// day holding a debit, and of DEFAULT.
func TestTransferDebitJournalDropsOnlyEmptyDaysTwoDaysPast(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		today := PartitionGranularityDaily.RangeStart(now)
		cutoverDay := PartitionGranularityDaily.AddRanges(today, -6)
		Testing_PartitionTransferDebitJournal(ctx, cutoverDay)
		family := transferDebitJournalPartitionFamily

		heldDay := PartitionGranularityDaily.AddRanges(cutoverDay, 1)
		balanceId := testDebitJournalBalance()
		held := testContractIdAt(heldDay.Add(time.Hour))
		testDebitJournalInsert(t, ctx, held, balanceId)
		recentDay := PartitionGranularityDaily.AddRanges(today, -1)

		guardRefusal := func(statements string) string {
			_, message := testProviderWorkSessionRolledBack(ctx, statements)
			return message
		}
		if message := guardRefusal(fmt.Sprintf(`SELECT transfer_debit_journal_drop_guard('%s')`, testDebitJournalLeaf(cutoverDay))); !strings.Contains(message, "dropped only by their retention") {
			t.Fatalf("the guard without the opt-in: %q", message)
		}
		optIn := `SET LOCAL urnetwork.transfer_debit_journal_retention = 'on'; `
		if message := guardRefusal(optIn + fmt.Sprintf(`SELECT transfer_debit_journal_drop_guard('%s')`, testDebitJournalLeaf(recentDay))); !strings.Contains(message, "kept 2 days") {
			t.Fatalf("the guard on a day inside two days: %q", message)
		}
		if message := guardRefusal(optIn + fmt.Sprintf(`SELECT transfer_debit_journal_drop_guard('%s')`, testDebitJournalLeaf(heldDay))); !strings.Contains(message, "holds debits") {
			t.Fatalf("the guard on a day holding a debit: %q", message)
		}
		if message := guardRefusal(optIn + fmt.Sprintf(`SELECT transfer_debit_journal_drop_guard('%s')`, transferDebitJournalDefaultLeafName())); !strings.Contains(message, "kept 2 days") {
			t.Fatalf("the guard on DEFAULT: %q", message)
		}
		if message := guardRefusal(optIn + fmt.Sprintf(`SELECT transfer_debit_journal_drop_guard('%s')`, testDebitJournalLeaf(cutoverDay))); message != "" {
			t.Fatalf("the guard refused an empty day six days past: %q", message)
		}

		report, _ := family.Retire(ctx, now, PartitionFamilyState{}, time.Now().Add(time.Minute))
		dropped := map[string]bool{}
		for _, name := range report.Dropped {
			dropped[name] = true
		}
		for day := cutoverDay; day.Before(today); day = PartitionGranularityDaily.NextRangeStart(day) {
			leaf := testDebitJournalLeaf(day)
			expired := !now.Before(PartitionGranularityDaily.NextRangeStart(day).Add(TransferDebitJournalRetention))
			switch {
			case day.Equal(heldDay):
				if dropped[leaf] || !testDebitJournalExists(t, ctx, leaf) {
					t.Fatalf("%s holding a debit was dropped", leaf)
				}
			case expired:
				if !dropped[leaf] || testDebitJournalExists(t, ctx, leaf) {
					t.Fatalf("%s, empty and two days past, was not dropped: %v", leaf, report.Dropped)
				}
			default:
				if dropped[leaf] || !testDebitJournalExists(t, ctx, leaf) {
					t.Fatalf("%s inside two days was dropped", leaf)
				}
			}
		}
		if !dropped[transferDebitJournalLegacyLeafName()] || testDebitJournalExists(t, ctx, transferDebitJournalLegacyLeafName()) {
			t.Fatalf("the drained legacy partition was not retired: %v", report.Dropped)
		}
		if blocker := report.Blocked[testDebitJournalLeaf(heldDay)]; !strings.Contains(blocker, "not yet applied and released") {
			t.Fatalf("the held day's blocker: %q (%v)", blocker, report.Blocked)
		}
		if !testDebitJournalExists(t, ctx, transferDebitJournalDefaultLeafName()) {
			t.Fatal("DEFAULT was dropped")
		}

		// applied but not yet released still holds the day
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `UPDATE transfer_debit_journal SET applied = true WHERE contract_id = $1`, held))
		}, TxReadCommitted)
		if report, _ := family.Retire(ctx, now, PartitionFamilyState{}, time.Now().Add(time.Minute)); slices.Contains(report.Dropped, testDebitJournalLeaf(heldDay)) {
			t.Fatal("a day holding an applied debit awaiting its release was dropped")
		}
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_debit_journal WHERE contract_id = $1`, held))
		}, TxReadCommitted)
		if report, _ := family.Retire(ctx, now, PartitionFamilyState{}, time.Now().Add(time.Minute)); !slices.Contains(report.Dropped, testDebitJournalLeaf(heldDay)) {
			t.Fatalf("the emptied day was not dropped: %v %v", report.Dropped, report.Blocked)
		}
	})
}

// A late debit that lands in a day after the registry's unlocked check keeps
// the day: the fence's guard reads the leaf again under its lock, and the
// drop rolls back with the debit attached. From that lock on no statement can
// reach the leaf, and the registry reads it once more after the detach.
func TestTransferDebitJournalDropRefusesADebitAfterItsCheck(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		today := PartitionGranularityDaily.RangeStart(NowUtc())
		cutoverDay := PartitionGranularityDaily.AddRanges(today, -5)
		Testing_PartitionTransferDebitJournal(ctx, cutoverDay)
		r := PartitionGranularityDaily.RangeOf(cutoverDay)
		leaf := testDebitJournalLeaf(cutoverDay)
		balanceId := testDebitJournalBalance()
		late := testContractIdAt(cutoverDay.Add(time.Hour))

		// the debit is written just after the registry's first, unlocked
		// read admits the day
		family := *transferDebitJournalPartitionFamily
		firstRead := true
		family.DropPrecondition = func(ctx context.Context, conn PgQuerier, r PartitionRange) string {
			if firstRead {
				firstRead = false
				testDebitJournalInsert(t, ctx, late, balanceId)
				return ""
			}
			return transferDebitJournalDayBlocker(ctx, conn, r)
		}
		if blocker := family.DropPrecondition(ctx, nil, r); blocker != "" {
			t.Fatal(blocker)
		}
		_, err := family.DropRange(ctx, r)
		if err == nil || (!strings.Contains(err.Error(), "holds debits") && !strings.Contains(err.Error(), "not yet applied")) {
			t.Fatal("the drop took a day holding a debit", err)
		}
		if relation := testDebitJournalRelation(t, ctx, late, balanceId); relation != leaf {
			t.Fatalf("the late debit is in %q, want %s", relation, leaf)
		}
	})
}

// A create of a day whose contracts already reached DEFAULT (a clock far
// ahead) moves them into the new day before its attach, so the attach is not
// refused, and reports how long the attach held its locks.
func TestTransferDebitJournalCreateMovesStragglersOutOfDefault(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		today := PartitionGranularityDaily.RangeStart(now)
		Testing_PartitionTransferDebitJournal(ctx, PartitionGranularityDaily.AddRanges(today, -1))
		farDay := PartitionGranularityDaily.AddRanges(today, TransferDebitJournalAheadDays+2)
		balanceId := testDebitJournalBalance()
		straggler := testContractIdAt(farDay.Add(3 * time.Hour))
		testDebitJournalInsert(t, ctx, straggler, balanceId)
		if relation := testDebitJournalRelation(t, ctx, straggler, balanceId); relation != transferDebitJournalDefaultLeafName() {
			t.Fatalf("the straggler is in %q", relation)
		}
		report, err := createTransferDebitJournalLeaf(ctx, transferDebitJournalTable, PartitionGranularityDaily.RangeOf(farDay), DefaultTransferDebitJournalPartitionSettings())
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("create of %s: attach held its locks %s, moved %d", report.Leaf, report.AttachDuration, report.Moved)
		if report.Moved != 1 || report.Skipped || report.Leaf != testDebitJournalLeaf(farDay) {
			t.Fatalf("unexpected create: %+v", report)
		}
		if relation := testDebitJournalRelation(t, ctx, straggler, balanceId); relation != testDebitJournalLeaf(farDay) {
			t.Fatalf("the straggler is in %q after the create", relation)
		}
		if again, err := createTransferDebitJournalLeaf(ctx, transferDebitJournalTable, PartitionGranularityDaily.RangeOf(farDay), DefaultTransferDebitJournalPartitionSettings()); err != nil || !again.Skipped {
			t.Fatalf("a second create did not skip: %+v %v", again, err)
		}
		// the leaf carries the published storage settings
		if reloptions := testContractPartitionScalar[[]string](t, ctx, `SELECT coalesce(reloptions, ARRAY[]::text[]) FROM pg_class WHERE oid = to_regclass($1)`, "public."+report.Leaf); !slices.Equal(reloptions, transferDebitJournalLeafReloptions) {
			t.Fatalf("leaf storage settings %v", reloptions)
		}
	})
}

// The rotation under settlement: while workers insert, apply and delete debits
// with the flush's own statements and lock bound, the runway creates a day and
// the retention drops one. Every debit inserted is applied and deleted exactly
// once, none is lost, and the measured lock holds are reported.
func TestTransferDebitJournalRotationUnderSettlement(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		today := PartitionGranularityDaily.RangeStart(now)
		Testing_PartitionTransferDebitJournal(ctx, PartitionGranularityDaily.AddRanges(today, -5))
		family := transferDebitJournalPartitionFamily
		dropDay := PartitionGranularityDaily.AddRanges(today, -5)
		createDay := PartitionGranularityDaily.AddRanges(today, TransferDebitJournalAheadDays+1)

		var inserted, applied, deleted, failed atomic.Int64
		var maxLatency, maxInsertStatement atomic.Int64
		stop := make(chan struct{})
		var workers sync.WaitGroup
		for range 4 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					balanceId := testDebitJournalBalance()
					contractIds := []Id{testContractIdAt(NowUtc()), testContractIdAt(NowUtc().Add(-time.Hour))}
					start := time.Now()
					insertErr := partitionRegistryRun(func() {
						Tx(ctx, func(tx PgTx) {
							RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout='250ms'`))
							for _, contractId := range contractIds {
								// the statement's own time by the database's clock, lock waits included
								var seconds float64
								Raise(tx.QueryRow(
									ctx,
									`INSERT INTO transfer_debit_journal (contract_id, balance_id, shard, debit_byte_count) VALUES ($1, $2, $3, 1)
									RETURNING extract(epoch FROM clock_timestamp() - statement_timestamp())::float8`,
									contractId, balanceId, testDebitJournalShard(balanceId),
								).Scan(&seconds))
								if statement := int64(seconds * 1e6); maxInsertStatement.Load() < statement {
									maxInsertStatement.Store(statement)
								}
							}
						}, TxReadCommitted, OptNoRetry())
					})
					if insertErr != nil {
						failed.Add(1)
						continue
					}
					inserted.Add(int64(len(contractIds)))
					// settle until done; a lock timeout behind the rotation is retried, as the flush's next cadence does
					for remaining, attempts := len(contractIds), 0; 0 < remaining; attempts++ {
						if 100 <= attempts {
							failed.Add(1000000)
							return
						}
						a, d, err := testDebitJournalSettle(ctx, balanceId, contractIds)
						applied.Add(a)
						deleted.Add(d)
						remaining -= int(d)
						if err != nil {
							failed.Add(1)
						}
					}
					if latency := time.Since(start).Microseconds(); maxLatency.Load() < latency {
						maxLatency.Store(latency)
					}
				}
			}()
		}

		// the rotation runs once settlement is flowing, and settlement goes on
		// after it on the new layout
		awaitInserted := func(count int64) bool {
			deadline := time.Now().Add(time.Minute)
			for inserted.Load() < count {
				if time.Now().After(deadline) {
					return false
				}
				time.Sleep(time.Millisecond)
			}
			return true
		}
		flowing := awaitInserted(16)
		createStart := time.Now()
		createReport, createErr := createTransferDebitJournalLeaf(ctx, transferDebitJournalTable, PartitionGranularityDaily.RangeOf(createDay), DefaultTransferDebitJournalPartitionSettings())
		createElapsed := time.Since(createStart)
		dropStart := time.Now()
		dropped, dropErr := family.DropRange(ctx, PartitionGranularityDaily.RangeOf(dropDay))
		dropElapsed := time.Since(dropStart)
		after := awaitInserted(inserted.Load() + 16)
		close(stop)
		workers.Wait()
		if !flowing || !after {
			t.Fatalf("settlement did not flow around the rotation: before %t, after %t", flowing, after)
		}
		if createErr != nil || dropErr != nil {
			t.Fatalf("rotation failed: create %v, drop %v", createErr, dropErr)
		}
		t.Logf("rotation under settlement: create of %s attach held %s (whole create %s); drop of %v took %s end to end; %d debits, %d lock-bounded retries, slowest insert statement %s (database clock), slowest debit end to end %s",
			createReport.Leaf, createReport.AttachDuration, createElapsed, dropped, dropElapsed, inserted.Load(), failed.Load(),
			time.Duration(maxInsertStatement.Load())*time.Microsecond, time.Duration(maxLatency.Load())*time.Microsecond)
		if inserted.Load() == 0 || applied.Load() != inserted.Load() || deleted.Load() != inserted.Load() {
			t.Fatalf("inserted %d, applied %d, deleted %d", inserted.Load(), applied.Load(), deleted.Load())
		}
		if remaining := testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM transfer_debit_journal`); remaining != 0 {
			t.Fatalf("%d debits left", remaining)
		}
	})
}

// The drop's locks against one settlement statement, ordered by barriers: the
// fence takes the parent, DEFAULT and the leaf; a settlement read of the
// parent then waits; the drop's rest (guard, detach, recheck, drop, commit)
// runs and the read completes. The read's wait after the release is that
// rest, measured by the database's clock and reported.
func TestTransferDebitJournalDropHoldsItsLocksBriefly(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		today := PartitionGranularityDaily.RangeStart(NowUtc())
		cutoverDay := PartitionGranularityDaily.AddRanges(today, -5)
		Testing_PartitionTransferDebitJournal(ctx, cutoverDay)
		locked, release := make(chan struct{}), make(chan struct{})
		settings := DefaultTransferDebitJournalPartitionSettings()
		settings.afterDropLockForTest = func() {
			close(locked)
			<-release
		}
		family := *transferDebitJournalPartitionFamily
		family.DropFence = func(ctx context.Context, tx PgTx, r PartitionRange) {
			fenceTransferDebitJournalDrop(ctx, tx, r, settings)
		}
		dropDone := make(chan error, 1)
		go func() {
			_, err := family.DropRange(ctx, PartitionGranularityDaily.RangeOf(cutoverDay))
			dropDone <- err
		}()
		<-locked
		type readResult struct {
			err error
			// the database's clock once the read held its locks
			granted time.Time
		}
		readDone := make(chan readResult, 1)
		go func() {
			var granted time.Time
			err := partitionRegistryRun(func() {
				Tx(ctx, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout='2s'`))
					var any bool
					Raise(tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM transfer_debit_journal WHERE shard = 3), clock_timestamp()`).Scan(&any, &granted))
				}, TxReadCommitted, OptNoRetry())
			})
			readDone <- readResult{err: err, granted: granted}
		}()
		deadline := time.Now().Add(30 * time.Second)
		for !testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'relation' AND NOT granted)`) {
			if time.Now().After(deadline) {
				t.Fatal("the settlement read never waited on the drop")
			}
			time.Sleep(time.Millisecond)
		}
		released := testContractPartitionScalar[time.Time](t, ctx, `SELECT clock_timestamp()`)
		close(release)
		if err := <-dropDone; err != nil {
			t.Fatal(err)
		}
		read := <-readDone
		if read.err != nil {
			t.Fatal(read.err)
		}
		t.Logf("after the release a settlement read waited %s (database clock) for the drop's guard, detach, recheck, drop and commit", read.granted.Sub(released))
		if testDebitJournalExists(t, ctx, testDebitJournalLeaf(cutoverDay)) {
			t.Fatal("the day was not dropped")
		}
	})
}

// The cutover: prepare stages the parent, DEFAULT and the days and validates
// the bound while debits keep landing in the plain table; the bound refuses a
// debit of a contract at or past the cutover day until the swap; abort undoes
// the prepare; an interrupted or lock-starved swap changes nothing; finalize
// swaps; a rollback brings the plain table back with every debit written since.
// Dry runs change nothing. Each exclusive step is one script, and reports how
// long it held its lock.
func TestTransferDebitJournalCutoverPrepareAbortFinalizeRollback(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()
		cutoverDay := PartitionGranularityDaily.AddRanges(PartitionGranularityDaily.RangeStart(now), 2)
		settings := DefaultTransferDebitJournalPartitionSettings()
		settings.MaxAttempts = 1
		settings.RetryInterval = 10 * time.Millisecond
		// the open writer below must meet the swap's lock wait, not its quiet wait
		settings.MaxTransactionAge = time.Hour
		migrate := func(options TransferDebitJournalPartitionMigrateOptions) error {
			options.Logf = t.Logf
			options.Settings = settings
			return MigrateTransferDebitJournalToPartitions(ctx, options)
		}
		balanceId := testDebitJournalBalance()
		early := testContractIdAt(now)
		testDebitJournalInsert(t, ctx, early, balanceId)

		if err := migrate(TransferDebitJournalPartitionMigrateOptions{CutoverDay: cutoverDay, DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if testDebitJournalExists(t, ctx, transferDebitJournalStagedParentName()) {
			t.Fatal("the prepare's dry run staged")
		}
		if err := migrate(TransferDebitJournalPartitionMigrateOptions{CutoverDay: PartitionGranularityDaily.RangeStart(now)}); err == nil || !strings.Contains(err.Error(), "problem") {
			t.Fatal("prepare accepted a cutover day inside its lead", err)
		}
		if err := migrate(TransferDebitJournalPartitionMigrateOptions{CutoverDay: cutoverDay}); err != nil {
			t.Fatal(err)
		}
		if !testDebitJournalExists(t, ctx, transferDebitJournalStagedParentName()) || IsTransferDebitJournalPartitioned(ctx) {
			t.Fatal("prepare did not stage, or swapped")
		}
		// a contract at the cutover day is refused until the swap
		insertErr := partitionRegistryRun(func() {
			testDebitJournalInsert(t, ctx, testContractIdAt(cutoverDay.Add(time.Minute)), balanceId)
		})
		if insertErr == nil || !strings.Contains(insertErr.Error(), contractLegacyBoundName(transferDebitJournalTable)) {
			t.Fatal("the bound let a debit at the cutover day in", insertErr)
		}
		// abort undoes the prepare
		if err := migrate(TransferDebitJournalPartitionMigrateOptions{Abort: true}); err != nil {
			t.Fatal(err)
		}
		if testDebitJournalExists(t, ctx, transferDebitJournalStagedParentName()) {
			t.Fatal("abort left the staged parent")
		}
		if day, prepared := preparedTransferDebitJournalCutoverDay(ctx); prepared || !day.IsZero() {
			t.Fatal("abort left the bound")
		}

		if err := migrate(TransferDebitJournalPartitionMigrateOptions{CutoverDay: cutoverDay}); err != nil {
			t.Fatal(err)
		}
		// an interrupted swap, and one that cannot get its lock, change nothing
		settings.beforeCutoverStepForTest = func(step string) error {
			if step == "swap" {
				return errors.New("synthetic interruption")
			}
			return nil
		}
		if err := migrate(TransferDebitJournalPartitionMigrateOptions{Finalize: true}); err == nil || IsTransferDebitJournalPartitioned(ctx) {
			t.Fatal("the interrupted swap swapped", err)
		}
		settings.beforeCutoverStepForTest = nil
		writer, err := AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		RaisePgResult(writer.Exec(ctx, `BEGIN`))
		RaisePgResult(writer.Exec(ctx, `INSERT INTO transfer_debit_journal (contract_id, balance_id, shard, debit_byte_count) VALUES ($1, $2, $3, 1)`, testContractIdAt(now), balanceId, testDebitJournalShard(balanceId)))
		if err := migrate(TransferDebitJournalPartitionMigrateOptions{Finalize: true}); err == nil || !strings.Contains(err.Error(), "lock timeout") || IsTransferDebitJournalPartitioned(ctx) {
			t.Fatal("the swap ran behind a writer", err)
		}
		RaisePgResult(writer.Exec(ctx, `COMMIT`))
		writer.Release()

		if err := migrate(TransferDebitJournalPartitionMigrateOptions{Finalize: true, DryRun: true}); err != nil || IsTransferDebitJournalPartitioned(ctx) {
			t.Fatal("finalize's dry run swapped", err)
		}
		if err := migrate(TransferDebitJournalPartitionMigrateOptions{Finalize: true}); err != nil {
			t.Fatal(err)
		}
		if !IsTransferDebitJournalPartitioned(ctx) {
			t.Fatal("finalize did not swap")
		}
		if relation := testDebitJournalRelation(t, ctx, early, balanceId); relation != transferDebitJournalLegacyLeafName() {
			t.Fatalf("the debit from before the swap is in %q", relation)
		}
		// written since the swap: one at the cutover day, one far ahead
		atCutover := testContractIdAt(cutoverDay.Add(time.Minute))
		farAhead := testContractIdAt(now.Add(60 * 24 * time.Hour))
		testDebitJournalInsert(t, ctx, atCutover, balanceId)
		testDebitJournalInsert(t, ctx, farAhead, balanceId)
		if relation := testDebitJournalRelation(t, ctx, atCutover, balanceId); relation != testDebitJournalLeaf(cutoverDay) {
			t.Fatalf("a debit at the cutover day is in %q", relation)
		}
		total := testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM transfer_debit_journal`)

		if err := migrate(TransferDebitJournalPartitionMigrateOptions{Rollback: true, DryRun: true}); err != nil || !IsTransferDebitJournalPartitioned(ctx) {
			t.Fatal("rollback's dry run rolled back", err)
		}
		if err := migrate(TransferDebitJournalPartitionMigrateOptions{Rollback: true}); err != nil {
			t.Fatal(err)
		}
		if IsTransferDebitJournalPartitioned(ctx) || testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM transfer_debit_journal`) != total {
			t.Fatal("the rollback lost a debit or left the journal partitioned")
		}
		for _, contractId := range []Id{early, atCutover, farAhead} {
			if relation := testDebitJournalRelation(t, ctx, contractId, balanceId); relation != transferDebitJournalTable {
				t.Fatalf("after the rollback a debit is in %q", relation)
			}
		}
		// the published names are back, with the plain table's storage
		for _, index := range []string{"transfer_debit_journal_pkey", "transfer_debit_journal_contract", "transfer_debit_journal_shard", "transfer_debit_journal_shard_create_time"} {
			if table := testContractPartitionScalar[*string](t, ctx, `SELECT indrelid::regclass::text FROM pg_index WHERE indexrelid = to_regclass($1)`, "public."+index); table == nil || *table != transferDebitJournalTable {
				t.Fatalf("index %s is not back on the table", index)
			}
		}
		if reloptions := testContractPartitionScalar[[]string](t, ctx, `SELECT coalesce(reloptions, ARRAY[]::text[]) FROM pg_class WHERE oid = 'public.transfer_debit_journal'::regclass`); !slices.Contains(reloptions, "fillfactor=70") {
			t.Fatalf("the rolled-back table lost its fillfactor: %v", reloptions)
		}
	})
}

// A swap whose prepared state changes while it waits for its lock changes
// nothing: a session drops the bound under its own lock after the swap read
// the state, the swap's lock request queues behind it, and once that session
// commits the swap's script finds the state changed under the lock and rolls
// back, before its attach could scan the table.
func TestTransferDebitJournalSwapRechecksUnderItsLock(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cutoverDay := PartitionGranularityDaily.AddRanges(PartitionGranularityDaily.RangeStart(NowUtc()), 2)
		settings := DefaultTransferDebitJournalPartitionSettings()
		settings.MaxAttempts = 1
		settings.MaxTransactionAge = time.Hour
		if err := prepareTransferDebitJournalCutover(ctx, cutoverDay, false, false, t.Logf, settings); err != nil {
			t.Fatal(err)
		}
		// the swap waits for its lock as long as the blocker holds it
		settings.LockTimeout = time.Minute
		blocker, err := AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Release()
		settings.beforeSwapScriptForTest = func() error {
			if _, err := blocker.Exec(ctx, `BEGIN`); err != nil {
				return err
			}
			_, err := blocker.Exec(ctx, fmt.Sprintf(`ALTER TABLE transfer_debit_journal DROP CONSTRAINT %s`, contractLegacyBoundName(transferDebitJournalTable)))
			return err
		}
		finalized := make(chan error, 1)
		go func() {
			finalized <- finalizeTransferDebitJournalCutover(ctx, true, false, t.Logf, settings)
		}()
		deadline := time.Now().Add(time.Minute)
		for !testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'relation' AND relation = 'public.transfer_debit_journal'::regclass AND mode = 'AccessExclusiveLock' AND NOT granted)`) {
			if time.Now().After(deadline) {
				t.Fatal("the swap never queued behind the blocker")
			}
			time.Sleep(time.Millisecond)
		}
		if _, err := blocker.Exec(ctx, `COMMIT`); err != nil {
			t.Fatal(err)
		}
		if err := <-finalized; err == nil || !strings.Contains(err.Error(), "changed while the swap waited for its lock") {
			t.Fatal("the swap went ahead on a changed state", err)
		}
		if IsTransferDebitJournalPartitioned(ctx) {
			t.Fatal("the journal was swapped")
		}
		if !testDebitJournalExists(t, ctx, transferDebitJournalStagedParentName()) {
			t.Fatal("the failed swap left no staged parent")
		}
		settings.LockTimeout = DefaultTransferDebitJournalPartitionSettings().LockTimeout
		if err := abortTransferDebitJournalCutover(ctx, false, t.Logf, settings); err != nil {
			t.Fatal(err)
		}
		if _, prepared := preparedTransferDebitJournalCutoverDay(ctx); prepared {
			t.Fatal("the abort left the prepare")
		}
	})
}

// A prepare still unfinalized within the abandon margin of its day is
// abandoned by the runway's before-cutover hook, before its bound could refuse
// a debit; earlier it stays.
func TestTransferDebitJournalAbandonsAStalePrepare(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cutoverDay := PartitionGranularityDaily.AddRanges(PartitionGranularityDaily.RangeStart(NowUtc()), 2)
		settings := DefaultTransferDebitJournalPartitionSettings()
		if err := prepareTransferDebitJournalCutover(ctx, cutoverDay, true, false, t.Logf, settings); err != nil {
			t.Fatal(err)
		}
		if abandonStaleTransferDebitJournalCutover(ctx, cutoverDay.Add(-settings.AbandonMargin-time.Minute), settings, t.Logf) {
			t.Fatal("abandoned a prepare before its margin")
		}
		if day, prepared := preparedTransferDebitJournalCutoverDay(ctx); !prepared || !day.Equal(cutoverDay) {
			t.Fatal("the prepare is gone")
		}
		if !abandonStaleTransferDebitJournalCutover(ctx, cutoverDay.Add(-settings.AbandonMargin+time.Minute), settings, t.Logf) {
			t.Fatal("did not abandon a prepare inside its margin")
		}
		if _, prepared := preparedTransferDebitJournalCutoverDay(ctx); prepared {
			t.Fatal("the abandoned prepare stays")
		}
	})
}

// The cutover's work in progress is not drift: the bound check on the plain
// table and the staged parent with its indexes; other extras stay drift.
func TestSchemaDiffAcceptsTransferDebitJournalCutover(t *testing.T) {
	staged := transferDebitJournalStagedParentName()
	expected := &SchemaSnapshot{
		tables:      map[string]bool{transferDebitJournalTable: true},
		columns:     map[string][]auditColumn{},
		indexes:     map[string]auditIndex{},
		constraints: map[string][]auditConstraint{transferDebitJournalTable: nil},
		partitioned: map[string]bool{},
	}
	actual := &SchemaSnapshot{
		tables:  map[string]bool{transferDebitJournalTable: true, staged: true},
		columns: map[string][]auditColumn{},
		indexes: map[string]auditIndex{
			"transfer_debit_journal_shard_p": {table: staged, def: "CREATE INDEX transfer_debit_journal_shard_p ON ONLY public." + staged + " USING btree (shard, balance_id, contract_id)"},
		},
		constraints: map[string][]auditConstraint{transferDebitJournalTable: {
			{name: contractLegacyBoundName(transferDebitJournalTable), typ: "c", def: "CHECK ((contract_id < '019a9b2a-0000-0000-0000-000000000000'::uuid))"},
			{name: "synthetic_extra_check", typ: "c", def: "CHECK (true)"},
		}},
		partitioned: map[string]bool{staged: true},
	}
	diff := diffSchemas(expected, actual)
	if len(diff.extraTables) != 0 || len(diff.extraIdx) != 0 {
		t.Fatalf("the staged parent drifts: %v %v", diff.extraTables, diff.extraIdx)
	}
	if extra := diff.extraConstraints[transferDebitJournalTable]; !slices.Equal(extra, []string{"synthetic_extra_check"}) {
		t.Fatalf("extra constraints %v", extra)
	}
}
