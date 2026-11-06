package server

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// D-OPS-6's pending migration groups, in whichever build runs this. A test
// binary may link a pending group, so what a binary that is not a test does is
// proved with a built probe.

// The migrations MG0 adds to a build that links it.
const sidecarMg0MigrationCount = 5

func TestSidecarPendingRunsOnlyInLocalCanaryOrTests(t *testing.T) {
	for _, c := range []struct {
		env        string
		testBinary bool
		allowed    bool
	}{
		{env: "local", allowed: true},
		{env: "canary", allowed: true},
		{env: "main", testBinary: true, allowed: true},
		{env: "", testBinary: true, allowed: true},
		{env: "main"},
		{env: "community"},
		{env: "beta"},
		{env: "LOCAL"},
		{env: " local"},
		{env: ""},
	} {
		if got := sidecarPendingAllowed(c.env, c.testBinary); got != c.allowed {
			t.Errorf("env %q in a test binary %t: allowed = %t, want %t", c.env, c.testBinary, got, c.allowed)
		}
	}
}

func sidecarBuildProbe(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	binary := filepath.Join(dir, name)
	buildArgs := append([]string{"build"}, args...)
	buildArgs = append(buildArgs, "-o", binary, "./testdata/sidecarpendingprobe")
	output, err := exec.Command("go", buildArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %s\n%s", strings.Join(buildArgs, " "), err, output)
	}
	return binary
}

// Runs the probe with WARP_ENV set, or unset when env is "".
func sidecarRunProbe(t *testing.T, binary string, env string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binary)
	command.Env = slices.DeleteFunc(os.Environ(), func(variable string) bool {
		return strings.HasPrefix(variable, "WARP_ENV=")
	})
	if env != "" {
		command.Env = append(command.Env, "WARP_ENV="+env)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

// Both build tags compile, a release build links main's migrations whatever its
// env, a build with the tag links MG0 at the slice's end, and a build with the
// tag refuses to start anywhere but local or canary (FS-54's first two cases).
func TestSidecarPendingBuildTags(t *testing.T) {
	dir := t.TempDir()
	// an empty -tags, so a GOFLAGS that sets the tag cannot pass for a release build
	release := sidecarBuildProbe(t, dir, "release", "-tags", "")
	pending := sidecarBuildProbe(t, dir, "pending", "-tags", "sidecar_pending")

	// every package that reads the group, and the migration runner's own, in both
	for _, tags := range []string{"", "sidecar_pending"} {
		args := []string{"build", "-tags", tags, ".", "./session", "./model", "./router", "./controller", "./api", "./connect", "./bringyourctl"}
		output, err := exec.Command("go", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("go %s: %s\n%s", strings.Join(args, " "), err, output)
		}
	}

	releaseCount := 0
	for _, env := range []string{"main", "community", "canary", "local", ""} {
		output, stderr, err := sidecarRunProbe(t, release, env)
		if err != nil {
			t.Fatalf("release build, WARP_ENV %q: %s\n%s", env, err, stderr)
		}
		count, linked := 0, true
		if _, err := fmt.Sscan(output, &count, &linked); err != nil {
			t.Fatalf("release build, WARP_ENV %q printed %q: %s", env, output, err)
		}
		if linked {
			t.Fatalf("release build, WARP_ENV %q links MG0", env)
		}
		if releaseCount != 0 && count != releaseCount {
			t.Fatalf("release build, WARP_ENV %q links %d migrations, not %d", env, count, releaseCount)
		}
		releaseCount = count
	}

	for _, env := range []string{"local", "canary"} {
		output, stderr, err := sidecarRunProbe(t, pending, env)
		if err != nil {
			t.Fatalf("pending build, WARP_ENV %q: %s\n%s", env, err, stderr)
		}
		if want := fmt.Sprintf("%d true", releaseCount+sidecarMg0MigrationCount); output != want {
			t.Fatalf("pending build, WARP_ENV %q printed %q, want %q", env, output, want)
		}
	}

	for _, env := range []string{"main", "community", "beta", ""} {
		output, stderr, err := sidecarRunProbe(t, pending, env)
		if err == nil {
			t.Fatalf("pending build ran in WARP_ENV %q and printed %q", env, output)
		}
		if !strings.Contains(stderr, "sidecar_pending") || output != "" {
			t.Fatalf("pending build in WARP_ENV %q failed without the refusal: %q\n%s", env, output, stderr)
		}
	}
}
