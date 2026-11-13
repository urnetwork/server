package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The modern provider gate's readiness signal (SIGNALS.md §2.29) without a
// start (FP2-MODERN.md): the fleet's agreement on the gate, judged on the
// settled taskworkers at any instant, the desired provider.yml's switch that
// cannot be read, and the switch every finding names.

// The instant an absent switch turned the gate on by itself until the start
// was retired, and the windows the signal kept around it.
var modernGateReadinessTestFormerStart = time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)

// Instants the signal once judged differently: before, inside and after the
// former windows, the former start itself and 18:00 UTC on its day.
func modernGateReadinessTestInstants() []time.Time {
	start := modernGateReadinessTestFormerStart
	return []time.Time{
		start.AddDate(0, 0, -30),
		start.Add(-72 * time.Hour),
		start.Add(-time.Hour),
		start,
		start.Add(6 * time.Hour),
		start.Add(18 * time.Hour),
		start.AddDate(0, 0, 7),
	}
}

// The failing findings.
func modernGateReadinessTestFailing(findings []finding) []finding {
	failing := []finding{}
	for _, f := range findings {
		if !f.healthy {
			failing = append(failing, f)
		}
	}
	return failing
}

// The settled taskworkers decide the gate alike, or the signal WARNs, never
// PAGEs, at every instant: one that decides it otherwise is caught by the
// min, not hidden by the max, and while it is on anywhere one that does not
// publish it decides it off. One without the gate beside a gate off
// everywhere agrees, as in a rollout from code before the gate, and so does a
// fleet with no settled taskworker publishing at all.
func TestModernGateReadinessWarnsOnADisagreeingFleet(t *testing.T) {
	for _, c := range []struct {
		name       string
		publishing int
		onEvery    bool
		onAny      bool
		warn       bool
	}{
		{name: "off everywhere", publishing: 4, onEvery: false, onAny: false},
		{name: "on everywhere", publishing: 4, onEvery: true, onAny: true},
		{name: "one deciding it off", publishing: 4, onEvery: false, onAny: true, warn: true},
		{name: "on everywhere, one without the gate", publishing: 3, onEvery: true, onAny: true, warn: true},
		{name: "off everywhere, one without the gate", publishing: 3, onEvery: false, onAny: false},
		{name: "none publishes the gate", publishing: 0, onEvery: false, onAny: false},
	} {
		observation := modernGateReadinessObservation{
			publishing:  c.publishing,
			taskworkers: 4,
			settled:     modernGateReadinessFleet{taskworkers: 4, publishing: c.publishing, onEvery: c.onEvery, onAny: c.onAny},
		}
		failing := modernGateReadinessTestFailing(evaluateModernGateReadiness(observation))
		if !c.warn {
			if len(failing) != 0 {
				t.Errorf("%s: findings %+v, want quiet", c.name, failing)
			}
			continue
		}
		if len(failing) != 1 || failing[0].class != "modern-gate-fleet-disagrees" || failing[0].tier != tierWarn || failing[0].frame != "" || failing[0].sustain != 2 {
			t.Errorf("%s: findings %+v, want one fleet warning", c.name, failing)
		}
	}
}

// One taskworker process as Mimir holds it: when it started, and the gate it
// publishes, nil for none.
type modernGateReadinessTestProcess struct {
	started time.Time
	on      *bool
}

// count taskworker processes that started a day before at, each publishing
// the gate as on.
func modernGateReadinessTestProcesses(count int, at time.Time, on bool) []modernGateReadinessTestProcess {
	processes := []modernGateReadinessTestProcess{}
	for range count {
		processes = append(processes, modernGateReadinessTestProcess{started: at.AddDate(0, 0, -1), on: &on})
	}
	return processes
}

