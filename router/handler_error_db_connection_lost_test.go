package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/session"
)

// What a cut Postgres connection is answered with.
const dbConnectionLostTestBody = `{"error":"Temporarily unavailable. Retry shortly."}`

// The six connection-loss classes of QD-759, each as Postgres words it.
func dbConnectionLostTestErrors() []*pgconn.PgError {
	return []*pgconn.PgError{
		{Severity: "FATAL", Code: "08000", Message: "synthetic connection exception"},
		{Severity: "FATAL", Code: "08003", Message: "synthetic connection does not exist"},
		{Severity: "FATAL", Code: "08006", Message: "synthetic connection failure"},
		{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"},
		{Severity: "FATAL", Code: "57P02", Message: "terminating connection because of crash of another server process"},
		{Severity: "FATAL", Code: "57P03", Message: "the database system is starting up"},
	}
}

// Failures no connection-loss class covers: other Postgres classes, the
// connection classes QD-759 does not name, and transport failures that carry
// no SQLSTATE.
func dbConnectionKeptTestErrors() []error {
	return []error{
		errors.New("synthetic storage failure"),
		&pgconn.PgError{Code: pgerrcode.UniqueViolation, Message: "synthetic duplicate key"},
		&pgconn.PgError{Code: pgerrcode.SQLClientUnableToEstablishSQLConnection, Message: "synthetic 08001"},
		&pgconn.PgError{Code: pgerrcode.SQLServerRejectedEstablishmentOfSQLConnection, Message: "synthetic 08004"},
		&pgconn.PgError{Code: pgerrcode.ProtocolViolation, Message: "synthetic 08P01"},
		&pgconn.PgError{Code: pgerrcode.QueryCanceled, Message: "synthetic 57014"},
		&pgconn.PgError{Code: pgerrcode.TooManyConnections, Message: "synthetic 53300"},
		&pgconn.PgError{Code: pgerrcode.SerializationFailure, Message: "synthetic 40001"},
		fmt.Errorf("query: %w", io.ErrUnexpectedEOF),
		fmt.Errorf("query: %w", pgconn.ErrConnClosed),
	}
}

// Each connection-loss class a Postgres or PgBouncer restart raises answers a
// retryable 503 with Retry-After: 1 and a fixed body, however it was wrapped
// or joined, and none of the cause's text reaches the caller (QD-759).
func TestRaiseHttpErrorAnswersACutPostgresConnectionAsRetryable(t *testing.T) {
	classes := dbConnectionLostTestErrors()
	codes := []string{}
	for _, class := range classes {
		codes = append(codes, class.Code)
	}
	if got, want := strings.Join(codes, " "), strings.Join([]string{
		pgerrcode.ConnectionException,
		pgerrcode.ConnectionDoesNotExist,
		pgerrcode.ConnectionFailure,
		pgerrcode.AdminShutdown,
		pgerrcode.CrashShutdown,
		pgerrcode.CannotConnectNow,
	}, " "); got != want {
		t.Fatalf("the classes under test are %s, want %s", got, want)
	}
	for _, class := range classes {
		for _, err := range []error{
			class,
			fmt.Errorf("[impl]commit: %w", class),
			fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", class)),
			errors.Join(errors.New("synthetic cleanup failure"), class),
		} {
			if !IsDbConnectionLost(err) {
				t.Errorf("%s %v: not a connection loss", class.Code, err)
			}
			w := httptest.NewRecorder()
			if statusError := RaiseHttpError(err, w); !statusError {
				t.Errorf("%s %v: not answered as a declared status", class.Code, err)
			}
			if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
				t.Errorf("%s %v: answered %d with Retry-After %q, want 503 and 1", class.Code, err, w.Code, w.Header().Get("Retry-After"))
			}
			if contentType := w.Header().Get("Content-Type"); contentType != "application/json" {
				t.Errorf("%s %v: content type %q", class.Code, err, contentType)
			}
			if body := strings.TrimSpace(w.Body.String()); body != dbConnectionLostTestBody {
				t.Errorf("%s %v: body %q", class.Code, err, body)
			}
		}
	}
}

// Every other internal failure keeps the fixed 500 with no retry hint: other
// Postgres classes, the connection classes QD-759 does not name, and a
// transport failure with no SQLSTATE.
func TestRaiseHttpErrorKeepsTheFixedInternalErrorForOtherFailures(t *testing.T) {
	for _, err := range dbConnectionKeptTestErrors() {
		if IsDbConnectionLost(err) {
			t.Errorf("%v: read as a connection loss", err)
		}
		w := httptest.NewRecorder()
		if statusError := RaiseHttpError(err, w); statusError {
			t.Errorf("%v: answered as a declared status", err)
		}
		if w.Code != http.StatusInternalServerError || w.Header().Get("Retry-After") != "" {
			t.Errorf("%v: answered %d with Retry-After %q, want a plain 500", err, w.Code, w.Header().Get("Retry-After"))
		}
		if body := w.Body.String(); body != UnexpectedErrorMessage+"\n" {
			t.Errorf("%v: body %q", err, body)
		}
	}
	for _, r := range []any{nil, "57P01", 57, error(nil)} {
		if IsDbConnectionLost(r) {
			t.Errorf("%#v: read as a connection loss", r)
		}
	}
}

