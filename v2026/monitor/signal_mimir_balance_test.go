// Synthetic Mimir balance fixtures use documentation-only addresses and
// generated identities; production peers, hostnames, and tenant labels never
// enter test source or alert assertions.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	mimirBalanceSyntheticPort = 19191
	mimirBalanceSyntheticRate = 900.0
)

type mimirBalanceSyntheticTotals struct {
	samples  int64
	received int64
	requests int64
}

func mimirBalanceSyntheticFrame(
	processStart string,
	totals mimirBalanceSyntheticTotals,
	localConnections int64,
	remoteConnections int64,
) string {
	return fmt.Sprintf(
		"instance_begin %d\n"+
			"observable 1\n"+
			"process_start %s\n"+
			"samples_in_total %d\n"+
			"received_total %d\n"+
			"requests_in_total %d\n"+
			"active_distributors 3\n"+
			"ingestion_rate_limit %.0f\n"+
			"instance_end 1\n"+
			"mimir_count 1\n"+
			"front_local_connections %d\n"+
			"front_remote_connections %d\n",
		mimirBalanceSyntheticPort,
		processStart,
		totals.samples,
		totals.received,
		totals.requests,
		mimirBalanceSyntheticRate,
		localConnections,
		remoteConnections,
	)
}

func runMimirBalanceSynthetic(
	t *testing.T,
	signal Signal,
	stateDir string,
	now time.Time,
	frames map[string]string,
) []Alert {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if !strings.Contains(command, mimirBalanceMarker) ||
			!strings.Contains(command, "cortex_distributor_samples_in_total") ||
			!strings.Contains(command, "front_remote_connections") {
			return "", fmt.Errorf("synthetic command omitted a bounded balance source")
		}
		frame, ok := frames[host.Name]
		if !ok {
			return "", fmt.Errorf("unexpected synthetic host")
		}
		return frame, nil
	}}
	settings := syntheticSettings(source)
	settings.StateDir = stateDir
	settings.Now = func() time.Time { return now }
	settings.Hosts = []HostSettings{
		{Name: "metrics-a.invalid", Roles: []string{"services"}},
		{Name: "metrics-b.invalid", Roles: []string{"services"}},
		{Name: "metrics-c.invalid", Roles: []string{"services"}},
	}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func mimirBalanceSyntheticFleet(
	processStart string,
	base map[string]mimirBalanceSyntheticTotals,
	deltas map[string]mimirBalanceSyntheticTotals,
	remote map[string]int64,
) map[string]string {
	frames := map[string]string{}
	for host, totals := range base {
		delta := deltas[host]
		frames[host] = mimirBalanceSyntheticFrame(
			processStart,
			mimirBalanceSyntheticTotals{
				samples:  totals.samples + delta.samples,
				received: totals.received + delta.received,
				requests: totals.requests + delta.requests,
			},
			20,
			remote[host],
		)
	}
	return frames
}

