// Synthetic Mimir balance fixtures use documentation-only addresses and
// generated identities; production peers, hostnames, and tenant labels never
// enter test source or alert assertions.
package monitor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		mimirBalanceSyntheticFleet(processStart, base, deltas, nil),
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
	if !strings.Contains(alert.Markdown(), "monotonic distributor counter decreased") {
		t.Fatalf("counter reset cause was lost: %s", alert.Markdown())
	}
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
