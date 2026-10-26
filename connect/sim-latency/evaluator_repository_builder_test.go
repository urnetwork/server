// Exercises the installed-release and whole-repository identity boundaries
// without Docker, root ownership changes, or operational source checkouts.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Extracts the real builder function, keeping the fixture outside its gates.
func submissionBuilderFunction(t *testing.T, name string) string {
	t.Helper()
	return sourceGitScriptSection(t, "build-submission.sh", name+"() {\n", "\n}\n") + "\n}\n"
}

// A broad policy on an old image must fail before nine controls, not only
// when source application begins after those controls have finished.
func TestEvaluatorChecksBroadImageCapabilityBeforeAnyControl(t *testing.T) {
	for _, fixture := range []struct {
		policy     string
		capability string
		want       bool
	}{
		{want: true},
		{policy: "mono_root_v1"},
		{policy: "mono_root_v1", capability: "mono_root_v1", want: true},
		{capability: "mono_root_v1", want: true},
		{policy: "unknown", capability: "unknown"},
	} {
		identity := map[string]any{
			"schema": 1, "kind": "sim-latency-evaluation-source", "temporary": true,
			"base_image_id": "sha256:" + strings.Repeat("a", 64), "base_sha": strings.Repeat("b", 40),
			"source_epoch": 0, "staging": false, "branch": "sim-latency", "candidate_patch_sha256": nil,
		}
		if fixture.capability != "" {
			identity["candidate_path_base"] = fixture.capability
		}
		content, err := json.Marshal(identity)
		if err != nil {
			t.Fatal(err)
		}
		body := `set -Eeuo pipefail
base_image_id="sha256:$(printf 'a%.0s' {1..64})"
base_sha="$(printf 'b%.0s' {1..40})"
source_epoch=0
staging=false
source_branch=sim-latency
patch_path_base="$BUILDER_TEST_POLICY"
` + evaluatorShellFunction(t, "validate_prepared_source_identity") + "validate_prepared_source_identity \"$BUILDER_TEST_IDENTITY\"\n"
		command := exec.CommandContext(t.Context(), "bash", "-c", body)
		command.Env = append(os.Environ(), "BUILDER_TEST_POLICY="+fixture.policy, "BUILDER_TEST_IDENTITY="+string(content))
		output, err := command.CombinedOutput()
		if (err == nil) != fixture.want {
			t.Fatalf("policy=%q image=%q: error=%v output=%s", fixture.policy, fixture.capability, err, output)
		}
	}
	scriptBytes, err := os.ReadFile(filepath.Join("evaluator", "container", "evaluator.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	gate := strings.Index(script, "\n    validate_prepared_source_identity \"$prepared_identity\"")
	control := strings.Index(script, "\n        run_stage baseline \"$i\"")
	if gate < 0 || control < 0 || gate >= control {
		t.Fatal("prospective image capability is not checked before the baseline control stage")
	}
}

// A dependency-only candidate may retain its server revision. Every other
// repository and each tool/source digest must nevertheless authenticate.
func TestSubmissionBuilderAuthenticatesWholeCandidateIdentity(t *testing.T) {
	commits := sourceTestCommits(strings.Repeat("a", 40))
	commits["connect"] = strings.Repeat("b", 40)
	identity := map[string]any{
		"schema": 1, "image_kind": "submission", "base_sha": commits["server"], "build_sha": commits["server"],
		"patch_sha256": strings.Repeat("c", 64), "policy_sha256": strings.Repeat("d", 64),
		"builder_sha256": strings.Repeat("e", 64), "image_key": strings.Repeat("f", 64),
		"patch_validator_sha256": strings.Repeat("1", 64), "candidate_commits_sha256": strings.Repeat("2", 64),
		"original_source_identity_sha256": strings.Repeat("3", 64), "candidate_commits": commits,
		"simulator_sha256": strings.Repeat("4", 64), "paths": []string{"connect/source.txt"},
	}
	labels := map[string]any{
		"com.urnetwork.competition.image-kind": "submission", "com.urnetwork.competition.base-sha": commits["server"],
		"org.opencontainers.image.revision": commits["server"], "com.urnetwork.competition.patch-sha256": identity["patch_sha256"],
		"com.urnetwork.competition.policy-sha256": identity["policy_sha256"], "com.urnetwork.competition.builder-sha256": identity["builder_sha256"],
		"com.urnetwork.competition.image-key": identity["image_key"], "com.urnetwork.competition.patch-validator-sha256": identity["patch_validator_sha256"],
		"com.urnetwork.competition.candidate-commits-sha256": identity["candidate_commits_sha256"],
		"com.urnetwork.competition.source-identity-sha256":   identity["original_source_identity_sha256"],
	}
	commitsBytes, err := json.Marshal(commits)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		field string
		label string
	}{
		{},
		{field: "patch_validator_sha256", label: "com.urnetwork.competition.patch-validator-sha256"},
		{field: "candidate_commits_sha256", label: "com.urnetwork.competition.candidate-commits-sha256"},
		{field: "original_source_identity_sha256", label: "com.urnetwork.competition.source-identity-sha256"},
		{field: "candidate_commits"},
	} {
		actualIdentity, actualLabels := maps.Clone(identity), maps.Clone(labels)
		if fixture.field == "candidate_commits" {
			changed := maps.Clone(commits)
			changed["sdk"] = strings.Repeat("9", 40)
			actualIdentity[fixture.field] = changed
		} else if fixture.field != "" {
			actualIdentity[fixture.field] = strings.Repeat("9", 64)
			actualLabels[fixture.label] = strings.Repeat("9", 64)
		}
		identityBytes, err := json.Marshal(actualIdentity)
		if err != nil {
			t.Fatal(err)
		}
		labelBytes, err := json.Marshal(actualLabels)
		if err != nil {
			t.Fatal(err)
		}
		prelude := `set -Eeuo pipefail
base_sha="$BUILDER_TEST_SERVER"
candidate_sha="$BUILDER_TEST_SERVER"
patch_sha256="$(printf 'c%.0s' {1..64})"
policy_sha256="$(printf 'd%.0s' {1..64})"
builder_sha256="$(printf 'e%.0s' {1..64})"
image_key="$(printf 'f%.0s' {1..64})"
patch_validator_sha256="$(printf '1%.0s' {1..64})"
candidate_commits_sha256="$(printf '2%.0s' {1..64})"
original_source_identity_sha256="$(printf '3%.0s' {1..64})"
candidate_commits_json="$BUILDER_TEST_COMMITS"
`
		for _, check := range []struct{ function, document string }{
			{function: "validate_submission_labels", document: string(labelBytes)},
			{function: "validate_submission_runtime_identity", document: string(identityBytes)},
		} {
			command := exec.CommandContext(t.Context(), "bash", "-c", prelude+submissionBuilderFunction(t, check.function)+check.function+" \"$BUILDER_TEST_DOCUMENT\"\n")
			command.Env = append(os.Environ(), "BUILDER_TEST_SERVER="+commits["server"], "BUILDER_TEST_COMMITS="+string(commitsBytes), "BUILDER_TEST_DOCUMENT="+check.document)
			output, err := command.CombinedOutput()
			wantSuccess := fixture.field == "" || fixture.field == "candidate_commits" && check.function == "validate_submission_labels"
			if (err == nil) != wantSuccess {
				t.Errorf("%s changed %q: error=%v, output=%s", check.function, fixture.field, err, output)
			}
		}
	}
}

