package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// The zero-copy cutover of provider_work_session_event to weekly observed_at
// partitions (PARTITION-MAINTENANCE-PLAN.md C4), on the contract cutover's
// steps (db_contract_partition_cutover.go). The table becomes the legacy
// partition, provider_work_session_event_legacy, of every event before the
// cutover week C; events from C on go to weekly partitions. Nothing is copied.
//
//   - prepare (default): build the identity index (client_id, sequence,
//     observed_at) concurrently and swap it in as the primary key, dropping
//     the receipt foreign key (one short exclusive lock; the receipt reference
//     trigger stays). Then stage the empty parent provider_work_session_event_p
//     with LIKE ... INCLUDING INDEXES, its weekly partitions C..C+4, and add
//     and validate CHECK (observed_at < C). The index build and the
//     validation scan the table under SHARE UPDATE EXCLUSIVE, so reads and
//     writes continue; they run on a dedicated session exempt from
//     transaction_timeout. Resumable: completed steps are skipped, and an
//     interrupted index build is dropped and rebuilt.
//   - finalize: probe the attach in a rolled-back transaction, then swap in one
//     short transaction. The validated check proves the legacy bound, so
//     ATTACH skips its scan, and every legacy index is adopted without a
//     build. A finalize run again after its swap committed only analyzes the
//     parent.
//   - rollback: until the first event at or after C, undo the swap. Every
//     event written since the swap is in the legacy partition, which becomes
//     the table again, so none is lost.
//   - abort: before finalize, drop the staged parent and the check. The
//     identity primary key stays: the generator proves sequence identity.
//
// The prepared check refuses every event from C on, so finalize must run
// before C; the runway pass abandons a cutover still unfinalized
// AbandonMargin before C (abandonStaleProviderWorkSessionCutover). Every step
// that takes ACCESS EXCLUSIVE first waits until no transaction older than
// MaxTransactionAge holds a lock on the journal tables, then waits at most
// LockTimeout for its locks, retrying LockAttempts times.
type ProviderWorkSessionPartitionMigrateOptions struct {
	// the Monday 00:00 UTC from which new events go to weekly partitions
	// (prepare only)
	CutoverWeek time.Time
	DryRun      bool
	Finalize    bool
	Rollback    bool
	Abort       bool
	Logf        func(string, ...any)
	// nil uses DefaultProviderWorkSessionPartitionSettings.
	Settings *ProviderWorkSessionPartitionSettings
}

// Tunables of the cutover. Durations must be positive (database timeouts at
// least a millisecond) and attempts at least one.
type ProviderWorkSessionPartitionSettings struct {
	// The cutover week must start at least this long after prepare, to leave
	// time for the index build, the validation and the swap.
	PrepareLead time.Duration
	// A prepared cutover not finalized this long before its week is abandoned
	// by the runway pass, which runs every few minutes.
	AbandonMargin time.Duration
	// Finalize refuses to start this close to the cutover week.
	FinalizeMargin time.Duration
	// An exclusive step waits until no transaction older than this holds a
	// lock on the journal tables, so its lock request never queues traffic
	// behind a long transaction.
	MaxTransactionAge time.Duration
	// The lock wait and statement bound of each exclusive step: catalog work
	// only, so a statement that would scan or build is cut short.
	LockTimeout      time.Duration
	StatementTimeout time.Duration
	// Attempts of an exclusive step while its locks are busy, and the wait
	// between them.
	LockAttempts      int
	LockRetryInterval time.Duration
	// The lock waits of the concurrent identity index build, which also waits
	// for every transaction older than its snapshots; an interrupted build is
	// rebuilt from the start, so this outlasts any transaction.
	BuildLockTimeout time.Duration
	// The lock wait of each validation attempt, its attempts, and the wait
	// between them. Validation takes SHARE UPDATE EXCLUSIVE, which an
	// autovacuum of the table holds too.
	ValidateLockTimeout   time.Duration
	ValidateAttempts      int
	ValidateRetryInterval time.Duration
}

// The contract cutover's budgets: a day's lead, abandoned six hours before the
// week, seconds of exclusive lock waits.
func DefaultProviderWorkSessionPartitionSettings() *ProviderWorkSessionPartitionSettings {
	return &ProviderWorkSessionPartitionSettings{
		PrepareLead:           24 * time.Hour,
		AbandonMargin:         6 * time.Hour,
		FinalizeMargin:        10 * time.Minute,
		MaxTransactionAge:     5 * time.Second,
		LockTimeout:           3 * time.Second,
		StatementTimeout:      30 * time.Second,
		LockAttempts:          60,
		LockRetryInterval:     10 * time.Second,
		BuildLockTimeout:      30 * time.Minute,
		ValidateLockTimeout:   60 * time.Second,
		ValidateAttempts:      10,
		ValidateRetryInterval: 30 * time.Second,
	}
}

func (self *ProviderWorkSessionPartitionSettings) validate() error {
	for _, timeout := range []time.Duration{self.LockTimeout, self.StatementTimeout, self.BuildLockTimeout, self.ValidateLockTimeout} {
		if timeout < time.Millisecond {
			return errors.New("the cutover needs database timeouts of at least a millisecond")
		}
	}
	if self.PrepareLead <= 0 || self.AbandonMargin <= 0 || self.FinalizeMargin <= 0 || self.MaxTransactionAge <= 0 ||
		self.LockRetryInterval <= 0 || self.ValidateRetryInterval <= 0 || self.LockAttempts < 1 || self.ValidateAttempts < 1 {
		return errors.New("the cutover needs positive margins, waits and attempts")
	}
	if self.PrepareLead <= self.AbandonMargin || self.AbandonMargin <= self.FinalizeMargin {
		return errors.New("the cutover's lead must exceed its abandon margin, and that its finalize margin")
	}
	return nil
}

