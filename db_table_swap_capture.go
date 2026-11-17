// The change capture of the table swap (db_table_swap.go). The swap copies the
// table into its target (the twin, or the kept old table on rollback) and
// builds the target's indexes outside its lock, while writers keep writing. A
// trigger on the table records the key of every row an insert, update or
// delete touches into a log table, in the writer's own transaction. The swap
// applies the captured keys to the target by key: it deletes the target's rows
// of those keys and copies the table's current rows of them. The order and the
// number of the changes to a key do not matter, and a key captured twice is
// applied once. Catch-up passes apply the log outside the lock; under the lock
// only the keys captured since the last pass remain.
//
// Why the target holds exactly the table's rows once the log is applied under
// the lock:
//   - CREATE TRIGGER takes SHARE ROW EXCLUSIVE on the table, which waits until
//     every transaction that has written the table ends. So every write that
//     did not fire the trigger committed or aborted before the trigger
//     committed. Every later write fires it: a statement opens the table after
//     taking its lock, which reads the trigger's invalidation.
//   - The copy's snapshot is taken after the trigger committed, so it holds
//     every write that did not fire the trigger. A later write logs its keys
//     in its own transaction: the log row commits exactly when the write
//     does, and aborts with it.
//   - A catch-up pass reads the log, the table and the target at one snapshot
//     (repeatable read). For each captured key, the row it copies is the one
//     that snapshot sees, and it deletes only the log rows that snapshot saw.
//     A write that commits later leaves its own log row.
//   - So at any snapshot, a key without a log row has the same row (or none)
//     in the table and in the target. The verification checks exactly that,
//     over every such key, before the lock.
//   - Under ACCESS EXCLUSIVE no write is in flight. The locked script applies
//     the remaining log, after which every key is equal.
//   - Deleting every captured key from the target before copying the table's
//     rows of them leaves the target equal to the table at one snapshot, so
//     the target's unique indexes (pending_task's run_once_key) never refuse
//     a copied row.
//
// TRUNCATE fires no row trigger, so a statement trigger refuses it while the
// capture is on. The triggers fire in session_replication_role replica too
// (ENABLE ALWAYS).
//
// What the capture creates is named after the table: the log
// <table>_next_log, the function <table>_next_capture() and the triggers
// table_swap_capture and table_swap_capture_truncate on the table. The log's
// comment names the target's oid. A run removes them when it ends; --abort
// removes what an interrupted run left.
package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	tableSwapLogSuffix              = "_next_log"
	tableSwapCaptureFunctionSuffix  = "_next_capture"
	tableSwapCaptureTrigger         = "table_swap_capture"
	tableSwapCaptureTruncateTrigger = "table_swap_capture_truncate"
)

// A run's target: the twin it creates, or the kept old table it rolls back
// into.
const (
	tableSwapForward  = "forward"
	tableSwapRollback = "rollback"
)

// The SQLSTATE the locked script raises when more changes were captured than
// it applies under the lock: the attempt releases the lock at once, and the
// swap catches up and tries again.
const tableSwapDeltaBusyCode = "UR701"

func tableSwapDeltaBusy(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == tableSwapDeltaBusyCode
}

// The settings the capture keeps its report in, transaction-local.
const (
	tableSwapCapturedSetting = "urnetwork.table_swap_captured"
	tableSwapAppliedSetting  = "urnetwork.table_swap_applied"
	tableSwapVerifiedSetting = "urnetwork.table_swap_verified"
)

// A trigger the capture of its own table installed: one of the capture's
// names, running that table's capture function. The preflight reports one as
// what an interrupted run left, not as a trigger of the table.
const tableSwapOwnCaptureTriggerSql = `(tgname IN ('` + tableSwapCaptureTrigger + `', '` + tableSwapCaptureTruncateTrigger + `')
	AND tgfoid = coalesce(to_regprocedure(format('public.%I()', (SELECT relname FROM pg_class WHERE oid = tgrelid) || '` + tableSwapCaptureFunctionSuffix + `'))::oid, 0))`

