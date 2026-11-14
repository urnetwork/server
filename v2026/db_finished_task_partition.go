// The zero-copy cutover of finished_task to daily partitions by run end time
// (PARTITION-MAINTENANCE-PLAN.md C3), and the settings it shares with the
// table's partition family, which the task package registers beside the Post
// retries it fences.
//
// The table becomes the legacy partition, finished_task_legacy, of a new
// parent partitioned by run_end_time. A task that finishes from the cutover
// day C on goes to its day's partition, finished_task_pYYYYMMDD.
//
//   - prepare (the default step): stage the empty parent finished_task_staged
//     with every index of the table and the days from C through AheadDays
//     ahead, then add and validate CHECK (run_end_time < C). The validation
//     scans under SHARE UPDATE EXCLUSIVE, so completions continue. Resumable:
//     completed steps are skipped.
//   - finalize: prove in a rolled-back transaction that every parent index
//     adopts a legacy index, then swap in one transaction (catalog work only,
//     no copy). The validated check proves the legacy bound, so ATTACH skips
//     its scan.
//   - rollback: undo the swap in one transaction, copying every row of the
//     daily partitions back into the legacy table, so the writes made after
//     the swap stay. Until C every completion still lands in the legacy
//     partition, and there is nothing to copy.
//   - abort: before finalize, drop the staged parent and the check.
//
// The prepared check refuses an end time from C on, so finalize must run
// before C. TaskCleanup abandons a prepare still unfinalized
// CutoverAbandonMargin before C, and finalize refuses from then on.
//
// Nothing is copied at the swap, so no task's row is written twice: a finished
// task's identity stays its single fenced move from pending_task
// (task/task_run_once.go).
package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"
)

// The settings of finished_task's daily partitions and of their cutover.
type FinishedTaskPartitionSettings struct {
	// Days after the current one that always have a partition: the registry's
	// runway creates them, and prepare stages them after the cutover day. A
	// completion whose end time has no partition fails, and its task runs
	// again.
	AheadDays int
	// A closed day is analyzed once this long after it ends, when the last
	// completions of the day and its Post acknowledgements have landed.
	LeafAnalyzeDelay time.Duration
	// The lock wait of a day's create, which takes SHARE UPDATE EXCLUSIVE on
	// the parent and so never blocks completions.
	CreateLockTimeout time.Duration
	// The lock wait of each exclusive lock of a drop and of the cutover's
	// steps. An ACCESS EXCLUSIVE request queues every later completion behind
	// it, so the wait stays short; a busy run retries later.
	ExclusiveLockTimeout time.Duration
	// Bounds each statement of a drop and of the swap, catalog work only, so a
	// statement that would scan or build is cut short.
	CatalogStatementTimeout time.Duration
	// A drop or an exclusive cutover step defers while a transaction older
	// than this holds a lock on finished_task or one of its partitions, so
	// its lock request never queues completions behind that transaction.
	MaxTransactionAge time.Duration
	// Failed Posts released per owner-admitted group before a day drops: the
	// candidates of one admission statement.
	ReleaseGroupSize int
	// Prepare accepts a cutover day at least this far ahead, leaving time for
	// the validation and the swap.
	MinCutoverLead time.Duration
	// A prepare not finalized this long before its cutover day is abandoned
	// (TaskCleanup), since its check would refuse every completion from the
	// cutover day on; finalize refuses from then on.
	CutoverAbandonMargin time.Duration
	// Attempts of an exclusive cutover step whose locks are busy, and the
	// wait between them.
	CutoverAttempts      int
	CutoverRetryInterval time.Duration
	// The lock wait of the check's validation. It takes SHARE UPDATE EXCLUSIVE
	// and so never blocks completions.
	ValidateLockTimeout time.Duration
	// Rollback copies the daily partitions' rows back under its exclusive
	// lock, which stalls completions while it runs: more rows than this
	// refuse the rollback.
	RollbackMaxCopyRows int64
	// Bounds the rollback's copy.
	RollbackCopyTimeout time.Duration
}

func DefaultFinishedTaskPartitionSettings() *FinishedTaskPartitionSettings {
	return &FinishedTaskPartitionSettings{
		AheadDays:               3,
		LeafAnalyzeDelay:        time.Hour,
		CreateLockTimeout:       5 * time.Second,
		ExclusiveLockTimeout:    2 * time.Second,
		CatalogStatementTimeout: 30 * time.Second,
		MaxTransactionAge:       5 * time.Second,
		ReleaseGroupSize:        64,
		MinCutoverLead:          12 * time.Hour,
		CutoverAbandonMargin:    2 * time.Hour,
		CutoverAttempts:         60,
		CutoverRetryInterval:    10 * time.Second,
		ValidateLockTimeout:     60 * time.Second,
		// about 1.5 hours of completions at 36 a second
		RollbackMaxCopyRows: 200_000,
		RollbackCopyTimeout: 2 * time.Minute,
	}
}

type FinishedTaskPartitionMigrateOptions struct {
	// The day, 00:00 UTC, from which finished tasks go to daily partitions
	// (prepare only).
	CutoverDay time.Time
	DryRun     bool
	Finalize   bool
	Rollback   bool
	Abort      bool
	Logf       func(string, ...any)
	// nil uses DefaultFinishedTaskPartitionSettings.
	Settings *FinishedTaskPartitionSettings
}

const finishedTaskPartitionTable = "finished_task"

// The parent a prepared cutover stages; finalize renames it finished_task.
const finishedTaskStagedParentName = "finished_task_staged"

// The legacy bound check, under the name readContractTableShape recognizes.
var finishedTaskLegacyBoundName = contractLegacyBoundName(finishedTaskPartitionTable)

const finishedTaskAttachProbeName = "finished_task_attach_probe"

// The primary key the migrations give the table: run_end_time is the
// partition key, which a partitioned table's unique keys must include.
const finishedTaskPrimaryKeyDefinition = "PRIMARY KEY (task_id, run_end_time)"

// The index of failed Posts, which the drop's fence reads.
const FinishedTaskPostRetryIndexName = "finished_task_post_retry"

