// The §26 probes replay recorded Mimir history (testdata/storm_*.json): each
// case holds the exact query_range answer the probe's own command received
// from read-only Main Mimir for one evaluation, with labels limited to the
// probe's bounded shapes. No test contacts Mimir.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Main's placement and inventory on 2026-10-26, as the §26 probes read them.
var stormMainServiceHosts = map[string][]string{
	"alt":        {"crisp", "fireside"},
	"api":        {"by-us-fmt-5-edge-0", "by-us-fmt-5-edge-1", "by-us-fmt-5-edge-3", "by-us-fmt-5-edge-4"},
	"connect":    {"by-us-fmt-5-edge-0", "by-us-fmt-5-edge-1", "by-us-fmt-5-edge-3", "by-us-fmt-5-edge-4"},
	"gossip":     {"by-us-fmt-5-edge-0", "by-us-fmt-5-edge-1", "by-us-fmt-5-edge-3", "by-us-fmt-5-edge-4"},
	"mcp":        {"by-us-fmt-5-edge-0", "by-us-fmt-5-edge-1", "by-us-fmt-5-edge-3", "by-us-fmt-5-edge-4"},
	"proxy":      {"crisp", "fireside"},
	"taskworker": {"by-us-fmt-5-edge-0", "by-us-fmt-5-edge-1", "by-us-fmt-5-edge-3", "by-us-fmt-5-edge-4"},
}

var stormMainServiceBlocks = map[string][]string{
	"alt":        {"g1"},
	"api":        {"beta", "g1", "g2", "g3", "g4"},
	"connect":    {"beta", "g1", "g2", "g3", "g4"},
	"gossip":     {"g1"},
	"mcp":        {"beta", "g1", "g2", "g3", "g4"},
	"proxy":      {"g1", "g2", "g3", "g4", "g5", "g6", "g7", "g8", "g9", "g10"},
	"taskworker": {"g1", "g2"},
}

func stormPortRange(low, high int) []int {
	ports := []int{}
	for port := low; port <= high; port++ {
		ports = append(ports, port)
	}
	return ports
}

// A Main-shaped environment whose every source call goes to source.
func stormMainSettings(now time.Time, source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Environment = "main"
	settings.Now = func() time.Time { return now }
	settings.Hosts = []HostSettings{
		{Name: "by-us-fmt-5-edge-0", Roles: []string{"services"}},
		{Name: "by-us-fmt-5-edge-1", Roles: []string{"services"}},
		{Name: "by-us-fmt-5-edge-3", Roles: []string{"services"}},
		{Name: "by-us-fmt-5-edge-4", Roles: []string{"services"}},
		{Name: "by-us-fmt-5-edge-2", Roles: []string{"pg-primary"}, PgBouncerInstances: 32},
		{Name: "by-us-fmt-5-edge-5", Roles: []string{"redis-cluster"}, RedisNodePorts: stormPortRange(6412, 6427)},
		{Name: "by-us-fmt-5-edge-6", Roles: []string{"redis-cluster"}, RedisEntryPort: 6379, RedisNodePorts: stormPortRange(6380, 6411)},
		{Name: "crisp", Roles: []string{"edge"}},
		{Name: "fireside", Roles: []string{"edge"}},
	}
	settings.LogServices = []string{"alt", "api", "connect", "gossip", "mcp", "proxy", "taskworker"}
	settings.LogServiceHosts = map[string][]string{}
	settings.LogServiceBlocks = map[string][]string{}
	for service, hosts := range stormMainServiceHosts {
		settings.LogServiceHosts[service] = append([]string(nil), hosts...)
		settings.LogServiceBlocks[service] = append([]string(nil), stormMainServiceBlocks[service]...)
	}
	return settings
}

// One recorded evaluation: the probe's query, its range and Mimir's answer.
type stormReplayCase struct {
	Name     string          `json:"name"`
	Now      time.Time       `json:"now"`
	Query    string          `json:"query"`
	Start    int64           `json:"start"`
	End      int64           `json:"end"`
	Step     int64           `json:"step"`
	Response json.RawMessage `json:"response"`
}

type stormReplayFixture struct {
	Signal string            `json:"signal"`
	Source string            `json:"source"`
	Cases  []stormReplayCase `json:"cases"`
}

