// Synthetic node filesystem histories exercise the shared max-of-windows
// estimator through the public signal. Nothing here reads a gateway or host.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const storageRunwayTestGigabyte = 1000 * 1000 * 1000

// One synthetic filesystem series. avail maps the age of a grid point (the
// grid end minus its time) to available bytes; omit drops grid points.
type storageRunwayTestVolume struct {
	host       string
	mountpoint string
	labels     map[string]string
	avail      func(age time.Duration) float64
	omit       func(age time.Duration) bool
	values     func(values [][]any) [][]any
}

// A complete matrix on the probe's own grid, with stable synthetic labels.
func storageRunwayTestBody(t *testing.T, grid storageRunwayGrid, volumes ...storageRunwayTestVolume) string {
	t.Helper()
	result := []any{}
	for _, volume := range volumes {
		metric := map[string]string{
			"__name__": "node_filesystem_avail_bytes", "env": "synthetic", "job": "node",
			"host": volume.host, "mountpoint": volume.mountpoint,
			"device": "/dev/private-source-canary", "fstype": "ext4",
		}
		for name, value := range volume.labels {
			if value == "" {
				delete(metric, name)
				continue
			}
			metric[name] = value
		}
		values := [][]any{}
		for at := grid.start; !at.After(grid.end); at = at.Add(grid.step) {
			age := grid.end.Sub(at)
			if volume.omit != nil && volume.omit(age) {
				continue
			}
			values = append(values, []any{at.Unix(), strconv.FormatFloat(volume.avail(age), 'f', -1, 64)})
		}
		if volume.values != nil {
			values = volume.values(values)
		}
		result = append(result, map[string]any{"metric": metric, "values": values})
	}
	body, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}})
	if err != nil {
		t.Fatal("synthetic range encoding failed")
	}
	return string(body)
}

// Flat volumes for every default rule over the synthetic inventory: the
// gateway's and each host's root, and the Redis data volume.
func storageRunwayTestFlat() []storageRunwayTestVolume {
	flat := func(bytes float64) func(time.Duration) float64 {
		return func(time.Duration) float64 { return bytes }
	}
	return []storageRunwayTestVolume{
		{host: "metrics-1", mountpoint: "/", avail: flat(500 * storageRunwayTestGigabyte)},
		{host: "pg-1", mountpoint: "/", avail: flat(600 * storageRunwayTestGigabyte)},
		{host: "redis-1", mountpoint: "/", avail: flat(80 * storageRunwayTestGigabyte)},
		{host: "redis-1", mountpoint: "/mnt/data", avail: flat(900 * storageRunwayTestGigabyte)},
	}
}

// Replace one default volume's history.
func storageRunwayTestWith(target string, volume storageRunwayTestVolume) []storageRunwayTestVolume {
	volumes := storageRunwayTestFlat()
	for index, existing := range volumes {
		if storageRunwayTarget(existing.host, existing.mountpoint) == target {
			volume.host, volume.mountpoint = existing.host, existing.mountpoint
			volumes[index] = volume
		}
	}
	return volumes
}

// The default probe grid at the synthetic clock.
func storageRunwayTestGrid() storageRunwayGrid {
	return DefaultStorageRunwayEstimatorSettings().grid(syntheticSettings(nil).Now())
}

// Check the exact gateway, query and grid on every public run.
func storageRunwayTestSettings(t *testing.T, body string, sourceErr error) SignalSettings {
	t.Helper()
	grid := storageRunwayTestGrid()
	settings := syntheticSettings(&syntheticSource{hostFn: func(target HostSettings, command string) (string, error) {
		if target.Name != "metrics-1" {
			t.Fatal("storage runway contacted a host instead of the service gateway")
		}
		for _, want := range []string{
			"/prometheus/api/v1/query_range", "--max-filesize",
			`node_filesystem_avail_bytes{env="synthetic",job="node",host=~`,
			"start=" + strconv.FormatInt(grid.start.Unix(), 10), "end=" + strconv.FormatInt(grid.end.Unix(), 10), "step=900",
		} {
			if !strings.Contains(command, want) {
				t.Fatalf("storage runway command lost %q", want)
			}
		}
		return body, sourceErr
	}})
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	return settings
}

func storageRunwayTestRun(t *testing.T, signal Signal, body string) Alerts {
	t.Helper()
	alerts, err := signal.Run(context.Background(), storageRunwayTestSettings(t, body, nil))
	if err != nil {
		t.Fatal("storage runway run failed")
	}
	return alerts
}

