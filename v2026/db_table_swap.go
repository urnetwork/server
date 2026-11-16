// The table swap (PARTITION-CANDIDATES.md §4.4): a queue table whose live rows
// are few but whose files stay at their high-water mark is copied into a fresh
// twin, and the names are swapped in one short transaction. The old file is
// kept under a dated name for a rollback window and dropped whole by a later,
// separate step.
//
// A table declares itself once, statically, with a profile
// (RegisterTableSwapProfile): the table, what must be stopped first, the
// profile's own checks (for pending_task, the task package's live owner
// check) and its lock bounds. Everything else is read from the catalog: the
// columns, their defaults, generated expressions and NOT NULL and CHECK
// constraints, the indexes and their constraints, the storage settings, the
// owner, the grants and the owned sequences.
//
// The steps of the command (bringyourctl model migrate table-swap):
//   - swap (the default): the preflight, a rehearsal and the swap.
//   - The preflight refuses a table this tool cannot copy faithfully
//     (tableSwapShape.refusals: triggers, rules, policies, publications,
//     foreign keys, inheritance, partitioning, identity columns, dependent
//     views or functions, extended statistics, column grants, a tablespace, a
//     replica identity, an invalid index, a deferrable or exclusion
//     constraint, no primary key), a relation under the twin's name, an old
//     table still kept from an earlier swap, a client transaction older than
//     MaxOpenTransactionAge, and whatever the profile's preflight refuses.
//   - The rehearsal runs the whole copy in a transaction that is rolled back,
//     holding only ACCESS SHARE on the table: the twin, the copy with its row
//     count and checksums, the index builds, and the twin compared with the
//     table column for column and index for index. It changes nothing,
//     measures how long the swap will hold its lock, leaves the table's live
//     pages in shared buffers, and reads the table's catalog fingerprint
//     (tableSwapFingerprintSql). A dry run rehearses even while the profile's
//     state refuses, so the lock's length is known ahead of a stop.
//   - The swap is one script sent in one message, BEGIN to COMMIT, so its lock
//     is held for the database's own work and no network round trip. In read
//     committed with LockTimeout and StatementTimeout it takes ACCESS
//     EXCLUSIVE on the table, checks that the table's fingerprint is the one
//     the rehearsal read and that no old table is kept, runs the profile's
//     locked check, creates the twin <table>_next without indexes, copies
//     every row through the primary key (sequential scans off: the live rows
//     are a small share of a large heap), checks the row count and two
//     order-independent 64-bit checksums over every column, generated ones
//     included, builds the indexes under <index>_next with their constraints,
//     checks the twin's fingerprint against the table's, moves the owned
//     sequences to the twin, renames the table and its indexes to
//     <name>_old_<stamp> and the twin and its indexes to the published names,
//     and commits. Any failed check raises and rolls everything back. ANALYZE
//     follows the commit.
//   - rollback: the swap in reverse into the kept old table, so its relation
//     comes back with the rows written since: a first transaction empties it
//     and drops its indexes under its own lock only (nothing reads it), and
//     the swap script copies the live rows into it, rebuilds its indexes and
//     swaps the names. The table that was swapped in is kept under a dated
//     name in its turn.
//   - drop old: drops each kept old table at least DropOldMinAge old, under
//     its own lock only.
//
// Copied rows carry the swap's transaction id. A read committed statement
// that waited on the swap's lock plans and takes its snapshot after the
// commit, so it sees them. A transaction whose snapshot is older than the
// commit and which touches the table only after it does not: an ON CONFLICT
// on such a row raises a serialization failure, which server.Tx reruns, and
// an UPDATE by key finds nothing. The profile's stop and the open transaction
// check bound that window; PARTITION-CANDIDATES.md §4.4 has the analysis for
// pending_task.
//
// Every step reruns safely: the swap is one transaction, so an interrupted run
// changes nothing, and a committed one leaves the old table, which the next
// run finds and reports. A dry run reports the preflight and the rehearsal and
// changes nothing.
package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// One table the swap may rotate, and what its swap needs.
type TableSwapProfile struct {
	// The table, in the public schema.
	Table string
	// What must be stopped before the swap, and how to check it, for the
	// dry run's report.
	Stop string
	// The profile's own checks before any lock: findings to report, and an
	// error that refuses the swap. nil checks nothing more.
	Preflight func(ctx context.Context, conn PgQuerier) (findings []string, err error)
	// One statement the swap script runs under the table's ACCESS EXCLUSIVE
	// lock, which raises to refuse: the preflight's state, checked again where
	// nothing can change it. nil runs none.
	LockedCheckSql func() string
	// nil uses DefaultTableSwapSettings.
	Settings *TableSwapSettings
}

