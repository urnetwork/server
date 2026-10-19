// Exercise the shared native INFO collector and its actual signal adapters.
// Every subprocess uses fake local Redis commands and synthetic observations.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const redisInfoAuthorityHealthy = "# Memory\r\nused_memory:1000\r\nmaxmemory:10000\r\nused_memory_dataset:900\r\nmem_clients_normal:0\r\nmem_clients_slaves:0\r\n# Clients\r\nconnected_clients:10\r\n"

// Execute the exact generated shell with scripted replies and exit statuses.
// No real Redis, SSH, production configuration, or network socket is used.
func redisInfoAuthorityFixture(t *testing.T, reply string, status int) (SignalSettings, string) {
	t.Helper()
	directory := t.TempDir()
	callPath := filepath.Join(directory, "calls")
	redisScript := `#!/bin/sh
[ "$#" -eq 3 ] && [ "$1" = "-p" ] && [ "$3" = "INFO" ] || exit 126
printf '%s\n' "$2" >> "$REDIS_INFO_TEST_CALLS"
if [ "$2" = 6381 ]; then
  printf '%s' "$REDIS_INFO_TEST_REPLY"
  exit "$REDIS_INFO_TEST_STATUS"
fi
printf '%s' "$REDIS_INFO_TEST_HEALTHY"
`
	for name, script := range map[string]string{
		"redis-cli": redisScript,
		"timeout":   "#!/bin/sh\nshift\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if !strings.Contains(command, "/proc/meminfo") {
			return "", nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		cmd.Env = []string{
			"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C",
			"REDIS_INFO_TEST_CALLS=" + callPath,
			"REDIS_INFO_TEST_REPLY=" + reply,
			fmt.Sprintf("REDIS_INFO_TEST_STATUS=%d", status),
			"REDIS_INFO_TEST_HEALTHY=" + redisInfoAuthorityHealthy,
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("synthetic INFO collector execution failed: %v", err)
		}
		return string(out), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts[1].Name = "redis-info.example"
	return settings, callPath
}

// Match the INFO gap specifically, so an independent visibility finding
// cannot make the authority assertion pass. Check JSON and Markdown privacy.
func redisInfoAuthorityRequireGap(t *testing.T, alerts Alerts) Alert {
	t.Helper()
	var gap *Alert
	for i := range alerts {
		alert := &alerts[i]
		if alert.Class == "cannot-observe" && alert.SignalID == "redis/node-mem" {
			gap = alert
		}
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "synthetic-private-sentinel") || strings.Contains(alert.Markdown(), "synthetic-private-sentinel") {
			t.Fatal("raw INFO observation leaked into public alert output")
		}
	}
	if gap == nil {
		t.Fatal("redis INFO authority assertion: unusable node observation became a complete healthy sample")
	}
	if gap.Target != "redis-info.example" || gap.Frame != "node-info" || gap.Severity != SeverityWarn || gap.Sustain != 2 {
		t.Fatal("INFO observation loss changed its stable host identity or warning sustain")
	}
	return *gap
}

// A native error may exit zero. Nonempty output is not measurement authority.
func TestRedisInfoAuthorityNativeErrorIsUnknown(t *testing.T) {
	for _, reply := range []string{
		"NOAUTH synthetic-private-sentinel\n",
		"ERR synthetic-private-sentinel\n",
		"LOADING synthetic-private-sentinel\n",
		redisInfoAuthorityHealthy + "NOAUTH synthetic-private-sentinel\n",
		"",
	} {
		for _, signal := range []Signal{NewRedisMemorySignal(), NewRedisBuffersSignal(), NewRedisConnectionsSignal()} {
			settings, _ := redisInfoAuthorityFixture(t, reply, 0)
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			redisInfoAuthorityRequireGap(t, alerts)
		}
	}
}

