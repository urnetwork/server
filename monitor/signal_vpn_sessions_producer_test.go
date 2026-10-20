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

// Only the actual generated command executes. All commands which would inspect
// a service, read privileged data or send a packet resolve to these synthetic
// executables. Real shell/cat/awk behavior is deliberately retained.
type vpnProducerFixture struct {
	*syntheticSource
	t           *testing.T
	directory   string
	now         time.Time
	mtime       time.Time
	status      string
	journal     string
	statusExit  int
	journalExit int
	active      string
	sub         string
	unreachable string
	calls       int
	output      string
	shell       string
	pipefail    bool
	diagnostics bool
}

func newVPNProducerFixture(t *testing.T) *vpnProducerFixture {
	t.Helper()
	now := time.Date(2035, 2, 3, 4, 5, 6, 0, time.UTC)
	fixture := &vpnProducerFixture{
		syntheticSource: &syntheticSource{}, t: t, directory: t.TempDir(),
		now: now, mtime: now.Add(-5 * time.Second), active: "active", sub: "running", shell: "/bin/sh",
	}
	fixture.status = fixture.completeStatus()
	commands := map[string]string{
		"systemctl": `#!/bin/sh
directory=${0%/*}
case "$*" in
  *ActiveState*) /bin/cat "$directory/active" ;;
  *SubState*) /bin/cat "$directory/sub" ;;
  *NRestarts*) printf '0\n' ;;
  *) exit 64 ;;
esac
`,
		"sudo": `#!/bin/sh
directory=${0%/*}
if [ "$1" = -n ]; then shift; fi
case "$1" in
  test) exit 0 ;;
  stat) printf '%s\n' "$VPN_PRODUCER_MTIME" ;;
  cat)
    /bin/cat "$directory/status-output" || exit 64
    status_exit=$(/bin/cat "$directory/status-exit") || exit 64
    printf 'status_exit=%s\n' "$status_exit" >> "$directory/producer-log"
    if [ -s "$directory/status-diagnostic" ]; then /bin/cat "$directory/status-diagnostic" >&2; fi
    exit "$status_exit"
    ;;
  journalctl)
    /bin/cat "$directory/journal-output" || exit 64
    journal_exit=$(/bin/cat "$directory/journal-exit") || exit 64
    printf 'journal_exit=%s\n' "$journal_exit" >> "$directory/producer-log"
    if [ -s "$directory/journal-diagnostic" ]; then /bin/cat "$directory/journal-diagnostic" >&2; fi
    exit "$journal_exit"
    ;;
  *) exit 64 ;;
esac
`,
		"ping": `#!/bin/sh
directory=${0%/*}
for address do :; done
printf '%s\n' "$address" >> "$directory/ping-log"
[ "$address" != "$(/bin/cat "$directory/unreachable")" ]
`,
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(fixture.directory, name), []byte(body), 0755); err != nil {
			t.Fatal("could not prepare synthetic VPN command")
		}
	}
	return fixture
}

func (self *vpnProducerFixture) clientRow(address, source string) string {
	return fmt.Sprintf("CLIENT_LIST,synthetic-private-client,%s:1200,%s,x,x,x,x,%d\n", source, address, self.now.Add(-time.Hour).Unix())
}

func (self *vpnProducerFixture) completeStatus() string {
	return self.clientRow("192.0.2.10", "198.51.100.10") +
		self.clientRow("192.0.2.20", "198.51.100.10") +
		self.clientRow("192.0.2.30", "203.0.113.30")
}