func TestMimirBalanceSignalDetectsUnderBudgetDistributorSkew(t *testing.T) {
	signal := NewMimirBalanceSignal()
	if signal.Number() != "11.20b" || signal.Key() != "mimir-balance" ||
		signal.ID() != "observability/mimir-balance" || signal.Cadence() != time.Minute {
		t.Fatalf("wrong signal metadata: %s %s %s %s", signal.Number(), signal.Key(), signal.ID(), signal.Cadence())
	}
	stateDir := t.TempDir()
	start := time.Date(2032, 3, 4, 5, 6, 0, 0, time.UTC)
	processStart := "1956789000.125"
	base := map[string]mimirBalanceSyntheticTotals{
		"metrics-a.invalid": {samples: 100000, received: 90000, requests: 1000},
		"metrics-b.invalid": {samples: 200000, received: 190000, requests: 2000},
		"metrics-c.invalid": {samples: 300000, received: 290000, requests: 3000},
	}
	zero := map[string]mimirBalanceSyntheticTotals{}
	if alerts := runMimirBalanceSynthetic(
		t, signal, stateDir, start,
		mimirBalanceSyntheticFleet(processStart, base, zero, map[string]int64{}),
	); len(alerts) != 0 {
		t.Fatalf("initial baseline alerted: %+v", alerts)
	}

	// Over one minute, the fleet attempts 850 samples/s against a 900/s global
	// budget. One child receives 400/s against its 300/s local share and accepts
	// only 300/s; the other two retain unused capacity.
	deltas := map[string]mimirBalanceSyntheticTotals{
		"metrics-a.invalid": {samples: 24000, received: 18000, requests: 120},
		"metrics-b.invalid": {samples: 15000, received: 15000, requests: 60},
		"metrics-c.invalid": {samples: 12000, received: 12000, requests: 30},
	}
	alerts := runMimirBalanceSynthetic(
		t, signal, stateDir, start.Add(time.Minute),
		mimirBalanceSyntheticFleet(
			processStart, base, deltas,
			map[string]int64{"metrics-a.invalid": 18},
		),
	)
	alert := requireAlertClass(t, alerts, "mimir-distributor-skew")
	if alert.Severity != SeverityWarn || alert.Sustain != 2 || alert.Target != "mimir-fleet" {
		t.Fatalf("wrong skew alert identity: %+v", alert)
	}
	for _, required := range []string{
		"fleet_attempted_samples_per_s=850.0",
		"fleet_accepted_samples_per_s=750.0",
		"inferred_unaccepted_samples_per_s=100.0",
		"effective_child_rate=300.0..300.0",
		"child_attempted_rate=200.0..400.0",
		"overloaded_instances=1",
		"remote_front_connections=18",
		"overloaded_remote_front_connections=18",
		"SIGNALS.md §11.20b",
	} {
		if !strings.Contains(alert.Markdown(), required) {
			t.Errorf("skew alert lacks %q:\n%s", required, alert.Markdown())
		}
	}
	requireAlertOmits(t, alert, "metrics-a.invalid", processStart)
}

func TestMimirBalanceSignalDoesNotMisclassifyAggregateCapacity(t *testing.T) {
	stateDir := t.TempDir()
	signal := NewMimirBalanceSignal()
	start := time.Date(2032, 3, 4, 6, 7, 0, 0, time.UTC)
	processStart := "1956792600.25"
	base := map[string]mimirBalanceSyntheticTotals{
		"metrics-a.invalid": {samples: 100000, received: 100000, requests: 1000},
		"metrics-b.invalid": {samples: 200000, received: 200000, requests: 2000},
		"metrics-c.invalid": {samples: 300000, received: 300000, requests: 3000},
	}
	runMimirBalanceSynthetic(
		t, signal, stateDir, start,
		mimirBalanceSyntheticFleet(processStart, base, nil, nil),
	)
	aggregateOverload := map[string]mimirBalanceSyntheticTotals{
		"metrics-a.invalid": {samples: 24000, received: 18000, requests: 120},
		"metrics-b.invalid": {samples: 18000, received: 18000, requests: 60},
		"metrics-c.invalid": {samples: 15000, received: 15000, requests: 30},
	}
	if alerts := runMimirBalanceSynthetic(
		t, signal, stateDir, start.Add(time.Minute),
		mimirBalanceSyntheticFleet(processStart, base, aggregateOverload, nil),
	); len(alerts) != 0 {
		t.Fatalf("aggregate capacity pressure was mislabeled as balance-only: %+v", alerts)
	}
}

// The balance class owns persistent remote-write placement. A loopback-only
// overload can be a local query/maintenance cohort, but cannot establish a
// remote publisher routing fault or justify reconnecting remote shippers.
func TestMimirBalanceSignalDoesNotMisclassifyLoopbackOnlyOverload(t *testing.T) {
	stateDir := t.TempDir()
	signal := NewMimirBalanceSignal()
	start := time.Date(2032, 3, 4, 6, 17, 0, 0, time.UTC)
	processStart := "1956793200.5"
	base := map[string]mimirBalanceSyntheticTotals{
		"metrics-a.invalid": {samples: 100000, received: 100000, requests: 1000},
		"metrics-b.invalid": {samples: 200000, received: 200000, requests: 2000},
		"metrics-c.invalid": {samples: 300000, received: 300000, requests: 3000},
	}
	runMimirBalanceSynthetic(t, signal, stateDir, start, mimirBalanceSyntheticFleet(processStart, base, nil, nil))
	deltas := map[string]mimirBalanceSyntheticTotals{
		"metrics-a.invalid": {samples: 24000, received: 18000, requests: 120},
		"metrics-b.invalid": {samples: 15000, received: 15000, requests: 60},
		"metrics-c.invalid": {samples: 12000, received: 12000, requests: 30},
	}
	if alerts := runMimirBalanceSynthetic(t, signal, stateDir, start.Add(time.Minute), mimirBalanceSyntheticFleet(processStart, base, deltas, nil)); len(alerts) != 0 {
		t.Fatalf("loopback-only overload was mislabeled as remote publisher skew: %+v", alerts)
	}
}