type tableSwapCapture struct {
	table string
	// <table>_next_log
	log string
	// <table>_next_capture
	function string
	// the table's primary key columns, in key order
	primaryKey []string
}

func newTableSwapCapture(table string, primaryKey []string) *tableSwapCapture {
	return &tableSwapCapture{
		table:      table,
		log:        table + tableSwapLogSuffix,
		function:   table + tableSwapCaptureFunctionSuffix,
		primaryKey: primaryKey,
	}
}

func (self *tableSwapCapture) quotedLog() string {
	return "public." + quoteIdentifier(self.log)
}

// The function with its (empty) argument list, as DROP FUNCTION and
// to_regprocedure name it.
func (self *tableSwapCapture) functionSignature() string {
	return "public." + quoteIdentifier(self.function) + "()"
}

// The key columns, each qualified with alias when one is given.
func (self *tableSwapCapture) keyList(alias string) string {
	keys := []string{}
	for _, column := range self.primaryKey {
		if alias == "" {
			keys = append(keys, quoteIdentifier(column))
		} else {
			keys = append(keys, quoteIdentifier(alias)+"."+quoteIdentifier(column))
		}
	}
	return strings.Join(keys, ", ")
}

// The key as one row value, for IN and = between relations.
func (self *tableSwapCapture) keyTuple(alias string) string {
	return "(" + self.keyList(alias) + ")"
}

// The log's comment: the table and the target the capture feeds.
func tableSwapCaptureMarker(table string, targetOid uint32, kind string) string {
	return fmt.Sprintf("table-swap capture of public.%s into oid %d (%s)", table, targetOid, kind)
}

var tableSwapCaptureMarkerPattern = regexp.MustCompile(`^table-swap capture of public\.(\S+) into oid ([0-9]+) \((forward|rollback)\)$`)

// The log table, its owner, grants and marker, and the capture function. The
// log has the key columns of the table, and every role that may insert,
// update or delete the table's rows may append to it. Autovacuum stays off
// the log: it lives minutes, and the swap drops it.
func (self *tableSwapCapture) createSql(source *tableSwapShape, targetOid uint32, kind string) []string {
	quotedTable := "public." + quoteIdentifier(self.table)
	statements := []string{
		fmt.Sprintf(
			"CREATE TABLE %s WITH (autovacuum_enabled = false) AS SELECT %s FROM %s WITH NO DATA",
			self.quotedLog(), self.keyList(""), quotedTable,
		),
		fmt.Sprintf("ALTER TABLE %s OWNER TO %s", self.quotedLog(), quoteIdentifier(source.owner)),
	}
	granted := map[string]bool{source.owner: true}
	for _, grant := range source.grants {
		parts := strings.Split(grant, "|")
		if len(parts) != 3 || granted[parts[0]] {
			continue
		}
		switch parts[1] {
		case "INSERT", "UPDATE", "DELETE":
		default:
			continue
		}
		granted[parts[0]] = true
		grantee := parts[0]
		if grantee != "PUBLIC" {
			grantee = quoteIdentifier(grantee)
		}
		statements = append(statements, fmt.Sprintf("GRANT INSERT ON %s TO %s", self.quotedLog(), grantee))
	}
	statements = append(statements,
		fmt.Sprintf("COMMENT ON TABLE %s IS %s", self.quotedLog(), tableSwapLiteral(tableSwapCaptureMarker(self.table, targetOid, kind))),
		self.functionSql(),
		fmt.Sprintf("ALTER FUNCTION %s OWNER TO %s", self.functionSignature(), quoteIdentifier(source.owner)),
	)
	return statements
}

