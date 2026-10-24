package model

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
)

// The cutover of provider_egress_health_history to daily partitions
// (PARTITION-MAINTENANCE-PLAN.md C2), an owner-run command:
// bringyourctl model migrate provider-egress-health-partition. Each step is
// idempotent and resumable, and prints what it would do under --dry-run.
//
//   - prepare (the default): stage provider_egress_health_history_new, range
//     partitioned by measured_at with the primary key (run_id, measured_at) and
//     every secondary index of the table, with a day partition for each day
//     from the copy window's first through StagedAheadDays after today. Then
//     install the change capture, a trigger that mirrors each row inserted into
//     the table within the window into the staged one, and copy the window hour
//     by hour, skipping an hour whose counts already agree. Older rows are not
//     carried over: nothing reads past eight hours but one diagnostic, bounded
//     to the retention. A rerun extends the days and the mirror and copies only
//     the hours that differ.
//   - --swap: one transaction locks both tables (ACCESS EXCLUSIVE, a short lock
//     wait, retried), checks that every day of the window holds as many rows in
//     each (exact: the staged rows are copies of the table's), drops the mirror
//     and swaps the names: the table becomes provider_egress_health_history_old,
//     the staged one provider_egress_health_history. Ingest waits on the lock
//     for that moment, then writes to the partitioned table.
//   - --verify: the partitioned table has its key, its indexes and its days
//     ahead, and every row of the window in the old table is in it.
//   - --rollback: until --drop-old, undo the swap and keep the rows written
//     since: mirror the partitioned table into the old one, copy back what it
//     holds, then swap the names back under the same lock and exact check. The
//     staged copy stays for a later prepare, or --abort.
//   - --drop-old: once --verify passes, drop the old table (about 125 GB on
//     main).
//   - --abort: before the swap, drop the mirror and the staged table.
//
// The lock is ACCESS EXCLUSIVE from the start rather than EXCLUSIVE: a store
// reads the history for its run id before it inserts, so a swap that held
// EXCLUSIVE while a store held its read would deadlock with it at the rename.

const providerEgressHealthHistoryStagedTable = "provider_egress_health_history_new"
const providerEgressHealthHistoryOldTable = "provider_egress_health_history_old"
const providerEgressHealthHistoryMirror = "provider_egress_health_history_cutover_mirror"
const providerEgressHealthHistoryRollbackMirror = "provider_egress_health_history_rollback_mirror"

// One step of the cutover, chosen by the command's flags.
type ProviderEgressHealthHistoryCutoverOptions struct {
	// Days of history the staged table takes: today and the days before it.
	// At least 2, so the window holds every day the ingest still takes.
	CopyDays int
	DryRun   bool
	Swap     bool
	Verify   bool
	Rollback bool
	Abort    bool
	DropOld  bool
	Logf     func(string, ...any)
	// nil uses DefaultProviderEgressHealthHistoryCutoverSettings
	Settings *ProviderEgressHealthHistoryCutoverSettings
	// a test's barrier: runs in the swap transaction once it holds its locks
	// and its check passed, before the names change
	swapLocked func()
}

// Tunables of the cutover.
type ProviderEgressHealthHistoryCutoverSettings struct {
	// Staged days after today, so --swap can follow prepare within this many
	// days; a later swap first reruns prepare.
	StagedAheadDays int
	// Each lock wait of a step that locks the table: a wait queues ingest
	// behind it, so it is short and the step retries.
	LockTimeout time.Duration
	// Bounds each statement of the swap and rollback transactions, which
	// hold their locks: the count checks and the catalog work.
	SwapStatementTimeout time.Duration
	// How many times, and how far apart, a busy lock is tried again.
	LockAttempts      int
	LockRetryInterval time.Duration
	// A transaction older than this that holds a lock on the table defers
	// the swap, so its lock request never queues ingest behind it.
	MaxTransactionAge time.Duration
	// The copy's unit: one statement per this much of measured_at.
	CopyChunk time.Duration
}

// A week to swap, seconds of lock wait retried for ten minutes, and copies of
// an hour each.
func DefaultProviderEgressHealthHistoryCutoverSettings() *ProviderEgressHealthHistoryCutoverSettings {
	return &ProviderEgressHealthHistoryCutoverSettings{
		StagedAheadDays:      7,
		LockTimeout:          5 * time.Second,
		SwapStatementTimeout: 60 * time.Second,
		LockAttempts:         60,
		LockRetryInterval:    10 * time.Second,
		MaxTransactionAge:    5 * time.Second,
		CopyChunk:            time.Hour,
	}
}

// Runs the step the options choose, at now.
func MigrateProviderEgressHealthHistoryToPartitions(ctx context.Context, now time.Time, options ProviderEgressHealthHistoryCutoverOptions) error {
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	settings := options.Settings
	if settings == nil {
		settings = DefaultProviderEgressHealthHistoryCutoverSettings()
	}
	steps := 0
	for _, step := range []bool{options.Swap, options.Verify, options.Rollback, options.Abort, options.DropOld} {
		if step {
			steps += 1
		}
	}
	if 1 < steps {
		return errors.New("choose one of --swap, --verify, --rollback, --abort and --drop-old")
	}
	cutover := &providerEgressHealthHistoryCutover{
		now:        now.UTC(),
		dryRun:     options.DryRun,
		logf:       logf,
		settings:   settings,
		swapLocked: options.swapLocked,
	}
	switch {
	case options.Swap:
		return cutover.swap(ctx)
	case options.Verify:
		return cutover.verify(ctx)
	case options.Rollback:
		return cutover.rollback(ctx)
	case options.Abort:
		return cutover.abort(ctx)
	case options.DropOld:
		return cutover.dropOld(ctx)
	default:
		copyDays := options.CopyDays
		if copyDays == 0 {
			copyDays = 2
		}
		if copyDays < 2 {
			return fmt.Errorf("--copy-days must be at least 2: the window must hold every day the ingest takes (%s)", DefaultProviderEgressHealthHistorySettings().MaxIngestAge)
		}
		return cutover.prepare(ctx, copyDays)
	}
}

// One run of the cutover. Not safe for concurrent use.
type providerEgressHealthHistoryCutover struct {
	now        time.Time
	dryRun     bool
	logf       func(string, ...any)
	settings   *ProviderEgressHealthHistoryCutoverSettings
	swapLocked func()
}

