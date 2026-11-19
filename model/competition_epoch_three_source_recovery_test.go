package model

import (
	"strings"
	"testing"
	"time"
)

func TestCompetitionEpochThreeSourceRecoveryEvidence(t *testing.T) {
	policy, original, seal, checkpoint := epochTwoOfficialReviewFixture()
	original.Review.SourceDisposition = "deferred"
	original.Review.ReviewerModel = "gpt-6.2-astra"
	prior := &CompetitionIntegrityReviewResult{RoundId: seal.RoundId, CreatedAt: checkpoint.StartedAt.Add(time.Minute), Request: original.Review}
	decision := CompetitionRewardDecision{RoundId: seal.RoundId, SelectedJobId: &checkpoint.JobId, Revision: checkpoint.RewardRevision, DecisionSha256: checkpoint.RewardDecisionSha256}
	now := checkpoint.Deadline.Add(2 * time.Minute)
	next := original
	proof := *original.Review.OfficialReviewOverride
	proof.CompletedAt = now.Add(-time.Minute)
	next.Review.OfficialReviewOverride = &proof
	next.Review.SourceDisposition = "approved"
	next.Review.IdempotencyKey = "source-recovery-test"
	next.Review.PrivateEvidenceSha256 = strings.Repeat("f", 64)
	t.Run("checkpoint still open", func(t *testing.T) {
		if err := ValidateCompetitionEpochThreeSourceRecoveryEvidence(prior, next, seal, decision, &checkpoint, policy, checkpoint.Deadline.Add(-time.Nanosecond)); err == nil {
			t.Fatal("recovery must not replace the still-open approval path")
		}
	})
	t.Run("staging", func(t *testing.T) {
		stagingPolicy := policy
		stagingPolicy.Staging = true
		if err := ValidateCompetitionEpochThreeSourceRecoveryEvidence(prior, next, seal, decision, &checkpoint, stagingPolicy, now); err == nil {
			t.Fatal("production recovery must not authorize a staging round")
		}
	})
	for _, tc := range []struct {
		name string
		edit func(*CompetitionOfficialReviewImport)
	}{
		{"exact", nil},
		{"changed score", func(r *CompetitionOfficialReviewImport) { r.Review.ScoreSha256 = strings.Repeat("9", 64) }},
		{"changed revision", func(r *CompetitionOfficialReviewImport) { r.Review.ExpectedRevision++ }},
		{"reused assessment", func(r *CompetitionOfficialReviewImport) {
			r.Review.PrivateEvidenceSha256 = original.Review.PrivateEvidenceSha256
		}},
		{"reward changed", func(r *CompetitionOfficialReviewImport) { r.Review.RewardDisposition = "disqualified" }},
		{"source deferred", func(r *CompetitionOfficialReviewImport) { r.Review.SourceDisposition = "deferred" }},
		{"wrong reviewer", func(r *CompetitionOfficialReviewImport) { r.Review.ReviewerModel = "gpt-6-astra" }},
		{"assessment before expiry", func(r *CompetitionOfficialReviewImport) {
			r.Review.OfficialReviewOverride.CompletedAt = checkpoint.Deadline.Add(-time.Nanosecond)
		}},
		{"invalid evidence digest", func(r *CompetitionOfficialReviewImport) {
			r.Review.PrivateEvidenceSha256 = "not-a-digest"
		}},
		{"future assessment", func(r *CompetitionOfficialReviewImport) {
			r.Review.OfficialReviewOverride.CompletedAt = now.Add(time.Minute)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := next
			copyProof := *next.Review.OfficialReviewOverride
			request.Review.OfficialReviewOverride = &copyProof
			if tc.edit != nil {
				tc.edit(&request)
			}
			err := ValidateCompetitionEpochThreeSourceRecoveryEvidence(prior, request, seal, decision, &checkpoint, policy, now)
			if (err == nil) != (tc.edit == nil) {
				t.Fatalf("unexpected recovery validation result: %v", err)
			}
		})
	}
}
