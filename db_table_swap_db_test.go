// The table swap against PostgreSQL (db_table_swap.go), on a synthetic queue
// that carries everything the swap must keep: a dropped column, defaults, a
// generated column, NOT NULL and CHECK constraints, a primary key, a unique
// key, an expression index, a partial index, an owned sequence, storage
// settings on the table and its TOAST table, a column statistics target and
// options, a grant and comments. The swap keeps every row and every published
// name, its dry run and its rehearsal change nothing, it refuses what it cannot
// copy, re-checks under its lock, changes nothing when interrupted before its
// commit, waits a bounded time for its lock, rolls back into the old relation
// with the writes made since, and drops the old table only after the rollback
// window.
package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const testTableSwapSchemaSql = `
CREATE TABLE test_table_swap_queue (
	item_id uuid NOT NULL,
	dropped_before integer,
	queue_key varchar(200),
	payload text NOT NULL DEFAULT '',
	run_at timestamp NOT NULL,
	release_time timestamp NOT NULL,
	attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
	serial_number bigserial,
	run_at_block bigint GENERATED ALWAYS AS (EXTRACT(epoch FROM run_at)) STORED,
	PRIMARY KEY (item_id),
	UNIQUE (queue_key)
) WITH (fillfactor = 50, autovacuum_vacuum_threshold = 50, autovacuum_vacuum_scale_factor = 0);
ALTER TABLE test_table_swap_queue DROP COLUMN dropped_before;
ALTER TABLE test_table_swap_queue SET (toast.autovacuum_enabled = false);
ALTER TABLE test_table_swap_queue ALTER COLUMN payload SET STATISTICS 500;
ALTER TABLE test_table_swap_queue ALTER COLUMN queue_key SET (n_distinct = -0.5);
CREATE INDEX test_table_swap_queue_poll ON test_table_swap_queue (run_at_block, attempt_count DESC);
CREATE INDEX test_table_swap_queue_key_prefix ON test_table_swap_queue
	(regexp_replace(queue_key::text, '/v[0-9]+', '', 'g'), run_at) WHERE queue_key IS NOT NULL;
GRANT SELECT ON test_table_swap_queue TO PUBLIC;
COMMENT ON TABLE test_table_swap_queue IS 'a synthetic queue';
COMMENT ON COLUMN test_table_swap_queue.payload IS 'the payload';
COMMENT ON INDEX test_table_swap_queue_poll IS 'the poll order'
`

// Fast lock waits and retries, and a rollback window a test can pass.
func testTableSwapSettings() *TableSwapSettings {
	settings := DefaultTableSwapSettings()
	settings.LockTimeout = 200 * time.Millisecond
	settings.LockAttempts = 3
	settings.LockRetryInterval = 10 * time.Millisecond
	settings.MaxTransactionAge = time.Hour
	settings.MaxOpenTransactionAge = time.Hour
	return settings
}

func testTableSwapCreate(t testing.TB, ctx context.Context) {
	t.Helper()
	MaintenanceTx(ctx, func(tx PgTx) {
		for _, statement := range strings.Split(testTableSwapSchemaSql, ";\n") {
			RaisePgResult(tx.Exec(ctx, statement))
		}
	})
}