// The capture function: an update logs the old key, and the new one when the
// key changed; an insert the new key; a delete the old key. It refuses a
// TRUNCATE, which fires no row trigger.
func (self *tableSwapCapture) functionSql() string {
	values := func(record string) string {
		keys := []string{}
		for _, column := range self.primaryKey {
			keys = append(keys, record+"."+quoteIdentifier(column))
		}
		return strings.Join(keys, ", ")
	}
	return strings.NewReplacer(
		"{function}", self.functionSignature(),
		"{log}", self.quotedLog(),
		"{keys}", self.keyList(""),
		"{old}", values("OLD"),
		"{new}", values("NEW"),
		"{refusal}", tableSwapLiteral(fmt.Sprintf(
			"public.%s is being swapped (bringyourctl model migrate table-swap): TRUNCATE is refused until the swap ends or --abort removes its change capture",
			self.table,
		)),
	).Replace(`CREATE FUNCTION {function} RETURNS trigger LANGUAGE plpgsql AS $table_swap_capture$
BEGIN
	IF TG_OP = 'TRUNCATE' THEN
		RAISE EXCEPTION {refusal};
	END IF;
	IF TG_OP = 'UPDATE' OR TG_OP = 'DELETE' THEN
		INSERT INTO {log} ({keys}) VALUES ({old});
	END IF;
	IF TG_OP = 'INSERT' THEN
		INSERT INTO {log} ({keys}) VALUES ({new});
	ELSIF TG_OP = 'UPDATE' THEN
		IF ({new}) IS DISTINCT FROM ({old}) THEN
			INSERT INTO {log} ({keys}) VALUES ({new});
		END IF;
	END IF;
	RETURN NULL;
END
$table_swap_capture$`)
}

// The triggers on relation: every row change, and TRUNCATE, which the
// function refuses. Both fire whatever session_replication_role a writer
// sets.
func (self *tableSwapCapture) triggerSql(relation string) []string {
	quoted := "public." + quoteIdentifier(relation)
	return []string{
		fmt.Sprintf("CREATE TRIGGER %s AFTER INSERT OR UPDATE OR DELETE ON %s FOR EACH ROW EXECUTE FUNCTION %s", tableSwapCaptureTrigger, quoted, self.functionSignature()),
		fmt.Sprintf("CREATE TRIGGER %s BEFORE TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION %s", tableSwapCaptureTruncateTrigger, quoted, self.functionSignature()),
		fmt.Sprintf("ALTER TABLE %s ENABLE ALWAYS TRIGGER %s", quoted, tableSwapCaptureTrigger),
		fmt.Sprintf("ALTER TABLE %s ENABLE ALWAYS TRIGGER %s", quoted, tableSwapCaptureTruncateTrigger),
	}
}

// Raises unless both capture triggers are on relation, firing always and
// running the capture function: a trigger dropped or disabled since the
// capture began means writes went uncaptured.
func (self *tableSwapCapture) capturedAssertSql(relation string) string {
	return tableSwapAssertSql(
		fmt.Sprintf(
			"(SELECT count(*) FROM pg_trigger WHERE tgrelid = %s::regclass AND tgname IN ('%s', '%s') AND tgenabled = 'A' AND tgfoid = %s::regprocedure) = 2",
			tableSwapLiteral("public."+quoteIdentifier(relation)), tableSwapCaptureTrigger, tableSwapCaptureTruncateTrigger, tableSwapLiteral(self.functionSignature()),
		),
		fmt.Sprintf("the change capture on %s is not whole: a capture trigger was dropped or disabled since it began", relation),
	)
}

func (self *tableSwapCapture) dropTriggerSql(relation string) []string {
	quoted := "public." + quoteIdentifier(relation)
	return []string{
		fmt.Sprintf("DROP TRIGGER %s ON %s", tableSwapCaptureTrigger, quoted),
		fmt.Sprintf("DROP TRIGGER %s ON %s", tableSwapCaptureTruncateTrigger, quoted),
	}
}