// Tunables of a swap. Durations must be positive (database timeouts at least a
// millisecond), attempts at least one and the copy bound positive.
type TableSwapSettings struct {
	// The lock wait of each lock the swap and the drop take. Every write to
	// the table queues behind the swap's ACCESS EXCLUSIVE request, and a
	// main-pool transaction gives up after PgLockTimeout, so the wait stays
	// well below it; a busy attempt is retried.
	LockTimeout time.Duration
	// Bounds each statement of the swap and of the rehearsal; the copy is the
	// longest.
	StatementTimeout time.Duration
	// Attempts of the swap and of the drop while their locks are busy, and the
	// wait between them.
	LockAttempts      int
	LockRetryInterval time.Duration
	// An attempt waits while a transaction older than this holds a lock on the
	// table, or a vacuum of it runs, so its lock request never queues writers
	// behind them.
	MaxTransactionAge time.Duration
	// The preflight refuses while a client transaction older than this is
	// open anywhere in the database: its snapshot predates the swap, and it
	// could touch the table after the commit and miss the copied rows.
	MaxOpenTransactionAge time.Duration
	// The most rows the swap copies under its lock; more refuses the swap.
	MaxCopyRows int64
	// The swap refuses when the rehearsal of its copy took longer than this:
	// the swap would hold its lock about that long.
	MaxRehearsalDuration time.Duration
	// The memory of the copy's checksums and of the index builds, in bytes.
	WorkMemBytes int64
	// Drop old refuses an old table younger than this, by its name's stamp
	// and the database's clock: the rollback window.
	DropOldMinAge time.Duration
	// How long a failed script's ROLLBACK may take before its session is
	// discarded instead.
	RollbackTimeout time.Duration
}

// Half a second of lock wait, a quarter of a writer's own bound; seconds for
// the copy; a day's rollback window.
func DefaultTableSwapSettings() *TableSwapSettings {
	return &TableSwapSettings{
		LockTimeout:           500 * time.Millisecond,
		StatementTimeout:      30 * time.Second,
		LockAttempts:          60,
		LockRetryInterval:     2 * time.Second,
		MaxTransactionAge:     5 * time.Second,
		MaxOpenTransactionAge: 2 * time.Minute,
		MaxCopyRows:           1_000_000,
		MaxRehearsalDuration:  10 * time.Second,
		WorkMemBytes:          256 * 1024 * 1024,
		DropOldMinAge:         24 * time.Hour,
		RollbackTimeout:       10 * time.Second,
	}
}

func (self *TableSwapSettings) validate() error {
	for name, value := range map[string]time.Duration{
		"LockTimeout":           self.LockTimeout,
		"StatementTimeout":      self.StatementTimeout,
		"LockRetryInterval":     self.LockRetryInterval,
		"MaxTransactionAge":     self.MaxTransactionAge,
		"MaxOpenTransactionAge": self.MaxOpenTransactionAge,
		"MaxRehearsalDuration":  self.MaxRehearsalDuration,
		"DropOldMinAge":         self.DropOldMinAge,
		"RollbackTimeout":       self.RollbackTimeout,
	} {
		if value < time.Millisecond {
			return fmt.Errorf("table swap setting %s must be at least a millisecond: %s", name, value)
		}
	}
	if self.LockAttempts < 1 {
		return fmt.Errorf("table swap setting LockAttempts must be at least one: %d", self.LockAttempts)
	}
	if self.MaxCopyRows < 1 {
		return fmt.Errorf("table swap setting MaxCopyRows must be positive: %d", self.MaxCopyRows)
	}
	if self.WorkMemBytes < 64*1024 {
		return fmt.Errorf("table swap setting WorkMemBytes must be at least 64 kB: %d", self.WorkMemBytes)
	}
	return nil
}

type TableSwapOptions struct {
	// The table, which must have a registered profile.
	Table    string
	DryRun   bool
	Rollback bool
	DropOld  bool
	Logf     func(string, ...any)
	// nil uses the profile's settings.
	Settings *TableSwapSettings
	// runs on the swap's connection, inside its transaction, after the copy
	// and before the renames; tests interrupt the swap there
	afterCopy func(context.Context, PgConn)
}

// The suffix of the twin a swap builds, and of its indexes while they are
// built.
const tableSwapNextSuffix = "_next"

// The infix of a kept old table and its indexes: <name>_old_<stamp>, the stamp
// the database's UTC clock at the swap, YYYYMMDDHHMMSS.
const tableSwapOldInfix = "_old_"

const tableSwapStampLayout = "20060102150405"

var tableSwapStampPattern = regexp.MustCompile(`^[0-9]{14}$`)

// The transaction-local settings a swap script keeps its report in: the rows
// it copied, and the times its steps started, read back before its commit.
const (
	tableSwapRowsSetting      = "urnetwork.table_swap_rows"
	tableSwapStartedAtSetting = "urnetwork.table_swap_started_at"
	tableSwapLockedAtSetting  = "urnetwork.table_swap_locked_at"
	tableSwapCopiedAtSetting  = "urnetwork.table_swap_copied_at"
	tableSwapIndexedAtSetting = "urnetwork.table_swap_indexed_at"
)

// The registered profiles, by table. Registration happens at package
// initialization; reads come later and from any goroutine.
type tableSwapRegistry struct {
	stateLock sync.Mutex
	profiles  map[string]*TableSwapProfile
}

var tableSwapProfiles = &tableSwapRegistry{profiles: map[string]*TableSwapProfile{}}

var tableSwapTablePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Registers a table's profile, at package initialization. Panics on an invalid
// or repeated table.
func RegisterTableSwapProfile(profile *TableSwapProfile) *TableSwapProfile {
	if !tableSwapTablePattern.MatchString(profile.Table) {
		panic(fmt.Errorf("table swap profile has an invalid table name: %q", profile.Table))
	}
	if limit := pgMaxIdentifierLength - len(tableSwapOldInfix) - len(tableSwapStampLayout); limit < len(profile.Table) {
		panic(fmt.Errorf("table swap profile table %s is longer than %d bytes, so its old name would not fit", profile.Table, limit))
	}
	if profile.Settings != nil {
		if err := profile.Settings.validate(); err != nil {
			panic(err)
		}
	}
	tableSwapProfiles.stateLock.Lock()
	defer tableSwapProfiles.stateLock.Unlock()
	if _, ok := tableSwapProfiles.profiles[profile.Table]; ok {
		panic(fmt.Errorf("table swap profile %s is registered twice", profile.Table))
	}
	tableSwapProfiles.profiles[profile.Table] = profile
	return profile
}

