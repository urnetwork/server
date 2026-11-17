// audit_device_event's daily partitions by event_time, kept 7 days
// (PARTITION-CANDIDATES.md §7; owner 2026-11-16), and the zero-copy cutover
// that makes them, on the finished_task cutover's steps
// (db_finished_task_partition.go). The public stats export reads the device day
// rollup (model/audit_device_day.go) instead of every retained event, so the
// raw events need only a few days.
//
// The table becomes the legacy partition, audit_device_event_legacy, of every
// event before the cutover day C; events from C on go to daily partitions,
// audit_device_event_pYYYYMMDD. Nothing is copied.
//
//   - prepare (the default step): refuse unless the device day rollup is
//     complete for the export's whole window (the legacy partition drops 7
//     days after C, and the export can no longer replay the raw events then);
//     drop the redundant UNIQUE (event_id), whose index nothing reads (event
//     ids are server.NewId(), and no statement uses ON CONFLICT on them), under
//     a short ACCESS EXCLUSIVE lock; stage the empty parent
//     audit_device_event_staged with the table's indexes and the days C through
//     AheadDays ahead; add and validate CHECK (event_time < C). The validation
//     scans under SHARE UPDATE EXCLUSIVE, so the sweep's inserts continue.
//     Resumable: completed steps are skipped.
//   - finalize: prove in a rolled-back transaction that every parent index
//     adopts a legacy index, then swap in one transaction (catalog work only).
//     The validated check proves the legacy bound, so ATTACH skips its scan.
//   - rollback: undo the swap in one transaction, copying the daily
//     partitions' rows back into the legacy table, up to RollbackMaxCopyRows:
//     about 18M events land a day, so a rollback belongs in the first hours or
//     not at all. The dropped unique index stays dropped.
//   - abort: before finalize, drop the staged parent and the check. The
//     dropped unique index stays dropped.
//
// The prepared check refuses an event from C on, so finalize must run before
// C; the runway pass abandons a prepare still unfinalized CutoverAbandonMargin
// before C (abandonStaleAuditDeviceEventCutover), and finalize refuses from
// then on.
//
// Once partitioned, the registry creates days ahead, analyzes the parent and
// each closed day, and drops a day, the legacy partition included, once its
// upper bound is AuditDeviceEventRetention past. Inserts take the sweep's
// clock, so every insert finds today's day; there is no DEFAULT partition.
// The hourly row delete stops (model.RemoveOldAuditDeviceEvents), and the
// REINDEX rotation skips the family.
package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog"
)

// The raw device events' retention once partitioned: the device day rollup
// holds the published series, and a week covers an outage of the sweep or the
// export, and incident review.
const AuditDeviceEventRetention = 7 * 24 * time.Hour

// The settings of audit_device_event's cutover.
type AuditDeviceEventPartitionSettings struct {
	// Days after the cutover day, or today when later, that prepare stages.
	AheadDays int
	// The lock wait of a day's create, which takes SHARE UPDATE EXCLUSIVE on
	// the parent and so never blocks the sweep.
	CreateLockTimeout time.Duration
	// The lock wait of each exclusive step: the unique index's drop, the
	// check's add, the swap, rollback and abort. An ACCESS EXCLUSIVE request
	// queues the sweep's inserts behind it, so the wait stays short; a busy run
	// retries.
	ExclusiveLockTimeout time.Duration
	// Bounds each catalog statement of an exclusive step.
	CatalogStatementTimeout time.Duration
	// An exclusive step defers while a transaction older than this holds a
	// lock on the table or its partitions.
	MaxTransactionAge time.Duration
	// Prepare accepts a cutover day at least this far ahead.
	MinCutoverLead time.Duration
	// A prepare not finalized this long before its cutover day is abandoned,
	// and finalize refuses from then on.
	CutoverAbandonMargin time.Duration
	// Attempts of an exclusive step whose locks are busy, and the wait between
	// them.
	CutoverAttempts      int
	CutoverRetryInterval time.Duration
	// The lock wait of the check's validation, which takes SHARE UPDATE
	// EXCLUSIVE.
	ValidateLockTimeout time.Duration
	// Rollback copies the daily partitions' rows back under its exclusive lock,
	// which stalls the sweep while it runs: more rows than this refuse it.
	RollbackMaxCopyRows int64
	// Bounds the rollback's copy.
	RollbackCopyTimeout time.Duration
	// Prepare refuses unless the device day rollup has a complete row for each
	// of this many days before today: the export's window.
	RollupDays int
}

func DefaultAuditDeviceEventPartitionSettings() *AuditDeviceEventPartitionSettings {
	return &AuditDeviceEventPartitionSettings{
		AheadDays:               3,
		CreateLockTimeout:       5 * time.Second,
		ExclusiveLockTimeout:    2 * time.Second,
		CatalogStatementTimeout: 30 * time.Second,
		MaxTransactionAge:       5 * time.Second,
		MinCutoverLead:          12 * time.Hour,
		CutoverAbandonMargin:    2 * time.Hour,
		CutoverAttempts:         60,
		CutoverRetryInterval:    10 * time.Second,
		ValidateLockTimeout:     60 * time.Second,
		// about an hour and a half of events
		RollbackMaxCopyRows: 1_000_000,
		RollbackCopyTimeout: 2 * time.Minute,
		RollupDays:          90,
	}
}

type AuditDeviceEventPartitionMigrateOptions struct {
	// The day, 00:00 UTC, from which events go to daily partitions (prepare
	// only).
	CutoverDay time.Time
	DryRun     bool
	Finalize   bool
	Rollback   bool
	Abort      bool
	Logf       func(string, ...any)
	// nil uses DefaultAuditDeviceEventPartitionSettings.
	Settings *AuditDeviceEventPartitionSettings
}

