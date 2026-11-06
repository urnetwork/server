// The proxy test runner's dashboard. The runner (tests repo, tests/proxy-test)
// runs on planetoid and writes a Prometheus textfile, which Fluent Bit's node
// exporter collector ships to Mimir with env="main", host="planetoid" and
// job="node". The metric names, types, labels and label values are a contract
// shared with the binary and its deploy, so the dashboard reads exactly those
// metrics, on main, each deduplicated with max.
package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const proxyTestDashboardName = "proxy-test.json"

// A metric of the runner's metrics file: whether it is a counter, and its
// labels besides the env, host and job that Fluent Bit adds.
type proxyTestMetric struct {
	counter bool
	labels  []string
}

// The contract's metrics file table.
var proxyTestMetrics = map[string]proxyTestMetric{
	"urnetwork_proxy_test_up":                             {},
	"urnetwork_proxy_test_heartbeat_timestamp_seconds":    {},
	"urnetwork_proxy_test_build_info":                     {labels: []string{"version"}},
	"urnetwork_proxy_test_countries":                      {},
	"urnetwork_proxy_test_runs_total":                     {counter: true, labels: []string{"country", "outcome"}},
	"urnetwork_proxy_test_phase_seconds_total":            {counter: true, labels: []string{"country", "phase"}},
	"urnetwork_proxy_test_phase_samples_total":            {counter: true, labels: []string{"country", "phase"}},
	"urnetwork_proxy_test_phase_seconds_last":             {labels: []string{"country", "phase"}},
	"urnetwork_proxy_test_phase_seconds_window":           {labels: []string{"country", "phase", "stat"}},
	"urnetwork_proxy_test_all_phase_seconds_window":       {labels: []string{"phase", "stat"}},
	"urnetwork_proxy_test_last_run_timestamp_seconds":     {labels: []string{"country"}},
	"urnetwork_proxy_test_last_success_timestamp_seconds": {labels: []string{"country"}},
	"urnetwork_proxy_test_exit_country_mismatch_total":    {counter: true, labels: []string{"country"}},
	"urnetwork_proxy_test_remove_failures_total":          {counter: true},
	"urnetwork_proxy_test_api_errors_total":               {counter: true, labels: []string{"call"}},
}

// The values of the contract's closed labels. country is open, so the
// dashboard selects it only through the country variable, and version is
// never matched.
var proxyTestLabelValues = map[string][]string{
	"outcome": {"ok", "create_error", "ready_timeout", "browser_error", "navigate_timeout", "http_error"},
	"phase":   {"create", "setup", "connect", "load"},
	"call":    {"login", "locations", "create", "remove", "list"},
}

// stat takes different values in the per-country and the all-country windows.
var proxyTestStatValues = map[string][]string{
	"urnetwork_proxy_test_phase_seconds_window":     {"p50", "p90", "max"},
	"urnetwork_proxy_test_all_phase_seconds_window": {"p50", "p90", "p99", "max"},
}

// The dashboard's panels.
const (
	proxyTestUpPanel             = 2
	proxyTestHeartbeatPanel      = 3
	proxyTestVersionPanel        = 4
	proxyTestRemoveFailuresPanel = 5
	proxyTestApiErrorsPanel      = 6
	proxyTestSetupP50Panel       = 8
	proxyTestSetupP90Panel       = 9
	proxyTestConnectP50Panel     = 10
	proxyTestConnectP90Panel     = 11
	proxyTestSuccessPanel        = 12
	proxyTestCountriesPanel      = 14
	proxyTestTestedPanel         = 15
	proxyTestCoveragePanel       = 16
	proxyTestPercentilesPanel    = 18
	proxyTestOutcomesPanel       = 19
	proxyTestMeanPanel           = 20
	proxyTestApiErrorTrendPanel  = 21
	proxyTestCountryTablePanel   = 23
	proxyTestSlowestPanel        = 24
	proxyTestFailureRatePanel    = 25
	proxyTestLatestRunPanel      = 26
)

// The panels that follow the country variable, and those over every country.
var proxyTestCountryPanels = []int{proxyTestOutcomesPanel, proxyTestMeanPanel, proxyTestCountryTablePanel, proxyTestSlowestPanel, proxyTestFailureRatePanel, proxyTestLatestRunPanel}
var proxyTestAllCountryPanels = []int{proxyTestSetupP50Panel, proxyTestSetupP90Panel, proxyTestConnectP50Panel, proxyTestConnectP90Panel, proxyTestSuccessPanel, proxyTestCountriesPanel, proxyTestTestedPanel, proxyTestCoveragePanel, proxyTestPercentilesPanel}

// The fields of the document that testDashboard does not read.
type proxyTestDatasource struct {
	Type string `json:"type"`
	Uid  string `json:"uid"`
}

type proxyTestDocument struct {
	Templating struct {
		List []struct {
			Name       string               `json:"name"`
			Type       string               `json:"type"`
			Datasource *proxyTestDatasource `json:"datasource"`
			Query      string               `json:"query"`
			Definition string               `json:"definition"`
			Multi      bool                 `json:"multi"`
			IncludeAll bool                 `json:"includeAll"`
			AllValue   string               `json:"allValue"`
		} `json:"list"`
	} `json:"templating"`
	Panels []struct {
		Id          int                  `json:"id"`
		Type        string               `json:"type"`
		Datasource  *proxyTestDatasource `json:"datasource"`
		FieldConfig struct {
			Defaults struct {
				Mappings []struct {
					Type    string                     `json:"type"`
					Options map[string]json.RawMessage `json:"options"`
				} `json:"mappings"`
			} `json:"defaults"`
		} `json:"fieldConfig"`
		Options struct {
			SortBy []struct {
				DisplayName string `json:"displayName"`
				Desc        bool   `json:"desc"`
			} `json:"sortBy"`
		} `json:"options"`
		Targets []struct {
			RefId      string               `json:"refId"`
			Datasource *proxyTestDatasource `json:"datasource"`
		} `json:"targets"`
	} `json:"panels"`
}