// The migrations of the partitioning, which run on the plain table before any
// cutover (their reasons are at their place in the migration list). The
// accessors below find each one's version by identity: versions are
// positional, so they follow the migrations wherever the list places them.
var finishedTaskIdentityIndexMigration = newRestartableOnlineSqlMigration(`
		DROP INDEX CONCURRENTLY IF EXISTS finished_task_task_id_run_end_time
	`, `
		CREATE UNIQUE INDEX CONCURRENTLY finished_task_task_id_run_end_time
		ON finished_task (task_id, run_end_time)
	`, `
		DROP INDEX IF EXISTS finished_task_task_id_run_end_time;
		CREATE UNIQUE INDEX finished_task_task_id_run_end_time
		ON finished_task (task_id, run_end_time)
	`)

var finishedTaskPrimaryKeyMigration = newSqlMigration(`
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE finished_task
			DROP CONSTRAINT finished_task_pkey,
			ADD CONSTRAINT finished_task_pkey PRIMARY KEY USING INDEX finished_task_task_id_run_end_time;
	`)

var finishedTaskPostRetryIndexMigration = newRestartableOnlineSqlMigration(`
		DROP INDEX CONCURRENTLY IF EXISTS finished_task_post_retry
	`, `
		CREATE INDEX CONCURRENTLY finished_task_post_retry
		ON finished_task (task_id)
		WHERE post_error IS NOT NULL AND NOT post_completed
	`, `
		DROP INDEX IF EXISTS finished_task_post_retry;
		CREATE INDEX finished_task_post_retry
		ON finished_task (task_id)
		WHERE post_error IS NOT NULL AND NOT post_completed
	`)

// The version of the migration that builds the unique (task_id,
// run_end_time) index online.
func FinishedTaskIdentityIndexMigrationVersion() int {
	return finishedTaskMigrationVersion(finishedTaskIdentityIndexMigration)
}

// The version of the migration that moves the primary key onto that index,
// which renames the index to finished_task_pkey.
func FinishedTaskPrimaryKeyMigrationVersion() int {
	return finishedTaskMigrationVersion(finishedTaskPrimaryKeyMigration)
}

// The version of the migration that builds the failed Post index online.
func FinishedTaskPostRetryIndexMigrationVersion() int {
	return finishedTaskMigrationVersion(finishedTaskPostRetryIndexMigration)
}

// The version a database reaches once migration has applied: its position in
// the migration list plus one.
func finishedTaskMigrationVersion(migration any) int {
	for i, m := range migrations {
		if m == migration {
			return i + 1
		}
	}
	panic(fmt.Errorf("%T is not in the migration list", migration))
}

// The days of finished_task as the registry names and bounds them. It is
// never registered: the task package registers the family with its fence.
var finishedTaskDays = &PartitionFamily{
	Name:        finishedTaskPartitionTable,
	Tables:      []string{finishedTaskPartitionTable},
	KeyColumn:   "run_end_time",
	KeyKind:     PartitionKeyTimestamp,
	Granularity: PartitionGranularityDaily,
	AheadRanges: 1,
}

var finishedTaskLegacyBoundPattern = regexp.MustCompile(`^CHECK \(\(run_end_time < '[^']*'::timestamp without time zone\)\)( NOT VALID)?$`)

func finishedTaskPartitionLogf(format string, args ...any) {
	glog.Infof("[partitions]finished_task: "+format+"\n", args...)
}

