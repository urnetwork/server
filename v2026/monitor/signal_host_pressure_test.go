package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func hostPressureSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "compute-a.example.test", Roles: []string{"services"}},
		{Name: "compute-b.example.test", Roles: []string{"services"}},
	}
	settings.Now = func() time.Time { return time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC) }
	return settings
}

func TestHostPressureDetectsRunnableDelayBelowCPUExecutionBand(t *testing.T) {
	called := map[string]int{}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if command != "hostname -s; head -c 512 /proc/pressure/cpu" {
			t.Fatalf("unexpected command: %q", command)
		}
		called[host.Name]++
		if host.Name == "compute-a.example.test" {
			return "compute-a\nsome avg10=53.57 avg60=54.17 avg300=54.10 total=255830557488\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", nil
		}
		return "compute-b\nsome avg10=0.88 avg60=1.76 avg300=1.67 total=13636852686\n", nil
	}}
	alerts, err := NewHostPressureSignal().Run(context.Background(), hostPressureSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "host-cpu-pressure")
	if len(alerts) != 1 || alert.Target != "compute-a.example.test" || alert.Severity != SeverityPage || alert.Sustain != 2 {
		t.Fatalf("wrong pressure alert: %+v", alerts)
	}
	for _, want := range []string{"cpu_some_avg60=54.17%", "Cgroup quotas", "not host-wide CPU exhaustion"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("missing %q from alert", want)
		}
	}
	if called["compute-a.example.test"] != 1 || called["compute-b.example.test"] != 1 {
		t.Fatalf("source calls = %+v", called)
	}
}

func TestHostPressureUnavailableNeverBecomesHealthy(t *testing.T) {
	cases := []struct {
		output string
		err    error
	}{
		{"", errors.New("transport unavailable")},
		{"wrong-host\nsome avg10=1 avg60=1 avg300=1 total=1\n", nil},
		{"compute-a\nsome avg10=1 avg60=NaN avg300=1 total=1\n", nil},
		{"compute-a\nsome avg10=1 avg60=1 avg300=1\n", nil},
	}
	for _, test := range cases {
		source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
			if host.Name == "compute-a.example.test" {
				return test.output, test.err
			}
			return "compute-b\nsome avg10=0 avg60=0 avg300=0 total=1\n", nil
		}}
		alerts, err := NewHostPressureSignal().Run(context.Background(), hostPressureSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "host-cpu-pressure-unavailable")
		if len(alerts) != 1 || alert.Target != "compute-a.example.test" || alert.Severity != SeverityWarn ||
			!strings.Contains(alert.Markdown(), "unknown, not zero") {
			t.Fatalf("unavailable pressure = %+v", alerts)
		}
	}
}

func TestHostPressureDoesNotContactInventoryDisabledHost(t *testing.T) {
	calls := 0
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		calls++
		if host.Name != "compute-a.example.test" {
			t.Fatalf("disabled host contacted: %s", host.Name)
		}
		return "compute-a\nsome avg10=0 avg60=0 avg300=0 total=1\n", nil
	}}
	settings := hostPressureSettings(source)
	settings.Hosts = settings.Hosts[:1]
	settings.disabledHosts = []HostSettings{{Name: "compute-b.example.test", Roles: []string{"services"}}}
	alerts, err := NewHostPressureSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 || calls != 1 {
		t.Fatalf("alerts=%+v calls=%d", alerts, calls)
	}
}
