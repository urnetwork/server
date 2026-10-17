package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Formatting is part of the archived identity. Parsing and serializing again
// must not silently substitute different bytes for a review's score digest.
func TestCompetitionReviewArtifactPreservesOriginalBytes(t *testing.T) {
	content := []byte("{\n  \"raw_score\": 71\n}\n")
	digest := sha256.Sum256(content)
	actual, err := readCompetitionReviewArtifact(io.NopCloser(bytes.NewReader(content)), hex.EncodeToString(digest[:]), int64(len(content)))
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("original score bytes were not preserved: %q, %v", actual, err)
	}
}

// A synthetic versioned adapter exercises the exact-version call without a
// live object store. Unknown versions are never replaced with the latest one.
type competitionReviewVersionedFixture struct {
	server.RetainedBlobStore
}

func (self competitionReviewVersionedFixture) GetVersion(ctx context.Context, key, version string) (io.ReadCloser, error) {
	if version != "synthetic-version" {
		return nil, errors.New("unexpected synthetic object version")
	}
	return self.RetainedBlobStore.GetVersion(ctx, key, "")
}

// A reviewer can inspect evidence before approving anything. Archived bytes,
// scope and seal identities remain mandatory even though approval is absent.
func TestCompetitionAutomationReviewPacketBeforeApproval(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		root := t.TempDir()
		store := competitionReviewVersionedFixture{RetainedBlobStore: server.NewLocalBlobStore(filepath.Join(root, "blobs"), "synthetic").(server.RetainedBlobStore)}
		fixture.settings.artifactArchive = &blobArtifactArchive{store: store}
		localScore := filepath.Join(root, "score.json")
		localManifest := filepath.Join(root, "manifest.json")
		var scoreBytes []byte
		job := addCompetitionAutomationJobWithManifest(t, fixture, 70, func(job *queuedJob, score *ScoreResult) (json.RawMessage, error) {
			var err error
			scoreBytes, err = json.MarshalIndent(score, "", "  ")
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(localScore, scoreBytes, 0600); err != nil {
				t.Fatal(err)
			}
			retained, err := store.PutRetained(ctx, "synthetic/score.json", localScore, "application/json", fixture.settings.RetainUntil)
			if err != nil {
				t.Fatal(err)
			}
			scoreDigest := sha256.Sum256(scoreBytes)
			manifest := artifactManifest{Schema: 1, JobId: job.JobId.String(), RoundId: job.RoundId.String(), PatchSha256: job.PatchSha256, EvaluatorImageDigest: fixture.settings.EvaluatorImageDigest,
				Artifacts: []evaluationArtifact{{Path: "score.json", Sha256: hex.EncodeToString(scoreDigest[:]), Bytes: int64(len(scoreBytes))}},
				Retention: &artifactRetention{ManifestKey: "synthetic/manifest.json", Objects: []retainedArtifact{{Path: "score.json", Key: retained.Key,
					Sha256: hex.EncodeToString(scoreDigest[:]), Bytes: retained.Size, VersionId: "synthetic-version"}}}}
			manifestBytes, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(localManifest, manifestBytes, 0600); err != nil {
				t.Fatal(err)
			}
			if err := store.Put(ctx, manifest.Retention.ManifestKey, localManifest, "application/json"); err != nil {
				t.Fatal(err)
			}
			return manifestBytes, nil
		})
		fixture.now = fixture.round.RevealAt
		if _, err := fixture.store.SealAndElect(ctx, fixture.settings, job.RoundId); err != nil {
			t.Fatal(err)
		}
		packet, err := fixture.store.SourceReviewPacket(ctx, fixture.settings, job.RoundId, job.JobId)
		if err != nil || packet == nil || !bytes.Equal(packet.Score, scoreBytes) || !bytes.Equal(packet.Patch, job.Patch) {
			t.Fatalf("pre-approval packet lost authenticated bytes: %+v, %v", packet, err)
		}
		lock, err := model.CompetitionSourceLockDigest(packet.SourceCommits)
		if err != nil || lock != packet.Entry.BaseSourceLockSha256 || packet.SourceCommits["server"] != packet.BaseSha || packet.ActivationGeneration != 1 {
			t.Fatalf("review packet lost its authenticated complete source base: %+v, %v", packet.SourceCommits, err)
		}
		paused := *fixture.settings
		paused.AutomationPolicy.Enabled = false
		if packet, err := fixture.store.SourceReviewPacket(ctx, &paused, job.RoundId, job.JobId); err != nil || packet == nil || !bytes.Equal(packet.Score, scoreBytes) {
			t.Fatalf("disabling new automation stranded an existing review: %v", err)
		}
		otherScope := *fixture.settings
		otherScope.AutomationPolicy.SeasonId = "synthetic-other-season"
		if _, err := fixture.store.SourceReviewPacket(ctx, &otherScope, job.RoundId, job.JobId); !errors.Is(err, ErrConflict) {
			t.Fatalf("cross-season packet was not refused: %v", err)
		}
		if err := os.WriteFile(localManifest, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, "synthetic/manifest.json", localManifest, "application/json"); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.SourceReviewPacket(ctx, fixture.settings, job.RoundId, job.JobId); err == nil {
			t.Fatal("tampered manifest was accepted for private review")
		}
	})
}

// Neither same-length tampering nor a truncated/extended object can reuse an
// immutable artifact identity. Empty and oversized declarations fail boundedly.
func TestCompetitionReviewArtifactRejectsConflictingBytes(t *testing.T) {
	content := []byte("synthetic archived score\n")
	digest := sha256.Sum256(content)
	expected := hex.EncodeToString(digest[:])
	for _, fixture := range []struct {
		content []byte
		size    int64
	}{
		{content: append([]byte("S"), content[1:]...), size: int64(len(content))},
		{content: content, size: int64(len(content)) - 1},
		{content: content, size: int64(len(content)) + 1},
		{content: content, size: 0},
		{content: content, size: 16*1024*1024 + 1},
	} {
		if _, err := readCompetitionReviewArtifact(io.NopCloser(bytes.NewReader(fixture.content)), expected, fixture.size); err == nil {
			t.Fatal("conflicting archived review artifact accepted")
		}
	}
}
