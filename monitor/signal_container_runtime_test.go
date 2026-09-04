package monitor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var containerRuntimeFixtureKeys = []string{
	"observation_schema",
	"docker_active",
	"containerd_active",
	"docker_client",
	"docker_server",
	"containerd_client",
	"containerd_server",
	"docker_history_complete",
	"containerd_history_complete",
	"runtime_window_complete",
	"warp_window_complete",
	"running_warp_units",
	"warp_deploy_successes",
	"warp_start_failures",
	"warp_exit125_failures",
	"runtime_protocol_errors",
	"journal_window_seconds",
}

func containerRuntimeFixture(overrides map[string]string) string {
	values := map[string]string{
		"observation_schema":          "1",
		"docker_active":               "active",
		"containerd_active":           "active",
		"docker_client":               "29.8.0",
		"docker_server":               "29.8.0",
		"containerd_client":           "v2.3.4",
		"containerd_server":           "v2.3.4",
		"docker_history_complete":     "1",
		"containerd_history_complete": "1",
		"runtime_window_complete":     "1",
		"warp_window_complete":        "1",
		"running_warp_units":          "24",
		"warp_deploy_successes":       "2",
		"warp_start_failures":         "0",
		"warp_exit125_failures":       "0",
		"runtime_protocol_errors":     "0",
		"journal_window_seconds":      "600",
	}
	for key, value := range overrides {
		values[key] = value
	}
	lines := make([]string, 0, len(containerRuntimeFixtureKeys))
	for _, key := range containerRuntimeFixtureKeys {
		if value, ok := values[key]; ok {
			lines = append(lines, key+"="+value)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestContainerRuntimeSignalSyntheticCompatibilityClasses(t *testing.T) {
	observations := map[string]string{
		"fresh": containerRuntimeFixture(nil),
		"docker-drift": containerRuntimeFixture(map[string]string{
			"docker_server": "29.7.2",
		}),
		"docker-major-drift": containerRuntimeFixture(map[string]string{
			"docker_server": "28.7.2",
		}),
		"containerd-split": containerRuntimeFixture(map[string]string{
			"containerd_client": "v2.3.3",
			"containerd_server": "v2.2.6",
		}),
		"failed-start": containerRuntimeFixture(map[string]string{
			"warp_start_failures":   "3",
			"warp_exit125_failures": "2",
		}),
		"native-ttrpc": containerRuntimeFixture(map[string]string{
			"runtime_protocol_errors": "4",
		}),
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if !strings.Contains(command, containerRuntimeMarker) {
			return "", errors.New("unexpected synthetic host command")
		}
		return observations[host.Name], nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "fresh", Roles: []string{"services"}},
		{Name: "docker-drift", Roles: []string{"services"}},
		{Name: "docker-major-drift", Roles: []string{"services"}},
		{Name: "containerd-split", Roles: []string{"services"}},
		{Name: "failed-start", Roles: []string{"services"}},
		{Name: "native-ttrpc", Roles: []string{"services"}},
	}

	alerts, err := NewContainerRuntimeSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 5 {
		t.Fatalf("alerts = %d, want two diagnostics and three incompatible: %+v", len(alerts), alerts)
	}
	diagnostics := map[string]Alert{}
	for _, alert := range alerts {
		if alert.Class == "container-runtime-refresh-pending" {
			diagnostics[alert.Target] = alert
		}
	}
	for _, target := range []string{"docker-drift", "docker-major-drift"} {
		diagnostic := diagnostics[target]
		if diagnostic.Severity != SeverityWarn {
			t.Fatalf("Docker diagnostic for %s = %+v", target, diagnostic)
		}
		for _, want := range []string{
			"not by itself proof",
			"A success predating the package transition does not prove",
			"do not reboot from this warning alone",
		} {
			if !strings.Contains(diagnostic.Markdown(), want) {
				t.Errorf("Docker diagnostic lacks %q: %s", want, diagnostic.Markdown())
			}
		}
	}
	if !strings.Contains(diagnostics["docker-drift"].Evidence, "29.8.0/29.7.2") ||
		!strings.Contains(diagnostics["docker-major-drift"].Evidence, "29.8.0/28.7.2") {
		t.Errorf("Docker version diagnostics = %+v", diagnostics)
	}

	pageTargets := map[string]Alert{}
	for _, alert := range alerts {
		if alert.Class == "container-runtime-incompatible" {
			pageTargets[alert.Target] = alert
		}
	}
	for _, target := range []string{"containerd-split", "failed-start", "native-ttrpc"} {
		if pageTargets[target].Severity != SeverityPage {
			t.Errorf("%s did not PAGE: %+v", target, pageTargets[target])
		}
	}
	if !strings.Contains(pageTargets["containerd-split"].Evidence, "v2.3.3/v2.2.6") {
		t.Errorf("containerd split evidence = %q", pageTargets["containerd-split"].Evidence)
	}
	if !strings.Contains(pageTargets["failed-start"].Evidence, "recent replacement start failures=3") ||
		!strings.Contains(pageTargets["failed-start"].Observed, "exit125_failures=2") {
		t.Errorf("replacement failure evidence = %+v", pageTargets["failed-start"])
	}
	if !strings.Contains(pageTargets["native-ttrpc"].Evidence, "recent runtime protocol errors=4") {
		t.Errorf("TTRPC evidence = %q", pageTargets["native-ttrpc"].Evidence)
	}
}

func TestContainerRuntimeSignalSyntheticMissingAmbiguousAndMalformedAreVisibility(t *testing.T) {
	missing := strings.Replace(containerRuntimeFixture(nil), "docker_server=29.8.0\n", "", 1)
	duplicate := containerRuntimeFixture(nil) + "docker_server=customer-secret\n"
	malformed := containerRuntimeFixture(map[string]string{"warp_start_failures": "not-a-count"})
	observations := map[string]string{
		"missing":   missing,
		"duplicate": duplicate,
		"malformed": malformed,
	}
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		return observations[host.Name], nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "missing", Roles: []string{"services"}},
		{Name: "duplicate", Roles: []string{"services"}},
		{Name: "malformed", Roles: []string{"services"}},
	}

	alerts, err := NewContainerRuntimeSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 3 {
		t.Fatalf("visibility alerts = %d, want 3: %+v", len(alerts), alerts)
	}
	for _, alert := range alerts {
		if alert.Class != "cannot-observe" || alert.Severity != SeverityWarn {
			t.Errorf("invalid observation was classified as production health: %+v", alert)
		}
		if strings.Contains(alert.Markdown(), "customer-secret") {
			t.Errorf("duplicate raw value leaked into visibility alert: %s", alert.Markdown())
		}
	}
}

