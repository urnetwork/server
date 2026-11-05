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
	"strings"
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
		// Observe this exact fixture while it is alive; an external poll can
		// miss the short interval between migration completion and cleanup.
		var schemaHead int
		server.Db(t.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(t.Context(), `SELECT COALESCE(MAX(end_version_number),0)
			 FROM migration_audit WHERE status='success'`).Scan(&schemaHead))
		}, server.OptReadOnly(), server.OptNoRetry())
		if schemaHead != server.MigrationCount() {
			t.Fatalf("review packet fixture schema head %d differs from source head %d", schemaHead, server.MigrationCount())
		}
		t.Logf("review_packet_fixture_schema_head=%d", schemaHead)
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
		if _, err := fixture.store.SourceReviewPacket(ctx, fixture.settings, job.RoundId, job.JobId); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unsealed candidate became final review evidence: %v", err)
		}
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
		if packet.RewardDecision == nil || packet.RewardDecision.SelectedJobId == nil || *packet.RewardDecision.SelectedJobId != job.JobId ||
			packet.RewardRevision != packet.RewardDecision.Revision || packet.WinnerReviewPolicy != fixture.settings.AutomationPolicy.WinnerReviewPolicy ||
			validateCompetitionReviewDecision(packet.RewardDecision, &packet.Seal) != nil {
			t.Fatal("review packet lost the exact current elected decision or frozen reviewer policy")
		}
		if packet.ScoreArtifact == nil || packet.ScoreArtifact.Path != "score.json" || packet.ScoreArtifact.Key != "synthetic/score.json" ||
			packet.ScoreArtifact.VersionId != "synthetic-version" || packet.ScoreArtifact.Sha256 != packet.Entry.ScoreSha256 || packet.ScoreArtifact.Bytes != int64(len(scoreBytes)) ||
			packet.ManifestArtifact == nil || packet.ManifestArtifact.Key != "synthetic/manifest.json" || packet.ManifestArtifact.Sha256 != packet.Entry.ArtifactManifestSha256 ||
			packet.ManifestArtifact.Bytes <= 0 || packet.ManifestArtifact.VersionId != "" {
			t.Fatal("packet omitted exact score version/manifest content identity or invented a manifest object version")
		}
		if err := ValidateCompetitionReviewInstrument(packet); err != nil || packet.Instrument.EvaluationPolicy != fixture.settings.EvaluationPolicy {
			t.Fatal("review packet lost activated instrument or frozen evaluation policy", err)
		}
		paused := *fixture.settings
		paused.AutomationPolicy.Enabled = false
		paused.AutomationPolicy.WinnerReviewPolicy = "synthetic-not-the-frozen-policy"
		if packet, err := fixture.store.SourceReviewPacket(ctx, &paused, job.RoundId, job.JobId); err != nil || packet == nil || !bytes.Equal(packet.Score, scoreBytes) || packet.WinnerReviewPolicy != fixture.settings.AutomationPolicy.WinnerReviewPolicy {
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

// A valid digest of another scope is still not the selected round's decision;
// a same-scope changed payload cannot retain an older digest either.
func TestCompetitionReviewDecisionRejectsCrossScopeAndChangedPayload(t *testing.T) {
	seal := model.CompetitionEvaluationSeal{RoundId: server.NewId(), Environment: "synthetic-environment", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season", SealSha256: strings.Repeat("a", 64)}
	decision := model.CompetitionRewardDecision{RoundId: seal.RoundId, Environment: seal.Environment, CompetitionId: seal.CompetitionId,
		SeasonId: seal.SeasonId, SealSha256: seal.SealSha256, Revision: 2, Action: "elect"}
	var err error
	decision.DecisionSha256, err = model.CompetitionAutomationDigest(decision)
	if err != nil || validateCompetitionReviewDecision(&decision, &seal) != nil {
		t.Fatalf("valid synthetic decision refused: %v", err)
	}
	for _, mutate := range []func(*model.CompetitionRewardDecision){
		func(value *model.CompetitionRewardDecision) { value.RoundId = server.NewId() },
		func(value *model.CompetitionRewardDecision) { value.Environment = "synthetic-other-environment" },
		func(value *model.CompetitionRewardDecision) { value.CompetitionId = "synthetic-other-competition" },
		func(value *model.CompetitionRewardDecision) { value.SeasonId = "synthetic-other-season" },
		func(value *model.CompetitionRewardDecision) { value.Staging = true },
		func(value *model.CompetitionRewardDecision) { value.SealSha256 = strings.Repeat("b", 64) },
		func(value *model.CompetitionRewardDecision) { value.Revision = 0 },
	} {
		changed := decision
		mutate(&changed)
		changed.DecisionSha256 = ""
		changed.DecisionSha256, err = model.CompetitionAutomationDigest(changed)
		if err != nil || !errors.Is(validateCompetitionReviewDecision(&changed, &seal), ErrConflict) {
			t.Fatalf("cross-scope or invalid decision accepted: %+v, %v", changed, err)
		}
	}
	decision.Action = "changed-synthetic-action"
	if !errors.Is(validateCompetitionReviewDecision(&decision, &seal), ErrConflict) || !errors.Is(validateCompetitionReviewDecision(&decision, nil), ErrConflict) {
		t.Fatal("changed or unscoped decision reused a sealed identity")
	}
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
