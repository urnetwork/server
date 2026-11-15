package monitor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Synthetic manager properties are also consumed by the full journal command
// fixture. No real units, files, identities or remote endpoints are accessed.
const journalVacuumUnitFixture = `
case "$2" in
  warp-journal-vacuum.service)
    [ "${JOURNAL_POLICY_FIXTURE_QUERY_FAILURE:-0}" = 0 ] || exit 1
    printf '%s\n' 'LoadState=loaded' \
      'FragmentPath=/etc/systemd/system/warp-journal-vacuum.service' \
      "DropInPaths=${JOURNAL_POLICY_FIXTURE_DROPINS:-}" \
      "NeedDaemonReload=${JOURNAL_POLICY_FIXTURE_RELOAD:-no}" \
      "ActiveState=${JOURNAL_POLICY_FIXTURE_ACTIVE:-inactive}" \
      "Result=${JOURNAL_POLICY_FIXTURE_RESULT:-success}" \
      "ExecMainCode=${JOURNAL_POLICY_FIXTURE_CODE:-1}" \
      "ExecMainStatus=${JOURNAL_POLICY_FIXTURE_STATUS:-0}" \
      "ExecMainStartTimestampMonotonic=${JOURNAL_POLICY_FIXTURE_START:-99990000000}" \
      "ExecMainExitTimestampMonotonic=${JOURNAL_POLICY_FIXTURE_END:-99995000000}"
    exit 0 ;;
  warp-journal-vacuum.timer)
    printf '%s\n' 'LoadState=loaded' \
      'FragmentPath=/etc/systemd/system/warp-journal-vacuum.timer' 'DropInPaths=' 'NeedDaemonReload=no' \
      "ActiveState=${JOURNAL_POLICY_FIXTURE_TIMER_ACTIVE:-active}" \
      "UnitFileState=${JOURNAL_POLICY_FIXTURE_ENABLED:-enabled}" \
      'ActiveEnterTimestampMonotonic=99900000000'
    exit 0 ;;
esac
`

const journalVacuumHashFixture = `
case "$1" in
  *warp-journal-vacuum.service)
    printf '%s  private-unit-path\n' "${JOURNAL_POLICY_FIXTURE_HASH:-fa8638d4be117b9743c19daacff52717e54f6b7df19eed1ac6773e9abac182b3}" ;;
  *warp-journal-vacuum.timer)
    printf '%s  private-unit-path\n' 70914fddcbdd70fa962dea5f2091b6c3cf9ab5abb66a727835d1c535d3e95802 ;;
  *) exit 1 ;;
esac
`

