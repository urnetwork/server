package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestGrafanaIngressSignalSyntheticEdgeSpecificFailures(t *testing.T) {
	addresses := map[string]string{
		"healthy":  "2001:db8:1::1",
		"upstream": "2001:db8:2::2",
		"response": "2001:db8:3::3",
		"timeout":  "2001:db8:4::4",
	}
	source := &syntheticSource{
		localFn: func(name string, args ...string) (string, error) {
			if name != "curl" {
				return "", errors.New("unexpected local command")
			}
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "https://main-grafana.example.test/api/health") {
				return "", errors.New("Grafana probe did not retain the configured health path")
			}
			switch {
			case strings.Contains(joined, "["+addresses["healthy"]+"]"):
				return edgeHTTPFixture("200", "0", addresses["healthy"], "0.080"), nil
			case strings.Contains(joined, "["+addresses["upstream"]+"]"):
				return edgeHTTPFixture("502", "0", addresses["upstream"], "0.091"), nil
			case strings.Contains(joined, "["+addresses["response"]+"]"):
				return edgeHTTPFixture("401", "0", addresses["response"], "0.084"), nil
			case strings.Contains(joined, "["+addresses["timeout"]+"]"):
				return "curl: (28) Timeout was reached\n" + edgeHTTPFixture("000", "28", "", "3.001"), errors.New("exit status 28")
			default:
				return "", errors.New("unexpected edge address")
			}
		},
	}
	settings := syntheticSettings(source)
	settings.Environment = "main"
	settings.PublicDomain = "example.test"
	settings.Hosts = []HostSettings{{
		Name: "edge.example.test",
		EdgeIPv6: []EdgeIPv6InterfaceSettings{
			{Interface: "eno-healthy", Address: addresses["healthy"]},
			{Interface: "eno-upstream", Address: addresses["upstream"]},
			{Interface: "eno-response", Address: addresses["response"]},
			{Interface: "eno-timeout", Address: addresses["timeout"]},
		},
	}}

	alerts, err := NewGrafanaIngressSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts = %d, want upstream and completed-response failures: %+v", len(alerts), alerts)
	}
	upstream := requireAlertClass(t, alerts, "grafana-edge-upstream")
	response := requireAlertClass(t, alerts, "grafana-edge-response")
	if upstream.SignalNumber != "11.17" || upstream.SignalKey != "grafana-ingress" {
		t.Fatalf("wrong signal identity: %+v", upstream)
	}
	if !strings.Contains(upstream.Markdown(), "service alias without a live DNAT target") {
		t.Fatalf("upstream alert lacks rollout discriminator: %s", upstream.Markdown())
	}
	if !strings.Contains(response.Markdown(), "HTTP 401") {
		t.Fatalf("response alert lacks returned status: %s", response.Markdown())
	}
	for _, alert := range alerts {
		if strings.Contains(alert.Frame, "eno-healthy") || strings.Contains(alert.Frame, "eno-timeout") {
			t.Fatalf("healthy or edge-ipv6-owned transport path alerted: %+v", alert)
		}
	}
}