// Rows of every kind a queue holds: claimed, backed off, keyed and unkeyed,
// far in the future, with a payload large enough to live in TOAST, and with
// NULLs.
func testTableSwapSeed(t testing.TB, ctx context.Context, count int) {
	t.Helper()
	MaintenanceTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO test_table_swap_queue (item_id, queue_key, payload, run_at, release_time, attempt_count)
			SELECT
				gen_random_uuid(),
				CASE WHEN g % 3 = 0 THEN NULL ELSE 'synthetic/v' || (g % 4) || '/key-' || g END,
				CASE WHEN g % 50 = 0 THEN repeat('large payload ' || g, 400) ELSE 'payload ' || g || ' ünïcode' END,
				now() AT TIME ZONE 'UTC' + (g % 7 - 3) * interval '1 hour' + CASE WHEN g % 97 = 0 THEN interval '30 days' ELSE interval '0' END,
				now() AT TIME ZONE 'UTC' + CASE WHEN g % 5 = 0 THEN interval '4 minutes' ELSE interval '-1 minute' END,
				g % 9
			FROM generate_series(1, $1) g
			`,
			count,
		))
	})
}

// Every row as JSON, by item id: every column, generated ones included.
func testTableSwapRows(t testing.TB, ctx context.Context, table string) map[string]string {
	t.Helper()
	rows := map[string]string{}
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, fmt.Sprintf(`SELECT item_id::text, row_to_json(t)::text FROM public.%s t`, quoteIdentifier(table)))
		WithPgResult(result, err, func() {
			for result.Next() {
				var itemId, row string
				Raise(result.Scan(&itemId, &row))
				rows[itemId] = row
			}
		})
	})
	return rows
}

// What a reader, the monitor or db audit sees of the table under its name:
// index and constraint definitions, storage settings, the owner, grants,
// comments and columns.
func testTableSwapCatalog(t testing.TB, ctx context.Context, table string) string {
	t.Helper()
	var catalog string
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(
			ctx,
			`
			SELECT concat_ws(E'\n',
				(SELECT string_agg(pg_get_indexdef(indexrelid), E'\n' ORDER BY indexrelid::regclass::text) FROM pg_index WHERE indrelid = c.oid),
				(SELECT string_agg(conname || ' ' || contype::text || ' ' || pg_get_constraintdef(oid), E'\n' ORDER BY conname) FROM pg_constraint WHERE conrelid = c.oid),
				array_to_string(c.reloptions, ','),
				(SELECT array_to_string(reloptions, ',') FROM pg_class WHERE oid = c.reltoastrelid),
				pg_get_userbyid(c.relowner),
				c.relacl::text,
				obj_description(c.oid, 'pg_class'),
				(SELECT string_agg(attname || ' ' || format_type(atttypid, atttypmod) || ' ' || attnotnull::text || ' ' || attgenerated::text || ' ' ||
					coalesce(attstattarget::text, '-') || ' ' || coalesce(array_to_string(attoptions, ','), '-') || ' ' ||
					coalesce(col_description(c.oid, attnum), '-'), E'\n' ORDER BY attnum)
					FROM pg_attribute WHERE attrelid = c.oid AND attnum > 0 AND NOT attisdropped),
				(SELECT string_agg(obj_description(indexrelid, 'pg_class'), ',') FROM pg_index WHERE indrelid = c.oid)
			)
			FROM pg_class c WHERE c.oid = to_regclass($1)
			`,
			"public."+table,
		).Scan(&catalog))
	})
	return catalog
}

func testTableSwapOid(t testing.TB, ctx context.Context, table string) uint32 {
	t.Helper()
	oid := testContractPartitionScalar[*uint32](t, ctx, `SELECT to_regclass($1)::oid`, "public."+table)
	if oid == nil {
		return 0
	}
	return *oid
}

var testTableSwapOldNamePattern = regexp.MustCompile(`^test_table_swap_queue_old_([0-9]{14})$`)

// The kept old tables' names.
func testTableSwapOldTables(t testing.TB, ctx context.Context) (names []string) {
	t.Helper()
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, `SELECT relname FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relname LIKE 'test\_table\_swap\_queue\_old\_%' AND relkind = 'r' ORDER BY relname`)
		WithPgResult(result, err, func() {
			for result.Next() {
				var name string
				Raise(result.Scan(&name))
				names = append(names, name)
			}
		})
	})
	return
}

func testTableSwapSequenceOwner(t testing.TB, ctx context.Context) string {
	t.Helper()
	return testContractPartitionScalar[string](t, ctx, `
		SELECT d.refobjid::regclass::text FROM pg_depend d
		WHERE d.classid = 'pg_class'::regclass AND d.objid = 'public.test_table_swap_queue_serial_number_seq'::regclass AND d.deptype = 'a'`)
}

// Ages a kept old table by moving its name's stamp back, as if it had been
// swapped age ago: the rollback window reads the stamp.
func testTableSwapAge(t testing.TB, ctx context.Context, oldTable string, age time.Duration) string {
	t.Helper()
	match := testTableSwapOldNamePattern.FindStringSubmatch(oldTable)
	if match == nil {
		t.Fatalf("%s is not a kept old table", oldTable)
	}
	stamp, err := time.ParseInLocation(tableSwapStampLayout, match[1], time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	aged := tableSwapOldName(testTableSwapTable, stamp.Add(-age).Format(tableSwapStampLayout))
	MaintenanceTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE public.%s RENAME TO %s`, quoteIdentifier(oldTable), quoteIdentifier(aged))))
	})
	return aged
}

