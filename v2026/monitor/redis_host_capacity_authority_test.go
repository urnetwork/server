// Exercise independent host-memory authority without production access.
package monitor

import (
	"bytes"
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

// Route each observation through its actual source channel. A node-critical
// battery must not receive the collector's malformed host-memory payload.
func redisHostCapacityAuthoritySettings(rows string) SignalSettings {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			switch {
			case strings.Contains(command, "/proc/meminfo") && strings.Contains(command, "for p in"):
				return rows, nil
			case strings.Contains(command, "CLIENT LIST"):
				return "client_list_total=1 client_cmd_hget_count=0 client_cmd_sadd_count=0 client_cmd_expire_count=0 client_output_memory_bytes=2097152 client_output_memory_max_bytes=2097152 client_output_over_1mb_count=1 client_max_idle_s=0 client_max_age_s=60 max_expire_idle_s=0", nil
			case strings.Contains(command, "ss -lnt"):
				return "accept_recv_q=0 accept_send_q=128", nil
			default:
				return "", fmt.Errorf("unexpected host-capacity fixture command")
			}
		},
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			if len(args) != 2 || args[0] != "INFO" || args[1] != "memory" {
				return "", fmt.Errorf("unexpected host-capacity fixture Redis command")
			}
			return "# Memory\nused_memory:10485760\nmaxmemory:10485760\nused_memory_dataset:8388608\nmem_clients_normal:2097152\nmem_clients_slaves:0\nmem_fragmentation_ratio:1\nused_memory_human:synthetic-private-battery-sentinel\nmaxmemory_human:synthetic-private-battery-sentinel", nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts[1].RedisNodePorts = []int{6380}
	return settings
}

// Check reducer health directly: omitted public alerts cannot prove healthy.
func TestRedisHostCapacityAuthorityFrames(t *testing.T) {
	for _, sample := range []struct {
		name, frame    string
		known, deficit bool
	}{
		{name: "healthy", frame: "host_memory 17179869184 12884901888", known: true},
		{name: "deficit", frame: "host_memory 17179869184 1073741824", known: true, deficit: true},
		{name: "observed-zero", frame: "host_memory 17179869184 0", known: true, deficit: true},
		{name: "reserve-at-ceiling", frame: "host_memory 549755813888 4294967296", known: true, deficit: true},
		{name: "missing"},
		{name: "native-unavailable", frame: "host_memory unavailable"},
		{name: "partial", frame: "host_memory 17179869184"},
		{name: "malformed", frame: "host_memory synthetic-private-sentinel 0"},
		{name: "duplicate", frame: "host_memory 17179869184 0\nhost_memory 17179869184 12884901888"},
		{name: "duplicate-invalid-first", frame: "host_memory unavailable\nhost_memory 17179869184 12884901888"},
		{name: "nan", frame: "host_memory NaN 0"},
		{name: "infinity", frame: "host_memory 17179869184 Inf"},
		{name: "negative", frame: "host_memory 17179869184 -1"},
		{name: "zero-total", frame: "host_memory 0 0"},
		{name: "available-exceeds-total", frame: "host_memory 17179869184 17179869185"},
		{name: "extra-field", frame: "host_memory 17179869184 0 extra"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			rows := sample.frame + "\n6380 100 100 80 0 10"
			settings := redisHostCapacityAuthoritySettings(rows)
			env, err := newProbeEnv(settings.withDefaults())
			if err != nil {
				t.Fatal(err)
			}
			h := env.cfg.redisSweepHosts()[0]
			observed := parseRedisHostMemory(h, rows)
			if len(observed.nodes) != 1 || observed.invalidRows || len(observed.unreachable) != 0 {
				t.Fatal("host validation changed the independent node census")
			}
			capacity := redisHostCapacityFinding(observed)
			if !sample.known {
				if !capacity.isUnobservable() || capacity.healthy || capacity.probeId != "redis/host-capacity" || capacity.frame != "host-memory" || capacity.target != h.name || capacity.sustain != 2 {
					t.Fatal("unusable host frame established or cleared host capacity")
				}
			} else if capacity.healthy == sample.deficit || capacity.isUnobservable() || capacity.class != "redis-host-capacity" {
				t.Fatal("authoritative host capacity lost its threshold verdict")
			}
			alerts, err := NewRedisMemorySignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			requireAlertClass(t, alerts, "node-mem-critical")
			for _, alert := range alerts {
				encoded, err := json.Marshal(alert)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(alert.Markdown(), "synthetic-private-sentinel") || strings.Contains(string(encoded), "synthetic-private-sentinel") {
					t.Fatal("raw invalid host frame entered public output")
				}
			}
		})
	}
}

