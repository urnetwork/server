package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// Exercise the registered route and job selection, including the embedded API.
func TestApiRouteErrorsRegistry(t *testing.T) {
	signals, err := IncludeSignals(NewSignals(), "25.3")
	if err != nil || len(signals) != 1 || signals[0].Key() != "api-route-errors" {
		t.Fatal("section 25.3 does not select its customer-route probe")
	}
}

// Two API slots and one alt slot, all serviced by the synthetic source.
func apiRouteTestScope(settings *SignalSettings) {
	settings.LogServiceHosts["api"] = []string{"by-us-fmt-5-edge-0", "by-us-fmt-5-edge-1"}
	settings.LogServiceHosts["alt"] = []string{"by-us-fmt-5-edge-0"}
	settings.LogServiceBlocks["api"] = []string{"g0"}
	settings.LogServiceBlocks["alt"] = []string{"g0"}
}

// Each route completes 100/s per slot unless mutate changes its source
// clocks or counters. One-minute snapshots span both warning windows.
func apiRouteTestResponse(t *testing.T, mutate func(job string, slot int, route, part string, values []float64)) string {
	t.Helper()
	const points = 21
	start := stormSyntheticStart(points)
	series := []stormSyntheticSeries{}
	for _, job := range []string{"api", "alt"} {
		slots := 2
		if job == "alt" {
			slots = 1
		}
		for slot := 0; slot < slots; slot++ {
			host := fmt.Sprintf("by-us-fmt-5-edge-%d", slot)
			add := func(part, name, route string, value func(i int) float64) {
				labels := map[string]string{"job": job, "host": host, "block": "g0"}
				if route != "" {
					labels["route"] = route
				}
				values := make([]float64, points)
				for i := range values {
					values[i] = value(i)
				}
				if mutate != nil {
					mutate(job, slot, name, part, values)
				}
				series = append(series, stormSyntheticSeries{part, labels, values})
			}
			add("clock", "", "", func(i int) float64 { return float64(start.Unix() + int64(60*i) - 3) })
			add("status_contract", "", "", func(int) float64 { return 1 })
			for _, item := range apiCustomerRoutes {
				add("qualified", item.name, item.route, func(int) float64 { return 1 })
				add("total", item.name, item.route, func(i int) float64 { return float64(i+1) * 6000 })
				add("completed", item.name, item.route, func(i int) float64 { return float64(i+1) * 6000 })
				add("failed", item.name, item.route, func(int) float64 { return 0 })
				add("canceled_failed", item.name, item.route, func(int) float64 { return 0 })
				add("panic_none", item.name, item.route, func(int) float64 { return 0 })
			}
		}
	}
	return stormSyntheticResponse(t, start, time.Minute, series)
}

// Threshold edges, elapsed endpoints, traffic eligibility, population weighting
// and job separation all pass through the production parser and reducer.
func TestApiRouteErrorsBands(t *testing.T) {
	for _, tc := range []struct {
		name             string
		requests, errors [2]int
		failAfter        int
		want             string
	}{
		{"healthy high traffic", [2]int{6000, 6000}, [2]int{0, 0}, 0, "healthy"},
		{"warn inclusive", [2]int{6000, 6000}, [2]int{120, 120}, 0, tierWarn},
		{"page inclusive", [2]int{6000, 6000}, [2]int{600, 600}, 0, tierPage},
		{"below warn", [2]int{6000, 6000}, [2]int{119, 119}, 0, "healthy"},
		{"October 19 route", [2]int{6000, 6000}, [2]int{606, 606}, 0, tierPage},
		{"sum then divide", [2]int{6000, 54000}, [2]int{600, 0}, 0, "healthy"},
		{"minimum mass inclusive", [2]int{30, 30}, [2]int{3, 3}, 0, tierPage},
		{"below minimum mass", [2]int{29, 30}, [2]int{29, 30}, 0, "hold"},
		{"zero completed", [2]int{0, 0}, [2]int{0, 0}, 0, "hold"},
		{"shortened page hold", [2]int{6000, 6000}, [2]int{600, 600}, 12, "hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := apiRouteTestResponse(t, func(job string, slot int, route, part string, v []float64) {
				if job != "api" || route != "network-reliability" {
					return
				}
				for i := range v {
					switch part {
					case "total", "completed":
						v[i] = float64((i + 1) * tc.requests[slot])
					case "failed":
						v[i] = float64(max(0, i+1-tc.failAfter) * tc.errors[slot])
					}
				}
			})
			states := stormFindingStates(t, apiRouteErrorsProbe{}, stormSyntheticNow, response, apiRouteTestScope)
			requireStormStates(t, tc.name, states, map[string]string{
				apiRouteErrorsClass + " main/api/network-reliability": tc.want,
				apiRouteErrorsClass + " main/alt/network-reliability": "healthy",
				apiRouteErrorsClass + " main/api/connect-control":     "healthy",
				apiRouteErrorsUnknown + " main":                       "healthy",
			})
		})
	}
}