func (self *vpnProducerFixture) settings() SignalSettings {
	self.t.Helper()
	statusDiagnostic, journalDiagnostic := "", ""
	if self.diagnostics {
		statusDiagnostic, journalDiagnostic = "synthetic-private-status-diagnostic\n", "synthetic-private-journal-diagnostic\n"
	}
	for name, value := range map[string]string{
		"status-diagnostic": statusDiagnostic, "journal-diagnostic": journalDiagnostic,
		"status-output": self.status, "journal-output": self.journal,
		"status-exit": strconv.Itoa(self.statusExit), "journal-exit": strconv.Itoa(self.journalExit),
		"active": self.active, "sub": self.sub, "unreachable": self.unreachable,
	} {
		if err := os.WriteFile(filepath.Join(self.directory, name), []byte(value), 0600); err != nil {
			self.t.Fatal("could not prepare synthetic VPN producer data")
		}
	}
	settings := syntheticSettings(self)
	settings.Now = func() time.Time { return self.now }
	settings.Hosts = []HostSettings{
		{Name: "synthetic-vpn-server.example", OverlayAddress: "192.0.2.1", Roles: []string{"vpn-server"}},
		{Name: "synthetic-alpha.example", OverlayAddress: "192.0.2.10", Roles: []string{"vpn-client"}},
		{Name: "synthetic-beta.example", OverlayAddress: "192.0.2.20", Roles: []string{"vpn-client"}},
		{Name: "synthetic-control.example", OverlayAddress: "192.0.2.30", Roles: []string{"vpn-client"}},
	}
	return settings
}

func (self *vpnProducerFixture) Host(ctx context.Context, host HostSettings, command string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if host.Name != "synthetic-vpn-server.example" || !strings.Contains(command, vpnSessionsMarker) {
		self.t.Fatal("VPN producer fixture received an unexpected source command")
	}
	self.calls++
	args := []string{"-c", command}
	if self.pipefail {
		args = []string{"-o", "pipefail", "-c", command}
	}
	execute := exec.CommandContext(ctx, self.shell, args...)
	execute.Env = []string{
		"PATH=" + self.directory + ":/usr/bin:/bin", "LANG=C", "LC_ALL=C",
		"VPN_PRODUCER_MTIME=" + strconv.FormatInt(self.mtime.Unix(), 10),
	}
	output, err := execute.CombinedOutput()
	self.output = string(output)
	if ctx.Err() != nil {
		return self.output, ctx.Err()
	}
	return self.output, err
}

func (self *vpnProducerFixture) requireCostAndPrivacy(t *testing.T, alerts Alerts) {
	t.Helper()
	if self.calls != 1 {
		t.Fatal("VPN producer completeness changed the single server command boundary")
	}
	producerLog, err := os.ReadFile(filepath.Join(self.directory, "producer-log"))
	if err != nil || strings.Count(string(producerLog), "status_exit="+strconv.Itoa(self.statusExit)+"\n") != 1 {
		t.Fatal("VPN source did not reach exactly one owned status producer exit")
	}
	if self.statusExit == 0 && strings.Count(string(producerLog), "journal_exit="+strconv.Itoa(self.journalExit)+"\n") != 1 {
		t.Fatal("VPN source did not reach the independently owned journal exit")
	}
	pingLog, err := os.ReadFile(filepath.Join(self.directory, "ping-log"))
	if err != nil || string(pingLog) != "192.0.2.10\n192.0.2.20\n192.0.2.30\n" {
		t.Fatal("VPN producer fixture did not retain exact admitted synthetic ICMP attempts")
	}
	for _, private := range []string{"synthetic-private-status-diagnostic", "synthetic-private-journal-diagnostic", "synthetic-private-client", "198.51.100.10", "203.0.113.30"} {
		if strings.Contains(alerts.Markdown(), private) || strings.Contains(self.output, private) {
			t.Fatal("VPN producer projected private source material")
		}
	}
}

func requireVPNProducerUnknown(t *testing.T, alerts Alerts, class string) {
	t.Helper()
	if len(alerts) != 1 || alerts[0].Class != class || alerts[0].Severity != SeverityWarn || alerts[0].Sustain != 1 || !isVisibilityAlert(alerts[0]) {
		t.Fatal("incomplete VPN producer became authoritative session loss or health")
	}
	if alerts[0].SignalNumber != "21.1" || alerts[0].SignalKey != "vpn-sessions" || alerts[0].SignalID != "network/vpn-sessions" {
		t.Fatal("VPN producer unknown lost its owning signal identity")
	}
}

