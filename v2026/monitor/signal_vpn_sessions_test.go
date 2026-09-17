package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestVPNSessionsSignalSyntheticHealthy(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	settings := vpnSessionTestSettings(t, now, fmt.Sprintf(
		"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 192.0.2.31 %d 1\nclient 192.0.2.32 %d 2\nclient 192.0.2.33 %d 2\nreach 192.0.2.31 true\nreach 192.0.2.32 true\nreach 192.0.2.33 true\n",
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
		"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 192.0.2.31 %d 1\nreach 192.0.2.31 true\nreach 192.0.2.32 false\nreach 192.0.2.33 false\ntimeout archive.example.test %d 1\ntimeout node.example.test %d 1\n",
		now.Add(-4*time.Second).Unix(), now.Add(-2*time.Hour).Unix(), now.Add(-14*time.Minute).Unix(), now.Add(-6*time.Minute).Unix(),
	))
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("shared-site alerts=%d, want 2: %+v", len(alerts), alerts)
	}
	for _, target := range []string{"archive.example.test", "node.example.test"} {
		alert := requireVPNSessionAlert(t, alerts, "vpn-site-session-loss", target)
		if alert.SignalNumber != "21.1" || alert.SignalKey != "vpn-sessions" || alert.Severity != SeverityPage || alert.Sustain != 2 {
			t.Fatalf("wrong shared-site identity: %+v", alert)
		}
		for _, want := range []string{
			"shared_public_source=true",
			"correlated_affected_hosts=archive.example.test,node.example.test",
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
		"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 192.0.2.31 %d 1\nclient 192.0.2.33 %d 2\nreach 192.0.2.31 true\nreach 192.0.2.32 false\nreach 192.0.2.33 true\ntimeout node.example.test %d 2\n",
		now.Add(-5*time.Second).Unix(), now.Add(-time.Hour).Unix(), now.Add(-time.Hour).Unix(), now.Add(-3*time.Minute).Unix(),
	))
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("isolated alerts=%d, want 1: %+v", len(alerts), alerts)
	}
	alert := requireVPNSessionAlert(t, alerts, "vpn-client-session-loss", "node.example.test")
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
		"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 192.0.2.31 %d 1\nclient 192.0.2.32 %d 2\nclient 192.0.2.33 %d 2\nreach 192.0.2.31 true\nreach 192.0.2.32 false\nreach 192.0.2.33 false\n",
		now.Add(-5*time.Second).Unix(), now.Add(-time.Hour).Unix(), now.Add(-2*time.Minute).Unix(), now.Add(-2*time.Minute).Unix(),
	))
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("shared-site data-path alerts=%d, want 2: %+v", len(alerts), alerts)
	}
	for _, target := range []string{"archive.example.test", "node.example.test"} {
		alert := requireVPNSessionAlert(t, alerts, "vpn-site-data-path-loss", target)
		if alert.Severity != SeverityPage || alert.Frame != "shared-public-source-data-path" || alert.Sustain != 2 {
			t.Fatalf("wrong data-path identity for %s: %+v", target, alert)
		}
		for _, want := range []string{
			"session_present=true",
			"data_path_reachable=false",
			"reachable_controls=1",
			"correlated_affected_hosts=archive.example.test,node.example.test",
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

func TestVPNSessionsSignalSyntheticCorrelatesMixedSessionStates(t *testing.T) {
	now := time.Date(2026, 9, 3, 9, 15, 0, 0, time.UTC)
	settings := vpnSessionTestSettings(t, now, fmt.Sprintf(
		"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 192.0.2.31 %d 1\nclient 192.0.2.33 %d 2\nreach 192.0.2.31 true\nreach 192.0.2.32 false\nreach 192.0.2.33 false\ntimeout node.example.test %d 2\n",
		now.Add(-5*time.Second).Unix(), now.Add(-time.Hour).Unix(), now.Add(-2*time.Minute).Unix(), now.Add(-time.Minute).Unix(),
	))
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("mixed-state alerts=%d, want 2: %+v", len(alerts), alerts)
	}
	archiveAlert := requireVPNSessionAlert(t, alerts, "vpn-site-data-path-loss", "archive.example.test")
	nodeAlert := requireVPNSessionAlert(t, alerts, "vpn-site-session-loss", "node.example.test")
	for _, alert := range []Alert{archiveAlert, nodeAlert} {
		markdown := alert.Markdown()
		if alert.Severity != SeverityPage || !strings.Contains(markdown, "correlated_affected_hosts=archive.example.test,node.example.test") || !strings.Contains(markdown, "current/recent public source") {
			t.Fatalf("mixed-state alert lost shared-site attribution: %+v\n%s", alert, markdown)
		}
	}
}

func TestVPNSessionsSignalSyntheticRequiresEveryReachabilityResult(t *testing.T) {
	now := time.Date(2026, 9, 3, 9, 5, 0, 0, time.UTC)
	settings := vpnSessionTestSettings(t, now, fmt.Sprintf(
		"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 192.0.2.31 %d 1\nreach 192.0.2.31 true\nreach 192.0.2.32 false\n",
		now.Add(-5*time.Second).Unix(), now.Add(-time.Hour).Unix(),
	))
	if _, err := NewVPNSessionsSignal().Run(context.Background(), settings); err == nil || !strings.Contains(err.Error(), "missing reachability for archive.example.test") {
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
				"server_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\nclient 192.0.2.31 %d 1\nclient 192.0.2.32 %d 2\nclient 192.0.2.33 %d 2\nreach 192.0.2.31 true\nreach 192.0.2.32 true\nreach 192.0.2.33 true\n",
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

func TestVPNSessionsCommandPreservesCombinedSourceGrouping(t *testing.T) {
	command, err := vpnSessionsCommand([]*host{
		{name: "control.example.test", overlayIp: "192.0.2.31"},
		{name: "archive.example.test", overlayIp: "192.0.2.33"},
		{name: "node.example.test", overlayIp: "192.0.2.32"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"printf '%s\\n' '---monitor-journal---'",
		"} | awk -v wanted_names=",
		"if (!(source in source_group)) source_group[source]=++source_groups",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("generated command lost %q:\n%s", want, command)
		}
	}
	if strings.Contains(command, "%!s(MISSING)") {
		t.Fatalf("generated command has an unexpanded Go format error:\n%s", command)
	}
	check := exec.Command("sh", "-n")
	check.Stdin = strings.NewReader(command)
	if output, err := check.CombinedOutput(); err != nil {
		t.Fatalf("generated command is not valid POSIX shell: %v\n%s", err, output)
	}

	binDir := t.TempDir()
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable("systemctl", `#!/bin/sh
case "$*" in
  *ActiveState*) printf '%s\n' active ;;
  *SubState*) printf '%s\n' running ;;
  *NRestarts*) printf '%s\n' 0 ;;
  *) exit 64 ;;
esac
`)
	writeExecutable("sudo", `#!/bin/sh
if [ "$1" = -n ]; then shift; fi
case "$1" in
  test) exit 0 ;;
  stat) printf '%s\n' "$VPN_TEST_MTIME" ;;
  cat) printf '%s' "$VPN_TEST_STATUS" ;;
  journalctl) printf '%s' "$VPN_TEST_JOURNAL" ;;
  *) exit 64 ;;
esac
`)
	writeExecutable("ping", `#!/bin/sh
for address do :; done
[ "$address" = 192.0.2.31 ]
`)
	execute := exec.Command("sh", "-c", command)
	execute.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"VPN_TEST_MTIME=1788427200",
		"VPN_TEST_STATUS="+
			"CLIENT_LIST,control.example.test,198.51.100.8:1200,192.0.2.31,x,x,x,x,1788420000\n"+
			"CLIENT_LIST,archive.example.test,203.0.113.9:1300,192.0.2.33,x,x,x,x,1788427109\n",
		"VPN_TEST_JOURNAL=1788427110.000000 host ovpn[1]: node.example.test/203.0.113.9:1400 Inactivity timeout (--ping-restart), restarting\n",
	)
	output, err := execute.CombinedOutput()
	if err != nil {
		t.Fatalf("execute generated command: %v\n%s", err, output)
	}
	observation, err := parseVPNSessionsObservation(string(output))
	if err != nil {
		t.Fatalf("parse generated command output: %v\n%s", err, output)
	}
	archiveGroup := observation.clients["192.0.2.33"].group
	nodeGroup := observation.timeouts["node.example.test"].group
	if archiveGroup <= 0 || nodeGroup != archiveGroup {
		t.Fatalf("current and timed-out clients with one source got different groups: archive=%d node=%d\n%s", archiveGroup, nodeGroup, output)
	}
	if edgeGroup := observation.clients["192.0.2.31"].group; edgeGroup == archiveGroup {
		t.Fatalf("different sources got one group: edge=%d archive=%d\n%s", edgeGroup, archiveGroup, output)
	}
}

func vpnSessionTestSettings(t *testing.T, now time.Time, output string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "server.example.test" || host.SSHUser != "ubuntu" || len(host.SSHKeyPaths) != 1 || host.SSHKeyPaths[0] != "/keys/vpn" {
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
		{Name: "server.example.test", OverlayAddress: "192.0.2.1", Roles: []string{"vpn-server"}, SSHUser: "ubuntu", SSHKeyPaths: []string{"/keys/vpn"}},
		{Name: "control.example.test", OverlayAddress: "192.0.2.31", Roles: []string{"vpn-client"}},
		{Name: "archive.example.test", OverlayAddress: "192.0.2.33", Roles: []string{"vpn-client"}},
		{Name: "node.example.test", OverlayAddress: "192.0.2.32", Roles: []string{"vpn-client"}},
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

func TestVPNSessionsSignalHostScopeStopsNestedClientContact(t *testing.T) {
	settings, pingLog, shellCalls := vpnSessionScopeTestSettings(t, true)
	scoped, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), scoped)
	if err != nil {
		t.Fatal(err)
	}
	if *shellCalls != 1 || vpnSessionScopePingAttempts(t, pingLog) != "192.0.2.10\n" {
		t.Fatalf("nested host contact escaped policy: calls=%d attempts=%q", *shellCalls, vpnSessionScopePingAttempts(t, pingLog))
	}
	if len(settings.Hosts) != 3 || len(scoped.Hosts) != 3 || len(alerts) != 1 {
		t.Fatalf("desired inventory or partial coverage changed: configured=%d scoped=%d alerts=%+v", len(settings.Hosts), len(scoped.Hosts), alerts)
	}
	alert := requireAlertClass(t, alerts, "monitor-host-scope-partial")
	if alert.SignalNumber != "21.1" || alert.SignalKey != "vpn-sessions" || alert.Severity != SeverityWarn {
		t.Fatalf("scope lost originating probe or unknown state: %+v", alert)
	}
	for _, value := range []string{"configured_hosts=3", "excluded_hosts=1", "blocked_hosts=1", "desired_topology_unchanged=true", "SIGNALS.md §1.6"} {
		if !strings.Contains(alert.Markdown(), value) {
			t.Errorf("partial-coverage Markdown lacks %q", value)
		}
	}
	for _, value := range []string{"excluded.example.test", "192.0.2.20", "198.51.100.20", "synthetic-vpn-secret"} {
		if strings.Contains(alert.Markdown(), value) {
			t.Errorf("partial-coverage Markdown leaked a private fixture value")
		}
	}
}

func TestVPNSessionsSignalHostScopePreservesAllowedClientFailure(t *testing.T) {
	settings, pingLog, _ := vpnSessionScopeTestSettings(t, false)
	scoped, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), scoped)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 || vpnSessionScopePingAttempts(t, pingLog) != "192.0.2.10\n" {
		t.Fatalf("permitted-client evidence lost: alerts=%+v attempts=%q", alerts, vpnSessionScopePingAttempts(t, pingLog))
	}
	alert := requireVPNSessionAlert(t, alerts, "vpn-client-session-loss", "allowed.example.test")
	if alert.Severity != SeverityWarn || !strings.Contains(alert.Markdown(), "configured_clients=2") || strings.Contains(alert.Markdown(), "excluded.example.test") {
		t.Fatalf("permitted-client finding shrank its desired denominator or attributed a paused peer: %+v", alert)
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
}

func TestVPNSessionsSignalHostScopeRejectsSharedClientEndpoint(t *testing.T) {
	settings, pingLog, _ := vpnSessionScopeTestSettings(t, true)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "shared.example.test", OverlayAddress: "192.0.2.20", Roles: []string{"vpn-client"}})
	scoped, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), scoped)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || vpnSessionScopePingAttempts(t, pingLog) != "192.0.2.10\n" || len(scoped.Hosts) != 4 {
		t.Fatalf("ambiguous paused endpoint reached transport or attribution: alerts=%+v attempts=%q", alerts, vpnSessionScopePingAttempts(t, pingLog))
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
}