const auditDeviceEventTable = "audit_device_event"

// The parent a prepared cutover stages; finalize renames it
// audit_device_event.
const auditDeviceEventStagedParentName = "audit_device_event_staged"

// The legacy bound check, under the name readContractTableShape recognizes.
var auditDeviceEventLegacyBoundName = contractLegacyBoundName(auditDeviceEventTable)

const auditDeviceEventAttachProbeName = "audit_device_event_attach_probe"

var auditDeviceEventLegacyBoundPattern = regexp.MustCompile(`^CHECK \(\(event_time < '[^']*'::timestamp without time zone\)\)( NOT VALID)?$`)

var auditDeviceEventPartitionFamily = RegisterPartitionFamily(&PartitionFamily{
	Name:        auditDeviceEventTable,
	Description: "device audit events: daily event_time partitions kept 7 days; the stats export reads the device day rollup (PARTITION-CANDIDATES.md §7)",
	Tables:      []string{auditDeviceEventTable},
	KeyColumn:   "event_time",
	KeyKind:     PartitionKeyTimestamp,
	Granularity: PartitionGranularityDaily,
	// the sweep writes at its own clock, so the runway only has to stay ahead
	// of now
	AheadRanges: 3,
	Retention:   AuditDeviceEventRetention,
	// a closed day takes no more inserts, so one analyze settles it
	LeafAnalyzeDelay: time.Hour,
	ReindexExempt:    true,
	BeforeCutover:    abandonStaleAuditDeviceEventCutover,
	CutoverTables:    []string{auditDeviceEventStagedParentName},
})

// The days of audit_device_event as the registry names and bounds them.
var auditDeviceEventDays = auditDeviceEventPartitionFamily

func auditDeviceEventPartitionLogf(format string, args ...any) {
	glog.Infof("[partitions]audit_device_event: "+format+"\n", args...)
}