func testTableSwap(t testing.TB, ctx context.Context, options TableSwapOptions) error {
	t.Helper()
	options.Table = testTableSwapTable
	if options.Logf == nil {
		options.Logf = t.Logf
	}
	if options.Settings == nil {
		options.Settings = testTableSwapSettings()
	}
	return SwapTable(ctx, options)
}

// The swap keeps every row, every column value and every published name; the
// table is a new relation and the old one is kept under a dated name with its
// indexes. The owned sequence moves with the table and keeps counting, the
// dry run before changes nothing, and a second swap is refused while the old
// table is kept.
func TestTableSwapCopiesEveryRowAndKeepsEveryPublishedName(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPreflight.reset(map[int]error{})
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 2000)
		rows := testTableSwapRows(t, ctx, testTableSwapTable)
		catalog := testTableSwapCatalog(t, ctx, testTableSwapTable)
		oid := testTableSwapOid(t, ctx, testTableSwapTable)
		lastSerial := testContractPartitionScalar[int64](t, ctx, `SELECT max(serial_number) FROM test_table_swap_queue`)

		if err := testTableSwap(t, ctx, TableSwapOptions{DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if testTableSwapOid(t, ctx, testTableSwapTable) != oid || 0 < len(testTableSwapOldTables(t, ctx)) || testTableSwapOid(t, ctx, testTableSwapTable+"_next") != 0 {
			t.Fatal("the dry run changed the table")
		}

		if err := testTableSwap(t, ctx, TableSwapOptions{}); err != nil {
			t.Fatal(err)
		}
		if swapped := testTableSwapOid(t, ctx, testTableSwapTable); swapped == oid || swapped == 0 {
			t.Fatal("the table is not a new relation")
		}
		if after := testTableSwapRows(t, ctx, testTableSwapTable); len(after) != len(rows) {
			t.Fatalf("%d rows after the swap of %d", len(after), len(rows))
		} else {
			for itemId, row := range rows {
				if after[itemId] != row {
					t.Fatalf("row %s changed:\n%s\n%s", itemId, row, after[itemId])
				}
			}
		}
		if after := testTableSwapCatalog(t, ctx, testTableSwapTable); after != catalog {
			t.Fatalf("the published catalog changed:\n%s\n---\n%s", catalog, after)
		}
		oldTables := testTableSwapOldTables(t, ctx)
		if len(oldTables) != 1 || testTableSwapOid(t, ctx, oldTables[0]) != oid {
			t.Fatalf("the old relation is not kept under a dated name: %v", oldTables)
		}
		stamp := testTableSwapOldNamePattern.FindStringSubmatch(oldTables[0])[1]
		for _, index := range []string{"test_table_swap_queue_pkey", "test_table_swap_queue_queue_key_key", "test_table_swap_queue_poll", "test_table_swap_queue_key_prefix"} {
			if testContractPartitionScalar[string](t, ctx, `SELECT indrelid::regclass::text FROM pg_index WHERE indexrelid = to_regclass($1)`, "public."+index+"_old_"+stamp) != oldTables[0] {
				t.Fatalf("the old table's index %s is not kept under its dated name", index)
			}
		}
		if table, ok := TableSwapRelation(oldTables[0]); !ok || table != testTableSwapTable || dbMaintenanceShouldReindexTable(oldTables[0]) {
			t.Fatal("the old table is not recognized as the swap's")
		}
		if owner := testTableSwapSequenceOwner(t, ctx); owner != testTableSwapTable {
			t.Fatalf("the sequence is owned by %s", owner)
		}
		var serial int64
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `INSERT INTO test_table_swap_queue (item_id, run_at, release_time) VALUES (gen_random_uuid(), now(), now()) RETURNING serial_number`).Scan(&serial))
		})
		if serial != lastSerial+1 {
			t.Fatalf("the sequence went from %d to %d", lastSerial, serial)
		}
		if testContractPartitionScalar[int64](t, ctx, fmt.Sprintf(`SELECT count(*) FROM public.%s`, oldTables[0])) != int64(len(rows)) {
			t.Fatal("the old table lost rows")
		}

		err := testTableSwap(t, ctx, TableSwapOptions{})
		if err == nil || !strings.Contains(err.Error(), "is done and its old table is kept") {
			t.Fatal("a second swap ran while the old table is kept", err)
		}
	})
}

