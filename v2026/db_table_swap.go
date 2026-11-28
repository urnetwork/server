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
//   - swap (the default): the preflight, a rehearsal, the pre-copy outside the
//     lock and the swap under it.
//   - The preflight refuses a table this tool cannot copy faithfully
//     (tableSwapShape.refusals: triggers, rules, policies, publications,
//     foreign keys, inheritance, partitioning, identity columns, dependent
//     views or functions, extended statistics, column grants, a tablespace, a
//     replica identity, an invalid index, a deferrable or exclusion
//     constraint, no primary key), a relation under the twin's name, an old
//     table still kept from an earlier swap, what an interrupted run left (its
//     change capture; --abort removes it), a run in progress, a client
//     transaction older than MaxOpenTransactionAge, and whatever the
//     profile's preflight refuses.
//   - The rehearsal runs, in a repeatable read transaction that is rolled
//     back and holds only ACCESS SHARE on the table, everything the swap does
//     but take its lock: the twin, the copy with its row count and checksums,
//     the index builds, the twin compared with the table column for column
//     and index for index, the change capture with its verification, and the
//     exclusive section itself with the most captured changes the swap
//     applies under its lock (MaxLockedDeltaRows). It changes nothing,
//     measures the copy outside the lock and the exclusive section, leaves
//     the table's live pages in shared buffers, and reads the table's catalog
//     fingerprint (tableSwapFingerprintSql). A dry run rehearses even while
//     the profile's state refuses, so the lock's length is known ahead of a
//     stop. The swap refuses an exclusive section longer than MaxLockHold.
//   - The pre-copy (db_table_swap_capture.go), outside the lock while writers
//     keep writing: the twin <table>_next without indexes and the change
//     capture's log and function; the capture triggers, under a brief SHARE
//     ROW EXCLUSIVE lock admitted like the swap's; the copy of every row
//     through the primary key with its count and checksums, and the index
//     builds under <index>_next with their constraints and the twin's
//     fingerprint checked against the rehearsal's, in one transaction; VACUUM
//     ANALYZE of the twin; catch-up passes that apply the captured changes;
//     and a verification that the twin holds the table's row of every key not
//     captured since.
//   - The swap is one script sent in one message, BEGIN to COMMIT, so its lock
//     is held for the database's own work and no network round trip. In read
//     committed with LockTimeout and StatementTimeout it takes ACCESS
//     EXCLUSIVE on the twin (nothing else uses it) and then on the table, and
//     transaction_timeout cuts it off LockTimeout + MaxLockHold after its
//     request for the table's lock. Under the lock it checks that the capture
//     triggers are whole and drops them, checks that the table's fingerprint
//     is the one the rehearsal read and that no old table is kept, runs the
//     profile's locked check, applies the changes captured since the last
//     catch-up (more than MaxLockedDeltaRows releases the lock at once, and
//     the swap catches up and tries again), checks the twin's fingerprint
//     against the table's, moves the owned sequences to the twin, renames the
//     table and its indexes to <name>_old_<stamp> and the twin and its
//     indexes to the published names, and commits. Any failed check raises
//     and rolls everything back. ANALYZE follows the commit, and the
//     capture's log and function are dropped.
//   - rollback: the swap in reverse into the kept old table, so its relation
//     comes back with the rows written since: a first transaction empties it
//     and drops its indexes under its own lock only (nothing reads it), and
//     the pre-copy and the swap run into it. The table that was swapped in is
//     kept under a dated name in its turn.
//   - abort: removes the change capture an interrupted run left, and the twin
//     the capture's log names.
//   - drop old: drops each kept old table at least DropOldMinAge old, under
//     its own lock only.
//
// Copied rows carry the transaction ids of the copy and of the catch-ups. A
// read committed statement that waited on the swap's lock plans and takes its
// snapshot after the commit, so it sees them. A transaction whose snapshot is
// older than the commit and which touches the table only after it may not: an
// ON CONFLICT on such a row raises a serialization failure, which server.Tx
// reruns, and an UPDATE by key finds nothing, or raises one. The profile's
// stop and the open transaction checks bound that window;
// PARTITION-CANDIDATES.md §4.4 has the analysis for pending_task.
//
// Every step reruns safely. A run that fails before its commit removes its
// change capture and its twin; a run killed outright leaves them for --abort,
// and the preflight refuses until then. The swap is one transaction, so an
// interrupted one changes nothing, and a committed one leaves the old table,
// which the next run finds and reports. A dry run reports the preflight and
// the rehearsal and changes nothing.
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
	// nothing can change it. It reads relation, the swap's target once the
	// changes captured since its copy are applied: under the lock the target
	// holds exactly the table's rows, compact, where the table's own heap is
	// the bloat the swap removes. nil runs none.
	LockedCheckSql func(relation string) string
	// nil uses DefaultTableSwapSettings.
	Settings *TableSwapSettings
}

// Tunables of a swap. Durations must be positive (database timeouts at least a
// millisecond), attempts and passes at least one and the bounds positive.
type TableSwapSettings struct {
	// The lock wait of each lock the swap, its capture and the drop take.
	// Every write to the table queues behind the swap's ACCESS EXCLUSIVE
	// request, and a main-pool transaction gives up after PgLockTimeout, so
	// the wait stays well below it; a busy attempt is retried.
	LockTimeout time.Duration
	// Bounds each statement of the swap, the pre-copy and the rehearsal; the
	// copy is the longest.
	StatementTimeout time.Duration
	// Attempts of each lock the swap, its capture and the drop take while
	// busy, and the wait between them.
	LockAttempts      int
	LockRetryInterval time.Duration
	// An attempt waits while an autovacuum worker, a prepared transaction or
	// a transaction older than this holds or awaits a lock on the relations,
	// or a vacuum of them runs, so its lock request never queues writers
	// behind them (the 11-17 lock admission,
	// db_contract_partition_admission.go).
	MaxTransactionAge time.Duration
	// The preflight refuses, and the swap's lock waits, while a client
	// transaction older than this is open anywhere in the database: its
	// snapshot predates the swap, and it could touch the table after the
	// commit and miss the copied rows.
	MaxOpenTransactionAge time.Duration
	// The most rows the swap copies; more refuses the swap.
	MaxCopyRows int64
	// The longest the swap holds ACCESS EXCLUSIVE on the table. The
	// rehearsal's exclusive section, run with MaxLockedDeltaRows captured
	// changes, must fit it, or the swap refuses; and transaction_timeout cuts
	// the swap's transaction off LockTimeout + MaxLockHold after its request
	// for the table's lock, so no writer queues behind it longer than that.
	MaxLockHold time.Duration
	// The most captured changes the swap applies under its lock. More
	// releases the lock at once; the swap catches up and tries again.
	MaxLockedDeltaRows int64
	// Catch-up passes before the first lock attempt run until one applies at
	// most CatchUpRows captured changes, or MaxCatchUpPasses have run.
	CatchUpRows      int64
	MaxCatchUpPasses int
	// The memory of the copy's checksums and of the index builds, in bytes.
	WorkMemBytes int64
	// Drop old refuses an old table younger than this, by its name's stamp
	// and the database's clock: the rollback window.
	DropOldMinAge time.Duration
	// How long a failed script's ROLLBACK may take before its session is
	// discarded instead.
	RollbackTimeout time.Duration
}

