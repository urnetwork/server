// Pins which failures `Db` and `Tx` rerun and which they surface at once, on
// the pgx wire protocol and against postgres. A callback counts its attempts;
// one past the expected count ends the run without failing, so a rerun that
// should not happen shows as an extra attempt, not as a minute-long wait.
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// The statement the wire tests fail or let through.
const syntheticEffectStatement = "INSERT INTO synthetic_effect VALUES (1)"

// Never pings, so every query on the wire is the callback's own.
func neverPingTestPool(context.Context, pgxpool.ShouldPingParams) bool {
	return false
}

// A postgres error response for the code, with a detail that tells two keys
// apart.
func syntheticPgError(code string, detail string) *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{
		Severity:       "ERROR",
		Code:           code,
		Message:        "synthetic " + code,
		Detail:         detail,
		TableName:      "synthetic_effect",
		ConstraintName: "synthetic_effect_key",
	}
}

// The value the counter holds.
func counterValue(t testing.TB, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetCounter().GetValue()
}

// Every failure class, by its SQLSTATE, and what a rerun can do about it.
func TestPgRetryTaxonomyClassifiesFailures(t *testing.T) {
	for _, c := range []struct {
		err   error
		class pgRetryClass
	}{
		{err: &pgconn.PgError{Code: "40001"}, class: pgRetryAlways},
		{err: &pgconn.PgError{Code: "40P01"}, class: pgRetryAlways},
		{err: fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "40001"}), class: pgRetryAlways},
		{err: &PgRetry{}, class: pgRetryAlways},
		{err: &pgconn.PgError{Code: "23505"}, class: pgRetryUnlessRepeated},
		{err: &pgconn.PgError{Code: "23503"}, class: pgRetryUnlessRepeated},
		{err: &pgconn.PgError{Code: "23502"}, class: pgRetryNever},
		{err: &pgconn.PgError{Code: "23514"}, class: pgRetryNever},
		{err: &pgconn.PgError{Code: "23P01"}, class: pgRetryNever},
		{err: &pgconn.PgError{Code: "23001"}, class: pgRetryNever},
		{err: &pgconn.PgError{Code: "23000"}, class: pgRetryNever},
		{err: &pgconn.PgError{Code: "22001"}, class: pgRetryNever},
		{err: &pgconn.PgError{Code: "22P02"}, class: pgRetryNever},
		{err: &pgconn.PgError{Code: "22023"}, class: pgRetryNever},
		{err: &pgconn.PgError{Code: "40000"}, class: pgRetryNever},
		{err: &pgconn.PgError{Code: "40002"}, class: pgRetryNever},
		{err: &pgconn.PgError{Code: "40003"}, class: pgRetryNever},
		{err: &pgconn.PgError{Code: "25P02"}, class: pgRetryNever},
		{err: &pgconn.PgError{Code: "42P01"}, class: pgRetryNever},
		{err: pgx.ErrTxCommitRollback, class: pgRetryNever},
		{err: errors.New("synthetic application error"), class: pgRetryNever},
		{err: &txAbortedError{statementErr: &pgconn.PgError{Code: "40001"}, err: pgx.ErrTxCommitRollback}, class: pgRetryAlways},
		{err: &txAbortedError{statementErr: &pgconn.PgError{Code: "23505"}, err: pgx.ErrTxCommitRollback}, class: pgRetryUnlessRepeated},
		{err: &txAbortedError{statementErr: &pgconn.PgError{Code: "23502"}, err: pgx.ErrTxCommitRollback}, class: pgRetryNever},
		{err: &txAbortedError{err: pgx.ErrTxCommitRollback}, class: pgRetryNever},
		{err: &txAbortedError{statementErr: &pgconn.PgError{Code: "23502"}, err: &pgconn.PgError{Code: "25P02"}}, class: pgRetryNever},
	} {
		if class := pgRetryClassOf(c.err); class != c.class {
			t.Errorf("%v class=%d, want %d", c.err, class, c.class)
		}
		if transient := isTransientError(c.err); transient != (c.class != pgRetryNever) {
			t.Errorf("%v transient=%t, want %t", c.err, transient, c.class != pgRetryNever)
		}
	}
}