// A linear decline at rate bytes per hour from a level at the grid end.
func storageRunwayTestLinear(atEnd, perHour float64) func(time.Duration) float64 {
	return func(age time.Duration) float64 { return atEnd + perHour*age.Hours() }
}

func storageRunwayTestAlertFor(t *testing.T, alerts Alerts, class, target string) Alert {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == class && alert.Target == target {
			return requireAlertClass(t, Alerts{alert}, class)
		}
	}
	t.Fatalf("no %s alert for %s in %d alerts", class, target, len(alerts))
	return Alert{}
}

func TestStorageRunwayRegisteredForEveryStatefulVolume(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "storage-runway" {
			continue
		}
		found++
		if signal.Number() != "2.4f" || signal.Cadence() != 15*time.Minute || signal.ID() != "storage/runway" {
			t.Fatal("storage runway lost its catalog identity or fifteen-minute cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4f storage runway estimator is not registered")
	}
}

func TestStorageRunwayHealthyFlatVolumes(t *testing.T) {
	alerts := storageRunwayTestRun(t, NewStorageRunwaySignal(), storageRunwayTestBody(t, storageRunwayTestGrid(), storageRunwayTestFlat()...))
	if len(alerts) != 0 {
		t.Fatalf("flat complete volumes alerted: %d alerts", len(alerts))
	}
}

// The 2026-10-20/21 edge-2 backtest from SIGNALS.md §2.4f: free above the
// 100 GB floor and the fastest decline select WARN, WARN and PAGE.
func TestStorageRunwayDatabaseBacktest(t *testing.T) {
	floor := 100.0 * storageRunwayTestGigabyte
	for _, test := range []struct {
		name     string
		avail    func(time.Duration) float64
		severity Severity
		band     string
	}{
		{
			name: "10-20 00:00 7.7 GB/h over 6h", severity: SeverityWarn, band: "warn",
			avail: func(age time.Duration) float64 {
				hours := min(age.Hours(), 6)
				return floor + 511*storageRunwayTestGigabyte + 7.7*storageRunwayTestGigabyte*hours
			},
		},
		{
			name: "10-21 12:00 9 GB/h over 1h", severity: SeverityWarn, band: "warn",
			avail: func(age time.Duration) float64 {
				if age <= time.Hour {
					return floor + 344*storageRunwayTestGigabyte + 9*storageRunwayTestGigabyte*age.Hours()
				}
				return floor + 353*storageRunwayTestGigabyte + 4*storageRunwayTestGigabyte*(age.Hours()-1)
			},
		},
		{
			name: "10-21 15:00 18 GB/h over 1h", severity: SeverityPage, band: "page",
			avail: func(age time.Duration) float64 {
				if age <= time.Hour {
					return floor + 306*storageRunwayTestGigabyte + 18*storageRunwayTestGigabyte*age.Hours()
				}
				return floor + 324*storageRunwayTestGigabyte + 9*storageRunwayTestGigabyte*(age.Hours()-1)
			},
		},
	} {
		body := storageRunwayTestBody(t, storageRunwayTestGrid(), storageRunwayTestWith("pg-1/root", storageRunwayTestVolume{avail: test.avail})...)
		alerts := storageRunwayTestRun(t, NewStorageRunwaySignal(), body)
		if len(alerts) != 1 {
			t.Fatalf("%s: got %d alerts, want only the database runway", test.name, len(alerts))
		}
		alert := storageRunwayTestAlertFor(t, alerts, "storage-runway", "pg-1/root")
		if alert.Severity != test.severity || alert.Sustain != 2 || alert.unobservable || alert.Frame != "" ||
			!strings.Contains(alert.Observed, "band="+test.band+" ") || !strings.Contains(alert.Observed, "floor_bytes=100000000000") {
			t.Fatalf("%s: wrong runway alert: severity=%s observed=%s", test.name, alert.Severity, alert.Observed)
		}
		if !strings.Contains(alert.Action, "§5.14") {
			t.Fatalf("%s: the database volume lost its §5.14 playbook action", test.name)
		}
		requireAlertOmits(t, alert, "private-source-canary")
	}
}