// Each thing the swap cannot copy faithfully refuses it, dry run or not, and
// leaves the table as it was.
func TestTableSwapRefusesWhatItCannotCopyFaithfully(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPreflight.reset(map[int]error{})
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION test_table_swap_noop() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`))
		})
		for _, test := range []struct {
			setup   []string
			refusal string
		}{
			{setup: []string{`CREATE TRIGGER test_table_swap_trigger BEFORE INSERT ON test_table_swap_queue FOR EACH ROW EXECUTE FUNCTION test_table_swap_noop()`}, refusal: "trigger(s)"},
			{setup: []string{`CREATE TABLE test_table_swap_reference (item_id uuid REFERENCES test_table_swap_queue (item_id))`}, refusal: "foreign key(s)"},
			{setup: []string{`CREATE VIEW test_table_swap_view AS SELECT item_id FROM test_table_swap_queue`}, refusal: "dependent view(s)"},
			{setup: []string{`CREATE FUNCTION test_table_swap_reader() RETURNS bigint LANGUAGE sql BEGIN ATOMIC SELECT count(*) FROM test_table_swap_queue; END`}, refusal: "dependent view(s) or function(s)"},
			{setup: []string{`CREATE TABLE test_table_swap_row_user (item test_table_swap_queue)`}, refusal: "row type"},
			{setup: []string{`ALTER TABLE test_table_swap_queue ADD COLUMN identity_number bigint GENERATED ALWAYS AS IDENTITY`}, refusal: "identity column"},
			{setup: []string{`CREATE STATISTICS test_table_swap_statistics ON queue_key, payload FROM test_table_swap_queue`}, refusal: "extended statistics"},
			{setup: []string{`ALTER TABLE test_table_swap_queue DROP CONSTRAINT test_table_swap_queue_pkey`}, refusal: "no primary key"},
			{setup: []string{`CREATE POLICY test_table_swap_policy ON test_table_swap_queue USING (true)`}, refusal: "row security polic"},
			{setup: []string{`ALTER TABLE test_table_swap_queue ADD CONSTRAINT test_table_swap_queue_deferred UNIQUE (payload) DEFERRABLE`}, refusal: "deferrable"},
			{setup: []string{`GRANT UPDATE (payload) ON test_table_swap_queue TO PUBLIC`}, refusal: "column grants"},
			{setup: []string{`CREATE TABLE test_table_swap_queue_next (item_id uuid)`}, refusal: "the swap builds its twin under that name"},
		} {
			testTableSwapCreate(t, ctx)
			testTableSwapSeed(t, ctx, 20)
			MaintenanceTx(ctx, func(tx PgTx) {
				for _, statement := range test.setup {
					RaisePgResult(tx.Exec(ctx, statement))
				}
			})
			oid := testTableSwapOid(t, ctx, testTableSwapTable)
			for _, dryRun := range []bool{true, false} {
				err := testTableSwap(t, ctx, TableSwapOptions{DryRun: dryRun})
				if err == nil || !strings.Contains(err.Error(), test.refusal) {
					t.Fatalf("%v (dry run %t): %v", test.setup, dryRun, err)
				}
			}
			if testTableSwapOid(t, ctx, testTableSwapTable) != oid || 0 < len(testTableSwapOldTables(t, ctx)) {
				t.Fatalf("%v: a refused swap changed the table", test.setup)
			}
			MaintenanceTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `DROP TABLE IF EXISTS test_table_swap_queue, test_table_swap_queue_next, test_table_swap_reference, test_table_swap_row_user CASCADE`))
				RaisePgResult(tx.Exec(ctx, `DROP FUNCTION IF EXISTS test_table_swap_reader()`))
			})
		}

		// an open transaction older than the bound, anywhere in the database
		testTableSwapCreate(t, ctx)
		conn, err := AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		RaisePgResult(conn.Exec(ctx, `BEGIN`))
		RaisePgResult(conn.Exec(ctx, `SELECT 1`))
		settings := testTableSwapSettings()
		settings.MaxOpenTransactionAge = time.Millisecond
		time.Sleep(10 * time.Millisecond)
		if err := testTableSwap(t, ctx, TableSwapOptions{Settings: settings}); err == nil || !strings.Contains(err.Error(), "a transaction older than") {
			t.Fatal("the swap ran beside an old open transaction", err)
		}
		RaisePgResult(conn.Exec(ctx, `ROLLBACK`))
	})
}

// The profile's locked check runs inside the swap script under the table's
// lock, where the state the preflight read can no longer change, and its
// refusal there rolls everything back. A dry run still rehearses while the
// preflight refuses, so the lock's length is known ahead.
func TestTableSwapChecksItsStateUnderTheLock(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 50)
		oid := testTableSwapOid(t, ctx, testTableSwapTable)

		// the preflight admits; the state changed before the lock
		testTableSwapPreflight.reset(map[int]error{})
		defer testTableSwapPreflight.reset(map[int]error{})
		testTableSwapPreflight.refuseLocked("synthetic live owner appeared")
		err := testTableSwap(t, ctx, TableSwapOptions{})
		if err == nil || !strings.Contains(err.Error(), "synthetic live owner appeared") {
			t.Fatal("the swap did not refuse under its lock", err)
		}
		if testTableSwapPreflight.callCount() != 1 {
			t.Fatalf("the preflight ran %d times", testTableSwapPreflight.callCount())
		}
		if testTableSwapOid(t, ctx, testTableSwapTable) != oid || 0 < len(testTableSwapOldTables(t, ctx)) || testTableSwapOid(t, ctx, testTableSwapTable+"_next") != 0 {
			t.Fatal("the refused swap changed the table")
		}

		// the table changed after the rehearsal read its fingerprint: a column
		// added between them refuses the swap under its lock
		testTableSwapPreflight.reset(map[int]error{})
		changed := testTableSwapSettings()
		changed.LockAttempts = 1
		swap := &tableSwap{profile: tableSwapProfile(testTableSwapTable), settings: changed, logf: t.Logf}
		var source *tableSwapShape
		MaintenanceDb(ctx, func(conn PgConn) {
			source = readTableSwapShape(ctx, conn, testTableSwapTable)
		})
		_, fingerprint, err := swap.rehearse(ctx, source)
		if err != nil {
			t.Fatal(err)
		}
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `ALTER TABLE test_table_swap_queue ADD COLUMN added_later integer`))
		})
		beforeHook, afterHook := swap.swapScript(source, testTableSwapTable+"_next", "20261118120000", fingerprint, nil)
		if _, err := swap.runScript(ctx, beforeHook, afterHook); err == nil || !strings.Contains(err.Error(), "changed since the rehearsal") {
			t.Fatal("the swap ran on a table that changed since its rehearsal", err)
		}
		if testTableSwapOid(t, ctx, testTableSwapTable) != oid || 0 < len(testTableSwapOldTables(t, ctx)) {
			t.Fatal("the refused swap changed the table")
		}
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `ALTER TABLE test_table_swap_queue DROP COLUMN added_later`))
		})

		// a dry run still rehearses the copy while the state refuses (a
		// running taskworker, for pending_task), so the lock's length is known
		refusal := errors.New("synthetic preflight refusal")
		testTableSwapPreflight.reset(map[int]error{0: refusal})
		lines := []string{}
		logf := func(format string, args ...any) {
			lines = append(lines, fmt.Sprintf(format, args...))
		}
		if err := testTableSwap(t, ctx, TableSwapOptions{DryRun: true, Logf: logf}); err == nil || !strings.Contains(err.Error(), refusal.Error()) {
			t.Fatal("the dry run did not report the refusal", err)
		}
		if !slices.ContainsFunc(lines, func(line string) bool { return strings.HasPrefix(line, "rehearsal, rolled back: 50 rows copied") }) {
			t.Fatal("the refused dry run did not rehearse", lines)
		}
		if !slices.ContainsFunc(lines, func(line string) bool { return strings.HasPrefix(line, "would send this script in one message") }) {
			t.Fatal("the dry run did not show its script", lines)
		}
		if testTableSwapOid(t, ctx, testTableSwapTable) != oid || testTableSwapOid(t, ctx, testTableSwapTable+"_next") != 0 {
			t.Fatal("the rehearsal changed the table")
		}
	})
}

// A swap interrupted after its copy, by an error or by its connection dying,
// changes nothing: no twin, no old table, the same relation and rows. The
// next run swaps.
func TestTableSwapInterruptedBeforeItsCommitChangesNothing(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPreflight.reset(map[int]error{})
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 300)
		rows := testTableSwapRows(t, ctx, testTableSwapTable)
		oid := testTableSwapOid(t, ctx, testTableSwapTable)

		for _, interrupt := range []func(context.Context, PgConn){
			func(context.Context, PgConn) { panic(errors.New("synthetic interruption")) },
			func(ctx context.Context, conn PgConn) {
				// the swap's own session dies, as in a crash
				_, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pg_backend_pid())`)
				Raise(err)
			},
		} {
			if err := testTableSwap(t, ctx, TableSwapOptions{afterCopy: interrupt}); err == nil {
				t.Fatal("the interrupted swap reported success")
			}
			if testTableSwapOid(t, ctx, testTableSwapTable) != oid || 0 < len(testTableSwapOldTables(t, ctx)) || testTableSwapOid(t, ctx, testTableSwapTable+"_next") != 0 {
				t.Fatal("the interrupted swap changed the table")
			}
			if after := testTableSwapRows(t, ctx, testTableSwapTable); len(after) != len(rows) {
				t.Fatal("the interrupted swap lost rows")
			}
		}
		if err := testTableSwap(t, ctx, TableSwapOptions{}); err != nil {
			t.Fatal(err)
		}
		if after := testTableSwapRows(t, ctx, testTableSwapTable); len(after) != len(rows) || testTableSwapOid(t, ctx, testTableSwapTable) == oid {
			t.Fatal("the next run did not swap")
		}
	})
}