// Every registered profile, by table name.
func TableSwapProfiles() []*TableSwapProfile {
	tableSwapProfiles.stateLock.Lock()
	defer tableSwapProfiles.stateLock.Unlock()
	profiles := make([]*TableSwapProfile, 0, len(tableSwapProfiles.profiles))
	for _, profile := range tableSwapProfiles.profiles {
		profiles = append(profiles, profile)
	}
	slices.SortFunc(profiles, func(a, b *TableSwapProfile) int {
		return strings.Compare(a.Table, b.Table)
	})
	return profiles
}

func tableSwapProfile(table string) *TableSwapProfile {
	tableSwapProfiles.stateLock.Lock()
	defer tableSwapProfiles.stateLock.Unlock()
	return tableSwapProfiles.profiles[table]
}

// The old table name of a swap at stamp.
func tableSwapOldName(name string, stamp string) string {
	return name + tableSwapOldInfix + stamp
}

func tableSwapNextName(name string) string {
	return name + tableSwapNextSuffix
}

// Whether name is a kept old table of a registered profile, or the twin of one,
// with the profile's table: relations a swap keeps beside its table, which db
// audit and the REINDEX rotation leave alone.
func TableSwapRelation(name string) (table string, ok bool) {
	for _, profile := range TableSwapProfiles() {
		if name == profile.Table+tableSwapNextSuffix {
			return profile.Table, true
		}
		if stamp, found := strings.CutPrefix(name, profile.Table+tableSwapOldInfix); found && tableSwapStampPattern.MatchString(stamp) {
			return profile.Table, true
		}
	}
	return "", false
}

// The command: swap, rollback or drop old, each rehearsed with DryRun.
func SwapTable(ctx context.Context, options TableSwapOptions) error {
	profile := tableSwapProfile(options.Table)
	if profile == nil {
		names := []string{}
		for _, profile := range TableSwapProfiles() {
			names = append(names, profile.Table)
		}
		return fmt.Errorf("no table swap profile for %q (profiles: %s)", options.Table, strings.Join(names, ", "))
	}
	if options.Rollback && options.DropOld {
		return errors.New("choose one of rollback and drop old")
	}
	settings := options.Settings
	if settings == nil {
		settings = profile.Settings
	}
	if settings == nil {
		settings = DefaultTableSwapSettings()
	}
	if err := settings.validate(); err != nil {
		return err
	}
	swap := &tableSwap{
		profile:   profile,
		settings:  settings,
		dryRun:    options.DryRun,
		logf:      options.Logf,
		afterCopy: options.afterCopy,
	}
	if swap.logf == nil {
		swap.logf = func(string, ...any) {}
	}
	switch {
	case options.DropOld:
		return swap.dropOld(ctx)
	case options.Rollback:
		return swap.rollback(ctx)
	default:
		return swap.swap(ctx)
	}
}

type tableSwap struct {
	profile   *TableSwapProfile
	settings  *TableSwapSettings
	dryRun    bool
	logf      func(string, ...any)
	afterCopy func(context.Context, PgConn)
}

// One kept old table, read from the catalog.
type tableSwapOldTable struct {
	name      string
	stamp     time.Time
	byteCount int64
}

// The kept old tables of the profile's table, oldest first, and the
// database's clock.
func (self *tableSwap) readOldTables(ctx context.Context, conn PgQuerier) (oldTables []tableSwapOldTable, now time.Time) {
	Raise(conn.QueryRow(ctx, `SELECT now() AT TIME ZONE 'UTC'`).Scan(&now))
	result, err := conn.Query(
		ctx,
		`
		SELECT c.relname, pg_total_relation_size(c.oid)
		FROM pg_class c
		WHERE c.relnamespace = 'public'::regnamespace
			AND c.relkind IN ('r', 'p')
			AND c.relname LIKE $1
		ORDER BY c.relname
		`,
		strings.ReplaceAll(self.profile.Table, "_", `\_`)+`\_old\_%`,
	)
	WithPgResult(result, err, func() {
		for result.Next() {
			var oldTable tableSwapOldTable
			Raise(result.Scan(&oldTable.name, &oldTable.byteCount))
			stamp, ok := strings.CutPrefix(oldTable.name, self.profile.Table+tableSwapOldInfix)
			if !ok || !tableSwapStampPattern.MatchString(stamp) {
				continue
			}
			oldTable.stamp, err = time.ParseInLocation(tableSwapStampLayout, stamp, time.UTC)
			Raise(err)
			oldTables = append(oldTables, oldTable)
		}
	})
	return
}

func tableSwapGibString(byteCount int64) string {
	return fmt.Sprintf("%.2f GiB", float64(byteCount)/float64(1024*1024*1024))
}

