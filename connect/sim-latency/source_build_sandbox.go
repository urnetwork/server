// Candidate check initialization and compilation use separate limited Docker
// containers. No candidate process runs in the control host or a build daemon.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Deployment owns these ceilings; candidate input cannot change them. Every
// phase gets a fresh cache and metadata directory, never another phase's writes.
type sourceBuildSandboxSettings struct {
	StateDirectory string        `json:"state_directory"`
	CpuSet         string        `json:"cpu_set"`
	CgroupParent   string        `json:"cgroup_parent"`
	MemoryBytes    int64         `json:"memory_bytes"`
	WorkBytes      int64         `json:"work_bytes"`
	TmpBytes       int64         `json:"tmp_bytes"`
	MetadataBytes  int64         `json:"metadata_bytes"`
	MetadataFiles  int           `json:"metadata_files"`
	BinaryBytes    int           `json:"binary_bytes"`
	Pids           int           `json:"pids"`
	NoFile         int           `json:"no_file"`
	Timeout        time.Duration `json:"timeout"`
	CleanupTimeout time.Duration `json:"cleanup_timeout"`
	OutputBytes    int           `json:"output_bytes"`
}

// Match the authoritative host's evaluation allocation, reserving management
// cores and host memory for cancellation/cleanup even under candidate overload.
func defaultSourceBuildSandboxSettings() sourceBuildSandboxSettings {
	return sourceBuildSandboxSettings{StateDirectory: "/var/lib/urnetwork/competition-reviewed-builds", CpuSet: "0,2,4,6,8,10,12,14,16,18", CgroupParent: "urnetwork-evaluation.slice",
		MemoryBytes: 72 * 1024 * 1024 * 1024, WorkBytes: 8 * 1024 * 1024 * 1024, TmpBytes: 2 * 1024 * 1024 * 1024,
		MetadataBytes: 16 * 1024 * 1024, MetadataFiles: 4096, BinaryBytes: 512 * 1024 * 1024,
		Pids: 65536, NoFile: 1048576, Timeout: 20 * time.Minute, CleanupTimeout: 30 * time.Second, OutputBytes: 4 * 1024 * 1024}
}

// These are authenticated staging paths, never candidate-provided commands.
type sourceBuildSandboxInput struct {
	Owner             string
	Phase             string
	ImageId           string
	Commit            string
	SourceDirectory   string
	MetadataDirectory string
	OutputDirectory   string
}