// The INFO child status must survive command substitution and normalization,
// even if a failed child left a complete-looking body on stdout.
func TestRedisInfoAuthorityNativeCommandExitIsUnknown(t *testing.T) {
	for _, status := range []int{1, 124, 126, 127, 137} {
		settings, _ := redisInfoAuthorityFixture(t, redisInfoAuthorityHealthy, status)
		alerts, err := NewRedisBuffersSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		redisInfoAuthorityRequireGap(t, alerts)
	}
}

// With no readable sibling, retain the existing probe-execution failure path
// and its sanitized visibility boundary instead of returning a healthy sweep.
func TestRedisInfoAuthorityAllNodesUnavailable(t *testing.T) {
	settings, _ := redisInfoAuthorityFixture(t, "NOAUTH synthetic-private-sentinel\n", 0)
	settings.Hosts[1].RedisNodePorts = []int{6381}
	alerts, err := NewRedisBuffersSignal().Run(context.Background(), settings)
	if err == nil || len(alerts) != 0 {
		t.Fatal("all unavailable INFO nodes became a successful observation")
	}
	if strings.Contains(err.Error(), "synthetic-private-sentinel") {
		t.Fatal("raw INFO response leaked through the probe failure")
	}
}

// Only the measurements consumed by the shared pressure, attribution, and
// connection reducers are required; missing diagnostic sections are healthy.
func TestRedisInfoAuthorityNativeRequiredMeasurements(t *testing.T) {
	for _, field := range []string{
		"used_memory:1000\r\n", "maxmemory:10000\r\n", "used_memory_dataset:900\r\n",
		"mem_clients_normal:0\r\n", "mem_clients_slaves:0\r\n", "connected_clients:10\r\n",
	} {
		reply := strings.Replace(redisInfoAuthorityHealthy, field, "", 1)
		settings, _ := redisInfoAuthorityFixture(t, reply, 0)
		alerts, err := NewRedisBuffersSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		redisInfoAuthorityRequireGap(t, alerts)
	}
	for _, field := range []string{
		"mem_clients_normal:NaN\r\n", "mem_clients_normal:Inf\r\n",
		"mem_clients_normal:-1\r\n", "mem_clients_normal:synthetic-private-sentinel\r\n",
		"mem_clients_normal:0:synthetic-private-sentinel\r\n",
		"mem_clients_normal:0\r\nmem_clients_normal:0\r\n",
	} {
		reply := strings.Replace(redisInfoAuthorityHealthy, "mem_clients_normal:0\r\n", field, 1)
		settings, _ := redisInfoAuthorityFixture(t, reply, 0)
		alerts, err := NewRedisBuffersSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		redisInfoAuthorityRequireGap(t, alerts)
	}
}

// Explicit numeric zero is authoritative. Empty db0/errorstats sections and
// absent optional diagnostic fields do not invalidate core measurements.
func TestRedisInfoAuthorityNativeZeroAndOptionalFieldsHealthy(t *testing.T) {
	zero := "used_memory:0\nmaxmemory:0\nused_memory_dataset:0\nmem_clients_normal:0\nmem_clients_slaves:0\nconnected_clients:0\n"
	for _, reply := range []string{redisInfoAuthorityHealthy, zero, redisInfoAuthorityHealthy + "new_optional_field:synthetic-private-sentinel\n"} {
		for _, signal := range []Signal{NewRedisMemorySignal(), NewRedisBuffersSignal()} {
			settings, _ := redisInfoAuthorityFixture(t, reply, 0)
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || len(alerts) != 0 {
				t.Fatalf("valid zero or missing optional INFO fields were rejected: err=%v alerts=%d", err, len(alerts))
			}
		}
	}
}

// Optional boot totals can reset; this snapshot does not interpret a delta.
func TestRedisInfoAuthorityNativeOptionalCounterReset(t *testing.T) {
	signal := NewRedisMemorySignal()
	for _, total := range []int{9000, 0} {
		reply := redisInfoAuthorityHealthy + fmt.Sprintf("evicted_keys:%d\ntotal_error_replies:%d\nerrorstat_OOM:count=%d\ncurrent_eviction_exceeded_time:0\ndb0:keys=1,expires=1,avg_ttl=0\n", total, total, total)
		settings, _ := redisInfoAuthorityFixture(t, reply, 0)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 {
			t.Fatalf("valid cumulative counter/reset caused an alert: err=%v alerts=%d", err, len(alerts))
		}
	}
}

