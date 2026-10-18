// Executes trusted shell validators with synthetic inputs only. No Docker,
// live repository refs, or network access is used by these regressions.
package main

import (
	"context"
	"encoding/json"
	"go/version"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

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
		name        string
		staging     bool
		recordStage bool
		branch      string
		margin      float64
		wantSuccess bool
	}{
		{name: "production", branch: "sim-latency", margin: 16.1, wantSuccess: true},
		{name: "staging", staging: true, recordStage: true, branch: stagingEvaluationSourceBranch, wantSuccess: true},
		{name: "implicit-stage", staging: true, branch: stagingEvaluationSourceBranch},
		{name: "stage-production-branch", staging: true, recordStage: true, branch: "sim-latency"},
		{name: "stage-positive-margin", staging: true, recordStage: true, branch: stagingEvaluationSourceBranch, margin: 16.1},
		{name: "production-zero-margin", branch: "sim-latency"},
		{name: "production-stage-branch", branch: stagingEvaluationSourceBranch, margin: 16.1},
	} {
		record := sourceRecord{Schema: 1, Staging: fixture.recordStage, Epoch: 0, Branch: fixture.branch,
			SignificantImprovementPercent: fixture.margin, Repositories: sourceTestCommits(strings.Repeat("a", 40))}
		recordBytes, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(t.Context(), "bash", "-c", body)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "LC_ALL=C", "source_epoch=0",
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
	repositoryRoot := filepath.Join("..", "..")
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