func TestMimirBalanceSignalPersistsBaselineAcrossWatcherReplacement(t *testing.T) {
	stateDir := t.TempDir()
	start := time.Date(2032, 3, 4, 7, 8, 0, 0, time.UTC)
	processStart := "1956796200.5"
	base := map[string]mimirBalanceSyntheticTotals{
		"metrics-a.invalid": {samples: 100000, received: 90000, requests: 1000},
		"metrics-b.invalid": {samples: 200000, received: 190000, requests: 2000},
		"metrics-c.invalid": {samples: 300000, received: 290000, requests: 3000},
	}
	runMimirBalanceSynthetic(
		t, NewMimirBalanceSignal(), stateDir, start,
		mimirBalanceSyntheticFleet(processStart, base, nil, nil),
	)
	deltas := map[string]mimirBalanceSyntheticTotals{
		"metrics-a.invalid": {samples: 24000, received: 18000, requests: 60},
		"metrics-b.invalid": {samples: 15000, received: 15000, requests: 60},
		"metrics-c.invalid": {samples: 12000, received: 12000, requests: 60},
	}
	alerts := runMimirBalanceSynthetic(
		t, NewMimirBalanceSignal(), stateDir, start.Add(time.Minute),
		mimirBalanceSyntheticFleet(processStart, base, deltas, map[string]int64{"metrics-a.invalid": 1}),
	)
	requireAlertClass(t, alerts, "mimir-distributor-skew")
}

func TestMimirBalanceParserFailsClosedOnCounterReset(t *testing.T) {
	stateDir := t.TempDir()
	signal := NewMimirBalanceSignal()
	start := time.Date(2032, 3, 4, 8, 9, 0, 0, time.UTC)
	processStart := "1956799800.75"
	base := map[string]mimirBalanceSyntheticTotals{
		"metrics-a.invalid": {samples: 100000, received: 90000, requests: 1000},
		"metrics-b.invalid": {samples: 200000, received: 190000, requests: 2000},
		"metrics-c.invalid": {samples: 300000, received: 290000, requests: 3000},
	}
	runMimirBalanceSynthetic(
		t, signal, stateDir, start,
		mimirBalanceSyntheticFleet(processStart, base, nil, nil),
	)
	reset := map[string]mimirBalanceSyntheticTotals{}
	for host, totals := range base {
		reset[host] = totals
	}
	reset["metrics-a.invalid"] = mimirBalanceSyntheticTotals{samples: 1, received: 1, requests: 1}
	frames := map[string]string{}
	for host, totals := range reset {
		frames[host] = mimirBalanceSyntheticFrame(processStart, totals, 1, 0)
	}
	alert := requireAlertClass(
		t,
		runMimirBalanceSynthetic(t, signal, stateDir, start.Add(time.Minute), frames),
		"cannot-observe",
	)
	if !strings.Contains(alert.Markdown(), "error_class="+observationErrorClassCounterReset) {
		t.Fatalf("counter reset cause was lost: %s", alert.Markdown())
	}
	requireAlertOmits(t, alert, "monotonic distributor counter decreased")
}

