// Deterministic controls exercise the public signal's native/history join,
// including the rendered Mimir adapter and the former uniform-growth miss.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// All labels and values are synthetic; each metric has original scrape times.
type redisClientsHistoryTestSeries struct {
	Metric map[string]string `json:"metric"`
	Values [][]any           `json:"values"`
}

// Build twelve minute-spaced scrapes, including a coverage-only older anchor.
func redisClientsHistoryTestSeriesForNode(port int, at time.Time, before, after int, runId string) []redisClientsHistoryTestSeries {
	result := []redisClientsHistoryTestSeries{}
	for _, metric := range []string{"redis_connected_clients", "redis_uptime_in_seconds", "redis_instance_info", "redis_up"} {
		series := redisClientsHistoryTestSeries{Metric: map[string]string{
			"__name__": metric, "env": "synthetic", "host": "redis-1", "job": "redis", "node": strconv.Itoa(port),
		}}
		if metric == "redis_instance_info" {
			series.Metric["run_id"] = runId
			series.Metric["tcp_port"] = strconv.Itoa(port)
		}
		for minute := -11; minute <= 0; minute++ {
			value := 1
			switch metric {
			case "redis_connected_clients":
				value = before
				if minute == 0 {
					value = after
				}
			case "redis_uptime_in_seconds":
				value = 3600 + 60*minute
			}
			series.Values = append(series.Values, []any{at.Add(time.Duration(minute) * time.Minute).Unix(), strconv.Itoa(value)})
		}
		result = append(result, series)
	}
	return result
}

// Encode the actual matrix response, retaining metric labels for parser tests.
func redisClientsHistoryTestBody(t *testing.T, series []redisClientsHistoryTestSeries, warnings ...string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"status": "success", "warnings": warnings,
		"data": map[string]any{"resultType": "matrix", "result": series},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// The native fixture contains every consumed field and the exact census.
func redisClientsHistoryTestNative(clients map[int]int) string {
	lines := []string{"host_memory 100000000000 90000000000"}
	for _, port := range []int{6380, 6381, 6382} {
		lines = append(lines, fmt.Sprintf("%d 100 1000 90 1 %d volatile-ttl 10 10 1000 0 0 0 0 100", port, clients[port]))
	}
	return strings.Join(lines, "\n")
}

// Supply native snapshots and raw metrics through the public source interface.
func redisClientsHistoryTestSettings(t *testing.T, at *time.Time, body *string, native *string, batteries *int) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(configured HostSettings, command string) (string, error) {
		if strings.Contains(command, "monitor-redis-clients-history") {
			if configured.Name != "metrics.example" {
				t.Fatalf("history targeted %q, want the configured service gateway", configured.Name)
			}
			return *body, nil
		}
		if strings.Contains(command, "CLIENT LIST") {
			*batteries += 1
			return "client_list_total=160 blocked_clients=0", nil
		}
		return *native, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return *at }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics.example", Roles: []string{"services"}})
	return settings
}

// Count only the concrete symptom so independent coverage findings cannot
// accidentally satisfy the regression's causal assertion.
func redisClientsHistoryTestSpikeCount(alerts Alerts) int {
	count := 0
	for _, alert := range alerts {
		if alert.Class == "clients-spike" {
			count++
		}
	}
	return count
}

