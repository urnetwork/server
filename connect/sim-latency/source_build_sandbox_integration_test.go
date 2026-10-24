// Opt-in real-container verification uses operator-provided, authenticated
// trusted-base inputs. Default package tests remain deterministic and offline.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// No real checkpoint, host path or production identity is embedded in fixtures.
// Preparation must make source/metadata readable but keep the outer root private.
func TestSourceBuildSandboxRealTrustedBasePhases(t *testing.T) {
	if os.Getenv("SIM_LATENCY_SOURCE_BUILD_REAL") != "1" {
		t.Skip("requires explicit trusted-base container qualification inputs")
	}
	settings := defaultSourceBuildSandboxSettings()
	settings.StateDirectory = os.Getenv("SIM_LATENCY_SOURCE_BUILD_REAL_ROOT")
	owner := os.Getenv("SIM_LATENCY_SOURCE_BUILD_REAL_OWNER")
	image := os.Getenv("SIM_LATENCY_SOURCE_BUILD_REAL_IMAGE")
	commit := os.Getenv("SIM_LATENCY_SOURCE_BUILD_REAL_COMMIT")
	for _, phase := range []string{"check", "compile"} {
		input := sourceBuildSandboxInput{Owner: owner, Phase: phase, ImageId: image, Commit: commit,
			SourceDirectory:   filepath.Join(settings.StateDirectory, owner, "source"),
			MetadataDirectory: filepath.Join(settings.StateDirectory, owner, phase, "metadata"),
			OutputDirectory:   filepath.Join(settings.StateDirectory, owner, phase, "output")}
		if _, err := sourceBuildSandboxArguments(input, settings); err != nil {
			t.Fatal(err)
		}
		docker := func(ctx context.Context, args []string, out, diagnostics io.Writer) error {
			command := exec.CommandContext(ctx, "/usr/bin/sudo", append([]string{"-n", "/usr/bin/docker", "--host=unix:///var/run/docker.sock"}, args...)...)
			command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
			command.Stdout, command.Stderr = out, diagnostics
			return command.Run()
		}
		qualifiedDocker := func(ctx context.Context, args []string, out, diagnostics io.Writer) error {
			if args[0] == "start" {
				var raw bytes.Buffer
				if err := docker(ctx, []string{"inspect", "--format", "{{json .}}", args[len(args)-1]}, &raw, diagnostics); err != nil {
					return err
				}
				var actual struct {
					Config     struct{ User string }
					HostConfig struct {
						Privileged     bool
						ReadonlyRootfs bool
						NetworkMode    string
						Memory         int64
						MemorySwap     int64
						PidsLimit      int
						CpusetCpus     string
						CapAdd         []string
						CapDrop        []string
						SecurityOpt    []string
						Tmpfs          map[string]string
					}
					Mounts []struct {
						Type        string
						RW          bool
						Destination string
					}
				}
				if err := json.Unmarshal(raw.Bytes(), &actual); err != nil {
					return err
				}
				if actual.Config.User != "65532:65532" || actual.HostConfig.Privileged || !actual.HostConfig.ReadonlyRootfs || actual.HostConfig.NetworkMode != "none" ||
					actual.HostConfig.Memory != settings.MemoryBytes || actual.HostConfig.MemorySwap != settings.MemoryBytes || actual.HostConfig.PidsLimit != settings.Pids ||
					actual.HostConfig.CpusetCpus != settings.CpuSet || len(actual.HostConfig.CapAdd) != 0 || !slices.Contains(actual.HostConfig.CapDrop, "ALL") ||
					!slices.Contains(actual.HostConfig.SecurityOpt, "no-new-privileges:true") {
					return fmt.Errorf("actual Docker boundary differs from source-build policy")
				}
				if !slices.Contains(strings.Split(actual.HostConfig.Tmpfs["/work"], ","), "exec") {
					return fmt.Errorf("private Go test scratch mount must permit execution")
				}
				for _, mount := range actual.Mounts {
					if mount.RW && mount.Type != "tmpfs" {
						return fmt.Errorf("source build gained writable host mount at %s", mount.Destination)
					}
				}
				if err := writePrivateReviewFile(filepath.Join(settings.StateDirectory, owner, phase, "container-inspect.json"), raw.Bytes()); err != nil {
					return err
				}
			}
			return docker(ctx, args, out, diagnostics)
		}
		started := time.Now()
		stdout, stderr, err := executeSourceBuildSandbox(context.Background(), input, settings, qualifiedDocker)
		if writeErr := writePrivateReviewFile(filepath.Join(settings.StateDirectory, owner, phase, "stderr.log"), stderr); writeErr != nil {
			t.Fatal(writeErr)
		}
		if err != nil {
			if writeErr := writePrivateReviewFile(filepath.Join(settings.StateDirectory, owner, phase, "failed-stdout.log"), stdout); writeErr != nil {
				t.Fatal(writeErr)
			}
			t.Fatalf("real %s phase failed: %v; private diagnostics retained", phase, err)
		}
		if phase == "compile" {
			info, err := buildinfo.Read(bytes.NewReader(stdout))
			if err != nil {
				t.Fatal("compiler did not return a Go binary", err)
			}
			actualCommit, modified := "", ""
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					actualCommit = setting.Value
				}
				if setting.Key == "vcs.modified" {
					modified = setting.Value
				}
			}
			if actualCommit != commit || modified != "false" {
				t.Fatal("compiled artifact does not bind exact clean source")
			}
			if err := writePrivateReviewFile(filepath.Join(settings.StateDirectory, owner, phase, "output", "sim-latency"), stdout); err != nil {
				t.Fatal(err)
			}
		} else if err := writePrivateReviewFile(filepath.Join(settings.StateDirectory, owner, phase, "stdout.log"), stdout); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(stdout)
		t.Logf("phase=%s duration=%s stdout_bytes=%d stdout_sha256=%s", phase, time.Since(started), len(stdout), hex.EncodeToString(digest[:]))
	}
}
