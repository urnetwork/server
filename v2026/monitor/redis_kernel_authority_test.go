package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Execute the real observation command with local synthetic source programs.
// Scripted exit statuses reproduce denial and timeout without sleeps or SSH.
func TestRedisKernelAuthorityNativeRead(t *testing.T) {
	cases := []struct {
		name      string
		kernel    string
		status    int
		cpuTicks  uint64
		wantClass string
		wantCpu   bool
	}{
		{name: "readable empty kernel"},
		{name: "readable nonmatching kernel", kernel: "synthetic ordinary kernel message\n"},
		{name: "cpu threshold", cpuTicks: 200},
		{name: "permission denied", status: 1, wantClass: "cannot-observe"},
		{name: "timeout exit", status: 124, wantClass: "cannot-observe"},
		{name: "command unavailable", status: 127, wantClass: "cannot-observe"},
		{name: "partial failed read", kernel: "Out of memory: Killed process 4242 (redis-server) synthetic-private-kernel\n", status: 1, wantClass: "cannot-observe"},
		{name: "cpu survives kernel denial", status: 1, cpuTicks: 251, wantClass: "cannot-observe", wantCpu: true},
		{name: "complete redis victim", kernel: "Out of memory: Killed process 4242 (redis-server)\n", wantClass: "redis-kernel-oom"},
	}
	for _, test := range cases {
		fixture := redisProcessCpuFixture{kernel: test.kernel, kernelStatus: test.status}
		if test.cpuTicks > 0 {
			fixture = redisProcessCpuSingleFixture("250.5", 0, test.cpuTicks)
			fixture.kernel = test.kernel
			fixture.kernelStatus = test.status
		}
		settings, _, _ := redisProcessCpuNativeFixture(t, fixture)
		alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: observation err=%v", test.name, err)
		}

		wantCount := 0
		if test.wantClass != "" {
			wantCount++
		}
		if test.wantCpu {
			wantCount++
			requireAlertClass(t, alerts, "redis-cpu-sustained")
		}
		if test.wantClass == "cannot-observe" {
			redisKernelAuthorityRequireUnknown(t, test.name, alerts)
		} else if test.wantClass != "" {
			alert := requireAlertClass(t, alerts, test.wantClass)
			if alert.Severity != SeverityPage {
				t.Fatalf("%s: complete victim severity=%s", test.name, alert.Severity)
			}
		}
		if len(alerts) != wantCount {
			t.Errorf("%s: got %d alerts, want %d", test.name, len(alerts), wantCount)
		}
	}
}

// A successful transport cannot turn a malformed or truncated frame into an
// authoritative empty kernel sample or publish an incomplete event payload.
func TestRedisKernelAuthorityRejectsIncompleteFrames(t *testing.T) {
	cases := []struct {
		name   string
		output string
	}{
		{name: "empty response"},
		{name: "missing trailer", output: "kernel_oom Out of memory: Killed process 4242 (redis-server) synthetic-private-kernel\n"},
		{name: "truncated trailer", output: "kernel_source compl"},
		{name: "unknown status", output: "kernel_source synthetic-private-kernel\n"},
		{name: "duplicated trailer", output: "kernel_source complete\nkernel_source complete\n"},
		{name: "trailing data", output: "kernel_source complete\nsynthetic-private-kernel\n"},
		{name: "malformed event", output: "kernel_oom\nkernel_source complete\n"},
		{name: "duplicate event", output: "kernel_oom synthetic-private-kernel\nkernel_oom synthetic-private-kernel\nkernel_source complete\n"},
		{name: "unexpected row", output: "synthetic-private-kernel\nkernel_source complete\n"},
		{name: "failed partial event", output: "kernel_oom Out of memory: Killed process 4242 (redis-server) synthetic-private-kernel\nkernel_source unavailable\n"},
	}
	for _, test := range cases {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			return "process_source complete 0\n" + test.output, nil
		}}
		settings := syntheticSettings(source)
		alerts, err := NewRedisProcessSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: unexpected adapter error: %v", test.name, err)
		}
		redisKernelAuthorityRequireUnknown(t, test.name, alerts)
		if len(alerts) != 1 {
			t.Errorf("%s: got %d alerts, want one visibility alert", test.name, len(alerts))
		}
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		findings, err := (redisProcessProbe{}).check(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range findings {
			if finding.healthy {
				t.Errorf("%s: incomplete kernel source produced a healthy finding", test.name)
			}
		}
	}
}

// Stable visibility identity and fixed rendered data must survive every source
// failure without leaking private diagnostics or inventing a Redis victim.
func redisKernelAuthorityRequireUnknown(t *testing.T, name string, alerts Alerts) {
	t.Helper()
	var unknown *Alert
	for index := range alerts {
		alert := &alerts[index]
		if alert.Class == "cannot-observe" {
			unknown = alert
		}
		if alert.Class == "redis-kernel-oom" {
			t.Errorf("%s: unavailable kernel evidence became a Redis OOM page", name)
		}
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded)+alert.Markdown(), "synthetic-private-kernel") {
			t.Errorf("%s: source diagnostic escaped into an alert", name)
		}
	}
	if unknown == nil {
		t.Errorf("%s: unavailable kernel source did not emit cannot-observe", name)
		return
	}
	if unknown.SignalNumber != "3.4" || unknown.SignalKey != "redis-process" ||
		unknown.SignalID != "monitor/visibility" || unknown.Target != "redis-1/redis-kernel" ||
		unknown.Frame != "" || unknown.Severity != SeverityWarn || unknown.Sustain != 2 || !unknown.unobservable {
		t.Errorf("%s: kernel source visibility identity or authority changed", name)
	}
	if unknown.Observed != "error_class=observation-command-failed" && unknown.Observed != "error_class=observation-invalid-response" {
		t.Errorf("%s: kernel source visibility lost its fixed error classification", name)
	}
}
