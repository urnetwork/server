package monitor

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §8.5a maps to signal_container_runtime.go and
// signal_container_runtime_test.go. Package replacement and runtime protocol
// compatibility are deliberately separate signals: neither grants authority
// to reboot a production host.
func NewContainerRuntimeSignal() Signal {
	return &signalAdapter{
		number: "8.5a", key: "container-runtime", name: "Container runtime compatibility",
		probe: containerRuntimeProbe{},
	}
}

type containerRuntimeProbe struct{}

func (containerRuntimeProbe) id() string             { return "host/container-runtime" }
func (containerRuntimeProbe) tier() string           { return tierWarn }
func (containerRuntimeProbe) cadence() time.Duration { return 5 * time.Minute }

const containerRuntimeMarker = "monitor-signal-8.5a-container-runtime"

// containerRuntimeCommand deliberately stays inside the unprivileged monitor
// account's observation boundary. Live daemon versions come from their
// current-boot startup journals; docker/containerd executables provide the
// installed client side. Warp's systemd journal, not the Docker socket, owns
// replacement-start outcomes.
const containerRuntimeCommand = `# ` + containerRuntimeMarker + `
set -u

docker_active=$(systemctl is-active docker.service 2>/dev/null || true)
containerd_active=$(systemctl is-active containerd.service 2>/dev/null || true)

docker_client=$(timeout 10 docker --version 2>/dev/null | awk '
  /^Docker version / { value=$3; sub(/,$/, "", value); print value; exit }
')
containerd_client=$(timeout 10 containerd --version 2>/dev/null | awk '
  { for (i=1; i<=NF; i++) if ($i ~ /^v?[0-9]+[.][0-9]+[.][0-9]+/) { print $i; exit } }
')

journal_baseline=$(journalctl -b -n 0 --no-pager --quiet --output-fields=MESSAGE -o json 2>&1)
journal_baseline_status=$?
if [ "$journal_baseline_status" -ne 0 ] || [ -n "$journal_baseline" ]; then
	exit 20
fi

read_bounded_journal() {
	journal_limit=$1
	shift
	journal_output=$(journalctl "$@" -n 0 --no-pager --quiet --output-fields=MESSAGE -o json 2>&1)
	journal_status=$?
	if [ -n "$journal_output" ] || { [ "$journal_status" -ne 0 ] && [ "$journal_status" -ne 1 ]; }; then
		return 2
	fi
	journal_output=$(journalctl "$@" -n "$journal_limit" --no-pager --quiet --output-fields=MESSAGE -o json 2>&1)
	journal_status=$?
	if [ "$journal_status" -eq 1 ] && [ -z "$journal_output" ]; then
		# systemd 249 returns one for a valid --grep query with no matches.
		return 0
	fi
	[ "$journal_status" -eq 0 ] || return 2
	if [ -n "$journal_output" ] && ! printf '%s\n' "$journal_output" | awk '
		NF && ($0 !~ /^[[:space:]]*[{].*[}][[:space:]]*$/ || $0 !~ /"MESSAGE"[[:space:]]*:/) { invalid=1 }
		END { exit invalid }
	'; then
		return 2
	fi
	printf '%s\n' "$journal_output"
}

docker_history=$(read_bounded_journal 2001 -b SYSLOG_IDENTIFIER=dockerd) || exit 21
containerd_history=$(read_bounded_journal 2001 -b SYSLOG_IDENTIFIER=containerd) || exit 22
docker_history_rows=$(printf '%s\n' "$docker_history" | awk 'NF { count++ } END { print count+0 }')
containerd_history_rows=$(printf '%s\n' "$containerd_history" | awk 'NF { count++ } END { print count+0 }')
docker_history_complete=1
containerd_history_complete=1
[ "$docker_history_rows" -lt 2001 ] || docker_history_complete=0
[ "$containerd_history_rows" -lt 2001 ] || containerd_history_complete=0
docker_server=$(printf '%s\n' "$docker_history" | awk '
  /Docker daemon/ && match($0, /version=v?[0-9]+[.][0-9]+[.][0-9]+/) {
    latest=substr($0, RSTART+8, RLENGTH-8)
  }
  END { print latest }
')
containerd_server=$(printf '%s\n' "$containerd_history" | awk '
  /starting containerd/ && match($0, /version=v?[0-9]+[.][0-9]+[.][0-9]+/) {
    latest=substr($0, RSTART+8, RLENGTH-8)
  }
  END { print latest }
')

runtime_recent=$(read_bounded_journal 2001 -b --since '10 minutes ago' SYSLOG_IDENTIFIER=dockerd SYSLOG_IDENTIFIER=containerd) || exit 25
runtime_recent_rows=$(printf '%s\n' "$runtime_recent" | awk 'NF { count++ } END { print count+0 }')
runtime_window_complete=1
[ "$runtime_recent_rows" -lt 2001 ] || runtime_window_complete=0
runtime_protocol_errors=$(printf '%s\n' "$runtime_recent" | awk '
  /failed to create TTRPC connection: unsupported protocol/ { count++ }
  END { print count+0 }
')

warp_units=$(systemctl list-units --type=service --state=running --no-legend --no-pager --plain 'warp-main-*.service' 2>/dev/null) || exit 23
running_warp_units=$(printf '%s\n' "$warp_units" | awk '$1 ~ /[.]service$/ { count++ } END { print count+0 }')
warp_journal=$(read_bounded_journal 2001 -b --since '10 minutes ago' _COMM=warpctl \
	--grep='Start container failed:|Deploy success version=') || exit 24
warp_journal_rows=$(printf '%s\n' "$warp_journal" | awk 'NF { count++ } END { print count+0 }')
warp_window_complete=1
[ "$warp_journal_rows" -lt 2001 ] || warp_window_complete=0
warp_deploy_successes=$(printf '%s\n' "$warp_journal" | awk '/Deploy success version=/ { count++ } END { print count+0 }')
warp_start_failures=$(printf '%s\n' "$warp_journal" | awk '/Start container failed:/ { count++ } END { print count+0 }')
warp_exit125_failures=$(printf '%s\n' "$warp_journal" | awk '/Start container failed:.*exit status 125/ { count++ } END { print count+0 }')

printf '%s\n' \
  'observation_schema=1' \
  "docker_active=${docker_active}" \
  "containerd_active=${containerd_active}" \
  "docker_client=${docker_client:--}" \
  "docker_server=${docker_server:--}" \
  "containerd_client=${containerd_client:--}" \
  "containerd_server=${containerd_server:--}" \
  "docker_history_complete=${docker_history_complete}" \
  "containerd_history_complete=${containerd_history_complete}" \
  "runtime_window_complete=${runtime_window_complete}" \
  "warp_window_complete=${warp_window_complete}" \
  "running_warp_units=${running_warp_units}" \
  "warp_deploy_successes=${warp_deploy_successes}" \
  "warp_start_failures=${warp_start_failures}" \
  "warp_exit125_failures=${warp_exit125_failures}" \
  "runtime_protocol_errors=${runtime_protocol_errors}" \
  'journal_window_seconds=600'
`

