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
		"runtime_logind_idle_action", "runtime_logind_lid_action",
		"runtime_logind_lid_external_action", "runtime_logind_lid_docked_action",
		"runtime_can_suspend",
		"suspend_entries", "resume_entries", "suspend_pairs", "latest_suspend_epoch",
		"latest_resume_epoch", "max_suspend_seconds", "unmatched_suspend",
		"topology_state", "media_health",
	}
	values := map[string]string{
		"observation_schema": "2", "sleep_policy": "deny-all", "logind_policy": "ignore-all",
		"desktop_policy": "nothing-all", "suspend_entries": "0", "resume_entries": "0",
		"runtime_logind_idle_action": "ignore", "runtime_logind_lid_action": "ignore",
		"runtime_logind_lid_external_action": "ignore", "runtime_logind_lid_docked_action": "ignore",
		"runtime_can_suspend": "no",
		"suspend_pairs":       "0", "latest_suspend_epoch": "0", "latest_resume_epoch": "0",
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

func syntheticHostpowerAlerts(t *testing.T, fixture string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return fixture, nil }}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC) }
	settings.Hosts = []HostSettings{{Name: "stationary.fixture.example", Roles: []string{"stationary"}}}
	alerts, err := NewHostpowerSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func assertNoHostpowerAlertClass(t *testing.T, alerts Alerts, class string) {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == class {
			t.Fatalf("unexpected %s alert: %s", class, alert.Markdown())
		}
	}
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

