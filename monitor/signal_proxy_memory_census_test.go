package monitor

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// proxyCensusOutput executes the exact applicability producer. A nonempty
// synthetic process uses a declared suffix seam before unrelated host reads;
// absence and failed-empty cases execute the entire emitted command unchanged.
func proxyCensusOutput(t *testing.T, command string, unitStatus, processStatus int, rows string, strict bool, suffix string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	fakeUnit := "#!/bin/sh\nprintf 'synthetic-private-unit-stderr\\n' >&2\nexit \"$SYNTHETIC_UNIT_STATUS\"\n"
	fakeProcess := `#!/bin/sh
status=$SYNTHETIC_PROCESS_STATUS
case "$1" in
  -e)
    if [ "$2" != "-o" ] || [ "$3" != "pid=,rss=,comm=" ]; then exit 97; fi
    printf '%s' "$SYNTHETIC_PROCESS_ROWS"
    ;;
  -C)
    selected=$(printf '%s\n' "$SYNTHETIC_PROCESS_ROWS" | awk 'NF==3 && $3=="bringyour-proxy" {print $1, $2}')
    printf '%s' "$selected"
    if [ "$status" -eq 0 ] && [ -z "$selected" ]; then status=1; fi
    ;;
  *) exit 97 ;;
esac
printf 'synthetic-private-ps-stderr\n' >&2
exit "$status"
`
	for name, text := range map[string]string{"systemctl": fakeUnit, "ps": fakeProcess} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0700); err != nil {
			t.Fatal("could not create synthetic applicability command")
		}
	}
	if suffix != "" {
		marker := "if ! boot_id="
		at := strings.Index(command, marker)
		if at < 0 {
			t.Fatal("emitted applicability prefix boundary missing")
		}
		command = command[:at] + "printf '%s' " + shellSingleQuote(suffix) + "\n"
	}
	options := "set +e\nset +o pipefail\n"
	if strict {
		options = "set -e\nset -o pipefail\n"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", options+command)
	cmd.Env = []string{
		"PATH=" + dir + ":/usr/bin:/bin", "LANG=C", "LC_ALL=C", "TZ=UTC",
		"SYNTHETIC_UNIT_STATUS=" + strconv.Itoa(unitStatus),
		"SYNTHETIC_PROCESS_STATUS=" + strconv.Itoa(processStatus),
		"SYNTHETIC_PROCESS_ROWS=" + rows,
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if stderr.Len() != 0 {
		t.Fatal("applicability script leaked private command stderr")
	}
	return string(out), err
}

// proxyCensusPublicFailure requires a source failure to remain visibility at
// the registered public entry rather than become successful proxy absence.
func proxyCensusPublicFailure(t *testing.T, unitStatus, processStatus int, strict bool) {
	t.Helper()
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if !strings.Contains(command, "proxy_unit_pattern='warp-synthetic-proxy-*.service'") {
			t.Fatal("applicability command escaped the synthetic environment")
		}
		return proxyCensusOutput(t, command, unitStatus, processStatus, "", strict, "")
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "census.example", Roles: []string{"services"}}}
	alerts, err := NewProxyMemorySignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 {
		t.Fatal("failed applicability census became quiet or lost host coverage")
	}
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if !unknown.unobservable || unknown.Sustain != 2 || unknown.Target != "census.example/proxy-memory" {
		t.Fatal("failed applicability census lost source availability authority")
	}
	requireAlertOmits(t, unknown, "synthetic-private-unit-stderr", "synthetic-private-ps-stderr")
}

// Failed whole-census status 1 is unknown just like any other error; a
// successful census, rather than an exit-code guess, proves the no-match case.
func TestProxyMemoryCensusProcessErrorIsUnknown(t *testing.T) {
	for _, status := range []int{1, 2} {
		for _, strict := range []bool{false, true} {
			proxyCensusPublicFailure(t, 0, status, strict)
		}
	}
}

// A failed unit producer cannot prove absence when a complete process census finds none.
func TestProxyMemoryCensusUnitErrorIsUnknown(t *testing.T) {
	for _, strict := range []bool{false, true} {
		proxyCensusPublicFailure(t, 3, 0, strict)
	}
}