// Successful native collection must still pass real pressure to the reducer,
// including the sum of both client-memory components.
func TestRedisInfoAuthorityNativePressureControls(t *testing.T) {
	for _, sample := range []struct {
		signal Signal
		reply  string
		class  string
	}{
		{
			signal: NewRedisMemorySignal(),
			reply:  strings.Replace(redisInfoAuthorityHealthy, "used_memory:1000", "used_memory:9300", 1),
			class:  "node-mem-critical",
		},
		{
			signal: NewRedisBuffersSignal(),
			reply:  strings.NewReplacer("mem_clients_normal:0", "mem_clients_normal:150", "mem_clients_slaves:0", "mem_clients_slaves:150").Replace(redisInfoAuthorityHealthy),
			class:  "client-buffers",
		},
	} {
		settings, _ := redisInfoAuthorityFixture(t, sample.reply, 0)
		alerts, err := sample.signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, sample.class)
		if alert.Target != "redis-info.example:6381" {
			t.Fatal("native INFO pressure was attributed to a different configured node")
		}
	}
}

// The configured list owns collection, including noncontiguous inventories.
func TestRedisInfoAuthorityNativeSparseInventory(t *testing.T) {
	settings, callPath := redisInfoAuthorityFixture(t, "NOAUTH synthetic-private-sentinel\n", 0)
	settings.Hosts[1].RedisNodePorts = []int{6380, 6382}
	alerts, err := NewRedisMemorySignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("complete sparse INFO census failed: err=%v alerts=%d", err, len(alerts))
	}
	calls, err := os.ReadFile(callPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "6380\n6382\n" {
		t.Fatal("INFO collector contacted a port outside the configured census")
	}
}

// Exercise compact-row authority through every adapter, retaining a valid
// sibling while omitted, duplicate, malformed, and foreign rows stay unknown.
func TestRedisInfoAuthorityCompactCensusIsExact(t *testing.T) {
	for _, middle := range []string{
		"", "6381 unreachable", "6381 0 1000", "6381 NaN 1000 0 0 0",
		"6381 0 1000 0 Inf 0", "6381 0 1000 0 -1 0", "6381 0 1000 0 synthetic-private-sentinel 0",
		"6381 0 1000 0 0 0 extra", "6381 0 1000 0 0 0\n6381 0 1000 0 0 0",
		"9999 0 1000 0 0 0", "6381 0 1000 0 0 0\n9999 0 1000 0 0 0",
		"6381 0 1000 0 0 0\nNOAUTH synthetic-private-sentinel",
	} {
		rows := "6380 100 1000 90 0 10\n" + middle + "\n6382 100 1000 90 0 10"
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return rows, nil }}
		settings := syntheticSettings(source)
		settings.Hosts[1].Name = "redis-info.example"
		for _, signal := range []Signal{NewRedisMemorySignal(), NewRedisBuffersSignal(), NewRedisConnectionsSignal()} {
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			redisInfoAuthorityRequireGap(t, alerts)
		}
	}
}

