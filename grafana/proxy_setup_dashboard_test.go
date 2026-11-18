// The proxy dashboard's setup-time and per-host saturation rows, panels 200
// to 214 at the end of the dashboard (proxy setup_metrics.go, P90SPEED.md
// §8): the CONNECT setup, route wait, first exit and first dial histograms by
// how the device was opened, and per host the hosted mint admissions and
// refusals, the Go heap against its memory limit, GC cycles a minute and
// abandoned CONNECTs, with the alert levels of SIGNALS.md §26.12-§26.14 drawn
// as threshold lines; and panels 215 to 223, find-providers2's liveness check
// per host and block (fix #4), on which a proxy canary is judged.
package grafana

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	proxySetupRow              = 200
	proxySetupPercentilesPanel = 201
	proxySetupByHostPanel      = 202
	proxyRouteWaitPanel        = 203
	proxyUnroutedPanel         = 204
	proxyBuildsPanel           = 205
	proxyFirstExitPanel        = 206
	proxyFirstDialPanel        = 207
	proxySaturationRow         = 208
	proxyMintsPanel            = 209
	proxyMintRefusedSharePanel = 210
	proxyHeapLimitPanel        = 211
	proxyHeapBytesPanel        = 212
	proxyGcPanel               = 213
	proxyAbandonedPanel        = 214
	// find-providers2 liveness (fix #4, branch fix/p90-L-1118)
	proxyLivenessRow           = 215
	proxyLivenessUnknownPanel  = 216
	proxyLivenessFailOpenPanel = 217
	proxyLivenessReasonsPanel  = 218
	proxyLivenessCachePanel    = 219
	proxyLivenessHitPanel      = 220
	proxyLivenessRefreshPanel  = 221
	proxyLivenessClientsPanel  = 222
	proxyLivenessErrorsPanel   = 223
)

// Each panel and the metric it must read.
var proxySetupPanelMetrics = map[int][]string{
	proxySetupPercentilesPanel: {"urnetwork_proxy_connect_setup_seconds_bucket"},
	proxySetupByHostPanel:      {"urnetwork_proxy_connect_setup_seconds_bucket"},
	proxyRouteWaitPanel:        {"urnetwork_proxy_connect_route_wait_seconds_bucket"},
	proxyUnroutedPanel:         {"urnetwork_proxy_connect_route_wait_seconds_count"},
	proxyBuildsPanel:           {"urnetwork_proxy_device_builds_total"},
	proxyFirstExitPanel:        {"urnetwork_proxy_device_first_exit_seconds_bucket"},
	proxyFirstDialPanel:        {"urnetwork_proxy_device_first_dial_seconds_bucket", "urnetwork_proxy_device_first_dial_seconds_count"},
	proxyMintsPanel:            {"urnetwork_jwt_state_queries_total", "urnetwork_hosted_mint_admission_refusals_total"},
	proxyMintRefusedSharePanel: {"urnetwork_jwt_state_queries_total", "urnetwork_hosted_mint_admission_refusals_total"},
	proxyHeapLimitPanel:        {"go_memstats_heap_inuse_bytes", "go_memstats_stack_inuse_bytes", "go_gc_gomemlimit_bytes"},
	proxyHeapBytesPanel:        {"go_memstats_heap_inuse_bytes", "go_memstats_stack_inuse_bytes", "go_gc_gomemlimit_bytes"},
	proxyGcPanel:               {"go_gc_duration_seconds_count"},
	proxyAbandonedPanel:        {"urnetwork_proxy_ingress_events_total"},
	proxyLivenessUnknownPanel:  {"urnetwork_findproviders2_liveness_candidates_total"},
	proxyLivenessFailOpenPanel: {"urnetwork_findproviders2_liveness_failopens_total"},
	proxyLivenessReasonsPanel:  {"urnetwork_findproviders2_liveness_failopens_total"},
	proxyLivenessCachePanel:    {"urnetwork_findproviders2_liveness_cache_total"},
	proxyLivenessHitPanel:      {"urnetwork_findproviders2_liveness_cache_total"},
	proxyLivenessRefreshPanel:  {"urnetwork_findproviders2_liveness_refreshes_total"},
	proxyLivenessClientsPanel:  {"urnetwork_findproviders2_liveness_cache_clients"},
	proxyLivenessErrorsPanel:   {"urnetwork_findproviders2_liveness_read_errors_total"},
}

