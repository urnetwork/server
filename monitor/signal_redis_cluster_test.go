package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedisClusterSignalSyntheticFailedCluster(t *testing.T) {
	source := &syntheticSource{redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
		if strings.Join(args, " ") == "CLUSTER INFO" {
			return "cluster_state:fail\ncluster_slots_fail:42\ncluster_known_nodes:32", nil
		}
		return "node-id 127.0.0.1:6380@16380 master,fail", nil
	}}
	alerts, err := NewRedisClusterSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cluster-state")
}

func TestRedisClusterSignalSyntheticWedgedNode(t *testing.T) {
	source := &syntheticSource{
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			if strings.Join(args, " ") == "CLUSTER INFO" {
				return "cluster_state:ok\ncluster_slots_fail:0\ncluster_known_nodes:32", nil
			}
			return "", nil
		},
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "for p in") {
				return "redis-ping-v1 begin 3\nredis-ping-v1 6380 pong\nredis-ping-v1 6381 timeout\nredis-ping-v1 6382 pong\nredis-ping-v1 end 3\n", nil
			}
			return "", nil
		},
	}
	alerts, err := NewRedisClusterSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "node-unreachable")
}

// The real generated sweep executes with fake local commands, never Redis,
// SSH, production configuration or a network socket. Exit codes are scripted,
// so timeout/failure ordering is deterministic rather than wall-clock driven.
func redisClusterSweepFixture(t *testing.T, reply string, status int) (SignalSettings, string) {
	t.Helper()
	directory := t.TempDir()
	callPath := filepath.Join(directory, "calls")
	redisScript := `#!/bin/sh
printf '%s\n' "$*" >> "$REDIS_TEST_CALLS"
port=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -p) port=$2; shift 2 ;;
    *) shift ;;
  esac
done
if [ "$port" = 6381 ]; then
  printf '%s' "$REDIS_TEST_REPLY"
  exit "$REDIS_TEST_STATUS"
fi
printf 'PONG\n'
`
	timeoutScript := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in
    -k|--kill-after) shift 2 ;;
    --kill-after=*) shift ;;
    [0-9]*) shift; break ;;
    *) exit 126 ;;
  esac
