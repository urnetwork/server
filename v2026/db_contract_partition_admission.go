// Lock admission for the contract cutover's steps (CONTRACT-PARTITION.md,
// "Where the build differs", 3).
//
// Each step that locks a live contract table first reads who holds or awaits a
// lock it would wait for, and waits or defers instead of queueing behind them:
//
//   - An autovacuum worker, whatever its age. A request queued behind its lock
//     for deadlock_timeout (1 s on Main) makes PostgreSQL cancel a routine
//     autovacuum, whose pass over these tables takes hours and then starts
//     over (db_maintenance.go, 2026-10-13), and it waits out its whole lock
//     timeout behind an anti-wraparound one, which nothing cancels. Since
//     2026-11 the large contract tables are under autovacuum most of the day.
//   - Any transaction older than MaxTransactionAge that holds or awaits a lock
//     the request conflicts with.
//
// The scope includes every partition below the named relations: once the
// tables are partitioned their vacuums run on the leaves, and a LOCK TABLE of
// a parent waits for the locks of its leaves.
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Settings of the cutover's prepare, finalize and abort steps. Rollback has
// its own (ContractPartitionRollbackSettings).
type ContractPartitionCutoverSettings struct {
	// Admission before each single-table lock request: the legacy bound's add
	// and validation, and abort (MaxTransactionAge must be positive).
	LockSettings ContractPartitionLockSettings
	// Bounds each single-table wait for an ACCESS EXCLUSIVE lock on a live
	// contract table: adding the legacy bound and abort. Every application
	// statement on the table queues behind the request, and a main-pool
	// statement waits at most PgLockTimeout (2 s) for a lock before it fails
	// with 55P03, which is not rerun. A queued statement also waits out the
	// step's own work, so this stays well below PgLockTimeout, and below
	// deadlock_timeout (1 s on Main), so a lock cycle with an application
	// transaction always ends this request, never the application's. At least
	// a millisecond.
	LockTimeout time.Duration
	// Bounds the legacy bound validation's wait for its SHARE UPDATE EXCLUSIVE
	// lock. Application statements do not queue behind it, but a waiting
	// validation holds a snapshot and would cancel a routine autovacuum that
	// started after the admission read. At least a millisecond.
	ValidateLockTimeout time.Duration
	// Admission reads per table and step in one prepare run before the table
	// is deferred to the next run; a brief analyze or a just-finished
	// transaction clears within them. At least 1.
	StepAttempts      int
	StepRetryInterval time.Duration
	// The swap's admission, and the bound on its LOCK TABLE statement as a
	// whole (statement_timeout) and on each lock in it (lock_timeout): it takes
	// the six tables' ACCESS EXCLUSIVE locks in one transaction.
	SwapLockSettings ContractPartitionLockSettings
	SwapLockTimeout  time.Duration
	// Attempts at the swap, each after its own admission. At least 1.
	SwapAttempts      int
	SwapRetryInterval time.Duration
	// Attempts per table at abort, which the maintenance task also runs when
	// it abandons a stale prepare. At least 1.
	AbortAttempts      int
	AbortRetryInterval time.Duration
}

// A single-table exclusive step (the legacy bound's add, abort, and the
// maintenance task's abandonment of a stale prepare) waits out the
// application's transactions but no long reader, and spaces its tries.
//
// On Main (2026-11-23) the contract tables always have holders: dozens of
// application transactions idle in transaction for 0.5 to 2.6 s, on several of
// the tables at once, and readers of 26 to 45 s (the provider list's fill, the
// payout sweeps). A lock request is granted only once every holder it found
// has ended, and until then every new statement on the table queues behind
// it. So:
//   - The admission skips a moment with a holder older than 3 s (a long
//     reader); the application's transactions are younger.
//   - The request waits at most 900 ms: below deadlock_timeout (1 s), so in a
//     lock cycle with an application transaction this request is the one that
//     ends, and well below PgLockTimeout (2 s), at which queued application
//     statements fail.
//   - The tries are 20 to 40 s apart (contractRetryWait), so each stall of at
//     most 900 ms is alone: PgBouncer absorbed single 1 s waits that night,
//     and backed up past 500 waiting clients when three came within seconds
//     on two tables. A run of the maintenance task (a 10 minute budget, every
//     15 minutes; its abandonment stops at the budget and resumes) makes
//     about 20 tries.
//
// A rule of 500 ms and 200 ms (1cec91a1d) never admitted a step on those
// tables: abort and the abandonment could never drop a stale prepare's legacy
// bound, which refuses every contract from the cutover day.
const (
	contractStepMaxTransactionAge = 3 * time.Second
	contractStepLockTimeout       = 900 * time.Millisecond
	contractStepRetryInterval     = 20 * time.Second
	contractStepAttempts          = 20
)