// Missing mass, stale clocks, resets and busy non-atomic scrapes hold this
// route open while a fully observed sibling job can independently page.
func TestApiRouteErrorsCoverageAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		name, part string
		change     func([]float64)
	}{
		{"missing status mass", "total", func(v []float64) { v[18]-- }},
		{"independent summary omitted", "completed", func(v []float64) { v[18] = math.NaN() }},
		{"fractional completion", "total", func(v []float64) { v[18] += .5 }},
		{"counter reset", "failed", func(v []float64) { v[18] = 0 }},
		{"failed exceeds all", "failed", func(v []float64) { v[18] = 1e12 }},
		{"publisher missing", "clock", func(v []float64) { v[18] = math.NaN() }},
		{"publisher stale", "clock", func(v []float64) { v[18] -= 60 }},
		{"publisher future", "clock", func(v []float64) { v[18] += 10 }},
		{"mixed child family", "qualified", func(v []float64) { v[18] = math.NaN() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := apiRouteTestResponse(t, func(job string, slot int, route, part string, v []float64) {
				if route == "network-user" && part == "failed" {
					for i := range v {
						v[i] = float64((i + 1) * 600)
					}
				}
				if job == "api" && slot == 1 && (route == "network-user" || part == "clock") && part == tc.part {
					tc.change(v)
				}
			})
			states := stormFindingStates(t, apiRouteErrorsProbe{}, stormSyntheticNow, response, apiRouteTestScope)
			requireStormStates(t, tc.name, states, map[string]string{apiRouteErrorsClass + " main/api/network-user": "hold", apiRouteErrorsClass + " main/alt/network-user": tierPage, apiRouteErrorsUnknown + " main": "unobservable"})
			alerts := runStormSynthetic(t, NewApiRouteErrorsSignal(), stormSyntheticNow, response, apiRouteTestScope)
			alert := requireAlertClass(t, alerts, apiRouteErrorsClass)
			if alert.Target != "main/alt/network-user" || !strings.Contains(alert.Markdown(), "GET ^/network/user$") || !strings.Contains(alert.Observed, "share=0.1000") {
				t.Fatal("missing route-specific, job-scoped incident evidence")
			}
		})
	}
	response := strings.Replace(apiRouteTestResponse(t, nil), `"route":"GET ^/network/user$"`, `"route":"private-route-value"`, 1)
	alerts := runStormSynthetic(t, NewApiRouteErrorsSignal(), stormSyntheticNow, response, apiRouteTestScope)
	visibility := requireAlertClass(t, alerts, apiRouteErrorsUnknown)
	requireAlertOmits(t, visibility, "private-route-value")
}