// A measured zero still needs the existing strictly-greater-than-92 policy.
func TestRedisHostCapacityAuthorityCriticalThresholdAndCensus(t *testing.T) {
	for _, used := range []int{92, 93} {
		settings := redisHostCapacityAuthoritySettings(fmt.Sprintf("host_memory 17179869184 0\n6380 %d 100 80 0 10", used))
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		observed := parseRedisHostMemory(env.cfg.redisSweepHosts()[0], fmt.Sprintf("host_memory 17179869184 0\n6380 %d 100 80 0 10", used))
		if redisHostCapacityFinding(observed).healthy != (used == 92) {
			t.Fatal("critical threshold changed")
		}
	}
	settings := redisHostCapacityAuthoritySettings("host_memory 17179869184 0\n6380 93 100 80 0 10")
	settings.Hosts[1].RedisNodePorts = []int{6380, 6381}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := newRedisMemoryProbe().check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	critical, unknown := false, false
	for _, finding := range findings {
		if finding.probeId == "redis/host-capacity" {
			t.Fatal("partial node census judged aggregate capacity")
		}
		critical = critical || finding.class == "node-mem-critical" && !finding.healthy
		unknown = unknown || finding.isUnobservable()
	}
	if !critical || !unknown {
		t.Fatal("partial census discarded node breach or observation gap")
	}
}

// Run the actual generated collector under local fake commands. The first awk
// reads fixture meminfo; subsequent awk invocations keep real INFO parsing.
func TestRedisHostCapacityAuthorityNativeStatusAndShape(t *testing.T) {
	for _, sample := range []struct {
		name, meminfo string
		status        int
		known         bool
	}{
		{name: "healthy", meminfo: "MemTotal: 16777216 kB\nMemAvailable: 12582912 kB\n", known: true},
		{name: "zero", meminfo: "MemTotal: 16777216 kB\nMemAvailable: 0 kB\n", known: true},
		{name: "partial", meminfo: "MemTotal: 16777216 kB\n"},
		{name: "malformed", meminfo: "MemTotal: bad kB\nMemAvailable: 0 kB\n"},
		{name: "wrong-units", meminfo: "MemTotal: 16777216 MB\nMemAvailable: 0 kB\n"},
		{name: "duplicate", meminfo: "MemTotal: 16777216 kB\nMemAvailable: 0 kB\nMemAvailable: 1 kB\n"},
		{name: "failed-with-valid-stdout", status: 1},
		{name: "timeout-with-valid-stdout", status: 124},
		{name: "cancel-status-with-valid-stdout", status: 137},
	} {
		t.Run(sample.name, func(t *testing.T) {
			directory := t.TempDir()
			fixturePath := filepath.Join(directory, "meminfo")
			if err := os.WriteFile(fixturePath, []byte(sample.meminfo), 0600); err != nil {
				t.Fatal(err)
			}
			awkScript := `#!/bin/sh
if [ "$2" = /proc/meminfo ]; then
  if [ "$HOST_MEMORY_STATUS" != 0 ]; then
    printf 'host_memory 17179869184 12884901888\n'
    exit "$HOST_MEMORY_STATUS"
  fi
  exec /usr/bin/awk "$1" "$HOST_MEMORY_FIXTURE"
fi
exec /usr/bin/awk "$@"
`
			for name, script := range map[string]string{
				"awk":       awkScript,
				"redis-cli": "#!/bin/sh\nprintf '%s' \"$HOST_MEMORY_INFO\"\n",
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
				cmd.Env = []string{"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C", "HOST_MEMORY_FIXTURE=" + fixturePath, fmt.Sprintf("HOST_MEMORY_STATUS=%d", sample.status), "HOST_MEMORY_INFO=" + strings.Replace(redisInfoAuthorityHealthy, "used_memory:1000", "used_memory:9300", 1)}
				out, err := cmd.Output()
				if err != nil {
					t.Fatal("local collector failed")
				}
				return string(out), nil
			}}
			settings := syntheticSettings(source)
			settings.Hosts[1].RedisNodePorts = []int{6380}
			env, err := newProbeEnv(settings.withDefaults())
			if err != nil {
				t.Fatal(err)
			}
			findings, err := newRedisMemoryProbe().check(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			critical, capacitySeen := false, false
			for _, finding := range findings {
				critical = critical || finding.class == "node-mem-critical" && !finding.healthy
				if finding.probeId == "redis/host-capacity" {
					capacitySeen = true
					if finding.isUnobservable() == sample.known {
						t.Fatal("native status/shape authority masked by INFO success")
					}
					if sample.name == "zero" && (finding.healthy || finding.class != "redis-host-capacity") {
						t.Fatal("native measured zero lost deficit")
					}
					if sample.name == "healthy" && !finding.healthy {
						t.Fatal("healthy native host frame rejected")
					}
				}
			}
			if !critical || !capacitySeen {
				t.Fatal("host visibility lost an independent node breach or host verdict")
			}
		})
	}
}

