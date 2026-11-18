// Exercise the generated shell and ordinary public signal with synthetic ping
// exits only; no packet, service, privileged read, or external endpoint is used.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Replace only the reserved ping executable; all existing producer boundaries,
// status/journal fixtures, target logging and public reduction remain enabled.
func vpnPingExecutionFixture(t *testing.T, exits [3]int) *vpnProducerFixture {
	t.Helper()
	fixture := newVPNProducerFixture(t)
	command := "#!/bin/sh\ndirectory=${0%/*}\nfor address do :; done\nprintf '%s\\n' \"$address\" >> \"$directory/ping-log\"\ncase \"$address\" in\n"
	for index, address := range []string{"192.0.2.10", "192.0.2.20", "192.0.2.30"} {
		if exits[index] < 0 || exits[index] > 255 {
			t.Fatal("synthetic ping exit outside shell status domain")
		}
		command += fmt.Sprintf("%s) status=%d ;;\n", address, exits[index])
	}
	command += "*) exit 64 ;;\nesac\nif [ \"$status\" -gt 1 ]; then printf '%s\\n' synthetic-private-ping-diagnostic >&2; fi\nexit \"$status\"\n"
	if err := os.WriteFile(filepath.Join(fixture.directory, "ping"), []byte(command), 0755); err != nil {
		t.Fatal("could not prepare synthetic ping exit producer")
	}
	return fixture
}

// A successful status/journal census does not turn command execution failures
// into measured loss, nor does an unknown row disappear as healthy absence.
func TestVpnSessionsPingExecutionFailureIsUnknown(t *testing.T) {
	for _, status := range []int{127, 2, 126, 137} {
		fixture := vpnPingExecutionFixture(t, [3]int{status, status, status})
		alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
		if err != nil || len(alerts) != 3 {
			t.Fatalf("exit=%d: unobserved ICMP execution became loss or health", status)
		}
		for _, alert := range alerts {
			if alert.Class != "cannot-observe" || !isVisibilityAlert(alert) || alert.Severity != SeverityWarn || alert.Sustain != 2 || alert.SignalNumber != "21.1" || alert.SignalKey != "vpn-sessions" {
				t.Fatalf("exit=%d: execution unknown lost owning visibility policy", status)
			}
		}
		if strings.Count(fixture.output, " unknown\n") != 3 || strings.Contains(fixture.output, "synthetic-private-ping-diagnostic") || strings.Contains(alerts.Markdown(), "synthetic-private-ping-diagnostic") {
			t.Fatal("ping execution did not project only fixed unknown tokens")
		}
		fixture.requireCostAndPrivacy(t, alerts)
	}
}

// The supported no-reply exit stays a real failed-path observation; absent a
// reachable control, three losses cannot assert the shared-site PAGE criterion.
func TestVpnSessionsPingNoReplyRetainsClientLoss(t *testing.T) {
	fixture := vpnPingExecutionFixture(t, [3]int{1, 1, 1})
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil || len(alerts) != 3 {
		t.Fatal("qualified no-reply observations were discarded")
	}
	for _, alert := range alerts {
		if alert.Class != "vpn-client-data-path-loss" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
			t.Fatal("no-reply without a reachable control changed its loss policy")
		}
	}
	fixture.requireCostAndPrivacy(t, alerts)
}

// Reply success remains the ordinary complete healthy control.
func TestVpnSessionsPingReplyRemainsHealthy(t *testing.T) {
	fixture := vpnPingExecutionFixture(t, [3]int{0, 0, 0})
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil || len(alerts) != 0 || strings.Count(fixture.output, "reach ") != 3 {
		t.Fatal("supported reply control did not remain healthy and complete")
	}
	for _, address := range []string{"192.0.2.10", "192.0.2.20", "192.0.2.30"} {
		if !strings.Contains(fixture.output, "reach "+address+" true\n") {
			t.Fatal("supported reply did not reach the exact-client parser")
		}
	}
	fixture.requireCostAndPrivacy(t, alerts)
}