// A partial sample can keep a real per-node breach, but cannot establish or
// clear a fleet-relative/capacity verdict or pollute learned fleet medians.
func TestRedisInfoAuthorityPartialCensusPreservesOnlyOwnedThresholds(t *testing.T) {
	rows := "host_memory 17179869184 1073741824\n6380 93 100 80 30 1000\n6381 10 100 9 0 10\n6382 10 100 9 0 10"
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return rows, nil }}
	settings := syntheticSettings(source)
	settings.Hosts[1].RedisNodePorts = []int{6380, 6381, 6382, 6383}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	env.baseline, err = newBaselineStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	probe := newRedisMemoryProbe()
	findings, err := probe.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	critical, buffers, unknown := false, false, false
	for _, finding := range findings {
		if finding.class == "mem-skew" || finding.class == "clients-spike" || finding.class == "redis-host-capacity" {
			t.Fatal("partial INFO census established or cleared a fleet/host verdict")
		}
		critical = critical || finding.class == "node-mem-critical" && !finding.healthy
		buffers = buffers || finding.class == "client-buffers" && !finding.healthy
		unknown = unknown || finding.isUnobservable()
	}
	if !critical || !buffers || !unknown {
		t.Fatal("partial INFO census discarded an owned breach or its visibility gap")
	}
	if len(env.baseline.metricSamples) != 0 {
		t.Fatal("partial INFO census polluted the fleet baseline")
	}
	rows += "\n6383 10 100 9 0 10"
	findings, err = probe.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	skew, clients := false, false
	for _, finding := range findings {
		skew = skew || finding.class == "mem-skew" && !finding.healthy
		clients = clients || finding.class == "clients-spike" && !finding.healthy
		if finding.isUnobservable() {
			t.Fatal("corrected complete census did not recover observation authority")
		}
	}
	if !skew || !clients || len(env.baseline.metricSamples["redis/used-median"]) != 1 {
		t.Fatal("complete corrected census did not restore fleet verdicts and baseline")
	}
}

// Observation loss must not reset a cached trip battery. A real healthy
// measurement rearms it, so a later independent breach gets fresh evidence.
func TestRedisInfoAuthorityUnknownDoesNotRearmBufferBattery(t *testing.T) {
	row := ""
	batteryCalls := 0
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "for p in") {
				return "6380 100 1000 90 0 10\n" + row + "\n6382 100 1000 90 0 10", nil
			}
			return "", nil
		},
		redisFn: func(HostSettings, int, ...string) (string, error) {
			batteryCalls++
			return "", nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts[1].Name = "redis-info.example"
	signal := NewRedisBuffersSignal()
	for i, sample := range []struct {
		row           string
		wantBatteries int
	}{
		{row: "6381 100 1000 60 30 10", wantBatteries: 1},
		{row: "6381 100 1000 60 NaN 10", wantBatteries: 1},
		{row: "6381 100 1000 60 30 10", wantBatteries: 1},
		{row: "6381 100 1000 90 0 10", wantBatteries: 1},
		{row: "6381 100 1000 60 30 10", wantBatteries: 2},
	} {
		row = sample.row
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			redisInfoAuthorityRequireGap(t, alerts)
		}
		if batteryCalls != sample.wantBatteries {
			t.Fatalf("sample %d: battery calls=%d want=%d", i, batteryCalls, sample.wantBatteries)
		}
	}
}

// Preserve the strict existing thresholds on authoritative complete inputs.
func TestRedisInfoAuthorityThresholdControls(t *testing.T) {
	for _, sample := range []struct {
		used int
		want string
	}{
		{used: 85}, {used: 86, want: "node-mem-high"}, {used: 92, want: "node-mem-high"}, {used: 93, want: "node-mem-critical"},
	} {
		rows := fmt.Sprintf("6380 %d 100 80 0 10", sample.used)
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return rows, nil }}
		settings := syntheticSettings(source)
		settings.Hosts[1].RedisNodePorts = []int{6380}
		alerts, err := NewRedisMemorySignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if sample.want == "" {
			if len(alerts) != 0 {
				t.Fatal("exact 85 percent memory boundary is not healthy")
			}
		} else {
			requireAlertClass(t, alerts, sample.want)
		}
	}
	for _, clients := range []int{25, 26} {
		rows := fmt.Sprintf("6380 100 1000 60 %d 10", clients)
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return rows, nil }}
		settings := syntheticSettings(source)
		settings.Hosts[1].RedisNodePorts = []int{6380}
		alerts, err := NewRedisBuffersSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if clients == 25 {
			if len(alerts) != 0 {
				t.Fatal("exact 25 percent buffer boundary is not healthy")
			}
		} else {
			requireAlertClass(t, alerts, "client-buffers")
		}
	}
}
