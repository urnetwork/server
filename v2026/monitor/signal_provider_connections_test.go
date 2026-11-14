package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Public-adapter fixtures retain raw process/release identity and source clocks.
// The separate engine suite derives historical scalars from actual raw history.
const providerConnectionsPrivate = "private-provider-fixture"

var providerConnectionsTestNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
var providerConnectionsTestStart = float64(providerConnectionsTestNow.Add(-time.Hour).Unix())
var providerConnectionsTestHosts = []string{providerConnectionsPrivate + "-a", providerConnectionsPrivate + "-b"}

const providerConnectionsTestBlock = "private-provider-fixture-blue"

type providerConnectionsTestProcess struct {
	host, instance    string
	start, transports float64
}

func providerConnectionsTestProcesses(each float64) []providerConnectionsTestProcess {
	return []providerConnectionsTestProcess{{host: providerConnectionsTestHosts[0], instance: "aabbcc11", start: providerConnectionsTestStart, transports: each}, {host: providerConnectionsTestHosts[1], instance: "aabbcc22", start: providerConnectionsTestStart, transports: each}}
}

// Service authority must include the fixture's expected host/block inventory.
func providerConnectionsTestSettings(source SignalSource, now func() time.Time) SignalSettings {
	settings := syntheticSettings(source)
	settings.Now = now
	settings.LogServices = []string{"connect"}
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	for _, name := range providerConnectionsTestHosts {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: name, Roles: []string{"services"}})
	}
	settings.LogServiceHosts = map[string][]string{"connect": append([]string(nil), providerConnectionsTestHosts...)}
	settings.LogServiceBlocks = map[string][]string{"connect": {providerConnectionsTestBlock}}
	return settings
}

func providerConnectionsTestRows(now time.Time, processes []providerConnectionsTestProcess, best, level float64) []map[string]any {
	rows := []map[string]any{}
	add := func(labels map[string]string, value float64) {
		rows = append(rows, map[string]any{"metric": labels, "value": []any{float64(now.UnixMilli()) / 1000, fmt.Sprintf("%g", value)}})
	}
	for key, value := range map[string]float64{"best_5m": best, "points_5m": 5, "expected_points_5m": 5, "p90_24h": level, "points_24h": 288} {
		add(map[string]string{"monitor_metric": key}, value)
	}
	for _, process := range processes {
		for field, value := range map[string]float64{"start": process.start, "rss": 1000000, "transports": process.transports} {
			if field == "transports" && value == 0 {
				continue
			} // actual producer deletes the last label
			for _, isTime := range []bool{false, true} {
				labels := map[string]string{"env": "synthetic", "job": "connect", "service": "connect", "host": process.host, "block": providerConnectionsTestBlock, "instance": process.instance, "monitor_metric": field}
				if field == "transports" {
					labels["app_version"] = "2026.10.1"
				}
				observed := value
				if isTime {
					labels["monitor_metric"] += "_time"
					observed = float64(now.Add(-9*time.Second).UnixMilli()) / 1000
				}
				add(labels, observed)
			}
		}
	}
	return rows
}

func providerConnectionsTestJson(t testing.TB, rows []map[string]any, annotations map[string]any) string {
	t.Helper()
	response := map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": rows}}
	for key, value := range annotations {
		response[key] = value
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func providerConnectionsTestNative(t testing.TB, now time.Time, host string, processes []providerConnectionsTestProcess) string {
	t.Helper()
	rows := []map[string]any{}
	for _, process := range processes {
		if process.host == host {
			rows = append(rows, map[string]any{"block": providerConnectionsTestBlock, "start": process.start})
		}
	}
	encoded, err := json.Marshal(map[string]any{"schema": 1, "host": host, "environment": "synthetic", "started": float64(now.UnixMilli()) / 1000, "completed": float64(now.UnixMilli()) / 1000, "complete": true, "rows": rows})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// Only registered public signals are driven; no production reducer is replaced.
func providerConnectionsSignalForTest(t *testing.T) Signal {
	t.Helper()
	for _, signal := range NewSignals() {
		if signal.Key() == "provider-connections" {
			return signal
		}
	}
	t.Fatal("provider-connections is not registered")
	return nil
}

func TestProviderConnectionsSignalSteadyFleetIsQuiet(t *testing.T) {
	now := providerConnectionsTestNow
	var calls atomic.Int32
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		calls.Add(1)
		if strings.Contains(command, "monitor-provider-connections-native") {
			t.Error("healthy fleet contacted native census")
			return "", errors.New("unexpected")
		}
		return providerConnectionsTestJson(t, providerConnectionsTestRows(now, providerConnectionsTestProcesses(37550), 75400, 77292), map[string]any{"warnings": []string{}, "infos": []string{}}), nil
	}}
	alerts, err := providerConnectionsSignalForTest(t).Run(context.Background(), providerConnectionsTestSettings(source, func() time.Time { return now }))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("healthy native publishers were not quiet: err=%v alerts=%d", err, len(alerts))
	}
	if calls.Load() != 1 {
		t.Fatal("healthy fixture did not reach exactly one public metrics read")
	}
}

