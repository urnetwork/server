// One approved review owns one durable build attempt. A control-process retry
// must not obtain a fresh image, test draw or deadline merely by restarting.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/server/v2026/controller"
)

type sourceBuildAttemptReservation = controller.CompetitionSourceBuildReservation

type sourceBuildPipelineObservation struct {
	Artifacts    *sourceBuildArtifacts `json:"artifacts"`
	PrivateError string                `json:"private_error"`
}

// Ephemeral checkout paths are transport, not authority. Every reviewed source,
// runtime leaf, prospective manifest and installed setting remains digest-bound.
func sourceBuildAttemptDigest(request reviewedSourceBuildRequest, settings sourceBuildBrokerSettings) (string, error) {
	if err := validateSourceBuildRequest(request); err != nil {
		return "", err
	}
	proposal := *request.Proposal
	proposal.StagedRoots, proposal.PatchPath = nil, ""
	proposal.ReadyAt, proposal.BuildReceipt = time.Time{}, reviewedSourceBuildReceipt{}
	request.Proposal = &proposal
	content, err := json.Marshal(struct {
		Request  reviewedSourceBuildRequest `json:"request"`
		Settings sourceBuildBrokerSettings  `json:"settings"`
	}{Request: request, Settings: settings})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:]), nil
}

// Authenticate the retained immutable image and the original binary artifact
// before letting a replacement qualifier resume an existing test/model plan.
func verifySourceBuildAttemptArtifacts(ctx context.Context, owner string, artifacts *sourceBuildArtifacts, settings sourceBuildSandboxSettings, docker sourceBuildDocker) error {
	if artifacts == nil || artifacts.Directory != filepath.Join(settings.StateDirectory, owner) || !validSha256(artifacts.SimulatorSha256) ||
		!strings.HasPrefix(artifacts.ImageId, "sha256:") || !validSha256(strings.TrimPrefix(artifacts.ImageId, "sha256:")) {
		return errors.New("retained source build artifacts lost their owner or binary identity")
	}
	canonical, err := filepath.EvalSymlinks(artifacts.Directory)
	info, statErr := os.Lstat(artifacts.Directory)
	if err != nil || statErr != nil || canonical != artifacts.Directory || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("retained source build directory is not private and canonical")
	}
	if err := verifySourceTestLogBundle(filepath.Join(artifacts.Directory, "compile", "output", "sim-latency"), artifacts.SimulatorSha256, int64(settings.BinaryBytes)); err != nil {
		return err
	}
	output, diagnostics := &sourceSupervisorOutput{limit: settings.OutputBytes}, &sourceSupervisorOutput{limit: settings.OutputBytes}
	if err := docker(ctx, []string{"image", "inspect", "--format", "{{json .}}", artifacts.ImageId}, output, diagnostics); err != nil || output.exceeded || diagnostics.exceeded {
		return errors.New("retained source image is unavailable or unreadable")
	}
	var image struct {
		Id     string
		Config struct {
			OnBuild []string
			Labels  map[string]string
		}
	}
	if json.Unmarshal(output.content, &image) != nil || image.Id != artifacts.ImageId || len(image.Config.OnBuild) != 0 ||
		image.Config.Labels["com.urnetwork.review-build.owner"] != owner || image.Config.Labels["com.urnetwork.competition.image-kind"] != "evaluator-base" {
		return errors.New("retained source image lost its exact owner or containment identity")
	}
	return nil
}