// A failed source, empty placement or invalid settings cannot clear the route.
// A canceled check propagates the context without rendering its source text.
func TestApiRouteErrorsSourceBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		err        error
		reason     string
	}{
		{"transport", "", errors.New("private-transport-cause"), "source_unavailable"},
		{"partial", `{"status":"success","data":{"resultType":"matrix","result":[]},"warnings":["private-source-body"]}`, nil, "invalid_response"},
		{"empty", `{"status":"success","data":{"resultType":"matrix","result":[]}}`, nil, "route_coverage_incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return tc.body, tc.err }}
			settings := stormMainSettings(stormSyntheticNow, source)
			apiRouteTestScope(&settings)
			alerts, err := NewApiRouteErrorsSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			a := requireAlertClass(t, alerts, apiRouteErrorsUnknown)
			if !a.unobservable || !strings.Contains(a.Observed, tc.reason) {
				t.Fatal("missing visibility reason")
			}
			requireAlertOmits(t, a, "private-source-body", "private-transport-cause")
		})
	}
	for _, invalid := range []bool{true, false} {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			t.Fatal("invalid settings or missing placement contacted a source")
			return "", nil
		}}
		settings := stormMainSettings(stormSyntheticNow, source)
		policy := DefaultApiRouteErrorsSettings()
		if invalid {
			policy.MaxChildrenPerRoute = 0
		} else {
			delete(settings.LogServiceHosts, "api")
			delete(settings.LogServiceHosts, "alt")
		}
		alerts, err := NewApiRouteErrorsSignalWithSettings(policy).Run(context.Background(), settings)
		if err != nil || !requireAlertClass(t, alerts, apiRouteErrorsUnknown).unobservable {
			t.Fatal("invalid prerequisites")
		}
	}
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return "", context.Canceled }}
	settings := stormMainSettings(stormSyntheticNow, source)
	apiRouteTestScope(&settings)
	_, err := NewApiRouteErrorsSignal().Run(context.Background(), settings)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("source cancellation lost")
	}
}