// The maximum over windows reports an acceleration at once; the minimum of
// the hourly rates (the rejected §2.4c shape) would hide it for a day.
func TestStorageRunwayAccelerationUsesFastestWindow(t *testing.T) {
	avail := func(age time.Duration) float64 {
		if age <= time.Hour {
			return 40*storageRunwayTestGigabyte + 30*storageRunwayTestGigabyte*age.Hours()
		}
		return 70*storageRunwayTestGigabyte + 0.2*storageRunwayTestGigabyte*(age.Hours()-1)
	}
	alerts := storageRunwayTestRun(t, NewStorageRunwaySignal(), storageRunwayTestBody(t, storageRunwayTestGrid(), storageRunwayTestWith("redis-1/root", storageRunwayTestVolume{avail: avail})...))
	alert := storageRunwayTestAlertFor(t, alerts, "storage-runway", "redis-1/root")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "rate_window=1h ") || !strings.Contains(alert.Observed, "decline_1h_gb_per_hour=30.00") {
		t.Fatalf("acceleration was not reported from the fastest window: %s", alert.Observed)
	}
}

// A 20 GB cleanup 20 minutes ago raises the newest sample. It removes the 1h
// window's decline and shortens the 6h one; the 24h window still carries most
// of the 5 GB/h pace, so the runway stays in its warning band.
func TestStorageRunwayCleanupShortensOnlyItsWindows(t *testing.T) {
	avail := func(age time.Duration) float64 {
		if age < 20*time.Minute {
			return 260 * storageRunwayTestGigabyte
		}
		return 240*storageRunwayTestGigabyte + 5*storageRunwayTestGigabyte*age.Hours()
	}
	alerts := storageRunwayTestRun(t, NewStorageRunwaySignal(), storageRunwayTestBody(t, storageRunwayTestGrid(), storageRunwayTestWith("redis-1/root", storageRunwayTestVolume{avail: avail})...))
	alert := storageRunwayTestAlertFor(t, alerts, "storage-runway", "redis-1/root")
	if alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, "decline_1h_gb_per_hour=0.00") ||
		!strings.Contains(alert.Observed, "decline_6h_gb_per_hour=1.67") || !strings.Contains(alert.Observed, "rate_window=24h") {
		t.Fatalf("a cleanup erased the longer windows' decline: %s", alert.Observed)
	}
}

// A volume that is flat now but lost more than half its free space in one
// day this week pages on its own pace, independently of the current rate.
func TestStorageRunwayPaceFloorPagesWithoutCurrentDecline(t *testing.T) {
	avail := func(age time.Duration) float64 {
		switch {
		case age >= 5*24*time.Hour:
			return 700 * storageRunwayTestGigabyte
		case age >= 4*24*time.Hour:
			return 400*storageRunwayTestGigabyte + 300*storageRunwayTestGigabyte*(age-4*24*time.Hour).Hours()/24
		default:
			return 400 * storageRunwayTestGigabyte
		}
	}
	alerts := storageRunwayTestRun(t, NewStorageRunwaySignal(), storageRunwayTestBody(t, storageRunwayTestGrid(), storageRunwayTestWith("redis-1/mnt/data", storageRunwayTestVolume{avail: avail})...))
	alert := storageRunwayTestAlertFor(t, alerts, "storage-runway", "redis-1/mnt/data")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "band=pace-floor ") || !strings.Contains(alert.Observed, "runway_hours=none-no-decline") ||
		!strings.Contains(alert.Observed, "largest_pace_decline_gb=300.0") {
		t.Fatalf("pace floor did not page: %s", alert.Observed)
	}
	// Twice the pace fits: no alert.
	settings := DefaultStorageRunwaySettings()
	settings.PaceFloorMultiple = 1.25
	if alerts := storageRunwayTestRun(t, NewStorageRunwaySignalWithSettings(settings), storageRunwayTestBody(t, storageRunwayTestGrid(), storageRunwayTestWith("redis-1/mnt/data", storageRunwayTestVolume{avail: avail})...)); len(alerts) != 0 {
		t.Fatal("a pace floor below the free space alerted")
	}
}