type containerRuntimeResult struct {
	host *host
	raw  string
	err  error
}

type containerRuntimeSample struct {
	dockerActive              string
	containerdActive          string
	dockerClient              string
	dockerServer              string
	containerdClient          string
	containerdServer          string
	dockerHistoryComplete     bool
	containerdHistoryComplete bool
	runtimeWindowComplete     bool
	warpWindowComplete        bool
	runningWarpUnits          int
	warpDeploySuccesses       int
	warpStartFailures         int
	warpExit125Failures       int
	runtimeProtocolErrors     int
	journalWindowSeconds      int
}

func (containerRuntimeProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	targets := env.cfg.hostsWithRole("services")
	if len(targets) == 0 {
		return nil, fmt.Errorf("container runtime: no services hosts in inventory")
	}

	results := make(chan containerRuntimeResult, len(targets))
	for _, target := range targets {
		target := target
		go func() {
			raw, err := env.runner.shell(ctx, target, containerRuntimeCommand)
			results <- containerRuntimeResult{host: target, raw: raw, err: err}
		}()
	}

	collected := make([]containerRuntimeResult, 0, len(targets))
	for range targets {
		collected = append(collected, <-results)
	}
	sort.Slice(collected, func(i, j int) bool {
		return collected[i].host.name < collected[j].host.name
	})

	findings := make([]finding, 0, len(collected))
	for _, result := range collected {
		if result.err != nil {
			findings = append(findings, cannotObserveFinding(
				result.host.name+"/container-runtime", result.err,
			))
			continue
		}
		sample, err := parseContainerRuntimeSample(result.raw)
		if err != nil {
			findings = append(findings, cannotObserveFinding(
				result.host.name+"/container-runtime", err,
			))
			continue
		}
		findings = append(findings, evaluateContainerRuntime(result.host.name, sample)...)
	}
	return findings, nil
}

