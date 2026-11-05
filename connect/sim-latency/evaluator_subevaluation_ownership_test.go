// Prospective helper ownership is exercised without Docker, systemd, mounts or
// root privileges. These source/argument tests do not qualify host containment.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// All scope identifiers are visibly synthetic and independent of live runs.
func subevaluationOwnershipEnvironment() []string {
	return []string{
		"URNETWORK_SUBEVALUATION_RUN_ID=" + strings.Repeat("a", 64),
		"URNETWORK_SUBEVALUATION_JOB_ID=11111111-2222-4333-8444-555555555555",
		"URNETWORK_SUBEVALUATION_ROUND_ID=66666666-7777-4888-8999-000000000000",
		"URNETWORK_SUBEVALUATION_ATTEMPT=2",
	}
}

// Only the checked-in helper and finite test body execute; ambient operator
// configuration and application credentials never enter these child processes.
func subevaluationOwnershipCommand(t *testing.T, body string, environment []string) *exec.Cmd {
	t.Helper()
	helper, err := filepath.Abs(filepath.Join("evaluator", "container", "subevaluation-ownership.sh"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/bash", "-c", "set -Eeuo pipefail\nsource \"$1\"\nsubevaluation_ownership_init\n"+body, "synthetic-subevaluation", helper)
	command.Env = append([]string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "URNETWORK_SUBEVALUATION_MANAGEMENT_CPUSET=20,22"}, environment...)
	return command
}

