// Replays only the immutable scorer, never candidate code, in a fresh contained
// Docker process. Original path-bearing reports remain byte-for-byte unchanged.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// The operator supplies management affinity; input manifests cannot select it.
type calibrationReplaySettings struct {
	CpuSet         string
	MemoryBytes    int64
	Pids           int
	Timeout        time.Duration
	CleanupTimeout time.Duration
	OutputBytes    int
	Observe        func(calibrationReplayObservation)
}

// Records actual replay identities without copying inherited environment data.
// These are offline observations, never live host/provider attestations.
type calibrationReplayObservation struct {
	ImageId                  string `json:"image_id"`
	ContainerId              string `json:"container_id"`
	Owner                    string `json:"owner"`
	InspectionSha256         string `json:"inspection_sha256,omitempty"`
	TerminalSha256           string `json:"terminal_sha256,omitempty"`
	ContainerCleanupVerified bool   `json:"container_cleanup_verified"`
	Failure                  string `json:"failure,omitempty"`
	CorpusDirectory          string `json:"corpus_directory,omitempty"`
	CorpusRetained           bool   `json:"corpus_retained"`
}

// Keeps replay on bounded management resources, separate from scored workloads.
func defaultCalibrationReplaySettings() calibrationReplaySettings {
	return calibrationReplaySettings{
		MemoryBytes: 2 * 1024 * 1024 * 1024, Pids: 128,
		Timeout: 2 * time.Minute, CleanupTimeout: 30 * time.Second, OutputBytes: 1024 * 1024,
	}
}

// Copies only the already-authenticated scorer corpus. The private parent stays
// 0700; its mounted child is readable by the remapped, non-root scorer identity.
func copyCalibrationReplayCorpus(ctx context.Context, source string, destination string, settings calibrationAnalysisSettings) error {
	var total int64
	examined := 0
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		examined++
		if examined > settings.MaximumTraversalEntries || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("calibration replay corpus is linked or exceeds its traversal bound")
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			if err := os.Mkdir(target, 0700); err != nil {
				return err
			}
			// Mkdir modes are filtered by umask. The bind-mounted child must
			// remain traversable by the scorer; its private parent stays 0700.
			return os.Chmod(target, 0755)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > settings.MaximumArtifactBytes || info.Size() > settings.MaximumCohortBytes-total {
			return errors.New("calibration replay corpus type or bytes exceed policy")
		}
		return func() error {
			input, err := os.Open(path)
			if err != nil {
				return err
			}
			defer input.Close()
			output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			n, copyErr := io.Copy(output, io.LimitReader(input, info.Size()+1))
			closeErr := output.Close()
			if copyErr != nil || closeErr != nil || n != info.Size() {
				return errors.New("calibration replay corpus changed during copy")
			}
			total += n
			return os.Chmod(target, 0444)
		}()
	})
}

// Maps host paths to the same /artifacts namespace used by the original scorer.
func calibrationReplayCommand(root string, inputs ScoreInputs) ([]string, error) {
	corpus := filepath.Join(root, "evidence", "scorer-input")
	args := []string{"score"}
	for _, field := range []struct {
		flag   string
		values []string
	}{
		{flag: "--run", values: inputs.Runs}, {flag: "--stderr", values: inputs.Stderr},
		{flag: "--accounting", values: inputs.Accounting}, {flag: "--samples", values: inputs.Samples},
		{flag: "--resource-report", values: inputs.ResourceReports}, {flag: "--marker", values: inputs.Markers},
		{flag: "--baseline", values: []string{inputs.BaselineManifest}},
	} {
		paths := make([]string, 0, len(field.values))
		for _, path := range field.values {
			relative, err := filepath.Rel(corpus, path)
			if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
				!filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(relative, ",\r\n\x00") {
				return nil, errors.New("calibration replay path escapes its original scorer mount")
			}
			paths = append(paths, "/artifacts/"+filepath.ToSlash(relative))
		}
		if len(paths) == 0 {
			return nil, errors.New("calibration replay command is missing inputs")
		}
		args = append(args, field.flag+"="+strings.Join(paths, ","))
	}
	return args, nil
}

// A unique owner and immutable container ID bound cleanup, including uncertain
// create acknowledgments. No name alone authorizes removing a container.
func replayCalibrationScore(ctx context.Context, root string, inputs ScoreInputs, image string,
	analysisSettings calibrationAnalysisSettings, settings calibrationReplaySettings, docker sourceBuildDocker) (content []byte, resultErr error) {
	return replayCalibrationBundle(ctx, root, inputs, image, "", false, analysisSettings, settings, docker)
}