var containerRuntimeVersionPattern = regexp.MustCompile(`^v?[0-9]+[.][0-9]+[.][0-9]+([-+][0-9A-Za-z._-]+)?$`)

func parseContainerRuntimeSample(raw string) (containerRuntimeSample, error) {
	required := map[string]bool{
		"observation_schema":          false,
		"docker_active":               false,
		"containerd_active":           false,
		"docker_client":               false,
		"docker_server":               false,
		"containerd_client":           false,
		"containerd_server":           false,
		"docker_history_complete":     false,
		"containerd_history_complete": false,
		"runtime_window_complete":     false,
		"warp_window_complete":        false,
		"running_warp_units":          false,
		"warp_deploy_successes":       false,
		"warp_start_failures":         false,
		"warp_exit125_failures":       false,
		"runtime_protocol_errors":     false,
		"journal_window_seconds":      false,
	}
	values := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return containerRuntimeSample{}, fmt.Errorf("container runtime: malformed observation line")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		seen, known := required[key]
		if !known {
			return containerRuntimeSample{}, fmt.Errorf("container runtime: unexpected observation field")
		}
		if seen {
			return containerRuntimeSample{}, fmt.Errorf("container runtime: duplicate %s field", key)
		}
		required[key] = true
		values[key] = value
	}
	for key, seen := range required {
		if !seen || values[key] == "" {
			return containerRuntimeSample{}, fmt.Errorf("container runtime: observation omitted %s", key)
		}
	}
	if values["observation_schema"] != "1" {
		return containerRuntimeSample{}, fmt.Errorf("container runtime: unsupported observation schema")
	}
	for _, key := range []string{"docker_active", "containerd_active"} {
		if !containerRuntimeActiveState(values[key]) {
			return containerRuntimeSample{}, fmt.Errorf("container runtime: invalid %s", key)
		}
	}
	for _, key := range []string{"docker_client", "docker_server", "containerd_client", "containerd_server"} {
		if values[key] != "-" && !containerRuntimeVersionPattern.MatchString(values[key]) {
			return containerRuntimeSample{}, fmt.Errorf("container runtime: invalid %s", key)
		}
	}

	sample := containerRuntimeSample{
		dockerActive:     values["docker_active"],
		containerdActive: values["containerd_active"],
		dockerClient:     values["docker_client"],
		dockerServer:     values["docker_server"],
		containerdClient: values["containerd_client"],
		containerdServer: values["containerd_server"],
	}
	booleans := map[string]*bool{
		"docker_history_complete":     &sample.dockerHistoryComplete,
		"containerd_history_complete": &sample.containerdHistoryComplete,
		"runtime_window_complete":     &sample.runtimeWindowComplete,
		"warp_window_complete":        &sample.warpWindowComplete,
	}
	for key, destination := range booleans {
		switch values[key] {
		case "0":
			*destination = false
		case "1":
			*destination = true
		default:
			return containerRuntimeSample{}, fmt.Errorf("container runtime: invalid %s", key)
		}
	}
	integers := map[string]*int{
		"running_warp_units":      &sample.runningWarpUnits,
		"warp_deploy_successes":   &sample.warpDeploySuccesses,
		"warp_start_failures":     &sample.warpStartFailures,
		"warp_exit125_failures":   &sample.warpExit125Failures,
		"runtime_protocol_errors": &sample.runtimeProtocolErrors,
		"journal_window_seconds":  &sample.journalWindowSeconds,
	}
	for key, destination := range integers {
		value, err := strconv.Atoi(values[key])
		if err != nil || value < 0 {
			return containerRuntimeSample{}, fmt.Errorf("container runtime: invalid %s", key)
		}
		*destination = value
	}
	if sample.journalWindowSeconds != 600 {
		return containerRuntimeSample{}, fmt.Errorf("container runtime: invalid journal_window_seconds")
	}
	if sample.warpExit125Failures > sample.warpStartFailures {
		return containerRuntimeSample{}, fmt.Errorf("container runtime: inconsistent Warp failure counts")
	}
	return sample, nil
}

