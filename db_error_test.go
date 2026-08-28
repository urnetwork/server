package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Wrapped PostgreSQL transaction failures retain their SQLSTATE classification.
// Database adapters commonly add context before errors reach the retry loop.
func TestDbClassifiesWrappedTransientError(t *testing.T) {
	err := fmt.Errorf("transaction callback: %w", &pgconn.PgError{Code: "40001"})
	if !isTransientError(err) {
		t.Fatal("wrapped serialization failure was not classified as transient")
	}
}

// A pgproto3 write failure unwraps to net.OpError. Recognizing the wrapped
// socket error makes dbWithPool discard that connection and use its bounded
// connection retry path instead of escalating an unexpected recovery.
func TestDbClassifiesWrappedConnectionWriteTimeout(t *testing.T) {
	err := fmt.Errorf(
		"write failed: %w",
		&net.OpError{
			Op:  "write",
			Net: "tcp",
			Err: os.ErrDeadlineExceeded,
		},
	)
	if !isConnectionError(err) {
		t.Fatal("wrapped TCP write timeout was not classified as a connection error")
	}
}

// SQL errors outside the connection-exception class must stay on the normal
// failure path even when an adapter wraps them with additional context.
func TestDbRejectsWrappedNonConnectionError(t *testing.T) {
	err := fmt.Errorf("query callback: %w", &pgconn.PgError{Code: "42P01"})
	if isConnectionError(err) {
		t.Fatal("undefined-table error was classified as a connection failure")
	}
}

// Unrelated wrappers must not become retryable merely because they implement
// standard error unwrapping.
func TestDbRejectsWrappedOrdinaryError(t *testing.T) {
	err := fmt.Errorf("query callback: %w", errors.New("invalid query result"))
	if isConnectionError(err) || isTransientError(err) {
		t.Fatal("ordinary wrapped error was classified as retryable")
	}
}