// Uniform growth is invisible to the native median and must reach the actual
// historical reducer through the bounded source adapter.
func TestRedisConnectionsHistoryUniformGrowthUsesRawSourceWindow(t *testing.T) {
	at := syntheticSettings(nil).Now()
	series := []redisClientsHistoryTestSeries{}
	for _, port := range []int{6380, 6381, 6382} {
		series = append(series, redisClientsHistoryTestSeriesForNode(port, at, 100, 160, strings.Repeat("a", 40))...)
	}
	body := redisClientsHistoryTestBody(t, series)
	native := redisClientsHistoryTestNative(map[int]int{6380: 160, 6381: 160, 6382: 160})
	batteries := 0
	settings := redisClientsHistoryTestSettings(t, &at, &body, &native, &batteries)
	// Execute the adapter's real shell command with a private curl stand-in.
	// It records the exact argv and only returns the local synthetic matrix.
	directory := t.TempDir()
	bodyPath := filepath.Join(directory, "matrix.json")
	argsPath := filepath.Join(directory, "args")
	if err := os.WriteFile(bodyPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	standIn := "#!/bin/sh\nprintf '%s\\0' \"$@\" > " + shellSingleQuote(argsPath) + "\n/bin/cat " + shellSingleQuote(bodyPath) + "\n"
	if err := os.WriteFile(filepath.Join(directory, "curl"), []byte(standIn), 0o700); err != nil {
		t.Fatal(err)
	}
	source := settings.Source.(*syntheticSource)
	ordinaryHost := source.hostFn
	queries := 0
	source.hostFn = func(configured HostSettings, command string) (string, error) {
		if strings.Contains(command, "monitor-redis-clients-history") {
			queries++
			if configured.Name != "metrics.example" {
				t.Fatalf("history targeted an unowned gateway")
			}
			child := exec.CommandContext(context.Background(), "/bin/sh", "-c", command)
			child.Env = []string{"PATH=" + directory + ":/usr/bin:/bin"}
			out, err := child.Output()
			return string(out), err
		}
		return ordinaryHost(configured, command)
	}
	alerts, err := NewRedisConnectionsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if got := redisClientsHistoryTestSpikeCount(alerts); got != 3 {
		t.Fatalf("uniform +60%% source history produced %d clients-spike alerts; want 3 despite native fleet ratio 1", got)
	}
	if len(alerts) != 3 || queries != 1 || batteries != 3 {
		t.Fatalf("alerts=%d history queries=%d batteries=%d; want 3, 1, 3", len(alerts), queries, batteries)
	}
	for _, alert := range alerts {
		if alert.SignalID != "redis/clients-spike" || alert.Sustain != 2 || alert.Frame != "" ||
			!strings.Contains(alert.Observed, "history_growth_percent=60.0") ||
			!strings.Contains(alert.Observed, "history_elapsed_seconds=600.000") {
			t.Fatalf("growth did not retain its exact identity, threshold, sustain, and source window: %+v", alert)
		}
		requireAlertOmits(t, alert, strings.Repeat("a", 40))
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSuffix(string(args), "\x00"), "\x00")
	if len(fields) != 10 || strings.Join(fields[:5], " ") != "-fsS --max-time 15 --max-filesize 1048576" ||
		fields[5] != "--data-urlencode" || fields[7] != "--data-urlencode" ||
		fields[8] != "time="+strconv.FormatInt(at.Unix(), 10) || fields[9] != "http://127.0.0.1:3100/prometheus/api/v1/query" {
		t.Fatalf("bounded Mimir adapter argv did not match the fixed contract: %q", fields)
	}
	for _, fragment := range []string{`query={__name__=~"redis_(connected_clients|uptime_in_seconds|instance_info|up)"`, `env="synthetic"`, `job="redis"`, `host=~"redis-1"`, `node=~"6380|6381|6382"`, `}[960s]`} {
		if !strings.Contains(fields[6], fragment) {
			t.Fatalf("raw history query lacks %q", fragment)
		}
	}
}

// The catalog threshold is inclusive, and a coverage anchor is not a baseline
// for growth outside the ten-minute interval.
func TestRedisConnectionsHistoryThresholdAndOlderAnchor(t *testing.T) {
	for _, test := range []struct {
		name       string
		before     int
		after      int
		olderOnly  bool
		wantSpikes int
	}{
		{name: "below", before: 100, after: 149},
		{name: "inclusive", before: 100, after: 150, wantSpikes: 3},
		{name: "flat", before: 160, after: 160},
		{name: "older anchor is not a growth baseline", before: 160, after: 160, olderOnly: true},
	} {
		at := syntheticSettings(nil).Now()
		series := []redisClientsHistoryTestSeries{}
		for _, port := range []int{6380, 6381, 6382} {
			node := redisClientsHistoryTestSeriesForNode(port, at, test.before, test.after, strings.Repeat("a", 40))
			if test.olderOnly {
				node[0].Values[0][1] = "100"
			}
			series = append(series, node...)
		}
		body := redisClientsHistoryTestBody(t, series)
		native := redisClientsHistoryTestNative(map[int]int{6380: test.after, 6381: test.after, 6382: test.after})
		batteries := 0
		alerts, err := NewRedisConnectionsSignal().Run(context.Background(), redisClientsHistoryTestSettings(t, &at, &body, &native, &batteries))
		if err != nil || redisClientsHistoryTestSpikeCount(alerts) != test.wantSpikes || len(alerts) != test.wantSpikes || batteries != test.wantSpikes {
			t.Fatalf("%s: spikes=%d alerts=%d batteries=%d err=%v", test.name, redisClientsHistoryTestSpikeCount(alerts), len(alerts), batteries, err)
		}
	}
}

// A recovery from an interior dip does not turn a net window decline into growth.
func TestRedisConnectionsHistoryUsesWindowEndpointsInsteadOfInteriorDip(t *testing.T) {
	at := syntheticSettings(nil).Now()
	series := []redisClientsHistoryTestSeries{}
	for _, port := range []int{6380, 6381, 6382} {
		node := redisClientsHistoryTestSeriesForNode(port, at, 300, 160, strings.Repeat("a", 40))
		for index := 6; index < 11; index++ {
			node[0].Values[index][1] = "100"
		}
		series = append(series, node...)
	}
	body := redisClientsHistoryTestBody(t, series)
	native := redisClientsHistoryTestNative(map[int]int{6380: 160, 6381: 160, 6382: 160})
	batteries := 0
	alerts, err := NewRedisConnectionsSignal().Run(context.Background(), redisClientsHistoryTestSettings(t, &at, &body, &native, &batteries))
	if err != nil || len(alerts) != 0 || batteries != 0 {
		t.Fatalf("300-to-100-to-160 window was misread as growth instead of net decline: alerts=%d batteries=%d err=%v", len(alerts), batteries, err)
	}
}

// Staggered exporter scrapes retain actual endpoint times; a threshold-changing
// left-boundary ambiguity must remain unknown.
func TestRedisConnectionsHistoryBracketsStaggeredScrapeBoundary(t *testing.T) {
	for _, disagree := range []bool{false, true} {
		at := syntheticSettings(nil).Now()
		series := []redisClientsHistoryTestSeries{}
		for _, port := range []int{6380, 6381, 6382} {
			node := redisClientsHistoryTestSeriesForNode(port, at, 100, 160, strings.Repeat("a", 40))
			for metric := range node {
				for index := range node[metric].Values {
					node[metric].Values[index][0] = at.Add(time.Duration(index-11) * 61 * time.Second).Unix()
					if metric == 1 {
						node[metric].Values[index][1] = strconv.Itoa(3600 + (index-11)*61)
					}
				}
			}
			if disagree {
				// The left neighbor is 610 seconds old; its 300 clients and
				// the 549-second endpoint's 100 clients disagree on the threshold.
				node[0].Values[1][1] = "300"
			}
			series = append(series, node...)
		}
		body := redisClientsHistoryTestBody(t, series)
		native := redisClientsHistoryTestNative(map[int]int{6380: 160, 6381: 160, 6382: 160})
		batteries := 0
		alerts, err := NewRedisConnectionsSignal().Run(context.Background(), redisClientsHistoryTestSettings(t, &at, &body, &native, &batteries))
		if err != nil || len(alerts) != 3 {
			t.Fatalf("staggered boundary disagree=%t: alerts=%d err=%v", disagree, len(alerts), err)
		}
		for _, alert := range alerts {
			if disagree {
				if !alert.unobservable || !strings.Contains(alert.Observed, "history_state=ambiguous-window-boundary") || batteries != 0 {
					t.Fatalf("ambiguous boundary became a concrete verdict: %+v", alert)
				}
			} else if alert.Class != "clients-spike" || !strings.Contains(alert.Observed, "history_elapsed_seconds=549.000") || batteries != 3 {
				t.Fatalf("staggered scrape lost its actual endpoint clocks: %+v", alert)
			}
		}
	}
}

// The operator's native cadence cannot fabricate a shorter source interval.
func TestRedisConnectionsHistorySparseNativeSamplesStayUnknown(t *testing.T) {
	at := syntheticSettings(nil).Now()
	native := redisClientsHistoryTestNative(map[int]int{6380: 100, 6381: 100, 6382: 100})
	settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) { return native, nil }})
	settings.Now = func() time.Time { return at }
	signal := NewRedisConnectionsSignal().(*signalAdapter)
	for sample := 0; sample < 2; sample++ {
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		findings, err := signal.probe.check(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		unknown := 0
		for _, finding := range findings {
			if finding.probeId != "redis/clients-spike" {
				continue
			}
			if finding.healthy || finding.class == "clients-spike" {
				t.Fatalf("fifteen-minute native snapshots fabricated a historical verdict: %+v", finding)
			}
			if finding.class == "cannot-observe" && finding.frame == "clients-history" && finding.isUnobservable() {
				unknown++
			}
		}
		if unknown != 1 {
			t.Fatalf("missing shared history produced %d explicit unknowns; want 1 covering all configured nodes", unknown)
		}
		at = at.Add(15 * time.Minute)
		native = redisClientsHistoryTestNative(map[int]int{6380: 160, 6381: 160, 6382: 160})
	}
}