// A fixed private executable records arguments without invoking a real Docker
// CLI. The production routing and positional public-role grammar remain real.
func subevaluationOwnershipClient(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixed-client")
	if err := os.WriteFile(path, []byte("#!/bin/bash\nset -Eeuo pipefail\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// Absence retains empty arrays, including no new Compose file or cgroup flag.
func TestSubevaluationOwnershipLegacyArgumentsStayEmpty(t *testing.T) {
	body := `printf '%s\000' "$subevaluation_scoped"
for value in "${subevaluation_docker_args[@]}" "${subevaluation_run_label_args[@]}" "${subevaluation_compose_args[@]}" "${subevaluation_cleanup_filter_args[@]}"; do
    printf '%s\000' "$value"
done
subevaluation_require_identity legacy legacy legacy
`
	output, err := subevaluationOwnershipCommand(t, body, nil).CombinedOutput()
	if err != nil || string(output) != "false\x00" {
		t.Fatalf("legacy scope gained an argument or prerequisite: %v: %q", err, output)
	}
}

// Exact pre-creation labels and the independently computed job/attempt slice
// cannot be replaced by Compose interpolation or a candidate-written file.
func TestSubevaluationOwnershipBindsExactPrecreationArguments(t *testing.T) {
	body := `printf '%s\000' "$subevaluation_scoped" "$subevaluation_preflight_cgroup"
for value in "${subevaluation_docker_args[@]}" "${subevaluation_run_label_args[@]}" "${subevaluation_compose_args[@]}" "${subevaluation_cleanup_filter_args[@]}"; do
    printf '%s\000' "$value"
done
subevaluation_require_identity "$URNETWORK_SUBEVALUATION_JOB_ID" "$URNETWORK_SUBEVALUATION_ROUND_ID" "$URNETWORK_SUBEVALUATION_ATTEMPT"
`
	output, err := subevaluationOwnershipCommand(t, body, subevaluationOwnershipEnvironment()).CombinedOutput()
	if err != nil {
		t.Fatalf("valid synthetic scope rejected: %v: %s", err, output)
	}
	token := sha256.Sum256([]byte("11111111-2222-4333-8444-555555555555:2"))
	group := "urnetwork-evaluation-" + hex.EncodeToString(token[:])[:32] + "-preflight.slice"
	overlay, err := filepath.Abs(filepath.Join("evaluator", "container", "compose.subevaluation.yml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"true", group,
		"--label", "com.urnetwork.competition.job-id=11111111-2222-4333-8444-555555555555",
		"--label", "com.urnetwork.competition.round-id=66666666-7777-4888-8999-000000000000",
		"--label", "com.urnetwork.competition.run-id=" + strings.Repeat("a", 64), "--cgroup-parent", group, "--ulimit", "core=1:1",
		"--security-opt", "seccomp=" + filepath.Join(filepath.Dir(overlay), "candidate-core.seccomp.json"),
		"--label", "com.urnetwork.competition.run-id=" + strings.Repeat("a", 64), "--file", overlay,
		"--filter", "label=com.urnetwork.competition.round-id=66666666-7777-4888-8999-000000000000",
		"--filter", "label=com.urnetwork.competition.run-id=" + strings.Repeat("a", 64)}
	if got := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00"); !reflect.DeepEqual(got, want) {
		t.Fatalf("ownership arguments differ: got %q, want %q", got, want)
	}
}

// Every partial tuple, empty value, noncanonical identifier and overflowing
// attempt fails before the caller may issue its first resource operation.
func TestSubevaluationOwnershipRejectsPartialAndMalformedScope(t *testing.T) {
	complete := subevaluationOwnershipEnvironment()
	for mask := 1; mask < 15; mask++ {
		environment := []string{}
		for index, value := range complete {
			if mask&(1<<index) != 0 {
				environment = append(environment, value)
			}
		}
		if output, err := subevaluationOwnershipCommand(t, "printf 'unexpected resource admission\\n'\n", environment).CombinedOutput(); err == nil || strings.Contains(string(output), "unexpected resource admission") {
			t.Fatalf("partial scope %d reached resource admission: %v: %s", mask, err, output)
		}
	}
	for _, change := range []struct {
		index int
		value string
	}{
		{index: 0, value: ""}, {index: 1, value: ""}, {index: 2, value: ""}, {index: 3, value: ""},
		{index: 0, value: strings.Repeat("A", 64)}, {index: 0, value: strings.Repeat("a", 63)},
		{index: 1, value: "111111112222-4333-8444-5555-55555555555"},
		{index: 2, value: "66666666-7777-4888-8999-00000000000A"},
		{index: 3, value: "0"}, {index: 3, value: "02"}, {index: 3, value: "-2"},
		{index: 3, value: "9223372036854775808"}, {index: 3, value: "99999999999999999999"},
		{index: 3, value: "2\n--privileged"},
	} {
		environment := append([]string(nil), complete...)
		name, _, _ := strings.Cut(environment[change.index], "=")
		environment[change.index] = name + "=" + change.value
		if output, err := subevaluationOwnershipCommand(t, "printf 'unexpected resource admission\\n'\n", environment).CombinedOutput(); err == nil || strings.Contains(string(output), "unexpected resource admission") {
			t.Errorf("malformed field %d value %q reached admission: %v: %s", change.index, change.value, err, output)
		}
	}
	// An omitted environment value derives the exact protected release path;
	// an explicitly empty, redirected or ambiguous value may not override it.
	for _, profile := range []string{"", "/other-release/candidate-core.seccomp.json", "candidate-core.seccomp.json", "/other-release/profile\n--privileged"} {
		environment := append(append([]string(nil), complete...), "URNETWORK_SUBEVALUATION_CORE_PROFILE="+profile)
		if output, err := subevaluationOwnershipCommand(t, "printf 'unexpected resource admission\\n'\n", environment).CombinedOutput(); err == nil || strings.Contains(string(output), "unexpected resource admission") {
			t.Fatalf("substituted core profile reached resource admission: %q: %v: %s", profile, err, output)
		}
	}
}

// The validated outer scope cannot label resources for a different decoded
// evaluator request, even when all replacement identifiers are well formed.
func TestSubevaluationOwnershipRejectsDifferentRequestIdentity(t *testing.T) {
	for _, arguments := range []string{
		`66666666-7777-4888-8999-000000000000 "$URNETWORK_SUBEVALUATION_ROUND_ID" 2`,
		`"$URNETWORK_SUBEVALUATION_JOB_ID" 11111111-2222-4333-8444-555555555555 2`,
		`"$URNETWORK_SUBEVALUATION_JOB_ID" "$URNETWORK_SUBEVALUATION_ROUND_ID" 3`,
	} {
		if output, err := subevaluationOwnershipCommand(t, "subevaluation_require_identity "+arguments+"\n", subevaluationOwnershipEnvironment()).CombinedOutput(); err == nil {
			t.Errorf("different decoded request inherited ownership: %s: %s", arguments, output)
		}
	}
}

// Execute actual entrypoints with malformed scope and two independent inert
// Docker tripwires. A regression cannot contact even an available local daemon.
func TestSubevaluationHelpersValidateScopeBeforeDocker(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "unexpected-docker")
	stub := []byte("#!/bin/bash\nprintf 'unexpected Docker operation\\n' >> \"$SUBEVALUATION_TEST_TRIPWIRE\"\nexit 99\n")
	for _, name := range []string{"sudo", "docker"} {
		if err := os.WriteFile(filepath.Join(root, name), stub, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bashEnvironment := filepath.Join(root, "bash-environment")
	if err := os.WriteFile(bashEnvironment, []byte("sudo() { printf 'unexpected sudo\\n' >> \"$SUBEVALUATION_TEST_TRIPWIRE\"; return 99; }\ndocker() { printf 'unexpected docker\\n' >> \"$SUBEVALUATION_TEST_TRIPWIRE\"; return 99; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"evaluator-subevaluation.sh", "build-submission.sh", "prepare-evaluation-source.sh", "docker-id-map.sh"} {
		command := exec.CommandContext(t.Context(), "/bin/bash", filepath.Join("evaluator", "container", name), "--help")
		command.Env = []string{"PATH=" + root + ":/usr/bin:/bin", "LANG=C", "LC_ALL=C", "BASH_ENV=" + bashEnvironment,
			"SUBEVALUATION_TEST_TRIPWIRE=" + marker, subevaluationOwnershipEnvironment()[0]}
		if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "subevaluation ownership scope is incomplete or malformed") {
			t.Errorf("%s did not refuse malformed scope at entry: %v: %s", name, err, output)
		}
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatal("a malformed prospective helper contacted its Docker tripwire", err)
	}
}

// Capture the actual source-export create line so labels and cgroup admission
// are proven before creation, not inferred from a later result or cleanup trap.
func TestSubevaluationSourceExportOwnsContainerBeforeCreate(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("evaluator", "container", "prepare-evaluation-source.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var create string
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "source_container=\"$(evaluation_docker create ") {
			if create != "" {
				t.Fatal("source export gained an ambiguous create path")
			}
			create = line
		}
	}
	if create == "" {
		t.Fatal("source export lost its inspectable pre-creation boundary")
	}
	for _, scoped := range []bool{false, true} {
		capture := filepath.Join(t.TempDir(), "create-arguments")
		body := "base_image_id=sha256:" + strings.Repeat("b", 64) + "\n" +
			`sudo() { printf '%s\000' "$@" > "$SUBEVALUATION_TEST_CAPTURE"; printf '%064d\n' 0; }` + "\n" + create + "\n"
		environment := []string{"SUBEVALUATION_TEST_CAPTURE=" + capture}
		if scoped {
			environment = append(environment, subevaluationOwnershipEnvironment()...)
			client := subevaluationOwnershipClient(t, `printf '%s\000' "$@" > "$SUBEVALUATION_TEST_CAPTURE"; printf '%064d\n' 0`+"\n")
			environment = append(environment, "URNETWORK_SUBEVALUATION_CLIENT="+client)
		}
		if output, err := subevaluationOwnershipCommand(t, body, environment).CombinedOutput(); err != nil {
			t.Fatalf("source create scope %t: %v: %s", scoped, err, output)
		}
		arguments, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Split(strings.TrimSuffix(string(arguments), "\x00"), "\x00")
		if !scoped {
			if want := []string{"-n", "docker", "create", "sha256:" + strings.Repeat("b", 64)}; !reflect.DeepEqual(got, want) {
				t.Fatalf("legacy create arguments changed: %q", got)
			}
		} else {
			token := sha256.Sum256([]byte("11111111-2222-4333-8444-555555555555:2"))
			profile, err := filepath.Abs(filepath.Join("evaluator", "container", "candidate-core.seccomp.json"))
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"--docker-client", strings.Repeat("a", 64), "--", "create", "--network", "none", "--read-only", "--cap-drop", "ALL",
				"--security-opt", "no-new-privileges:true", "--memory", "4g", "--memory-swap", "4g", "--pids-limit", "1024", "--cpuset-cpus", "20,22",
				"--label", "com.urnetwork.competition.job-id=11111111-2222-4333-8444-555555555555", "--label", "com.urnetwork.competition.round-id=66666666-7777-4888-8999-000000000000",
				"--label", "com.urnetwork.competition.run-id=" + strings.Repeat("a", 64), "--cgroup-parent", "urnetwork-evaluation-" + hex.EncodeToString(token[:])[:32] + "-preflight.slice",
				"--ulimit", "core=1:1", "--security-opt", "seccomp=" + profile, "sha256:" + strings.Repeat("b", 64)}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("source container was created without exact bounded prospective ownership: got %q want %q", got, want)
			}
		}
	}
}

// Every actual remote run/create command is explicitly inventoried. This is a
// coverage tripwire, not evidence that a real daemon joined delayed requests.
func TestSubevaluationEveryTemporaryDockerPathDeclaresOwnership(t *testing.T) {
	for _, entry := range []struct {
		name  string
		count int
	}{
		{name: "evaluator-subevaluation.sh", count: 6}, {name: "build-submission.sh", count: 1},
		{name: "prepare-evaluation-source.sh", count: 1}, {name: "docker-id-map.sh", count: 1},
		{name: "tracked-submission-build.sh", count: 2},
	} {
		content, err := os.ReadFile(filepath.Join("evaluator", "container", entry.name))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(content), "\n")
		count := 0
		for index, line := range lines {
			if !strings.Contains(line, "evaluation_docker run ") && !strings.Contains(line, "evaluation_docker create ") {
				continue
			}
			count++
			block := line
			for strings.HasSuffix(lines[index], "\\") {
				index++
				if index >= len(lines) {
					t.Fatal("incomplete Docker command", entry.name)
				}
				block += "\n" + lines[index]
			}
			owned := strings.Contains(block, `"${subevaluation_docker_args[@]}"`) ||
				strings.Contains(block, `"${subevaluation_run_label_args[@]}"`) && strings.Contains(block, `--cgroup-parent "$compare_cgroup"`)
			if entry.name == "tracked-submission-build.sh" && strings.Contains(block, `--cgroup-parent "$build_cgroup_parent"`) {
				owned = true
				for _, required := range []string{
					`--label "com.urnetwork.competition.job-id=$URNETWORK_SUBEVALUATION_JOB_ID"`,
					`--label "com.urnetwork.competition.round-id=$URNETWORK_SUBEVALUATION_ROUND_ID"`,
					`--label "com.urnetwork.competition.run-id=$subevaluation_run_id"`,
					`--label "com.urnetwork.competition.builder-stage=$stage"`,
					`--security-opt "seccomp=$URNETWORK_SUBEVALUATION_CORE_PROFILE" --ulimit core=1:1`,
				} {
					if !strings.Contains(block, required) {
						owned = false
					}
				}
			}
			if !owned {
				t.Errorf("%s temporary Docker command has no declared pre-creation ownership: %s", entry.name, block)
			}
		}
		if count != entry.count {
			t.Errorf("%s temporary Docker inventory changed: got %d, want %d", entry.name, count, entry.count)
		}
	}
}

