// The task claimer retains its existing direct session across executions. Its
// short raw transaction probes one candidate queue key before locking that row;
// a refusal skips that candidate without starting or retrying business work.
package server

import (
	"context"
	"errors"
)

// The execution guard needs a real direct backend independently of the short
// business owners. Check the explicit resource and this transaction's actual
// backend before any session advisory lock can be taken on its behalf.
func ValidatePgTaskClaimTransaction(ctx context.Context, conn PgConn, rawTx PgTx) (returnErr error) {
	HandleError(func() {
		if _, wrapped := rawTx.(*postCommitPgTx); wrapped || rawTx.Conn() != conn.Conn() {
			Raise(errors.New("task claim transaction does not own its direct checkout"))
		}
		Raise(requirePgOwnershipResource().validate(conn))
		var backendPid uint32
		var isolation string
		Raise(rawTx.QueryRow(ctx, `SELECT pg_backend_pid(),current_setting('transaction_isolation')`).Scan(&backendPid, &isolation))
		if backendPid != conn.Conn().PgConn().PID() || isolation != "read committed" {
			Raise(errors.New("task claim requires a direct read-committed backend"))
		}
	}, func(err error) { returnErr = err })
	return
}

// This narrow bridge does not replace complete-set financial admission. The
// raw read-committed claim transaction owns all acquired xact keys until its
// actual commit or rollback; this helper never waits or invents a release event.
func TryPgTaskClaimOwnership(ctx context.Context, rawTx PgTx, key PgOwnershipKey) (bool, error) {
	if _, wrapped := rawTx.(*postCommitPgTx); wrapped {
		return false, errors.New("task claim ownership requires its raw transaction")
	}
	var isolation string
	var backendPid uint32
	var acquired bool
	err := rawTx.QueryRow(ctx, `SELECT current_setting('transaction_isolation'),pg_backend_pid(),
        CASE WHEN current_setting('transaction_isolation')='read committed' AND pg_backend_pid()=$3
        THEN pg_try_advisory_xact_lock($1::integer,$2::integer) ELSE false END`,
		key.first, key.second, rawTx.Conn().PgConn().PID()).Scan(&isolation, &backendPid, &acquired)
	if err != nil {
		return false, err
	}
	if isolation != "read committed" || backendPid != rawTx.Conn().PgConn().PID() {
		return false, errors.New("task claim ownership requires its direct read-committed backend")
	}
	return acquired, nil
}

// The task session guard and its short queue admission share one backend and
// one wire exchange. A refused session must not acquire the queue key. Keep
// the session attempt materialized so one release retires exactly one lock,
// and preserve the raw transaction's queue ownership until its actual end.
func TryPgTaskClaimSessionAndQueueOwnership(ctx context.Context, rawTx PgTx, sessionKey int64, key PgOwnershipKey) (sessionAcquired, queueAcquired bool, err error) {
	if _, wrapped := rawTx.(*postCommitPgTx); wrapped {
		return false, false, errors.New("task claim ownership requires its raw transaction")
	}
	var isolation string
	var backendPid uint32
	err = rawTx.QueryRow(ctx, `WITH task_claim_session AS MATERIALIZED (
        SELECT current_setting('transaction_isolation') AS isolation,pg_backend_pid() AS backend_pid,
            CASE WHEN current_setting('transaction_isolation')='read committed' AND pg_backend_pid()=$4
            THEN pg_try_advisory_lock($1::bigint) ELSE false END AS session_acquired
    )
    SELECT isolation,backend_pid,session_acquired,
        CASE WHEN session_acquired THEN pg_try_advisory_xact_lock($2::integer,$3::integer) ELSE false END
    FROM task_claim_session`, sessionKey, key.first, key.second, rawTx.Conn().PgConn().PID()).Scan(
		&isolation, &backendPid, &sessionAcquired, &queueAcquired)
	if err != nil {
		return false, false, err
	}
	if isolation != "read committed" || backendPid != rawTx.Conn().PgConn().PID() {
		return false, false, errors.New("task claim ownership requires its direct read-committed backend")
	}
	return sessionAcquired, queueAcquired, nil
}