// Missing source families preserve independent spatial and temporal positives.
func TestRedisConnectionsHistoryPartialSourcePreservesBothKnownArms(t *testing.T) {
	at := syntheticSettings(nil).Now()
	series := redisClientsHistoryTestSeriesForNode(6380, at, 100, 160, strings.Repeat("a", 40))
	partial := redisClientsHistoryTestSeriesForNode(6381, at, 100, 100, strings.Repeat("a", 40))
	series = append(series, partial[:2]...)
	body := redisClientsHistoryTestBody(t, series)
	native := redisClientsHistoryTestNative(map[int]int{6380: 100, 6381: 100, 6382: 1000})
	batteries := 0
	alerts, err := NewRedisConnectionsSignal().Run(context.Background(), redisClientsHistoryTestSettings(t, &at, &body, &native, &batteries))
	if err != nil {
		t.Fatal(err)
	}
	spikes := map[string]bool{}
	unknown := map[string]bool{}
	for _, alert := range alerts {
		if alert.Class == "clients-spike" {
			spikes[alert.Target] = true
		}
		if alert.Class == "cannot-observe" && alert.Frame == "clients-history" && alert.unobservable {
			unknown[alert.Target] = true
		}
	}
	if len(spikes) != 2 || !spikes["redis-1:6380"] || !spikes["redis-1:6382"] || len(unknown) != 2 ||
		!unknown["redis-1:6381"] || !unknown["redis-1:6382"] || batteries != 2 {
		t.Fatalf("partial history erased a known arm or fabricated fleet coverage: spikes=%v unknown=%v batteries=%d", spikes, unknown, batteries)
	}
}

