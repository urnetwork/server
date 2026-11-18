package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Two synthetic Redis generations. Run ids stay in memory only.
var (
	redisRefusalsRunA = strings.Repeat("a1", 20)
	redisRefusalsRunB = strings.Repeat("b2", 20)
)

// One census row: pass, port, run id, uptime, connected clients, maxclients,
// rejected connections and connections received.
func redisRefusalsRow(pass, port int, runId string, connected, rejected, received int64) string {
	return fmt.Sprintf("%d %d %s %d %d %d %d %d", pass, port, runId, 86400, connected, 65536, rejected, received)
}

// Both passes of a census in which every synthetic node is flat at the given
// rejected count, with the overrides replacing whole rows.
func redisRefusalsCensusOutput(rejected int64, overrides map[string]string) string {
	rows := []string{}
	for _, pass := range []int{1, 2} {
		for _, port := range []int{6380, 6381, 6382} {
			key := fmt.Sprintf("%d/%d", pass, port)
			if row, ok := overrides[key]; ok {
				if row != "" {
					rows = append(rows, row)
				}
				continue
			}
			rows = append(rows, redisRefusalsRow(pass, port, redisRefusalsRunA, 1200, rejected, 5000))
		}
	}
	return strings.Join(rows, "\n") + "\n"
}

// Runs one signal instance over a census, checking the collection it sends.
func runRedisRefusalsFixture(t *testing.T, signal Signal, output string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "redis-1" {
			return "", fmt.Errorf("unexpected host %s", host.Name)
		}
		for _, want := range []string{"INFO server clients stats", "rejected_connections", "run_id", "sleep ", "6380 6381 6382"} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("refusal census missing %q", want)
			}
		}
		return output, nil
	}}
	alerts, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func redisRefusalsAlertsOfClass(alerts Alerts, class string) Alerts {
	selected := Alerts{}
	for _, alert := range alerts {
		if alert.Class == class {
			selected = append(selected, alert)
		}
	}
	return selected
}

// A cumulative boot total that does not move is healthy, even far above zero:
// a node that refused connections hours ago must not page forever.
func TestRedisRefusalsSignalFlatCountersAreHealthy(t *testing.T) {
	signal := NewRedisRefusalsSignal()
	for run := 0; run < 2; run++ {
		if alerts := runRedisRefusalsFixture(t, signal, redisRefusalsCensusOutput(2278013, nil)); len(alerts) != 0 {
			t.Fatalf("flat refusal counters alerted on run %d: %+v", run, alerts)
		}
	}
}

// Refusals inside the census window page at once on the refusing node only.
func TestRedisRefusalsSignalPagesOnGrowthWithinTheWindow(t *testing.T) {
	alerts := runRedisRefusalsFixture(t, NewRedisRefusalsSignal(), redisRefusalsCensusOutput(0, map[string]string{
		"1/6381": redisRefusalsRow(1, 6381, redisRefusalsRunA, 65530, 1000, 900000),
		"2/6381": redisRefusalsRow(2, 6381, redisRefusalsRunA, 65536, 1150, 900200),
	}))
	refusals := redisRefusalsAlertsOfClass(alerts, "redis-connection-refusals")
	if len(refusals) != 1 || len(alerts) != 1 {
		t.Fatalf("want one refusal alert, got %+v", alerts)
	}
	alert := requireAlertClass(t, refusals, "redis-connection-refusals")
	if alert.Severity != SeverityPage || alert.Target != "redis-1:6381" || alert.Sustain != 1 || alert.SignalNumber != "3.5a" || alert.SignalID != "redis/connection-refusals" {
		t.Fatalf("refusal alert contract changed: %+v", alert)
	}
	for _, want := range []string{"rejected_in_window=150", "connected_clients=65536", "maxclients=65536", "connections_received_in_window=200"} {
		if !strings.Contains(alert.Observed, want) {
			t.Fatalf("observed values lack %q: %s", want, alert.Observed)
		}
	}
	requireAlertOmits(t, alert, redisRefusalsRunA, redisRefusalsRunB)
}

// A drip between runs that the short window misses still pages: the second
// observation of the same generation finds the counter grown.
func TestRedisRefusalsSignalPagesOnGrowthSinceThePreviousObservation(t *testing.T) {
	signal := NewRedisRefusalsSignal()
	if alerts := runRedisRefusalsFixture(t, signal, redisRefusalsCensusOutput(10, nil)); len(alerts) != 0 {
		t.Fatalf("first flat census alerted: %+v", alerts)
	}
	alerts := runRedisRefusalsFixture(t, signal, redisRefusalsCensusOutput(10, map[string]string{
		"1/6382": redisRefusalsRow(1, 6382, redisRefusalsRunA, 1200, 37, 5000),
		"2/6382": redisRefusalsRow(2, 6382, redisRefusalsRunA, 1200, 37, 5000),
	}))
	alert := requireAlertClass(t, redisRefusalsAlertsOfClass(alerts, "redis-connection-refusals"), "redis-connection-refusals")
	if alert.Target != "redis-1:6382" || !strings.Contains(alert.Observed, "rejected_in_window=0") || !strings.Contains(alert.Observed, "rejected_since_previous=27") {
		t.Fatalf("growth since the previous observation was not reported: %+v", alert)
	}
	// once refusals stop, the next observation is healthy again
	if alerts := runRedisRefusalsFixture(t, signal, redisRefusalsCensusOutput(10, map[string]string{
		"1/6382": redisRefusalsRow(1, 6382, redisRefusalsRunA, 1200, 37, 5000),
		"2/6382": redisRefusalsRow(2, 6382, redisRefusalsRunA, 1200, 37, 5000),
	})); len(alerts) != 0 {
		t.Fatalf("stopped refusals still alert: %+v", alerts)
	}
}

