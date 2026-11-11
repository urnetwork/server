//go:build linux

// The prospective builder is exercised with real shell/client lifetimes and
// private files. The fake client never contacts Docker or executes Go/candidate
// code; native compiler/profile/resource qualification remains separate.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Assets come from this exact package-cwd archive, never an installed release.
func trackedBuilderAsset(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(filepath.Dir(evaluatorRoutingHelper(t)), name)
}

// This fixture executes the complete new helper; no extracted stand-in can
// hide another execution stage or different source/cache/output vocabulary.
func runTrackedBuilderProtocol(t *testing.T, fault string) (string, int) {
	t.Helper()
	root := t.TempDir()
	cacheRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheRoot, "seed.mod"), []byte("trusted-cache"), 0600); err != nil {
		t.Fatal(err)
	}
	client := filepath.Join(root, "client")
	program := `#!/bin/bash
set -euo pipefail
printf '%q ' "$@" >> "$SYNTHETIC_BUILD_LOG"
printf '\n' >> "$SYNTHETIC_BUILD_LOG"
role="$1"
shift 3
if [[ "$role" == --host-operation ]]; then
  case "$1" in
    prepare-build-check-cache|prepare-build-compile-cache)
      stage=check
      [[ "$1" != prepare-build-compile-cache ]] || stage=compile
      printf '{"stage":"%s","sha256":"%064d","bytes":13,"files":1}\n' "$stage" 1
      ;;
    prepare-build-output) ;;
    reclaim-build-output)
      printf 'synthetic verified bytes' > "$SYNTHETIC_BUILD_ROOT/assembly/sim-latency"
      hash="$(sha256sum "$SYNTHETIC_BUILD_ROOT/assembly/sim-latency")"; hash="${hash%% *}"
      size="$(stat -c %s "$SYNTHETIC_BUILD_ROOT/assembly/sim-latency")"
      printf '{"sha256":"%s","bytes":%s,"go_version":"go1.27.4","go_package":"github.com/urnetwork/server/connect/sim-latency","vcs_revision":"%s","vcs_modified":"false"}\n' "$hash" "$size" "$SYNTHETIC_REVISION"
      ;;
    *) exit 91 ;;
  esac
  exit 0
fi
[[ "$role" == --docker-client ]] || exit 92
case "$1" in
  create)
    count=0
    [[ ! -e "$SYNTHETIC_BUILD_COUNTER" ]] || read -r count < "$SYNTHETIC_BUILD_COUNTER"
    count=$((count + 1)); printf '%s\n' "$count" > "$SYNTHETIC_BUILD_COUNTER"
    printf '%064d\n' "$count"
    ;;
  cp)
    [[ "${@: -1}" == - ]] || exit 96
    tar -C "$SYNTHETIC_CACHE_ROOT" -cf - seed.mod
    ;;
  container)
    [[ "$2" == start ]] || exit 93
    case "${3: -1}" in
      2) printf 'check-owned cache mutation' > "$SYNTHETIC_BUILD_ROOT/check-cache/seed.mod" ;;
      3) [[ "$(<"$SYNTHETIC_BUILD_ROOT/compile-cache/seed.mod")" == trusted-cache ]] || exit 94 ;;
    esac
    ;;
  wait)
    if [[ "$SYNTHETIC_BUILD_FAULT" == unknown ]]; then exit 124; fi
    if [[ "$SYNTHETIC_BUILD_FAULT" == compile && "${2: -1}" == 3 ]]; then printf '1\n'; else printf '0\n'; fi
    ;;
  logs) printf 'synthetic finite compiler log\n' ;;
  rm) ;;
  build) [[ "$SYNTHETIC_BUILD_FAULT" != assembly ]] || exit 1 ;;
  *) exit 95 ;;
esac
`
	if err := os.WriteFile(client, []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	body := `source "$1"
subevaluation_ownership_init
source "$2"
infrastructure_failure() { printf 'synthetic infrastructure refusal: %s\n' "$*" >&2; exit 2; }
evaluation_git() { if [[ "$*" == *rev-parse* ]]; then printf '%s\n' "$SYNTHETIC_REVISION"; fi; }
build_parent="$SYNTHETIC_ROOT/.evidence-runtime"
source_root="$build_parent/evaluation-sources.AAAAAAAA/candidate"
source_identity="$source_root/.evaluation-source.json"
build_context="$build_parent/build-context"
SCRIPT_DIR="$(dirname "$2")"
install -d -m 0700 "$source_root" "$build_context"
printf '{"synthetic_source":true}\n' > "$source_identity"
printf '{"schema":1,"base_sha":"synthetic","paths":[]}\n' > "$build_context/source-application.json"
candidate_sha="$SYNTHETIC_REVISION"
REPOSITORIES=(server connect sdk proxy glog goidenticons userwireguard sn warp gvisor)
jq -nS --arg revision "$candidate_sha" 'reduce ["server","connect","sdk","proxy","glog","goidenticons","userwireguard","sn","warp","gvisor"][] as $name ({}; .[$name]=$revision)' > "$build_context/candidate-commits.json"
resource_boundary_json='{"build_pids_limit":4096,"build_work_limit":"10g"}'
BUILD_TIMEOUT_SECONDS=600
build_cgroup_parent=urnetwork-submission-build-synthetic.slice
evaluation_cpuset=2,4
build_memory_limit=12g
base_image_id="sha256:$SYNTHETIC_DIGEST"
base_image="urnetwork/sim-latency-evaluator-base-id:$SYNTHETIC_DIGEST"
base_sha="$candidate_sha"
patch_sha256="$SYNTHETIC_DIGEST"; policy_sha256="$SYNTHETIC_DIGEST"; builder_sha256="$SYNTHETIC_DIGEST"
patch_validator_sha256="$SYNTHETIC_DIGEST"; candidate_commits_sha256="$SYNTHETIC_DIGEST"; original_source_identity_sha256="$SYNTHETIC_DIGEST"
image_key="$SYNTHETIC_DIGEST"; image_tag=synthetic-only
trap tracked_submission_cleanup EXIT
tracked_submission_build
`
	command := exec.CommandContext(t.Context(), "bash", "-eu", "-c", body, "tracked-protocol", evaluatorRoutingHelper(t), trackedBuilderAsset(t, "tracked-submission-build.sh"))
	command.Env = append(controlEnvironment(), "SYNTHETIC_ROOT="+root, "SYNTHETIC_BUILD_ROOT="+filepath.Join(root, ".evidence-runtime", "tracked-build"),
		"SYNTHETIC_CACHE_ROOT="+cacheRoot,
		"SYNTHETIC_BUILD_LOG="+filepath.Join(root, "calls"), "SYNTHETIC_BUILD_COUNTER="+filepath.Join(root, "counter"), "SYNTHETIC_BUILD_FAULT="+fault,
		"SYNTHETIC_REVISION="+strings.Repeat("c", 40), "SYNTHETIC_DIGEST="+strings.Repeat("d", 64),
		"URNETWORK_SUBEVALUATION_RUN_ID="+strings.Repeat("a", 64), "URNETWORK_SUBEVALUATION_JOB_ID=00000000-0000-0000-0000-000000000001",
		"URNETWORK_SUBEVALUATION_ROUND_ID=00000000-0000-0000-0000-000000000002", "URNETWORK_SUBEVALUATION_ATTEMPT=1",
		"URNETWORK_SUBEVALUATION_MANAGEMENT_CPUSET=0", "URNETWORK_SUBEVALUATION_CLIENT="+client)
	output, err := command.CombinedOutput()
	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal("private tracked builder fixture could not start", err)
		}
		code = exit.ExitCode()
	}
	log, readErr := os.ReadFile(filepath.Join(root, "calls"))
	if readErr != nil {
		t.Fatal("private tracked protocol emitted no finite client trace", readErr, string(output))
	}
	return string(log) + "\nfixture-output:\n" + string(output), code
}

