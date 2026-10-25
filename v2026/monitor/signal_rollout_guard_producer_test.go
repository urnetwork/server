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

// Exercise the command received by the public adapter. Only its installed
// executable path is relocated to a private marker fixture; no producer logic
// is copied or replaced, and no real systemctl, units or Warp binary is read.
type rolloutProducerControl struct {
	filesStatus       int
	runningStatus     int
	filesRows         bool
	runningRows       bool
	environmentStatus int
	disabled          bool
	guard             string
	stale             bool
	startUnavailable  bool
}

func rolloutProducerSettings(t *testing.T, controls map[string]rolloutProducerControl) SignalSettings {
	t.Helper()
	directories := map[string]string{}
	for name, control := range controls {
		directory := t.TempDir()
		directories[name] = directory
		for _, tool := range []string{"awk", "grep"} {
			realTool, err := exec.LookPath(tool)
			if err != nil {
				t.Fatalf("required local selector %s unavailable", tool)
			}
			if err := os.Symlink(realTool, filepath.Join(directory, tool)); err != nil {
				t.Fatal(err)
			}
		}
		scripts := map[string]string{
			"systemctl": `#!/bin/sh
printf '%s\n' "$*" >> "$ROLLOUT_TEST_TRACE"
case "$*" in
 'list-unit-files --type=service --no-legend --no-pager warp-synthetic-*.service')
  if [ "$ROLLOUT_TEST_FILES_ROWS" = true ]; then printf '%s\n' 'warp-synthetic-api-0.service enabled'; fi
  exit "$ROLLOUT_TEST_FILES_STATUS" ;;
 'list-units --type=service --state=running --no-legend --no-pager --plain warp-synthetic-*.service')
  if [ "$ROLLOUT_TEST_RUNNING_ROWS" = true ]; then printf '%s\n' 'warp-synthetic-api-0.service loaded active running Synthetic'; fi
  exit "$ROLLOUT_TEST_RUNNING_STATUS" ;;
 'show warp-synthetic-api-0.service -p Environment --value')
  if [ "$ROLLOUT_TEST_ENV_STATUS" != 0 ]; then printf '%s\n' 'synthetic-private-producer-error' >&2; exit "$ROLLOUT_TEST_ENV_STATUS"; fi
  if [ "$ROLLOUT_TEST_DISABLED" = true ]; then printf '%s\n' 'WARPCTL_STAGGER_HOST_DRAIN=0'; fi ;;
 'show warp-synthetic-api-0.service -p ExecMainStartTimestamp --value')
  if [ "$ROLLOUT_TEST_START_UNAVAILABLE" = true ]; then exit 19; fi
  printf '%s\n' 'synthetic-worker-start' ;;
 *) printf '%s\n' 'UNEXPECTED' >> "$ROLLOUT_TEST_TRACE"; exit 126 ;;
esac
`,
			"stat": `#!/bin/sh
if ! { [ "$#" = 3 ] && [ "$1" = -Lc ] && [ "$2" = %Z ] && [ "$3" = "$ROLLOUT_TEST_WARP" ]; }; then
 printf '%s\n' UNEXPECTED >> "$ROLLOUT_TEST_TRACE"
 exit 126
fi
printf '%s\n' 100
`,
			"date": `#!/bin/sh
if ! { [ "$#" = 3 ] && [ "$1" = -d ] && [ "$2" = synthetic-worker-start ] && [ "$3" = +%s ]; }; then
 printf '%s\n' UNEXPECTED >> "$ROLLOUT_TEST_TRACE"
 exit 126
fi
if [ "$ROLLOUT_TEST_STALE" = true ]; then printf '%s\n' 99; else printf '%s\n' 101; fi
`,
		}
		for tool, script := range scripts {
			if err := os.WriteFile(filepath.Join(directory, tool), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
		}
		marker := "unrecognized synthetic artifact\n"
		switch control.guard {
		case rolloutGuardFull:
			marker = "host rollout lock not acquired within\n"
		case rolloutGuardDrainOnly:
			marker = "Draining %d overlapping container(s) (staggered=%t)\n"
		}
		if control.guard != rolloutGuardMissing {
			if err := os.WriteFile(filepath.Join(directory, "warp-marker"), []byte(marker), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		directory, ok := directories[host.Name]
		if !ok {
			t.Errorf("rollout producer harness: unexpected synthetic host")
			return "", fmt.Errorf("synthetic harness contract failed")
		}
		if !strings.Contains(command, rolloutGuardMarker) || !strings.Contains(command, "rollout_unit_pattern='warp-synthetic-*.service'") {
			t.Errorf("rollout producer harness: wrong producer or environment scope")
			return "", fmt.Errorf("synthetic harness contract failed")
		}
		const installedPath = "warpctl_path=/usr/local/sbin/warpctl"
		if strings.Count(command, installedPath) != 1 {
			t.Errorf("rollout producer harness: installed-path relocation is not unique")
			return "", fmt.Errorf("synthetic harness contract failed")
		}
		warpPath := filepath.Join(directory, "warp-marker")
		command = strings.Replace(command, installedPath, "warpctl_path="+shellSingleQuote(warpPath), 1)
		control := controls[host.Name]
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		child := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		child.WaitDelay = 250 * time.Millisecond
		child.Env = []string{
			"PATH=" + directory, "LC_ALL=C", "ROLLOUT_TEST_TRACE=" + filepath.Join(directory, "calls"), "ROLLOUT_TEST_WARP=" + warpPath,
			fmt.Sprintf("ROLLOUT_TEST_FILES_STATUS=%d", control.filesStatus), fmt.Sprintf("ROLLOUT_TEST_RUNNING_STATUS=%d", control.runningStatus),
			fmt.Sprintf("ROLLOUT_TEST_FILES_ROWS=%t", control.filesRows), fmt.Sprintf("ROLLOUT_TEST_RUNNING_ROWS=%t", control.runningRows),
			fmt.Sprintf("ROLLOUT_TEST_ENV_STATUS=%d", control.environmentStatus), fmt.Sprintf("ROLLOUT_TEST_DISABLED=%t", control.disabled),
			fmt.Sprintf("ROLLOUT_TEST_STALE=%t", control.stale), fmt.Sprintf("ROLLOUT_TEST_START_UNAVAILABLE=%t", control.startUnavailable),
		}
		body, err := child.CombinedOutput()
		calls, readErr := os.ReadFile(filepath.Join(directory, "calls"))
		if readErr != nil || strings.Contains(string(calls), "UNEXPECTED") {
			t.Errorf("rollout producer harness: fake tool call contract failed")
			return "", fmt.Errorf("synthetic harness contract failed")
		}
		// A failed first file census may legitimately fail fast. A successful
		// file census requires the running census, independently of alert outcome.
		if !strings.Contains(string(calls), "list-unit-files ") || (control.filesStatus == 0 && !strings.Contains(string(calls), "list-units ")) {
			t.Errorf("rollout producer harness: discovery call sequence incomplete")
			return "", fmt.Errorf("synthetic harness contract failed")
		}
		return string(body), err
	}}
	settings := syntheticSettings(source)
	settings.Hosts = nil
	for name := range controls {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: name, Roles: []string{"services"}})
	}
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "synthetic-nonservices", Roles: []string{"redis-cluster"}})
	return settings
}