// Builds fixed argv rather than interpolating paths, commits or code in shell
// syntax. The only shell program is this trusted immutable Go-tool protocol.
func sourceBuildSandboxArguments(input sourceBuildSandboxInput, settings sourceBuildSandboxSettings) ([]string, error) {
	if input.Phase != "check" && input.Phase != "compile" && input.Phase != "metadata" && input.Phase != "correctness" || !validSha256(input.Owner) || !sourceGitShaPattern.MatchString(input.Commit) ||
		!strings.HasPrefix(input.ImageId, "sha256:") || !validSha256(strings.TrimPrefix(input.ImageId, "sha256:")) ||
		settings.CpuSet == "" || settings.CgroupParent == "" || settings.MemoryBytes <= 0 || settings.WorkBytes <= 0 || settings.TmpBytes <= 0 ||
		settings.MetadataBytes <= 0 || settings.MetadataFiles <= 0 || settings.BinaryBytes <= 0 || settings.Pids <= 0 || settings.NoFile <= 0 || settings.Timeout <= 0 || settings.CleanupTimeout <= 0 || settings.OutputBytes <= 0 {
		return nil, errors.New("source build sandbox identity or limits are invalid")
	}
	if !filepath.IsAbs(settings.StateDirectory) || filepath.Clean(settings.StateDirectory) != settings.StateDirectory || settings.StateDirectory == "/" {
		return nil, errors.New("source build sandbox state directory is invalid")
	}
	ownerDirectory := filepath.Join(settings.StateDirectory, input.Owner)
	if input.SourceDirectory != filepath.Join(ownerDirectory, "source") ||
		input.MetadataDirectory != filepath.Join(ownerDirectory, input.Phase, "metadata") ||
		input.OutputDirectory != filepath.Join(ownerDirectory, input.Phase, "output") {
		return nil, errors.New("source build mounts must belong to this attempt and independent phase")
	}
	paths := []string{input.SourceDirectory, input.MetadataDirectory, input.OutputDirectory}
	for i, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, ",\r\n\x00") {
			return nil, errors.New("source build sandbox needs canonical private mount paths")
		}
		for _, previous := range paths[:i] {
			if path == previous || strings.HasPrefix(path, previous+"/") || strings.HasPrefix(previous, path+"/") {
				return nil, errors.New("source build sandbox mount paths overlap")
			}
		}
	}
	args := []string{"create", "--init", "--pull=never", "--network=none", "--read-only", "--user=65532:65532", "--cap-drop=ALL", "--security-opt=no-new-privileges:true", "--cgroupns=private",
		"--name=urnetwork-reviewed-" + input.Owner + "-" + input.Phase,
		"--cgroup-parent=" + settings.CgroupParent, "--cpuset-cpus=" + settings.CpuSet, "--memory=" + strconv.FormatInt(settings.MemoryBytes, 10),
		"--memory-swap=" + strconv.FormatInt(settings.MemoryBytes, 10), "--pids-limit=" + strconv.Itoa(settings.Pids), "--ulimit=nofile=" + strconv.Itoa(settings.NoFile) + ":" + strconv.Itoa(settings.NoFile),
		"--ulimit=core=0:0",
		"--ulimit=fsize=" + strconv.Itoa(settings.BinaryBytes) + ":" + strconv.Itoa(settings.BinaryBytes),
		"--label=com.urnetwork.review-build.owner=" + input.Owner, "--label=com.urnetwork.review-build.phase=" + input.Phase,
		"--mount=type=bind,source=" + input.SourceDirectory + ",target=/workspace,readonly",
		"--mount=type=bind,source=" + input.MetadataDirectory + ",target=/input/server-metadata,readonly",
		"--tmpfs=/work:rw,exec,nosuid,nodev,uid=65532,gid=65532,mode=0700,size=" + strconv.FormatInt(settings.WorkBytes, 10),
		"--tmpfs=/tmp:rw,nosuid,nodev,noexec,uid=65532,gid=65532,mode=1770,size=" + strconv.FormatInt(settings.TmpBytes, 10),
		"--env=SOURCE_BUILD_COMMIT=" + input.Commit,
		"--env=GOCACHE=/work/cache", "--env=GOTMPDIR=/work/tmp", "--env=TMPDIR=/tmp", "--env=GOFLAGS=-mod=readonly", "--env=GOPROXY=off", "--env=GOSUMDB=off", "--entrypoint=/bin/bash"}
	if input.Phase != "metadata" {
		args = append(args, "--tmpfs=/opt/urnetwork/go/pkg/mod/cache/download/github.com/urnetwork/server/@v:rw,nosuid,nodev,noexec,uid=65532,gid=65532,mode=0700,size="+strconv.FormatInt(settings.MetadataBytes, 10))
	}
	script := "mkdir -p /work/cache /work/tmp; cp -R /opt/urnetwork/go/build-cache/. /work/cache/; cp -R /input/server-metadata/. /opt/urnetwork/go/pkg/mod/cache/download/github.com/urnetwork/server/@v/; test \"$(git -C /workspace/server rev-parse HEAD)\" = \"$SOURCE_BUILD_COMMIT\"; test -z \"$(git -C /workspace/server status --porcelain --untracked-files=all)\"; cd /workspace/server; "
	if input.Phase == "metadata" {
		// Export only the immutable incumbent's module metadata. In particular,
		// do not shadow this path with the later phases' empty writable tmpfs.
		script = "tar -C /opt/urnetwork/go/pkg/mod/cache/download/github.com/urnetwork/server/@v -cf - ."
	} else if input.Phase == "check" {
		script += "go vet ./connect/...; go test -run '^$' ./connect/..."
	} else if input.Phase == "correctness" {
		script += sourceCorrectnessSuiteScript()
	} else {
		args = append(args, "--tmpfs=/output:rw,nosuid,nodev,noexec,uid=65532,gid=65532,mode=0700,size="+strconv.Itoa(settings.BinaryBytes))
		script += "go build -trimpath -o /output/sim-latency ./connect/sim-latency; test \"$(go version -m /output/sim-latency | sed -n 's/.*vcs.revision=//p')\" = \"$SOURCE_BUILD_COMMIT\"; test \"$(go version -m /output/sim-latency | sed -n 's/.*vcs.modified=//p')\" = false; cat /output/sim-latency"
	}
	return append(args, input.ImageId, "-ceu", script), nil
}

// Docker command execution is injectable for deterministic fence/cleanup tests.
// The production implementation never inherits proxy, main or Docker overrides.
type sourceBuildDocker func(context.Context, []string, io.Writer, io.Writer) error

// Only the host Docker client is privileged; no daemon socket enters a sandbox.
func runSourceBuildDocker(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, "/usr/bin/docker", append([]string{"--host=unix:///var/run/docker.sock"}, args...)...)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	command.Stdout, command.Stderr = stdout, stderr
	return command.Run()
}