// The checks before any lock. Shape refusals are about the table itself: what
// the swap cannot copy faithfully, the names it would give, and a relation
// already under its twin's name; the copy cannot even be rehearsed. State
// refusals are about now: the kept old tables, open transactions and the
// profile's own preflight (a running taskworker, for pending_task); a dry run
// still rehearses the copy and reports them.
func (self *tableSwap) preflight(ctx context.Context, rollback bool) (shape *tableSwapShape, oldTables []tableSwapOldTable, shapeRefusals []string, stateRefusals []string, err error) {
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			var now time.Time
			oldTables, now = self.readOldTables(ctx, conn)
			shape = readTableSwapShape(ctx, conn, self.profile.Table)
			if shape == nil {
				shapeRefusals = append(shapeRefusals, fmt.Sprintf("%s does not exist", self.profile.Table))
				return
			}
			shapeRefusals = append(shapeRefusals, shape.refusals...)
			shapeRefusals = append(shapeRefusals, tableSwapNameRefusals(self.profile.Table, shape)...)
			if next := readTableSwapShape(ctx, conn, self.profile.Table+tableSwapNextSuffix); next != nil {
				shapeRefusals = append(shapeRefusals, fmt.Sprintf("%s exists; the swap builds its twin under that name and drops nothing it did not make", self.profile.Table+tableSwapNextSuffix))
			}
			for _, oldTable := range oldTables {
				self.logf(
					"kept old table %s (%s), swapped at %s, %s ago",
					oldTable.name,
					tableSwapGibString(oldTable.byteCount),
					oldTable.stamp.Format(time.RFC3339),
					now.Sub(oldTable.stamp).Round(time.Second),
				)
			}
			switch {
			case rollback && len(oldTables) == 0:
				stateRefusals = append(stateRefusals, fmt.Sprintf("no kept old table of %s to roll back to", self.profile.Table))
			case rollback && 1 < len(oldTables):
				stateRefusals = append(stateRefusals, fmt.Sprintf("%d kept old tables of %s; a rollback needs exactly one", len(oldTables), self.profile.Table))
			case !rollback && 0 < len(oldTables):
				stateRefusals = append(stateRefusals, fmt.Sprintf("the swap of %s at %s is done and its old table is kept: run --rollback, or --drop-old once the rollback window has passed", self.profile.Table, oldTables[len(oldTables)-1].stamp.Format(time.RFC3339)))
			}
			stateRefusals = append(stateRefusals, self.openTransactionRefusals(ctx, conn)...)
			if self.profile.Preflight != nil {
				findings, profileErr := self.profile.Preflight(ctx, conn)
				for _, finding := range findings {
					self.logf("%s", finding)
				}
				if profileErr != nil {
					stateRefusals = append(stateRefusals, profileErr.Error())
				}
			}
		})
	})
	return
}

// Client transactions older than MaxOpenTransactionAge anywhere in the
// database, other than this session's.
func (self *tableSwap) openTransactionRefusals(ctx context.Context, conn PgQuerier) (refusals []string) {
	result, err := conn.Query(
		ctx,
		`
		SELECT pid, coalesce(usename, ''), extract(epoch FROM now() - xact_start)::bigint,
			left(regexp_replace(coalesce(query, ''), '\s+', ' ', 'g'), 80)
		FROM pg_stat_activity
		WHERE datname = current_database()
			AND backend_type = 'client backend'
			AND pid <> pg_backend_pid()
			AND xact_start < now() - make_interval(secs => $1)
		ORDER BY xact_start
		LIMIT 10
		`,
		self.settings.MaxOpenTransactionAge.Seconds(),
	)
	WithPgResult(result, err, func() {
		for result.Next() {
			var pid int64
			var user string
			var ageSeconds int64
			var query string
			Raise(result.Scan(&pid, &user, &ageSeconds, &query))
			refusals = append(refusals, fmt.Sprintf("a transaction older than %s is open: pid %d (%s), %ds: %s", self.settings.MaxOpenTransactionAge, pid, user, ageSeconds, query))
		}
	})
	return
}

// What keeps the swap from requesting its lock now: transactions older than
// MaxTransactionAge holding a lock on relations, and a vacuum of one of them,
// which its lock request would queue every writer behind. Also the database's
// clock, for the old table's stamp.
func (self *tableSwap) busy(ctx context.Context, relations []string) (reasons []string, now time.Time, err error) {
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT now() AT TIME ZONE 'UTC'`).Scan(&now))
			if busy := partitionRelationsBusy(ctx, conn, relations, self.settings.MaxTransactionAge); 0 < busy {
				reasons = append(reasons, fmt.Sprintf("%d transaction(s) older than %s hold locks on %s", busy, self.settings.MaxTransactionAge, strings.Join(relations, ", ")))
			}
			var vacuums int
			Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM pg_stat_progress_vacuum WHERE relid = ANY (ARRAY(SELECT to_regclass(name) FROM unnest($1::text[]) name))`,
				relations,
			).Scan(&vacuums))
			if 0 < vacuums {
				reasons = append(reasons, fmt.Sprintf("%d vacuum(s) of %s running", vacuums, strings.Join(relations, ", ")))
			}
		})
	})
	return
}

