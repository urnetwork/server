package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026/router"
)

// The route metric's count of one route, status and outcome, read from the
// process registry the router reports to.
func routeRequestCount(t *testing.T, route string, status string, outcome string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_http_requests_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["route"] == route && labels["status"] == status && labels["outcome"] == outcome {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// The four app read routes that answered 503 on 2026-11-11. Each request
// here comes from a caller that has already gone, as the apps' canceled
// parallel attempts do (the lb logged 499 for each): authentication stops on
// the caller's context, and the route's 503 reaches nobody. The route table
// must count each as canceled with no status, not as a 503 outage. Pure: the
// credential never verifies, so nothing reads the database or redis.
func TestAppReadRoutesCountAGoneCallerCanceledNot503(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apiRouter := router.NewRouter(ctx, Routes())
	for _, path := range []string{"/account/referral-code", "/subscription/balance", "/network/reliability", "/transfer/stats"} {
		t.Run(path, func(t *testing.T) {
			route := "GET ^" + path + "$"
			unavailableBefore := routeRequestCount(t, route, "503", "canceled")
			unansweredBefore := routeRequestCount(t, route, "none", "canceled")
			requestCtx, cancelRequest := context.WithCancel(ctx)
			cancelRequest()
			r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(requestCtx)
			r.RemoteAddr = "198.51.100.41:4000"
			r.Header.Set("Authorization", "Bearer synthetically-invalid")
			w := httptest.NewRecorder()
			apiRouter.ServeHTTP(w, r)
			if got := routeRequestCount(t, route, "503", "canceled") - unavailableBefore; got != 0 {
				t.Fatalf("%s counted %v 503s for a gone caller", route, got)
			}
			if got := routeRequestCount(t, route, "none", "canceled") - unansweredBefore; got != 1 {
				t.Fatalf("%s counted %v canceled requests with no status, want 1", route, got)
			}
		})
	}
}
