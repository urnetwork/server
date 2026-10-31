package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	redisRatesResponseLimit = 256 * 1024
	redisRatesFreshness     = 3 * time.Minute
	redisRatesDashboardUID  = "urnetwork-redis-cluster"
	redisRatesAlertRuleUID  = "redis-node-wedged"
)

// Signal redis-rates implements SIGNALS.md §1.4a. It joins fresh Redis
// exporter coverage with the exact live Grafana dashboard and alert-rule
// definitions so an undersized rate range cannot masquerade as missing Redis
// telemetry.
func NewRedisRatesSignal() Signal {
	return newRedisRatesSignal(newGrafanaObservationHTTPClient(10*time.Second), "")
}

func newRedisRatesSignal(client grafanaHTTPClient, endpoint string) Signal {
	return &signalAdapter{
		number: "1.4a", key: "redis-rates", name: "Redis counter-rate visibility",
		probe: redisRatesProbe{client: client, endpoint: endpoint},
	}
}

type redisRatesProbe struct {
	client   grafanaHTTPClient
	endpoint string
}

func (redisRatesProbe) id() string             { return "observability/redis-rate-windows" }
func (redisRatesProbe) tier() string           { return tierWarn }
func (redisRatesProbe) cadence() time.Duration { return time.Minute }

type redisRateMetric struct {
	name  string
	short string
}

var redisRateMetrics = []redisRateMetric{
	{name: "redis_commands_processed_total", short: "commands"},
	{name: "redis_evicted_keys_total", short: "evicted"},
	{name: "redis_expired_keys_total", short: "expired"},
}

// A configured exporter source is the host and decimal node-port pair that
// the Redis Fluent Bit multi-target publisher stamps alongside env/job.
type redisRateSource struct {
	host string
	node string
}

// Counts contain only unique configured sources, after the exact identity
// and per-source multiplicity checks; foreign sources cannot fill a hole.
type redisRateCoverage struct {
	expected int
	counts   map[string]int
}

func (c redisRateCoverage) count(short, window string) int {
	return c.counts[short+"_"+window]
}

func (c redisRateCoverage) complete() bool {
	if c.expected <= 0 {
		return false
	}
	for _, metric := range redisRateMetrics {
		if c.count(metric.short, "raw") != c.expected || c.count(metric.short, "5m") != c.expected {
			return false
		}
	}
	return true
}

func (c redisRateCoverage) summary() string {
	parts := []string{fmt.Sprintf("expected_nodes=%d", c.expected)}
	for _, metric := range redisRateMetrics {
		parts = append(parts, fmt.Sprintf(
			"%s_raw=%d %s_1m=%d %s_5m=%d",
			metric.short, c.count(metric.short, "raw"),
			metric.short, c.count(metric.short, "1m"),
			metric.short, c.count(metric.short, "5m"),
		))
	}
	return strings.Join(parts, " ")
}

type redisRatesDashboardEnvelope struct {
	Dashboard struct {
		UID     string            `json:"uid"`
		Version int               `json:"version"`
		Panels  []redisRatesPanel `json:"panels"`
	} `json:"dashboard"`
}

type redisRatesPanel struct {
	ID      int               `json:"id"`
	Panels  []redisRatesPanel `json:"panels"`
	Targets []struct {
		Expression string `json:"expr"`
	} `json:"targets"`
}

type redisRatesAlertRule struct {
	UID   string `json:"uid"`
	Title string `json:"title"`
	Data  []struct {
		Model struct {
			Expression string `json:"expr"`
		} `json:"model"`
	} `json:"data"`
}

type redisRateWindowAudit struct {
	uid                 string
	revision            int
	expectedOccurrences int
	observedOccurrences int
	fixedOccurrences    int
	dynamicOccurrences  int
	otherOccurrences    int
}

func (a redisRateWindowAudit) healthy() bool {
	return a.uid != "" && a.observedOccurrences == a.expectedOccurrences &&
		a.fixedOccurrences == a.expectedOccurrences &&
		a.dynamicOccurrences == 0 && a.otherOccurrences == 0
}