// One Mimir answer for the readiness query: the parts the processes give at
// at, as the query reads them, every process for the gate's presence and
// those that started five minutes before at for the agreement. An aggregate
// over no series is absent, as in Mimir.
func modernGateReadinessTestPayload(t testing.TB, at time.Time, processes []modernGateReadinessTestProcess) string {
	t.Helper()
	result := []map[string]any{}
	part := func(name string, value float64) {
		result = append(result, map[string]any{
			"metric": map[string]string{"monitor_modern_gate_part": name},
			"value":  []any{float64(at.Unix()), strconv.FormatFloat(value, 'f', -1, 64)},
		})
	}
	publishing, settled, settledGates := 0, 0, []float64{}
	for _, process := range processes {
		isSettled := process.started.Before(at.Add(-modernGateReadinessSettleAge))
		if isSettled {
			settled += 1
		}
		if process.on == nil {
			continue
		}
		publishing += 1
		if isSettled {
			gate := 0.0
			if *process.on {
				gate = 1
			}
			settledGates = append(settledGates, gate)
		}
	}
	if 0 < publishing {
		part("on_count", float64(publishing))
	}
	if 0 < len(processes) {
		part("taskworkers", float64(len(processes)))
	}
	if 0 < settled {
		part("settled_taskworkers", float64(settled))
	}
	if 0 < len(settledGates) {
		part("settled_on_count", float64(len(settledGates)))
		part("settled_on_min", slices.Min(settledGates))
		part("settled_on_max", slices.Max(settledGates))
	}
	payload, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// The signal with the desired provider.yml's failure, "" for none.
func modernGateReadinessTestSignal(failure string) Signal {
	return newModernGateReadinessSignal(func() string { return failure })
}

// Settings whose one services host answers each query by its evaluation time.
func modernGateReadinessTestSettings(t testing.TB, now time.Time, payloads map[int64]string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" || !strings.Contains(command, "127.0.0.1:3100/prometheus/api/v1/query") || !strings.Contains(command, "urnetwork_stats_provider_modern_gate_on") {
			return "", fmt.Errorf("unexpected modern gate readiness command on %s", host.Name)
		}
		for at, payload := range payloads {
			if strings.Contains(command, "'time="+strconv.FormatInt(at, 10)+"'") {
				return payload, nil
			}
		}
		return "", fmt.Errorf("no payload for the command's time")
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	return settings
}

// Through the signal's own query and the Mimir answer, at every instant the
// former windows judged, the former start included: a fleet that agrees is
// quiet, whether the gate is off or on everywhere, and one taskworker still
// deciding the gate on, as code from before the retirement does past its
// start with the switch absent, WARNs, reading the min and the max, and
// names the switch. Nothing pages: there is no start to page on.
func TestModernGateReadinessSignalJudgesTheFleetWithoutAStart(t *testing.T) {
	on := true
	for _, now := range modernGateReadinessTestInstants() {
		for _, agreeingOn := range []bool{false, true} {
			payload := modernGateReadinessTestPayload(t, now, modernGateReadinessTestProcesses(4, now, agreeingOn))
			alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{now.Unix(): payload}))
			if err != nil || len(alerts) != 0 {
				t.Errorf("at %s, agreeing on %t: alerts %+v err %v, want quiet", now, agreeingOn, alerts, err)
			}
		}

		processes := append(modernGateReadinessTestProcesses(3, now, false), modernGateReadinessTestProcess{started: now.AddDate(0, 0, -1), on: &on})
		payload := modernGateReadinessTestPayload(t, now, processes)
		alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{now.Unix(): payload}))
		if err != nil {
			t.Fatalf("at %s: %v", now, err)
		}
		if len(alerts) != 1 {
			t.Fatalf("at %s: alerts %+v, want the fleet alone", now, alerts)
		}
		alert := requireAlertClass(t, alerts, "modern-gate-fleet-disagrees")
		if alert.Severity != SeverityWarn || alert.SignalNumber != "2.29" || alert.SignalKey != "modern-gate-readiness" {
			t.Fatalf("at %s: alert %+v, want a §2.29 warning", now, alert)
		}
		for _, part := range []string{"gate_on_min=0 gate_on_max=1", "quality_speed_requires_modern_provider: true", "quality_speed_requires_modern_provider: false", "quality_speed_modern_provider_start` is retired", "SIGNALS.md §2.29", "FP2-MODERN.md"} {
			if !strings.Contains(alert.Markdown(), part) {
				t.Errorf("at %s: alert does not say %q:\n%s", now, part, alert.Markdown())
			}
		}
	}
}