// The advisory lock refers to an actual open process-owned file, not a stale
// marker. An unfinished durable pipeline reservation still requires explicit
// reconciliation after that lock is released; its disappearance is not a pass.
func executeJournaledSourceBuildBroker(ctx context.Context, request reviewedSourceBuildRequest, settings sourceBuildBrokerSettings, docker sourceBuildDocker,
	pipeline sourceBuildPipelineFunc, qualify sourceBuildQualificationFunc) (*reviewedSourceBuildReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if settings.Timeout <= 0 || settings.MaximumRequestBytes <= 0 || pipeline == nil || qualify == nil || docker == nil {
		return nil, errors.New("durable source build settings are incomplete")
	}
	digest, err := sourceBuildAttemptDigest(request, settings)
	if err != nil {
		return nil, err
	}
	state := settings.Sandbox.StateDirectory
	canonical, err := filepath.EvalSymlinks(state)
	info, statErr := os.Lstat(state)
	if err != nil || statErr != nil || !filepath.IsAbs(state) || canonical != state || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("durable source build state must be private and canonical")
	}
	key := sha256.Sum256([]byte(request.Proposal.ReviewId.String()))
	directory := filepath.Join(state, ".attempts", hex.EncodeToString(key[:]))
	for _, path := range []string{filepath.Dir(directory), directory} {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		canonical, err := filepath.EvalSymlinks(path)
		info, statErr := os.Lstat(path)
		if err != nil || statErr != nil || canonical != path || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("source build attempt journal was redirected or exposed")
		}
		parent, err := os.Open(filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		syncErr, closeErr := parent.Sync(), parent.Close()
		if err := errors.Join(syncErr, closeErr); err != nil {
			return nil, err
		}
	}
	lockPath := filepath.Join(directory, "process.lock")
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), lockPath)
	defer lock.Close()
	info, err = lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("source build process lock is unsafe")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.New("source build attempt already has an active controller")
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	reservationPath := filepath.Join(directory, "reservation.json")
	var reservation sourceBuildAttemptReservation
	fresh := false
	if err := readSourceTestRecord(reservationPath, settings.MaximumRequestBytes, &reservation); errors.Is(err, os.ErrNotExist) {
		for _, name := range []string{"pipeline.json", "qualified-build-receipt.json"} {
			if _, err := os.Lstat(filepath.Join(directory, name)); !errors.Is(err, os.ErrNotExist) {
				return nil, errors.New("source build evidence has no original reservation")
			}
		}
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		deadline := now.Add(settings.Timeout)
		if earlier, ok := ctx.Deadline(); ok && earlier.Before(deadline) {
			deadline = earlier
		}
		reservation = sourceBuildAttemptReservation{Schema: 1, RequestSha256: digest, Owner: hex.EncodeToString(nonce[:]), StartedAt: now, Deadline: deadline}
		content, err := json.Marshal(reservation)
		if err != nil {
			return nil, err
		}
		if err := publishSourceTestRecord(reservationPath, content); err != nil {
			return nil, err
		}
		fresh = true
	} else if err != nil {
		return nil, err
	}
	if reservation.Schema != 1 || reservation.RequestSha256 != digest || !validSha256(reservation.Owner) || reservation.StartedAt.IsZero() || !reservation.StartedAt.Before(reservation.Deadline) {
		return nil, errors.New("source build retry changed its original reviewed request or settings")
	}
	// No stage is allowed a new budget after process replacement.
	ctx, cancel := context.WithDeadline(ctx, reservation.Deadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	retainedPipeline := func(ctx context.Context, request reviewedSourceBuildRequest, owner string, sandbox sourceBuildSandboxSettings, docker sourceBuildDocker) (*sourceBuildArtifacts, error) {
		path := filepath.Join(directory, "pipeline.json")
		var observation sourceBuildPipelineObservation
		if !fresh {
			if err := readSourceTestRecord(path, settings.MaximumRequestBytes, &observation); errors.Is(err, os.ErrNotExist) {
				return nil, errors.New("source pipeline is interrupted; reconcile its actual owner before recovery")
			} else if err != nil {
				return nil, err
			}
		} else {
			artifacts, pipelineErr := pipeline(ctx, request, owner, sandbox, docker)
			observation.Artifacts = artifacts
			if pipelineErr != nil {
				observation.PrivateError = pipelineErr.Error()
			}
			content, err := json.Marshal(observation)
			if err != nil || int64(len(content)) > settings.MaximumRequestBytes {
				return nil, errors.New("source pipeline observation exceeds its evidence budget")
			}
			if err := publishSourceTestRecord(path, content); err != nil {
				return nil, errors.Join(pipelineErr, err)
			}
		}
		if observation.PrivateError != "" {
			return nil, errors.New("source pipeline failed; inspect retained diagnostics before recovery")
		}
		if err := verifySourceBuildAttemptArtifacts(ctx, owner, observation.Artifacts, sandbox, docker); err != nil {
			return nil, err
		}
		return observation.Artifacts, nil
	}
	retainedQualify := func(ctx context.Context, qualification sourceBuildQualificationRequest) (reviewedSourceBuildReceipt, error) {
		path := filepath.Join(directory, "qualified-build-receipt.json")
		var receipt reviewedSourceBuildReceipt
		if err := readSourceTestRecord(path, settings.MaximumRequestBytes, &receipt); err == nil {
			return receipt, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return receipt, err
		}
		receipt, err := qualify(ctx, qualification)
		if err != nil {
			return receipt, err
		}
		if err := validateReviewedSourceBuildForMode(receipt, request.Proposal.ReviewId, request.Proposal.PatchSha256, request.Proposal.SourceLockSha256, request.Proposal.Staging); err != nil {
			return receipt, err
		}
		if receipt.EvaluatorImageDigest != qualification.Artifacts.ImageId || receipt.SandboxId != reservation.Owner {
			return receipt, errors.New("source qualification changed its retained image or owner")
		}
		if err := ctx.Err(); err != nil {
			return receipt, err
		}
		content, err := json.Marshal(receipt)
		if err != nil || int64(len(content)) > settings.MaximumRequestBytes {
			return receipt, errors.New("source qualification receipt exceeds its evidence budget")
		}
		return receipt, publishSourceTestRecord(path, content)
	}
	return executeSourceBuildBroker(ctx, request, reservation.Owner, settings, docker, retainedPipeline, retainedQualify)
}