func loadStormReplayFixture(t testing.TB, key string) map[string]stormReplayCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/storm_" + strings.ReplaceAll(key, "-", "_") + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture stormReplayFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Signal != key {
		t.Fatalf("fixture belongs to %q, want %q", fixture.Signal, key)
	}
	cases := map[string]stormReplayCase{}
	for _, one := range fixture.Cases {
		cases[one.Name] = one
	}
	return cases
}

// Runs the signal at the case's clock against its recorded answer, and
// requires the probe to send exactly the recorded query and range.
func runStormReplay(t *testing.T, signal Signal, replay stormReplayCase, mutate func(*SignalSettings)) Alerts {
	t.Helper()
	calls := 0
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		calls++
		if host.Name != "by-us-fmt-5-edge-0" {
			t.Fatalf("history read left the first services gateway: %s", host.Name)
		}
		for _, fragment := range []string{
			"query_range",
			shellSingleQuote("query=" + replay.Query),
			shellSingleQuote("start=" + strconv.FormatInt(replay.Start, 10)),
			shellSingleQuote("end=" + strconv.FormatInt(replay.End, 10)),
			shellSingleQuote("step=" + strconv.FormatInt(replay.Step, 10)),
		} {
			if !strings.Contains(command, fragment) {
				t.Fatalf("%s: the probe's command differs from the recorded read at %q:\n%s", replay.Name, fragment, command)
			}
		}
		return string(replay.Response), nil
	}}
	settings := stormMainSettings(replay.Now, source)
	if mutate != nil {
		mutate(&settings)
	}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("%s: %v", replay.Name, err)
	}
	if calls != 1 {
		t.Fatalf("%s: %d history reads, want 1", replay.Name, calls)
	}
	return alerts
}

// The alerts of one class, by target.
func stormAlertsByTarget(alerts Alerts, class string) map[string]Alert {
	targets := map[string]Alert{}
	for _, alert := range alerts {
		if alert.Class == class {
			targets[alert.Target] = alert
		}
	}
	return targets
}

// Requires exactly the given classes, each on exactly the given targets.
func requireStormAlerts(t *testing.T, name string, alerts Alerts, want map[string][]string) {
	t.Helper()
	got := map[string][]string{}
	for _, alert := range alerts {
		got[alert.Class] = append(got[alert.Class], alert.Target)
	}
	for class, targets := range want {
		if strings.Join(got[class], ",") != strings.Join(targets, ",") {
			t.Fatalf("%s: class %s on %v, want %v (all alerts %v)", name, class, got[class], targets, got)
		}
	}
	for class := range got {
		if _, wanted := want[class]; !wanted {
			t.Fatalf("%s: unexpected class %s on %v", name, class, got[class])
		}
	}
}

// Recorded answers carry no process instance nonce and no address.
var stormPrivateLabelPattern = regexp.MustCompile(`"instance"|\b[0-9]{1,3}(?:\.[0-9]{1,3}){3}\b|":"[0-9a-f]{8}"`)

func TestStormReplayFixturesCarryOnlyBoundedLabels(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	fixtures := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "storm_") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		fixtures++
		raw, err := os.ReadFile("testdata/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if match := stormPrivateLabelPattern.Find(raw); match != nil {
			t.Fatalf("%s carries a private label or address: %s", entry.Name(), match)
		}
	}
	if fixtures != 10 {
		t.Fatalf("found %d §26 replay fixtures, want 10", fixtures)
	}
}

// A synthetic range answer: each series is a part, its labels and one value
// per step, NaN for a step without a value.
type stormSyntheticSeries struct {
	part   string
	labels map[string]string
	values []float64
}

func stormSyntheticResponse(t testing.TB, start time.Time, step time.Duration, series []stormSyntheticSeries) string {
	t.Helper()
	rows := []map[string]any{}
	for _, one := range series {
		labels := map[string]string{fleetPartLabel: one.part}
		for name, value := range one.labels {
			labels[name] = value
		}
		values := [][]any{}
		for index, value := range one.values {
			if math.IsNaN(value) {
				continue
			}
			values = append(values, []any{start.Add(time.Duration(index) * step).Unix(), strconv.FormatFloat(value, 'g', -1, 64)})
		}
		rows = append(rows, map[string]any{"metric": labels, "values": values})
	}
	encoded, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": rows}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// Repeats one value over count steps.
func stormRepeat(value float64, count int) []float64 {
	values := make([]float64, count)
	for index := range values {
		values[index] = value
	}
	return values
}

// Runs a signal against one synthetic answer at a fixed clock.
func runStormSynthetic(t *testing.T, signal Signal, now time.Time, response string, mutate func(*SignalSettings)) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return response, nil }}
	settings := stormMainSettings(now, source)
	if mutate != nil {
		mutate(&settings)
	}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