// A relation's kind, or "" when it does not exist.
func providerEgressHealthHistoryRelkind(ctx context.Context, table string) (relkind string) {
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE((SELECT relkind::text FROM pg_class WHERE oid = to_regclass($1)), '')`,
			"public."+table,
		).Scan(&relkind))
	})
	return
}

// Whether a trigger of that name is on the table.
func providerEgressHealthHistoryTriggerExists(ctx context.Context, table string, trigger string) (exists bool) {
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = to_regclass($1) AND tgname = $2 AND NOT tgisinternal)`,
			"public."+table,
			trigger,
		).Scan(&exists))
	})
	return
}

// One column as both tables must have it.
type providerEgressHealthHistoryColumn struct {
	name     string
	typeName string
	notNull  bool
}

func readProviderEgressHealthHistoryColumns(ctx context.Context, conn server.PgQuerier, table string) (columns []providerEgressHealthHistoryColumn) {
	result, err := conn.Query(
		ctx,
		`
		SELECT attname, format_type(atttypid, atttypmod), attnotnull
		FROM pg_attribute
		WHERE attrelid = to_regclass($1) AND 0 < attnum AND NOT attisdropped
		ORDER BY attnum
		`,
		"public."+table,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var column providerEgressHealthHistoryColumn
			server.Raise(result.Scan(&column.name, &column.typeName, &column.notNull))
			columns = append(columns, column)
		}
	})
	return
}

// The quoted column names, joined, with an optional prefix on each.
func providerEgressHealthHistoryColumnList(columns []providerEgressHealthHistoryColumn, prefix string) string {
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		names = append(names, prefix+pgx.Identifier{column.name}.Sanitize())
	}
	return strings.Join(names, ", ")
}

// One index of a table.
type providerEgressHealthHistoryIndex struct {
	name       string
	definition string
	primary    bool
	unique     bool
	valid      bool
}

func readProviderEgressHealthHistoryIndexes(ctx context.Context, conn server.PgQuerier, table string) (indexes []providerEgressHealthHistoryIndex) {
	result, err := conn.Query(
		ctx,
		`
		SELECT c.relname, pg_get_indexdef(i.indexrelid), i.indisprimary, i.indisunique, i.indisvalid AND i.indisready
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		WHERE i.indrelid = to_regclass($1)
		ORDER BY c.relname
		`,
		"public."+table,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var index providerEgressHealthHistoryIndex
			server.Raise(result.Scan(&index.name, &index.definition, &index.primary, &index.unique, &index.valid))
			indexes = append(indexes, index)
		}
	})
	return
}

var providerEgressHealthHistoryIndexPattern = regexp.MustCompile(`^CREATE (UNIQUE )?INDEX (\S+) ON (ONLY )?(\S+) (USING .*)$`)

// An index's name moved from one table name prefix to another:
// provider_egress_health_history_time to provider_egress_health_history_new_time.
func providerEgressHealthHistoryIndexName(name string, fromTable string, toTable string) (string, bool) {
	suffix, ok := strings.CutPrefix(name, fromTable+"_")
	if !ok {
		return "", false
	}
	return toTable + "_" + suffix, true
}

// The secondary index on the staged table under its staged name, or why it
// cannot be staged.
func stagedProviderEgressHealthHistoryIndexSql(index providerEgressHealthHistoryIndex) (string, error) {
	if !index.valid {
		return "", fmt.Errorf("index %s is invalid; drop it (DROP INDEX CONCURRENTLY) or rebuild it first", index.name)
	}
	if index.unique {
		return "", fmt.Errorf("unique index %s cannot be partitioned by measured_at", index.name)
	}
	groups := providerEgressHealthHistoryIndexPattern.FindStringSubmatch(strings.Join(strings.Fields(index.definition), " "))
	if groups == nil {
		return "", fmt.Errorf("index %s has an unexpected definition: %s", index.name, index.definition)
	}
	name, ok := providerEgressHealthHistoryIndexName(index.name, providerEgressHealthHistoryTable, providerEgressHealthHistoryStagedTable)
	if !ok || 63 < len(name) {
		return "", fmt.Errorf("index %s cannot take a staged name", index.name)
	}
	return fmt.Sprintf(
		"CREATE INDEX %s ON public.%s %s",
		pgx.Identifier{name}.Sanitize(),
		pgx.Identifier{providerEgressHealthHistoryStagedTable}.Sanitize(),
		groups[5],
	), nil
}

// Runs callback in one maintenance transaction at read committed, without
// retries, and returns its failure.
func providerEgressHealthHistoryTx(ctx context.Context, callback func(server.PgTx)) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if server.IsDoneError(r) {
				panic(r)
			}
			if recoveredErr, ok := r.(error); ok {
				err = recoveredErr
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	server.MaintenanceTx(ctx, callback, server.TxReadCommitted, server.OptNoRetry())
	return
}

// A lock wait that timed out or lost a deadlock can succeed on a later try.
func providerEgressHealthHistoryLockBusy(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == pgerrcode.LockNotAvailable || pgErr.Code == pgerrcode.DeadlockDetected)
}

