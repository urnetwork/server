//go:build linux

// Native counterfactual executes only the pinned image's coreutils binary in
// a new bounded compiler-shaped tmpfs, with no input or production mounts.
package competitionagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Default Docker tmpfs must reproduce denied execution; explicit exec must
// succeed under the same nonroot/core/seccomp/network/resource restrictions.
func TestCompilerTmpfsNative(t *testing.T) {
	path := os.Getenv("COMPETITION_COMPILER_TMPFS_NATIVE_CONFIG")
	if path == "" || os.Geteuid() != 0 {
		t.Skip("requires explicit root private compiler fixture configuration")
	}
	config, err := LoadDockerProbeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(config.DockerRole.ResourceBoundary.Path)
	if err != nil {
		t.Fatal(err)
	}
	var boundary RuntimeResourceBoundary
	if err := json.Unmarshal(content, &boundary); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	for _, explicit := range []bool{false, true} {
		var nonce [12]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			t.Fatal(err)
		}
		name := "synthetic-compiler-tmpfs-" + hex.EncodeToString(nonce[:])
		options := "/work:rw,nosuid,nodev,uid=65532,gid=65532,mode=0700,size=16m"
		if explicit {
			options += ",exec"
		}
		args := []string{"--host", "unix://" + config.DockerSocketPath, "run", "--rm", "--pull", "never", "--name", name,
			"--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges=true",
			"--security-opt", "seccomp=" + config.DockerRole.CandidateCoreProfile.Path, "--ulimit", "core=1:1",
			"--user", "65532:65532", "--workdir", "/", "--cpuset-cpus", boundary.ManagementCpuSet,
			"--cgroup-parent", managementPrivateSliceName, "--memory", "128m", "--memory-swap", "128m", "--pids-limit", "32", "--shm-size", "8m",
			"--tmpfs", options, "--entrypoint", "/bin/bash", config.EvaluatorImageDigest,
			"-euc", "cp /usr/bin/true /work/probe; exec /work/probe"}
		output := &boundedOutput{maximum: 4096}
		command := exec.CommandContext(ctx, config.Docker.Path, args...)
		command.Stdout, command.Stderr = output, output
		err := command.Run()
		var failure *exec.ExitError
		status := 0
		if err != nil {
			if !errors.As(err, &failure) {
				t.Fatal(err)
			}
			status = failure.ExitCode()
		}
		check := exec.CommandContext(ctx, config.Docker.Path, "--host", "unix://"+config.DockerSocketPath, "ps", "-aq", "--filter", "name=^/"+name+"$")
		remaining, checkErr := check.Output()
		if checkErr != nil || strings.TrimSpace(string(remaining)) != "" {
			t.Fatalf("exact native fixture cleanup unproved: %v", checkErr)
		}
		if output.exceeded || explicit && status != 0 || !explicit && status != 126 {
			t.Fatalf("Docker execution counterfactual explicit=%t status=%d: %s", explicit, status, output.Bytes())
		}
	}
	t.Log("default noexec refusal, explicit compiler execution and exact cleanup verified")
}