// Half a second of lock wait, a quarter of a writer's own bound, and at most a
// second of lock held, so a writer waits at most 1.5 s of its 2 s; seconds for
// the copy, outside the lock; a day's rollback window.
func DefaultTableSwapSettings() *TableSwapSettings {
	return &TableSwapSettings{
		LockTimeout:           500 * time.Millisecond,
		StatementTimeout:      30 * time.Second,
		LockAttempts:          60,
		LockRetryInterval:     2 * time.Second,
		MaxTransactionAge:     5 * time.Second,
		MaxOpenTransactionAge: 2 * time.Minute,
		MaxCopyRows:           1_000_000,
		MaxLockHold:           time.Second,
		MaxLockedDeltaRows:    1000,
		CatchUpRows:           100,
		MaxCatchUpPasses:      30,
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
		"MaxLockHold":           self.MaxLockHold,
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
	if self.MaxCatchUpPasses < 1 {
		return fmt.Errorf("table swap setting MaxCatchUpPasses must be at least one: %d", self.MaxCatchUpPasses)
	}
	if self.MaxCopyRows < 1 {
		return fmt.Errorf("table swap setting MaxCopyRows must be positive: %d", self.MaxCopyRows)
	}
	if self.MaxLockedDeltaRows < 1 {
		return fmt.Errorf("table swap setting MaxLockedDeltaRows must be positive: %d", self.MaxLockedDeltaRows)
	}
	if self.CatchUpRows < 0 {
		return fmt.Errorf("table swap setting CatchUpRows must not be negative: %d", self.CatchUpRows)
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
	// Removes the change capture an interrupted run left.
	Abort bool
	Logf  func(string, ...any)
	// nil uses the profile's settings.
	Settings *TableSwapSettings

	// Tests only: callbacks between the steps of a run.
	Hooks *TableSwapHooks
	// Tests only: runs on the swap's connection, inside its locked
	// transaction, after the captured changes are applied and before the
	// renames; tests interrupt the swap there.
	afterCopy func(context.Context, PgConn)
	// Optional fixture observation on that same backend, before either lock.
	afterBeginSql string
}

// Tests only: callbacks between the steps of a run. AfterCapture runs once
// the capture triggers are on, before the copy; AfterPrecopy once the copy
// and the index builds have committed; BeforeLock before each attempt's
// locked script, after its catch-up and admission; UnderLock inside the
// locked transaction, where afterCopy runs, while the swap holds its locks;
// AfterLocked once the locked script has returned, and its error stands for
// the script's, as when the commit's reply is lost. A panic in one fails the
// run.
type TableSwapHooks struct {
	AfterCapture func(ctx context.Context)
	AfterPrecopy func(ctx context.Context)
	BeforeLock   func(ctx context.Context, attempt int)
	UnderLock    func(ctx context.Context)
	AfterLocked  func() error
}

// The suffix of the twin a swap builds, and of its indexes while they are
// built.
const tableSwapNextSuffix = "_next"

// The infix of a kept old table and its indexes: <name>_old_<stamp>, the stamp
// the database's UTC clock at the swap, YYYYMMDDHHMMSS.
const tableSwapOldInfix = "_old_"

const tableSwapStampLayout = "20060102150405"

var tableSwapStampPattern = regexp.MustCompile(`^[0-9]{14}$`)

// The transaction-local settings a script keeps its report in: the rows it
// copied, and the times its steps started, read back before its end.
const (
	tableSwapRowsSetting      = "urnetwork.table_swap_rows"
	tableSwapStartedAtSetting = "urnetwork.table_swap_started_at"
	tableSwapLockedAtSetting  = "urnetwork.table_swap_locked_at"
	tableSwapCopiedAtSetting  = "urnetwork.table_swap_copied_at"
	tableSwapIndexedAtSetting = "urnetwork.table_swap_indexed_at"
	tableSwapDeltaAtSetting   = "urnetwork.table_swap_delta_at"
	tableSwapAppliedAtSetting = "urnetwork.table_swap_applied_at"
	tableSwapVerifyAtSetting  = "urnetwork.table_swap_verify_at"
	tableSwapEndedAtSetting   = "urnetwork.table_swap_ended_at"
	// what the rehearsal's checks under the lock would refuse now
	tableSwapRehearsalRefusalSetting = "urnetwork.table_swap_rehearsal_refusal"
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

// Whether name is a kept old table of a registered profile, its twin or its
// change capture's log, with the profile's table: relations a swap keeps
// beside its table, which db audit and the REINDEX rotation leave alone.
func TableSwapRelation(name string) (table string, ok bool) {
	for _, profile := range TableSwapProfiles() {
		if name == profile.Table+tableSwapNextSuffix || name == profile.Table+tableSwapLogSuffix {
			return profile.Table, true
		}
		if stamp, found := strings.CutPrefix(name, profile.Table+tableSwapOldInfix); found && tableSwapStampPattern.MatchString(stamp) {
			return profile.Table, true
		}
	}
	return "", false
}

// The command: swap, rollback, abort or drop old, each rehearsed with DryRun.
func SwapTable(ctx context.Context, options TableSwapOptions) error {
	profile := tableSwapProfile(options.Table)
	if profile == nil {
		names := []string{}
		for _, profile := range TableSwapProfiles() {
			names = append(names, profile.Table)
		}
		return fmt.Errorf("no table swap profile for %q (profiles: %s)", options.Table, strings.Join(names, ", "))
	}
	steps := 0
	for _, step := range []bool{options.Rollback, options.DropOld, options.Abort} {
		if step {
			steps += 1
		}
	}
	if 1 < steps {
		return errors.New("choose one of rollback, drop old and abort")
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
		profile:       profile,
		settings:      settings,
		dryRun:        options.DryRun,
		logf:          options.Logf,
		afterCopy:     options.afterCopy,
		afterBeginSql: options.afterBeginSql,
	}
	if options.Hooks != nil {
		swap.hooks = *options.Hooks
	}
	if swap.logf == nil {
		swap.logf = func(string, ...any) {}
	}
	switch {
	case options.DropOld:
		return swap.dropOld(ctx)
	case options.Abort:
		return swap.abort(ctx)
	case options.Rollback:
		return swap.rollback(ctx)
	default:
		return swap.swap(ctx)
	}
}

type tableSwap struct {
	profile       *TableSwapProfile
	settings      *TableSwapSettings
	dryRun        bool
	logf          func(string, ...any)
	hooks         TableSwapHooks
	afterCopy     func(context.Context, PgConn)
	afterBeginSql string
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
// the swap cannot copy faithfully, the names it would give, a relation already
// under its twin's name, and a change capture an interrupted run left (or a
// run in progress); the copy cannot even be rehearsed. State refusals are
// about now: the kept old tables, open transactions and the profile's own
// preflight (a running taskworker, for pending_task); a dry run still
// rehearses the copy and reports them.
func (self *tableSwap) preflight(ctx context.Context, rollback bool) (shape *tableSwapShape, oldTables []tableSwapOldTable, shapeRefusals []string, stateRefusals []string, err error) {
	table := self.profile.Table
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			var now time.Time
			oldTables, now = self.readOldTables(ctx, conn)
			capture := readTableSwapCaptureState(ctx, conn, newTableSwapCapture(table, nil))
			shape = readTableSwapShape(ctx, conn, table)
			if shape == nil {
				shapeRefusals = append(shapeRefusals, fmt.Sprintf("%s does not exist", table))
				return
			}
			shapeRefusals = append(shapeRefusals, shape.refusals...)
			shapeRefusals = append(shapeRefusals, tableSwapNameRefusals(table, shape)...)
			shapeRefusals = append(shapeRefusals, self.captureRefusals(capture)...)
			if next := readTableSwapShape(ctx, conn, table+tableSwapNextSuffix); next != nil && !capture.ownsTwin() {
				shapeRefusals = append(shapeRefusals, fmt.Sprintf("%s exists; the swap builds its twin under that name and drops nothing it did not make", table+tableSwapNextSuffix))
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
				stateRefusals = append(stateRefusals, fmt.Sprintf("no kept old table of %s to roll back to", table))
			case rollback && 1 < len(oldTables):
				stateRefusals = append(stateRefusals, fmt.Sprintf("%d kept old tables of %s; a rollback needs exactly one", len(oldTables), table))
			case !rollback && 0 < len(oldTables):
				stateRefusals = append(stateRefusals, fmt.Sprintf("the swap of %s at %s is done and its old table is kept: run --rollback, or --drop-old once the rollback window has passed", table, oldTables[len(oldTables)-1].stamp.Format(time.RFC3339)))
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

// A run in progress, and what an interrupted run left.
func (self *tableSwap) captureRefusals(state *tableSwapCaptureState) (refusals []string) {
	table := self.profile.Table
	if 0 < len(state.runHolders) {
		refusals = append(refusals, fmt.Sprintf("a run of the swap of %s is in progress (pid %v)", table, state.runHolders))
	}
	switch {
	case !state.left():
	case state.committed():
		refusals = append(refusals, fmt.Sprintf("the swap of %s committed, but its change capture was not removed (%s): run --abort", table, state))
	default:
		refusals = append(refusals, fmt.Sprintf("an interrupted run of the swap of %s left its change capture (%s): run --abort, then the swap again", table, state))
	}
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

// What keeps a lock request on relations from being made now, under the 11-17
// lock admission (db_contract_partition_admission.go): an autovacuum worker,
// a prepared transaction, a session this role cannot see, or a transaction
// older than MaxTransactionAge that holds or awaits a lock on one of them, and
// a vacuum of one of them, which the request would queue every writer behind.
// With openTransactions, also a client transaction older than
// MaxOpenTransactionAge anywhere. Also the database's clock, for the old
// table's stamp.
func (self *tableSwap) busy(ctx context.Context, relations []string, openTransactions bool) (reasons []string, now time.Time, err error) {
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT now() AT TIME ZONE 'UTC'`).Scan(&now))
			blockers := contractLockBlockers(ctx, conn, &contractLockScope{
				relations: relations,
				request:   contractLockAccessExclusive,
				settings:  ContractPartitionLockSettings{MaxTransactionAge: self.settings.MaxTransactionAge},
			})
			if 0 < len(blockers) {
				reasons = append(reasons, fmt.Sprintf(
					"a lock request on %s would queue behind an autovacuum or a transaction older than %s: %s",
					strings.Join(relations, ", "), self.settings.MaxTransactionAge, strings.Join(blockers, "; "),
				))
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
			if openTransactions {
				reasons = append(reasons, self.openTransactionRefusals(ctx, conn)...)
			}
		})
	})
	return
}

// Runs one locking step until it succeeds, fails for a reason other than a
// busy lock, or runs out of attempts. With relations, each attempt first
// waits for them to be admitted (busy), and repeats the whole step with the
// database's clock read then.
func (self *tableSwap) retryExclusive(ctx context.Context, relations []string, run func(now time.Time) error) (err error) {
	for attempt := 1; attempt <= self.settings.LockAttempts; attempt += 1 {
		var reasons []string
		var now time.Time
		if 0 < len(relations) {
			if reasons, now, err = self.busy(ctx, relations, false); err != nil {
				return
			}
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

// The bounds of a swap, pre-copy or rehearsal transaction.
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

// A check as the rehearsal runs it: the same statement, whose refusal is kept
// for the report (tableSwapRehearsalRefusalSetting) instead of raised.
func tableSwapRehearsedCheckSql(statement string) string {
	return fmt.Sprintf(`DO $table_swap_rehearsal$
BEGIN
	EXECUTE %s;
EXCEPTION WHEN OTHERS THEN
	PERFORM set_config('%s', concat_ws('; ', nullif(current_setting('%s', true), ''), SQLERRM), true);
END
$table_swap_rehearsal$`, tableSwapLiteral(statement), tableSwapRehearsalRefusalSetting, tableSwapRehearsalRefusalSetting)
}

// The copy into target, empty and unindexed, and the index builds under their
// next names: the copy with its count and checksums, the indexes with their
// constraints, and, with a fingerprint, the target's fingerprint checked
// against it. Each step's start is marked for the report.
func (self *tableSwap) precopySql(source *tableSwapShape, target string, sourceFingerprint string) []string {
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
	if sourceFingerprint != "" {
		statements = append(statements, tableSwapAssertSql(
			fmt.Sprintf("%s = %s", tableSwapFingerprintSql(target, tableSwapNextSuffix), tableSwapLiteral(sourceFingerprint)),
			target+" does not match "+source.name+" as the rehearsal read it; a dry run reports how",
		))
	}
	statements = append(statements, tableSwapMarkSql(tableSwapIndexedAtSetting))
	return statements
}

// The report of a pre-copy: the rows copied, and how long the copy and the
// index builds took, in seconds.
const tableSwapPrecopyReportSql = `SELECT
	current_setting('` + tableSwapRowsSetting + `')::bigint,
	extract(epoch FROM current_setting('` + tableSwapCopiedAtSetting + `')::timestamptz - current_setting('` + tableSwapStartedAtSetting + `')::timestamptz)::float8,
	extract(epoch FROM current_setting('` + tableSwapIndexedAtSetting + `')::timestamptz - current_setting('` + tableSwapCopiedAtSetting + `')::timestamptz)::float8`

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

// The rehearsal's renames: as many as the swap's, of the twin's own relations,
// each to a placeholder and back. The table's own cannot be renamed without
// its lock.
func (self *tableSwap) rehearsedRenameSql(source *tableSwapShape, twin string) []string {
	type relation struct {
		kind string
		name string
	}
	relations := []relation{{kind: "TABLE", name: twin}}
	for _, index := range source.indexes {
		relations = append(relations, relation{kind: "INDEX", name: tableSwapNextName(index.name)})
	}
	statements := []string{}
	for i, relation := range relations {
		statements = append(statements, fmt.Sprintf("ALTER %s public.%s RENAME TO %s", relation.kind, quoteIdentifier(relation.name), quoteIdentifier(fmt.Sprintf("table_swap_rehearsal_%d", i))))
	}
	for i, relation := range relations {
		statements = append(statements, fmt.Sprintf("ALTER %s public.%s RENAME TO %s", relation.kind, quoteIdentifier(fmt.Sprintf("table_swap_rehearsal_%d", i)), quoteIdentifier(relation.name)))
	}
	return statements
}

// One locked script: the swap's, or the rehearsal's run of its exclusive
// section.
type tableSwapLockedScript struct {
	source            *tableSwapShape
	capture           *tableSwapCapture
	target            string
	stamp             string
	sourceFingerprint string
	// the kept old table a rollback swaps back in, or nil
	oldTable  *tableSwapOldTable
	rehearsal bool
}

// The script under the lock, split at the hook point after the captured
// changes are applied: BEGIN, the bounds, the target's lock, the cut-off,
// the table's lock; the capture triggers checked and dropped; the checks
// under the lock; the captured changes; the target's fingerprint; then the
// renames. The rehearsal's has no BEGIN and takes no lock: its capture
// triggers are on the twin, its checks report instead of raising, and its
// renames are the twin's own (rehearsedRenameSql).
func (self *tableSwap) lockedSql(script tableSwapLockedScript) (beforeHook []string, afterHook []string) {
	table := self.profile.Table
	quotedTable := "public." + quoteIdentifier(table)
	quotedTarget := "public." + quoteIdentifier(script.target)
	check := func(statement string) string {
		if script.rehearsal {
			return tableSwapRehearsedCheckSql(statement)
		}
		return statement
	}
	captured := table
	if script.rehearsal {
		captured = script.target
	} else {
		beforeHook = append(beforeHook, "BEGIN ISOLATION LEVEL READ COMMITTED")
		beforeHook = append(beforeHook, self.boundsSql()...)
		if self.afterBeginSql != "" {
			beforeHook = append(beforeHook, self.afterBeginSql)
		}
		beforeHook = append(beforeHook,
			// nothing but the swap uses the target: waiting for it blocks no
			// writer, and the cut-off starts after it
			fmt.Sprintf("LOCK TABLE %s IN ACCESS EXCLUSIVE MODE", quotedTarget),
			// the cut-off, counted from here: a timer a role default armed at
			// BEGIN is disarmed first, since a positive value set while one
			// runs leaves it alone (db_transaction_timeout.go)
			"SET LOCAL transaction_timeout = 0",
			fmt.Sprintf("SET LOCAL transaction_timeout = '%dms'", (self.settings.LockTimeout+self.settings.MaxLockHold).Milliseconds()),
			fmt.Sprintf("LOCK TABLE %s IN ACCESS EXCLUSIVE MODE", quotedTable),
		)
	}
	beforeHook = append(beforeHook, tableSwapMarkSql(tableSwapLockedAtSetting))
	beforeHook = append(beforeHook, script.capture.capturedAssertSql(captured))
	beforeHook = append(beforeHook, script.capture.dropTriggerSql(captured)...)
	beforeHook = append(beforeHook, check(tableSwapAssertSql(
		fmt.Sprintf("%s = %s", tableSwapFingerprintSql(table, ""), tableSwapLiteral(script.sourceFingerprint)),
		table+" changed since the rehearsal; run the swap again",
	)))
	// a swap keeps no old table beside the one it makes; a rollback, only its own
	oldTablesCondition := fmt.Sprintf(
		"NOT EXISTS (SELECT 1 FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relname ~ %s",
		tableSwapLiteral("^"+regexp.QuoteMeta(table+tableSwapOldInfix)+"[0-9]{14}$"),
	)
	if script.oldTable != nil {
		oldTablesCondition += " AND relname <> " + tableSwapLiteral(script.oldTable.name)
	}
	beforeHook = append(beforeHook, check(tableSwapAssertSql(oldTablesCondition+")", "an old table of "+table+" is kept")))
	beforeHook = append(beforeHook,
		tableSwapMarkSql(tableSwapDeltaAtSetting),
		script.capture.applySql(script.source, script.target, self.settings.MaxLockedDeltaRows),
		tableSwapMarkSql(tableSwapAppliedAtSetting),
	)
	// the profile's check reads the target, which now holds exactly the
	// table's rows: compact, where the table's heap is the bloat
	if self.profile.LockedCheckSql != nil {
		if lockedCheck := self.profile.LockedCheckSql(script.target); lockedCheck != "" {
			beforeHook = append(beforeHook, check(lockedCheck))
		}
	}
	beforeHook = append(beforeHook, tableSwapAssertSql(
		fmt.Sprintf("%s = %s", tableSwapFingerprintSql(script.target, tableSwapNextSuffix), tableSwapFingerprintSql(table, "")),
		script.target+" does not match "+table+"; a dry run reports how",
	))
	if script.rehearsal {
		afterHook = self.rehearsedRenameSql(script.source, script.target)
	} else {
		afterHook = self.renameSql(script.source, script.target, script.stamp)
	}
	return
}

// The report of a locked script: the captured changes it applied, how long
// applying them took, and how long the table's lock has been held, in
// seconds.
const tableSwapLockedReportSql = `SELECT
	current_setting('` + tableSwapCapturedSetting + `')::bigint,
	extract(epoch FROM current_setting('` + tableSwapAppliedAtSetting + `')::timestamptz - current_setting('` + tableSwapDeltaAtSetting + `')::timestamptz)::float8,
	extract(epoch FROM clock_timestamp() - current_setting('` + tableSwapLockedAtSetting + `')::timestamptz)::float8`

// How long a script has held the lock it marked, in seconds.
const tableSwapHeldReportSql = `SELECT extract(epoch FROM clock_timestamp() - current_setting('` + tableSwapLockedAtSetting + `')::timestamptz)::float8`

func tableSwapSeconds(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

func tableSwapRound(duration time.Duration) time.Duration {
	return duration.Round(time.Millisecond)
}

// What the swap's locked transaction did, as the database measured it.
type tableSwapLockedReport struct {
	// false when the swap committed but its report was lost
	known         bool
	capturedCount int64
	deltaDuration time.Duration
	lockDuration  time.Duration
}

func parseTableSwapLockedReport(values []string) (report tableSwapLockedReport, err error) {
	if len(values) != 3 {
		return report, fmt.Errorf("the locked script's report has %d values", len(values))
	}
	if report.capturedCount, err = strconv.ParseInt(values[0], 10, 64); err != nil {
		return
	}
	var deltaSeconds, lockSeconds float64
	if deltaSeconds, err = strconv.ParseFloat(values[1], 64); err != nil {
		return
	}
	if lockSeconds, err = strconv.ParseFloat(values[2], 64); err != nil {
		return
	}
	report.known = true
	report.deltaDuration = tableSwapSeconds(deltaSeconds)
	report.lockDuration = tableSwapSeconds(lockSeconds)
	return
}

// What the rehearsal measured. The exclusive section ran with deltaRows
// captured changes, the most the swap applies under its lock.
type tableSwapRehearsal struct {
	rowCount          int64
	copyDuration      time.Duration
	indexDuration     time.Duration
	verifyDuration    time.Duration
	deltaRows         int64
	deltaDuration     time.Duration
	exclusiveDuration time.Duration
	// what the checks under the lock would refuse now, or empty
	lockedRefusal string
}

// The rehearsal line. Operator scripts parse it: the rows, the copy and the
// index builds before the lock, and the exclusive section ("about ...").
func (self *tableSwapRehearsal) String(table string) string {
	return fmt.Sprintf(
		"%d rows copied in %s and indexes built in %s before the lock; the swap holds ACCESS EXCLUSIVE on %s about %s (its checks and renames %s, and %s to apply %d captured change(s), the most it applies under the lock)",
		self.rowCount, tableSwapRound(self.copyDuration), tableSwapRound(self.indexDuration),
		table, tableSwapRound(self.exclusiveDuration),
		tableSwapRound(self.exclusiveDuration-self.deltaDuration), tableSwapRound(self.deltaDuration), self.deltaRows,
	)
}

var errTableSwapRehearsed = errors.New("rehearsed")

// Runs callback in one maintenance transaction of the given isolation and
// returns its failure.
func tableSwapTx(ctx context.Context, isolation any, callback func(PgTx)) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if recoveredErr, ok := r.(error); ok {
				err = recoveredErr
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	MaintenanceTx(ctx, callback, isolation, OptNoRetry())
	return
}

// Everything the swap does but take its lock, in a repeatable read
// transaction that is rolled back and holds only ACCESS SHARE on the table:
// the twin, the copy and the index builds, the twin compared with the table
// column for column and index for index, the change capture with the
// verification, and the exclusive section with the most captured changes the
// swap applies under its lock, its checks reporting instead of raising.
// Returns the table's fingerprint, as the swap's lock must find it.
func (self *tableSwap) rehearse(ctx context.Context, source *tableSwapShape, oldTable *tableSwapOldTable) (rehearsal tableSwapRehearsal, sourceFingerprint string, err error) {
	table := self.profile.Table
	twin := table + tableSwapNextSuffix
	capture := newTableSwapCapture(table, source.primaryKey)
	kind := tableSwapForward
	if oldTable != nil {
		kind = tableSwapRollback
	}
	err = tableSwapTx(ctx, TxRepeatableRead, func(tx PgTx) {
		exec := func(statements []string) {
			RaisePgResult(tx.Exec(ctx, strings.Join(statements, ";\n")))
		}
		statements := slices.Clone(self.boundsSql())
		statements = append(statements, source.twinSql(twin)...)
		statements = append(statements, self.precopySql(source, twin, "")...)
		exec(statements)
		var copySeconds, indexSeconds float64
		Raise(tx.QueryRow(ctx, tableSwapPrecopyReportSql).Scan(&rehearsal.rowCount, &copySeconds, &indexSeconds))
		rehearsal.copyDuration = tableSwapSeconds(copySeconds)
		rehearsal.indexDuration = tableSwapSeconds(indexSeconds)
		if differences := source.differences(readTableSwapShape(ctx, tx, twin), tableSwapNextName); 0 < len(differences) {
			panic(fmt.Errorf("the rehearsed twin %s does not match %s:\n  - %s", twin, source.name, strings.Join(differences, "\n  - ")))
		}
		var twinFingerprint string
		Raise(tx.QueryRow(ctx, "SELECT "+tableSwapFingerprintSql(source.name, "")+", "+tableSwapFingerprintSql(twin, tableSwapNextSuffix)).Scan(&sourceFingerprint, &twinFingerprint))
		if twinFingerprint != sourceFingerprint {
			panic(fmt.Errorf("the rehearsed twin %s matches %s column for column but not by fingerprint", twin, source.name))
		}

		// the change capture as the swap creates it, and the verification
		var twinOid uint32
		Raise(tx.QueryRow(ctx, `SELECT to_regclass($1)::oid`, "public."+quoteIdentifier(twin)).Scan(&twinOid))
		exec(capture.createSql(source, twinOid, kind))
		exec([]string{tableSwapMarkSql(tableSwapVerifyAtSetting), capture.verifySql(source, twin), tableSwapMarkSql(tableSwapEndedAtSetting)})
		var verifySeconds float64
		Raise(tx.QueryRow(ctx, `SELECT extract(epoch FROM current_setting('`+tableSwapEndedAtSetting+`')::timestamptz - current_setting('`+tableSwapVerifyAtSetting+`')::timestamptz)::float8`).Scan(&verifySeconds))
		rehearsal.verifyDuration = tableSwapSeconds(verifySeconds)

		// the exclusive section, with the most captured changes the swap
		// applies under its lock
		rehearsal.deltaRows = min(self.settings.MaxLockedDeltaRows, rehearsal.rowCount)
		exec(capture.triggerSql(twin))
		exec([]string{fmt.Sprintf(
			"INSERT INTO %s (%s) SELECT %s FROM public.%s ORDER BY %s LIMIT %d",
			capture.quotedLog(), capture.keyList(""), capture.keyList(""), quoteIdentifier(table), capture.keyList(""), rehearsal.deltaRows,
		)})
		beforeHook, afterHook := self.lockedSql(tableSwapLockedScript{
			source:            source,
			capture:           capture,
			target:            twin,
			sourceFingerprint: sourceFingerprint,
			oldTable:          oldTable,
			rehearsal:         true,
		})
		exec(append(append(slices.Clone(beforeHook), afterHook...), tableSwapMarkSql(tableSwapEndedAtSetting)))
		var appliedCount int64
		var deltaSeconds, exclusiveSeconds float64
		Raise(tx.QueryRow(
			ctx,
			`SELECT
				current_setting('`+tableSwapCapturedSetting+`')::bigint,
				extract(epoch FROM current_setting('`+tableSwapAppliedAtSetting+`')::timestamptz - current_setting('`+tableSwapDeltaAtSetting+`')::timestamptz)::float8,
				extract(epoch FROM current_setting('`+tableSwapEndedAtSetting+`')::timestamptz - current_setting('`+tableSwapLockedAtSetting+`')::timestamptz)::float8,
				coalesce(current_setting('`+tableSwapRehearsalRefusalSetting+`', true), '')`,
		).Scan(&appliedCount, &deltaSeconds, &exclusiveSeconds, &rehearsal.lockedRefusal))
		if appliedCount != rehearsal.deltaRows {
			panic(fmt.Errorf("the rehearsal applied %d captured changes, not %d", appliedCount, rehearsal.deltaRows))
		}
		rehearsal.deltaDuration = tableSwapSeconds(deltaSeconds)
		rehearsal.exclusiveDuration = tableSwapSeconds(exclusiveSeconds)
		panic(errTableSwapRehearsed)
	})
	if errors.Is(err, errTableSwapRehearsed) {
		err = nil
	}
	return
}

// Runs a script on one maintenance session: the part before the hook point,
// the hook (tests only), then the rest with the report and COMMIT. Without a
// hook the whole script is one message, so a lock it takes is held for the
// database's own work and no network round trip. Returns the report's values,
// the last one-row result with columns columns. A failed script leaves the
// session in a failed transaction, which is rolled back before the session
// goes back, or the session is discarded.
func (self *tableSwap) runScript(ctx context.Context, beforeHook []string, afterHook []string, report string, columns int, hook func(context.Context, PgConn)) (values []string, err error) {
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
	exec := func(statements []string) (reportValues []string, execErr error) {
		results, execErr := conn.Conn().PgConn().Exec(ctx, strings.Join(statements, ";\n")).ReadAll()
		if execErr != nil {
			return
		}
		for _, result := range results {
			if result.Err != nil {
				return nil, result.Err
			}
			if len(result.FieldDescriptions) == columns && len(result.Rows) == 1 {
				reportValues = []string{}
				for _, value := range result.Rows[0] {
					reportValues = append(reportValues, string(value))
				}
			}
		}
		return
	}
	if hook != nil {
		if _, err = exec(beforeHook); err != nil {
			return
		}
		if err = partitionRegistryRun(func() { hook(ctx, conn) }); err != nil {
			return
		}
	} else {
		afterHook = append(slices.Clone(beforeHook), afterHook...)
	}
	values, err = exec(append(slices.Clone(afterHook), report, "COMMIT"))
	if err != nil {
		return
	}
	committed = true
	if len(values) != columns {
		err = fmt.Errorf("the script's report has %d values, not %d", len(values), columns)
	}
	return
}

// The session lock a run holds from before its first change to its end. A
// second run, and --abort, refuse while it is held, so no run removes what
// another one is building. It is a session advisory lock in its two-key form,
// which the task advisory locks never take.
func (self *tableSwap) acquireRunLock(ctx context.Context) (release func(), err error) {
	conn, err := AcquireMaintenanceDbConn(ctx)
	if err != nil {
		return
	}
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1), hashtext($2))`, tableSwapRunLockNamespace, self.profile.Table).Scan(&locked); err != nil {
		discardPgConnection(ctx, conn)
		return
	}
	if !locked {
		conn.Release()
		err = fmt.Errorf("another run of the swap of %s holds its run lock", self.profile.Table)
		return
	}
	release = func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), self.settings.RollbackTimeout)
		defer cancel()
		if _, unlockErr := conn.Exec(releaseCtx, `SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`, tableSwapRunLockNamespace, self.profile.Table); unlockErr != nil {
			discardPgConnection(releaseCtx, conn)
			return
		}
		conn.Release()
	}
	return
}

func (self *tableSwap) captureState(ctx context.Context, capture *tableSwapCapture) (state *tableSwapCaptureState, err error) {
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			state = readTableSwapCaptureState(ctx, conn, capture)
		})
	})
	return
}

