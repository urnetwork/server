package router

// handler_caller_gone_test.go pins how the route metrics count a request
// whose caller has gone. Authentication of such a request stops on the
// caller's context and becomes session.ErrAuthUnavailable, which every
// authenticated wrapper answers "503 Authentication temporarily unavailable".
// Nobody receives that answer (the lb logged 499 for each), yet the metrics
// counted it as a 503: on 2026-11-11 every 503 of GET /account/referral-code
// and GET /subscription/balance, and a tenth of those of GET
// /network/reliability. A 503 that goes out after the caller has gone now
// counts as no status, as when nothing is written; other statuses keep their
// exact count. The answers themselves do not change. Pure: the credential
// never verifies, so nothing reads the database or redis.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/session"
)

func callerGoneTestRequest(t *testing.T, path string, gone bool, intended ...string) *http.Request {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if gone {
		cancel()
	}
	request := httptest.NewRequest(http.MethodPost, "http://localhost"+path, bytes.NewBufferString("{}"))
	request = request.WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:1"
	request.Header.Set("Authorization", "Bearer synthetically-invalid")
	for _, value := range intended {
		request.Header.Add(session.IntendedNetworkIdHeader, value)
	}
	return request
}

// Serves one request through a router holding only route, with its own
// metrics, and returns the answer and the metrics.
func serveCallerGoneRoute(t *testing.T, route *Route, request *http.Request) (*httptest.ResponseRecorder, *httpMetrics) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router := NewRouter(ctx, []*Route{route})
	router.metrics = newHttpMetrics(prometheus.NewRegistry())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response, router.metrics
}

func requireRouteCount(t *testing.T, metrics *httpMetrics, route *Route, status string, outcome string, want float64) {
	t.Helper()
	if got := testutil.ToFloat64(metrics.requests.WithLabelValues(route.id, status, outcome)); got != want {
		t.Fatalf("%s %s/%s requests = %v, want %v", route.id, status, outcome, got, want)
	}
}

// Through every wrapper, a 503 answered to a caller that has gone counts as
// canceled with no status: the authentication stop's 503 and the cache's 503.
// Any other answer to a gone caller, such as a declared refusal or an
// undeclared failure, keeps its exact status beside the canceled outcome, and
// the same request from a caller that waits counts its status as before. The
// answers themselves do not change.
func TestRouterCountsAGoneCallerCanceledNotUnavailable(t *testing.T) {
	errs := map[string]error{
		"cache-unavailable": &handlerCacheUnavailableError{},
		"declared-refusal":  fmt.Errorf("%d Synthetic conflict.", http.StatusConflict),
		"undeclared":        errors.New("synthetic failure"),
	}
	type wrapperCase struct {
		name string
		// whether the wrapper authenticates; the invalid credential then
		// never reaches the impl
		authenticated bool
		handler       func(err error) http.HandlerFunc
	}
	cases := []wrapperCase{}
	for _, authenticated := range []struct {
		name string
		wrap func(ImplFunction[bool], http.ResponseWriter, *http.Request, ...FormatFunction[bool])
	}{
		{"require_auth", WrapRequireAuth[bool]},
		{"require_client", WrapRequireClient[bool]},
		{"no_auth", WrapNoAuth[bool]},
	} {
		cases = append(cases, wrapperCase{authenticated.name, authenticated.name != "no_auth", func(err error) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				authenticated.wrap(func(*session.ClientSession) (bool, error) { return false, err }, w, r)
			}
		}})
	}
	for _, authenticated := range []struct {
		name string
		wrap func(ImplWithInputFunction[map[string]any, bool], http.ResponseWriter, *http.Request, ...FormatFunction[bool])
	}{
		{"input_auth", WrapWithInputRequireAuth[map[string]any, bool]},
		{"input_client", WrapWithInputRequireClient[map[string]any, bool]},
		{"optional_auth", WrapWithInputOptionalAuth[map[string]any, bool]},
		{"input_no_auth", WrapWithInputNoAuth[map[string]any, bool]},
	} {
		cases = append(cases, wrapperCase{authenticated.name, authenticated.name != "input_no_auth", func(err error) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				authenticated.wrap(func(map[string]any, *session.ClientSession) (bool, error) { return false, err }, w, r)
			}
		}})
	}
	for _, wrapper := range cases {
		for errName, err := range errs {
			t.Run(wrapper.name+"/"+errName, func(t *testing.T) {
				route := NewRoute(http.MethodPost, "/route", wrapper.handler(err))
				want := http.StatusUnauthorized
				if !wrapper.authenticated {
					want = map[string]int{
						"cache-unavailable": http.StatusServiceUnavailable,
						"declared-refusal":  http.StatusConflict,
						"undeclared":        http.StatusInternalServerError,
					}[errName]
				}
				response, metrics := serveCallerGoneRoute(t, route, callerGoneTestRequest(t, "/route", false))
				if response.Code != want {
					t.Fatalf("waiting caller answered %d, want %d", response.Code, want)
				}
				requireRouteCount(t, metrics, route, fmt.Sprint(want), "completed", 1)

				goneWant := want
				if wrapper.authenticated {
					// the authentication stop, never a credential rejection
					goneWant = http.StatusServiceUnavailable
				}
				response, metrics = serveCallerGoneRoute(t, route, callerGoneTestRequest(t, "/route", true))
				if response.Code != goneWant {
					t.Fatalf("gone caller answered %d, want the unchanged %d", response.Code, goneWant)
				}
				if goneWant == http.StatusServiceUnavailable {
					requireRouteCount(t, metrics, route, "none", "canceled", 1)
					requireRouteCount(t, metrics, route, "503", "canceled", 0)
				} else {
					requireRouteCount(t, metrics, route, fmt.Sprint(goneWant), "canceled", 1)
					requireRouteCount(t, metrics, route, "none", "canceled", 0)
				}
			})
		}
	}
}