// The complete orchestration uses fresh ids/caches, the exact guarded CREATE
// class, joined reclaim, and only then metadata-only BuildKit assembly.
func TestTrackedBuilderUsesTwoIndependentGuardedExecutionStages(t *testing.T) {
	log, code := runTrackedBuilderProtocol(t, "")
	if code != 0 {
		t.Fatal("positive tracked protocol failed", code, log)
	}
	for _, token := range []string{"builder-stage=check", "builder-stage=compile", "core=1:1", "seccomp=", "dst=/workspace", "readonly",
		"prepare-build-check-cache", "prepare-build-compile-cache", "reclaim-build-output", "Dockerfile.submission.tracked", "CANDIDATE_SOURCE_IDENTITY_SHA256="} {
		if !strings.Contains(log, token) {
			t.Fatal("complete tracked protocol omitted its fixed authority", token, log)
		}
	}
	if strings.Count(log, "--docker-client "+strings.Repeat("a", 64)+" -- create ") != 3 || strings.Index(log, "reclaim-build-output") > strings.Index(log, "-- build ") {
		t.Fatal("untracked execution or premature final assembly entered the protocol", log)
	}
}

// Ordinary compiler rejection is acknowledged candidate failure, never an
// unknown result or permission to assemble/reclaim another output.
func TestTrackedBuilderCompileRejectionNeverAssemblesAnImage(t *testing.T) {
	log, code := runTrackedBuilderProtocol(t, "compile")
	if code != 1 || strings.Contains(log, "reclaim-build-output") || strings.Contains(log, "-- build ") || !strings.Contains(log, "candidate compile rejected with exit 1") {
		t.Fatal("definite compile rejection lost its distinct terminal classification", code, log)
	}
}