// The receipt key the cutover drops, the identity index it swaps in as the
// primary key, and that key.
const providerWorkSessionReceiptTable = "provider_work_session_receipt"
const providerWorkSessionReceiptForeignKeyName = "provider_work_session_receipt_client_id_sequence_fkey"
const providerWorkSessionEventPrimaryKeyName = "provider_work_session_event_pkey"
const providerWorkSessionEventIdentityIndexName = "provider_work_session_event_identity"
const providerWorkSessionEventIdentityKey = "PRIMARY KEY (client_id, sequence, observed_at)"
const providerWorkSessionEventSequenceKey = "PRIMARY KEY (client_id, sequence)"

func MigrateProviderWorkSessionEventsToPartitions(ctx context.Context, options ProviderWorkSessionPartitionMigrateOptions) error {
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	settings := options.Settings
	if settings == nil {
		settings = DefaultProviderWorkSessionPartitionSettings()
	}
	if err := settings.validate(); err != nil {
		return err
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
	cutover := newProviderWorkSessionCutover(settings, options.DryRun, logf)
	switch {
	case options.Finalize:
		return cutover.finalize(ctx, true)
	case options.Rollback:
		return cutover.rollback(ctx)
	case options.Abort:
		return cutover.abort(ctx)
	default:
		return cutover.prepare(ctx, options.CutoverWeek)
	}
}

// One run of the cutover command.
type providerWorkSessionCutover struct {
	settings *ProviderWorkSessionPartitionSettings
	dryRun   bool
	logf     func(string, ...any)
}

func newProviderWorkSessionCutover(settings *ProviderWorkSessionPartitionSettings, dryRun bool, logf func(string, ...any)) *providerWorkSessionCutover {
	return &providerWorkSessionCutover{settings: settings, dryRun: dryRun, logf: logf}
}

// Whether the journal is partitioned yet: the registry's default for the
// family (PartitionFamily.IsPartitioned), read without the family so the
// family's own hooks can ask.
func isProviderWorkSessionEventPartitioned(ctx context.Context) (partitioned bool) {
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE((SELECT relkind = 'p' FROM pg_class WHERE oid = to_regclass($1)), false)`,
			"public."+providerWorkSessionEventTable,
		).Scan(&partitioned))
	})
	return
}

func providerWorkSessionStagedParent() string {
	return contractStagedParentName(providerWorkSessionEventTable)
}

func providerWorkSessionLegacyBound() string {
	return contractLegacyBoundName(providerWorkSessionEventTable)
}

func readProviderWorkSessionShapes(ctx context.Context) (shape *contractTableShape, staged *contractTableShape) {
	MaintenanceDb(ctx, func(conn PgConn) {
		shape = readContractTableShape(ctx, conn, providerWorkSessionEventTable)
		staged = readContractTableShape(ctx, conn, providerWorkSessionStagedParent())
	})
	return
}

// The table's primary key definition, or "".
func providerWorkSessionPrimaryKey(shape *contractTableShape) string {
	for _, index := range shape.indexes {
		if index.primary {
			return index.constraintDefinition
		}
	}
	return ""
}

// The transactions older than maxTransactionAge holding a lock on the journal
// tables, which an ACCESS EXCLUSIVE request would queue traffic behind.
func providerWorkSessionTablesBusy(ctx context.Context, maxTransactionAge time.Duration) (busy int, err error) {
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(
				ctx,
				`
				SELECT count(DISTINCT activity.pid)
				FROM pg_locks lock
				JOIN pg_stat_activity activity ON activity.pid = lock.pid
				WHERE lock.relation = ANY (ARRAY(SELECT to_regclass(name) FROM unnest($1::text[]) name))
					AND activity.pid <> pg_backend_pid()
					AND activity.xact_start < now() - make_interval(secs => $2)
				`,
				[]string{
					"public." + providerWorkSessionEventTable,
					"public." + providerWorkSessionStagedParent(),
					"public." + providerWorkSessionReceiptTable,
				},
				maxTransactionAge.Seconds(),
			).Scan(&busy))
		})
	})
	return
}

// Runs one exclusive step until it succeeds, fails for a reason other than a
// busy lock, or runs out of attempts. Each attempt first waits for the journal
// tables to be free of long transactions, and repeats the whole step, so it
// reads state again under its own locks.
func (self *providerWorkSessionCutover) retryExclusive(ctx context.Context, run func() error) (err error) {
	for attempt := 1; attempt <= self.settings.LockAttempts; attempt += 1 {
		var busy int
		if busy, err = providerWorkSessionTablesBusy(ctx, self.settings.MaxTransactionAge); err != nil {
			return
		}
		if 0 < busy {
			self.logf("%d transaction(s) older than %s hold journal table locks (attempt %d/%d)", busy, self.settings.MaxTransactionAge, attempt, self.settings.LockAttempts)
			err = fmt.Errorf("%d long transaction(s) hold journal table locks", busy)
		} else {
			err = run()
			if err == nil || !contractPartitionLockBusy(err) {
				return
			}
			self.logf("lock busy (attempt %d/%d): %v", attempt, self.settings.LockAttempts, err)
		}
		if attempt == self.settings.LockAttempts {
			return
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(self.settings.LockRetryInterval):
		}
	}
	return
}

// The bounds of an exclusive step's transaction.
func (self *providerWorkSessionCutover) exclusiveBoundsSql() []string {
	return []string{
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds()),
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.settings.StatementTimeout.Milliseconds()),
	}
}

// Runs statements on a dedicated maintenance session exempt from
// transaction_timeout and statement_timeout, outside a transaction, then
// discards the session so none of its settings return to the pool.
func providerWorkSessionScan(ctx context.Context, lockTimeout time.Duration, statements []string) (err error) {
	conn, acquireErr := AcquireMaintenanceDbConn(ctx)
	if acquireErr != nil {
		return acquireErr
	}
	defer discardPgConnection(ctx, conn)
	return partitionRegistryRun(func() {
		WithoutTransactionTimeout(ctx, conn, func() {
			RaisePgResult(conn.Exec(ctx, `SET statement_timeout = 0`))
			RaisePgResult(conn.Exec(ctx, fmt.Sprintf("SET lock_timeout = '%dms'", lockTimeout.Milliseconds())))
			for _, statement := range statements {
				RaisePgResult(conn.Exec(ctx, statement))
			}
		})
	})
}

// Reasons the table cannot become the legacy partition as it is. The identity
// index of an interrupted build is not one: prepare rebuilds it.
func providerWorkSessionCutoverProblems(shape *contractTableShape) (problems []string) {
	if shape.relkind != "r" {
		problems = append(problems, fmt.Sprintf("%s is not a plain table (relkind %s)", shape.table, shape.relkind))
	}
	observedAt := slices.IndexFunc(shape.columns, func(column contractColumn) bool {
		return column.name == "observed_at"
	})
	if observedAt < 0 || shape.columns[observedAt].typeName != "timestamp without time zone" || !shape.columns[observedAt].notNull {
		problems = append(problems, fmt.Sprintf("%s.observed_at is not a NOT NULL timestamp", shape.table))
	}
	for _, check := range shape.checks {
		if check.noInherit {
			problems = append(problems, fmt.Sprintf("%s check %s is NO INHERIT", shape.table, check.name))
		}
	}
	primary := 0
	for _, index := range shape.indexes {
		if index.name == providerWorkSessionEventIdentityIndexName && !index.primary {
			continue
		}
		if !index.valid || !index.ready {
			problems = append(problems, fmt.Sprintf("%s index %s is invalid; drop it (DROP INDEX CONCURRENTLY) or rebuild it before the cutover", shape.table, index.name))
			continue
		}
		if pgMaxIdentifierLength < len(index.name)+len(contractStagedIndexSuffix) || pgMaxIdentifierLength < len(index.name)+len(contractLegacyIndexSuffix) {
			problems = append(problems, fmt.Sprintf("%s index name %s is too long to stage", shape.table, index.name))
		}
		if index.primary {
			primary += 1
			if index.constraintDefinition != providerWorkSessionEventSequenceKey && index.constraintDefinition != providerWorkSessionEventIdentityKey {
				problems = append(problems, fmt.Sprintf("%s has an unexpected primary key: %s", shape.table, index.constraintDefinition))
			}
		} else if index.unique {
			problems = append(problems, fmt.Sprintf("%s unique index %s is not the primary key", shape.table, index.name))
		} else if _, ok := contractRetargetIndex(index.definition, index.name, shape.table); !ok {
			problems = append(problems, fmt.Sprintf("%s index %s has an unexpected definition", shape.table, index.name))
		}
	}
	if primary != 1 {
		problems = append(problems, fmt.Sprintf("%s has %d primary keys", shape.table, primary))
	}
	for _, trigger := range shape.triggers {
		if trigger.constraint {
			problems = append(problems, fmt.Sprintf("%s trigger %s is a constraint trigger", shape.table, trigger.name))
		}
		if _, ok := contractRetargetTrigger(trigger.definition, shape.table, shape.table+"_x"); !ok {
			problems = append(problems, fmt.Sprintf("%s trigger %s has an unexpected definition", shape.table, trigger.name))
		}
	}
	for _, foreignKey := range shape.foreignKeys {
		problems = append(problems, fmt.Sprintf("%s foreign key %s references %s", shape.table, foreignKey.name, foreignKey.referencedTable))
	}
	for _, foreignKey := range shape.referencedBy {
		if foreignKey.table != providerWorkSessionReceiptTable || foreignKey.name != providerWorkSessionReceiptForeignKeyName {
			problems = append(problems, fmt.Sprintf("%s is referenced by %s of %s", shape.table, foreignKey.name, foreignKey.table))
		}
	}
	return
}

// Whether the migration that publishes the receipt reference trigger and the
// drop guard (providerWorkSessionPartitionSchemaSql) is applied.
func providerWorkSessionPartitionMigrated(ctx context.Context) (migrated bool) {
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(
			ctx,
			`
			SELECT to_regprocedure('public.provider_work_session_event_drop_guard(text)') IS NOT NULL
				AND to_regprocedure('public.provider_work_session_admit_floor(uuid)') IS NOT NULL
				AND EXISTS (
					SELECT 1 FROM pg_trigger
					WHERE tgrelid = to_regclass('public.provider_work_session_receipt')
						AND tgname = 'provider_work_session_receipt_reference' AND tgenabled = 'O'
				)
			`,
		).Scan(&migrated))
	})
	return
}

// The identity swap: the receipt foreign key goes (the reference trigger
// keeps the reference), then the identity index replaces the primary key.
// The tables are locked in the order the journal's writers lock them.
func (self *providerWorkSessionCutover) identitySwapSql() []string {
	return append(self.exclusiveBoundsSql(),
		fmt.Sprintf("LOCK TABLE public.%s, public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(providerWorkSessionEventTable), quoteIdentifier(providerWorkSessionReceiptTable)),
		fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT IF EXISTS %s", quoteIdentifier(providerWorkSessionReceiptTable), quoteIdentifier(providerWorkSessionReceiptForeignKeyName)),
		fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT %s", quoteIdentifier(providerWorkSessionEventTable), quoteIdentifier(providerWorkSessionEventPrimaryKeyName)),
		fmt.Sprintf(
			"ALTER TABLE public.%s ADD CONSTRAINT %s PRIMARY KEY USING INDEX %s",
			quoteIdentifier(providerWorkSessionEventTable),
			quoteIdentifier(providerWorkSessionEventPrimaryKeyName),
			quoteIdentifier(providerWorkSessionEventIdentityIndexName),
		),
	)
}

// The statement that builds the identity index, and the one that drops an
// interrupted build first.
func providerWorkSessionIdentityIndexSql() (drop string, build string) {
	drop = fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS public.%s", quoteIdentifier(providerWorkSessionEventIdentityIndexName))
	build = fmt.Sprintf(
		"CREATE UNIQUE INDEX CONCURRENTLY %s ON public.%s USING btree (client_id, sequence, observed_at)",
		quoteIdentifier(providerWorkSessionEventIdentityIndexName),
		quoteIdentifier(providerWorkSessionEventTable),
	)
	return
}

// The backend building the identity index, or 0. A build continues on the
// server after its client goes away, until it finishes or is canceled.
func providerWorkSessionIdentityBuildPid(ctx context.Context) (pid int32) {
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE((SELECT pid FROM pg_stat_progress_create_index WHERE index_relid = to_regclass($1) LIMIT 1), 0)`,
			"public."+providerWorkSessionEventIdentityIndexName,
		).Scan(&pid))
	})
	return
}

