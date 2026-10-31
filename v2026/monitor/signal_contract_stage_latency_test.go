package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A synthetic fleet: one api, connect, proxy and taskworker slot, each with a
// fresh capability gauge, and the 2026-10-22 06:30Z stage means.
type contractStageFixture struct {
	at       time.Time
	stages   map[string]float64
	outcomes map[string]float64
	scalars  map[string]float64
	enabled  map[string]float64
	fresh    map[string]float64
	extra    []map[string]any
}

var contractStageFixtureSlots = []fleetSlot{
	{service: "api", host: "api-a.example", block: "g1"},
	{service: "connect", host: "connect-a.example", block: "g1"},
	{service: "proxy", host: "proxy-a.example", block: "g1"},
	{service: "taskworker", host: "worker-a.example", block: "g1"},
}

func newContractStageFixture(now time.Time) *contractStageFixture {
	at := fleetEvaluationTime(now, DefaultContractStageLatencySettings().EvaluationLag)
	replies := 10000.0
	stagesMs := map[string]float64{
		"transaction": 317, "relationship": 221, "metadata": 175, "endpoint_lookup": 163,
		"client_stamp": 151, "other": 122, "companion_origin": 62, "grant_selection": 32,
	}
	fixture := &contractStageFixture{
		at: at, stages: map[string]float64{}, outcomes: map[string]float64{"contract_reply": replies, "canceled": 400, "error": 20},
		scalars: map[string]float64{"hold_mean": 1.35, "hold_points": 10, "hold_expected": 10, "control_mean": 0.16, "pgbouncer_waiting": 900},
		enabled: map[string]float64{}, fresh: map[string]float64{},
	}
	for stage, milliseconds := range stagesMs {
		fixture.stages[stage] = milliseconds / 1000 * replies
	}
	for _, slot := range contractStageFixtureSlots {
		key := slot.service + "/" + slot.host + "/" + slot.block
		fixture.enabled[key] = 1
		fixture.fresh[key] = float64(at.Add(-20 * time.Second).Unix())
	}
	return fixture
}

func (self *contractStageFixture) json(t testing.TB) string {
	t.Helper()
	rows := []map[string]any{}
	value := func(v float64) string {
		switch {
		case math.IsNaN(v):
			return "NaN"
		case math.IsInf(v, 1):
			return "+Inf"
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	add := func(labels map[string]string, v float64) {
		rows = append(rows, map[string]any{"metric": labels, "value": []any{float64(self.at.Unix()), value(v)}})
	}
	for stage, v := range self.stages {
		add(map[string]string{fleetPartLabel: "stage_seconds", "stage": stage}, v)
	}
	for outcome, v := range self.outcomes {
		add(map[string]string{fleetPartLabel: "outcomes", "outcome": outcome}, v)
	}
	for part, v := range self.scalars {
		add(map[string]string{fleetPartLabel: part}, v)
	}
	for key, v := range self.enabled {
		parts := strings.Split(key, "/")
		add(map[string]string{fleetPartLabel: "enabled_value", "job": parts[0], "host": parts[1], "block": parts[2]}, v)
	}
	for key, v := range self.fresh {
		parts := strings.Split(key, "/")
		add(map[string]string{fleetPartLabel: "fresh/enabled", "job": parts[0], "host": parts[1], "block": parts[2]}, v)
	}
	for _, row := range self.extra {
		rows = append(rows, row)
	}
	encoded, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": rows}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// Synthetic inventory: a services gateway plus one host per producing service.
func contractStageSettings(now time.Time, source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	settings.LogServices = []string{}
	settings.LogServiceHosts = map[string][]string{}
	settings.LogServiceBlocks = map[string][]string{}
	for _, slot := range contractStageFixtureSlots {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: slot.host, Roles: []string{"edge"}})
		settings.LogServices = append(settings.LogServices, slot.service)
		settings.LogServiceHosts[slot.service] = []string{slot.host}
		settings.LogServiceBlocks[slot.service] = []string{slot.block}
	}
	return settings
}

func runContractStageFixture(t *testing.T, now time.Time, payload string, mutate func(*SignalSettings)) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" {
			t.Fatalf("fleet read contacted a non-gateway host")
		}
		return payload, nil
	}}
	settings := contractStageSettings(now, source)
	if mutate != nil {
		mutate(&settings)
	}
	alerts, err := NewContractStageLatencySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

var contractStageNow = time.Date(2026, 10, 22, 6, 32, 0, 0, time.UTC)