// A missing wait response remains infrastructure custody; its old stage id
// is cleaned by the fixed owner and is never replayed as another compilation.
func TestTrackedBuilderUnknownWaitNeverStartsSuccessorOrAssembly(t *testing.T) {
	log, code := runTrackedBuilderProtocol(t, "unknown")
	if code != 2 || strings.Contains(log, "builder-stage=compile") || strings.Contains(log, "reclaim-build-output") || strings.Contains(log, "-- build ") ||
		!strings.Contains(log, "rm --force") || !strings.Contains(log, "tracked compiler completion remains uncertain") {
		t.Fatal("uncertain check response reached a successor or lost cleanup custody", code, log)
	}
}

// A failure in the fixed no-execution assembly cannot relabel already-passed
// compilation as a candidate rejection, even when Docker returns exit one.
func TestTrackedBuilderTrustedAssemblyFailureRemainsInfrastructure(t *testing.T) {
	log, code := runTrackedBuilderProtocol(t, "assembly")
	if code != 2 || !strings.Contains(log, "reclaim-build-output") || !strings.Contains(log, "-- build ") ||
		!strings.Contains(log, "trusted image assembly failed or its outcome remains uncertain") || strings.Contains(log, "candidate compile rejected") {
		t.Fatal("trusted assembly failure became candidate rejection or lost completed compilation", code, log)
	}
}

// The actual immutable Dockerfile fits the assembly grammar without running a
// Docker frontend or trusting a source-code substring count as native proof.
func TestTrackedBuilderDockerfileHasNoExecutionOrExternalFrontend(t *testing.T) {
	content, err := os.ReadFile(trackedBuilderAsset(t, "Dockerfile.submission.tracked"))
	if err != nil || validateTrackedSubmissionDockerfile(content) != nil {
		t.Fatal("actual tracked assembly left its closed no-execution grammar", err)
	}
}