// Both control construction and candidate scoring execute in the exact frozen
// image. Neither a copied baseline payload nor a candidate summary is authority.
func replayCalibrationBundle(ctx context.Context, root string, inputs ScoreInputs, image string, roundId string, baseline bool,
	analysisSettings calibrationAnalysisSettings, settings calibrationReplaySettings, docker sourceBuildDocker) (content []byte, resultErr error) {
	if settings.CpuSet == "" || settings.MemoryBytes <= 0 || settings.Pids <= 0 || settings.Timeout <= 0 ||
		settings.CleanupTimeout <= 0 || settings.OutputBytes <= 0 || !strings.HasPrefix(image, "sha256:") ||
		!validSha256(strings.TrimPrefix(image, "sha256:")) || docker == nil {
		return nil, errors.New("calibration replay identity or limits are invalid")
	}
	command, err := calibrationReplayCommand(root, inputs)
	if err != nil {
		return nil, err
	}
	if baseline {
		if roundId == "" {
			return nil, errors.New("calibration baseline replay requires its bound round")
		}
		command[0] = "score-baseline"
		baselineCommand := command[:1]
		for _, arg := range command[1:] {
			if !strings.HasPrefix(arg, "--baseline=") {
				baselineCommand = append(baselineCommand, arg)
			}
		}
		command = append(baselineCommand, "--round-id="+roundId,
			"--takeover-margin="+strconv.FormatFloat(analysisSettings.MarginFloor, 'g', -1, 64))
	}
	privateRoot, err := os.MkdirTemp("", "sim-latency-calibration-replay-")
	if err != nil {
		return nil, err
	}
	observation := calibrationReplayObservation{ImageId: image, CorpusDirectory: privateRoot, CorpusRetained: true}
	containerMayExist, cleanupVerified := false, false
	defer func() {
		if resultErr != nil {
			observation.Failure = resultErr.Error()
		}
		if settings.Observe != nil {
			settings.Observe(observation)
		}
	}()
	defer func() {
		if containerMayExist && !cleanupVerified {
			return
		}
		if err := os.RemoveAll(privateRoot); err != nil {
			resultErr = errors.Join(resultErr, err)
		} else {
			observation.CorpusRetained = false
		}
	}()
	corpus := filepath.Join(privateRoot, "input")
	if err := copyCalibrationReplayCorpus(ctx, filepath.Join(root, "evidence", "scorer-input"), corpus, analysisSettings); err != nil {
		return nil, err
	}
	if strings.ContainsAny(corpus, ",\r\n\x00") {
		return nil, errors.New("calibration replay temporary mount is invalid")
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	owner := hex.EncodeToString(nonce[:])
	name := "urnetwork-calibration-" + owner
	observation.Owner = owner
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	run := func(commandCtx context.Context, args ...string) ([]byte, error) {
		stdout, stderr := &sourceSupervisorOutput{limit: settings.OutputBytes}, &sourceSupervisorOutput{limit: settings.OutputBytes}
		err := docker(commandCtx, args, stdout, stderr)
		if stdout.exceeded || stderr.exceeded {
			return nil, errors.Join(err, errors.New("calibration replay output exceeded its bound"))
		}
		return stdout.content, err
	}
	id := ""
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settings.CleanupTimeout)
		defer cancel()
		if id == "" {
			recovered, err := run(cleanupCtx, "inspect", "--format", "{{.Id}}", name)
			if err != nil {
				resultErr = errors.Join(resultErr, errors.New("calibration replay cleanup cannot recover creation acknowledgment"))
				return
			}
			id = strings.TrimSpace(string(recovered))
		}
		if !validSha256(id) {
			resultErr = errors.Join(resultErr, errors.New("calibration replay cleanup identity is invalid"))
			return
		}
		labels, err := run(cleanupCtx, "inspect", "--format", "{{json .Config.Labels}}", id)
		var values map[string]string
		if err != nil || json.Unmarshal(labels, &values) != nil || values["com.urnetwork.calibration.owner"] != owner {
			resultErr = errors.Join(resultErr, errors.New("calibration replay cleanup owner is unauthenticated"))
			return
		}
		if _, err := run(cleanupCtx, "rm", "--force", "--volumes", id); err != nil {
			resultErr = errors.Join(resultErr, errors.New("calibration replay cleanup failed"))
			return
		}
		residual, err := run(cleanupCtx, "container", "ls", "--all", "--no-trunc", "--quiet", "--filter", "id="+id)
		if err != nil || len(bytes.TrimSpace(residual)) != 0 {
			resultErr = errors.Join(resultErr, errors.New("calibration replay cleanup left a residual container"))
			return
		}
		observation.ContainerCleanupVerified = true
		cleanupVerified = true
	}()
	args := []string{"create", "--pull=never", "--network=none", "--read-only", "--user=65532:65532",
		"--cap-drop=ALL", "--security-opt=no-new-privileges:true", "--cgroupns=private", "--name=" + name,
		"--cpuset-cpus=" + settings.CpuSet, "--memory=" + strconv.FormatInt(settings.MemoryBytes, 10),
		"--memory-swap=" + strconv.FormatInt(settings.MemoryBytes, 10), "--pids-limit=" + strconv.Itoa(settings.Pids),
		"--label=com.urnetwork.calibration.owner=" + owner,
		"--mount=type=bind,source=" + corpus + ",target=/artifacts,readonly",
		"--entrypoint=/opt/urnetwork/bin/sim-latency", image}
	args = append(args, command...)
	containerMayExist = true
	created, err := run(ctx, args...)
	if err != nil {
		return nil, err
	}
	id = strings.TrimSpace(string(created))
	if !validSha256(id) {
		id = ""
		return nil, errors.New("calibration replay create returned an invalid identity")
	}
	observation.ContainerId = id
	inspection, err := run(ctx, "inspect", id)
	if err != nil {
		return nil, err
	}
	if err := validateCalibrationReplayInspection(inspection, id, owner, image, corpus, command, settings); err != nil {
		return nil, err
	}
	inspectionDigest := sha256.Sum256(inspection)
	observation.InspectionSha256 = hex.EncodeToString(inspectionDigest[:])
	content, err = run(ctx, "start", "--attach", id)
	if err != nil {
		return nil, err
	}
	stateBytes, err := run(ctx, "inspect", "--format", "{{json .State}}", id)
	var state struct {
		Status    string
		Running   bool
		ExitCode  int
		OomKilled bool `json:"OOMKilled"`
	}
	if err != nil || json.Unmarshal(stateBytes, &state) != nil || state.Status != "exited" || state.Running || state.ExitCode != 0 || state.OomKilled {
		return nil, errors.New("calibration replay did not exit cleanly")
	}
	terminalDigest := sha256.Sum256(stateBytes)
	observation.TerminalSha256 = hex.EncodeToString(terminalDigest[:])
	return content, ctx.Err()
}