var stormSyntheticNow = time.Date(2026, 10, 26, 15, 2, 0, 0, time.UTC)

// The probe's raw findings, including healthy findings and clear holds, as
// "class target" -> "page", "warn", "healthy", "hold" or "unobservable".
func stormFindingStates(t *testing.T, probe probe, now time.Time, response string, mutate func(*SignalSettings)) map[string]string {
	t.Helper()
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return response, nil }}
	settings := stormMainSettings(now, source)
	if mutate != nil {
		mutate(&settings)
	}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := probe.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, one := range findings {
		state := one.tier
		switch {
		case one.clearHold:
			state = "hold"
		case one.healthy:
			state = "healthy"
		case one.isUnobservable():
			state = "unobservable"
		}
		states[one.class+" "+one.target] = state
	}
	return states
}

// Requires the given finding states; other findings are not checked.
func requireStormStates(t *testing.T, name string, states map[string]string, want map[string]string) {
	t.Helper()
	for identity, state := range want {
		if states[identity] != state {
			t.Fatalf("%s: %s is %q, want %q (all %v)", name, identity, states[identity], state, states)
		}
	}
}

// The evaluation steps a synthetic answer must cover for a read of points.
func stormSyntheticStart(points int) time.Time {
	at := fleetEvaluationTime(stormSyntheticNow, DefaultStormReadSettings().EvaluationLag)
	return at.Add(-time.Duration(points-1) * time.Minute)
}

func TestStormHoldOfSeparatesHeldClearAndBroken(t *testing.T) {
	steps := []int64{60, 120, 180}
	level := stormLevel{threshold: 10}
	readings := func(values ...stormReading) func(int64) stormReading {
		return func(step int64) stormReading { return values[step/60-1] }
	}
	for _, test := range []struct {
		name     string
		readings []stormReading
		want     stormHold
	}{
		{"held", []stormReading{stormValue(11), stormValue(12), stormValue(10.5)}, stormHold{held: true, complete: true}},
		{"level is not beyond", []stormReading{stormValue(11), stormValue(10), stormValue(12)}, stormHold{complete: true}},
		{"clear", []stormReading{stormValue(10), stormValue(0), stormValue(9)}, stormHold{clear: true, complete: true}},
		{"gap is unknown", []stormReading{stormValue(11), {}, stormValue(12)}, stormHold{}},
		{"left out is neither", []stormReading{stormValue(0), stormLeftOut, stormValue(0)}, stormHold{complete: true}},
	} {
		if got := stormHoldOf(steps, level, readings(test.readings...)); got != test.want {
			t.Fatalf("%s: %+v, want %+v", test.name, got, test.want)
		}
	}
	under := stormLevel{threshold: 0.25, under: true}
	if !under.beyond(0.2499) || under.beyond(0.25) || under.beyond(math.Inf(1)) {
		t.Fatal("an under level must be strict")
	}
	if got := stormHoldOf(nil, level, readings()); got.held || got.clear {
		t.Fatal("an empty hold neither holds nor clears")
	}
}

func TestStormSeriesKeyRejectsUnboundedLabels(t *testing.T) {
	shape := stormShape{keys: []string{"host", "node"}, extra: []string{"job"}}
	if key, reason := stormSeriesKey(map[string]string{"host": "h", "node": "6380", "job": "redis"}, shape); key != "h/6380" || reason != "" {
		t.Fatalf("key %q reason %q", key, reason)
	}
	if _, reason := stormSeriesKey(map[string]string{"host": "h", "node": "6380", "instance": "aabbccdd"}, shape); reason != "unrecognized_label" {
		t.Fatalf("an instance label was accepted: %q", reason)
	}
	if _, reason := stormSeriesKey(map[string]string{"host": "h"}, shape); reason != "unlabeled_series" {
		t.Fatalf("a series without its key was accepted: %q", reason)
	}
}