// Through the signal, an hour after the former start: a block restarted two
// minutes ago, whose new process has not registered the gate yet while its
// drained one's series are still readable, is not judged and does not warn;
// the same process ten minutes after its start, deciding the gate off while
// the rest have it on, warns.
func TestModernGateReadinessSignalJudgesSettledProcesses(t *testing.T) {
	now := modernGateReadinessTestFormerStart.Add(time.Hour)
	off := false
	for _, c := range []struct {
		name    string
		process modernGateReadinessTestProcess
		warn    bool
	}{
		{name: "restarted two minutes ago, the gate not yet registered", process: modernGateReadinessTestProcess{started: now.Add(-2 * time.Minute)}},
		{name: "restarted two minutes ago, deciding the gate off", process: modernGateReadinessTestProcess{started: now.Add(-2 * time.Minute), on: &off}},
		{name: "started ten minutes ago, deciding the gate off", process: modernGateReadinessTestProcess{started: now.Add(-10 * time.Minute), on: &off}, warn: true},
	} {
		// four taskworkers with the gate on, the drained process of the
		// restarted block among them, and its new process
		processes := append(modernGateReadinessTestProcesses(4, now, true), c.process)
		payload := modernGateReadinessTestPayload(t, now, processes)
		alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{now.Unix(): payload}))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !c.warn {
			if len(alerts) != 0 {
				t.Errorf("%s: alerts %+v, want none", c.name, alerts)
			}
			continue
		}
		if len(alerts) != 1 || alerts[0].Class != "modern-gate-fleet-disagrees" || alerts[0].Severity != SeverityWarn {
			t.Errorf("%s: alerts %+v, want the fleet's warning", c.name, alerts)
		}
	}
}

// Through the signal, in the middle of a rollout from code before the gate:
// two settled taskworkers publish the gate off, two publish nothing. Both
// decide the gate off, so nothing fires at any instant, the former windows
// included.
func TestModernGateReadinessSignalIsQuietDuringARolloutFromBeforeTheGate(t *testing.T) {
	for _, now := range modernGateReadinessTestInstants() {
		processes := append(modernGateReadinessTestProcesses(2, now, false), modernGateReadinessTestProcess{started: now.AddDate(0, 0, -1)}, modernGateReadinessTestProcess{started: now.AddDate(0, 0, -1)})
		payload := modernGateReadinessTestPayload(t, now, processes)
		alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{now.Unix(): payload}))
		if err != nil || len(alerts) != 0 {
			t.Errorf("at %s: alerts %+v err %v, want quiet", now, alerts, err)
		}
	}
}