func TestVPNSessionsFailedStatusProducerCannotProveAbsentClients(t *testing.T) {
	fixture := newVPNProducerFixture(t)
	fixture.status, fixture.statusExit = "", 7
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil {
		t.Fatal("failed synthetic status producer did not complete the public discriminator")
	}
	fixture.requireCostAndPrivacy(t, alerts)
	if len(alerts) != 1 || alerts[0].Class != "vpn-status-unavailable" {
		t.Fatal("failed status producer was accepted as a complete absent-client census")
	}
	requireVPNProducerUnknown(t, alerts, "vpn-status-unavailable")
}

func TestVPNSessionsPartialStatusProducerCannotProveAbsentClients(t *testing.T) {
	fixture := newVPNProducerFixture(t)
	fixture.status, fixture.statusExit = fixture.clientRow("192.0.2.10", "198.51.100.10"), 7
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil {
		t.Fatal("partial synthetic status producer did not complete the public discriminator")
	}
	fixture.requireCostAndPrivacy(t, alerts)
	if len(alerts) != 1 || alerts[0].Class != "vpn-status-unavailable" {
		t.Fatal("partial status producer was accepted as a complete absent-client census")
	}
	requireVPNProducerUnknown(t, alerts, "vpn-status-unavailable")
}

func TestVPNSessionsPartialStatusProducerCannotProveHealth(t *testing.T) {
	fixture := newVPNProducerFixture(t)
	fixture.status = strings.TrimSuffix(fixture.completeStatus(), "\n")
	fixture.statusExit = 7
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil {
		t.Fatal("partial full-looking status producer did not complete the public discriminator")
	}
	fixture.requireCostAndPrivacy(t, alerts)
	if len(alerts) != 1 || alerts[0].Class != "vpn-status-unavailable" {
		t.Fatal("failed full-looking status prefix was accepted as complete session health")
	}
	requireVPNProducerUnknown(t, alerts, "vpn-status-unavailable")
}

func TestVPNSessionsCompleteProducerHealthyAndMissingControls(t *testing.T) {
	for _, shell := range []string{"/bin/sh", "/bin/bash"} {
		for _, status := range []string{"complete", "missing", "empty"} {
			fixture := newVPNProducerFixture(t)
			fixture.shell = shell
			if shell == "/bin/bash" {
				fixture.pipefail = true
			}
			if status == "missing" {
				fixture.status = fixture.clientRow("192.0.2.10", "198.51.100.10") + fixture.clientRow("192.0.2.30", "203.0.113.30")
			} else if status == "empty" {
				fixture.status = ""
			}
			alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
			if err != nil {
				t.Fatal("complete producer control did not execute")
			}
			want := 0
			if status == "missing" {
				want = 1
			} else if status == "empty" {
				want = 3
			}
			if len(alerts) != want {
				t.Fatal("complete status producer changed healthy or known missing-client policy")
			}
			for _, alert := range alerts {
				if alert.Class != "vpn-client-session-loss" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
					t.Fatal("successful complete empty/missing census lost the existing session-loss WARN2")
				}
			}
			fixture.requireCostAndPrivacy(t, alerts)
		}
	}
}

