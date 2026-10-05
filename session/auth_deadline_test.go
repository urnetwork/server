package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

func TestAuthDeadlineClassifiesOnlyKnownDependencyErrors(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, server.DbContextDoneError, &pgconn.ConnectError{}} {
		if !authDependencyUnavailable(err) {
			t.Fatal("known dependency cancellation or establishment error was not classified")
		}
	}
	if !authDependencyUnavailable(fmt.Errorf("wrapped: %w", server.DbContextDoneError)) {
		t.Fatal("wrapped database cancellation lost classification")
	}
	for _, err := range []error{nil, errors.New("invalid signature"), errors.New("unexpected program error")} {
		if authDependencyUnavailable(err) {
			t.Fatal("ordinary credential or program error became a dependency error")
		}
	}
}