// Executes the real host branch through an external validator fixture. The
// actual validator's local-Git application is exercised by its own CLI tests.
func TestSubmissionBuilderRequiresAuthenticatedBroadImageBeforeApplication(t *testing.T) {
	for _, fixture := range []struct {
		name          string
		capability    string
		missingLock   bool
		changedLock   bool
		validatorExit string
		wantExit      int
	}{
		{name: "dependency-only", capability: "mono_root_v1"},
		{name: "old-image", wantExit: 2},
		{name: "missing-lock", capability: "mono_root_v1", missingLock: true, wantExit: 2},
		{name: "changed-lock", capability: "mono_root_v1", changedLock: true, wantExit: 2},
		{name: "application-infrastructure", capability: "mono_root_v1", validatorExit: "2", wantExit: 2},
		{name: "canonical-refusal", capability: "mono_root_v1", validatorExit: "1", wantExit: 1},
	} {
		root := t.TempDir()
		sourceRoot := filepath.Join(root, "source")
		contextRoot := filepath.Join(root, "context")
		for _, directory := range []string{sourceRoot, contextRoot} {
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		commits := sourceTestCommits(strings.Repeat("a", 40))
		lock := map[string]any{"schema": 1, "development_snapshot": false, "repositories": commits}
		if fixture.capability != "" {
			lock["candidate_path_base"] = fixture.capability
		}
		lockBytes, err := json.Marshal(lock)
		if err != nil {
			t.Fatal(err)
		}
		lockDigest := sha256.Sum256(lockBytes)
		lockHex := hex.EncodeToString(lockDigest[:])
		if !fixture.missingLock {
			if fixture.changedLock {
				lockBytes = append(lockBytes, '\n')
			}
			if err := os.WriteFile(filepath.Join(sourceRoot, ".evaluation-source-lock.json"), lockBytes, 0o400); err != nil {
				t.Fatal(err)
			}
		}
		identity := map[string]any{
			"schema": 1, "kind": "sim-latency-evaluation-source", "temporary": true,
			"base_image_id": "sha256:" + strings.Repeat("b", 64), "base_sha": commits["server"],
			"staging": false, "branch": "sim-latency", "source_lock_sha256": lockHex,
			"repositories": commits, "candidate_patch_sha256": nil,
		}
		if fixture.capability != "" {
			identity["candidate_path_base"] = fixture.capability
		}
		identityBytes, err := json.Marshal(identity)
		if err != nil {
			t.Fatal(err)
		}
		identityPath := filepath.Join(sourceRoot, ".evaluation-source.json")
		if err := os.WriteFile(identityPath, identityBytes, 0o400); err != nil {
			t.Fatal(err)
		}
		candidateCommits := maps.Clone(commits)
		candidateCommits["connect"] = strings.Repeat("c", 40)
		receiptBytes, err := json.Marshal(map[string]any{
			"schema": 1, "base_sha": commits["server"], "patch_sha256": strings.Repeat("d", 64),
			"policy_sha256": strings.Repeat("e", 64), "candidate_commits": candidateCommits,
		})
		if err != nil {
			t.Fatal(err)
		}
		validatorPath := filepath.Join(root, "validator")
		validator := `#!/usr/bin/env bash
set -Eeuo pipefail
printf 'called\n' > "$BUILDER_TEST_CALLED"
identity=""
expected=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --source-identity) identity="$2" ;;
        --expected-source-identity-sha256) expected="$2" ;;
    esac
    shift 2
done
[[ "$(sha256sum "$identity" | awk '{print $1}')" == "$expected" ]]
if [[ -n "$BUILDER_TEST_VALIDATOR_EXIT" ]]; then exit "$BUILDER_TEST_VALIDATOR_EXIT"; fi
printf '%s\n' "$BUILDER_TEST_RECEIPT"
`
		if err := os.WriteFile(validatorPath, []byte(validator), 0o700); err != nil {
			t.Fatal(err)
		}
		prelude := `set -Eeuo pipefail
source_root="$BUILDER_TEST_SOURCE"
build_context="$BUILDER_TEST_CONTEXT"
base_image_id="sha256:$(printf 'b%.0s' {1..64})"
base_sha="$(printf 'a%.0s' {1..40})"
source_lock_sha256="$BUILDER_TEST_LOCK_SHA256"
SOURCE_APPLICATION_TIMEOUT_SECONDS=70
staging=false
source_branch=sim-latency
patch_path_base=mono_root_v1
patch_sha256="$(printf 'd%.0s' {1..64})"
policy_sha256="$(printf 'e%.0s' {1..64})"
trusted_validator="$BUILDER_TEST_VALIDATOR"
infrastructure_failure() { printf 'infrastructure failure: %s\n' "$*" >&2; exit 2; }
`
		policySection := sourceGitScriptSection(t, "build-submission.sh", "REPOSITORIES=(", "\nbase_sha=")
		applicationSection := sourceGitScriptSection(t, "build-submission.sh", "source_identity=", "\nimage_key=")
		command := exec.CommandContext(t.Context(), "bash", "-c", prelude+policySection+"\n"+applicationSection)
		calledPath := filepath.Join(root, "called")
		command.Env = append(os.Environ(), "BUILDER_TEST_SOURCE="+sourceRoot, "BUILDER_TEST_CONTEXT="+contextRoot,
			"BUILDER_TEST_LOCK_SHA256="+lockHex, "BUILDER_TEST_VALIDATOR="+validatorPath, "BUILDER_TEST_CALLED="+calledPath,
			"BUILDER_TEST_RECEIPT="+string(receiptBytes), "BUILDER_TEST_VALIDATOR_EXIT="+fixture.validatorExit)
		output, err := command.CombinedOutput()
		exitCode := 0
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if exitCode != fixture.wantExit {
			t.Fatalf("%s: exit=%d, want %d: %s", fixture.name, exitCode, fixture.wantExit, output)
		}
		_, markerErr := os.Stat(calledPath)
		wantCalled := fixture.capability == "mono_root_v1" && !fixture.missingLock && !fixture.changedLock
		if (markerErr == nil) != wantCalled {
			t.Fatalf("%s: application crossed the image capability gate: %v", fixture.name, markerErr)
		}
		actualIdentity, err := os.ReadFile(identityPath)
		if err != nil {
			t.Fatal(err)
		}
		if fixture.wantExit != 0 {
			if string(actualIdentity) != string(identityBytes) {
				t.Fatalf("%s: refusal changed the source identity", fixture.name)
			}
			continue
		}
		var candidateIdentity struct {
			Repositories map[string]string `json:"repositories"`
		}
		if err := json.Unmarshal(actualIdentity, &candidateIdentity); err != nil || !maps.Equal(candidateIdentity.Repositories, candidateCommits) {
			t.Fatalf("dependency-only candidate did not bind its full commit map: %v: %s", err, actualIdentity)
		}
	}
}

// Simulates metadata only. Bytes, symlinks and executable/hash checks use the
// real temporary files; this fixture never claims actual root installation.
func TestSubmissionBuilderReleaseIdentityBindsAllTrustedTools(t *testing.T) {
	root := t.TempDir()
	helperBytes, err := os.ReadFile(filepath.Join("evaluator", "container", "submission-builder-identity.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"submission-builder-identity.sh", "build-submission.sh", "Dockerfile.submission", "evaluator.sh", "prepare-evaluation-source.sh"} {
		content, err := os.ReadFile(filepath.Join("evaluator", "container", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	validator := []byte("synthetic validator artifact\n")
	validatorDigest := sha256.Sum256(validator)
	validatorHex := hex.EncodeToString(validatorDigest[:])
	if err := os.WriteFile(filepath.Join(root, "competitionpatch"), validator, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "competitionpatch.sha256"), []byte(validatorHex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(helperBytes), "\nfail() {")
	if start < 0 {
		t.Fatal("release identity helper is missing its protected execution body")
	}
	body := `set -Eeuo pipefail
SCRIPT_DIR="$BUILDER_TEST_RELEASE"
stat() {
    case "$2" in
        %u) if [[ "$3" == "$BUILDER_TEST_UNSAFE" ]]; then printf '1000\n'; else printf '0\n'; fi ;;
        %a) printf '644\n' ;;
        *) command stat "$@" ;;
    esac
}
` + string(helperBytes[start:])
	run := func(unsafe string) ([]byte, error) {
		command := exec.CommandContext(t.Context(), "bash", "-c", body)
		command.Env = append(os.Environ(), "BUILDER_TEST_RELEASE="+root, "BUILDER_TEST_UNSAFE="+unsafe)
		return command.CombinedOutput()
	}
	output, err := run("")
	if err != nil {
		t.Fatalf("synthetic release: %v: %s", err, output)
	}
	var manifest map[string]any
	if err := json.Unmarshal(output, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["patch_validator_sha256"] != validatorHex || manifest["kind"] != "sim-latency-submission-builder/v2" {
		t.Fatalf("unbound release identity: %s", output)
	}
	originalBuilder := manifest["builder_sha256"]
	for _, name := range []string{"build-submission.sh", "Dockerfile.submission", "evaluator.sh", "prepare-evaluation-source.sh", "submission-builder-identity.sh"} {
		path := filepath.Join(root, name)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(content, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		changed, err := run("")
		if err != nil || json.Unmarshal(changed, &manifest) != nil || manifest["builder_sha256"] == originalBuilder {
			t.Fatalf("%s was absent from the composite identity: %v: %s", name, err, changed)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, unsafe := range []string{root, filepath.Join(root, "competitionpatch")} {
		if output, err := run(unsafe); err == nil {
			t.Fatalf("release accepted unprotected path %s: %s", unsafe, output)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "competitionpatch.sha256"), []byte(strings.Repeat("9", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := run(""); err == nil {
		t.Fatalf("release accepted a mismatched validator pin: %s", output)
	}
	if err := os.Remove(filepath.Join(root, "competitionpatch.sha256")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "evaluator.sh"), filepath.Join(root, "competitionpatch.sha256")); err != nil {
		t.Fatal(err)
	}
	if output, err := run(""); err == nil {
		t.Fatalf("release accepted a symlinked pin: %s", output)
	}
}

// These are source-structure assertions, not container qualification. They
// preserve independent replay and the discarded unprivileged execution stage.
func TestSubmissionDockerReplaysAuthenticatedWholeSource(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("evaluator", "container", "Dockerfile.submission"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(content)
	validatorCheck := strings.Index(dockerfile, `test "$(sha256sum /opt/urnetwork/submission/competitionpatch`)
	identityCheck := strings.Index(dockerfile, `test "$(sha256sum /opt/urnetwork/submission/original-source.json`)
	mapCheck := strings.Index(dockerfile, `test "$(sha256sum /opt/urnetwork/submission/candidate-commits.json`)
	apply := strings.Index(dockerfile, "    timeout --signal=TERM --kill-after=5s 70s /opt/urnetwork/submission/competitionpatch \\\n")
	private := strings.Index(dockerfile, "    chmod 0700 /workspace;")
	readable := strings.Index(dockerfile, "    chmod 0755 /workspace")
	unprivileged := strings.Index(dockerfile, "USER 65532:65532")
	if validatorCheck < 0 || identityCheck < 0 || mapCheck < 0 || apply < 0 || private < 0 || readable < 0 || unprivileged < 0 || !(validatorCheck < apply && identityCheck < apply && mapCheck < apply && private < apply && apply < readable && readable < unprivileged) {
		t.Fatal("Docker replay lost hash-before-execute or private-apply/readable-build ordering")
	}
	for _, required := range []string{
		"COPY --chmod=0555 competitionpatch /opt/urnetwork/submission/competitionpatch",
		"--apply-source-root /workspace", "--source-identity /opt/urnetwork/submission/original-source.json",
		".candidate_commits == $expected[0]", `.candidate_path_base == "mono_root_v1"`,
		"FROM source-prep AS candidate-check", "FROM source-prep AS candidate-binary", "FROM source-prep AS final",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Docker replay lost %q", required)
		}
	}
	if strings.Contains(dockerfile, "/opt/urnetwork/bin/competitionpatch") || strings.Contains(dockerfile, "FROM candidate-check AS") {
		t.Fatal("candidate image trusts a stale base validator or package-init filesystem")
	}
	buildBytes, err := os.ReadFile(filepath.Join("evaluator", "container", "build-submission.sh"))
	if err != nil {
		t.Fatal(err)
	}
	build := string(buildBytes)
	if strings.Count(build, "validate_submission_labels \"") != 2 || !strings.Contains(build, `"$base_image_id" "$patch_sha256" "$policy_sha256" "$builder_sha256" | sha256sum`) {
		t.Fatal("cache and fresh build no longer share the composite release identity gates")
	}
}
