// Direct PostgreSQL CPU observations must be registered independently of
// aggregate host load and database statement wall time.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The host-load signal cannot detect database CPU without runnable saturation.
func TestPgCpuSignalRegistered(t *testing.T) {
	for _, signal := range NewSignals() {
		if signal.Key() == "pg-cpu" {
			return
		}
	}
	t.Fatal("missing independent PostgreSQL CPU signal")
}

// Synthetic cgroup incarnations exercise multi-unit aggregation without
// retaining any production host, PID, or service identity.
func pgCpuTestOutput(now time.Time, deltaNs uint64) string {
	var output strings.Builder
	for index := range 2 {
		fmt.Fprintf(&output, "--pg-cpu-snapshot--\nhost=database-a\ntimestamp=%d\nboot=00000000-0000-0000-0000-000000000001\nuptime=%d.00 0.00\ncores=8\n\n",
			now.Add(time.Duration(index-1)*5*time.Second).Unix(), 100+index*5)
		for unit := range 2 {
			fmt.Fprintf(&output, "Id=postgresql@18-synthetic%d.service\nActiveState=active\nSubState=running\nCPUAccounting=yes\nCPUUsageNSec=%d\nInvocationID=%032d\n\n",
				unit, uint64(100000000000)+uint64(index)*deltaNs/2, unit+1)
		}
		fmt.Fprintf(&output, "uptime_end=%d.00 0.00\n--pg-cpu-end--\n", 100+index*5)
	}
	return output.String()
}

// The signal needs only the enabled database OS source; SQL and host-load
// evidence cannot silently become prerequisites for the CPU detector.
func pgCpuTestSettings(t *testing.T, output string, sourceErr error) SignalSettings {
	t.Helper()
	source := &syntheticSource{
		hostTimeoutFn: func(host HostSettings, command string, timeout time.Duration) (string, error) {
			if host.Name != "database-a.example" || command != pgCpuCommand || timeout != pgCpuTimeout {
				t.Fatal("CPU observation escaped its bounded inventory-owned source")
			}
			return output, sourceErr
		},
		postgresFn: func(string) ([]Row, error) {
			t.Fatal("CPU probe attempted to infer CPU from PostgreSQL")
			return nil, nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "database-a.example", Roles: []string{"pg-primary"}}}
	return settings
}

// High database CPU is detected without load/core >= 1.25, and aggregated
// cgroup nanoseconds preserve subsecond backend work lost by coarse ps TIME.
func TestPgCpuHighAndHealthyControls(t *testing.T) {
	for _, test := range []struct {
		name     string
		deltaNs  uint64
		severity Severity
		want     string
	}{
		{name: "idle", deltaNs: 0},
		{name: "headroom", deltaNs: 2800000000},
		{name: "below warning", deltaNs: 9999999998},
		{name: "warning boundary", deltaNs: 10000000000, severity: SeverityWarn, want: "postgres_cpu_fraction=0.2500"},
		{name: "below page", deltaNs: 33999999998, severity: SeverityWarn, want: "postgres_cpu_fraction=0.8500"},
		{name: "page", deltaNs: 34000000000, severity: SeverityPage, want: "postgres_cpu_cores=6.800"},
	} {
		now := syntheticSettings(nil).Now()
		settings := pgCpuTestSettings(t, pgCpuTestOutput(now, test.deltaNs), nil)
		alerts, err := NewPgCpuSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if test.want == "" {
			if len(alerts) != 0 {
				t.Fatalf("%s: unexpected alerts: %+v", test.name, alerts)
			}
			continue
		}
		alert := requireAlertClass(t, alerts, "pg-cpu-high")
		if len(alerts) != 1 || alert.Severity != test.severity || alert.Sustain != 2 || alert.Target != "database-a.example" {
			t.Fatalf("%s: wrong CPU alert: %+v", test.name, alert)
		}
		for _, want := range []string{test.want, "active_service_count=2", "False-positive qualifier", "False-negative qualifiers", "is not CPU time", "§1.3d"} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Fatalf("%s: CPU alert omits %q", test.name, want)
			}
		}
		requireAlertOmits(t, alert, "00000000-0000-0000-0000-000000000001", "00000000000000000000000000000001", "postgresql@18-synthetic")
	}
}