func TestGrafanaIngressSignalAttributesSchedulerGridOncePerFailedHost(t *testing.T) {
	addresses := []string{"2001:db8:4::1", "2001:db8:4::2"}
	hostCalls := 0
	source := &syntheticSource{
		localFn: func(name string, args ...string) (string, error) {
			if name != "curl" {
				return "", errors.New("unexpected local command")
			}
			joined := strings.Join(args, " ")
			for _, address := range addresses {
				if strings.Contains(joined, "["+address+"]") {
					return edgeHTTPFixture("502", "0", address, "0.090"), nil
				}
			}
			return "", errors.New("unexpected edge address")
		},
		hostFn: func(host HostSettings, command string) (string, error) {
			hostCalls++
			if host.Name != "edge.example.test" || !strings.Contains(command, "journalctl") {
				return "", fmt.Errorf("unexpected Grafana battery: host=%s command=%q", host.Name, command)
			}
			return strings.Join([]string{
				"unit_state active running",
				`Poll result {"status":"error not ready (grafana connection refused)"}`,
				"Failed to provision alerting: invalid alert rule: interval (15s) should be non-zero and divided exactly by scheduler interval: 10",
				"[grafana]exited (exit status 1). Restarting.",
			}, "\n"), nil
		},
	}
	settings := syntheticSettings(source)
	settings.Environment = "main"
	settings.PublicDomain = "example.test"
	settings.Hosts = []HostSettings{{
		Name: "edge.example.test",
		EdgeIPv6: []EdgeIPv6InterfaceSettings{
			{Interface: "eno3", Address: addresses[0]},
			{Interface: "eno4", Address: addresses[1]},
		},
	}}

	alerts, err := NewGrafanaIngressSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if hostCalls != 1 {
		t.Fatalf("Grafana host battery calls = %d, want one shared call", hostCalls)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts = %d, want one per failed interface: %+v", len(alerts), alerts)
	}
	for _, alert := range alerts {
		markdown := alert.Markdown()
		for _, want := range []string{
			"root_cause=alert-interval-scheduler-grid",
			"rejected_interval=15s",
			"scheduler_interval_seconds=10",
			"supervised child can crash-loop",
			"does not implicate interface routing",
			"TestProvisionedAlertIntervalsMatchGrafanaScheduler",
			"SIGNALS.md §11.16 and §11.17",
		} {
			if !strings.Contains(markdown, want) {
				t.Fatalf("scheduler-grid diagnosis missing %q: %s", want, markdown)
			}
		}
	}
}

func TestGrafanaIngressSignalDoesNotAttributeUnrelatedHostBatteryToTlsOrNonUpstreamResponses(t *testing.T) {
	for _, test := range []struct {
		code string
		exit string
	}{
		{code: "000", exit: "60"},
		{code: "401", exit: "0"},
		{code: "404", exit: "0"},
	} {
		batteryCalls := 0
		settings := syntheticSettings(&syntheticSource{
			localFn: func(_ string, args ...string) (string, error) {
				if strings.Contains(strings.Join(args, " "), "[2001:db8::1]") {
					return edgeHTTPFixture("502", "0", "2001:db8::1", "0.080"), nil
				}
				output := edgeHTTPFixture(test.code, test.exit, "2001:db8::2", "0.070")
				if test.exit != "0" {
					return output, errors.New("synthetic tls failure")
				}
				return output, nil
			},
			hostFn: func(HostSettings, string) (string, error) {
				batteryCalls++
				return "invalid alert rule: interval (15s) should be non-zero and divided exactly by scheduler interval: 10", nil
			},
		})
		settings.Environment = "main"
		settings.PublicDomain = "example.test"
		settings.Hosts = []HostSettings{{Name: "edge.example.test", EdgeIPv6: []EdgeIPv6InterfaceSettings{
			{Interface: "upstream", Address: "2001:db8::1"},
			{Interface: "other", Address: "2001:db8::2"},
		}}}
		alerts, err := NewGrafanaIngressSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if batteryCalls != 1 || len(alerts) != 2 {
			t.Fatalf("same-host upstream battery or typed failure identity changed: calls=%d alerts=%d", batteryCalls, len(alerts))
		}
		upstream := requireAlertClass(t, alerts, "grafana-edge-upstream")
		if !strings.Contains(upstream.Observed, "root_cause=alert-interval-scheduler-grid") {
			t.Error("corroborating upstream response lost its interval discriminator")
		}
		other := requireAlertClass(t, alerts, "grafana-edge-response")
		if strings.Contains(other.Observed, "root_cause=") || strings.Contains(other.Mechanism, "provisioned") || strings.Contains(other.Action, "corrected Grafana image") {
			t.Errorf("HTTP %s / exit %s attributed an unrelated same-host interval log", test.code, test.exit)
		}
		if test.code == "000" {
			for _, text := range []string{"completed a public Grafana request", "TLS reached", "HTTP 000"} {
				if strings.Contains(other.Markdown(), text) {
					t.Errorf("pre-HTTP TLS failure falsely completed transport/request: %q", text)
				}
			}
		}
	}
}