// The transactions older than MaxTransactionAge holding a lock on a table: a
// lock request would queue ingest behind them.
func (self *providerEgressHealthHistoryCutover) busyTransactions(ctx context.Context, tables ...string) (busy int) {
	relations := []string{}
	for _, table := range tables {
		relations = append(relations, "public."+table)
	}
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT count(DISTINCT activity.pid)
			FROM pg_locks lock
			JOIN pg_stat_activity activity ON activity.pid = lock.pid
			WHERE lock.relation = ANY (ARRAY(SELECT to_regclass(name) FROM unnest($1::text[]) name))
				AND activity.pid <> pg_backend_pid()
				AND activity.xact_start < now() - make_interval(secs => $2)
			`,
			relations,
			self.settings.MaxTransactionAge.Seconds(),
		).Scan(&busy))
	})
	return
}

// Runs a locking step, retrying while its locks are busy or a long
// transaction holds the tables, so its lock request never waits long.
func (self *providerEgressHealthHistoryCutover) retryLocked(ctx context.Context, tables []string, run func() error) (err error) {
	for attempt := 1; attempt <= self.settings.LockAttempts; attempt += 1 {
		if busy := self.busyTransactions(ctx, tables...); 0 < busy {
			self.logf("%d transaction(s) older than %s hold locks on %s (attempt %d/%d)", busy, self.settings.MaxTransactionAge, strings.Join(tables, ", "), attempt, self.settings.LockAttempts)
			err = fmt.Errorf("%d long transaction(s) hold locks on %s", busy, strings.Join(tables, ", "))
		} else {
			err = run()
			if err == nil || !providerEgressHealthHistoryLockBusy(err) {
				return
			}
			self.logf("lock busy (attempt %d/%d): %v", attempt, self.settings.LockAttempts, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(self.settings.LockRetryInterval):
		}
	}
	return
}

// Row counts by the start of each step of measured_at in [from, until),
// steps of length step from from.
func providerEgressHealthHistoryCounts(ctx context.Context, conn server.PgQuerier, table string, from time.Time, until time.Time, step time.Duration) map[time.Time]int64 {
	counts := map[time.Time]int64{}
	result, err := conn.Query(
		ctx,
		fmt.Sprintf(
			`
			SELECT
				$1::timestamp + floor(extract(epoch FROM (measured_at - $1::timestamp))::double precision / $3::double precision) * make_interval(secs => $3::double precision),
				count(*)
			FROM public.%s
			WHERE $1::timestamp <= measured_at AND measured_at < $2::timestamp
			GROUP BY 1
			`,
			pgx.Identifier{table}.Sanitize(),
		),
		from,
		until,
		step.Seconds(),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var start time.Time
			var count int64
			server.Raise(result.Scan(&start, &count))
			counts[start.UTC()] = count
		}
	})
	return counts
}

// The steps whose counts differ between two count maps, oldest first.
func providerEgressHealthHistoryCountDifferences(a map[time.Time]int64, b map[time.Time]int64) (differ []time.Time) {
	for start, count := range a {
		if b[start] != count {
			differ = append(differ, start)
		}
	}
	for start, count := range b {
		if _, ok := a[start]; !ok && count != 0 {
			differ = append(differ, start)
		}
	}
	slices.SortFunc(differ, func(x time.Time, y time.Time) int { return x.Compare(y) })
	return slices.Compact(differ)
}

// The partitions of the staged table, or after the swap of the partitioned
// one. The staged table's leaves already carry the names they keep after the
// swap, the family's names for provider_egress_health_history, so each is
// judged canonical against those.
func providerEgressHealthHistoryLeaves(ctx context.Context, parent string) (leaves []server.PartitionLeaf, err error) {
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		leaves, err = providerEgressHealthHistoryPartitionFamily.ListLeaves(ctx, conn, parent)
	})
	for i, leaf := range leaves {
		if leaf.Range.Default || leaf.Range.Lower.IsZero() || leaf.Range.Upper.IsZero() {
			leaves[i].Canonical = false
			continue
		}
		r := server.PartitionGranularityDaily.RangeOf(leaf.Range.Lower)
		leaves[i].Canonical = leaf.Range.Lower.Equal(r.Lower) && leaf.Range.Upper.Equal(r.Upper) &&
			leaf.Name == providerEgressHealthHistoryPartitionFamily.LeafName(providerEgressHealthHistoryTable, r)
	}
	return
}

// The first and the end of the days the leaves cover without a gap from the
// first, or false when there are none.
func providerEgressHealthHistoryLeafSpan(leaves []server.PartitionLeaf) (from time.Time, until time.Time, ok bool) {
	starts := []time.Time{}
	for _, leaf := range leaves {
		if leaf.Canonical && !leaf.Range.Lower.IsZero() && !leaf.Range.Upper.IsZero() {
			starts = append(starts, leaf.Range.Lower)
		}
	}
	if len(starts) == 0 {
		return time.Time{}, time.Time{}, false
	}
	slices.SortFunc(starts, func(x time.Time, y time.Time) int { return x.Compare(y) })
	from = starts[0]
	until = from
	for _, start := range starts {
		if !start.Equal(until) {
			break
		}
		until = server.PartitionGranularityDaily.NextRangeStart(until)
	}
	return from, until, true
}

// The mirror trigger function's definition: each row of the window inserted
// into source is inserted into target, a row already there skipped.
func providerEgressHealthHistoryMirrorSql(function string, target string, columns []providerEgressHealthHistoryColumn, from time.Time, until time.Time) string {
	return fmt.Sprintf(
		`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $mirror$
		BEGIN
			IF '%s'::timestamp <= NEW.measured_at AND NEW.measured_at < '%s'::timestamp THEN
				INSERT INTO public.%s (%s) VALUES (%s) ON CONFLICT DO NOTHING;
			END IF;
			RETURN NULL;
		END
		$mirror$
		`,
		function,
		from.Format("2006-01-02 15:04:05"),
		until.Format("2006-01-02 15:04:05"),
		pgx.Identifier{target}.Sanitize(),
		providerEgressHealthHistoryColumnList(columns, ""),
		providerEgressHealthHistoryColumnList(columns, "NEW."),
	)
}

// Copies the rows of source in [from, until) that target lacks, one chunk per
// statement on a maintenance session exempt from transaction_timeout, and
// skips a chunk whose counts already agree. Returns the rows copied.
func (self *providerEgressHealthHistoryCutover) copyWindow(ctx context.Context, source string, target string, columns []providerEgressHealthHistoryColumn, from time.Time, until time.Time) (copied int64) {
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.WithoutTransactionTimeout(ctx, conn, func() {
			sourceCounts := providerEgressHealthHistoryCounts(ctx, conn, source, from, until, self.settings.CopyChunk)
			targetCounts := providerEgressHealthHistoryCounts(ctx, conn, target, from, until, self.settings.CopyChunk)
			differ := providerEgressHealthHistoryCountDifferences(sourceCounts, targetCounts)
			self.logf("copy %s to %s: %d of %d %s chunks differ", source, target, len(differ), len(sourceCounts), self.settings.CopyChunk)
			if self.dryRun {
				return
			}
			columnList := providerEgressHealthHistoryColumnList(columns, "")
			for i, start := range differ {
				tag, err := conn.Exec(
					ctx,
					fmt.Sprintf(
						`INSERT INTO public.%s (%s) SELECT %s FROM public.%s WHERE $1 <= measured_at AND measured_at < $2 ON CONFLICT DO NOTHING`,
						pgx.Identifier{target}.Sanitize(),
						columnList,
						columnList,
						pgx.Identifier{source}.Sanitize(),
					),
					start,
					start.Add(self.settings.CopyChunk),
				)
				server.Raise(err)
				copied += tag.RowsAffected()
				if (i+1)%24 == 0 || i+1 == len(differ) {
					self.logf("copied %d/%d chunks, %d rows", i+1, len(differ), copied)
				}
			}
		})
	})
	return
}

// Installs the mirror on table, or refreshes the window of one installed: the
// function is replaced, and the trigger created when it is missing (on a
// partitioned table PostgreSQL clones it onto every partition), a brief SHARE
// ROW EXCLUSIVE lock.
func (self *providerEgressHealthHistoryCutover) installMirror(ctx context.Context, table string, function string, target string, columns []providerEgressHealthHistoryColumn, from time.Time, until time.Time) error {
	statements := []string{
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds()),
		providerEgressHealthHistoryMirrorSql(function, target, columns, from, until),
	}
	if !providerEgressHealthHistoryTriggerExists(ctx, table, function) {
		statements = append(statements, fmt.Sprintf(
			"CREATE TRIGGER %s AFTER INSERT ON public.%s FOR EACH ROW EXECUTE FUNCTION %s()",
			function,
			pgx.Identifier{table}.Sanitize(),
			function,
		))
	}
	if self.dryRun {
		self.logf("would install the mirror on %s:\n%s", table, strings.Join(statements, ";\n"))
		return nil
	}
	return self.retryLocked(ctx, []string{table}, func() error {
		return providerEgressHealthHistoryTx(ctx, func(tx server.PgTx) {
			for _, statement := range statements {
				server.RaisePgResult(tx.Exec(ctx, statement))
			}
		})
	})
}

func (self *providerEgressHealthHistoryCutover) prepare(ctx context.Context, copyDays int) error {
	liveKind := providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryTable)
	switch liveKind {
	case "p":
		self.logf("%s is already partitioned; nothing to prepare", providerEgressHealthHistoryTable)
		return nil
	case "r":
	default:
		return fmt.Errorf("%s does not exist as a table (relkind %q)", providerEgressHealthHistoryTable, liveKind)
	}
	if oldKind := providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryOldTable); oldKind != "" {
		return fmt.Errorf("%s exists beside the plain table; inspect it before preparing again", providerEgressHealthHistoryOldTable)
	}
	stagedKind := providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryStagedTable)
	if stagedKind != "" && stagedKind != "p" {
		return fmt.Errorf("%s exists but is not partitioned (relkind %s); drop it and prepare again", providerEgressHealthHistoryStagedTable, stagedKind)
	}

	today := server.PartitionGranularityDaily.RangeStart(self.now)
	from := server.PartitionGranularityDaily.AddRanges(today, -(copyDays - 1))
	until := server.PartitionGranularityDaily.AddRanges(today, self.settings.StagedAheadDays+1)
	var leaves []server.PartitionLeaf
	if stagedKind == "p" {
		var err error
		leaves, err = providerEgressHealthHistoryLeaves(ctx, providerEgressHealthHistoryStagedTable)
		if err != nil {
			return err
		}
		// a resumed prepare keeps the window it started with
		if stagedFrom, _, ok := providerEgressHealthHistoryLeafSpan(leaves); ok && stagedFrom.Before(from) {
			from = stagedFrom
		}
	}
	self.logf("window: measured_at from %s; staged days through %s", from.Format(time.DateOnly), until.AddDate(0, 0, -1).Format(time.DateOnly))

	var liveColumns []providerEgressHealthHistoryColumn
	var liveIndexes []providerEgressHealthHistoryIndex
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		liveColumns = readProviderEgressHealthHistoryColumns(ctx, conn, providerEgressHealthHistoryTable)
		liveIndexes = readProviderEgressHealthHistoryIndexes(ctx, conn, providerEgressHealthHistoryTable)
	})
	if !slices.ContainsFunc(liveColumns, func(column providerEgressHealthHistoryColumn) bool {
		return column.name == "measured_at" && column.typeName == "timestamp without time zone" && column.notNull
	}) {
		return fmt.Errorf("%s.measured_at is not a NOT NULL timestamp", providerEgressHealthHistoryTable)
	}
	// the staged table carries no trigger or foreign key across the swap, so
	// the table must have none but the mirror
	var attachments int
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT
				(SELECT count(*) FROM pg_trigger WHERE tgrelid = to_regclass($1) AND NOT tgisinternal AND tgname <> $2) +
				(SELECT count(*) FROM pg_constraint WHERE (conrelid = to_regclass($1) OR confrelid = to_regclass($1)) AND contype = 'f')
			`,
			"public."+providerEgressHealthHistoryTable,
			providerEgressHealthHistoryMirror,
		).Scan(&attachments))
	})
	if 0 < attachments {
		return fmt.Errorf("%s has %d trigger(s) or foreign key(s) the cutover would not carry over", providerEgressHealthHistoryTable, attachments)
	}

	if stagedKind == "" {
		statements := []string{
			fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds()),
			fmt.Sprintf(
				"CREATE TABLE public.%s (LIKE public.%s INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING STORAGE INCLUDING COMPRESSION) PARTITION BY RANGE (measured_at)",
				pgx.Identifier{providerEgressHealthHistoryStagedTable}.Sanitize(),
				pgx.Identifier{providerEgressHealthHistoryTable}.Sanitize(),
			),
			fmt.Sprintf(
				"ALTER TABLE public.%s ADD CONSTRAINT %s PRIMARY KEY (run_id, measured_at)",
				pgx.Identifier{providerEgressHealthHistoryStagedTable}.Sanitize(),
				pgx.Identifier{providerEgressHealthHistoryStagedTable + "_pkey"}.Sanitize(),
			),
		}
		for _, index := range liveIndexes {
			if index.primary {
				continue
			}
			statement, err := stagedProviderEgressHealthHistoryIndexSql(index)
			if err != nil {
				return err
			}
			statements = append(statements, statement)
		}
		if self.dryRun {
			self.logf("would stage %s:\n%s", providerEgressHealthHistoryStagedTable, strings.Join(statements, ";\n"))
		} else {
			err := providerEgressHealthHistoryTx(ctx, func(tx server.PgTx) {
				for _, statement := range statements {
					server.RaisePgResult(tx.Exec(ctx, statement))
				}
				// LIKE names the not-null constraints after the staged table;
				// they take the table's names, which stay per table
				for _, statement := range providerEgressHealthHistoryNotNullRenameSql(ctx, tx) {
					server.RaisePgResult(tx.Exec(ctx, statement))
				}
			})
			if err != nil {
				return fmt.Errorf("stage %s: %w", providerEgressHealthHistoryStagedTable, err)
			}
			self.logf("staged %s with %d indexes", providerEgressHealthHistoryStagedTable, len(liveIndexes))
		}
	}

	// the days of the window and ahead
	for day := from; day.Before(until); day = server.PartitionGranularityDaily.NextRangeStart(day) {
		r := server.PartitionGranularityDaily.RangeOf(day)
		if server.PartitionRangeCovered(leaves, r) {
			continue
		}
		statement := fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS public.%s PARTITION OF public.%s FOR VALUES FROM (%s) TO (%s)",
			pgx.Identifier{providerEgressHealthHistoryPartitionFamily.LeafName(providerEgressHealthHistoryTable, r)}.Sanitize(),
			pgx.Identifier{providerEgressHealthHistoryStagedTable}.Sanitize(),
			server.PartitionKeyTimestamp.BoundSql(r.Lower),
			server.PartitionKeyTimestamp.BoundSql(r.Upper),
		)
		if self.dryRun {
			self.logf("would create: %s", statement)
			continue
		}
		if err := providerEgressHealthHistoryTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds())))
			server.RaisePgResult(tx.Exec(ctx, statement))
		}); err != nil {
			return fmt.Errorf("create the partition of %s: %w", day.Format(time.DateOnly), err)
		}
	}

	var stagedColumns []providerEgressHealthHistoryColumn
	if stagedKind == "p" || !self.dryRun {
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			stagedColumns = readProviderEgressHealthHistoryColumns(ctx, conn, providerEgressHealthHistoryStagedTable)
		})
	} else {
		stagedColumns = liveColumns
	}
	if !slices.Equal(stagedColumns, liveColumns) {
		return fmt.Errorf("%s columns differ from %s; --abort and prepare again", providerEgressHealthHistoryStagedTable, providerEgressHealthHistoryTable)
	}

	// the change capture first, then the copy: every row committed before the
	// copy's snapshot is copied, and every later one is mirrored
	if err := self.installMirror(ctx, providerEgressHealthHistoryTable, providerEgressHealthHistoryMirror, providerEgressHealthHistoryStagedTable, liveColumns, from, until); err != nil {
		return fmt.Errorf("install the mirror: %w", err)
	}
	if stagedKind == "" && self.dryRun {
		self.logf("would copy measured_at from %s, by %s", from.Format(time.DateOnly), self.settings.CopyChunk)
	} else {
		copyUntil := self.now.Add(self.settings.CopyChunk)
		if until.Before(copyUntil) {
			copyUntil = until
		}
		copied := self.copyWindow(ctx, providerEgressHealthHistoryTable, providerEgressHealthHistoryStagedTable, liveColumns, from, copyUntil)
		self.logf("copied %d rows", copied)
	}
	if self.dryRun {
		self.logf("dry run: nothing changed")
	} else {
		self.logf("prepared; run --swap within %d days", self.settings.StagedAheadDays)
	}
	return nil
}

