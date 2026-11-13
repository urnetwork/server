package monitor

import (
	"context"
	"errors"
	"fmt"
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

// A native process result reaches the real SSH wrapper before the signal sees
// it. A complete-looking PSI body cannot override a failed command.
func TestHostPressureRetainsSourceFailureProvenance(t *testing.T) {
	exit255 := syntheticProcessExit(t, 255)
	exit1 := syntheticProcessExit(t, 1)
	const privateText = "synthetic-pressure-private-canary"
	for _, testCase := range []struct {
		name       string
		err        error
		stderr     string
		errorClass string
	}{
		{name: "native SSH status", err: exit255, errorClass: observationErrorClassSSHExit255},
		{name: "wrapped native SSH status", err: fmt.Errorf("%s: %w", privateText, exit255), errorClass: observationErrorClassSSHExit255},
		{name: "command status", err: exit1, errorClass: observationErrorClassCommandFailed},
		{name: "status text lacks native authority", err: errors.New("exit status 255"), errorClass: observationErrorClassCommandFailed},
		{name: "command stderr lacks native authority", err: exit1, stderr: "exit status 255", errorClass: observationErrorClassCommandFailed},
		{name: "access failure", err: exit255, stderr: "Permission denied", errorClass: observationErrorClassAccessDenied},
	} {
		runner := newRunner(&monitorConfig{addressMode: addressModeOverlay})
		runner.runSSH = func(context.Context, []string, string) (string, string, error) {
			return "compute-a\nsome avg10=90 avg60=90 avg300=90 total=1\n", testCase.stderr + " " + privateText, testCase.err
		}
		calls := 0
		source := &syntheticSource{hostTimeoutFn: func(target HostSettings, command string, timeout time.Duration) (string, error) {
			calls++
			if command != "hostname -s; head -c 512 /proc/pressure/cpu" || timeout != 10*time.Second {
				t.Fatal("pressure source command or budget changed")
			}
			if target.Name == "compute-b.example.test" {
				return "compute-b\nsome avg10=0 avg60=0 avg300=0 total=1\n", nil
			}
			output, err := runner.sshTimeout(context.Background(), &host{name: target.Name, overlayIp: "192.0.2.91"}, command, "", timeout)
			if !errors.Is(err, testCase.err) {
				t.Fatal("source fixture lost the held execution error")
			}
			return output, err
		}}
		alerts, err := NewHostPressureSignal().Run(context.Background(), hostPressureSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "host-cpu-pressure-unavailable")
		want := "source_status=unavailable failure_phase=source error_class=" + testCase.errorClass
		if alert.Observed != want {
			t.Errorf("%s: source failure provenance missing: got %q, want %q", testCase.name, alert.Observed, want)
		}
		if calls != 2 || len(alerts) != 1 || alert.Identity() != "host/cpu-pressure|host-cpu-pressure-unavailable|compute-a.example.test|" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
			t.Fatal("source failure changed identity, severity, sustain, sibling health, or source call count")
		}
		if testCase.errorClass == observationErrorClassSSHExit255 && !strings.Contains(alert.Action, "remote command") {
			t.Error("native status guidance lost the remote-command alternative")
		}
		requireAlertOmits(t, alert, privateText, "192.0.2.91", "exit status 255", "cpu_some_avg60=90", "ssh_spawn=")
	}
}