func TestContractStageLatencyPagesWhenEveryStageSlowsTogether(t *testing.T) {
	fixture := newContractStageFixture(contractStageNow)
	alerts := runContractStageFixture(t, contractStageNow, fixture.json(t), nil)
	alert := requireAlertClass(t, alerts, "contract-stage-latency")
	if len(alerts) != 1 || alert.Severity != SeverityPage || alert.Sustain != 1 || alert.Target != "synthetic/contract-creation" || alert.Frame != "" {
		t.Fatalf("incident alert = %+v", alerts)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"hold_min_mean_ms=1350.0", "transaction=317.0ms", "relationship=221.0ms", "concentrated_stage=none",
		"pool queueing", "control_route_mean_ms=160.0", "pgbouncer_clients_waiting=900",
		"expected_slots=4 complete_slots=4", "do not restart Connect",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("incident alert lacks %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "query or plan problem in that stage, not pool saturation") {
		t.Fatal("a fleet-wide slowdown was attributed to one stage")
	}
}

func TestContractStageLatencyBandEdges(t *testing.T) {
	for _, c := range []struct {
		hold     float64
		severity Severity
	}{
		{hold: 0.05, severity: ""},
		{hold: 0.25, severity: ""},
		{hold: 0.2501, severity: SeverityWarn},
		{hold: 1.0, severity: SeverityWarn},
		{hold: 1.0001, severity: SeverityPage},
	} {
		fixture := newContractStageFixture(contractStageNow)
		fixture.scalars["hold_mean"] = c.hold
		alerts := runContractStageFixture(t, contractStageNow, fixture.json(t), nil)
		if c.severity == "" {
			if len(alerts) != 0 {
				t.Fatalf("hold %.4f s alerted: %+v", c.hold, alerts)
			}
			continue
		}
		alert := requireAlertClass(t, alerts, "contract-stage-latency")
		if len(alerts) != 1 || alert.Severity != c.severity {
			t.Fatalf("hold %.4f s = %+v, want %s", c.hold, alerts, c.severity)
		}
	}
}

// Concentration identifies the application stage, not a query or pool cause.
func TestContractStageLatencyNamesConcentratedStage(t *testing.T) {
	fixture := newContractStageFixture(contractStageNow)
	fixture.stages = map[string]float64{"companion_origin": 0.3 * 10000, "relationship": 0.02 * 10000, "other": 0.008 * 10000}
	fixture.scalars["hold_mean"] = 0.32
	fixture.scalars["pgbouncer_waiting"] = 0
	alert := requireAlertClass(t, runContractStageFixture(t, contractStageNow, fixture.json(t), nil), "contract-stage-latency")
	if alert.Severity != SeverityWarn || !strings.Contains(alert.Markdown(), "concentrated_stage=companion_origin") ||
		!strings.Contains(alert.Mechanism, "Stage totals alone do not identify a query, pool, host or network cause.") {
		t.Fatalf("concentrated stage diagnosis lost:\n%s", alert.Markdown())
	}
	if strings.Contains(alert.Mechanism, "Every database stage of CreateContract slowed together") {
		t.Fatal("one slow stage was reported as pool queueing")
	}
}

// Calls that return without any reply hold an unbounded mean: that pages.
func TestContractStageLatencyNoRepliesPages(t *testing.T) {
	fixture := newContractStageFixture(contractStageNow)
	fixture.outcomes = map[string]float64{"canceled": 4000, "contract_reply": 0}
	fixture.scalars["hold_mean"] = math.Inf(1)
	alert := requireAlertClass(t, runContractStageFixture(t, contractStageNow, fixture.json(t), nil), "contract-stage-latency")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), "hold_min_mean_ms=unbounded") {
		t.Fatalf("returned calls without replies did not page:\n%s", alert.Markdown())
	}
}

// Unknown never certifies a fast mean: each control reports visibility, and
// only a proven slow hold is also an incident.
func TestContractStageLatencyUnknownControls(t *testing.T) {
	for _, c := range []struct {
		name     string
		mutate   func(*contractStageFixture)
		incident bool
		reason   string
	}{
		{name: "hold incomplete", mutate: func(f *contractStageFixture) { f.scalars["hold_points"] = 9 }, reason: "hold_incomplete"},
		{name: "hold expected missing", mutate: func(f *contractStageFixture) { delete(f.scalars, "hold_expected") }, reason: "hold_incomplete"},
		{name: "idle hold", mutate: func(f *contractStageFixture) { f.scalars["hold_mean"] = math.NaN() }, reason: "hold_mean_unknown"},
		{name: "few calls", mutate: func(f *contractStageFixture) {
			f.outcomes = map[string]float64{"contract_reply": 60, "canceled": 39}
		}, reason: "insufficient_returned_calls"},
		{name: "no stage series", mutate: func(f *contractStageFixture) { f.stages = map[string]float64{} }, reason: "stage_series_missing"},
		{name: "unknown label", mutate: func(f *contractStageFixture) { f.stages["Hostile Stage"] = 1 }, reason: "unrecognized_label"},
		{name: "negative counter", mutate: func(f *contractStageFixture) { f.stages["metadata"] = -1 }, reason: "invalid_value"},
		{name: "missing slot slow", mutate: func(f *contractStageFixture) {
			delete(f.fresh, "proxy/proxy-a.example/g1")
		}, incident: true, reason: "slot_coverage_incomplete"},
		{name: "missing slot fast", mutate: func(f *contractStageFixture) {
			delete(f.fresh, "proxy/proxy-a.example/g1")
			f.scalars["hold_mean"] = 0.05
		}, reason: "slot_coverage_incomplete"},
		{name: "stale slot", mutate: func(f *contractStageFixture) {
			f.fresh["api/api-a.example/g1"] = float64(f.at.Add(-4 * time.Minute).Unix())
			f.scalars["hold_mean"] = 0.05
		}, reason: "slot_coverage_incomplete"},
		{name: "capability off", mutate: func(f *contractStageFixture) {
			f.enabled["connect/connect-a.example/g1"] = 0
			f.scalars["hold_mean"] = 0.05
		}, reason: "slot_coverage_incomplete"},
	} {
		fixture := newContractStageFixture(contractStageNow)
		c.mutate(fixture)
		alerts := runContractStageFixture(t, contractStageNow, fixture.json(t), nil)
		unknown := requireAlertClass(t, alerts, "contract-stage-latency-unobservable")
		if !unknown.unobservable || !strings.Contains(unknown.Observed, c.reason) {
			t.Fatalf("%s: unknown alert lacks %s: %+v", c.name, c.reason, unknown)
		}
		incidents := requireAlertClassCount(alerts, "contract-stage-latency")
		if (incidents == 1) != c.incident || len(alerts) != 1+incidents {
			t.Fatalf("%s: alerts = %+v", c.name, alerts)
		}
	}
}

