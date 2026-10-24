// Executes trusted shell validators with synthetic inputs only. No Docker,
// live repository refs, or network access is used by these regressions.
package main

import (
	"context"
	"encoding/json"
	"go/version"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

// Zero is explicit staging only; production retains the finite positive cap.
func TestSourceMarginRequiresExplicitMode(t *testing.T) {
	for _, fixture := range []struct {
		percent    float64
		production bool
		staging    bool
	}{
		{percent: 0, staging: true},
		{percent: 16.1, production: true},
		{percent: 50, production: true},
		{percent: math.SmallestNonzeroFloat64, production: true},
		{percent: -1},
		{percent: 50.1},
		{percent: math.NaN()},
		{percent: math.Inf(1)},
		{percent: math.Inf(-1)},
	} {
		if got := validSourceMargin(fixture.percent, false); got != fixture.production {
			t.Fatalf("production margin %v accepted=%t, want %t", fixture.percent, got, fixture.production)
		}
		if got := validSourceMargin(fixture.percent, true); got != fixture.staging {
			t.Fatalf("staging margin %v accepted=%t, want %t", fixture.percent, got, fixture.staging)
		}
	}
}

// Exercise the real CLI mode parser and margin selector without Docker. The
// old positive-only filter rejected the valid staging record before runtime.
func TestEvaluatorSmokeMarginBindsExplicitModeAndSourceBranch(t *testing.T) {
	modeParser := sourceGitScriptSection(t, "smoke-test.sh", "base_image=\"\"\n", "\nfor command in ")
	marginSelector := sourceGitScriptSection(t, "smoke-test.sh", "takeover_margin=\"$(", "\n\nsudo -n docker run")
	body := "set -Eeuo pipefail\n" + modeParser + "\n" + marginSelector + officialBaselineMarginFixture(t) + "\nprintf '%s\\n' \"$takeover_margin\"\n"
	for _, fixture := range []struct {
		name    string
		staging bool
		record  string
		want    string
	}{
		{name: "production-omitted-mode", record: `{"schema":1,"branch":"sim-latency","significant_improvement_percent":16.1}`, want: "0.161"},
		{name: "production-explicit-false", record: `{"schema":1,"staging":false,"branch":"sim-latency","significant_improvement_percent":50}`, want: "0.5"},
		{name: "staging-zero", staging: true, record: `{"schema":1,"staging":true,"branch":"sim-latency-staging","significant_improvement_percent":0}`, want: "0.0"},
		{name: "staging-positive", staging: true, record: `{"schema":1,"staging":true,"branch":"sim-latency-staging","significant_improvement_percent":16.1}`},
		{name: "staging-positive-underflow", staging: true, record: `{"schema":1,"staging":true,"branch":"sim-latency-staging","significant_improvement_percent":1e-323}`},
		{name: "staging-negative", staging: true, record: `{"schema":1,"staging":true,"branch":"sim-latency-staging","significant_improvement_percent":-1}`},
		{name: "staging-mode-omitted", staging: true, record: `{"schema":1,"branch":"sim-latency-staging","significant_improvement_percent":0}`},
		{name: "staging-mode-false", staging: true, record: `{"schema":1,"staging":false,"branch":"sim-latency-staging","significant_improvement_percent":0}`},
		{name: "staging-production-branch", staging: true, record: `{"schema":1,"staging":true,"branch":"sim-latency","significant_improvement_percent":0}`},
		{name: "production-zero", record: `{"schema":1,"branch":"sim-latency","significant_improvement_percent":0}`},
		{name: "production-positive-underflow", record: `{"schema":1,"branch":"sim-latency","significant_improvement_percent":1e-323}`},
		{name: "production-over-cap", record: `{"schema":1,"branch":"sim-latency","significant_improvement_percent":50.1}`},
		{name: "production-staging-mode", record: `{"schema":1,"staging":true,"branch":"sim-latency","significant_improvement_percent":16.1}`},
		{name: "production-staging-branch", record: `{"schema":1,"branch":"sim-latency-staging","significant_improvement_percent":16.1}`},
		{name: "production-null-mode", record: `{"schema":1,"staging":null,"branch":"sim-latency","significant_improvement_percent":16.1}`},
		{name: "staging-string-margin", staging: true, record: `{"schema":1,"staging":true,"branch":"sim-latency-staging","significant_improvement_percent":"0"}`},
		{name: "staging-missing-margin", staging: true, record: `{"schema":1,"staging":true,"branch":"sim-latency-staging"}`},
		{name: "staging-unknown-schema", staging: true, record: `{"schema":2,"staging":true,"branch":"sim-latency-staging","significant_improvement_percent":0}`},
	} {
		args := []string{"-c", body, "synthetic-smoke", "synthetic-base"}
		if fixture.staging {
			args = append(args, "--staging")
		}
		command := exec.CommandContext(t.Context(), "bash", args...)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "LC_ALL=C", "base_source_record=" + fixture.record, "SOURCE_TEST_ROOT=" + t.TempDir()}
		output, err := command.CombinedOutput()
		if fixture.want != "" {
			if err != nil || strings.TrimSpace(string(output)) != fixture.want {
				t.Fatalf("%s: margin output=%q error=%v, want %q", fixture.name, output, err, fixture.want)
			}
		} else if err == nil {
			t.Fatalf("%s: accepted mismatched source record with margin %q", fixture.name, output)
		}
	}
}