// Unknown cannot supply a second affected same-source peer. A measured sibling
// loss and independent reachable control retain their own authority.
func TestVpnSessionsPingUnknownPreservesMeasuredSibling(t *testing.T) {
	fixture := vpnPingExecutionFixture(t, [3]int{127, 1, 0})
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil || len(alerts) != 2 {
		t.Fatal("unknown execution erased an independent measured sibling")
	}
	requireVPNSessionAlert(t, alerts, "cannot-observe", "synthetic-alpha.example")
	loss := requireVPNSessionAlert(t, alerts, "vpn-client-data-path-loss", "synthetic-beta.example")
	if loss.Severity != SeverityWarn || !strings.Contains(loss.Observed, "reachable_controls=1") || !strings.Contains(loss.Observed, "shared_public_source=false") {
		t.Fatal("unknown sibling contributed unsupported site attribution")
	}
	fixture.requireCostAndPrivacy(t, alerts)
}

// An unknown control cannot upgrade two measured same-source failures to PAGE.
func TestVpnSessionsPingUnknownCannotSupplyReachableControl(t *testing.T) {
	fixture := vpnPingExecutionFixture(t, [3]int{1, 1, 127})
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil || len(alerts) != 3 {
		t.Fatal("unknown control changed measured loss cardinality")
	}
	for _, target := range []string{"synthetic-alpha.example", "synthetic-beta.example"} {
		alert := requireVPNSessionAlert(t, alerts, "vpn-client-data-path-loss", target)
		if alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, "reachable_controls=0") {
			t.Fatal("unobserved control supplied site PAGE authority")
		}
	}
	requireVPNSessionAlert(t, alerts, "cannot-observe", "synthetic-control.example")
	fixture.requireCostAndPrivacy(t, alerts)
}

// A genuine reply does retain the existing shared-source PAGE discriminator.
func TestVpnSessionsPingQualifiedControlRetainsSiteLoss(t *testing.T) {
	fixture := vpnPingExecutionFixture(t, [3]int{1, 1, 0})
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil || len(alerts) != 2 {
		t.Fatal("qualified site-loss observations were discarded")
	}
	for _, target := range []string{"synthetic-alpha.example", "synthetic-beta.example"} {
		alert := requireVPNSessionAlert(t, alerts, "vpn-site-data-path-loss", target)
		if alert.Severity != SeverityPage || alert.Sustain != 2 || !strings.Contains(alert.Observed, "unrelated_reachable_controls=1") {
			t.Fatal("supported reply/no-reply control lost existing site policy")
		}
	}
	fixture.requireCostAndPrivacy(t, alerts)
}

// Missing membership is independently proved by complete fresh status even
// when ping execution for that exact client is unknown.
func TestVpnSessionsPingUnknownPreservesAbsentSession(t *testing.T) {
	fixture := vpnPingExecutionFixture(t, [3]int{0, 127, 0})
	fixture.status = fixture.clientRow("192.0.2.10", "198.51.100.10") + fixture.clientRow("192.0.2.30", "203.0.113.30")
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil || len(alerts) != 2 {
		t.Fatal("ping unknown erased independently complete missing membership")
	}
	requireVPNSessionAlert(t, alerts, "cannot-observe", "synthetic-beta.example")
	alert := requireVPNSessionAlert(t, alerts, "vpn-client-session-loss", "synthetic-beta.example")
	if alert.Severity != SeverityWarn || alert.Sustain != 2 {
		t.Fatal("independent missing session changed policy")
	}
	fixture.requireCostAndPrivacy(t, alerts)
}

// Existing authoritative failed unit evidence still owns the stronger early
// boundary; optional ping uncertainty cannot replace that PAGE.
func TestVpnSessionsPingUnknownPreservesFailedUnit(t *testing.T) {
	fixture := vpnPingExecutionFixture(t, [3]int{127, 127, 127})
	fixture.active, fixture.sub = "failed", "failed"
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil || len(alerts) != 1 {
		t.Fatal("ping uncertainty replaced authoritative failed unit evidence")
	}
	alert := requireVPNSessionAlert(t, alerts, "vpn-server-unhealthy", "synthetic-vpn-server.example")
	if alert.Severity != SeverityPage || alert.Sustain != 1 {
		t.Fatal("failed unit PAGE changed under ping uncertainty")
	}
	fixture.requireCostAndPrivacy(t, alerts)
}