func (a redisRateWindowAudit) summary() string {
	revision := ""
	if a.revision > 0 {
		revision = fmt.Sprintf(" revision=%d", a.revision)
	}
	return fmt.Sprintf(
		"uid=%s%s expected_rate_windows=%d observed_rate_windows=%d fixed_5m=%d dynamic=%d other=%d",
		a.uid, revision, a.expectedOccurrences, a.observedOccurrences,
		a.fixedOccurrences, a.dynamicOccurrences, a.otherOccurrences,
	)
}

func (p redisRatesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	environment := strings.TrimSpace(env.cfg.env)
	domain := strings.TrimSpace(env.cfg.publicDomain)
	if environment == "" || domain == "" {
		return nil, nil
	}

	hostname := environment + "-grafana." + domain
	findings := make([]finding, 0, 5)
	coverage, metricHost, coverageErr := observeRedisRateCoverage(ctx, env)
	if coverageErr != nil {
		findings = append(findings, cannotObserveFinding("redis-rate-source", coverageErr))
	} else {
		findings = append(findings, evaluateRedisRateCoverage(metricHost, coverage))
	}

	if env.cfg.grafanaAdminPassword == "" {
		findings = append(findings, cannotObserveFinding(hostname+"/redis-rate-definitions", fmt.Errorf("Grafana admin password is not configured")))
		return findings, nil
	}
	client := p.client
	if client == nil {
		client = newGrafanaObservationHTTPClient(10 * time.Second)
	}
	endpoint := strings.TrimRight(p.endpoint, "/")
	if endpoint == "" {
		endpoint = "https://" + hostname
	}

	var dashboard redisRatesDashboardEnvelope
	dashboardTarget := hostname + "/" + redisRatesDashboardUID
	if err := getBoundedGrafanaJSON(
		ctx, client, endpoint+"/api/dashboards/uid/"+redisRatesDashboardUID,
		env.cfg.grafanaAdminPassword, &dashboard,
	); err != nil {
		findings = append(findings, cannotObserveFinding(dashboardTarget, err))
	} else {
		audit, err := auditRedisRatesDashboard(dashboard)
		if err != nil {
			findings = append(findings, cannotObserveFinding(dashboardTarget, err))
		} else {
			findings = append(findings, evaluateRedisDashboardRateWindows(dashboardTarget, audit, coverage, coverageErr))
		}
	}

	var alertRule redisRatesAlertRule
	alertTarget := hostname + "/" + redisRatesAlertRuleUID
	if err := getBoundedGrafanaJSON(
		ctx, client, endpoint+"/api/v1/provisioning/alert-rules/"+redisRatesAlertRuleUID,
		env.cfg.grafanaAdminPassword, &alertRule,
	); err != nil {
		findings = append(findings, cannotObserveFinding(alertTarget, err))
	} else {
		audit, err := auditRedisRatesAlertRule(alertRule)
		if err != nil {
			findings = append(findings, cannotObserveFinding(alertTarget, err))
		} else {
			findings = append(findings, evaluateRedisAlertRateWindows(alertTarget, audit, coverage, coverageErr))
		}
	}

	return findings, nil
}

func observeRedisRateCoverage(ctx context.Context, env *probeEnv) (redisRateCoverage, string, error) {
	expectedSources := map[redisRateSource]bool{}
	for _, nodeHost := range env.cfg.redisNodeHosts() {
		for _, port := range nodeHost.redisNodePorts() {
			source := redisRateSource{host: nodeHost.name, node: strconv.Itoa(port)}
			if expectedSources[source] {
				return redisRateCoverage{}, "", fmt.Errorf("ambiguous Redis source inventory")
			}
			expectedSources[source] = true
		}
	}
	if len(expectedSources) == 0 {
		return redisRateCoverage{}, "", fmt.Errorf("no Redis node inventory is configured")
	}
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return redisRateCoverage{}, "", fmt.Errorf("no services host is configured for the loopback Mimir query")
	}

	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(redisRateCoverageQuery(env.cfg.env))
	out, metricHost, err := mimirShellFirstServiceGateway(
		ctx, env.runner, metricHosts, nil,
		"curl -fsS --max-time 15 '"+queryURL+"'", mimirShellResponseMaxBytes)
	if err != nil {
		return redisRateCoverage{}, "", fmt.Errorf("query Mimir through service gateways: %w", err)
	}
	coverage, err := parseRedisRateCoverage(out, env.cfg.env, expectedSources)
	if err != nil {
		return redisRateCoverage{}, metricHost.name, err
	}
	return coverage, metricHost.name, nil
}