func TestMimirBalanceScriptReducesMetricsAndSocketPeers(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable := func(name string, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable("ss", `#!/bin/sh
case "$*" in
  *-ltnH*)
    printf '%s\n' 'LISTEN 0 4096 127.0.0.1:19191 198.51.100.2:*'
    ;;
  *)
    printf '%s\n' \
      '0 0 127.0.0.1:3100 127.0.0.1:47001' \
      '0 0 192.0.2.10:3100 198.51.100.20:47002' \
      '0 0 192.0.2.10:3100 [2001:db8::20]:47003'
    ;;
esac
`)
	writeExecutable("curl", `#!/bin/sh
case "$*" in
  *'/api/v1/status/buildinfo')
    printf '%s\n' '{"application":"Grafana Mimir","tenant":"generated-private-tenant"}'
    ;;
  *'/config')
    printf '%s\n' 'limits:' '  ingestion_rate: 900' 'private: generated-config-value'
    ;;
  *'/metrics')
    printf '%s\n' \
      'process_start_time_seconds 1956803400.125' \
      '# HELP cortex_distributor_samples_in_total Synthetic attempted samples.' \
      '# TYPE cortex_distributor_samples_in_total counter' \
      'cortex_distributor_samples_in_total{user="generated-private-tenant"} 125000' \
      '# HELP cortex_distributor_received_samples_total Synthetic accepted samples.' \
      '# TYPE cortex_distributor_received_samples_total counter' \
      'cortex_distributor_received_samples_total{user="generated-private-tenant"} 120000' \
      '# HELP cortex_distributor_requests_in_total Synthetic requests.' \
      '# TYPE cortex_distributor_requests_in_total counter' \
      'cortex_distributor_requests_in_total{user="generated-private-tenant",version="1.0"} 2500' \
      'cortex_ring_members{name="distributor",state="ACTIVE"} 3'
    ;;
  *) exit 2 ;;
esac
`)

	command := exec.Command("sh", "-c", mimirBalanceCommand())
	command.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Mimir balance reducer: %v\n%s", err, output)
	}
	sample, err := parseMimirBalanceHostSample(string(output))
	if err != nil {
		t.Fatalf("parse reducer output: %v\n%s", err, output)
	}
	if sample.count != 1 || len(sample.instances) != 1 || sample.localConnections != 1 ||
		sample.remoteConnections != 2 {
		t.Fatalf("reducer lost bounded host values: %+v\n%s", sample, output)
	}
	instance := sample.instances[0]
	if !instance.observable || instance.samplesInTotal != 125000 || instance.receivedTotal != 120000 ||
		instance.requestsInTotal != 2500 || instance.activeDistributors != 3 ||
		instance.ingestionRateLimit != mimirBalanceSyntheticRate {
		t.Fatalf("reducer lost bounded child values: %+v\n%s", instance, output)
	}
	for _, forbidden := range []string{
		"generated-private-tenant",
		"generated-config-value",
		"198.51.100.20",
		"2001:db8::20",
	} {
		if strings.Contains(string(output), forbidden) {
			t.Errorf("reducer leaked synthetic private source %q: %s", forbidden, output)
		}
	}
}

func TestMimirBalanceSignalIsUniquelyRegistered(t *testing.T) {
	selected, err := IncludeSignals(
		NewSignals(),
		"11.20b",
		"mimir-balance",
		"observability/mimir-balance",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].Key() != "mimir-balance" {
		t.Fatalf("Mimir balance registry selection = %+v", selected)
	}
}

// Diagnostic controls use complete synthetic children; no production state,
// peer, label, or command error is needed to distinguish these source branches.
const mimirBalanceDiagnosticProcess = "2000000001"
const mimirBalanceDiagnosticPrivate = "synthetic-private-mimir credential=synthetic-secret tenant=synthetic-tenant peer=192.0.2.77 url=https://synthetic-user:synthetic-password@example.invalid/private\nobservation_stage=comparison-stale"

func mimirBalanceDiagnosticFrames(process string, step int64) map[string]string {
	frames := map[string]string{}
	for _, name := range []string{"metrics-a.invalid", "metrics-b.invalid", "metrics-c.invalid"} {
		frames[name] = mimirBalanceSyntheticFrame(process, mimirBalanceSyntheticTotals{
			samples: 100000 + 6000*step, received: 90000 + 6000*step, requests: 1000 + 60*step,
		}, 20, 1)
	}
	return frames
}

func runMimirBalanceDiagnostic(t *testing.T, stateDir string, now time.Time, frames map[string]string, failures map[string]error) Alerts {
	t.Helper()
	if len(failures) == 0 {
		return runMimirBalanceSynthetic(t, NewMimirBalanceSignal(), stateDir, now, frames)
	}
	source := &syntheticSource{hostFn: func(target HostSettings, command string) (string, error) {
		if !strings.Contains(command, mimirBalanceMarker) {
			return "", fmt.Errorf("unexpected synthetic diagnostic command")
		}
		return frames[target.Name], failures[target.Name]
	}}
	settings := syntheticSettings(source)
	settings.StateDir, settings.Now = stateDir, func() time.Time { return now }
	settings.Hosts = []HostSettings{
		{Name: "metrics-a.invalid", Roles: []string{"services"}},
		{Name: "metrics-b.invalid", Roles: []string{"services"}},
		{Name: "metrics-c.invalid", Roles: []string{"services"}},
	}
	alerts, err := NewMimirBalanceSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("synthetic public diagnostic run failed")
	}
	return alerts
}