// Run only the production read-only reducer against synthetic commands.
func runJournalPolicyFixture(t *testing.T, values map[string]string) string {
	t.Helper()
	bin := t.TempDir()
	commands := map[string]string{
		"systemctl":  journalVacuumUnitFixture + "exit 1\n",
		"sha256sum":  journalVacuumHashFixture,
		"timeout":    "shift\nexec \"$@\"\n",
		"awk":        "case \"$*\" in *'/proc/uptime'*) echo 900000000000; exit 0 ;; esac\nexec /usr/bin/awk \"$@\"\n",
		"python3":    "[ \"${JOURNAL_POLICY_FIXTURE_CLOCK_FAILURE:-0}\" = 0 ] || exit 1\necho 100000000000\n",
		"dpkg-query": "printf '%s' \"${JOURNAL_POLICY_FIXTURE_VERSION:-unverified}\"\n",
		"journalctl": `
if [ -n "${JOURNAL_POLICY_FIXTURE_JOURNAL_STDERR:-}" ]; then
  printf '%s\n' "$JOURNAL_POLICY_FIXTURE_JOURNAL_STDERR" >&2
fi
if [ -n "${JOURNAL_POLICY_FIXTURE_RAW:-}" ]; then
  printf '%s\n' "$JOURNAL_POLICY_FIXTURE_RAW"
else
  count=${JOURNAL_POLICY_FIXTURE_ROTATIONS:-0}
  while [ "$count" -gt 0 ]; do
    printf '%s\n' 'Retention time reached, rotating.'
    count=$((count - 1))
  done
fi
exit "${JOURNAL_POLICY_FIXTURE_JOURNAL_EXIT:-0}"
`,
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("sh", "-c", journalVacuumPolicyCommand+`
printf '%s\n' "policy=$journal_vacuum_policy_state" "runtime=$journal_vacuum_runtime_state" \
  "age=$journal_vacuum_last_success_age_seconds" "build=$journal_retention_build" \
  "rotation_state=$journal_retention_rotation_state" "rotations=$journal_retention_rotations_5m"
`)
	command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	for key, value := range values {
		command.Env = append(command.Env, "JOURNAL_POLICY_FIXTURE_"+key+"="+value)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic policy reducer failed: %v; output=%q", err, output)
	}
	return string(output)
}

func TestJournalAgePolicyRequiresRecentCompletedSuccess(t *testing.T) {
	for _, c := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{name: "healthy", want: "runtime=complete\nage=5\n"},
		{name: "exact freshness boundary", values: map[string]string{"START": "99570000000", "END": "99580000000"}, want: "runtime=complete\nage=420\n"},
		{name: "freshness boundary plus one microsecond", values: map[string]string{"START": "99570000000", "END": "99579999999"}, want: "runtime=stale\nage=421\n"},
		{name: "stale", values: map[string]string{"START": "99570000000", "END": "99579000000"}, want: "runtime=stale\nage=421\n"},
		{name: "failed command", values: map[string]string{"RESULT": "exit-code", "STATUS": "1"}, want: "runtime=failed\nage=0\n"},
		{name: "disabled timer", values: map[string]string{"ENABLED": "disabled"}, want: "runtime=failed\nage=0\n"},
		{name: "running not completion", values: map[string]string{"ACTIVE": "activating"}, want: "runtime=running\nage=0\n"},
		{name: "never ran", values: map[string]string{"START": "0", "END": "0", "CODE": "0"}, want: "runtime=pending\nage=0\n"},
		{name: "future timestamp", values: map[string]string{"END": "100001000000"}, want: "runtime=unavailable\nage=0\n"},
		{name: "clock unavailable", values: map[string]string{"CLOCK_FAILURE": "1"}, want: "runtime=unavailable\nage=0\n"},
		{name: "changed unit", values: map[string]string{"HASH": "private-changed-unit"}, want: "policy=drift\n"},
		{name: "unreloaded unit", values: map[string]string{"RELOAD": "yes"}, want: "policy=drift\n"},
		{name: "drop-in", values: map[string]string{"DROPINS": "/private/synthetic.conf"}, want: "policy=drift\n"},
		{name: "unreadable", values: map[string]string{"QUERY_FAILURE": "1"}, want: "policy=unavailable\n"},
	} {
		output := runJournalPolicyFixture(t, c.values)
		if !strings.Contains(output, c.want) || strings.Contains(output, "private-") || strings.Contains(output, "/private/") {
			t.Errorf("%s: output=%q; want %q without private fixture content", c.name, output, c.want)
		}
	}
}

func TestJournalAgePolicyUsesSystemdMonotonicClockAcrossSuspend(t *testing.T) {
	if !strings.Contains(journalVacuumPolicyCommand, "time.clock_gettime_ns(time.CLOCK_MONOTONIC)") {
		t.Fatal("vacuum age must use the same suspend-excluding clock as systemd execution timestamps")
	}
	// The fixture's boot clock is deliberately far ahead after a synthetic
	// suspend. A fresh success must still be five monotonic seconds old.
	if output := runJournalPolicyFixture(t, nil); !strings.Contains(output, "runtime=complete\nage=5\n") {
		t.Errorf("mismatched suspend-inclusive timebase: %q", output)
	}
}