// A violation is rerun once; the same violation again ends the loop. A
// different key, a different constraint, or a failure in between is new
// evidence.
func TestCallbackRetryEvidenceEndsOnRepeatedViolation(t *testing.T) {
	violation := func(constraintName string, detail string) error {
		return &pgconn.PgError{Code: "23505", ConstraintName: constraintName, Detail: detail}
	}
	for _, c := range []struct {
		name      string
		errs      []error
		canReruns []bool
	}{
		{
			name:      "repeated",
			errs:      []error{violation("k", "Key (id)=(1)"), violation("k", "Key (id)=(1)")},
			canReruns: []bool{true, false},
		},
		{
			name:      "regenerated keys",
			errs:      []error{violation("k", "Key (id)=(1)"), violation("k", "Key (id)=(2)"), violation("k", "Key (id)=(3)")},
			canReruns: []bool{true, true, true},
		},
		{
			name:      "another constraint",
			errs:      []error{violation("k", "Key (id)=(1)"), violation("j", "Key (id)=(1)")},
			canReruns: []bool{true, true},
		},
		{
			name:      "serialization failure between",
			errs:      []error{violation("k", "Key (id)=(1)"), &pgconn.PgError{Code: "40001"}, violation("k", "Key (id)=(1)"), violation("k", "Key (id)=(1)")},
			canReruns: []bool{true, true, true, false},
		},
		{
			name:      "foreign key",
			errs:      []error{&pgconn.PgError{Code: "23503", ConstraintName: "f", Detail: "Key (p)=(1) is not present"}, &pgconn.PgError{Code: "23503", ConstraintName: "f", Detail: "Key (p)=(1) is not present"}},
			canReruns: []bool{true, false},
		},
		{
			name:      "swallowed violation",
			errs:      []error{&txAbortedError{statementErr: violation("k", "Key (id)=(1)"), err: pgx.ErrTxCommitRollback}, violation("k", "Key (id)=(1)")},
			canReruns: []bool{true, false},
		},
		{
			name:      "deadlocks",
			errs:      []error{&pgconn.PgError{Code: "40P01"}, &pgconn.PgError{Code: "40P01"}, &pgconn.PgError{Code: "40P01"}},
			canReruns: []bool{true, true, true},
		},
		{
			name:      "permanent",
			errs:      []error{&pgconn.PgError{Code: "23502"}},
			canReruns: []bool{false},
		},
	} {
		evidence := &callbackRetryEvidence{}
		for i, err := range c.errs {
			if canRerun := evidence.CanRerun(err); canRerun != c.canReruns[i] {
				t.Errorf("%s: attempt %d %v can rerun=%t, want %t", c.name, i+1, err, canRerun, c.canReruns[i])
			}
		}
	}
}

// The cause comes first, so callers that look for the database error find the
// statement that aborted the transaction; the rollback stays recognizable.
func TestTxAbortedErrorUnwrapsCauseFirst(t *testing.T) {
	cause := &pgconn.PgError{Code: "23502", Message: "synthetic not null"}
	refused := &pgconn.PgError{Code: "25P02", Message: "synthetic refused"}
	for _, err := range []error{
		&txAbortedError{statementErr: cause, err: pgx.ErrTxCommitRollback},
		&txAbortedError{statementErr: cause, err: refused},
	} {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr != cause {
			t.Errorf("%v: first database error %v, want the cause", err, pgErr)
		}
	}
	if !errors.Is(&txAbortedError{statementErr: cause, err: pgx.ErrTxCommitRollback}, pgx.ErrTxCommitRollback) {
		t.Error("an aborted commit no longer reads as pgx.ErrTxCommitRollback")
	}
	var pgErr *pgconn.PgError
	if errors.As(&txAbortedError{err: pgx.ErrTxCommitRollback}, &pgErr) {
		t.Error("an aborted commit without a recorded cause reads as a database error")
	}
}