// Preserve configured source labels and count extra publisher series within
// each source. A zero marker distinguishes empty and omitted partitions.
func redisRateCoverageQuery(environment string) string {
	parts := make([]string, 0, len(redisRateMetrics)*3)
	for _, metric := range redisRateMetrics {
		selector := metric.name + `{env=` + strconv.Quote(environment) + `,job="redis"}`
		fresh := selector + ` and (timestamp(` + selector + `) >= time() - ` +
			strconv.FormatInt(int64(redisRatesFreshness/time.Second), 10) + `)`
		checks := []struct {
			name       string
			expression string
		}{
			{name: metric.short + "_raw", expression: "count by (env, job, host, node) (" + fresh + ")"},
			{name: metric.short + "_1m", expression: "count by (env, job, host, node) (rate(" + selector + "[1m]) and " + fresh + ")"},
			{name: metric.short + "_5m", expression: "count by (env, job, host, node) (rate(" + selector + "[5m]) and " + fresh + ")"},
		}
		for _, check := range checks {
			parts = append(parts, `label_replace((`+check.expression+` or vector(0)),"monitor_check",`+
				strconv.Quote(check.name)+`,"__name__",".*")`)
		}
	}
	return strings.Join(parts, " or ")
}

// Reduce only complete, unambiguous source partitions. Equal global counts
// do not establish cohort coverage, and partial success is unknown. Source
// labels, warning strings and malformed response values never enter errors.
func parseRedisRateCoverage(output string, environment string, expectedSources map[redisRateSource]bool) (redisRateCoverage, error) {
	if environment == "" || len(expectedSources) == 0 {
		return redisRateCoverage{}, fmt.Errorf("no Redis source inventory is configured")
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return redisRateCoverage{}, fmt.Errorf("invalid Mimir response encoding")
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return redisRateCoverage{}, fmt.Errorf("Mimir did not return a successful vector")
	}
	if len(response.Warnings) != 0 {
		return redisRateCoverage{}, fmt.Errorf("Mimir returned partial query warnings")
	}
	partitionSources := map[string]map[redisRateSource]bool{}
	for _, metric := range redisRateMetrics {
		for _, window := range []string{"raw", "1m", "5m"} {
			partitionSources[metric.short+"_"+window] = map[redisRateSource]bool{}
		}
	}
	markers := map[string]bool{}
	for _, series := range response.Data.Result {
		check := series.Metric["monitor_check"]
		sources, ok := partitionSources[check]
		if !ok {
			return redisRateCoverage{}, fmt.Errorf("unexpected Mimir coverage partition")
		}
		_, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return redisRateCoverage{}, fmt.Errorf("invalid Mimir coverage sample")
		}
		if len(series.Metric) == 1 {
			if value != 0 || markers[check] {
				return redisRateCoverage{}, fmt.Errorf("invalid Mimir coverage partition marker")
			}
			markers[check] = true
			continue
		}
		if len(series.Metric) != 5 || series.Metric["env"] != environment ||
			series.Metric["job"] != "redis" || series.Metric["host"] == "" || series.Metric["node"] == "" {
			return redisRateCoverage{}, fmt.Errorf("invalid Mimir coverage source labels")
		}
		source := redisRateSource{host: series.Metric["host"], node: series.Metric["node"]}
		if !expectedSources[source] {
			continue
		}
		if value != 1 || sources[source] {
			return redisRateCoverage{}, fmt.Errorf("ambiguous Mimir coverage for a configured Redis source")
		}
		sources[source] = true
	}
	if len(markers) != len(partitionSources) {
		return redisRateCoverage{}, fmt.Errorf("Mimir response omitted coverage partition markers")
	}
	counts := make(map[string]int, len(partitionSources))
	for check, sources := range partitionSources {
		counts[check] = len(sources)
	}
	return redisRateCoverage{expected: len(expectedSources), counts: counts}, nil
}

