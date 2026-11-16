package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

// Pure: the failures a request may retry as it is are dependency failures,
// whatever wraps them; a refusal about the request itself, and an error that is
// not the database's or Redis's, are not.
func TestIsDependencyUnavailableSeparatesFailuresFromRefusals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "context done", err: dbContextDoneCause(ctx, errors.New("query")), want: true},
		{name: "deadline", err: fmt.Errorf("read: %w", context.DeadlineExceeded), want: true},
		{name: "socket", err: fmt.Errorf("failed to write: %w", refused), want: true},
		{name: "closed connection", err: fmt.Errorf("prepare: %w", pgconn.ErrConnClosed), want: true},
		{name: "connection exception", err: &pgconn.PgError{Code: pgerrcode.ConnectionFailure}, want: true},
		{name: "too many connections", err: &pgconn.PgError{Code: pgerrcode.TooManyConnections}, want: true},
		{name: "admin shutdown", err: fmt.Errorf("[q]%w", &pgconn.PgError{Code: pgerrcode.AdminShutdown}), want: true},
		{name: "statement timeout", err: &pgconn.PgError{Code: pgerrcode.QueryCanceled}, want: true},
		{name: "serialization", err: &pgconn.PgError{Code: pgerrcode.SerializationFailure}, want: true},
		{name: "lock timeout", err: &pgconn.PgError{Code: pgerrcode.LockNotAvailable}, want: true},
		{name: "redis cluster down", err: errors.New("CLUSTERDOWN The cluster is down"), want: true},
		{name: "redis pool", err: errors.New("redis: connection pool timeout"), want: true},
		{name: "unique violation", err: &pgconn.PgError{Code: pgerrcode.UniqueViolation}, want: false},
		{name: "syntax", err: &pgconn.PgError{Code: pgerrcode.SyntaxError}, want: false},
		{name: "declared refusal", err: fmt.Errorf("%d Not authorized.", 401), want: false},
		{name: "internal", err: errors.New("signing key is not loaded"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, c := range cases {
		if got := IsDependencyUnavailable(c.err); got != c.want {
			t.Errorf("%s (%v): %t, want %t", c.name, c.err, got, c.want)
		}
	}
}