func TestContainerRuntimeSignalSyntheticMissingObservation(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return "", errors.New("synthetic SSH unavailable")
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "edge", Roles: []string{"services"}}}

	alerts, err := NewContainerRuntimeSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "cannot-observe")
	if alert.Target != "edge/container-runtime" {
		t.Fatalf("target = %q", alert.Target)
	}
}

func TestContainerRuntimeSignalSyntheticVacuumedStartupIsOptionalButTruncationIsVisibility(t *testing.T) {
	observations := map[string]string{
		"missing-startup": containerRuntimeFixture(map[string]string{
			"docker_server":               "-",
			"containerd_server":           "-",
			"docker_history_complete":     "0",
			"containerd_history_complete": "0",
		}),
		"missing-client": containerRuntimeFixture(map[string]string{
			"containerd_client": "-",
		}),
		"truncated-clean": containerRuntimeFixture(map[string]string{
			"runtime_window_complete": "0",
			"warp_window_complete":    "0",
		}),
		"truncated-failure": containerRuntimeFixture(map[string]string{
			"runtime_window_complete": "0",
			"runtime_protocol_errors": "1",
		}),
	}
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		return observations[host.Name], nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "missing-startup", Roles: []string{"services"}},
		{Name: "missing-client", Roles: []string{"services"}},
		{Name: "truncated-clean", Roles: []string{"services"}},
		{Name: "truncated-failure", Roles: []string{"services"}},
	}

	alerts, err := NewContainerRuntimeSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	classesByTarget := map[string]map[string]bool{}
	for _, alert := range alerts {
		if classesByTarget[alert.Target] == nil {
			classesByTarget[alert.Target] = map[string]bool{}
		}
		classesByTarget[alert.Target][alert.Class] = true
	}
	if len(classesByTarget["missing-startup"]) != 0 {
		t.Fatalf("expected one-hour startup vacuum emitted an alert: %+v", alerts)
	}
	if !classesByTarget["missing-client/container-runtime-containerd-client"]["cannot-observe"] {
		t.Fatalf("missing installed client was not visibility: %+v", alerts)
	}
	if !classesByTarget["truncated-clean/container-runtime-journal-window"]["cannot-observe"] ||
		!classesByTarget["truncated-clean/container-runtime-warp-window"]["cannot-observe"] {
		t.Fatalf("truncated clean windows were treated as healthy: %+v", alerts)
	}
	if !classesByTarget["truncated-failure"]["container-runtime-incompatible"] ||
		!classesByTarget["truncated-failure/container-runtime-journal-window"]["cannot-observe"] {
		t.Fatalf("concrete failure was hidden by truncation: %+v", alerts)
	}
	for _, alert := range alerts {
		if alert.Target == "missing-startup" || alert.Target == "truncated-clean" {
			t.Errorf("unknown history emitted a production failure: %+v", alert)
		}
	}
}