func readProxyTestDocument(t *testing.T) proxyTestDocument {
	t.Helper()
	body, err := dashboardsFs.ReadFile("dashboards/" + proxyTestDashboardName)
	if err != nil {
		t.Fatal(err)
	}
	var document proxyTestDocument
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("parse %s: %v", proxyTestDashboardName, err)
	}
	return document
}

// The contract's query for the country variable.
const proxyTestCountryVariableQuery = `label_values(urnetwork_proxy_test_runs_total{env="main"}, country)`

// The series selector of a label_values() variable query.
var proxyTestLabelValuesPattern = regexp.MustCompile(`^label_values\((.*), [a-z_]+\)$`)

// The series selectors of the dashboard's variables.
func proxyTestVariableSelectors(t *testing.T) []string {
	t.Helper()
	selectors := []string{}
	for _, variable := range readProxyTestDocument(t).Templating.List {
		match := proxyTestLabelValuesPattern.FindStringSubmatch(variable.Query)
		if match == nil {
			t.Errorf("variable %s is not a label_values() query: %s", variable.Name, variable.Query)
			continue
		}
		selectors = append(selectors, match[1])
	}
	return selectors
}

// Every query of the dashboard: the panel expressions, then the variables'
// series selectors.
func proxyTestQueries(t *testing.T) []string {
	t.Helper()
	return append(dashboardDocumentExpressions(t, proxyTestDashboardName), proxyTestVariableSelectors(t)...)
}

func proxyTestTarget(t *testing.T, dashboard testDashboard, id int, refId string) testTarget {
	t.Helper()
	panel := dashboardPanelById(dashboard, id)
	if panel == nil {
		t.Fatalf("proxy test panel %d is missing", id)
	}
	for _, target := range panel.Targets {
		if target.RefId == refId {
			return target
		}
	}
	t.Fatalf("proxy test panel %d (%s) has no query %s", id, panel.Title, refId)
	return testTarget{}
}

func TestProxyTestDashboardIdentity(t *testing.T) {
	dashboard := readTestDashboard(t, proxyTestDashboardName)
	if dashboard.Title != "urnetwork / proxy test" || dashboard.Uid != "urnetwork-proxy-test" {
		t.Errorf("title %q, uid %q", dashboard.Title, dashboard.Uid)
	}
	if !slices.Equal(dashboard.Tags, []string{"urnetwork", "proxy", "planetoid"}) {
		t.Errorf("tags = %q", dashboard.Tags)
	}
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Error("the proxy test dashboard must stay internal")
	}

	document := readProxyTestDocument(t)
	mimir := proxyTestDatasource{Type: "prometheus", Uid: "warp-mimir"}
	if len(document.Templating.List) != 1 {
		t.Fatalf("the dashboard has %d variables, want only country", len(document.Templating.List))
	}
	variable := document.Templating.List[0]
	if variable.Name != "country" || variable.Type != "query" || variable.Query != proxyTestCountryVariableQuery ||
		variable.Definition != proxyTestCountryVariableQuery || !variable.Multi || !variable.IncludeAll || variable.AllValue != ".*" ||
		variable.Datasource == nil || *variable.Datasource != mimir {
		t.Errorf("country variable must be a multi-select with All from %s: %+v", proxyTestCountryVariableQuery, variable)
	}

	for index, panel := range document.Panels {
		if panel.Type == "row" {
			continue
		}
		if panel.Datasource == nil || *panel.Datasource != mimir {
			t.Errorf("panel %d does not read warp-mimir", panel.Id)
		}
		if len(panel.Targets) == 0 {
			t.Errorf("panel %d has no query", panel.Id)
		}
		for _, target := range panel.Targets {
			if target.Datasource == nil || *target.Datasource != mimir {
				t.Errorf("panel %d query %s does not read warp-mimir", panel.Id, target.RefId)
			}
		}
		if dashboard.Panels[index].Description == "" {
			t.Errorf("panel %d (%s) says nothing of how to read it", panel.Id, dashboard.Panels[index].Title)
		}
	}
}

// Every metric of the contract is on a panel, and no query reads anything
// else.
func TestProxyTestDashboardReadsEveryContractMetric(t *testing.T) {
	dashboard := readTestDashboard(t, proxyTestDashboardName)
	joined := strings.Join(dashboardExpressions(dashboard), "\n")
	for metric := range proxyTestMetrics {
		if len(metricOccurrences(joined, metric)) == 0 {
			t.Errorf("no panel reads %s", metric)
		}
	}
	selector := regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_:]*)\{`)
	for _, query := range proxyTestQueries(t) {
		for _, metric := range metricNamePattern.FindAllString(query, -1) {
			if _, ok := proxyTestMetrics[metric]; !ok {
				t.Errorf("query reads %s, which is not in the runner's metrics file: %s", metric, query)
			}
		}
		for _, match := range selector.FindAllStringSubmatch(query, -1) {
			if _, ok := proxyTestMetrics[match[1]]; !ok {
				t.Errorf("query selects %s, which is not in the runner's metrics file: %s", match[1], query)
			}
		}
	}
}

// A metric read: the metric, its label matchers (label, operator, value) and
// the query text before it.
type proxyTestRead struct {
	metric   string
	matchers [][3]string
	before   string
}

var proxyTestSelectorPattern = regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_:]*)\{([^{}]*)\}`)
var proxyTestMatcherPattern = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)(=~|!~|!=|=)"([^"\\]*)"$`)