// A restart between observations starts a new counter: the lower total is a
// new baseline, not a negative growth or a page.
func TestRedisRefusalsSignalRestartBetweenRunsRebaselines(t *testing.T) {
	signal := NewRedisRefusalsSignal()
	if alerts := runRedisRefusalsFixture(t, signal, redisRefusalsCensusOutput(500, nil)); len(alerts) != 0 {
		t.Fatalf("first census alerted: %+v", alerts)
	}
	alerts := runRedisRefusalsFixture(t, signal, redisRefusalsCensusOutput(500, map[string]string{
		"1/6380": redisRefusalsRow(1, 6380, redisRefusalsRunB, 300, 0, 40),
		"2/6380": redisRefusalsRow(2, 6380, redisRefusalsRunB, 300, 0, 40),
	}))
	if len(alerts) != 0 {
		t.Fatalf("a restart between runs raised: %+v", alerts)
	}
	// growth in the new generation since its first observation pages
	alerts = runRedisRefusalsFixture(t, signal, redisRefusalsCensusOutput(500, map[string]string{
		"1/6380": redisRefusalsRow(1, 6380, redisRefusalsRunB, 300, 4, 40),
		"2/6380": redisRefusalsRow(2, 6380, redisRefusalsRunB, 300, 4, 40),
	}))
	alert := requireAlertClass(t, redisRefusalsAlertsOfClass(alerts, "redis-connection-refusals"), "redis-connection-refusals")
	if alert.Target != "redis-1:6380" || !strings.Contains(alert.Observed, "rejected_since_previous=4") {
		t.Fatalf("new-generation growth was not measured from its own baseline: %+v", alert)
	}
}

// A restart or a statistics reset inside the window leaves that node unknown
// for the run: neither a page nor a healthy clear.
func TestRedisRefusalsSignalGenerationChangeOrResetInsideTheWindowIsUnknown(t *testing.T) {
	for name, overrides := range map[string]map[string]string{
		"restart": {
			"1/6381": redisRefusalsRow(1, 6381, redisRefusalsRunA, 1200, 900, 5000),
			"2/6381": redisRefusalsRow(2, 6381, redisRefusalsRunB, 10, 0, 1),
		},
		"stats-reset": {
			"1/6381": redisRefusalsRow(1, 6381, redisRefusalsRunA, 1200, 900, 5000),
			"2/6381": redisRefusalsRow(2, 6381, redisRefusalsRunA, 1200, 0, 0),
		},
	} {
		t.Run(name, func(t *testing.T) {
			alerts := runRedisRefusalsFixture(t, NewRedisRefusalsSignal(), redisRefusalsCensusOutput(900, overrides))
			if len(redisRefusalsAlertsOfClass(alerts, "redis-connection-refusals")) != 0 {
				t.Fatalf("an unmeasurable window raised a refusal: %+v", alerts)
			}
			unknown := requireAlertClass(t, alerts, "cannot-observe")
			if unknown.Target != "redis-1" || unknown.Frame != "refusal-census" || !strings.Contains(unknown.Observed, "6381") {
				t.Fatalf("unknown node not reported on its host census: %+v", unknown)
			}
			requireAlertOmits(t, unknown, redisRefusalsRunA, redisRefusalsRunB)
		})
	}
}

// Missing, duplicate, foreign or malformed rows never become a healthy zero.
func TestRedisRefusalsSignalIncompleteCensusIsUnknown(t *testing.T) {
	for name, output := range map[string]string{
		"unreachable": redisRefusalsCensusOutput(0, map[string]string{"1/6382": "1 6382 unreachable"}),
		"missing":     redisRefusalsCensusOutput(0, map[string]string{"2/6382": ""}),
		"duplicate":   redisRefusalsCensusOutput(0, nil) + redisRefusalsRow(2, 6380, redisRefusalsRunA, 1200, 0, 5000) + "\n",
		"foreign":     redisRefusalsCensusOutput(0, nil) + redisRefusalsRow(1, 7000, redisRefusalsRunA, 1200, 0, 5000) + "\n",
		"malformed":   redisRefusalsCensusOutput(0, map[string]string{"1/6380": "1 6380 " + redisRefusalsRunA + " 1 2 3 -4 5"}),
		"short-run":   redisRefusalsCensusOutput(0, map[string]string{"1/6380": "1 6380 abc 86400 1200 65536 0 5000"}),
	} {
		t.Run(name, func(t *testing.T) {
			alerts := runRedisRefusalsFixture(t, NewRedisRefusalsSignal(), output)
			unknown := requireAlertClass(t, alerts, "cannot-observe")
			if unknown.Target != "redis-1" || unknown.Frame != "refusal-census" {
				t.Fatalf("incomplete census not reported on its host: %+v", unknown)
			}
			if len(redisRefusalsAlertsOfClass(alerts, "redis-connection-refusals")) != 0 {
				t.Fatalf("incomplete census raised a refusal: %+v", alerts)
			}
		})
	}
}