func TestVPNSessionsFailedHistoryRetainsCurrentFailuresWithoutPartialAttribution(t *testing.T) {
	for _, partial := range []bool{false, true} {
		fixture := newVPNProducerFixture(t)
		fixture.status = fixture.clientRow("192.0.2.10", "198.51.100.10") + fixture.clientRow("192.0.2.30", "203.0.113.30")
		fixture.unreachable = "192.0.2.10"
		fixture.journalExit = 8
		if partial {
			fixture.journal = fmt.Sprintf("%d.000000 fixture ovpn[1]: synthetic-beta.example/198.51.100.10:1400 Inactivity timeout (--ping-restart), restarting synthetic-private-journal-prefix\n", fixture.now.Add(-time.Minute).Unix())
		}
		alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
		if err != nil {
			t.Fatal("failed optional journal erased the complete current-session observation")
		}
		if len(alerts) != 3 {
			t.Fatal("failed optional history erased known current failures or lacked explicit unknown")
		}
		unknown := requireVPNSessionAlert(t, alerts, "vpn-session-history-unavailable", "synthetic-vpn-server.example")
		if unknown.Severity != SeverityWarn || unknown.Sustain != 1 || !isVisibilityAlert(unknown) {
			t.Fatal("failed journal lost explicit history visibility")
		}
		for _, target := range []string{"synthetic-alpha.example", "synthetic-beta.example"} {
			class := "vpn-client-data-path-loss"
			if target == "synthetic-beta.example" {
				class = "vpn-client-session-loss"
			}
			alert := requireVPNSessionAlert(t, alerts, class, target)
			if alert.Severity != SeverityWarn || alert.Sustain != 2 || strings.Contains(alert.Observed, "shared_public_source=true") {
				t.Fatal("partial failed history acquired site attribution or changed current loss policy")
			}
		}
		if strings.Contains(alerts.Markdown(), "synthetic-private-journal-prefix") {
			t.Fatal("failed journal rendered its private body")
		}
		fixture.requireCostAndPrivacy(t, alerts)
	}
}

func TestVPNSessionsFailedHistoryCannotProveQuietHealth(t *testing.T) {
	fixture := newVPNProducerFixture(t)
	fixture.journalExit = 8
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil {
		t.Fatal("failed optional history did not retain current observation")
	}
	requireVPNProducerUnknown(t, alerts, "vpn-session-history-unavailable")
	fixture.requireCostAndPrivacy(t, alerts)
}

func TestVPNSessionsFailedProducersPreserveAuthoritativeUnitPage(t *testing.T) {
	fixture := newVPNProducerFixture(t)
	fixture.active, fixture.sub = "failed", "failed"
	fixture.status, fixture.statusExit, fixture.journalExit = "", 7, 8
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil || len(alerts) != 1 || alerts[0].Class != "vpn-server-unhealthy" || alerts[0].Severity != SeverityPage || alerts[0].Sustain != 1 {
		t.Fatal("failed independent producers erased authoritative failed-unit PAGE1")
	}
	fixture.requireCostAndPrivacy(t, alerts)
}

func TestVPNSessionsProducerUnknownDoesNotRewriteFreshnessPolicy(t *testing.T) {
	for _, offset := range []time.Duration{-91 * time.Second, 31 * time.Second} {
		fixture := newVPNProducerFixture(t)
		fixture.status, fixture.statusExit = "", 7
		fixture.mtime = fixture.now.Add(offset)
		alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
		if err != nil || len(alerts) != 1 || alerts[0].Class != "vpn-status-stale" || alerts[0].Severity != SeverityWarn || alerts[0].Sustain != 2 {
			t.Fatal("producer completeness rewrote stale/future status WARN2 policy")
		}
		fixture.requireCostAndPrivacy(t, alerts)
	}
}