// The hint is the settings' and not a fixed number: 1 s by default, whole
// seconds rounded up, and none at all when it is non-positive, where the
// answer stays the retryable 503.
func TestDbConnectionLostRetryAfterFollowsTheSettings(t *testing.T) {
	defaults := DefaultRouterSettings()
	if defaults.DbConnectionLostRetryAfter != time.Second || defaults.LockTimeoutRetryAfter != time.Second || defaults.AuthUnavailableRetryAfter != time.Second {
		t.Fatalf("default retry hints are %s, %s and %s, want 1s each", defaults.DbConnectionLostRetryAfter, defaults.LockTimeoutRetryAfter, defaults.AuthUnavailableRetryAfter)
	}
	cut := fmt.Errorf("commit: %w", &pgconn.PgError{Code: pgerrcode.AdminShutdown, Message: "terminating connection due to administrator command"})
	lockTimeout := fmt.Errorf("update: %w", &pgconn.PgError{Code: pgerrcode.LockNotAvailable, Message: "canceling statement due to lock timeout"})
	defer SetRouterSettings(nil)
	for _, test := range []struct {
		retryAfter time.Duration
		header     string
	}{
		{retryAfter: time.Second, header: "1"},
		{retryAfter: time.Millisecond, header: "1"},
		{retryAfter: 2500 * time.Millisecond, header: "3"},
		{retryAfter: 7 * time.Second, header: "7"},
		{retryAfter: 0, header: ""},
		{retryAfter: -time.Second, header: ""},
	} {
		settings := DefaultRouterSettings()
		settings.DbConnectionLostRetryAfter = test.retryAfter
		SetRouterSettings(settings)
		w := httptest.NewRecorder()
		RaiseHttpError(cut, w)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != test.header {
			t.Errorf("%s: answered %d with Retry-After %q, want 503 and %q", test.retryAfter, w.Code, w.Header().Get("Retry-After"), test.header)
		}
		// a lock timeout's hint is its own field, which this one does not move
		w = httptest.NewRecorder()
		RaiseHttpError(lockTimeout, w)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
			t.Errorf("%s: a lock timeout answered %d with Retry-After %q, want 503 and 1", test.retryAfter, w.Code, w.Header().Get("Retry-After"))
		}
	}

	settings := DefaultRouterSettings()
	settings.LockTimeoutRetryAfter = 4 * time.Second
	SetRouterSettings(settings)
	w := httptest.NewRecorder()
	RaiseHttpError(lockTimeout, w)
	if w.Header().Get("Retry-After") != "4" {
		t.Errorf("a lock timeout's Retry-After is %q, want the settings' 4", w.Header().Get("Retry-After"))
	}
	w = httptest.NewRecorder()
	RaiseHttpError(cut, w)
	if w.Header().Get("Retry-After") != "1" {
		t.Errorf("a cut connection's Retry-After is %q, want its own 1", w.Header().Get("Retry-After"))
	}

	SetRouterSettings(nil)
	w = httptest.NewRecorder()
	RaiseHttpError(cut, w)
	if w.Header().Get("Retry-After") != "1" {
		t.Errorf("restored defaults answer Retry-After %q, want 1", w.Header().Get("Retry-After"))
	}
}