// Runway band edges on a volume that only started declining an hour ago, so
// its small daily decline keeps the independent pace floor out of the way.
func TestStorageRunwayThresholdEdges(t *testing.T) {
	for _, test := range []struct {
		hours float64
		want  Severity
	}{
		{hours: 72.1, want: ""},
		{hours: 71.9, want: SeverityWarn},
		{hours: 24.1, want: SeverityWarn},
		{hours: 23.9, want: SeverityPage},
	} {
		free := 2 * storageRunwayTestGigabyte * test.hours
		avail := func(age time.Duration) float64 { return free + 2*storageRunwayTestGigabyte*min(age.Hours(), 1) }
		alerts := storageRunwayTestRun(t, NewStorageRunwaySignal(), storageRunwayTestBody(t, storageRunwayTestGrid(), storageRunwayTestWith("redis-1/mnt/data", storageRunwayTestVolume{avail: avail})...))
		if test.want == "" {
			if len(alerts) != 0 {
				t.Fatalf("runway %.1f h alerted", test.hours)
			}
			continue
		}
		if alert := storageRunwayTestAlertFor(t, alerts, "storage-runway", "redis-1/mnt/data"); alert.Severity != test.want {
			t.Fatalf("runway %.1f h has severity %s, want %s", test.hours, alert.Severity, test.want)
		}
	}
}

// Missing, stale, ambiguous and malformed series stay unknown for their own
// volume; sibling volumes keep their measured verdicts.
func TestStorageRunwayInvalidSeriesStayUnknown(t *testing.T) {
	flat := func(time.Duration) float64 { return 80 * storageRunwayTestGigabyte }
	for _, test := range []struct {
		name   string
		volume storageRunwayTestVolume
		drop   bool
		extra  bool
		reason string
	}{
		{name: "missing", drop: true, reason: "missing-filesystem-series"},
		{name: "stale", volume: storageRunwayTestVolume{avail: flat, omit: func(age time.Duration) bool { return age < time.Hour }}, reason: "stale-filesystem-series"},
		{name: "incomplete windows", volume: storageRunwayTestVolume{avail: flat, omit: func(age time.Duration) bool { return age > 5*time.Hour && age < 7*time.Hour }}, reason: "incomplete-decline-windows"},
		{name: "duplicate source", volume: storageRunwayTestVolume{avail: flat}, extra: true, reason: "ambiguous-filesystem-source"},
		{name: "unknown label", volume: storageRunwayTestVolume{avail: flat, labels: map[string]string{"private-source-canary": "x"}}, reason: "unsupported-source-label"},
		{name: "foreign environment", volume: storageRunwayTestVolume{avail: flat, labels: map[string]string{"env": "other"}}, reason: "unowned-filesystem-series"},
		{name: "foreign job", volume: storageRunwayTestVolume{avail: flat, labels: map[string]string{"job": "backup"}}, reason: "unowned-filesystem-series"},
		{name: "missing device", volume: storageRunwayTestVolume{avail: flat, labels: map[string]string{"device": ""}}, reason: "unowned-filesystem-series"},
		{name: "fractional bytes", volume: storageRunwayTestVolume{avail: func(time.Duration) float64 { return 1.5 }}, reason: "invalid-byte-value"},
		{name: "negative", volume: storageRunwayTestVolume{avail: flat, values: func(values [][]any) [][]any { values[3][1] = "-1"; return values }}, reason: "invalid-sample-value"},
		{name: "nonfinite", volume: storageRunwayTestVolume{avail: flat, values: func(values [][]any) [][]any { values[3][1] = "NaN"; return values }}, reason: "invalid-sample-value"},
		{name: "off grid", volume: storageRunwayTestVolume{avail: flat, values: func(values [][]any) [][]any {
			values[3][0] = values[3][0].(int64) + 1
			return values
		}}, reason: "off-grid-or-unordered-sample"},
		{name: "reversed", volume: storageRunwayTestVolume{avail: flat, values: func(values [][]any) [][]any {
			values[3], values[4] = values[4], values[3]
			return values
		}}, reason: "off-grid-or-unordered-sample"},
		{name: "numeric value", volume: storageRunwayTestVolume{avail: flat, values: func(values [][]any) [][]any { values[3][1] = 1; return values }}, reason: "invalid-sample-shape"},
	} {
		volumes := storageRunwayTestWith("redis-1/root", test.volume)
		if test.drop {
			volumes = append(volumes[:2], volumes[3:]...)
		}
		if test.extra {
			duplicate := test.volume
			duplicate.host, duplicate.mountpoint = "redis-1", "/"
			duplicate.labels = map[string]string{"device": "/dev/private-source-canary-2"}
			volumes = append(volumes, duplicate)
		}
		alerts := storageRunwayTestRun(t, NewStorageRunwaySignal(), storageRunwayTestBody(t, storageRunwayTestGrid(), volumes...))
		if len(alerts) != 1 {
			t.Fatalf("%s: got %d alerts, want one unknown volume", test.name, len(alerts))
		}
		alert := storageRunwayTestAlertFor(t, alerts, "storage-runway-unobservable", "redis-1/root")
		if !alert.unobservable || alert.Severity != SeverityWarn || alert.Sustain != 2 || !strings.Contains(alert.Observed, "reason="+test.reason) {
			t.Fatalf("%s: wrong unknown volume: severity=%s observed=%s", test.name, alert.Severity, alert.Observed)
		}
		requireAlertOmits(t, alert, "private-source-canary")
	}
}