// One run of the pre-copy and the swap into target.
type tableSwapRun struct {
	source            *tableSwapShape
	sourceFingerprint string
	capture           *tableSwapCapture
	// the twin (forward), or the kept old table (rollback)
	target    string
	oldTable  *tableSwapOldTable
	kind      string
	targetOid uint32
	// whether the capture and target were created, and whether the swap
	// committed
	created bool
	swapped bool
	stamp   string

	rowCount       int64
	copyDuration   time.Duration
	indexDuration  time.Duration
	caughtUp       int64
	catchUpPasses  int
	verifiedRows   int64
	verifyDuration time.Duration
	locked         tableSwapLockedReport
}

func (self *tableSwapRun) String() string {
	locked := "the report of the locked transaction was lost"
	if self.locked.known {
		locked = fmt.Sprintf("%d captured change(s) applied under the lock in %s", self.locked.capturedCount, tableSwapRound(self.locked.deltaDuration))
	}
	return fmt.Sprintf(
		"%d rows pre-copied in %s and indexes built in %s before the lock, %d captured change(s) caught up outside it, %s",
		self.rowCount, tableSwapRound(self.copyDuration), tableSwapRound(self.indexDuration), self.caughtUp, locked,
	)
}

// The pre-copy into target outside the lock and the swap under it, with the
// change capture on between them. target is the twin, which this creates
// (forward), or the kept old table, emptied before (rollback). Until the swap
// commits, any failure removes the capture again, and the twin with it; after
// it, the capture's log and function are dropped.
func (self *tableSwap) precopyAndSwap(ctx context.Context, source *tableSwapShape, sourceFingerprint string, target string, oldTable *tableSwapOldTable) (run *tableSwapRun, err error) {
	table := self.profile.Table
	run = &tableSwapRun{
		source:            source,
		sourceFingerprint: sourceFingerprint,
		capture:           newTableSwapCapture(table, source.primaryKey),
		target:            target,
		oldTable:          oldTable,
		kind:              tableSwapForward,
	}
	if oldTable != nil {
		run.kind = tableSwapRollback
	}
	defer func() {
		if !run.created {
			return
		}
		finishCtx := context.WithoutCancel(ctx)
		if run.swapped {
			if dropErr := self.dropCapture(finishCtx, run.capture); dropErr != nil {
				self.logf("the swap of %s committed, but its change capture's log and function remain (%v): run --abort to remove them", table, dropErr)
			}
			return
		}
		state, stateErr := self.captureState(finishCtx, run.capture)
		if stateErr != nil {
			err = errors.Join(err, fmt.Errorf("could not read what the change capture left (run --dry-run, then --abort): %w", stateErr))
			return
		}
		if removeErr := self.removeCapture(finishCtx, state); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("could not remove the change capture (run --abort): %w", removeErr))
			return
		}
		self.logf("removed the change capture of %s%s", table, tableSwapTwinNote(state))
	}()

	if err = self.createCapture(ctx, run); err != nil {
		return run, fmt.Errorf("create the change capture: %w", err)
	}
	run.created = true
	if err = self.installCapture(ctx, run); err != nil {
		return run, fmt.Errorf("install the change capture on %s: %w", table, err)
	}
	if self.hooks.AfterCapture != nil {
		if err = partitionRegistryRun(func() { self.hooks.AfterCapture(ctx) }); err != nil {
			return
		}
	}
	if err = self.precopy(ctx, run); err != nil {
		return run, fmt.Errorf("pre-copy %s into %s: %w", table, target, err)
	}
	self.logf("pre-copied %d rows of %s into %s in %s and built its indexes in %s, outside the lock", run.rowCount, table, target, tableSwapRound(run.copyDuration), tableSwapRound(run.indexDuration))
	if self.hooks.AfterPrecopy != nil {
		if err = partitionRegistryRun(func() { self.hooks.AfterPrecopy(ctx) }); err != nil {
			return
		}
	}
	self.vacuumTarget(ctx, target)

	catchUpStart := time.Now()
	for run.catchUpPasses < self.settings.MaxCatchUpPasses {
		var applied int64
		if applied, err = self.catchUp(ctx, run); err != nil {
			return
		}
		if applied <= self.settings.CatchUpRows {
			break
		}
	}
	self.logf("caught up %d captured change(s) in %d pass(es), %s", run.caughtUp, run.catchUpPasses, tableSwapRound(time.Since(catchUpStart)))
	if err = self.verify(ctx, run); err != nil {
		return
	}
	err = self.swapUnderLock(ctx, run)
	return
}