// The renames that give the staged table's not-null constraints the table's
// names.
func providerEgressHealthHistoryNotNullRenameSql(ctx context.Context, conn server.PgQuerier) (statements []string) {
	notNullNames := func(table string) map[string]string {
		names := map[string]string{}
		result, err := conn.Query(
			ctx,
			`
			SELECT a.attname, c.conname
			FROM pg_constraint c
			JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
			WHERE c.conrelid = to_regclass($1) AND c.contype = 'n'
			`,
			"public."+table,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var column, name string
				server.Raise(result.Scan(&column, &name))
				names[column] = name
			}
		})
		return names
	}
	live := notNullNames(providerEgressHealthHistoryTable)
	staged := notNullNames(providerEgressHealthHistoryStagedTable)
	columns := []string{}
	for column := range staged {
		columns = append(columns, column)
	}
	slices.Sort(columns)
	for _, column := range columns {
		if name, ok := live[column]; ok && name != staged[column] {
			statements = append(statements, fmt.Sprintf(
				"ALTER TABLE public.%s RENAME CONSTRAINT %s TO %s",
				pgx.Identifier{providerEgressHealthHistoryStagedTable}.Sanitize(),
				pgx.Identifier{staged[column]}.Sanitize(),
				pgx.Identifier{name}.Sanitize(),
			))
		}
	}
	return
}

