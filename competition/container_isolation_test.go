package competition

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

type dockerInstruction struct {
	stage string
	user  string
	text  string
}

// Candidate package initialization runs when the compile-only test binary is
// started. It must be unprivileged and its entire filesystem must be discarded
// before the final submission image is assembled.
func TestSubmissionDockerfileIsolatesCandidateExecution(t *testing.T) {
	instructions := readDockerInstructions(t, "container/Dockerfile.submission")
	stages := map[string]string{}
	var finalCopies []string
	checkDependency := false
	for _, instruction := range instructions {
		fields := strings.Fields(instruction.text)
		if len(fields) >= 4 && fields[0] == "FROM" && strings.EqualFold(fields[len(fields)-2], "AS") {
			stages[fields[len(fields)-1]] = fields[1]
		}
		if strings.HasPrefix(instruction.text, "RUN ") &&
			(strings.Contains(instruction.text, "go vet ") ||
				strings.Contains(instruction.text, "go test ") ||
				strings.Contains(instruction.text, "go build ")) && instruction.user != "65532:65532" {
			t.Fatalf("candidate command runs as %q in stage %q: %s", instruction.user, instruction.stage, instruction.text)
		}
		if instruction.stage == "final" && strings.HasPrefix(instruction.text, "COPY ") {
			finalCopies = append(finalCopies, instruction.text)
		}
		if instruction.stage == "candidate-binary" &&
			strings.HasPrefix(instruction.text, "COPY ") &&
			strings.Contains(instruction.text, "--from=candidate-check") &&
			strings.Contains(instruction.text, "/opt/urnetwork/candidate-check/passed") {
			checkDependency = true
		}
		if instruction.stage == "final" && strings.Contains(instruction.text, "--from=candidate-check") {
			t.Fatal("final image imports the candidate-check filesystem")
		}
	}

	if stages["candidate-check"] != "source-prep" ||
		stages["candidate-binary"] != "source-prep" ||
		stages["final"] != "source-prep" {
		t.Fatalf("untrusted stages do not branch independently from source-prep: %#v", stages)
	}
	if !checkDependency {
		t.Fatal("candidate binary stage does not require the discarded check stage to pass")
	}
	if len(finalCopies) != 1 ||
		!strings.Contains(finalCopies[0], "--from=candidate-binary") ||
		!strings.Contains(finalCopies[0], "/opt/urnetwork/candidate-build/output/sim-latency") ||
		!strings.HasSuffix(finalCopies[0], "/opt/urnetwork/bin/sim-latency") {
		t.Fatalf("final image imports more than the isolated candidate binary: %#v", finalCopies)
	}
}

// The development smoke validates containment and scorer plumbing, not the
// production load frontier. Keep its small fleet away from artificial churn,
// scheduler-scale timeouts, and client/lane reuse that manufactures contract
// contention under the unchanged 97% scorer floor.
func TestContainerSmokeUsesStablePlumbingProfile(t *testing.T) {
	scriptBytes, err := os.ReadFile("container/smoke-test.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)

	requiredCounts := map[string]int{
		"--count=32":                     1,
		"--clients=8":                    1,
		"--rate=16":                      1,
		"--quality-window=8":             1,
		"'APEX_TEST_TIMEOUT=3s'":         2,
		"'APEX_ANNOUNCE_TIMEOUT=2s'":     2,
		"'APEX_PIPELINE_INTERVAL=100ms'": 2,
	}
	for value, want := range requiredCounts {
		if got := strings.Count(script, value); got != want {
			t.Errorf("smoke profile %q count = %d, want %d", value, got, want)
		}
	}
	for _, forbidden := range []string{
		"--count=8",
		"--clients=2",
		"--quality-window=2",
		"--count=128",
		"--clients=16",
		"--rate=30",
		"--rate=120",
		"APEX_TEST_TIMEOUT=10ms",
		"APEX_ANNOUNCE_TIMEOUT=10ms",
		"s/^      uptime_s:",
		"s/^      downtime_s:",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("smoke profile reintroduced unstable setting %q", forbidden)
		}
	}
}

