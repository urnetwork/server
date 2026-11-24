// Compiler selection is fixed across base images and offline promotion phases.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every executable phase refuses a toolchain upgrade before invoking Go work.
func TestSourceBuildSandboxPinsCompetitionToolchain(t *testing.T) {
	for _, phase := range []string{"metadata", "check", "correctness", "compile"} {
		input := sourceSandboxFixture()
		input.Phase = phase
		input.MetadataDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), phase, "metadata")
		input.OutputDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), phase, "output")
		args, err := sourceBuildSandboxArguments(input, sourceSandboxTestSettings())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(args, "--env=GOTOOLCHAIN=local") {
			t.Fatalf("%s permits automatic compiler selection", phase)
		}
		if phase != "metadata" && !strings.HasPrefix(args[len(args)-1], `test "$(go env GOVERSION)" = go1.27.3;`) {
			t.Fatalf("%s does not reject a mismatched compiler before work", phase)
		}
	}
}

// The host build refuses old/new compilers before invoking a build. The
// accepted compiler reaches the build with automatic switching disabled.
func TestHostMakeBuildRequiresExactCompetitionToolchain(t *testing.T) {
	makeCommand, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is unavailable")
	}
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"go1.26.6", "go1.27.2", "go1.27.4", "go1.27.3"} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "Makefile"), makefile, 0600); err != nil {
			t.Fatal(err)
		}
		stub := `#!/bin/sh
test "$GOTOOLCHAIN" = local || exit 91
case "$1:$2" in
  env:GOOS) printf 'linux\n' ;;
  env:GOARCH) printf 'amd64\n' ;;
  env:GOVERSION) printf '%s\n' "$SYNTHETIC_GO_VERSION" ;;
  build:*) printf 'build\n' > "$SYNTHETIC_BUILD_MARKER" ;;
  *) exit 92 ;;
esac
`
		compiler := filepath.Join(root, "go")
		if err := os.WriteFile(compiler, []byte(stub), 0700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(root, "built")
		command := exec.Command(makeCommand, "build", "GO="+compiler)
		command.Dir = root
		command.Env = append(os.Environ(), "SYNTHETIC_GO_VERSION="+version, "SYNTHETIC_BUILD_MARKER="+marker)
		output, runErr := command.CombinedOutput()
		_, markerErr := os.Stat(marker)
		if version == "go1.27.3" {
			if runErr != nil || markerErr != nil {
				t.Fatalf("required compiler did not build: %v, %v: %s", runErr, markerErr, output)
			}
		} else if runErr == nil || !os.IsNotExist(markerErr) || !strings.Contains(string(output), "requires Go 1.27.3") {
			t.Fatalf("wrong compiler %s reached build: %v, %v: %s", version, runErr, markerErr, output)
		}
	}
}

// Direct scripts must fail before database preflight or worker construction.
func TestCompetitionEntrypointsPinLocalToolchain(t *testing.T) {
	for _, name := range []string{"tests.sh", "run-local-main.sh", "run-main.sh"} {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content), "GOTOOLCHAIN=local") || !strings.Contains(string(content), "go env GOVERSION") || !strings.Contains(string(content), "go1.27.3") {
			t.Errorf("%s does not enforce the competition compiler", name)
		}
	}
}

// A build argument override cannot silently produce an evaluator with another
// Go. The base moved to go1.27.4 with server's module (QD-674, QD-1393); the
// guards that judge what it builds move at the owner's re-qualification (owner
// decision 272).
func TestEvaluatorBasePinsExactCompetitionToolchain(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("evaluator", "container", "Dockerfile.base"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"ARG GO_IMAGE=golang:1.27.4-bookworm@sha256:",
		"GOTOOLCHAIN=local",
		`RUN test "$(go env GOVERSION)" = go1.27.4`,
	} {
		if !strings.Contains(string(content), required) {
			t.Errorf("base image is missing exact toolchain enforcement: %s", required)
		}
	}
	if strings.Contains(string(content), "1.27.3") {
		t.Error("base image still names the previous toolchain")
	}
}

// Runs build-base.sh's own refusal on a Dockerfile holding the given lines.
func evaluatorBaseGoImageRecorded(t *testing.T, dockerfile string) (bool, string) {
	t.Helper()
	function := g2ScriptFunction(t, filepath.Join("evaluator", "container", "build-base.sh"), "require_recorded_go_image")
	path := filepath.Join(t.TempDir(), "Dockerfile.base")
	if err := os.WriteFile(path, []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/bash", "-c", "set -Eeuo pipefail\n"+function+"\nrequire_recorded_go_image \"$1\"", "synthetic-go-image", path)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	output, err := command.CombinedOutput()
	return err == nil, string(output)
}

