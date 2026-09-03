package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestVPNSessionsSignalSyntheticHealthy(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	settings := vpnSessionTestSettings(t, now, fmt.Sprintf(
		"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 172.28.208.173 %d 1\nclient 172.28.208.185 %d 2\nclient 172.28.208.187 %d 2\nreach 172.28.208.173 true\nreach 172.28.208.185 true\nreach 172.28.208.187 true\n",
		now.Add(-5*time.Second).Unix(), now.Add(-time.Hour).Unix(), now.Add(-time.Hour).Unix(), now.Add(-time.Hour).Unix(),
	))
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy VPN sessions alerted: %+v", alerts)
	}
}

func TestVPNSessionsSignalSyntheticSharedSiteLoss(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 20, 0, 0, time.UTC)
	settings := vpnSessionTestSettings(t, now, fmt.Sprintf(
		"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 172.28.208.173 %d 1\nreach 172.28.208.173 true\nreach 172.28.208.185 false\nreach 172.28.208.187 false\ntimeout planetoid %d 1\ntimeout snow %d 1\n",
		now.Add(-4*time.Second).Unix(), now.Add(-2*time.Hour).Unix(), now.Add(-14*time.Minute).Unix(), now.Add(-6*time.Minute).Unix(),
	))
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("shared-site alerts=%d, want 2: %+v", len(alerts), alerts)
	}
	for _, target := range []string{"planetoid", "snow"} {
		alert := requireVPNSessionAlert(t, alerts, "vpn-site-session-loss", target)
		if alert.SignalNumber != "21.1" || alert.SignalKey != "vpn-sessions" || alert.Severity != SeverityPage || alert.Sustain != 2 {
			t.Fatalf("wrong shared-site identity: %+v", alert)
		}
		for _, want := range []string{
			"shared_public_source=true",
			"correlated_missing_hosts=planetoid,snow",
			"offsite LAN, router/NAT, WAN",
			"source-address equality",
			"public source itself is never emitted",
			"dedicated direct-path control",
			"Bulk backups must never move onto the management VPN",
			"Preserve advancing Subtensor databases",
			"SIGNALS.md §21.1",
		} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Errorf("%s alert missing %q:\n%s", target, want, alert.Markdown())
			}
		}
	}
}

func TestVPNSessionsSignalSyntheticIsolatedLoss(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 30, 0, 0, time.UTC)
	settings := vpnSessionTestSettings(t, now, fmt.Sprintf(
		"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 172.28.208.173 %d 1\nclient 172.28.208.187 %d 2\nreach 172.28.208.173 true\nreach 172.28.208.185 false\nreach 172.28.208.187 true\ntimeout snow %d 2\n",
		now.Add(-5*time.Second).Unix(), now.Add(-time.Hour).Unix(), now.Add(-time.Hour).Unix(), now.Add(-3*time.Minute).Unix(),
	))
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("isolated alerts=%d, want 1: %+v", len(alerts), alerts)
	}
	alert := requireVPNSessionAlert(t, alerts, "vpn-client-session-loss", "snow")
	if alert.Severity != SeverityWarn || alert.Frame != "isolated-or-unknown-source" {
		t.Fatalf("isolated loss identity: %+v", alert)
	}
	if !strings.Contains(alert.Markdown(), "shared_public_source=false") ||
		!strings.Contains(alert.Markdown(), "this client, its host, or the route/NAT path") {
		t.Fatalf("isolated loss lacks bounded attribution:\n%s", alert.Markdown())
	}
}

func TestVPNSessionsSignalSyntheticSharedSiteDataPathLoss(t *testing.T) {
	now := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	settings := vpnSessionTestSettings(t, now, fmt.Sprintf(
		"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 172.28.208.173 %d 1\nclient 172.28.208.185 %d 2\nclient 172.28.208.187 %d 2\nreach 172.28.208.173 true\nreach 172.28.208.185 false\nreach 172.28.208.187 false\n",
		now.Add(-5*time.Second).Unix(), now.Add(-time.Hour).Unix(), now.Add(-2*time.Minute).Unix(), now.Add(-2*time.Minute).Unix(),
	))
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("shared-site data-path alerts=%d, want 2: %+v", len(alerts), alerts)
	}
	for _, target := range []string{"planetoid", "snow"} {
		alert := requireVPNSessionAlert(t, alerts, "vpn-site-data-path-loss", target)
		if alert.Severity != SeverityPage || alert.Frame != "shared-public-source-data-path" || alert.Sustain != 2 {
			t.Fatalf("wrong data-path identity for %s: %+v", target, alert)
		}
		for _, want := range []string{
			"session_present=true",
			"data_path_reachable=false",
			"reachable_controls=1",
			"correlated_unreachable_hosts=planetoid,snow",
			"CLIENT_LIST row proves a control session, not usable forwarding",
			"never emits public sources",
			"same-source configured peers recover",
		} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Errorf("%s data-path alert missing %q:\n%s", target, want, alert.Markdown())
			}
		}
	}
}

