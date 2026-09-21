// Keeps the frozen evaluator's exact repository protocol consistent across
// consumers while allowing the main control-plane module graph to evolve.
package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Reads the network-free `go mod edit -json` shape without treating a moving
// main checkout as authority to remove repositories from an existing lock.
func requiredLocalSourceRepositories(moduleBytes []byte) ([]string, error) {
	var module sourceModuleGraphFixture
	if err := json.Unmarshal(moduleBytes, &module); err != nil {
		return nil, err
	}
	requiredVersions := map[string]string{}
	for _, requirement := range module.Require {
		requiredVersions[requirement.Path] = requirement.Version
	}
	repositoryNames := []string{filepath.Base(module.Module.Path)}
	for _, replacement := range module.Replace {
		requiredVersion, required := requiredVersions[replacement.Old.Path]
		if !required || replacement.New.Version != "" ||
			(replacement.Old.Version != "" && replacement.Old.Version != requiredVersion) {
			continue
		}
		repository := filepath.Base(replacement.New.Path)
		if replacement.New.Path != "../"+repository {
			return nil, fmt.Errorf("required local module %s is outside the sibling-repository layout: %s", replacement.Old.Path, replacement.New.Path)
		}
		repositoryNames = append(repositoryNames, repository)
	}
	slices.Sort(repositoryNames)
	return repositoryNames, nil
}

// Main can shed a dependency that the frozen evaluator still needs. A newly
// required unpinned local repository must nevertheless fail closed.
func requireLocalSourceRepositoryCoverage(moduleBytes []byte, lockedRepositoryNames []string) error {
	repositoryNames, err := requiredLocalSourceRepositories(moduleBytes)
	if err != nil {
		return err
	}
	for _, repository := range repositoryNames {
		if !slices.Contains(lockedRepositoryNames, repository) {
			return fmt.Errorf("required local repository %s is absent from frozen source repositories %v", repository, lockedRepositoryNames)
		}
	}
	return nil
}

// A real local module read catches newly required dependencies. The exact
// evaluator graph comes from the source protocol/checkpoint, not current main.
func TestSourceRepositorySetMatchesRequiredLocalModules(t *testing.T) {
	command := exec.Command("go", "mod", "edit", "-json", filepath.Join("..", "..", "go.mod"))
	moduleBytes, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := requireLocalSourceRepositoryCoverage(moduleBytes, sourceRepositoryNames()); err != nil {
		t.Fatal(err)
	}
}

// This independent fixture prevents a removed main dependency from silently
// shrinking every serializer, validator, image copy, and source ledger together.
func TestSourceRepositorySetPreservesFrozenEvaluatorGraph(t *testing.T) {
	want := []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "operator-proxy", "warp"}
	if got := sourceRepositoryNames(); !slices.Equal(got, want) {
		t.Fatalf("frozen evaluator repositories = %v, want %v", got, want)
	}
}

// Synthetic module inputs contain no checkout paths, remote Git, or network
// resolution; they exercise required/local/versioned replacement semantics.
type sourceModuleVersionFixture struct {
	Path    string
	Version string
}

type sourceModuleReplacementFixture struct {
	Old sourceModuleVersionFixture
	New sourceModuleVersionFixture
}

type sourceModuleGraphFixture struct {
	Module  sourceModuleVersionFixture
	Require []sourceModuleVersionFixture
	Replace []sourceModuleReplacementFixture
}

