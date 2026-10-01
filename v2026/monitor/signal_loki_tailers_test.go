package monitor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func lokiTailFixture(version string, processStart, tails, streams float64) string {
	return fmt.Sprintf(
		"instance_begin 14788\nversion %s\nprocess_start %.0f\ntails_active %v\nstreams_active %v\ninstance_end\nloki_count 1\n",
		version, processStart, tails, streams,
	)
}

func runLokiTailersSynthetic(t *testing.T, fixtures map[string]string) []Alert {
	t.Helper()
	var commands atomic.Int64
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		commands.Add(1)
		if !strings.Contains(command, lokiTailersMarker) || !strings.Contains(command, "/metrics") {
			return "", fmt.Errorf("unexpected Loki tailers command on %s: %s", host.Name, command)
		}
		fixture, ok := fixtures[host.Name]
		if !ok {
			return "", fmt.Errorf("unexpected Loki host %s", host.Name)
		}
		return fixture, nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = make([]HostSettings, 0, len(fixtures)+1)
	for host := range fixtures {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: host, Roles: []string{"services"}})
	}
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-only", Roles: []string{"pg-primary"}})

	alerts, err := NewLokiTailersSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if commands.Load() != int64(len(fixtures)) {
		t.Fatalf("Loki tailer commands = %d, want %d services hosts", commands.Load(), len(fixtures))
	}
	return alerts
}

func TestLokiTailersSignalHealthySyntheticFleet(t *testing.T) {
	alerts := runLokiTailersSynthetic(t, map[string]string{
		"edge-0": lokiTailFixture("3.7.3", 1788162008, 8, 8),
		"edge-1": lokiTailFixture("3.7.3", 1788162011, 0, 0),
	})
	if len(alerts) != 0 {
		t.Fatalf("healthy Loki tail accounting alerted: %+v", alerts)
	}
}

func TestLokiTailersSignalSyntheticNegativeAccounting(t *testing.T) {
	alerts := runLokiTailersSynthetic(t, map[string]string{
		"edge-0": lokiTailFixture("3.7.3", 1788162008, 0, -1),
		"edge-1": lokiTailFixture("3.7.3", 1788162011, -16, -16),
		"edge-4": lokiTailFixture("3.7.3", 1788162014, -16, -20),
	})
	if len(alerts) != 3 {
		t.Fatalf("negative Loki gauges produced %d alerts, want 3: %+v", len(alerts), alerts)
	}
	for _, alert := range alerts {
		if alert.Class != "loki-tail-accounting-invalid" || alert.Sustain != 1 ||
			alert.SignalNumber != "11.19" || alert.SignalKey != "loki-tailers" {
			t.Fatalf("wrong Loki accounting alert identity: %+v", alert)
		}
		markdown := alert.Markdown()
		for _, want := range []string{
			"tail_max_duration", "deferred close", "not idempotent",
			"sole v163 watcher", "23:50:39Z", "00:51:27Z", "tails=-8 and streams=-9",
			"sole v164 watcher", "unchanged Loki process", "0/-1",
			"active tails can numerically mask earlier over-decrements",
			"Zero is not sufficient when the sibling gauge remains negative",
			"instrumentation invariant violation", "sync.Once", "ba01c98", "two one-hour rotations",
		} {
			if !strings.Contains(markdown, want) {
				t.Fatalf("Loki accounting alert lacks %q:\n%s", want, markdown)
			}
		}
	}
	observed := ""
	for _, alert := range alerts {
		observed += "\n" + alert.Observed
	}
	if !strings.Contains(observed, "tails_active=0 streams_active=-1") {
		t.Fatalf("masked active-tail underflow was lost: %+v", alerts)
	}
	if !strings.Contains(observed, "streams_active=-20") {
		t.Fatalf("production-shaped stream underflow was lost: %+v", alerts)
	}
}