func getBoundedGrafanaJSON(
	ctx context.Context,
	client grafanaHTTPClient,
	endpoint string,
	adminPassword string,
	destination any,
) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.SetBasicAuth("admin", adminPassword)
	response, err := doScopedGrafanaRequest(client, request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Grafana returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, redisRatesResponseLimit+1))
	if err != nil {
		return err
	}
	if len(body) > redisRatesResponseLimit {
		return fmt.Errorf("Grafana response exceeded %d bytes", redisRatesResponseLimit)
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return fmt.Errorf("decode Grafana JSON: %w", err)
	}
	return nil
}

func auditRedisRatesDashboard(response redisRatesDashboardEnvelope) (redisRateWindowAudit, error) {
	if response.Dashboard.UID != redisRatesDashboardUID {
		return redisRateWindowAudit{}, fmt.Errorf("unexpected response: Grafana dashboard UID mismatch")
	}
	expressionsByPanel := map[int][]string{}
	var collect func([]redisRatesPanel)
	collect = func(panels []redisRatesPanel) {
		for _, panel := range panels {
			for _, target := range panel.Targets {
				expressionsByPanel[panel.ID] = append(expressionsByPanel[panel.ID], target.Expression)
			}
			collect(panel.Panels)
		}
	}
	collect(response.Dashboard.Panels)

	expected := map[int][]string{
		8:  {"redis_commands_duration_seconds_total", "redis_commands_processed_total"},
		9:  {"redis_commands_processed_total"},
		11: {"redis_evicted_keys_total", "redis_expired_keys_total"},
	}
	audit := redisRateWindowAudit{
		uid: redisRatesDashboardUID, revision: response.Dashboard.Version, expectedOccurrences: 5,
	}
	for panelID, metrics := range expected {
		expressions, ok := expressionsByPanel[panelID]
		if !ok {
			continue
		}
		joined := strings.Join(expressions, "\n")
		for _, metric := range metrics {
			classifyRedisRateWindows(&audit, joined, metric)
		}
	}
	return audit, nil
}

func auditRedisRatesAlertRule(rule redisRatesAlertRule) (redisRateWindowAudit, error) {
	if rule.UID != redisRatesAlertRuleUID {
		return redisRateWindowAudit{}, fmt.Errorf("unexpected response: Grafana alert-rule UID mismatch")
	}
	audit := redisRateWindowAudit{uid: rule.UID, expectedOccurrences: 2}
	joined := ""
	for _, item := range rule.Data {
		joined += "\n" + item.Model.Expression
	}
	classifyRedisRateWindows(&audit, joined, "redis_commands_duration_seconds_total")
	classifyRedisRateWindows(&audit, joined, "redis_commands_processed_total")
	return audit, nil
}

func classifyRedisRateWindows(audit *redisRateWindowAudit, expression, metric string) {
	pattern := regexp.MustCompile(`rate\s*\(\s*` + regexp.QuoteMeta(metric) + `(?:\{[^}]*\})?\[([^]]+)\]\s*\)`)
	for _, match := range pattern.FindAllStringSubmatch(expression, -1) {
		audit.observedOccurrences++
		switch match[1] {
		case "5m":
			audit.fixedOccurrences++
		case "$__rate_interval":
			audit.dynamicOccurrences++
		default:
			audit.otherOccurrences++
		}
	}
}

func evaluateRedisRateCoverage(metricHost string, coverage redisRateCoverage) finding {
	target := "redis-exporter-fleet"
	if coverage.complete() {
		return healthyFinding("observability/redis-rate-windows", tierWarn, "redis-rate-source-coverage", target)
	}
	return finding{
		probeId: "observability/redis-rate-windows", tier: tierWarn,
		class: "redis-rate-source-coverage", target: target, frame: "raw-and-five-minute", sustain: 2,
		symptom:   "Fresh Redis exporter counters or their five-minute rates do not cover every configured node",
		mechanism: "A missing raw series identifies collection or ingestion loss; a present raw series without a five-minute rate lacks enough samples for a counter derivative. This boundary must be healthy before an empty dashboard can be attributed solely to its query window.",
		baseline:  "Fresh raw commands, evictions, and expirations plus their five-minute rates each cover every configured Redis node.",
		observed:  coverage.summary(),
		evidence:  "The bounded loopback Mimir query verified configured env/job/host/node identities and unique per-source series through gateway " + metricHost + "; public evidence retains only cohort counts, never source labels or counter values.",
		context:   "A dashboard definition can be wrong at the same time, but changing it cannot restore a missing exporter or remote-write series.",
		action:    "Check the missing node's exporter scrape, Fluent Bit input, remote-write response, and Mimir freshness. Do not restart Redis or deploy a dashboard merely to create absent source samples.",
		verify:    "Two consecutive warning-free queries return exactly one fresh raw and five-minute-rate series for every configured host/node pair and all three counters.",
		playbook:  "SIGNALS.md §1.4a and §11.14",
	}
}