// The swap and rollback take the six tables' ACCESS EXCLUSIVE locks in one
// transaction: a bounded blip of contract traffic, a few times at most.
//   - The admission skips a moment with a holder older than 3 s, as the
//     single-table steps do.
//   - The LOCK TABLE statement as a whole waits at most 1.6 s
//     (statement_timeout, and lock_timeout for each lock in it): with the
//     swap's own catalog work (about 0.1 to 0.3 s from Main's network) a
//     queued application statement waits under PgLockTimeout (2 s), so none
//     fails with 55P03. On 2026-11-23 the six tables' holders all ended
//     within 1.5 s at 9 to 17% of moments, and none within 0.2 s.
//   - The tables are locked in ContractPartitionTables order: transfer_contract
//     first, which every application path but settlement touches first. A
//     settlement that read transfer_escrow and waits for transfer_contract
//     behind the swap waited first, so its deadlock check ends it (40P01);
//     settlement is not rerun and completes on the next report or deadline.
//   - At most 10 tries, 2 to 4 minutes apart (contractRetryWait), so each
//     blip is alone; a run lasts at most about 40 minutes.
//
// A rule of 500 ms and 200 ms (1cec91a1d) never admitted the swap on Main's
// workload of 2026-11-23.
const (
	contractSwapMaxTransactionAge = 3 * time.Second
	contractSwapLockWait          = 1600 * time.Millisecond
	contractSwapRetryInterval     = 2 * time.Minute
	contractSwapAttempts          = 10
)

func DefaultContractPartitionCutoverSettings() *ContractPartitionCutoverSettings {
	return &ContractPartitionCutoverSettings{
		LockSettings:        ContractPartitionLockSettings{MaxTransactionAge: contractStepMaxTransactionAge},
		LockTimeout:         contractStepLockTimeout,
		ValidateLockTimeout: 5 * time.Second,
		StepAttempts:        contractStepAttempts,
		StepRetryInterval:   contractStepRetryInterval,
		SwapLockSettings:    ContractPartitionLockSettings{MaxTransactionAge: contractSwapMaxTransactionAge},
		SwapLockTimeout:     contractSwapLockWait,
		SwapAttempts:        contractSwapAttempts,
		SwapRetryInterval:   contractSwapRetryInterval,
		AbortAttempts:       contractStepAttempts,
		AbortRetryInterval:  contractStepRetryInterval,
	}
}

func (self *ContractPartitionCutoverSettings) validate() error {
	if self.LockSettings.MaxTransactionAge <= 0 || self.LockTimeout < time.Millisecond || self.ValidateLockTimeout < time.Millisecond ||
		self.SwapLockSettings.MaxTransactionAge <= 0 || self.SwapLockTimeout < time.Millisecond ||
		self.StepAttempts < 1 || self.SwapAttempts < 1 || self.AbortAttempts < 1 ||
		self.StepRetryInterval < 0 || self.SwapRetryInterval < 0 || self.AbortRetryInterval < 0 {
		return errors.New("the cutover requires a positive transaction age, lock timeouts of at least a millisecond, at least one attempt per step and non-negative retry intervals")
	}
	return nil
}