// Context bytes and a content pin alone cannot authorize an arbitrary RUN,
// trigger, frontend, stage, imported image or alternate copied executable.
func TestTrackedBuilderDockerfileRejectsExecutionAndSourceExpansion(t *testing.T) {
	prefix := "ARG EVALUATOR_BASE_IMAGE\nFROM ${EVALUATOR_BASE_IMAGE}\n"
	for _, suffix := range []string{"RUN true\n", "ONBUILD RUN true\n", "ENTRYPOINT [\"/candidate\"]\n", "SHELL [\"/bin/sh\"]\n",
		"FROM synthetic.example/other\n", "COPY --from=foreign /binary /binary\n", "COPY sim-latency /candidate\n", "ARG UNKNOWN\n", "CMD [\"/candidate\"]\n"} {
		if err := validateTrackedSubmissionDockerfile([]byte(prefix + suffix)); err == nil {
			t.Fatal("executable or expanded assembly instruction was admitted", suffix)
		}
	}
	for _, content := range []string{"# syntax=synthetic.example/runner\n" + prefix, "# escape=`\n" + prefix, prefix + "LABEL x=\\\n", prefix + "LABEL x=\\\n# hidden\nRUN true\n"} {
		if err := validateTrackedSubmissionDockerfile([]byte(content)); err == nil {
			t.Fatal("frontend/parser ambiguity was admitted", content)
		}
	}
}

// Build-stage creation is allowed only before final assembly/retirement has
// taken custody. A completed image must not reopen arbitrary compiler work.
func TestTrackedBuilderAdmissionClosesBeforeFinalAssemblyAndRetirement(t *testing.T) {
	for _, phase := range []string{"open", "build", "retiring", "closed", "unknown"} {
		fixture, _, _ := newBuildRetirementProxyTest(t)
		switch phase {
		case "build":
			fixture.proxy.build = &dockerProxyBuild{}
		case "retiring":
			fixture.proxy.retirement = &dockerBuildRetirement{}
		case "closed":
			fixture.proxy.buildAdmissionsClosed = true
		case "unknown":
			fixture.proxy.unknown = 1
		}
		err := fixture.proxy.requireBuildRetired(t.Context(), fixture.plan.BuildCgroupName)
		if (err == nil) != (phase == "open") {
			t.Fatal("tracked compiler admission ignored its original phase owner", phase, err)
		}
	}
}

// The final source hook alone is insufficient proof: both real stage schemas
// must pass the common profile/resource/bind gate before one daemon CREATE.
func TestTrackedBuilderCompleteCreateGateAcceptsBothIsolatedStages(t *testing.T) {
	for _, stage := range []string{"check", "compile"} {
		fixture := newCandidateSourceTest(t)
		resource := testRunResourceIdentity()
		policy, err := newBuildRetirementPolicy(runSliceResources{ResourcePolicySha256: fixture.fixture.plan.ResourcePolicySha256, Identity: resource,
			BuildTasksMaximum: resource.BuildPidsLimit, CpuPeriodMicros: 100000}, fixture.fixture.plan.BuildCgroupName, int64(os.Getpagesize()))
		if err != nil {
			t.Fatal(err)
		}
		fixture.fixture.proxy.plan.BuildRetirement = policy
		reads, creates := fixture.daemon(t, nil)
		var body []byte
		if err := fixture.trackedBuild(t, stage, func(object map[string]json.RawMessage, host *dockerPolicyHostConfig) {
			host.Memory, host.MemorySwap, host.CpusetCpus = resource.BuildMemoryLimitBytes, resource.BuildMemoryLimitBytes, fixture.fixture.plan.CpuSet
			host.PidsLimit = &resource.BuildPidsLimit
			host.Tmpfs = map[string]string{"/work": "rw,nosuid,nodev,exec,uid=65532,gid=65532,mode=0700,size=" + resource.BuildWorkLimit}
			object["HostConfig"], _ = json.Marshal(host)
			body, _ = json.Marshal(object)
		}); err != nil {
			t.Fatal("isolated stage did not reach composed common admission", stage, err)
		}
		response := httptest.NewRecorder()
		fixture.fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/v1.56/containers/create", bytes.NewReader(body)))
		if response.Code != http.StatusCreated || creates.Load() != 1 || reads.Load() != 0 {
			t.Fatal("complete tracked CREATE gate rejected a frozen isolated stage", stage, response.Code, response.Body.String())
		}
	}
}

