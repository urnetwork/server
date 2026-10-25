// Native CONFIG reply authority is exercised before reduction through public Run.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Each scripted node owns one raw reply and its independent producer outcome.
type cardinalityConfigReply struct {
	body     string
	exitCode int
}

// Preserve the public Source.Host context rather than reducing prebuilt census data.
type cardinalityConfigHostSource struct {
	*syntheticSource
	observeHost func(context.Context, HostSettings, string) (string, error)
}

// No real source fallback is available to this test-owned host boundary.
func (self *cardinalityConfigHostSource) Host(ctx context.Context, host HostSettings, command string) (string, error) {
	return self.observeHost(ctx, host, command)
}

// All witnesses belong to bounded private fake commands, never Redis or Mimir.
type cardinalityConfigNativeFixture struct {
	settings     SignalSettings
	directory    string
	callPath     string
	gatewayCalls int
	hostCalls    int
	hostOutputs  []string
}

// The Mimir response is independent of the live Redis source reduction.
func cardinalityConfigMetrics(positive bool) string {
	values := map[string]int{"total": 70000, "egress_sums": 625, "egress_counts": 625}
	if positive {
		values["egress_buckets"], values["redis_latency"] = 8100, 9000
	}
	return metricCardinalityFixture(values, "synthetic-config-private-label")
}

