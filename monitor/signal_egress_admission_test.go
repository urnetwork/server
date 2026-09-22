package monitor

// Synthetic process bundles exercise the same bounded adapter and reducer as
// production without contacting Mimir or retaining provider identity fixtures.

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Only generated synthetic process identities and finite metric labels enter
// fixtures. Unknown labels deliberately exercise the private parser boundary.
type egressAdmissionTestProcess struct {
	job, host, block, instance string
	start                      float64
	values                     map[string]float64
	omit                       string
	stale                      string
	duplicate                  string
}

// Gives both roles idle, complete executable capabilities and past counters.
func egressAdmissionTestProcesses(now time.Time) []egressAdmissionTestProcess {
	return []egressAdmissionTestProcess{
		{job: "api", host: "synthetic-api.example", block: "fixture-block", instance: "synthetic-api-process", start: float64(now.Add(-time.Hour).Unix()), values: map[string]float64{"requests": 10, "selected/stale-health/false": 10}},
		{job: "taskworker", host: "synthetic-worker.example", block: "fixture-block", instance: "synthetic-worker-process", start: float64(now.Add(-time.Hour).Unix()), values: map[string]float64{"submission/health/acknowledged": 10, "submission/attempt/acknowledged": 10}},
	}
}

// Underlying timestamps are explicit companions, separate from evaluation time.
func egressAdmissionTestJson(t *testing.T, now time.Time, processes []egressAdmissionTestProcess) string {
	t.Helper()
	series := []map[string]any{}
	for _, process := range processes {
		for _, key := range egressAdmissionKeys(process.job) {
			if key == process.omit {
				continue
			}
			labels := map[string]string{
				"env": "synthetic", "job": process.job, "host": process.host, "block": process.block, "instance": process.instance,
				"ignored_private_label": "synthetic-private-label.example",
			}
			parts := strings.Split(key, "/")
			labels["monitor_egress_family"] = parts[0]
			if parts[0] == "selected" {
				labels["lane"], labels["expired"] = parts[1], parts[2]
			}
			if parts[0] == "submission" {
				labels["kind"], labels["outcome"] = parts[1], parts[2]
			}
			if parts[0] == "full" {
				labels["result"] = parts[1]
			}
			if parts[0] == "pass_error" {
				labels["step"] = parts[1]
			}
			value := process.values[key]
			switch key {
			case "rss":
				value = 1024 * 1024
			case "start":
				value = process.start
			case "enabled":
				value = 1
			}
			for _, part := range []string{"value", "timestamp"} {
				metric := map[string]string{}
				for name, label := range labels {
					metric[name] = label
				}
				metric["monitor_egress_part"] = part
				sample := value
				if part == "timestamp" {
					sample = float64(now.Add(-5 * time.Second).Unix())
					if key == process.stale {
						sample = float64(now.Add(-2 * time.Minute).Unix())
					}
				}
				series = append(series, map[string]any{"metric": metric, "value": []any{now.Unix(), strconv.FormatFloat(sample, 'f', -1, 64)}})
				if key == process.duplicate && part == "value" {
					duplicate := map[string]string{}
					for name, label := range metric {
						duplicate[name] = label
					}
					duplicate["ignored_private_label"] = "synthetic-other-private-label.example"
					series = append(series, map[string]any{"metric": duplicate, "value": []any{now.Unix(), strconv.FormatFloat(sample, 'f', -1, 64)}})
				}
			}
		}
	}
	encoded, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": series}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// Runs the real adapter with one synthetic inventory gateway and asserts cost.
func runEgressAdmissionTest(t *testing.T, signal Signal, now time.Time, body string) Alerts {
	t.Helper()
	calls := 0
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		calls++
		decoded, err := url.QueryUnescape(command)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"--max-time 15", "--max-filesize 2097152", egressAdmissionQuery("synthetic")} {
			if !strings.Contains(decoded, want) {
				t.Fatalf("bounded query lacks %q", want)
			}
		}
		return body, nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "synthetic-metrics.example", Roles: []string{"services"}}}
	settings.Now = func() time.Time { return now }
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("queries = %d, want exactly one", calls)
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "synthetic-api.example", "synthetic-worker.example", "synthetic-api-process", "synthetic-worker-process", "synthetic-private-label.example", "synthetic-other-private-label.example", "fixture-block")
	}
	return alerts
}

