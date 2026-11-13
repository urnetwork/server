// This file pins the release-to-Main LAN execution contract without accessing
// production configuration or services.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunMainLanUsesVersionedBinaryAndHostSettings verifies that the runner
// preserves release identity, selects the requested host stanza, and prevents
// inherited database routes from bypassing that stanza.
func TestRunMainLanUsesVersionedBinaryAndHostSettings(t *testing.T) {
	tempDir := t.TempDir()
	outputPath := filepath.Join(tempDir, "environment.txt")
	binaryPath := filepath.Join(tempDir, "synthetic-bringyourctl")
	binarySource := `#!/usr/bin/env bash
set -euo pipefail
{
    printf 'env=%s\n' "$WARP_ENV"
    printf 'service=%s\n' "$WARP_SERVICE"
    printf 'version=%s\n' "$WARP_VERSION"
    printf 'domain=%s\n' "$WARP_DOMAIN"
    printf 'host=%s\n' "$WARP_HOST"
    printf 'postgres=%s\n' "${BRINGYOUR_POSTGRES_HOSTNAME:-}"
    printf 'redis=%s\n' "${BRINGYOUR_REDIS_HOSTNAME:-}"
    printf 'args=%s\n' "$*"
} > "$SYNTHETIC_OUTPUT"
`
	if err := os.WriteFile(binaryPath, []byte(binarySource), 0o700); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("bash", "run-main-lan.sh", "db", "migrate")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"BRINGYOURCTL_BINARY=" + binaryPath,
		"BRINGYOUR_POSTGRES_HOSTNAME=postgres.synthetic.example",
		"BRINGYOUR_REDIS_HOSTNAME=redis.synthetic.example",
		"SYNTHETIC_OUTPUT=" + outputPath,
		"WARP_MAIN_DOMAIN=service.synthetic.example",
		"WARP_MAIN_LAN_HOST=synthetic-us-fmt",
		"WARP_VERSION=2099.1.2+345",
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run synthetic Main LAN command: %v\n%s", err, output)
	}

	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := []string{
		"env=main",
		"service=bringyourctl",
		"version=2099.1.2+345",
		"domain=service.synthetic.example",
		"host=synthetic-us-fmt",
		"postgres=",
		"redis=",
		"args=db migrate",
	}
	for _, wantLine := range wantLines {
		if !strings.Contains(string(output), wantLine+"\n") {
			t.Errorf("synthetic runner output missing %q:\n%s", wantLine, output)
		}
	}
}

// TestRunMainLanRequiresReleaseVersion prevents an unversioned administrative
// command from entering the release pre-deploy path.
func TestRunMainLanRequiresReleaseVersion(t *testing.T) {
	command := exec.Command("bash", "run-main-lan.sh", "db", "migrate")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"WARP_VERSION=",
	}
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("unversioned Main LAN command succeeded")
	}
	if !strings.Contains(string(output), "WARP_VERSION must identify the release being deployed") {
		t.Fatalf("unversioned failure did not explain the release invariant: %s", output)
	}
}

// TestRunMainLanRejectsMissingBinary prevents a missing release artifact from
// falling back to source execution from a mutable checkout.
func TestRunMainLanRejectsMissingBinary(t *testing.T) {
	missingBinary := filepath.Join(t.TempDir(), "missing-bringyourctl")
	command := exec.Command("bash", "run-main-lan.sh", "grafana", "load-defaults")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"BRINGYOURCTL_BINARY=" + missingBinary,
		"WARP_MAIN_DOMAIN=service.synthetic.example",
		"WARP_MAIN_LAN_HOST=synthetic-us-fmt",
		"WARP_VERSION=2099.1.2+345",
	}
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("Main LAN command succeeded without its versioned binary")
	}
	if !strings.Contains(string(output), "Versioned bringyourctl binary is not executable") {
		t.Fatalf("missing binary failure did not explain the artifact invariant: %s", output)
	}
}