// A read the signal cannot complete is an error, never quiet, at any instant:
// no answer for the evaluation time, a part the query does not ask for, such
// as the start an older reader read, and a sample that is not a count.
func TestModernGateReadinessSignalCannotObserveAFailedRead(t *testing.T) {
	now := modernGateReadinessTestFormerStart.AddDate(0, 0, -30)
	invalidPart := func(name string, value string) string {
		return fmt.Sprintf(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"monitor_modern_gate_part":%q},"value":[%d,%q]}]}}`, name, now.Unix(), value)
	}
	for _, c := range []struct {
		name     string
		payloads map[int64]string
		err      string
	}{
		{name: "no answer", payloads: map[int64]string{}, err: "query Mimir"},
		{name: "the start", payloads: map[int64]string{now.Unix(): invalidPart("start", "1762819200")}, err: "invalid unknown series"},
		{name: "the shadow", payloads: map[int64]string{now.Unix(): invalidPart("shadow", "3")}, err: "invalid unknown series"},
		{name: "a NaN count", payloads: map[int64]string{now.Unix(): invalidPart("on_count", "NaN")}, err: "invalid sample"},
		{name: "a negative count", payloads: map[int64]string{now.Unix(): invalidPart("settled_on_min", "-1")}, err: "invalid sample"},
	} {
		alerts, err := modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, c.payloads))
		if err == nil || len(alerts) != 0 || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: alerts %+v err %v, want an observation error naming %q", c.name, alerts, err, c.err)
		}
	}
}

// A switch in the desired provider.yml that cannot be read WARNs as an
// unknown state, the switch that was meant: beside a fleet that holds the
// gate off as it says, and beside a Mimir that cannot be read. Without a
// failure nothing fires.
func TestModernGateReadinessSignalWarnsOnAnUnreadableSwitch(t *testing.T) {
	now := modernGateReadinessTestFormerStart.Add(-time.Hour)
	failure := "quality_speed_requires_modern_provider cannot be read"
	// the taskworkers on the failed release: the gate off everywhere
	off := modernGateReadinessTestPayload(t, now, modernGateReadinessTestProcesses(4, now, false))

	alerts, err := modernGateReadinessTestSignal(failure).Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{now.Unix(): off}))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts %+v, want the failure alone", alerts)
	}
	alert := requireAlertClass(t, alerts, "modern-gate-schedule-unreadable")
	if alert.Severity != SeverityWarn || !isVisibilityAlert(alert) || !strings.Contains(alert.Markdown(), "failure="+failure) || !strings.Contains(alert.Markdown(), "quality_speed_requires_modern_provider: false") {
		t.Fatalf("alert %+v, want a visibility warning naming the failure and the key to correct", alert)
	}
	if strings.Contains(alert.Markdown(), "quality_speed_modern_provider_start:") {
		t.Fatalf("alert %+v still tells the owner to set the retired start", alert)
	}

	// no answer for the evaluation time: Mimir cannot be read
	alerts, err = modernGateReadinessTestSignal(failure).Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{}))
	if err != nil {
		t.Fatal(err)
	}
	classes := []string{}
	for _, alert := range alerts {
		classes = append(classes, alert.Class)
	}
	if strings.Join(classes, ",") != "modern-gate-schedule-unreadable,cannot-observe" {
		t.Fatalf("classes %v, want the failure beside cannot-observe", classes)
	}

	alerts, err = modernGateReadinessTestSignal("").Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{now.Unix(): off}))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("alerts %+v err %v without a failure, want none", alerts, err)
	}
}

// The production signal decodes the desired provider.yml with the model's
// own decoder: a switch that cannot be read WARNs, and the retired start,
// which once WARNed as a postponement that failed, is no failure whatever it
// holds.
func TestModernGateReadinessSignalReadsTheDesiredSwitch(t *testing.T) {
	now := modernGateReadinessTestFormerStart.Add(-time.Hour)
	off := modernGateReadinessTestPayload(t, now, modernGateReadinessTestProcesses(4, now, false))
	for _, c := range []struct {
		provider string
		failure  string
	}{
		{provider: "quality_speed_requires_modern_provider: maybe\n", failure: "quality_speed_requires_modern_provider cannot be read"},
		{provider: "quality_speed_requires_modern_provider: ~\n", failure: "quality_speed_requires_modern_provider has no value"},
		{provider: "quality_speed_modern_provider_start: next week\n"},
		{provider: "quality_speed_modern_provider_start:\n"},
		{provider: "quality_speed_requires_modern_provider: false\nquality_speed_modern_provider_start: 2026-11-11\n"},
	} {
		alerts, err := func() (Alerts, error) {
			pop := server.Config.PushSimpleResource("provider.yml", []byte(c.provider))
			defer pop()
			return NewModernGateReadinessSignal().Run(context.Background(), modernGateReadinessTestSettings(t, now, map[int64]string{now.Unix(): off}))
		}()
		if err != nil {
			t.Fatalf("%q: %v", c.provider, err)
		}
		if c.failure == "" {
			if len(alerts) != 0 {
				t.Errorf("%q: alerts %+v, want none", c.provider, alerts)
			}
			continue
		}
		if len(alerts) != 1 || alerts[0].Class != "modern-gate-schedule-unreadable" || !strings.Contains(alerts[0].Markdown(), "failure="+c.failure) {
			t.Errorf("%q: alerts %+v, want the failure %q", c.provider, alerts, c.failure)
		}
	}
}

// The query reads every part from the configured environment only, and none
// of the start, the shadow, the buckets or the counts' clock the windows
// read.
func TestModernGateReadinessQueryReadsEveryPart(t *testing.T) {
	query := modernGateReadinessQuery("synthetic")
	settled := ` and on(instance) (process_start_time_seconds{env="synthetic",job="taskworker"} < time() - 300))`
	for _, part := range []string{
		`count(urnetwork_stats_provider_modern_gate_on{env="synthetic",job="taskworker"})`,
		`count(process_start_time_seconds{env="synthetic",job="taskworker"})`,
		`count(process_start_time_seconds{env="synthetic",job="taskworker"} < time() - 300)`,
		`count(urnetwork_stats_provider_modern_gate_on{env="synthetic",job="taskworker"}` + settled,
		`min(urnetwork_stats_provider_modern_gate_on{env="synthetic",job="taskworker"}` + settled,
		`max(urnetwork_stats_provider_modern_gate_on{env="synthetic",job="taskworker"}` + settled,
	} {
		if !strings.Contains(query, part) {
			t.Errorf("query %q does not read %q", query, part)
		}
	}
	for _, retired := range []string{"urnetwork_stats_provider_modern_gate_start_seconds", "urnetwork_stats_provider_modern_gate_shadow", "urnetwork_stats_provider_egress_index", "urnetwork_stats_provider_egress_source_started_seconds"} {
		if strings.Contains(query, retired) {
			t.Errorf("query %q still reads %s", query, retired)
		}
	}
	if strings.Count(query, "env=") != 9 {
		t.Errorf("query %q reads a part outside the environment", query)
	}
}