// The renames of a table and its indexes from one name to another, each
// index keeping its suffix.
func providerEgressHealthHistoryRenameSql(indexes []providerEgressHealthHistoryIndex, fromTable string, toTable string) (statements []string, err error) {
	statements = append(statements, fmt.Sprintf(
		"ALTER TABLE public.%s RENAME TO %s",
		pgx.Identifier{fromTable}.Sanitize(),
		pgx.Identifier{toTable}.Sanitize(),
	))
	for _, index := range indexes {
		name, ok := providerEgressHealthHistoryIndexName(index.name, fromTable, toTable)
		if !ok || 63 < len(name) {
			return nil, fmt.Errorf("index %s of %s cannot be renamed for %s", index.name, fromTable, toTable)
		}
		if index.primary {
			// renames the constraint and its index
			statements = append(statements, fmt.Sprintf(
				"ALTER TABLE public.%s RENAME CONSTRAINT %s TO %s",
				pgx.Identifier{toTable}.Sanitize(),
				pgx.Identifier{index.name}.Sanitize(),
				pgx.Identifier{name}.Sanitize(),
			))
		} else {
			statements = append(statements, fmt.Sprintf(
				"ALTER INDEX public.%s RENAME TO %s",
				pgx.Identifier{index.name}.Sanitize(),
				pgx.Identifier{name}.Sanitize(),
			))
		}
	}
	return
}

// Under the transaction's locks, the days of [from, until) whose row counts
// differ between the two tables.
func providerEgressHealthHistoryDayDifferences(ctx context.Context, tx server.PgTx, a string, b string, from time.Time, until time.Time) []time.Time {
	day := 24 * time.Hour
	return providerEgressHealthHistoryCountDifferences(
		providerEgressHealthHistoryCounts(ctx, tx, a, from, until, day),
		providerEgressHealthHistoryCounts(ctx, tx, b, from, until, day),
	)
}