func TestHostpowerSignalSyntheticRuntimePolicyClasses(t *testing.T) {
	t.Run("missing external-power property uses safe base lid action", func(t *testing.T) {
		alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
			"runtime_logind_lid_external_action": "unsupported",
		}))
		if len(alerts) != 0 {
			t.Fatalf("supported runtime policy alerts=%+v, want none", alerts)
		}
	})

	t.Run("empty external-power property falls back to safe base lid action", func(t *testing.T) {
		alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
			"runtime_logind_lid_external_action": "fallback",
		}))
		if len(alerts) != 0 {
			t.Fatalf("fallback runtime policy alerts=%+v, want none", alerts)
		}
	})

	t.Run("not-applicable caller result is a healthy non-affirmative control", func(t *testing.T) {
		alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
			"runtime_can_suspend": "na",
		}))
		if len(alerts) != 0 {
			t.Fatalf("not-applicable runtime capability alerts=%+v, want none", alerts)
		}
	})

	t.Run("safe config with blocked stale runtime warns not loaded", func(t *testing.T) {
		for _, capability := range []string{"no", "na"} {
			t.Run(capability, func(t *testing.T) {
				alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
					"runtime_logind_lid_action": "suspend",
					"runtime_can_suspend":       capability,
				}))
				alert := requireAlertClass(t, alerts, "hostpower-policy-not-loaded")
				if alert.Severity != SeverityWarn || alert.Sustain != 2 {
					t.Fatalf("not-loaded alert=%+v", alert)
				}
				markdown := alert.Markdown()
				for _, want := range []string{
					"runtime_lid=suspend", "can_suspend_caller_result=" + capability,
					"effective deny-all sleep policy blocks", "defense-in-depth convergence",
				} {
					if !strings.Contains(markdown, want) {
						t.Errorf("not-loaded Markdown lacks %q:\n%s", want, markdown)
					}
				}
				for _, forbidden := range []string{
					"org.freedesktop.login1", "busctl", "MainPID", "object path", "global suspend capability",
				} {
					if strings.Contains(markdown, forbidden) {
						t.Fatalf("not-loaded Markdown exposes or misstates %q:\n%s", forbidden, markdown)
					}
				}
				assertNoHostpowerAlertClass(t, alerts, "hostpower-suspend-policy-unsafe")
				assertNoHostpowerAlertClass(t, alerts, "hostpower-policy-runtime-unobservable")
			})
		}
	})

	t.Run("external-power fallback inherits stale base action", func(t *testing.T) {
		alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
			"runtime_logind_lid_action":          "suspend",
			"runtime_logind_lid_external_action": "fallback",
			"runtime_can_suspend":                "no",
		}))
		requireAlertClass(t, alerts, "hostpower-policy-not-loaded")
		assertNoHostpowerAlertClass(t, alerts, "hostpower-suspend-policy-unsafe")
		assertNoHostpowerAlertClass(t, alerts, "hostpower-policy-runtime-unobservable")
	})

	t.Run("fallback is rejected for required runtime action", func(t *testing.T) {
		_, err := parseHostpowerSample(hostpowerFixture(map[string]string{
			"runtime_logind_lid_action": "fallback",
		}), time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC))
		if err == nil {
			t.Fatal("required runtime action accepted the optional-property fallback sentinel")
		}
	})

	t.Run("external-power fallback inherits destructive base action", func(t *testing.T) {
		alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
			"runtime_logind_lid_action":          "soft-reboot",
			"runtime_logind_lid_external_action": "fallback",
			"runtime_can_suspend":                "no",
		}))
		alert := requireAlertClass(t, alerts, "hostpower-suspend-policy-unsafe")
		if alert.Severity != SeverityPage || alert.Sustain != 1 {
			t.Fatalf("destructive inherited runtime alert=%+v", alert)
		}
		assertNoHostpowerAlertClass(t, alerts, "hostpower-policy-not-loaded")
		assertNoHostpowerAlertClass(t, alerts, "hostpower-policy-runtime-unobservable")
	})

	t.Run("available suspend capability pages unsafe runtime", func(t *testing.T) {
		for _, capability := range []string{"yes", "challenge"} {
			t.Run(capability, func(t *testing.T) {
				alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
					"runtime_logind_lid_action": "suspend",
					"runtime_can_suspend":       capability,
				}))
				alert := requireAlertClass(t, alerts, "hostpower-suspend-policy-unsafe")
				if alert.Severity != SeverityPage || alert.Sustain != 1 {
					t.Fatalf("unsafe runtime alert=%+v", alert)
				}
				assertNoHostpowerAlertClass(t, alerts, "hostpower-policy-not-loaded")
				assertNoHostpowerAlertClass(t, alerts, "hostpower-policy-runtime-unobservable")
			})
		}
	})

	t.Run("destructive live action pages despite blocked suspend", func(t *testing.T) {
		for _, action := range []string{"poweroff", "reboot", "soft-reboot", "halt", "kexec", "factory-reset"} {
			t.Run(action, func(t *testing.T) {
				alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
					"runtime_logind_lid_action": action,
					"runtime_can_suspend":       "no",
				}))
				alert := requireAlertClass(t, alerts, "hostpower-suspend-policy-unsafe")
				if alert.Severity != SeverityPage || alert.Sustain != 1 {
					t.Fatalf("destructive runtime alert=%+v", alert)
				}
				assertNoHostpowerAlertClass(t, alerts, "hostpower-policy-not-loaded")
				assertNoHostpowerAlertClass(t, alerts, "hostpower-policy-runtime-unobservable")
			})
		}
	})

	t.Run("current non-destructive action vocabulary remains a convergence warning", func(t *testing.T) {
		for _, action := range []string{
			"hibernate", "hybrid-sleep", "suspend-then-hibernate", "sleep", "lock", "secure-attention-key",
		} {
			t.Run(action, func(t *testing.T) {
				alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
					"runtime_logind_lid_action": action,
					"runtime_can_suspend":       "no",
				}))
				requireAlertClass(t, alerts, "hostpower-policy-not-loaded")
				assertNoHostpowerAlertClass(t, alerts, "hostpower-suspend-policy-unsafe")
				assertNoHostpowerAlertClass(t, alerts, "hostpower-policy-runtime-unobservable")
			})
		}
	})

	t.Run("documented CanSuspend caller results remain parseable", func(t *testing.T) {
		for _, capability := range []string{
			"yes", "no", "challenge", "na", "inhibited", "inhibitor-blocked", "challenge-inhibitor-blocked",
		} {
			t.Run(capability, func(t *testing.T) {
				sample, err := parseHostpowerSample(hostpowerFixture(map[string]string{
					"runtime_can_suspend": capability,
				}), time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC))
				if err != nil || sample.runtimeCanSuspend != capability {
					t.Fatalf("CanSuspend result %q parsed as %+v, err=%v", capability, sample, err)
				}
			})
		}
	})

	t.Run("temporarily inhibited suspend capability is still available", func(t *testing.T) {
		for _, capability := range []string{"inhibited", "inhibitor-blocked", "challenge-inhibitor-blocked"} {
			t.Run(capability, func(t *testing.T) {
				alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
					"runtime_can_suspend": capability,
				}))
				requireAlertClass(t, alerts, "hostpower-suspend-policy-unsafe")
				assertNoHostpowerAlertClass(t, alerts, "hostpower-policy-not-loaded")
				assertNoHostpowerAlertClass(t, alerts, "hostpower-policy-runtime-unobservable")
			})
		}
	})

	t.Run("known suspend capability pages alongside independent unknown", func(t *testing.T) {
		alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
			"runtime_logind_idle_action": "unobservable",
			"runtime_can_suspend":        "yes",
		}))
		requireAlertClass(t, alerts, "hostpower-suspend-policy-unsafe")
		requireAlertClass(t, alerts, "hostpower-policy-runtime-unobservable")
		assertNoHostpowerAlertClass(t, alerts, "hostpower-policy-not-loaded")
	})

	t.Run("runtime observation loss is distinct unknown", func(t *testing.T) {
		alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
			"runtime_logind_idle_action": "unobservable",
		}))
		alert := requireAlertClass(t, alerts, "hostpower-policy-runtime-unobservable")
		if alert.Severity != SeverityWarn || alert.Sustain != 2 || !strings.Contains(alert.Markdown(), "UNKNOWN is not healthy") {
			t.Fatalf("runtime visibility alert=%+v\n%s", alert, alert.Markdown())
		}
		assertNoHostpowerAlertClass(t, alerts, "hostpower-suspend-policy-unsafe")
		assertNoHostpowerAlertClass(t, alerts, "hostpower-policy-not-loaded")
	})
}

