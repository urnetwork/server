// The table swap's change capture against PostgreSQL
// (db_table_swap_capture.go). Writes of every kind made while the swap copies
// outside its lock end up in the swapped-in table exactly as in the table they
// were made in: inserts, ON CONFLICT merges, updates of any column, the unique
// key and the primary key included, deletes, a unique key reused in one
// transaction, transactions open across the copy's snapshot, and one that
// aborts. The oracle is the old relation itself, which every write reached and
// which the swap keeps: after the swap, the new table holds exactly its rows.
// Under the lock only the changes since the last catch-up are applied, at
// most MaxLockedDeltaRows of them, and the lock is cut off past its bound. A
// run that fails removes its capture; one killed outright leaves it for
// --abort, which the preflight asks for.
package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The synthetic queue's item ids, in order.
func testTableSwapItemIds(t testing.TB, ctx context.Context) []string {
	t.Helper()
	ids := []string{}
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, `SELECT item_id::text FROM test_table_swap_queue ORDER BY item_id`)
		WithPgResult(result, err, func() {
			for result.Next() {
				var id string
				Raise(result.Scan(&id))
				ids = append(ids, id)
			}
		})
	})
	return ids
}

// Runs statements in one read committed maintenance transaction.
func testTableSwapWrite(ctx context.Context, statements ...string) {
	MaintenanceTx(ctx, func(tx PgTx) {
		for _, statement := range statements {
			RaisePgResult(tx.Exec(ctx, statement))
		}
	}, TxReadCommitted, OptNoRetry())
}

// The nth write of a sequence that has every kind a queue sees, on the rows of
// ids: an insert; an ON CONFLICT merge on the unique key; an update by key of
// the payload and a time; an update of the unique key; a delete; a delete and
// an insert reusing its unique key in one transaction; an update of several
// rows by predicate; and a change of the primary key. Each commits on its own.
func testTableSwapWriteKind(ctx context.Context, n int, ids []string, prefix string) {
	id := func(k int) string {
		return ids[k%len(ids)]
	}
	switch n % 8 {
	case 0:
		testTableSwapWrite(ctx, fmt.Sprintf(`INSERT INTO test_table_swap_queue (item_id, queue_key, payload, run_at, release_time) VALUES (gen_random_uuid(), '%s-insert-%d', 'inserted %d', now(), now())`, prefix, n, n))
	case 1:
		testTableSwapWrite(ctx, fmt.Sprintf(`INSERT INTO test_table_swap_queue AS queue (item_id, queue_key, payload, run_at, release_time) VALUES (gen_random_uuid(), '%s-merge-%d', 'merged %d', now(), now()) ON CONFLICT (queue_key) DO UPDATE SET attempt_count = (queue.attempt_count + 1) %% 9, payload = queue.payload || ' +%d'`, prefix, n%4, n, n))
	case 2:
		testTableSwapWrite(ctx, fmt.Sprintf(`UPDATE test_table_swap_queue SET payload = payload || ' updated %d', run_at = run_at + interval '1 second' WHERE item_id = '%s'`, n, id(n)))
	case 3:
		testTableSwapWrite(ctx, fmt.Sprintf(`UPDATE test_table_swap_queue SET queue_key = '%s-moved-%d' WHERE item_id = '%s'`, prefix, n, id(n*7)))
	case 4:
		testTableSwapWrite(ctx, fmt.Sprintf(`DELETE FROM test_table_swap_queue WHERE item_id = '%s'`, id(n*13)))
	case 5:
		key := fmt.Sprintf("%s-merge-%d", prefix, (n/8)%4)
		testTableSwapWrite(
			ctx,
			fmt.Sprintf(`DELETE FROM test_table_swap_queue WHERE queue_key = '%s'`, key),
			fmt.Sprintf(`INSERT INTO test_table_swap_queue (item_id, queue_key, payload, run_at, release_time) VALUES (gen_random_uuid(), '%s', 'reused %d', now(), now())`, key, n),
		)
	case 6:
		testTableSwapWrite(ctx, fmt.Sprintf(`UPDATE test_table_swap_queue SET attempt_count = attempt_count + 1 WHERE item_id IN ('%s', '%s', '%s') AND attempt_count < 8`, id(n), id(n+1), id(n+2)))
	case 7:
		testTableSwapWrite(ctx, fmt.Sprintf(`UPDATE test_table_swap_queue SET item_id = gen_random_uuid() WHERE item_id = '%s'`, id(n*17)))
	}
}