// Runs one exclusive step until it succeeds, fails for a reason other than a
// busy lock, or runs out of attempts. Each attempt first waits for relations
// to be free of long transactions and vacuums, and repeats the whole step with
// the database's clock read then.
func (self *tableSwap) retryExclusive(ctx context.Context, relations []string, run func(now time.Time) error) (err error) {
	for attempt := 1; attempt <= self.settings.LockAttempts; attempt += 1 {
		var reasons []string
		var now time.Time
		if reasons, now, err = self.busy(ctx, relations); err != nil {
			return
		}
		if 0 < len(reasons) {
			self.logf("waiting (attempt %d/%d): %s", attempt, self.settings.LockAttempts, strings.Join(reasons, "; "))
			err = fmt.Errorf("busy: %s", strings.Join(reasons, "; "))
		} else {
			err = run(now)
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

// The bounds of a swap or rehearsal transaction.
func (self *tableSwap) boundsSql() []string {
	return []string{
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds()),
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.settings.StatementTimeout.Milliseconds()),
		fmt.Sprintf("SET LOCAL work_mem = '%dkB'", self.settings.WorkMemBytes/1024),
		fmt.Sprintf("SET LOCAL maintenance_work_mem = '%dkB'", self.settings.WorkMemBytes/1024),
		// the live rows are a small share of the heap: read them through the
		// primary key
		"SET LOCAL enable_seqscan = off",
	}
}

func tableSwapMarkSql(setting string) string {
	return fmt.Sprintf("SELECT set_config('%s', clock_timestamp()::text, true)", setting)
}

// Raises message unless condition holds, as one statement of a script.
func tableSwapAssertSql(condition string, message string) string {
	return fmt.Sprintf("DO $table_swap$ BEGIN IF NOT coalesce(%s, false) THEN RAISE EXCEPTION '%%', %s; END IF; END $table_swap$", condition, tableSwapLiteral(message))
}

// The copy into target, empty and unindexed, and everything after it but the
// renames: the copy with its count and checksums, the index builds under
// their next names, and, with assertTwin, the twin's fingerprint checked
// against the table's. Each step's start is marked for the report.
func (self *tableSwap) copyScript(source *tableSwapShape, target string, assertTwin bool) []string {
	statements := []string{
		tableSwapAssertSql(
			fmt.Sprintf("NOT EXISTS (SELECT 1 FROM public.%s) AND NOT EXISTS (SELECT 1 FROM pg_index WHERE indrelid = %s::regclass)", quoteIdentifier(target), tableSwapLiteral("public."+quoteIdentifier(target))),
			target+" is not the empty, unindexed twin the copy needs",
		),
		tableSwapMarkSql(tableSwapStartedAtSetting),
		source.copySql(target, self.settings.MaxCopyRows),
		tableSwapMarkSql(tableSwapCopiedAtSetting),
	}
	statements = append(statements, source.indexSql(target, tableSwapNextName)...)
	if assertTwin {
		statements = append(statements, tableSwapAssertSql(
			fmt.Sprintf("%s = %s", tableSwapFingerprintSql(target, tableSwapNextSuffix), tableSwapFingerprintSql(source.name, "")),
			target+" does not match "+source.name+"; a dry run reports how",
		))
	}
	statements = append(statements, tableSwapMarkSql(tableSwapIndexedAtSetting))
	return statements
}

// The renames of a swap into twin: owned sequences move to it, the table and
// its indexes take their old names, and the twin and its indexes take the
// published ones.
func (self *tableSwap) renameSql(source *tableSwapShape, twin string, stamp string) []string {
	statements := []string{}
	for _, sequence := range source.sequences {
		statements = append(statements, fmt.Sprintf(
			"ALTER SEQUENCE %s OWNED BY public.%s.%s",
			sequence.qualifiedName, quoteIdentifier(twin), quoteIdentifier(sequence.column),
		))
	}
	statements = append(statements, fmt.Sprintf(
		"ALTER TABLE public.%s RENAME TO %s",
		quoteIdentifier(source.name), quoteIdentifier(tableSwapOldName(self.profile.Table, stamp)),
	))
	for _, index := range source.indexes {
		statements = append(statements, fmt.Sprintf(
			"ALTER INDEX public.%s RENAME TO %s",
			quoteIdentifier(index.name), quoteIdentifier(tableSwapOldName(index.name, stamp)),
		))
	}
	statements = append(statements, fmt.Sprintf(
		"ALTER TABLE public.%s RENAME TO %s",
		quoteIdentifier(twin), quoteIdentifier(self.profile.Table),
	))
	for _, index := range source.indexes {
		statements = append(statements, fmt.Sprintf(
			"ALTER INDEX public.%s RENAME TO %s",
			quoteIdentifier(tableSwapNextName(index.name)), quoteIdentifier(index.name),
		))
	}
	return statements
}

// The report a swap or rehearsal reads back before its end: the rows copied,
// the copy's and the index builds' durations, and how long the lock has been
// held, in seconds.
const tableSwapReportSql = `SELECT
	current_setting('` + tableSwapRowsSetting + `')::bigint,
	extract(epoch FROM current_setting('` + tableSwapCopiedAtSetting + `')::timestamptz - current_setting('` + tableSwapStartedAtSetting + `')::timestamptz)::float8,
	extract(epoch FROM current_setting('` + tableSwapIndexedAtSetting + `')::timestamptz - current_setting('` + tableSwapCopiedAtSetting + `')::timestamptz)::float8,
	extract(epoch FROM clock_timestamp() - current_setting('` + tableSwapLockedAtSetting + `')::timestamptz)::float8`

// What one copy did, and how long its steps took, as the database measured
// them.
type tableSwapCopyReport struct {
	rowCount      int64
	copyDuration  time.Duration
	indexDuration time.Duration
	lockDuration  time.Duration
}

func (self *tableSwapCopyReport) String() string {
	return fmt.Sprintf("%d rows copied in %s, indexes built in %s", self.rowCount, self.copyDuration.Round(time.Millisecond), self.indexDuration.Round(time.Millisecond))
}

func tableSwapSeconds(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

// Parses the report from its four text values.
func parseTableSwapReport(values []string) (report tableSwapCopyReport, err error) {
	if len(values) != 4 {
		return report, fmt.Errorf("the swap's report has %d values", len(values))
	}
	if report.rowCount, err = strconv.ParseInt(values[0], 10, 64); err != nil {
		return
	}
	seconds := make([]float64, 3)
	for i := range seconds {
		if seconds[i], err = strconv.ParseFloat(values[i+1], 64); err != nil {
			return
		}
	}
	report.copyDuration = tableSwapSeconds(seconds[0])
	report.indexDuration = tableSwapSeconds(seconds[1])
	report.lockDuration = tableSwapSeconds(seconds[2])
	return
}

var errTableSwapRehearsed = errors.New("rehearsed")

// The whole copy into a twin in a transaction that is rolled back, holding
// only ACCESS SHARE on the table, then the twin compared with the table
// column for column and index for index. Returns the table's fingerprint, as
// the swap's lock must find it.
func (self *tableSwap) rehearse(ctx context.Context, source *tableSwapShape) (report tableSwapCopyReport, sourceFingerprint string, err error) {
	twin := self.profile.Table + tableSwapNextSuffix
	err = contractPartitionTx(ctx, func(tx PgTx) {
		statements := slices.Clone(self.boundsSql())
		statements = append(statements, tableSwapMarkSql(tableSwapLockedAtSetting))
		statements = append(statements, source.twinSql(twin)...)
		statements = append(statements, self.copyScript(source, twin, false)...)
		RaisePgResult(tx.Exec(ctx, strings.Join(statements, ";\n")))
		var rowCount int64
		var copySeconds, indexSeconds, lockSeconds float64
		Raise(tx.QueryRow(ctx, tableSwapReportSql).Scan(&rowCount, &copySeconds, &indexSeconds, &lockSeconds))
		report = tableSwapCopyReport{
			rowCount:      rowCount,
			copyDuration:  tableSwapSeconds(copySeconds),
			indexDuration: tableSwapSeconds(indexSeconds),
		}
		if differences := source.differences(readTableSwapShape(ctx, tx, twin), tableSwapNextName); 0 < len(differences) {
			panic(fmt.Errorf("the rehearsed twin %s does not match %s:\n  - %s", twin, source.name, strings.Join(differences, "\n  - ")))
		}
		var twinFingerprint string
		Raise(tx.QueryRow(ctx, "SELECT "+tableSwapFingerprintSql(source.name, "")+", "+tableSwapFingerprintSql(twin, tableSwapNextSuffix)).Scan(&sourceFingerprint, &twinFingerprint))
		if twinFingerprint != sourceFingerprint {
			panic(fmt.Errorf("the rehearsed twin %s matches %s column for column but not by fingerprint", twin, source.name))
		}
		panic(errTableSwapRehearsed)
	})
	if errors.Is(err, errTableSwapRehearsed) {
		err = nil
	}
	return
}

// Runs a swap script on one maintenance session: the part before the hook
// point, the hook (tests only), then the rest with the report and COMMIT.
// Without a hook the whole script is one message, so the lock it takes is
// held for the database's own work and no network round trip. A failed
// script leaves the session in a failed transaction, which is rolled back
// before the session goes back, or the session is discarded.
func (self *tableSwap) runScript(ctx context.Context, beforeHook []string, afterHook []string) (report tableSwapCopyReport, err error) {
	conn, err := AcquireMaintenanceDbConn(ctx)
	if err != nil {
		return
	}
	committed := false
	defer func() {
		if committed {
			conn.Release()
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), self.settings.RollbackTimeout)
		defer cancel()
		if _, rollbackErr := conn.Exec(rollbackCtx, "ROLLBACK"); rollbackErr != nil {
			discardPgConnection(rollbackCtx, conn)
			return
		}
		conn.Release()
	}()
	// the last row of a four-column result: the report
	exec := func(statements []string) (reportValues []string, execErr error) {
		results, execErr := conn.Conn().PgConn().Exec(ctx, strings.Join(statements, ";\n")).ReadAll()
		if execErr != nil {
			return
		}
		for _, result := range results {
			if result.Err != nil {
				return nil, result.Err
			}
			if len(result.FieldDescriptions) == 4 && len(result.Rows) == 1 {
				reportValues = []string{}
				for _, value := range result.Rows[0] {
					reportValues = append(reportValues, string(value))
				}
			}
		}
		return
	}
	if self.afterCopy != nil {
		if _, err = exec(beforeHook); err != nil {
			return
		}
		if err = partitionRegistryRun(func() { self.afterCopy(ctx, conn) }); err != nil {
			return
		}
	} else {
		afterHook = append(slices.Clone(beforeHook), afterHook...)
	}
	reportValues, err := exec(append(slices.Clone(afterHook), tableSwapReportSql, "COMMIT"))
	if err != nil {
		return
	}
	committed = true
	report, err = parseTableSwapReport(reportValues)
	return
}

// The swap script into target, split at the hook point after the copy: BEGIN,
// the bounds, the lock, the checks under it, the twin (forward) or the kept
// old table's emptiness (rollback, with oldTable), the copy and index builds
// with their checks; then the renames.
func (self *tableSwap) swapScript(source *tableSwapShape, target string, stamp string, sourceFingerprint string, oldTable *tableSwapOldTable) (beforeHook []string, afterHook []string) {
	table := self.profile.Table
	locked := []string{"public." + quoteIdentifier(table)}
	if oldTable != nil {
		locked = append(locked, "public."+quoteIdentifier(oldTable.name))
	}
	beforeHook = []string{"BEGIN ISOLATION LEVEL READ COMMITTED"}
	beforeHook = append(beforeHook, self.boundsSql()...)
	beforeHook = append(beforeHook,
		fmt.Sprintf("LOCK TABLE %s IN ACCESS EXCLUSIVE MODE", strings.Join(locked, ", ")),
		tableSwapMarkSql(tableSwapLockedAtSetting),
		tableSwapAssertSql(
			fmt.Sprintf("%s = %s", tableSwapFingerprintSql(table, ""), tableSwapLiteral(sourceFingerprint)),
			table+" changed since the rehearsal; run the swap again",
		),
	)
	// a swap keeps no old table beside the one it makes; a rollback, only its own
	oldTablesCondition := fmt.Sprintf(
		"NOT EXISTS (SELECT 1 FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relname ~ %s",
		tableSwapLiteral("^"+regexp.QuoteMeta(table+tableSwapOldInfix)+"[0-9]{14}$"),
	)
	if oldTable != nil {
		oldTablesCondition += " AND relname <> " + tableSwapLiteral(oldTable.name)
	}
	beforeHook = append(beforeHook, tableSwapAssertSql(oldTablesCondition+")", "an old table of "+table+" is kept"))
	if self.profile.LockedCheckSql != nil {
		if check := self.profile.LockedCheckSql(); check != "" {
			beforeHook = append(beforeHook, check)
		}
	}
	if oldTable == nil {
		beforeHook = append(beforeHook, source.twinSql(target)...)
	}
	beforeHook = append(beforeHook, self.copyScript(source, target, true)...)
	afterHook = self.renameSql(source, target, stamp)
	return
}

// The forward swap.
func (self *tableSwap) swap(ctx context.Context) error {
	table := self.profile.Table
	twin := table + tableSwapNextSuffix
	if self.profile.Stop != "" {
		self.logf("the swap of %s needs: %s", table, self.profile.Stop)
	}
	source, _, shapeRefusals, stateRefusals, err := self.preflight(ctx, false)
	if err != nil {
		return err
	}
	if source != nil {
		self.logf("%s: %s, about %d rows (pg_class), %d columns, %d indexes, %d owned sequence(s)", table, tableSwapGibString(source.byteCount), source.estimatedRows, len(source.columns), len(source.indexes), len(source.sequences))
	}
	refusals := append(slices.Clone(shapeRefusals), stateRefusals...)
	if 0 < len(shapeRefusals) || (0 < len(stateRefusals) && !self.dryRun) {
		if self.dryRun {
			self.logf("dry run: nothing changed")
		}
		return self.refuse("swap", refusals)
	}

	// a dry run rehearses even while the state refuses (a taskworker still
	// running, for pending_task), so the lock's length is known ahead
	report, sourceFingerprint, err := self.rehearse(ctx, source)
	if err != nil {
		return fmt.Errorf("the rehearsal of the copy of %s failed: %w", table, err)
	}
	self.logf("rehearsal, rolled back: %s; the swap holds ACCESS EXCLUSIVE on %s about this long", report.String(), table)
	if self.settings.MaxRehearsalDuration < report.copyDuration+report.indexDuration {
		refusals = append(refusals, fmt.Sprintf("the rehearsal took %s, longer than %s", (report.copyDuration+report.indexDuration).Round(time.Millisecond), self.settings.MaxRehearsalDuration))
	}
	if self.dryRun {
		beforeHook, afterHook := self.swapScript(source, twin, "<stamp>", sourceFingerprint, nil)
		self.logf(
			"would send this script in one message, waiting at most %s for the lock, %d attempts:\n%s",
			self.settings.LockTimeout, self.settings.LockAttempts,
			strings.Join(append(append(slices.Clone(beforeHook), afterHook...), tableSwapReportSql, "COMMIT", "ANALYZE public."+quoteIdentifier(table)), ";\n"),
		)
		self.logf("dry run: nothing changed")
	}
	if 0 < len(refusals) {
		return self.refuse("swap", refusals)
	}
	if self.dryRun {
		return nil
	}

	var stamp string
	var swapped tableSwapCopyReport
	err = self.retryExclusive(ctx, []string{"public." + table}, func(now time.Time) (runErr error) {
		stamp = now.Format(tableSwapStampLayout)
		beforeHook, afterHook := self.swapScript(source, twin, stamp, sourceFingerprint, nil)
		swapped, runErr = self.runScript(ctx, beforeHook, afterHook)
		return
	})
	if err != nil {
		return fmt.Errorf("the swap of %s: %w", table, err)
	}
	self.logf("swapped %s in one transaction: %s; ACCESS EXCLUSIVE held %s before the commit", table, swapped.String(), swapped.lockDuration.Round(time.Millisecond))
	self.analyze(ctx)
	self.logf("the old table is kept as %s for the rollback window; drop it with --drop-old", tableSwapOldName(table, stamp))
	return nil
}

// Statistics for the new table, after the commit; autovacuum would analyze it
// within its next pass, and a failure here is only reported.
func (self *tableSwap) analyze(ctx context.Context) {
	analyzeStart := time.Now()
	err := partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, fmt.Sprintf("ANALYZE public.%s", quoteIdentifier(self.profile.Table))))
		})
	})
	if err != nil {
		self.logf("ANALYZE %s failed, autovacuum analyzes it later: %v", self.profile.Table, err)
		return
	}
	self.logf("analyzed %s in %s", self.profile.Table, time.Since(analyzeStart).Round(time.Millisecond))
}