func TestJournalRetentionRotationReducerFailsClosed(t *testing.T) {
	for _, c := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{name: "healthy", want: "rotation_state=complete\nrotations=0\n"},
		{name: "no matches", values: map[string]string{"JOURNAL_EXIT": "1"}, want: "rotation_state=complete\nrotations=0\n"},
		{name: "failed empty read", values: map[string]string{"JOURNAL_EXIT": "1", "JOURNAL_STDERR": "private-read-error"}, want: "rotation_state=unavailable\nrotations=0\n"},
		{name: "exact marker", values: map[string]string{"ROTATIONS": "19"}, want: "rotation_state=complete\nrotations=19\n"},
		{name: "truncated", values: map[string]string{"ROTATIONS": "401"}, want: "rotation_state=truncated\nrotations=401\n"},
		{name: "failed partial producer", values: map[string]string{"ROTATIONS": "2", "JOURNAL_EXIT": "1"}, want: "rotation_state=unavailable\nrotations=0\n"},
		{name: "timeout", values: map[string]string{"ROTATIONS": "2", "JOURNAL_EXIT": "124"}, want: "rotation_state=unavailable\nrotations=0\n"},
		{name: "malformed", values: map[string]string{"RAW": "private-journal-secret"}, want: "rotation_state=unavailable\nrotations=0\n"},
	} {
		output := runJournalPolicyFixture(t, c.values)
		if !strings.Contains(output, c.want) || strings.Contains(output, "private-") {
			t.Errorf("%s: output=%q; want %q without journal content", c.name, output, c.want)
		}
	}
}

func TestJournalRetentionBuildDoesNotGuessBackports(t *testing.T) {
	for _, version := range []string{"249.99-synthetic", "255.4-synthetic-patched", "255.99-synthetic", "257-synthetic"} {
		if output := runJournalPolicyFixture(t, map[string]string{"VERSION": version}); !strings.Contains(output, "build=unverified\n") {
			t.Errorf("version=%q produced %q; want unverified", version, output)
		}
	}
	if output := runJournalPolicyFixture(t, map[string]string{"VERSION": "255.4-1ubuntu8.17"}); !strings.Contains(output, "build=known-affected\n") {
		t.Errorf("source-audited package output=%q; want known-affected", output)
	}
}

func TestJournalRetentionUnsafeConfigurationAndObservedBranchPageIndependently(t *testing.T) {
	for _, overrides := range []map[string]string{
		{"max_retention": "1hour", "journal_retention_build": "known-affected"},
		{"max_retention": "0", "journal_retention_rotations_5m": "19"},
		{"journal_retention_rotation_state": "truncated", "journal_retention_rotations_5m": "401"},
	} {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return journalBufferFixture(overrides), nil }}
		settings := syntheticSettings(source)
		settings.Hosts = []HostSettings{{Name: "synthetic-edge", Roles: []string{"services"}}}
		alerts, err := NewJournalBufferSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "journal-buffer-retention-rotation")
		if alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), "one-hour") ||
			!strings.Contains(alert.Markdown(), "Neither") || strings.Contains(alert.Markdown(), "hash-table pressure") {
			t.Errorf("unsafe branch alert=%+v; want bounded PAGE without inferred data loss", alert)
		}
	}
}

func TestJournalRetentionUnknownBuildCannotResolveUnsafeBranch(t *testing.T) {
	sample, err := parseJournalBufferSample(journalBufferFixture(map[string]string{"max_retention": "1hour"}))
	if err != nil {
		t.Fatal(err)
	}
	findings := evaluateJournalBuffer("synthetic-edge", sample)
	unknown := false
	for _, f := range findings {
		if f.class == "journal-buffer-retention-rotation" {
			t.Errorf("unverified build produced branch finding=%+v", f)
		}
		unknown = unknown || f.class == "cannot-observe" && strings.HasSuffix(f.target, "/journal-retention-rotation")
	}
	if !unknown {
		t.Error("unverified enabled retention must remain visible, not healthy")
	}
}

func TestJournalRetentionVisibilityPreservesEvidenceClass(t *testing.T) {
	base := journalBufferSample{
		journalVacuumPolicyState:      "valid",
		journalVacuumRuntimeState:     "complete",
		journalRetentionBuild:         "known-fixed",
		journalRetentionRotationState: "complete",
	}
	for _, tc := range []struct {
		name   string
		sample journalBufferSample
		want   string
	}{
		{
			name: "missing evidence",
			sample: func() journalBufferSample {
				s := base
				s.journalRetentionRotationState = "unavailable"
				return s
			}(),
			want: observationErrorClassStateUnavailable,
		},
		{
			name: "capped census",
			sample: func() journalBufferSample {
				s := base
				s.journalRetentionRotationState = "truncated"
				return s
			}(),
			want: observationErrorClassBoundExceeded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings := evaluateJournalAgePolicy("synthetic-edge", tc.sample, false, true)
			var visibility *finding
			for i := range findings {
				if findings[i].class == "cannot-observe" && strings.HasSuffix(findings[i].target, "/journal-retention-rotation") {
					visibility = &findings[i]
					break
				}
			}
			if visibility == nil || !strings.Contains(visibility.observed, "error_class="+tc.want) {
				t.Fatalf("visibility=%+v; want error_class=%s", visibility, tc.want)
			}
		})
	}
}