// Writes of every kind in a loop until stopped, each committing on its own.
type testTableSwapWriter struct {
	stop chan struct{}
	done chan struct{}

	stateLock sync.Mutex
	writes    int
	err       error
}

func testTableSwapStartWriter(ctx context.Context, ids []string, prefix string) *testTableSwapWriter {
	writer := &testTableSwapWriter{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(writer.done)
		for n := 0; ; n += 1 {
			select {
			case <-writer.stop:
				return
			default:
			}
			if err := partitionRegistryRun(func() { testTableSwapWriteKind(ctx, n, ids, prefix) }); err != nil {
				writer.stateLock.Lock()
				writer.err = err
				writer.stateLock.Unlock()
				return
			}
			writer.stateLock.Lock()
			writer.writes += 1
			writer.stateLock.Unlock()
		}
	}()
	return writer
}

// Waits until the writer has committed count more writes than when called.
func (self *testTableSwapWriter) await(t testing.TB, count int) {
	t.Helper()
	self.stateLock.Lock()
	target := self.writes + count
	self.stateLock.Unlock()
	deadline := time.Now().Add(time.Minute)
	for {
		self.stateLock.Lock()
		writes, err := self.writes, self.err
		self.stateLock.Unlock()
		if err != nil {
			t.Fatal("the writer failed:", err)
		}
		if target <= writes {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the writer committed %d writes, not %d", writes, target)
		}
		time.Sleep(time.Millisecond)
	}
}

// Stops the writer, and returns how many writes it committed.
func (self *testTableSwapWriter) finish(t testing.TB) int {
	t.Helper()
	select {
	case <-self.stop:
	default:
		close(self.stop)
	}
	<-self.done
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err != nil {
		t.Fatal("the writer failed:", self.err)
	}
	return self.writes
}

// A transaction held open on its own session across steps of a swap.
type testTableSwapOpenTx struct {
	conn PgConn
}

func testTableSwapBegin(t testing.TB, ctx context.Context, statements ...string) *testTableSwapOpenTx {
	t.Helper()
	conn, err := AcquireMaintenanceDbConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	RaisePgResult(conn.Exec(ctx, "BEGIN ISOLATION LEVEL READ COMMITTED"))
	for _, statement := range statements {
		RaisePgResult(conn.Exec(ctx, statement))
	}
	return &testTableSwapOpenTx{conn: conn}
}

func (self *testTableSwapOpenTx) end(ctx context.Context, statement string) {
	RaisePgResult(self.conn.Exec(ctx, statement))
	self.conn.Release()
}

// The rows of the queue whose queue_key is key.
func testTableSwapKeyCount(t testing.TB, ctx context.Context, table string, key string) int64 {
	t.Helper()
	return testContractPartitionScalar[int64](t, ctx, fmt.Sprintf(`SELECT count(*) FROM public.%s WHERE queue_key = $1`, quoteIdentifier(table)), key)
}

var testTableSwapCaughtUpPattern = regexp.MustCompile(`caught up ([0-9]+) captured change`)

var testTableSwapAppliedUnderLockPattern = regexp.MustCompile(`([0-9]+) captured change\(s\) applied under the lock`)

func testTableSwapLoggedCount(t testing.TB, lines []string, pattern *regexp.Regexp) (count int64) {
	t.Helper()
	for _, line := range lines {
		if match := pattern.FindStringSubmatch(line); match != nil {
			fmt.Sscan(match[1], &count)
			return
		}
	}
	t.Fatalf("no line matches %s", pattern)
	return
}