// Missing/conflicting execution flags must fail before any daemon mutation.
func TestTrackedBuilderCompleteCreateGateRejectsNonExecutableWork(t *testing.T) {
	for _, execution := range []string{"", ",exec,noexec"} {
		fixture := newCandidateSourceTest(t)
		resource := testRunResourceIdentity()
		policy, err := newBuildRetirementPolicy(runSliceResources{ResourcePolicySha256: fixture.fixture.plan.ResourcePolicySha256, Identity: resource,
			BuildTasksMaximum: resource.BuildPidsLimit, CpuPeriodMicros: 100000}, fixture.fixture.plan.BuildCgroupName, int64(os.Getpagesize()))
		if err != nil {
			t.Fatal(err)
		}
		fixture.fixture.proxy.plan.BuildRetirement = policy
		reads, creates := fixture.daemon(t, nil)
		var body []byte
		if err := fixture.trackedBuild(t, "check", func(object map[string]json.RawMessage, host *dockerPolicyHostConfig) {
			host.Memory, host.MemorySwap, host.CpusetCpus = resource.BuildMemoryLimitBytes, resource.BuildMemoryLimitBytes, fixture.fixture.plan.CpuSet
			host.PidsLimit = &resource.BuildPidsLimit
			host.Tmpfs = map[string]string{"/work": "rw,nosuid,nodev,uid=65532,gid=65532,mode=0700,size=" + resource.BuildWorkLimit + execution}
			object["HostConfig"], _ = json.Marshal(host)
			body, _ = json.Marshal(object)
		}); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		fixture.fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/v1.56/containers/create", bytes.NewReader(body)))
		if response.Code != http.StatusForbidden || creates.Load() != 0 || reads.Load() != 0 {
			t.Fatal("non-executable work reached the upstream daemon", execution, response.Code)
		}
	}
}

// The common finite bind grammar deliberately knows both cache paths, but its
// role-specific composition still refuses cross-stage cache reuse before issue.
func TestTrackedBuilderCompleteCreateGateRejectsCrossStageCache(t *testing.T) {
	fixture := newCandidateSourceTest(t)
	_, creates := fixture.daemon(t, nil)
	var body []byte
	if err := fixture.trackedBuild(t, "compile", func(object map[string]json.RawMessage, host *dockerPolicyHostConfig) {
		object["HostConfig"], _ = json.Marshal(host)
		body, _ = json.Marshal(object)
	}); err != nil {
		t.Fatal("positive compile setup did not bind its complete stage", err)
	}
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatal(err)
	}
	host := object["HostConfig"].(map[string]any)
	binds := host["Binds"].([]any)
	binds[1] = strings.Replace(binds[1].(string), "/compile-cache:", "/check-cache:", 1)
	dockerPolicyTestDirectory(t, filepath.Join(fixture.fixture.plan.ContextRoot, ".evidence-runtime", "tracked-build", "check-cache"))
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	fixture.fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/v1.56/containers/create", bytes.NewReader(body)))
	if response.Code != http.StatusForbidden || creates.Load() != 0 {
		t.Fatal("cross-stage cache reuse reached upstream CREATE", response.Code)
	}
}