// Source errors and lifecycle boundaries cannot become growth or healthy zeros.
func TestRedisConnectionsHistoryRejectsUnqualifiedSource(t *testing.T) {
	for _, test := range []struct {
		name   string
		reason string
		mutate func([]redisClientsHistoryTestSeries, time.Time)
	}{
		{name: "missing generation", reason: "invalid-generation", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) { delete(s[2].Metric, "run_id") }},
		{name: "wrong port", reason: "invalid-generation", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) { s[2].Metric["tcp_port"] = "6381" }},
		{name: "invalid count", reason: "invalid-value", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) { s[0].Values[11][1] = "NaN" }},
		{name: "fractional count", reason: "invalid-value", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) { s[0].Values[11][1] = "160.5" }},
		{name: "negative count", reason: "invalid-value", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) { s[0].Values[11][1] = "-1" }},
		{name: "zero baseline", reason: "zero-baseline", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) { s[0].Values[1][1] = "0" }},
		{name: "source down", reason: "incomplete-latest-sample", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) { s[3].Values[11][1] = "0" }},
		{name: "generation warmup", reason: "generation-warmup", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) { s[1].Values[11][1] = "300" }},
		{name: "uptime reset", reason: "uptime-reset", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) { s[1].Values[10][1] = "2000" }},
		{name: "missing latest companion", reason: "incomplete-latest-sample", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) { s[2].Values = s[2].Values[:11] }},
		{name: "missing old companion", reason: "incomplete-history", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) {
			s[2].Values = append(s[2].Values[:4], s[2].Values[5:]...)
		}},
		{name: "duplicate correction", reason: "ambiguous-samples", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) {
			s[0].Values = append(s[0].Values, []any{s[0].Values[11][0], "149"})
		}},
		{name: "future", reason: "invalid-source-time", mutate: func(s []redisClientsHistoryTestSeries, at time.Time) { s[0].Values[11][0] = at.Add(time.Second).Unix() }},
		{name: "stale", reason: "stale-history", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) {
			for i := range s {
				for j := range s[i].Values {
					s[i].Values[j][0] = s[i].Values[j][0].(int64) - 240
				}
			}
		}},
		{name: "gap", reason: "history-gap", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) {
			for i := range s {
				s[i].Values = append(s[i].Values[:4], s[i].Values[8:]...)
			}
		}},
		{name: "history warmup", reason: "history-warmup", mutate: func(s []redisClientsHistoryTestSeries, _ time.Time) {
			for i := range s {
				s[i].Values = s[i].Values[3:]
			}
		}},
		{name: "fifteen minute source gap", reason: "history-gap", mutate: func(s []redisClientsHistoryTestSeries, at time.Time) {
			for i := range s {
				s[i].Values = [][]any{s[i].Values[0], s[i].Values[11]}
				s[i].Values[0][0] = at.Add(-15 * time.Minute).Unix()
			}
		}},
	} {
		at := syntheticSettings(nil).Now()
		series := redisClientsHistoryTestSeriesForNode(6380, at, 100, 160, strings.Repeat("a", 40))
		test.mutate(series, at)
		body := redisClientsHistoryTestBody(t, series)
		native := redisClientsHistoryTestNative(map[int]int{6380: 160, 6381: 160, 6382: 160})
		batteries := 0
		alerts, err := NewRedisConnectionsSignal().Run(context.Background(), redisClientsHistoryTestSettings(t, &at, &body, &native, &batteries))
		if err != nil || redisClientsHistoryTestSpikeCount(alerts) != 0 || batteries != 0 {
			t.Fatalf("%s: unqualified source manufactured a growth spike: alerts=%d batteries=%d err=%v", test.name, len(alerts), batteries, err)
		}
		found := false
		for _, alert := range alerts {
			if alert.Target == "redis-1:6380" && alert.Frame == "clients-history" && alert.unobservable &&
				strings.Contains(alert.Observed, "history_state="+test.reason) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: missing explicit %q unknown", test.name, test.reason)
		}
	}
}