// A cut connection raised under a route, as server.Tx raises it, reaches the
// router's recover: each class answers the same retryable 503, counted as
// unavailable and not as a panic, and an ordinary raise keeps the fixed 500.
func TestRouterAnswersARaisedCutPostgresConnectionAsRetryable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	metrics := newHttpMetrics(prometheus.NewRegistry())

	classes := dbConnectionLostTestErrors()
	routes := []*Route{}
	for _, class := range classes {
		routes = append(routes, NewRoute(http.MethodPost, "/cut/"+class.Code, func(http.ResponseWriter, *http.Request) {
			panic(fmt.Errorf("commit: %w", class))
		}))
	}
	kept := dbConnectionKeptTestErrors()
	for i, err := range kept {
		routes = append(routes, NewRoute(http.MethodPost, fmt.Sprintf("/kept/%d", i), func(http.ResponseWriter, *http.Request) {
			panic(err)
		}))
	}
	router := NewRouter(ctx, routes)
	router.metrics = metrics

	for i, class := range classes {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/cut/"+class.Code, nil))
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
			t.Errorf("%s: answered %d with Retry-After %q, want 503 and 1", class.Code, w.Code, w.Header().Get("Retry-After"))
		}
		if body := strings.TrimSpace(w.Body.String()); body != dbConnectionLostTestBody {
			t.Errorf("%s: body %q", class.Code, body)
		}
		if got := testutil.ToFloat64(metrics.requests.WithLabelValues(routes[i].id, "503", "unavailable")); got != 1 {
			t.Errorf("%s: 503 unavailable requests = %v, want 1", class.Code, got)
		}
		if got := testutil.ToFloat64(metrics.requests.WithLabelValues(routes[i].id, "500", "panic")); got != 0 {
			t.Errorf("%s: counted as %v panics", class.Code, got)
		}
	}
	for i, err := range kept {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/kept/%d", i), nil))
		if w.Code != http.StatusInternalServerError || w.Header().Get("Retry-After") != "" || w.Body.String() != UnexpectedErrorMessage+"\n" {
			t.Errorf("%v: answered %d with Retry-After %q and body %q, want the fixed 500", err, w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
		if got := testutil.ToFloat64(metrics.requests.WithLabelValues(routes[len(classes)+i].id, "500", "panic")); got != 1 {
			t.Errorf("%v: 500 panic requests = %v, want 1", err, got)
		}
	}
}

// A connection cut after the route committed its response cannot be answered
// 503: the status is sent. The response aborts, as a done error's does, so
// the caller sees a broken response and not a whole one with an error
// appended.
func TestRouterAbortsACommittedResponseWhoseConnectionWasCut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	metrics := newHttpMetrics(prometheus.NewRegistry())
	route := NewRoute(http.MethodGet, "/committed", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"partial":`))
		panic(&pgconn.PgError{Code: pgerrcode.AdminShutdown, Message: "terminating connection due to administrator command"})
	})
	router := NewRouter(ctx, []*Route{route})
	router.metrics = metrics

	w := httptest.NewRecorder()
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("recovered %v, want http.ErrAbortHandler", recovered)
			}
		}()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/committed", nil))
	}()
	if w.Code != http.StatusOK || w.Body.String() != `{"partial":` || w.Header().Get("Retry-After") != "" {
		t.Errorf("the committed response became %d %q with Retry-After %q", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
	}
	if got := testutil.ToFloat64(metrics.requests.WithLabelValues(route.id, "200", "aborted")); got != 1 {
		t.Errorf("200 aborted requests = %v, want 1", got)
	}
}

// A cut connection an impl returns reaches RaiseHttpError through the
// wrappers' tags, and one under PrivateErrors, returned or raised, keeps its
// retry hint, where every other undeclared failure there is the fixed 503
// without one.
func TestWrappersAnswerACutPostgresConnectionAsRetryable(t *testing.T) {
	cut := func() error {
		return fmt.Errorf("commit: %w", &pgconn.PgError{Code: pgerrcode.AdminShutdown, Message: "terminating connection due to administrator command"})
	}
	returns := func(clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		return "", cut()
	}
	raises := func(clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		panic(cut())
	}
	returnsWithInput := func(input map[string]string, clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		return "", cut()
	}
	raisesWithInput := func(input map[string]string, clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		panic(cut())
	}
	want := privateErrorTestResponse{code: http.StatusServiceUnavailable, body: dbConnectionLostTestBody + "\n", contentType: "application/json", retryAfter: "1"}
	for name, response := range map[string]privateErrorTestResponse{
		"returned":                    privateErrorTestServe(returns),
		"returned with input":         privateErrorTestServeWithInput(returnsWithInput),
		"private returned":            privateErrorTestServe(PrivateErrors(returns)),
		"private raised":              privateErrorTestServe(PrivateErrors(raises)),
		"private returned with input": privateErrorTestServeWithInput(PrivateErrorsWithInput(returnsWithInput)),
		"private raised with input":   privateErrorTestServeWithInput(PrivateErrorsWithInput(raisesWithInput)),
	} {
		if response != want {
			t.Errorf("%s: answered %+v, want %+v", name, response, want)
		}
	}

	// another Postgres failure under PrivateErrors stays the fixed 503, with no hint
	response := privateErrorTestServe(PrivateErrors(func(clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		return "", fmt.Errorf("insert: %w", &pgconn.PgError{Code: pgerrcode.UniqueViolation, Message: "synthetic duplicate key"})
	}))
	if response.code != http.StatusServiceUnavailable || response.retryAfter != "" || response.body != "Service temporarily unavailable.\n" {
		t.Errorf("another Postgres failure under PrivateErrors answered %+v", response)
	}
}