// Whether audit_device_event is partitioned yet.
func IsAuditDeviceEventPartitioned(ctx context.Context) (partitioned bool) {
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE((SELECT relkind = 'p' FROM pg_class WHERE oid = to_regclass('public.audit_device_event')), false)`,
		).Scan(&partitioned))
	})
	return
}

func MigrateAuditDeviceEventsToPartitions(ctx context.Context, options AuditDeviceEventPartitionMigrateOptions) error {
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	settings := options.Settings
	if settings == nil {
		settings = DefaultAuditDeviceEventPartitionSettings()
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
		return finalizeAuditDeviceEventCutover(ctx, true, options.DryRun, logf, settings)
	case options.Rollback:
		return rollbackAuditDeviceEventCutover(ctx, options.DryRun, logf, settings)
	case options.Abort:
		return abortAuditDeviceEventCutover(ctx, options.DryRun, logf, settings, settings.CutoverAttempts)
	default:
		if options.CutoverDay.IsZero() {
			return errors.New("the cutover day is required to prepare")
		}
		if !NowUtc().Before(options.CutoverDay.Add(-settings.CutoverAbandonMargin)) {
			return fmt.Errorf("the cutover day %s is less than %s away, where a prepare is abandoned; prepare a later day", options.CutoverDay.UTC().Format(time.DateOnly), settings.CutoverAbandonMargin)
		}
		return prepareAuditDeviceEventCutover(ctx, options.CutoverDay, true, options.DryRun, logf, settings)
	}
}

// Transactions older than minTransactionAge, other than conn's own, that hold
// a lock on audit_device_event, the staged parent of a prepared cutover, or
// one of their partitions.
func auditDeviceEventLongLockHolders(ctx context.Context, conn PgQuerier, minTransactionAge time.Duration) (holders int) {
	Raise(conn.QueryRow(
		ctx,
		`
		WITH parent AS (
			SELECT oid FROM pg_class
			WHERE oid IN (to_regclass('public.audit_device_event'), to_regclass('public.audit_device_event_staged'))
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
func readAuditDeviceEventLegacyBound(ctx context.Context, conn PgQuerier) (cutoverDay time.Time, validated bool, exists bool, err error) {
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
		WHERE conrelid = to_regclass('public.audit_device_event') AND conname = $1 AND contype = 'c'
		`,
		auditDeviceEventLegacyBoundName,
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
	if !auditDeviceEventLegacyBoundPattern.MatchString(definition) || bound == nil {
		err = fmt.Errorf("audit_device_event has an unexpected %s: %s", auditDeviceEventLegacyBoundName, definition)
		return
	}
	cutoverDay, _, ok := PartitionKeyTimestamp.ParseBound(*bound)
	if !ok || !PartitionGranularityDaily.RangeStart(cutoverDay).Equal(cutoverDay) {
		err = fmt.Errorf("audit_device_event has an unexpected %s: %s", auditDeviceEventLegacyBoundName, definition)
	}
	return
}

// The UNIQUE (event_id) constraint, which prepare drops, or "" once it is gone.
func auditDeviceEventUniqueEventIdConstraint(shape *contractTableShape) string {
	for _, index := range shape.indexes {
		if index.unique && !index.primary && index.constraintDefinition == "UNIQUE (event_id)" {
			return index.constraint
		}
	}
	return ""
}

// Reasons audit_device_event cannot become the legacy partition as it is. The
// unique event id constraint is not one: prepare drops it first.
func auditDeviceEventCutoverProblems(shape *contractTableShape) (problems []string) {
	if shape.relkind != "r" {
		problems = append(problems, fmt.Sprintf("%s is not a plain table (relkind %s)", shape.table, shape.relkind))
	}
	key := slices.IndexFunc(shape.columns, func(column contractColumn) bool {
		return column.name == "event_time"
	})
	if key < 0 || shape.columns[key].typeName != "timestamp without time zone" || !shape.columns[key].notNull {
		problems = append(problems, fmt.Sprintf("%s.event_time is not a NOT NULL timestamp", shape.table))
	}
	for _, check := range shape.checks {
		if check.noInherit {
			problems = append(problems, fmt.Sprintf("%s check %s is NO INHERIT", shape.table, check.name))
		}
	}
	uniqueEventId := auditDeviceEventUniqueEventIdConstraint(shape)
	primary := 0
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
			if index.constraintDefinition != "PRIMARY KEY (event_time, device_id, event_id)" {
				problems = append(problems, fmt.Sprintf("%s primary key is %s, not (event_time, device_id, event_id)", shape.table, index.constraintDefinition))
			}
		case index.unique && index.constraint == uniqueEventId && uniqueEventId != "":
			// dropped by prepare
		case index.unique:
			problems = append(problems, fmt.Sprintf("%s unique index %s is not the primary key", shape.table, index.name))
		default:
			if _, ok := contractRetargetIndex(index.definition, index.name, shape.table); !ok {
				problems = append(problems, fmt.Sprintf("%s index %s has an unexpected definition", shape.table, index.name))
			}
		}
	}
	if primary != 1 {
		problems = append(problems, fmt.Sprintf("%s has %d primary keys", shape.table, primary))
	}
	for _, trigger := range shape.triggers {
		problems = append(problems, fmt.Sprintf("%s has trigger %s, which the cutover does not carry", shape.table, trigger.name))
	}
	for _, foreignKey := range append(slices.Clone(shape.foreignKeys), shape.referencedBy...) {
		problems = append(problems, fmt.Sprintf("%s has foreign key %s, which the cutover does not carry", shape.table, foreignKey.name))
	}
	return
}

// The days before today, from RollupDays back, that the device day rollup
// lacks a complete row for. The export needs them all once the raw events are
// partitioned away.
//
// The window's day has a name of its own: inside the NOT EXISTS an
// unqualified day is audit_device_day.day, which would compare the rollup's
// column with itself and hide every lacking day behind any complete row.
func auditDeviceEventRollupLacking(ctx context.Context, conn PgQuerier, settings *AuditDeviceEventPartitionSettings) (lacking []string, err error) {
	var migrated bool
	Raise(conn.QueryRow(ctx, `SELECT to_regclass('public.audit_device_day') IS NOT NULL`).Scan(&migrated))
	if !migrated {
		return nil, errors.New("audit_device_day does not exist; apply the database migrations and deploy the taskworker that keeps it first")
	}
	result, queryErr := conn.Query(
		ctx,
		`
		SELECT to_char(window_days.window_day, 'YYYY-MM-DD')
		FROM (
			SELECT (now() AT TIME ZONE 'UTC')::date - days_back AS window_day
			FROM generate_series(1, $1::integer) AS days_back
		) window_days
		WHERE NOT EXISTS (
			SELECT 1 FROM audit_device_day
			WHERE audit_device_day.day = window_days.window_day AND audit_device_day.complete
		)
		ORDER BY 1
		`,
		settings.RollupDays,
	)
	WithPgResult(result, queryErr, func() {
		for result.Next() {
			var day string
			Raise(result.Scan(&day))
			lacking = append(lacking, day)
		}
	})
	return
}

// The statements that stage the partitioned parent. LIKE ... INCLUDING INDEXES
// clones each index from its stored expression trees, so ATTACH adopts the
// legacy table's indexes; checks are added after, with their validity.
func auditDeviceEventStagedParentSql(shape *contractTableShape) (statements []string) {
	statements = append(statements, fmt.Sprintf(
		"CREATE TABLE public.%s (LIKE public.%s INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING INDEXES INCLUDING STORAGE INCLUDING COMPRESSION INCLUDING COMMENTS) PARTITION BY RANGE (event_time)",
		quoteIdentifier(auditDeviceEventStagedParentName),
		quoteIdentifier(shape.table),
	))
	for _, check := range shape.checks {
		if check.name == auditDeviceEventLegacyBoundName {
			continue
		}
		statements = append(statements, fmt.Sprintf(
			"ALTER TABLE public.%s ADD CONSTRAINT %s %s",
			quoteIdentifier(auditDeviceEventStagedParentName),
			quoteIdentifier(check.name),
			check.definition,
		))
	}
	for _, column := range shape.columns {
		if 0 <= column.statisticsTarget {
			statements = append(statements, fmt.Sprintf(
				"ALTER TABLE public.%s ALTER COLUMN %s SET STATISTICS %d",
				quoteIdentifier(auditDeviceEventStagedParentName),
				quoteIdentifier(column.name),
				column.statisticsTarget,
			))
		}
	}
	return
}

// Stages the parent in one transaction, its indexes under their published
// names plus the staged suffix.
func stageAuditDeviceEventParent(ctx context.Context, shape *contractTableShape, settings *AuditDeviceEventPartitionSettings) error {
	return contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.CreateLockTimeout.Milliseconds())))
		for _, statement := range auditDeviceEventStagedParentSql(shape) {
			RaisePgResult(tx.Exec(ctx, statement))
		}
		staged := readContractTableShape(ctx, tx, auditDeviceEventStagedParentName)
		statements, err := contractStagedRenameSql(shape, staged)
		Raise(err)
		for _, statement := range statements {
			RaisePgResult(tx.Exec(ctx, statement))
		}
	})
}

// The statements that create day's partition under the staged parent, as the
// registry's runway creates one under the published parent.
func auditDeviceEventStagedDaySql(r PartitionRange) []string {
	leaf := auditDeviceEventDays.LeafName(auditDeviceEventTable, r)
	return []string{
		fmt.Sprintf(
			"CREATE TABLE public.%s (LIKE public.%s INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING STORAGE INCLUDING COMPRESSION)",
			quoteIdentifier(leaf),
			quoteIdentifier(auditDeviceEventStagedParentName),
		),
		fmt.Sprintf(
			"ALTER TABLE public.%s ATTACH PARTITION public.%s FOR VALUES FROM (%s) TO (%s)",
			quoteIdentifier(auditDeviceEventStagedParentName),
			quoteIdentifier(leaf),
			PartitionKeyTimestamp.BoundSql(r.Lower),
			PartitionKeyTimestamp.BoundSql(r.Upper),
		),
	}
}

func (self *AuditDeviceEventPartitionSettings) lockTimeoutSql() string {
	return fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.ExclusiveLockTimeout.Milliseconds())
}

func (self *AuditDeviceEventPartitionSettings) catalogStatementTimeoutSql() string {
	return fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.CatalogStatementTimeout.Milliseconds())
}

// Runs a cutover step until it succeeds, fails for a reason other than a busy
// lock, or runs out of attempts. A quiet step, one that takes ACCESS
// EXCLUSIVE, first waits for the table to be free of long transactions.
func auditDeviceEventCutoverRetry(ctx context.Context, settings *AuditDeviceEventPartitionSettings, attempts int, quiet bool, run func() error, logf func(string, ...any)) (err error) {
	for attempt := 1; attempt <= attempts; attempt += 1 {
		holders := 0
		if quiet {
			err = partitionRegistryRun(func() {
				MaintenanceDb(ctx, func(conn PgConn) {
					holders = auditDeviceEventLongLockHolders(ctx, conn, settings.MaxTransactionAge)
				})
			})
			if err != nil {
				return err
			}
		}
		if 0 < holders {
			err = fmt.Errorf("%d transaction(s) older than %s hold audit_device_event locks", holders, settings.MaxTransactionAge)
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

// An operator's new prepare needs the cutover day MinCutoverLead ahead and the
// device day rollup complete for the export's window; a test cuts over at a
// day already past, without the rollup.
func prepareAuditDeviceEventCutover(ctx context.Context, cutoverDay time.Time, operator bool, dryRun bool, logf func(string, ...any), settings *AuditDeviceEventPartitionSettings) error {
	if IsAuditDeviceEventPartitioned(ctx) {
		logf("audit_device_event is already partitioned; nothing to prepare")
		return nil
	}
	cutoverDay = cutoverDay.UTC()
	if !PartitionGranularityDaily.RangeStart(cutoverDay).Equal(cutoverDay) {
		return fmt.Errorf("the cutover day must start at 00:00 UTC: %s", cutoverDay)
	}
	logf("cutover day %s: events before it stay in %s, which drops %s after it", cutoverDay.Format(time.DateOnly), PartitionLegacyLeafName(auditDeviceEventTable), AuditDeviceEventRetention)
	logf("finalize must complete before %s; an unfinished prepare is then abandoned", cutoverDay.Add(-settings.CutoverAbandonMargin).Format(time.RFC3339))

	var shape *contractTableShape
	var staged *contractTableShape
	var boundDay time.Time
	var boundValidated bool
	var boundExists bool
	var boundErr error
	var lacking []string
	var rollupErr error
	MaintenanceDb(ctx, func(conn PgConn) {
		shape = readContractTableShape(ctx, conn, auditDeviceEventTable)
		staged = readContractTableShape(ctx, conn, auditDeviceEventStagedParentName)
		boundDay, boundValidated, boundExists, boundErr = readAuditDeviceEventLegacyBound(ctx, conn)
		if operator {
			lacking, rollupErr = auditDeviceEventRollupLacking(ctx, conn, settings)
		}
	})
	var problems []string
	if shape == nil {
		problems = append(problems, "audit_device_event does not exist")
	} else {
		problems = append(problems, auditDeviceEventCutoverProblems(shape)...)
		if boundErr != nil {
			problems = append(problems, boundErr.Error())
		} else if boundExists && !boundDay.Equal(cutoverDay) {
			problems = append(problems, fmt.Sprintf("audit_device_event is prepared for the cutover day %s; abort it first", boundDay.Format(time.DateOnly)))
		} else if operator && !boundExists && cutoverDay.Before(NowUtc().Add(settings.MinCutoverLead)) {
			problems = append(problems, fmt.Sprintf("the cutover day %s must start at least %s from now, to leave time for the validation and the swap", cutoverDay.Format(time.DateOnly), settings.MinCutoverLead))
		}
		if staged != nil {
			problems = append(problems, contractStagedProblems(shape, staged)...)
		}
	}
	if rollupErr != nil {
		problems = append(problems, rollupErr.Error())
	} else if 0 < len(lacking) {
		problems = append(problems, fmt.Sprintf("the device day rollup lacks %d complete day(s) of the export's %d (first %s, last %s); the export fills them from the raw events within the hour after the taskworker that keeps it runs", len(lacking), settings.RollupDays, lacking[0], lacking[len(lacking)-1]))
	}
	if 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the cutover", len(problems))
	}

	// the unique index nothing reads; the staged parent must not clone it
	if constraint := auditDeviceEventUniqueEventIdConstraint(shape); constraint != "" {
		statement := fmt.Sprintf("ALTER TABLE public.audit_device_event DROP CONSTRAINT %s", quoteIdentifier(constraint))
		if dryRun {
			logf("would drop the redundant unique index, a short ACCESS EXCLUSIVE lock: %s", statement)
		} else {
			err := auditDeviceEventCutoverRetry(ctx, settings, settings.CutoverAttempts, true, func() error {
				return contractPartitionExec(ctx, []string{settings.lockTimeoutSql(), settings.catalogStatementTimeoutSql(), statement})
			}, logf)
			if err != nil {
				return fmt.Errorf("drop %s: %w", constraint, err)
			}
			logf("dropped %s", constraint)
			MaintenanceDb(ctx, func(conn PgConn) {
				shape = readContractTableShape(ctx, conn, auditDeviceEventTable)
			})
		}
	}

	if staged == nil {
		if dryRun {
			logf("would stage %s, then give its indexes their published names plus %q:\n%s", auditDeviceEventStagedParentName, contractStagedIndexSuffix, strings.Join(auditDeviceEventStagedParentSql(shape), ";\n"))
		} else {
			if err := stageAuditDeviceEventParent(ctx, shape, settings); err != nil {
				return fmt.Errorf("stage %s: %w", auditDeviceEventStagedParentName, err)
			}
			logf("staged %s with %d indexes", auditDeviceEventStagedParentName, len(shape.indexes))
		}
	}

	var leaves []PartitionLeaf
	if !dryRun || staged != nil {
		var listErr error
		MaintenanceDb(ctx, func(conn PgConn) {
			leaves, listErr = auditDeviceEventDays.ListLeaves(ctx, conn, auditDeviceEventStagedParentName)
		})
		if listErr != nil && !dryRun {
			return listErr
		}
	}
	for _, day := range finishedTaskStagedDays(cutoverDay, NowUtc(), settings.AheadDays) {
		if slices.ContainsFunc(leaves, func(leaf PartitionLeaf) bool { return leaf.Range.Overlaps(day) }) {
			continue
		}
		name := auditDeviceEventDays.LeafName(auditDeviceEventTable, day)
		statements := auditDeviceEventStagedDaySql(day)
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
			"ALTER TABLE public.audit_device_event ADD CONSTRAINT %s CHECK (event_time < %s) NOT VALID",
			quoteIdentifier(auditDeviceEventLegacyBoundName),
			PartitionKeyTimestamp.BoundSql(cutoverDay),
		)
		if dryRun {
			logf("would add %s: %s", auditDeviceEventLegacyBoundName, statement)
		} else {
			// a brief ACCESS EXCLUSIVE lock, without a scan
			err := auditDeviceEventCutoverRetry(ctx, settings, settings.CutoverAttempts, true, func() error {
				return contractPartitionExec(ctx, []string{settings.lockTimeoutSql(), settings.catalogStatementTimeoutSql(), statement})
			}, logf)
			if err != nil {
				return fmt.Errorf("add %s: %w", auditDeviceEventLegacyBoundName, err)
			}
			logf("added %s", auditDeviceEventLegacyBoundName)
		}
	}
	if !boundValidated {
		if dryRun {
			logf("would validate %s (scans audit_device_event under SHARE UPDATE EXCLUSIVE)", auditDeviceEventLegacyBoundName)
		} else {
			logf("validating %s: scans audit_device_event under SHARE UPDATE EXCLUSIVE; the sweep's inserts continue", auditDeviceEventLegacyBoundName)
			validateStart := time.Now()
			err := auditDeviceEventCutoverRetry(ctx, settings, settings.CutoverAttempts, false, func() error {
				return validateAuditDeviceEventLegacyBound(ctx, settings)
			}, logf)
			if err != nil {
				return fmt.Errorf("validate %s: %w", auditDeviceEventLegacyBoundName, err)
			}
			logf("validated %s in %s", auditDeviceEventLegacyBoundName, time.Since(validateStart).Round(time.Second))
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
func validateAuditDeviceEventLegacyBound(ctx context.Context, settings *AuditDeviceEventPartitionSettings) error {
	return partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			WithoutTransactionTimeout(ctx, conn, func() {
				TxInConn(ctx, conn, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.ValidateLockTimeout.Milliseconds())))
					RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = 0`))
					RaisePgResult(tx.Exec(ctx, fmt.Sprintf("ALTER TABLE public.audit_device_event VALIDATE CONSTRAINT %s", quoteIdentifier(auditDeviceEventLegacyBoundName))))
				}, TxReadCommitted, OptNoRetry())
			})
		}, OptNoRetry())
	})
}