func providerEgressHealthHistoryDayList(days []time.Time) string {
	names := []string{}
	for _, day := range days {
		names = append(names, day.Format(time.DateOnly))
	}
	return strings.Join(names, " ")
}

func (self *providerEgressHealthHistoryCutover) swap(ctx context.Context) error {
	if liveKind := providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryTable); liveKind == "p" {
		self.logf("%s is already partitioned; nothing to swap", providerEgressHealthHistoryTable)
		return nil
	} else if liveKind != "r" {
		return fmt.Errorf("%s does not exist as a table (relkind %q)", providerEgressHealthHistoryTable, liveKind)
	}
	if providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryStagedTable) != "p" {
		return errors.New("not prepared: the staged table is missing; run prepare first")
	}
	if !providerEgressHealthHistoryTriggerExists(ctx, providerEgressHealthHistoryTable, providerEgressHealthHistoryMirror) {
		return errors.New("not prepared: the mirror is missing; run prepare again")
	}
	leaves, err := providerEgressHealthHistoryLeaves(ctx, providerEgressHealthHistoryStagedTable)
	if err != nil {
		return err
	}
	from, until, ok := providerEgressHealthHistoryLeafSpan(leaves)
	if !ok {
		return errors.New("not prepared: the staged table has no partitions")
	}
	// the registry keeps the days ahead from the swap on; the staged days must
	// reach that far, or the mirror's window ends before the swap
	need := server.PartitionGranularityDaily.AddRanges(server.PartitionGranularityDaily.RangeStart(self.now), providerEgressHealthHistoryPartitionFamily.AheadRanges+1)
	if until.Before(need) {
		return fmt.Errorf("the staged days end %s, before %s; run prepare again to extend them", until.Format(time.DateOnly), need.Format(time.DateOnly))
	}
	var liveColumns, stagedColumns []providerEgressHealthHistoryColumn
	var liveIndexes, stagedIndexes []providerEgressHealthHistoryIndex
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		liveColumns = readProviderEgressHealthHistoryColumns(ctx, conn, providerEgressHealthHistoryTable)
		stagedColumns = readProviderEgressHealthHistoryColumns(ctx, conn, providerEgressHealthHistoryStagedTable)
		liveIndexes = readProviderEgressHealthHistoryIndexes(ctx, conn, providerEgressHealthHistoryTable)
		stagedIndexes = readProviderEgressHealthHistoryIndexes(ctx, conn, providerEgressHealthHistoryStagedTable)
	})
	if !slices.Equal(liveColumns, stagedColumns) {
		return fmt.Errorf("%s columns differ from %s; --abort and prepare again", providerEgressHealthHistoryStagedTable, providerEgressHealthHistoryTable)
	}
	if len(liveIndexes) != len(stagedIndexes) {
		return fmt.Errorf("%s has %d indexes and %s %d; --abort and prepare again", providerEgressHealthHistoryTable, len(liveIndexes), providerEgressHealthHistoryStagedTable, len(stagedIndexes))
	}
	for _, index := range stagedIndexes {
		if !index.valid {
			return fmt.Errorf("staged index %s is invalid", index.name)
		}
	}

	// an early, unlocked look at the copy: a difference here would fail the
	// locked check after holding the lock
	var unlockedDiffer []time.Time
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.WithoutTransactionTimeout(ctx, conn, func() {
			day := 24 * time.Hour
			unlockedDiffer = providerEgressHealthHistoryCountDifferences(
				providerEgressHealthHistoryCounts(ctx, conn, providerEgressHealthHistoryTable, from, until, day),
				providerEgressHealthHistoryCounts(ctx, conn, providerEgressHealthHistoryStagedTable, from, until, day),
			)
		})
	})
	if 0 < len(unlockedDiffer) {
		self.logf("the copy differs on %s; rows inserted since the last count are mirrored, so the locked check decides", providerEgressHealthHistoryDayList(unlockedDiffer))
	}

	renames, err := providerEgressHealthHistoryRenameSql(liveIndexes, providerEgressHealthHistoryTable, providerEgressHealthHistoryOldTable)
	if err != nil {
		return err
	}
	stagedRenames, err := providerEgressHealthHistoryRenameSql(stagedIndexes, providerEgressHealthHistoryStagedTable, providerEgressHealthHistoryTable)
	if err != nil {
		return err
	}
	statements := []string{
		fmt.Sprintf("DROP TRIGGER %s ON public.%s", providerEgressHealthHistoryMirror, pgx.Identifier{providerEgressHealthHistoryTable}.Sanitize()),
		fmt.Sprintf("DROP FUNCTION %s()", providerEgressHealthHistoryMirror),
	}
	statements = append(statements, renames...)
	statements = append(statements, stagedRenames...)
	if self.dryRun {
		self.logf("would lock both tables, check every day from %s to %s, then:\n%s", from.Format(time.DateOnly), until.Format(time.DateOnly), strings.Join(statements, ";\n"))
		self.logf("dry run: nothing changed")
		return nil
	}
	swapStart := time.Now()
	err = self.retryLocked(ctx, []string{providerEgressHealthHistoryTable, providerEgressHealthHistoryStagedTable}, func() error {
		return providerEgressHealthHistoryTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds())))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.settings.SwapStatementTimeout.Milliseconds())))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
				"LOCK TABLE public.%s, public.%s IN ACCESS EXCLUSIVE MODE",
				pgx.Identifier{providerEgressHealthHistoryTable}.Sanitize(),
				pgx.Identifier{providerEgressHealthHistoryStagedTable}.Sanitize(),
			)))
			// the exact-copy check under the fence: the staged rows are copies
			// of the table's, so equal counts are equal rows
			if differ := providerEgressHealthHistoryDayDifferences(ctx, tx, providerEgressHealthHistoryTable, providerEgressHealthHistoryStagedTable, from, until); 0 < len(differ) {
				panic(fmt.Errorf("the copy differs on %s; run prepare again", providerEgressHealthHistoryDayList(differ)))
			}
			if self.swapLocked != nil {
				self.swapLocked()
			}
			for _, statement := range statements {
				server.RaisePgResult(tx.Exec(ctx, statement))
			}
		})
	})
	if err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	self.logf("swapped in %s: %s is partitioned; the old table is %s", time.Since(swapStart).Round(time.Millisecond), providerEgressHealthHistoryTable, providerEgressHealthHistoryOldTable)
	server.HandleError(func() {
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf("ANALYZE public.%s", pgx.Identifier{providerEgressHealthHistoryTable}.Sanitize())))
		})
	})
	self.logf("check it with --verify, then reclaim the old table with --drop-old; until then --rollback undoes the swap")
	return nil
}