func TestVPNSessionsProducerLegacyAndContradictoryProvenanceStayUnknown(t *testing.T) {
	fixture := newVPNProducerFixture(t)
	base := fmt.Sprintf("server_unit_observed true\nserver_active_state active\nserver_sub_state running\nserver_restarts 0\nstatus_mtime_epoch %d\n", fixture.now.Unix())
	for _, body := range []string{base, base + "status_observed false\njournal_observed true\n"} {
		settings := fixture.settings()
		settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return body, nil }}
		alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal("legacy or explicitly unavailable producer did not remain observable as unknown")
		}
		requireVPNProducerUnknown(t, alerts, "vpn-status-unavailable")
	}
	for _, marker := range []string{"status_observed true\nstatus_observed false\n", "status_observed synthetic-private-marker\n", "status_observed true extra\n"} {
		settings := fixture.settings()
		settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return base + marker, nil }}
		alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
		if err == nil || len(alerts) != 0 || strings.Contains(err.Error(), "synthetic-private-marker") {
			t.Fatal("contradictory/malformed producer provenance became health or exposed source text")
		}
	}
}

func TestVPNSessionsProducerScopeAndEndedParentKeepAdmission(t *testing.T) {
	fixture := newVPNProducerFixture(t)
	settings := fixture.settings()
	settings.ExcludedHosts = []string{"synthetic-beta.example"}
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" {
		t.Fatal("producer completeness changed excluded-client unknown policy")
	}
	pingLog, err := os.ReadFile(filepath.Join(fixture.directory, "ping-log"))
	if err != nil || string(pingLog) != "192.0.2.10\n192.0.2.30\n" {
		t.Fatal("producer command contacted an excluded nested client")
	}
	fixture = newVPNProducerFixture(t)
	settings = fixture.settings()
	settings.ExcludedHosts = []string{"synthetic-beta.example"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err = NewVPNSessionsSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || fixture.calls != 0 {
		t.Fatal("ended owner entered a producer or became a scope denial")
	}
}

func TestVPNSessionsPartialHistoryCannotSupplySiteAttribution(t *testing.T) {
	fixture := newVPNProducerFixture(t)
	fixture.status = fixture.clientRow("192.0.2.10", "198.51.100.10") + fixture.clientRow("192.0.2.30", "203.0.113.30")
	fixture.unreachable = "192.0.2.10"
	fixture.journalExit = 8
	fixture.journal = fmt.Sprintf("%d.000000 fixture ovpn[1]: synthetic-beta.example/198.51.100.10:1400 Inactivity timeout (--ping-restart), restarting\n", fixture.now.Add(-time.Minute).Unix())
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil {
		t.Fatal("partial-history discriminator did not reach the public verdict")
	}
	fixture.requireCostAndPrivacy(t, alerts)
	for _, alert := range alerts {
		if alert.Class == "vpn-site-data-path-loss" || alert.Class == "vpn-site-session-loss" {
			t.Fatal("failed journal prefix acquired authoritative site attribution")
		}
	}
	if len(alerts) != 3 {
		t.Fatal("partial history lacked unknown visibility or erased independent current failures")
	}
	requireVPNSessionAlert(t, alerts, "vpn-session-history-unavailable", "synthetic-vpn-server.example")
	requireVPNSessionAlert(t, alerts, "vpn-client-data-path-loss", "synthetic-alpha.example")
	requireVPNSessionAlert(t, alerts, "vpn-client-session-loss", "synthetic-beta.example")
	fixture.requireCostAndPrivacy(t, alerts)
}

func TestVPNSessionsProducerFailuresKeepDiagnosticsPrivate(t *testing.T) {
	fixture := newVPNProducerFixture(t)
	fixture.status, fixture.statusExit, fixture.journalExit = "synthetic-private-source-prefix\n", 7, 8
	fixture.diagnostics = true
	alerts, err := NewVPNSessionsSignal().Run(context.Background(), fixture.settings())
	if err != nil {
		t.Fatal("failed producer diagnostics escaped the fixed completion projection")
	}
	requireVPNProducerUnknown(t, alerts, "vpn-status-unavailable")
	if strings.Contains(fixture.output, "synthetic-private-source-prefix") || strings.Contains(alerts.Markdown(), "synthetic-private-source-prefix") {
		t.Fatal("raw failed status prefix was exported")
	}
	fixture.requireCostAndPrivacy(t, alerts)
}
