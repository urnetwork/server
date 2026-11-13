// Decides which failed statements and commits a rerun of the whole database
// callback can resolve, so `Db` and `Tx` rerun only those and surface every
// other failure at once with its own cause.
//
// The taxonomy:
//   - always rerun: serialization_failure (40001), deadlock_detected (40P01)
//     and an explicit `PgRetry`. Postgres rolled back for a conflict with
//     concurrent work that a rerun with a fresh snapshot avoids.
//   - rerun unless repeated: unique_violation (23505) and
//     foreign_key_violation (23503). A concurrent writer can cause one: at
//     repeatable read, a row committed after the transaction's snapshot is
//     invisible to its reads and meets its insert on the unique index (the
//     select-then-insert idiom across the model relies on that rerun). A
//     permanent conflict causes one too. The rerun tells them apart: its fresh
//     snapshot sees every concurrent commit, so when it ends in the very same
//     violation, no concurrent writer is going to resolve it.
//   - never rerun: everything else, including not_null_violation (23502),
//     check_violation (23514), the rest of class 23, every data exception
//     (class 22), and statement_completion_unknown (40003).
//
// A callback that goes on after one of its statements failed leaves an
// aborted transaction: postgres refuses every later statement
// (in_failed_sql_transaction, 25P02) and turns the commit into a rollback,
// which pgx reports as `pgx.ErrTxCommitRollback` without the cause. Each pooled
// connection records the statement error that aborted its transaction, and the
// commit or the refused statement is classified by that recorded cause. A
// rollback with no recorded cause is not rerun.
package server

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"
)

// What a rerun of the whole callback can do about a failure.
type pgRetryClass int

const (
	// a rerun repeats the failure
	pgRetryNever pgRetryClass = iota
	// a rerun with a fresh snapshot avoids the conflict
	pgRetryAlways
	// a rerun resolves a conflict with a concurrent writer, and repeats a
	// permanent one exactly
	pgRetryUnlessRepeated
)

// Classifies by the first database error in the chain. A `txAbortedError`
// unwraps to its recorded statement error first, so an aborted transaction is
// classified by the statement that aborted it.
func pgRetryClassOf(err error) pgRetryClass {
	var retryErr *PgRetry
	if errors.As(err, &retryErr) {
		return pgRetryAlways
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return pgRetryNever
	}
	switch pgErr.Code {
	case pgerrcode.SerializationFailure, pgerrcode.DeadlockDetected:
		return pgRetryAlways
	case pgerrcode.UniqueViolation, pgerrcode.ForeignKeyViolation:
		return pgRetryUnlessRepeated
	default:
		return pgRetryNever
	}
}

// The fields that identify a violation independent of when it happened. Two
// attempts that fail with equal identities hit the same constraint on the same
// key.
type pgViolationIdentity struct {
	code           string
	schemaName     string
	tableName      string
	constraintName string
	columnName     string
	message        string
	detail         string
}

// The failures of a retry loop's attempts, as evidence for whether the next
// rerun can succeed. Each retry loop owns one; not safe for concurrent use.
type callbackRetryEvidence struct {
	previousViolation    pgViolationIdentity
	hasPreviousViolation bool
}

// Reports whether a rerun of the callback can succeed where this attempt
// failed with err, and keeps err as the evidence the next attempt is judged
// by. A unique or foreign key violation is rerun unless the previous attempt
// ended in the identical violation (see the file comment).
func (self *callbackRetryEvidence) CanRerun(err error) bool {
	class := pgRetryClassOf(err)
	repeated := false
	isViolation := false
	var identity pgViolationIdentity
	var pgErr *pgconn.PgError
	if class == pgRetryUnlessRepeated && errors.As(err, &pgErr) {
		isViolation = true
		identity = pgViolationIdentity{
			code:           pgErr.Code,
			schemaName:     pgErr.SchemaName,
			tableName:      pgErr.TableName,
			constraintName: pgErr.ConstraintName,
			columnName:     pgErr.ColumnName,
			message:        pgErr.Message,
			detail:         pgErr.Detail,
		}
		repeated = self.hasPreviousViolation && self.previousViolation == identity
	}
	self.previousViolation = identity
	self.hasPreviousViolation = isViolation

	switch class {
	case pgRetryAlways:
		recordRerunDecision(err, rerunDecisionRerun)
		return true
	case pgRetryUnlessRepeated:
		if repeated {
			recordRerunDecision(err, rerunDecisionEndedRepeated)
			return false
		}
		recordRerunDecision(err, rerunDecisionRerun)
		return true
	default:
		return false
	}
}