func tableSwapTwinNote(state *tableSwapCaptureState) string {
	if state.ownsTwin() {
		return " and its twin"
	}
	return ""
}

// The target (forward: the twin, created as the table) and the capture's log
// and function, in one transaction; the log's marker names the target's oid.
func (self *tableSwap) createCapture(ctx context.Context, run *tableSwapRun) error {
	return self.retryExclusive(ctx, nil, func(time.Time) error {
		return contractPartitionTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds())))
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.settings.StatementTimeout.Milliseconds())))
			if run.kind == tableSwapForward {
				for _, statement := range run.source.twinSql(run.target) {
					RaisePgResult(tx.Exec(ctx, statement))
				}
			}
			var targetOid *uint32
			Raise(tx.QueryRow(ctx, `SELECT to_regclass($1)::oid`, "public."+quoteIdentifier(run.target)).Scan(&targetOid))
			if targetOid == nil {
				panic(fmt.Errorf("%s does not exist", run.target))
			}
			run.targetOid = *targetOid
			for _, statement := range run.capture.createSql(run.source, run.targetOid, run.kind) {
				RaisePgResult(tx.Exec(ctx, statement))
			}
		})
	})
}

// The capture triggers on the table, in one message under SHARE ROW
// EXCLUSIVE: it waits out every transaction that has written the table, so
// every later write is captured, and blocks writers (not readers) for the
// moment it is held. Admitted like the swap's lock, and retried while busy.
func (self *tableSwap) installCapture(ctx context.Context, run *tableSwapRun) error {
	table := self.profile.Table
	statements := []string{
		"BEGIN ISOLATION LEVEL READ COMMITTED",
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds()),
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.settings.StatementTimeout.Milliseconds()),
		fmt.Sprintf("LOCK TABLE public.%s IN SHARE ROW EXCLUSIVE MODE", quoteIdentifier(table)),
		tableSwapMarkSql(tableSwapLockedAtSetting),
	}
	statements = append(statements, run.capture.triggerSql(table)...)
	var held time.Duration
	err := self.retryExclusive(ctx, []string{"public." + table}, func(time.Time) error {
		values, runErr := self.runScript(ctx, statements, nil, tableSwapHeldReportSql, 1, nil)
		if runErr != nil {
			return runErr
		}
		seconds, parseErr := strconv.ParseFloat(values[0], 64)
		if parseErr != nil {
			return parseErr
		}
		held = tableSwapSeconds(seconds)
		return nil
	})
	if err != nil {
		return err
	}
	self.logf("the change capture is on %s (log %s): its triggers held SHARE ROW EXCLUSIVE for %s", table, run.capture.log, tableSwapRound(held))
	return nil
}

