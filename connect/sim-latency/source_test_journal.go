// Qualification test execution is reserved durably before candidate code runs.
// A completed observation replays; an interrupted reservation never reruns
// automatically. The supervisor must reconcile its actual process owner.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urnetwork/server/model"
)

type sourceTestJournalSettings struct {
	MaximumRecordBytes   int64
	MaximumEvidenceBytes int64
	Now                  func() time.Time
}

func defaultSourceTestJournalSettings() sourceTestJournalSettings {
	return sourceTestJournalSettings{MaximumRecordBytes: 1024 * 1024, MaximumEvidenceBytes: 16 * 1024 * 1024, Now: time.Now}
}

type sourceTestReservation struct {
	Schema     int       `json:"schema"`
	PlanSha256 string    `json:"plan_sha256"`
	StartedAt  time.Time `json:"started_at"`
	Deadline   time.Time `json:"deadline"`
}

type sourceTestObservation struct {
	Schema       int                  `json:"schema"`
	PlanSha256   string               `json:"plan_sha256"`
	CompletedAt  time.Time            `json:"completed_at"`
	Execution    *sourceTestExecution `json:"execution"`
	PrivateError string               `json:"private_error"`
}

// Link-after-fsync is exclusive publication, not rename-overwrite. Sync the
// directory before dispatching work so a crash cannot forget its reservation.
func publishSourceTestRecord(path string, content []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".independent-test-record-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o400); err != nil {
		file.Close()
		return err
	}
	_, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Link(temporary, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// Refuse links, replacements and oversized records before decoding authority.
func readSourceTestRecord(path string, limit int64, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 || info.Size() <= 0 || info.Size() > limit {
		return errors.New("independent test journal record is unsafe or oversized")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("independent test journal changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, limit))
	var extra [1]byte
	extraCount, extraErr := file.Read(extra[:])
	if err != nil || int64(len(content)) != info.Size() || extraCount != 0 || !errors.Is(extraErr, io.EOF) {
		return errors.New("independent test journal exceeds its read budget")
	}
	if err := rejectWinnerReviewCaseAliases(content); err != nil {
		return err
	}
	return decodeStrictJSONBytes(content, target, "independent test journal")
}

// Caller-supplied verification must authenticate the retained log bytes, not
// merely recognize the digest's syntax. It runs on initial success and replay.
func executeJournaledSourceTest(ctx context.Context, directory string, plan sourceTestPlan, deadline time.Time, settings sourceTestJournalSettings,
	run func(context.Context) (*sourceTestExecution, error), verify func(*sourceTestExecution) error) (*sourceTestExecution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if settings.MaximumRecordBytes <= 0 || settings.Now == nil || run == nil || verify == nil || !validSha256(plan.Owner) ||
		!validSha256(plan.SourceLockSha256) || !validSha256(plan.PatchSha256) || !validSha256(plan.SuiteSha256) ||
		!strings.HasPrefix(plan.ImageId, "sha256:") || !validSha256(strings.TrimPrefix(plan.ImageId, "sha256:")) ||
		!filepath.IsAbs(directory) || filepath.Clean(directory) != directory || filepath.Base(directory) != "independent-test-journal" || filepath.Base(filepath.Dir(directory)) != plan.Owner {
		return nil, errors.New("independent test journal requires exact private plan identity")
	}
	parent := filepath.Dir(directory)
	canonical, err := filepath.EvalSymlinks(parent)
	info, statErr := os.Lstat(parent)
	if err != nil || statErr != nil || canonical != parent || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("independent test journal parent is not canonical and private")
	}
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	canonical, err = filepath.EvalSymlinks(directory)
	info, statErr = os.Lstat(directory)
	if err != nil || statErr != nil || canonical != directory || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("independent test journal directory was redirected")
	}
	parentDirectory, err := os.Open(parent)
	if err != nil {
		return nil, err
	}
	syncErr := parentDirectory.Sync()
	closeErr := parentDirectory.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return nil, err
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(planBytes)
	planDigest := hex.EncodeToString(digest[:])
	reservationPath, outcomePath := filepath.Join(directory, "reservation.json"), filepath.Join(directory, "observation.json")
	var reservation sourceTestReservation
	owned := false
	if err := readSourceTestRecord(reservationPath, settings.MaximumRecordBytes, &reservation); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(outcomePath); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("independent test observation has no reservation; reconcile evidence")
		}
		now := settings.Now().UTC()
		if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(deadline) {
			deadline = parentDeadline
		}
		if deadline.IsZero() || !now.Before(deadline) {
			return nil, errors.New("independent test execution deadline has expired")
		}
		reservation = sourceTestReservation{Schema: 1, PlanSha256: planDigest, StartedAt: now, Deadline: deadline.UTC()}
		content, err := json.Marshal(reservation)
		if err != nil || int64(len(content)) > settings.MaximumRecordBytes {
			return nil, errors.New("independent test reservation exceeds its budget")
		}
		if err := publishSourceTestRecord(reservationPath, content); err == nil {
			owned = true
		} else if errors.Is(err, os.ErrExist) {
			if err := readSourceTestRecord(reservationPath, settings.MaximumRecordBytes, &reservation); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if reservation.Schema != 1 || reservation.PlanSha256 != planDigest || reservation.StartedAt.IsZero() || !reservation.StartedAt.Before(reservation.Deadline) {
		return nil, errors.New("independent test reservation belongs to changed or corrupt context")
	}
	validate := func(execution *sourceTestExecution) error {
		if execution == nil || execution.ImageId != plan.ImageId || execution.SuiteSha256 != plan.SuiteSha256 || execution.TestCount <= 0 || execution.ExitCode < 0 || execution.DurationMillis < 0 || !validSha256(execution.TestLogsSha256) {
			return errors.New("independent test observation does not bind complete execution")
		}
		return verify(execution)
	}
	if !owned {
		var outcome sourceTestObservation
		if err := readSourceTestRecord(outcomePath, settings.MaximumRecordBytes, &outcome); errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("independent test execution is active or interrupted; reconcile its actual owner, do not rerun")
		} else if err != nil {
			return nil, err
		}
		if outcome.Schema != 1 || outcome.PlanSha256 != planDigest || outcome.CompletedAt.Before(reservation.StartedAt) || outcome.CompletedAt.After(reservation.Deadline) {
			return nil, errors.New("independent test observation has invalid original timing or identity")
		}
		if outcome.PrivateError != "" {
			return nil, errors.New("independent test previously failed or was uncertain; operator investigation required")
		}
		if err := validate(outcome.Execution); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return outcome.Execution, nil
	}
	executionCtx, cancel := context.WithDeadline(ctx, reservation.Deadline)
	defer cancel()
	execution, executionErr := run(executionCtx)
	if executionErr == nil {
		executionErr = executionCtx.Err()
	}
	if executionErr == nil {
		executionErr = validate(execution)
	}
	completed := settings.Now().UTC()
	if completed.Before(reservation.StartedAt) || completed.After(reservation.Deadline) {
		executionErr = errors.Join(executionErr, errors.New("independent test completed outside its original time budget"))
	}
	outcome := sourceTestObservation{Schema: 1, PlanSha256: planDigest, CompletedAt: completed, Execution: execution}
	if executionErr != nil {
		outcome.PrivateError = executionErr.Error()
	}
	content, err := json.Marshal(outcome)
	if err != nil || int64(len(content)) > settings.MaximumRecordBytes {
		return nil, errors.New("independent test outcome exceeds its journal budget; execution must not rerun")
	}
	if err := publishSourceTestRecord(outcomePath, content); err != nil {
		return nil, err
	}
	if executionErr != nil {
		return nil, executionErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return execution, nil
}

// Retained correctness logs are read within a separate file budget. A replay
// cannot trust only a digest-shaped string if its evidence was lost or changed.
func verifySourceTestLogBundle(path, expected string, limit int64) error {
	if !validSha256(expected) || limit <= 0 {
		return errors.New("independent test evidence identity is invalid")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 || info.Size() <= 0 || info.Size() > limit {
		return errors.New("independent test evidence is absent, redirected or oversized")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("independent test evidence changed while opening")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(file, limit))
	var extra [1]byte
	extraCount, extraErr := file.Read(extra[:])
	if err != nil || count != info.Size() || extraCount != 0 || !errors.Is(extraErr, io.EOF) || hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("independent test evidence does not match its completed observation")
	}
	return nil
}

// Wraps the prescribed-test callback so completed observations survive transport
// failures without another container or another test execution.
func executeJournaledSourceCorrectness(ctx context.Context, request reviewedSourceBuildRequest, artifacts sourceBuildArtifacts, owner string, sandbox sourceBuildSandboxSettings,
	settings sourceTestJournalSettings, docker sourceBuildDocker) (*sourceTestExecution, error) {
	if err := validateSourceBuildRequest(request); err != nil {
		return nil, err
	}
	if artifacts.Directory != filepath.Join(sandbox.StateDirectory, owner) || settings.MaximumEvidenceBytes <= 0 || settings.Now == nil || sandbox.Timeout <= 0 {
		return nil, errors.New("journaled correctness requires the exact private build context")
	}
	plan := sourceTestPlan{Owner: owner, ImageId: artifacts.ImageId, SourceLockSha256: request.Proposal.SourceLockSha256, PatchSha256: request.Proposal.PatchSha256, SuiteSha256: sourceCorrectnessSuiteDigest()}
	if request.Proposal.WinnerReviewPolicy == model.CompetitionSnowWinnerReviewPolicy {
		plan.WinnerReviewPolicy = request.Proposal.WinnerReviewPolicy
	}
	return executeJournaledSourceTest(ctx, filepath.Join(artifacts.Directory, "independent-test-journal"), plan, settings.Now().Add(sandbox.Timeout), settings,
		func(testCtx context.Context) (*sourceTestExecution, error) {
			return executeSourceCorrectnessSuite(testCtx, request, artifacts, owner, sandbox, docker)
		}, func(execution *sourceTestExecution) error {
			return verifySourceTestLogBundle(filepath.Join(artifacts.Directory, "correctness", "test-logs.json"), execution.TestLogsSha256, settings.MaximumEvidenceBytes)
		})
}