// One swap (forward or rollback) with writes of every kind around its copy:
// two transactions open across the copy's snapshot, one committed after the
// copy and one rolled back; a writer committing writes of every kind from the
// capture until the lock; and one write of every kind between the last
// catch-up and the lock, which the locked transaction applies. Returns the
// swap's log.
func testTableSwapUnderWrites(t testing.TB, ctx context.Context, options TableSwapOptions, prefix string) (lines []string, writes int) {
	t.Helper()
	ids := testTableSwapItemIds(t, ctx)
	if len(ids) < 200 {
		t.Fatal("too few rows to write on", len(ids))
	}
	straddleIds, writerIds := ids[:10], ids[100:]
	var committed, aborted *testTableSwapOpenTx
	var writer *testTableSwapWriter
	lines = []string{}
	options.Logf = func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		lines = append(lines, line)
		t.Logf("%s", line)
	}
	options.Hooks = &TableSwapHooks{
		AfterCapture: func(ctx context.Context) {
			committed = testTableSwapBegin(t, ctx,
				fmt.Sprintf(`INSERT INTO test_table_swap_queue (item_id, queue_key, payload, run_at, release_time) VALUES (gen_random_uuid(), '%s-straddle-insert', 'straddled', now(), now())`, prefix),
				fmt.Sprintf(`UPDATE test_table_swap_queue SET payload = payload || ' straddled' WHERE item_id = '%s'`, straddleIds[0]),
				fmt.Sprintf(`DELETE FROM test_table_swap_queue WHERE item_id = '%s'`, straddleIds[1]),
				fmt.Sprintf(`UPDATE test_table_swap_queue SET item_id = gen_random_uuid() WHERE item_id = '%s'`, straddleIds[2]),
				fmt.Sprintf(`UPDATE test_table_swap_queue SET queue_key = '%s-straddle-moved' WHERE item_id = '%s'`, prefix, straddleIds[3]),
			)
			aborted = testTableSwapBegin(t, ctx,
				fmt.Sprintf(`INSERT INTO test_table_swap_queue (item_id, queue_key, payload, run_at, release_time) VALUES (gen_random_uuid(), '%s-straddle-aborted', 'aborted', now(), now())`, prefix),
				fmt.Sprintf(`DELETE FROM test_table_swap_queue WHERE item_id = '%s'`, straddleIds[4]),
				fmt.Sprintf(`UPDATE test_table_swap_queue SET payload = payload || ' aborted' WHERE item_id = '%s'`, straddleIds[5]),
			)
			writer = testTableSwapStartWriter(ctx, writerIds, prefix)
		},
		AfterPrecopy: func(ctx context.Context) {
			committed.end(ctx, "COMMIT")
			aborted.end(ctx, "ROLLBACK")
			// writes the copy cannot hold, which only the capture brings over
			writer.await(t, 24)
		},
		BeforeLock: func(ctx context.Context, attempt int) {
			if attempt != 1 {
				return
			}
			writes = writer.finish(t)
			for n := range 8 {
				testTableSwapWriteKind(ctx, 1000+n, writerIds, prefix+"-last")
			}
		},
	}
	if err := testTableSwap(t, ctx, options); err != nil {
		t.Fatal(err)
	}
	if writer != nil {
		writer.finish(t)
	}
	return
}