// Successful whole-census output with no matching proxy names remains scope;
// both an empty census and unrelated/lookalike process names exercise the filter.
func TestProxyMemoryCensusCompleteEmptyIsQuiet(t *testing.T) {
	for _, rows := range []string{"", "101 2048 synthetic-helper\n102 4096 bringyour-proxy-helper\n"} {
		for _, strict := range []bool{false, true} {
			source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
				out, err := proxyCensusOutput(t, command, 0, 0, rows, strict, "")
				if err != nil || out != "proxy_host 0\n" {
					t.Fatal("complete empty applicability changed its legitimate absence contract")
				}
				return out, err
			}}
			settings := syntheticSettings(source)
			settings.Hosts = []HostSettings{{Name: "census.example", Roles: []string{"services"}}}
			alerts, err := NewProxyMemorySignal().Run(context.Background(), settings)
			if err != nil || len(alerts) != 0 {
				t.Fatal("complete empty census manufactured an outage")
			}
		}
	}
}

// A failed ps command's valid-looking prefix is not an observed process set.
func TestProxyMemoryCensusFailedPrefixIsDiscarded(t *testing.T) {
	suffix := strings.TrimPrefix(proxyMemoryFixture(proxyMemoryDeltaFixtureSample()), "proxy_host 1\n")
	for _, status := range []int{1, 2} {
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			return proxyCensusOutput(t, command, 0, status, "424242 1024 bringyour-proxy\n", false, suffix)
		}}
		alerts, err := NewProxyMemorySignal().Run(context.Background(), proxyMemorySyntheticSettings(source))
		if err != nil || len(alerts) != 1 || !requireAlertClass(t, alerts, "cannot-observe").unobservable {
			t.Fatal("failed process output qualified an active or healthy host")
		}
	}
}

// Known live processes retain independently qualified OOM, memory headroom
// and UDP evidence despite unit-census failure, without claiming zero units.
func TestProxyMemoryCensusKnownProcessRetainsIndependentEvidence(t *testing.T) {
	now := syntheticSettings(nil).Now()
	sample := proxyMemoryDeltaFixtureSample()
	sample.runningUnits = 0
	sample.proxyProcesses = 1
	sample.proxyMemoryUnbounded = 1
	sample.proxyRSSKiB = 5 * 1024 * 1024
	sample.proxyMaxRSSKiB = sample.proxyRSSKiB
	sample.memAvailableKiB = 1024 * 1024
	sample.recentProxyOOMKills = 1
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		suffix := strings.TrimPrefix(proxyMemoryFixture(sample), "proxy_host 1\n")
		return proxyCensusOutput(t, command, 3, 0, "424242 5242880 bringyour-proxy\n", false, suffix)
	}}
	settings := proxyMemorySyntheticSettings(source)
	settings.Now = func() time.Time { return now }
	signal := NewProxyMemorySignal()
	signal.Run(context.Background(), settings)
	now = now.Add(time.Minute)
	sample.udpInDatagrams += 99
	sample.udpRcvbufErrors++
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("partial census discarded independent host evidence")
	}
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if !unknown.unobservable || unknown.Target != "proxy-1/proxy-running-units" {
		t.Fatal("known process erased the unit census availability boundary")
	}
	for _, class := range []string{"proxy-host-oom", "proxy-rollout-headroom", "proxy-udp-receive-drops"} {
		positive := requireAlertClass(t, alerts, class)
		if !strings.Contains(positive.Observed, "running_block_units=unknown") {
			t.Fatal("unavailable unit count was published as numeric zero")
		}
	}
	for _, alert := range alerts {
		if alert.Class == "proxy-rollout-overlap" {
			t.Fatal("missing unit census manufactured a process-versus-unit overlap")
		}
	}
}

// Legacy full frames retain known unit counts. New qualifiers reject
// duplicates, malformed values, unsupported absence and contradictory counts.
func TestProxyMemoryCensusQualifierSchema(t *testing.T) {
	expected := proxyMemoryDeltaFixtureSample()
	base := proxyMemoryFixture(expected)
	expected.proxyHost = true
	legacy, err := parseProxyMemorySample(base)
	if err != nil || legacy != expected {
		t.Fatal("legacy complete frame lost its known unit-count contract")
	}
	for _, qualifier := range []string{
		"running_units_unknown\n", "running_units_unknown \n", "running_units_unknown 2\n", "running_units_unknown -1\n", "running_units_unknown private\n",
		"running_units_unknown 0\nrunning_units_unknown 0\n", "running_units_unknown 1\n",
	} {
		if _, err := parseProxyMemorySample(base + qualifier); err == nil {
			t.Fatal("invalid unit-census qualifier was accepted")
		}
	}
	if _, err := parseProxyMemorySample("proxy_host 0\nrunning_units_unknown 1\n"); err == nil {
		t.Fatal("unknown unit census qualified absence")
	}
}