// Every trusted build input participates in both the build record check and
// the runtime identity check before an untrusted candidate can execute.
func TestEvaluatorAuthenticatesEveryCandidateBuildInput(t *testing.T) {
	scriptBytes, err := os.ReadFile("container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)

	requiredChecks := []string{
		`[ "$(jq -er '.base_image_id' "$candidate_build_json")" = "$base_image_id" ]`,
		`[ "$(jq -er '.base_sha' "$candidate_build_json")" = "$base_sha" ]`,
		`[ "$(jq -er '.patch_sha256' "$candidate_build_json")" = "$patch_sha256" ]`,
		`[ "$(jq -er '.policy_sha256' "$candidate_build_json")" = "$policy_sha256" ]`,
		`[ "$(jq -er '.builder_sha256' "$candidate_build_json")" = "$builder_sha256" ]`,
		`[ "$(jq -er '.image_key' "$candidate_build_json")" = "$image_key" ]`,
		`.policy_sha256 == $policy_sha`,
		`.builder_sha256 == $builder_sha`,
		`.image_key == $image_key`,
	}
	for _, requiredCheck := range requiredChecks {
		if !strings.Contains(script, requiredCheck) {
			t.Errorf("evaluator is missing candidate identity check %q", requiredCheck)
		}
	}
}

// Only the two frozen host local directories cross the container boundary.
// Mounting either parent would expose main/all material even though the leaf
// bind mounts are read-only.
func TestEvaluatorMountsOnlyLocalConfigAndVault(t *testing.T) {
	composeBytes, err := os.ReadFile("container/compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	compose := string(composeBytes)
	for value, want := range map[string]int{
		"target: /runtime/config/local": 2,
		"target: /runtime/vault/local":  2,
	} {
		if got := strings.Count(compose, value); got != want {
			t.Errorf("Compose %q count = %d, want %d", value, got, want)
		}
	}
	for _, forbidden := range []string{
		"EVALUATION_RUNTIME_DIR",
		"target: /runtime\n",
		"target: /runtime/config\n",
		"target: /runtime/vault\n",
		"target: /runtime/config/all",
		"target: /runtime/config/main",
		"target: /runtime/vault/all",
		"target: /runtime/vault/main",
	} {
		if strings.Contains(compose, forbidden) {
			t.Errorf("Compose exposes forbidden runtime mount %q", forbidden)
		}
	}

	evaluatorBytes, err := os.ReadFile("container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	evaluator := string(evaluatorBytes)
	for _, required := range []string{
		`config_local_directory="$(jq -er '.config_local_directory' "$request_path")"`,
		`vault_local_directory="$(jq -er '.vault_local_directory' "$request_path")"`,
		`EVALUATION_CONFIG_LOCAL_DIR=$config_local_directory`,
		`EVALUATION_VAULT_LOCAL_DIR=$vault_local_directory`,
		`authenticate_local_mounts`,
		`kind:"sim-latency-local-mounts",direct_bind:true`,
		`.Destination == "/runtime/config/local" and .RW == false`,
		`.Destination == "/runtime/vault/local" and .RW == false`,
		`[.Mounts[] | select(.Destination | startswith("/runtime"))] | length == 2`,
		`mounts:[.Mounts[] | {type:.Type,destination:.Destination,rw:.RW}]`,
	} {
		if !strings.Contains(evaluator, required) {
			t.Errorf("evaluator local-only mount attestation is missing %q", required)
		}
	}
	if strings.Contains(evaluator, "EVALUATION_RUNTIME_DIR=") {
		t.Fatal("evaluator still emits the parent runtime mount")
	}
	if strings.Contains(evaluator, "PREPARE_RUNTIME") || strings.Contains(evaluator, `$runtime/config/local`) {
		t.Fatal("production evaluator still creates or mounts a copied local tree")
	}

	// The preparer remains only as a throwaway smoke fixture. The production
	// evaluator above must not invoke it.
	prepareRuntimeBytes, err := os.ReadFile("container/prepare-runtime.sh")
	if err != nil {
		t.Fatal(err)
	}
	prepareRuntime := string(prepareRuntimeBytes)
	for _, required := range []string{
		`"$runtime_root/vault/local"`,
		`"$runtime_root/config/local"`,
		"runtime tree does not match the local-only allowlist",
	} {
		if !strings.Contains(prepareRuntime, required) {
			t.Errorf("runtime local-only allowlist is missing %q", required)
		}
	}
	if strings.Contains(prepareRuntime, `"$runtime_root/site/local"`) {
		t.Fatal("runtime preparer still creates a site overlay")
	}

	hashBytes, err := os.ReadFile("container/hash-local-mount.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`! -type d ! -type f`, `sort -z`, `sha256sum "$root/$relative"`} {
		if !strings.Contains(string(hashBytes), required) {
			t.Errorf("direct local digest helper is missing %q", required)
		}
	}

	buildBaseBytes, err := os.ReadFile("container/build-base.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		string(buildBaseBytes),
		"readonly REPOSITORIES=(server connect proxy sdk glog goidenticons userwireguard sn)",
	) {
		t.Fatal("evaluator base repository allowlist changed; re-audit config/vault exclusion")
	}
	dockerfileBytes, err := os.ReadFile("container/Dockerfile.base")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"COPY source/config", "COPY source/vault"} {
		if strings.Contains(string(dockerfileBytes), forbidden) {
			t.Errorf("evaluator base image includes forbidden repository content %q", forbidden)
		}
	}

	smokeBytes, err := os.ReadFile("container/smoke-test.sh")
	if err != nil {
		t.Fatal(err)
	}
	smoke := string(smokeBytes)
	for _, required := range []string{
		"local-source/config/local",
		"local-source/vault/local",
		"config/all/forbidden",
		"config/main/forbidden",
		"vault/all/forbidden",
		"vault/main/forbidden",
		"test ! -e /runtime/config/all",
		"test ! -e /runtime/config/main",
		"test ! -e /runtime/vault/all",
		"test ! -e /runtime/vault/main",
		"! touch /runtime/config/local/write-test",
		"! touch /runtime/vault/local/write-test",
		`"exact read-only config/local and vault/local mounts"`,
	} {
		if !strings.Contains(smoke, required) {
			t.Errorf("live local-only mount gate is missing %q", required)
		}
	}
	if !strings.Contains(compose, `APEX_CONTAINER_EVALUATION: "true"`) {
		t.Fatal("direct local mounts do not enable per-stage throwaway credential overrides")
	}

	hostCheckBytes, err := os.ReadFile("host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	hostCheck := string(hostCheckBytes)
	for _, required := range []string{
		`.local_only_read_only_mounts_verified == true`,
		`.no_production_secrets_verified == true`,
		`containment_no_production_secrets=true`,
	} {
		if !strings.Contains(hostCheck, required) {
			t.Errorf("host qualification does not bind local-only evidence %q", required)
		}
	}
}

func TestDockerIDMapTranslation(t *testing.T) {
	script := "container/docker-id-map.sh"
	tests := []struct {
		name    string
		mapping string
		id      string
		want    string
		ok      bool
	}{
		{name: "identity", mapping: "0 0 4294967295\n", id: "65532", want: "65532", ok: true},
		{name: "daemon remap", mapping: "0 100000 65536\n", id: "65532", want: "165532", ok: true},
		{name: "rootless split root", mapping: "0 1000 1\n1 100000 65536\n", id: "65532", want: "165531", ok: true},
		{name: "unmapped gap", mapping: "0 100000 100\n200 200000 100\n", id: "150", ok: false},
		{name: "overlap", mapping: "0 100000 1000\n500 200000 1000\n", id: "750", ok: false},
		{name: "malformed", mapping: "zero 100000 65536\n", id: "1", ok: false},
	}
	for _, test := range tests {
		mappingPath := t.TempDir() + "/id_map"
		if err := os.WriteFile(mappingPath, []byte(test.mapping), 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command(script, "--translate", mappingPath, test.id).CombinedOutput()
		if test.ok && err != nil {
			t.Fatalf("%s translation failed: %v: %s", test.name, err, output)
		}
		if !test.ok && err == nil {
			t.Fatalf("%s invalid mapping translated to %q", test.name, output)
		}
		if test.ok && strings.TrimSpace(string(output)) != test.want {
			t.Fatalf("%s translation = %q, want %q", test.name, strings.TrimSpace(string(output)), test.want)
		}
	}

	evaluatorBytes, err := os.ReadFile("container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	evaluator := string(evaluatorBytes)
	for _, required := range []string{
		`$DOCKER_ID_MAP --image "$base_image_id" --uid 65532 --gid 65532`,
		`container_host_uid="$(jq -er '.host_uid'`,
		`container_host_gid="$(jq -er '.host_gid'`,
		`"$work_dir/docker-id-map.json"`,
		`chown -R "$container_host_uid:$container_host_gid"`,
		`install -o "$container_host_uid" -g "$container_host_gid"`,
	} {
		if !strings.Contains(evaluator, required) {
			t.Errorf("evaluator is not user-namespace ownership aware: missing %q", required)
		}
	}
	for _, forbidden := range []string{`chown -R 65532:65532`, `install -o 65532 -g 65532`} {
		if strings.Contains(evaluator, forbidden) {
			t.Errorf("evaluator assumes identity user mapping: %q", forbidden)
		}
	}

	hostCheckBytes, err := os.ReadFile("host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	hostCheck := string(hostCheckBytes)
	for _, required := range []string{
		`$DOCKER_ID_MAP --image "$image_digest" --uid 65532 --gid 65532`,
		`[ "$docker_id_map_remapped" = true ]`,
		`.docker_user_namespace_verified == true`,
		`.docker_uid_map_sha256 == $docker_uid_map_sha256`,
		`.docker_gid_map_sha256 == $docker_gid_map_sha256`,
	} {
		if !strings.Contains(hostCheck, required) {
			t.Errorf("host qualification is missing live Docker id-map check %q", required)
		}
	}
}

// The daemon example is part of the frozen host identity, not deployment
// advice. Keep the exact namespace and hardening policy machine-checked, and
// require the live host check to authenticate both its bytes and semantics.
func TestDockerDaemonConfigurationIsFailClosed(t *testing.T) {
	configBytes, err := os.ReadFile("docker-daemon.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(configBytes, &config); err != nil {
		t.Fatalf("decode Docker daemon config: %v", err)
	}
	if len(config) != 7 {
		t.Fatalf("Docker daemon config has %d top-level fields, want 7", len(config))
	}
	if config["userns-remap"] != "default" || config["no-new-privileges"] != true ||
		config["userland-proxy"] != false || config["log-driver"] != "local" ||
		config["shutdown-timeout"] != float64(45) || config["ipv6"] != false {
		t.Fatalf("Docker daemon hardening changed: %#v", config)
	}
	logOptions, ok := config["log-opts"].(map[string]any)
	if !ok || len(logOptions) != 3 || logOptions["max-size"] != "16m" ||
		logOptions["max-file"] != "2" || logOptions["compress"] != "true" {
		t.Fatalf("Docker log bounds changed: %#v", config["log-opts"])
	}

	hostCheckBytes, err := os.ReadFile("host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	hostCheck := string(hostCheckBytes)
	for _, required := range []string{
		`readonly DOCKER_DAEMON_CONFIG=/etc/docker/daemon.json`,
		`[ ! -L "$DOCKER_DAEMON_CONFIG" ]`,
		`[ "$(stat -c %u "$DOCKER_DAEMON_CONFIG"`,
		`& 0022)) -eq 0`,
		`."userns-remap" | type == "string" and length > 0`,
		`."no-new-privileges" == true`,
		`."userland-proxy" == false`,
		`."log-driver" == "local"`,
		`."log-opts"."max-size" == "16m"`,
		`."shutdown-timeout" == 45`,
		`[ "$docker_daemon_config_sha256" = "$expected_docker_daemon_config_sha256" ]`,
	} {
		if !strings.Contains(hostCheck, required) {
			t.Errorf("host qualification is missing Docker daemon check %q", required)
		}
	}

	hostConfigBytes, err := os.ReadFile("host-config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var hostConfig map[string]any
	if err := json.Unmarshal(hostConfigBytes, &hostConfig); err != nil {
		t.Fatalf("decode host config: %v", err)
	}
	if hostConfig["docker_daemon_config_sha256"] != "REPLACE_WITH_64_HEX" {
		t.Fatalf("host config does not freeze Docker daemon bytes: %#v", hostConfig["docker_daemon_config_sha256"])
	}
}

// Daemon-wide user-namespace remapping cannot create BuildKit's host-network
// executor mounts on the authoritative Ubuntu host. The trusted base needs
// outbound package downloads, but it does not need the host namespace; keep it
// on Docker's ordinary bridge while submission builds remain networkless.
func TestEvaluatorBaseBuildAvoidsHostNetwork(t *testing.T) {
	buildBytes, err := os.ReadFile("container/build-base.sh")
	if err != nil {
		t.Fatal(err)
	}
	build := string(buildBytes)
	if !strings.Contains(build, "--network default") {
		t.Fatal("evaluator base build does not select Docker's default bridge")
	}
	if strings.Contains(build, "--network host") {
		t.Fatal("evaluator base build joins the host network namespace")
	}
}

// The season-one editable surface is a literal file list, not a directory
// pattern. Keep this pre-freeze policy narrow while the final source identity
// is pending, and make any later expansion require an explicit test change.
func TestExamplePatchPolicyMatchesReviewedSurface(t *testing.T) {
	policyBytes, err := os.ReadFile("container/policy.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var policy PatchPolicy
	if err := json.Unmarshal(policyBytes, &policy); err != nil {
		t.Fatalf("decode patch policy: %v", err)
	}
	if policy.MaxPatchBytes != 262144 {
		t.Fatalf("max patch bytes = %d, want 262144", policy.MaxPatchBytes)
	}
	if len(policy.AllowedPaths) != 1 || policy.AllowedPaths[0] != "connect/resident_contract_manager.go" {
		t.Fatalf("editable surface is not the reviewed literal file: %#v", policy.AllowedPaths)
	}
	if strings.ContainsAny(policy.AllowedPaths[0], `*?[\\`) {
		t.Fatalf("editable surface contains a glob: %q", policy.AllowedPaths[0])
	}
	if !pathAllowed(policy.AllowedPaths[0], policy) {
		t.Fatal("reviewed editable file is not accepted by the server validator")
	}
	for _, protected := range []string{
		"competition/worker.go",
		"connect/sim-latency/main.go",
		"stats/writer.go",
		"db_migrations.go",
		"config/local/settings.yml",
		"config/all/settings.yml",
		"vault/local/jwt.yml",
		"vault/all/jwt.yml",
		"site/local/settings.yml",
		"go.mod",
	} {
		if pathAllowed(protected, policy) {
			t.Errorf("protected path %q is editable", protected)
		}
	}
}

// Infrastructure failures must leave enough immutable evidence to diagnose a
// rejected replicate without retaining hidden inputs or throwaway credentials.
func TestEvaluatorRetainsOnlySanitizedFailureEvidence(t *testing.T) {
	evaluatorBytes, err := os.ReadFile("container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	evaluator := string(evaluatorBytes)
	retainCall := strings.Index(evaluator, `"$RETAIN_FAILURE_EVIDENCE" \`)
	unmountCall := strings.Index(evaluator, `sudo -n umount "$active_work_mount"`)
	if retainCall < 0 || unmountCall < 0 || unmountCall < retainCall {
		t.Fatal("evaluator does not retain failure evidence before unmounting its tmpfs")
	}
	for _, required := range []string{
		`FAILURE_EVALUATOR_LINE="$failure_line"`,
		`retained sanitized failure evidence`,
		`baseline_scorer_log="$work_dir/baseline-scorer.log"`,
		`candidate_scorer_log="$work_dir/candidate-scorer.log"`,
		`inspect_json="$(sudo -n docker inspect`,
		`<<<"$inspect_json" > "$inspect_path"`,
		`[ "$scorer_exit" -eq 0 ] || die "baseline scorer exited $scorer_exit"`,
	} {
		if !strings.Contains(evaluator, required) {
			t.Errorf("evaluator failure diagnostics are missing %q", required)
		}
	}

	retainerBytes, err := os.ReadFile("container/retain-failure-evidence.sh")
	if err != nil {
		t.Fatal(err)
	}
	retainer := string(retainerBytes)
	for _, required := range []string{
		`"$source_dir/input"`,
		`"$source_dir/scorer-input"`,
		`"$source_dir/score-runtime"`,
		`-type d -name runtime -exec rm -rf`,
		`-name '*.env' -o -name '*.env.new'`,
		`-name containers.json -print0`,
		`(keys | sort) == ["config","host_config","id","image_id","mounts","name","state"]`,
		`! -type f ! -type d -delete`,
		`kind:"sim-latency-evaluator-failure"`,
		`kind:"sim-latency-failed-evidence-manifest"`,
		`find "$destination_dir" -type d -exec chmod 0500`,
		`find "$destination_dir" -type f -exec chmod 0400`,
	} {
		if !strings.Contains(retainer, required) {
			t.Errorf("failure evidence sanitizer is missing %q", required)
		}
	}
	if strings.Contains(evaluator, `docker inspect "$runner_id" "$postgres_id" "$redis_id" > "$inspect_path"`) {
		t.Fatal("evaluator persists raw Docker inspection data before sanitization")
	}
}

// Fresh images receive the same complete authentication as cache hits; a
// successful Docker build alone is not evidence that its labels are trusted.
func TestSubmissionBuilderAuthenticatesEveryCandidateBuildInput(t *testing.T) {
	scriptBytes, err := os.ReadFile("container/build-submission.sh")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(scriptBytes), `actual_labels=`, 2)
	if len(parts) != 2 {
		t.Fatal("submission builder does not independently inspect final image labels")
	}
	verification := parts[1]

	for _, requiredCheck := range []string{
		`."com.urnetwork.competition.base-sha" == $base_sha`,
		`."org.opencontainers.image.revision" == $candidate_sha`,
		`."com.urnetwork.competition.patch-sha256" == $patch_sha256`,
		`."com.urnetwork.competition.policy-sha256" == $policy_sha256`,
		`."com.urnetwork.competition.builder-sha256" == $builder_sha256`,
		`."com.urnetwork.competition.image-key" == $image_key`,
		`.policy_sha256 == $policy_sha256`,
		`.builder_sha256 == $builder_sha256`,
		`.image_key == $image_key`,
	} {
		if !strings.Contains(verification, requiredCheck) {
			t.Errorf("submission builder is missing final identity check %q", requiredCheck)
		}
	}
}

// Host management remains schedulable even when candidate code saturates its
// CPU set or reaches every runtime/build memory ceiling.
func TestEvaluatorReservesManagementResources(t *testing.T) {
	boundaryBytes, err := os.ReadFile("container/resource-boundary.sh")
	if err != nil {
		t.Fatal(err)
	}
	boundary := string(boundaryBytes)
	for _, required := range []string{
		"EVALUATION_PHYSICAL_CORE_COUNT=10",
		"MANAGEMENT_PHYSICAL_CORE_COUNT=2",
		"RUNNER_MEMORY_LIMIT=72g",
		"MINIMUM_MANAGEMENT_MEMORY_RESERVE_BYTES=25769803776",
		"disjoint_cpu_sets:true,memory_capacity_passed:true",
	} {
		if !strings.Contains(boundary, required) {
			t.Errorf("resource boundary is missing %q", required)
		}
	}

	evaluatorBytes, err := os.ReadFile("container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	evaluator := string(evaluatorBytes)
	for _, required := range []string{
		`taskset -c "$management_cpuset"`,
		`EVALUATION_CPUSET=$cpuset`,
		`APEX_CPU_COUNT=$evaluation_cpu_count`,
		`resource-boundary.json`,
		`management_cpu_reserved:true`,
		`management_memory_reserved:true`,
		`offline_build_resource_limits:true`,
	} {
		if !strings.Contains(evaluator, required) {
			t.Errorf("evaluator resource boundary is missing %q", required)
		}
	}

	builderBytes, err := os.ReadFile("container/build-submission.sh")
	if err != nil {
		t.Fatal(err)
	}
	builder := string(builderBytes)
	for _, required := range []string{
		`--cgroup-parent "$build_cgroup_parent"`,
		`--resource "cpuset-cpus=$evaluation_cpuset"`,
		`--resource "memory=$build_memory_limit"`,
		`--resource "memory-swap=$build_memory_limit"`,
		`timeout --signal=TERM --kill-after=30s`,
	} {
		if !strings.Contains(builder, required) {
			t.Errorf("submission build boundary is missing %q", required)
		}
	}

}

// The worker must be restricted to the management set without making the host
// topology appear to contain only two CPUs. Host-online CPUs and inherited
// worker affinity are distinct qualification facts.
func TestHostSelfCheckSeparatesHostTopologyFromWorkerAffinity(t *testing.T) {
	hostCheckBytes, err := os.ReadFile("host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	hostCheck := string(hostCheckBytes)
	for _, required := range []string{
		`host_cpu_list="$(lscpu -p=CPU`,
		`worker_cpu_list="$(awk '/^Cpus_allowed_list:/`,
		`[ "$logical_cpu_count" = 12 ] && [ "$host_cpu_list" = "$expected_cpu_list" ]`,
		`[ "$worker_cpu_list" = "$expected_management_cpu_list" ]`,
		`worker_affinity_pinned:$worker_affinity_pinned`,
	} {
		if !strings.Contains(hostCheck, required) {
			t.Errorf("host/worker CPU qualification is missing %q", required)
		}
	}
	if strings.Contains(hostCheck, `logical_cpu_count="$(nproc`) {
		t.Fatal("host CPU count still inherits the worker's management-only affinity")
	}
}

func TestAuthoritativeHostControlsAreFailClosed(t *testing.T) {
	controlBytes, err := os.ReadFile("authoritative-host-controls.sh")
	if err != nil {
		t.Fatal(err)
	}
	control := string(controlBytes)
	for _, required := range []string{
		`/sys/devices/system/cpu/smt/control off`,
		`write_root_file "$governor_path" performance`,
		`write_root_file /sys/devices/system/cpu/intel_pstate/no_turbo 1`,
		`sysctl -q -w vm.overcommit_memory=1`,
		`[ "$logical_cpu_count" -eq 12 ]`,
		`[ "$threads_per_core" -eq 1 ]`,
		`[ "$governors" = performance ]`,
		`[ "$turbo_state" = disabled ]`,
		`.management_logical_cpu_count == 2`,
		`[ "$passed" = true ]`,
	} {
		if !strings.Contains(control, required) {
			t.Errorf("authoritative host controls are missing %q", required)
		}
	}

	controlUnitBytes, err := os.ReadFile("authoritative-host-controls.service.example")
	if err != nil {
		t.Fatal(err)
	}
	controlUnit := string(controlUnitBytes)
	for _, required := range []string{
		`Before=containerd.service docker.service competitionworker.service`,
		`ConditionFileIsExecutable=/usr/local/libexec/urnetwork/authoritative-host-controls`,
		`ExecStart=/usr/local/libexec/urnetwork/authoritative-host-controls --apply`,
		`RemainAfterExit=yes`,
		`TimeoutStartSec=60`,
	} {
		if !strings.Contains(controlUnit, required) {
			t.Errorf("host-control unit is missing %q", required)
		}
	}
	if strings.Contains(controlUnit, "ConditionPathIsExecutable") {
		t.Fatal("host-control unit uses the unsupported ConditionPathIsExecutable directive")
	}

	irqBytes, err := os.ReadFile("authoritative-host-irqs.sh")
	if err != nil {
		t.Fatal(err)
	}
	irq := string(irqBytes)
	for _, required := range []string{
		`readonly MIN_DEVICE_IRQ=16`,
		`management_cpuset="$(jq -er '.management_cpuset'`,
		`evaluation_cpuset="$(jq -er '.evaluation_cpuset'`,
		`/proc/interrupts | sort -n -u`,
		`printf '%s\n' "$management_cpuset" | sudo -n tee "$affinity_path"`,
		`[ "$configured" != "$management_cpuset" ]`,
		`[ "${#failed_irqs[@]}" -eq 0 ]`,
		`[ "$passed" = true ]`,
	} {
		if !strings.Contains(irq, required) {
			t.Errorf("authoritative IRQ control is missing %q", required)
		}
	}

	irqUnitBytes, err := os.ReadFile("authoritative-host-irqs.service.example")
	if err != nil {
		t.Fatal(err)
	}
	irqUnit := string(irqUnitBytes)
	for _, required := range []string{
		`After=urnetwork-authoritative-host-controls.service local-fs.target`,
		`Before=containerd.service docker.service competitionworker.service`,
		`Requires=urnetwork-authoritative-host-controls.service`,
		`ConditionFileIsExecutable=/usr/local/libexec/urnetwork/authoritative-host-irqs`,
		`ExecStart=/usr/local/libexec/urnetwork/authoritative-host-irqs --apply`,
	} {
		if !strings.Contains(irqUnit, required) {
			t.Errorf("IRQ unit is missing %q", required)
		}
	}

	installerBytes, err := os.ReadFile("install-authoritative-host-controls.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerBytes)
	for _, required := range []string{
		`install -D -o root -g root -m 0555 "$CONTROL_SOURCE" "$CONTROL_TARGET"`,
		`install -D -o root -g root -m 0555 "$BOUNDARY_SOURCE" "$BOUNDARY_TARGET"`,
		`install -D -o root -g root -m 0555 "$IRQ_SOURCE" "$IRQ_TARGET"`,
		`install -D -o root -g root -m 0444 "$UNIT_SOURCE" "$UNIT_TARGET"`,
		`install -D -o root -g root -m 0444 "$IRQ_UNIT_SOURCE" "$IRQ_UNIT_TARGET"`,
		`systemctl enable "$UNIT_NAME" "$IRQ_UNIT_NAME"`,
		`sudo -n "$CONTROL_TARGET" --check`,
		`sudo -n "$IRQ_TARGET" --check`,
	} {
		if !strings.Contains(installer, required) {
			t.Errorf("host-control installer is missing %q", required)
		}
	}
}

// Build output is attacker-controlled and must be drained without allowing a
// noisy package initializer to consume the host-memory reserve.
func TestEvaluatorBoundsCandidateBuildLogWhileDrainingIt(t *testing.T) {
	scriptBytes, err := os.ReadFile("container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, required := range []string{
		`mkfifo -m 0600 "$candidate_build_pipe"`,
		`head -c "$MAX_BUILD_LOG_BYTES"`,
		`cat >/dev/null`,
		`2> "$candidate_build_pipe"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("bounded build log is missing %q", required)
		}
	}
}

// The live gate must prove both kernel OOM containment and cleanup from the
// disjoint management CPU set, then reject any residual labeled object.
func TestResourceBombGateCoversOOMAndCleanup(t *testing.T) {
	scriptBytes, err := os.ReadFile("container/test-resource-bomb-cleanup.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, required := range []string{
		`memory_exit_code" -eq 137`,
		`'{{.State.OOMKilled}}'`,
		`[ "$observed_cpuset" = "$evaluation_cpuset" ]`,
		`"$IMAGE" cpu "$evaluation_cpuset"`,
		`--production-memory-limit`,
		`'.runner_memory_limit_bytes'`,
		`production_memory_limit:$production_memory_limit`,
		`taskset -c "$management_cpuset" sudo -n docker rm -f`,
		`cleanup_elapsed_ms`,
		`residual_containers:0,residual_networks:0`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("resource bomb gate is missing %q", required)
		}
	}
	fixtureBytes, err := os.ReadFile("container/testdata/resource-bomb/main.go")
	if err != nil {
		t.Fatal(err)
	}
	fixture := string(fixtureBytes)
	for _, required := range []string{
		"runtime.LockOSThread()",
		"unix.SchedSetaffinity(0, &affinity)",
		"unix.SYS_GETCPU",
		`fmt.Println("cpu-bomb-ready")`,
	} {
		if !strings.Contains(fixture, required) {
			t.Errorf("resource bomb fixture is missing %q", required)
		}
	}

	hostCheckBytes, err := os.ReadFile("host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	hostCheck := string(hostCheckBytes)
	for _, required := range []string{
		`.production_memory_limit_verified == true`,
		`.memory_bomb_limit_bytes == $runner_memory_limit_bytes`,
		`.memory_bomb_exit_code == 137`,
		`.default_deny_network_verified == true`,
		`.no_published_ports_verified == true`,
		`.scorer_network_none_verified == true`,
		`.evidence_manifest_sha256 | type == "string"`,
		`.evaluation_complete_sha256 | type == "string"`,
		`.cleanup_elapsed_ms <= .cleanup_limit_ms`,
		`.residual_containers == 0 and .residual_networks == 0`,
	} {
		if !strings.Contains(hostCheck, required) {
			t.Errorf("host qualification is missing production bomb check %q", required)
		}
	}
	if strings.Contains(hostCheck, `/proc/net/route`) {
		t.Fatal("host qualification still substitutes the host route table for evaluator network evidence")
	}
}

// Host readiness consumes a short root-owned marker, so marker promotion must
// authenticate the full evaluator chain and reject semantically unsafe mount
// evidence even when all attacker-controlled hashes are internally coherent.
func TestHostContainmentPromotionAuthenticatesEvidence(t *testing.T) {
	promoterBytes, err := os.ReadFile("promote-host-containment.sh")
	if err != nil {
		t.Fatal(err)
	}
	promoter := string(promoterBytes)
	for _, required := range []string{
		`promotion must run as root`,
		`host config parent is not root-owned`,
		`worker result did not pass every score and containment gate`,
		`authenticate_declared_artifact evaluation.complete.json`,
		`authenticate_declared_artifact evidence-manifest.json`,
		`evidence hash mismatch`,
		`evidence/local-mounts.json`,
		`evidence/docker-id-map.json`,
		`direct local mount evidence is invalid`,
		`Docker user-namespace evidence is invalid`,
		`.destination == "/runtime/config/local" and .rw == false`,
		`.destination == "/runtime/vault/local" and .rw == false`,
		`container evidence violates the local-only boundary`,
		`.cleanup_marker_sha256 = $marker_sha`,
	} {
		if !strings.Contains(promoter, required) {
			t.Errorf("host containment promoter is missing %q", required)
		}
	}

	testBytes, err := os.ReadFile("test-promote-host-containment.sh")
	if err != nil {
		t.Fatal(err)
	}
	testScript := string(testBytes)
	for _, required := range []string{
		`host containment promotion test passed`,
		`.mounts[0].destination) = "/runtime/config"`,
		`unsafe parent config mount was promoted`,
		`identity Docker user namespace was promoted`,
		`failed promotion left a marker`,
		`failed user-namespace promotion left a marker`,
	} {
		if !strings.Contains(testScript, required) {
			t.Errorf("host containment promotion regression is missing %q", required)
		}
	}
}

func readDockerInstructions(t *testing.T, path string) []dockerInstruction {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var instructions []dockerInstruction
	var pending string
	stage := ""
	user := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pending += strings.TrimSuffix(line, "\\")
		if strings.HasSuffix(line, "\\") {
			pending += " "
			continue
		}
		instruction := strings.Join(strings.Fields(pending), " ")
		pending = ""
		fields := strings.Fields(instruction)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "FROM":
			stage = ""
			user = ""
			if len(fields) >= 4 && strings.EqualFold(fields[len(fields)-2], "AS") {
				stage = fields[len(fields)-1]
			}
		case "USER":
			if len(fields) != 2 {
				t.Fatalf("malformed USER instruction: %s", instruction)
			}
			user = fields[1]
		}
		instructions = append(instructions, dockerInstruction{stage: stage, user: user, text: instruction})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if pending != "" {
		t.Fatal("Dockerfile ends with an incomplete instruction")
	}
	return instructions
}