func sourceModuleGraphBytes(t *testing.T, repositories []string) []byte {
	t.Helper()
	module := sourceModuleGraphFixture{Module: sourceModuleVersionFixture{Path: "module.example/server"}}
	for _, repository := range repositories {
		if repository == "server" {
			continue
		}
		dependency := sourceModuleVersionFixture{Path: "module.example/" + repository, Version: "v0.0.0"}
		module.Require = append(module.Require, dependency)
		module.Replace = append(module.Replace, sourceModuleReplacementFixture{
			Old: dependency, New: sourceModuleVersionFixture{Path: "../" + repository},
		})
	}
	encoded, err := json.Marshal(module)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// Removing Warp on main must not change the ten-repository evaluator lock.
func TestSourceRepositoryCoverageRetainsFrozenDependencyRemovedFromMain(t *testing.T) {
	locked := sourceRepositoryNames()
	frozen := slices.Clone(locked)
	currentMain := slices.DeleteFunc(slices.Clone(locked), func(repository string) bool { return repository == "warp" })
	for _, graph := range [][]string{frozen, currentMain} {
		if err := requireLocalSourceRepositoryCoverage(sourceModuleGraphBytes(t, graph), locked); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(locked, frozen) || !slices.Contains(locked, "warp") {
		t.Fatalf("main module inspection changed the frozen lock: %v", locked)
	}
}

func TestSourceRepositoryCoverageRejectsNewUnpinnedDependency(t *testing.T) {
	locked := sourceRepositoryNames()
	required := append(slices.Clone(locked), "synthetic-new-dependency")
	err := requireLocalSourceRepositoryCoverage(sourceModuleGraphBytes(t, required), locked)
	if err == nil || !strings.Contains(err.Error(), "synthetic-new-dependency is absent") {
		t.Fatalf("new unpinned local module accepted: %v", err)
	}
}

func TestSourceRepositoryCoverageRetainsSiblingLayoutBoundary(t *testing.T) {
	var module sourceModuleGraphFixture
	if err := json.Unmarshal(sourceModuleGraphBytes(t, []string{"server", "connect"}), &module); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../../connect", "/synthetic/connect", "../connect/nested"} {
		module.Replace[0].New.Path = path
		encoded, err := json.Marshal(module)
		if err != nil {
			t.Fatal(err)
		}
		if err := requireLocalSourceRepositoryCoverage(encoded, sourceRepositoryNames()); err == nil || !strings.Contains(err.Error(), "outside the sibling-repository layout") {
			t.Fatalf("non-sibling module %q accepted: %v", path, err)
		}
	}
}

// Every independently deployed shell consumer must agree with the Go policy,
// including both its iteration list and its exact-set JSON validator.
func TestEvaluatorSourceToolsUseCompleteRepositorySet(t *testing.T) {
	repositoryNames := sourceRepositoryNames()
	repositoryList := strings.Join(repositoryNames, " ")
	slices.Sort(repositoryNames)
	policyPattern := regexp.MustCompile(`--argjson repositories '(\[[^'\n]+\])'`)
	for _, fixture := range []struct {
		path         string
		iteration    string
		validateJson bool
	}{
		{path: "evaluator/container/build-base.sh", iteration: "readonly REPOSITORIES=(" + repositoryList + ")", validateJson: true},
		{path: "evaluator/container/prepare-evaluation-source.sh", iteration: "readonly REPOSITORIES=(" + repositoryList + ")", validateJson: true},
		{path: "evaluator/container/build-submission.sh", iteration: "for repository in " + repositoryList + "; do", validateJson: true},
		{path: "evaluator/container/evaluator.sh", iteration: "for repository in " + repositoryList + "; do", validateJson: false},
		{path: "run-main.sh", iteration: "", validateJson: true},
	} {
		scriptBytes, err := os.ReadFile(fixture.path)
		if err != nil {
			t.Fatal(err)
		}
		script := string(scriptBytes)
		if fixture.iteration != "" && strings.Count(script, fixture.iteration) != 1 {
			t.Errorf("%s does not iterate exactly the complete source set", fixture.path)
		}
		if fixture.validateJson {
			matches := policyPattern.FindAllStringSubmatch(script, -1)
			if len(matches) != 1 {
				t.Fatalf("%s has %d repository validators, want 1", fixture.path, len(matches))
			}
			var policyRepositoryNames []string
			if err := json.Unmarshal([]byte(matches[0][1]), &policyRepositoryNames); err != nil {
				t.Fatal(err)
			}
			slices.Sort(policyRepositoryNames)
			if !slices.Equal(policyRepositoryNames, repositoryNames) {
				t.Errorf("%s validates %v, want %v", fixture.path, policyRepositoryNames, repositoryNames)
			}
		}
	}
}

// Image copying must not omit a locked dependency or silently add an unlocked
// repository beside the authenticated source graph.
func TestEvaluatorBaseCopiesExactlyLockedRepositories(t *testing.T) {
	dockerfileBytes, err := os.ReadFile(filepath.Join("evaluator", "container", "Dockerfile.base"))
	if err != nil {
		t.Fatal(err)
	}
	copyPattern := regexp.MustCompile(`(?m)^COPY source/([^\s]+) /workspace/([^\s]+)$`)
	repositoryNames := []string{}
	for _, match := range copyPattern.FindAllStringSubmatch(string(dockerfileBytes), -1) {
		if match[1] != match[2] {
			t.Errorf("image remaps source repository %s to %s", match[1], match[2])
		}
		repositoryNames = append(repositoryNames, match[1])
	}
	slices.Sort(repositoryNames)
	lockedRepositoryNames := sourceRepositoryNames()
	slices.Sort(lockedRepositoryNames)
	if !slices.Equal(repositoryNames, lockedRepositoryNames) {
		t.Fatalf("image source repositories = %v, want %v", repositoryNames, lockedRepositoryNames)
	}
}

// Execute the builder's real commit collection and lock serializer without
// Docker, remote Git, or Bash associative arrays, including hyphenated names.
func TestEvaluatorBaseSourceLockPreservesAllCommits(t *testing.T) {
	scriptBytes, err := os.ReadFile(filepath.Join("evaluator", "container", "build-base.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	initializationStart := strings.Index(script, "\nreadonly REPOSITORIES=(")
	initializationEnd := strings.Index(script, "\n\nsource_record=")
	collectionStart := strings.Index(script, "\nfor repository in \"${REPOSITORIES[@]}\"; do\n")
	collectionEnd := strings.Index(script, "\nsource_lock_sha256=")
	if initializationStart < 0 || initializationEnd <= initializationStart || collectionStart < initializationEnd || collectionEnd <= collectionStart {
		t.Fatal("base source-lock collection or serializer is missing")
	}
	workspaceRoot := t.TempDir()
	repositoryCommits := map[string]string{}
	for index, repository := range sourceRepositoryNames() {
		commit := fmt.Sprintf("%040x", index+1)
		repositoryCommits[repository] = commit
		if err := os.MkdirAll(filepath.Join(workspaceRoot, repository, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	recordBytes, err := json.Marshal(sourceRecord{Repositories: repositoryCommits})
	if err != nil {
		t.Fatal(err)
	}
	const prelude = `set -Eeuo pipefail
# Enforce the Bash 3.2 boundary even when the host has a newer Bash.
declare() {
    [ "${1:-}" != -A ] || { printf 'fixture associative arrays are unavailable\n' >&2; return 2; }
    builtin declare "$@"
}
overlay_worktree() { :; }
git() {
    [ "$1" != init ] || return 0
    [ "$1" = -C ] || return 91
    local repository="${2##*/}"
    shift 2
    case "$1" in
        remote)
            case "$2" in
                get-url) printf 'file://%s/%s\n' "$WORKSPACE_ROOT" "$repository" ;;
                add) : ;;
                *) return 92 ;;
            esac ;;
        fetch|checkout|status) : ;;
        rev-parse)
            [ "$2" = HEAD ] || return 93
            jq -er --arg repository "$repository" '.repositories[$repository]' <<<"$source_record" ;;
        *) printf 'unexpected Git dependency: %s\n' "$*" >&2; return 94 ;;
    esac
}
`
	body := prelude + script[initializationStart:initializationEnd] + "\n" + script[collectionStart:collectionEnd] + "\nprintf '%s\\n' \"$base_sha\"\n"
	for _, includeWorktree := range []bool{false, true} {
		buildContext := t.TempDir()
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body)
		command.Env = []string{
			"PATH=" + os.Getenv("PATH"), "LANG=C", "LC_ALL=C",
			"WORKSPACE_ROOT=" + workspaceRoot, "build_context=" + buildContext,
			"source_record=" + string(recordBytes), "include_worktree=" + strconv.FormatBool(includeWorktree),
		}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("include_worktree=%t: serialize base source lock: %v: %s", includeWorktree, err, output)
		}
		if strings.TrimSpace(string(output)) != repositoryCommits["server"] {
			t.Fatalf("include_worktree=%t: base SHA = %q, want %s", includeWorktree, output, repositoryCommits["server"])
		}
		lockPath := filepath.Join(buildContext, "source-lock.json")
		if !includeWorktree {
			if _, err := loadEvaluatorSourceLock(lockPath); err != nil {
				t.Fatal(err)
			}
		}
		lockBytes, err := os.ReadFile(lockPath)
		if err != nil {
			t.Fatal(err)
		}
		var lock evaluatorSourceLock
		if err := json.Unmarshal(lockBytes, &lock); err != nil {
			t.Fatal(err)
		}
		if lock.Schema != 1 || lock.DevelopmentSnapshot != includeWorktree || !maps.Equal(lock.Repositories, repositoryCommits) {
			t.Fatalf("include_worktree=%t: base source lock = %s, want commits %v", includeWorktree, lockBytes, repositoryCommits)
		}
	}
}

// Exercise the actual staging preflight before any API side effect. A matching
// count alone must not accept a substituted dependency or a malformed commit.
func TestRunMainStagingSourceRequiresExactRepositorySet(t *testing.T) {
	scriptBytes, err := os.ReadFile("run-main.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	start := strings.Index(script, "\nverify_staging_source() {\n")
	end := strings.Index(script, "\nlatest_source_epoch() {\n")
	if start < 0 || end <= start {
		t.Fatal("staging source preflight is missing")
	}
	binaryPath := filepath.Join(t.TempDir(), "sim-latency")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/bash\nprintf '%s\\n' \"$SOURCE_TEST_RECORD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	prelude := `set -Eeuo pipefail
source_config=/synthetic/source.yml
workspace_root=/synthetic/repositories
sim_binary() { printf '%s\n' "$SOURCE_TEST_BINARY"; }
fail() { printf '%s\n' "$*" >&2; exit 1; }
write_evidence() { [[ $1 == staging-source.json ]]; printf '%s\n' "$2"; }
`
	for _, fixture := range []struct {
		name   string
		omit   string
		extra  string
		change string
	}{
		{name: "complete"},
		{name: "missing-operator-proxy", omit: "operator-proxy"},
		{name: "missing-warp", omit: "warp"},
		{name: "unexpected", extra: "unexpected"},
		{name: "substituted", omit: "warp", extra: "unexpected"},
		{name: "malformed-commit", change: "operator-proxy"},
	} {
		repositoryCommits := sourceTestCommits(strings.Repeat("a", 40))
		delete(repositoryCommits, fixture.omit)
		if fixture.extra != "" {
			repositoryCommits[fixture.extra] = strings.Repeat("b", 40)
		}
		if fixture.change != "" {
			repositoryCommits[fixture.change] = "invalid"
		}
		recordBytes, err := json.Marshal(sourceRecord{Schema: 1, Epoch: 0, Branch: stagingEvaluationSourceBranch, Repositories: repositoryCommits})
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command("bash", "-c", prelude+script[start:end]+"\nverify_staging_source\n")
		command.Env = append(os.Environ(), "SOURCE_TEST_BINARY="+binaryPath, "SOURCE_TEST_RECORD="+string(recordBytes))
		output, err := command.CombinedOutput()
		if fixture.name == "complete" {
			if err != nil || strings.TrimSpace(string(output)) != string(recordBytes) {
				t.Fatalf("complete staging source: %v: %s", err, output)
			}
		} else if err == nil || !strings.Contains(string(output), "staging source check returned an invalid identity") {
			t.Errorf("%s staging source: error = %v, output = %s", fixture.name, err, output)
		}
	}
}

// The source preparer sees only synthetic Docker responses and test-local Git
// clones; no daemon, real image, root privileges, or operator checkout is used.
func TestPrepareEvaluationSourceAuthenticatesCompleteRepositorySet(t *testing.T) {
	root := t.TempDir()
	imageRoot := filepath.Join(root, "image")
	commandRoot := filepath.Join(root, "bin")
	for _, directory := range []string{imageRoot, commandRoot} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repositoryCommits := map[string]string{}
	for _, repository := range sourceRepositoryNames() {
		repositoryCommits[repository] = sourceTestRepository(t, imageRoot, repository)
		sourceTestGit(t, filepath.Join(imageRoot, repository), "checkout", "--quiet", "--detach")
	}
	sudoFixture := `#!/usr/bin/env bash
set -Eeuo pipefail
[[ $1 == -n ]]
shift
case $1 in
    chown) exit 0 ;;
    chmod|rm) exec "$@" ;;
    docker) shift ;;
    *) exit 91 ;;
esac
case $1 in
    info|rm) ;;
    create) printf '%s\n' synthetic-source-container ;;
    image)
        [[ $2 == inspect && $3 == --format ]]
        case $4 in
            '{{.Id}}') printf '%s\n' "$SOURCE_TEST_IMAGE_ID" ;;
            *image-kind*) printf '%s\n' evaluator-base ;;
            *base-sha*) git -C "$SOURCE_TEST_IMAGE_ROOT/server" rev-parse HEAD ;;
            *source-epoch*) printf '%s\n' 0 ;;
            *source-lock-sha256*) sha256sum "$SOURCE_TEST_IMAGE_ROOT/source-lock.json" | awk '{print $1}' ;;
            *) exit 92 ;;
        esac
        ;;
    cp)
        case $2 in
            synthetic-source-container:/opt/urnetwork/source-lock.json)
                cp "$SOURCE_TEST_IMAGE_ROOT/source-lock.json" "$3"
                ;;
            synthetic-source-container:/workspace/*)
                cp -a "$SOURCE_TEST_IMAGE_ROOT/${2##*/}" "$3"
                ;;
            *) exit 93 ;;
        esac
        ;;
    *) exit 94 ;;