// The pinned Mimir engine receives the production query and raw samples.
// Both 503/canceled and 500/panic count. None/canceled and 4xx count only
// toward completions. Other routes and jobs cannot dilute the numerator.
func TestApiRouteErrorsPromqlEngine(t *testing.T) {
	runCatalogPromqlScript(t, func() string {
		settings := DefaultApiRouteErrorsSettings()
		inventory := fleetSlotInventory{slots: []fleetSlot{{service: "api", host: "alpha", block: "g0", allowed: true}}}
		query := apiRouteErrorsQuery(inventory.selector("synthetic"), settings)
		const identity = `env="synthetic",job="api",service="api",host="alpha",block="g0",instance="aaaabb00"`
		const route = `,route="GET ^/network/user$"`
		const expectedRoute = `job="api",host="alpha",block="g0",route="GET ^/network/user$"`
		line := func(metric, labels, values string) string {
			return "  " + metric + "{" + identity + labels + "} " + values + "\n"
		}
		start := line("process_start_time_seconds", "", "60+0x20")
		ok := line(apiRouteRequestsMetric, route+`,status="200",outcome="completed"`, "20+40x20")
		canceled := line(apiRouteRequestsMetric, route+`,status="none",outcome="canceled"`, "11+3x20")
		live := line(apiRouteRequestsMetric, route+`,status="503",outcome="canceled"`, "3+2x20")
		panics := line(apiRouteRequestsMetric, route+`,status="500",outcome="panic"`, "5+1x20")
		rejected := line(apiRouteRequestsMetric, route+`,status="401",outcome="completed"`, "7+4x20")
		done := line(apiRouteCountMetric, route, "46+50x20")
		excluded := strings.ReplaceAll(ok, "GET ^/network/user$", "GET ^/hello$") +
			strings.ReplaceAll(ok, "GET ^/network/user$", "GET ^/network/provider-egress-test$") +
			strings.ReplaceAll(live, "GET ^/network/user$", "GET ^/status$") +
			strings.ReplaceAll(rejected, "GET ^/network/user$", "not_found")
		type native struct {
			name, series                               string
			clock, qualified, total, failed, completed int
		}
		healthy := start + ok + line(apiRouteCountMetric, route, "20+40x20")
		raw := start + ok + canceled + live + panics + rejected + done
		source := line("urnetwork_source_info", fmt.Sprintf(`,revision="%s",modified="false",image_digest="sha256:%s"`, settings.VerifiedStatusRevisions[0], strings.Repeat("e", 64)), "1+0x20")
		cases := []native{
			{"full typed family", raw + excluded, 1200, 1, 1046, 68, 1046},
			{"verified source", raw + source, 1200, 1, 1046, 68, 1046},
			{"modified source", raw + strings.Replace(source, `modified="false"`, `modified="true"`, 1), 1200, 1, 1046, 68, 1046},
			{"unreviewed revision", raw + strings.Replace(source, settings.VerifiedStatusRevisions[0], strings.Repeat("a", 40), 1), 1200, 1, 1046, 68, 1046},
			{"ambiguous source", raw + source + strings.Replace(source, settings.VerifiedStatusRevisions[0], strings.Repeat("a", 40), 1), 1200, 1, 1046, 68, 1046},
			{"stale source", raw + strings.Replace(source, "1+0x20", "1+0x19", 1), 1200, 1, 1046, 68, 1046},
			{"missing image", raw + strings.Replace(source, "sha256:"+strings.Repeat("e", 64), "", 1), 1200, 1, 1046, 68, 1046},
			{"never created 5xx", healthy, 1200, 1, 820, 0, 820},
			{"route absent", start + excluded, 1200, -1, -1, -1, -1},
			{"wrong service", strings.ReplaceAll(healthy, `service="api"`, `service="alt"`), -1, 1, 820, 0, 820},
			{"overlap", healthy + strings.ReplaceAll(start, "aaaabb00", "aaaabb01"), -1, 1, 820, 0, 820},
			{"unrecognized status", strings.Replace(healthy, `status="200"`, `status="bad"`, 1), 1200, -1, 820, 0, 820},
			{"unknown outcome", strings.Replace(healthy, `outcome="completed"`, `outcome="unknown"`, 1), 1200, -1, 820, 0, 820},
			{"stale error child", healthy + line(apiRouteRequestsMetric, route+`,status="503",outcome="completed"`, "1+0x19"), 1200, -1, 821, 1, 820},
			{"masked reset", healthy + line(apiRouteRequestsMetric, route+`,status="503",outcome="completed"`, "50+1x19 0"), 1200, -1, 820, 0, 820},
			{"new error child", healthy + line(apiRouteRequestsMetric, route+`,status="503",outcome="completed"`, strings.Repeat("_ ", 19)+"4 8"), 1200, 1, 828, 8, 820},
			{"stale publisher", strings.Replace(healthy, "60+0x20", "60+0x19", 1), -1, -1, 820, 0, 820},
		}
		var script strings.Builder
		for _, tc := range cases {
			fmt.Fprintf(&script, "# %s\nclear\nload 1m\n%s\neval instant at 20m %s\n", tc.name, tc.series, query)
			for _, v := range []struct {
				part  string
				value int
			}{{"clock", tc.clock}, {"qualified", tc.qualified}, {"total", tc.total}, {"failed", tc.failed}, {"completed", tc.completed}} {
				if v.value < 0 {
					continue
				}
				labels := expectedRoute
				if v.part == "clock" {
					labels = `job="api",host="alpha",block="g0"`
				}
				fmt.Fprintf(&script, "  {%s,monitor_part=%q} %d\n", labels, v.part, v.value)
			}
			if tc.total >= 0 {
				canceledFailed := 0
				if tc.failed == 68 {
					canceledFailed = 43
				}
				fmt.Fprintf(&script, "  {%s,monitor_part=%q} %d\n", expectedRoute, "canceled_failed", canceledFailed)
				fmt.Fprintf(&script, "  {%s,monitor_part=%q} 0\n", expectedRoute, "panic_none")
			}
			if tc.name == "verified source" {
				fmt.Fprintln(&script, `  {job="api",host="alpha",block="g0",monitor_part="status_contract"} 1`)
			}
			script.WriteString("\n")
		}
		// On the alt job a failing status listener and generic 404 children
		// can be present beside a healthy API route. Neither enters its share.
		altQuery := apiRouteErrorsQuery(fleetSlotInventory{slots: []fleetSlot{{service: "alt", host: "alpha", block: "g0", allowed: true}}}.selector("synthetic"), settings)
		altRaw := strings.ReplaceAll(healthy+excluded, `="api"`, `="alt"`)
		fmt.Fprintf(&script, "# alt status listener and shared 404s\nclear\nload 1m\n%s\neval instant at 20m %s\n", altRaw, altQuery)
		altRoute := strings.Replace(expectedRoute, `job="api"`, `job="alt"`, 1)
		fmt.Fprintln(&script, `  {job="alt",host="alpha",block="g0",monitor_part="clock"} 1200`)
		for _, part := range []struct {
			name  string
			value int
		}{{"qualified", 1}, {"total", 820}, {"failed", 0}, {"canceled_failed", 0}, {"panic_none", 0}, {"completed", 820}} {
			fmt.Fprintf(&script, "  {%s,monitor_part=%q} %d\n", altRoute, part.name, part.value)
		}
		return script.String()
	})
}