// Behind an open writer the swap waits at most its lock timeout per attempt
// and gives up after its attempts, so writers never queue behind it longer
// than that; with the writer gone it swaps, the writer's row included. A
// transaction older than MaxTransactionAge holding a lock keeps the swap from
// even requesting its lock.
func TestTableSwapWaitsABoundedTimeForItsLock(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPreflight.reset(map[int]error{})
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 100)

		writer, err := AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Release()
		RaisePgResult(writer.Exec(ctx, `BEGIN`))
		var writtenId string
		Raise(writer.QueryRow(ctx, `INSERT INTO test_table_swap_queue (item_id, run_at, release_time) VALUES (gen_random_uuid(), now(), now()) RETURNING item_id::text`).Scan(&writtenId))

		settings := testTableSwapSettings()
		settings.LockAttempts = 2
		start := time.Now()
		err = testTableSwap(t, ctx, TableSwapOptions{Settings: settings})
		if err == nil || !strings.Contains(err.Error(), "lock timeout") {
			t.Fatal("the swap did not give up behind the writer", err)
		}
		// two lock waits of 200ms and the copy's rehearsal, far below a minute
		if elapsed := time.Since(start); time.Minute < elapsed {
			t.Fatalf("the swap waited %s", elapsed)
		}
		if 0 < len(testTableSwapOldTables(t, ctx)) {
			t.Fatal("the swap that gave up changed the table")
		}

		busy := testTableSwapSettings()
		busy.MaxTransactionAge = time.Millisecond
		busy.LockAttempts = 2
		time.Sleep(10 * time.Millisecond)
		if err := testTableSwap(t, ctx, TableSwapOptions{Settings: busy}); err == nil || !strings.Contains(err.Error(), "busy") || !strings.Contains(err.Error(), "older than") {
			t.Fatal("the swap requested its lock behind an old transaction", err)
		}

		RaisePgResult(writer.Exec(ctx, `COMMIT`))
		if err := testTableSwap(t, ctx, TableSwapOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, ok := testTableSwapRows(t, ctx, testTableSwapTable)[writtenId]; !ok {
			t.Fatal("the writer's row is not in the swapped table")
		}
	})
}