done
exec "$@"
`
	for name, script := range map[string]string{"redis-cli": redisScript, "timeout": timeoutScript} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	source := &syntheticSource{
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			if strings.Join(args, " ") == "CLUSTER INFO" {
				return "cluster_state:ok\ncluster_slots_fail:0\ncluster_known_nodes:3", nil
			}
			return "", nil
		},
		hostFn: func(_ HostSettings, command string) (string, error) {
			if !strings.Contains(command, "for p in") {
				return "", nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
			cmd.Env = []string{
				"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C",
				"REDIS_TEST_CALLS=" + callPath, "REDIS_TEST_REPLY=" + reply,
				fmt.Sprintf("REDIS_TEST_STATUS=%d", status),
			}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("synthetic sweep command failed: %v", err)
			}
			return string(out), nil
		},
	}
	return syntheticSettings(source), callPath
}

// Assert public output, including JSON and Markdown, without exposing a raw
// observation in the assertion message.
func redisClusterRequireUnknown(t *testing.T, alerts Alerts, forbidden ...string) {
	t.Helper()
	requireAlertClass(t, alerts, "cannot-observe")
	for _, alert := range alerts {
		if alert.Class == "node-unreachable" || alert.Class == "cluster-state" {
			t.Fatalf("observation uncertainty became a production page: %s", alert.Class)
		}
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range forbidden {
			if strings.Contains(string(encoded), value) || strings.Contains(alert.Markdown(), value) {
				t.Fatal("raw observation leaked into public alert output")
			}
		}
	}
}

func TestRedisClusterSignalPingExitZeroRequiresPong(t *testing.T) {
	for _, reply := range []string{"NOAUTH synthetic-private-sentinel\n", "ERR synthetic-private-sentinel\n", "", "PONG extra\n", "PONG\nPONG\n", "PONG\n\n"} {
		settings, _ := redisClusterSweepFixture(t, reply, 0)
		alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		redisClusterRequireUnknown(t, alerts, "synthetic-private-sentinel")
	}
}

func TestRedisClusterSignalLiteralPongHealthy(t *testing.T) {
	settings, _ := redisClusterSweepFixture(t, "PONG\n", 0)
	alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("complete literal PONG observations failed: err=%v alerts=%d", err, len(alerts))
	}
}

func TestRedisClusterSignalCommandFailureIsUnknown(t *testing.T) {
	for _, status := range []int{1, 126, 127, 137} {
		settings, _ := redisClusterSweepFixture(t, "NOAUTH synthetic-private-sentinel\n", status)
		alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		redisClusterRequireUnknown(t, alerts, "synthetic-private-sentinel")
	}
}

func TestRedisClusterSignalTimeoutDoesNotDiagnoseWedge(t *testing.T) {
	settings, _ := redisClusterSweepFixture(t, "", 124)
	alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "node-unreachable")
	if alert.Frame != "6381" || alert.Severity != SeverityPage || alert.Sustain != 1 {
		t.Fatal("local timeout identity or immediate severity changed")
	}
	if strings.Contains(alert.Baseline, "timeout = event-loop wedge") || !strings.Contains(alert.Context, "host-loopback-only") || !strings.Contains(alert.Mechanism, "not proof of an event-loop wedge") {
		t.Fatal("a bounded local PING failure was over-attributed or its path scope was omitted")
	}
}

func TestRedisClusterSignalSparseInventoryDoesNotProbeUnconfiguredPorts(t *testing.T) {
	settings, callPath := redisClusterSweepFixture(t, "PONG\n", 0)
	settings.Hosts[1].RedisNodePorts = []int{6380, 6382}
	alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("sparse healthy inventory failed: err=%v alerts=%d", err, len(alerts))
	}
	calls, err := os.ReadFile(callPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "6381") || len(strings.Split(strings.TrimSpace(string(calls)), "\n")) != 2 {
		t.Fatal("sweep expanded beyond the exact configured node ports")
	}
}

func TestRedisClusterSignalMissingPingOutputIsUnknown(t *testing.T) {
	source := &syntheticSource{redisFn: func(_ HostSettings, _ int, _ ...string) (string, error) {
		return "cluster_state:ok\ncluster_slots_fail:0\ncluster_known_nodes:3", nil
	}}
	alerts, err := NewRedisClusterSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	redisClusterRequireUnknown(t, alerts)
}

func TestRedisClusterSignalDuplicateOrForeignPingOutputIsUnknown(t *testing.T) {
	for _, output := range []string{
		"redis-ping-v1 begin 3\nredis-ping-v1 6380 pong\nredis-ping-v1 6381 timeout\nredis-ping-v1 6381 pong\nredis-ping-v1 6382 pong\nredis-ping-v1 end 3\n",
		"redis-ping-v1 begin 3\nredis-ping-v1 6380 pong\nredis-ping-v1 9999 timeout\nredis-ping-v1 6382 pong\nredis-ping-v1 end 3\n",
		"redis-ping-v1 begin 3\nredis-ping-v1 6380 pong\nredis-ping-v1 6381 synthetic-private-sentinel\nredis-ping-v1 6382 pong\nredis-ping-v1 end 3\n",
	} {
		source := &syntheticSource{
			redisFn: func(_ HostSettings, _ int, _ ...string) (string, error) {
				return "cluster_state:ok\ncluster_slots_fail:0\ncluster_known_nodes:3", nil
			},
			hostFn: func(_ HostSettings, _ string) (string, error) { return output, nil },
		}
		alerts, err := NewRedisClusterSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		redisClusterRequireUnknown(t, alerts, "synthetic-private-sentinel")
	}
}

func TestRedisClusterSignalPartialPingRetainsKnownTimeout(t *testing.T) {
	source := &syntheticSource{
		redisFn: func(_ HostSettings, _ int, _ ...string) (string, error) {
			return "cluster_state:ok\ncluster_slots_fail:0\ncluster_known_nodes:3", nil
		},
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "for p in") {
				return "redis-ping-v1 begin 3\nredis-ping-v1 6380 pong\nredis-ping-v1 6381 timeout\n", nil
			}
			return "", nil
		},
	}
	alerts, err := NewRedisClusterSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	if alert := requireAlertClass(t, alerts, "node-unreachable"); alert.Frame != "6381" {
		t.Fatal("partial coverage lost exact timeout authority")
	}
}

func TestRedisClusterSignalInvalidInfoIsUnknown(t *testing.T) {
	for _, info := range []string{
		"NOAUTH synthetic-private-sentinel", "",
		"cluster_state:ok\ncluster_slots_fail:0",
		"cluster_state:ok\ncluster_slots_fail:invalid\ncluster_known_nodes:3",
		"cluster_state:fail\ncluster_state:ok\ncluster_slots_fail:0\ncluster_known_nodes:3",
	} {
		settings, _ := redisClusterSweepFixture(t, "PONG\n", 0)
		settings.Source.(*syntheticSource).redisFn = func(_ HostSettings, _ int, _ ...string) (string, error) { return info, nil }
		alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		redisClusterRequireUnknown(t, alerts, "synthetic-private-sentinel")
	}
}

func TestRedisClusterSignalPartialInfoRetainsKnownFault(t *testing.T) {
	for _, info := range []string{
		"cluster_state:fail", "cluster_slots_fail:42",
		"cluster_state:fail\ncluster_slots_fail:invalid\ncluster_known_nodes:3",
	} {
		settings, _ := redisClusterSweepFixture(t, "PONG\n", 0)
		settings.Source.(*syntheticSource).redisFn = func(_ HostSettings, _ int, args ...string) (string, error) {
			if strings.Join(args, " ") == "CLUSTER INFO" {
				return info, nil
			}
			return "", nil
		}
		alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, "cluster-state")
		requireAlertClass(t, alerts, "cannot-observe")
	}
}

func TestRedisClusterSignalTransportFailurePreservesKnownClusterFault(t *testing.T) {
	source := &syntheticSource{
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			if strings.Join(args, " ") == "CLUSTER INFO" {
				return "cluster_state:fail\ncluster_slots_fail:42\ncluster_known_nodes:3", nil
			}
			return "", nil
		},
		hostFn: func(_ HostSettings, _ string) (string, error) {
			return "", errors.New("permission denied synthetic-private-sentinel")
		},
	}
	alerts, err := NewRedisClusterSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal("one source error discarded the independent cluster fault")
	}
	requireAlertClass(t, alerts, "cluster-state")
	requireAlertClass(t, alerts, "cannot-observe")
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "synthetic-private-sentinel")
	}
}

func TestRedisClusterSignalCancellationDropsPartialFindings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &syntheticSource{
		redisFn: func(_ HostSettings, _ int, _ ...string) (string, error) {
			return "cluster_state:fail\ncluster_slots_fail:42\ncluster_known_nodes:3", nil
		},
		hostFn: func(_ HostSettings, _ string) (string, error) {
			cancel()
			return "redis-ping-v1 begin 3\nredis-ping-v1 6380 timeout\n", nil
		},
	}
	alerts, err := NewRedisClusterSignal().Run(ctx, syntheticSettings(source))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("cancellation exposed partial findings: err=%v alerts=%d", err, len(alerts))
	}
}
