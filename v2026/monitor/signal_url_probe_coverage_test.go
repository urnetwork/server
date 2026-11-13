package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"gopkg.in/yaml.v3"
)

const syntheticUrlProbeDesiredConfig = `enabled: true
shard_count: 2
idle_delay_seconds: 5
max_time_seconds: 900
api_url: https://api.example
platform_url: wss://connect.example
tunnel_recreate_attempts: 2
url_probe_result_version: 1
url_success_interval_seconds: 1200
url_failure_interval_seconds: 60
url_probe:
  limit: 8
  concurrency: 8
  probe_timeout_seconds: 60
`

// The explicit replacement workflow retires the obsolete full/blackhole
// denominators, not monitoring: absent URL telemetry must still be unknown.
func TestEgressCoverageUrlOnlyHandsOffWithoutLegacyDatabaseQueries(t *testing.T) {
	pop := server.Config.PushSimpleResource("provider_egress_probe.yml", []byte(syntheticUrlProbeDesiredConfig))
	defer pop()
	hostCalls := 0
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			t.Fatal("URL-only rollout queried the retired full/blackhole denominator")
			return nil, nil
		},
		hostFn: func(HostSettings, string) (string, error) {
			hostCalls++
			return `{"status":"success","data":{"resultType":"vector","result":[]}}`, nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics.example", Roles: []string{"services"}})
	settings.LogServices = []string{"taskworker"}
	settings.LogServiceHosts = map[string][]string{"taskworker": {"worker-a.example", "worker-b.example"}}
	settings.LogServiceBlocks = map[string][]string{"taskworker": {"worker-group"}}
	if alerts, err := NewEgressCoverageSignal().Run(context.Background(), settings); err != nil || len(alerts) != 0 {
		t.Fatalf("retired workflow emitted a legacy verdict: %v %+v", err, alerts)
	}
	alerts, err := NewUrlProbeCoverageSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
	if hostCalls != 1 || len(alerts) != 1 {
		t.Fatalf("workflow handoff hid missing replacement telemetry: contacts=%d alerts=%+v", hostCalls, alerts)
	}
}

// An exact process fixture supplies source timestamps separately from query
// evaluation time, so retained values cannot masquerade as a fresh census.
func urlProbeCoverageFixture(now time.Time, host string, shard int) *urlProbeCoverageProcess {
	values := map[string]float64{
		"start": float64(now.Add(-2 * time.Hour).Unix()), "configured": 2, "capability": urlProbeCoverageCapability,
		"success": 13000, "success_samples": 120, "success_early": 10,
		"error": 0, "error_samples": 120, "error_early": 10,
	}
	scrape := float64(now.Add(-15 * time.Second).Unix())
	for _, name := range []string{"start_time", "configured_time", "success_time", "error_time", "capability_time"} {
		values[name] = scrape
	}
	values["heartbeat:"+fmt.Sprint(shard)] = float64(now.Add(-time.Minute).Unix())
	values["heartbeat_time:"+fmt.Sprint(shard)] = scrape
	if shard == 0 {
		for _, state := range urlProbeFleetStates {
			values["fleet:"+state] = 0
			values["fleet_time:"+state] = scrape
		}
		for _, state := range []string{"eligible", "complete", "quota_complete", "secure_complete"} {
			values["fleet:"+state] = 10000
		}
		values["observed"] = float64(now.Add(-time.Minute).Unix())
		values["observed_time"] = scrape
		values["oldest"] = 0
		values["oldest_time"] = scrape
		values["cohort_started"] = float64(now.Add(-8 * time.Hour).Unix())
		values["cohort_started_time"] = scrape
		values["cohort_contract"] = 1
		values["cohort_contract_time"] = scrape
		for _, cohort := range urlProbeAdmissionCohorts {
			for _, state := range urlProbeAdmissionCohortStates {
				key := cohort + ":" + state
				values["cohort:"+key] = 0
				values["cohort_time:"+key] = scrape
			}
		}
		values["cohort:mature:eligible"] = 10000
		values["cohort:mature:quota_complete"] = 10000
	}
	return &urlProbeCoverageProcess{host: host, block: "worker-group", instance: host + "-private-process", values: values}
}