// Whether finished_task is partitioned yet.
func IsFinishedTaskPartitioned(ctx context.Context) (partitioned bool) {
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE((SELECT relkind = 'p' FROM pg_class WHERE oid = to_regclass('public.finished_task')), false)`,
		).Scan(&partitioned))
	})
	return
}

func MigrateFinishedTasksToPartitions(ctx context.Context, options FinishedTaskPartitionMigrateOptions) error {
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	settings := options.Settings
	if settings == nil {
		settings = DefaultFinishedTaskPartitionSettings()
	}
	actions := 0
	for _, action := range []bool{options.Finalize, options.Rollback, options.Abort} {
		if action {
			actions += 1
		}
	}
	if 1 < actions {
		return errors.New("choose one of finalize, rollback and abort")
	}
	switch {
	case options.Finalize:
		return finalizeFinishedTaskCutover(ctx, true, options.DryRun, logf, settings)
	case options.Rollback:
		return rollbackFinishedTaskCutover(ctx, options.DryRun, logf, settings)
	case options.Abort:
		return abortFinishedTaskCutover(ctx, options.DryRun, logf, settings, settings.CutoverAttempts)
	default:
		if options.CutoverDay.IsZero() {
			return errors.New("the cutover day is required to prepare")
		}
		if !NowUtc().Before(options.CutoverDay.Add(-settings.CutoverAbandonMargin)) {
			return fmt.Errorf("the cutover day %s is less than %s away, where a prepare is abandoned; prepare a later day", options.CutoverDay.UTC().Format(time.DateOnly), settings.CutoverAbandonMargin)
		}
		return prepareFinishedTaskCutover(ctx, options.CutoverDay, true, options.DryRun, logf, settings)
	}
}

// Transactions older than minTransactionAge, other than conn's own, that hold
// a lock on finished_task, the staged parent of a prepared cutover, or one of
// their partitions: an exclusive request on the table would queue every later
// completion behind them.
func FinishedTaskLongLockHolders(ctx context.Context, conn PgQuerier, minTransactionAge time.Duration) (holders int) {
	Raise(conn.QueryRow(
		ctx,
		`
		WITH parent AS (
			SELECT oid FROM pg_class
			WHERE oid IN (to_regclass('public.finished_task'), to_regclass('public.finished_task_staged'))
		)
		SELECT count(DISTINCT activity.pid)
		FROM pg_locks lock
		JOIN pg_stat_activity activity ON activity.pid = lock.pid
		WHERE lock.locktype = 'relation'
			AND lock.database = (SELECT oid FROM pg_database WHERE datname = current_database())
			AND lock.relation IN (
				SELECT oid FROM parent
				UNION ALL
				SELECT inhrelid FROM pg_inherits WHERE inhparent IN (SELECT oid FROM parent)
			)
			AND activity.pid <> pg_backend_pid()
			AND activity.xact_start < now() - make_interval(secs => $1)
		`,
		minTransactionAge.Seconds(),
	).Scan(&holders))
	return
}

// The prepared legacy bound: its cutover day and whether it is validated.
func readFinishedTaskLegacyBound(ctx context.Context, conn PgQuerier) (cutoverDay time.Time, validated bool, exists bool, err error) {
	var definition string
	var bound *string
	result, queryErr := conn.Query(
		ctx,
		`
		SELECT
			pg_get_constraintdef(oid),
			convalidated,
			to_char(((regexp_match(pg_get_constraintdef(oid), '''([^'']*)''::timestamp'))[1])::timestamp, 'YYYY-MM-DD HH24:MI:SS.US')
		FROM pg_constraint
		WHERE conrelid = to_regclass('public.finished_task') AND conname = $1 AND contype = 'c'
		`,
		finishedTaskLegacyBoundName,
	)
	WithPgResult(result, queryErr, func() {
		if result.Next() {
			Raise(result.Scan(&definition, &validated, &bound))
			exists = true
		}
	})
	if !exists {
		return
	}
	// the bound is cast back in the reading session, so it parses whatever
	// the session's date style
	if !finishedTaskLegacyBoundPattern.MatchString(definition) || bound == nil {
		err = fmt.Errorf("finished_task has an unexpected %s: %s", finishedTaskLegacyBoundName, definition)
		return
	}
	cutoverDay, _, ok := PartitionKeyTimestamp.ParseBound(*bound)
	if !ok || !PartitionGranularityDaily.RangeStart(cutoverDay).Equal(cutoverDay) {
		err = fmt.Errorf("finished_task has an unexpected %s: %s", finishedTaskLegacyBoundName, definition)
	}
	return
}

// Reasons finished_task cannot become the legacy partition as it is.
func finishedTaskCutoverProblems(shape *contractTableShape) (problems []string) {
	if shape.relkind != "r" {
		problems = append(problems, fmt.Sprintf("%s is not a plain table (relkind %s)", shape.table, shape.relkind))
	}
	key := slices.IndexFunc(shape.columns, func(column contractColumn) bool {
		return column.name == "run_end_time"
	})
	if key < 0 || shape.columns[key].typeName != "timestamp without time zone" || !shape.columns[key].notNull {
		problems = append(problems, fmt.Sprintf("%s.run_end_time is not a NOT NULL timestamp", shape.table))
	}
	for _, check := range shape.checks {
		if check.noInherit {
			problems = append(problems, fmt.Sprintf("%s check %s is NO INHERIT", shape.table, check.name))
		}
	}
	primary := 0
	postRetry := false
	for _, index := range shape.indexes {
		if !index.valid || !index.ready {
			problems = append(problems, fmt.Sprintf("%s index %s is invalid; drop it (DROP INDEX CONCURRENTLY) or rebuild it before the cutover", shape.table, index.name))
			continue
		}
		if pgMaxIdentifierLength < len(index.name)+max(len(contractStagedIndexSuffix), len(contractLegacyIndexSuffix)) {
			problems = append(problems, fmt.Sprintf("%s index name %s is too long to stage", shape.table, index.name))
		}
		switch {
		case index.primary:
			primary += 1
			if index.constraintDefinition != finishedTaskPrimaryKeyDefinition {
				problems = append(problems, fmt.Sprintf("%s primary key is %s, not %s; apply the database migrations first", shape.table, index.constraintDefinition, finishedTaskPrimaryKeyDefinition))
			}
		case index.unique:
			problems = append(problems, fmt.Sprintf("%s unique index %s is not the primary key", shape.table, index.name))
		default:
			if _, ok := contractRetargetIndex(index.definition, index.name, shape.table); !ok {
				problems = append(problems, fmt.Sprintf("%s index %s has an unexpected definition", shape.table, index.name))
			}
		}
		if index.name == FinishedTaskPostRetryIndexName {
			postRetry = true
		}
	}
	if primary != 1 {
		problems = append(problems, fmt.Sprintf("%s has %d primary keys", shape.table, primary))
	}
	if !postRetry {
		problems = append(problems, fmt.Sprintf("%s has no index %s, which the drop's fence reads; apply the database migrations first", shape.table, FinishedTaskPostRetryIndexName))
	}
	// the table has none: neither the staged parent nor its days would carry
	// one, so the cutover refuses rather than lose it
	for _, trigger := range shape.triggers {
		problems = append(problems, fmt.Sprintf("%s has trigger %s, which the cutover does not carry", shape.table, trigger.name))
	}
	for _, foreignKey := range append(slices.Clone(shape.foreignKeys), shape.referencedBy...) {
		problems = append(problems, fmt.Sprintf("%s has foreign key %s, which the cutover does not carry", shape.table, foreignKey.name))
	}
	return
}

// The statements that stage the partitioned parent. LIKE ... INCLUDING INDEXES
// clones each index from its stored expression trees, so ATTACH adopts the
// legacy table's indexes (see the contract cutover); checks are added after,
// with their validity.
func finishedTaskStagedParentSql(shape *contractTableShape) (statements []string) {
	statements = append(statements, fmt.Sprintf(
		"CREATE TABLE public.%s (LIKE public.%s INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING INDEXES INCLUDING STORAGE INCLUDING COMPRESSION INCLUDING COMMENTS) PARTITION BY RANGE (run_end_time)",
		quoteIdentifier(finishedTaskStagedParentName),
		quoteIdentifier(shape.table),
	))
	for _, check := range shape.checks {
		statements = append(statements, fmt.Sprintf(
			"ALTER TABLE public.%s ADD CONSTRAINT %s %s",
			quoteIdentifier(finishedTaskStagedParentName),
			quoteIdentifier(check.name),
			check.definition,
		))
	}
	for _, column := range shape.columns {
		if 0 <= column.statisticsTarget {
			statements = append(statements, fmt.Sprintf(
				"ALTER TABLE public.%s ALTER COLUMN %s SET STATISTICS %d",
				quoteIdentifier(finishedTaskStagedParentName),
				quoteIdentifier(column.name),
				column.statisticsTarget,
			))
		}
	}
	return
}

// Stages the parent in one transaction, its indexes under their published
// names plus the staged suffix.
func stageFinishedTaskParent(ctx context.Context, shape *contractTableShape, settings *FinishedTaskPartitionSettings) error {
	return contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.CreateLockTimeout.Milliseconds())))
		for _, statement := range finishedTaskStagedParentSql(shape) {
			RaisePgResult(tx.Exec(ctx, statement))
		}
		staged := readContractTableShape(ctx, tx, finishedTaskStagedParentName)
		statements, err := contractStagedRenameSql(shape, staged)
		Raise(err)
		for _, statement := range statements {
			RaisePgResult(tx.Exec(ctx, statement))
		}
	})
}

// The statements that create day's partition under the staged parent, as the
// registry's runway creates one under the published parent.
func finishedTaskStagedDaySql(r PartitionRange) []string {
	leaf := finishedTaskDays.LeafName(finishedTaskPartitionTable, r)
	return []string{
		fmt.Sprintf(
			"CREATE TABLE public.%s (LIKE public.%s INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING STORAGE INCLUDING COMPRESSION)",
			quoteIdentifier(leaf),
			quoteIdentifier(finishedTaskStagedParentName),
		),
		fmt.Sprintf(
			"ALTER TABLE public.%s ATTACH PARTITION public.%s FOR VALUES FROM (%s) TO (%s)",
			quoteIdentifier(finishedTaskStagedParentName),
			quoteIdentifier(leaf),
			PartitionKeyTimestamp.BoundSql(r.Lower),
			PartitionKeyTimestamp.BoundSql(r.Upper),
		),
	}
}

// The days prepare stages: from the cutover day through AheadDays after the
// later of it and today, so a cutover day already past (only a test makes
// one) still covers today.
func finishedTaskStagedDays(cutoverDay time.Time, now time.Time, aheadDays int) (days []PartitionRange) {
	last := PartitionGranularityDaily.RangeStart(now)
	if last.Before(cutoverDay) {
		last = cutoverDay
	}
	last = PartitionGranularityDaily.AddRanges(last, aheadDays)
	for day := cutoverDay; !last.Before(day); day = PartitionGranularityDaily.NextRangeStart(day) {
		days = append(days, PartitionGranularityDaily.RangeOf(day))
	}
	return
}

func (self *FinishedTaskPartitionSettings) lockTimeoutSql() string {
	return fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.ExclusiveLockTimeout.Milliseconds())
}

func (self *FinishedTaskPartitionSettings) catalogStatementTimeoutSql() string {
	return fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.CatalogStatementTimeout.Milliseconds())
}

// Runs a cutover step until it succeeds, fails for a reason other than a busy
// lock, or runs out of attempts. Each attempt repeats the whole step, so it
// reads current state after taking its locks. A quiet step, one that takes
// ACCESS EXCLUSIVE, first waits for the table to be free of long
// transactions, so its lock request never queues completions behind one.
func finishedTaskCutoverRetry(ctx context.Context, settings *FinishedTaskPartitionSettings, attempts int, quiet bool, run func() error, logf func(string, ...any)) (err error) {
	for attempt := 1; attempt <= attempts; attempt += 1 {
		holders := 0
		if quiet {
			err = partitionRegistryRun(func() {
				MaintenanceDb(ctx, func(conn PgConn) {
					holders = FinishedTaskLongLockHolders(ctx, conn, settings.MaxTransactionAge)
				})
			})
			if err != nil {
				return err
			}
		}
		if 0 < holders {
			err = fmt.Errorf("%d transaction(s) older than %s hold finished_task locks", holders, settings.MaxTransactionAge)
			logf("%s (attempt %d/%d)", err, attempt, attempts)
		} else {
			err = run()
			if err == nil || !contractPartitionLockBusy(err) {
				return
			}
			logf("lock busy (attempt %d/%d): %v", attempt, attempts, err)
		}
		if attempt < attempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(settings.CutoverRetryInterval):
			}
		}
	}
	return
}

// Runs callback in one read-committed maintenance transaction on a session
// exempt from transaction_timeout, for a step that can run long, and returns
// its failure.
func finishedTaskLongTx(ctx context.Context, callback func(PgTx)) error {
	return partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			WithoutTransactionTimeout(ctx, conn, func() {
				TxInConn(ctx, conn, callback, TxReadCommitted, OptNoRetry())
			})
		}, OptNoRetry())
	})
}

// An operator's new prepare needs the cutover day MinCutoverLead ahead; one
// that resumes a prepare of the same day does not. A test cuts over at a day
// already past.
func prepareFinishedTaskCutover(ctx context.Context, cutoverDay time.Time, operator bool, dryRun bool, logf func(string, ...any), settings *FinishedTaskPartitionSettings) error {
	if IsFinishedTaskPartitioned(ctx) {
		logf("finished_task is already partitioned; nothing to prepare")
		return nil
	}
	cutoverDay = cutoverDay.UTC()
	if !PartitionGranularityDaily.RangeStart(cutoverDay).Equal(cutoverDay) {
		return fmt.Errorf("the cutover day must start at 00:00 UTC: %s", cutoverDay)
	}
	logf("cutover day %s: tasks that finish before it stay in %s", cutoverDay.Format(time.DateOnly), PartitionLegacyLeafName(finishedTaskPartitionTable))
	logf("finalize must complete before %s; an unfinished prepare is then abandoned", cutoverDay.Add(-settings.CutoverAbandonMargin).Format(time.RFC3339))

	var shape *contractTableShape
	var staged *contractTableShape
	var boundDay time.Time
	var boundValidated bool
	var boundExists bool
	var boundErr error
	MaintenanceDb(ctx, func(conn PgConn) {
		shape = readContractTableShape(ctx, conn, finishedTaskPartitionTable)
		staged = readContractTableShape(ctx, conn, finishedTaskStagedParentName)
		boundDay, boundValidated, boundExists, boundErr = readFinishedTaskLegacyBound(ctx, conn)
	})
	var problems []string
	if shape == nil {
		problems = append(problems, "finished_task does not exist")
	} else {
		problems = append(problems, finishedTaskCutoverProblems(shape)...)
		if boundErr != nil {
			problems = append(problems, boundErr.Error())
		} else if boundExists && !boundDay.Equal(cutoverDay) {
			problems = append(problems, fmt.Sprintf("finished_task is prepared for the cutover day %s; abort it first", boundDay.Format(time.DateOnly)))
		} else if operator && !boundExists && cutoverDay.Before(NowUtc().Add(settings.MinCutoverLead)) {
			problems = append(problems, fmt.Sprintf("the cutover day %s must start at least %s from now, to leave time for the validation and the swap", cutoverDay.Format(time.DateOnly), settings.MinCutoverLead))
		}
		if staged != nil {
			problems = append(problems, contractStagedProblems(shape, staged)...)
		}
	}
	if 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the cutover", len(problems))
	}

	if staged == nil {
		if dryRun {
			logf("would stage %s, then give its indexes their published names plus %q:\n%s", finishedTaskStagedParentName, contractStagedIndexSuffix, strings.Join(finishedTaskStagedParentSql(shape), ";\n"))
		} else {
			if err := stageFinishedTaskParent(ctx, shape, settings); err != nil {
				return fmt.Errorf("stage %s: %w", finishedTaskStagedParentName, err)
			}
			logf("staged %s with %d indexes", finishedTaskStagedParentName, len(shape.indexes))
		}
	}

	var leaves []PartitionLeaf
	var listErr error
	MaintenanceDb(ctx, func(conn PgConn) {
		leaves, listErr = finishedTaskDays.ListLeaves(ctx, conn, finishedTaskStagedParentName)
	})
	if listErr != nil {
		return listErr
	}
	for _, day := range finishedTaskStagedDays(cutoverDay, NowUtc(), settings.AheadDays) {
		if slices.ContainsFunc(leaves, func(leaf PartitionLeaf) bool { return leaf.Range.Overlaps(day) }) {
			continue
		}
		name := finishedTaskDays.LeafName(finishedTaskPartitionTable, day)
		statements := finishedTaskStagedDaySql(day)
		if dryRun {
			logf("would create %s:\n%s", name, strings.Join(statements, ";\n"))
			continue
		}
		if err := contractPartitionExec(ctx, append([]string{fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.CreateLockTimeout.Milliseconds())}, statements...)); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		logf("created %s", name)
	}

	if !boundExists {
		statement := fmt.Sprintf(
			"ALTER TABLE public.finished_task ADD CONSTRAINT %s CHECK (run_end_time < %s) NOT VALID",
			quoteIdentifier(finishedTaskLegacyBoundName),
			PartitionKeyTimestamp.BoundSql(cutoverDay),
		)
		if dryRun {
			logf("would add %s: %s", finishedTaskLegacyBoundName, statement)
		} else {
			// a brief ACCESS EXCLUSIVE lock, without a scan
			err := finishedTaskCutoverRetry(ctx, settings, settings.CutoverAttempts, true, func() error {
				return contractPartitionExec(ctx, []string{settings.lockTimeoutSql(), settings.catalogStatementTimeoutSql(), statement})
			}, logf)
			if err != nil {
				return fmt.Errorf("add %s: %w", finishedTaskLegacyBoundName, err)
			}
			logf("added %s", finishedTaskLegacyBoundName)
		}
	}
	if !boundValidated {
		if dryRun {
			logf("would validate %s (scans finished_task under SHARE UPDATE EXCLUSIVE)", finishedTaskLegacyBoundName)
		} else {
			logf("validating %s: scans finished_task under SHARE UPDATE EXCLUSIVE; completions continue", finishedTaskLegacyBoundName)
			validateStart := time.Now()
			// SHARE UPDATE EXCLUSIVE never blocks completions, so the step
			// does not wait for a quiet table
			err := finishedTaskCutoverRetry(ctx, settings, settings.CutoverAttempts, false, func() error {
				return validateFinishedTaskLegacyBound(ctx, settings)
			}, logf)
			if err != nil {
				return fmt.Errorf("validate %s: %w", finishedTaskLegacyBoundName, err)
			}
			logf("validated %s in %s", finishedTaskLegacyBoundName, time.Since(validateStart).Round(time.Second))
		}
	}
	if dryRun {
		logf("dry run: nothing changed")
	} else {
		logf("prepared; run --finalize before %s", cutoverDay.Add(-settings.CutoverAbandonMargin).Format(time.RFC3339))
	}
	return nil
}

// Validates the legacy bound: one scan of the table under SHARE UPDATE
// EXCLUSIVE, which may run past transaction_timeout, so its session runs
// without it.
func validateFinishedTaskLegacyBound(ctx context.Context, settings *FinishedTaskPartitionSettings) error {
	return partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			WithoutTransactionTimeout(ctx, conn, func() {
				TxInConn(ctx, conn, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.ValidateLockTimeout.Milliseconds())))
					RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = 0`))
					RaisePgResult(tx.Exec(ctx, fmt.Sprintf("ALTER TABLE public.finished_task VALIDATE CONSTRAINT %s", quoteIdentifier(finishedTaskLegacyBoundName))))
				}, TxReadCommitted, OptNoRetry())
			})
		}, OptNoRetry())
	})
}

// The swap's ATTACH must adopt a valid legacy index for every parent index; a
// parent index without a match is built on the whole table under the swap's
// lock. An empty clone of the table has the same index trees (LIKE), so attach
// one to the staged parent on the day just below the cutover day, in a
// transaction that is rolled back, and check that every parent index adopted
// one of the clone's indexes instead of building its own.
func probeFinishedTaskLegacyAttach(ctx context.Context, cutoverDay time.Time) (problems []string) {
	errProbed := errors.New("probed")
	err := contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
		RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = '60s'`))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			"CREATE TABLE public.%s (LIKE public.finished_task INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING INDEXES)",
			quoteIdentifier(finishedTaskAttachProbeName),
		)))
		cloned := map[string]bool{}
		for _, index := range readContractTableShape(ctx, tx, finishedTaskAttachProbeName).indexes {
			cloned[index.name] = true
		}
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			"ALTER TABLE public.%s ATTACH PARTITION public.%s FOR VALUES FROM (%s) TO (%s)",
			quoteIdentifier(finishedTaskStagedParentName),
			quoteIdentifier(finishedTaskAttachProbeName),
			PartitionKeyTimestamp.BoundSql(PartitionGranularityDaily.AddRanges(cutoverDay, -1)),
			PartitionKeyTimestamp.BoundSql(cutoverDay),
		)))
		result, err := tx.Query(
			ctx,
			`
			SELECT parent_index.relname, COALESCE(child_index.relname, '')
			FROM pg_index parent
			JOIN pg_class parent_index ON parent_index.oid = parent.indexrelid
			LEFT JOIN pg_inherits i ON i.inhparent = parent.indexrelid
				AND i.inhrelid IN (SELECT indexrelid FROM pg_index WHERE indrelid = to_regclass($2))
			LEFT JOIN pg_class child_index ON child_index.oid = i.inhrelid
			WHERE parent.indrelid = to_regclass($1)
			ORDER BY parent_index.relname
			`,
			"public."+finishedTaskStagedParentName,
			"public."+finishedTaskAttachProbeName,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var parentIndex, childIndex string
				Raise(result.Scan(&parentIndex, &childIndex))
				if !cloned[childIndex] {
					problems = append(problems, fmt.Sprintf("%s index %s would be built on finished_task at the swap instead of adopting a legacy index", finishedTaskStagedParentName, parentIndex))
				}
			}
		})
		panic(errProbed)
	})
	if !errors.Is(err, errProbed) {
		problems = append(problems, fmt.Sprintf("attach probe of finished_task: %v", err))
	}
	return
}

// The swap, after its locks: the table and its indexes move to the legacy
// names, the table attaches below the cutover day, and the staged parent and
// its indexes take the published names.
func finishedTaskSwapSql(shape *contractTableShape, staged *contractTableShape, cutoverDay time.Time) (statements []string) {
	legacy := PartitionLegacyLeafName(finishedTaskPartitionTable)
	statements = append(statements, fmt.Sprintf("ALTER TABLE public.finished_task RENAME TO %s", quoteIdentifier(legacy)))
	for _, index := range shape.indexes {
		statements = append(statements, fmt.Sprintf(
			"ALTER INDEX public.%s RENAME TO %s",
			quoteIdentifier(index.name),
			quoteIdentifier(index.name+contractLegacyIndexSuffix),
		))
	}
	statements = append(statements, fmt.Sprintf(
		"ALTER TABLE public.%s ATTACH PARTITION public.%s FOR VALUES FROM (MINVALUE) TO (%s)",
		quoteIdentifier(finishedTaskStagedParentName),
		quoteIdentifier(legacy),
		PartitionKeyTimestamp.BoundSql(cutoverDay),
	))
	statements = append(statements, fmt.Sprintf("ALTER TABLE public.%s RENAME TO finished_task", quoteIdentifier(finishedTaskStagedParentName)))
	for _, index := range staged.indexes {
		name, _ := strings.CutSuffix(index.name, contractStagedIndexSuffix)
		statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(index.name), quoteIdentifier(name)))
	}
	return
}

// The locks the swap takes first: the table and the staged parent with its
// days, so the renames and the attach see no concurrent statement.
func finishedTaskSwapLockSql(settings *FinishedTaskPartitionSettings) []string {
	return []string{
		settings.lockTimeoutSql(),
		settings.catalogStatementTimeoutSql(),
		fmt.Sprintf("LOCK TABLE public.finished_task, public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(finishedTaskStagedParentName)),
	}
}

func finalizeFinishedTaskCutover(ctx context.Context, deadline bool, dryRun bool, logf func(string, ...any), settings *FinishedTaskPartitionSettings) error {
	if IsFinishedTaskPartitioned(ctx) {
		logf("finished_task is already partitioned")
		if dryRun {
			return nil
		}
		// a swap that committed before its analyze ran
		return analyzeFinishedTaskParent(ctx, logf)
	}
	var shape *contractTableShape
	var staged *contractTableShape
	var cutoverDay time.Time
	var boundValidated bool
	var boundExists bool
	var boundErr error
	var leaves []PartitionLeaf
	var listErr error
	MaintenanceDb(ctx, func(conn PgConn) {
		shape = readContractTableShape(ctx, conn, finishedTaskPartitionTable)
		staged = readContractTableShape(ctx, conn, finishedTaskStagedParentName)
		cutoverDay, boundValidated, boundExists, boundErr = readFinishedTaskLegacyBound(ctx, conn)
		leaves, listErr = finishedTaskDays.ListLeaves(ctx, conn, finishedTaskStagedParentName)
	})
	var problems []string
	var syncStatements []string
	switch {
	case shape == nil:
		problems = append(problems, "finished_task does not exist")
	case boundErr != nil:
		problems = append(problems, boundErr.Error())
	case !boundExists || !boundValidated:
		problems = append(problems, "finished_task is not prepared: its legacy bound is missing or not validated")
	case staged == nil:
		problems = append(problems, fmt.Sprintf("%s is not staged", finishedTaskStagedParentName))
	case listErr != nil:
		problems = append(problems, listErr.Error())
	default:
		problems = append(problems, finishedTaskCutoverProblems(shape)...)
		problems = append(problems, contractStagedProblems(shape, staged)...)
		statements, indexProblems := contractStagedIndexSyncSql(shape, staged)
		problems = append(problems, indexProblems...)
		syncStatements = statements
		if !slices.ContainsFunc(leaves, func(leaf PartitionLeaf) bool {
			return leaf.Range.Covers(PartitionGranularityDaily.RangeOf(cutoverDay))
		}) {
			problems = append(problems, fmt.Sprintf("%s has no partition for the cutover day", finishedTaskStagedParentName))
		}
	}
	if 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the swap", len(problems))
	}
	if deadline && !NowUtc().Before(cutoverDay.Add(-settings.CutoverAbandonMargin)) {
		return fmt.Errorf("too close to the cutover day %s; abort and prepare a later day", cutoverDay.Format(time.DateOnly))
	}

	if 0 < len(syncStatements) {
		if dryRun {
			logf("would sync staged indexes:\n%s", strings.Join(syncStatements, ";\n"))
		} else {
			if err := contractPartitionExec(ctx, append([]string{`SET LOCAL lock_timeout = '5s'`}, syncStatements...)); err != nil {
				return fmt.Errorf("sync staged indexes: %w", err)
			}
			logf("synced %d staged index change(s)", len(syncStatements))
			MaintenanceDb(ctx, func(conn PgConn) {
				staged = readContractTableShape(ctx, conn, finishedTaskStagedParentName)
			})
		}
	}

	if problems := probeFinishedTaskLegacyAttach(ctx, cutoverDay); 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the swap", len(problems))
	}
	logf("probed the swap: every parent index adopts a legacy index")

	if dryRun {
		statements := append(finishedTaskSwapLockSql(settings), finishedTaskSwapSql(shape, staged, cutoverDay)...)
		logf("would swap in one transaction, rechecking the prepared bound under its locks:\n%s", strings.Join(statements, ";\n"))
		logf("dry run: nothing changed")
		return nil
	}
	swapStart := time.Now()
	err := finishedTaskCutoverRetry(ctx, settings, settings.CutoverAttempts, true, func() error {
		return contractPartitionTx(ctx, func(tx PgTx) {
			for _, statement := range finishedTaskSwapLockSql(settings) {
				RaisePgResult(tx.Exec(ctx, statement))
			}
			// read committed reads these after the locks: an abandon or abort
			// that committed while the swap waited leaves the table without
			// its bound, and its ATTACH would scan the table under the lock
			current := readContractTableShape(ctx, tx, finishedTaskPartitionTable)
			currentStaged := readContractTableShape(ctx, tx, finishedTaskStagedParentName)
			day, validated, exists, boundErr := readFinishedTaskLegacyBound(ctx, tx)
			Raise(boundErr)
			if current == nil || current.relkind != "r" || currentStaged == nil || !exists || !validated || !day.Equal(cutoverDay) {
				Raise(errors.New("the prepared cutover changed while the swap waited for its locks"))
			}
			for _, statement := range finishedTaskSwapSql(current, currentStaged, cutoverDay) {
				RaisePgResult(tx.Exec(ctx, statement))
			}
		})
	}, logf)
	if err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	logf("swapped finished_task in %s; tasks that finish from %s go to daily partitions", time.Since(swapStart).Round(time.Millisecond), cutoverDay.Format(time.DateOnly))
	return analyzeFinishedTaskParent(ctx, logf)
}

// The parent starts without statistics, and autovacuum never analyzes a
// partitioned parent. The sample reads every partition, so the session runs
// without transaction_timeout.
func analyzeFinishedTaskParent(ctx context.Context, logf func(string, ...any)) error {
	err := partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			WithoutTransactionTimeout(ctx, conn, func() {
				TxInConn(ctx, conn, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = 0`))
					RaisePgResult(tx.Exec(ctx, `ANALYZE ONLY public.finished_task`))
				}, TxReadCommitted, OptNoRetry())
			})
		}, OptNoRetry())
	})
	if err != nil {
		return fmt.Errorf("analyze finished_task: %w", err)
	}
	logf("analyzed the parent")
	return nil
}

// The rollback plan, read through conn after its lock: the statements, and
// the rows the copy moves back from the daily partitions. It refuses a
// partitioned table it did not make, and more rows than the copy may take.
func finishedTaskRollbackPlanInConn(ctx context.Context, conn PgQuerier, settings *FinishedTaskPartitionSettings) (statements []string, copyRows int64, err error) {
	legacyName := PartitionLegacyLeafName(finishedTaskPartitionTable)
	leaves, err := finishedTaskDays.ListLeaves(ctx, conn, finishedTaskPartitionTable)
	if err != nil {
		return nil, 0, err
	}
	legacy := readContractTableShape(ctx, conn, legacyName)
	legacyAttached := slices.ContainsFunc(leaves, func(leaf PartitionLeaf) bool {
		return leaf.Name == legacyName && leaf.Range.Legacy
	})
	if legacy == nil || !legacyAttached {
		return nil, 0, fmt.Errorf("finished_task has no legacy partition %s; it was not partitioned by this cutover", legacyName)
	}
	for _, leaf := range leaves {
		if leaf.Name == legacyName {
			continue
		}
		var rows int64
		// counts at most one row past the limit
		Raise(conn.QueryRow(ctx, fmt.Sprintf(
			`SELECT count(*) FROM (SELECT 1 FROM ONLY public.%s LIMIT $1) bounded`,
			quoteIdentifier(leaf.Name),
		), settings.RollbackMaxCopyRows+1-copyRows).Scan(&rows))
		copyRows += rows
		if settings.RollbackMaxCopyRows < copyRows {
			return nil, copyRows, fmt.Errorf("the daily partitions hold more than %d rows, which the rollback copies back under its exclusive lock; raise the limit to roll back anyway", settings.RollbackMaxCopyRows)
		}
	}
	columns := []string{}
	for _, column := range legacy.columns {
		if column.generated == "" {
			columns = append(columns, quoteIdentifier(column.name))
		}
	}
	statements = append(statements,
		fmt.Sprintf("ALTER TABLE public.finished_task DETACH PARTITION public.%s", quoteIdentifier(legacyName)),
		fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT IF EXISTS %s", quoteIdentifier(legacyName), quoteIdentifier(finishedTaskLegacyBoundName)),
	)
	if 0 < copyRows {
		statements = append(statements, finishedTaskRollbackCopySql(columns))
	}
	statements = append(statements,
		`DROP TABLE public.finished_task`,
		fmt.Sprintf("ALTER TABLE public.%s RENAME TO finished_task", quoteIdentifier(legacyName)),
	)
	for _, index := range legacy.indexes {
		if name, ok := strings.CutSuffix(index.name, contractLegacyIndexSuffix); ok {
			statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(index.name), quoteIdentifier(name)))
		}
	}
	return statements, copyRows, nil
}

// With the legacy table detached, whether a task id of the daily partitions,
// which the parent alone holds, is also in the legacy table. Each probe reads
// the legacy primary key, which leads with the task id.
const finishedTaskRollbackIdentitySql = `SELECT EXISTS (
	SELECT 1 FROM public.finished_task daily
	WHERE EXISTS (SELECT 1 FROM ONLY public.finished_task_legacy legacy_row WHERE legacy_row.task_id = daily.task_id)
)`

// With the legacy table detached, the parent holds only the daily partitions.
func finishedTaskRollbackCopySql(columns []string) string {
	list := strings.Join(columns, ", ")
	return fmt.Sprintf(
		"INSERT INTO public.%s (%s) SELECT %s FROM public.finished_task",
		quoteIdentifier(PartitionLegacyLeafName(finishedTaskPartitionTable)),
		list,
		list,
	)
}

func finishedTaskRollbackLockSql(settings *FinishedTaskPartitionSettings) []string {
	return []string{
		settings.lockTimeoutSql(),
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.RollbackCopyTimeout.Milliseconds()),
		`LOCK TABLE public.finished_task IN ACCESS EXCLUSIVE MODE`,
	}
}

// Locks the parent and its partitions before reading the inventory, so the
// rows counted are the rows copied; the copy, the checks and every drop share
// one transaction, and a retry repeats it with fresh reads.
func rollbackFinishedTaskCutover(ctx context.Context, dryRun bool, logf func(string, ...any), settings *FinishedTaskPartitionSettings) error {
	if !IsFinishedTaskPartitioned(ctx) {
		logf("finished_task is not partitioned; nothing to roll back")
		return nil
	}
	if dryRun {
		var statements []string
		var copyRows int64
		var planErr error
		MaintenanceDb(ctx, func(conn PgConn) {
			statements, copyRows, planErr = finishedTaskRollbackPlanInConn(ctx, conn, settings)
		})
		if planErr != nil {
			return planErr
		}
		logf("would lock finished_task, recheck its partitions and copy back %d row(s) of the daily partitions, in one transaction:\n%s", copyRows, strings.Join(append(finishedTaskRollbackLockSql(settings), statements...), ";\n"))
		logf("dry run: nothing changed")
		return nil
	}
	var copied int64
	err := finishedTaskCutoverRetry(ctx, settings, settings.CutoverAttempts, true, func() error {
		return finishedTaskLongTx(ctx, func(tx PgTx) {
			copied = 0
			for _, statement := range finishedTaskRollbackLockSql(settings) {
				RaisePgResult(tx.Exec(ctx, statement))
			}
			statements, copyRows, err := finishedTaskRollbackPlanInConn(ctx, tx, settings)
			Raise(err)
			for _, statement := range statements {
				if strings.HasPrefix(statement, "INSERT INTO ") {
					// the legacy table's key holds the run end time, so it
					// would take a second row of one task; the move refused
					// one on every partition, and the copy checks it again
					var duplicate bool
					Raise(tx.QueryRow(ctx, finishedTaskRollbackIdentitySql).Scan(&duplicate))
					if duplicate {
						Raise(errors.New("a task id of the daily partitions is also in the legacy partition; the rollback would hold two rows of one task"))
					}
				}
				tag := RaisePgResult(tx.Exec(ctx, statement))
				if strings.HasPrefix(statement, "INSERT INTO ") {
					// the exact copy: every row the count saw under the lock
					if tag.RowsAffected() != copyRows {
						Raise(fmt.Errorf("the rollback copied %d row(s) of the %d its locked count saw", tag.RowsAffected(), copyRows))
					}
					copied = copyRows
				}
			}
		})
	}, logf)
	if err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	logf("rolled back: finished_task is a plain table again, with the %d row(s) written to the daily partitions since the cutover day", copied)
	return nil
}

func abortFinishedTaskCutover(ctx context.Context, dryRun bool, logf func(string, ...any), settings *FinishedTaskPartitionSettings, attempts int) error {
	if IsFinishedTaskPartitioned(ctx) {
		return errors.New("finished_task is partitioned; use rollback")
	}
	statements := []string{
		fmt.Sprintf("DROP TABLE IF EXISTS public.%s", quoteIdentifier(finishedTaskStagedParentName)),
		fmt.Sprintf("ALTER TABLE public.finished_task DROP CONSTRAINT IF EXISTS %s", quoteIdentifier(finishedTaskLegacyBoundName)),
	}
	if dryRun {
		logf("would run:\n%s", strings.Join(statements, ";\n"))
		return nil
	}
	err := finishedTaskCutoverRetry(ctx, settings, attempts, true, func() error {
		return contractPartitionExec(ctx, append([]string{settings.lockTimeoutSql(), settings.catalogStatementTimeoutSql()}, statements...))
	}, logf)
	if err != nil {
		return fmt.Errorf("abort: %w", err)
	}
	logf("aborted the finished_task cutover")
	return nil
}

// Abandons a prepared cutover once its cutover day is less than
// CutoverAbandonMargin away: its check would refuse every completion from that
// day on. TaskCleanup calls it on every run while the table is plain, one
// attempt a run, so a busy table retries on the next run. Returns whether it
// abandoned one; a failure is returned, never raised.
func GuardFinishedTaskPartitionCutover(ctx context.Context, now time.Time, settings *FinishedTaskPartitionSettings) (abandoned bool, err error) {
	var cutoverDay time.Time
	var exists bool
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			var partitioned bool
			Raise(conn.QueryRow(
				ctx,
				`SELECT COALESCE((SELECT relkind = 'p' FROM pg_class WHERE oid = to_regclass('public.finished_task')), false)`,
			).Scan(&partitioned))
			if partitioned {
				return
			}
			var boundErr error
			cutoverDay, _, exists, boundErr = readFinishedTaskLegacyBound(ctx, conn)
			Raise(boundErr)
		})
	})
	if err != nil || !exists || now.Before(cutoverDay.Add(-settings.CutoverAbandonMargin)) {
		return false, err
	}
	glog.Errorf(
		"[partitions]finished_task: abandoning the cutover prepared for %s: it was not finalized by %s, and its legacy bound would refuse every completion from that day\n",
		cutoverDay.Format(time.DateOnly),
		cutoverDay.Add(-settings.CutoverAbandonMargin).Format(time.RFC3339),
	)
	err = partitionRegistryRun(func() {
		Raise(abortFinishedTaskCutover(ctx, false, finishedTaskPartitionLogf, settings, 1))
	})
	return err == nil, err
}

// While a cutover is prepared, the staged parent with its indexes and the
// legacy bound check are its own work in progress, not drift; finalize or
// abort removes them. Once partitioned, the parent carries the migrations'
// columns, keys and indexes, and its partitions are not audited.
func (d *SchemaDiff) acceptFinishedTaskPartitionCutover() {
	if d.actual.partitioned[finishedTaskStagedParentName] && !d.expected.tables[finishedTaskStagedParentName] {
		d.extraTables = slices.DeleteFunc(d.extraTables, func(table string) bool {
			return table == finishedTaskStagedParentName
		})
		d.extraIdx = slices.DeleteFunc(d.extraIdx, func(name string) bool {
			return d.actual.indexes[name].table == finishedTaskStagedParentName
		})
	}
	extra := slices.DeleteFunc(d.extraConstraints[finishedTaskPartitionTable], func(name string) bool {
		return name == finishedTaskLegacyBoundName
	})
	if len(extra) == 0 {
		delete(d.extraConstraints, finishedTaskPartitionTable)
	} else {
		d.extraConstraints[finishedTaskPartitionTable] = extra
	}
}

// URNETWORK_TEST_FINISHED_TASK_PARTITIONS=1 runs every test on a partitioned
// finished_task, cut over at TestingFinishedTaskCutoverDay.
func testFinishedTaskPartitions() bool {
	switch strings.ToLower(os.Getenv("URNETWORK_TEST_FINISHED_TASK_PARTITIONS")) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// Ten days back: history a test writes in the last ten days lands in its
// day's partition, older history in the legacy partition.
func TestingFinishedTaskCutoverDay(now time.Time) time.Time {
	return PartitionGranularityDaily.AddRanges(PartitionGranularityDaily.RangeStart(now), -10)
}

// Cuts a freshly migrated test database's finished_task over at cutoverDay,
// which may be past: the days from it through the days ahead of today are
// staged, so a test can write history into any of them.
func Testing_PartitionFinishedTasks(ctx context.Context, cutoverDay time.Time) {
	if IsFinishedTaskPartitioned(ctx) {
		return
	}
	settings := DefaultFinishedTaskPartitionSettings()
	logf := func(string, ...any) {}
	Raise(prepareFinishedTaskCutover(ctx, cutoverDay, false, false, logf, settings))
	Raise(finalizeFinishedTaskCutover(ctx, false, false, logf, settings))
}