// Unsupported, stale, partial, restarted, and reset sources cannot produce a
// CPU spike or a healthy result. Each mutation targets one source invariant.
func TestPgCpuUnavailableControls(t *testing.T) {
	now := syntheticSettings(nil).Now()
	valid := pgCpuTestOutput(now, 34000000000)
	for _, test := range []struct {
		name   string
		output string
		err    error
	}{
		{name: "transport", err: errors.New("synthetic private diagnostic")},
		{name: "missing", output: ""},
		{name: "hostname", output: strings.ReplaceAll(valid, "host=database-a", "host=other-host")},
		{name: "stale", output: pgCpuTestOutput(now.Add(-time.Minute), 34000000000)},
		{name: "future", output: pgCpuTestOutput(now.Add(time.Minute), 34000000000)},
		{name: "boot changed", output: strings.Replace(valid, "boot=00000000-0000-0000-0000-000000000001", "boot=00000000-0000-0000-0000-000000000002", 1)},
		{name: "service restarted", output: strings.Replace(valid, "InvocationID=00000000000000000000000000000001", "InvocationID=00000000000000000000000000000003", 1)},
		{name: "counter reset", output: strings.Replace(valid, "CPUUsageNSec=117000000000", "CPUUsageNSec=1", 1)},
		{name: "counter unsupported", output: strings.Replace(valid, "CPUUsageNSec=100000000000", "CPUUsageNSec=18446744073709551615", 1)},
		{name: "counter absent", output: strings.Replace(valid, "CPUUsageNSec=100000000000", "CPUUsageNSec=", 1)},
		{name: "accounting disabled", output: strings.Replace(valid, "CPUAccounting=yes", "CPUAccounting=no", 1)},
		{name: "inactive only", output: strings.ReplaceAll(valid, "ActiveState=active", "ActiveState=inactive")},
		{name: "unit cohort changed", output: strings.Replace(valid, "Id=postgresql@18-synthetic0.service", "Id=postgresql@18-other.service", 1)},
		{name: "duplicate unit", output: strings.ReplaceAll(valid, "Id=postgresql@18-synthetic1.service", "Id=postgresql@18-synthetic0.service")},
		{name: "capacity changed", output: strings.Replace(valid, "cores=8", "cores=4", 1)},
		{name: "zero capacity", output: strings.ReplaceAll(valid, "cores=8", "cores=0")},
		{name: "impossible rate", output: pgCpuTestOutput(now, 50000000000)},
		{name: "short interval", output: strings.ReplaceAll(valid, "105.00", "102.00")},
		{name: "invalid uptime", output: strings.Replace(valid, "uptime=100.00", "uptime=NaN", 1)},
		{name: "slow source", output: strings.Replace(valid, "uptime_end=100.00", "uptime_end=101.00", 1)},
		{name: "truncated", output: strings.TrimSuffix(valid, "--pg-cpu-end--\n")},
	} {
		alerts, err := NewPgCpuSignal().Run(context.Background(), pgCpuTestSettings(t, test.output, test.err))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		alert := requireAlertClass(t, alerts, "pg-cpu-unavailable")
		if len(alerts) != 1 || alert.Severity != SeverityWarn || !strings.Contains(alert.Markdown(), "unknown, not zero") {
			t.Fatalf("%s: invalid visibility result: %+v", test.name, alerts)
		}
		requireAlertOmits(t, alert, "synthetic private diagnostic", "other-host", "postgresql@18-synthetic")
	}
}

// The host scope removes disabled inventory targets before source invocation.
func TestPgCpuDoesNotContactDisabledHost(t *testing.T) {
	settings := pgCpuTestSettings(t, pgCpuTestOutput(syntheticSettings(nil).Now(), 0), nil)
	settings.disabledHosts = []HostSettings{{Name: "disabled-database.example", Roles: []string{"pg-primary"}}}
	alerts, err := NewPgCpuSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("disabled-host control: alerts=%+v err=%v", alerts, err)
	}
}
