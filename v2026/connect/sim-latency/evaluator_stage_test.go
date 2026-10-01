package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Extract the evaluator's real function, so the fixture supplies only external
// dependencies and cannot silently substitute a simplified control flow.
func evaluatorShellFunction(t *testing.T, name string) string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("evaluator", "container", "evaluator.sh"))
	if err != nil {
		t.Fatal(err)
	}
	marker := "\n" + name + "() {\n"
	start := strings.Index(string(script), marker)
	if start < 0 {
		t.Fatalf("evaluator function %s missing", name)
	}
	end := strings.Index(string(script[start+1:]), "\n}\n")
	if end < 0 {
		t.Fatalf("evaluator function %s not terminated", name)
	}
	return string(script[start+1 : start+1+end+3])
}

// The epoch control must finish before a submitted build can heat, exhaust,
// or otherwise perturb the dedicated measurement host.
func TestEvaluatorMeasuresRoundBaselineBeforeCandidateBuild(t *testing.T) {
	scriptBytes, err := os.ReadFile(filepath.Join("evaluator", "container", "evaluator.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	baselineSeal := strings.Index(script, "\nseal_evaluation_source \"$baseline_source_root\" \"$base_sha\" \"\"\n")
	baselineRun := strings.Index(script, "\n        run_stage baseline \"$i\"")
	buildCall := strings.Index(script, "\nrm -f -- \"$score_runner_env\" \"$score_compose_env\"\nbuild_candidate\n")
	candidateRun := strings.Index(script, "\n    run_stage candidate \"$i\"")
	if baselineSeal < 0 || baselineRun < 0 || buildCall < 0 || candidateRun < 0 {
		t.Fatalf("evaluator is missing a required baseline/build stage marker")
	}
	if !(baselineSeal < baselineRun && baselineRun < buildCall && buildCall < candidateRun) {
		t.Fatalf(
			"unsafe evaluator order: baseline seal=%d baseline run=%d build=%d candidate run=%d",
			baselineSeal,
			baselineRun,
			buildCall,
			candidateRun,
		)
	}
	if !strings.Contains(
		script,
		"for relative in baseline.json submission-error.json evaluation-progress.json evaluation.complete.json evidence-manifest.json; do",
	) {
		t.Fatal("terminal candidate build failure does not retain the completed round baseline")
	}
}

// Controls only synthetic Docker/cgroup observations; no daemon, mounts, live
// credentials or production services are reachable through the command stub.
type evaluatorStageFixture struct {
	role              string
	exitCode          int
	waitOutput        string
	fastExit          bool
	oomKills          int
	preRunOomKills    int
	postgresOomKills  int
	redisOomKills     int
	missingOomCounter string
	attempt           int
	mutateInspection  func([]map[string]any)
}

// The FIFO proves the background sampler published its data before Docker
// wait completes. Fast-exit cases deliberately never create a runner cgroup.
func runEvaluatorStageFixture(t *testing.T, fixture evaluatorStageFixture) (string, []byte, error) {
	t.Helper()
	root := t.TempDir()
	if fixture.role == "" {
		fixture.role = "candidate"
	}
	if fixture.attempt == 0 {
		fixture.attempt = 1
	}
	// Share this synthetic identity with the shell instead of requiring GNU hashing.
	attemptDigest := sha256.Sum256([]byte(syntheticJobId + ":" + strconv.Itoa(fixture.attempt)))
	attemptToken := hex.EncodeToString(attemptDigest[:16])
	project := "urnetwork-eval-" + attemptToken + "-" + fixture.role + "-01"
	parent := "urnetwork-evaluation-" + attemptToken + "-" + fixture.role + "-01.slice"
	inspection := syntheticInspect(healthyServiceState(), healthyServiceState())
	for i, service := range []string{"runner", "postgres", "redis"} {
		container := inspection[i]
		container["Name"] = "/" + project + "-" + service + "-1"
		container["HostConfig"].(map[string]any)["CgroupParent"] = parent
		labels := container["Config"].(map[string]any)["Labels"].(map[string]any)
		labels["com.docker.compose.project"] = project
		labels["com.urnetwork.competition.stage"] = fixture.role
		container["Config"].(map[string]any)["Env"] = []string{"SYNTHETIC_SECRET=must-not-retain"}
	}
	inspection[0]["State"].(map[string]any)["ExitCode"] = fixture.exitCode
	if fixture.mutateInspection != nil {
		fixture.mutateInspection(inspection)
	}
	inspectionBytes, err := json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	inspectionPath := filepath.Join(root, "synthetic-inspection.json")
	if err := os.WriteFile(inspectionPath, inspectionBytes, 0600); err != nil {
		t.Fatal(err)
	}
	readyPath := filepath.Join(root, "sampler-ready")
	if err := syscall.Mkfifo(readyPath, 0600); err != nil {
		t.Fatal(err)
	}
	evaluationId := fixture.role + "-01-" + syntheticJobId[:8]
	runDirectory := filepath.Join(root, ".evidence-runtime", "runs", fixture.role+"-01", "output", evaluationId)
	if err := os.MkdirAll(runDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	for file, contents := range map[string]string{
		"run.json":               "{}",
		"accounting.source.json": fmt.Sprintf(`{"schema":1,"kind":"sim-latency-provider-accounting-source","evaluation_id":%q,"complete":true,"provider_egress_bytes":10,"measure_start_ms":1,"measure_end_ms":2}`, evaluationId),
	} {
		if err := os.WriteFile(filepath.Join(runDirectory, file), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const dependencies = `
set -Eeuo pipefail
umask 077
# Force the missing GNU dependency on every host, including Linux.
sha256sum() { printf 'fixture sha256sum is unavailable\n' >&2; return 127; }
artifact_dir="$FIXTURE_ROOT"
work_dir="$artifact_dir/.evidence-runtime"
job_id=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa
round_id=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb
attempt="$FIXTURE_ATTEMPT"
attempt_token="$FIXTURE_ATTEMPT_TOKEN"
cpuset=20,22
config_local_directory=/synthetic/config/local
vault_local_directory=/synthetic/vault/local
baseline_source_root=/synthetic/source
candidate_source_root=/synthetic/source
container_host_uid="$(id -u)"
container_host_gid="$(id -g)"
RUNNER_MEMORY_BYTES=77309411328
POSTGRES_MEMORY_BYTES=17179869184
REDIS_MEMORY_BYTES=8589934592
RUNNER_PIDS_LIMIT=65536
POSTGRES_PIDS_LIMIT=4096
REDIS_PIDS_LIMIT=4096
migration_hashes=()
redis_generations=()
active_sampler_pid=""
die() { printf 'fixture evaluator: %s\n' "$*" >&2; exit 1; }
new_secret() { printf synthetic-throwaway; }
sleep() { :; }
authenticate_local_mounts() { :; }
write_runner_env() { :; }
write_compose_env() { :; }
sha256_file() { printf '%064d' 0; }
date() {
    [ "$#" -eq 2 ] && [ "$2" = +%s%3N ] || die "unexpected date dependency: $*"
    case "$1" in
        --date=2026-01-01T00:00:00Z) printf '1767225600000\n' ;;
        --date=2026-01-01T00:00:01Z) printf '1767225601000\n' ;;
        *) die "unexpected synthetic timestamp: $1" ;;
    esac
}
sync() {
    if [ "$#" -eq 1 ] && [ "$1" = "$artifact_dir" ]; then
        return 0
    fi
    [ "$#" -gt 1 ] && [ "$1" = -d ] || die "unexpected sync dependency: $*"
    shift
    local path
    for path in "$@"; do
        [ -f "$path" ] || die "sync target missing: $path"
    done
}
compose_with() {
    local last="${!#}"
    case " $* " in
        *' ps '*) printf 'synthetic-%s\n' "$last" ;;
        *' migrate '*) printf '%s\n' '{"schema":1,"database_version":1,"migration_count":1}' ;;
    esac
}
container_cgroup() {
    [ "$FIXTURE_FAST" != true ] || [ "$1" != synthetic-runner ] || return 1
    printf '/synthetic/%s/docker-%s\n' "$cgroup_parent" "$1"
}
read_cgroup_oom_kills() {
    if [ -z "${runner_id:-}" ]; then
        printf '%s\n' "$FIXTURE_PRE_RUN_OOM_KILLS"
        return
    fi
    case "$1" in
        *synthetic-postgres)
            [ "$FIXTURE_MISSING_OOM_COUNTER" != postgres ] || return 1
            printf '%s\n' "$FIXTURE_POSTGRES_OOM_KILLS" ;;
        *synthetic-redis)
            [ "$FIXTURE_MISSING_OOM_COUNTER" != redis ] || return 1
            printf '%s\n' "$FIXTURE_REDIS_OOM_KILLS" ;;
        *)
            [ "$FIXTURE_MISSING_OOM_COUNTER" != parent ] || return 1
            printf '%s\n' "$FIXTURE_OOM_KILLS" ;;
    esac
}
sample_cgroup_counters() {
    printf '10 20 0\n' > "$1"
    printf 'ready\n' > "$FIXTURE_READY"
}
validate_output_tree() { printf 'resource report reached\n'; exit 0; }
sudo() {
    [ "$1" = -n ] || exit 96
    shift
    case "$1" in
        chown) return 0 ;;
        sync) "$@"; return ;;
        chmod|test|jq|install) command "$@"; return ;;
        docker) shift ;;
        *) printf 'unexpected sudo dependency: %s\n' "$*" >&2; exit 97 ;;
    esac
    case "$1" in
        inspect)
            if [ "${2:-}" = --format ]; then
                printf 'exited\n'
            else
                command cat "$FIXTURE_INSPECTION"
            fi ;;
        wait)
            if [ "$FIXTURE_FAST" != true ]; then
                local ready
                read -r ready < "$FIXTURE_READY"
                [ "$ready" = ready ] || exit 98
            fi
            printf '%s\n' "$FIXTURE_WAIT" ;;
        logs) : ;;
        network) printf '%s\n' '[{"Internal":true}]' ;;
        *) printf 'unexpected docker dependency: %s\n' "$*" >&2; exit 99 ;;
    esac
}
`
	script, err := os.ReadFile(filepath.Join("evaluator", "container", "evaluator.sh"))
	if err != nil {
		t.Fatal(err)
	}
	inspectionFunction := ""
	if strings.Contains(string(script), "\nwrite_container_inspection() {\n") {
		inspectionFunction = evaluatorShellFunction(t, "write_container_inspection")
	}
	body := dependencies + inspectionFunction + "\n" +
		evaluatorShellFunction(t, "run_stage") + "\nrun_stage \"$FIXTURE_ROLE\" 1 sha256:synthetic-evaluator-image synthetic-build synthetic-simulator synthetic-patch\n"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/bash", "-c", body)
	waitOutput := fixture.waitOutput
	if waitOutput == "" {
		waitOutput = strconv.Itoa(fixture.exitCode)
	}
	command.Env = []string{
		"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "TZ=UTC",
		"FIXTURE_ROOT=" + root, "FIXTURE_ROLE=" + fixture.role,
		"FIXTURE_INSPECTION=" + inspectionPath, "FIXTURE_READY=" + readyPath,
		"FIXTURE_FAST=" + strconv.FormatBool(fixture.fastExit), "FIXTURE_WAIT=" + waitOutput,
		"FIXTURE_OOM_KILLS=" + strconv.Itoa(fixture.oomKills),
		"FIXTURE_PRE_RUN_OOM_KILLS=" + strconv.Itoa(fixture.preRunOomKills),
		"FIXTURE_POSTGRES_OOM_KILLS=" + strconv.Itoa(fixture.postgresOomKills),
		"FIXTURE_REDIS_OOM_KILLS=" + strconv.Itoa(fixture.redisOomKills),
		"FIXTURE_MISSING_OOM_COUNTER=" + fixture.missingOomCounter,
		"FIXTURE_ATTEMPT=" + strconv.Itoa(fixture.attempt),
		"FIXTURE_ATTEMPT_TOKEN=" + attemptToken,
	}
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("fixture timed out: %s", output)
	}
	return root, output, err
}

// A candidate's own nonzero exit is terminal after trusted containment checks,
// including when the process vanished before the first live cgroup sample.
func TestEvaluatorCandidateRunFailureIsTerminalIncludingFastExit(t *testing.T) {
	for _, fastExit := range []bool{false, true} {
		root, output, err := runEvaluatorStageFixture(t, evaluatorStageFixture{exitCode: 7, fastExit: fastExit})
		if err == nil {
			t.Fatalf("fast=%t: failed runner unexpectedly succeeded: %s", fastExit, output)
		}
		data, err := os.ReadFile(filepath.Join(root, "evaluator-failure.json"))
		if err != nil {
			t.Fatalf("fast=%t: terminal envelope missing: %v: %s", fastExit, err, output)
		}
		var failure struct {
			Schema   int    `json:"schema"`
			Kind     string `json:"kind"`
			JobId    string `json:"job_id"`
			RoundId  string `json:"round_id"`
			Attempt  int    `json:"attempt"`
			Role     string `json:"role"`
			Stage    string `json:"stage"`
			ExitCode int    `json:"exit_code"`
			Error    struct {
				Kind      string `json:"kind"`
				Code      string `json:"code"`
				Message   string `json:"message"`
				Retriable bool   `json:"retriable"`
			} `json:"error"`
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&failure); err != nil {
			t.Fatal(err)
		}
		if failure.Schema != 1 || failure.Kind != "sim-latency-stage-failure" ||
			failure.JobId != syntheticJobId || failure.RoundId != syntheticRoundId || failure.Attempt != 1 ||
			failure.Role != "candidate" || failure.Stage != "run" || failure.ExitCode != 7 ||
			failure.Error.Kind != "submission" || failure.Error.Code != "run_process_failed" ||
			failure.Error.Message != "candidate runner exited unsuccessfully" || failure.Error.Retriable {
			t.Errorf("invalid terminal failure: %s", data)
		}
		info, err := os.Stat(filepath.Join(root, "evaluator-failure.json"))
		if err != nil || info.Mode().Perm() != 0400 {
			t.Fatalf("sidecar is not immutable: %v, %v", info, err)
		}
	}
}

// A nonzero candidate limit exit is attributable even after its cgroup has
// disappeared: the previously clean parent and surviving backing counters
// establish that every new OOM belonged to the candidate runner.
func TestEvaluatorCandidateOomFailureIsTerminal(t *testing.T) {
	for _, fastExit := range []bool{false, true} {
		root, output, err := runEvaluatorStageFixture(t, evaluatorStageFixture{
			exitCode: 137, fastExit: fastExit, oomKills: 1,
			mutateInspection: func(containers []map[string]any) { containers[0]["State"].(map[string]any)["OOMKilled"] = true },
		})
		if err == nil {
			t.Fatalf("candidate OOM unexpectedly succeeded: %s", output)
		}
		data, err := os.ReadFile(filepath.Join(root, "evaluator-failure.json"))
		if err != nil {
			t.Fatalf("fast=%t candidate OOM was not terminal: %v: %s", fastExit, err, output)
		}
		var failure struct {
			ExitCode int `json:"exit_code"`
		}
		if err := json.Unmarshal(data, &failure); err != nil || failure.ExitCode != 137 {
			t.Fatalf("candidate OOM failure: %s: %v", data, err)
		}
	}
}

// Retained systemd counters from one attempt must never poison a retry's
// preflight, and the full stage suffix must survive the Docker name limit.
func TestEvaluatorStageIdentitiesSeparateAttempts(t *testing.T) {
	parents := map[string]bool{}
	projects := map[string]bool{}
	for _, attempt := range []int{1, 2, 123456789} {
		root, output, _ := runEvaluatorStageFixture(t, evaluatorStageFixture{exitCode: 7, fastExit: true, attempt: attempt})
		if _, err := os.Stat(filepath.Join(root, "evaluator-failure.json")); err != nil {
			t.Fatalf("attempt %d failed to reach classified run: %v: %s", attempt, err, output)
		}
		data, err := os.ReadFile(filepath.Join(root, ".evidence-runtime", "runs", "candidate-01", "containers.json"))
		if err != nil {
			t.Fatal(err)
		}
		var containers []struct {
			Config struct {
				Labels map[string]string `json:"labels"`
			} `json:"config"`
			HostConfig struct {
				CgroupParent string `json:"cgroup_parent"`
			} `json:"host_config"`
		}
		if err := json.Unmarshal(data, &containers); err != nil {
			t.Fatal(err)
		}
		project := containers[0].Config.Labels["com.docker.compose.project"]
		parent := containers[0].HostConfig.CgroupParent
		if len(project) > 63 || !strings.HasSuffix(project, "-candidate-01") || parents[parent] || projects[project] {
			t.Errorf("attempt %d reused or truncated stage identity: project=%s parent=%s", attempt, project, parent)
		}
		parents[parent], projects[project] = true, true
	}
}

// Backing-service faults, invalid exit status and baseline failures remain
// retryable infrastructure paths and retain only sanitized inspection data.
func TestEvaluatorRunFailureClassificationIsFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		fixture evaluatorStageFixture
	}{
		{name: "baseline", fixture: evaluatorStageFixture{role: "baseline", exitCode: 7}},
		{name: "invalid exit", fixture: evaluatorStageFixture{exitCode: 7, waitOutput: "257"}},
		{name: "mismatched exit", fixture: evaluatorStageFixture{exitCode: 7, waitOutput: "6"}},
		{name: "service oom", fixture: evaluatorStageFixture{exitCode: 7, mutateInspection: func(containers []map[string]any) { containers[1]["State"] = oomKilledServiceState() }}},
		{name: "service restart", fixture: evaluatorStageFixture{exitCode: 7, mutateInspection: func(containers []map[string]any) { containers[2]["RestartCount"] = 1 }}},
		{name: "postgres backend-only oom", fixture: evaluatorStageFixture{exitCode: 7, oomKills: 1, postgresOomKills: 1}},
		{name: "redis backend-only oom", fixture: evaluatorStageFixture{exitCode: 7, oomKills: 1, redisOomKills: 1}},
		{name: "pre-run oom", fixture: evaluatorStageFixture{exitCode: 7, preRunOomKills: 1}},
		{name: "missing parent oom", fixture: evaluatorStageFixture{exitCode: 137, fastExit: true, missingOomCounter: "parent"}},
		{name: "missing postgres oom", fixture: evaluatorStageFixture{exitCode: 137, fastExit: true, missingOomCounter: "postgres"}},
		{name: "inconsistent counters", fixture: evaluatorStageFixture{exitCode: 137, oomKills: 1, postgresOomKills: 2}},
		{name: "unattributed docker oom", fixture: evaluatorStageFixture{exitCode: 137, mutateInspection: func(containers []map[string]any) { containers[0]["State"].(map[string]any)["OOMKilled"] = true }}},
		{name: "container replacement", fixture: evaluatorStageFixture{exitCode: 7, mutateInspection: func(containers []map[string]any) { containers[0]["Id"] = "synthetic-other-runner" }}},
		{name: "missing runner mounts", fixture: evaluatorStageFixture{exitCode: 7, mutateInspection: func(containers []map[string]any) { delete(containers[0], "Mounts") }}},
		{name: "null runner mounts", fixture: evaluatorStageFixture{exitCode: 7, mutateInspection: func(containers []map[string]any) { containers[0]["Mounts"] = nil }}},
		{name: "empty runner mounts", fixture: evaluatorStageFixture{exitCode: 7, mutateInspection: func(containers []map[string]any) { containers[0]["Mounts"] = []map[string]any{} }}},
	}
	for _, c := range cases {
		root, output, err := runEvaluatorStageFixture(t, c.fixture)
		if err == nil {
			t.Fatalf("%s unexpectedly succeeded: %s", c.name, output)
		}
		if _, err := os.Lstat(filepath.Join(root, "evaluator-failure.json")); !os.IsNotExist(err) {
			t.Errorf("%s published terminal submission failure: %v", c.name, err)
		}
		role := c.fixture.role
		if role == "" {
			role = "candidate"
		}
		inspection, err := os.ReadFile(filepath.Join(root, ".evidence-runtime", "runs", role+"-01", "containers.json"))
		if err != nil {
			t.Fatalf("%s lost failure inspection: %v: %s", c.name, err, output)
		}
		for _, excluded := range []string{"SYNTHETIC_SECRET", "must-not-retain", syntheticConfigLocal, syntheticVaultLocal, syntheticSource, `"Config"`} {
			if strings.Contains(string(inspection), excluded) {
				t.Errorf("%s retained private inspection field %q", c.name, excluded)
			}
		}
	}
}

// Successful reports use the actual aggregated Docker/cgroup observations;
// even a backend-only OOM prevents an otherwise healthy zero-exit run.
func TestEvaluatorResourceReportUsesObservedState(t *testing.T) {
	root, output, err := runEvaluatorStageFixture(t, evaluatorStageFixture{})
	if err != nil || !bytes.Contains(output, []byte("resource report reached")) {
		t.Fatalf("healthy run did not reach resource report: %v: %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(root, ".evidence-runtime", "runs", "candidate-01", "resources.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report ResourceReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.ExitCode != 0 || report.OomKilled || report.HardKilled || report.LimitEscape || report.MeasurementMissing {
		t.Fatalf("healthy resource report flags: %s", data)
	}
	measurementStart := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	if report.MeasurementStartMs != measurementStart.UnixMilli() || report.MeasurementEndMs != measurementStart.Add(time.Second).UnixMilli() {
		t.Fatalf("resource report lost the synthetic runner timestamps: %s", data)
	}
	root, output, err = runEvaluatorStageFixture(t, evaluatorStageFixture{oomKills: 1})
	if err == nil {
		t.Fatalf("backend OOM produced successful report: %s", output)
	}
	data, err = os.ReadFile(filepath.Join(root, ".evidence-runtime", "runs", "candidate-01", "container-resource-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		OomKilled bool `json:"oom_killed"`
	}
	if err := json.Unmarshal(data, &observed); err != nil || !observed.OomKilled {
		t.Fatalf("backend OOM was lost from observation: %s: %v", data, err)
	}
}

// The real counter sampler sums all three service cgroups, including OOM kills
// of children whose container init never exited. One explicit iteration is
// sufficient; the sleep dependency ends the fixture after publishing its file.
func TestEvaluatorResourceSamplerAggregatesAllServiceOomEvents(t *testing.T) {
	root := t.TempDir()
	samplePath := filepath.Join(root, "sample.txt")
	body := `set -Eeuo pipefail
read_cgroup_usage_usec() { printf '%s\n' "$1"; }
read_cgroup_peak() { printf '%s\n' "$((10 * $1))"; }
read_cgroup_oom_kills() { printf '%s\n' "$1"; }
sleep() { exit 0; }
` + evaluatorShellFunction(t, "sample_cgroup_counters") + "\nsample_cgroup_counters \"$1\" 1 2 3\n"
	command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body, "synthetic-sampler", samplePath)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("counter sampler: %v: %s", err, output)
	}
	sample, err := os.ReadFile(samplePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(sample) != "6 60 6\n" {
		t.Fatalf("aggregated service counters = %q, want CPU 6 / peak 60 / OOM kills 6", sample)
	}
}

// A terminal sidecar is invalidated if trusted cleanup cannot prove zero
// labeled containers and networks; sanitized infrastructure evidence may still
// be retained before bounded unmount, but the controller must not treat the
// candidate as completed.
func TestEvaluatorTerminalFailureRequiresVerifiedCleanup(t *testing.T) {
	cases := []struct {
		name           string
		containers     string
		networks       string
		unavailable    bool
		terminal       bool
		mount          bool
		unmountFailure bool
		removeFailure  bool
	}{
		{name: "clean", terminal: true},
		{name: "container remains", containers: "synthetic-survivor"},
		{name: "network remains", networks: "synthetic-network"},
		{name: "daemon unavailable", unavailable: true},
		{name: "mount cleaned", mount: true, terminal: true},
		{name: "mount remains", mount: true, unmountFailure: true},
		{name: "mount directory remains", mount: true, removeFailure: true},
	}
	for _, c := range cases {
		root := t.TempDir()
		eventsPath := filepath.Join(root, "cleanup-events")
		retainerPath := filepath.Join(root, "retain-failure-evidence.sh")
		retainer := `#!/bin/bash
set -eu
[ "$1" = "$FIXTURE_MOUNT_PATH" ]
[ "$2" = "$FIXTURE_ROOT/failed-evidence" ]
[ "$FAILURE_EXIT_CODE" = 1 ]
[ "$FAILURE_EVALUATOR_LINE" = 42 ]
printf 'retain\n' >> "$FIXTURE_EVENTS"
`
		if err := os.WriteFile(retainerPath, []byte(retainer), 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "evaluator-failure.json")
		if err := os.WriteFile(path, []byte("synthetic terminal marker"), 0400); err != nil {
			t.Fatal(err)
		}
		mountPath := ""
		if c.mount {
			mountPath = filepath.Join(root, ".evidence-runtime")
			if err := os.Mkdir(mountPath, 0700); err != nil {
				t.Fatal(err)
			}
		}
		body := `set -u
artifact_dir="$FIXTURE_ROOT"
active_work_mount="$FIXTURE_MOUNT_PATH"
fixture_mounted="$FIXTURE_MOUNT"
RETAIN_FAILURE_EVIDENCE="$FIXTURE_RETAINER"
failure_line=42
job_id=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa
worker_uid=0
worker_gid=0
log() { printf '%s\n' "$*" >&2; }
mountpoint() { [ "$fixture_mounted" = true ]; }
rmdir() {
    [ "$fixture_mounted" != true ] && [ "$FIXTURE_REMOVE_FAILURE" != true ] || return 1
    command rmdir "$@"
}

sudo() {
    [ "$1" = -n ] || exit 96
    shift
    local bounded=false
    if [ "$1" = timeout ]; then
        [ "$2" = --signal=TERM ] && [ "$3" = --kill-after=1s ] && [ "$4" = 3s ] || exit 97
        shift 4
        bounded=true
    fi
    if [ "$1" = umount ]; then
        [ "$bounded" = true ] || exit 97
        printf 'umount\n' >> "$FIXTURE_EVENTS"
        [ "$FIXTURE_UNMOUNT_FAILURE" != true ] || return 1
        fixture_mounted=false
        return 0
    fi
    [ "$1" != chown ] || return 0
    [ "$1" = docker ] || exit 98
    shift
    [ "$FIXTURE_UNAVAILABLE" != true ] || return 42
    case "$1 ${2:-}" in
        'ps -aq') printf '%s' "$FIXTURE_CONTAINERS" ;;
        'network ls') printf '%s' "$FIXTURE_NETWORKS" ;;
        'network rm'|'rm -f') : ;;
        *) exit 99 ;;
    esac
}
` + evaluatorShellFunction(t, "cleanup") + "\nfalse\ncleanup\n"
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body)
		command.Env = []string{
			"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "FIXTURE_ROOT=" + root,
			"FIXTURE_CONTAINERS=" + c.containers, "FIXTURE_NETWORKS=" + c.networks,
			"FIXTURE_UNAVAILABLE=" + strconv.FormatBool(c.unavailable),
			"FIXTURE_MOUNT_PATH=" + mountPath, "FIXTURE_MOUNT=" + strconv.FormatBool(c.mount),
			"FIXTURE_UNMOUNT_FAILURE=" + strconv.FormatBool(c.unmountFailure),
			"FIXTURE_REMOVE_FAILURE=" + strconv.FormatBool(c.removeFailure),
			"FIXTURE_RETAINER=" + retainerPath, "FIXTURE_EVENTS=" + eventsPath,
		}
		output, err := command.CombinedOutput()
		if exitError, ok := err.(*exec.ExitError); !ok || exitError.ExitCode() != 1 {
			t.Fatalf("%s cleanup exit = %v: %s", c.name, err, output)
		}
		_, statErr := os.Lstat(path)
		if c.terminal && statErr != nil || !c.terminal && !os.IsNotExist(statErr) {
			t.Errorf("%s sidecar presence = %v, terminal=%t: %s", c.name, statErr, c.terminal, output)
		}
		events, eventsErr := os.ReadFile(eventsPath)
		if c.mount {
			if eventsErr != nil || string(events) != "retain\numount\n" {
				t.Errorf("%s retention/unmount order = %q, error=%v: %s", c.name, events, eventsErr, output)
			}
		} else if !os.IsNotExist(eventsErr) {
			t.Errorf("%s cleanup touched absent evidence mount: %q, error=%v", c.name, events, eventsErr)
		}
	}
}
