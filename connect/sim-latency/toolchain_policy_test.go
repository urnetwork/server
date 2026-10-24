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

// A build argument override cannot silently produce an evaluator with another Go.
func TestEvaluatorBasePinsExactCompetitionToolchain(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("evaluator", "container", "Dockerfile.base"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"ARG GO_IMAGE=golang:1.27.3-bookworm@sha256:",
		"GOTOOLCHAIN=local",
		`RUN test "$(go env GOVERSION)" = go1.27.3`,
	} {
		if !strings.Contains(string(content), required) {
			t.Errorf("base image is missing exact toolchain enforcement: %s", required)
		}
	}
}