// An unknown volume that declined more than 5 GB/h over the last six hours
// pages (emission rule 1); a slower one warns.
func TestStorageRunwayUnknownKeepsMeasuredRecentSeverity(t *testing.T) {
	for _, test := range []struct {
		perHour float64
		want    Severity
	}{
		{perHour: 8 * storageRunwayTestGigabyte, want: SeverityPage},
		{perHour: 2 * storageRunwayTestGigabyte, want: SeverityWarn},
	} {
		volume := storageRunwayTestVolume{
			avail: storageRunwayTestLinear(5000*storageRunwayTestGigabyte, test.perHour),
			omit:  func(age time.Duration) bool { return age < time.Hour },
		}
		alerts := storageRunwayTestRun(t, NewStorageRunwaySignal(), storageRunwayTestBody(t, storageRunwayTestGrid(), storageRunwayTestWith("redis-1/mnt/data", volume)...))
		alert := storageRunwayTestAlertFor(t, alerts, "storage-runway-unobservable", "redis-1/mnt/data")
		if alert.Severity != test.want || !strings.Contains(alert.Observed, "recent_decline_basis=measured") {
			t.Fatalf("recent decline %.0f GB/h has unknown severity %s, want %s: %s", gb(test.perHour), alert.Severity, test.want, alert.Observed)
		}
	}
}

// A failed read after a measured fast decline pages from the instance's
// memory; once the memory is older than the risk window it warns again.
func TestStorageRunwayFailedReadRemembersRecentDecline(t *testing.T) {
	signal := NewStorageRunwaySignal()
	fast := storageRunwayTestVolume{avail: storageRunwayTestLinear(8000*storageRunwayTestGigabyte, 9*storageRunwayTestGigabyte)}
	healthy := storageRunwayTestBody(t, storageRunwayTestGrid(), storageRunwayTestWith("redis-1/mnt/data", fast)...)
	if alerts := storageRunwayTestRun(t, signal, healthy); len(alerts) != 0 {
		t.Fatal("a long-runway volume alerted")
	}
	start := syntheticSettings(nil).Now()
	for _, test := range []struct {
		later time.Duration
		want  Severity
	}{
		{later: 15 * time.Minute, want: SeverityPage},
		{later: 7 * time.Hour, want: SeverityWarn},
	} {
		settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			return "", errors.New("exit status 1: private-source-canary")
		}})
		settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
		at := start.Add(test.later)
		settings.Now = func() time.Time { return at }
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 4 {
			t.Fatalf("failed read after %s did not keep every volume unknown", test.later)
		}
		alert := storageRunwayTestAlertFor(t, alerts, "storage-runway-unobservable", "redis-1/mnt/data")
		if alert.Severity != test.want || !strings.Contains(alert.Observed, "error_class=observation-command-failed") {
			t.Fatalf("after %s the unknown runway has severity %s, want %s: %s", test.later, alert.Severity, test.want, alert.Observed)
		}
		requireAlertOmits(t, alert, "private-source-canary")
	}
}

// A failed API answer, a warning, a resampled vector or an oversized body is
// one fixed reason for every volume, never a healthy zero.
func TestStorageRunwayInvalidAnswerIsUnknownForEveryVolume(t *testing.T) {
	valid := storageRunwayTestBody(t, storageRunwayTestGrid(), storageRunwayTestFlat()...)
	for _, body := range []string{
		strings.Replace(valid, `"success"`, `"error"`, 1),
		strings.Replace(valid, `"matrix"`, `"vector"`, 1),
		strings.Replace(valid, `"status":"success"`, `"status":"success","warnings":["private-source-canary"]`, 1),
		valid + strings.Repeat(" ", DefaultStorageRunwayEstimatorSettings().MaxResponseBytes),
		"private-source-canary",
	} {
		alerts := storageRunwayTestRun(t, NewStorageRunwaySignal(), body)
		if len(alerts) != 4 {
			t.Fatalf("an invalid answer produced %d alerts, want one unknown per volume", len(alerts))
		}
		for _, alert := range alerts {
			if alert.Class != "storage-runway-unobservable" || !alert.unobservable {
				t.Fatal("an invalid answer certified a volume")
			}
			requireAlertOmits(t, alert, "private-source-canary")
		}
	}
}