// Applies the captured keys to target, as one statement: deletes the target's
// rows of every captured key, copies the table's rows of them, checks the
// count and the two checksums of the rows read against those written (as the
// copy does), and empties the log of the rows it read. With maxCaptured, more
// captured rows than that raise tableSwapDeltaBusyCode before anything
// changes. Keeps the captured and the copied counts
// (tableSwapCapturedSetting, tableSwapAppliedSetting).
//
// The table is read one captured key at a time through its primary key
// (LATERAL), whatever the planner estimates of the log, which is never
// analyzed: a join that scanned the bloated table would cost the lock what
// the copy did.
//
// Read committed under the lock, where nothing writes; repeatable read
// outside it, so the log, the table and the target are read at one snapshot.
func (self *tableSwapCapture) applySql(source *tableSwapShape, target string, maxCaptured int64) string {
	bound := ""
	if 0 < maxCaptured {
		bound = fmt.Sprintf(`IF %d < captured_count THEN
		RAISE EXCEPTION USING ERRCODE = '%s', MESSAGE = format('%%s holds %%s captured changes, more than the swap applies under its lock (%d)', %s, captured_count);
	END IF;`, maxCaptured, tableSwapDeltaBusyCode, maxCaptured, tableSwapLiteral(self.log))
	}
	return strings.NewReplacer(
		"{log}", self.quotedLog(),
		"{logName}", tableSwapLiteral(self.log),
		"{table}", "public."+quoteIdentifier(self.table),
		"{tableName}", tableSwapLiteral(self.table),
		"{target}", "public."+quoteIdentifier(target),
		"{targetName}", tableSwapLiteral(target),
		"{keys}", self.keyList(""),
		"{capturedKey}", self.keyTuple("captured"),
		"{sourceKey}", self.keyTuple("source_table"),
		"{sourceRowKeys}", self.keyList("source_row"),
		"{targetKey}", self.keyTuple("target_table"),
		"{all}", tableSwapQuotedColumns(source.columns, false),
		"{stored}", tableSwapQuotedColumns(source.columns, true),
		"{bound}", bound,
		"{captured}", tableSwapCapturedSetting,
		"{applied}", tableSwapAppliedSetting,
	).Replace(`DO $table_swap$
DECLARE
	captured_count bigint;
	source_count bigint;
	source_xor bigint;
	source_sum numeric;
	copied_count bigint;
	copied_xor bigint;
	copied_sum numeric;
BEGIN
	SELECT count(*) INTO captured_count FROM {log};
	{bound}
	DELETE FROM {target} target_table USING (SELECT DISTINCT {keys} FROM {log}) captured WHERE {targetKey} = {capturedKey};
	WITH source_rows AS MATERIALIZED (
		SELECT source_row.*
		FROM (SELECT DISTINCT {keys} FROM {log}) captured
		CROSS JOIN LATERAL (SELECT {all} FROM {table} source_table WHERE {sourceKey} = {capturedKey}) source_row
		ORDER BY {sourceRowKeys}
	), copied AS (
		INSERT INTO {target} ({stored}) SELECT {stored} FROM source_rows
		RETURNING {all}
	)
	SELECT
		(SELECT count(*) FROM source_rows),
		(SELECT coalesce(bit_xor(hashtextextended(source_row::text, 0)), 0) FROM source_rows source_row),
		(SELECT coalesce(sum(hashtextextended(source_row::text, 1)::numeric), 0) FROM source_rows source_row),
		(SELECT count(*) FROM copied),
		(SELECT coalesce(bit_xor(hashtextextended(copied_row::text, 0)), 0) FROM copied copied_row),
		(SELECT coalesce(sum(hashtextextended(copied_row::text, 1)::numeric), 0) FROM copied copied_row)
	INTO source_count, source_xor, source_sum, copied_count, copied_xor, copied_sum;
	IF source_count IS DISTINCT FROM copied_count OR source_xor IS DISTINCT FROM copied_xor OR source_sum IS DISTINCT FROM copied_sum THEN
		RAISE EXCEPTION 'the captured changes applied to % do not match %: % rows (checksums %, %) against % rows (checksums %, %)',
			{targetName}, {tableName}, copied_count, copied_xor, copied_sum, source_count, source_xor, source_sum;
	END IF;
	IF (SELECT count(*) FROM {target} target_table WHERE {targetKey} IN (SELECT {keys} FROM {log})) <> source_count THEN
		RAISE EXCEPTION '% does not hold the % rows of the captured keys it was given', {targetName}, source_count;
	END IF;
	DELETE FROM {log};
	PERFORM set_config('{captured}', captured_count::text, true);
	PERFORM set_config('{applied}', source_count::text, true);
END
$table_swap$`)
}