// The lock a step requests on its scope.
type contractLockRequest int

const (
	// ACCESS EXCLUSIVE: the legacy bound, the swap, abort and rollback. It
	// conflicts with every lock, application reads and writes included.
	contractLockAccessExclusive contractLockRequest = iota
	// SHARE UPDATE EXCLUSIVE: the legacy bound's validation. It conflicts with
	// vacuum, analyze, index builds and DDL, not with reads and writes.
	contractLockShareUpdateExclusive
)

// The relations a step locks, the lock it requests and its admission.
type contractLockScope struct {
	// each relation and every partition below it
	relations []string
	// relations the step locks ONLY, without their partitions: a week's
	// retirement locks the parents and that week's leaves, never the other
	// leaves, whose vacuums it need not wait for
	only []string
	// with the TOAST table of each relation: a step that rewrites or drops a
	// table locks its TOAST table too, which autovacuum vacuums on its own
	toast    bool
	request  contractLockRequest
	settings ContractPartitionLockSettings
}

// Every relation lock that another backend holds or awaits in the scope ($1,
// relation names, and every partition below them; $2, relation names alone;
// with $3, the TOAST table of each), with what the admission judges it by. A
// prepared transaction has no backend (backend_type null). TOAST tables are
// found by their owner's reltoastrelid, never by name: a name lookup in
// pg_toast needs a privilege on that schema.
const contractScopeLocksSql = `
	WITH RECURSIVE tree (relid) AS (
		SELECT to_regclass(name) FROM unnest($1::text[]) AS name WHERE to_regclass(name) IS NOT NULL
		UNION
		SELECT inheritance.inhrelid
		FROM pg_inherits AS inheritance
		JOIN tree ON inheritance.inhparent = tree.relid
	), named (relid) AS (
		SELECT relid FROM tree
		UNION
		SELECT to_regclass(name) FROM unnest($2::text[]) AS name WHERE to_regclass(name) IS NOT NULL
	), scope (relid) AS (
		SELECT relid FROM named
		UNION
		SELECT relation.reltoastrelid
		FROM pg_class AS relation
		WHERE $3 AND relation.oid IN (SELECT relid FROM named) AND relation.reltoastrelid <> 0
	)
	SELECT
		coalesce(lock.pid, 0),
		activity.backend_type,
		lock.relation::regclass::text,
		lock.mode,
		lock.granted,
		extract(epoch FROM clock_timestamp() - activity.xact_start)::float8
	FROM pg_locks AS lock
	LEFT JOIN pg_stat_activity AS activity ON activity.pid = lock.pid
	WHERE lock.locktype = 'relation'
		AND lock.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		AND lock.relation IN (SELECT relid FROM scope)
		AND lock.pid IS DISTINCT FROM pg_backend_pid()
	ORDER BY lock.pid, lock.granted DESC, lock.relation::regclass::text
`

// One relation lock in a step's scope.
type contractScopeLock struct {
	// 0 for a prepared transaction
	pid int64
	// empty when the session is not visible to this role (pg_stat_activity
	// shows another role's sessions only to pg_read_all_stats; on Main every
	// pool connects as the same superuser role), or for a prepared transaction
	backendType string
	relation    string
	mode        string
	granted     bool
	// the age of the holder's transaction; negative when unknown
	transactionAge time.Duration
}

// The lock modes that conflict with SHARE UPDATE EXCLUSIVE.
var contractShareUpdateExclusiveConflicts = map[string]bool{
	"ShareUpdateExclusiveLock": true,
	"ShareLock":                true,
	"ShareRowExclusiveLock":    true,
	"ExclusiveLock":            true,
	"AccessExclusiveLock":      true,
}

