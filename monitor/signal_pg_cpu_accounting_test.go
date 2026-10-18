// New systemd releases omit the deprecated accounting switch while still
// publishing the service's independent CPU counter.
package monitor

import (
	"context"
	"strings"
	"testing"
)

// A missing optional switch cannot hide measured CPU or prevent a healthy
// observation; legacy and modern shapes may coexist in the same census.
func TestPgCpuAccountingPropertyOptional(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, c := range []struct {
		name        string
		deltaNs     uint64
		removeCount int
		wantClass   string
	}{
		{name: "modern high", deltaNs: 34000000000, removeCount: -1, wantClass: "pg-cpu-high"},
		{name: "modern idle", removeCount: -1},
		{name: "mixed high", deltaNs: 34000000000, removeCount: 1, wantClass: "pg-cpu-high"},
	} {
		output := strings.Replace(pgCpuTestOutput(now, c.deltaNs), "CPUAccounting=yes\n", "", c.removeCount)
		alerts, err := NewPgCpuSignal().Run(context.Background(), pgCpuTestSettings(t, output, nil))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.wantClass == "" {
			if len(alerts) != 0 {
				t.Fatalf("%s: supported accounting reported unavailable: %+v", c.name, alerts)
			}
			continue
		}
		if len(alerts) != 1 || alerts[0].Class != c.wantClass || alerts[0].Severity != SeverityPage ||
			!strings.Contains(alerts[0].Observed, "postgres_cpu_cores=6.800") ||
			!strings.Contains(alerts[0].Observed, "active_service_count=2") {
			t.Fatalf("%s: independent CPU counter was not used: %+v", c.name, alerts)
		}
	}
}

// Optional accounting never makes a missing, unsupported or incoherent counter
// usable, and explicit legacy refusals retain their previous meaning.
func TestPgCpuOptionalAccountingKeepsSourceGuards(t *testing.T) {
	now := syntheticSettings(nil).Now()
	legacy := pgCpuTestOutput(now, 34000000000)
	modern := strings.ReplaceAll(legacy, "CPUAccounting=yes\n", "")
	for _, c := range []struct{ name, output string }{
		{name: "legacy disabled", output: strings.Replace(legacy, "CPUAccounting=yes", "CPUAccounting=no", 1)},
		{name: "legacy malformed", output: strings.Replace(legacy, "CPUAccounting=yes", "CPUAccounting=unknown", 1)},
		{name: "legacy empty", output: strings.Replace(legacy, "CPUAccounting=yes", "CPUAccounting=", 1)},
		{name: "counter missing", output: strings.Replace(modern, "CPUUsageNSec=100000000000\n", "", 1)},
		{name: "counter empty", output: strings.Replace(modern, "CPUUsageNSec=100000000000", "CPUUsageNSec=", 1)},
		{name: "counter sentinel", output: strings.Replace(modern, "CPUUsageNSec=100000000000", "CPUUsageNSec=18446744073709551615", 1)},
		{name: "counter malformed", output: strings.Replace(modern, "CPUUsageNSec=100000000000", "CPUUsageNSec=unknown", 1)},
		{name: "counter reset", output: strings.Replace(modern, "CPUUsageNSec=117000000000", "CPUUsageNSec=1", 1)},
		{name: "counter duplicate", output: strings.Replace(modern, "CPUUsageNSec=100000000000", "CPUUsageNSec=100000000000\nCPUUsageNSec=100000000000", 1)},
		{name: "unexpected field", output: strings.Replace(modern, "CPUUsageNSec=100000000000", "CPUUsageNSec=100000000000\nUnknown=1", 1)},
		{name: "invocation changed", output: strings.Replace(modern, "InvocationID=00000000000000000000000000000001", "InvocationID=00000000000000000000000000000003", 1)},
		{name: "inactive only", output: strings.ReplaceAll(modern, "ActiveState=active", "ActiveState=inactive")},
	} {
		alerts, err := NewPgCpuSignal().Run(context.Background(), pgCpuTestSettings(t, c.output, nil))
		if err != nil || len(alerts) != 1 || alerts[0].Class != "pg-cpu-unavailable" {
			t.Fatalf("%s: unusable source accepted: alerts=%+v err=%v", c.name, alerts, err)
		}
	}
}
