package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// A smoke: a fixed set of real routes whose status codes must hold from one
// release to the next and across a migration. A route a later release removes or
// breaks shows as a changed code, which is what FS-55 is for.

// One route a smoke asks, and the codes that count as holding.
type fs55Route struct {
	method string
	path   string
	// a body for a POST, "" for none
	body string
	// the codes that hold, any of them
	wantCodes []int
}

// What one route answered.
type fs55RouteResult struct {
	route fs55Route
	code  int
	err   error
}

func (self fs55RouteResult) held() bool {
	return self.err == nil && slices.Contains(self.route.wantCodes, self.code)
}

func (self fs55RouteResult) String() string {
	if self.err != nil {
		return fmt.Sprintf("%s %s: %s", self.route.method, self.route.path, self.err)
	}
	verdict := "holds"
	if !self.held() {
		verdict = fmt.Sprintf("does not hold, want %v", self.route.wantCodes)
	}
	return fmt.Sprintf("%s %s: %d %s", self.route.method, self.route.path, self.code, verdict)
}

// api's routes every release answers the same way with no session: its status
// route, which reads the migration head, an unauthenticated read refused, and a
// public route that reads the database. Each needs no credential and writes
// nothing, so a smoke changes no state a later phase reads.
func apiSmokeRoutes() []fs55Route {
	return []fs55Route{
		{method: http.MethodGet, path: "/status", wantCodes: []int{http.StatusOK}},
		{method: http.MethodGet, path: "/network/clients", wantCodes: []int{http.StatusUnauthorized, http.StatusForbidden}},
		{method: http.MethodPost, path: "/auth/network-check", body: `{"network_name":"fs55-smoke-check"}`, wantCodes: []int{http.StatusOK}},
	}
}

// Asks each route once, in order, with its own bound.
func smokeRoutes(ctx context.Context, baseUrl string, routes []fs55Route, routeTimeout time.Duration) []fs55RouteResult {
	client := &http.Client{
		// a redirect is an answer to report, not a route to follow
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	results := make([]fs55RouteResult, 0, len(routes))
	for _, route := range routes {
		result := fs55RouteResult{route: route}
		func() {
			routeCtx, cancel := context.WithTimeout(ctx, routeTimeout)
			defer cancel()
			var body io.Reader
			if route.body != "" {
				body = strings.NewReader(route.body)
			}
			request, err := http.NewRequestWithContext(routeCtx, route.method, baseUrl+route.path, body)
			if err != nil {
				result.err = err
				return
			}
			if route.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			response, err := client.Do(request)
			if err != nil {
				result.err = err
				return
			}
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
			result.code = response.StatusCode
		}()
		results = append(results, result)
	}
	return results
}