// Transport cancellation remains unavailable even with plausible partial stdout.
func TestRedisHostCapacityAuthorityTransportCanceled(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return "host_memory 17179869184 12884901888\n6380 93 100 80 0 10", context.Canceled
	}}
	settings := syntheticSettings(source)
	settings.Hosts[1].RedisNodePorts = []int{6380}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := newRedisMemoryProbe().check(context.Background(), env)
	if err == nil || len(findings) != 0 {
		t.Fatal("canceled transport produced healthy or measured findings")
	}
}

// Keep real node-critical battery collection active while the host collector
// carries a malformed canary. Host returns already-reduced shell diagnostics;
// Redis returns raw INFO, whose human fields must stay private.
func TestRedisHostCapacityAuthorityMalformedFrameKeepsBatteryChannels(t *testing.T) {
	settings := redisHostCapacityAuthoritySettings("host_memory synthetic-private-sentinel 0\n6380 10485760 10485760 8388608 2097152 10")
	source := settings.Source.(*syntheticSource)
	hostFn, redisFn := source.hostFn, source.redisFn
	coreCalls, infoCalls, clientCalls, queueCalls := 0, 0, 0, 0
	source.hostFn = func(host HostSettings, command string) (string, error) {
		if host.Name != "redis-1" {
			t.Fatal("battery fixture contacted an unexpected host")
		}
		switch {
		case strings.Contains(command, "/proc/meminfo") && strings.Contains(command, "for p in"):
			coreCalls++
		case strings.Contains(command, "CLIENT LIST"):
			clientCalls++
		case strings.Contains(command, "ss -lnt"):
			queueCalls++
		default:
			t.Fatal("battery fixture received an unexpected host source channel")
		}
		return hostFn(host, command)
	}
	source.redisFn = func(host HostSettings, port int, args ...string) (string, error) {
		if host.Name != "redis-1" || port != 6380 || len(args) != 2 || args[0] != "INFO" || args[1] != "memory" {
			t.Fatal("battery fixture received an unexpected Redis source channel")
		}
		infoCalls++
		return redisFn(host, port, args...)
	}
	alerts, err := NewRedisMemorySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if coreCalls != 1 || infoCalls != 1 || clientCalls != 1 || queueCalls != 1 {
		t.Fatal("node-critical battery changed its independent source collection bounds")
	}
	var jsonl bytes.Buffer
	if err := WriteAlertsJSONL(&jsonl, alerts); err != nil {
		t.Fatal("synthetic alert JSON rendering failed")
	}
	for _, public := range []string{jsonl.String(), AlertsMarkdown(alerts)} {
		for _, canary := range []string{"synthetic-private-sentinel", "synthetic-private-battery-sentinel"} {
			if strings.Contains(public, canary) {
				t.Fatal("raw source text entered public alert output")
			}
		}
	}
	critical := requireAlertClass(t, alerts, "node-mem-critical")
	if critical.SignalID != "redis/node-mem-critical" || critical.Target != "redis-1:6380" ||
		critical.Severity != SeverityPage || critical.Sustain != 1 || !strings.Contains(critical.Symptom, "100%") {
		t.Fatal("optional diagnostic changed the independently measured critical-memory finding")
	}
	wantEvidence := strings.Join([]string{
		"node 6380 memory: used_bytes=10485760 maxmemory_bytes=10485760 dataset_bytes=8388608 client_bytes=2097152 fragmentation_ratio=1",
		"node 6380 client buffers: client_list_total=1 client_cmd_hget_count=0 client_cmd_sadd_count=0 client_cmd_expire_count=0 client_output_memory_bytes=2097152 client_output_memory_max_bytes=2097152 client_output_over_1mb_count=1 client_max_idle_s=0 client_max_age_s=60 max_expire_idle_s=0",
		"node 6380 accept queue: accept_recv_q=0 accept_send_q=128",
	}, "\n")
	if critical.Evidence != wantEvidence {
		t.Fatal("node-critical battery lost its independently routed numeric source evidence")
	}
	unknown := false
	for _, alert := range alerts {
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{"synthetic-private-sentinel", "synthetic-private-battery-sentinel"} {
			if strings.Contains(alert.Markdown(), canary) || strings.Contains(string(encoded), canary) {
				t.Fatal("raw source text entered an individual alert")
			}
		}
		unknown = unknown || alert.SignalID == "redis/host-capacity" && alert.Class == "cannot-observe" &&
			alert.Target == "redis-1" && alert.Frame == "host-memory" && alert.Severity == SeverityWarn && alert.Sustain == 2
	}
	if !unknown {
		t.Fatal("malformed host observation lost its independent unknown finding")
	}
}

