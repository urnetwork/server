// Literal Redis/TCP category text supplies diagnostic counts, not original
// target, topology, lifecycle, retry, or borrower-outcome authority.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The public signal must retain positive categories while requiring original
// event evidence before its rendered guidance asserts an operational cause.
func TestLogErrorsRedisCategoryAuthority(t *testing.T) {
	for _, test := range []struct {
		class     string
		line      string
		threshold int
		severity  Severity
		forbidden string
	}{
		{class: "clusterdown", line: "synthetic relay quoted diagnostic: CLUSTERDOWN", threshold: 5, severity: SeverityPage, forbidden: "slot coverage lost; transient during elections is expected and retried"},
		{class: "redis-loading", line: "synthetic relay quoted diagnostic: LOADING", threshold: 50, severity: SeverityWarn, forbidden: "node restarting (rdb load) / replica mid-failover"},
		{class: "redis-loading", line: "synthetic relay quoted diagnostic: READONLY", threshold: 50, severity: SeverityWarn, forbidden: "only sustained > 2 min matters"},
		{class: "connection-refused", line: "synthetic relay quoted diagnostic: connect: connection refused", threshold: 10, severity: SeverityPage, forbidden: "had no accepting listener"},
	} {
		alerts := redisCategoryAuthorityAlerts(t, strings.Repeat(test.line+"\n", test.threshold))
		alert := requireAlertClass(t, alerts, test.class)
		if len(alerts) != 1 || alert.Identity() != "logs/"+test.class+"|"+test.class+"|grafana|" ||
			alert.SignalNumber != "1.5" || alert.Severity != test.severity || alert.Sustain != 1 ||
			alert.Observed != fmt.Sprintf("rate=%d/min class=%s target=grafana", test.threshold, test.class) {
			t.Fatalf("%s: category wording changed diagnostic identity, threshold or severity: %+v", test.line, alerts)
		}
		if strings.Contains(alert.Markdown(), test.forbidden) {
			t.Errorf("literal category asserted its former unsupported cause: %s", alert.Markdown())
		}
		for _, required := range []struct{ value, text string }{
			{value: alert.Mechanism, text: "diagnostic lines"},
			{value: alert.Context, text: "selected service log stream"},
			{value: alert.Context, text: "quoted diagnostics"},
			{value: alert.Action, text: "original event"},
			{value: alert.Action, text: "immutable"},
			{value: alert.Action, text: "operation"},
			{value: alert.Verify, text: "independently acknowledged"},
			{value: alert.Verify, text: "missing logs"},
		} {
			if !strings.Contains(required.value, required.text) {
				t.Errorf("%s: category %s omitted authority boundary %q", test.line, test.class, required.text)
			}
		}
		if test.class == "redis-loading" && !strings.Contains(alert.Context, "not a two-minute persistence test") {
			t.Error("one fixed-clock window acquired an unimplemented persistence gate")
		}
	}
}

// Zero, healthy and below-band windows remain quiet; the exact line-count
// thresholds and both loading alternatives retain their positive bands.
func TestLogErrorsRedisCategoryThresholdsAndControls(t *testing.T) {
	for _, test := range []struct {
		class     string
		line      string
		threshold int
	}{
		{class: "clusterdown", line: "CLUSTERDOWN", threshold: 5},
		{class: "redis-loading", line: "LOADING", threshold: 50},
		{class: "redis-loading", line: "READONLY", threshold: 50},
		{class: "connection-refused", line: "connect: connection refused", threshold: 10},
	} {
		for _, count := range []int{0, test.threshold - 1, test.threshold, test.threshold + 1} {
			alerts := redisCategoryAuthorityAlerts(t, strings.Repeat(test.line+"\n", count)+"synthetic operation completed successfully\n")
			if count < test.threshold {
				if len(alerts) != 0 {
					t.Fatalf("%s/%d: healthy or below-band input produced an alert: %+v", test.line, count, alerts)
				}
				continue
			}
			alert := requireAlertClass(t, alerts, test.class)
			if len(alerts) != 1 || alert.Observed != fmt.Sprintf("rate=%d/min class=%s target=grafana", count, test.class) {
				t.Fatalf("%s/%d: category diagnostic band changed: %+v", test.line, count, alerts)
			}
		}
	}
	alerts := redisCategoryAuthorityAlerts(t, strings.Repeat("panic: synthetic unrelated failure\n", 5))
	alert := requireAlertClass(t, alerts, "panic")
	if len(alerts) != 1 || alert.Severity != SeverityPage {
		t.Fatalf("category wording changed an unrelated positive class: %+v", alerts)
	}
}

// Every external read is replaced; unexpected helper calls fail the fixture.
func redisCategoryAuthorityAlerts(t *testing.T, lines string) []Alert {
	t.Helper()
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if name != "warpctl" || len(args) == 0 {
			return "", fmt.Errorf("unexpected synthetic helper")
		}
		switch args[0] {
		case "ls":
			return "repo names synthetic-grafana", nil
		case "logs":
			return lines, nil
		default:
			return "", fmt.Errorf("unexpected synthetic operation")
		}
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}