func TestJournalAgePolicyMalformedMetadataCannotLeakOrCertifyHealth(t *testing.T) {
	for _, overrides := range []map[string]string{
		{"journal_vacuum_runtime_state": "private-secret"},
		{"journal_vacuum_last_success_age_seconds": "421"},
		{"journal_vacuum_policy_state": "drift"},
		{"journal_retention_rotation_state": "unavailable", "journal_retention_rotations_5m": "1"},
		{"journal_retention_rotation_state": "truncated"},
	} {
		_, err := parseJournalBufferSample(journalBufferFixture(overrides))
		if err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Errorf("overrides=%v error=%v; want fixed malformed-evidence error", overrides, err)
		}
	}
}

func TestJournalAgePolicyDriftPagesWithoutRuntimeAuthority(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return journalBufferFixture(map[string]string{
			"journal_vacuum_policy_state": "drift", "journal_vacuum_runtime_state": "unavailable",
			"journal_vacuum_last_success_age_seconds": "0",
		}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "synthetic-edge", Roles: []string{"services"}}}
	alerts, err := NewJournalBufferSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "journal-buffer-vacuum")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "vacuum_policy=drift vacuum_runtime=unavailable") {
		t.Errorf("affirmative policy drift was hidden: %+v", alert)
	}
	config := requireAlertClass(t, alerts, "journal-buffer-config")
	if !strings.Contains(config.Evidence, "vacuum_policy=drift vacuum_runtime=unavailable") || !strings.Contains(config.Observed, "vacuum_policy=drift") {
		t.Errorf("config finding omitted the changed authority: %+v", config)
	}
}

func TestJournalAgePolicyUnknownDoesNotHideIndependentPages(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return journalBufferFixture(map[string]string{
			"journald_active": "failed", "journal_vacuum_policy_state": "unavailable",
			"journal_vacuum_runtime_state": "unavailable", "journal_vacuum_last_success_age_seconds": "0",
			"journal_retention_rotations_5m": "3",
		}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "synthetic-edge", Roles: []string{"services"}}}
	alerts, err := NewJournalBufferSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"journal-buffer-unavailable", "journal-buffer-retention-rotation"} {
		if alert := requireAlertClass(t, alerts, class); alert.Severity != SeverityPage {
			t.Errorf("independent %s was suppressed: %+v", class, alert)
		}
	}
	requireAlertClass(t, alerts, "cannot-observe")
	for _, alert := range alerts {
		if alert.Class == "journal-buffer-vacuum" {
			t.Errorf("unavailable policy is not proved drift: %+v", alert)
		}
		if strings.Contains(alert.Markdown(), "/var/log/journal") || strings.Contains(alert.Markdown(), "private-journal-secret") {
			t.Errorf("private source detail escaped into Markdown: %q", alert.Markdown())
		}
	}
}

func TestJournalRetentionDurationDoesNotInventEnabledState(t *testing.T) {
	for _, c := range []struct {
		value          string
		enabled, known bool
	}{
		{value: "-", known: true}, {value: "0", known: true}, {value: "0s", known: true},
		{value: "infinity", known: true}, {value: "1hour", enabled: true, known: true},
		{value: "7day", enabled: true, known: true}, {value: "0.5h", enabled: true, known: true},
		{value: "private-config-secret"}, {value: "-1h"}, {value: "1h30min"},
	} {
		enabled, known := journalRetentionEnabled(c.value)
		if enabled != c.enabled || known != c.known {
			t.Errorf("value=%q got enabled=%t known=%t; want %t/%t", c.value, enabled, known, c.enabled, c.known)
		}
	}
}