// The copy into the target and its index builds, in one transaction whose
// snapshot is taken after the capture triggers committed. Autovacuum, which
// would hold the index builds back, sees no change to the target until the
// commit.
func (self *tableSwap) precopy(ctx context.Context, run *tableSwapRun) error {
	return contractPartitionTx(ctx, func(tx PgTx) {
		statements := slices.Clone(self.boundsSql())
		statements = append(statements, self.precopySql(run.source, run.target, run.sourceFingerprint)...)
		RaisePgResult(tx.Exec(ctx, strings.Join(statements, ";\n")))
		var copySeconds, indexSeconds float64
		Raise(tx.QueryRow(ctx, tableSwapPrecopyReportSql).Scan(&run.rowCount, &copySeconds, &indexSeconds))
		run.copyDuration = tableSwapSeconds(copySeconds)
		run.indexDuration = tableSwapSeconds(indexSeconds)
	})
}

// VACUUM ANALYZE of the target before the swap: statistics from its first
// moment as the table, and its pages marked visible. It never waits for a
// lock (SKIP_LOCKED), and a failure is only reported.
func (self *tableSwap) vacuumTarget(ctx context.Context, target string) {
	start := time.Now()
	err := partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, fmt.Sprintf("VACUUM (ANALYZE, SKIP_LOCKED) public.%s", quoteIdentifier(target))))
		})
	})
	if err != nil {
		self.logf("VACUUM ANALYZE %s failed, the swap goes on: %v", target, err)
		return
	}
	self.logf("vacuumed and analyzed %s in %s", target, tableSwapRound(time.Since(start)))
}