func urlProbeCoverageFixtureJson(t *testing.T, now time.Time, processes ...*urlProbeCoverageProcess) string {
	t.Helper()
	rows := []any{}
	for _, process := range processes {
		if value, present := process.values["fleet:runs_needed"]; present {
			process.values["fleet:successes_needed"] = value
		}
		for name, value := range process.values {
			labels := map[string]string{"env": "synthetic", "job": "taskworker", "host": process.host, "block": process.block, "instance": process.instance}
			parts := strings.SplitN(name, ":", 2)
			labels["monitor_metric"] = parts[0]
			if len(parts) == 2 {
				if parts[0] == "cohort" || parts[0] == "cohort_time" {
					cohortState := strings.SplitN(parts[1], ":", 2)
					labels["cohort"], labels["state"] = cohortState[0], cohortState[1]
				} else if strings.HasPrefix(parts[0], "fleet") {
					labels["state"] = parts[1]
				} else {
					labels["shard"] = parts[1]
				}
			}
			rows = append(rows, map[string]any{"metric": labels, "value": []any{now.Unix(), fmt.Sprint(value)}})
		}
	}
	data, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": rows}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Runs the public Signal boundary with an isolated desired-config seam and
// only synthetic observations. It never reads local Config or contacts a host.
func runUrlProbeCoverageFixture(t *testing.T, now time.Time, payload string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics.example" || !strings.Contains(command, "--max-time 15 --max-filesize 2097152") || !strings.Contains(command, "--data-urlencode") {
			t.Fatal("URL coverage query lost its bounded inventory-owned transport")
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics.example", Roles: []string{"services"}})
	settings.LogServices = []string{"taskworker"}
	settings.LogServiceHosts = map[string][]string{"taskworker": {"worker-a.example", "worker-b.example"}}
	settings.LogServiceBlocks = map[string][]string{"taskworker": {"worker-group"}}
	signal := NewUrlProbeCoverageSignal().(*signalAdapter)
	signal.probe = urlProbeCoverageProbe{loadDesired: func() (urlProbeCoverageDesired, error) {
		return urlProbeCoverageDesired{enabled: true, shardCount: 2}, nil
	}}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func TestUrlProbeCoverageHealthyAndExactHourlyBoundary(t *testing.T) {
	now := time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	first.values["success"] = 12500
	second.values["success"] = 12500
	if alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second)); len(alerts) != 0 {
		t.Fatalf("complete quota at exact four-hour hourly rate was not healthy: %+v", alerts)
	}
}

func TestUrlProbeCoverageHighAggregateRateCannotHideStarvedProviders(t *testing.T) {
	now := time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	for _, state := range []string{"complete", "secure_complete", "quota_complete"} {
		first.values["fleet:"+state] = 8000
	}
	first.values["fleet:runs_needed"] = 20000
	first.values["fleet:overdue"] = 1000
	first.values["fleet:warming"] = 1000
	first.values["fleet:due"] = 2000
	first.values["cohort:mature:eligible"] = 9000
	first.values["cohort:mature:quota_complete"] = 8000
	first.values["cohort:mature:runs_needed"] = 10000
	first.values["cohort:warming:eligible"] = 1000
	first.values["cohort:warming:runs_needed"] = 10000
	first.values["success"] = 100000
	alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second))
	alert := requireAlertClass(t, alerts, "url-probe-coverage-deficit")
	if len(alerts) != 1 || alert.Severity != Severity(tierPage) || alert.Sustain != 2 {
		t.Fatalf("per-provider starvation was hidden by aggregate successes: %+v", alerts)
	}
	for _, text := range []string{"eligible=10000", "secure_complete=8000", "runs_needed=20000", "never summed", "not attempted requests", "ordinary URL failure", "SIGNALS.md §2.19f"} {
		if !strings.Contains(strings.ToLower(alert.Markdown()), strings.ToLower(text)) {
			t.Fatalf("missing coverage discriminator %q", text)
		}
	}
	requireAlertOmits(t, alert, first.instance, second.instance, first.host, second.host)
}