func mimirBalanceDiagnosticState(t *testing.T, stateDir string) mimirBalancePersistedState {
	t.Helper()
	var state mimirBalancePersistedState
	loaded, err := loadProviderState(stateDir, "mimir-balance", mimirBalanceStateVersion, &state)
	if err != nil || !loaded {
		t.Fatal("synthetic diagnostic history was not persisted")
	}
	return state
}

func assessMimirBalanceDiagnostic(t *testing.T, now time.Time, state mimirBalancePersistedState, frames map[string]string, failures map[string]error) (mimirBalanceAssessment, mimirBalancePersistedState) {
	t.Helper()
	results := []mimirBalanceHostResult{}
	for _, name := range []string{"metrics-a.invalid", "metrics-b.invalid", "metrics-c.invalid"} {
		result := mimirBalanceHostResult{host: &host{name: name}, err: failures[name]}
		if result.err == nil {
			result.sample, result.err = parseMimirBalanceHostSample(frames[name])
		}
		results = append(results, result)
	}
	return assessMimirBalance(now, state, results)
}

func requireMimirBalanceDiagnostic(t *testing.T, alert Alert, target, stage, errorClass string) {
	t.Helper()
	if alert.Identity() != "monitor/visibility|cannot-observe|"+target+"|" || alert.Severity != SeverityWarn || alert.Sustain != 2 || alert.PageSustain != 0 {
		t.Error("diagnostic changed visibility identity, severity, or sustain")
	}
	if alert.SignalNumber != "11.20b" || alert.SignalKey != "mimir-balance" {
		t.Error("diagnostic lost its signal owner")
	}
	structured, err := json.Marshal(alert)
	if err != nil {
		t.Fatal(err)
	}
	var jsonl strings.Builder
	if err := (Alerts{alert}).WriteJSONL(&jsonl); err != nil {
		t.Fatal(err)
	}
	for format, output := range map[string]string{"Alert": string(structured), "Markdown": alert.Markdown(), "JSONL": jsonl.String()} {
		if !strings.Contains(output, "observation_stage="+stage) || !strings.Contains(output, "error_class="+errorClass) {
			t.Errorf("%s omitted the fixed stage or existing error class", format)
		}
		for index, forbidden := range []string{
			"synthetic-private-mimir", "synthetic-secret", "synthetic-tenant", "192.0.2.77",
			"synthetic-user", "synthetic-password", "example.invalid", mimirBalanceDiagnosticProcess,
			fmt.Sprint(mimirBalanceSyntheticPort), fmt.Sprint(mimirBalanceSyntheticPort + 1),
		} {
			if strings.Contains(output, forbidden) {
				t.Errorf("%s leaked synthetic private component %d", format, index)
			}
		}
	}
	if !strings.Contains(alert.Context, "first") || !strings.Contains(alert.Context, "additional") {
		t.Error("diagnostic omitted first-failure/additional-failures qualification")
	}
}

