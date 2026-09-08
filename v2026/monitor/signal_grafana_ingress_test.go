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
			if !strings.Contains(joined, "https://main-grafana.example.com/api/health") {
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
	settings.PublicDomain = "example.com"
	settings.Hosts = []HostSettings{{
		Name: "by-us-test-edge-4",
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
			if host.Name != "by-us-test-edge-4" || !strings.Contains(command, "journalctl") {
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
	settings.PublicDomain = "example.com"
	settings.Hosts = []HostSettings{{
		Name: "by-us-test-edge-4",
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