// proxyTestReads returns the metric reads of a query, and fails a query that
// names a metric without a selector or a selector without a metric.
func proxyTestReads(t *testing.T, query string) []proxyTestRead {
	t.Helper()
	reads := []proxyTestRead{}
	for _, match := range proxyTestSelectorPattern.FindAllStringSubmatchIndex(query, -1) {
		read := proxyTestRead{metric: query[match[2]:match[3]], before: query[:match[2]]}
		for _, matcher := range strings.Split(query[match[4]:match[5]], ",") {
			parts := proxyTestMatcherPattern.FindStringSubmatch(strings.TrimSpace(matcher))
			if parts == nil {
				t.Errorf("unreadable label matcher %q: %s", matcher, query)
				continue
			}
			read.matchers = append(read.matchers, [3]string{parts[1], parts[2], parts[3]})
		}
		reads = append(reads, read)
	}
	if len(reads) != strings.Count(query, "{") {
		t.Errorf("query has a selector without a metric name: %s", query)
	}
	for _, index := range metricNamePattern.FindAllStringIndex(query, -1) {
		if !strings.HasPrefix(query[index[1]:], "{") {
			t.Errorf("query reads %s without a selector: %s", query[index[0]:index[1]], query)
		}
	}
	return reads
}

// Every read filters env="main", and only main: the runner's series reach
// Mimir only through planetoid's Fluent Bit, which adds that label.
func TestProxyTestDashboardQueriesFilterMain(t *testing.T) {
	queries := proxyTestQueries(t)
	if len(queries) == 0 {
		t.Fatal("the proxy test dashboard has no queries")
	}
	for _, query := range queries {
		reads := proxyTestReads(t, query)
		if len(reads) == 0 {
			t.Errorf("query reads no metric: %s", query)
		}
		for _, read := range reads {
			envs := 0
			for _, matcher := range read.matchers {
				if matcher[0] == "env" {
					envs++
					if matcher[1] != "=" || matcher[2] != "main" {
						t.Errorf("%s is read with env%s%q, not env=\"main\": %s", read.metric, matcher[1], matcher[2], query)
					}
				}
			}
			if envs != 1 {
				t.Errorf("%s is read without exactly one env=\"main\": %s", read.metric, query)
			}
		}
	}
}

// max(, or max by (labels) (, directly before a read; clamp_max( is not one.
var proxyTestMaxPattern = regexp.MustCompile(`(?:^|[^a-z_])max(?: by \(([a-z_, ]*)\) \(|\()$`)
var proxyTestByPattern = regexp.MustCompile(`\bby \(([^)]*)\)`)
var proxyTestVariablePattern = regexp.MustCompile(`\$[a-zA-Z_]+`)

// Each read matches only the contract's labels and values, reads a counter
// through increase() or rate() and a gauge directly or over time, and sits
// directly in a max whose grouping keeps every label the read does not pin.
// So a duplicate scrape or a second host never double-counts, a restart's
// counter reset is not a drop, and no max merges two of the runner's series.
// A variable's series selector is only held to the labels and values.
func TestProxyTestDashboardReadsFollowTheContract(t *testing.T) {
	variableSelectors := proxyTestVariableSelectors(t)
	if len(variableSelectors) != 1 {
		t.Fatalf("variable selectors = %q, want the country variable's", variableSelectors)
	}
	for _, query := range proxyTestQueries(t) {
		for _, variable := range proxyTestVariablePattern.FindAllString(query, -1) {
			if variable != "$country" {
				t.Errorf("query uses %s; the dashboard defines only $country: %s", variable, query)
			}
		}
		if strings.Contains(query, "without") {
			t.Errorf("query aggregates without labels, which keeps host: %s", query)
		}
		for _, by := range proxyTestByPattern.FindAllStringSubmatch(query, -1) {
			for _, label := range strings.Split(by[1], ",") {
				if label = strings.TrimSpace(label); !slices.Contains([]string{"country", "outcome", "phase", "stat", "call", "version"}, label) {
					t.Errorf("query groups by %q, which is not a runner label: %s", label, query)
				}
			}
		}
		variableSelector := slices.Contains(variableSelectors, query)

		for _, read := range proxyTestReads(t, query) {
			metric, ok := proxyTestMetrics[read.metric]
			if !ok {
				continue
			}
			pinned := map[string]bool{}
			seen := map[string]bool{}
			for _, matcher := range read.matchers {
				label, op, value := matcher[0], matcher[1], matcher[2]
				if seen[label] {
					t.Errorf("%s repeats its %s matcher: %s", read.metric, label, query)
				}
				seen[label] = true
				switch {
				case label == "env":
				case label == "country":
					if op != "=~" || value != "$country" {
						t.Errorf("%s selects country%s%q; countries are selected only with country=~\"$country\": %s", read.metric, op, value, query)
					}
				case slices.Contains(metric.labels, label):
					allowed := proxyTestLabelValues[label]
					if label == "stat" {
						allowed = proxyTestStatValues[read.metric]
					}
					values := []string{value}
					if op == "=~" || op == "!~" {
						values = strings.Split(value, "|")
					}
					for _, value := range values {
						if !slices.Contains(allowed, value) {
							t.Errorf("%s matches %s value %q, which the runner never writes: %s", read.metric, label, value, query)
						}
					}
					if op == "=" {
						pinned[label] = true
					}
				default:
					t.Errorf("%s matches %s, which is not one of its labels: %s", read.metric, label, query)
				}
			}
			if variableSelector {
				continue
			}

			before := read.before
			function := ""
			for _, candidate := range []string{"increase(", "rate(", "max_over_time(", "last_over_time("} {
				if strings.HasSuffix(before, candidate) {
					function, before = candidate, strings.TrimSuffix(before, candidate)
					break
				}
			}
			counterFunction := function == "increase(" || function == "rate("
			if metric.counter && !counterFunction {
				t.Errorf("the counter %s is read without increase() or rate(), so a runner restart reads as a drop: %s", read.metric, query)
			}
			if !metric.counter && counterFunction {
				t.Errorf("the gauge %s is read through %s), which is for counters: %s", read.metric, function, query)
			}

			aggregation := proxyTestMaxPattern.FindStringSubmatch(before)
			if aggregation == nil {
				t.Errorf("%s is not read directly inside max, so a duplicate scrape or a second host would double-count: %s", read.metric, query)
				continue
			}
			grouping := []string{}
			if aggregation[1] != "" {
				for _, label := range strings.Split(aggregation[1], ",") {
					grouping = append(grouping, strings.TrimSpace(label))
				}
			}
			for _, label := range metric.labels {
				if !pinned[label] && !slices.Contains(grouping, label) {
					t.Errorf("the max over %s drops %s, so it merges distinct series and not only duplicates: %s", read.metric, label, query)
				}
			}
			for _, label := range grouping {
				if !slices.Contains(metric.labels, label) {
					t.Errorf("the max over %s groups by %s, which is not one of its labels: %s", read.metric, label, query)
				}
			}
		}
	}
}