func TestGrafanaIngressSignalMalformedDiagnosticsAreUnknownNotOutageOrRecovery(t *testing.T) {
	valid := edgeHTTPFixture("200", "0", "2001:db8::1", "0.080")
	for _, output := range []string{
		"", "monitor_http_code=200\n", strings.ReplaceAll(valid, "monitor_exitcode=0\n", ""),
		valid + "monitor_http_code=200\n", strings.ReplaceAll(valid, "monitor_http_code=200", "monitor_http_code=000"),
		strings.ReplaceAll(valid, "monitor_http_code=200", "monitor_http_code=garbage"),
		strings.ReplaceAll(valid, "monitor_exitcode=0", "monitor_exitcode=-1"),
		strings.ReplaceAll(valid, "monitor_remote_ip=2001:db8::1", "monitor_remote_ip=synthetic-secret"),
		strings.ReplaceAll(valid, "monitor_time_total=0.080", "monitor_time_total=NaN"),
		strings.ReplaceAll(valid, "monitor_remote_ip=2001:db8::1", "monitor_remote_ip=2001:db8::2"),
	} {
		settings := syntheticSettings(&syntheticSource{localFn: func(string, ...string) (string, error) { return output, nil }})
		settings.Environment = "main"
		settings.PublicDomain = "example.test"
		settings.Hosts = []HostSettings{{Name: "edge.example.test", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "public", Address: "2001:db8::1"}}}}
		alerts, err := NewGrafanaIngressSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
			t.Errorf("malformed/missing diagnostics supplied typed outage or healthy absence: alerts=%d", len(alerts))
			continue
		}
		if strings.Contains(alerts[0].Markdown(), "synthetic-secret") {
			t.Error("malformed diagnostic value leaked to Markdown")
		}
	}
}

func TestGrafanaIngressSignalDoesNotRetainRawBatteryOrTransportPayloads(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{
		localFn: func(string, ...string) (string, error) {
			return "synthetic-secret raw curl diagnostic\n" + edgeHTTPFixture("502", "0", "2001:db8::1", "0.080"), nil
		},
		hostFn: func(HostSettings, string) (string, error) { return "synthetic-secret unrelated child output", nil },
	})
	settings.Environment = "main"
	settings.PublicDomain = "example.test"
	settings.Hosts = []HostSettings{{Name: "edge.example.test", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "public", Address: "2001:db8::1"}}}}
	alerts, err := NewGrafanaIngressSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	upstream := requireAlertClass(t, alerts, "grafana-edge-upstream")
	if strings.Contains(upstream.Markdown(), "synthetic-secret") || strings.Contains(upstream.Markdown(), "raw curl diagnostic") {
		t.Error("raw transport or battery payload entered Alert Markdown")
	}
}

func TestGrafanaIngressSignalCancellationDoesNotContactOrAlert(t *testing.T) {
	calls := 0
	settings := syntheticSettings(&syntheticSource{localFn: func(string, ...string) (string, error) { calls++; return "", nil }})
	settings.Environment = "main"
	settings.PublicDomain = "example.test"
	settings.Hosts = []HostSettings{{Name: "edge.example.test", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "public", Address: "2001:db8::1"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := NewGrafanaIngressSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls != 0 {
		t.Fatalf("canceled Grafana transport supplied observation evidence: alerts=%d calls=%d err=%v", len(alerts), calls, err)
	}
}

func TestGrafanaIngressSignalPartialHttpResponseCannotSelectUpstreamProvisioningCause(t *testing.T) {
	batteryCalls := 0
	settings := syntheticSettings(&syntheticSource{
		localFn: func(string, ...string) (string, error) {
			return edgeHTTPFixture("502", "28", "2001:db8::1", "5.000"), errors.New("synthetic-secret timeout after headers")
		},
		hostFn: func(HostSettings, string) (string, error) { batteryCalls++; return "", nil },
	})
	settings.Environment = "main"
	settings.PublicDomain = "example.test"
	settings.Hosts = []HostSettings{{Name: "edge.example.test", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "public", Address: "2001:db8::1"}}}}
	alerts, err := NewGrafanaIngressSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	response := requireAlertClass(t, alerts, "grafana-edge-response")
	if batteryCalls != 0 || !strings.Contains(response.Mechanism, "failed before request completion") || strings.Contains(response.Markdown(), "synthetic-secret") || strings.Contains(response.Observed, "root_cause=") {
		t.Error("partial HTTP response became a completed upstream request or retained raw error")
	}
}