func TestUrlProbeCoverageQuotaDoesNotClearTlsAndUnknownTargets(t *testing.T) {
	now := time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	first.values["fleet:complete"] = 9999
	first.values["fleet:secure_complete"] = 9999
	first.values["fleet:security_pending"] = 1
	first.values["fleet:security_unknown_targets"] = 1
	first.values["fleet:warming"] = 1
	alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second))
	alert := requireAlertClass(t, alerts, "url-probe-security-pending")
	requireAlertClass(t, alerts, "url-probe-security-recovery-unknown")
	if len(alerts) != 2 || alert.Severity != Severity(tierWarn) || !strings.Contains(alert.Observed, "runs_needed=0") {
		t.Fatalf("quota-full security recovery disappeared: %+v", alerts)
	}
}

func TestUrlProbeCoverageHourlyAcceptedMeasuredRateNotLegacyAttempts(t *testing.T) {
	now := time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	for _, value := range []float64{0, 12499} {
		first.values["success"], second.values["success"] = value, value
		alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second))
		alert := requireAlertClass(t, alerts, "url-probe-throughput-deficit")
		if len(alerts) != 1 || alert.Sustain != 2 || !strings.Contains(alert.Observed, "required_measured_runs_per_hour=25000.0") {
			t.Fatalf("hourly throughput deficit was not detected: %+v", alerts)
		}
	}
	query := urlProbeCoverageQuery("synthetic")
	for _, want := range []string{`outcome="success"`, `outcome="error"`, "increase(", "[1h]", "offset 55m", "timestamp(", `job="taskworker"`} {
		if !strings.Contains(query, want) {
			t.Fatalf("missing source/time contract %q", want)
		}
	}
	if strings.Contains(query, "blackhole") || strings.Contains(query, "attempted") {
		t.Fatal("legacy attempts were substituted for acknowledged URL success")
	}
}

func TestUrlProbeCoverageMissingWorkerCannotBecomeLowThroughput(t *testing.T) {
	now := time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	first.values["success"] = 0
	first.values["fleet:complete"], first.values["fleet:secure_complete"], first.values["fleet:quota_complete"] = 0, 0, 0
	first.values["fleet:runs_needed"] = 100000
	first.values["fleet:overdue"] = 10000
	first.values["cohort:mature:quota_complete"] = 0
	first.values["cohort:mature:runs_needed"] = 100000
	alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first))
	requireAlertClass(t, alerts, "url-probe-coverage-deficit")
	requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
	if len(alerts) != 2 {
		t.Fatalf("missing worker became a falsely complete low-rate verdict: %+v", alerts)
	}
}

func TestUrlProbeCoverageRetainedAndIncoherentCensusCannotAppearHealthy(t *testing.T) {
	now := time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC)
	for _, change := range []struct {
		key   string
		value float64
	}{
		{key: "observed", value: float64(now.Add(-181 * time.Second).Unix())},
		{key: "observed", value: float64(now.Add(31 * time.Second).Unix())},
		{key: "fleet_time:eligible", value: float64(now.Add(-30 * time.Second).Unix())},
		{key: "fleet:complete", value: 9999},
		{key: "fleet:runs_needed", value: 1},
		{key: "fleet:eligible", value: 9999.5},
		{key: "fleet:security_unknown_targets", value: 1},
		{key: "oldest_time", value: 0},
	} {
		first := urlProbeCoverageFixture(now, "worker-a.example", 0)
		second := urlProbeCoverageFixture(now, "worker-b.example", 1)
		first.values[change.key] = change.value
		alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second))
		requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
		if len(alerts) != 1 {
			t.Fatalf("invalid census %s became numeric coverage: %+v", change.key, alerts)
		}
	}
}