func TestMimirBalanceDiagnosticUnavailableIntervals(t *testing.T) {
	for _, test := range []struct {
		name, stage string
		elapsed     time.Duration
	}{
		{"zero", "comparison-nonadvancing", 0},
		{"negative", "comparison-nonadvancing", -time.Nanosecond},
		{"stale", "comparison-stale", 3*time.Minute + time.Nanosecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			start := time.Date(2032, 4, 5, 6, 7, 0, 0, time.UTC)
			if alerts := runMimirBalanceDiagnostic(t, dir, start, mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 0), nil); len(alerts) != 0 {
				t.Fatal("first complete observation should only arm the baseline")
			}
			before := mimirBalanceDiagnosticState(t, dir)
			now := start.Add(test.elapsed)
			frames := mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 1)
			assessment, next := assessMimirBalanceDiagnostic(t, now, before, frames, nil)
			if !assessment.directComplete || assessment.rateComplete || assessment.comparableInstances != 0 || mimirBalanceIsSkewed(assessment) {
				t.Error("unavailable interval became a complete or skew comparison")
			}
			alerts := runMimirBalanceDiagnostic(t, dir, now, frames, nil)
			if len(alerts) != 3 {
				t.Fatalf("want one visibility observation per host, got %d", len(alerts))
			}
			unseen := map[string]bool{"metrics-a.invalid/mimir-balance": true, "metrics-b.invalid/mimir-balance": true, "metrics-c.invalid/mimir-balance": true}
			for _, alert := range alerts {
				if !unseen[alert.Target] {
					t.Error("unavailable comparison repeated or changed a host target")
				}
				delete(unseen, alert.Target)
				requireMimirBalanceDiagnostic(t, alert, alert.Target, test.stage, observationErrorClassUnclassified)
				if !strings.Contains(alert.Mechanism, "comparison") || strings.Contains(alert.Mechanism, "network path failed") {
					t.Error("unavailable comparison was diagnosed as a source/path failure")
				}
				if !strings.Contains(alert.Action, "next") || !strings.Contains(alert.Action, "baseline") || strings.Contains(alert.Action, "Restore the observation path") {
					t.Error("comparison action did not preserve unknown/next-baseline semantics")
				}
			}
			after := mimirBalanceDiagnosticState(t, dir)
			if !reflect.DeepEqual(after, next) || len(after.Histories) != 3 {
				t.Fatal("public run did not persist the assessed current snapshots")
			}
			for _, history := range after.Histories {
				if history.ObservedUnixNS != now.UnixNano() || history.SamplesInTotal != 106000 || history.ReceivedTotal != 96000 || history.RequestsInTotal != 1060 {
					t.Error("unavailable interval failed to install the valid current baseline")
				}
			}
			following := mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 2)
			assessment, _ = assessMimirBalanceDiagnostic(t, now.Add(time.Minute), after, following, nil)
			if !assessment.directComplete || !assessment.rateComplete || assessment.comparableInstances != 3 || assessment.fleetAttemptedRate != 300 {
				t.Error("next valid minute did not compare against the newly armed baseline")
			}
			if alerts := runMimirBalanceDiagnostic(t, dir, now.Add(time.Minute), following, nil); len(alerts) != 0 {
				t.Error("next complete balanced comparison remained unavailable")
			}
		})
	}
}

func TestMimirBalanceDiagnosticBoundaryAndWarmup(t *testing.T) {
	for _, test := range []struct {
		name, process string
		elapsed       time.Duration
		complete      bool
	}{
		{"one-minute", mimirBalanceDiagnosticProcess, time.Minute, true},
		{"exact-three-minutes", mimirBalanceDiagnosticProcess, 3 * time.Minute, true},
		{"new-generation", "2000000002", time.Minute, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			start := time.Date(2032, 4, 5, 6, 7, 0, 0, time.UTC)
			frames := mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 0)
			warmup, _ := assessMimirBalanceDiagnostic(t, start, mimirBalancePersistedState{}, frames, nil)
			if !warmup.directComplete || warmup.rateComplete || len(warmup.visibilityFailures) != 0 {
				t.Error("first observation was confused with failed comparison")
			}
			if alerts := runMimirBalanceDiagnostic(t, dir, start, frames, nil); len(alerts) != 0 {
				t.Fatal("first observation should not alert")
			}
			before := mimirBalanceDiagnosticState(t, dir)
			frames = mimirBalanceDiagnosticFrames(test.process, 1)
			assessment, next := assessMimirBalanceDiagnostic(t, start.Add(test.elapsed), before, frames, nil)
			if !assessment.directComplete || assessment.rateComplete != test.complete || len(assessment.visibilityFailures) != 0 {
				t.Error("admitted boundary or generation warmup was misclassified")
			}
			if !test.complete && assessment.generationChanges != 3 {
				t.Error("replacement generations did not arm independent baselines")
			}
			if alerts := runMimirBalanceDiagnostic(t, dir, start.Add(test.elapsed), frames, nil); len(alerts) != 0 {
				t.Error("healthy boundary or expected generation warmup alerted")
			}
			if after := mimirBalanceDiagnosticState(t, dir); !reflect.DeepEqual(after, next) {
				t.Error("boundary/warmup changed persisted-state semantics")
			}
		})
	}
}