// Missing service authority fails before either placement map can start a read.
func TestProviderConnectionsUndeclaredServiceNeverContactsSource(t *testing.T) {
	for _, placement := range []string{"blocks", "hosts"} {
		var calls atomic.Int32
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls.Add(1)
			return "", nil
		}}
		settings := providerConnectionsTestSettings(source, func() time.Time { return providerConnectionsTestNow })
		settings.LogServices = nil
		if placement == "blocks" {
			settings.LogServiceHosts = nil
		} else {
			settings.LogServiceBlocks = nil
		}
		alerts, err := NewProviderConnectionsSignal().Run(context.Background(), settings)
		want := "monitor: log " + placement + " configured for unknown service \"connect\""
		if err == nil || err.Error() != want || len(alerts) != 0 || calls.Load() != 0 {
			t.Fatalf("%s placement bypassed service authority or reached a source", placement)
		}
	}
}

// A low partial sum is not a product failure: the missing publisher may still
// hold transports, and the independent native roster exposes that uncertainty.
func TestProviderConnectionsMissingLiveSiblingNeverPages(t *testing.T) {
	now := providerConnectionsTestNow
	all := providerConnectionsTestProcesses(1000)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if strings.Contains(command, "monitor-provider-connections-native") {
			return providerConnectionsTestNative(t, now, host.Name, all), nil
		}
		return providerConnectionsTestJson(t, providerConnectionsTestRows(now, all[:1], 1000, 76000), nil), nil
	}}
	signal := providerConnectionsSignalForTest(t)
	settings := providerConnectionsTestSettings(source, func() time.Time { return now })
	for i := 0; i < 7; i++ {
		now = providerConnectionsTestNow.Add(time.Duration(i) * time.Minute)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, "provider-connections-unobservable")
		if requireAlertClassCount(alerts, "provider-connections-collapse") != 0 {
			t.Fatal("partial metrics manufactured a fleet collapse")
		}
	}
}

func TestProviderConnectionsSignalPagesAfterCompleteProspectiveWindow(t *testing.T) {
	for _, empty := range []bool{false, true} {
		now := providerConnectionsTestNow
		processes := providerConnectionsTestProcesses(882)
		if empty {
			processes = nil
		}
		var nativeCalls atomic.Int32
		source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
			if strings.Contains(command, "monitor-provider-connections-native") {
				nativeCalls.Add(1)
				return providerConnectionsTestNative(t, now, host.Name, processes), nil
			}
			return providerConnectionsTestJson(t, providerConnectionsTestRows(now, processes, 2131, 76473), nil), nil
		}}
		signal := providerConnectionsSignalForTest(t)
		settings := providerConnectionsTestSettings(source, func() time.Time { return now })
		for i := 0; i < 6; i++ {
			now = providerConnectionsTestNow.Add(time.Duration(i) * time.Minute)
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if i < 5 {
				if requireAlertClassCount(alerts, "provider-connections-collapse") != 0 {
					t.Fatal("native warmup fabricated a prior minute")
				}
				continue
			}
			alert := requireAlertClass(t, alerts, "provider-connections-collapse")
			if alert.SignalNumber != "1.1a" || alert.Target != "synthetic/connect-fleet" || alert.Severity != SeverityPage || alert.Sustain != 1 {
				t.Fatal("collapse contract changed")
			}
			want := "observed_provider_transports=1764"
			if empty {
				want = "observed_provider_transports=0"
			}
			if !strings.Contains(alert.Observed, want) || strings.Contains(alert.Markdown(), "connected_providers=") {
				t.Fatal("transport units or independently proved native zero were lost")
			}
			requireAlertOmits(t, alert, providerConnectionsPrivate, "aabbcc11", "aabbcc22", "2026.10.1")
		}
		if nativeCalls.Load() != 12 {
			t.Fatalf("native reads=%d, want one per configured host per anomaly observation", nativeCalls.Load())
		}
	}
}