func TestUrlProbeCoverageOldOwnerCannotFillReplacementCells(t *testing.T) {
	now := time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC)
	old := urlProbeCoverageFixture(now, "worker-a.example", 0)
	current := urlProbeCoverageFixture(now, "worker-a.example", 0)
	current.instance = "synthetic-replacement"
	current.values["start"] = float64(now.Add(-90 * time.Minute).Unix())
	delete(current.values, "fleet:eligible")
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, old, current, second))
	requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
	if len(alerts) != 1 {
		t.Fatalf("old owner filled replacement's missing census: %+v", alerts)
	}
}

// Complete, overdue and warming are an exhaustive disjoint partition of the
// producer's eligible census, including security-quarantined providers.
func TestUrlProbeCoverageCensusCannotLoseOrDoubleCountCohort(t *testing.T) {
	now := time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC)
	for _, warming := range []float64{999, 1001} {
		first := urlProbeCoverageFixture(now, "worker-a.example", 0)
		second := urlProbeCoverageFixture(now, "worker-b.example", 1)
		for _, state := range []string{"complete", "secure_complete", "quota_complete"} {
			first.values["fleet:"+state] = 8000
		}
		first.values["fleet:runs_needed"] = 20000
		first.values["fleet:overdue"] = 1000
		first.values["fleet:warming"] = warming
		alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second))
		requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
		if len(alerts) != 1 {
			t.Fatalf("incoherent cohort with warming=%.0f produced a numeric verdict: %+v", warming, alerts)
		}
	}
}

func TestUrlProbeCoverageDuplicateShardOwnerCannotBeSummed(t *testing.T) {
	now := time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 0)
	alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second))
	alert := requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
	if len(alerts) != 1 || !strings.Contains(alert.Observed, "shard_0_owners=2") {
		t.Fatalf("two global censuses became double the provider population: %+v", alerts)
	}
}

func TestUrlProbeCoverageWarmupAndMissingCounterAreUnknownNotZero(t *testing.T) {
	now := time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC)
	for _, name := range []string{"success", "success_time", "success_samples", "success_early", "start"} {
		first := urlProbeCoverageFixture(now, "worker-a.example", 0)
		second := urlProbeCoverageFixture(now, "worker-b.example", 1)
		if name == "start" {
			second.values[name] = float64(now.Add(-30 * time.Minute).Unix())
		} else {
			delete(second.values, name)
		}
		alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second))
		requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
		if len(alerts) != 1 {
			t.Fatalf("unobserved %s counter generated a numeric low-rate verdict: %+v", name, alerts)
		}
	}
}

func TestUrlProbeCoverageZeroEligibleIsNotAnOutage(t *testing.T) {
	now := time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	for _, state := range urlProbeFleetStates {
		first.values["fleet:"+state] = 0
	}
	first.values["cohort:mature:eligible"], first.values["cohort:mature:quota_complete"] = 0, 0
	first.values["success"], second.values["success"] = 0, 0
	if alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second)); len(alerts) != 0 {
		t.Fatalf("zero eligible population manufactured a deficit: %+v", alerts)
	}
}

func TestUrlProbeCoverageMalformedAndPartialTransportIsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 27, 18, 0, 0, 0, time.UTC)
	valid := urlProbeCoverageFixtureJson(t, now, urlProbeCoverageFixture(now, "worker-a.example", 0), urlProbeCoverageFixture(now, "worker-b.example", 1))
	for _, payload := range []string{
		"invalid-synthetic-response", urlProbeCoverageFixtureJson(t, now),
		strings.Replace(valid, `"status":"success"`, `"warnings":["synthetic-partial"],"status":"success"`, 1),
		strings.Replace(valid, `"synthetic"`, `"wrong-environment"`, 1),
		strings.Repeat("x", urlProbeCoverageResponseMax+1),
	} {
		alerts := runUrlProbeCoverageFixture(t, now, payload)
		requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
		if len(alerts) != 1 {
			t.Fatalf("malformed observation generated a numeric verdict: %+v", alerts)
		}
	}
}