// Every write made while the swap copies outside its lock is in the
// swapped-in table, and only those that committed: the new table holds
// exactly the rows of the old relation, which every write reached. Then the
// same for the rollback, back into the old relation.
func TestTableSwapCapturesEveryWriteAroundItsCopy(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPreflight.reset(map[int]error{})
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 3000)
		oid := testTableSwapOid(t, ctx, testTableSwapTable)
		ids := testTableSwapItemIds(t, ctx)
		before := testTableSwapRows(t, ctx, testTableSwapTable)

		lines, writes := testTableSwapUnderWrites(t, ctx, TableSwapOptions{}, "forward")
		if writes < 20 {
			t.Fatalf("the writer committed only %d writes while the swap copied", writes)
		}
		oldTables := testTableSwapOldTables(t, ctx)
		if len(oldTables) != 1 || testTableSwapOid(t, ctx, oldTables[0]) != oid {
			t.Fatal("the old relation is not kept", oldTables)
		}
		after := testTableSwapRows(t, ctx, testTableSwapTable)
		old := testTableSwapRows(t, ctx, oldTables[0])
		if !maps.Equal(after, old) {
			missing, extra, changed := 0, 0, 0
			for id, row := range old {
				if other, ok := after[id]; !ok {
					missing += 1
				} else if other != row {
					changed += 1
				}
			}
			for id := range after {
				if _, ok := old[id]; !ok {
					extra += 1
				}
			}
			t.Fatalf("the swapped-in table differs from the old relation: %d rows missing, %d extra, %d changed", missing, extra, changed)
		}
		if maps.Equal(after, before) {
			t.Fatal("no write reached the table")
		}
		// the straddling transactions: the committed one's writes, not the
		// aborted one's
		if testTableSwapKeyCount(t, ctx, testTableSwapTable, "forward-straddle-insert") != 1 || testTableSwapKeyCount(t, ctx, testTableSwapTable, "forward-straddle-moved") != 1 {
			t.Fatal("the committed straddling insert or key move is missing")
		}
		if !strings.Contains(after[ids[0]], "straddled") {
			t.Fatal("the committed straddling update is missing")
		}
		if _, ok := after[ids[1]]; ok {
			t.Fatal("the committed straddling delete is missing")
		}
		if _, ok := after[ids[2]]; ok {
			t.Fatal("the committed straddling change of the primary key is missing")
		}
		if testTableSwapKeyCount(t, ctx, testTableSwapTable, "forward-straddle-aborted") != 0 {
			t.Fatal("the aborted straddling insert is in the table")
		}
		if _, ok := after[ids[4]]; !ok || after[ids[5]] != before[ids[5]] {
			t.Fatal("the aborted straddling delete or update reached the table")
		}
		if caughtUp := testTableSwapLoggedCount(t, lines, testTableSwapCaughtUpPattern); caughtUp == 0 {
			t.Fatal("no captured change was caught up before the lock")
		}
		if applied := testTableSwapLoggedCount(t, lines, testTableSwapAppliedUnderLockPattern); applied < 8 {
			t.Fatalf("the locked transaction applied %d captured changes, fewer than the 8 writes made after the last catch-up", applied)
		}
		if left := testTableSwapCaptureLeft(t, ctx); left != "" {
			t.Fatal("the swap left its change capture:", left)
		}

		// the rollback, with the same writes around its copy, brings the old
		// relation back holding exactly the rows of the table it replaces
		swappedOid := testTableSwapOid(t, ctx, testTableSwapTable)
		testTableSwapUnderWrites(t, ctx, TableSwapOptions{Rollback: true}, "rollback")
		if testTableSwapOid(t, ctx, testTableSwapTable) != oid {
			t.Fatal("the rollback did not bring the old relation back")
		}
		oldTables = testTableSwapOldTables(t, ctx)
		if len(oldTables) != 1 || testTableSwapOid(t, ctx, oldTables[0]) != swappedOid {
			t.Fatal("the swapped-in table is not kept", oldTables)
		}
		if restored, replaced := testTableSwapRows(t, ctx, testTableSwapTable), testTableSwapRows(t, ctx, oldTables[0]); !maps.Equal(restored, replaced) {
			t.Fatalf("the restored relation differs from the table it replaces: %d rows against %d", len(restored), len(replaced))
		}
		if testTableSwapKeyCount(t, ctx, testTableSwapTable, "rollback-straddle-insert") != 1 || testTableSwapKeyCount(t, ctx, testTableSwapTable, "rollback-straddle-aborted") != 0 {
			t.Fatal("the rollback's straddling writes")
		}
		if left := testTableSwapCaptureLeft(t, ctx); left != "" {
			t.Fatal("the rollback left its change capture:", left)
		}
	})
}