// Fresh source revisions replace prior conclusions and batteries follow the
// combined trip, with an independent reset on a proven generation change.
func TestRedisConnectionsHistoryReevaluatesCorrectionsAndGeneration(t *testing.T) {
	at := syntheticSettings(nil).Now()
	body := ""
	native := redisClientsHistoryTestNative(map[int]int{6380: 160, 6381: 160, 6382: 160})
	batteries := 0
	settings := redisClientsHistoryTestSettings(t, &at, &body, &native, &batteries)
	signal := NewRedisConnectionsSignal()
	for index, test := range []struct {
		before        int
		runId         string
		wantSpikes    int
		wantBatteries int
	}{
		{before: 100, runId: "a", wantSpikes: 3, wantBatteries: 3},
		{before: 100, runId: "a", wantSpikes: 3, wantBatteries: 3},
		{before: 160, runId: "a", wantBatteries: 3},
		{before: 100, runId: "a", wantSpikes: 3, wantBatteries: 6},
		{before: 100, runId: "b", wantSpikes: 3, wantBatteries: 9},
	} {
		series := []redisClientsHistoryTestSeries{}
		for _, port := range []int{6380, 6381, 6382} {
			series = append(series, redisClientsHistoryTestSeriesForNode(port, at, test.before, 160, strings.Repeat(test.runId, 40))...)
		}
		body = redisClientsHistoryTestBody(t, series)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || redisClientsHistoryTestSpikeCount(alerts) != test.wantSpikes || len(alerts) != test.wantSpikes || batteries != test.wantBatteries {
			t.Fatalf("revision %d: spikes=%d alerts=%d batteries=%d err=%v", index, redisClientsHistoryTestSpikeCount(alerts), len(alerts), batteries, err)
		}
	}
}

// Matching host and port are insufficient when the source spans two run ids.
func TestRedisConnectionsHistoryRejectsCrossGenerationWindow(t *testing.T) {
	at := syntheticSettings(nil).Now()
	series := redisClientsHistoryTestSeriesForNode(6380, at, 100, 160, strings.Repeat("a", 40))
	newGeneration := redisClientsHistoryTestSeriesForNode(6380, at, 100, 160, strings.Repeat("b", 40))[2]
	newGeneration.Values = newGeneration.Values[6:]
	series[2].Values = series[2].Values[:6]
	series = append(series, newGeneration)
	body := redisClientsHistoryTestBody(t, series)
	native := redisClientsHistoryTestNative(map[int]int{6380: 160, 6381: 160, 6382: 160})
	batteries := 0
	alerts, err := NewRedisConnectionsSignal().Run(context.Background(), redisClientsHistoryTestSettings(t, &at, &body, &native, &batteries))
	if err != nil || redisClientsHistoryTestSpikeCount(alerts) != 0 || batteries != 0 {
		t.Fatalf("cross-generation window manufactured growth: alerts=%d batteries=%d err=%v", len(alerts), batteries, err)
	}
	for _, alert := range alerts {
		if alert.Target == "redis-1:6380" && strings.Contains(alert.Observed, "history_state=generation-change") && alert.unobservable {
			return
		}
	}
	t.Fatal("cross-generation window did not remain explicitly unknown")
}