// At most one line per interval, with the count of the reports it held back,
// naming the callback and the cause by schema identifiers only.
func TestAbortedTransactionLogIsBounded(t *testing.T) {
	log := &abortedTransactionLog{}
	abortedErr := &txAbortedError{
		statementErr: &pgconn.PgError{Code: "23505", TableName: "synthetic_table", ConstraintName: "synthetic_key", Message: "synthetic message", Detail: "Key (secret)=(synthetic-user-data) already exists."},
		err:          pgx.ErrTxCommitRollback,
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	line, ok := log.Line(start, "synthetic.callback", abortedErr)
	if !ok {
		t.Fatal("first report was held back")
	}
	for _, want := range []string{"synthetic.callback", "sqlstate 23505", "synthetic_table", "synthetic_key", "commit rolled back", "0 more"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
	for _, unwanted := range []string{"synthetic-user-data", "synthetic message"} {
		if strings.Contains(line, unwanted) {
			t.Errorf("line %q carries the postgres message or detail %q", line, unwanted)
		}
	}
	for i := 1; i <= 3; i++ {
		if _, ok := log.Line(start.Add(time.Duration(i)*time.Second), "synthetic.callback", abortedErr); ok {
			t.Fatalf("report %d inside the interval was logged", i)
		}
	}
	line, ok = log.Line(start.Add(abortedTransactionReportInterval), "synthetic.other", &txAbortedError{err: &pgconn.PgError{Code: "25P02"}})
	if !ok {
		t.Fatal("report after the interval was held back")
	}
	for _, want := range []string{"synthetic.other", "no statement error was recorded", "later statement was refused", "3 more"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
}

// Runs the callback in txWithPool on the pool until it returns or panics; the
// callback ends the run (returning normally) on attempt endAttempt.
func runCountedTx(pool *safePgPool, endAttempt int, attempt func(tx PgTx, attempt int)) (recovered any, attempts int) {
	recovered = captureDbErrorPanic(func() {
		txWithPool(context.Background(), pool, func(tx PgTx) {
			attempts += 1
			if attempts == endAttempt {
				return
			}
			attempt(tx, attempts)
		})
	})
	return
}

// The first database error of the panic.
func recoveredPgError(recovered any) *pgconn.PgError {
	err, ok := recovered.(error)
	if !ok {
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}
	return pgErr
}

// A unique violation that the rerun repeats is permanent: it surfaces with its
// own error after one rerun instead of being rerun for the whole budget.
func TestTxPermanentUniqueViolationEndsAfterOneRerun(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		fixture.queryError = func(connectionIndex int, query string) *pgproto3.ErrorResponse {
			if query == syntheticEffectStatement {
				return syntheticPgError("23505", "Key (id)=(1) already exists.")
			}
			return nil
		}
	})
	repeatedBefore := counterValue(t, rerunDecisionCounter.WithLabelValues("unique_violation", rerunDecisionEndedRepeated))
	recovered, attempts := runCountedTx(pool, 3, func(tx PgTx, attempt int) {
		RaisePgResult(tx.Exec(context.Background(), syntheticEffectStatement))
	})
	pgErr := recoveredPgError(recovered)
	if pgErr == nil || pgErr.Code != "23505" || pgErr.ConstraintName != "synthetic_effect_key" || attempts != 2 {
		t.Fatalf("permanent unique violation=%v attempts=%d, want its 23505 after 2 attempts", recovered, attempts)
	}
	if repeated := counterValue(t, rerunDecisionCounter.WithLabelValues("unique_violation", rerunDecisionEndedRepeated)) - repeatedBefore; repeated != 1 {
		t.Fatalf("ended_repeated decisions=%v, want 1", repeated)
	}
}

// A conflict on a new key each attempt (a regenerated code, the next free
// slot) is not a repeat, so it keeps being rerun until an attempt succeeds.
func TestTxRegeneratedUniqueViolationKeepsRerunning(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		fixture.queryError = func(connectionIndex int, query string) *pgproto3.ErrorResponse {
			switch query {
			case "INSERT INTO synthetic_effect VALUES (1)":
				return syntheticPgError("23505", "Key (id)=(1) already exists.")
			case "INSERT INTO synthetic_effect VALUES (2)":
				return syntheticPgError("23505", "Key (id)=(2) already exists.")
			}
			return nil
		}
	})
	recovered, attempts := runCountedTx(pool, 4, func(tx PgTx, attempt int) {
		RaisePgResult(tx.Exec(context.Background(), fmt.Sprintf("INSERT INTO synthetic_effect VALUES (%d)", attempt)))
	})
	if recovered != nil || attempts != 3 {
		t.Fatalf("regenerated conflicts=%v attempts=%d, want success on attempt 3", recovered, attempts)
	}
}