func TestStormReadSettingsBoundHolds(t *testing.T) {
	read := DefaultStormReadSettings()
	if !read.valid(5*time.Minute, 30*time.Minute) || read.holdSteps(10*time.Minute) != 10 {
		t.Fatal("§26 holds are whole minutes")
	}
	for _, hold := range []time.Duration{0, 90 * time.Second, 121 * time.Minute} {
		if read.valid(hold) {
			t.Fatalf("hold %s accepted", hold)
		}
	}
	read.Cadence = 0
	if read.valid() || read.schedule() != time.Minute {
		t.Fatal("an invalid cadence reached the scheduler")
	}
}

// A failed or malformed read is the probe's own visibility class with a
// fixed reason; the source's text never reaches the alert, and a canceled
// owner publishes nothing.
func TestStormProbesReportFailedReadsAsVisibility(t *testing.T) {
	signals := []Signal{
		NewPgBouncerBacklogSignal(), NewServicesCpuSignal(), NewDbAcquireWaitSignal(), NewContractCreationsStuckSignal(),
		NewReservationReleaseSignal(), NewRedisPoolTimeoutsSignal(), NewRedisMainThreadSignal(), NewProxyHostCpuSignal(),
		NewContractReplyCollapseSignal(), NewHostedContractCapSignal(),
	}
	for _, signal := range signals {
		for _, variant := range []string{"transport", "malformed", "foreign_label"} {
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				switch variant {
				case "transport":
					return "private-canary", errors.New("private-canary 192.0.2.7")
				case "malformed":
					return "private-canary", nil
				}
				return `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"monitor_part":"private-canary"},"values":[]}]}}`, nil
			}}
			alerts, err := signal.Run(context.Background(), stormMainSettings(stormSyntheticNow, source))
			if err != nil {
				t.Fatalf("%s %s: %v", signal.Key(), variant, err)
			}
			unobservable := signal.Key() + "-unobservable"
			found := false
			for _, alert := range alerts {
				requireAlertOmits(t, alert, "private-canary", "192.0.2.7")
				if alert.Class == unobservable {
					found = alert.unobservable && alert.Severity == SeverityWarn
				}
			}
			if !found {
				t.Fatalf("%s %s: no %s visibility alert in %+v", signal.Key(), variant, unobservable, alerts)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			cancel()
			return "", context.Canceled
		}}
		alerts, err := signal.Run(ctx, stormMainSettings(stormSyntheticNow, source))
		if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
			t.Fatalf("%s: a canceled owner published %v (%v)", signal.Key(), alerts, err)
		}
	}
}

// Invalid policy is visibility before any source contact.
func TestStormProbesRejectInvalidPolicyWithoutReading(t *testing.T) {
	invalid := []Signal{
		NewPgBouncerBacklogSignalWithSettings(&PgBouncerBacklogSettings{}),
		NewServicesCpuSignalWithSettings(&ServicesCpuSettings{}),
		NewDbAcquireWaitSignalWithSettings(&DbAcquireWaitSettings{}),
		NewContractCreationsStuckSignalWithSettings(&ContractCreationsStuckSettings{}),
		NewReservationReleaseSignalWithSettings(&ReservationReleaseSettings{}),
		NewRedisPoolTimeoutsSignalWithSettings(&RedisPoolTimeoutsSettings{}),
		NewRedisMainThreadSignalWithSettings(&RedisMainThreadSettings{}),
		NewProxyHostCpuSignalWithSettings(&ProxyHostCpuSettings{}),
		NewContractReplyCollapseSignalWithSettings(&ContractReplyCollapseSettings{}),
		NewHostedContractCapSignalWithSettings(&HostedContractCapSettings{}),
	}
	for _, signal := range invalid {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			t.Fatalf("%s read its source under an invalid policy", signal.Key())
			return "", nil
		}}
		alerts, err := signal.Run(context.Background(), stormMainSettings(stormSyntheticNow, source))
		if err != nil || len(alerts) != 1 || alerts[0].Class != signal.Key()+"-unobservable" || !strings.Contains(alerts[0].Observed, "invalid_policy") {
			t.Fatalf("%s: %v %+v", signal.Key(), err, alerts)
		}
		if signal.Cadence() != time.Minute {
			t.Fatalf("%s: invalid cadence reached the scheduler", signal.Key())
		}
	}
}