// Only the explicit prospective file supplements each existing service/network
// label map; the legacy Compose template never requires a subevaluation run id.
func TestSubevaluationComposeOverlayCoversAllOwnedObjects(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("evaluator", "container", "compose.subevaluation.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct {
		Services map[string]struct {
			Labels   map[string]string                   `yaml:"labels"`
			Ulimits  map[string]struct{ Soft, Hard int } `yaml:"ulimits"`
			Security []string                            `yaml:"security_opt"`
		} `yaml:"services"`
		Networks map[string]struct{ Labels map[string]string } `yaml:"networks"`
	}
	if err := yaml.Unmarshal(content, &overlay); err != nil {
		t.Fatal(err)
	}
	label := "${URNETWORK_SUBEVALUATION_RUN_ID:?set the authenticated subevaluation run identity}"
	if len(overlay.Services) != 5 || len(overlay.Networks) != 1 {
		t.Fatal("prospective overlay does not enumerate every Compose object")
	}
	for _, name := range []string{"postgres", "migrate", "redis", "runner", "scorer"} {
		service := overlay.Services[name]
		if labels := service.Labels; len(labels) != 1 || labels["com.urnetwork.competition.run-id"] != label {
			t.Error("prospective service lacks one exact run ownership label", name)
		}
		if limit, ok := service.Ulimits["core"]; !ok || len(service.Ulimits) != 1 || limit.Soft != 1 || limit.Hard != 1 {
			t.Error("prospective service changed its exact inherited core-one class", name)
		}
		expectedSecurity := []string{"no-new-privileges:true", "seccomp=${URNETWORK_SUBEVALUATION_CORE_PROFILE:?set the pinned inherited core profile}"}
		if !reflect.DeepEqual(service.Security, expectedSecurity) {
			t.Error("prospective service omitted the exact pinned inherited profile", name, service.Security)
		}
	}
	if labels := overlay.Networks["evaluation"].Labels; len(labels) != 1 || labels["com.urnetwork.competition.run-id"] != label {
		t.Error("prospective network lacks its exact run ownership label")
	}
	legacy, err := os.ReadFile(filepath.Join("evaluator", "container", "compose.yml"))
	if err != nil || strings.Contains(string(legacy), "SUBEVALUATION") || strings.Contains(string(legacy), "competition.run-id") {
		t.Fatal("legacy Compose gained a prospective scope prerequisite", err)
	}
}