// Checks, as one statement at one snapshot (repeatable read), that the target
// holds exactly the table's rows of every key the log does not hold: the
// count and the two checksums over every column, generated ones included.
// Both sides are read through their primary keys. Keeps the rows compared
// and the captured rows outstanding.
func (self *tableSwapCapture) verifySql(source *tableSwapShape, target string) string {
	return strings.NewReplacer(
		"{log}", self.quotedLog(),
		"{table}", "public."+quoteIdentifier(self.table),
		"{tableName}", tableSwapLiteral(self.table),
		"{target}", "public."+quoteIdentifier(target),
		"{targetName}", tableSwapLiteral(target),
		"{keys}", self.keyList(""),
		"{capturedKey}", self.keyTuple("captured"),
		"{sourceKey}", self.keyTuple("source_table"),
		"{targetKey}", self.keyTuple("target_table"),
		"{all}", tableSwapQuotedColumns(source.columns, false),
		"{captured}", tableSwapCapturedSetting,
		"{verified}", tableSwapVerifiedSetting,
	).Replace(`DO $table_swap$
DECLARE
	captured_count bigint;
	source_count bigint;
	source_xor bigint;
	source_sum numeric;
	target_count bigint;
	target_xor bigint;
	target_sum numeric;
BEGIN
	SELECT count(*) INTO captured_count FROM {log};
	SELECT count(*), coalesce(bit_xor(hashtextextended(source_row::text, 0)), 0), coalesce(sum(hashtextextended(source_row::text, 1)::numeric), 0)
	INTO source_count, source_xor, source_sum
	FROM (
		SELECT {all} FROM {table} source_table
		WHERE NOT EXISTS (SELECT 1 FROM {log} captured WHERE {capturedKey} = {sourceKey})
		ORDER BY {keys}
	) source_row;
	SELECT count(*), coalesce(bit_xor(hashtextextended(target_row::text, 0)), 0), coalesce(sum(hashtextextended(target_row::text, 1)::numeric), 0)
	INTO target_count, target_xor, target_sum
	FROM (
		SELECT {all} FROM {target} target_table
		WHERE NOT EXISTS (SELECT 1 FROM {log} captured WHERE {capturedKey} = {targetKey})
		ORDER BY {keys}
	) target_row;
	IF source_count IS DISTINCT FROM target_count OR source_xor IS DISTINCT FROM target_xor OR source_sum IS DISTINCT FROM target_sum THEN
		RAISE EXCEPTION '% differs from % outside the % captured change(s): % rows (checksums %, %) against % rows (checksums %, %)',
			{targetName}, {tableName}, captured_count, target_count, target_xor, target_sum, source_count, source_xor, source_sum;
	END IF;
	PERFORM set_config('{captured}', captured_count::text, true);
	PERFORM set_config('{verified}', source_count::text, true);
END
$table_swap$`)
}

// What the catalog holds of a table's capture: the log with its marker, the
// function, the capture triggers on the table, the table's and the twin's
// oids, and the session holding the run lock.
type tableSwapCaptureState struct {
	capture     *tableSwapCapture
	logOid      uint32
	marker      string
	targetOid   uint32
	kind        string
	functionOid uint32
	// name:tgenabled of each capture trigger on the table
	triggers   []string
	tableOid   uint32
	twinOid    uint32
	runHolders []int64
}