func TestHostpowerSignalRetainedSuspendHistoryIsIndependentOfRuntimePolicyGeneration(t *testing.T) {
	now := time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC)
	suspend := now.Add(-20 * time.Hour)
	resume := now.Add(-time.Hour)
	alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
		"runtime_logind_lid_action": "suspend",
		"runtime_can_suspend":       "no",
		"suspend_entries":           "1",
		"resume_entries":            "1",
		"suspend_pairs":             "1",
		"latest_suspend_epoch":      fmt.Sprint(suspend.Unix()),
		"latest_resume_epoch":       fmt.Sprint(resume.Unix()),
		"max_suspend_seconds":       fmt.Sprint(int64((19 * time.Hour) / time.Second)),
	}))
	requireAlertClass(t, alerts, "hostpower-policy-not-loaded")
	observed := requireAlertClass(t, alerts, "hostpower-suspend-observed")
	if observed.Severity != SeverityPage || !strings.Contains(observed.Markdown(), "longest_paired_suspend=19h0m0s") {
		t.Fatalf("retained suspend alert=%+v\n%s", observed, observed.Markdown())
	}
	assertNoHostpowerAlertClass(t, alerts, "hostpower-suspend-policy-unsafe")
}

func TestEvaluateHostpowerRuntimeUnknownWithholdsOtherPolicyLifecycleFindings(t *testing.T) {
	sample, err := parseHostpowerSample(hostpowerFixture(map[string]string{
		"runtime_logind_idle_action": "unobservable",
	}), time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	findings := evaluateHostpower("stationary.fixture.example", sample)
	foundUnknown := false
	for _, finding := range findings {
		switch finding.class {
		case "hostpower-suspend-policy-unsafe", "hostpower-policy-not-loaded":
			t.Fatalf("runtime UNKNOWN emitted %s lifecycle finding (healthy=%t)", finding.class, finding.healthy)
		case "hostpower-policy-runtime-unobservable":
			if finding.healthy {
				t.Fatal("runtime UNKNOWN emitted a healthy runtime visibility finding")
			}
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Fatal("runtime UNKNOWN omitted its visibility finding")
	}
}

func TestHostpowerSignalMalformedRuntimePolicyIsPrivacyReducedVisibility(t *testing.T) {
	privateDetail := "opaque-private-policy-detail"
	alerts := syntheticHostpowerAlerts(t, hostpowerFixture(map[string]string{
		"runtime_logind_lid_action": "suspend\nraw_dbus=" + privateDetail,
	}))
	alert := requireAlertClass(t, alerts, "cannot-observe")
	if strings.Contains(alert.Markdown(), privateDetail) || strings.Contains(alert.Markdown(), "raw_dbus") {
		t.Fatalf("malformed runtime output leaked through visibility alert:\n%s", alert.Markdown())
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

func TestHostpowerCommandReducesCurrentActionHistoryAndHardware(t *testing.T) {
	binDir := t.TempDir()
	commands := map[string]string{
		"systemd-analyze": `#!/bin/sh
case "$*" in
  *sleep.conf) printf '%s\n' AllowSuspend=no AllowHibernation=no AllowSuspendThenHibernate=no AllowHybridSleep=no ;;
  *logind.conf) printf '%s\n' HandleLidSwitch=ignore HandleLidSwitchExternalPower=ignore HandleLidSwitchDocked=ignore IdleAction=ignore ;;
  *) exit 2 ;;
esac
`,
		"busctl": `#!/bin/sh
if [ "$1" = "--no-pager" ]; then
  shift
fi
case "$1" in
  introspect)
    printf '%s\n' \
      '.IdleAction property s "ignore" -' \
      '.HandleLidSwitch property s "ignore" -' \
      '.HandleLidSwitchDocked property s "ignore" -'
    if [ "${HOSTPOWER_EXTERNAL_MODE:-fallback}" != unsupported ]; then
      printf '%s\n' '.HandleLidSwitchExternalPower property s "" -'
    fi
    ;;
  get-property)
    case "$5" in
      HandleLidSwitchExternalPower)
        if [ "${HOSTPOWER_EXTERNAL_MODE:-fallback}" = unsupported ]; then
          exit 1
        fi
        printf '%s\n' 's ""'
        ;;
      HandleLidSwitch) printf 's "%s"\n' "${HOSTPOWER_LID_ACTION:-ignore}" ;;
      *) printf '%s\n' 's "ignore"' ;;
    esac
    ;;
  call)
    printf '%s\n' 's "no"'
    ;;
  *) exit 2 ;;
esac
`,
		"gsettings": "#!/bin/sh\nprintf \"'nothing'\\n\"\n",
		"journalctl": `#!/bin/sh
case " $* " in
  *' -n 0 '*) exit 0 ;;
esac
printf '%s\n' \
  '1789131600.000000 host.fixture.example kernel: PM: suspend exit' \
  '1789128000.000000 host.fixture.example kernel: PM: suspend entry (s2idle)'
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
	run := func(lidAction, externalMode string) hostpowerSample {
		t.Helper()
		command := exec.Command("sh", "-c", "archive_expected=1\n"+hostpowerCommand)
		command.Env = append(os.Environ(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"HOSTPOWER_LID_ACTION="+lidAction,
			"HOSTPOWER_EXTERNAL_MODE="+externalMode,
		)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("hostpower command failed: %v\n%s", err, output)
		}
		sample, err := parseHostpowerSample(string(output), time.Unix(1789135200, 0).UTC())
		if err != nil {
			t.Fatal(err)
		}
		return sample
	}

	sample := run("soft-reboot", "fallback")
	if sample.suspendPairs != 1 || sample.maxSuspend != time.Hour || sample.unmatchedSuspend ||
		sample.runtimeLid != "soft-reboot" ||
		sample.runtimeLidPower != "fallback" || sample.runtimeCanSuspend != "no" ||
		sample.topologyState != "shared-removable" || sample.mediaHealth != "unobservable" {
		t.Fatalf("reduced sample=%+v", sample)
	}
	unsafe := false
	for _, finding := range evaluateHostpower("stationary.fixture.example", sample) {
		if finding.class == "hostpower-suspend-policy-unsafe" && !finding.healthy &&
			finding.tier == tierPage && finding.sustain == 1 {
			unsafe = true
		}
	}
	if !unsafe {
		t.Fatal("shell-reduced soft-reboot action did not produce an immediate unsafe policy page")
	}
	unsupported := run("ignore", "unsupported")
	if unsupported.runtimeLidPower != "unsupported" {
		t.Fatalf("absent optional external-power property reduced as %q, want unsupported", unsupported.runtimeLidPower)
	}
	for _, want := range []string{"-k -b 0", "--grep 'PM: suspend (entry|exit)'", "-n 128", "LC_ALL=C sort -n -k1,1", "systemd-analyze cat-config", "DCONF_PROFILE=user", "busctl get-property", "busctl call", "soft-reboot", "hostpower-hardware.sh"} {
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
