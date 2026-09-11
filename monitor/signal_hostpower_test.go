package monitor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hostpowerFixture(overrides map[string]string) string {
	keys := []string{
		"observation_schema", "sleep_policy", "logind_policy", "desktop_policy",
		"suspend_entries", "resume_entries", "suspend_pairs", "latest_suspend_epoch",
		"latest_resume_epoch", "max_suspend_seconds", "unmatched_suspend",
		"topology_state", "media_health",
	}
	values := map[string]string{
		"observation_schema": "1", "sleep_policy": "deny-all", "logind_policy": "ignore-all",
		"desktop_policy": "nothing-all", "suspend_entries": "0", "resume_entries": "0",
		"suspend_pairs": "0", "latest_suspend_epoch": "0", "latest_resume_epoch": "0",
		"max_suspend_seconds": "0", "unmatched_suspend": "0", "topology_state": "separate",
		"media_health": "healthy",
	}
	for key, value := range overrides {
		values[key] = value
	}
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+"="+values[key])
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestHostpowerSignalSyntheticHealthyAndScoped(t *testing.T) {
	calls := 0
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		calls++
		if host.Name != "backup.fixture.example" || !strings.Contains(command, hostpowerMarker) ||
			!strings.HasPrefix(command, "archive_expected=1\n") {
			return "", fmt.Errorf("unexpected hostpower command")
		}
		return hostpowerFixture(nil), nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	settings.Hosts = []HostSettings{
		{Name: "backup.fixture.example", Roles: []string{"backup", "stationary"}},
		{Name: "service.fixture.example", Roles: []string{"services"}},
	}
	alerts, err := NewHostpowerSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 || calls != 1 {
		t.Fatalf("healthy alerts=%+v calls=%d, want none and one scoped call", alerts, calls)
	}
}

func TestHostpowerSignalSyntheticIncidentClasses(t *testing.T) {
	now := time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC)
	suspend := now.Add(-20 * time.Hour)
	resume := now.Add(-time.Hour)
	fixture := hostpowerFixture(map[string]string{
		"sleep_policy": "unsafe", "desktop_policy": "unsafe",
		"suspend_entries": "1", "resume_entries": "1", "suspend_pairs": "1",
		"latest_suspend_epoch": fmt.Sprint(suspend.Unix()), "latest_resume_epoch": fmt.Sprint(resume.Unix()),
		"max_suspend_seconds": fmt.Sprint(int64((19 * time.Hour) / time.Second)),
		"topology_state":      "shared-removable", "media_health": "unobservable",
	})
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return fixture, nil }}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = []HostSettings{{Name: "archive.fixture.example", Roles: []string{"backup"}}}

	alerts, err := NewHostpowerSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{
		"hostpower-suspend-policy-unsafe", "hostpower-suspend-observed",
		"hostpower-shared-removable-domain", "hostpower-media-health-unobservable",
	} {
		alert := requireAlertClass(t, alerts, class)
		if !strings.Contains(alert.Playbook, "§11.22") {
			t.Fatalf("%s does not cross-link archive recovery: %+v", class, alert)
		}
	}
	if alert := requireAlertClass(t, alerts, "hostpower-suspend-observed"); alert.Severity != SeverityPage ||
		!strings.Contains(alert.Markdown(), "longest_paired_suspend=19h0m0s") {
		t.Fatalf("long suspend alert=%+v\n%s", alert, alert.Markdown())
	}
	topology := requireAlertClass(t, alerts, "hostpower-shared-removable-domain")
	for _, forbidden := range []string{"/sys/", "/dev/", "serial=", "mac="} {
		if strings.Contains(strings.ToLower(topology.Markdown()), forbidden) {
			t.Fatalf("topology alert leaked %q:\n%s", forbidden, topology.Markdown())
		}
	}
	media := requireAlertClass(t, alerts, "hostpower-media-health-unobservable")
	if !strings.Contains(media.Markdown(), "SCSI Health=OK") || !strings.Contains(media.Markdown(), "UNKNOWN, never healthy") {
		t.Fatalf("media visibility alert lost bridge limitation:\n%s", media.Markdown())
	}
}

