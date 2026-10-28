// Private model-review reports use the same protected, replicated evidence
// store as evaluations. Retention never confers review or source authority.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
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
	if settings == nil || roundId == (server.Id{}) || jobId == (server.Id{}) || len(content) == 0 || len(content) > maximumEvidenceBytes {
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