func TestMimirBalanceDiagnosticBranchesAndPrivacy(t *testing.T) {
	for _, test := range []struct {
		name, stage, errorClass string
		preserveHost            bool
		elapsed                 time.Duration
	}{
		{"host-error", "host-observation-failed", observationErrorClassUnclassified, true, time.Minute},
		{"parser-error", "host-observation-failed", observationErrorClassUnclassified, true, time.Minute},
		{"no-children", "no-local-child", observationErrorClassUnclassified, true, time.Minute},
		{"unobservable-child", "child-observation-unavailable", observationErrorClassUnclassified, true, time.Minute},
		{"counter-decrease", "counter-decreased", observationErrorClassCounterReset, false, time.Minute},
		{"reset-before-stale", "counter-decreased", observationErrorClassCounterReset, false, 3*time.Minute + time.Nanosecond},
		{"fleet-view", "fleet-view-inconsistent", observationErrorClassUnclassified, false, time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			start := time.Date(2032, 4, 5, 6, 7, 0, 0, time.UTC)
			runMimirBalanceDiagnostic(t, dir, start, mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 0), nil)
			before := mimirBalanceDiagnosticState(t, dir)
			frames := mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 1)
			failures := map[string]error{}
			target := "metrics-a.invalid/mimir-balance"
			switch test.name {
			case "host-error":
				failures["metrics-a.invalid"] = fmt.Errorf("command/parser failed: %s", mimirBalanceDiagnosticPrivate)
			case "parser-error":
				frames["metrics-a.invalid"] = mimirBalanceDiagnosticPrivate
			case "no-children":
				frames["metrics-a.invalid"] = "mimir_count 0\nfront_local_connections 0\nfront_remote_connections 0\n"
			case "unobservable-child":
				frames["metrics-a.invalid"] = fmt.Sprintf("instance_begin %d\nobservable 0\ninstance_end 1\nmimir_count 1\nfront_local_connections 0\nfront_remote_connections 0\n", mimirBalanceSyntheticPort)
			case "counter-decrease", "reset-before-stale":
				frames["metrics-a.invalid"] = mimirBalanceSyntheticFrame(mimirBalanceDiagnosticProcess, mimirBalanceSyntheticTotals{samples: 1, received: 1, requests: 1}, 20, 1)
			case "fleet-view":
				frames["metrics-a.invalid"] = strings.ReplaceAll(frames["metrics-a.invalid"], "active_distributors 3", "active_distributors 4")
				target = "mimir-fleet/mimir-balance"
			}
			now := start.Add(test.elapsed)
			assessment, next := assessMimirBalanceDiagnostic(t, now, before, frames, failures)
			if assessment.rateComplete || mimirBalanceIsSkewed(assessment) {
				t.Error("partial observation acquired complete-fleet/skew authority")
			}
			alerts := runMimirBalanceDiagnostic(t, dir, now, frames, failures)
			found := 0
			for _, alert := range alerts {
				if alert.Target == target {
					found++
					requireMimirBalanceDiagnostic(t, alert, target, test.stage, test.errorClass)
					if test.stage == "host-observation-failed" && strings.Contains(alert.Observed, "comparison-stale") {
						t.Error("host error forged the owner-local branch")
					}
				} else if test.name != "reset-before-stale" || alert.Class != "cannot-observe" {
					t.Error("unexpected target or detection from a partial diagnostic")
				}
			}
			if found != 1 {
				t.Errorf("want exactly one retained target finding, got %d", found)
			}
			after := mimirBalanceDiagnosticState(t, dir)
			if !reflect.DeepEqual(after, next) || len(after.Histories) != 3 {
				t.Fatal("branch changed public history persistence")
			}
			for index, history := range after.Histories {
				if history.Host == "metrics-a.invalid" && test.preserveHost {
					if !reflect.DeepEqual(history, before.Histories[index]) {
						t.Error("source failure replaced the prior host history")
					}
				} else if history.ObservedUnixNS != now.UnixNano() {
					t.Error("valid current child failed to refresh its own history")
				}
			}
		})
	}
}