// Exercise the real Compose adapter with an inert sudo function: only the
// future scope adds a file, and its run id crosses sudo via the stage env file.
func TestSubevaluationComposeAdapterPreservesLegacyArguments(t *testing.T) {
	function := g2ScriptFunction(t, filepath.Join("evaluator", "container", "evaluator-subevaluation.sh"), "compose_with")
	for _, scoped := range []bool{false, true} {
		body := "COMPOSE_FILE=/synthetic-release/compose.yml\n" + function + "\n" +
			`sudo() { printf '%s\000' "$@"; }` + "\ncompose_with synthetic-project /synthetic-stage/compose.env --profile run up --detach\n"
		var environment []string
		if scoped {
			environment = append(subevaluationOwnershipEnvironment(), "URNETWORK_SUBEVALUATION_CLIENT="+subevaluationOwnershipClient(t, `printf '%s\000' "$@"`+"\n"))
		}
		output, err := subevaluationOwnershipCommand(t, body, environment).CombinedOutput()
		if err != nil {
			t.Fatalf("compose scope %t: %v: %s", scoped, err, output)
		}
		want := []string{"-n", "docker", "compose", "--env-file", "/synthetic-stage/compose.env", "--file", "/synthetic-release/compose.yml"}
		if scoped {
			want = append([]string{"--docker-client", strings.Repeat("a", 64), "--", "compose"}, want[3:]...)
			overlay, err := filepath.Abs(filepath.Join("evaluator", "container", "compose.subevaluation.yml"))
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, "--file", overlay)
		}
		want = append(want, "--project-name", "synthetic-project", "--profile", "run", "up", "--detach")
		if got := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00"); !reflect.DeepEqual(got, want) {
			t.Fatalf("compose scope %t arguments differ: %q", scoped, got)
		}
	}
	script, err := os.ReadFile(filepath.Join("evaluator", "container", "evaluator-subevaluation.sh"))
	if err != nil {
		t.Fatal(err)
	}
	writer := g2ScriptFunction(t, filepath.Join("evaluator", "container", "evaluator-subevaluation.sh"), "write_compose_env")
	if !strings.Contains(writer, `"URNETWORK_SUBEVALUATION_RUN_ID=$subevaluation_run_id" >> "$pending"`) || !strings.Contains(string(script), `subevaluation_require_identity "$job_id" "$round_id" "$attempt"`) {
		t.Fatal("prospective Compose did not retain and bind the authenticated outer run scope")
	}
}