func TestUrlProbeCoverageDesiredGeometryHasNoLegacyDefault(t *testing.T) {
	for _, document := range []string{
		"", "enabled: true\nshard_count: 4\n",
		strings.Replace(syntheticUrlProbeDesiredConfig, "shard_count: 2", "shard_count: 0", 1),
		strings.Replace(syntheticUrlProbeDesiredConfig, "shard_count: 2", "shard_count: 257", 1),
		strings.Replace(syntheticUrlProbeDesiredConfig, "url_probe_result_version: 1", "url_probe_result_version: 0", 1),
		strings.Replace(syntheticUrlProbeDesiredConfig, "  concurrency: 8", "  concurrency: 9", 1),
		strings.Replace(syntheticUrlProbeDesiredConfig, "  probe_timeout_seconds: 60", "  probe_timeout_seconds: 0", 1),
		syntheticUrlProbeDesiredConfig + "  unexpected_mode: true\n",
	} {
		if _, err := parseUrlProbeCoverageDesired(func(value any) error { return yaml.Unmarshal([]byte(document), value) }); err == nil {
			t.Fatal("missing desired URL geometry acquired guessed shard coverage")
		}
	}
	for _, document := range []string{"enabled: false", syntheticUrlProbeDesiredConfig} {
		if _, err := parseUrlProbeCoverageDesired(func(value any) error { return yaml.Unmarshal([]byte(document), value) }); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUrlProbeCoverageCancellationAndDisabledConfigAvoidContact(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		t.Fatal("canceled or disabled probe contacted a host")
		return "", nil
	}}
	signal := NewUrlProbeCoverageSignal().(*signalAdapter)
	signal.probe = urlProbeCoverageProbe{loadDesired: func() (urlProbeCoverageDesired, error) { return urlProbeCoverageDesired{}, nil }}
	if alerts, err := signal.Run(context.Background(), syntheticSettings(source)); err != nil || len(alerts) != 0 {
		t.Fatalf("disabled probe did work: %v %+v", err, alerts)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := signal.Run(ctx, syntheticSettings(source)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was lost: %v", err)
	}
}

// A rate-history gap does not invalidate the separate, coherent global census.
// Invalid or ambiguous censuses still return before any numeric projection.
func TestUrlProbeCoverageHourlyGapRetainsQualifiedCensus(t *testing.T) {
	now := time.Date(2026, 10, 4, 2, 23, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	delete(second.values, "success_early")
	alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second))
	alert := requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
	if len(alerts) != 1 {
		t.Fatal("hourly gap invented a quota deficit")
	}
	for _, field := range []string{"hourly_measured_run_ranges_or_expected_process_coverage_incomplete", "eligible=10000", "quota_complete=10000", "secure_complete=10000", "runs_needed=0", "overdue=0", "census_reason=ok"} {
		if !strings.Contains(alert.Observed, field) {
			t.Fatalf("qualified census field missing: %s", field)
		}
	}
	for _, invalid := range []string{"stale", "ambiguous"} {
		t.Run(invalid, func(t *testing.T) {
			a := urlProbeCoverageFixture(now, "worker-a.example", 0)
			b := urlProbeCoverageFixture(now, "worker-b.example", 1)
			if invalid == "stale" {
				a.values["observed"] = float64(now.Add(-181 * time.Second).Unix())
			} else {
				b = urlProbeCoverageFixture(now, "worker-b.example", 0)
			}
			alert := requireAlertClass(t, runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, a, b)), "url-probe-coverage-unobservable")
			for _, field := range []string{"eligible=", "quota_complete=", "runs_needed=", "overdue="} {
				if strings.Contains(alert.Observed, field) {
					t.Fatalf("invalid census exposed numeric field: %s", field)
				}
			}
		})
	}
}