// Why the partitioned table is not ready, or nil: its primary key, its
// secondary indexes beside the old table's, and its days ahead.
func (self *providerEgressHealthHistoryCutover) partitionedProblems(ctx context.Context) (problems []string) {
	if kind := providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryTable); kind != "p" {
		return []string{fmt.Sprintf("%s is not partitioned (relkind %q)", providerEgressHealthHistoryTable, kind)}
	}
	var indexes, oldIndexes []providerEgressHealthHistoryIndex
	var primaryKey string
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		indexes = readProviderEgressHealthHistoryIndexes(ctx, conn, providerEgressHealthHistoryTable)
		oldIndexes = readProviderEgressHealthHistoryIndexes(ctx, conn, providerEgressHealthHistoryOldTable)
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE((SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = to_regclass($1) AND contype = 'p' AND conname = $2), '')`,
			"public."+providerEgressHealthHistoryTable,
			providerEgressHealthHistoryTable+"_pkey",
		).Scan(&primaryKey))
	})
	if primaryKey != "PRIMARY KEY (run_id, measured_at)" {
		problems = append(problems, fmt.Sprintf("the primary key is %q", primaryKey))
	}
	for _, index := range indexes {
		if !index.valid {
			problems = append(problems, fmt.Sprintf("index %s is invalid", index.name))
		}
	}
	for _, oldIndex := range oldIndexes {
		if oldIndex.primary {
			continue
		}
		name, _ := providerEgressHealthHistoryIndexName(oldIndex.name, providerEgressHealthHistoryOldTable, providerEgressHealthHistoryTable)
		if !slices.ContainsFunc(indexes, func(index providerEgressHealthHistoryIndex) bool { return index.name == name }) {
			problems = append(problems, fmt.Sprintf("index %s is missing", name))
		}
	}
	leaves, err := providerEgressHealthHistoryLeaves(ctx, providerEgressHealthHistoryTable)
	if err != nil {
		return append(problems, err.Error())
	}
	if covered, future := providerEgressHealthHistoryPartitionFamily.Runway(leaves, self.now); !covered || future < providerEgressHealthHistoryPartitionFamily.AheadRanges {
		problems = append(problems, fmt.Sprintf("the partitions cover the current day %t and %d days after it", covered, future))
	}
	return
}

// The rows of the old table in the partitioned table's span that the
// partitioned table lacks.
func (self *providerEgressHealthHistoryCutover) missingRows(ctx context.Context) (missing int64, from time.Time, err error) {
	leaves, err := providerEgressHealthHistoryLeaves(ctx, providerEgressHealthHistoryTable)
	if err != nil {
		return 0, time.Time{}, err
	}
	from, _, ok := providerEgressHealthHistoryLeafSpan(leaves)
	if !ok {
		return 0, time.Time{}, errors.New("the partitioned table has no partitions")
	}
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.WithoutTransactionTimeout(ctx, conn, func() {
			server.Raise(conn.QueryRow(
				ctx,
				fmt.Sprintf(
					`
					SELECT count(*)
					FROM public.%s old
					WHERE $1 <= old.measured_at AND NOT EXISTS (
						SELECT 1 FROM public.%s partitioned
						WHERE partitioned.run_id = old.run_id AND partitioned.measured_at = old.measured_at
					)
					`,
					pgx.Identifier{providerEgressHealthHistoryOldTable}.Sanitize(),
					pgx.Identifier{providerEgressHealthHistoryTable}.Sanitize(),
				),
				from,
			).Scan(&missing))
		})
	})
	return
}

func (self *providerEgressHealthHistoryCutover) verify(ctx context.Context) error {
	problems := self.partitionedProblems(ctx)
	if providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryOldTable) == "r" {
		missing, from, err := self.missingRows(ctx)
		if err != nil {
			problems = append(problems, err.Error())
		} else if 0 < missing {
			problems = append(problems, fmt.Sprintf("%d rows of %s from %s are missing", missing, providerEgressHealthHistoryOldTable, from.Format(time.DateOnly)))
		} else {
			self.logf("every row of %s from %s is in %s", providerEgressHealthHistoryOldTable, from.Format(time.DateOnly), providerEgressHealthHistoryTable)
		}
	} else {
		self.logf("%s is gone; only the partitioned table was checked", providerEgressHealthHistoryOldTable)
	}
	if 0 < len(problems) {
		for _, problem := range problems {
			self.logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) found", len(problems))
	}
	self.logf("verified")
	return nil
}

func (self *providerEgressHealthHistoryCutover) rollback(ctx context.Context) error {
	if kind := providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryTable); kind != "p" {
		return fmt.Errorf("%s is not partitioned (relkind %q); before the swap use --abort", providerEgressHealthHistoryTable, kind)
	}
	if kind := providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryOldTable); kind != "r" {
		return fmt.Errorf("%s is gone; the swap can no longer be undone", providerEgressHealthHistoryOldTable)
	}
	if kind := providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryStagedTable); kind != "" {
		return fmt.Errorf("%s exists (relkind %s); the names it would take back are in use", providerEgressHealthHistoryStagedTable, kind)
	}
	leaves, err := providerEgressHealthHistoryLeaves(ctx, providerEgressHealthHistoryTable)
	if err != nil {
		return err
	}
	from, until, ok := providerEgressHealthHistoryLeafSpan(leaves)
	if !ok {
		return errors.New("the partitioned table has no partitions")
	}
	// the days on demand past the span, and every leaf's days, are counted
	for _, leaf := range leaves {
		if leaf.Canonical && !leaf.Range.Upper.IsZero() && until.Before(leaf.Range.Upper) {
			until = leaf.Range.Upper
		}
	}
	var columns, oldColumns []providerEgressHealthHistoryColumn
	var indexes, oldIndexes []providerEgressHealthHistoryIndex
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		columns = readProviderEgressHealthHistoryColumns(ctx, conn, providerEgressHealthHistoryTable)
		oldColumns = readProviderEgressHealthHistoryColumns(ctx, conn, providerEgressHealthHistoryOldTable)
		indexes = readProviderEgressHealthHistoryIndexes(ctx, conn, providerEgressHealthHistoryTable)
		oldIndexes = readProviderEgressHealthHistoryIndexes(ctx, conn, providerEgressHealthHistoryOldTable)
	})
	if !slices.Equal(columns, oldColumns) {
		return fmt.Errorf("%s columns differ from %s; rows cannot be copied back", providerEgressHealthHistoryOldTable, providerEgressHealthHistoryTable)
	}
	// rows written since the swap: mirror them, then copy back what is there
	if err := self.installMirror(ctx, providerEgressHealthHistoryTable, providerEgressHealthHistoryRollbackMirror, providerEgressHealthHistoryOldTable, columns, from, until); err != nil {
		return fmt.Errorf("install the rollback mirror: %w", err)
	}
	copyUntil := self.now.Add(self.settings.CopyChunk)
	if until.Before(copyUntil) {
		copyUntil = until
	}
	copied := self.copyWindow(ctx, providerEgressHealthHistoryTable, providerEgressHealthHistoryOldTable, columns, from, copyUntil)
	self.logf("copied back %d rows", copied)

	toStaged, err := providerEgressHealthHistoryRenameSql(indexes, providerEgressHealthHistoryTable, providerEgressHealthHistoryStagedTable)
	if err != nil {
		return err
	}
	toLive, err := providerEgressHealthHistoryRenameSql(oldIndexes, providerEgressHealthHistoryOldTable, providerEgressHealthHistoryTable)
	if err != nil {
		return err
	}
	statements := []string{
		fmt.Sprintf("DROP TRIGGER %s ON public.%s", providerEgressHealthHistoryRollbackMirror, pgx.Identifier{providerEgressHealthHistoryTable}.Sanitize()),
		fmt.Sprintf("DROP FUNCTION %s()", providerEgressHealthHistoryRollbackMirror),
	}
	statements = append(statements, toStaged...)
	statements = append(statements, toLive...)
	if self.dryRun {
		self.logf("would lock both tables, check every day from %s to %s, then:\n%s", from.Format(time.DateOnly), until.Format(time.DateOnly), strings.Join(statements, ";\n"))
		self.logf("dry run: nothing changed")
		return nil
	}
	err = self.retryLocked(ctx, []string{providerEgressHealthHistoryTable, providerEgressHealthHistoryOldTable}, func() error {
		return providerEgressHealthHistoryTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds())))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.settings.SwapStatementTimeout.Milliseconds())))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
				"LOCK TABLE public.%s, public.%s IN ACCESS EXCLUSIVE MODE",
				pgx.Identifier{providerEgressHealthHistoryTable}.Sanitize(),
				pgx.Identifier{providerEgressHealthHistoryOldTable}.Sanitize(),
			)))
			// every day the partitioned table holds is in the old table: its
			// rows before the swap were checked equal, and every one written
			// since was mirrored or copied back
			if differ := providerEgressHealthHistoryDayDifferences(ctx, tx, providerEgressHealthHistoryTable, providerEgressHealthHistoryOldTable, from, until); 0 < len(differ) {
				panic(fmt.Errorf("the copy back differs on %s; run --rollback again", providerEgressHealthHistoryDayList(differ)))
			}
			for _, statement := range statements {
				server.RaisePgResult(tx.Exec(ctx, statement))
			}
		})
	})
	if err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	self.logf("rolled back: %s is the plain table again; the partitioned copy is %s (prepare resumes it, --abort drops it)", providerEgressHealthHistoryTable, providerEgressHealthHistoryStagedTable)
	return nil
}

func (self *providerEgressHealthHistoryCutover) dropOld(ctx context.Context) error {
	if kind := providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryOldTable); kind == "" {
		self.logf("%s does not exist; nothing to drop", providerEgressHealthHistoryOldTable)
		return nil
	}
	if err := self.verify(ctx); err != nil {
		return fmt.Errorf("the old table stays until --verify passes: %w", err)
	}
	var size string
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT pg_size_pretty(pg_total_relation_size(to_regclass($1)))`, "public."+providerEgressHealthHistoryOldTable).Scan(&size))
	})
	statements := []string{
		// an unfinished rollback's mirror writes into the old table
		fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON public.%s", providerEgressHealthHistoryRollbackMirror, pgx.Identifier{providerEgressHealthHistoryTable}.Sanitize()),
		fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", providerEgressHealthHistoryRollbackMirror),
		fmt.Sprintf("DROP TABLE public.%s", pgx.Identifier{providerEgressHealthHistoryOldTable}.Sanitize()),
	}
	if self.dryRun {
		self.logf("would drop %s (%s):\n%s", providerEgressHealthHistoryOldTable, size, strings.Join(statements, ";\n"))
		return nil
	}
	err := self.retryLocked(ctx, []string{providerEgressHealthHistoryTable, providerEgressHealthHistoryOldTable}, func() error {
		return providerEgressHealthHistoryTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds())))
			for _, statement := range statements {
				server.RaisePgResult(tx.Exec(ctx, statement))
			}
		})
	})
	if err != nil {
		return fmt.Errorf("drop %s: %w", providerEgressHealthHistoryOldTable, err)
	}
	self.logf("dropped %s: %s returned", providerEgressHealthHistoryOldTable, size)
	return nil
}

func (self *providerEgressHealthHistoryCutover) abort(ctx context.Context) error {
	if kind := providerEgressHealthHistoryRelkind(ctx, providerEgressHealthHistoryTable); kind == "p" {
		return errors.New("the table is partitioned; use --rollback")
	}
	statements := []string{
		fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON public.%s", providerEgressHealthHistoryMirror, pgx.Identifier{providerEgressHealthHistoryTable}.Sanitize()),
		fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", providerEgressHealthHistoryMirror),
		fmt.Sprintf("DROP TABLE IF EXISTS public.%s", pgx.Identifier{providerEgressHealthHistoryStagedTable}.Sanitize()),
	}
	if self.dryRun {
		self.logf("would run:\n%s", strings.Join(statements, ";\n"))
		return nil
	}
	err := self.retryLocked(ctx, []string{providerEgressHealthHistoryTable}, func() error {
		return providerEgressHealthHistoryTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds())))
			for _, statement := range statements {
				server.RaisePgResult(tx.Exec(ctx, statement))
			}
		})
	})
	if err != nil {
		return fmt.Errorf("abort: %w", err)
	}
	self.logf("aborted: the mirror and %s are gone", providerEgressHealthHistoryStagedTable)
	return nil
}