func TestEgressAdmissionWarmupAndStableHealthyControl(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	processes := egressAdmissionTestProcesses(now)
	signal := NewEgressAdmissionSignal()
	first := runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, processes))
	unknown := requireAlertClass(t, first, "egress-admission-unobservable")
	for _, want := range []string{"only when executable capability is proved missing", "If capability is present", "instead of redeploying from absent metrics"} {
		if !strings.Contains(unknown.Markdown(), want) {
			t.Fatalf("capability action omitted %q", want)
		}
	}
	now = now.Add(time.Minute)
	processes[0].values["requests"]++
	processes[0].values["selected/stale-health/false"] += 2
	processes[1].values["submission/health/acknowledged"] += 2
	processes[1].values["submission/attempt/acknowledged"] += 2
	alerts := runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, processes))
	if len(alerts) != 0 {
		t.Fatalf("healthy control alerted: %+v", alerts)
	}
	now = now.Add(time.Minute)
	if alerts = runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, processes)); len(alerts) != 0 {
		t.Fatal("a complete idle interval is observable, though not recovery proof")
	}
}

func TestEgressAdmissionMissingMixedResetAndSourceFreshnessFailClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]egressAdmissionTestProcess) []egressAdmissionTestProcess
	}{
		{name: "missing-role", mutate: func(p []egressAdmissionTestProcess) []egressAdmissionTestProcess { return p[:1] }},
		{name: "missing-capability", mutate: func(p []egressAdmissionTestProcess) []egressAdmissionTestProcess { p[0].omit = "enabled"; return p }},
		{name: "missing-fixed-zero", mutate: func(p []egressAdmissionTestProcess) []egressAdmissionTestProcess {
			p[1].omit = "submission/health/unsupported"
			return p
		}},
		{name: "stale-underlying-source", mutate: func(p []egressAdmissionTestProcess) []egressAdmissionTestProcess { p[1].stale = "enabled"; return p }},
		{name: "duplicate-ignored-label", mutate: func(p []egressAdmissionTestProcess) []egressAdmissionTestProcess {
			p[1].duplicate = "submission/health/acknowledged"
			return p
		}},
		{name: "counter-reset", mutate: func(p []egressAdmissionTestProcess) []egressAdmissionTestProcess {
			p[1].values["submission/health/acknowledged"] = 1
			return p
		}},
		{name: "same-label-new-start", mutate: func(p []egressAdmissionTestProcess) []egressAdmissionTestProcess { p[1].start += 1; return p }},
		{name: "new-instance", mutate: func(p []egressAdmissionTestProcess) []egressAdmissionTestProcess {
			p[1].instance = "synthetic-new-generation"
			return p
		}},
		{name: "overlapping-generation", mutate: func(p []egressAdmissionTestProcess) []egressAdmissionTestProcess {
			other := p[1]
			other.instance = "synthetic-overlap"
			return append(p, other)
		}},
		{name: "selection-without-request", mutate: func(p []egressAdmissionTestProcess) []egressAdmissionTestProcess {
			p[0].values["selected/stale-health/true"]++
			return p
		}},
		{name: "impossible-unlocated-expiry", mutate: func(p []egressAdmissionTestProcess) []egressAdmissionTestProcess {
			p[0].values["requests"]++
			p[0].values["selected/no-location/true"]++
			return p
		}},
	} {
		now := time.Unix(1_700_000_000, 0).UTC()
		processes := egressAdmissionTestProcesses(now)
		signal := NewEgressAdmissionSignal()
		runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, processes))
		now = now.Add(time.Minute)
		alerts := runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, test.mutate(processes)))
		if len(alerts) != 1 || alerts[0].Class != "egress-admission-unobservable" {
			t.Fatalf("%s must fail closed without manufactured events: %+v", test.name, alerts)
		}
	}
}

func TestEgressAdmissionExpiredAndAllSubmissionFailuresKeepOwningLimits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	processes := egressAdmissionTestProcesses(now)
	signal := NewEgressAdmissionSignal()
	runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, processes))
	now = now.Add(time.Minute)
	processes[0].values["requests"]++
	processes[0].values["selected/stale-health/true"] += 3
	for _, kind := range []string{"health", "attempt"} {
		for _, outcome := range []string{"unsupported", "canceled", "error_or_unknown"} {
			processes[1].values["submission/"+kind+"/"+outcome]++
		}
	}
	alerts := runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, processes))
	if len(alerts) != 3 {
		t.Fatalf("findings = %d, want expired plus each submission kind", len(alerts))
	}
	expired := requireAlertClass(t, alerts, "egress-admission-expired")
	if expired.Frame != "stale-health" || !strings.Contains(expired.Observed, "expired_selected=3") || !strings.Contains(expired.Markdown(), "not which provider") {
		t.Fatal("expired admission lost its actual-boundary limit")
	}
	for _, want := range []string{"traffic-bearing", "due-request activity", "Quiet zero traffic is not recovery proof"} {
		if !strings.Contains(expired.Markdown(), want) {
			t.Fatalf("expired-selection verification omitted %q", want)
		}
	}
	for _, alert := range alerts {
		if alert.Class == "egress-submission-unacknowledged" {
			for _, want := range []string{"unsupported=1", "canceled=1", "error_or_unknown=1", "acknowledgment was lost", "Quiet zero traffic is not recovery proof"} {
				if !strings.Contains(alert.Markdown(), want) {
					t.Fatalf("outcome evidence lacks %q", want)
				}
			}
		}
	}
}

