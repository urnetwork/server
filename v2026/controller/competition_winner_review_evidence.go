// Private model-review reports use the same protected, replicated evidence
// store as evaluations. Retention never confers review or source authority.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The exact immutable version is private; the public review exposes its digest.
type CompetitionWinnerReviewEvidence struct {
	Key         string    `json:"key"`
	VersionId   string    `json:"version_id"`
	Sha256      string    `json:"sha256"`
	RetainUntil time.Time `json:"retain_until"`
}

// Runs outside database transactions and refuses local/unprotected storage.
// The report is bounded and content-addressed under its exact round and job.
func RetainCompetitionWinnerReviewEvidence(ctx context.Context, settings *Settings, roundId, jobId server.Id, content []byte, expected string) (*CompetitionWinnerReviewEvidence, error) {
	maximumEvidenceBytes := 64 * 1024
	if settings != nil && model.IsCompetitionSnowWinnerReviewPolicy(settings.AutomationPolicy.WinnerReviewPolicy) {
		maximumEvidenceBytes = 512 * 1024
	}
	return retainCompetitionReviewEvidence(ctx, settings, roundId, jobId, content, expected, 1, maximumEvidenceBytes)
}

// The approved multi-pass protocol retains complete visible calls rather than
// squeezing them into the historical single-call report. This separate bound
// never widens the old API or silently truncates an oversized evidence bundle.
// Empty original leaves (for example stderr) retain their exact empty digest.
func RetainCompetitionReviewHandoffEvidence(ctx context.Context, settings *Settings, roundId, jobId server.Id, content []byte, expected string) (*CompetitionWinnerReviewEvidence, error) {
	if settings == nil || settings.AutomationPolicy.WinnerReviewPolicy != model.CompetitionSnowWinnerReviewPolicy {
		return nil, errors.New("multi-pass retention requires the exact Qwen/Gemma policy")
	}
	return retainCompetitionReviewEvidence(ctx, settings, roundId, jobId, content, expected, 0, 128*1024*1024)
}

func retainCompetitionReviewEvidence(ctx context.Context, settings *Settings, roundId, jobId server.Id, content []byte, expected string, minimumEvidenceBytes, maximumEvidenceBytes int) (*CompetitionWinnerReviewEvidence, error) {
	if settings == nil || roundId == (server.Id{}) || jobId == (server.Id{}) || len(content) < minimumEvidenceBytes || len(content) > maximumEvidenceBytes {
		return nil, errors.New("winner review evidence identity or size is invalid")
	}
	for _, scope := range []string{settings.CompetitionId, settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId} {
		if !tokenNamePattern.MatchString(scope) {
			return nil, errors.New("winner review evidence scope is invalid")
		}
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != expected {
		return nil, errors.New("winner review evidence digest changed")
	}
	archive, ok := settings.artifactArchive.(*blobArtifactArchive)
	if !ok || archive == nil {
		return nil, errors.New("winner review requires the protected evidence archive")
	}
	store, err := archive.runtimeManifestStore(ctx)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "competition-private-review-")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err = file.Write(content); err != nil {
		file.Close()
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	key := filepath.ToSlash(filepath.Join(store.Prefix(), "competition", "v2", "review-evidence", settings.CompetitionId, settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId, roundId.String(), jobId.String(), expected+".txt"))
	receipt, err := archive.retainFile(ctx, settings, "private-review.txt", key, path, expected, int64(len(content)), "text/plain")
	if err != nil {
		return nil, err
	}
	if receipt.VersionId == "" || receipt.Mode != "COMPLIANCE" {
		return nil, errors.New("winner review evidence lacks an immutable retained version")
	}
	return &CompetitionWinnerReviewEvidence{Key: receipt.Key, VersionId: receipt.VersionId, Sha256: receipt.Sha256, RetainUntil: receipt.RetainUntil}, nil
}

// A completed review must remain replayable from the exact protected version,
// not just a local cache. The key cannot escape this round/job's content address.
// This read confers no review, source, reward or deletion authority.
func LoadCompetitionReviewHandoffEvidence(ctx context.Context, settings *Settings, roundId, jobId server.Id, receipt model.CompetitionReviewHandoffRetention, expectedBytes int64) ([]byte, error) {
	if settings == nil || settings.AutomationPolicy.WinnerReviewPolicy != model.CompetitionSnowWinnerReviewPolicy || roundId == (server.Id{}) || jobId == (server.Id{}) ||
		expectedBytes < 0 || expectedBytes > 128*1024*1024 || !sha256Pattern.MatchString(receipt.Sha256) || receipt.VersionId == "" || receipt.RetainUntil.IsZero() {
		return nil, errors.New("protected handoff original identity is incomplete")
	}
	for _, scope := range []string{settings.CompetitionId, settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId} {
		if !tokenNamePattern.MatchString(scope) {
			return nil, errors.New("protected handoff original scope is invalid")
		}
	}
	archive, ok := settings.artifactArchive.(*blobArtifactArchive)
	if !ok || archive == nil || archive.store == nil {
		return nil, errors.New("protected handoff original store is unavailable")
	}
	store, err := archive.runtimeManifestStore(ctx)
	if err != nil {
		return nil, err
	}
	key := filepath.ToSlash(filepath.Join(store.Prefix(), "competition", "v2", "review-evidence", settings.CompetitionId, settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId, roundId.String(), jobId.String(), receipt.Sha256+".txt"))
	if receipt.Key != key {
		return nil, errors.New("protected handoff original key is outside its exact scope")
	}
	reader, err := archive.store.GetVersion(ctx, key, receipt.VersionId)
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(reader, expectedBytes+1))
	closeErr := reader.Close()
	digest := sha256.Sum256(content)
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(content)) != expectedBytes || hex.EncodeToString(digest[:]) != receipt.Sha256 {
		return nil, errors.New("protected handoff original version differs from retained bytes")
	}
	return content, nil
}