func TestVPNSessionsSignalSyntheticRequiresEveryReachabilityResult(t *testing.T) {
	now := time.Date(2026, 9, 3, 9, 5, 0, 0, time.UTC)
	settings := vpnSessionTestSettings(t, now, fmt.Sprintf(
		"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 172.28.208.173 %d 1\nreach 172.28.208.173 true\nreach 172.28.208.185 false\n",
		now.Add(-5*time.Second).Unix(), now.Add(-time.Hour).Unix(),
	))
	if _, err := NewVPNSessionsSignal().Run(context.Background(), settings); err == nil || !strings.Contains(err.Error(), "missing reachability for planetoid") {
		t.Fatalf("incomplete reachability error=%v", err)
	}
}

func TestVPNSessionsSignalSyntheticServerAndStatusFailures(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 40, 0, 0, time.UTC)
	tests := []struct {
		name     string
		output   string
		class    string
		severity Severity
	}{
		{
			name:   "server stopped",
			output: fmt.Sprintf("server_active_state failed\nserver_sub_state failed\nserver_restarts 1\nstatus_mtime_epoch %d\n", now.Add(-time.Minute).Unix()),
			class:  "vpn-server-unhealthy", severity: SeverityPage,
		},
		{
			name:   "stale status",
			output: fmt.Sprintf("server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\n", now.Add(-2*time.Minute).Unix()),
			class:  "vpn-status-stale", severity: SeverityWarn,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alerts, err := NewVPNSessionsSignal().Run(context.Background(), vpnSessionTestSettings(t, now, test.output))
			if err != nil {
				t.Fatal(err)
			}
			if len(alerts) != 1 || alerts[0].Class != test.class || alerts[0].Severity != test.severity {
				t.Fatalf("alerts=%+v, want one %s/%s", alerts, test.severity, test.class)
			}
		})
	}
}

func TestVPNSessionsSignalSyntheticStatusFreshnessBoundary(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 45, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		age        time.Duration
		wantAlerts int
	}{
		{name: "default interval plus tolerance", age: 90 * time.Second, wantAlerts: 0},
		{name: "past tolerance", age: 91 * time.Second, wantAlerts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := fmt.Sprintf(
				"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 172.28.208.173 %d 1\nclient 172.28.208.185 %d 2\nclient 172.28.208.187 %d 2\nreach 172.28.208.173 true\nreach 172.28.208.185 true\nreach 172.28.208.187 true\n",
				now.Add(-test.age).Unix(), now.Add(-time.Hour).Unix(), now.Add(-time.Hour).Unix(), now.Add(-time.Hour).Unix(),
			)
			alerts, err := NewVPNSessionsSignal().Run(context.Background(), vpnSessionTestSettings(t, now, output))
			if err != nil {
				t.Fatal(err)
			}
			if len(alerts) != test.wantAlerts {
				t.Fatalf("alerts=%+v, want %d", alerts, test.wantAlerts)
			}
			if test.wantAlerts == 1 && alerts[0].Class != "vpn-status-stale" {
				t.Fatalf("alert=%+v, want vpn-status-stale", alerts[0])
			}
		})
	}
}

func TestVPNSessionsSignalSyntheticRejectsMalformedStatus(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 50, 0, 0, time.UTC)
	settings := vpnSessionTestSettings(t, now,
		"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch nope\n")
	if _, err := NewVPNSessionsSignal().Run(context.Background(), settings); err == nil || !strings.Contains(err.Error(), "invalid status_mtime_epoch") {
		t.Fatalf("malformed status error=%v", err)
	}
}

func TestVPNSessionsSignalNoopsWithoutInventory(t *testing.T) {
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), syntheticSettings(&syntheticSource{}))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("unconfigured VPN signal alerts=%+v err=%v", alerts, err)
	}
}

func vpnSessionTestSettings(t *testing.T, now time.Time, output string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "vpn-0" || host.SSHUser != "ubuntu" || len(host.SSHKeyPaths) != 1 || host.SSHKeyPaths[0] != "/keys/vpn" {
			return "", fmt.Errorf("unexpected VPN host settings: %+v", host)
		}
		for _, want := range []string{vpnSessionsMarker, "openvpn-status.log", "source_group", "expected_addresses", "ping -n -c 1"} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("VPN command missing %q", want)
			}
		}
		return output, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = []HostSettings{
		{Name: "vpn-0", OverlayAddress: "172.28.208.1", Roles: []string{"vpn-server"}, SSHUser: "ubuntu", SSHKeyPaths: []string{"/keys/vpn"}},
		{Name: "edge-0", OverlayAddress: "172.28.208.173", Roles: []string{"vpn-client"}},
		{Name: "planetoid", OverlayAddress: "172.28.208.187", Roles: []string{"vpn-client"}},
		{Name: "snow", OverlayAddress: "172.28.208.185", Roles: []string{"vpn-client"}},
	}
	return settings
}

func requireVPNSessionAlert(t *testing.T, alerts Alerts, class, target string) Alert {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == class && alert.Target == target {
			return alert
		}
	}
	t.Fatalf("missing %s alert for %s: %+v", class, target, alerts)
	return Alert{}
}