func TestRolloutGuardProducerDiscoveryFailureCannotBecomeUnmanaged(t *testing.T) {
	tests := []struct {
		name    string
		control rolloutProducerControl
	}{
		{"both-empty-failed", rolloutProducerControl{filesStatus: 17, runningStatus: 18, guard: rolloutGuardFull}},
		{"files-empty-failed", rolloutProducerControl{filesStatus: 17, guard: rolloutGuardFull}},
		{"running-empty-failed", rolloutProducerControl{runningStatus: 18, guard: rolloutGuardFull}},
		{"files-partial-failed", rolloutProducerControl{filesStatus: 17, filesRows: true, runningRows: true, guard: rolloutGuardFull}},
		{"running-partial-failed", rolloutProducerControl{runningStatus: 18, filesRows: true, runningRows: true, guard: rolloutGuardFull}},
		{"missing-command-status", rolloutProducerControl{filesStatus: 127, runningStatus: 127, guard: rolloutGuardFull}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := rolloutProducerSettings(t, map[string]rolloutProducerControl{"synthetic-service": test.control})
			alerts, err := NewRolloutGuardSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			requireAlertClass(t, alerts, "cannot-observe")
		})
	}
}

func TestRolloutGuardProducerEnvironmentFailureCannotCertifyGuard(t *testing.T) {
	settings := rolloutProducerSettings(t, map[string]rolloutProducerControl{"synthetic-service": {filesRows: true, runningRows: true, environmentStatus: 19, guard: rolloutGuardFull}})
	alerts, err := NewRolloutGuardSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	for _, alert := range alerts {
		if strings.Contains(alert.Markdown(), "synthetic-private-producer-error") {
			t.Fatal("private producer error escaped")
		}
	}
}