// Attaches an empty clone of the table to the staged parent on the day just
// below the cutover day, in a transaction that is rolled back, and checks that
// every parent index adopted one of the clone's indexes instead of building
// its own: the swap's ATTACH would otherwise build that index on the whole
// table under its lock.
func probeAuditDeviceEventLegacyAttach(ctx context.Context, cutoverDay time.Time) (problems []string) {
	errProbed := errors.New("probed")
	err := contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
		RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = '60s'`))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			"CREATE TABLE public.%s (LIKE public.audit_device_event INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING INDEXES)",
			quoteIdentifier(auditDeviceEventAttachProbeName),
		)))
		cloned := map[string]bool{}
		for _, index := range readContractTableShape(ctx, tx, auditDeviceEventAttachProbeName).indexes {
			cloned[index.name] = true
		}
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			"ALTER TABLE public.%s ATTACH PARTITION public.%s FOR VALUES FROM (%s) TO (%s)",
			quoteIdentifier(auditDeviceEventStagedParentName),
			quoteIdentifier(auditDeviceEventAttachProbeName),
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
			"public."+auditDeviceEventStagedParentName,
			"public."+auditDeviceEventAttachProbeName,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var parentIndex, childIndex string
				Raise(result.Scan(&parentIndex, &childIndex))
				if !cloned[childIndex] {
					problems = append(problems, fmt.Sprintf("%s index %s would be built on audit_device_event at the swap instead of adopting a legacy index", auditDeviceEventStagedParentName, parentIndex))
				}
			}
		})
		panic(errProbed)
	})
	if !errors.Is(err, errProbed) {
		problems = append(problems, fmt.Sprintf("attach probe of audit_device_event: %v", err))
	}
	return
}

// The swap, after its locks: the table and its indexes move to the legacy
// names, the table attaches below the cutover day, and the staged parent and
// its indexes take the published names.
func auditDeviceEventSwapSql(shape *contractTableShape, staged *contractTableShape, cutoverDay time.Time) (statements []string) {
	legacy := PartitionLegacyLeafName(auditDeviceEventTable)
	statements = append(statements, fmt.Sprintf("ALTER TABLE public.audit_device_event RENAME TO %s", quoteIdentifier(legacy)))
	for _, index := range shape.indexes {
		statements = append(statements, fmt.Sprintf(
			"ALTER INDEX public.%s RENAME TO %s",
			quoteIdentifier(index.name),
			quoteIdentifier(index.name+contractLegacyIndexSuffix),
		))
	}
	statements = append(statements, fmt.Sprintf(
		"ALTER TABLE public.%s ATTACH PARTITION public.%s FOR VALUES FROM (MINVALUE) TO (%s)",
		quoteIdentifier(auditDeviceEventStagedParentName),
		quoteIdentifier(legacy),
		PartitionKeyTimestamp.BoundSql(cutoverDay),
	))
	statements = append(statements, fmt.Sprintf("ALTER TABLE public.%s RENAME TO audit_device_event", quoteIdentifier(auditDeviceEventStagedParentName)))
	for _, index := range staged.indexes {
		name, _ := strings.CutSuffix(index.name, contractStagedIndexSuffix)
		statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(index.name), quoteIdentifier(name)))
	}
	return
}

// The locks the swap takes first: the table and the staged parent with its
// days.
func auditDeviceEventSwapLockSql(settings *AuditDeviceEventPartitionSettings) []string {
	return []string{
		settings.lockTimeoutSql(),
		settings.catalogStatementTimeoutSql(),
		fmt.Sprintf("LOCK TABLE public.audit_device_event, public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(auditDeviceEventStagedParentName)),
	}
}

func finalizeAuditDeviceEventCutover(ctx context.Context, deadline bool, dryRun bool, logf func(string, ...any), settings *AuditDeviceEventPartitionSettings) error {
	if IsAuditDeviceEventPartitioned(ctx) {
		logf("audit_device_event is already partitioned")
		if dryRun {
			return nil
		}
		// a swap that committed before its analyze ran
		return analyzeAuditDeviceEventParent(ctx, logf)
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
		shape = readContractTableShape(ctx, conn, auditDeviceEventTable)
		staged = readContractTableShape(ctx, conn, auditDeviceEventStagedParentName)
		cutoverDay, boundValidated, boundExists, boundErr = readAuditDeviceEventLegacyBound(ctx, conn)
		leaves, listErr = auditDeviceEventDays.ListLeaves(ctx, conn, auditDeviceEventStagedParentName)
	})
	var problems []string
	var syncStatements []string
	switch {
	case shape == nil:
		problems = append(problems, "audit_device_event does not exist")
	case boundErr != nil:
		problems = append(problems, boundErr.Error())
	case !boundExists || !boundValidated:
		problems = append(problems, "audit_device_event is not prepared: its legacy bound is missing or not validated")
	case staged == nil:
		problems = append(problems, fmt.Sprintf("%s is not staged", auditDeviceEventStagedParentName))
	case listErr != nil:
		problems = append(problems, listErr.Error())
	default:
		problems = append(problems, auditDeviceEventCutoverProblems(shape)...)
		if constraint := auditDeviceEventUniqueEventIdConstraint(shape); constraint != "" {
			problems = append(problems, fmt.Sprintf("audit_device_event still has %s; run prepare again", constraint))
		}
		problems = append(problems, contractStagedProblems(shape, staged)...)
		statements, indexProblems := contractStagedIndexSyncSql(shape, staged)
		problems = append(problems, indexProblems...)
		syncStatements = statements
		if !slices.ContainsFunc(leaves, func(leaf PartitionLeaf) bool {
			return leaf.Range.Covers(PartitionGranularityDaily.RangeOf(cutoverDay))
		}) {
			problems = append(problems, fmt.Sprintf("%s has no partition for the cutover day", auditDeviceEventStagedParentName))
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
				staged = readContractTableShape(ctx, conn, auditDeviceEventStagedParentName)
			})
		}
	}

	if problems := probeAuditDeviceEventLegacyAttach(ctx, cutoverDay); 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the swap", len(problems))
	}
	logf("probed the swap: every parent index adopts a legacy index")

	if dryRun {
		statements := append(auditDeviceEventSwapLockSql(settings), auditDeviceEventSwapSql(shape, staged, cutoverDay)...)
		logf("would swap in one transaction, rechecking the prepared bound under its locks:\n%s", strings.Join(statements, ";\n"))
		logf("dry run: nothing changed")
		return nil
	}
	swapStart := time.Now()
	err := auditDeviceEventCutoverRetry(ctx, settings, settings.CutoverAttempts, true, func() error {
		return contractPartitionTx(ctx, func(tx PgTx) {
			for _, statement := range auditDeviceEventSwapLockSql(settings) {
				RaisePgResult(tx.Exec(ctx, statement))
			}
			// read committed reads these after the locks: an abandon or abort
			// that committed while the swap waited leaves the table without
			// its bound, and its ATTACH would scan the table under the lock
			current := readContractTableShape(ctx, tx, auditDeviceEventTable)
			currentStaged := readContractTableShape(ctx, tx, auditDeviceEventStagedParentName)
			day, validated, exists, boundErr := readAuditDeviceEventLegacyBound(ctx, tx)
			Raise(boundErr)
			if current == nil || current.relkind != "r" || currentStaged == nil || !exists || !validated || !day.Equal(cutoverDay) {
				Raise(errors.New("the prepared cutover changed while the swap waited for its locks"))
			}
			for _, statement := range auditDeviceEventSwapSql(current, currentStaged, cutoverDay) {
				RaisePgResult(tx.Exec(ctx, statement))
			}
		})
	}, logf)
	if err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	logf("swapped audit_device_event in %s; events from %s go to daily partitions", time.Since(swapStart).Round(time.Millisecond), cutoverDay.Format(time.DateOnly))
	return analyzeAuditDeviceEventParent(ctx, logf)
}

// The parent starts without statistics, and autovacuum never analyzes a
// partitioned parent. The sample reads every partition, so the session runs
// without transaction_timeout.
func analyzeAuditDeviceEventParent(ctx context.Context, logf func(string, ...any)) error {
	err := partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			WithoutTransactionTimeout(ctx, conn, func() {
				TxInConn(ctx, conn, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = 0`))
					RaisePgResult(tx.Exec(ctx, `ANALYZE ONLY public.audit_device_event`))
				}, TxReadCommitted, OptNoRetry())
			})
		}, OptNoRetry())
	})
	if err != nil {
		return fmt.Errorf("analyze audit_device_event: %w", err)
	}
	logf("analyzed the parent")
	return nil
}

