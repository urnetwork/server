// Exercise attribution through actual request authentication, not a metric seam.
package session_test

import (
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A bounded-label snapshot; gathering does not acquire a database connection.
func sessionStateQueryMetric(t testing.TB, caller, operation, credential, outcome string) float64 {
	t.Helper()
	return sessionObservedCounter(t, "urnetwork_jwt_state_queries_total", map[string]string{"caller": caller, "operation": operation, "credential": credential, "outcome": outcome})
}

// Exact labels select one cell; nil labels sum all fixed cells of a family.
func sessionObservedCounter(t testing.TB, name string, want map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		var sum float64
		for _, metric := range family.Metric {
			sum += metric.GetCounter().GetValue()
			matched := len(metric.Label) == len(want)
			for _, label := range metric.Label {
				matched = matched && want[label.GetName()] == label.GetValue()
			}
			if matched {
				return metric.GetCounter().GetValue()
			}
		}
		if want == nil {
			return sum
		}
	}
	t.Fatalf("counter or child is missing: %s", name)
	return 0
}

// An inherited source or client-controlled header cannot misattribute the API.
// One request emits one query event, regardless of its controller operation.
func TestSessionStateQueryMetricUsesActualApiCaller(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := session.WithStateQuerySource(t.Context(), session.StateQueryProberControl)
		networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "api-state-query", userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "synthetic", "synthetic")
		token := session.NewByJwt(networkId, userId, "api-state-query", false, false).Client(deviceId, clientId).Testing_Sign()
		proberBefore := sessionStateQueryMetric(t, "prober", "control", "client", "state_valid")
		unknownBefore := sessionStateQueryMetric(t, "unknown", "unknown", "client", "state_valid")
		for _, control := range []struct{ method, path, operation string }{
			{"POST", "/connect/control", "control"},
			{"POST", "/network/auth-client", "mint"},
			{"POST", "/network/remove-client", "retire"},
			{"POST", "/network/find-providers2", "discovery"},
			{"GET", "/auth/refresh", "refresh"},
			{"GET", "/network/clients", "other"},
		} {
			acquiresBefore := sessionObservedCounter(t, "urnetwork_pg_pool_acquires_total", map[string]string{"pool": "default", "outcome": "acquired"})
			before := sessionStateQueryMetric(t, "api", control.operation, "client", "state_valid")
			req := httptest.NewRequest(control.method, "https://fixture.example"+control.path, nil).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Jwt-Caller", "hosted")
			s, err := session.NewClientSessionFromRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			err = s.Auth(req)
			s.Cancel()
			if err != nil || sessionStateQueryMetric(t, "api", control.operation, "client", "state_valid") != before+1 {
				t.Fatalf("API source lost: operation=%s rejected=%t", control.operation, err != nil)
			}
			if sessionObservedCounter(t, "urnetwork_pg_pool_acquires_total", map[string]string{"pool": "default", "outcome": "acquired"}) != acquiresBefore+1 {
				t.Fatal("API authentication added work beyond its single state query")
			}
		}
		if sessionStateQueryMetric(t, "prober", "control", "client", "state_valid") != proberBefore || sessionStateQueryMetric(t, "unknown", "unknown", "client", "state_valid") != unknownBefore {
			t.Fatal("API used an inherited or unknown caller")
		}
	})
}
