package server

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

// IsDependencyUnavailable reports whether err is a dependency failing a request
// rather than refusing it, so the same request may succeed when retried:
//   - the operation's context ended (DbContextDoneError, canceled, deadline) or
//     a database timeout fired;
//   - a database connection could not be made, or was lost (a connection
//     exception, a closed connection, a socket error);
//   - PostgreSQL or its pooler ran out of capacity (insufficient resources,
//     such as too many connections), an operator or a statement timeout ended
//     the statement (operator intervention, such as an admin shutdown or a
//     canceled query), a transaction lost a serialization or deadlock race, or
//     a lock wait reached its bound;
//   - a Redis connection failed as isRedisConnectionError counts it, or its
//     pool had no connection to give.
//
// A refusal the database reports about the request itself, such as a
// constraint violation, and every error that is not the database's or
// Redis's, is not.
func IsDependencyUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, DbContextDoneError) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		pgconn.Timeout(err) {
		return true
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	if isConnectionError(err) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgerrcode.IsInsufficientResources(pgErr.Code) ||
			pgerrcode.IsOperatorIntervention(pgErr.Code) ||
			pgerrcode.IsTransactionRollback(pgErr.Code) ||
			pgErr.Code == pgerrcode.LockNotAvailable
	}
	return isRedisConnectionError(err) || strings.Contains(err.Error(), "redis: connection pool timeout")
}