func TestMimirBalanceDiagnosticPartialHostAndFirstFailure(t *testing.T) {
	child := func(frame string, port int) string {
		body, _, _ := strings.Cut(frame, "mimir_count")
		return strings.Replace(body, fmt.Sprintf("instance_begin %d", mimirBalanceSyntheticPort), fmt.Sprintf("instance_begin %d", port), 1)
	}
	trailer := "mimir_count 2\nfront_local_connections 20\nfront_remote_connections 1\n"
	for _, test := range []struct {
		name, stage, errorClass string
		failures                int
	}{
		{"valid-sibling", "child-observation-unavailable", observationErrorClassUnclassified, 1},
		{"reset-first", "counter-decreased", observationErrorClassCounterReset, 2},
		{"unavailable-first", "child-observation-unavailable", observationErrorClassUnclassified, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			start := time.Date(2032, 4, 5, 6, 7, 0, 0, time.UTC)
			frames := mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 0)
			original := frames["metrics-a.invalid"]
			frames["metrics-a.invalid"] = child(original, mimirBalanceSyntheticPort) + child(original, mimirBalanceSyntheticPort+1) + trailer
			if alerts := runMimirBalanceDiagnostic(t, dir, start, frames, nil); len(alerts) != 0 {
				t.Fatal("partial-host fixture did not arm both child baselines")
			}
			before := mimirBalanceDiagnosticState(t, dir)
			frames = mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 1)
			valid := child(frames["metrics-a.invalid"], mimirBalanceSyntheticPort)
			unavailable := fmt.Sprintf("instance_begin %d\nobservable 0\ninstance_end 1\n", mimirBalanceSyntheticPort+1)
			reset := child(mimirBalanceSyntheticFrame(mimirBalanceDiagnosticProcess, mimirBalanceSyntheticTotals{samples: 1, received: 1, requests: 1}, 20, 1), mimirBalanceSyntheticPort)
			switch test.name {
			case "valid-sibling":
				frames["metrics-a.invalid"] = valid + unavailable + trailer
			case "reset-first":
				frames["metrics-a.invalid"] = reset + unavailable + trailer
			case "unavailable-first":
				frames["metrics-a.invalid"] = unavailable + reset + trailer
			}
			now := start.Add(time.Minute)
			assessment, next := assessMimirBalanceDiagnostic(t, now, before, frames, nil)
			if assessment.directComplete || assessment.rateComplete || mimirBalanceIsSkewed(assessment) || len(assessment.visibilityFailures) != test.failures {
				t.Error("partial-host assessment lost incompleteness or child failure count")
			}
			alerts := runMimirBalanceDiagnostic(t, dir, now, frames, nil)
			if len(alerts) != 1 {
				t.Fatalf("first-per-target deduplication changed: got %d alerts", len(alerts))
			}
			requireMimirBalanceDiagnostic(t, alerts[0], "metrics-a.invalid/mimir-balance", test.stage, test.errorClass)
			after := mimirBalanceDiagnosticState(t, dir)
			if !reflect.DeepEqual(after, next) || len(after.Histories) != 4 {
				t.Fatal("partial-host persistence changed the bounded identity set")
			}
			for index, history := range after.Histories {
				if history.Host == "metrics-a.invalid" {
					if !reflect.DeepEqual(history, before.Histories[index]) {
						t.Error("partial host must preserve both prior same-generation child histories")
					}
				} else if history.ObservedUnixNS != now.UnixNano() || history.SamplesInTotal != 106000 {
					t.Error("independent complete host did not advance its own baseline")
				}
			}
		})
	}
}

func TestMimirBalanceDiagnosticIdentityBound(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2032, 4, 5, 6, 7, 0, 0, time.UTC)
	frames := mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 0)
	runMimirBalanceDiagnostic(t, dir, start, frames, nil)
	before := mimirBalanceDiagnosticState(t, dir)
	frames = mimirBalanceDiagnosticFrames(mimirBalanceDiagnosticProcess, 1)
	body, _, _ := strings.Cut(frames["metrics-a.invalid"], "mimir_count")
	var children strings.Builder
	for index := 0; index <= mimirBalanceHistoryLimit; index++ {
		children.WriteString(strings.Replace(body, fmt.Sprintf("instance_begin %d", mimirBalanceSyntheticPort), fmt.Sprintf("instance_begin %d", 20000+index), 1))
	}
	fmt.Fprintf(&children, "mimir_count %d\nfront_local_connections 20\nfront_remote_connections 1\n", mimirBalanceHistoryLimit+1)
	frames["metrics-a.invalid"] = children.String()
	now := start.Add(time.Minute)
	assessment, next := assessMimirBalanceDiagnostic(t, now, before, frames, nil)
	if assessment.directComplete || assessment.rateComplete || !reflect.DeepEqual(next, before) {
		t.Error("identity overflow must remain unknown and preserve the complete prior state")
	}
	alerts := runMimirBalanceDiagnostic(t, dir, now, frames, nil)
	if len(alerts) != 1 {
		t.Fatalf("want only the bounded-state target, got %d alerts", len(alerts))
	}
	requireMimirBalanceDiagnostic(t, alerts[0], "mimir-balance/state", "identity-bound-exceeded", observationErrorClassBoundExceeded)
	if after := mimirBalanceDiagnosticState(t, dir); !reflect.DeepEqual(after, before) {
		t.Error("public overflow run changed persisted history")
	}
}