// Not null, check and the other integrity violations, data exceptions, and
// rollbacks other than serialization failures and deadlocks cannot succeed on
// a rerun: each surfaces with its own error after its first attempt.
func TestTxNeverRerunsPermanentFailures(t *testing.T) {
	for _, code := range []string{"23502", "23514", "23P01", "23001", "23000", "22001", "22P02", "22023", "40000", "40002", "40003", "42P01"} {
		_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
			fixture.queryError = func(connectionIndex int, query string) *pgproto3.ErrorResponse {
				if query == syntheticEffectStatement {
					return syntheticPgError(code, "")
				}
				return nil
			}
		})
		recovered, attempts := runCountedTx(pool, 2, func(tx PgTx, attempt int) {
			RaisePgResult(tx.Exec(context.Background(), syntheticEffectStatement))
		})
		if pgErr := recoveredPgError(recovered); pgErr == nil || pgErr.Code != code || attempts != 1 {
			t.Errorf("%s failure=%v attempts=%d, want its own error after 1 attempt", code, recovered, attempts)
		}
	}
}

// Serialization failures and deadlocks are still rerun, by statement and at
// commit.
func TestTxRerunsSerializationFailuresAndDeadlocks(t *testing.T) {
	for _, c := range []struct {
		code  string
		query string
	}{
		{code: "40001", query: syntheticEffectStatement},
		{code: "40P01", query: syntheticEffectStatement},
		{code: "40001", query: "commit"},
		{code: "40P01", query: "commit"},
	} {
		failures := 0
		_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
			fixture.queryError = func(connectionIndex int, query string) *pgproto3.ErrorResponse {
				if query == c.query && failures < 2 {
					failures += 1
					return syntheticPgError(c.code, "")
				}
				return nil
			}
		})
		recovered, attempts := runCountedTx(pool, 4, func(tx PgTx, attempt int) {
			RaisePgResult(tx.Exec(context.Background(), syntheticEffectStatement))
		})
		if recovered != nil || attempts != 3 {
			t.Errorf("%s at %q=%v attempts=%d, want success on attempt 3", c.code, c.query, recovered, attempts)
		}
	}
}

// A callback that drops a failed statement's error and returns normally gets a
// commit that postgres turns into a rollback. That commit is not rerun for the
// budget: it fails at once, carrying the dropped statement error, unless that
// error is itself transient.
func TestTxAbortedCommitFailsFastWithRecordedCause(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		fixture.queryError = func(connectionIndex int, query string) *pgproto3.ErrorResponse {
			if query == syntheticEffectStatement {
				return syntheticPgError("23502", "")
			}
			return nil
		}
	})
	abortedBefore := counterValue(t, abortedTransactionCounter.WithLabelValues("other"))
	recovered, attempts := runCountedTx(pool, 2, func(tx PgTx, attempt int) {
		// the dropped error is the defect under test
		_, _ = tx.Exec(context.Background(), syntheticEffectStatement)
	})
	err, _ := recovered.(error)
	pgErr := recoveredPgError(recovered)
	if pgErr == nil || pgErr.Code != "23502" || !errors.Is(err, pgx.ErrTxCommitRollback) || attempts != 1 {
		t.Fatalf("aborted commit=%v attempts=%d, want the commit rollback carrying the 23502 after 1 attempt", recovered, attempts)
	}
	if aborted := counterValue(t, abortedTransactionCounter.WithLabelValues("other")) - abortedBefore; aborted != 1 {
		t.Fatalf("aborted transactions counted=%v, want 1", aborted)
	}
}