// Explicit unknown is accepted, but malformed, duplicate and missing rows
// remain refusals. Private malformed values never enter parser error text.
func TestVpnSessionsPingProjectionKeepsParserStrict(t *testing.T) {
	for _, mode := range []string{"malformed", "duplicate", "missing"} {
		fixture := vpnPingExecutionFixture(t, [3]int{127, 0, 0})
		settings := fixture.settings()
		settings.Source = &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
			out, err := fixture.Host(context.Background(), host, command)
			if err != nil {
				return out, err
			}
			switch mode {
			case "malformed":
				out = strings.Replace(out, "reach 192.0.2.10 unknown\n", "reach 192.0.2.10 synthetic-private-ping-value\n", 1)
			case "duplicate":
				out += "reach 192.0.2.10 true\n"
			case "missing":
				out = strings.Replace(out, "reach 192.0.2.10 unknown\n", "", 1)
			}
			return out, nil
		}}
		alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
		if err == nil || len(alerts) != 0 || strings.Contains(err.Error(), "synthetic-private-ping-value") {
			t.Fatalf("%s reachability projection bypassed strict refusal or privacy", mode)
		}
		fixture.requireCostAndPrivacy(t, alerts)
	}
}

// Denied clients remain absent from the actual command destinations and cannot
// contribute reachability or group attribution, even with synthetic exit data.
func TestVpnSessionsPingUnknownKeepsNestedAdmission(t *testing.T) {
	fixture := vpnPingExecutionFixture(t, [3]int{127, 1, 0})
	settings := fixture.settings()
	settings.ExcludedHosts = []string{"synthetic-beta.example"}
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 || fixture.calls != 1 {
		t.Fatal("unknown ping changed nested admission or owning command count")
	}
	requireVPNSessionAlert(t, alerts, "cannot-observe", "synthetic-alpha.example")
	requireVPNSessionAlert(t, alerts, "monitor-host-scope-partial", "monitor-host-scope")
	pingLog, err := os.ReadFile(filepath.Join(fixture.directory, "ping-log"))
	if err != nil || string(pingLog) != "192.0.2.10\n192.0.2.30\n" {
		t.Fatal("unknown projection contacted a denied nested client")
	}
}

// Ended parents cannot enter the generated command; live owners canceled by a
// completed source cannot publish either its unknown or measured findings.
func TestVpnSessionsPingUnknownKeepsCancellationLifecycle(t *testing.T) {
	fixture := vpnPingExecutionFixture(t, [3]int{127, 1, 0})
	settings := fixture.settings()
	settings.ExcludedHosts = []string{"synthetic-beta.example"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := NewVPNSessionsSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || fixture.calls != 0 {
		t.Fatal("ended owner entered ICMP observation")
	}
	for _, scoped := range []bool{false, true} {
		fixture := vpnPingExecutionFixture(t, [3]int{127, 1, 0})
		settings := fixture.settings()
		if scoped {
			settings.ExcludedHosts = []string{"synthetic-beta.example"}
		}
		ctx, cancel := context.WithCancel(context.Background())
		settings.Source = &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
			out, err := fixture.Host(ctx, host, command)
			cancel()
			return out, err
		}}
		alerts, err := NewVPNSessionsSignal().Run(ctx, settings)
		cancel()
		if !errors.Is(err, context.Canceled) || len(alerts) != 0 || fixture.calls != 1 {
			t.Fatal("completed source published ICMP findings after owner cancellation")
		}
	}
}

// Complete missing-session and recent-timeout evidence can still establish
// shared-source loss when the remaining admitted client supplies a real reply.
func TestVpnSessionsPingUnknownPreservesCorrelatedSessionLoss(t *testing.T) {
	fixture := vpnPingExecutionFixture(t, [3]int{127, 127, 0})
	fixture.status = fixture.clientRow("192.0.2.30", "203.0.113.30")
	for _, name := range []string{"synthetic-alpha.example", "synthetic-beta.example"} {
		fixture.journal += fmt.Sprintf("%d.000000 fixture ovpn[1]: %s/198.51.100.10:1400 Inactivity timeout (--ping-restart), restarting\n", fixture.now.Unix(), name)
	}
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil || len(alerts) != 4 {
		t.Fatal("ICMP uncertainty erased independently correlated missing sessions")
	}
	for _, target := range []string{"synthetic-alpha.example", "synthetic-beta.example"} {
		requireVPNSessionAlert(t, alerts, "cannot-observe", target)
		alert := requireVPNSessionAlert(t, alerts, "vpn-site-session-loss", target)
		if alert.Severity != SeverityPage || alert.Sustain != 2 || !strings.Contains(alert.Observed, "unrelated_reachable_controls=1") {
			t.Fatal("complete absence/history and real control lost site policy")
		}
	}
	fixture.requireCostAndPrivacy(t, alerts)
}