// Whole-response failure remains unknown without erasing native violations
// or publishing source diagnostics and labels.
func TestRedisConnectionsHistorySourceFailuresPreserveMedianAndRedact(t *testing.T) {
	for _, kind := range []string{"transport", "warnings", "oversized", "foreign", "malformed", "empty matrix"} {
		at := syntheticSettings(nil).Now()
		series := redisClientsHistoryTestSeriesForNode(6380, at, 100, 160, strings.Repeat("a", 40))
		secret := "synthetic-private-source-detail"
		body := redisClientsHistoryTestBody(t, series)
		switch kind {
		case "warnings":
			body = redisClientsHistoryTestBody(t, series, secret)
		case "oversized":
			body = strings.Repeat(" ", 1024*1024+1)
		case "foreign":
			series[0].Metric["host"] = secret
			body = redisClientsHistoryTestBody(t, series)
		case "malformed":
			body = secret
		case "empty matrix":
			body = redisClientsHistoryTestBody(t, nil)
		}
		native := redisClientsHistoryTestNative(map[int]int{6380: 100, 6381: 100, 6382: 1000})
		batteries := 0
		settings := redisClientsHistoryTestSettings(t, &at, &body, &native, &batteries)
		if kind == "transport" {
			source := settings.Source.(*syntheticSource)
			ordinaryHost := source.hostFn
			source.hostFn = func(configured HostSettings, command string) (string, error) {
				if strings.Contains(command, "monitor-redis-clients-history") {
					return "", errors.New(secret)
				}
				return ordinaryHost(configured, command)
			}
		}
		alerts, err := NewRedisConnectionsSignal().Run(context.Background(), settings)
		if err != nil || redisClientsHistoryTestSpikeCount(alerts) != 1 || batteries != 1 {
			t.Fatalf("%s: source failure erased native median spike: alerts=%d batteries=%d err=%v", kind, len(alerts), batteries, err)
		}
		unknown := 0
		for _, alert := range alerts {
			requireAlertOmits(t, alert, secret, strings.Repeat("a", 40))
			if alert.Class == "cannot-observe" && alert.Frame == "clients-history" && alert.unobservable {
				unknown++
				if alert.Target != "redis-clients-history" || !strings.Contains(alert.Observed, "configured_nodes=3 unobservable_nodes=3") {
					t.Fatalf("%s: shared failure lost its full configured-node coverage: %+v", kind, alert)
				}
			}
		}
		if unknown != 1 {
			t.Fatalf("%s: history source failure produced %d unknowns, want 1 shared-source incident", kind, unknown)
		}
	}
}

// Cancellation must stop before either observation path starts.
func TestRedisConnectionsHistoryCancellationStartsNoSource(t *testing.T) {
	calls := 0
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { calls++; return "", nil }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewRedisConnectionsSignal().Run(ctx, syntheticSettings(source))
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("cancellation: calls=%d err=%v", calls, err)
	}
}

// Historical authority remains usable during an independent native source failure.
func TestRedisConnectionsHistoryRetainsGrowthWhenNativeInfoFails(t *testing.T) {
	at := syntheticSettings(nil).Now()
	series := []redisClientsHistoryTestSeries{}
	for _, port := range []int{6380, 6381, 6382} {
		series = append(series, redisClientsHistoryTestSeriesForNode(port, at, 100, 160, strings.Repeat("a", 40))...)
	}
	body := redisClientsHistoryTestBody(t, series)
	native := ""
	batteries := 0
	settings := redisClientsHistoryTestSettings(t, &at, &body, &native, &batteries)
	source := settings.Source.(*syntheticSource)
	ordinaryHost := source.hostFn
	source.hostFn = func(configured HostSettings, command string) (string, error) {
		if strings.Contains(command, "for p in") && !strings.Contains(command, "CLIENT LIST") {
			return "", errors.New("synthetic native INFO unavailable")
		}
		return ordinaryHost(configured, command)
	}
	alerts, err := NewRedisConnectionsSignal().Run(context.Background(), settings)
	if err != nil || redisClientsHistoryTestSpikeCount(alerts) != 3 || batteries != 3 {
		t.Fatalf("native failure erased authoritative historical growth: alerts=%d batteries=%d err=%v", len(alerts), batteries, err)
	}
	for _, alert := range alerts {
		if alert.Target == "redis-connections/current" && alert.unobservable {
			return
		}
	}
	t.Fatal("native observation failure was hidden by historical coverage")
}