func TestVPNSessionsSignalHostScopeRejectsNormalizedSharedEndpoint(t *testing.T) {
	settings, pingLog, _ := vpnSessionScopeTestSettings(t, true)
	settings.Hosts[2].OverlayAddress = " 192.0.2.20 "
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "shared.example.test", OverlayAddress: "192.0.2.20", Roles: []string{"vpn-client"}})
	scoped, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), scoped)
	if err != nil || len(alerts) != 1 || vpnSessionScopePingAttempts(t, pingLog) != "192.0.2.10\n" {
		t.Fatalf("normalized shared target escaped admission: alerts=%+v err=%v attempts=%q", alerts, err, vpnSessionScopePingAttempts(t, pingLog))
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
}

func TestVPNSessionsSignalHostScopeNoPolicyKeepsAllClients(t *testing.T) {
	settings, pingLog, shellCalls := vpnSessionScopeTestSettings(t, true)
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || *shellCalls != 1 || vpnSessionScopePingAttempts(t, pingLog) != "192.0.2.10\n192.0.2.20\n" {
		t.Fatalf("unscoped VPN behavior changed: alerts=%+v err=%v calls=%d attempts=%q", alerts, err, *shellCalls, vpnSessionScopePingAttempts(t, pingLog))
	}
}

func TestVPNSessionsSignalHostScopeRetainsCentralServerFailure(t *testing.T) {
	settings, pingLog, _ := vpnSessionScopeTestSettings(t, true)
	source := settings.Source.(*syntheticSource)
	inspect := source.hostFn
	source.hostFn = func(configured HostSettings, command string) (string, error) {
		output, err := inspect(configured, command)
		output = strings.Replace(output, "server_active_state active", "server_active_state failed", 1)
		return output, err
	}
	scoped, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), scoped)
	if err != nil || len(alerts) != 2 || vpnSessionScopePingAttempts(t, pingLog) != "192.0.2.10\n" {
		t.Fatalf("central server evidence was lost: alerts=%+v err=%v", alerts, err)
	}
	alert := requireVPNSessionAlert(t, alerts, "vpn-server-unhealthy", "server.example.test")
	if alert.Severity != SeverityPage {
		t.Fatalf("central server failure lost severity: %+v", alert)
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
}