// A healthy sibling's independently observed fault is not discarded when
// another host's applicability cannot be established.
func TestProxyMemoryCensusPartialHostEvidence(t *testing.T) {
	source := &syntheticSource{hostFn: func(target HostSettings, command string) (string, error) {
		if target.Name == "census.example" {
			return proxyCensusOutput(t, command, 0, 2, "", false, "")
		}
		sample := proxyMemoryDeltaFixtureSample()
		sample.recentProxyOOMKills = 1
		return proxyMemoryFixture(sample), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "census.example", Roles: []string{"services"}},
		{Name: "positive.example", Roles: []string{"services"}},
	}
	alerts, err := NewProxyMemorySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("partial host availability discarded the sibling")
	}
	requireAlertClass(t, alerts, "proxy-host-oom")
	requireAlertClass(t, alerts, "cannot-observe")
}

// Use production cadence/sustain and real notifier Handle, not a reducer-only
// surrogate: failed applicability cannot clear an already established OOM.
func TestProxyMemoryCensusFailureCannotClearIncident(t *testing.T) {
	_, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	var phase atomic.Int32
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if phase.Load() != 0 {
			status := 2
			if phase.Load() == 2 {
				status = 0
			}
			return proxyCensusOutput(t, command, 0, status, "", false, "")
		}
		sample := proxyMemoryDeltaFixtureSample()
		sample.recentProxyOOMKills = 1
		return proxyMemoryFixture(sample), nil
	}}
	settings := proxyMemorySyntheticSettings(source)
	signal := NewProxyMemorySignal()
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan cadenceFloorTick, 1)
	handled := make(chan SignalExecution, 1)
	finished := make(chan error, 1)
	go func() {
		finished <- NewWithSignals(settings, signal).runExecutionLoop(ctx, RunLoopOptions{MinimumProbeCadence: signal.Cadence()},
			func(_ context.Context, current Signal, execution SignalExecution) error {
				notifier.Handle(current, execution)
				handled <- execution
				return nil
			}, func(cadence time.Duration) runLoopTicker {
				tick := cadenceFloorTick{cadence: cadence, ticks: make(chan time.Time, 1)}
				ticks <- tick
				return &manualRunLoopTicker{c: tick.ticks}
			})
	}()
	t.Cleanup(func() {
		cancel()
		if err := receiveCadenceFloorValue(t, finished); err != nil {
			t.Error("proxy census execution loop failed to join")
		}
	})
	advance := func(next int32) SignalExecution {
		t.Helper()
		tick := receiveCadenceFloorValue(t, ticks)
		if tick.cadence != time.Minute {
			t.Fatal("applicability repair changed proxy native cadence")
		}
		phase.Store(next)
		tick.ticks <- time.Time{}
		return receiveCadenceFloorValue(t, handled)
	}
	opened := advance(0)
	key := slackIncidentKey(requireAlertClass(t, opened.Alerts, "proxy-host-oom"))
	state := func() (bool, int) {
		notifier.stateLock.Lock()
		defer notifier.stateLock.Unlock()
		i := notifier.keyIncidents[key]
		if i == nil {
			return false, 0
		}
		return true, i.signalKeyAbsentRuns[signal.Key()]
	}
	first, second := advance(1), advance(1)
	if present, absent := state(); !present || absent != 0 {
		t.Fatal("failed applicability falsely cleared or advanced OOM absence")
	}
	if len(first.HeldAlerts) != 1 || !first.HeldAlerts[0].unobservable || first.HeldAlerts[0].Class != "cannot-observe" {
		t.Fatal("first failed applicability lost held availability")
	}
	if !requireAlertClass(t, second.Alerts, "cannot-observe").unobservable {
		t.Fatal("second failed applicability lost emitted availability")
	}
	advance(2)
	if present, absent := state(); !present || absent != 1 {
		t.Fatal("one complete no-match cleared OOM prematurely")
	}
	advance(2)
	if present, _ := state(); present {
		t.Fatal("two complete successful no-match observations failed to clear original OOM")
	}
}