// One catch-up pass: the captured changes applied to the target at one
// snapshot (repeatable read), outside the lock.
func (self *tableSwap) catchUp(ctx context.Context, run *tableSwapRun) (applied int64, err error) {
	err = tableSwapTx(ctx, TxRepeatableRead, func(tx PgTx) {
		statements := append(slices.Clone(self.boundsSql()), run.capture.applySql(run.source, run.target, 0))
		RaisePgResult(tx.Exec(ctx, strings.Join(statements, ";\n")))
		Raise(tx.QueryRow(ctx, `SELECT current_setting('`+tableSwapCapturedSetting+`')::bigint`).Scan(&applied))
	})
	if err != nil {
		return 0, fmt.Errorf("catch up %s: %w", run.target, err)
	}
	run.caughtUp += applied
	run.catchUpPasses += 1
	return
}

// The target holds the table's row of every key not captured since, at one
// snapshot, outside the lock.
func (self *tableSwap) verify(ctx context.Context, run *tableSwapRun) error {
	start := time.Now()
	var outstanding int64
	err := tableSwapTx(ctx, TxRepeatableRead, func(tx PgTx) {
		statements := append(slices.Clone(self.boundsSql()), run.capture.verifySql(run.source, run.target))
		RaisePgResult(tx.Exec(ctx, strings.Join(statements, ";\n")))
		Raise(tx.QueryRow(ctx, `SELECT current_setting('`+tableSwapVerifiedSetting+`')::bigint, current_setting('`+tableSwapCapturedSetting+`')::bigint`).Scan(&run.verifiedRows, &outstanding))
	})
	if err != nil {
		return fmt.Errorf("verify %s against %s: %w", run.target, self.profile.Table, err)
	}
	run.verifyDuration = time.Since(start)
	self.logf("verified %s against %s: %d rows equal, %d captured change(s) outstanding, in %s", run.target, self.profile.Table, run.verifiedRows, outstanding, tableSwapRound(run.verifyDuration))
	return nil
}