// Only the memory adapter owns host capacity. Buffer thresholds and connection
// evidence do not depend on MemAvailable; shared node gaps still apply to all.
func TestRedisHostCapacityAuthorityAdapterDependencies(t *testing.T) {
	for _, frame := range []string{
		"",
		"host_memory synthetic-private-sentinel 0",
		"host_memory 17179869184 0\nhost_memory 17179869184 12884901888",
	} {
		for _, clients := range []int{25, 26} {
			settings := redisHostCapacityAuthoritySettings(fmt.Sprintf("%s\n6380 100 1000 60 %d 10", frame, clients))
			alerts, err := NewRedisBuffersSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if clients == 25 && len(alerts) != 0 {
				t.Fatal("exact 25 percent buffer boundary depends on unrelated host memory")
			}
			if clients == 26 {
				requireAlertClass(t, alerts, "client-buffers")
			}
			for _, signal := range []Signal{NewRedisBuffersSignal(), NewRedisConnectionsSignal()} {
				observations, err := signal.Run(context.Background(), settings)
				if err != nil {
					t.Fatal(err)
				}
				for _, alert := range observations {
					if alert.SignalID == "redis/host-capacity" {
						t.Fatal("node-only adapter emitted unrelated host-capacity judgment")
					}
				}
			}
			memoryAlerts, err := NewRedisMemorySignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			unknown := false
			for _, alert := range memoryAlerts {
				unknown = unknown || alert.SignalID == "redis/host-capacity" && alert.Class == "cannot-observe"
			}
			if !unknown {
				t.Fatal("memory owner lost its required host observation gap")
			}
		}
	}
	for _, signal := range []Signal{NewRedisBuffersSignal(), NewRedisConnectionsSignal()} {
		settings := redisHostCapacityAuthoritySettings("6380 100 1000 60 25 10")
		settings.Hosts[1].RedisNodePorts = []int{6380, 6381}
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		unknown := false
		for _, alert := range alerts {
			unknown = unknown || alert.SignalID == "redis/node-mem" && alert.Class == "cannot-observe"
			if alert.SignalID == "redis/host-capacity" {
				t.Fatal("node-only adapter acquired unrelated host dependency during partial census")
			}
		}
		if !unknown {
			t.Fatal("node-only adapter lost the shared incomplete INFO census gap")
		}
	}
}