esac
`
	if err := os.WriteFile(filepath.Join(commandRoot, "sudo"), []byte(sudoFixture), 0o700); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	const gitFixture = `#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "$SOURCE_TEST_FAIL_STATUS" == true && "$3" == status ]]; then
    printf 'synthetic Git status failure\n' >&2
    exit 128
fi
exec "$SOURCE_TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(commandRoot, "git"), []byte(gitFixture), 0o700); err != nil {
		t.Fatal(err)
	}
	imageId := "sha256:" + strings.Repeat("b", 64)
	for _, fixture := range []struct {
		name       string
		omit       string
		extra      string
		change     string
		failStatus bool
		wantErr    string
	}{
		{name: "complete"},
		{name: "git-status-failure", failStatus: true, wantErr: "synthetic Git status failure"},
		{name: "missing-operator-proxy", omit: "operator-proxy", wantErr: "source lock is malformed"},
		{name: "missing-warp", omit: "warp", wantErr: "source lock is malformed"},
		{name: "unexpected", extra: "unexpected", wantErr: "source lock is malformed"},
		{name: "substituted", omit: "warp", extra: "unexpected", wantErr: "source lock is malformed"},
		{name: "operator-proxy-mismatch", change: "operator-proxy", wantErr: "repository operator-proxy does not match the source lock"},
		{name: "warp-mismatch", change: "warp", wantErr: "repository warp does not match the source lock"},
	} {
		lockedCommits := maps.Clone(repositoryCommits)
		if fixture.omit != "" {
			delete(lockedCommits, fixture.omit)
		}
		if fixture.extra != "" {
			lockedCommits[fixture.extra] = strings.Repeat("c", 40)
		}
		if fixture.change != "" {
			lockedCommits[fixture.change] = strings.Repeat("d", 40)
		}
		lockBytes, err := json.Marshal(evaluatorSourceLock{Schema: 1, Repositories: lockedCommits})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(imageRoot, "source-lock.json"), lockBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(root, fixture.name)
		command := exec.Command("bash", "evaluator/container/prepare-evaluation-source.sh", "--base-image", imageId, "--destination", destination)
		command.Env = append(os.Environ(),
			"PATH="+commandRoot+string(os.PathListSeparator)+os.Getenv("PATH"),
			"SOURCE_TEST_IMAGE_ROOT="+imageRoot,
			"SOURCE_TEST_IMAGE_ID="+imageId,
			"SOURCE_TEST_REAL_GIT="+gitPath,
			fmt.Sprintf("SOURCE_TEST_FAIL_STATUS=%t", fixture.failStatus),
		)
		output, err := command.CombinedOutput()
		if fixture.wantErr != "" {
			if err == nil || !strings.Contains(string(output), fixture.wantErr) {
				t.Fatalf("%s: error = %v, output = %s, want %q", fixture.name, err, output, fixture.wantErr)
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatalf("%s: rejected source checkout survived cleanup: %v", fixture.name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("prepare complete source: %v: %s", err, output)
		}
		var identity struct {
			BaseImageId  string            `json:"base_image_id"`
			Repositories map[string]string `json:"repositories"`
		}
		if err := json.Unmarshal(output, &identity); err != nil {
			t.Fatal(err)
		}
		if identity.BaseImageId != imageId || !maps.Equal(identity.Repositories, repositoryCommits) {
			t.Fatalf("prepared source identity = %+v, want image %s and repositories %v", identity, imageId, repositoryCommits)
		}
		for _, repository := range sourceRepositoryNames() {
			preparedRoot := filepath.Join(destination, repository)
			if branch := sourceTestGit(t, preparedRoot, "symbolic-ref", "--short", "HEAD"); branch != "sim-latency" {
				t.Errorf("prepared %s branch = %s", repository, branch)
			}
			if status := sourceTestGit(t, preparedRoot, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
				t.Errorf("prepared %s is dirty: %s", repository, status)
			}
			if branch := sourceTestGit(t, filepath.Join(imageRoot, repository), "rev-parse", "--abbrev-ref", "HEAD"); branch != "HEAD" {
				t.Errorf("image fixture %s was modified: branch = %s", repository, branch)
			}
		}
	}
}