// Bind the single range request to both actual job inventories, the full
// 20 minutes behind the evaluation lag, and a single services gateway.
func TestApiRouteErrorsBoundedRead(t *testing.T) {
	policy := DefaultApiRouteErrorsSettings()
	inventory := fleetSlotInventory{slots: []fleetSlot{
		{service: "api", host: "by-us-fmt-5-edge-0", block: "g0", allowed: true},
		{service: "api", host: "by-us-fmt-5-edge-1", block: "g0", allowed: true},
		{service: "alt", host: "by-us-fmt-5-edge-0", block: "g0", allowed: true},
	}}
	at := stormSyntheticNow.Add(-policy.EvaluationLag)
	replay := stormReplayCase{Name: "Route error census", Now: stormSyntheticNow, Query: apiRouteErrorsQuery(inventory.selector("main"), policy),
		Start: at.Add(-20 * time.Minute).Unix(), End: at.Unix(), Step: 60, Response: []byte(apiRouteTestResponse(t, nil))}
	if alerts := runStormReplay(t, NewApiRouteErrorsSignal(), replay, apiRouteTestScope); len(alerts) != 0 {
		t.Fatal("healthy replay returned findings")
	}

	// A missing required API or an excluded API slot never borrows the
	// other job's observations. Alt may be absent from desired placement.
	for _, tc := range []struct {
		name       string
		change     func(*SignalSettings)
		visibility string
	}{
		{"missing API", func(s *SignalSettings) { delete(s.LogServiceHosts, "api") }, "unobservable"},
		{"excluded API slot", func(s *SignalSettings) {
			s.LogServiceHosts["api"] = append(s.LogServiceHosts["api"], "unconfigured-host")
		}, "unobservable"},
		{"alt unplaced", func(s *SignalSettings) { delete(s.LogServiceHosts, "alt") }, "healthy"},
	} {
		states := stormFindingStates(t, apiRouteErrorsProbe{}, stormSyntheticNow, string(replay.Response), func(s *SignalSettings) { apiRouteTestScope(s); tc.change(s) })
		requireStormStates(t, tc.name, states, map[string]string{apiRouteErrorsUnknown + " main": tc.visibility})
	}
}

// Earlier routers published a numeric 5xx for a caller who already left, or
// discarded a panic status. Unverified status semantics only affect a band
// if these two cohorts can change its verdict.
func TestApiRouteErrorsStatusContract(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		proven                      bool
		failed, canceled, panicNone int
		incident, visibility        string
	}{
		{"legacy canceled only", false, 600, 600, 0, "hold", "unobservable"},
		{"verified live caller", true, 600, 600, 0, tierPage, "healthy"},
		{"legacy other failures prove page", false, 750, 150, 0, tierPage, "healthy"},
		{"legacy uncertainty spans page", false, 660, 120, 0, tierWarn, "unobservable"},
		{"legacy entire upper bound below warn", false, 20, 20, 10, "healthy", "healthy"},
		{"legacy suppressed panic can cross warn", false, 0, 0, 120, "hold", "unobservable"},
		{"verified panic without status", true, 0, 0, 120, "healthy", "healthy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := apiRouteTestResponse(t, func(job string, slot int, route, part string, v []float64) {
				if job != "api" {
					return
				}
				if part == "status_contract" && !tc.proven {
					for i := range v {
						v[i] = math.NaN()
					}
				}
				if route != "network-user" {
					return
				}
				for i := range v {
					switch part {
					case "failed":
						v[i] = float64((i + 1) * tc.failed)
					case "canceled_failed":
						v[i] = float64((i + 1) * tc.canceled)
					case "panic_none":
						v[i] = float64((i + 1) * tc.panicNone)
					}
				}
			})
			states := stormFindingStates(t, apiRouteErrorsProbe{}, stormSyntheticNow, response, apiRouteTestScope)
			requireStormStates(t, tc.name, states, map[string]string{apiRouteErrorsClass + " main/api/network-user": tc.incident, apiRouteErrorsUnknown + " main": tc.visibility})
			if tc.visibility == "unobservable" {
				alerts := runStormSynthetic(t, NewApiRouteErrorsSignal(), stormSyntheticNow, response, apiRouteTestScope)
				if !strings.Contains(requireAlertClass(t, alerts, apiRouteErrorsUnknown).Observed, "status_contract_unverified") {
					t.Fatal("version uncertainty lost its discriminator")
				}
			}
		})
	}
}