func containerRuntimeActiveState(value string) bool {
	switch value {
	case "active", "activating", "deactivating", "failed", "inactive", "maintenance", "reloading", "unknown":
		return true
	default:
		return false
	}
}

func containerRuntimeVersion(value string) string {
	return strings.TrimPrefix(value, "v")
}

func (sample containerRuntimeSample) observed() string {
	return fmt.Sprintf(
		"docker_active=%s docker_client=%s docker_server=%s docker_history_complete=%t containerd_active=%s containerd_client=%s containerd_server=%s containerd_history_complete=%t running_warp_units=%d window=%ds runtime_window_complete=%t warp_window_complete=%t deploy_successes=%d start_failures=%d exit125_failures=%d protocol_errors=%d",
		sample.dockerActive, sample.dockerClient, sample.dockerServer,
		sample.dockerHistoryComplete, sample.containerdActive, sample.containerdClient,
		sample.containerdServer, sample.containerdHistoryComplete, sample.runningWarpUnits,
		sample.journalWindowSeconds, sample.runtimeWindowComplete, sample.warpWindowComplete, sample.warpDeploySuccesses,
		sample.warpStartFailures, sample.warpExit125Failures, sample.runtimeProtocolErrors,
	)
}

func evaluateContainerRuntime(target string, sample containerRuntimeSample) []finding {
	issues := []string{}
	diagnostics := []string{}
	visibility := []finding{}
	if sample.dockerActive != "active" {
		issues = append(issues, "docker service="+sample.dockerActive)
	}
	if sample.containerdActive != "active" {
		issues = append(issues, "containerd service="+sample.containerdActive)
	}

	if sample.dockerClient == "-" {
		visibility = append(visibility, cannotObserveFinding(
			target+"/container-runtime-docker-client",
			fmt.Errorf("installed Docker client version is unavailable"),
		))
	} else if sample.dockerServer != "-" {
		dockerClient, dockerServer := containerRuntimeVersion(sample.dockerClient), containerRuntimeVersion(sample.dockerServer)
		if dockerClient != dockerServer {
			message := fmt.Sprintf("docker client/server=%s/%s", sample.dockerClient, sample.dockerServer)
			diagnostics = append(diagnostics, message)
		}
	}
	if sample.containerdClient == "-" {
		visibility = append(visibility, cannotObserveFinding(
			target+"/container-runtime-containerd-client",
			fmt.Errorf("installed containerd client version is unavailable"),
		))
	} else if sample.containerdServer != "-" {
		containerdClient, containerdServer := containerRuntimeVersion(sample.containerdClient), containerRuntimeVersion(sample.containerdServer)
		if containerdClient != containerdServer {
			issues = append(issues, fmt.Sprintf(
				"containerd client/server=%s/%s", sample.containerdClient, sample.containerdServer,
			))
		}
	}
	if sample.runningWarpUnits < 1 {
		issues = append(issues, fmt.Sprintf("running Warp units=%d", sample.runningWarpUnits))
	}
	if sample.runtimeProtocolErrors > 0 {
		issues = append(issues, fmt.Sprintf("recent runtime protocol errors=%d", sample.runtimeProtocolErrors))
	}
	if sample.warpStartFailures > 0 {
		issues = append(issues, fmt.Sprintf("recent replacement start failures=%d", sample.warpStartFailures))
	}
	if !sample.runtimeWindowComplete {
		visibility = append(visibility, cannotObserveFinding(
			target+"/container-runtime-journal-window",
			fmt.Errorf("bounded runtime journal reached its 2000-entry limit"),
		))
	}
	if !sample.warpWindowComplete {
		visibility = append(visibility, cannotObserveFinding(
			target+"/container-runtime-warp-window",
			fmt.Errorf("bounded failure/outcome journal reached its 2000-entry limit"),
		))
	}

	observed := sample.observed()
	findings := []finding{}
	if len(issues) > 0 {
		evidence := "incompatible: " + strings.Join(issues, ", ")
		if len(diagnostics) > 0 {
			evidence += "; diagnostic: " + strings.Join(diagnostics, ", ")
		}
		findings = append(findings, finding{
			probeId: "host/container-runtime", tier: tierPage,
			class: "container-runtime-incompatible", target: target, sustain: 1,
			symptom:   fmt.Sprintf("%s cannot safely launch or supervise replacement containers", target),
			mechanism: "Containerd's installed client and live daemon protocols differ, a runtime service is unavailable, or Warp recorded a concrete replacement-container failure. The historical 2.3 shim against a 2.2 daemon rejected every new container even while existing containers and Warp units appeared active.",
			baseline:  "Docker and containerd services are active, any still-retained containerd client/server versions match, at least one main Warp unit is running, and the bounded runtime and Warp journals contain no bootstrap or replacement-start failure.",
			observed:  observed,
			evidence:  evidence,
			context:   "Docker client/server drift alone is diagnostic because API compatibility can span Docker releases. A concrete Warp start failure, native TTRPC rejection, service loss, or any containerd split remains page-worthy.",
			action:    "Stop new deployments to this host and prove the exact runtime split or failed start. Do not reboot automatically. With explicit operator authorization, recover one host at a time by restarting or rebooting its runtime, then wait for its replacement containers before touching another host.",
			verify:    "Use the privileged maintenance probe to require matching containerd client/server versions, then require active runtime services, running Warp units, one successful replacement start after the affected transition, and zero new start or bootstrap-protocol failures.",
			playbook:  "SIGNALS.md §8.5a",
		})
	}
	if len(issues) == 0 && len(diagnostics) > 0 {
		findings = append(findings, finding{
			probeId: "host/container-runtime-refresh", tier: tierWarn,
			class: "container-runtime-refresh-pending", target: target, sustain: 2,
			symptom:   fmt.Sprintf("%s has Docker client/live-daemon version drift without a concrete replacement-start failure", target),
			mechanism: "The on-disk Docker client and the current-boot daemon report different versions. That is package-transition evidence, not by itself proof of a protocol failure or authority to reboot.",
			baseline:  "Docker client and live daemon versions converge after the host's separately authorized restart; compatible pending refreshes never cause an Ansible-initiated reboot.",
			observed:  observed,
			evidence:  "diagnostic: " + strings.Join(diagnostics, ", "),
			context:   fmt.Sprintf("The bounded Warp journal reports %d successful deploy(s), %d start failure(s), and %d exit-125 failure(s). A success predating the package transition does not prove a post-transition create.", sample.warpDeploySuccesses, sample.warpStartFailures, sample.warpExit125Failures),
			action:    "Keep the host in observation and let the separately authorized staggered restart policy own refresh. Escalate immediately if containerd versions diverge, a replacement start fails, or a bootstrap-protocol error appears; do not reboot from this warning alone.",
			verify:    "After the authorized restart, require matching versions and running Warp units. Independently require one successful replacement start after the package transition and no new start or runtime-protocol errors.",
			playbook:  "SIGNALS.md §8.5a",
		})
	}
	return append(findings, visibility...)
}
