// Exercise query attribution through the real H1 and QUIC admission handlers.
package connect

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

// A bounded-label snapshot; gathering does not acquire a database connection.
func connectStateQueryMetric(t testing.TB, caller, operation, credential, outcome string) float64 {
	t.Helper()
	return connectObservedCounter(t, "urnetwork_jwt_state_queries_total", map[string]string{"caller": caller, "operation": operation, "credential": credential, "outcome": outcome})
}

// Exact labels select one cell; nil labels sum all fixed cells of a family.
func connectObservedCounter(t testing.TB, name string, want map[string]string) float64 {
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

// One handshake sends one JWT-state query. Both H1 authentication forms and
// H1+ share H1's bucket; QUIC has a separate trusted source. This test measures
// admission queries, not subsequent packet processing or provider throughput.
func TestConnectStateQueryMetricActualHandshakes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		env, _, token := authenticationDeadlineEnvironment(ctx, t, 3*time.Second, nil)
		defer env.Close()
		for _, carrier := range []string{"h1_header", "h1_frame", "h1plus", "h3"} {
			caller := "connect_h1"
			if carrier == "h3" {
				caller = "connect_h3"
			}
			before := connectStateQueryMetric(t, caller, "handshake", "client", "state_valid")
			unknownBefore := connectStateQueryMetric(t, "unknown", "unknown", "client", "state_valid")
			client, err := dialAuthenticationDeadlineClient(ctx, env, carrier, token)
			if err != nil {
				t.Fatal(err)
			}
			admitted := TestingWaitForConnectCondition(ctx, 3*time.Second, time.Millisecond, func(context.Context) (bool, string) {
				observed := connectStateQueryMetric(t, caller, "handshake", "client", "state_valid")
				return observed == before+1, "waiting for attributed state query"
			})
			if admitted != nil {
				client.close()
				t.Fatal(admitted)
			}

			client.close()
			if !waitForAuthenticationDeadlineConnections(ctx, env.handler, 0, 3*time.Second) {
				t.Fatal("carrier did not join")
			}
			got := connectStateQueryMetric(t, caller, "handshake", "client", "state_valid") - before
			if got != 1 || connectStateQueryMetric(t, "unknown", "unknown", "client", "state_valid") != unknownBefore {
				t.Fatalf("carrier=%s query_delta=%.0f: query source lost or handshake revalidated", carrier, got)
			}
			t.Logf("carrier=%s handshake_state_queries=%.0f", carrier, got)
		}
	})
}