func (self *tableSwap) refuse(action string, refusals []string) error {
	return fmt.Errorf("the %s of %s refuses:\n  - %s", action, self.profile.Table, strings.Join(refusals, "\n  - "))
}

// The rollback: the swap in reverse into the kept old table.
func (self *tableSwap) rollback(ctx context.Context) error {
	table := self.profile.Table
	if self.profile.Stop != "" {
		self.logf("the rollback of %s needs: %s", table, self.profile.Stop)
	}
	source, oldTables, shapeRefusals, stateRefusals, err := self.preflight(ctx, true)
	if err != nil {
		return err
	}
	if refusals := append(slices.Clone(shapeRefusals), stateRefusals...); 0 < len(refusals) {
		if self.dryRun {
			self.logf("dry run: nothing changed")
		}
		return self.refuse("rollback", refusals)
	}
	oldTable := oldTables[0]

	report, sourceFingerprint, err := self.rehearse(ctx, source)
	if err != nil {
		return fmt.Errorf("the rehearsal of the copy of %s failed: %w", table, err)
	}
	self.logf("rehearsal, rolled back: %s", report.String())
	if self.dryRun {
		self.logf("would empty %s and drop its indexes under its own lock, then copy %s back into it, rebuild its indexes and swap the names in one script", oldTable.name, table)
		self.logf("dry run: nothing changed")
		return nil
	}

	// nothing reads the old table: empty it under its own lock, so the swap
	// transaction has no files to unlink at its commit
	err = self.retryExclusive(ctx, []string{"public." + oldTable.name}, func(time.Time) error {
		return contractPartitionTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds())))
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.settings.StatementTimeout.Milliseconds())))
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(oldTable.name))))
			old := readTableSwapShape(ctx, tx, oldTable.name)
			if old == nil {
				panic(fmt.Errorf("%s is gone", oldTable.name))
			}
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("TRUNCATE public.%s", quoteIdentifier(oldTable.name))))
			for _, statement := range old.dropIndexSql() {
				RaisePgResult(tx.Exec(ctx, statement))
			}
		})
	})
	if err != nil {
		return fmt.Errorf("empty %s: %w", oldTable.name, err)
	}
	self.logf("emptied %s and dropped its indexes", oldTable.name)

	var stamp string
	var swapped tableSwapCopyReport
	err = self.retryExclusive(ctx, []string{"public." + table, "public." + oldTable.name}, func(now time.Time) (runErr error) {
		// the table swapped in is kept under its own stamp, a second or more
		// after the old one's: a stamp has whole seconds
		at := now.Truncate(time.Second)
		if !at.After(oldTable.stamp) {
			at = oldTable.stamp.Add(time.Second)
		}
		stamp = at.Format(tableSwapStampLayout)
		beforeHook, afterHook := self.swapScript(source, oldTable.name, stamp, sourceFingerprint, &oldTable)
		swapped, runErr = self.runScript(ctx, beforeHook, afterHook)
		return
	})
	if err != nil {
		return fmt.Errorf("the rollback of %s: %w", table, err)
	}
	self.logf("rolled %s back into %s: %s; ACCESS EXCLUSIVE held %s before the commit", table, oldTable.name, swapped.String(), swapped.lockDuration.Round(time.Millisecond))
	self.analyze(ctx)
	self.logf("the table that was swapped in is kept as %s; drop it with --drop-old", tableSwapOldName(table, stamp))
	return nil
}

