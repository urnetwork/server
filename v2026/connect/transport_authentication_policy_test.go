package connect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/router"
)

func TestConnectAuthenticationStatusKnownDependencies(t *testing.T) {
	for _, cause := range []struct {
		name string
		err  error
	}{
		{"database_done", server.DbContextDoneError},
		{"canceled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
		{"connection", &pgconn.ConnectError{}},
		{"wrapped_database_done", fmt.Errorf("query: %w", server.DbContextDoneError)},
		{"joined_database_deadline", errors.Join(server.DbContextDoneError, context.DeadlineExceeded)},
	} {
		t.Run(cause.name, func(t *testing.T) {
			for _, panics := range []bool{false, true} {
				finished := false
				status := connectH1AuthenticationStatus(t.Context(), func() (int, error) {
					defer func() { finished = true }()
					if panics {
						panic(cause.err)
					}
					return http.StatusUnauthorized, cause.err
				})
				if status != http.StatusServiceUnavailable || !finished {
					t.Fatalf("dependency panic=%t status=%d finished=%t", panics, status, finished)
				}
			}
		})
	}
}

func TestConnectAuthenticationStatusPreservesCredentialRejection(t *testing.T) {
	for _, want := range []int{0, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden} {
		status := connectH1AuthenticationStatus(t.Context(), func() (int, error) {
			if want == 0 {
				return 0, nil
			}
			return want, errors.New("synthetic invalid credential")
		})
		if status != want {
			t.Fatalf("credential status=%d want=%d", status, want)
		}
	}
}

func TestConnectAuthenticationStatusCancellationCannotAdmit(t *testing.T) {
	for _, before := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if before {
			cancel()
		}
		called := false
		status := connectH1AuthenticationStatus(ctx, func() (int, error) {
			called = true
			cancel()
			return 0, nil
		})
		cancel()
		if status != http.StatusServiceUnavailable || called == before {
			t.Fatalf("canceled admission before=%t called=%t status=%d", before, called, status)
		}
	}
}

func TestConnectAuthenticationStatusReturnedErrorCannotAdmit(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled_%t", canceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := errors.New("synthetic unchecked authentication failure")
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				connectH1AuthenticationStatus(ctx, func() (int, error) {
					if canceled {
						cancel()
					}
					return 0, want
				})
			}()
			if recovered != want {
				t.Fatalf("unchecked error lost internal-error ownership: got=%v want=%v", recovered, want)
			}
		})
	}
}

// The real router still owns unexpected panics. Expiring the child deadline
// while a bug is raised must not relabel that bug as a retryable dependency.
func TestConnectAuthenticationStatusUnexpectedPanicRemainsInternalError(t *testing.T) {
	// Join the real router's stats worker before this test returns.
	synctest.Test(t, func(t *testing.T) {
		for _, canceled := range []bool{false, true} {
			for _, value := range []any{errors.New("synthetic auth program bug"), "synthetic auth panic"} {
				ctx, cancel := context.WithCancel(t.Context())
				admitted := false
				routes := router.NewRouter(t.Context(), []*router.Route{router.NewRoute(http.MethodGet, "/", func(w http.ResponseWriter, r *http.Request) {
					status := connectH1AuthenticationStatus(ctx, func() (int, error) {
						if canceled {
							cancel()
						}
						panic(value)
					})
					if status != 0 {
						http.Error(w, "rejected", status)
						return
					}
					admitted = true
				})})
				response := httptest.NewRecorder()
				routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://auth.example/", nil))
				cancel()
				if response.Code != http.StatusInternalServerError || admitted {
					t.Fatalf("unexpected panic became dependency/admission: canceled=%t status=%d admitted=%t", canceled, response.Code, admitted)
				}
			}
		}
	})
}