// The true producer deletes all labels in a zero-provider process. A running
// process with an absent family is unknown even if every other process is low.
func TestProviderConnectionsLazyEmptyLiveProcessRemainsUnknown(t *testing.T) {
	now := providerConnectionsTestNow
	processes := providerConnectionsTestProcesses(0)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if strings.Contains(command, "monitor-provider-connections-native") {
			return providerConnectionsTestNative(t, now, host.Name, processes), nil
		}
		return providerConnectionsTestJson(t, providerConnectionsTestRows(now, processes, 1000, 76000), nil), nil
	}}
	signal := providerConnectionsSignalForTest(t)
	settings := providerConnectionsTestSettings(source, func() time.Time { return now })
	for i := 0; i < 6; i++ {
		now = providerConnectionsTestNow.Add(time.Duration(i) * time.Minute)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, "provider-connections-unobservable")
		if requireAlertClassCount(alerts, "provider-connections-collapse") != 0 {
			t.Fatal("lazy gauge absence was treated as zero")
		}
	}
}

func TestProviderConnectionsMalformedFramesArePrivateUnknown(t *testing.T) {
	for _, mode := range []string{"fractional_points", "fractional_bucket", "numeric_value", "future_source", "future_evaluation", "overflow_time", "negative", "duplicate", "unknown_tag", "partial_warning", "partial_envelope", "histogram_value", "malformed_warning", "foreign_job", "private_extra_label", "short_baseline", "missing_start", "null", "duplicate_json"} {
		now := providerConnectionsTestNow
		rows := providerConnectionsTestRows(now, providerConnectionsTestProcesses(1000), 2000, 76000)
		annotations := map[string]any{}
		for _, row := range rows {
			labels, value := row["metric"].(map[string]string), row["value"].([]any)
			field := labels["monitor_metric"]
			switch mode {
			case "fractional_points":
				if field == "points_24h" {
					value[1] = "12.5"
				}
			case "fractional_bucket":
				if field == "transports" {
					value[1] = "1000.5"
				}
			case "numeric_value":
				if field == "transports" {
					value[1] = 1000
				}
			case "future_source":
				if field == "start_time" {
					value[1] = fmt.Sprintf("%d", now.Add(time.Minute).Unix())
				}
			case "future_evaluation":
				value[0] = float64(now.Add(time.Minute).Unix())
			case "overflow_time":
				if field == "start_time" {
					value[1] = "1e300"
				}
			case "negative":
				if field == "transports" {
					value[1] = "-1"
				}
			case "foreign_job":
				if field == "transports" {
					labels["job"] = providerConnectionsPrivate
				}
			case "private_extra_label":
				if field == "transports" {
					labels["private_raw"] = providerConnectionsPrivate
				}
			case "short_baseline":
				if field == "points_24h" {
					value[1] = "11"
				}
			case "missing_start":
				if field == "start" {
					value[1] = "0"
				}
			case "null":
				if field == "transports" {
					value[1] = nil
				}
			}
		}
		switch mode {
		case "duplicate":
			rows = append(rows, rows[0])
		case "unknown_tag":
			rows = append(rows, map[string]any{"metric": map[string]string{"monitor_metric": providerConnectionsPrivate}, "value": []any{now.Unix(), "1"}})
		case "partial_warning":
			annotations["warnings"] = []string{"partial result: " + providerConnectionsPrivate}
		case "partial_envelope":
			annotations["partial"] = true
		case "histogram_value":
			rows[0]["histogram"] = map[string]any{"private": providerConnectionsPrivate}
		case "malformed_warning":
			annotations["warnings"] = map[string]any{"private": providerConnectionsPrivate}
		}
		payload := providerConnectionsTestJson(t, rows, annotations)
		if mode == "duplicate_json" {
			payload = strings.Replace(payload, `"status":"success"`, `"status":"success","status":"success"`, 1)
		}
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "monitor-provider-connections-native") {
				return "", errors.New(providerConnectionsPrivate)
			}
			return payload, nil
		}}
		alerts, err := providerConnectionsSignalForTest(t).Run(context.Background(), providerConnectionsTestSettings(source, func() time.Time { return now }))
		if err != nil {
			t.Fatal("raw source became an execution error")
		}
		alert := requireAlertClass(t, alerts, "provider-connections-unobservable")
		if requireAlertClassCount(alerts, "provider-connections-collapse") != 0 {
			t.Fatal("malformed evidence paged")
		}
		requireAlertOmits(t, alert, providerConnectionsPrivate, "partial result")
		encoded, _ := json.Marshal(alerts)
		if strings.Contains(string(encoded), providerConnectionsPrivate) {
			t.Fatal("private source escaped JSON")
		}
	}
}