// Whether a request should not queue behind lock: an autovacuum worker's lock,
// whatever its age or mode; a prepared transaction's, which nothing ends; a
// session this role cannot see, which it cannot judge; and the lock of a
// transaction older than maxTransactionAge when its mode conflicts with the
// request (every mode conflicts with ACCESS EXCLUSIVE).
func contractScopeLockBlocks(request contractLockRequest, lock contractScopeLock, maxTransactionAge time.Duration) bool {
	switch {
	case lock.pid == 0 || lock.backendType == "":
		return true
	case lock.backendType == "autovacuum worker":
		return true
	case lock.transactionAge < maxTransactionAge:
		return false
	case request == contractLockAccessExclusive:
		return true
	default:
		return contractShareUpdateExclusiveConflicts[lock.mode]
	}
}

func (self contractScopeLock) String() string {
	holds := "holds"
	if !self.granted {
		holds = "awaits"
	}
	backendType := self.backendType
	switch {
	case self.pid == 0:
		backendType = "prepared transaction"
	case backendType == "":
		backendType = "session not visible to this role"
	}
	line := fmt.Sprintf("pid %d (%s) %s %s on %s", self.pid, backendType, holds, self.mode, self.relation)
	if 0 <= self.transactionAge {
		line += fmt.Sprintf(", its transaction %s old", self.transactionAge.Round(time.Second))
	}
	return line
}

// Sees the locks each admission read found, and returns the locks it judges.
// Nil in production; a test presents a lock no test session can take, such
// as an autovacuum worker's.
var contractScopeLocksObserved func(scope *contractLockScope, locks []contractScopeLock) []contractScopeLock

// The backends the scope's lock request should not queue behind, one line
// each (one per backend), or none.
func contractLockBlockers(ctx context.Context, conn PgQuerier, scope *contractLockScope) (blockers []string) {
	relations := scope.relations
	if relations == nil {
		relations = []string{}
	}
	only := scope.only
	if only == nil {
		only = []string{}
	}
	var locks []contractScopeLock
	result, err := conn.Query(ctx, contractScopeLocksSql, relations, only, scope.toast)
	WithPgResult(result, err, func() {
		for result.Next() {
			var lock contractScopeLock
			var backendType *string
			var age *float64
			Raise(result.Scan(&lock.pid, &backendType, &lock.relation, &lock.mode, &lock.granted, &age))
			if backendType != nil {
				lock.backendType = *backendType
			}
			lock.transactionAge = -1
			if age != nil {
				lock.transactionAge = time.Duration(*age * float64(time.Second))
			}
			locks = append(locks, lock)
		}
	})
	if contractScopeLocksObserved != nil {
		locks = contractScopeLocksObserved(scope, locks)
	}
	blocking := map[int64]bool{}
	for _, lock := range locks {
		if blocking[lock.pid] || !contractScopeLockBlocks(scope.request, lock, scope.settings.MaxTransactionAge) {
			continue
		}
		blocking[lock.pid] = true
		blockers = append(blockers, lock.String())
	}
	return
}

func contractLockAdmission(ctx context.Context, scope *contractLockScope) (blockers []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			if recoveredErr, ok := r.(error); ok {
				err = recoveredErr
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	MaintenanceDb(ctx, func(conn PgConn) {
		blockers = contractLockBlockers(ctx, conn, scope)
	})
	return
}

// A step that did not run because its scope was busy. Nothing changed.
type ContractPartitionBusyError struct {
	Blockers []string
}

func (self *ContractPartitionBusyError) Error() string {
	return fmt.Sprintf("the contract tables are busy: %s", strings.Join(self.Blockers, "; "))
}

// Prepare left tables for a later run: their legacy bound was not added or
// not validated because a vacuum or a long transaction held them. Everything
// else it reports done is done; prepare is resumable, so run it again.
type ContractPartitionDeferredError struct {
	// table: why
	Deferred []string
}

func (self *ContractPartitionDeferredError) Error() string {
	return fmt.Sprintf("prepare deferred %d table(s); run it again: %s", len(self.Deferred), strings.Join(self.Deferred, "; "))
}

// The SET LOCAL statement of a lock timeout, in milliseconds.
func contractLockTimeoutSql(timeout time.Duration) string {
	return fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", max(int64(1), timeout.Milliseconds()))
}