func TestVPNSessionsSignalHostScopeAllClientsPausedStaysUnknown(t *testing.T) {
	settings, pingLog, shellCalls := vpnSessionScopeTestSettings(t, true)
	scoped, err := ExcludeHosts(settings, "allowed.example.test", "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), scoped)
	if err != nil || len(alerts) != 1 || *shellCalls != 1 || vpnSessionScopePingAttempts(t, pingLog) != "" || len(scoped.Hosts) != 3 {
		t.Fatalf("paused clients became outage/healthy evidence or prevented central observation: alerts=%+v err=%v calls=%d", alerts, err, *shellCalls)
	}
	alert := requireAlertClass(t, alerts, "monitor-host-scope-partial")
	if !strings.Contains(alert.Markdown(), "excluded_hosts=2 blocked_hosts=2") {
		t.Fatalf("all-paused coverage lacks exact counts: %+v", alert)
	}
}

func TestVPNSessionsSignalHostScopeDoesNotContactExcludedServer(t *testing.T) {
	settings, pingLog, shellCalls := vpnSessionScopeTestSettings(t, true)
	scoped, err := ExcludeHosts(settings, "server.example.test")
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), scoped)
	if err != nil || len(alerts) != 1 || *shellCalls != 0 || vpnSessionScopePingAttempts(t, pingLog) != "" {
		t.Fatalf("excluded server reached transport or became outage evidence: alerts=%+v err=%v calls=%d", alerts, err, *shellCalls)
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
}