func TestEgressAdmissionPagesWhenFullProbesFailWithoutAnySubmission(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	processes := egressAdmissionTestProcesses(now)
	signal := NewEgressAdmissionSignal()
	runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, processes))
	now = now.Add(time.Minute)
	processes[1].values["full/attempted"] += 8
	processes[1].values["full/failed"] += 8
	processes[1].values["pass_error/canceled"]++
	alerts := runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, processes))
	alert := requireAlertClass(t, alerts, "egress-full-no-submission")
	if alert.Severity != SeverityPage || alert.Frame != "control-plane" {
		t.Fatalf("full no-submission alert identity = %+v", alert)
	}
	for _, want := range []string{"full_attempted=8", "full_submitted=0", "full_failed=8", "pass_canceled=1", "Tunnel construction alone is not readiness", "durable escrow reservations"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("full no-submission alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if requireAlertClassCount(alerts, "egress-submission-unacknowledged") != 0 {
		t.Fatalf("no reporter call was misreported as an unacknowledged reporter result: %+v", alerts)
	}
}

func TestEgressAdmissionMixedTelemetryPreservesKnownFailure(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	processes := egressAdmissionTestProcesses(now)
	signal := NewEgressAdmissionSignal()
	runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, processes))
	now = now.Add(time.Minute)
	processes[0].omit = "enabled"
	processes[1].values["submission/health/error_or_unknown"]++
	alerts := runEgressAdmissionTest(t, signal, now, egressAdmissionTestJson(t, now, processes))
	requireAlertClass(t, alerts, "egress-admission-unobservable")
	requireAlertClass(t, alerts, "egress-submission-unacknowledged")
}

func TestEgressAdmissionRepeatedScrapeAndLongGapRemainUnobservable(t *testing.T) {
	for _, gap := range []time.Duration{0, egressAdmissionMaxInterval + time.Second} {
		now := time.Unix(1_700_000_000, 0).UTC()
		processes := egressAdmissionTestProcesses(now)
		signal := NewEgressAdmissionSignal()
		body := egressAdmissionTestJson(t, now, processes)
		runEgressAdmissionTest(t, signal, now, body)
		later := now.Add(gap)
		if gap > 0 {
			body = egressAdmissionTestJson(t, later, processes)
		}
		requireAlertClass(t, runEgressAdmissionTest(t, signal, later, body), "egress-admission-unobservable")
	}
}

func TestEgressAdmissionBoundedInvalidResponseAndQueryContract(t *testing.T) {
	query := egressAdmissionQuery("synthetic")
	if strings.Contains(query, `}{`) || !strings.Contains(query, `urnetwork_egress_probe_pass_providers_total{env="synthetic",job=~"taskworker",schedule="full"}`) {
		t.Fatalf("full-pass selector is not one valid, bounded label matcher: %s", query)
	}
	if strings.Count(query, `"monitor_egress_part","timestamp"`) != 9 || strings.Count(query, `"monitor_egress_part","value"`) != 9 {
		t.Fatal("every fixed family needs its source timestamp; query cost changed")
	}
	for _, forbidden := range []string{"provider_id", "client_id", "network_id", "shard", "group_left", "query_range"} {
		if strings.Contains(query, forbidden) {
			t.Fatal("query expanded identity/cardinality scope")
		}
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	for _, body := range []string{
		strings.Repeat("x", egressAdmissionResponseMax+1),
		`{"status":"error","error":"synthetic-private-body.example"}`,
		`{"status":"success","warnings":["synthetic-private-body.example"],"data":{"resultType":"vector","result":[]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[]}}`,
	} {
		alerts := runEgressAdmissionTest(t, NewEgressAdmissionSignal(), now, body)
		alert := requireAlertClass(t, alerts, "egress-admission-unobservable")
		requireAlertOmits(t, alert, "synthetic-private-body.example")
	}
	if _, err := parseEgressAdmission(strings.Repeat("x", egressAdmissionResponseMax+1), "synthetic", now); err == nil {
		t.Fatal("in-process byte guard missing")
	}
}