// The namespace of the run lock, a session advisory lock in its two-key form
// (objsubid 2), which the task advisory locks (one key) never take.
const tableSwapRunLockNamespace = "urnetwork.table_swap"

const tableSwapCaptureStateSql = `
	SELECT
		coalesce(to_regclass($1)::oid, 0),
		coalesce(obj_description(to_regclass($1), 'pg_class'), ''),
		coalesce(to_regprocedure($2)::oid, 0),
		ARRAY(
			SELECT tgname::text || ':' || tgenabled::text
			FROM pg_trigger
			WHERE tgrelid = to_regclass($3) AND tgname IN ($4, $5) AND tgfoid = to_regprocedure($2)
			ORDER BY tgname
		),
		coalesce(to_regclass($3)::oid, 0),
		coalesce(to_regclass($6)::oid, 0),
		ARRAY(
			SELECT pid::bigint
			FROM pg_locks
			WHERE locktype = 'advisory' AND objsubid = 2 AND granted
				AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
				AND classid::bigint = (hashtext($7)::bigint & 4294967295)
				AND objid::bigint = (hashtext($8)::bigint & 4294967295)
			ORDER BY pid
		)
`

func readTableSwapCaptureState(ctx context.Context, conn PgQuerier, capture *tableSwapCapture) *tableSwapCaptureState {
	state := &tableSwapCaptureState{capture: capture}
	Raise(conn.QueryRow(
		ctx,
		tableSwapCaptureStateSql,
		capture.quotedLog(),
		capture.functionSignature(),
		"public."+quoteIdentifier(capture.table),
		tableSwapCaptureTrigger,
		tableSwapCaptureTruncateTrigger,
		"public."+quoteIdentifier(capture.table+tableSwapNextSuffix),
		tableSwapRunLockNamespace,
		capture.table,
	).Scan(&state.logOid, &state.marker, &state.functionOid, &state.triggers, &state.tableOid, &state.twinOid, &state.runHolders))
	if match := tableSwapCaptureMarkerPattern.FindStringSubmatch(state.marker); match != nil && match[1] == capture.table {
		if oid, err := strconv.ParseUint(match[2], 10, 32); err == nil {
			state.targetOid = uint32(oid)
			state.kind = match[3]
		}
	}
	return state
}

// Whether anything of a capture is in the catalog.
func (self *tableSwapCaptureState) left() bool {
	return self.logOid != 0 || self.functionOid != 0 || 0 < len(self.triggers)
}

// Whether the swap the capture fed committed: the table is now its target.
func (self *tableSwapCaptureState) committed() bool {
	return self.targetOid != 0 && self.targetOid == self.tableOid
}

// Whether the twin is the capture's own forward target.
func (self *tableSwapCaptureState) ownsTwin() bool {
	return self.kind == tableSwapForward && self.targetOid != 0 && self.twinOid == self.targetOid && !self.committed()
}

// What the capture left, for a report.
func (self *tableSwapCaptureState) String() string {
	parts := []string{}
	if 0 < len(self.triggers) {
		parts = append(parts, fmt.Sprintf("trigger(s) %s on %s", strings.Join(self.triggers, ", "), self.capture.table))
	}
	if self.functionOid != 0 {
		parts = append(parts, "function "+self.capture.function+"()")
	}
	if self.logOid != 0 {
		if self.marker == "" {
			parts = append(parts, "log "+self.capture.log)
		} else {
			parts = append(parts, fmt.Sprintf("log %s (%s)", self.capture.log, self.marker))
		}
	}
	if self.ownsTwin() {
		parts = append(parts, fmt.Sprintf("its twin %s (oid %d)", self.capture.table+tableSwapNextSuffix, self.twinOid))
	}
	if self.committed() {
		parts = append(parts, "after its swap committed")
	}
	return strings.Join(parts, ", ")
}