// The rollback brings the old relation back as the table, with every row
// written since the swap and every published name, and keeps the table that
// was swapped in under a dated name. The sequence follows. The old tables
// drop only past the rollback window, and only by the drop step; a dry run
// of either changes nothing.
func TestTableSwapRollbackRestoresTheOldRelationWithLaterWrites(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPreflight.reset(map[int]error{})
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 500)
		catalog := testTableSwapCatalog(t, ctx, testTableSwapTable)
		oid := testTableSwapOid(t, ctx, testTableSwapTable)
		if err := testTableSwap(t, ctx, TableSwapOptions{}); err != nil {
			t.Fatal(err)
		}
		swappedOid := testTableSwapOid(t, ctx, testTableSwapTable)

		// writes after the swap: inserts, updates and deletes
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO test_table_swap_queue (item_id, queue_key, run_at, release_time) SELECT gen_random_uuid(), 'after-' || g, now(), now() FROM generate_series(1, 40) g`))
			RaisePgResult(tx.Exec(ctx, `UPDATE test_table_swap_queue SET attempt_count = attempt_count + 1, payload = payload || ' updated' WHERE attempt_count = 2`))
			RaisePgResult(tx.Exec(ctx, `DELETE FROM test_table_swap_queue WHERE attempt_count = 4`))
		})
		rows := testTableSwapRows(t, ctx, testTableSwapTable)
		lastSerial := testContractPartitionScalar[int64](t, ctx, `SELECT max(serial_number) FROM test_table_swap_queue`)

		if err := testTableSwap(t, ctx, TableSwapOptions{Rollback: true, DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if testTableSwapOid(t, ctx, testTableSwapTable) != swappedOid {
			t.Fatal("the rollback's dry run changed the table")
		}
		if err := testTableSwap(t, ctx, TableSwapOptions{Rollback: true}); err != nil {
			t.Fatal(err)
		}
		if testTableSwapOid(t, ctx, testTableSwapTable) != oid {
			t.Fatal("the rollback did not bring the old relation back")
		}
		if after := testTableSwapRows(t, ctx, testTableSwapTable); len(after) != len(rows) {
			t.Fatalf("%d rows after the rollback of %d", len(after), len(rows))
		} else {
			for itemId, row := range rows {
				if after[itemId] != row {
					t.Fatalf("row %s changed in the rollback", itemId)
				}
			}
		}
		if after := testTableSwapCatalog(t, ctx, testTableSwapTable); after != catalog {
			t.Fatalf("the rollback changed the published catalog:\n%s\n---\n%s", catalog, after)
		}
		oldTables := testTableSwapOldTables(t, ctx)
		if len(oldTables) != 1 || testTableSwapOid(t, ctx, oldTables[0]) != swappedOid {
			t.Fatalf("the swapped-in table is not kept: %v", oldTables)
		}
		if owner := testTableSwapSequenceOwner(t, ctx); owner != testTableSwapTable {
			t.Fatalf("the sequence is owned by %s", owner)
		}
		var serial int64
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `INSERT INTO test_table_swap_queue (item_id, run_at, release_time) VALUES (gen_random_uuid(), now(), now()) RETURNING serial_number`).Scan(&serial))
		})
		if serial != lastSerial+1 {
			t.Fatalf("the sequence went from %d to %d", lastSerial, serial)
		}

		// inside the default day's rollback window the drop refuses, dry run
		// or not; past it, the dry run lists and the drop drops
		window := testTableSwapSettings()
		window.DropOldMinAge = DefaultTableSwapSettings().DropOldMinAge
		for _, dryRun := range []bool{true, false} {
			if err := testTableSwap(t, ctx, TableSwapOptions{DropOld: true, DryRun: dryRun, Settings: window}); err == nil || !strings.Contains(err.Error(), "inside the rollback window") {
				t.Fatal("the drop ran inside the rollback window", err)
			}
		}
		aged := testTableSwapAge(t, ctx, oldTables[0], window.DropOldMinAge+time.Hour)
		if err := testTableSwap(t, ctx, TableSwapOptions{DropOld: true, DryRun: true, Settings: window}); err != nil {
			t.Fatal(err)
		}
		if names := testTableSwapOldTables(t, ctx); len(names) != 1 || names[0] != aged {
			t.Fatal("the drop's dry run dropped", names)
		}
		if err := testTableSwap(t, ctx, TableSwapOptions{DropOld: true, Settings: window}); err != nil {
			t.Fatal(err)
		}
		if 0 < len(testTableSwapOldTables(t, ctx)) || testTableSwapOid(t, ctx, testTableSwapTable) != oid {
			t.Fatal("the drop left the old table, or touched the table")
		}
		if err := testTableSwap(t, ctx, TableSwapOptions{DropOld: true, Settings: window}); err != nil {
			t.Fatal("a drop with nothing kept", err)
		}

		// with no old table kept, the table swaps again
		if err := testTableSwap(t, ctx, TableSwapOptions{}); err != nil {
			t.Fatal(err)
		}
	})
}

// The drop never takes a sequence the new table still uses: an old table that
// still owns it refuses the drop and stays.
func TestTableSwapDropOldRefusesASequenceTheNewTableUses(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testTableSwapPreflight.reset(map[int]error{})
		testTableSwapCreate(t, ctx)
		testTableSwapSeed(t, ctx, 30)
		if err := testTableSwap(t, ctx, TableSwapOptions{}); err != nil {
			t.Fatal(err)
		}
		oldTables := testTableSwapOldTables(t, ctx)
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`ALTER SEQUENCE test_table_swap_queue_serial_number_seq OWNED BY public.%s.serial_number`, oldTables[0])))
		})
		aged := testTableSwapAge(t, ctx, oldTables[0], 2*DefaultTableSwapSettings().DropOldMinAge)
		if err := testTableSwap(t, ctx, TableSwapOptions{DropOld: true}); err == nil {
			t.Fatal("the drop took the sequence the new table uses")
		}
		if names := testTableSwapOldTables(t, ctx); !slices.Equal(names, []string{aged}) {
			t.Fatal("the refused drop changed the old table", names)
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `INSERT INTO test_table_swap_queue (item_id, run_at, release_time) VALUES (gen_random_uuid(), now(), now())`))
		})
	})
}