// The rollback plan, read through conn after its lock: the statements, and
// the rows the copy moves back from the daily partitions. It refuses a
// partitioned table it did not make, and more rows than the copy may take.
func auditDeviceEventRollbackPlanInConn(ctx context.Context, conn PgQuerier, settings *AuditDeviceEventPartitionSettings) (statements []string, copyRows int64, err error) {
	legacyName := PartitionLegacyLeafName(auditDeviceEventTable)
	leaves, err := auditDeviceEventDays.ListLeaves(ctx, conn, auditDeviceEventTable)
	if err != nil {
		return nil, 0, err
	}
	legacy := readContractTableShape(ctx, conn, legacyName)
	legacyAttached := slices.ContainsFunc(leaves, func(leaf PartitionLeaf) bool {
		return leaf.Name == legacyName && leaf.Range.Legacy
	})
	if legacy == nil || !legacyAttached {
		return nil, 0, fmt.Errorf("audit_device_event has no legacy partition %s; it was not partitioned by this cutover, or the legacy partition has dropped", legacyName)
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
			return nil, copyRows, fmt.Errorf("the daily partitions hold more than %d rows, which the rollback copies back under its exclusive lock; fix forward, or raise the limit and accept the stall", settings.RollbackMaxCopyRows)
		}
	}
	columns := []string{}
	for _, column := range legacy.columns {
		if column.generated == "" {
			columns = append(columns, quoteIdentifier(column.name))
		}
	}
	list := strings.Join(columns, ", ")
	statements = append(statements,
		fmt.Sprintf("ALTER TABLE public.audit_device_event DETACH PARTITION public.%s", quoteIdentifier(legacyName)),
		fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT IF EXISTS %s", quoteIdentifier(legacyName), quoteIdentifier(auditDeviceEventLegacyBoundName)),
	)
	if 0 < copyRows {
		// with the legacy table detached, the parent holds only the days
		statements = append(statements, fmt.Sprintf("INSERT INTO public.%s (%s) SELECT %s FROM public.audit_device_event", quoteIdentifier(legacyName), list, list))
	}
	statements = append(statements,
		`DROP TABLE public.audit_device_event`,
		fmt.Sprintf("ALTER TABLE public.%s RENAME TO audit_device_event", quoteIdentifier(legacyName)),
	)
	for _, index := range legacy.indexes {
		if name, ok := strings.CutSuffix(index.name, contractLegacyIndexSuffix); ok {
			statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(index.name), quoteIdentifier(name)))
		}
	}
	return statements, copyRows, nil
}

