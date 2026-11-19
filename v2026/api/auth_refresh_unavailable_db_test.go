package api

// End to end through the router the API serves, against the database: a
// refresh whose database or session store fails answers 503, which the apps
// retry, never 500 and never a refusal in the result, which they sign out on
// (CPUOPT6 follow-up, owner 2026-11-15). The mint hook stands in for a read
// between the router's state check and the mint: its raise is what server.Db
// raises when a query fails.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/session"
)

// requireRefreshUnavailable requires the dependency answer: 503 with
// authentication's fixed unavailable body, nothing of the cause.
func requireRefreshUnavailable(t testing.TB, w *httptest.ResponseRecorder, path string) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "Authentication temporarily unavailable.") {
		t.Fatalf("%s with a failed dependency = %d: %s, want 503", path, w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "53300") || strings.Contains(w.Body.String(), "SQLSTATE") {
		t.Fatalf("%s answered the database's cause: %s", path, w.Body.String())
	}
}

// A database failure in a client token's refresh, after the router's state
// check, answers 503; it was a 500 from the router's recovery.
func TestClientRefreshAnswersADatabaseFailureWith503(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)
		_, _, presented := self.agedClientToken(t)

		var w *httptest.ResponseRecorder
		withRefreshMintHook(func(*session.ClientSession) {
			panic(&pgconn.PgError{Severity: "FATAL", Code: pgerrcode.TooManyConnections, Message: "sorry, too many clients already"})
		}, func() {
			w = self.serve(http.MethodGet, "/auth/refresh", "Bearer "+presented.Testing_Sign(), nil)
		})
		requireRefreshUnavailable(t, w, "/auth/refresh")

		// the database is back: the same token refreshes
		var result controller.RefreshTokenResult
		self.call(t, http.MethodGet, "/auth/refresh", "Bearer "+presented.Testing_Sign(), nil, http.StatusOK, &result)
		if result.Error != nil || result.ByJwt == "" {
			t.Fatalf("client refresh after the failure = %+v", result)
		}
	})
}

// The renewal itself failing for a dependency answers 503: here the request's
// session context ends before the renewal's transaction while the caller is
// still connected, as a child deadline does. The renewal returned that error
// undeclared, a 500.
func TestClientRefreshAnswersARenewalDependencyFailureWith503(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)
		_, _, presented := self.agedClientToken(t)

		var w *httptest.ResponseRecorder
		withRefreshMintHook(func(clientSession *session.ClientSession) {
			clientSession.Cancel()
		}, func() {
			w = self.serve(http.MethodGet, "/auth/refresh", "Bearer "+presented.Testing_Sign(), nil)
		})
		requireRefreshUnavailable(t, w, "/auth/refresh")
	})
}

// A network token's refresh answers a database failure the same way.
func TestNetworkRefreshAnswersADatabaseFailureWith503(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)
		presented := self.agedNetworkToken()

		var w *httptest.ResponseRecorder
		withRefreshMintHook(func(*session.ClientSession) {
			panic(&pgconn.PgError{Severity: "FATAL", Code: pgerrcode.AdminShutdown, Message: "terminating connection due to administrator command"})
		}, func() {
			w = self.serve(http.MethodPost, "/auth/network-refresh", "Bearer "+presented.Testing_Sign(), nil)
		})
		requireRefreshUnavailable(t, w, "/auth/network-refresh")
	})
}

// A failure that is no dependency's keeps the router's 500: the refresh does
// not disguise a defect as an outage.
func TestClientRefreshKeepsA500ForAFailureThatIsNoDependency(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)
		_, _, presented := self.agedClientToken(t)

		var w *httptest.ResponseRecorder
		withRefreshMintHook(func(*session.ClientSession) {
			panic(errors.New("deterministic defect"))
		}, func() {
			w = self.serve(http.MethodGet, "/auth/refresh", "Bearer "+presented.Testing_Sign(), nil)
		})
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("/auth/refresh with a defect = %d: %s, want 500", w.Code, w.Body.String())
		}
	})
}
