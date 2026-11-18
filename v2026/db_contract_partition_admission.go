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
	// Admission before each lock request (MaxTransactionAge must be positive).
	LockSettings ContractPartitionLockSettings
	// Bounds each wait for an ACCESS EXCLUSIVE lock on a live contract table:
	// adding the legacy bound, the swap and abort. Every application statement
	// on the table queues behind the request, and a main-pool statement waits
	// at most PgLockTimeout (2 s) for a lock before it fails with 55P03, which
	// is not rerun. A queued statement also waits out the step's own work, so
	// this stays well below PgLockTimeout. At least a millisecond.
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
	// Attempts at the swap, each after its own admission. At least 1.
	SwapAttempts      int
	SwapRetryInterval time.Duration
	// Attempts per table at abort, which the maintenance task also runs when
	// it abandons a stale prepare. At least 1.
	AbortAttempts      int
	AbortRetryInterval time.Duration
}

func DefaultContractPartitionCutoverSettings() *ContractPartitionCutoverSettings {
	return &ContractPartitionCutoverSettings{
		LockSettings:        *DefaultContractPartitionLockSettings(),
		LockTimeout:         time.Second,
		ValidateLockTimeout: 5 * time.Second,
		StepAttempts:        3,
		StepRetryInterval:   2 * time.Second,
		SwapAttempts:        60,
		SwapRetryInterval:   10 * time.Second,
		AbortAttempts:       24,
		AbortRetryInterval:  5 * time.Second,
	}
}

func (self *ContractPartitionCutoverSettings) validate() error {
	if self.LockSettings.MaxTransactionAge <= 0 || self.LockTimeout < time.Millisecond || self.ValidateLockTimeout < time.Millisecond ||
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
