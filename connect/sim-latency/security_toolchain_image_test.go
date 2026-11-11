// A hotfix image keeps historical sources but must rebuild its executable
// payload with the exact approved compiler, without merging old compiler files.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the Dockerfile's compiler guard: unsupported compilers cannot reach
// a build marker. Static checks bind that guard to the isolated compiler tree.
func TestSecurityToolchainImageRejectsCompilerMismatch(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("evaluator", "container", "Dockerfile.security-toolchain"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(content)
	for _, required := range []string{
		"COPY toolchain/ /opt/urnetwork/security-toolchain/",
		"GOROOT=/opt/urnetwork/security-toolchain",
		"PATH=/opt/urnetwork/security-toolchain/bin:",
		"GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off",
		"go build -p=1 -mod=readonly -trimpath -o /usr/local/libexec/competitionpatch",
		"go build -p=1 -mod=readonly -trimpath -o /usr/local/libexec/competitiondbinit",
		"go build -p=1 -mod=readonly -trimpath -o /opt/urnetwork/bin/sim-latency",
		"grep -Fx TestScoreProducesSharedContract",
		"test -z \"$(git status --porcelain --untracked-files=all)\"",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Fatalf("security hotfix lost required build boundary: %s", required)
		}
	}
	_, run, found := strings.Cut(dockerfile, "RUN --network=none ")
	if !found {
		t.Fatal("offline compiler guard is missing")
	}
	guard, _, found := strings.Cut(run, " \\\n")
	if !found || guard != `test "$(go env GOVERSION)" = go1.27.4` {
		t.Fatalf("compiler guard is not exact: %q", guard)
	}
	for _, version := range []string{"go1.27.3", "go1.27.4", "go1.27.5"} {
		root := t.TempDir()
		compiler := "#!/bin/sh\ntest \"$GOTOOLCHAIN\" = local || exit 91\nprintf '%s\\n' \"$SYNTHETIC_GO_VERSION\"\n"
		if err := os.WriteFile(filepath.Join(root, "go"), []byte(compiler), 0700); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("sh", "-c", guard+" && printf accepted")
		command.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "GOTOOLCHAIN=local", "SYNTHETIC_GO_VERSION="+version)
		output, runErr := command.CombinedOutput()
		if version == "go1.27.4" {
			if runErr != nil || string(output) != "accepted" {
				t.Fatalf("approved compiler rejected: %v: %s", runErr, output)
			}
		} else if runErr == nil || strings.Contains(string(output), "accepted") {
			t.Fatalf("unapproved compiler %s reached build: %v: %s", version, runErr, output)
		}
	}
}