// A status that went out while the caller waited keeps its count when the
// caller goes before the request finishes, and so does the router's own 503
// for a done error that escapes a route while its caller waits.
func TestRouterKeepsTheStatusOfACallerThatWasThere(t *testing.T) {
	answered := NewRoute(http.MethodPost, "/answered", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		// the caller goes after the status went out
		if cancel, ok := r.Context().Value(callerGoneTestCancelKey{}).(context.CancelFunc); ok {
			cancel()
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := callerGoneTestRequest(t, "/answered", false)
	request = request.WithContext(context.WithValue(ctx, callerGoneTestCancelKey{}, cancel))
	_, metrics := serveCallerGoneRoute(t, answered, request)
	requireRouteCount(t, metrics, answered, "418", "canceled", 1)
	requireRouteCount(t, metrics, answered, "none", "canceled", 0)

	expired := NewRoute(http.MethodPost, "/expired", func(http.ResponseWriter, *http.Request) {
		panic(context.Canceled)
	})
	_, metrics = serveCallerGoneRoute(t, expired, callerGoneTestRequest(t, "/expired", false))
	requireRouteCount(t, metrics, expired, "503", "canceled", 1)
}

type callerGoneTestCancelKey struct{}

// The intended network gate resolves the credential's network before the
// handler, and its stop on a gone caller's context is the same authentication
// stop: the handler does not run, and its 503 counts as no status.
func TestIntendedNetworkGateCountsAGoneCallerCanceled(t *testing.T) {
	reached := false
	route := RefuseIntendedNetworkMismatch(NewRoute(http.MethodPost, "/gated", func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	intended := "01980000-0000-0000-0000-000000000000"
	response, metrics := serveCallerGoneRoute(t, route, callerGoneTestRequest(t, "/gated", true, intended))
	if reached || response.Code != http.StatusServiceUnavailable {
		t.Fatalf("gone caller: handler ran = %v, answered %d; want the gate's unchanged 503", reached, response.Code)
	}
	requireRouteCount(t, metrics, route, "none", "canceled", 1)
	requireRouteCount(t, metrics, route, "503", "canceled", 0)

	// a waiting caller whose credential does not verify reaches the handler
	response, metrics = serveCallerGoneRoute(t, route, callerGoneTestRequest(t, "/gated", false, intended))
	if !reached || response.Code != http.StatusOK {
		t.Fatalf("waiting caller: handler ran = %v, answered %d; want the handler", reached, response.Code)
	}
	requireRouteCount(t, metrics, route, "200", "completed", 1)
}
