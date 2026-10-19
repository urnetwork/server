// Actual generated shell/parse/Signal controls use only synthetic command
// fixtures. Unit read success and state values are independently controlled.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type vpnUnitFixture struct {
	active, sub, restarts            string
	activeExit, subExit, restartExit int
}

func vpnUnitAuthoritySettings(t *testing.T, fixture vpnUnitFixture) (SignalSettings, string) {
	t.Helper()
	settings, pingLog, _ := vpnSessionScopeTestSettings(t, true)
	directory := filepath.Dir(pingLog)
	for name, value := range map[string]string{
		"active-output": fixture.active, "sub-output": fixture.sub, "restart-output": fixture.restarts,
		"active-exit": strconv.Itoa(fixture.activeExit), "sub-exit": strconv.Itoa(fixture.subExit), "restart-exit": strconv.Itoa(fixture.restartExit),
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
directory=${0%/*}
case "$*" in
  *ActiveState*) field=active ;;
  *SubState*) field=sub ;;
  *NRestarts*) field=restart ;;
  *) exit 64 ;;
esac
cat "$directory/$field-output"
exit "$(cat "$directory/$field-exit")"
`
	if err := os.WriteFile(filepath.Join(directory, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return settings, pingLog
}

func TestVPNSessionsUnitInspectionFailureIsNotAnOutage(t *testing.T) {
	settings, _ := vpnUnitAuthoritySettings(t, vpnUnitFixture{activeExit: 7, subExit: 8, restarts: "0"})
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || alerts[0].Class != "vpn-server-unavailable" || alerts[0].Severity != SeverityWarn || alerts[0].Sustain != 1 {
		t.Fatal("failed unit inspection became affirmative outage")
	}
	if alerts[0].SignalNumber != "21.1" || alerts[0].SignalKey != "vpn-sessions" || alerts[0].SignalID != "network/vpn-sessions" || !isVisibilityAlert(alerts[0]) || !strings.Contains(alerts[0].Observed, "unit_state_observed=false") {
		t.Fatal("inspection failure lost originating visibility authority")
	}
	for _, forbidden := range []string{"does not own a running process", "therefore a server-side control-plane outage", "vpn-server-unhealthy"} {
		if strings.Contains(alerts[0].Markdown(), forbidden) {
			t.Fatal("unknown inspection retained affirmative outage narrative")
		}
	}
}

func TestVPNSessionsUnitInspectionPartialAndMisleadingStdoutStayUnknown(t *testing.T) {
	for _, fixture := range []vpnUnitFixture{
		{active: "active", activeExit: 1, sub: "running", restarts: "0"},
		{active: "active", sub: "running", subExit: 1, restarts: "0"},
		{active: "failed", activeExit: 1, sub: "failed", restarts: "0"},
		{active: "", sub: "running", restarts: "0"},
		{active: "active\nfailed", sub: "running", restarts: "0"},
		{active: "active", sub: "running synthetic-private-unit-text", restarts: "0"},
		{active: "synthetic-private-unit-text", sub: "running", restarts: "0"},
	} {
		settings, _ := vpnUnitAuthoritySettings(t, fixture)
		alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 || alerts[0].Class != "vpn-server-unavailable" || alerts[0].Severity != SeverityWarn || !isVisibilityAlert(alerts[0]) {
			t.Fatal("partial/failed/invalid state read acquired unit authority")
		}
		if strings.Contains(alerts[0].Markdown(), "synthetic-private-unit-text") {
			t.Fatal("unrecognized command text escaped fixed state projection")
		}
	}
}

func TestVPNSessionsUnitInspectionAuthoritativeFailuresKeepPage(t *testing.T) {
	for _, fixture := range []vpnUnitFixture{
		{active: "failed", sub: "failed", restarts: "3"},
		{active: "inactive", sub: "dead", restarts: "3"},
		{active: "activating", sub: "start-pre", restarts: "3"},
		{active: "active", sub: "exited", restarts: "3"},
	} {
		settings, _ := vpnUnitAuthoritySettings(t, fixture)
		alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 || alerts[0].Class != "vpn-server-unhealthy" || alerts[0].Severity != SeverityPage || alerts[0].Sustain != 1 || !isVisibilityAlert(alerts[0]) {
			t.Fatal("authoritative non-running unit state lost existing PAGE1 visibility classification")
		}
		if !strings.Contains(alerts[0].Observed, "nrestarts=3") || strings.Contains(alerts[0].Mechanism, "does not own a running process") {
			t.Fatal("observed nonbaseline state acquired an unsupported process-absence claim")
		}
	}
}

func TestVPNSessionsUnitInspectionRestartMetadataDoesNotBecomeZero(t *testing.T) {
	for _, fixture := range []vpnUnitFixture{
		{active: "failed", sub: "failed", restarts: "0", restartExit: 9},
		{active: "failed", sub: "failed", restarts: ""},
		{active: "failed", sub: "failed", restarts: "synthetic-private-restart-text"},
		{active: "failed", sub: "failed", restarts: "999999999999999999999999999999999999999"},
	} {
		settings, _ := vpnUnitAuthoritySettings(t, fixture)
		alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 || alerts[0].Class != "vpn-server-unhealthy" || alerts[0].Severity != SeverityPage || !strings.Contains(alerts[0].Observed, "nrestarts=unknown") {
			t.Fatal("unavailable restart metadata became zero or erased authoritative unit failure")
		}
		if strings.Contains(alerts[0].Markdown(), "synthetic-private-restart-text") {
			t.Fatal("restart command text was exported")
		}
	}
}

func TestVPNSessionsUnitInspectionHealthyGeneratedReader(t *testing.T) {
	for _, fixture := range []vpnUnitFixture{
		{active: "active", sub: "running", restarts: "0"},
		{active: "active", sub: "running", restarts: "0", restartExit: 9},
	} {
		settings, pingLog := vpnUnitAuthoritySettings(t, fixture)
		alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 || vpnSessionScopePingAttempts(t, pingLog) != "192.0.2.10\n192.0.2.20\n" {
			t.Fatal("healthy unit/session/path control changed")
		}
	}
}

func TestVPNSessionsUnitInspectionLegacyOrContradictoryBodyIsUnknown(t *testing.T) {
	now := time.Date(2035, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, prefix := range []string{"", "server_unit_observed false\n", "server_unit_observed true\n"} {
		active := "failed"
		if strings.Contains(prefix, "true") {
			active = "unknown"
		}
		body := prefix + fmt.Sprintf("server_active_state %s\nserver_sub_state failed\nserver_restarts unknown\nstatus_mtime_epoch %d\n", active, now.Unix())
		settings := vpnSessionTestSettings(t, now, body)
		settings.Source.(*syntheticSource).hostFn = func(HostSettings, string) (string, error) { return body, nil }
		alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 || alerts[0].Class != "vpn-server-unavailable" || !isVisibilityAlert(alerts[0]) {
			t.Fatal("missing or contradictory success provenance became unit outage authority")
		}
	}
}

func TestVPNSessionsUnitInspectionTransportErrorNeverUsesPartialBody(t *testing.T) {
	settings, _ := vpnUnitAuthoritySettings(t, vpnUnitFixture{active: "failed", sub: "failed", restarts: "1"})
	source := settings.Source.(*syntheticSource)
	inspect := source.hostFn
	source.hostFn = func(h HostSettings, command string) (string, error) {
		body, _ := inspect(h, command)
		return body, errors.New("synthetic transport failure")
	}
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
	if err == nil || len(alerts) != 0 {
		t.Fatal("failed transport's partial body became a unit verdict")
	}
}

func TestVPNSessionsUnitInspectionUnknownPreservesScopeCoverage(t *testing.T) {
	for _, excluded := range [][]string{{"excluded.example.test"}, {"allowed.example.test", "excluded.example.test"}} {
		settings, pingLog := vpnUnitAuthoritySettings(t, vpnUnitFixture{activeExit: 1, subExit: 1, restarts: "0"})
		scoped, err := ExcludeHosts(settings, excluded...)
		if err != nil {
			t.Fatal(err)
		}
		alerts, err := NewVPNSessionsSignal().Run(context.Background(), scoped)
		if err != nil || len(alerts) != 2 {
			t.Fatal("unit unknown lost independent client scope warning")
		}
		requireVPNSessionAlert(t, alerts, "vpn-server-unavailable", "server.example.test")
		requireAlertClass(t, alerts, "monitor-host-scope-partial")
		want := "192.0.2.10\n"
		if len(excluded) == 2 {
			want = ""
		}
		if vpnSessionScopePingAttempts(t, pingLog) != want {
			t.Fatal("unit inspection change contacted a denied client")
		}
	}
}

func TestVPNSessionsUnitInspectionUnavailableKeepsPriorPageInConsumer(t *testing.T) {
	failed, _ := vpnUnitAuthoritySettings(t, vpnUnitFixture{active: "failed", sub: "failed", restarts: "1"})
	unknown, _ := vpnUnitAuthoritySettings(t, vpnUnitFixture{activeExit: 1, subExit: 1, restarts: "0"})
	signal := NewVPNSessionsSignal()
	page, err := signal.Run(context.Background(), failed)
	if err != nil {
		t.Fatal(err)
	}
	warn, err := signal.Run(context.Background(), unknown)
	if err != nil {
		t.Fatal(err)
	}
	webhook, webhookURL := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookURL))
	notifier.Handle(signal, SignalExecution{Alerts: page})
	notifier.Handle(signal, SignalExecution{Alerts: warn})
	closeSlackNotifier(t, notifier)
	texts := webhook.posted()
	if countSlackPosts(texts, "[PAGE] raised", "vpn-server-unhealthy") != 1 || countSlackPosts(texts, "[CLEARED]", "vpn-server-unhealthy") != 0 || countSlackPosts(texts, "[WARN] raised", "vpn-server-unavailable") != 1 {
		t.Fatal("inspection unknown cleared prior PAGE or retained the false PAGE raise")
	}
}

func TestVPNSessionsSameSourceControlIsNotAnUnrelatedWitness(t *testing.T) {
	now := time.Date(2035, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, mode := range []string{"missing", "unreachable", "mixed"} {
		for _, controlGroup := range []int{1, 2} {
			body := fmt.Sprintf("server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 192.0.2.31 %d %d\nreach 192.0.2.31 true\nreach 192.0.2.32 false\nreach 192.0.2.33 false\n", now.Unix(), now.Add(-time.Hour).Unix(), controlGroup)
			for ordinal, target := range []string{"node.example.test", "archive.example.test"} {
				if mode == "unreachable" || mode == "mixed" && ordinal == 1 {
					body += fmt.Sprintf("client 192.0.2.%d %d 1\n", 32+ordinal, now.Add(-time.Hour).Unix())
				} else {
					body += fmt.Sprintf("timeout %s %d 1\n", target, now.Add(-time.Minute).Unix())
				}
			}
			alerts, err := NewVPNSessionsSignal().Run(context.Background(), vpnSessionTestSettings(t, now, body))
			if err != nil || len(alerts) != 2 {
				t.Fatal("source-control fixture lost affected clients")
			}
			for _, alert := range alerts {
				if alert.Severity != SeverityPage || !strings.Contains(alert.Class, "vpn-site-") {
					t.Fatal("same-source positive central-path control was discarded")
				}
				if strings.Contains(alert.Mechanism, "while unrelated overlay controls remain healthy") || strings.Contains(alert.Mechanism, "can reach unrelated configured clients") {
					t.Fatal("same-source control acquired an unrelated-source witness")
				}
				want := "unrelated_reachable_controls=0"
				if controlGroup == 2 {
					want = "unrelated_reachable_controls=1"
				}
				if !strings.Contains(alert.Observed, want) || !strings.Contains(alert.Mechanism, "not by itself proof") {
					t.Fatal("source separation or its causal limitation was lost")
				}
				witness := "No reachable control in a different source group is established."
				if controlGroup == 2 {
					witness = "source separation is established without identifying a failed device"
				}
				if !strings.Contains(alert.Mechanism, witness) {
					t.Fatal("same-source and independent-source positive controls were conflated")
				}
			}
		}
	}
}

func TestVPNSessionsNoControlKeepsClientLossAndUnknownSource(t *testing.T) {
	now := time.Date(2035, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, recent := range []bool{false, true} {
		body := fmt.Sprintf("server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nreach 192.0.2.31 false\nreach 192.0.2.32 false\nreach 192.0.2.33 false\n", now.Unix())
		if recent {
			body += fmt.Sprintf("timeout node.example.test %d 1\ntimeout archive.example.test %d 1\n", now.Add(-time.Minute).Unix(), now.Add(-time.Minute).Unix())
		}
		alerts, err := NewVPNSessionsSignal().Run(context.Background(), vpnSessionTestSettings(t, now, body))
		if err != nil || len(alerts) != 3 {
			t.Fatal("missing control turned lost clients healthy")
		}
		for _, alert := range alerts {
			if alert.Severity != SeverityWarn || alert.Class != "vpn-client-session-loss" {
				t.Fatal("no positive control manufactured site attribution")
			}
		}
		if !recent && !strings.Contains(alerts[0].Observed, "unrelated_reachable_controls=unknown") {
			t.Fatal("missing source fabricated unrelated-control evidence")
		}
	}
}