// A healthy sibling stays judged when another node's row is unusable, and a
// refusing sibling still pages.
func TestRedisRefusalsSignalJudgesValidSiblingsOfAnUnknownNode(t *testing.T) {
	alerts := runRedisRefusalsFixture(t, NewRedisRefusalsSignal(), redisRefusalsCensusOutput(0, map[string]string{
		"1/6380": "1 6380 unreachable",
		"1/6382": redisRefusalsRow(1, 6382, redisRefusalsRunA, 65536, 5, 100),
		"2/6382": redisRefusalsRow(2, 6382, redisRefusalsRunA, 65536, 9, 110),
	}))
	refusal := requireAlertClass(t, redisRefusalsAlertsOfClass(alerts, "redis-connection-refusals"), "redis-connection-refusals")
	if refusal.Target != "redis-1:6382" {
		t.Fatalf("sibling refusal lost: %+v", refusal)
	}
	requireAlertClass(t, alerts, "cannot-observe")
}

// An unreachable Redis host is that host's unknown, not a probe-wide failure
// that would hide another host's refusals. With no host readable the probe
// fails, and its visibility alert keeps the transport text private.
func TestRedisRefusalsSignalUnreachableHostIsUnknown(t *testing.T) {
	unreachable := fmt.Errorf("synthetic-redis: exit status 255: ssh: connect to host 192.0.2.80 port 22: Connection refused")
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name == "redis-1" {
			return "", unreachable
		}
		return strings.Join([]string{
			redisRefusalsRow(1, 6390, redisRefusalsRunA, 65536, 7, 100),
			redisRefusalsRow(1, 6391, redisRefusalsRunA, 1200, 0, 100),
			redisRefusalsRow(2, 6390, redisRefusalsRunA, 65536, 19, 140),
			redisRefusalsRow(2, 6391, redisRefusalsRunA, 1200, 0, 100),
		}, "\n"), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "redis-2", Roles: []string{"redis-cluster"}, RedisNodePorts: []int{6390, 6391}})
	alerts, err := NewRedisRefusalsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	refusal := requireAlertClass(t, redisRefusalsAlertsOfClass(alerts, "redis-connection-refusals"), "redis-connection-refusals")
	if refusal.Target != "redis-2:6390" {
		t.Fatalf("readable host's refusal lost: %+v", refusal)
	}
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if !strings.HasPrefix(unknown.Target, "redis-1") {
		t.Fatalf("unreachable host not reported on its own target: %+v", unknown)
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "192.0.2.80", "Connection refused")
	}

	alone := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return "", unreachable }}
	signal := NewRedisRefusalsSignal()
	if alerts, err := signal.Run(context.Background(), syntheticSettings(alone)); err == nil || len(alerts) != 0 {
		t.Fatalf("no readable host did not fail the probe: %+v %v", alerts, err)
	} else {
		requireAlertOmits(t, visibilityAlert(syntheticSettings(alone), signal, err), "192.0.2.80", "Connection refused")
	}
}

// The census window is long enough to see a refusal storm and short enough
// to hold the Redis host's command slot only briefly.
func TestRedisRefusalsSignalCensusWindow(t *testing.T) {
	var commands []string
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		commands = append(commands, command)
		return redisRefusalsCensusOutput(0, nil), nil
	}}
	if _, err := NewRedisRefusalsSignal().Run(context.Background(), syntheticSettings(source)); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || !strings.Contains(commands[0], fmt.Sprintf("sleep %d", int(redisRefusalsWindow/time.Second))) {
		t.Fatalf("census did not run as one bounded two-pass command: %d commands", len(commands))
	}
	if redisRefusalsWindow < time.Second || 5*time.Second < redisRefusalsWindow {
		t.Fatalf("census window %s outside its bounded range", redisRefusalsWindow)
	}
}

// Overlay this test alone to reproduce the absent probe at its registry
// boundary.
func TestRedisRefusalsSignalIsRegistered(t *testing.T) {
	matches := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "redis-refusals" {
			continue
		}
		matches++
		if signal.Number() != "3.5a" || signal.ID() != "redis/connection-refusals" || signal.Cadence() != 5*time.Minute {
			t.Fatalf("registered refusal signal does not match its catalog contract: %s %s %s", signal.Number(), signal.ID(), signal.Cadence())
		}
	}
	if matches != 1 {
		t.Fatal("missing unique registered SIGNALS.md section 3.5a refusal signal")
	}
}