// Each fifteen-minute monitor observation uses its own current source window;
// no prior native sample or stored growth flag supplies the ten-minute arm.
func TestRedisConnectionsHistoryUsesFreshWindowsAtFifteenMinuteCadence(t *testing.T) {
	at := syntheticSettings(nil).Now()
	body := ""
	native := ""
	batteries := 0
	settings := redisClientsHistoryTestSettings(t, &at, &body, &native, &batteries)
	signal := NewRedisConnectionsSignal()
	for _, counts := range []struct {
		before int
		after  int
	}{
		{before: 100, after: 160},
		{before: 160, after: 256},
		{before: 256, after: 256},
	} {
		series := []redisClientsHistoryTestSeries{}
		for _, port := range []int{6380, 6381, 6382} {
			series = append(series, redisClientsHistoryTestSeriesForNode(port, at, counts.before, counts.after, strings.Repeat("a", 40))...)
		}
		body = redisClientsHistoryTestBody(t, series)
		native = redisClientsHistoryTestNative(map[int]int{6380: counts.after, 6381: counts.after, 6382: counts.after})
		alerts, err := signal.Run(context.Background(), settings)
		want := 3
		if counts.before == counts.after {
			want = 0
		}
		if err != nil || len(alerts) != want || redisClientsHistoryTestSpikeCount(alerts) != want || batteries != 3 {
			t.Fatalf("fifteen-minute observation lost fresh authority or persisted old growth: spikes=%d alerts=%d batteries=%d err=%v", redisClientsHistoryTestSpikeCount(alerts), len(alerts), batteries, err)
		}
		at = at.Add(15 * time.Minute)
	}
}

// One unavailable shared query must not consume the notifier budget once per
// dependent node and hide a native spike that the public signal still observes.
func TestRedisConnectionsHistorySharedSourceFailureKeepsNativeSpikeVisible(t *testing.T) {
	for _, body := range []string{"", redisClientsHistoryTestBody(t, nil)} {
		source := &syntheticSource{hostFn: func(configured HostSettings, command string) (string, error) {
			if strings.Contains(command, redisClientsHistoryMarker) {
				return body, nil
			}
			if strings.Contains(command, "CLIENT LIST") {
				return "client_list_total=1000 blocked_clients=0", nil
			}
			if strings.Contains(command, "for p in") {
				rows := []string{"host_memory 100000000000 90000000000"}
				for _, port := range configured.RedisNodePorts {
					clients := 10
					if configured.Name == "redis-b" && port == 6404 {
						clients = 1000
					}
					rows = append(rows, fmt.Sprintf("%d 100 1000 90 1 %d volatile-ttl 10 10 1000 0 0 0 0 100", port, clients))
				}
				return strings.Join(rows, "\n"), nil
			}
			return "", nil
		}}
		settings := syntheticSettings(source)
		settings.Hosts = []HostSettings{
			{Name: "redis-a", Roles: []string{"redis-cluster"}, RedisEntryPort: 6379, RedisNodePorts: redisTestPortRange(6380, 6403)},
			{Name: "redis-b", Roles: []string{"redis-cluster"}, RedisNodePorts: redisTestPortRange(6404, 6427)},
			{Name: "metrics.example", Roles: []string{"services"}},
		}
		signal := NewRedisConnectionsSignal()
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || redisClientsHistoryTestSpikeCount(alerts) != 1 {
			t.Fatalf("shared-source failure erased the native median spike: alerts=%d err=%v", len(alerts), err)
		}
		coverage := []Alert{}
		for _, alert := range alerts {
			if alert.Class == "cannot-observe" && alert.Frame == "clients-history" && alert.unobservable {
				coverage = append(coverage, alert)
			}
		}
		if len(coverage) != 1 {
			t.Fatalf("one shared history source failure produced %d coverage incidents; want 1 covering all 48 configured nodes", len(coverage))
		}
		if coverage[0].Target != "redis-clients-history" || !strings.Contains(coverage[0].Observed, "configured_nodes=48 unobservable_nodes=48") {
			t.Fatalf("shared coverage lost the complete configured census: %+v", coverage[0])
		}
		webhook, webhookUrl := newFakeSlackWebhook(t)
		harness := newSlackTestHarness()
		notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
		postSignalRunsThroughTheGate(t, signal, settings, 2, func(int) {}, notifier)
		closeSlackNotifier(t, notifier)
		texts := webhook.posted()
		if countSlackPosts(texts, "[WARN] raised", "redis/clients-spike · clients-spike · redis-b:6404") != 1 ||
			countSlackPosts(texts, "[WARN] raised", "redis/clients-spike · cannot-observe · redis-clients-history") != 1 || len(texts) != 2 {
			t.Fatalf("shared history failure hid the known native spike or duplicated coverage in Slack:\n%s", strings.Join(texts, "\n---\n"))
		}
	}
}