// Use the immutable wrapper's actual lexical guard and authenticated-threshold
// comparison. Only its source-check process and binary hash I/O are synthetic.
func officialBaselineMarginFixture(t *testing.T) string {
	t.Helper()
	guard := sourceGitScriptSection(t, "../../official-run.sh", "    [[ \"$APEX_TAKEOVER_MARGIN\"", "\n    \"$APEX_SCORER_BIN\" score-baseline")
	return `
APEX_TAKEOVER_MARGIN="$takeover_margin"
APEX_BASELINE_MANIFEST="$SOURCE_TEST_ROOT/baseline.json"
APEX_SCORER_BIN=/synthetic/scorer
APEX_SCORER_SHA256=synthetic-scorer-digest
APEX_SIM_BIN=synthetic_source_check
APEX_EPOCH=0
die() { printf '%s\n' "$*" >&2; exit 1; }
sha256_file() { printf '%s\n' "$APEX_SCORER_SHA256"; }
synthetic_source_check() {
    [ "$*" = 'source-check --epoch 0 --source-config /opt/urnetwork/sim-latency.yml --repos-root /workspace --json' ] || exit 97
    jq '. + {epoch: 0}' <<<"$base_source_record"
}
check_official_baseline_margin() {
` + guard + "\n}\ncheck_official_baseline_margin\n"
}

