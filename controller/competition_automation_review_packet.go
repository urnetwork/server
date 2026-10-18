// Private review material is authenticated before it reaches the reviewing agent.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// This trusted control-plane result is not a public projection. Score contains
// original archived bytes, not a JSONB serialization with a different digest.
type CompetitionSourceReviewPacket struct {
	Seal                  model.CompetitionEvaluationSeal `json:"seal"`
	Entry                 model.CompetitionSealedEntry    `json:"entry"`
	Patch                 []byte                          `json:"patch"`
	Score                 []byte                          `json:"score"`
	PatchPolicy           PatchPolicy                     `json:"patch_policy"`
	BaseSha               string                          `json:"base_sha"`
	EvaluatedImageDigest  string                          `json:"evaluated_image_digest"`
	ReviewCriteriaVersion string                          `json:"review_criteria_version"`
	RewardRevision        int                             `json:"reward_revision"`
	SourceCommits         map[string]string               `json:"source_commits"`
	SourceGeneration      int                             `json:"source_generation"`
	ActivationGeneration  int64                           `json:"activation_generation"`
}

// Reads bounded immutable bytes and checks both identity and declared length.
func readCompetitionReviewArtifact(reader io.ReadCloser, expected string, size int64) ([]byte, error) {
	if !sha256Pattern.MatchString(expected) || size <= 0 || size > 16*1024*1024 {
		reader.Close()
		return nil, errors.New("review artifact identity or length is invalid")
	}
	content, err := io.ReadAll(io.LimitReader(reader, size+1))
	closeErr := reader.Close()
	if err != nil || closeErr != nil || int64(len(content)) != size {
		return nil, errors.New("review artifact length does not match")
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != expected {
		return nil, errors.New("review artifact digest does not match")
	}
	return content, nil
}

// Materializes evidence without requiring an approval first. Database resources
// are released before any MinIO request; candidate code is never executed here.
func (self PostgresStore) SourceReviewPacket(ctx context.Context, settings *Settings, roundId, jobId server.Id) (*CompetitionSourceReviewPacket, error) {
	// Disabling new automation must not strand existing review obligations.
	// This read still requires the exact configured scope of the frozen round.
	if settings == nil || !tokenNamePattern.MatchString(settings.CompetitionId) ||
		!tokenNamePattern.MatchString(settings.AutomationPolicy.Environment) || !tokenNamePattern.MatchString(settings.AutomationPolicy.SeasonId) {
		return nil, ErrAutomationDisabled
	}
	packet := &CompetitionSourceReviewPacket{}
	var manifestJson []byte
	err := captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			var sealJson, policyJson []byte
			server.Raise(conn.QueryRow(ctx, `SELECT s.seal_json,j.patch_bytes,j.artifact_manifest_json,r.policy_json,
				COALESCE((SELECT max(revision) FROM competition_reward_decision WHERE round_id=r.round_id),0)
				FROM competition_evaluation_seal s JOIN competition_round r ON r.round_id=s.round_id
				JOIN competition_job j ON j.round_id=r.round_id
				WHERE r.round_id=$1 AND j.job_id=$2 AND r.competition_id=$3`, roundId, jobId, settings.CompetitionId).
				Scan(&sealJson, &packet.Patch, &manifestJson, &policyJson, &packet.RewardRevision))
			server.Raise(json.Unmarshal(sealJson, &packet.Seal))
			policy, err := decodeRoundPolicySnapshot(policyJson)
			server.Raise(err)
			if policy.AutomationPolicy == nil || !policy.AutomationPolicy.Enabled || policy.AutomationPolicy.Staging != settings.AutomationPolicy.Staging || packet.Seal.Staging != settings.AutomationPolicy.Staging || policy.AutomationPolicy.Environment != settings.AutomationPolicy.Environment ||
				policy.AutomationPolicy.SeasonId != settings.AutomationPolicy.SeasonId {
				panic(ErrConflict)
			}
			found := false
			for _, entry := range packet.Seal.Entries {
				if entry.JobId == jobId && entry.State == "succeeded" && entry.Score != nil {
					packet.Entry, found = entry, true
					break
				}
			}
			patchDigest := sha256.Sum256(packet.Patch)
			if !found || packet.Seal.RoundId != roundId || hex.EncodeToString(patchDigest[:]) != packet.Entry.PatchSha256 ||
				packet.Entry.BaseSourceLockSha256 != policy.AutomationPolicy.SourceLockSha256 {
				panic(ErrConflict)
			}
			packet.PatchPolicy, packet.BaseSha, packet.EvaluatedImageDigest = policy.PatchPolicy, policy.BaseSha, policy.EvaluatorImageDigest
			packet.ReviewCriteriaVersion = policy.AutomationPolicy.ReviewCriteriaVersion
			var intentJson []byte
			server.Raise(conn.QueryRow(ctx, `SELECT intent_json FROM competition_source_activation
				WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4
				  AND activated_at IS NOT NULL AND abandoned_at IS NULL`, policy.AutomationPolicy.Environment,
				settings.CompetitionId, policy.AutomationPolicy.SeasonId, policy.AutomationPolicy.SourceActivationGeneration).Scan(&intentJson))
			var intent model.CompetitionSourceActivation
			server.Raise(json.Unmarshal(intentJson, &intent))
			lock, err := model.CompetitionSourceLockDigest(intent.Commits)
			server.Raise(err)
			if intent.Staging != policy.AutomationPolicy.Staging || intent.TargetRoundId != roundId || lock != packet.Entry.BaseSourceLockSha256 || intent.Commits["server"] != packet.BaseSha ||
				intent.SourceGeneration != policy.AutomationPolicy.SourceGeneration || intent.EvaluatorImageDigest != packet.EvaluatedImageDigest {
				panic(ErrConflict)
			}
			packet.SourceCommits, packet.SourceGeneration, packet.ActivationGeneration = intent.Commits, intent.SourceGeneration, intent.Generation
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	archive, ok := settings.artifactArchive.(*blobArtifactArchive)
	if !ok || archive == nil || archive.store == nil {
		return nil, errors.New("immutable review archive is unavailable")
	}
	var manifest artifactManifest
	if err := json.Unmarshal(manifestJson, &manifest); err != nil || manifest.Retention == nil || manifest.Retention.ManifestKey == "" {
		return nil, errors.New("review artifact retention identity is unavailable")
	}
	// The manifest's content-addressed key is checked against the sealed digest.
	reader, err := archive.store.Get(ctx, manifest.Retention.ManifestKey)
	if err != nil {
		return nil, err
	}
	manifestBytes, readErr := io.ReadAll(io.LimitReader(reader, 16*1024*1024+1))
	closeErr := reader.Close()
	manifestDigest := sha256.Sum256(manifestBytes)
	if readErr != nil || closeErr != nil || len(manifestBytes) > 16*1024*1024 || hex.EncodeToString(manifestDigest[:]) != packet.Entry.ArtifactManifestSha256 {
		return nil, errors.New("archived review manifest does not match the sealed identity")
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.Retention == nil ||
		manifest.JobId != jobId.String() || manifest.RoundId != roundId.String() || manifest.PatchSha256 != packet.Entry.PatchSha256 ||
		manifest.EvaluatorImageDigest != packet.EvaluatedImageDigest {
		return nil, errors.New("archived review manifest has a conflicting identity")
	}
	var scoreArtifact *retainedArtifact
	for index := range manifest.Retention.Objects {
		artifact := &manifest.Retention.Objects[index]
		if artifact.Path == "score.json" {
			if scoreArtifact != nil || artifact.Sha256 != packet.Entry.ScoreSha256 || artifact.Key == "" || artifact.VersionId == "" {
				return nil, errors.New("archived review score has a conflicting immutable identity")
			}
			scoreArtifact = artifact
		}
	}
	if scoreArtifact == nil {
		return nil, errors.New("archived review score is missing")
	}
	reader, err = archive.store.GetVersion(ctx, scoreArtifact.Key, scoreArtifact.VersionId)
	if err != nil {
		return nil, err
	}
	packet.Score, err = readCompetitionReviewArtifact(reader, scoreArtifact.Sha256, scoreArtifact.Bytes)
	if err != nil {
		return nil, err
	}
	var archivedScore model.CompetitionScoreResult
	if err := json.Unmarshal(packet.Score, &archivedScore); err != nil {
		return nil, errors.New("archived review score is malformed")
	}
	actualScoreDigest, actualErr := model.CompetitionAutomationDigest(archivedScore)
	expectedScoreDigest, expectedErr := model.CompetitionAutomationDigest(packet.Entry.Score)
	if actualErr != nil || expectedErr != nil || actualScoreDigest != expectedScoreDigest {
		return nil, errors.New("archived review score disagrees with the sealed result")
	}
	return packet, nil
}