// The stamp of the old table a swap at now keeps. A rollback keeps the table
// that was swapped in under its own stamp, a second or more after the old
// one's: a stamp has whole seconds.
func tableSwapStampAt(now time.Time, oldTable *tableSwapOldTable) string {
	if oldTable == nil {
		return now.Format(tableSwapStampLayout)
	}
	at := now.Truncate(time.Second)
	if !at.After(oldTable.stamp) {
		at = oldTable.stamp.Add(time.Second)
	}
	return at.Format(tableSwapStampLayout)
}

// The swap under the lock. Each attempt catches up, waits for admission of
// the target and the table (and no old open transaction), and sends the
// locked script; a busy lock or too many changes captured since the catch-up
// release the lock at once, and the next attempt catches up again. A failure
// the commit may have outrun (its reply lost) is checked against the catalog.
func (self *tableSwap) swapUnderLock(ctx context.Context, run *tableSwapRun) (err error) {
	table := self.profile.Table
	relations := []string{"public." + run.target, "public." + table}
	for attempt := 1; attempt <= self.settings.LockAttempts; attempt += 1 {
		if 1 < attempt {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(self.settings.LockRetryInterval):
			}
		}
		if _, err = self.catchUp(ctx, run); err != nil {
			return
		}
		reasons, now, busyErr := self.busy(ctx, relations, true)
		if busyErr != nil {
			return busyErr
		}
		if 0 < len(reasons) {
			self.logf("waiting (attempt %d/%d): %s", attempt, self.settings.LockAttempts, strings.Join(reasons, "; "))
			err = fmt.Errorf("busy: %s", strings.Join(reasons, "; "))
			continue
		}
		run.stamp = tableSwapStampAt(now, run.oldTable)
		if self.hooks.BeforeLock != nil {
			if err = partitionRegistryRun(func() { self.hooks.BeforeLock(ctx, attempt) }); err != nil {
				return
			}
		}
		beforeHook, afterHook := self.lockedSql(tableSwapLockedScript{
			source:            run.source,
			capture:           run.capture,
			target:            run.target,
			stamp:             run.stamp,
			sourceFingerprint: run.sourceFingerprint,
			oldTable:          run.oldTable,
		})
		hook := self.afterCopy
		if self.hooks.UnderLock != nil {
			hook = func(ctx context.Context, conn PgConn) {
				if self.afterCopy != nil {
					self.afterCopy(ctx, conn)
				}
				self.hooks.UnderLock(ctx)
			}
		}
		values, runErr := self.runScript(ctx, beforeHook, afterHook, tableSwapLockedReportSql, 3, hook)
		if runErr == nil && self.hooks.AfterLocked != nil {
			runErr = self.hooks.AfterLocked()
		}
		if runErr == nil {
			run.swapped = true
			if run.locked, err = parseTableSwapLockedReport(values); err != nil {
				self.logf("the swap of %s committed, but its report did not parse: %v", table, err)
			}
			return nil
		}
		if contractPartitionLockBusy(runErr) || tableSwapDeltaBusy(runErr) {
			self.logf("lock busy (attempt %d/%d): %v", attempt, self.settings.LockAttempts, runErr)
			err = runErr
			continue
		}
		if state, stateErr := self.captureState(context.WithoutCancel(ctx), run.capture); stateErr == nil && state.committed() && state.targetOid == run.targetOid {
			run.swapped = true
			self.logf("the swap of %s committed, but its reply was lost: %v", table, runErr)
			return nil
		}
		return runErr
	}
	return
}

// After the commit: the capture's log and function, which nothing uses any
// more (the locked script dropped the triggers).
func (self *tableSwap) dropCapture(ctx context.Context, capture *tableSwapCapture) error {
	return self.retryExclusive(ctx, []string{capture.quotedLog()}, func(time.Time) error {
		return contractPartitionExec(ctx, []string{
			fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds()),
			fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.settings.StatementTimeout.Milliseconds()),
			"DROP TABLE IF EXISTS " + capture.quotedLog(),
			"DROP FUNCTION IF EXISTS " + capture.functionSignature(),
		})
	})
}

// Removes what a run that did not swap left: the capture triggers first,
// under a brief ACCESS EXCLUSIVE lock admitted like the swap's, then the
// function and the log, and the twin the log names as its target (forward).
// A kept old table (rollback) stays: a rollback empties it again. After a
// committed swap only the log and the function are left to drop.
func (self *tableSwap) removeCapture(ctx context.Context, state *tableSwapCaptureState) error {
	capture := state.capture
	table := capture.table
	if 0 < len(state.triggers) {
		statements := []string{
			"BEGIN ISOLATION LEVEL READ COMMITTED",
			fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds()),
			fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.settings.StatementTimeout.Milliseconds()),
			fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(table)),
			tableSwapMarkSql(tableSwapLockedAtSetting),
		}
		for _, trigger := range state.triggers {
			name, _, _ := strings.Cut(trigger, ":")
			statements = append(statements, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON public.%s", quoteIdentifier(name), quoteIdentifier(table)))
		}
		var held time.Duration
		err := self.retryExclusive(ctx, []string{"public." + table}, func(time.Time) error {
			values, runErr := self.runScript(ctx, statements, nil, tableSwapHeldReportSql, 1, nil)
			if runErr != nil {
				return runErr
			}
			seconds, parseErr := strconv.ParseFloat(values[0], 64)
			if parseErr != nil {
				return parseErr
			}
			held = tableSwapSeconds(seconds)
			return nil
		})
		if err != nil {
			return fmt.Errorf("drop the capture triggers on %s: %w", table, err)
		}
		self.logf("dropped the capture triggers on %s; ACCESS EXCLUSIVE held %s", table, tableSwapRound(held))
	}
	statements := []string{
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings.LockTimeout.Milliseconds()),
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", self.settings.StatementTimeout.Milliseconds()),
		"DROP FUNCTION IF EXISTS " + capture.functionSignature(),
		"DROP TABLE IF EXISTS " + capture.quotedLog(),
	}
	relations := []string{capture.quotedLog()}
	if state.ownsTwin() {
		twin := table + tableSwapNextSuffix
		statements = append(statements,
			tableSwapAssertSql(
				fmt.Sprintf("to_regclass(%s)::oid = %d", tableSwapLiteral("public."+quoteIdentifier(twin)), state.targetOid),
				twin+" is no longer the twin the change capture fed",
			),
			fmt.Sprintf("DROP TABLE public.%s", quoteIdentifier(twin)),
		)
		relations = append(relations, "public."+twin)
	}
	if err := self.retryExclusive(ctx, relations, func(time.Time) error {
		return contractPartitionExec(ctx, statements)
	}); err != nil {
		return fmt.Errorf("drop the change capture's function and log: %w", err)
	}
	return nil
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
	rehearsal, sourceFingerprint, err := self.rehearse(ctx, source, nil)
	if err != nil {
		return fmt.Errorf("the rehearsal of the copy of %s failed: %w", table, err)
	}
	refusals = append(refusals, self.reportRehearsal(&rehearsal)...)
	if self.dryRun {
		self.logPlan(source, sourceFingerprint, twin, nil)
		self.logf("dry run: nothing changed")
	}
	if 0 < len(refusals) {
		return self.refuse("swap", refusals)
	}
	if self.dryRun {
		return nil
	}

	release, err := self.acquireRunLock(ctx)
	if err != nil {
		return fmt.Errorf("the swap of %s: %w", table, err)
	}
	defer release()
	if err := self.requireNoCapture(ctx, source); err != nil {
		return fmt.Errorf("the swap of %s: %w", table, err)
	}
	run, err := self.precopyAndSwap(ctx, source, sourceFingerprint, twin, nil)
	if err != nil {
		return fmt.Errorf("the swap of %s: %w", table, err)
	}
	self.logf("swapped %s in one transaction: %s; ACCESS EXCLUSIVE held %s before the commit", table, run.String(), self.heldString(run))
	self.analyze(ctx)
	self.logf("the old table is kept as %s for the rollback window; drop it with --drop-old", tableSwapOldName(table, run.stamp))
	return nil
}