// A commit rollback with no recorded statement error has no evidence that a
// rerun can succeed, so it fails at once.
func TestTxAbortedCommitWithoutRecordedCauseFailsFast(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		fixture.commitRollbacks.Store(1)
	})
	recovered, attempts := runCountedTx(pool, 2, func(tx PgTx, attempt int) {
		RaisePgResult(tx.Exec(context.Background(), syntheticEffectStatement))
	})
	err, _ := recovered.(error)
	var abortedErr *txAbortedError
	if !errors.As(err, &abortedErr) || abortedErr.statementErr != nil || !errors.Is(err, pgx.ErrTxCommitRollback) || attempts != 1 {
		t.Fatalf("unexplained commit rollback=%v attempts=%d, want an aborted commit without a cause after 1 attempt", recovered, attempts)
	}
}

// A dropped error that is transient keeps the rerun that resolves it: the
// rerun's statement succeeds and its commit holds.
func TestTxAbortedCommitRerunsTransientCause(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		failures := 0
		_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
			fixture.queryError = func(connectionIndex int, query string) *pgproto3.ErrorResponse {
				if query == syntheticEffectStatement && failures == 0 {
					failures += 1
					return syntheticPgError(code, "")
				}
				return nil
			}
		})
		recovered, attempts := runCountedTx(pool, 3, func(tx PgTx, attempt int) {
			_, _ = tx.Exec(context.Background(), syntheticEffectStatement)
		})
		if recovered != nil || attempts != 2 {
			t.Errorf("dropped %s=%v attempts=%d, want success on attempt 2", code, recovered, attempts)
		}
	}
}

// A statement refused in a transaction an earlier dropped error aborted
// (25P02) surfaces that earlier error as its cause.
func TestTxRefusedStatementCarriesAbortingCause(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		fixture.queryError = func(connectionIndex int, query string) *pgproto3.ErrorResponse {
			if query == syntheticEffectStatement {
				return syntheticPgError("23514", "")
			}
			return nil
		}
	})
	recovered, attempts := runCountedTx(pool, 2, func(tx PgTx, attempt int) {
		_, _ = tx.Exec(context.Background(), syntheticEffectStatement)
		RaisePgResult(tx.Exec(context.Background(), "SELECT 1"))
	})
	err, _ := recovered.(error)
	var refused *pgconn.PgError
	var abortedErr *txAbortedError
	if pgErr := recoveredPgError(recovered); pgErr == nil || pgErr.Code != "23514" || !errors.As(err, &abortedErr) || attempts != 1 {
		t.Fatalf("refused statement=%v attempts=%d, want the 23514 cause after 1 attempt", recovered, attempts)
	}
	if !errors.As(abortedErr.err, &refused) || refused.Code != "25P02" {
		t.Fatalf("refused statement=%v, want the 25P02 refusal kept", abortedErr.err)
	}
}

// The recorder of a reused connection starts each transaction empty, so an
// error from an earlier use is never taken as a later commit's cause.
func TestTxStatementErrorRecorderStartsEachAttemptEmpty(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		fixture.queryError = func(connectionIndex int, query string) *pgproto3.ErrorResponse {
			if query == syntheticEffectStatement {
				return syntheticPgError("40001", "")
			}
			return nil
		}
	})
	// an earlier use of the one connection records a transient error
	captureDbErrorPanic(func() {
		dbWithPool(context.Background(), pool, func(conn PgConn) {
			RaisePgResult(conn.Exec(context.Background(), syntheticEffectStatement))
		}, OptNoRetry())
	})
	fixture.commitRollbacks.Store(1)
	recovered, attempts := runCountedTx(pool, 2, func(tx PgTx, attempt int) {
		RaisePgResult(tx.Exec(context.Background(), "SELECT 1"))
	})
	err, _ := recovered.(error)
	var abortedErr *txAbortedError
	if !errors.As(err, &abortedErr) || abortedErr.statementErr != nil || attempts != 1 {
		t.Fatalf("commit rollback after an earlier error=%v attempts=%d, want no recorded cause after 1 attempt", recovered, attempts)
	}
}

