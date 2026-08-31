package monitor

import (
	"context"
	"errors"
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
