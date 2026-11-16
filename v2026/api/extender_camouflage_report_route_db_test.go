package api

// End to end through the router the api serves, against the database and Redis: a
// real client token posts a camouflage report (connect/EXTENDER-SPLICE-ROLLOUT.md).
// The route answers disabled until `extender.yml` turns it on, counts a report once,
// holds a client to its budget, refuses a body past its bound, and records nothing
// about who posted.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
)

const extenderCamouflageReportTestPath = "/network/extender-camouflage-report"

// The total of one counter family's series with one label value, in the process's
// default registry, which is where the route counts.
func extenderCamouflageReportTestCount(t testing.TB, family string, labelName string, labelValue string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, metricFamily := range families {
		if metricFamily.GetName() != family {
			continue
		}
		for _, metric := range metricFamily.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == labelName && label.GetValue() == labelValue {
					total += metric.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}

func installExtenderCamouflageReportTestConfig(t testing.TB, enabled bool) {
	t.Helper()
	rootKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	pop := server.Vault.PushSimpleResource("extender.yml", []byte(fmt.Sprintf(
		`
root_private_key_hex: %s
network_host: report.example
camouflage_report:
  enabled: %t
`,
		hex.EncodeToString(rootKeySeed),
		enabled,
	)))
	controller.Testing_ResetExtenderConfig()
	t.Cleanup(func() {
		pop()
		controller.Testing_ResetExtenderConfig()
	})
}

func TestExtenderCamouflageReportRouteWithARealClientToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fixture := newRouteAccessDbFixture(t, ctx)
		_, clientToken := fixture.mintClient(t, fixture.root(), nil)
		_, otherClientToken := fixture.mintClient(t, fixture.root(), nil)

		post := func(token string, reportId string) *controller.ExtenderCamouflageReportResult {
			t.Helper()
			var result controller.ExtenderCamouflageReportResult
			fixture.call(t, http.MethodPost, extenderCamouflageReportTestPath, "Bearer "+token, map[string]any{
				"report_id":      reportId,
				"client_version": "2026.10.30-1085916410",
				"ip_versions":    []int{4},
				"client":         map[string]any{"both_failed": 3},
				// a claim the route has no use for
				"country": "zz",
			}, http.StatusOK, &result)
			return &result
		}
		reportId := func() string {
			return hex.EncodeToString(server.NewId().Bytes())
		}

		// without a token, nothing
		if w := fixture.serve(http.MethodPost, extenderCamouflageReportTestPath, "", map[string]any{}); w.Code != http.StatusUnauthorized {
			t.Fatalf("no token = %d", w.Code)
		}
		// a network credential is not a client
		if w := fixture.serve(http.MethodPost, extenderCamouflageReportTestPath, fixture.root(), map[string]any{}); w.Code != http.StatusUnauthorized {
			t.Fatalf("the network's own token = %d", w.Code)
		}

		// off: an operator that says nothing has it off
		installExtenderCamouflageReportTestConfig(t, false)
		before := extenderCamouflageReportTestCount(t, "urnetwork_extender_camouflage_client_outcomes_total", "outcome", "both_failed")
		if result := post(clientToken, reportId()); result.Outcome != controller.ExtenderCamouflageReportDisabled {
			t.Fatalf("off = %+v", result)
		}
		if count := extenderCamouflageReportTestCount(t, "urnetwork_extender_camouflage_client_outcomes_total", "outcome", "both_failed"); count != before {
			t.Errorf("counted while off: %v, was %v", count, before)
		}

		installExtenderCamouflageReportTestConfig(t, true)
		id := reportId()
		if result := post(clientToken, id); result.Outcome != controller.ExtenderCamouflageReportAccepted {
			t.Fatalf("on = %+v", result)
		}
		if count := extenderCamouflageReportTestCount(t, "urnetwork_extender_camouflage_client_outcomes_total", "outcome", "both_failed"); count != before+3 {
			t.Errorf("counted %v, was %v", count, before)
		}
		if result := post(clientToken, id); result.Outcome != controller.ExtenderCamouflageReportDuplicate {
			t.Fatalf("a retry = %+v", result)
		}
		if count := extenderCamouflageReportTestCount(t, "urnetwork_extender_camouflage_client_outcomes_total", "outcome", "both_failed"); count != before+3 {
			t.Errorf("a retry counted again: %v, was %v", count, before)
		}

		// a body past the bound is refused for its size, whatever it holds
		tooLarge := extenderCamouflageReportTestCount(t, "urnetwork_extender_camouflage_reports_total", "outcome", "too_large")
		var refused controller.ExtenderCamouflageReportResult
		fixture.call(t, http.MethodPost, extenderCamouflageReportTestPath, "Bearer "+clientToken, map[string]any{
			"report_id": reportId(),
			"padding":   strings.Repeat("x", controller.ExtenderCamouflageReportMaxByteCount),
		}, http.StatusOK, &refused)
		if refused.Outcome != controller.ExtenderCamouflageReportInvalid {
			t.Errorf("too large = %+v", refused)
		}
		if count := extenderCamouflageReportTestCount(t, "urnetwork_extender_camouflage_reports_total", "outcome", "too_large"); count != tooLarge+1 {
			t.Errorf("too large counted %v, was %v", count, tooLarge)
		}

		// a client runs out of its own budget, and another does not. The fixed window
		// may turn over mid test, which only makes more room, so the refusal is looked
		// for up to two windows' worth
		limited := false
		for range 24 {
			if result := post(clientToken, reportId()); result.Outcome == controller.ExtenderCamouflageReportRateLimited {
				limited = true
				if result.RetryAfterSeconds <= 0 {
					t.Errorf("rate limited = %+v", result)
				}
				break
			}
		}
		if !limited {
			t.Errorf("the budget never refused")
		}
		if result := post(otherClientToken, reportId()); result.Outcome != controller.ExtenderCamouflageReportAccepted {
			t.Errorf("another client = %+v", result)
		}

		// the answer is the route's whole contract
		w := fixture.serve(http.MethodPost, extenderCamouflageReportTestPath, "Bearer "+otherClientToken, map[string]any{
			"report_id": reportId(),
			"client":    map[string]any{"refused": 1},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		answer := map[string]any{}
		if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		if len(answer) != 1 || answer["outcome"] != controller.ExtenderCamouflageReportAccepted {
			t.Errorf("answer = %s", w.Body.String())
		}
	})
}