func TestProxyDashboardShowsSetupTimeAndHostSaturation(t *testing.T) {
	dashboard := readTestDashboard(t, "proxy.json")
	for _, id := range []int{proxySetupRow, proxySaturationRow, proxyLivenessRow} {
		if row := dashboardPanelById(dashboard, id); row == nil || row.Type != "row" {
			t.Fatalf("proxy dashboard row %d is missing", id)
		}
	}
	for id, metrics := range proxySetupPanelMetrics {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil {
			t.Fatalf("proxy dashboard panel %d is missing", id)
		}
		if panel.Description == "" {
			t.Errorf("panel %d (%s) says nothing of how to read it", id, panel.Title)
		}
		joined := ""
		for _, target := range panel.Targets {
			joined += target.Expr + "\n"
		}
		for _, metric := range metrics {
			if !strings.Contains(joined, metric+"{") {
				t.Errorf("panel %d (%s) does not read %s", id, panel.Title, metric)
			}
		}
		// a quantile sums its buckets by le and keeps its grouping
		if strings.Contains(joined, "histogram_quantile(") && !strings.Contains(joined, "sum by (le, ") {
			t.Errorf("panel %d (%s) takes a quantile without summing its buckets by le: %s", id, panel.Title, joined)
		}
	}

	// the alert levels of SIGNALS.md §26.12 and §26.13
	document := map[string]any{}
	body, err := dashboardsFs.ReadFile("dashboards/proxy.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	thresholds := map[int]float64{}
	for _, item := range document["panels"].([]any) {
		panel := item.(map[string]any)
		id := int(panel["id"].(float64))
		fieldConfig, _ := panel["fieldConfig"].(map[string]any)
		defaults, _ := fieldConfig["defaults"].(map[string]any)
		thresholdConfig, _ := defaults["thresholds"].(map[string]any)
		steps, _ := thresholdConfig["steps"].([]any)
		for _, step := range steps {
			if value, ok := step.(map[string]any)["value"].(float64); ok {
				thresholds[id] = value
			}
		}
	}
	if thresholds[proxyMintsPanel] != 5 || thresholds[proxyHeapLimitPanel] != 0.95 {
		t.Errorf("alert lines: mint refusals %v, heap %v; want 5 and 0.95 (SIGNALS.md §26.12, §26.13)", thresholds[proxyMintsPanel], thresholds[proxyHeapLimitPanel])
	}
	// P90SPEED.md §10's target for the unknown share, and the lease cache's bound
	if thresholds[proxyLivenessUnknownPanel] != 0.05 || thresholds[proxyLivenessClientsPanel] != 65536 {
		t.Errorf("liveness lines: unknown share %v, cache clients %v; want 0.05 and 65536", thresholds[proxyLivenessUnknownPanel], thresholds[proxyLivenessClientsPanel])
	}
	// the liveness row is per host and block, and service="proxy" (the scope
	// check of TestProxyDashboardCoversBoundedServiceTrafficAndCapacity)
	for _, id := range []int{proxyLivenessUnknownPanel, proxyLivenessFailOpenPanel, proxyLivenessHitPanel, proxyLivenessClientsPanel, proxyLivenessErrorsPanel} {
		if expr := dashboardPanelById(dashboard, id).Targets[0].Expr; !strings.Contains(expr, "by (host, block)") {
			t.Errorf("liveness panel %d is not per host and block: %s", id, expr)
		}
	}
}

// promtool evaluates the panels over a synthetic hour of two proxy hosts:
// the quantiles come out of the buckets by host and open, the refusal share,
// the heap against the limit and GC cycles a minute are per host, and a
// second environment's series and a process without the scope labels are
// never read.
func TestProxyDashboardSetupPromqlEvaluates(t *testing.T) {
	promtool, ctx := proxyTestPromtool(t)
	dashboard := readTestDashboard(t, "proxy.json")
	variables := strings.NewReplacer(`"$env"`, `"main"`, `"$block"`, `".*"`, `"$host"`, `".*"`, "$__rate_interval", "5m")
	query := func(id int, refId string) string {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil {
			t.Fatalf("panel %d is missing", id)
		}
		for _, target := range panel.Targets {
			if target.RefId == refId {
				return variables.Replace(target.Expr)
			}
		}
		t.Fatalf("panel %d has no query %s", id, refId)
		return ""
	}

	series := func(metric string, host string, labels string, values string) map[string]string {
		return map[string]string{
			"series": fmt.Sprintf(`%s{env="main",service="proxy",host=%q,block="g1",instance="a1"%s}`, metric, host, labels),
			"values": values,
		}
	}
	// per second: of 10 cold setups on crisp, 2 under 1 s, 5 under 2 s, 8
	// under 5 s, 9 under 8 s and all under 13 s, so the p90 is the 9th, at
	// the top of the (5, 8] bucket; of fireside's 10 warm setups 1 is under
	// 0.25 s and 9 under 0.5 s. Whole counts a second keep the arithmetic
	// exact.
	buckets := func(host string, open string, counts map[string]int) []map[string]string {
		out := []map[string]string{}
		for _, le := range []string{"0.05", "0.1", "0.25", "0.5", "1", "2", "3", "5", "8", "13", "20", "30", "60", "+Inf"} {
			out = append(out, series("urnetwork_proxy_connect_setup_seconds_bucket", host, fmt.Sprintf(`,protocol="http",open=%q,le=%q`, open, le), fmt.Sprintf("0+%dx120", 60*counts[le])))
		}
		return out
	}
	coldCrisp := map[string]int{"1": 2, "2": 5, "3": 5, "5": 8, "8": 9, "13": 10, "20": 10, "30": 10, "60": 10, "+Inf": 10}
	warmFireside := map[string]int{"0.05": 0, "0.1": 0, "0.25": 1, "0.5": 9, "1": 10, "2": 10, "3": 10, "5": 10, "8": 10, "13": 10, "20": 10, "30": 10, "60": 10, "+Inf": 10}
	input := append(buckets("crisp", "cold", coldCrisp), buckets("fireside", "warm", warmFireside)...)
	input = append(input,
		// crisp refuses 1 mint a second and admits 3; fireside refuses 3 and admits 1
		series("urnetwork_hosted_mint_admission_refusals_total", "crisp", "", "0+60x120"),
		series("urnetwork_jwt_state_queries_total", "crisp", `,caller="hosted",operation="mint",outcome="valid"`, "0+180x120"),
		series("urnetwork_hosted_mint_admission_refusals_total", "fireside", "", "0+180x120"),
		series("urnetwork_jwt_state_queries_total", "fireside", `,caller="hosted",operation="mint",outcome="valid"`, "0+60x120"),
		// another caller's mints are not hosted mints
		series("urnetwork_jwt_state_queries_total", "crisp", `,caller="api",operation="mint",outcome="valid"`, "0+100x120"),
		// crisp: 7.6 of 8 GiB; fireside: 2 of 6.5 GiB on one process and 6.4 on another
		series("go_memstats_heap_inuse_bytes", "crisp", "", "7000000000+0x120"),
		series("go_memstats_stack_inuse_bytes", "crisp", "", "600000000+0x120"),
		series("go_gc_gomemlimit_bytes", "crisp", "", "8000000000+0x120"),
		series("go_memstats_heap_inuse_bytes", "fireside", "", "2000000000+0x120"),
		series("go_memstats_stack_inuse_bytes", "fireside", "", "0+0x120"),
		series("go_gc_gomemlimit_bytes", "fireside", "", "6500000000+0x120"),
		map[string]string{"series": `go_memstats_heap_inuse_bytes{env="main",service="proxy",host="fireside",block="g2",instance="b2"}`, "values": "6000000000+0x120"},
		map[string]string{"series": `go_memstats_stack_inuse_bytes{env="main",service="proxy",host="fireside",block="g2",instance="b2"}`, "values": "175000000+0x120"},
		map[string]string{"series": `go_gc_gomemlimit_bytes{env="main",service="proxy",host="fireside",block="g2",instance="b2"}`, "values": "6500000000+0x120"},
		// 30 GC cycles a minute on crisp's process
		series("go_gc_duration_seconds_count", "crisp", "", "0+30x120"),
		// another environment is never read
		map[string]string{"series": `go_gc_duration_seconds_count{env="other",service="proxy",host="crisp",block="g1",instance="c3"}`, "values": "0+1000x120"},
		// liveness on crisp g1: a second, 3 candidates live and 1 unknown; 3 from the cache and 1 read
		series("urnetwork_findproviders2_liveness_candidates_total", "crisp", `,rank_mode="quality",verdict="live"`, "0+180x120"),
		series("urnetwork_findproviders2_liveness_candidates_total", "crisp", `,rank_mode="quality",verdict="unknown"`, "0+60x120"),
		series("urnetwork_findproviders2_liveness_cache_total", "crisp", `,result="hit"`, "0+180x120"),
		series("urnetwork_findproviders2_liveness_cache_total", "crisp", `,result="read"`, "0+60x120"),
	)
	value := func(labels string, v float64) map[string]any { return map[string]any{"labels": labels, "value": v} }
	evaluations := []struct {
		id      int
		refId   string
		samples []map[string]any
	}{
		{proxySetupPercentilesPanel, "A", []map[string]any{value(`{open="cold"}`, 8), value(`{open="warm"}`, 0.5)}},
		{proxySetupByHostPanel, "A", []map[string]any{value(`{host="crisp",open="cold"}`, 8), value(`{host="fireside",open="warm"}`, 0.5)}},
		{proxyMintsPanel, "B", []map[string]any{value(`{host="crisp"}`, 1), value(`{host="fireside"}`, 3)}},
		{proxyMintRefusedSharePanel, "A", []map[string]any{value(`{host="crisp"}`, 0.25), value(`{host="fireside"}`, 0.75)}},
		{proxyHeapLimitPanel, "A", []map[string]any{value(`{host="crisp"}`, 0.95), value(`{host="fireside"}`, 0.95)}},
		{proxyHeapBytesPanel, "B", []map[string]any{value(`{host="crisp"}`, 8000000000), value(`{host="fireside"}`, 6500000000)}},
		{proxyGcPanel, "A", []map[string]any{value(`{host="crisp"}`, 30)}},
		{proxyLivenessUnknownPanel, "A", []map[string]any{value(`{host="crisp",block="g1"}`, 0.25)}},
		{proxyLivenessHitPanel, "A", []map[string]any{value(`{host="crisp",block="g1"}`, 0.75)}},
	}
	tests := []map[string]any{}
	expressionTests := []map[string]any{}
	for _, e := range evaluations {
		expressionTests = append(expressionTests, map[string]any{"expr": query(e.id, e.refId), "eval_time": "60m", "exp_samples": e.samples})
	}
	tests = append(tests, map[string]any{"name": "an hour of two proxy hosts", "interval": "1m", "input_series": input, "promql_expr_test": expressionTests})
	fixture, err := json.Marshal(map[string]any{"evaluation_interval": "1m", "tests": tests})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "proxy-setup.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("proxy setup expression evaluation: %v\n%s", err, output)
	}
}