func TestContainerRuntimeCommandUsesOnlyUnprivilegedObservationBoundaries(t *testing.T) {
	for _, forbidden := range []string{
		"/proc/",
		"docker ps",
		"sudo",
		"docker version",
		"-n 10001",
		"mktemp",
		"journal_stdout",
		"journal_stderr",
	} {
		if strings.Contains(containerRuntimeCommand, forbidden) {
			t.Errorf("container runtime command contains privileged boundary %q", forbidden)
		}
	}
	for _, required := range []string{
		"docker --version",
		"containerd --version",
		"systemctl list-units",
		"journalctl -b -n 0 --no-pager --quiet --output-fields=MESSAGE -o json",
		"[ \"$journal_baseline_status\" -ne 0 ] || [ -n \"$journal_baseline\" ]",
		"read_bounded_journal 2001 -b SYSLOG_IDENTIFIER=dockerd",
		"read_bounded_journal 2001 -b SYSLOG_IDENTIFIER=containerd",
		"-n 0 --no-pager --quiet",
		"[ \"$journal_status\" -ne 0 ] && [ \"$journal_status\" -ne 1 ]",
		"[ \"$journal_status\" -eq 1 ] && [ -z \"$journal_output\" ]",
		"$0 !~ /\"MESSAGE\"[[:space:]]*:/",
		"_COMM=warpctl",
		"--grep='Start container failed:|Deploy success version=') || exit 24",
		"runtime_window_complete",
		"warp_window_complete",
		"Deploy success version=",
		"Start container failed:",
		"exit status 125",
		"failed to create TTRPC connection: unsupported protocol",
	} {
		if !strings.Contains(containerRuntimeCommand, required) {
			t.Errorf("container runtime command lacks %q", required)
		}
	}
}

func TestContainerRuntimeCommandParsesProductionShapedUnprivilegedSources(t *testing.T) {
	output, err := runContainerRuntimeCommandFixture(t, "positive")
	if err != nil {
		t.Fatalf("container runtime command: %v\n%s", err, output)
	}
	sample, err := parseContainerRuntimeSample(string(output))
	if err != nil {
		t.Fatalf("parse production-shaped command output: %v\n%s", err, output)
	}
	if sample.dockerClient != "29.8.0" || sample.dockerServer != "29.8.0" ||
		sample.containerdClient != "v2.3.4" || sample.containerdServer != "v2.3.4" ||
		sample.runningWarpUnits != 1 || sample.warpDeploySuccesses != 1 ||
		sample.warpStartFailures != 0 || sample.runtimeProtocolErrors != 0 {
		t.Fatalf("unexpected production-shaped sample: %+v\n%s", sample, output)
	}
}