func TestRolloutGuardProducerHealthyAndUnsafeControls(t *testing.T) {
	tests := []struct {
		name    string
		control rolloutProducerControl
		class   string
	}{
		{"complete-zero-unmanaged", rolloutProducerControl{guard: rolloutGuardFull}, ""},
		{"complete-managed-full", rolloutProducerControl{filesRows: true, runningRows: true, guard: rolloutGuardFull}, ""},
		{"known-legacy", rolloutProducerControl{filesRows: true, runningRows: true, guard: rolloutGuardDrainOnly}, "rollout-guard-stale"},
		{"known-disabled", rolloutProducerControl{filesRows: true, runningRows: true, guard: rolloutGuardFull, disabled: true}, "rollout-guard-disabled"},
		{"missing-marker", rolloutProducerControl{filesRows: true, runningRows: true, guard: rolloutGuardMissing}, "rollout-guard-unverified"},
		{"unrecognized-marker", rolloutProducerControl{filesRows: true, runningRows: true, guard: rolloutGuardUnknown}, "rollout-guard-unverified"},
		{"stale-worker", rolloutProducerControl{filesRows: true, runningRows: true, guard: rolloutGuardFull, stale: true}, "rollout-guard-workers-stale"},
		{"unverifiable-worker", rolloutProducerControl{filesRows: true, runningRows: true, guard: rolloutGuardFull, startUnavailable: true}, "rollout-guard-workers-stale"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := rolloutProducerSettings(t, map[string]rolloutProducerControl{"synthetic-service": test.control})
			alerts, err := NewRolloutGuardSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if test.class == "" {
				if len(alerts) != 0 {
					t.Fatalf("successful complete control emitted %d alerts", len(alerts))
				}
				return
			}
			requireAlertClass(t, alerts, test.class)
		})
	}
}

func TestRolloutGuardProducerIndependentKnownGuardSurvives(t *testing.T) {
	settings := rolloutProducerSettings(t, map[string]rolloutProducerControl{
		"synthetic-unobserved":   {filesStatus: 17, runningStatus: 18, guard: rolloutGuardFull},
		"synthetic-known-legacy": {filesRows: true, runningRows: true, guard: rolloutGuardDrainOnly},
	})
	alerts, err := NewRolloutGuardSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "rollout-guard-stale")
	requireAlertClass(t, alerts, "cannot-observe")
}

func TestRolloutGuardNarrationMatchesPromotionLease(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("disabled-%t", disabled), func(t *testing.T) {
			control := rolloutProducerControl{filesRows: true, runningRows: true, guard: rolloutGuardDrainOnly, disabled: disabled}
			settings := rolloutProducerSettings(t, map[string]rolloutProducerControl{"synthetic-service": control})
			alerts, err := NewRolloutGuardSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			class := "rollout-guard-stale"
			if disabled {
				class = "rollout-guard-disabled"
			}
			alert := requireAlertClass(t, alerts, class)
			body := alert.Markdown()
			for _, wrong := range []string{"remains held through synchronous old-container drain", "candidate start through old-container drain is covered by one host lease"} {
				if strings.Contains(body, wrong) {
					t.Fatalf("finding contradicts startup/promotion lease with retirement outside: %s", wrong)
				}
			}
			for _, want := range []string{"candidate", "retirement", "outside the lease"} {
				if !strings.Contains(body, want) {
					t.Fatalf("finding omits supported lease boundary %q", want)
				}
			}
		})
	}
}