func TestGrafanaIngressSignalMidObservationCancellationDoesNotRunBatteryOrAlert(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batteryCalls := 0
	settings := syntheticSettings(&syntheticSource{
		localFn: func(string, ...string) (string, error) {
			cancel()
			return edgeHTTPFixture("502", "0", "2001:db8::1", "0.080"), nil
		},
		hostFn: func(HostSettings, string) (string, error) { batteryCalls++; return "", nil },
	})
	settings.Environment = "main"
	settings.PublicDomain = "example.test"
	settings.Hosts = []HostSettings{{Name: "edge.example.test", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "public", Address: "2001:db8::1"}}}}
	alerts, err := NewGrafanaIngressSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || batteryCalls != 0 {
		t.Fatalf("cancellation after exact-edge observation became incident evidence: alerts=%d batteryCalls=%d err=%v", len(alerts), batteryCalls, err)
	}
}

func TestGrafanaRejectedAlertIntervalRejectsMalformedAndUnboundedValues(t *testing.T) {
	for _, test := range []struct {
		interval  string
		scheduler string
		want      bool
	}{
		{interval: "15s", scheduler: "10", want: true},
		{interval: "synthetic-secret", scheduler: "10"},
		{interval: "15s", scheduler: "0"},
		{interval: "15s", scheduler: "010"},
		{interval: "15s", scheduler: "9999999999999999999999"},
		{interval: "999999999999999h", scheduler: "10"},
	} {
		diagnosis := fmt.Sprintf("invalid alert rule: interval (%s) should be non-zero and divided exactly by scheduler interval: %s", test.interval, test.scheduler)
		interval, scheduler, ok := grafanaRejectedAlertInterval(diagnosis)
		if ok != test.want {
			t.Errorf("invalid or oversized interval discriminator gave wrong result: want=%t got=%t", test.want, ok)
		}
		if !ok && (interval != "" || scheduler != "") {
			t.Error("rejected parser result retained a raw discriminator")
		}
	}
}

func TestGrafanaIngressRecentProvisioningSignatureIsNotCurrentGenerationProof(t *testing.T) {
	output := edgeHTTPFixture("502", "0", "2001:db8::1", "0.080")
	result := grafanaIngressResult{
		host: &host{name: "edge.example.test"}, configured: EdgeIPv6InterfaceSettings{Interface: "public", Address: "2001:db8::1"},
		public: exactHTTPSResult{output: output, values: parseKeyValueLines(output)},
	}
	observed := grafanaIngressFinding(result, "recent predecessor journal: invalid alert rule: interval (15s) should be non-zero and divided exactly by scheduler interval: 10")
	if observed == nil || observed.class != "grafana-edge-upstream" || !strings.Contains(observed.observed, "root_cause=alert-interval-scheduler-grid") {
		t.Fatal("recent journal signature lost the proved upstream finding/discriminator")
	}
	for _, field := range []string{observed.mechanism, observed.context, observed.action} {
		if !strings.Contains(field, "active") || !strings.Contains(field, "recent") {
			t.Error("recent unbound journal categorically attributed the active child/artifact")
		}
	}
	if !strings.Contains(observed.context, "does not prove") || !strings.Contains(observed.action, "confirm") {
		t.Error("provisioning repair lacks the exact current-generation discriminator")
	}
}