// Source-scope visibility retains previously raised node history incidents.
// Recovery still requires two actual complete healthy signal executions.
func TestRedisConnectionsHistorySharedSourceFailureHoldsPriorGrowthUntilHealthyRecovery(t *testing.T) {
	at := syntheticSettings(nil).Now()
	start := at
	body := ""
	native := redisClientsHistoryTestNative(map[int]int{6380: 160, 6381: 160, 6382: 160})
	batteries := 0
	settings := redisClientsHistoryTestSettings(t, &at, &body, &native, &batteries)
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	postSignalRunsThroughTheGate(t, NewRedisConnectionsSignal(), settings, 6, func(run int) {
		if run == 4 || run == 5 {
			// Both source-failed runs count as unknown. The first subsequent
			// healthy run is only the first observation of the recovery hold.
			func() {
				notifier.stateLock.Lock()
				defer notifier.stateLock.Unlock()
				for _, port := range []int{6380, 6381, 6382} {
					key := slackIncidentKey(Alert{SignalID: "redis/clients-spike", Class: "clients-spike", Target: fmt.Sprintf("redis-1:%d", port)})
					incident := notifier.keyIncidents[key]
					if incident == nil || incident.signalKeyAbsentRuns["redis-connections"] != run-4 {
						t.Fatalf("run %d: shared history unknown or one healthy run cleared a prior node growth incident", run)
					}
				}
				key := slackIncidentKey(Alert{SignalID: "redis/clients-spike", Class: "cannot-observe", Target: "redis-clients-history"})
				incident := notifier.keyIncidents[key]
				if incident == nil || !incident.alert.unobservable || incident.signalKeyAbsentRuns["redis-connections"] != run-4 {
					t.Fatalf("run %d: shared history coverage lost its visibility or recovery hold", run)
				}
			}()
		}
		at = start.Add(time.Duration(run) * 15 * time.Minute)
		if run == 2 || run == 3 {
			body = ""
			return
		}
		before := 160
		if run < 2 {
			before = 100
		}
		series := []redisClientsHistoryTestSeries{}
		for _, port := range []int{6380, 6381, 6382} {
			series = append(series, redisClientsHistoryTestSeriesForNode(port, at, before, 160, strings.Repeat("a", 40))...)
		}
		body = redisClientsHistoryTestBody(t, series)
	}, notifier)
	closeSlackNotifier(t, notifier)
	texts := webhook.posted()
	for _, port := range []int{6380, 6381, 6382} {
		identity := fmt.Sprintf("redis/clients-spike · clients-spike · redis-1:%d", port)
		if countSlackPosts(texts, "[WARN] raised", identity) != 1 || countSlackPosts(texts, "[CLEARED]", identity) != 1 {
			t.Fatalf("historical growth did not retain one incident through missing source and actual recovery:\n%s", strings.Join(texts, "\n---\n"))
		}
	}
	identity := "redis/clients-spike · cannot-observe · redis-clients-history"
	if countSlackPosts(texts, "[WARN] raised", identity) != 1 || countSlackPosts(texts, "[CLEARED]", identity) != 1 || batteries != 3 {
		t.Fatalf("shared source or battery trip did not recover from actual complete healthy observations: batteries=%d\n%s", batteries, strings.Join(texts, "\n---\n"))
	}
}