// Drops each kept old table at least DropOldMinAge old, each in its own
// transaction under its own lock: nothing reads an old table.
func (self *tableSwap) dropOld(ctx context.Context) error {
	var oldTables []tableSwapOldTable
	var now time.Time
	err := partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			oldTables, now = self.readOldTables(ctx, conn)
		})
	})
	if err != nil {
		return err
	}
	if len(oldTables) == 0 {
		self.logf("no kept old table of %s", self.profile.Table)
		return nil
	}
	refusals := []string{}
	for _, oldTable := range oldTables {
		if age := now.Sub(oldTable.stamp); age < self.settings.DropOldMinAge {
			refusals = append(refusals, fmt.Sprintf("%s is %s old, inside the rollback window of %s", oldTable.name, age.Round(time.Second), self.settings.DropOldMinAge))
		}
	}
	if 0 < len(refusals) {
		return self.refuse("drop of the old tables", refusals)
	}
	for _, oldTable := range oldTables {
		if self.dryRun {
			self.logf("would drop %s (%s), swapped at %s", oldTable.name, tableSwapGibString(oldTable.byteCount), oldTable.stamp.Format(time.RFC3339))
			continue
		}
		dropStart := time.Now()
		err := self.retryExclusive(ctx, []string{"public." + oldTable.name}, func(time.Time) error {
			return contractPartitionTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds())))
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.settings.StatementTimeout.Milliseconds())))
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(oldTable.name))))
				if current, _ := self.readOldTables(ctx, tx); !slices.ContainsFunc(current, func(t tableSwapOldTable) bool { return t.name == oldTable.name }) {
					panic(fmt.Errorf("%s is no longer a kept old table", oldTable.name))
				}
				// RESTRICT: a sequence the table still owned while the new
				// table's default used it would refuse the drop
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf("DROP TABLE public.%s RESTRICT", quoteIdentifier(oldTable.name))))
			})
		})
		if err != nil {
			return fmt.Errorf("drop %s: %w", oldTable.name, err)
		}
		self.logf("dropped %s (%s) in %s", oldTable.name, tableSwapGibString(oldTable.byteCount), time.Since(dropStart).Round(time.Millisecond))
	}
	if self.dryRun {
		self.logf("dry run: nothing changed")
	}
	return nil
}
