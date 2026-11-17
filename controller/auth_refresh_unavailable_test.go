package controller

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server/session"
)

// Pure: a renewal that failed for a dependency is answered as an unavailable
// store (session.ErrAuthUnavailable, a 503 the apps retry); a declared answer
// and any other failure are returned as they are. Before 2026-11-15 every
// undeclared renewal error was a 500.
func TestRefreshRenewalErrorAnswersDependencyFailuresAsUnavailable(t *testing.T) {
	declared := fmt.Errorf("%d Not authorized.", 401)
	internal := errors.New("signing key is not loaded")
	refused := &pgconn.PgError{Code: pgerrcode.UniqueViolation}
	for _, failure := range []error{
		&pgconn.PgError{Code: pgerrcode.TooManyConnections},
		fmt.Errorf("renew: %w", pgconn.ErrConnClosed),
		session.ErrSessionStoreUnavailable,
		session.ErrAuthUnavailable,
	} {
		if got := refreshRenewalError("refresh", failure); got != session.ErrAuthUnavailable {
			t.Errorf("renewal failure %v answered %v, want session.ErrAuthUnavailable", failure, got)
		}
	}
	for _, kept := range []error{declared, internal, refused} {
		if got := refreshRenewalError("refresh", kept); got != kept {
			t.Errorf("renewal error %v answered %v, want it unchanged", kept, got)
		}
	}
}

// Pure: a dependency raise inside a refresh returns session.ErrAuthUnavailable
// and no result; any other raise continues, as the router answered it before.
func TestRecoverRefreshDependencyUnavailableKeepsOtherRaises(t *testing.T) {
	refresh := func(raise any) (result *RefreshTokenResult, returnErr error) {
		defer recoverRefreshDependencyUnavailable("refresh", &result, &returnErr)
		result = &RefreshTokenResult{ByJwt: "not returned"}
		panic(raise)
	}
	result, err := refresh(&pgconn.PgError{Code: pgerrcode.AdminShutdown})
	if result != nil || err != session.ErrAuthUnavailable {
		t.Fatalf("dependency raise = %+v, %v", result, err)
	}
	for _, raise := range []any{errors.New("programming error"), "not an error"} {
		func() {
			defer func() {
				if recovered := recover(); recovered != raise {
					t.Errorf("raise %v continued as %v", raise, recovered)
				}
			}()
			refresh(raise)
			t.Errorf("raise %v was recovered", raise)
		}()
	}
}