// The contract's panels: runner health, the last hour over every country,
// coverage, the trends, the per-country table and the two bar gauges.
func TestProxyTestDashboardPanels(t *testing.T) {
	dashboard := readTestDashboard(t, proxyTestDashboardName)
	document := readProxyTestDocument(t)
	panel := func(id int, kind string, unit string) *testPanel {
		t.Helper()
		found := dashboardPanelById(dashboard, id)
		if found == nil {
			t.Fatalf("proxy test panel %d is missing", id)
		}
		if found.Type != kind || found.FieldConfig.Defaults.Unit != unit {
			t.Errorf("panel %d (%s) is a %s in %q, want a %s in %q", id, found.Title, found.Type, found.FieldConfig.Defaults.Unit, kind, unit)
		}
		for _, target := range found.Targets {
			instant := target.Instant && target.Range != nil && !*target.Range
			if (kind == "timeseries") == instant {
				t.Errorf("panel %d (%s) query %s: instant=%v", id, found.Title, target.RefId, instant)
			}
		}
		return found
	}
	expect := func(id int, refId string, want string) {
		t.Helper()
		if got := proxyTestTarget(t, dashboard, id, refId).Expr; got != want {
			t.Errorf("panel %d query %s = %s, want %s", id, refId, got, want)
		}
	}
	contains := func(id int, refId string, parts ...string) {
		t.Helper()
		got := proxyTestTarget(t, dashboard, id, refId).Expr
		for _, part := range parts {
			if !strings.Contains(got, part) {
				t.Errorf("panel %d query %s lacks %s: %s", id, refId, part, got)
			}
		}
	}

	// runner health: UP needs a fresh heartbeat, and the heartbeat age is red
	// from 120 s
	panel(proxyTestUpPanel, "stat", "none")
	expect(proxyTestUpPanel, "A", `max(urnetwork_proxy_test_up{env="main"}) * (1 + (time() - max(urnetwork_proxy_test_heartbeat_timestamp_seconds{env="main"}) > bool 120))`)
	states := map[string]string{}
	for _, documentPanel := range document.Panels {
		if documentPanel.Id != proxyTestUpPanel {
			continue
		}
		for _, mapping := range documentPanel.FieldConfig.Defaults.Mappings {
			for value, raw := range mapping.Options {
				var result struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				states[value] = result.Text
			}
		}
	}
	if !maps.Equal(states, map[string]string{"0": "DOWN", "1": "UP", "2": "STALE"}) {
		t.Errorf("runner up states = %v", states)
	}
	heartbeat := panel(proxyTestHeartbeatPanel, "stat", "s")
	expect(proxyTestHeartbeatPanel, "A", `time() - max(urnetwork_proxy_test_heartbeat_timestamp_seconds{env="main"})`)
	firstRed := -1.0
	for _, step := range heartbeat.FieldConfig.Defaults.Thresholds.Steps {
		if step.Color == "red" && step.Value != nil {
			firstRed = *step.Value
			break
		}
	}
	if firstRed != 120 {
		t.Errorf("heartbeat age must turn red at 120 s, turns red at %v", firstRed)
	}
	panel(proxyTestVersionPanel, "stat", "none")
	expect(proxyTestVersionPanel, "A", `max by (version) (urnetwork_proxy_test_build_info{env="main"})`)
	if target := proxyTestTarget(t, dashboard, proxyTestVersionPanel, "A"); target.LegendFormat != "{{version}}" {
		t.Errorf("runner version must show the version label, shows %q", target.LegendFormat)
	}
	panel(proxyTestRemoveFailuresPanel, "stat", "short")
	contains(proxyTestRemoveFailuresPanel, "A", `increase(urnetwork_proxy_test_remove_failures_total{env="main"}[24h])`)
	panel(proxyTestApiErrorsPanel, "stat", "short")
	contains(proxyTestApiErrorsPanel, "A", `increase(urnetwork_proxy_test_api_errors_total{env="main"}[24h])`)

	// the last hour over every country
	for id, window := range map[int]string{
		proxyTestSetupP50Panel:   `phase="setup",stat="p50"`,
		proxyTestSetupP90Panel:   `phase="setup",stat="p90"`,
		proxyTestConnectP50Panel: `phase="connect",stat="p50"`,
		proxyTestConnectP90Panel: `phase="connect",stat="p90"`,
	} {
		panel(id, "stat", "s")
		expect(id, "A", `max(urnetwork_proxy_test_all_phase_seconds_window{env="main",`+window+`})`)
	}
	panel(proxyTestSuccessPanel, "stat", "percentunit")
	expect(proxyTestSuccessPanel, "A", `(sum(max by (country, outcome) (increase(urnetwork_proxy_test_runs_total{env="main",outcome="ok"}[1h]))) or vector(0)) / (sum(max by (country, outcome) (increase(urnetwork_proxy_test_runs_total{env="main"}[1h]))) > 0)`)

	// coverage
	panel(proxyTestCountriesPanel, "stat", "short")
	expect(proxyTestCountriesPanel, "A", `max(urnetwork_proxy_test_countries{env="main"})`)
	tested := `count(max by (country) (max_over_time(urnetwork_proxy_test_last_run_timestamp_seconds{env="main"}[24h])) > time() - 86400)`
	panel(proxyTestTestedPanel, "stat", "short")
	contains(proxyTestTestedPanel, "A", tested)
	panel(proxyTestCoveragePanel, "stat", "percentunit")
	contains(proxyTestCoveragePanel, "A", tested, `max(urnetwork_proxy_test_countries{env="main"})`)

	// the trends
	panel(proxyTestPercentilesPanel, "timeseries", "s")
	expect(proxyTestPercentilesPanel, "A", `max by (phase, stat) (urnetwork_proxy_test_all_phase_seconds_window{env="main",phase=~"setup|connect|load",stat=~"p50|p90"})`)
	panel(proxyTestOutcomesPanel, "timeseries", "short")
	expect(proxyTestOutcomesPanel, "A", `sum by (outcome) (max by (country, outcome) (increase(urnetwork_proxy_test_runs_total{env="main",country=~"$country"}[1h])))`)
	panel(proxyTestMeanPanel, "timeseries", "s")
	expect(proxyTestMeanPanel, "A", `max by (country, phase) (increase(urnetwork_proxy_test_phase_seconds_total{env="main",country=~"$country",phase=~"setup|connect"}[1h])) / (max by (country, phase) (increase(urnetwork_proxy_test_phase_samples_total{env="main",country=~"$country",phase=~"setup|connect"}[1h])) > 0)`)
	panel(proxyTestApiErrorTrendPanel, "timeseries", "short")
	expect(proxyTestApiErrorTrendPanel, "A", `max by (call) (increase(urnetwork_proxy_test_api_errors_total{env="main"}[1h]))`)

	// the per-country table: 24 h p50 and p90 of setup, connect and load, runs,
	// failures and success, last tested and exit mismatches, sorted by setup
	// p90, slowest first
	table := panel(proxyTestCountryTablePanel, "table", "")
	columns := map[string]string{}
	for _, override := range table.FieldConfig.Overrides {
		for _, property := range override.Properties {
			if property.Id == "displayName" {
				columns[strings.Trim(string(property.Value), `"`)] = strings.TrimPrefix(override.Matcher.Options, "Value #")
			}
		}
	}
	wantColumns := map[string]string{}
	for refId, column := range map[string]struct{ name, part string }{
		"A": {"setup p50", `phase="setup",stat="p50"`},
		"B": {"setup p90", `phase="setup",stat="p90"`},
		"C": {"connect p50", `phase="connect",stat="p50"`},
		"D": {"connect p90", `phase="connect",stat="p90"`},
		"E": {"load p50", `phase="load",stat="p50"`},
		"F": {"load p90", `phase="load",stat="p90"`},
		"G": {"runs", `urnetwork_proxy_test_runs_total{env="main",country=~"$country"}[24h]`},
		"H": {"failures", `outcome!="ok"}[24h]`},
		"I": {"success", `outcome="ok"}[24h]`},
		"J": {"last tested", `urnetwork_proxy_test_last_run_timestamp_seconds{env="main",country=~"$country"}`},
		"K": {"exit mismatches", `increase(urnetwork_proxy_test_exit_country_mismatch_total{env="main",country=~"$country"}[24h])`},
	} {
		wantColumns[column.name] = refId
		contains(proxyTestCountryTablePanel, refId, column.part)
		if target := proxyTestTarget(t, dashboard, proxyTestCountryTablePanel, refId); target.Format != "table" {
			t.Errorf("table query %s is not in table format", refId)
		}
		if refId <= "F" {
			contains(proxyTestCountryTablePanel, refId, "urnetwork_proxy_test_phase_seconds_window{")
		}
	}
	if !maps.Equal(columns, wantColumns) || len(table.Targets) != len(wantColumns) {
		t.Errorf("per-country table columns = %v, want %v", columns, wantColumns)
	}
	for _, documentPanel := range document.Panels {
		if documentPanel.Id == proxyTestCountryTablePanel {
			if len(documentPanel.Options.SortBy) != 1 || documentPanel.Options.SortBy[0].DisplayName != "setup p90" || !documentPanel.Options.SortBy[0].Desc {
				t.Errorf("per-country table must sort by setup p90, slowest first: %+v", documentPanel.Options.SortBy)
			}
		}
	}

	// the bar gauges: the slowest 20 countries by setup p90, and failure rate
	// by country over 24 h
	panel(proxyTestSlowestPanel, "bargauge", "s")
	expect(proxyTestSlowestPanel, "A", `sort_desc(topk(20, max by (country) (urnetwork_proxy_test_phase_seconds_window{env="main",country=~"$country",phase="setup",stat="p90"})))`)
	panel(proxyTestFailureRatePanel, "bargauge", "percentunit")
	contains(proxyTestFailureRatePanel, "A", "topk(20, ", `outcome!="ok"}[24h]`, `urnetwork_proxy_test_runs_total{env="main",country=~"$country"}[24h]`)

	// the newest values per country
	panel(proxyTestLatestRunPanel, "table", "")
	contains(proxyTestLatestRunPanel, "B", "urnetwork_proxy_test_last_success_timestamp_seconds{")
	for refId, phase := range map[string]string{"C": "create", "D": "setup", "E": "connect", "F": "load"} {
		contains(proxyTestLatestRunPanel, refId, `urnetwork_proxy_test_phase_seconds_last{env="main",country=~"$country",phase="`+phase+`"}`)
	}

	// the country variable drives the per-country panels only
	for _, id := range proxyTestCountryPanels {
		for _, target := range dashboardPanelById(dashboard, id).Targets {
			if !strings.Contains(target.Expr, `country=~"$country"`) {
				t.Errorf("per-country panel %d query %s ignores the country variable: %s", id, target.RefId, target.Expr)
			}
		}
	}
	for _, id := range proxyTestAllCountryPanels {
		for _, target := range dashboardPanelById(dashboard, id).Targets {
			if strings.Contains(target.Expr, "$country") {
				t.Errorf("all-country panel %d query %s follows the country variable: %s", id, target.RefId, target.Expr)
			}
		}
	}
}

