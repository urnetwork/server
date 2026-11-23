package alt

// alt's public api front answers nothing the lb keeps from outside sources (§19.4's api row): 403, as the lb answers
// a source outside the ones it lists, to every route of api's set under the internal gate prefix and to the abuse
// staff's route, which alt's installs of api's route set would otherwise serve to a bearer from anywhere
// (controller.InstallApiRoutes; alt_installs_test.go), and every other route of the set handed to the router.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api"
)

// A path that names an instance of the route pattern: each capture an id, each escaped dot a dot. Empty for a
// pattern of another shape.
func testRouteSamplePath(pattern string) string {
	path := strings.ReplaceAll(pattern, "([^/]+)", server.NewId().String())
	path = strings.ReplaceAll(path, `\.`, ".")
	routeRegex, err := regexp.Compile("^(?:" + pattern + ")$")
	if err != nil || !routeRegex.MatchString(path) {
		return ""
	}
	return path
}

// The public api front answers every route of api's set under the internal gate prefix, and the abuse staff's route,
// 403 by any method, before the router sees the request, as the lb answers them to a source outside the ones it lists;
// /status stays a path with no route; every other route of the set reaches the router as the client sent it.
func TestAltApiFrontRefusesWhatTheLbServesOnlyToItsOwnSources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reached := []string{}
	routerStandIn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	})
	front := newApiFrontHandler(routerStandIn, NewLimits(ctx, testLimitsSettings(t)))
	// one peer per request, so the per-address limits never decide an answer
	peer := 0
	serve := func(method string, path string) (int, string, bool) {
		peer += 1
		reachedCount := len(reached)
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = fmt.Sprintf("[2001:db8::%x]:53100", peer)
		w := httptest.NewRecorder()
		front.ServeHTTP(w, r)
		return w.Code, w.Body.String(), reachedCount < len(reached)
	}

	refusedRoutes := []string{}
	for _, route := range api.Routes() {
		routeKey := route.Method() + " " + route.Pattern()
		path := testRouteSamplePath(route.Pattern())
		restricted := strings.HasPrefix(route.Pattern(), server.InternalGatePathPrefix) || route.Pattern() == abuseStaffPath
		if path == "" {
			if restricted {
				t.Errorf("%s: no sample path for a route the front refuses", routeKey)
			}
			continue
		}
		code, body, routed := serve(route.Method(), path)
		switch {
		case route.Pattern() == statusPath:
			if code != http.StatusNotFound || routed {
				t.Errorf("%s answered %d, routed %t, over the public front", routeKey, code, routed)
			}
		case restricted:
			refusedRoutes = append(refusedRoutes, routeKey)
			if code != http.StatusForbidden || body != http.StatusText(http.StatusForbidden)+"\n" || routed {
				t.Errorf("%s answered %d %q, routed %t, over the public front", routeKey, code, body, routed)
			}
		default:
			if code != http.StatusTeapot || !routed {
				t.Errorf("%s answered %d, routed %t, over the public front", routeKey, code, routed)
			}
		}
	}
	// the reads alt's install of the projection serves, and the staff route, among the refused
	for _, routeKey := range []string{
		"GET /internal/gate/projection/([^/]+)",
		"GET /internal/gate/rows/([^/]+)/([^/]+)",
		"GET /internal/gate/listen-facts/([^/]+)",
		"GET /internal/gate/limits/([^/]+)",
		"POST /internal/gate/edge-client",
		"POST /internal/gate/sidecar-identity-clients",
		"POST /abuse/action",
	} {
		if !slices.Contains(refusedRoutes, routeKey) {
			t.Errorf("%s is not among the routes the front refused: %v", routeKey, refusedRoutes)
		}
	}

	// by any method, and only the prefix and the one staff path
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete} {
		for _, path := range []string{server.InternalGatePathPrefix, server.InternalGatePathPrefix + "no-such-route", abuseStaffPath} {
			if code, _, routed := serve(method, path); code != http.StatusForbidden || routed {
				t.Errorf("%s %s answered %d, routed %t, over the public front", method, path, code, routed)
			}
		}
	}
	for _, path := range []string{"/internal/gatex", "/internal/gate", "/abuse/actions", "/abuse/action/", "/abuse/status"} {
		if code, _, routed := serve(http.MethodGet, path); code != http.StatusTeapot || !routed {
			t.Errorf("GET %s answered %d, routed %t, over the public front", path, code, routed)
		}
	}
}