// A transaction its callback went on with after one of its statements failed.
// Postgres refuses every later statement and turns the commit into a rollback.
type txAbortedError struct {
	// the statement error that aborted the transaction, as the connection
	// recorded it; nil when none was recorded
	statementErr error
	// what the callback ended in instead: the commit's rollback
	// (`pgx.ErrTxCommitRollback`), or a later statement refused in the aborted
	// transaction (25P02)
	err error
}

// What the callback ended in, then the recorded cause.
func (self *txAbortedError) Error() string {
	if self.statementErr == nil {
		return fmt.Sprintf("%v: the transaction callback went on after a failed statement, and no statement error was recorded", self.err)
	}
	return fmt.Sprintf("%v: the transaction callback went on after a failed statement: %v", self.err, self.statementErr)
}

// The statement error comes first, so `errors.As` finds the cause before the
// rollback or the refusal.
func (self *txAbortedError) Unwrap() []error {
	if self.statementErr == nil {
		return []error{self.err}
	}
	return []error{self.statementErr, self.err}
}

// The `PgConn.CustomData` key of a connection's statement error recorder.
const pgStatementErrorRecorderKey = "github.com/urnetwork/server/v2026.statementErrorRecorder"

// The statement error that last aborted a connection's transaction: the latest
// error the connection received other than a refusal in an already aborted
// transaction. A savepoint rollback that recovers the transaction is followed
// by a newer error before any later abort, so the latest error is the cause.
// Written by pgx's error handler on the goroutine that reads the connection;
// the atomic also orders a write made by the pool's idle health check.
type pgStatementErrorRecorder struct {
	statementErr atomic.Pointer[pgconn.PgError]
}

// Records statement errors on every connection of the pool, keeping pgx's
// handling (which closes the connection on a FATAL error).
func configurePgPoolStatementErrors(config *pgxpool.Config) {
	onPgError := config.ConnConfig.OnPgError
	config.ConnConfig.OnPgError = func(conn *pgconn.PgConn, pgErr *pgconn.PgError) bool {
		if pgErr.Code != pgerrcode.InFailedSQLTransaction {
			data := conn.CustomData()
			recorder, _ := data[pgStatementErrorRecorderKey].(*pgStatementErrorRecorder)
			if recorder == nil {
				recorder = &pgStatementErrorRecorder{}
				data[pgStatementErrorRecorderKey] = recorder
			}
			recorder.statementErr.Store(pgErr)
		}
		if onPgError == nil {
			return !strings.EqualFold(pgErr.Severity, "FATAL")
		}
		return onPgError(conn, pgErr)
	}
}

// The connection's recorder, or nil before the connection received its first
// statement error.
func pgStatementErrorRecorderOf(conn *pgconn.PgConn) *pgStatementErrorRecorder {
	recorder, _ := conn.CustomData()[pgStatementErrorRecorderKey].(*pgStatementErrorRecorder)
	return recorder
}

// Forgets the recorded error, so an earlier use of the pooled connection is
// not taken as evidence. Safe on a nil recorder.
func (self *pgStatementErrorRecorder) Reset() {
	if self != nil {
		self.statementErr.Store(nil)
	}
}

// The recorded statement error as an error (a nil interface when none was
// recorded). Safe on a nil recorder.
func (self *pgStatementErrorRecorder) StatementErr() error {
	if self == nil {
		return nil
	}
	if pgErr := self.statementErr.Load(); pgErr != nil {
		return pgErr
	}
	return nil
}

// Attributes a statement refused in an aborted transaction (25P02) to the
// recorded statement error that aborted it, and reports the callback that went
// on. Any other error is returned as is.
func withAbortingStatementError(conn *pgconn.PgConn, err error, callback any) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.InFailedSQLTransaction {
		return err
	}
	statementErr := pgStatementErrorRecorderOf(conn).StatementErr()
	if statementErr == nil {
		return err
	}
	abortedErr := &txAbortedError{
		statementErr: statementErr,
		err:          err,
	}
	reportAbortedTransaction(callback, abortedErr)
	return abortedErr
}

// The commit error for a commit postgres turned into a rollback, carrying the
// recorded statement error, and reports the callback that went on.
func abortedCommitError(conn *pgconn.PgConn, commitErr error, callback any) error {
	abortedErr := &txAbortedError{
		statementErr: pgStatementErrorRecorderOf(conn).StatementErr(),
		err:          commitErr,
	}
	reportAbortedTransaction(callback, abortedErr)
	return abortedErr
}