// Requires the actual created container to match the constrained replay plan.
func validateCalibrationReplayInspection(content []byte, id string, owner string, image string, corpus string,
	command []string, settings calibrationReplaySettings) error {
	var containers []struct {
		Id     string
		Image  string
		Config struct {
			User       string
			Entrypoint []string
			Cmd        []string
			Labels     map[string]string
		}
		HostConfig struct {
			NetworkMode    string
			ReadonlyRootfs bool
			Privileged     bool
			CgroupnsMode   string
			CpusetCpus     string
			Memory         int64
			MemorySwap     int64
			PidsLimit      int
			CapDrop        []string
			SecurityOpt    []string
		}
		Mounts []struct {
			Type        string
			Source      string
			Destination string
			Rw          bool `json:"RW"`
		}
	}
	if json.Unmarshal(content, &containers) != nil || len(containers) != 1 {
		return errors.New("calibration replay inspection is invalid")
	}
	container := containers[0]
	security := false
	for _, option := range container.HostConfig.SecurityOpt {
		security = security || option == "no-new-privileges:true" || option == "no-new-privileges"
	}
	if container.Id != id || container.Image != image || container.Config.Labels["com.urnetwork.calibration.owner"] != owner ||
		container.Config.User != "65532:65532" || !reflect.DeepEqual(container.Config.Entrypoint, []string{"/opt/urnetwork/bin/sim-latency"}) ||
		!reflect.DeepEqual(container.Config.Cmd, command) || container.HostConfig.NetworkMode != "none" || !container.HostConfig.ReadonlyRootfs ||
		container.HostConfig.Privileged || container.HostConfig.CgroupnsMode != "private" || container.HostConfig.CpusetCpus != settings.CpuSet ||
		container.HostConfig.Memory != settings.MemoryBytes || container.HostConfig.MemorySwap != settings.MemoryBytes ||
		container.HostConfig.PidsLimit != settings.Pids || !reflect.DeepEqual(container.HostConfig.CapDrop, []string{"ALL"}) || !security || len(container.Mounts) != 1 {
		return errors.New("calibration replay container differs from its containment plan")
	}
	mount := container.Mounts[0]
	if mount.Type != "bind" || mount.Source != corpus || mount.Destination != "/artifacts" || mount.Rw {
		return errors.New("calibration replay artifact mount is redirected or writable")
	}
	return nil
}