// More changes captured since the last catch-up than the swap applies under
// its lock make the locked script refuse before it changes anything; the lock
// is released at once, the swap catches up and its next attempt swaps.
func TestTableSwapAppliesAtMostItsBoundUnderTheLock(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPreflight.reset(map[int]error{})
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 500)
		settings := testTableSwapSettings()
		settings.MaxLockedDeltaRows = 5
		attempts := []int{}
		lines := []string{}
		err := testTableSwap(t, ctx, TableSwapOptions{
			Settings: settings,
			Logf: func(format string, args ...any) {
				line := fmt.Sprintf(format, args...)
				lines = append(lines, line)
				t.Logf("%s", line)
			},
			Hooks: &TableSwapHooks{
				BeforeLock: func(ctx context.Context, attempt int) {
					attempts = append(attempts, attempt)
					if attempt == 1 {
						testTableSwapWrite(ctx, `INSERT INTO test_table_swap_queue (item_id, queue_key, run_at, release_time) SELECT gen_random_uuid(), 'bound-' || g, now(), now() FROM generate_series(1, 20) g`)
					}
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(attempts, []int{1, 2}) {
			t.Fatalf("attempts %v, not a refused first and a second", attempts)
		}
		if !slices.ContainsFunc(lines, func(line string) bool {
			return strings.HasPrefix(line, "lock busy (attempt 1/") && strings.Contains(line, "more than the swap applies under its lock (5)")
		}) {
			t.Fatal("the first attempt did not refuse its 20 captured changes")
		}
		oldTables := testTableSwapOldTables(t, ctx)
		if len(oldTables) != 1 || !maps.Equal(testTableSwapRows(t, ctx, testTableSwapTable), testTableSwapRows(t, ctx, oldTables[0])) {
			t.Fatal("the swapped-in table differs from the old relation")
		}
		if testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM test_table_swap_queue WHERE queue_key LIKE 'bound-%'`) != 20 {
			t.Fatal("the writes refused under the first lock are not in the table")
		}
	})
}

// transaction_timeout cuts the locked transaction off LockTimeout +
// MaxLockHold after its request for the table's lock, whatever holds it: the
// session is ended, nothing is swapped, and the capture is removed.
func TestTableSwapCutsItsLockOff(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPreflight.reset(map[int]error{})
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 200)
		rows := testTableSwapRows(t, ctx, testTableSwapTable)
		oid := testTableSwapOid(t, ctx, testTableSwapTable)
		settings := testTableSwapSettings()
		settings.MaxLockHold = 300 * time.Millisecond
		var held time.Duration
		err := testTableSwap(t, ctx, TableSwapOptions{
			Settings: settings,
			afterCopy: func(ctx context.Context, conn PgConn) {
				start := time.Now()
				defer func() { held = time.Since(start) }()
				RaisePgResult(conn.Exec(ctx, `SELECT pg_sleep(10)`))
			},
		})
		if err == nil || !strings.Contains(err.Error(), "transaction timeout") {
			t.Fatal("the locked transaction was not cut off", err)
		}
		if 5*time.Second < held {
			t.Fatalf("the locked transaction ran on for %s", held)
		}
		if testTableSwapOid(t, ctx, testTableSwapTable) != oid || 0 < len(testTableSwapOldTables(t, ctx)) || testTableSwapOid(t, ctx, testTableSwapTable+"_next") != 0 {
			t.Fatal("the cut-off swap changed the table")
		}
		if !maps.Equal(testTableSwapRows(t, ctx, testTableSwapTable), rows) {
			t.Fatal("the cut-off swap changed rows")
		}
		if left := testTableSwapCaptureLeft(t, ctx); left != "" {
			t.Fatal("the cut-off swap left its change capture:", left)
		}
	})
}

// The cut-off holds where a transaction_timeout default (the application
// roles' 15 minutes, db_transaction_timeout.go) armed a timer when the
// transaction began: a positive value set while a timer runs leaves that timer
// alone, so the locked script disarms it first. The script's own cut-off
// statements, run after a session default of 15 minutes, end the session
// within the cut-off.
func TestTableSwapCutOffOutrunsADefaultTransactionTimeout(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testTableSwapSettings()
		settings.MaxLockHold = 300 * time.Millisecond
		swap := &tableSwap{profile: tableSwapProfile(testTableSwapTable), settings: settings, logf: t.Logf}
		source := testTableSwapIndexedShape()
		beforeHook, _ := swap.lockedSql(tableSwapLockedScript{
			source:  source,
			capture: newTableSwapCapture(testTableSwapTable, source.primaryKey),
			target:  testTableSwapTable + "_next",
			stamp:   "20261118120000",
		})
		cutOff := slices.DeleteFunc(slices.Clone(beforeHook), func(statement string) bool {
			return !strings.HasPrefix(statement, "SET LOCAL transaction_timeout")
		})
		if len(cutOff) != 2 {
			t.Fatal("the cut-off statements", cutOff)
		}
		conn, err := AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer discardPgConnection(ctx, conn)
		RaisePgResult(conn.Exec(ctx, `SET transaction_timeout = '15min'`))
		RaisePgResult(conn.Exec(ctx, "BEGIN"))
		for _, statement := range cutOff {
			RaisePgResult(conn.Exec(ctx, statement))
		}
		start := time.Now()
		_, err = conn.Exec(ctx, `SELECT pg_sleep(5)`)
		if err == nil || !strings.Contains(err.Error(), "transaction timeout") {
			t.Fatal("the cut-off did not end the transaction", err)
		}
		if elapsed := time.Since(start); 3*time.Second < elapsed {
			t.Fatalf("the cut-off ended the transaction after %s, not within %s", elapsed, settings.LockTimeout+settings.MaxLockHold)
		}
	})
}

// A swap whose reply is lost after its commit is found committed by the
// catalog: it reports the swap, drops the capture's log and function, and
// removes nothing of the new table.
func TestTableSwapFindsItsCommitWhenItsReplyIsLost(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPreflight.reset(map[int]error{})
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 300)
		rows := testTableSwapRows(t, ctx, testTableSwapTable)
		oid := testTableSwapOid(t, ctx, testTableSwapTable)
		lines := []string{}
		err := testTableSwap(t, ctx, TableSwapOptions{
			Logf: func(format string, args ...any) {
				line := fmt.Sprintf(format, args...)
				lines = append(lines, line)
				t.Logf("%s", line)
			},
			Hooks: &TableSwapHooks{AfterLocked: func() error { return errors.New("synthetic lost reply") }},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, "its reply was lost: synthetic lost reply") }) {
			t.Fatal("the lost reply was not reported")
		}
		if swapped := testTableSwapOid(t, ctx, testTableSwapTable); swapped == oid || len(testTableSwapOldTables(t, ctx)) != 1 {
			t.Fatal("the table was not swapped")
		}
		if !maps.Equal(testTableSwapRows(t, ctx, testTableSwapTable), rows) {
			t.Fatal("the swapped-in table lost rows")
		}
		if left := testTableSwapCaptureLeft(t, ctx); left != "" {
			t.Fatal("the swap left its change capture:", left)
		}
	})
}

// Writes that went uncaptured refuse the swap. A capture trigger disabled
// after the copy, with a write made then, is found by the verification before
// the lock: the twin differs from the table outside the captured keys. A
// capture trigger disabled or dropped after the verification is found under
// the lock. Either way nothing is swapped and the capture goes.
func TestTableSwapRefusesAChangeCaptureNoLongerWhole(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPreflight.reset(map[int]error{})
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 200)
		oid := testTableSwapOid(t, ctx, testTableSwapTable)
		uncaptured := func(change string) func(context.Context) {
			return func(ctx context.Context) {
				testTableSwapWrite(ctx, change)
				testTableSwapWrite(ctx, `UPDATE test_table_swap_queue SET payload = payload || ' uncaptured' WHERE attempt_count = 1`)
			}
		}
		for _, test := range []struct {
			hooks   *TableSwapHooks
			refusal string
		}{
			{
				hooks:   &TableSwapHooks{AfterPrecopy: uncaptured(`ALTER TABLE test_table_swap_queue DISABLE TRIGGER table_swap_capture`)},
				refusal: "outside the 0 captured change(s)",
			},
			{
				hooks: &TableSwapHooks{BeforeLock: func(ctx context.Context, attempt int) {
					testTableSwapWrite(ctx, `ALTER TABLE test_table_swap_queue DISABLE TRIGGER table_swap_capture`)
				}},
				refusal: "is not whole",
			},
			{
				hooks: &TableSwapHooks{BeforeLock: func(ctx context.Context, attempt int) {
					testTableSwapWrite(ctx, `DROP TRIGGER table_swap_capture_truncate ON test_table_swap_queue`)
				}},
				refusal: "is not whole",
			},
		} {
			settings := testTableSwapSettings()
			settings.LockAttempts = 1
			rows := testTableSwapRows(t, ctx, testTableSwapTable)
			err := testTableSwap(t, ctx, TableSwapOptions{Settings: settings, Hooks: test.hooks})
			if err == nil || !strings.Contains(err.Error(), test.refusal) {
				t.Fatal(test.refusal, "did not refuse the swap:", err)
			}
			if testTableSwapOid(t, ctx, testTableSwapTable) != oid || 0 < len(testTableSwapOldTables(t, ctx)) || testTableSwapOid(t, ctx, testTableSwapTable+"_next") != 0 {
				t.Fatal(test.refusal, "changed the table")
			}
			if left := testTableSwapCaptureLeft(t, ctx); left != "" {
				t.Fatal(test.refusal, "left the change capture:", left)
			}
			if after := testTableSwapRows(t, ctx, testTableSwapTable); len(after) != len(rows) {
				t.Fatal(test.refusal, "lost rows")
			}
		}
	})
}

// A change capture left by a run that died: writes go on and are captured, a
// TRUNCATE is refused, the swap and the rollback refuse (dry run or not) and
// ask for --abort, --abort refuses while a run holds the run lock, its dry run
// changes nothing, and --abort removes the triggers, the function, the log
// and the twin, and nothing of the table. A capture left after a committed
// swap loses only its log and function.
func TestTableSwapAbortRemovesWhatAnInterruptedRunLeft(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPreflight.reset(map[int]error{})
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 300)
		oid := testTableSwapOid(t, ctx, testTableSwapTable)

		// a run that dies after the capture is on and the copy is done
		dead := &tableSwap{profile: tableSwapProfile(testTableSwapTable), settings: testTableSwapSettings(), logf: t.Logf}
		var source *tableSwapShape
		MaintenanceDb(ctx, func(conn PgConn) {
			source = readTableSwapShape(ctx, conn, testTableSwapTable)
		})
		run := &tableSwapRun{
			source:   source,
			capture:  newTableSwapCapture(testTableSwapTable, source.primaryKey),
			target:   testTableSwapTable + tableSwapNextSuffix,
			kind:     tableSwapForward,
			oldTable: nil,
		}
		if err := dead.createCapture(ctx, run); err != nil {
			t.Fatal(err)
		}
		if err := dead.installCapture(ctx, run); err != nil {
			t.Fatal(err)
		}
		if err := dead.precopy(ctx, run); err != nil {
			t.Fatal(err)
		}
		if testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM pg_trigger WHERE tgrelid = 'test_table_swap_queue'::regclass AND tgname LIKE 'table\_swap\_capture%' AND tgenabled = 'A'`) != 2 {
			t.Fatal("the capture triggers do not fire always")
		}

		// writes go on, and are captured; a TRUNCATE is refused
		testTableSwapWrite(ctx, `INSERT INTO test_table_swap_queue (item_id, run_at, release_time) VALUES (gen_random_uuid(), now(), now())`)
		testTableSwapWrite(ctx, `UPDATE test_table_swap_queue SET payload = payload || ' left' WHERE attempt_count = 2`)
		testTableSwapWrite(ctx, `DELETE FROM test_table_swap_queue WHERE attempt_count = 3`)
		if testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM test_table_swap_queue_next_log`) == 0 {
			t.Fatal("the writes were not captured")
		}
		if err := partitionRegistryRun(func() { testTableSwapWrite(ctx, `TRUNCATE test_table_swap_queue`) }); err == nil || !strings.Contains(err.Error(), "is being swapped") {
			t.Fatal("a TRUNCATE ran under the capture", err)
		}
		rows := testTableSwapRows(t, ctx, testTableSwapTable)

		for _, options := range []TableSwapOptions{{DryRun: true}, {}, {Rollback: true, DryRun: true}} {
			if err := testTableSwap(t, ctx, options); err == nil || !strings.Contains(err.Error(), "left its change capture") || !strings.Contains(err.Error(), "run --abort") {
				t.Fatal("the swap ran beside a left change capture", options, err)
			}
		}

		// a run in progress keeps --abort off
		live := &tableSwap{profile: tableSwapProfile(testTableSwapTable), settings: testTableSwapSettings(), logf: t.Logf}
		release, err := live.acquireRunLock(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := testTableSwap(t, ctx, TableSwapOptions{Abort: true}); err == nil || !strings.Contains(err.Error(), "in progress") {
			t.Fatal("--abort ran beside a live run", err)
		}
		if err := testTableSwap(t, ctx, TableSwapOptions{}); err == nil || !strings.Contains(err.Error(), "in progress") {
			t.Fatal("a second run started beside a live one", err)
		}
		release()

		if err := testTableSwap(t, ctx, TableSwapOptions{Abort: true, DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if testTableSwapCaptureLeft(t, ctx) == "" || testTableSwapOid(t, ctx, testTableSwapTable+"_next") == 0 {
			t.Fatal("the dry run of --abort changed something")
		}
		if err := testTableSwap(t, ctx, TableSwapOptions{Abort: true}); err != nil {
			t.Fatal(err)
		}
		if left := testTableSwapCaptureLeft(t, ctx); left != "" {
			t.Fatal("--abort left", left)
		}
		if testTableSwapOid(t, ctx, testTableSwapTable+"_next") != 0 {
			t.Fatal("--abort left the twin")
		}
		if testTableSwapOid(t, ctx, testTableSwapTable) != oid || !maps.Equal(testTableSwapRows(t, ctx, testTableSwapTable), rows) {
			t.Fatal("--abort changed the table")
		}
		if err := testTableSwap(t, ctx, TableSwapOptions{Abort: true}); err != nil {
			t.Fatal("a second --abort", err)
		}

		// the swap runs again; then a capture left after it committed loses
		// only its log and function
		if err := testTableSwap(t, ctx, TableSwapOptions{}); err != nil {
			t.Fatal(err)
		}
		swappedOid := testTableSwapOid(t, ctx, testTableSwapTable)
		MaintenanceTx(ctx, func(tx PgTx) {
			for _, statement := range newTableSwapCapture(testTableSwapTable, source.primaryKey).createSql(source, swappedOid, tableSwapForward) {
				RaisePgResult(tx.Exec(ctx, statement))
			}
		})
		if err := testTableSwap(t, ctx, TableSwapOptions{Rollback: true, DryRun: true}); err == nil || !strings.Contains(err.Error(), "committed, but its change capture was not removed") {
			t.Fatal("the rollback ran beside a left change capture", err)
		}
		if err := testTableSwap(t, ctx, TableSwapOptions{Abort: true}); err != nil {
			t.Fatal(err)
		}
		if left := testTableSwapCaptureLeft(t, ctx); left != "" {
			t.Fatal("--abort left", left)
		}
		if testTableSwapOid(t, ctx, testTableSwapTable) != swappedOid || len(testTableSwapOldTables(t, ctx)) != 1 {
			t.Fatal("--abort after a committed swap touched a table")
		}
	})
}

var testTableSwapRehearsalLinePattern = regexp.MustCompile(`^rehearsal, rolled back: ([0-9]+) rows copied in (\S+) and indexes built in (\S+) before the lock; the swap holds ACCESS EXCLUSIVE on (\S+) about (\S+) `)

// The rehearsal line says how long the swap holds its lock, as the operator
// scripts parse it (ptswap1116/common.sh rehearsal_seconds), with the most
// captured changes it applies under the lock; the swap refuses an exclusive
// section longer than MaxLockHold, and a dry run changes nothing.
func TestTableSwapRehearsalMeasuresItsExclusiveSection(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPreflight.reset(map[int]error{})
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 2000)
		oid := testTableSwapOid(t, ctx, testTableSwapTable)
		settings := testTableSwapSettings()
		settings.MaxLockedDeltaRows = 300
		lines := []string{}
		logf := func(format string, args ...any) {
			line := fmt.Sprintf(format, args...)
			lines = append(lines, line)
			t.Logf("%s", line)
		}
		if err := testTableSwap(t, ctx, TableSwapOptions{DryRun: true, Settings: settings, Logf: logf}); err != nil {
			t.Fatal(err)
		}
		index := slices.IndexFunc(lines, func(line string) bool { return testTableSwapRehearsalLinePattern.MatchString(line) })
		if index < 0 {
			t.Fatal("no rehearsal line the operator scripts parse", lines)
		}
		match := testTableSwapRehearsalLinePattern.FindStringSubmatch(lines[index])
		exclusive, err := time.ParseDuration(match[5])
		if match[1] != "2000" || match[4] != testTableSwapTable || err != nil || settings.MaxLockHold < exclusive {
			t.Fatal("the rehearsal line", lines[index], err)
		}
		if !strings.Contains(lines[index], "to apply 300 captured change(s)") {
			t.Fatal("the rehearsal did not apply the most changes the lock allows", lines[index])
		}
		if testTableSwapOid(t, ctx, testTableSwapTable) != oid || testTableSwapOid(t, ctx, testTableSwapTable+"_next") != 0 || 0 < len(testTableSwapOldTables(t, ctx)) {
			t.Fatal("the dry run changed the table")
		}
		if left := testTableSwapCaptureLeft(t, ctx); left != "" {
			t.Fatal("the dry run left a change capture:", left)
		}

		tight := testTableSwapSettings()
		tight.MaxLockHold = time.Millisecond
		if err := testTableSwap(t, ctx, TableSwapOptions{Settings: tight}); err == nil || !strings.Contains(err.Error(), "the swap may hold its lock") {
			t.Fatal("the swap ran past its lock bound", err)
		}
		if testTableSwapOid(t, ctx, testTableSwapTable) != oid || testTableSwapOid(t, ctx, testTableSwapTable+"_next") != 0 {
			t.Fatal("the refused swap changed the table")
		}
	})
}