// The minimum time between two default-level reports of aborted transactions.
const abortedTransactionReportInterval = 10 * time.Second

// Bounds the default-level reports of transactions a callback went on with
// after a failed statement (a defect in that callback): at most one line per
// interval, counting the rest. Safe for concurrent use.
type abortedTransactionLog struct {
	stateLock       sync.Mutex
	nextReportTime  time.Time
	suppressedCount int
}

// The process's one bounded report of aborted transactions.
var abortedTransactions = &abortedTransactionLog{}

// The line to log for this report at now, and whether to log it. The line
// names the callback and the statement error by schema identifiers only:
// postgres messages and details can carry user data.
func (self *abortedTransactionLog) Line(now time.Time, callbackName string, abortedErr *txAbortedError) (string, bool) {
	suppressedCount := 0
	ok := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if now.Before(self.nextReportTime) {
			self.suppressedCount += 1
			return false
		}
		self.nextReportTime = now.Add(abortedTransactionReportInterval)
		suppressedCount = self.suppressedCount
		self.suppressedCount = 0
		return true
	}()
	if !ok {
		return "", false
	}
	cause := "no statement error was recorded"
	var pgErr *pgconn.PgError
	if errors.As(abortedErr.statementErr, &pgErr) {
		cause = fmt.Sprintf(
			"sqlstate %s table %q constraint %q column %q",
			pgErr.Code,
			pgErr.TableName,
			pgErr.ConstraintName,
			pgErr.ColumnName,
		)
	}
	ended := "the commit rolled back"
	if !errors.Is(abortedErr.err, pgx.ErrTxCommitRollback) {
		ended = "a later statement was refused"
	}
	return fmt.Sprintf(
		"[db]transaction callback %s went on after a failed statement (%s); %s; %d more since the last report\n",
		callbackName,
		cause,
		ended,
		suppressedCount,
	), true
}

// Logs the callback that went on after a failed statement (bounded), and
// counts it.
func reportAbortedTransaction(callback any, abortedErr *txAbortedError) {
	abortedTransactionCounter.WithLabelValues(rerunMetricClass(abortedErr.statementErr)).Inc()
	callbackName := "?"
	if f := reflect.ValueOf(callback); f.Kind() == reflect.Func && !f.IsNil() {
		if fn := runtime.FuncForPC(f.Pointer()); fn != nil {
			callbackName = fn.Name()
		}
	}
	if line, ok := abortedTransactions.Line(time.Now(), callbackName, abortedErr); ok {
		glog.Infof("%s", line)
	}
}

// The decisions a retry loop makes about a rerunnable failure.
const (
	rerunDecisionRerun         = "rerun"
	rerunDecisionEndedRepeated = "ended_repeated"
	rerunDecisionEndedBudget   = "ended_budget"
)

// Counts reruns, and the reruns ended by a repeated violation or by the
// budget, so a rollout can watch what stopped being rerun.
var rerunDecisionCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "db",
		Name:      "rerun_decisions_total",
		Help:      "Decisions about rerunning a database callback after a rerunnable failure, by failure class",
	},
	[]string{"class", "decision"},
)

// Counts the callbacks that went on after a failed statement (each a defect
// to fix), by the cause's class.
var abortedTransactionCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "db",
		Name:      "aborted_transactions_total",
		Help:      "Transactions a callback went on with after a failed statement, by the recorded statement's failure class",
	},
	[]string{"class"},
)

// Registers the rerun metrics with the default registry.
func init() {
	prometheus.MustRegister(rerunDecisionCounter, abortedTransactionCounter)
}

// A bounded metric label for the failure.
func rerunMetricClass(err error) string {
	var retryErr *PgRetry
	if errors.As(err, &retryErr) {
		return "explicit_retry"
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return "unrecorded"
	}
	switch pgErr.Code {
	case pgerrcode.SerializationFailure:
		return "serialization_failure"
	case pgerrcode.DeadlockDetected:
		return "deadlock_detected"
	case pgerrcode.UniqueViolation:
		return "unique_violation"
	case pgerrcode.ForeignKeyViolation:
		return "foreign_key_violation"
	default:
		return "other"
	}
}

// Counts one rerun decision about the failure.
func recordRerunDecision(err error, decision string) {
	rerunDecisionCounter.WithLabelValues(rerunMetricClass(err), decision).Inc()
}