func TestContainerRuntimeCommandDistinguishesSystemd249NoMatchFromJournalFailure(t *testing.T) {
	output, err := runContainerRuntimeCommandFixture(t, "no-match")
	if err != nil {
		t.Fatalf("systemd 249 no-match result failed: %v\n%s", err, output)
	}
	sample, err := parseContainerRuntimeSample(string(output))
	if err != nil {
		t.Fatalf("parse no-match command output: %v\n%s", err, output)
	}
	if sample.warpDeploySuccesses != 0 || sample.warpStartFailures != 0 ||
		sample.warpExit125Failures != 0 || !sample.warpWindowComplete {
		t.Fatalf("systemd 249 no-match result was not an observed empty window: %+v", sample)
	}

	for _, mode := range []string{
		"baseline-failure",
		"baseline-stderr",
		"preflight-failure",
		"preflight-stderr",
		"query-failure",
		"query-stderr",
		"malformed-output",
	} {
		t.Run(mode, func(t *testing.T) {
			output, err := runContainerRuntimeCommandFixture(t, mode)
			if err == nil {
				t.Fatalf("journal %s was treated as an empty observation:\n%s", mode, output)
			}
			var exitError *exec.ExitError
			expectedExitCode := 24
			if strings.HasPrefix(mode, "baseline-") {
				expectedExitCode = 20
			}
			if !errors.As(err, &exitError) || exitError.ExitCode() != expectedExitCode {
				t.Fatalf("journal %s error = %v, want command exit %d\n%s", mode, err, expectedExitCode, output)
			}
			if strings.Contains(string(output), "synthetic-private-journal-error") {
				t.Fatalf("journal %s leaked raw stderr: %s", mode, output)
			}
		})
	}
}

func runContainerRuntimeCommandFixture(t *testing.T, journalMode string) ([]byte, error) {
	t.Helper()
	binDir := t.TempDir()
	commands := map[string]string{
		"systemctl": `#!/bin/sh
case "$1" in
  is-active) echo active ;;
  show)
    case "$2" in
      docker.service) echo 11111111111111111111111111111111 ;;
      containerd.service) echo 22222222222222222222222222222222 ;;
      *) exit 2 ;;
    esac
    ;;
  list-units) echo 'warp-main-api-g1.service loaded active running Warpctl' ;;
  *) exit 2 ;;
esac
`,
		"docker": `#!/bin/sh
echo 'Docker version 29.8.0, build synthetic'
`,
		"containerd": `#!/bin/sh
echo 'containerd github.com/containerd/containerd/v2 v2.3.4 synthetic'
`,
		"timeout": `#!/bin/sh
shift
exec "$@"
`,
		"journalctl": `#!/bin/sh
case " $* " in
  *' -n 0 '*)
    case " $* " in
      *' SYSLOG_IDENTIFIER='*|*' _COMM='*)
        case "${CONTAINER_RUNTIME_JOURNAL_MODE}: $* " in
          no-match:*'_COMM=warpctl '*) exit 1 ;;
          preflight-failure:*'_COMM=warpctl '*)
            echo 'synthetic-private-journal-error' >&2
            exit 1
            ;;
          preflight-stderr:*'_COMM=warpctl '*)
            echo 'synthetic-private-journal-error' >&2
            exit 0
            ;;
        esac
        ;;
      *)
        case "${CONTAINER_RUNTIME_JOURNAL_MODE}" in
          baseline-failure)
            echo 'synthetic-private-journal-error' >&2
            exit 2
            ;;
          baseline-stderr)
            echo 'synthetic-private-journal-error' >&2
            exit 0
            ;;
        esac
        ;;
    esac
    exit 0
    ;;
  *' _COMM=warpctl '*)
    case "${CONTAINER_RUNTIME_JOURNAL_MODE}" in
      positive)
        echo '{"MESSAGE":"Deploy success version=synthetic, configVersion=synthetic"}'
        ;;
      no-match) exit 1 ;;
      query-failure)
        echo 'synthetic-private-journal-error' >&2
        exit 1
        ;;
      query-stderr)
        echo '{"MESSAGE":"Deploy success version=partial, configVersion=partial"}'
        echo 'synthetic-private-journal-error' >&2
        exit 0
        ;;
      malformed-output)
        echo '{"MESSAGE":"truncated"'
        exit 0
        ;;
      *) exit 2 ;;
    esac
    ;;
  *' SYSLOG_IDENTIFIER=dockerd '*) echo '{"MESSAGE":"time=synthetic level=info msg=Docker daemon version=29.8.0"}' ;;
  *' SYSLOG_IDENTIFIER=containerd '*) echo '{"MESSAGE":"time=synthetic level=info msg=starting containerd version=v2.3.4"}' ;;
  *) exit 2 ;;
esac
`,
	}
	for name, body := range commands {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	command := exec.Command("sh", "-c", containerRuntimeCommand)
	command.Env = append(
		os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"CONTAINER_RUNTIME_JOURNAL_MODE="+journalMode,
	)
	return command.CombinedOutput()
}