// Fake ownership metadata is limited to this test. Real bytes/hash/regular-file
// checks bind the prospective helper and overlay into the actual builder output.
func TestSubevaluationBuilderIdentityPinsOwnershipRelease(t *testing.T) {
	root := t.TempDir()
	var helper []byte
	for _, name := range []string{"submission-builder-identity.sh", "build-submission.sh", "Dockerfile.submission", "Dockerfile.submission.tracked", "tracked-submission-build.sh", "evaluator.sh", "prepare-evaluation-source.sh", "subevaluation-ownership.sh", "compose.yml", "compose.subevaluation.yml", "docker-id-map.sh"} {
		content, err := os.ReadFile(filepath.Join("evaluator", "container", name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "submission-builder-identity.sh" {
			helper = content
		}
		if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// This test proves release hashing only. Canonical profile derivation and
	// CREATE admission have their own bounded parser/native qualification gates.
	for _, name := range []string{"docker-stock.seccomp.json", "candidate-core.seccomp.json"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("{\"synthetic_hash_input\":true}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	validator := []byte("synthetic never-executed validator artifact\n")
	digest := sha256.Sum256(validator)
	if err := os.WriteFile(filepath.Join(root, "competitionpatch"), validator, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "competitionpatch.sha256"), []byte(hex.EncodeToString(digest[:])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(helper), "\nfail() {")
	if start < 0 {
		t.Fatal("builder release identity body is missing")
	}
	body := `set -Eeuo pipefail
SCRIPT_DIR="$SUBEVALUATION_TEST_RELEASE"
stat() {
    case "$2" in
        %u) if [[ "$3" == "$SUBEVALUATION_TEST_UNSAFE" ]]; then printf '1000\n'; else printf '0\n'; fi ;;
        %a) printf '644\n' ;;
        *) command stat "$@" ;;
    esac
}
` + string(helper[start:])
	run := func(unsafe string) (map[string]string, error) {
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body)
		command.Env = append([]string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "SUBEVALUATION_TEST_RELEASE=" + root, "SUBEVALUATION_TEST_UNSAFE=" + unsafe}, subevaluationOwnershipEnvironment()...)
		output, err := command.CombinedOutput()
		if err != nil {
			return nil, err
		}
		var manifest map[string]json.RawMessage
		if err := json.Unmarshal(output, &manifest); err != nil {
			return nil, err
		}
		values := map[string]string{}
		for _, name := range []string{"builder_sha256", "subevaluation_ownership_sha256", "subevaluation_compose_sha256", "subevaluation_docker_id_map_sha256", "tracked_dockerfile_sha256", "tracked_builder_sha256", "stock_seccomp_profile_sha256", "candidate_core_profile_sha256", "compose_base_sha256"} {
			var value string
			if err := json.Unmarshal(manifest[name], &value); err != nil {
				return nil, err
			}
			values[name] = value
		}
		return values, nil
	}
	original, err := run("")
	if err != nil {
		t.Fatal("valid synthetic ownership release rejected", err)
	}
	for _, file := range []struct{ name, field string }{
		{name: "subevaluation-ownership.sh", field: "subevaluation_ownership_sha256"}, {name: "compose.subevaluation.yml", field: "subevaluation_compose_sha256"},
		{name: "docker-id-map.sh", field: "subevaluation_docker_id_map_sha256"},
		{name: "Dockerfile.submission.tracked", field: "tracked_dockerfile_sha256"}, {name: "tracked-submission-build.sh", field: "tracked_builder_sha256"},
		{name: "docker-stock.seccomp.json", field: "stock_seccomp_profile_sha256"}, {name: "candidate-core.seccomp.json", field: "candidate_core_profile_sha256"},
		{name: "compose.yml", field: "compose_base_sha256"},
	} {
		path := filepath.Join(root, file.name)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		expected := sha256.Sum256(content)
		if original[file.field] != hex.EncodeToString(expected[:]) {
			t.Fatal("builder release omitted the exact ownership file digest", file.name)
		}
		if _, err := run(path); err == nil {
			t.Error("builder accepted unprotected prospective ownership bytes", file.name)
		}
		if err := os.WriteFile(path, append(content, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		changed, err := run("")
		if err != nil || changed["builder_sha256"] == original["builder_sha256"] || changed[file.field] == original[file.field] {
			t.Fatal("builder cache identity did not retain changed ownership bytes", file.name, err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The baseline producer keeps its sealed no-candidate purpose. A private
// lifecycle helper cannot silently replace it, even when scoring policy agrees.
func TestSubevaluationBaselinePrefixRejectsPrivateEvaluator(t *testing.T) {
	path := filepath.Join("evaluator", "container", "evaluator-minimum-work.sh")
	function := g2ScriptFunction(t, filepath.Join("evaluator", "container", "prepare-round-baseline-minimum-work.sh"), "round_baseline_control_prefix")
	run := func(source string) error {
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", "set -Eeuo pipefail\n"+function+"\nround_baseline_control_prefix \"$1\" >/dev/null\n", "synthetic-baseline-prefix", source)
		command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
		return command.Run()
	}
	if err := run(path); err != nil {
		t.Fatal("baseline no longer authenticates its exact sealed control prefix", err)
	}
	if err := run(filepath.Join("evaluator", "container", "evaluator-subevaluation.sh")); err == nil {
		t.Fatal("private candidate lifecycle replaced the no-candidate baseline release")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := filepath.Join(t.TempDir(), "changed-evaluator.sh")
	if err := os.WriteFile(changed, append(content, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(changed); err == nil {
		t.Fatal("prospective baseline accepted changed, unqualified evaluator bytes")
	}
}