func TestLokiTailersSignalSyntheticMissingChildAndGauge(t *testing.T) {
	alerts := runLokiTailersSynthetic(t, map[string]string{
		"missing": "loki_count 0\n",
		"partial": "instance_begin 14788\nversion 3.7.3\ntails_active 1\ninstance_end\nloki_count 1\n",
	})
	if len(alerts) != 2 {
		t.Fatalf("missing Loki observations produced %d alerts, want 2: %+v", len(alerts), alerts)
	}
	classes := map[string]Alert{}
	for _, alert := range alerts {
		classes[alert.Class] = alert
	}
	if missing, ok := classes["loki-tail-child-missing"]; !ok || missing.Target != "missing" || missing.Sustain != 2 {
		t.Fatalf("missing-child alert = %+v", missing)
	}
	if invalid, ok := classes["loki-tail-accounting-invalid"]; !ok || invalid.Target != "partial" ||
		!strings.Contains(invalid.Observed, "streams_present=false") {
		t.Fatalf("missing-gauge alert = %+v", invalid)
	}
}

func TestParseLokiTailHostSampleRejectsFramingMismatch(t *testing.T) {
	_, err := parseLokiTailHostSample("instance_begin 1\ntails_active 0\nstreams_active 0\ninstance_end\nloki_count 2\n")
	if err == nil || !strings.Contains(err.Error(), "count=2 instances=1") {
		t.Fatalf("framing mismatch error = %v", err)
	}
}

func TestLokiTailersScriptParsesProductionMetricShapes(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable("ss", `#!/bin/sh
printf '%s\n' \
  'LISTEN 0 4096 127.0.0.1:14788 0.0.0.0:*' \
  'LISTEN 0 4096 127.0.0.1:14819 0.0.0.0:*'
`)
	writeExecutable("curl", `#!/bin/sh
case "${*}" in
  *':14788/metrics')
    printf '%s\n' \
      'loki_build_info{branch="release-3.7.x",goarch="amd64",goos="linux",goversion="go1.26.4",revision="82cdcdc0",tags="netgo",version="3.7.3"} 1' \
      'loki_querier_tail_active -16' \
      'loki_querier_tail_active_streams -20' \
      'process_start_time_seconds 1.78816200821e+09'
    ;;
  *':14819/metrics')
    printf '%s\n' 'cortex_build_info{version="3.1.1"} 1'
    ;;
  *) exit 2 ;;
esac
`)

	command := exec.Command("sh", "-c", lokiTailersScript)
	command.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Loki tailers script: %v\n%s", err, output)
	}
	sample, err := parseLokiTailHostSample(string(output))
	if err != nil {
		t.Fatalf("parse Loki tailers script output: %v\n%s", err, output)
	}
	if sample.count != 1 || len(sample.instances) != 1 {
		t.Fatalf("Loki tailers script instances = %+v", sample)
	}
	instance := sample.instances[0]
	if instance.version != "3.7.3" || instance.processStart != 1788162008.21 ||
		instance.tailsActive != -16 || instance.streamsActive != -20 ||
		!instance.tailsSeen || !instance.streamsSeen || !instance.processSeen {
		t.Fatalf("Loki script lost production-shaped metrics: %+v\n%s", instance, output)
	}
}

func TestLokiTailersSignalPreservesPartialResultsThroughObservationLoss(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == "unreadable" {
			return "", fmt.Errorf("synthetic SSH loss")
		}
		return lokiTailFixture("3.7.3", 1788162014, -16, -20), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "edge-4", Roles: []string{"services"}},
		{Name: "unreadable", Roles: []string{"services"}},
	}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (lokiTailersProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]bool{}
	for _, finding := range findings {
		if !finding.healthy {
			classes[finding.class] = true
		}
	}
	if !classes["loki-tail-accounting-invalid"] || !classes["cannot-observe"] {
		t.Fatalf("partial Loki findings were discarded: %+v", findings)
	}
}