// Preserve local admission phases even when a lifecycle error has precedence.
// A release failure can follow valid stdout and a held native command failure.
func TestHostPressureRetainsAdmissionFailurePhases(t *testing.T) {
	nativeFailure := syntheticProcessExit(t, 255)
	for _, testCase := range []struct {
		name         string
		acquireError error
		releaseError error
		nativeError  error
		phase        string
		calls        int
	}{
		{name: "acquire deadline", acquireError: context.DeadlineExceeded, phase: "acquire", calls: 0},
		{name: "release deadline after valid body", releaseError: context.DeadlineExceeded, phase: "release", calls: 1},
		{name: "release deadline with held native error", releaseError: context.DeadlineExceeded, nativeError: nativeFailure, phase: "release", calls: 1},
	} {
		calls := 0
		admission := &sshAdmissionPhaseFixture{acquireCommand: func(context.Context, string) (func() error, error) {
			if testCase.acquireError != nil {
				return nil, testCase.acquireError
			}
			return func() error { return testCase.releaseError }, nil
		}}
		runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: admission})
		runner.runSSH = func(context.Context, []string, string) (string, string, error) {
			calls++
			return "compute-a\nsome avg10=0 avg60=0 avg300=0 total=1\n", "synthetic-admission-private-canary", testCase.nativeError
		}
		source := &syntheticSource{hostTimeoutFn: func(target HostSettings, command string, timeout time.Duration) (string, error) {
			output, err := runner.sshTimeout(context.Background(), &host{name: target.Name, overlayIp: "192.0.2.91"}, command, "", timeout)
			if testCase.nativeError != nil && !errors.Is(err, testCase.nativeError) {
				t.Fatal("release fixture discarded the held native error")
			}
			return output, err
		}}
		settings := hostPressureSettings(source)
		settings.Hosts = settings.Hosts[:1]
		alerts, err := NewHostPressureSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "host-cpu-pressure-unavailable")
		want := "source_status=unavailable failure_phase=source error_class=observation-timeout ssh_admission_failures=" + testCase.phase
		if alert.Observed != want {
			t.Errorf("%s: admission provenance missing: got %q, want %q", testCase.name, alert.Observed, want)
		}
		if calls != testCase.calls || len(alerts) != 1 || alert.Severity != SeverityWarn || alert.Sustain != 2 {
			t.Fatal("admission failure changed the unavailable observation or invoked unexpected transport")
		}
		if !strings.Contains(alert.Action, "Correlate native process evidence before inferring contact") {
			t.Error("admission guidance claimed contact from a local phase")
		}
		requireAlertOmits(t, alert, "synthetic-admission-private-canary", "192.0.2.91", "ssh_spawn=", "failure_phase=parse")
	}
}

// Successful source return is insufficient without a complete matched sample.
func TestHostPressureRejectsEmptyAndMalformedSuccessAsParseFailure(t *testing.T) {
	for _, output := range []string{
		"",
		"compute-a\n",
		"wrong-host\nsome avg10=0 avg60=0 avg300=0 total=1\n",
		"compute-a\nsynthetic-private-PSI-unavailable\n",
		"compute-a\nsome avg10=0 avg60=NaN avg300=0 total=1\n",
		"compute-a\nsome avg10=0 avg60=0 avg300=0\n",
		"compute-a\nsome avg10=0 avg60=0 avg60=0 total=1\n",
		"compute-a\nsome avg10=0 avg60=0 avg300=0 total=invalid\n",
	} {
		source := &syntheticSource{hostFn: func(target HostSettings, _ string) (string, error) {
			if target.Name == "compute-a.example.test" {
				return output, nil
			}
			return "compute-b\nsome avg10=0 avg60=0 avg300=0 total=1\n", nil
		}}
		alerts, err := NewHostPressureSignal().Run(context.Background(), hostPressureSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "host-cpu-pressure-unavailable")
		if alert.Observed != "source_status=unavailable failure_phase=parse error_class=observation-invalid-response" {
			t.Errorf("parse failure provenance missing: got %q", alert.Observed)
		}
		if len(alerts) != 1 || alert.Severity != SeverityWarn || alert.Sustain != 2 {
			t.Fatal("empty or malformed success became healthy or changed the unavailable alert")
		}
		requireAlertOmits(t, alert, "synthetic-private-PSI-unavailable", "wrong-host", "NaN", "total=invalid", "ssh_admission_failures=", "failure_phase=source")
	}
}

// Only avg60 selects the pressure band; valid in-band input emits no Alerts.
func TestHostPressureValidZeroAndBelowThresholdRemainHealthy(t *testing.T) {
	for _, average := range []string{"0", "19.99"} {
		calls := 0
		source := &syntheticSource{hostTimeoutFn: func(target HostSettings, command string, timeout time.Duration) (string, error) {
			calls++
			if command != "hostname -s; head -c 512 /proc/pressure/cpu" || timeout != 10*time.Second {
				t.Fatal("healthy pressure command or budget changed")
			}
			return strings.Split(target.Name, ".")[0] + "\nsome avg10=90 avg60=" + average + " avg300=90 total=1\n", nil
		}}
		alerts, err := NewHostPressureSignal().Run(context.Background(), hostPressureSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) != 0 || calls != 2 || NewHostPressureSignal().Cadence() != time.Minute {
			t.Fatal("valid in-band PSI no longer yields an empty alert set at the unchanged cadence")
		}
	}
}