// No gateway, an excluded host and a disabled host never read or certify the
// volumes they own.
func TestStorageRunwayInventoryAuthority(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		t.Fatal("storage runway queried without a gateway")
		return "", nil
	}})
	alerts, err := NewStorageRunwaySignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 3 {
		t.Fatal("missing gateway did not leave every volume unknown")
	}
	for _, alert := range alerts {
		if alert.Class != "storage-runway-unobservable" || !strings.Contains(alert.Observed, "reason=service-gateway-unavailable") {
			t.Fatal("missing gateway certified a volume")
		}
	}

	body := storageRunwayTestBody(t, storageRunwayTestGrid(), storageRunwayTestWith("redis-1/root", storageRunwayTestVolume{
		avail: storageRunwayTestLinear(storageRunwayTestGigabyte, 10*storageRunwayTestGigabyte),
	})...)
	excluded := storageRunwayTestSettings(t, body, nil)
	excluded.ExcludedHosts = []string{"redis-1"}
	alerts, err = NewStorageRunwaySignal().Run(context.Background(), excluded)
	if err != nil {
		t.Fatal("excluded host run failed")
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
	for _, alert := range alerts {
		if strings.HasPrefix(alert.Target, "redis-1/") {
			t.Fatal("an excluded host's volume was reported")
		}
	}
}

// Ended callers receive lifecycle with the original source cause; nothing is
// published after cancellation.
func TestStorageRunwayCancellationKeepsOriginalCause(t *testing.T) {
	for _, beforeContact := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cause := errors.New("private-source-canary")
		settings := storageRunwayTestSettings(t, "", nil)
		settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			if beforeContact {
				t.Fatal("canceled work contacted a gateway")
			}
			cancel()
			return storageRunwayTestBody(t, storageRunwayTestGrid(), storageRunwayTestFlat()...), cause
		}}
		if beforeContact {
			cancel()
		}
		alerts, err := NewStorageRunwaySignal().Run(ctx, settings)
		cancel()
		if len(alerts) != 0 || !errors.Is(err, context.Canceled) || (!beforeContact && !errors.Is(err, cause)) {
			t.Fatal("canceled storage runway lost its causes or published findings")
		}
		if strings.Contains(err.Error(), "private-source-canary") {
			t.Fatal("lifecycle error exposed its private source")
		}
	}
}

// The estimator itself: windows end at the newest sample, the rate is the
// fastest measured window, and a volume at its floor while declining has no
// runway left.
func TestStorageRunwayEstimatorArithmetic(t *testing.T) {
	settings := DefaultStorageRunwayEstimatorSettings()
	grid := settings.grid(syntheticSettings(nil).Now())
	samples := []storageRunwaySample{}
	for at := grid.start; !at.After(grid.end); at = at.Add(grid.step) {
		age := grid.end.Sub(at).Hours()
		samples = append(samples, storageRunwaySample{at: at, value: 100*storageRunwayTestGigabyte + storageRunwayTestGigabyte*age})
	}
	estimate := estimateStorageRunway(settings, samples, grid.end, 0)
	runway, declining := estimate.runway()
	if !estimate.complete || !declining || (runway-100*time.Hour).Abs() > time.Second || estimate.rateWindow != time.Hour {
		t.Fatalf("linear estimate is wrong: complete=%t runway=%s window=%s", estimate.complete, runway, estimate.rateWindow)
	}
	estimate = estimateStorageRunway(settings, samples, grid.end, 150*storageRunwayTestGigabyte)
	if runway, declining := estimate.runway(); !declining || runway != 0 {
		t.Fatal("a declining volume below its floor kept runway")
	}
	flat := []storageRunwaySample{{at: grid.end.Add(-time.Hour), value: 1}, {at: grid.end, value: 1}}
	if _, declining := estimateStorageRunway(settings, flat, grid.end, 0).runway(); declining {
		t.Fatal("a flat volume produced a runway")
	}
}