// Db callbacks follow the same taxonomy: a repeated violation ends after one
// rerun.
func TestDbPermanentUniqueViolationEndsAfterOneRerun(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		fixture.queryError = func(connectionIndex int, query string) *pgproto3.ErrorResponse {
			if query == syntheticEffectStatement {
				return syntheticPgError("23505", "Key (id)=(1) already exists.")
			}
			return nil
		}
	})
	attempts := 0
	recovered := captureDbErrorPanic(func() {
		dbWithPool(context.Background(), pool, func(conn PgConn) {
			attempts += 1
			if attempts == 3 {
				return
			}
			RaisePgResult(conn.Exec(context.Background(), syntheticEffectStatement))
		})
	})
	if pgErr := recoveredPgError(recovered); pgErr == nil || pgErr.Code != "23505" || attempts != 2 {
		t.Fatalf("permanent unique violation=%v attempts=%d, want its 23505 after 2 attempts", recovered, attempts)
	}
}

// Against postgres, each case runs one callback through Tx and checks the
// error it surfaces and its attempt count. A callback ends the run on the
// attempt after the expected count, so an unwanted rerun shows as a success
// with an extra attempt. The unique race and the dropped serialization failure
// are made deterministic by a concurrent transaction the first attempt runs and
// commits on another connection.
func TestTxRetryTaxonomyAgainstPostgres(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()

		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(
				ctx,
				`
				CREATE TABLE retry_parent (parent_id integer PRIMARY KEY);
				CREATE TABLE retry_item (
					item_id integer PRIMARY KEY,
					owner text NOT NULL,
					amount integer NOT NULL DEFAULT 0 CHECK (0 <= amount),
					code varchar(4),
					parent_id integer REFERENCES retry_parent (parent_id)
				);
				INSERT INTO retry_parent (parent_id) VALUES (1);
				INSERT INTO retry_item (item_id, owner) VALUES (1, 'existing');
				`,
			))
		}, OptReadWrite())

		// another transaction, committed before the attempt goes on
		concurrently := func(sql string) {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, sql))
			})
		}

		for _, c := range []struct {
			name           string
			attempt        func(tx PgTx, attempt int)
			attempts       int
			code           string
			constraintName string
			aborted        bool
		}{
			{
				name: "permanent unique violation",
				attempt: func(tx PgTx, attempt int) {
					RaisePgResult(tx.Exec(ctx, `INSERT INTO retry_item (item_id, owner) VALUES (1, 'duplicate')`))
				},
				attempts:       2,
				code:           "23505",
				constraintName: "retry_item_pkey",
			},
			{
				name: "not null violation",
				attempt: func(tx PgTx, attempt int) {
					RaisePgResult(tx.Exec(ctx, `INSERT INTO retry_item (item_id, owner) VALUES (2, NULL)`))
				},
				attempts: 1,
				code:     "23502",
			},
			{
				name: "check violation",
				attempt: func(tx PgTx, attempt int) {
					RaisePgResult(tx.Exec(ctx, `INSERT INTO retry_item (item_id, owner, amount) VALUES (3, 'negative', -1)`))
				},
				attempts:       1,
				code:           "23514",
				constraintName: "retry_item_amount_check",
			},
			{
				name: "data exception",
				attempt: func(tx PgTx, attempt int) {
					RaisePgResult(tx.Exec(ctx, `INSERT INTO retry_item (item_id, owner, code) VALUES (4, 'long', 'too long')`))
				},
				attempts: 1,
				code:     "22001",
			},
			{
				name: "permanent foreign key violation",
				attempt: func(tx PgTx, attempt int) {
					RaisePgResult(tx.Exec(ctx, `INSERT INTO retry_item (item_id, owner, parent_id) VALUES (5, 'orphan', 99)`))
				},
				attempts:       2,
				code:           "23503",
				constraintName: "retry_item_parent_id_fkey",
			},
			{
				name: "unique race",
				attempt: func(tx PgTx, attempt int) {
					var owner string
					err := tx.QueryRow(ctx, `SELECT owner FROM retry_item WHERE item_id = 6`).Scan(&owner)
					if err == nil {
						return
					}
					if !errors.Is(err, pgx.ErrNoRows) {
						Raise(err)
					}
					if attempt == 1 {
						concurrently(`INSERT INTO retry_item (item_id, owner) VALUES (6, 'concurrent')`)
					}
					RaisePgResult(tx.Exec(ctx, `INSERT INTO retry_item (item_id, owner) VALUES (6, 'mine')`))
				},
				attempts: 2,
			},
			{
				name: "dropped not null violation",
				attempt: func(tx PgTx, attempt int) {
					// the dropped error is the defect under test
					_, _ = tx.Exec(ctx, `INSERT INTO retry_item (item_id, owner) VALUES (7, NULL)`)
				},
				attempts: 1,
				code:     "23502",
				aborted:  true,
			},
			{
				name: "dropped serialization failure",
				attempt: func(tx PgTx, attempt int) {
					var amount int
					Raise(tx.QueryRow(ctx, `SELECT amount FROM retry_item WHERE item_id = 1`).Scan(&amount))
					if attempt == 1 {
						concurrently(`UPDATE retry_item SET amount = amount + 1 WHERE item_id = 1`)
					}
					_, _ = tx.Exec(ctx, `UPDATE retry_item SET amount = amount + 10 WHERE item_id = 1`)
				},
				attempts: 2,
			},
			{
				name: "statement after a dropped violation",
				attempt: func(tx PgTx, attempt int) {
					_, _ = tx.Exec(ctx, `INSERT INTO retry_item (item_id, owner) VALUES (8, NULL)`)
					RaisePgResult(tx.Exec(ctx, `SELECT 1`))
				},
				attempts: 1,
				code:     "23502",
				aborted:  true,
			},
		} {
			attempts := 0
			recovered := captureDbErrorPanic(func() {
				Tx(ctx, func(tx PgTx) {
					attempts += 1
					if attempts == c.attempts+1 {
						return
					}
					c.attempt(tx, attempts)
				})
			})
			err, _ := recovered.(error)
			pgErr := recoveredPgError(recovered)
			var abortedErr *txAbortedError
			switch {
			case attempts != c.attempts:
				t.Errorf("%s: attempts=%d (%v), want %d", c.name, attempts, recovered, c.attempts)
			case c.code == "" && recovered != nil:
				t.Errorf("%s: %v, want success", c.name, recovered)
			case c.code != "" && (pgErr == nil || pgErr.Code != c.code):
				t.Errorf("%s: %v, want its %s", c.name, recovered, c.code)
			case c.constraintName != "" && pgErr.ConstraintName != c.constraintName:
				t.Errorf("%s: constraint %q, want %q", c.name, pgErr.ConstraintName, c.constraintName)
			case c.aborted != errors.As(err, &abortedErr):
				t.Errorf("%s: %v aborted=%t, want %t", c.name, recovered, !c.aborted, c.aborted)
			}
		}

		var raceOwner string
		var amount int
		Db(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT owner FROM retry_item WHERE item_id = 6`).Scan(&raceOwner))
			Raise(conn.QueryRow(ctx, `SELECT amount FROM retry_item WHERE item_id = 1`).Scan(&amount))
		})
		if raceOwner != "concurrent" {
			t.Errorf("unique race owner=%q, want the concurrent writer's row found by the rerun", raceOwner)
		}
		if amount != 11 {
			t.Errorf("dropped serialization failure amount=%d, want the concurrent update and the rerun's", amount)
		}
	})
}