func TestVPNSessionsSignalHostScopeCancellationDoesNotContactOrAlert(t *testing.T) {
	settings, pingLog, shellCalls := vpnSessionScopeTestSettings(t, true)
	scoped, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := NewVPNSessionsSignal().Run(ctx, scoped)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || *shellCalls != 0 || vpnSessionScopePingAttempts(t, pingLog) != "" {
		t.Fatalf("cancellation contacted source or became policy evidence: alerts=%+v err=%v calls=%d", alerts, err, *shellCalls)
	}
}

func vpnSessionScopeTestSettings(t *testing.T, allowedPresent bool) (SignalSettings, string, *int) {
	t.Helper()
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	binDir := t.TempDir()
	pingLog := filepath.Join(binDir, "ping.log")
	commands := map[string]string{
		"systemctl": `#!/bin/sh
case "$*" in
  *ActiveState*) printf '%s\n' active ;;
  *SubState*) printf '%s\n' running ;;
  *NRestarts*) printf '%s\n' 0 ;;
  *) exit 64 ;;
esac
`,
		"sudo": `#!/bin/sh
if [ "$1" = -n ]; then shift; fi
case "$1" in
  test) exit 0 ;;
  stat) printf '%s\n' "$VPN_TEST_MTIME" ;;
  cat) printf '%s' "$VPN_TEST_STATUS" ;;
  journalctl) printf '%s' "$VPN_TEST_JOURNAL" ;;
  *) exit 64 ;;
esac
`,
		"ping": `#!/bin/sh
for address do :; done
printf '%s\n' "$address" >> "$VPN_TEST_PING_LOG"
exit 0
`,
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	status := fmt.Sprintf("CLIENT_LIST,excluded.example.test,198.51.100.20:1200,192.0.2.20,x,x,x,x,%d\n", now.Add(-time.Hour).Unix())
	if allowedPresent {
		status += fmt.Sprintf("CLIENT_LIST,allowed.example.test,198.51.100.10:1300,192.0.2.10,x,x,x,x,%d\n", now.Add(-time.Hour).Unix())
	}
	shellCalls := new(int)
	source := &syntheticSource{hostFn: func(configured HostSettings, command string) (string, error) {
		*shellCalls++
		if configured.Name != "server.example.test" {
			return "", fmt.Errorf("unexpected synthetic VPN server")
		}
		execute := exec.Command("sh", "-c", command)
		execute.Env = append(os.Environ(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"VPN_TEST_MTIME="+strconv.FormatInt(now.Add(-5*time.Second).Unix(), 10),
			"VPN_TEST_STATUS="+status,
			"VPN_TEST_JOURNAL="+fmt.Sprintf("%d.000000 fixture ovpn[1]: excluded.example.test/198.51.100.20:1400 Inactivity timeout (--ping-restart), restarting synthetic-vpn-secret\n", now.Add(-time.Minute).Unix()),
			"VPN_TEST_PING_LOG="+pingLog,
		)
		output, err := execute.CombinedOutput()
		return string(output), err
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = []HostSettings{
		{Name: "server.example.test", OverlayAddress: "192.0.2.1", Roles: []string{"vpn-server"}},
		{Name: "allowed.example.test", OverlayAddress: "192.0.2.10", Roles: []string{"vpn-client"}},
		{Name: "excluded.example.test", OverlayAddress: "192.0.2.20", Roles: []string{"vpn-client"}},
	}
	return settings, pingLog, shellCalls
}

func vpnSessionScopePingAttempts(t *testing.T, pingLog string) string {
	t.Helper()
	data, err := os.ReadFile(pingLog)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