func TestContractStageLatencyExcludedHostIsAGap(t *testing.T) {
	fixture := newContractStageFixture(contractStageNow)
	fixture.scalars["hold_mean"] = 0.05
	alerts := runContractStageFixture(t, contractStageNow, fixture.json(t), func(settings *SignalSettings) {
		settings.ExcludedHosts = []string{"worker-a.example"}
	})
	unknown := requireAlertClass(t, alerts, "contract-stage-latency-unobservable")
	if len(alerts) != 1 || !strings.Contains(unknown.Observed, "taskworker/worker-a.example/g1[excluded]") {
		t.Fatalf("excluded slot was not a gap: %+v", alerts)
	}
}

func TestContractStageLatencySourceFailuresAreUnknown(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload string
		err     error
	}{
		{name: "transport", err: errors.New("synthetic gateway refused")},
		{name: "json", payload: "not json"},
		{name: "error status", payload: `{"status":"error","error":"synthetic"}`},
		{name: "warnings", payload: `{"status":"success","warnings":["synthetic"],"data":{"resultType":"vector","result":[]}}`},
		{name: "wrong time", payload: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"monitor_part":"hold_mean"},"value":[1,"0.01"]}]}}`},
		{name: "untagged", payload: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"0.01"]}]}}`},
	} {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return c.payload, c.err }}
		alerts, err := NewContractStageLatencySignal().Run(context.Background(), contractStageSettings(contractStageNow, source))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		unknown := requireAlertClass(t, alerts, "contract-stage-latency-unobservable")
		if len(alerts) != 1 {
			t.Fatalf("%s: alerts = %+v", c.name, alerts)
		}
		requireAlertOmits(t, unknown, "synthetic gateway refused")
	}
}

// The single read is bounded, POSTed, evaluated behind now, and asks Mimir
// for the hold rather than learning it in process.
func TestContractStageLatencyQueryShape(t *testing.T) {
	commands := []string{}
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		commands = append(commands, command)
		return newContractStageFixture(contractStageNow).json(t), nil
	}}
	if _, err := NewContractStageLatencySignal().Run(context.Background(), contractStageSettings(contractStageNow, source)); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 {
		t.Fatalf("fleet read ran %d commands", len(commands))
	}
	command := commands[0]
	at := contractStageNow.Add(-2 * time.Minute).Unix()
	for _, want := range []string{
		"--max-time 15", "--max-filesize 1048576", "time=" + strconv.FormatInt(at, 10),
		"min_over_time((sum(rate(urnetwork_contract_creation_completed_stage_seconds_total{env=\"synthetic\"}[300s])) / sum(rate(urnetwork_contract_creation_completed_total{env=\"synthetic\",outcome=\"contract_reply\"}[300s])))[600s:60s])",
		"count_over_time(vector(1)[600s:60s])",
		"urnetwork_contract_creation_stage_timing_enabled{env=\"synthetic\",job=~\"api|connect|proxy|taskworker\"",
	} {
		if !strings.Contains(command, want) {
			unescaped, _ := url.QueryUnescape(command)
			t.Fatalf("query lacks %q:\n%s", want, unescaped)
		}
	}
}

func TestContractStageLatencyCanceledParentPublishesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		cancel()
		return "", context.Canceled
	}}
	alerts, err := NewContractStageLatencySignal().Run(ctx, contractStageSettings(contractStageNow, source))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("canceled run = %+v, %v", alerts, err)
	}
}