func TestHostpowerSignalSyntheticMediaFailureAndVisibility(t *testing.T) {
	fixtures := map[string]string{
		"failed.fixture.example":  hostpowerFixture(map[string]string{"media_health": "failed"}),
		"unknown.fixture.example": hostpowerFixture(map[string]string{"topology_state": "unobservable"}),
	}
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) { return fixtures[host.Name], nil }}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	settings.Hosts = []HostSettings{
		{Name: "failed.fixture.example", Roles: []string{"backup"}},
		{Name: "unknown.fixture.example", Roles: []string{"backup"}},
	}
	alerts, err := NewHostpowerSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if requireAlertClass(t, alerts, "hostpower-media-health-failed").Severity != SeverityPage {
		t.Fatalf("explicit failed media did not page: %+v", alerts)
	}
	if requireAlertClass(t, alerts, "hostpower-topology-unobservable").Sustain != 2 {
		t.Fatalf("topology visibility is not sustained: %+v", alerts)
	}
}

func TestHostpowerSignalMalformedOutputIsPrivacyReducedVisibility(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return hostpowerFixture(map[string]string{"media_health": "private-detail\nraw_path=/private/device"}), nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	settings.Hosts = []HostSettings{{Name: "backup.fixture.example", Roles: []string{"backup"}}}
	alerts, err := NewHostpowerSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "cannot-observe")
	if strings.Contains(alert.Markdown(), "private-detail") || strings.Contains(alert.Markdown(), "/private/device") {
		t.Fatalf("malformed host output leaked through visibility alert:\n%s", alert.Markdown())
	}
}

func TestHostpowerCommandReducesCurrentBootHistoryAndHardware(t *testing.T) {
	binDir := t.TempDir()
	commands := map[string]string{
		"systemd-analyze": `#!/bin/sh
case "$*" in
  *sleep.conf) printf '%s\n' AllowSuspend=no AllowHibernation=no AllowSuspendThenHibernate=no AllowHybridSleep=no ;;
  *logind.conf) printf '%s\n' HandleLidSwitch=ignore HandleLidSwitchExternalPower=ignore HandleLidSwitchDocked=ignore IdleAction=ignore ;;
  *) exit 2 ;;
esac
`,
		"gsettings": "#!/bin/sh\nprintf \"'nothing'\\n\"\n",
		"journalctl": `#!/bin/sh
case " $* " in
  *' -n 0 '*) exit 0 ;;
esac
printf '%s\n' \
  '1789128000.000000 host.fixture.example kernel: PM: suspend entry (s2idle)' \
  '1789131600.000000 host.fixture.example kernel: PM: suspend exit'
`,
		"sudo": `#!/bin/sh
printf '%s\n' topology_state=shared-removable media_health=unobservable
`,
	}
	for name, body := range commands {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("sh", "-c", "archive_expected=1\n"+hostpowerCommand)
	command.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("hostpower command failed: %v\n%s", err, output)
	}
	sample, err := parseHostpowerSample(string(output), time.Unix(1789135200, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if sample.suspendPairs != 1 || sample.maxSuspend != time.Hour || sample.unmatchedSuspend ||
		sample.topologyState != "shared-removable" || sample.mediaHealth != "unobservable" {
		t.Fatalf("reduced sample=%+v", sample)
	}
	for _, want := range []string{"-k -b 0", "--grep 'PM: suspend (entry|exit)'", "-n 128", "systemd-analyze cat-config", "DCONF_PROFILE=user", "hostpower-hardware.sh"} {
		if !strings.Contains(hostpowerCommand, want) {
			t.Errorf("hostpower command lacks %q", want)
		}
	}
}

func TestHostpowerSignalNoopsWithoutStationaryInventory(t *testing.T) {
	alerts, err := NewHostpowerSignal().Run(context.Background(), syntheticSettings(&syntheticSource{}))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("unconfigured hostpower alerts=%+v err=%v", alerts, err)
	}
}