// The statements that stage the partitioned parent from the table. LIKE ...
// INCLUDING INDEXES clones each index from its stored expression trees, so
// ATTACH adopts the table's own indexes at the swap; checks are added after
// with their validity, since a valid parent check cannot adopt a NOT VALID one.
func providerWorkSessionStagedParentSql(shape *contractTableShape) (statements []string) {
	staged := providerWorkSessionStagedParent()
	statements = append(statements, fmt.Sprintf(
		"CREATE TABLE public.%s (LIKE public.%s INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING INDEXES INCLUDING STORAGE INCLUDING COMPRESSION INCLUDING COMMENTS) PARTITION BY RANGE (observed_at)",
		quoteIdentifier(staged),
		quoteIdentifier(shape.table),
	))
	for _, check := range shape.checks {
		statements = append(statements, fmt.Sprintf(
			"ALTER TABLE public.%s ADD CONSTRAINT %s %s",
			quoteIdentifier(staged),
			quoteIdentifier(check.name),
			check.definition,
		))
	}
	for _, column := range shape.columns {
		if 0 <= column.statisticsTarget {
			statements = append(statements, fmt.Sprintf(
				"ALTER TABLE public.%s ALTER COLUMN %s SET STATISTICS %d",
				quoteIdentifier(staged),
				quoteIdentifier(column.name),
				column.statisticsTarget,
			))
		}
	}
	return
}