// The real evaluation path needs the same zero serialization as smoke. A
// valid format alone cannot bypass the immutable source's threshold identity.
func TestEvaluatorScoreMarginUsesOfficialDecimalAndFrozenThreshold(t *testing.T) {
	selector := sourceGitScriptSection(t, "evaluator.sh", "takeover_margin=\"$(", "\nqueue_limit=")
	for _, fixture := range []struct {
		name          string
		staging       bool
		margin        any
		sourcePercent float64
		want          string
	}{
		{name: "staging-zero", staging: true, margin: 0, want: "0.0"},
		{name: "production-positive", margin: 0.161, sourcePercent: 16.1, want: "0.161"},
		{name: "production-cap", margin: 0.5, sourcePercent: 50, want: "0.5"},
		{name: "staging-positive", staging: true, margin: 0.1, sourcePercent: 10},
		{name: "staging-negative", staging: true, margin: -0.1, sourcePercent: -10},
		{name: "production-zero", margin: 0},
		{name: "production-over-cap", margin: 0.51, sourcePercent: 51},
		{name: "staging-source-mismatch", staging: true, margin: 0, sourcePercent: 16.1},
		{name: "production-source-mismatch", margin: 0.1, sourcePercent: 16.1},
		{name: "staging-string-zero", staging: true, margin: "0"},
	} {
		root := t.TempDir()
		requestPath := filepath.Join(root, "request.json")
		request, err := json.Marshal(map[string]any{"evaluation_policy": map[string]any{"takeover_margin": fixture.margin}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(requestPath, request, 0o600); err != nil {
			t.Fatal(err)
		}
		record, err := json.Marshal(map[string]any{"schema": 1, "staging": fixture.staging, "branch": sourceBranch(fixture.staging), "significant_improvement_percent": fixture.sourcePercent})
		if err != nil {
			t.Fatal(err)
		}
		body := "set -Eeuo pipefail\ndie() { printf '%s\\n' \"$*\" >&2; exit 1; }\n" + selector + officialBaselineMarginFixture(t) + "\nprintf '%s\\n' \"$takeover_margin\"\n"
		command := exec.CommandContext(t.Context(), "bash", "-c", body)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "LC_ALL=C", "request_path=" + requestPath,
			"staging=" + strconv.FormatBool(fixture.staging), "SOURCE_TEST_ROOT=" + root, "base_source_record=" + string(record)}
		output, err := command.CombinedOutput()
		if fixture.want != "" {
			if err != nil || strings.TrimSpace(string(output)) != fixture.want {
				t.Fatalf("%s: official margin output=%q error=%v, want %q", fixture.name, output, err, fixture.want)
			}
		} else if err == nil {
			t.Fatalf("%s: official margin accepted %q", fixture.name, output)
		}
	}
}

// Runs the actual base builder's record gate before it can clone or build.
func TestEvaluatorBaseRecordRequiresExplicitMatchingStagingMode(t *testing.T) {
	content, err := os.ReadFile("evaluator/container/build-base.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	start := strings.Index(script, "\n    jq -e \\\n")
	end := strings.Index(script[start+1:], "\nfi\n")
	if start < 0 || end < 0 {
		t.Fatal("base builder source-record validator is missing")
	}
	body := "set -Eeuo pipefail\n" + script[start:start+1+end] + "\n"
	for _, fixture := range []struct {
		name           string
		staging        bool
		recordStage    bool
		branch         string
		margin         float64
		includeRetired bool
		wantSuccess    bool
	}{
		{name: "production", branch: "sim-latency", margin: 16.1, wantSuccess: true},
		{name: "production-retired-repository", branch: "sim-latency", margin: 16.1, includeRetired: true},
		{name: "staging", staging: true, recordStage: true, branch: stagingEvaluationSourceBranch, wantSuccess: true},
		{name: "implicit-stage", staging: true, branch: stagingEvaluationSourceBranch},
		{name: "stage-production-branch", staging: true, recordStage: true, branch: "sim-latency"},
		{name: "stage-positive-margin", staging: true, recordStage: true, branch: stagingEvaluationSourceBranch, margin: 16.1},
		{name: "stage-retired-repository", staging: true, recordStage: true, branch: stagingEvaluationSourceBranch, includeRetired: true},
		{name: "production-zero-margin", branch: "sim-latency"},
		{name: "production-stage-branch", branch: stagingEvaluationSourceBranch, margin: 16.1},
	} {
		record := sourceRecord{Schema: 1, Staging: fixture.recordStage, Epoch: 0, Branch: fixture.branch,
			SignificantImprovementPercent: fixture.margin, Repositories: sourceTestCommits(strings.Repeat("a", 40))}
		if fixture.includeRetired {
			record.Repositories["operator-proxy"] = strings.Repeat("b", 40)
		}
		recordBytes, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(t.Context(), "bash", "-c", body)
		repositoryNames, err := json.Marshal(sourceRepositoryNamesForMode(fixture.staging))
		if err != nil {
			t.Fatal(err)
		}
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "LC_ALL=C", "source_epoch=0", "repository_names_json=" + string(repositoryNames),
			"staging=" + strconv.FormatBool(fixture.staging), "source_branch=" + sourceBranch(fixture.staging), "source_record=" + string(recordBytes)}
		output, err := command.CombinedOutput()
		if (err == nil) != fixture.wantSuccess {
			t.Fatalf("%s: source record gate error=%v output=%s", fixture.name, err, output)
		}
	}
}

// Reads the exact Go policy from a pinned Git object, not a moving worktree,
// then compares it with the digest-pinned Docker toolchain used for new builds.
func evaluatorGoPinSupportsModule(moduleBytes, dockerfileBytes []byte) bool {
	module, err := modfile.Parse("go.mod", moduleBytes, nil)
	if err != nil || module.Go == nil {
		return false
	}
	match := regexp.MustCompile(`(?m)^ARG GO_IMAGE=golang:([0-9]+\.[0-9]+(?:\.[0-9]+)?)-[a-z0-9]+@sha256:[0-9a-f]{64}$`).FindSubmatch(dockerfileBytes)
	if len(match) != 2 {
		return false
	}
	actual, required := "go"+string(match[1]), "go"+module.Go.Version
	if module.Toolchain != nil && version.Compare(module.Toolchain.Name, required) > 0 {
		required = module.Toolchain.Name
	}
	return version.IsValid(actual) && version.IsValid(required) && version.Compare(actual, required) >= 0
}

// The old 1.26.5 image could not build a server frozen with go 1.27.2. Synthetic
// fixtures prove the guard catches that root cause without downloading images.
func TestEvaluatorBaseGoPinSupportsFrozenServerModule(t *testing.T) {
	module := []byte("module synthetic.example/server\n\ngo 1.27.2\n")
	oldImage := []byte("ARG GO_IMAGE=golang:1.26.5-bookworm@sha256:" + strings.Repeat("a", 64) + "\n")
	newImage := []byte("ARG GO_IMAGE=golang:1.27.2-bookworm@sha256:" + strings.Repeat("b", 64) + "\n")
	if evaluatorGoPinSupportsModule(module, oldImage) || !evaluatorGoPinSupportsModule(module, newImage) {
		t.Fatal("digest-pinned toolchain compatibility guard did not reject the stale image")
	}
	if evaluatorGoPinSupportsModule(append(module, []byte("toolchain go1.27.3\n")...), newImage) {
		t.Fatal("newer explicitly pinned toolchain was ignored")
	}
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	head, err := sourceGitText(context.Background(), repositoryRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	lockedModule, err := sourceGitBlobAtCommit(context.Background(), repositoryRoot, head, "go.mod", maximumSourceConfigBytes)
	if err != nil {
		t.Fatal(err)
	}
	dockerfile, err := os.ReadFile("evaluator/container/Dockerfile.base")
	if err != nil || !evaluatorGoPinSupportsModule(lockedModule, dockerfile) {
		t.Fatalf("new evaluator toolchain is older than frozen server %s: %v", head, err)
	}
}

// Patch releases also change the minimum toolchain: the old digest-pinned
// 1.27.2 image must not satisfy a module that now requires 1.27.3.
func TestEvaluatorGoPinRejectsPreviousPatchRelease(t *testing.T) {
	module := []byte("module synthetic.example/server\n\ngo 1.27.3\n")
	stale := []byte("ARG GO_IMAGE=golang:1.27.2-bookworm@sha256:" + strings.Repeat("a", 64) + "\n")
	current := []byte("ARG GO_IMAGE=golang:1.27.3-bookworm@sha256:" + strings.Repeat("b", 64) + "\n")
	if evaluatorGoPinSupportsModule(module, stale) || !evaluatorGoPinSupportsModule(module, current) {
		t.Fatal("patch-level module requirement did not reject the previous compiler")
	}
	workingModule, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile, err := os.ReadFile("evaluator/container/Dockerfile.base")
	if err != nil || !evaluatorGoPinSupportsModule(workingModule, dockerfile) {
		t.Fatalf("evaluator toolchain does not support the current module: %v", err)
	}
}