func TestProviderConnectionsThresholdAndFractionalPercentile(t *testing.T) {
	for _, c := range []struct {
		each, level float64
		page        bool
	}{{each: 25000, level: 100000, page: false}, {each: 24999, level: 100000, page: true}, {each: 499, level: 999, page: false}, {each: 249, level: 1000, page: true}, {each: 250, level: 1000.5, page: true}} {
		now := providerConnectionsTestNow
		processes := providerConnectionsTestProcesses(c.each)
		source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
			if strings.Contains(command, "monitor-provider-connections-native") {
				return providerConnectionsTestNative(t, now, host.Name, processes), nil
			}
			return providerConnectionsTestJson(t, providerConnectionsTestRows(now, processes, 2*c.each, c.level), nil), nil
		}}
		signal := providerConnectionsSignalForTest(t)
		settings := providerConnectionsTestSettings(source, func() time.Time { return now })
		for i := 0; i < 6; i++ {
			now = providerConnectionsTestNow.Add(time.Duration(i) * time.Minute)
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if i == 5 && (requireAlertClassCount(alerts, "provider-connections-collapse") == 1) != c.page {
				t.Fatalf("wrong threshold at best=%g level=%g", 2*c.each, c.level)
			}
		}
	}
}

// The real run loop retains a page through unknowns and one complete recovery
// observation. Two uninterrupted complete recoveries clear it; an unknown
// resets a retained incident's credit and cannot revive an already cleared one.
func TestProviderConnectionsNotifierDoesNotRecoverOnUnknown(t *testing.T) {
	for _, c := range []struct {
		completeRuns int
		wantClears   int
	}{{completeRuns: 1, wantClears: 0}, {completeRuns: 2, wantClears: 1}} {
		var run atomic.Int32
		lastRun := 8 + c.completeRuns
		now := func() time.Time { return providerConnectionsTestNow.Add(time.Duration(run.Load()) * time.Minute) }
		source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
			i := int(run.Load())
			if i == 6 || i == 7 || i == lastRun {
				return "", errors.New(providerConnectionsPrivate)
			}
			processes := providerConnectionsTestProcesses(1000)
			if i >= 8 {
				processes = providerConnectionsTestProcesses(37550)
			}
			if strings.Contains(command, "monitor-provider-connections-native") {
				return providerConnectionsTestNative(t, now(), host.Name, processes), nil
			}
			best := 2000.0
			if i >= 8 {
				best = 75100
			}
			return providerConnectionsTestJson(t, providerConnectionsTestRows(now(), processes, best, 76000), nil), nil
		}}
		webhook, webhookUrl := newFakeSlackWebhook(t)
		harness := newSlackTestHarness()
		notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
		t.Cleanup(func() { closeSlackNotifier(t, notifier) })
		signal := providerConnectionsSignalForTest(t)
		pageKey := slackIncidentKey(Alert{SignalID: signal.ID(), Class: "provider-connections-collapse", Target: "synthetic/connect-fleet"})
		checkIncident := func(nextRun int, wantPresent bool, wantAbsentRuns int) {
			t.Helper()
			notifier.stateLock.Lock()
			defer notifier.stateLock.Unlock()
			incident, present := notifier.keyIncidents[pageKey]
			if present != wantPresent {
				t.Fatalf("recovery_runs=%d before_run=%d page_present=%t, want %t", c.completeRuns, nextRun, present, wantPresent)
			}
			if present {
				absentRuns, owned := incident.signalKeyAbsentRuns[signal.Key()]
				if !owned || absentRuns != wantAbsentRuns {
					t.Fatalf("recovery_runs=%d before_run=%d page_absent_runs=%d owned=%t, want %d", c.completeRuns, nextRun, absentRuns, owned, wantAbsentRuns)
				}
			}
		}
		postSignalRunsThroughTheGate(t, signal, providerConnectionsTestSettings(source, now), lastRun+1, func(i int) {
			// The helper calls this only after the prior Handle has completed.
			// Inspect state at that barrier, independently of http sender timing.
			switch i {
			case 5:
				checkIncident(i, false, 0)
			case 6, 7, 8:
				checkIncident(i, true, 0)
			case 9:
				checkIncident(i, true, 1)
			case 10:
				checkIncident(i, false, 0)
			}
			run.Store(int32(i))
			if i > 0 {
				harness.advance(time.Minute)
			}
		}, notifier)
		if c.wantClears == 0 {
			checkIncident(lastRun+1, true, 0)
		} else {
			checkIncident(lastRun+1, false, 0)
		}
		closeSlackNotifier(t, notifier)
		posts := webhook.posted()
		identity := "provider-connections-collapse · synthetic/connect-fleet"
		raised, cleared := countSlackPosts(posts, "[PAGE] raised", identity), countSlackPosts(posts, "[CLEARED]", identity)
		if raised != 1 || cleared != c.wantClears {
			t.Fatalf("recovery_runs=%d raised=%d cleared=%d, want raised=1 cleared=%d", c.completeRuns, raised, cleared, c.wantClears)
		}
		for _, text := range posts {
			if strings.Contains(text, providerConnectionsPrivate) {
				t.Fatal("notifier exposed a private source label")
			}
		}
		t.Logf("recovery_runs=%d raised=%d cleared=%d", c.completeRuns, raised, cleared)
	}
}