// Stages the partitioned parent in one transaction, its indexes named as the
// table's plus the staged suffix.
func stageProviderWorkSessionParent(ctx context.Context, shape *contractTableShape) error {
	return contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
		for _, statement := range providerWorkSessionStagedParentSql(shape) {
			RaisePgResult(tx.Exec(ctx, statement))
		}
		staged := readContractTableShape(ctx, tx, providerWorkSessionStagedParent())
		statements, err := contractStagedRenameSql(shape, staged)
		Raise(err)
		for _, statement := range statements {
			RaisePgResult(tx.Exec(ctx, statement))
		}
	})
}

// The statements that create the week's leaf under parent: a table with the
// parent's columns and checks, the table's TRUNCATE guards (a parent's
// statement trigger does not fire for a statement that names a partition),
// then ATTACH, which builds the parent's indexes on the empty leaf and clones
// its row triggers. The leaf takes the registry's name, so the registry
// maintains it after the swap.
func providerWorkSessionWeekLeafSql(parent string, parentShape *contractTableShape, guards *contractTableShape, weekStart time.Time) (statements []string, err error) {
	r := PartitionGranularityWeekly.RangeOf(weekStart)
	name := providerWorkSessionEventLeaf(r)
	leaf := &contractTableShape{table: name, columns: parentShape.columns, checks: parentShape.checks}
	statements = append(statements, leaf.createTableSql(name, false)...)
	guardStatements, err := guards.truncateGuardsSql(providerWorkSessionEventTable, name)
	if err != nil {
		return nil, err
	}
	statements = append(statements, guardStatements...)
	statements = append(statements, fmt.Sprintf(
		"ALTER TABLE public.%s ATTACH PARTITION public.%s FOR VALUES FROM (%s) TO (%s)",
		quoteIdentifier(parent),
		quoteIdentifier(name),
		PartitionKeyTimestamp.BoundSql(r.Lower),
		PartitionKeyTimestamp.BoundSql(r.Upper),
	))
	return
}

// The check that proves the legacy partition's bound, added NOT VALID.
func providerWorkSessionLegacyBoundSql(cutoverWeek time.Time) string {
	return fmt.Sprintf(
		"ALTER TABLE public.%s ADD CONSTRAINT %s CHECK (observed_at < %s) NOT VALID",
		quoteIdentifier(providerWorkSessionEventTable),
		quoteIdentifier(providerWorkSessionLegacyBound()),
		PartitionKeyTimestamp.BoundSql(cutoverWeek),
	)
}

func (self *providerWorkSessionCutover) prepare(ctx context.Context, cutoverWeek time.Time) error {
	if cutoverWeek.IsZero() {
		return errors.New("the cutover week is required to prepare")
	}
	if cutoverWeek.Before(NowUtc().Add(self.settings.PrepareLead)) {
		return fmt.Errorf("the cutover week %s must start at least %s from now, to leave time for the index build, the validation and the swap", cutoverWeek.UTC().Format(time.DateOnly), self.settings.PrepareLead)
	}
	return self.prepareForWeek(ctx, cutoverWeek)
}