func auditDeviceEventRollbackLockSql(settings *AuditDeviceEventPartitionSettings) []string {
	return []string{
		settings.lockTimeoutSql(),
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.RollbackCopyTimeout.Milliseconds()),
		`LOCK TABLE public.audit_device_event IN ACCESS EXCLUSIVE MODE`,
	}
}

// Locks the parent and its partitions before reading the inventory, so the
// rows counted are the rows copied; the copy, the checks and every drop share
// one transaction, and a retry repeats it with fresh reads.
func rollbackAuditDeviceEventCutover(ctx context.Context, dryRun bool, logf func(string, ...any), settings *AuditDeviceEventPartitionSettings) error {
	if !IsAuditDeviceEventPartitioned(ctx) {
		logf("audit_device_event is not partitioned; nothing to roll back")
		return nil
	}
	if dryRun {
		var statements []string
		var copyRows int64
		var planErr error
		MaintenanceDb(ctx, func(conn PgConn) {
			statements, copyRows, planErr = auditDeviceEventRollbackPlanInConn(ctx, conn, settings)
		})
		if planErr != nil {
			return planErr
		}
		logf("would lock audit_device_event, recheck its partitions and copy back %d row(s) of the daily partitions, in one transaction:\n%s", copyRows, strings.Join(append(auditDeviceEventRollbackLockSql(settings), statements...), ";\n"))
		logf("dry run: nothing changed")
		return nil
	}
	var copied int64
	err := auditDeviceEventCutoverRetry(ctx, settings, settings.CutoverAttempts, true, func() error {
		return finishedTaskLongTx(ctx, func(tx PgTx) {
			copied = 0
			for _, statement := range auditDeviceEventRollbackLockSql(settings) {
				RaisePgResult(tx.Exec(ctx, statement))
			}
			statements, copyRows, err := auditDeviceEventRollbackPlanInConn(ctx, tx, settings)
			Raise(err)
			for _, statement := range statements {
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
	logf("rolled back: audit_device_event is a plain table again, with the %d row(s) written to the daily partitions since the cutover day", copied)
	return nil
}

func abortAuditDeviceEventCutover(ctx context.Context, dryRun bool, logf func(string, ...any), settings *AuditDeviceEventPartitionSettings, attempts int) error {
	if IsAuditDeviceEventPartitioned(ctx) {
		return errors.New("audit_device_event is partitioned; use rollback")
	}
	statements := []string{
		fmt.Sprintf("DROP TABLE IF EXISTS public.%s", quoteIdentifier(auditDeviceEventStagedParentName)),
		fmt.Sprintf("ALTER TABLE public.audit_device_event DROP CONSTRAINT IF EXISTS %s", quoteIdentifier(auditDeviceEventLegacyBoundName)),
	}
	if dryRun {
		logf("would run:\n%s", strings.Join(statements, ";\n"))
		return nil
	}
	err := auditDeviceEventCutoverRetry(ctx, settings, attempts, true, func() error {
		return contractPartitionExec(ctx, append([]string{settings.lockTimeoutSql(), settings.catalogStatementTimeoutSql()}, statements...))
	}, logf)
	if err != nil {
		return fmt.Errorf("abort: %w", err)
	}
	logf("aborted the audit_device_event cutover")
	return nil
}

// The family's BeforeCutover: abandons a prepared cutover once its cutover day
// is less than CutoverAbandonMargin away, since its check would refuse every
// event from that day. One attempt a pass; a busy table retries on the next
// runway pass.
func abandonStaleAuditDeviceEventCutover(ctx context.Context, now time.Time) error {
	settings := DefaultAuditDeviceEventPartitionSettings()
	var cutoverDay time.Time
	var exists bool
	err := partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			var boundErr error
			cutoverDay, _, exists, boundErr = readAuditDeviceEventLegacyBound(ctx, conn)
			Raise(boundErr)
		})
	})
	if err != nil || !exists || now.Before(cutoverDay.Add(-settings.CutoverAbandonMargin)) {
		return err
	}
	glog.Errorf(
		"[partitions]audit_device_event: abandoning the cutover prepared for %s: it was not finalized by %s, and its legacy bound would refuse every event from that day\n",
		cutoverDay.Format(time.DateOnly),
		cutoverDay.Add(-settings.CutoverAbandonMargin).Format(time.RFC3339),
	)
	return abortAuditDeviceEventCutover(ctx, false, auditDeviceEventPartitionLogf, settings, 1)
}

// The cutover is not drift: the unique event id index the migrations still
// define is dropped by the cutover's prepare (owner, 2026-11-16) and recorded
// by a later migration, and while a cutover is prepared the legacy bound check
// is its own work in progress. The staged parent is a CutoverTables table of
// the family.
func (d *SchemaDiff) acceptAuditDeviceEventPartitionCutover() {
	missing := slices.DeleteFunc(d.missingConstraints[auditDeviceEventTable], func(con auditConstraint) bool {
		return con.typ == "u" && con.def == "UNIQUE (event_id)"
	})
	if len(missing) == 0 {
		delete(d.missingConstraints, auditDeviceEventTable)
	} else {
		d.missingConstraints[auditDeviceEventTable] = missing
	}
	extra := slices.DeleteFunc(d.extraConstraints[auditDeviceEventTable], func(name string) bool {
		return name == auditDeviceEventLegacyBoundName
	})
	if len(extra) == 0 {
		delete(d.extraConstraints, auditDeviceEventTable)
	} else {
		d.extraConstraints[auditDeviceEventTable] = extra
	}
}

// Cuts a freshly migrated test database's audit_device_event over at
// cutoverDay, which may be past, without the rollup's precondition: the days
// from it through the days ahead of today are staged, so a test can write
// events into any of them.
func Testing_PartitionAuditDeviceEvents(ctx context.Context, cutoverDay time.Time) {
	if IsAuditDeviceEventPartitioned(ctx) {
		return
	}
	settings := DefaultAuditDeviceEventPartitionSettings()
	logf := func(string, ...any) {}
	Raise(prepareAuditDeviceEventCutover(ctx, cutoverDay, false, false, logf, settings))
	Raise(finalizeAuditDeviceEventCutover(ctx, false, false, logf, settings))
}