// The base is built only from a recorded Go image digest: the all-zero digest
// that stands for an unrecorded one, a missing or malformed digest and an
// ambiguous pin are refused before Docker is asked for anything.
func TestEvaluatorBaseRefusesAnUnrecordedGoImageDigest(t *testing.T) {
	recorded := "ARG GO_IMAGE=golang:1.27.4-bookworm@sha256:" + strings.Repeat("a", 64) + "\n"
	if ok, output := evaluatorBaseGoImageRecorded(t, "# synthetic\n"+recorded+"FROM ${GO_IMAGE} AS go-toolchain\n"); !ok {
		t.Fatalf("a recorded digest was refused: %s", output)
	}
	for _, dockerfile := range []string{
		"ARG GO_IMAGE=golang:1.27.4-bookworm@sha256:" + strings.Repeat("0", 64) + "\n",
		"ARG GO_IMAGE=golang:1.27.4-bookworm\n",
		"ARG GO_IMAGE=golang:1.27.4-bookworm@sha256:\n",
		"ARG GO_IMAGE=golang:1.27.4-bookworm@sha256:" + strings.Repeat("A", 64) + "\n",
		"ARG GO_IMAGE=golang:1.27.4-bookworm@sha256:" + strings.Repeat("a", 63) + "\n",
		"ARG GO_IMAGE=golang:1.27.4@sha256:" + strings.Repeat("a", 64) + "\n",
		recorded + recorded,
		"FROM golang:1.27.4-bookworm\n",
	} {
		ok, output := evaluatorBaseGoImageRecorded(t, dockerfile)
		if ok || !strings.Contains(output, "the evaluator Go image digest is not recorded") {
			t.Errorf("Go image pin %q was accepted: %s", dockerfile, output)
		}
	}
}

// Until the owner records the golang:1.27.4 index digest at the go1.27.4
// re-qualification (owner decision 272, QD-1393), this tree's base holds the
// unrecorded digest and build-base.sh refuses it first, before it reads a
// source record, clones a repository or asks Docker for anything.
func TestEvaluatorBaseGoImageAwaitsItsRecordedDigest(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("evaluator", "container", "Dockerfile.base"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "\nARG GO_IMAGE=golang:1.27.4-bookworm@sha256:"+strings.Repeat("0", 64)+"\n") {
		t.Fatal("the base names a Go image digest that was not recorded at the owner's re-qualification")
	}
	if ok, output := evaluatorBaseGoImageRecorded(t, string(dockerfile)); ok {
		t.Fatalf("the base's unrecorded Go image was accepted: %s", output)
	}
	script, err := os.ReadFile(filepath.Join("evaluator", "container", "build-base.sh"))
	if err != nil {
		t.Fatal(err)
	}
	refusal := strings.Index(string(script), "\nrequire_recorded_go_image \"$SCRIPT_DIR/Dockerfile.base\" || exit 1\n")
	if refusal < 0 || strings.Count(string(script), "require_recorded_go_image \"") != 1 {
		t.Fatal("build-base.sh does not refuse an unrecorded Go image exactly once")
	}
	for _, later := range []string{"\nsudo -n docker info", "\nbuild_context=", "source-record", "git init --quiet", "docker build"} {
		if at := strings.Index(string(script), later); at < 0 || at < refusal {
			t.Errorf("build-base.sh reaches %q before it refuses an unrecorded Go image", later)
		}
	}
}

// A disposable script checkout must compile its own fixture, not the caller's
// module or inherited workspace. Execute the real shell compile boundary.
func TestResourceBombBuildOwnsModuleAndToolchain(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("evaluator", "container", "test-resource-bomb-cleanup.sh"))
	if err != nil {
		t.Fatal(err)
	}
	_, afterCleanup, found := strings.Cut(string(content), "\ncleanup_job_objects\n")
	if !found {
		t.Fatal("resource-bomb fixture build boundary is missing")
	}
	build, _, found := strings.Cut(afterCleanup, "\ninstall -m 0444 ")
	if !found {
		t.Fatal("resource-bomb image boundary is missing")
	}
	for _, version := range []string{"go1.27.3", "go1.26.6", "go1.27.4"} {
		root := t.TempDir()
		module := filepath.Join(root, "snapshot", "server")
		caller := filepath.Join(root, "unrelated")
		for _, directory := range []string{module, caller} {
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
		}
		stub := `#!/bin/sh
test "$PWD" = "$SERVER_ROOT" || { echo wrong-module >&2; exit 91; }
test "$GOWORK" = off || { echo wrong-workspace >&2; exit 92; }
test "$GOTOOLCHAIN" = local || { echo automatic-toolchain >&2; exit 93; }
case "$1:$2" in
  env:GOVERSION) printf '%s\n' "$SYNTHETIC_GO_VERSION" ;;
  build:*)
    test "$CGO_ENABLED" = 0 || exit 94
    test "$5" = ./connect/sim-latency/evaluator/container/testdata/resource-bomb || exit 95
    printf 'built\n' > "$SYNTHETIC_BUILD_MARKER"
    ;;
  *) exit 96 ;;
esac
`
		if err := os.WriteFile(filepath.Join(root, "go"), []byte(stub), 0700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(root, "built")
		command := exec.Command("bash", "-c", "set -Eeuo pipefail; die() { printf '%s\\n' \"$*\" >&2; exit 1; }; "+build)
		command.Dir = caller
		command.Env = append(os.Environ(),
			"PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"),
			"SERVER_ROOT="+module,
			"build_context="+root,
			"GOWORK="+filepath.Join(caller, "foreign.work"),
			"GOTOOLCHAIN=auto",
			"SYNTHETIC_GO_VERSION="+version,
			"SYNTHETIC_BUILD_MARKER="+marker,
		)
		output, runErr := command.CombinedOutput()
		_, markerErr := os.Stat(marker)
		if version == "go1.27.3" {
			if runErr != nil || markerErr != nil {
				t.Fatalf("snapshot fixture did not build in its own module: %v, %v: %s", runErr, markerErr, output)
			}
		} else if runErr == nil || !os.IsNotExist(markerErr) || !strings.Contains(string(output), "requires Go 1.27.3") {
			t.Fatalf("wrong fixture compiler %s reached build: %v, %v: %s", version, runErr, markerErr, output)
		}
	}
}