func (self *providerWorkSessionCutover) prepareForWeek(ctx context.Context, cutoverWeek time.Time) error {
	cutoverWeek = cutoverWeek.UTC()
	if !PartitionGranularityWeekly.RangeStart(cutoverWeek).Equal(cutoverWeek) {
		return fmt.Errorf("the cutover week must start Monday 00:00 UTC: %s", cutoverWeek)
	}
	if isProviderWorkSessionEventPartitioned(ctx) {
		return errors.New("provider_work_session_event is already partitioned")
	}
	if !providerWorkSessionPartitionMigrated(ctx) {
		return errors.New("the receipt reference trigger or the drop guard is missing; apply the database migrations first")
	}
	self.logf("cutover week %s: events observed before it stay in %s", cutoverWeek.Format(time.DateOnly), PartitionLegacyLeafName(providerWorkSessionEventTable))
	self.logf("finalize must complete before %s; an unfinished preparation is then abandoned", cutoverWeek.Add(-self.settings.AbandonMargin).Format(time.RFC3339))

	shape, staged := readProviderWorkSessionShapes(ctx)
	if shape == nil {
		return fmt.Errorf("%s does not exist", providerWorkSessionEventTable)
	}
	problems := providerWorkSessionCutoverProblems(shape)
	if shape.legacyBound != nil {
		if week, prepared, err := preparedProviderWorkSessionCutoverWeek(ctx); err != nil {
			problems = append(problems, err.Error())
		} else if prepared && !week.Equal(cutoverWeek) {
			problems = append(problems, fmt.Sprintf("%s is prepared for the cutover week %s; abort it first", shape.table, week.Format(time.DateOnly)))
		}
	}
	if staged != nil {
		problems = append(problems, contractStagedProblems(shape, staged)...)
	}
	if 0 < len(problems) {
		for _, problem := range problems {
			self.logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the cutover", len(problems))
	}

	// the identity primary key, built concurrently and swapped in briefly
	if providerWorkSessionPrimaryKey(shape) == providerWorkSessionEventSequenceKey {
		dropBuild, build := providerWorkSessionIdentityIndexSql()
		identity := contractTableShapeIndex(shape, providerWorkSessionEventIdentityIndexName)
		swap := self.identitySwapSql()
		if self.dryRun {
			if identity == nil || !identity.valid || !identity.ready {
				self.logf("would build the identity index, scanning %s under SHARE UPDATE EXCLUSIVE:\n%s;\n%s", shape.table, dropBuild, build)
			}
			self.logf("would swap in the identity primary key in one transaction:\n%s", strings.Join(swap, ";\n"))
		} else {
			if identity == nil || !identity.valid || !identity.ready {
				statements := []string{build}
				if identity != nil {
					// an interrupted build left it invalid, unless a build
					// whose client went away is still running on the server
					if pid := providerWorkSessionIdentityBuildPid(ctx); pid != 0 {
						return fmt.Errorf("%s is being built by backend %d; let it finish or cancel it (pg_cancel_backend), then prepare again", providerWorkSessionEventIdentityIndexName, pid)
					}
					statements = []string{dropBuild, build}
				}
				self.logf("building %s: scans %s under SHARE UPDATE EXCLUSIVE; reads and writes continue", providerWorkSessionEventIdentityIndexName, shape.table)
				buildStart := time.Now()
				if err := providerWorkSessionScan(ctx, self.settings.BuildLockTimeout, statements); err != nil {
					return fmt.Errorf("build %s: %w", providerWorkSessionEventIdentityIndexName, err)
				}
				self.logf("built %s in %s", providerWorkSessionEventIdentityIndexName, time.Since(buildStart).Round(time.Second))
			}
			if err := self.retryExclusive(ctx, func() error {
				return contractPartitionExec(ctx, swap)
			}); err != nil {
				return fmt.Errorf("swap in the identity primary key: %w", err)
			}
			self.logf("swapped in %s and dropped %s", providerWorkSessionEventIdentityKey, providerWorkSessionReceiptForeignKeyName)
			shape, staged = readProviderWorkSessionShapes(ctx)
		}
	}

	stagedName := providerWorkSessionStagedParent()
	if staged == nil {
		if self.dryRun {
			self.logf("would stage %s, then give its indexes their published names plus %q:\n%s", stagedName, contractStagedIndexSuffix, strings.Join(providerWorkSessionStagedParentSql(shape), ";\n"))
		} else {
			if err := stageProviderWorkSessionParent(ctx, shape); err != nil {
				return fmt.Errorf("stage %s: %w", stagedName, err)
			}
			self.logf("staged %s with %d indexes", stagedName, len(shape.indexes))
			_, staged = readProviderWorkSessionShapes(ctx)
		}
	}
	stagedShape := staged
	if stagedShape == nil {
		// dry run: the staged parent would have the table's columns and checks
		stagedShape = shape
	}
	existing := map[string]bool{}
	if staged != nil {
		var leaves []PartitionLeaf
		var listErr error
		MaintenanceDb(ctx, func(conn PgConn) {
			leaves, listErr = providerWorkSessionEventPartitionFamily.ListLeaves(ctx, conn, stagedName)
		})
		if listErr != nil {
			return listErr
		}
		for _, leaf := range leaves {
			existing[leaf.Name] = true
		}
	}
	for week := 0; week <= providerWorkSessionEventPartitionFamily.AheadRanges; week += 1 {
		weekStart := PartitionGranularityWeekly.AddRanges(cutoverWeek, week)
		name := providerWorkSessionEventLeaf(PartitionGranularityWeekly.RangeOf(weekStart))
		if existing[name] {
			continue
		}
		statements, err := providerWorkSessionWeekLeafSql(stagedName, stagedShape, shape, weekStart)
		if err != nil {
			return err
		}
		if self.dryRun {
			self.logf("would create %s:\n%s", name, strings.Join(statements, ";\n"))
			continue
		}
		if err := contractPartitionExec(ctx, append([]string{`SET LOCAL lock_timeout = '5s'`}, statements...)); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		self.logf("created %s", name)
	}

	bound := providerWorkSessionLegacyBound()
	if shape.legacyBound == nil {
		statement := providerWorkSessionLegacyBoundSql(cutoverWeek)
		if self.dryRun {
			self.logf("would add %s: %s", bound, statement)
		} else {
			// a brief ACCESS EXCLUSIVE lock, without a scan
			if err := self.retryExclusive(ctx, func() error {
				return contractPartitionExec(ctx, append(self.exclusiveBoundsSql(), statement))
			}); err != nil {
				return fmt.Errorf("add %s: %w", bound, err)
			}
			self.logf("added %s", bound)
		}
	}
	if shape.legacyBound == nil || !shape.legacyBound.validated {
		statement := fmt.Sprintf("ALTER TABLE public.%s VALIDATE CONSTRAINT %s", quoteIdentifier(providerWorkSessionEventTable), quoteIdentifier(bound))
		if self.dryRun {
			self.logf("would validate %s (scans %s under SHARE UPDATE EXCLUSIVE)", bound, providerWorkSessionEventTable)
		} else {
			self.logf("validating %s: scans %s under SHARE UPDATE EXCLUSIVE; reads and writes continue", bound, providerWorkSessionEventTable)
			validateStart := time.Now()
			if err := self.validateLegacyBound(ctx, statement); err != nil {
				return fmt.Errorf("validate %s: %w", bound, err)
			}
			self.logf("validated %s in %s", bound, time.Since(validateStart).Round(time.Second))
		}
	}
	if self.dryRun {
		self.logf("dry run: nothing changed")
	} else {
		self.logf("prepared; run --finalize before %s", cutoverWeek.Add(-self.settings.AbandonMargin).Format(time.RFC3339))
	}
	return nil
}

// Validates the legacy bound on a session exempt from the transaction bound,
// retrying while an autovacuum or another lock holder keeps it waiting.
func (self *providerWorkSessionCutover) validateLegacyBound(ctx context.Context, statement string) (err error) {
	for attempt := 1; attempt <= self.settings.ValidateAttempts; attempt += 1 {
		err = providerWorkSessionScan(ctx, self.settings.ValidateLockTimeout, []string{statement})
		if err == nil || !contractPartitionLockBusy(err) || attempt == self.settings.ValidateAttempts {
			return
		}
		self.logf("validation lock busy (attempt %d/%d): %v", attempt, self.settings.ValidateAttempts, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(self.settings.ValidateRetryInterval):
		}
	}
	return
}

// ATTACH must adopt a valid legacy index for every parent index; a parent
// index without a match would be built on the whole table under the swap's
// lock. An empty clone of the table has the same index trees (LIKE), so attach
// one to the staged parent for the week before the cutover week, in a
// transaction that is rolled back, and check every parent index adopted one of
// the clone's indexes instead of building its own.
func probeProviderWorkSessionLegacyAttach(ctx context.Context, cutoverWeek time.Time) (problems []string) {
	staged := providerWorkSessionStagedParent()
	probe := providerWorkSessionEventTable + "_attach_probe"
	errProbed := errors.New("probed")
	err := contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
		RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = '60s'`))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			"CREATE TABLE public.%s (LIKE public.%s INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING INDEXES)",
			quoteIdentifier(probe),
			quoteIdentifier(providerWorkSessionEventTable),
		)))
		cloned := map[string]bool{}
		for _, index := range readContractTableShape(ctx, tx, probe).indexes {
			cloned[index.name] = true
		}
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			"ALTER TABLE public.%s ATTACH PARTITION public.%s FOR VALUES FROM (%s) TO (%s)",
			quoteIdentifier(staged),
			quoteIdentifier(probe),
			PartitionKeyTimestamp.BoundSql(PartitionGranularityWeekly.AddRanges(cutoverWeek, -1)),
			PartitionKeyTimestamp.BoundSql(cutoverWeek),
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
			"public."+staged,
			"public."+probe,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var parentIndex, childIndex string
				Raise(result.Scan(&parentIndex, &childIndex))
				if !cloned[childIndex] {
					problems = append(problems, fmt.Sprintf("%s index %s would be built on %s at the swap instead of adopting a legacy index", staged, parentIndex, providerWorkSessionEventTable))
				}
			}
		})
		panic(errProbed)
	})
	if !errors.Is(err, errProbed) {
		problems = append(problems, fmt.Sprintf("attach probe of %s: %v", providerWorkSessionEventTable, err))
	}
	return
}

// The swap transaction. Both tables are locked first, so the renames and the
// attach see no concurrent statement; the table's row triggers are dropped
// and recreated on the parent, which clones them onto every partition, while
// its TRUNCATE guard stays on the legacy partition for a TRUNCATE that names
// it.
func (self *providerWorkSessionCutover) swapSql(shape *contractTableShape, staged *contractTableShape, cutoverWeek time.Time) (statements []string) {
	table := providerWorkSessionEventTable
	legacy := PartitionLegacyLeafName(table)
	stagedName := providerWorkSessionStagedParent()
	statements = append(statements, self.exclusiveBoundsSql()...)
	statements = append(statements, fmt.Sprintf("LOCK TABLE public.%s, public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(table), quoteIdentifier(stagedName)))
	for _, trigger := range shape.triggers {
		if !trigger.truncate {
			statements = append(statements, fmt.Sprintf("DROP TRIGGER %s ON public.%s", quoteIdentifier(trigger.name), quoteIdentifier(table)))
		}
	}
	statements = append(statements, fmt.Sprintf("ALTER TABLE public.%s RENAME TO %s", quoteIdentifier(table), quoteIdentifier(legacy)))
	for _, index := range shape.indexes {
		statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(index.name), quoteIdentifier(index.name+contractLegacyIndexSuffix)))
	}
	statements = append(statements, fmt.Sprintf(
		"ALTER TABLE public.%s ATTACH PARTITION public.%s FOR VALUES FROM (MINVALUE) TO (%s)",
		quoteIdentifier(stagedName),
		quoteIdentifier(legacy),
		PartitionKeyTimestamp.BoundSql(cutoverWeek),
	))
	statements = append(statements, fmt.Sprintf("ALTER TABLE public.%s RENAME TO %s", quoteIdentifier(stagedName), quoteIdentifier(table)))
	for _, index := range staged.indexes {
		name, _ := strings.CutSuffix(index.name, contractStagedIndexSuffix)
		statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(index.name), quoteIdentifier(name)))
	}
	for _, trigger := range shape.triggers {
		statements = append(statements, trigger.definition)
		statements = append(statements, contractTriggerEnabledSql(table, trigger)...)
	}
	return
}

func (self *providerWorkSessionCutover) finalize(ctx context.Context, deadline bool) error {
	if isProviderWorkSessionEventPartitioned(ctx) {
		var legacy *contractTableShape
		MaintenanceDb(ctx, func(conn PgConn) {
			legacy = readContractTableShape(ctx, conn, PartitionLegacyLeafName(providerWorkSessionEventTable))
		})
		if legacy == nil {
			return errors.New("provider_work_session_event is partitioned without a legacy partition")
		}
		self.logf("already swapped")
		if self.dryRun {
			self.logf("dry run: nothing changed")
			return nil
		}
		return self.analyzeParent(ctx)
	}
	shape, staged := readProviderWorkSessionShapes(ctx)
	if shape == nil {
		return fmt.Errorf("%s does not exist", providerWorkSessionEventTable)
	}
	problems := providerWorkSessionCutoverProblems(shape)
	if primaryKey := providerWorkSessionPrimaryKey(shape); primaryKey != providerWorkSessionEventIdentityKey {
		problems = append(problems, fmt.Sprintf("%s has primary key %q, not the identity key; prepare first", shape.table, primaryKey))
	}
	if 0 < len(shape.referencedBy) {
		problems = append(problems, fmt.Sprintf("%s is still referenced by a foreign key; prepare first", shape.table))
	}
	cutoverWeek, prepared, err := preparedProviderWorkSessionCutoverWeek(ctx)
	if err != nil {
		problems = append(problems, err.Error())
	}
	if !prepared || shape.legacyBound == nil || !shape.legacyBound.validated {
		problems = append(problems, fmt.Sprintf("%s is not prepared: its legacy bound is missing or not validated", shape.table))
	}
	var syncStatements []string
	if staged == nil {
		problems = append(problems, fmt.Sprintf("%s is not staged", providerWorkSessionStagedParent()))
	} else {
		problems = append(problems, contractStagedProblems(shape, staged)...)
		statements, indexProblems := contractStagedIndexSyncSql(shape, staged)
		problems = append(problems, indexProblems...)
		syncStatements = statements
		var leaves []PartitionLeaf
		var listErr error
		MaintenanceDb(ctx, func(conn PgConn) {
			leaves, listErr = providerWorkSessionEventPartitionFamily.ListLeaves(ctx, conn, providerWorkSessionStagedParent())
		})
		if listErr != nil {
			problems = append(problems, listErr.Error())
		} else if prepared && !PartitionRangeCovered(leaves, PartitionGranularityWeekly.RangeOf(cutoverWeek)) {
			problems = append(problems, fmt.Sprintf("%s has no partition for the cutover week", providerWorkSessionStagedParent()))
		}
	}
	if 0 < len(problems) {
		for _, problem := range problems {
			self.logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the swap", len(problems))
	}
	if deadline && !NowUtc().Before(cutoverWeek.Add(-self.settings.FinalizeMargin)) {
		return fmt.Errorf("too close to the cutover week %s; abort and prepare a later week", cutoverWeek.Format(time.DateOnly))
	}

	if 0 < len(syncStatements) {
		if self.dryRun {
			self.logf("would sync staged indexes:\n%s", strings.Join(syncStatements, ";\n"))
		} else {
			if err := contractPartitionExec(ctx, append([]string{`SET LOCAL lock_timeout = '5s'`}, syncStatements...)); err != nil {
				return fmt.Errorf("sync staged indexes: %w", err)
			}
			self.logf("synced %d staged index change(s)", len(syncStatements))
			_, staged = readProviderWorkSessionShapes(ctx)
		}
	}

	if problems := probeProviderWorkSessionLegacyAttach(ctx, cutoverWeek); 0 < len(problems) {
		for _, problem := range problems {
			self.logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the swap", len(problems))
	}
	self.logf("probed the swap: every parent index adopts a legacy index")

	statements := self.swapSql(shape, staged, cutoverWeek)
	if self.dryRun {
		self.logf("would swap in one transaction:\n%s", strings.Join(statements, ";\n"))
		self.logf("dry run: nothing changed")
		return nil
	}
	swapStart := time.Now()
	if err := self.retryExclusive(ctx, func() error {
		return contractPartitionExec(ctx, statements)
	}); err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	self.logf("swapped %s in %s; events from %s go to weekly partitions", providerWorkSessionEventTable, time.Since(swapStart).Round(time.Millisecond), cutoverWeek.Format(time.DateOnly))
	return self.analyzeParent(ctx)
}

// The parent starts without statistics; autovacuum never analyzes it.
func (self *providerWorkSessionCutover) analyzeParent(ctx context.Context) error {
	statement := fmt.Sprintf("ANALYZE ONLY public.%s", quoteIdentifier(providerWorkSessionEventTable))
	if err := contractPartitionExec(ctx, []string{`SET LOCAL statement_timeout = 0`, liftTransactionTimeoutInTxSql, statement}); err != nil {
		return fmt.Errorf("analyze %s: %w", providerWorkSessionEventTable, err)
	}
	self.logf("analyzed the parent")
	return nil
}

// Builds rollback from the catalog as the caller's transaction reads it,
// after it locked the parent and, through it, every partition. A weekly leaf
// that holds a row ends the rollback window: those events are not in the
// legacy partition.
func providerWorkSessionRollbackSqlInConn(ctx context.Context, conn PgQuerier) (statements []string, err error) {
	table := providerWorkSessionEventTable
	legacyName := PartitionLegacyLeafName(table)
	parent := readContractTableShape(ctx, conn, table)
	legacy := readContractTableShape(ctx, conn, legacyName)
	if parent == nil || parent.relkind != "p" {
		return nil, fmt.Errorf("%s is not partitioned", table)
	}
	if legacy == nil {
		return nil, fmt.Errorf("%s has no legacy partition", table)
	}
	leaves, err := providerWorkSessionEventPartitionFamily.ListLeaves(ctx, conn, table)
	if err != nil {
		return nil, err
	}
	var problems []string
	var weekly []string
	for _, leaf := range leaves {
		if leaf.Name == legacyName {
			continue
		}
		var empty bool
		Raise(conn.QueryRow(ctx, fmt.Sprintf(`SELECT NOT EXISTS (SELECT 1 FROM public.%s)`, quoteIdentifier(leaf.Name))).Scan(&empty))
		if !empty {
			problems = append(problems, fmt.Sprintf("%s holds events; the swap can no longer be undone", leaf.Name))
		}
		weekly = append(weekly, leaf.Name)
	}
	if 0 < len(problems) {
		return nil, fmt.Errorf("rollback preconditions: %s", strings.Join(problems, "; "))
	}
	return providerWorkSessionRollbackSql(parent, legacy, weekly)
}

// The rollback statements for the parent, its legacy partition and its empty
// weekly leaves: the parent's triggers go (a row trigger's clones with it),
// every leaf is detached and the weekly ones dropped, and the legacy partition
// becomes the table again under its published index names, with the row guard
// recreated and its own TRUNCATE guard kept.
func providerWorkSessionRollbackSql(parent *contractTableShape, legacy *contractTableShape, weekly []string) (statements []string, err error) {
	table := providerWorkSessionEventTable
	legacyName := PartitionLegacyLeafName(table)
	for _, trigger := range parent.triggers {
		statements = append(statements, fmt.Sprintf("DROP TRIGGER %s ON public.%s", quoteIdentifier(trigger.name), quoteIdentifier(table)))
	}
	for _, name := range weekly {
		statements = append(statements,
			fmt.Sprintf("ALTER TABLE public.%s DETACH PARTITION public.%s", quoteIdentifier(table), quoteIdentifier(name)),
			fmt.Sprintf("DROP TABLE public.%s", quoteIdentifier(name)),
		)
	}
	statements = append(statements,
		fmt.Sprintf("ALTER TABLE public.%s DETACH PARTITION public.%s", quoteIdentifier(table), quoteIdentifier(legacyName)),
		fmt.Sprintf("DROP TABLE public.%s", quoteIdentifier(table)),
		fmt.Sprintf("ALTER TABLE public.%s RENAME TO %s", quoteIdentifier(legacyName), quoteIdentifier(table)),
		fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT %s", quoteIdentifier(table), quoteIdentifier(providerWorkSessionLegacyBound())),
	)
	for _, index := range legacy.indexes {
		if name, ok := strings.CutSuffix(index.name, contractLegacyIndexSuffix); ok {
			statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(index.name), quoteIdentifier(name)))
		}
	}
	for _, trigger := range parent.triggers {
		if trigger.truncate && slices.ContainsFunc(legacy.triggers, func(legacyTrigger contractTrigger) bool {
			return legacyTrigger.name == trigger.name
		}) {
			// the legacy partition kept its own guard
			continue
		}
		statements = append(statements, trigger.definition)
		statements = append(statements, contractTriggerEnabledSql(table, trigger)...)
	}
	return statements, nil
}

// Locks the parent, and with it every partition, before reading the
// inventory, checking emptiness and planning the drops, all in one
// transaction; a retry repeats the whole transaction with fresh reads.
func (self *providerWorkSessionCutover) rollback(ctx context.Context) error {
	if !isProviderWorkSessionEventPartitioned(ctx) {
		return errors.New("provider_work_session_event is not partitioned")
	}
	lockStatements := append(self.exclusiveBoundsSql(), fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(providerWorkSessionEventTable)))
	if self.dryRun {
		var statements []string
		var err error
		MaintenanceDb(ctx, func(conn PgConn) {
			statements, err = providerWorkSessionRollbackSqlInConn(ctx, conn)
		})
		if err != nil {
			return err
		}
		self.logf("would lock and recheck every partition, then roll back in one transaction:\n%s", strings.Join(append(lockStatements, statements...), ";\n"))
		self.logf("dry run: nothing changed")
		return nil
	}
	err := self.retryExclusive(ctx, func() error {
		return contractPartitionTx(ctx, func(tx PgTx) {
			for _, statement := range lockStatements {
				RaisePgResult(tx.Exec(ctx, statement))
			}
			// read committed reads the catalog after the lock, including
			// events and partitions committed while it waited
			statements, err := providerWorkSessionRollbackSqlInConn(ctx, tx)
			Raise(err)
			for _, statement := range statements {
				RaisePgResult(tx.Exec(ctx, statement))
			}
		})
	})
	if err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	self.logf("rolled back: %s is a plain table again, with every event written since the swap", providerWorkSessionEventTable)
	return nil
}

// Drops the staged parent and its weekly partitions, and the legacy bound.
func (self *providerWorkSessionCutover) abortSql() []string {
	return append(self.exclusiveBoundsSql(),
		fmt.Sprintf("DROP TABLE IF EXISTS public.%s", quoteIdentifier(providerWorkSessionStagedParent())),
		fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT IF EXISTS %s", quoteIdentifier(providerWorkSessionEventTable), quoteIdentifier(providerWorkSessionLegacyBound())),
	)
}

func (self *providerWorkSessionCutover) abort(ctx context.Context) error {
	if isProviderWorkSessionEventPartitioned(ctx) {
		return errors.New("provider_work_session_event is partitioned; use rollback")
	}
	statements := self.abortSql()
	if self.dryRun {
		self.logf("would run:\n%s", strings.Join(statements, ";\n"))
		return nil
	}
	if err := self.retryExclusive(ctx, func() error {
		return contractPartitionExec(ctx, statements)
	}); err != nil {
		return fmt.Errorf("abort: %w", err)
	}
	self.logf("aborted the cutover of %s", providerWorkSessionEventTable)
	return nil
}

// URNETWORK_TEST_PROVIDER_WORK_SESSION_PARTITIONS=1 runs every test on the
// partitioned journal: see Testing_PartitionProviderWorkSessionEvents.
func testProviderWorkSessionPartitions() bool {
	switch strings.ToLower(os.Getenv("URNETWORK_TEST_PROVIDER_WORK_SESSION_PARTITIONS")) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// Cuts a migrated test database's journal over to weekly partitions from the
// current week, as Testing_PartitionContracts does for the contract tables:
// earlier events stay in the legacy partition and current ones go to the
// week's partition. Events already in the table must be from before the
// current week.
func Testing_PartitionProviderWorkSessionEvents(ctx context.Context) {
	Testing_PartitionProviderWorkSessionEventsAt(ctx, PartitionGranularityWeekly.RangeStart(NowUtc()))
}

// As Testing_PartitionProviderWorkSessionEvents, from cutoverWeek.
func Testing_PartitionProviderWorkSessionEventsAt(ctx context.Context, cutoverWeek time.Time) {
	if isProviderWorkSessionEventPartitioned(ctx) {
		return
	}
	cutover := newProviderWorkSessionCutover(DefaultProviderWorkSessionPartitionSettings(), false, func(string, ...any) {})
	Raise(cutover.prepareForWeek(ctx, cutoverWeek))
	Raise(cutover.finalize(ctx, false))
}
