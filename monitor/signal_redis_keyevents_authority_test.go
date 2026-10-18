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

// Execute the actual generated host command with local scripted executables.
// Exit codes force failures without waiting for real timeouts or using Redis.
func redisKeyEventSourceFixture(t *testing.T, flags string, subscribers int, failedCommand, reply string, exitCode int) (SignalSettings, string) {
	t.Helper()
	directory := t.TempDir()
	callPath := filepath.Join(directory, "calls")
	redisScript := `#!/bin/sh
printf 'redis-cli %s\n' "$*" >> "$KEYEVENT_TEST_CALLS"
port=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -p) port=$2; shift 2 ;;
    -h) shift 2 ;;
    --raw|-2) shift ;;
    CONFIG|CLIENT) operation=$1; break ;;
    *) exit 126 ;;
  esac
done
if [ "$port" = 6381 ] && [ "$operation" = "$KEYEVENT_TEST_FAILED_COMMAND" ]; then
  printf '%s' "$KEYEVENT_TEST_REPLY"
  exit "$KEYEVENT_TEST_EXIT"
fi
case "$operation" in
  CONFIG) printf 'notify-keyspace-events\n%s\n' "$KEYEVENT_TEST_FLAGS" ;;
  CLIENT)
    count=0
    while [ "$count" -lt "$KEYEVENT_TEST_SUBSCRIBERS" ]; do
      count=$((count + 1))
      printf 'id=%s addr=192.0.2.20:1234 name=synthetic-private-client flags=P db=0 sub=0 psub=2 cmd=psubscribe\n' "$count"
    done ;;
  *) exit 126 ;;
esac
`
	timeoutScript := `#!/bin/sh
printf 'timeout %s\n' "$*" >> "$KEYEVENT_TEST_CALLS"
while [ "$#" -gt 0 ]; do
  case "$1" in
    -k|--kill-after) shift 2 ;;
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
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		process := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		process.Env = []string{
			"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C",
			"KEYEVENT_TEST_CALLS=" + callPath,
			"KEYEVENT_TEST_FLAGS=" + flags,
			"KEYEVENT_TEST_SUBSCRIBERS=" + strconv.Itoa(subscribers),
			"KEYEVENT_TEST_FAILED_COMMAND=" + failedCommand,
			"KEYEVENT_TEST_REPLY=" + reply,
			"KEYEVENT_TEST_EXIT=" + strconv.Itoa(exitCode),
		}
		output, err := process.CombinedOutput()
		if strings.Contains(string(output), "synthetic-private") || strings.Contains(string(output), "192.0.2.20") {
			t.Error("host aggregate exposed raw Redis observation data")
		}
		return string(output), err
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "redis-subject.example", Roles: []string{"redis-cluster"}, RedisNodePorts: []int{6380, 6381, 6382}},
		{Name: "redis-control.example", Roles: []string{"redis-cluster"}, RedisEntryPort: 6379, RedisNodePorts: []int{6480, 6481}},
	}
	return settings, callPath
}

// Reject a failed host's census without resolving its old findings or losing
// independently authoritative results from the other configured host.
func redisKeyEventRequireUnknown(t *testing.T, settings SignalSettings) {
	t.Helper()
	findings := redisTestProbeFindings(t, redisKeyEventProbe{}, settings)
	unknown, controlHealthy := false, false
	for _, result := range findings {
		if result.class == "cannot-observe" && result.target == "redis-subject.example/key-events" {
			unknown = true
			if !strings.Contains(result.observed, "unobserved_nodes=3") {
				t.Error("visibility lost the configured node denominator")
			}
		}
		if result.healthy && (result.class == "keyevent-config-drift" || strings.HasPrefix(result.target, "redis-subject.example:")) {
			t.Error("unavailable node census established a healthy result")
		}
		if result.healthy && result.target == "redis-control.example:6480" {
			controlHealthy = true
		}
	}
	if !unknown || !controlHealthy {
		t.Errorf("visibility=%t independent_control_healthy=%t, want both", unknown, controlHealthy)
	}
}

// A valid-looking reply cannot erase a nonzero inner exit, including timeout
// and unavailable executable outcomes hidden by the former shell pipelines.
func TestRedisKeyEventsSourceCommandFailuresCannotEstablishHealth(t *testing.T) {
	for _, operation := range []string{"CONFIG", "CLIENT"} {
		for _, exitCode := range []int{1, 124, 127, 137} {
			reply := ""
			if operation == "CONFIG" {
				reply = "notify-keyspace-events\n\n"
			}
			settings, _ := redisKeyEventSourceFixture(t, "", 0, operation, reply, exitCode)
			redisKeyEventRequireUnknown(t, settings)
		}
	}
}

// redis-cli can exit zero for Redis errors. Missing/partial protocol output
// must also remain distinct from a successfully observed empty value.
func TestRedisKeyEventsSourceRejectsErrorAndMalformedReplies(t *testing.T) {
	cases := []struct {
		operation string
		reply     string
	}{
		{operation: "CONFIG", reply: "NOAUTH synthetic-private-token\n"},
		{operation: "CONFIG", reply: "WRONGPASS synthetic-private-token\n"},
		{operation: "CONFIG", reply: "NOPERM synthetic-private-token\n"},
		{operation: "CONFIG", reply: ""},
		{operation: "CONFIG", reply: "notify-keyspace-events\n"},
		{operation: "CONFIG", reply: "notify-keyspace-events\nKE"},
		{operation: "CONFIG", reply: "other-setting\nKE\n"},
		{operation: "CONFIG", reply: "notify-keyspace-events\nKE\nextra\n"},
		{operation: "CONFIG", reply: "notify-keyspace-events\nsynthetic-private-token\n"},
		{operation: "CLIENT", reply: "NOAUTH synthetic-private-token\n"},
		{operation: "CLIENT", reply: "ERR synthetic-private-token\n"},
		{operation: "CLIENT", reply: "id=1 flags=P"},
		{operation: "CLIENT", reply: "id=1 flags=\n"},
		{operation: "CLIENT", reply: "id=1 flags=N\n\n"},
		{operation: "CLIENT", reply: "id=1 flags=P trailing\n\n"},
		{operation: "CLIENT", reply: "id=1 flags=P flags=P\n\n"},
		{operation: "CLIENT", reply: "id=1 flags=P\nid=1 flags=P\n\n"},
		{operation: "CLIENT", reply: "id=1 flags=P\n\nid=2 flags=P\n\n"},
	}
	for _, test := range cases {
		settings, _ := redisKeyEventSourceFixture(t, "KE", 2, test.operation, test.reply, 0)
		redisKeyEventRequireUnknown(t, settings)
	}
}

// CONFIG retains its empty value line, while redis-cli renders CLIENT LIST
// verbatim: a successful empty bulk string is zero bytes, not a failed command.
func TestRedisKeyEventsSourceObservedEmptyValuesRemainHealthy(t *testing.T) {
	settings, _ := redisKeyEventSourceFixture(t, "", 0, "", "", 0)
	findings := redisTestProbeFindings(t, redisKeyEventProbe{}, settings)
	if len(findings) != 6 {
		t.Fatalf("got %d findings, want config and all five node verdicts", len(findings))
	}
	for _, result := range findings {
		if !result.healthy {
			t.Errorf("observed dark state was not healthy: class=%s", result.class)
		}
	}
}

// Sparse inventory must neither contact nor invent the intervening port.
func TestRedisKeyEventsSourceUsesExactPortsAndHardCommandBounds(t *testing.T) {
	settings, callPath := redisKeyEventSourceFixture(t, "KE", 2, "", "", 0)
	settings.Hosts[0].RedisNodePorts = []int{6382, 6380}
	findings := redisTestProbeFindings(t, redisKeyEventProbe{}, settings)
	for _, result := range findings {
		if !result.healthy {
			t.Errorf("valid sparse census was not healthy: class=%s", result.class)
		}
		if result.target == "redis-subject.example:6381" {
			t.Error("unconfigured node acquired a verdict")
		}
	}
	calls, err := os.ReadFile(callPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if strings.Contains(line, "-p 6381 ") {
			t.Error("source contacted an unconfigured node port")
		}
		if strings.HasPrefix(line, "timeout ") && !strings.HasPrefix(line, "timeout -k 1s 3s redis-cli ") {
			t.Error("source command has no bounded termination escalation")
		}
		if strings.HasPrefix(line, "redis-cli ") && !strings.Contains(line, "--raw -h 127.0.0.1 -p ") {
			t.Error("source command did not pin the raw host-loopback endpoint")
		}
	}
}

// Complete numeric and flag authority is required at the source boundary too:
// a nominally successful host result cannot supply an invented or partial set.
func TestRedisKeyEventsRejectsMalformedOrIncompleteNodeCensus(t *testing.T) {
	for _, output := range []string{
		"", "6380 KE 0\n", "6380 KE 0\n6381 KE 0\n",
		"6380 KE 0\n6381 KE 0\n6382 KE 0\n6382 KE 0\n",
		"6380 KE 0\n6381 KE 0\n6399 KE 0\n",
		"6380 KE 0\n6381 KE 0\n6382 KE invalid\n",
		"6380 KE 0\n6381 KE 0\n6382 KE -1\n",
		"6380 KE 0\n6381 KE 0\n6382 KE 99999999999999999999\n",
		"6380 KE 0\n6381 KE 0\n6382 KE 0 extra\n",
		"6380 KE 0\n6381 KE 0\n6382 synthetic-private-token 0\n",
		"6380 KE 0\n6381 KE 0\n6382 KE\n",
	} {
		source := &syntheticSource{hostFn: func(h HostSettings, _ string) (string, error) {
			if h.Name == "redis-subject.example" {
				return output, nil
			}
			return "6480 KE 0\n6481 KE 0\n", nil
		}}
		settings, _ := redisKeyEventSourceFixture(t, "KE", 0, "", "", 0)
		settings.Source = source
		redisKeyEventRequireUnknown(t, settings)
	}
}

// A source failure on one host cannot erase an independently measured drift
// or overloaded subscriber population on the other host.
func TestRedisKeyEventsPartialCensusPreservesIndependentFaultsAndIdentity(t *testing.T) {
	signal := NewRedisKeyEventsSignal()
	if signal.Key() != "redis-keyevents" || signal.Number() != "9.1" || signal.ID() != "redis/key-events" {
		t.Fatal("key-event signal registration changed")
	}
	settings, _ := redisKeyEventSourceFixture(t, "KE", 0, "", "", 0)
	settings.Source = &syntheticSource{hostFn: func(h HostSettings, _ string) (string, error) {
		if h.Name == "redis-subject.example" {
			return "6380 KE 0\n", nil
		}
		return "6480 KE 1001\n6481 - 0\n", nil
	}}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	drift := requireAlertClass(t, alerts, "keyevent-config-drift")
	pubsub := requireAlertClass(t, alerts, "pubsub-conn-shape")
	if unknown.Target != "redis-subject.example/key-events" || drift.Target != "redis-control.example" || pubsub.Target != "redis-control.example:6480" {
		t.Error("partial census lost configured host/node attribution")
	}
	if !strings.Contains(drift.Observed, "unobserved_hosts=redis-subject.example unobserved_nodes=3") {
		t.Error("drift lost its missing-node qualifier")
	}
	// Alert IDs identify findings; the registry key and probe ID above have
	// separate contracts. Visibility retains its shared finding identity.
	expectedClassIdKVs := map[string]string{
		"cannot-observe":        "monitor/visibility",
		"keyevent-config-drift": "redis/keyevent-config-drift",
		"pubsub-conn-shape":     "redis/pubsub-conn-shape",
	}
	for _, alert := range alerts {
		expectedId, ok := expectedClassIdKVs[alert.Class]
		if !ok {
			t.Fatalf("partial census emitted an unexpected class %q", alert.Class)
		}
		if alert.SignalID != expectedId || alert.SignalKey != "redis-keyevents" || alert.SignalNumber != "9.1" {
			t.Errorf("class %q has identity %q/%q/%q, want 9.1/redis-keyevents/%s", alert.Class, alert.SignalNumber, alert.SignalKey, alert.SignalID, expectedId)
		}
	}
}

// Raw credentials, reply bodies and client identifiers stay out of public
// artifacts for both transport failures and exit-zero error replies.
func TestRedisKeyEventsSourceAndAlertRedaction(t *testing.T) {
	for _, sourceError := range []bool{false, true} {
		settings, _ := redisKeyEventSourceFixture(t, "KE", 2, "CONFIG", "NOAUTH synthetic-private-token\n", 0)
		if sourceError {
			settings.Source = &syntheticSource{hostFn: func(h HostSettings, _ string) (string, error) {
				if h.Name == "redis-subject.example" {
					return "6380 KE 0\n6381 KE 0\n6382 KE 0\n", errors.New("NOAUTH synthetic-private-token")
				}
				return "6480 KE 0\n6481 KE 0\n", nil
			}}
		}
		alerts, err := NewRedisKeyEventsSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, "cannot-observe")
		for _, alert := range alerts {
			encoded, err := json.Marshal(alert)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"synthetic-private", "192.0.2.20"} {
				if strings.Contains(string(encoded), forbidden) || strings.Contains(alert.Markdown(), forbidden) {
					t.Error("public alert contains a raw observation identifier")
				}
			}
		}
	}
}

// Public severity and node identity still come from a measured CLIENT census.
func TestRedisKeyEventsSourceCountsActualPubsubRows(t *testing.T) {
	settings, _ := redisKeyEventSourceFixture(t, "KE", 1001, "", "", 0)
	alerts, err := NewRedisKeyEventsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 5 {
		t.Fatalf("got %d alerts, want one measured subscriber fault per node", len(alerts))
	}
	for _, alert := range alerts {
		if alert.Class != "pubsub-conn-shape" || !strings.Contains(alert.Observed, "pubsub_conns=1001") {
			t.Error("measured pubsub rows lost their alert class or count")
		}
	}
}

// Recovery restores health only after the same complete configured census is
// observed, including the previously unavailable node.
func TestRedisKeyEventsCorrectedCensusRestoresHealth(t *testing.T) {
	settings, _ := redisKeyEventSourceFixture(t, "", 0, "", "", 0)
	complete := false
	settings.Source = &syntheticSource{hostFn: func(h HostSettings, _ string) (string, error) {
		rows := []string{}
		for _, port := range h.RedisNodePorts {
			if !complete && port == 6381 {
				continue
			}
			rows = append(rows, fmt.Sprintf("%d - 0", port))
		}
		return strings.Join(rows, "\n"), nil
	}}
	redisKeyEventRequireUnknown(t, settings)
	complete = true
	for _, result := range redisTestProbeFindings(t, redisKeyEventProbe{}, settings) {
		if !result.healthy {
			t.Error("complete corrected census did not restore health")
		}
	}
}