// The stage env file must cross the real client's cleared environment. Reading
// inherited shell variables in an argument-only fake client cannot prove this.
func TestTrackedBuilderComposeMetadataSurvivesClearedClientEnvironment(t *testing.T) {
	fixture := newDockerClientTestFixture(t)
	content, err := os.ReadFile(trackedBuilderAsset(t, "evaluator-subevaluation.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	start := strings.Index(text, "write_compose_env() {\n")
	if start < 0 {
		t.Fatal("actual Compose environment writer is absent")
	}
	end := strings.Index(text[start:], "\n}\n")
	if end < 0 {
		t.Fatal("actual Compose environment writer is incomplete")
	}
	writer := text[start : start+end+3]
	path := filepath.Join(fixture.config.WorkDirectory, fixture.runId, "stage.env")
	body := writer + `
die() { exit 96; }
job_id=synthetic-job; round_id=synthetic-round; cpuset=2,4; base_image_id=synthetic-base
config_local_directory=/synthetic/config/local; vault_local_directory=/synthetic/vault/local; POSTGRES_INIT=/synthetic/init
for name in RUNNER_MEMORY_LIMIT RUNNER_PIDS_LIMIT RUNNER_TMP_LIMIT RUNNER_WORK_LIMIT MIGRATOR_MEMORY_LIMIT MIGRATOR_PIDS_LIMIT MIGRATOR_TMP_LIMIT POSTGRES_MEMORY_LIMIT POSTGRES_PIDS_LIMIT POSTGRES_DATA_LIMIT POSTGRES_MAX_CONNECTIONS POSTGRES_SHARED_BUFFERS REDIS_MEMORY_LIMIT REDIS_PIDS_LIMIT REDIS_DATA_LIMIT REDIS_MAX_CLIENTS REDIS_TCP_BACKLOG SCORER_MEMORY_LIMIT SCORER_PIDS_LIMIT SCORER_TMP_LIMIT EVALUATION_NOFILE; do printf -v "$name" '%s' synthetic; done
stage_db_admin_password=synthetic; stage_db_password=synthetic; stage_redis_password=synthetic
subevaluation_scoped=true; subevaluation_run_id=synthetic-run
URNETWORK_SUBEVALUATION_BRIDGE_NAME=unev-synthetic
URNETWORK_SUBEVALUATION_CORE_PROFILE=/synthetic-release/candidate-core.seccomp.json
write_compose_env "$1" synthetic-project candidate synthetic-image /synthetic/runner.env /synthetic/input /synthetic/output synthetic-cgroup /synthetic/source
`
	command := exec.CommandContext(t.Context(), "bash", "-eu", "-c", body, "compose-metadata", path)
	command.Env = controlEnvironment()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal("actual stage environment writer failed", err, string(output))
	}
	program := "#!/bin/sh\nset -eu\n[ -z \"${URNETWORK_SUBEVALUATION_CORE_PROFILE+x}\" ]\n[ -z \"${URNETWORK_SUBEVALUATION_BRIDGE_NAME+x}\" ]\n" +
		"while [ \"$#\" -gt 0 ]; do if [ \"$1\" = --env-file ]; then shift; exec /bin/cat \"$1\"; fi; shift; done\nexit 97\n"
	if err := os.WriteFile(fixture.config.Docker.Path, []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	fixture.config.Docker = testAgentPin(t, fixture.config.Docker.Path)
	t.Setenv("URNETWORK_SUBEVALUATION_CORE_PROFILE", "/synthetic-invalid/inherited-profile")
	t.Setenv("URNETWORK_SUBEVALUATION_BRIDGE_NAME", "inherited-invalid")
	var output bytes.Buffer
	if err := runDockerClient(context.Background(), fixture.config, fixture.runId, []string{"compose", "--env-file", path, "config"}, nil, &output, &output, fixture.settings); err != nil {
		t.Fatal("cleared client could not retain explicit Compose metadata", err, output.String())
	}
	for _, expected := range []string{"URNETWORK_SUBEVALUATION_RUN_ID=synthetic-run\n", "URNETWORK_SUBEVALUATION_BRIDGE_NAME=unev-synthetic\n", "URNETWORK_SUBEVALUATION_CORE_PROFILE=/synthetic-release/candidate-core.seccomp.json\n"} {
		if !strings.Contains(output.String(), expected) || strings.Contains(output.String(), "inherited-invalid") {
			t.Fatal("stage metadata relied on stripped/inherited environment", strconv.Quote(expected), output.String())
		}
	}
}