// Cleanup is based on an immutable container ID and independently rechecked
// owner label, including after deadline cancellation. Ambiguity fails closed.
func executeSourceBuildSandbox(ctx context.Context, input sourceBuildSandboxInput, settings sourceBuildSandboxSettings, docker sourceBuildDocker) (stdout, stderr []byte, resultErr error) {
	args, err := sourceBuildSandboxArguments(input, settings)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	output, diagnostics := &sourceSupervisorOutput{limit: settings.OutputBytes}, &sourceSupervisorOutput{limit: settings.OutputBytes}
	id := ""
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), settings.CleanupTimeout)
		defer cleanupCancel()
		if id == "" {
			// The daemon may create the container before its client loses the
			// acknowledgment. Recover the exact unique name, then verify labels.
			var recovered bytes.Buffer
			name := "urnetwork-reviewed-" + input.Owner + "-" + input.Phase
			if err := docker(cleanupCtx, []string{"inspect", "--format", "{{.Id}}", name}, &recovered, io.Discard); err != nil {
				resultErr = errors.Join(resultErr, errors.New("source build cleanup cannot recover creation acknowledgment"))
				return
			}
			id = strings.TrimSpace(recovered.String())
			if !validSha256(id) {
				resultErr = errors.Join(resultErr, errors.New("source build cleanup recovered an invalid identity"))
				return
			}
		}
		var labels bytes.Buffer
		if err := docker(cleanupCtx, []string{"inspect", "--format", "{{json .Config.Labels}}", id}, &labels, io.Discard); err != nil {
			resultErr = errors.Join(resultErr, errors.New("source build cleanup cannot authenticate owner"))
			return
		}
		var ownerKVs map[string]string
		if json.Unmarshal(labels.Bytes(), &ownerKVs) != nil || ownerKVs["com.urnetwork.review-build.owner"] != input.Owner || ownerKVs["com.urnetwork.review-build.phase"] != input.Phase {
			resultErr = errors.Join(resultErr, errors.New("source build cleanup owner changed"))
			return
		}
		if err := docker(cleanupCtx, []string{"rm", "--force", "--volumes", id}, io.Discard, io.Discard); err != nil {
			resultErr = errors.Join(resultErr, errors.New("source build cleanup failed"))
			return
		}
		var residual bytes.Buffer
		if err := docker(cleanupCtx, []string{"container", "ls", "--all", "--no-trunc", "--quiet", "--filter", "id=" + id}, &residual, io.Discard); err != nil || strings.TrimSpace(residual.String()) != "" {
			resultErr = errors.Join(resultErr, errors.New("source build cleanup left residual containers"))
		}
	}()
	if err := docker(ctx, args, output, diagnostics); err != nil {
		return nil, nil, fmt.Errorf("source build container creation failed: %w", err)
	}
	id = strings.TrimSpace(string(output.content))
	if !validSha256(id) || output.exceeded {
		id = ""
		return nil, nil, errors.New("source build container identity is invalid")
	}
	outputLimit := settings.OutputBytes
	if input.Phase == "compile" {
		outputLimit = settings.BinaryBytes
	} else if input.Phase == "metadata" {
		outputLimit = int(settings.MetadataBytes)
	}
	output, diagnostics = &sourceSupervisorOutput{limit: outputLimit}, &sourceSupervisorOutput{limit: settings.OutputBytes}
	err = docker(ctx, []string{"start", "--attach", id}, output, diagnostics)
	if err != nil {
		resultErr = errors.New("contained source build did not complete successfully")
	}
	if err == nil {
		var stateBytes bytes.Buffer
		var state struct {
			Status    string
			Running   bool
			ExitCode  int
			OomKilled bool `json:"OOMKilled"`
		}
		stateErr := docker(ctx, []string{"inspect", "--format", "{{json .State}}", id}, &stateBytes, io.Discard)
		if stateErr != nil || json.Unmarshal(stateBytes.Bytes(), &state) != nil || state.Status != "exited" || state.Running || state.ExitCode != 0 || state.OomKilled {
			resultErr = errors.New("contained source build did not have a clean terminal exit")
		}
	}
	if output.exceeded || diagnostics.exceeded {
		resultErr = errors.Join(resultErr, errors.New("contained source build output exceeded its bound"))
	}
	if ctx.Err() != nil {
		resultErr = errors.Join(resultErr, ctx.Err())
	}
	if resultErr == nil && input.Phase == "compile" {
		_, resultErr = verifySourceBuildBinary(output.content, input.Commit)
	}
	if resultErr != nil {
		resultErr = fmt.Errorf("reviewed source %s: %w", input.Phase, resultErr)
	}
	return output.content, diagnostics.content, resultErr
}