// The pinned promtool, found as the other dashboard tests find it.
func proxyTestPromtool(t *testing.T) (string, context.Context) {
	t.Helper()
	promtool := os.Getenv("URNETWORK_PROMTOOL")
	if promtool == "" {
		var err error
		promtool, err = exec.LookPath("promtool")
		if err != nil {
			t.Fatal("official promtool 3.5.0 is required; run bash ./grafana/promtool.sh from the server root and export its path as URNETWORK_PROMTOOL")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	t.Cleanup(cancel)
	version, err := exec.CommandContext(ctx, promtool, "--version").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(version), "promtool, version 3.5.0 ") {
		t.Fatal("official promtool 3.5.0 is required")
	}
	return promtool, ctx
}

// A query as Mimir receives it with every country selected.
func proxyTestConcrete(query string) string {
	return strings.ReplaceAll(query, `"$country"`, `".*"`)
}

// promtool parses every query, the country variable's selector included.
func TestProxyTestDashboardPromqlParses(t *testing.T) {
	promtool, ctx := proxyTestPromtool(t)
	rules := []map[string]string{}
	for index, query := range proxyTestQueries(t) {
		rules = append(rules, map[string]string{"record": fmt.Sprintf("proxy_test_dashboard_query_%d", index), "expr": proxyTestConcrete(query)})
	}
	// Json is also valid yaml; its quoting preserves the exact query.
	fixture, err := json.Marshal(map[string]any{"groups": []map[string]any{{"name": "proxy-test-dashboard", "rules": rules}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "proxy-test-queries.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(ctx, promtool, "check", "rules", path).CombinedOutput()
	if err != nil {
		t.Fatalf("proxy test queries do not parse: %v\n%s", err, output)
	}
	if want := fmt.Sprintf("SUCCESS: %d rules found", len(rules)); !strings.Contains(string(output), want) {
		t.Fatalf("promtool did not check every query (%s):\n%s", want, output)
	}
}

// promtool evaluates the panels over a synthetic metrics file 25 hours into a
// runner's life, with a copy of some series from a second host, a runner
// restart and another environment's series, then over a crashed runner, a
// stopped runner and no telemetry from main.
func TestProxyTestDashboardPromqlEvaluatesTheRunnerFile(t *testing.T) {
	promtool, ctx := proxyTestPromtool(t)
	dashboard := readTestDashboard(t, proxyTestDashboardName)
	query := func(id int, refId string) string {
		return proxyTestConcrete(proxyTestTarget(t, dashboard, id, refId).Expr)
	}

	const (
		up             = "urnetwork_proxy_test_up"
		heartbeat      = "urnetwork_proxy_test_heartbeat_timestamp_seconds"
		buildInfo      = "urnetwork_proxy_test_build_info"
		countries      = "urnetwork_proxy_test_countries"
		runs           = "urnetwork_proxy_test_runs_total"
		phaseSeconds   = "urnetwork_proxy_test_phase_seconds_total"
		phaseSamples   = "urnetwork_proxy_test_phase_samples_total"
		phaseLast      = "urnetwork_proxy_test_phase_seconds_last"
		phaseWindow    = "urnetwork_proxy_test_phase_seconds_window"
		allWindow      = "urnetwork_proxy_test_all_phase_seconds_window"
		lastRun        = "urnetwork_proxy_test_last_run_timestamp_seconds"
		lastSuccess    = "urnetwork_proxy_test_last_success_timestamp_seconds"
		mismatch       = "urnetwork_proxy_test_exit_country_mismatch_total"
		removeFailures = "urnetwork_proxy_test_remove_failures_total"
		apiErrors      = "urnetwork_proxy_test_api_errors_total"
	)
	// One sample a minute from 0 to 25 h, evaluated at 25 h (time() = 90000).
	file := func(env string, host string) func(metric, labels, values string) map[string]string {
		return func(metric, labels, values string) map[string]string {
			return map[string]string{"series": fmt.Sprintf(`%s{env=%q,host=%q,job="node"%s}`, metric, env, host, labels), "values": values}
		}
	}
	planetoid := file("main", "planetoid")
	// the same series again, as a duplicate scrape or a second host writes it
	copied := file("main", "planetoid-copy")
	other := file("other", "planetoid")
	constant := func(value string) string { return value + "+0x1500" }

	value := func(v float64) []map[string]any { return []map[string]any{{"labels": "{}", "value": v}} }
	by := func(label string, values map[string]float64) []map[string]any {
		samples := []map[string]any{}
		for key, v := range values {
			samples = append(samples, map[string]any{"labels": fmt.Sprintf("{%s=%q}", label, key), "value": v})
		}
		return samples
	}
	none := []map[string]any{}
	type evaluation struct {
		id      int
		refId   string
		samples []map[string]any
	}

	day := []map[string]string{
		planetoid(up, "", constant("1")),
		planetoid(heartbeat, "", "0+60x1500"),
		planetoid(buildInfo, `,version="2026.11.06-1"`, constant("1")),
		planetoid(countries, "", constant("5")),
		copied(countries, "", constant("5")),
		planetoid(removeFailures, "", constant("0")),
		planetoid(apiErrors, `,call="create"`, "0+1x1500"),
		copied(apiErrors, `,call="create"`, "0+1x1500"),
		planetoid(apiErrors, `,call="login"`, constant("0")),

		planetoid(allWindow, `,phase="setup",stat="p50"`, constant("2.5")),
		copied(allWindow, `,phase="setup",stat="p50"`, constant("2")),
		planetoid(allWindow, `,phase="setup",stat="p90"`, constant("6")),
		planetoid(allWindow, `,phase="setup",stat="p99"`, constant("9")),
		planetoid(allWindow, `,phase="connect",stat="p50"`, constant("0.75")),
		planetoid(allWindow, `,phase="connect",stat="p90"`, constant("1.5")),
		planetoid(allWindow, `,phase="load",stat="p50"`, constant("2")),
		planetoid(allWindow, `,phase="load",stat="p90"`, constant("4")),
		planetoid(allWindow, `,phase="create",stat="p50"`, constant("0.5")),
		other(allWindow, `,phase="setup",stat="p50"`, constant("99")),

		// de: 3 ok and 1 http_error a minute, ok also from the second host;
		// fr: only create errors; jp: only ok; us: 1 ok a minute across a
		// runner restart 30 minutes before the end
		planetoid(runs, `,country="de",outcome="ok"`, "0+3x1500"),
		copied(runs, `,country="de",outcome="ok"`, "0+3x1500"),
		planetoid(runs, `,country="de",outcome="http_error"`, "0+1x1500"),
		planetoid(runs, `,country="fr",outcome="create_error"`, "0+1x1500"),
		planetoid(runs, `,country="jp",outcome="ok"`, "0+2x1500"),
		planetoid(runs, `,country="us",outcome="ok"`, "0+1x1470 1+1x29"),
		other(runs, `,country="de",outcome="ok"`, "0+100x1500"),
		planetoid(mismatch, `,country="de"`, "0+1x1500"),

		// de: a 6 s setup and a 1 s connect every 2 minutes, also from the
		// second host; it: samples long ago and none in the last hour
		planetoid(phaseSeconds, `,country="de",phase="setup"`, "0+6x1500"),
		copied(phaseSeconds, `,country="de",phase="setup"`, "0+6x1500"),
		planetoid(phaseSamples, `,country="de",phase="setup"`, "0+2x1500"),
		copied(phaseSamples, `,country="de",phase="setup"`, "0+2x1500"),
		planetoid(phaseSeconds, `,country="de",phase="connect"`, "0+1x1500"),
		planetoid(phaseSamples, `,country="de",phase="connect"`, "0+2x1500"),
		planetoid(phaseSeconds, `,country="de",phase="load"`, "0+10x1500"),
		planetoid(phaseSamples, `,country="de",phase="load"`, "0+2x1500"),
		planetoid(phaseSeconds, `,country="it",phase="setup"`, constant("20")),
		planetoid(phaseSamples, `,country="it",phase="setup"`, constant("5")),

		planetoid(phaseWindow, `,country="de",phase="setup",stat="p50"`, constant("3")),
		planetoid(phaseWindow, `,country="de",phase="setup",stat="p90"`, constant("4.5")),
		copied(phaseWindow, `,country="de",phase="setup",stat="p90"`, constant("4")),
		planetoid(phaseWindow, `,country="de",phase="setup",stat="max"`, constant("8")),
		planetoid(phaseWindow, `,country="de",phase="connect",stat="p50"`, constant("1")),
		planetoid(phaseWindow, `,country="de",phase="connect",stat="p90"`, constant("2")),
		planetoid(phaseWindow, `,country="de",phase="load",stat="p50"`, constant("3")),
		planetoid(phaseWindow, `,country="de",phase="load",stat="p90"`, constant("5")),
		planetoid(phaseWindow, `,country="jp",phase="setup",stat="p50"`, constant("1")),
		planetoid(phaseWindow, `,country="jp",phase="setup",stat="p90"`, constant("2")),

		// de and us run every minute; fr last ran an hour ago; jp last ran 5 h
		// ago, before a restart dropped its series; it last ran 25 h ago
		planetoid(lastRun, `,country="de"`, "0+60x1500"),
		copied(lastRun, `,country="de"`, "0+60x1500"),
		planetoid(lastRun, `,country="fr"`, constant("86400")),
		planetoid(lastRun, `,country="jp"`, "0+60x1200 _x300"),
		planetoid(lastRun, `,country="us"`, "0+60x1500"),
		planetoid(lastRun, `,country="it"`, constant("0")),
		planetoid(lastSuccess, `,country="de"`, "0+60x1500"),
		planetoid(lastSuccess, `,country="jp"`, "0+60x1200 _x300"),
		planetoid(lastSuccess, `,country="us"`, "0+60x1500"),
		planetoid(phaseLast, `,country="de",phase="create"`, constant("0.5")),
		planetoid(phaseLast, `,country="de",phase="setup"`, constant("3.5")),
		planetoid(phaseLast, `,country="de",phase="connect"`, constant("0.8")),
		planetoid(phaseLast, `,country="de",phase="load"`, constant("2.5")),
		planetoid(phaseLast, `,country="jp",phase="setup"`, "1.5+0x1200 _x300"),
	}
	lastTested := by("country", map[string]float64{"de": 0, "fr": 3600, "jp": 18000, "us": 0, "it": 90000})
	dayEvaluations := []evaluation{
		{proxyTestUpPanel, "A", value(1)},
		{proxyTestHeartbeatPanel, "A", value(0)},
		{proxyTestVersionPanel, "A", by("version", map[string]float64{"2026.11.06-1": 1})},
		{proxyTestRemoveFailuresPanel, "A", value(0)},
		{proxyTestApiErrorsPanel, "A", value(1440)},
		{proxyTestSetupP50Panel, "A", value(2.5)},
		{proxyTestSetupP90Panel, "A", value(6)},
		{proxyTestConnectP50Panel, "A", value(0.75)},
		{proxyTestConnectP90Panel, "A", value(1.5)},
		// (180 de + 120 jp + 60 us ok) / (360 ok + 60 http_error + 60 create_error)
		{proxyTestSuccessPanel, "A", value(0.75)},
		{proxyTestCountriesPanel, "A", value(5)},
		{proxyTestTestedPanel, "A", value(4)},
		{proxyTestCoveragePanel, "A", value(0.8)},
		{proxyTestPercentilesPanel, "A", []map[string]any{
			{"labels": `{phase="setup",stat="p50"}`, "value": 2.5},
			{"labels": `{phase="setup",stat="p90"}`, "value": 6},
			{"labels": `{phase="connect",stat="p50"}`, "value": 0.75},
			{"labels": `{phase="connect",stat="p90"}`, "value": 1.5},
			{"labels": `{phase="load",stat="p50"}`, "value": 2},
			{"labels": `{phase="load",stat="p90"}`, "value": 4},
		}},
		{proxyTestOutcomesPanel, "A", by("outcome", map[string]float64{"ok": 360, "http_error": 60, "create_error": 60})},
		{proxyTestMeanPanel, "A", []map[string]any{
			{"labels": `{country="de",phase="setup"}`, "value": 3},
			{"labels": `{country="de",phase="connect"}`, "value": 0.5},
		}},
		{proxyTestApiErrorTrendPanel, "A", by("call", map[string]float64{"create": 60, "login": 0})},
		{proxyTestApiErrorTrendPanel, "B", value(0)},
		{proxyTestCountryTablePanel, "A", by("country", map[string]float64{"de": 3, "jp": 1})},
		{proxyTestCountryTablePanel, "B", by("country", map[string]float64{"de": 4.5, "jp": 2})},
		{proxyTestCountryTablePanel, "C", by("country", map[string]float64{"de": 1})},
		{proxyTestCountryTablePanel, "D", by("country", map[string]float64{"de": 2})},
		{proxyTestCountryTablePanel, "E", by("country", map[string]float64{"de": 3})},
		{proxyTestCountryTablePanel, "F", by("country", map[string]float64{"de": 5})},
		{proxyTestCountryTablePanel, "G", by("country", map[string]float64{"de": 5760, "fr": 1440, "jp": 2880, "us": 1440})},
		{proxyTestCountryTablePanel, "H", by("country", map[string]float64{"de": 1440, "fr": 1440, "jp": 0, "us": 0})},
		{proxyTestCountryTablePanel, "I", by("country", map[string]float64{"de": 0.75, "fr": 0, "jp": 1, "us": 1})},
		{proxyTestCountryTablePanel, "J", lastTested},
		{proxyTestCountryTablePanel, "K", by("country", map[string]float64{"de": 1440, "fr": 0, "jp": 0, "us": 0})},
		{proxyTestSlowestPanel, "A", by("country", map[string]float64{"de": 4.5, "jp": 2})},
		{proxyTestFailureRatePanel, "A", by("country", map[string]float64{"de": 0.25, "fr": 1})},
		{proxyTestLatestRunPanel, "A", lastTested},
		{proxyTestLatestRunPanel, "B", by("country", map[string]float64{"de": 0, "jp": 18000, "us": 0})},
		{proxyTestLatestRunPanel, "C", by("country", map[string]float64{"de": 0.5})},
		{proxyTestLatestRunPanel, "D", by("country", map[string]float64{"de": 3.5, "jp": 1.5})},
		{proxyTestLatestRunPanel, "E", by("country", map[string]float64{"de": 0.8})},
		{proxyTestLatestRunPanel, "F", by("country", map[string]float64{"de": 2.5})},
	}

	cases := []struct {
		name        string
		series      []map[string]string
		evaluations []evaluation
	}{
		{"a day of runs", day, dayEvaluations},
		{"a crashed runner whose file still says up, with every run failing", []map[string]string{
			planetoid(up, "", constant("1")),
			planetoid(heartbeat, "", constant("89700")),
			planetoid(countries, "", constant("3")),
			planetoid(runs, `,country="fr",outcome="create_error"`, "0+1x1500"),
			planetoid(lastRun, `,country="fr"`, constant("0")),
		}, []evaluation{
			{proxyTestUpPanel, "A", value(2)},
			{proxyTestHeartbeatPanel, "A", value(300)},
			{proxyTestRemoveFailuresPanel, "A", value(0)},
			{proxyTestApiErrorsPanel, "A", value(0)},
			{proxyTestSetupP50Panel, "A", none},
			{proxyTestSuccessPanel, "A", value(0)},
			{proxyTestTestedPanel, "A", value(0)},
			{proxyTestCoveragePanel, "A", value(0)},
			{proxyTestCountryTablePanel, "I", by("country", map[string]float64{"fr": 0})},
			{proxyTestFailureRatePanel, "A", by("country", map[string]float64{"fr": 1})},
		}},
		{"a stopped runner with no run in the last day", []map[string]string{
			planetoid(up, "", constant("0")),
			planetoid(heartbeat, "", constant("86400")),
			planetoid(runs, `,country="de",outcome="ok"`, constant("5")),
		}, []evaluation{
			{proxyTestUpPanel, "A", value(0)},
			{proxyTestHeartbeatPanel, "A", value(3600)},
			{proxyTestSuccessPanel, "A", none},
			{proxyTestCountryTablePanel, "G", by("country", map[string]float64{"de": 0})},
			{proxyTestCountryTablePanel, "H", by("country", map[string]float64{"de": 0})},
			{proxyTestCountryTablePanel, "I", none},
			{proxyTestFailureRatePanel, "A", none},
		}},
		{"no telemetry from main", []map[string]string{
			other(up, "", constant("1")),
			other(heartbeat, "", "0+60x1500"),
			other(countries, "", constant("5")),
			other(runs, `,country="de",outcome="ok"`, "0+1x1500"),
			other(lastRun, `,country="de"`, "0+60x1500"),
		}, []evaluation{
			{proxyTestUpPanel, "A", none},
			{proxyTestHeartbeatPanel, "A", none},
			{proxyTestVersionPanel, "A", none},
			{proxyTestRemoveFailuresPanel, "A", none},
			{proxyTestApiErrorsPanel, "A", none},
			{proxyTestSetupP50Panel, "A", none},
			{proxyTestSuccessPanel, "A", none},
			{proxyTestCountriesPanel, "A", none},
			{proxyTestTestedPanel, "A", value(0)},
			{proxyTestCoveragePanel, "A", none},
			{proxyTestOutcomesPanel, "A", none},
			{proxyTestCountryTablePanel, "G", none},
			{proxyTestCountryTablePanel, "J", none},
		}},
	}
	tests := []map[string]any{}
	for _, c := range cases {
		expressionTests := []map[string]any{}
		for _, e := range c.evaluations {
			expressionTests = append(expressionTests, map[string]any{"expr": query(e.id, e.refId), "eval_time": "1500m", "exp_samples": e.samples})
		}
		tests = append(tests, map[string]any{"name": c.name, "interval": "1m", "input_series": c.series, "promql_expr_test": expressionTests})
	}
	// Json is also valid yaml; its quoting preserves the exact embedded query.
	fixture, err := json.Marshal(map[string]any{"evaluation_interval": "1m", "tests": tests})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "proxy-test.yml")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, promtool, "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("proxy test expression evaluation: %v\n%s", err, output)
	}
}