// A complete measured failure remains time-qualified beside one later unknown;
// expiration is visibility-only and never a healthy finding.
func TestProviderConnectionsRetainsIndependentFailureWithItsOwnClock(t *testing.T) {
	now := providerConnectionsTestNow
	unavailable := false
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if unavailable {
			return "", errors.New(providerConnectionsPrivate)
		}
		if strings.Contains(command, "monitor-provider-connections-native") {
			return providerConnectionsTestNative(t, now, host.Name, providerConnectionsTestProcesses(1000)), nil
		}
		return providerConnectionsTestJson(t, providerConnectionsTestRows(now, providerConnectionsTestProcesses(1000), 2000, 76000), nil), nil
	}}
	signal := providerConnectionsSignalForTest(t)
	settings := providerConnectionsTestSettings(source, func() time.Time { return now })
	for i := 0; i < 6; i++ {
		now = providerConnectionsTestNow.Add(time.Duration(i) * time.Minute)
		if _, err := signal.Run(context.Background(), settings); err != nil {
			t.Fatal(err)
		}
	}
	provedAt := now
	unavailable = true
	now = now.Add(time.Minute)
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "provider-connections-unobservable")
	page := requireAlertClass(t, alerts, "provider-connections-collapse")
	if !strings.Contains(page.Observed, "measured_window_end="+provedAt.Format(time.RFC3339)) || !strings.Contains(page.Observed, "current_source=unknown") {
		t.Fatal("prior complete failure borrowed the later query clock")
	}
	now = now.Add(time.Minute)
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if requireAlertClassCount(alerts, "provider-connections-collapse") != 0 {
		t.Fatal("prior failure exceeded its bounded measurement age")
	}
	requireAlertClass(t, alerts, "provider-connections-unobservable")
}

// A returned identity with no timestamp companions cannot be silently discarded
// as historical while healthy siblings certify a current complete fleet.
func TestProviderConnectionsUntimeableIdentityCannotCertifyRecovery(t *testing.T) {
	now := providerConnectionsTestNow
	rows := providerConnectionsTestRows(now, providerConnectionsTestProcesses(40000), 80000, 80000)
	extra := providerConnectionsTestProcess{host: providerConnectionsTestHosts[0], instance: "missing-clock", start: providerConnectionsTestStart + 10, transports: 1000}
	for _, row := range providerConnectionsTestRows(now, []providerConnectionsTestProcess{extra}, 80000, 80000) {
		labels := row["metric"].(map[string]string)
		if labels["instance"] == extra.instance && !strings.HasSuffix(labels["monitor_metric"], "_time") {
			rows = append(rows, row)
		}
	}
	var nativeCalls atomic.Int32
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "monitor-provider-connections-native") {
			nativeCalls.Add(1)
			return "", errors.New(providerConnectionsPrivate)
		}
		return providerConnectionsTestJson(t, rows, nil), nil
	}}
	alerts, err := NewProviderConnectionsSignal().Run(context.Background(), providerConnectionsTestSettings(source, func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "provider-connections-unobservable")
	if nativeCalls.Load() != 2 || requireAlertClassCount(alerts, "provider-connections-collapse") != 0 {
		t.Fatal("untimed returned identity certified healthy or skipped native authority")
	}
}