func evaluateRedisDashboardRateWindows(target string, audit redisRateWindowAudit, coverage redisRateCoverage, coverageErr error) finding {
	if audit.healthy() {
		return healthyFinding("observability/redis-rate-windows", tierWarn, "redis-dashboard-rate-window", target)
	}
	source := "source_coverage=unknown"
	if coverageErr == nil {
		source = coverage.summary()
	}
	return finding{
		probeId: "observability/redis-rate-windows", tier: tierWarn,
		class: "redis-dashboard-rate-window", target: target, frame: redisRatesDashboardUID, sustain: 1,
		symptom:   "The live Redis dashboard does not use the required five-minute range for every counter rate",
		mechanism: "Redis exporter scrapes are intentionally staggered from 61 to 108 seconds. Grafana can resolve $__rate_interval to one minute, which contains fewer than two samples and renders counter panels as No data while the raw counters remain healthy.",
		baseline:  "Dashboard panels 8, 9, and 11 contain exactly five required rate expressions and every one uses a fixed [5m] range.",
		observed:  audit.summary() + " " + source,
		evidence:  "The authenticated bounded dashboard read retained only its UID, revision, panel IDs, and rate-window counts; expressions and credentials are not emitted.",
		context:   "When raw and five-minute coverage equal the node inventory, this is a dashboard visibility defect rather than a Redis outage. Unknown source coverage remains a separate alert and prevents that stronger attribution.",
		action:    "Build and deploy Grafana from a server revision containing commit 3e59900c. Do not restart Redis, change scrape staggering, or substitute a shorter automatic range.",
		verify:    "Every active Grafana block reports the new artifact; two consecutive live dashboard reads show five fixed [5m] windows and zero dynamic/other windows; all 48 command, eviction, and expiration rate series are queryable and the panels render them.",
		playbook:  "SIGNALS.md §1.4a",
	}
}

func evaluateRedisAlertRateWindows(target string, audit redisRateWindowAudit, coverage redisRateCoverage, coverageErr error) finding {
	if audit.healthy() {
		return healthyFinding("observability/redis-rate-windows", tierWarn, "redis-alert-rate-window", target)
	}
	source := "source_coverage=unknown"
	if coverageErr == nil {
		source = coverage.summary()
	}
	return finding{
		probeId: "observability/redis-rate-windows", tier: tierWarn,
		class: "redis-alert-rate-window", target: target, frame: redisRatesAlertRuleUID, sustain: 1,
		symptom:   "The live Redis wedged-node alert does not use the required five-minute counter ranges",
		mechanism: "A two-minute range covers too few samples for the slowest intentionally staggered Redis scrapes, so the latency ratio can omit nodes precisely when the alert is expected to cover the full cluster.",
		baseline:  "The redis-node-wedged rule contains exactly two rate expressions and both use a fixed [5m] range.",
		observed:  audit.summary() + " " + source,
		evidence:  "The authenticated bounded provisioning read retained only the rule UID and rate-window counts; the rule body and credential are not emitted.",
		context:   "This is alert coverage loss, not proof that a Redis node is wedged. The independent PING and cluster-state signal remains authoritative for current node liveness.",
		action:    "Build and deploy Grafana from a Warp revision containing commit a314e4d together with the corrected server dashboard. Do not restart Redis or relax the wedged-node threshold.",
		verify:    "Every active Grafana block reports the new artifact; two consecutive provisioning reads show two fixed [5m] windows; a direct five-minute query covers every configured node; and the rule evaluation has no missing series.",
		playbook:  "SIGNALS.md §1.4a",
	}
}