func (self *tableSwap) heldString(run *tableSwapRun) string {
	if !run.locked.known {
		return "an unknown time"
	}
	return tableSwapRound(run.locked.lockDuration).String()
}

// The rehearsal line, what the checks under the lock would refuse now, and
// the refusal of an exclusive section longer than MaxLockHold.
func (self *tableSwap) reportRehearsal(rehearsal *tableSwapRehearsal) (refusals []string) {
	table := self.profile.Table
	self.logf("rehearsal, rolled back: %s", rehearsal.String(table))
	self.logf(
		"rehearsal: the verification before the lock read both tables in %s; the swap's transaction is cut off %s after its request for %s's lock",
		tableSwapRound(rehearsal.verifyDuration), self.settings.LockTimeout+self.settings.MaxLockHold, table,
	)
	if rehearsal.lockedRefusal != "" {
		self.logf("rehearsal: the checks under the lock would refuse now: %s", rehearsal.lockedRefusal)
	}
	if self.settings.MaxLockHold < rehearsal.exclusiveDuration {
		refusals = append(refusals, fmt.Sprintf("the rehearsal's exclusive section took %s, longer than the %s the swap may hold its lock", tableSwapRound(rehearsal.exclusiveDuration), self.settings.MaxLockHold))
	}
	return
}

// What a dry run would do: the pre-copy outside the lock, then the locked
// script.
func (self *tableSwap) logPlan(source *tableSwapShape, sourceFingerprint string, target string, oldTable *tableSwapOldTable) {
	table := self.profile.Table
	capture := newTableSwapCapture(table, source.primaryKey)
	kind := tableSwapForward
	plan := []string{}
	if oldTable == nil {
		plan = append(plan, source.twinSql(target)...)
	} else {
		kind = tableSwapRollback
	}
	plan = append(plan, capture.createSql(source, 0, kind)...)
	plan = append(plan, fmt.Sprintf("LOCK TABLE public.%s IN SHARE ROW EXCLUSIVE MODE", quoteIdentifier(table)))
	plan = append(plan, capture.triggerSql(table)...)
	plan = append(plan, self.precopySql(source, target, sourceFingerprint)...)
	plan = append(plan,
		fmt.Sprintf("VACUUM (ANALYZE, SKIP_LOCKED) public.%s", quoteIdentifier(target)),
		capture.applySql(source, target, 0),
		capture.verifySql(source, target),
	)
	self.logf("would pre-copy into %s outside the lock, writers writing on, with the change capture on from before the copy (the capture's lock waits at most %s):\n%s", target, self.settings.LockTimeout, strings.Join(plan, ";\n"))
	beforeHook, afterHook := self.lockedSql(tableSwapLockedScript{
		source:            source,
		capture:           capture,
		target:            target,
		stamp:             "<stamp>",
		sourceFingerprint: sourceFingerprint,
		oldTable:          oldTable,
	})
	self.logf(
		"would send this script in one message after a last catch-up, waiting at most %s for each lock, %d attempts, cut off %s after its request for %s's lock:\n%s",
		self.settings.LockTimeout, self.settings.LockAttempts, self.settings.LockTimeout+self.settings.MaxLockHold, table,
		strings.Join(append(append(slices.Clone(beforeHook), afterHook...), tableSwapLockedReportSql, "COMMIT", "ANALYZE public."+quoteIdentifier(table)), ";\n"),
	)
}

// Under the run lock, nothing of a capture may be left: an earlier run that
// died after the preflight read it.
func (self *tableSwap) requireNoCapture(ctx context.Context, source *tableSwapShape) error {
	state, err := self.captureState(ctx, newTableSwapCapture(self.profile.Table, source.primaryKey))
	if err != nil {
		return err
	}
	if state.left() {
		return fmt.Errorf("a change capture was left since the preflight (%s): run --abort", state)
	}
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

	rehearsal, sourceFingerprint, err := self.rehearse(ctx, source, &oldTable)
	if err != nil {
		return fmt.Errorf("the rehearsal of the copy of %s failed: %w", table, err)
	}
	refusals := self.reportRehearsal(&rehearsal)
	if self.dryRun {
		self.logf("would empty %s and drop its indexes under its own lock, then pre-copy %s into it and swap the names under the lock", oldTable.name, table)
		self.logPlan(source, sourceFingerprint, oldTable.name, &oldTable)
		self.logf("dry run: nothing changed")
	}
	if 0 < len(refusals) {
		return self.refuse("rollback", refusals)
	}
	if self.dryRun {
		return nil
	}

	release, err := self.acquireRunLock(ctx)
	if err != nil {
		return fmt.Errorf("the rollback of %s: %w", table, err)
	}
	defer release()
	if err := self.requireNoCapture(ctx, source); err != nil {
		return fmt.Errorf("the rollback of %s: %w", table, err)
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

	run, err := self.precopyAndSwap(ctx, source, sourceFingerprint, oldTable.name, &oldTable)
	if err != nil {
		return fmt.Errorf("the rollback of %s: %w", table, err)
	}
	self.logf("rolled %s back into %s: %s; ACCESS EXCLUSIVE held %s before the commit", table, oldTable.name, run.String(), self.heldString(run))
	self.analyze(ctx)
	self.logf("the table that was swapped in is kept as %s; drop it with --drop-old", tableSwapOldName(table, run.stamp))
	return nil
}

// Removes the change capture an interrupted run left, and the twin its log
// names (forward). Refuses while a run holds the run lock: a live run removes
// its own.
func (self *tableSwap) abort(ctx context.Context) error {
	table := self.profile.Table
	capture := newTableSwapCapture(table, nil)
	state, err := self.captureState(ctx, capture)
	if err != nil {
		return err
	}
	if !state.left() {
		self.logf("no change capture of %s is left", table)
		if self.dryRun {
			self.logf("dry run: nothing changed")
		}
		return nil
	}
	self.logf("left by a run of the swap of %s: %s", table, state)
	if 0 < len(state.runHolders) {
		return self.refuse("abort", []string{fmt.Sprintf("a run of the swap of %s is in progress (pid %v); it removes its change capture itself", table, state.runHolders)})
	}
	if self.dryRun {
		self.logf("would drop the capture triggers on %s under a brief ACCESS EXCLUSIVE lock, then the capture's function and log%s", table, tableSwapTwinNote(state))
		self.logf("dry run: nothing changed")
		return nil
	}
	release, err := self.acquireRunLock(ctx)
	if err != nil {
		return fmt.Errorf("the abort of %s: %w", table, err)
	}
	defer release()
	if state, err = self.captureState(ctx, capture); err != nil {
		return err
	}
	if err := self.removeCapture(ctx, state); err != nil {
		return fmt.Errorf("the abort of %s: %w", table, err)
	}
	self.logf("removed the change capture of %s%s", table, tableSwapTwinNote(state))
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