// Keep the real generated host script and replace only its two native producers.
func newCardinalityConfigNativeFixture(t *testing.T, replies []cardinalityConfigReply, metricResponse string) *cardinalityConfigNativeFixture {
	t.Helper()
	fixture := &cardinalityConfigNativeFixture{directory: t.TempDir()}
	fixture.callPath = filepath.Join(fixture.directory, "calls")
	redisScript := `#!/bin/sh
set -eu
[ "$#" -eq 8 ] && [ "$1" = --raw ] && [ "$2" = -h ] && [ "$3" = 127.0.0.1 ] && [ "$4" = -p ] && [ "$6" = CONFIG ] && [ "$7" = GET ] && [ "$8" = latency-tracking ] || exit 126
case "$5" in 6380|6381) ;; *) exit 126 ;; esac
printf 'redis-cli %s\n' "$*" >> "$CARDINALITY_CONFIG_CALLS"
/bin/cat "$CARDINALITY_CONFIG_ROOT/reply-$5"
code=$(/bin/cat "$CARDINALITY_CONFIG_ROOT/exit-$5")
exit "$code"
`
	timeoutScript := `#!/bin/sh
set -eu
[ "$#" -eq 10 ] && [ "$1" = 4 ] && [ "$2" = redis-cli ] || exit 126
printf 'timeout %s\n' "$*" >> "$CARDINALITY_CONFIG_CALLS"
shift
exec "$@"
`
	for name, script := range map[string]string{"redis-cli": redisScript, "timeout": timeoutScript} {
		if err := os.WriteFile(filepath.Join(fixture.directory, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	fixture.setReplies(t, replies)
	source := &cardinalityConfigHostSource{syntheticSource: &syntheticSource{}}
	source.observeHost = func(ctx context.Context, host HostSettings, command string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if host.Name == "metrics.example" && strings.Contains(command, metricCardinalityMarker) {
			if !strings.Contains(command, "curl -fsS --max-time 20") {
				t.Fatal("independent Mimir source changed its bounded command")
			}
			fixture.gatewayCalls++
			return metricResponse, nil
		}
		if host.Name != "redis-source.example" || !strings.Contains(command, metricCardinalityRedisSourceMarker) ||
			!strings.Contains(command, "for port in 6380 6381; do") || !strings.Contains(command, "CONFIG GET latency-tracking") {
			t.Fatal("public source did not supply the actual configured native CONFIG command")
		}
		fixture.hostCalls++
		commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		process := exec.CommandContext(commandCtx, "/bin/sh", "-c", command)
		process.Env = []string{
			"PATH=" + fixture.directory + ":/usr/bin:/bin", "LANG=C", "LC_ALL=C", "TZ=UTC",
			"CARDINALITY_CONFIG_ROOT=" + fixture.directory,
			"CARDINALITY_CONFIG_CALLS=" + fixture.callPath,
		}
		output, err := process.CombinedOutput()
		if commandCtx.Err() != nil {
			t.Fatal("private native fixture exceeded its existing short command budget")
		}
		if strings.Contains(string(output), "synthetic-config-private-tail") || strings.Contains(string(output), "synthetic-config-private-label") {
			t.Fatal("generated host census exposed a raw reply or metric label")
		}
		fixture.hostOutputs = append(fixture.hostOutputs, string(output))
		return string(output), err
	}
	fixture.settings = syntheticSettings(source)
	fixture.settings.Hosts = []HostSettings{
		{Name: "metrics.example", Roles: []string{"services"}},
		{Name: "redis-source.example", Roles: []string{"redis-cluster"}, RedisEntryPort: 6379, RedisNodePorts: []int{6380, 6381}},
	}
	return fixture
}

// File replacement occurs only before the next acknowledged manual cadence tick.
func (self *cardinalityConfigNativeFixture) setReplies(t *testing.T, replies []cardinalityConfigReply) {
	t.Helper()
	if len(replies) != 2 {
		t.Fatal("native CONFIG fixture requires exactly two configured node replies")
	}
	for index, port := range []int{6380, 6381} {
		if replies[index].exitCode < 0 || replies[index].exitCode > 255 {
			t.Fatal("native CONFIG fixture has an invalid scripted exit")
		}
		for name, contents := range map[string]string{
			"reply-" + strconv.Itoa(port): replies[index].body,
			"exit-" + strconv.Itoa(port):  strconv.Itoa(replies[index].exitCode),
		} {
			if err := os.WriteFile(filepath.Join(self.directory, name), []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Exact producer calls prove the malformed bytes crossed the generated shell boundary.
func (self *cardinalityConfigNativeFixture) requireCalls(t *testing.T, runs int) {
	t.Helper()
	if self.gatewayCalls != runs || self.hostCalls != runs || len(self.hostOutputs) != runs {
		t.Fatal("public fixture did not complete its independent gateway and native host reads")
	}
	calls, err := os.ReadFile(self.callPath)
	if err != nil {
		t.Fatal("native CONFIG producer witnesses are missing")
	}
	oneRun := ""
	for _, port := range []int{6380, 6381} {
		args := fmt.Sprintf("--raw -h 127.0.0.1 -p %d CONFIG GET latency-tracking", port)
		oneRun += "timeout 4 redis-cli " + args + "\nredis-cli " + args + "\n"
	}
	if string(calls) != strings.Repeat(oneRun, runs) {
		t.Fatal("generated CONFIG command changed its native timeout, argv, order or node membership")
	}
	for _, output := range self.hostOutputs {
		if _, err := parseRedisLatencyTrackingObservation(output, 2); err != nil {
			t.Fatal("actual native fixture returned an invalid fixed census before public adjudication")
		}
	}
}

// Privacy is checked in public objects, JSONL and both Markdown renderers.
func requireCardinalityConfigPrivacy(t *testing.T, alerts Alerts) {
	t.Helper()
	encoded, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal(err)
	}
	var jsonl strings.Builder
	if err := WriteAlertsJSONL(&jsonl, alerts); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"synthetic-config-private-tail", "synthetic-config-private-label", "6380", "6381"} {
		if strings.Contains(string(encoded), marker) || strings.Contains(jsonl.String(), marker) || strings.Contains(AlertsMarkdown(alerts), marker) {
			t.Fatal("CONFIG reply authority leaked a private payload or node port")
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, marker)
		}
	}
}

// Invalid nodes must remain unknown without manufacturing an enabled/disabled verdict.
func requireCardinalityConfigUnknown(t *testing.T, alerts Alerts, invalid int, unreachable int) {
	t.Helper()
	unknown := requireAlertClass(t, alerts, "redis-latencystats-source-unobservable")
	if !isVisibilityAlert(unknown) || unknown.Target != "redis-cluster" ||
		!strings.Contains(unknown.Observed, "source_invalid_nodes="+strconv.Itoa(invalid)) ||
		!strings.Contains(unknown.Observed, "source_unreachable_nodes="+strconv.Itoa(unreachable)) {
		t.Fatal("native malformed reply lost exact source coverage visibility")
	}
	requireCardinalityConfigPrivacy(t, alerts)
}

// The causal reply has a valid disabled prefix, blank third row and hidden fourth row.
func TestCardinalityNativeConfigDiscardedTailIsUnknown(t *testing.T) {
	reply := cardinalityConfigReply{body: "latency-tracking\nno\n\nsynthetic-config-private-tail\n"}
	fixture := newCardinalityConfigNativeFixture(t, []cardinalityConfigReply{reply, reply}, cardinalityConfigMetrics(false))
	alerts, err := NewCardinalitySignal().Run(context.Background(), fixture.settings)
	if err != nil {
		t.Fatal("successful native CONFIG fixture failed before reply-authority adjudication")
	}
	fixture.requireCalls(t, 1)
	if len(alerts) != 1 || alerts[0].Class != "redis-latencystats-source-unobservable" {
		t.Fatal("generated CONFIG reply tail was accepted as disabled source authority")
	}
	requireCardinalityConfigUnknown(t, alerts, 2, 0)
}

// Blank-only extra records are not the default raw CLI command delimiter.
func TestCardinalityNativeConfigExtraBlankRecordsAreUnknown(t *testing.T) {
	for _, body := range []string{"latency-tracking\nno\n\n", "latency-tracking\nyes\n\n\n"} {
		reply := cardinalityConfigReply{body: body}
		fixture := newCardinalityConfigNativeFixture(t, []cardinalityConfigReply{reply, reply}, cardinalityConfigMetrics(false))
		alerts, err := NewCardinalitySignal().Run(context.Background(), fixture.settings)
		if err != nil || len(alerts) != 1 {
			t.Fatal("extra blank CONFIG records became source health or drift")
		}
		fixture.requireCalls(t, 1)
		requireCardinalityConfigUnknown(t, alerts, 2, 0)
	}
}

// A complete raw command has its own final LF; truncation is not normalized.
func TestCardinalityNativeConfigMissingTerminalLineFeedIsUnknown(t *testing.T) {
	for _, body := range []string{"latency-tracking\nno", "latency-tracking\nyes"} {
		reply := cardinalityConfigReply{body: body}
		fixture := newCardinalityConfigNativeFixture(t, []cardinalityConfigReply{reply, reply}, cardinalityConfigMetrics(false))
		alerts, err := NewCardinalitySignal().Run(context.Background(), fixture.settings)
		if err != nil || len(alerts) != 1 {
			t.Fatal("unterminated CONFIG raw reply supplied source health or drift")
		}
		fixture.requireCalls(t, 1)
		requireCardinalityConfigUnknown(t, alerts, 2, 0)
	}
}

// Whole-stream validation rejects every extra record before shell-string capture.
func TestCardinalityNativeConfigMalformedWholeReplyIsUnknown(t *testing.T) {
	for _, body := range []string{
		"latency-tracking\nno\nsynthetic-config-private-tail\n",
		"latency-tracking\nno\n\n\nsynthetic-config-private-tail\n",
		"latency-tracking\nno\n\nredis-latency-config-exit 0\n",
		"latency-tracking\nno\n\x00",
		"latency-tracking\nno\x00\n",
		"latency-tracking\x00\nno\n",
		"wrong-key\nno\n", "latency-tracking\n\n", "latency-tracking\r\nno\r\n", "",
	} {
		reply := cardinalityConfigReply{body: body}
		fixture := newCardinalityConfigNativeFixture(t, []cardinalityConfigReply{reply, reply}, cardinalityConfigMetrics(false))
		alerts, err := NewCardinalitySignal().Run(context.Background(), fixture.settings)
		if err != nil || len(alerts) != 1 {
			t.Fatal("malformed complete CONFIG stream became source health or drift")
		}
		fixture.requireCalls(t, 1)
		requireCardinalityConfigUnknown(t, alerts, 2, 0)
	}
}

// Ordinary key/no/LF is still complete disabled-source authority and quiet output.
func TestCardinalityNativeConfigTwoLineNoIsHealthy(t *testing.T) {
	reply := cardinalityConfigReply{body: "latency-tracking\nno\n"}
	fixture := newCardinalityConfigNativeFixture(t, []cardinalityConfigReply{reply, reply}, cardinalityConfigMetrics(false))
	alerts, err := NewCardinalitySignal().Run(context.Background(), fixture.settings)
	if err != nil || len(alerts) != 0 {
		t.Fatal("valid two-line disabled CONFIG reply lost source health")
	}
	fixture.requireCalls(t, 1)
	if fixture.hostOutputs[0] != "expected=2 enabled=0 disabled=2 invalid=0 unreachable=0\n" {
		t.Fatal("valid no reply changed the fixed census")
	}
}

// Ordinary key/yes/LF keeps the existing source-drift class and its severity.
func TestCardinalityNativeConfigTwoLineYesIsDrift(t *testing.T) {
	reply := cardinalityConfigReply{body: "latency-tracking\nyes\n"}
	fixture := newCardinalityConfigNativeFixture(t, []cardinalityConfigReply{reply, reply}, cardinalityConfigMetrics(false))
	alerts, err := NewCardinalitySignal().Run(context.Background(), fixture.settings)
	if err != nil || len(alerts) != 1 {
		t.Fatal("valid two-line enabled CONFIG reply lost source drift")
	}
	fixture.requireCalls(t, 1)
	alert := requireAlertClass(t, alerts, "redis-latencystats-source-drift")
	if alert.Severity != SeverityWarn || alert.Sustain != 1 || !strings.Contains(alert.Observed, "source_enabled_nodes=2") {
		t.Fatal("valid yes reply changed source-drift policy")
	}
	requireCardinalityConfigPrivacy(t, alerts)
}

// A malformed sibling cannot erase a separately valid enabled node.
func TestCardinalityNativeConfigValidSiblingSurvivesMalformedNode(t *testing.T) {
	fixture := newCardinalityConfigNativeFixture(t, []cardinalityConfigReply{
		{body: "latency-tracking\nyes\n"},
		{body: "latency-tracking\nno\n\nsynthetic-config-private-tail\n"},
	}, cardinalityConfigMetrics(false))
	alerts, err := NewCardinalitySignal().Run(context.Background(), fixture.settings)
	if err != nil || len(alerts) != 2 {
		t.Fatal("native partial-node observation erased valid drift or malformed-node visibility")
	}
	fixture.requireCalls(t, 1)
	requireAlertClass(t, alerts, "redis-latencystats-source-drift")
	requireCardinalityConfigUnknown(t, alerts, 1, 0)
}

// Command failure remains unreachable even when partial stdout looks valid.
func TestCardinalityNativeConfigFailedProducerCannotSupplyHealthy(t *testing.T) {
	for _, code := range []int{1, 124} {
		fixture := newCardinalityConfigNativeFixture(t, []cardinalityConfigReply{
			{body: "latency-tracking\nno\n"},
			{body: "latency-tracking\nno\n", exitCode: code},
		}, cardinalityConfigMetrics(false))
		alerts, err := NewCardinalitySignal().Run(context.Background(), fixture.settings)
		if err != nil || len(alerts) != 1 {
			t.Fatal("failed CONFIG producer's valid prefix supplied source health")
		}
		fixture.requireCalls(t, 1)
		requireCardinalityConfigUnknown(t, alerts, 0, 1)
	}
}

// Malformed live Redis evidence cannot suppress independent Mimir positives.
func TestCardinalityNativeConfigUnknownKeepsMimirPositive(t *testing.T) {
	reply := cardinalityConfigReply{body: "latency-tracking\nno\n\nsynthetic-config-private-tail\n"}
	fixture := newCardinalityConfigNativeFixture(t, []cardinalityConfigReply{reply, reply}, cardinalityConfigMetrics(true))
	alerts, err := NewCardinalitySignal().Run(context.Background(), fixture.settings)
	if err != nil || len(alerts) != 3 {
		t.Fatal("malformed CONFIG reply erased independent accepted-series findings")
	}
	fixture.requireCalls(t, 1)
	requireAlertClass(t, alerts, "taskworker-histogram-cardinality")
	requireAlertClass(t, alerts, "redis-latencystats-cardinality")
	requireCardinalityConfigUnknown(t, alerts, 2, 0)
}

// Real gate and notifier transitions retain drift until two complete disabled reads.
func TestCardinalityNativeConfigUnknownCannotClearSourceDrift(t *testing.T) {
	yes := cardinalityConfigReply{body: "latency-tracking\nyes\n"}
	no := cardinalityConfigReply{body: "latency-tracking\nno\n"}
	bad := cardinalityConfigReply{body: "latency-tracking\nno\n\nsynthetic-config-private-tail\n"}
	fixture := newCardinalityConfigNativeFixture(t, []cardinalityConfigReply{yes, yes}, cardinalityConfigMetrics(false))
	signal := NewCardinalitySignal()
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	key := slackIncidentKey(Alert{SignalID: signal.ID(), Class: "redis-latencystats-source-drift", Target: "redis-cluster"})
	postSignalRunsThroughTheGate(t, signal, fixture.settings, 5, func(next int) {
		if next > 0 {
			harness.advance(5 * time.Minute)
			notifier.stateLock.Lock()
			incident, present := notifier.keyIncidents[key]
			absent := -1
			if present {
				absent = incident.signalKeyAbsentRuns[signal.Key()]
			}
			notifier.stateLock.Unlock()
			want := 0
			if next == 4 {
				want = 1
			}
			if !present || absent != want {
				t.Fatal("malformed native CONFIG evidence cleared or advanced absence for observed source drift")
			}
		}
		if next == 1 || next == 2 {
			fixture.setReplies(t, []cardinalityConfigReply{bad, bad})
		} else if next >= 3 {
			fixture.setReplies(t, []cardinalityConfigReply{no, no})
		}
	}, notifier)
	fixture.requireCalls(t, 5)
	notifier.stateLock.Lock()
	_, present := notifier.keyIncidents[key]
	notifier.stateLock.Unlock()
	if present {
		t.Fatal("complete native disabled-source observations lost existing recovery")
	}
	closeSlackNotifier(t, notifier)
	identity := "redis-latencystats-source-drift · redis-cluster"
	if countSlackPosts(webhook.posted(), "[WARN] raised", identity) != 1 || countSlackPosts(webhook.posted(), "[CLEARED]", identity) != 1 {
		t.Fatal("native reply authority changed named source-drift raise/hold/clear transitions")
	}
}
