// Synthetic prereviews keep their real times and cannot inherit epoch one's
// legacy waiver, native custody or permission to approve a different candidate.
package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Complete six-finding content exercises the real validator, not a shape stub.
func epochTwoOfficialReviewFixture() (CompetitionAutomationPolicy, CompetitionOfficialReviewImport, CompetitionEvaluationSeal, CompetitionWinnerReviewCheckpoint) {
	policy, request := officialReviewFixture()
	request.Review.OfficialReviewOverride.NativeQualificationWaived = false
	request.Review.OfficialReviewOverride.UnacceptedCalibrationAcknowledged = false
	completed := request.Review.OfficialReviewOverride.CompletedAt
	seal := CompetitionEvaluationSeal{RoundId: request.RoundId, Epoch: 2, BaselineSha256: request.Review.BaselineSha256, PublicationAt: completed.Add(time.Hour),
		Entries: []CompetitionSealedEntry{{JobId: request.Review.JobId, State: "succeeded", CompletedAt: completed.Add(-time.Minute), PatchSha256: request.Review.PatchSha256,
			BaseSourceLockSha256: request.Review.BaseSourceLockSha256, ScoreSha256: request.Review.ScoreSha256, ArtifactManifestSha256: request.Review.ArtifactManifestSha256}}}
	checkpoint := CompetitionWinnerReviewCheckpoint{RoundId: request.RoundId, JobId: request.Review.JobId, RewardRevision: request.Review.ExpectedRevision,
		RewardDecisionSha256: request.Review.ExpectedDecisionSha256, StartedAt: seal.PublicationAt, Deadline: seal.PublicationAt.Add(time.Hour)}
	return policy, request, seal, checkpoint
}

// The same request is refused by the frozen legacy validator and accepted only
// as an explicitly selected epoch-two reviewer substitution.
func TestCompetitionEpochTwoOfficialReviewPreservesEpochOne(t *testing.T) {
	policy, request, _, _ := epochTwoOfficialReviewFixture()
	before, _ := json.Marshal(request)
	if ValidateCompetitionOfficialReviewImport(request, policy) == nil || ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 1) == nil {
		t.Fatal("epoch-two request acquired legacy waiver authority")
	}
	if err := ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 2); err != nil {
		t.Fatal(err)
	}
	staging := policy
	staging.Staging = true
	if ValidateCompetitionOfficialReviewImportForEpoch(request, staging, 2) == nil {
		t.Fatal("production reviewer substitution entered staging")
	}
	unsupported := policy
	unsupported.WinnerReviewPolicy = ""
	if ValidateCompetitionOfficialReviewImportForEpoch(request, unsupported, 2) == nil {
		t.Fatal("reviewer substitution invented an absent closing policy")
	}
	for _, epoch := range []int{0, 3, 6} {
		if ValidateCompetitionOfficialReviewImportForEpoch(request, policy, epoch) == nil {
			t.Fatal("reviewer substitution crossed epoch", epoch)
		}
	}
	legacyPolicy, legacy := officialReviewFixture()
	if ValidateCompetitionOfficialReviewImportForEpoch(legacy, legacyPolicy, 1) != nil || ValidateCompetitionOfficialReviewImportForEpoch(legacy, legacyPolicy, 2) == nil {
		t.Fatal("epoch-one legacy behavior changed or its waiver entered epoch two")
	}
	after, _ := json.Marshal(request)
	if string(before) != string(after) {
		t.Fatal("validation altered immutable evidence")
	}
}

// Both genuine IDs remain exact. No alias, neighboring model, or changed effort
// can enter the import path or pass stored checkpoint validation.
func TestCompetitionEpochTwoOfficialReviewPinsExactReviewer(t *testing.T) {
	for _, pair := range []struct {
		model, reasoning string
		accepted         bool
	}{
		{"gpt-6-astra", "max", true},
		{"gpt-6.2-astra", "max", true},
		{"gpt-6.2-astra", "high", false},
		{"gpt-6-astra", "low", false},
		{"gpt-6.2-sol", "max", false},
		{"gpt-6.2-astra ", "max", false},
		{"Astra Max", "max", false},
		{"", "max", false},
		{"gpt-6.2-astra", "", false},
	} {
		policy, request, _, checkpoint := epochTwoOfficialReviewFixture()
		request.Review.ReviewerModel, request.Review.ReviewerReasoning = pair.model, pair.reasoning
		review := CompetitionIntegrityReviewResult{RoundId: request.RoundId, Request: request.Review, CreatedAt: checkpoint.StartedAt}
		if accepted := ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 2) == nil; accepted != pair.accepted {
			t.Fatal("unexpected epoch-two import identity", pair, accepted)
		}
		if accepted := CompetitionReviewWithinCheckpointForEpoch(&review, &checkpoint, 2); accepted != pair.accepted {
			t.Fatal("unexpected epoch-two checkpoint identity", pair, accepted)
		}
	}

	policy, request, _, checkpoint := epochTwoOfficialReviewFixture()
	request.Review.ReviewerModel = "gpt-6.2-astra"
	review := CompetitionIntegrityReviewResult{RoundId: request.RoundId, Request: request.Review, CreatedAt: checkpoint.StartedAt}
	if ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 3) == nil || CompetitionReviewWithinCheckpointForEpoch(&review, &checkpoint, 3) {
		t.Fatal("epoch-two reviewer entered a later epoch")
	}
	policy.Staging = true
	if ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 2) == nil {
		t.Fatal("epoch-two reviewer entered staging")
	}
	policy, request = officialReviewFixture()
	request.Review.ReviewerModel = "gpt-6.2-astra"
	review.Request = request.Review
	if ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 1) == nil || CompetitionReviewWithinCheckpointForEpoch(&review, &checkpoint, 1) {
		t.Fatal("epoch-two reviewer acquired the historical epoch-one waiver")
	}
}

// An honest integrity finding does not by itself approve production deployment.
func TestCompetitionEpochTwoOfficialReviewDoesNotForceSourceApproval(t *testing.T) {
	policy, request, _, _ := epochTwoOfficialReviewFixture()
	request.Review.SourceDisposition = "deferred"
	request.Review.ReasonCodes = []string{"SOURCE_NOT_READY"}
	if err := ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 2); err != nil {
		t.Fatal("independent source readiness was forced approved", err)
	}
	failed := request
	failed.Review.Findings = append([]CompetitionReviewFinding{}, request.Review.Findings...)
	failed.Review.ReasonCodes = []string{"UNSAFE_BEHAVIOR"}
	failed.Review.Findings[5].Disposition, failed.Review.Findings[5].ReasonCode = "fail", "UNSAFE_BEHAVIOR"
	manual, manualPolicy := failed.Review, policy
	manual.OfficialReviewOverride, manualPolicy.WinnerReviewPolicy = nil, ""
	if err := validateCompetitionIntegrityReviewContent(manual, manualPolicy, false); err != nil {
		t.Fatal("root-cause control no longer exercises the permissive manual boundary", err)
	}
	if ValidateCompetitionOfficialReviewImportForEpoch(failed, policy, 2) == nil {
		t.Fatal("failed honesty finding gained reward approval by deferring source")
	}
	request.Review.RewardDisposition = "disqualified"
	if ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 2) == nil {
		t.Fatal("source concern disqualified a reward")
	}
	request.Review.SourceDisposition = "rejected"
	request.Review.ReasonCodes = []string{"SCORE_TAMPERING"}
	if ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 2) == nil {
		t.Fatal("unsubstantiated allegation disqualified a reward")
	}
	request.Review.Findings[0].Disposition, request.Review.Findings[0].ReasonCode = "fail", "SCORE_TAMPERING"
	if err := ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 2); err != nil {
		t.Fatal("substantiated review rejected", err)
	}
	request.Review.SourceDisposition = "deferred"
	if ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 2) == nil {
		t.Fatal("substantive disqualification failed to reject the same source")
	}
	request.Review.RewardDisposition, request.Review.SourceDisposition = "approved", "rejected"
	if ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 2) == nil {
		t.Fatal("winner rejection revoked source without the paired reward finding")
	}
	policy, request, _, _ = epochTwoOfficialReviewFixture()
	request.Review.RewardDisposition, request.Review.SourceDisposition, request.Review.ReasonCodes = "disqualified", "rejected", []string{"NO_PRODUCTION_BENEFIT"}
	request.Review.Findings[1].Disposition, request.Review.Findings[1].ReasonCode = "fail", "NO_PRODUCTION_BENEFIT"
	if err := ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 2); err != nil {
		t.Fatal("non-misconduct production-benefit ineligibility was erased", err)
	}
}

// Every provider, evidence, criterion and addendum predicate remains active.
func TestCompetitionEpochTwoOfficialReviewRejectsWaiverAndFabricatedCustody(t *testing.T) {
	for i, mutate := range []func(*CompetitionOfficialReviewImport){
		func(r *CompetitionOfficialReviewImport) {
			r.Review.OfficialReviewOverride.NativeQualificationWaived = true
		},
		func(r *CompetitionOfficialReviewImport) { r.Review.ReviewerModel = "synthetic-other-model" },
		func(r *CompetitionOfficialReviewImport) { r.Review.ReviewerReasoning = "high" },
		func(r *CompetitionOfficialReviewImport) { r.Review.HandoffId = server.NewId().String() },
		func(r *CompetitionOfficialReviewImport) { r.Review.Findings[0].Model = "synthetic-native-member" },
		func(r *CompetitionOfficialReviewImport) { r.Review.Findings = r.Review.Findings[:5] },
		func(r *CompetitionOfficialReviewImport) { r.Review.Findings[0].Disposition = "uncertain" },
		func(r *CompetitionOfficialReviewImport) { r.Review.Findings[1].Disposition = "not_applicable" },
		func(r *CompetitionOfficialReviewImport) {
			r.Review.SourceDisposition, r.Review.ReasonCodes = "deferred", []string{"UNSAFE_BEHAVIOR"}
			r.Review.Findings[5].Disposition, r.Review.Findings[5].ReasonCode = "fail", "UNSAFE_BEHAVIOR"
		},
		func(r *CompetitionOfficialReviewImport) { r.Review.ProductionBenefitAddendum = nil },
		func(r *CompetitionOfficialReviewImport) { r.Review.ExpectedDecisionSha256 = "" },
		func(r *CompetitionOfficialReviewImport) { r.Review.CriteriaVersion = "synthetic-other-criteria" },
		func(r *CompetitionOfficialReviewImport) { r.Review.OfficialReviewOverride.CompletedAt = time.Time{} },
	} {
		policy, request, _, _ := epochTwoOfficialReviewFixture()
		mutate(&request)
		if ValidateCompetitionOfficialReviewImportForEpoch(request, policy, 2) == nil {
			t.Fatal("invalid review accepted", i)
		}
	}
}

// The earlier completion is retained exactly; only the import clock must fit
// the current elected checkpoint, which this helper never mutates.
func TestCompetitionEpochTwoOfficialPrereviewAdoptsWithoutRetiming(t *testing.T) {
	_, request, seal, checkpoint := epochTwoOfficialReviewFixture()
	review := CompetitionIntegrityReviewResult{RoundId: request.RoundId, Request: request.Review, CreatedAt: checkpoint.StartedAt.Add(time.Minute)}
	before := checkpoint
	if !CompetitionOfficialReviewPrereviewMatchesSeal(request, seal) || !CompetitionReviewWithinCheckpointForEpoch(&review, &checkpoint, 2) ||
		checkpoint != before || !review.Request.OfficialReviewOverride.CompletedAt.Before(checkpoint.StartedAt) {
		t.Fatal("genuine prereview was rejected, retimed or reset a checkpoint")
	}
	if CompetitionReviewWithinCheckpoint(&review, &checkpoint) {
		t.Fatal("legacy clock exemption accepted epoch-two content")
	}
}

// Matching hashes alone cannot claim review of score evidence not yet created.
func TestCompetitionEpochTwoOfficialPrereviewRequiresExistingExactEvidence(t *testing.T) {
	for i, mutate := range []func(*CompetitionOfficialReviewImport, *CompetitionEvaluationSeal){
		func(r *CompetitionOfficialReviewImport, s *CompetitionEvaluationSeal) {
			s.Entries[0].CompletedAt = r.Review.OfficialReviewOverride.CompletedAt.Add(time.Second)
		},
		func(r *CompetitionOfficialReviewImport, s *CompetitionEvaluationSeal) {
			s.Entries[0].CompletedAt = time.Time{}
		},
		func(r *CompetitionOfficialReviewImport, s *CompetitionEvaluationSeal) { s.Entries[0].State = "failed" },
		func(r *CompetitionOfficialReviewImport, s *CompetitionEvaluationSeal) {
			s.Entries[0].JobId = server.NewId()
		},
		func(r *CompetitionOfficialReviewImport, s *CompetitionEvaluationSeal) {
			s.Entries[0].PatchSha256 = "changed"
		},
		func(r *CompetitionOfficialReviewImport, s *CompetitionEvaluationSeal) {
			s.Entries[0].BaseSourceLockSha256 = "changed"
		},
		func(r *CompetitionOfficialReviewImport, s *CompetitionEvaluationSeal) {
			s.Entries[0].ScoreSha256 = "changed"
		},
		func(r *CompetitionOfficialReviewImport, s *CompetitionEvaluationSeal) {
			s.Entries[0].ArtifactManifestSha256 = "changed"
		},
		func(r *CompetitionOfficialReviewImport, s *CompetitionEvaluationSeal) { s.BaselineSha256 = "changed" },
		func(r *CompetitionOfficialReviewImport, s *CompetitionEvaluationSeal) { s.RoundId = server.NewId() },
		func(r *CompetitionOfficialReviewImport, s *CompetitionEvaluationSeal) { s.Staging = true },
		func(r *CompetitionOfficialReviewImport, s *CompetitionEvaluationSeal) { s.Epoch = 3 },
	} {
		_, request, seal, _ := epochTwoOfficialReviewFixture()
		mutate(&request, &seal)
		if CompetitionOfficialReviewPrereviewMatchesSeal(request, seal) {
			t.Fatal("unavailable or mismatched evidence accepted", i)
		}
	}
}

// No missing, expired, foreign or native-owned checkpoint can be adopted.
func TestCompetitionEpochTwoOfficialPrereviewKeepsImportDeadlineAndIdentity(t *testing.T) {
	for i, mutate := range []func(*CompetitionIntegrityReviewResult, *CompetitionWinnerReviewCheckpoint){
		func(r *CompetitionIntegrityReviewResult, c *CompetitionWinnerReviewCheckpoint) {
			r.CreatedAt = c.Deadline
		},
		func(r *CompetitionIntegrityReviewResult, c *CompetitionWinnerReviewCheckpoint) {
			r.CreatedAt = c.StartedAt.Add(-time.Second)
		},
		func(r *CompetitionIntegrityReviewResult, c *CompetitionWinnerReviewCheckpoint) {
			r.Request.OfficialReviewOverride.CompletedAt = r.CreatedAt.Add(time.Second)
		},
		func(r *CompetitionIntegrityReviewResult, c *CompetitionWinnerReviewCheckpoint) {
			c.JobId = server.NewId()
		},
		func(r *CompetitionIntegrityReviewResult, c *CompetitionWinnerReviewCheckpoint) {
			c.RoundId = server.NewId()
		},
		func(r *CompetitionIntegrityReviewResult, c *CompetitionWinnerReviewCheckpoint) { c.RewardRevision++ },
		func(r *CompetitionIntegrityReviewResult, c *CompetitionWinnerReviewCheckpoint) {
			c.RewardDecisionSha256 = "changed"
		},
		func(r *CompetitionIntegrityReviewResult, c *CompetitionWinnerReviewCheckpoint) {
			c.HandoffId = server.NewId().String()
		},
		func(r *CompetitionIntegrityReviewResult, c *CompetitionWinnerReviewCheckpoint) {
			completed := r.Request.OfficialReviewOverride.CompletedAt
			r.ModelCompletedAt = &completed
		},
	} {
		_, request, _, checkpoint := epochTwoOfficialReviewFixture()
		review := CompetitionIntegrityReviewResult{RoundId: request.RoundId, Request: request.Review, CreatedAt: checkpoint.StartedAt.Add(time.Minute)}
		mutate(&review, &checkpoint)
		if CompetitionReviewWithinCheckpointForEpoch(&review, &checkpoint, 2) {
			t.Fatal("invalid import clock or identity accepted", i)
		}
	}
	_, request, _, _ := epochTwoOfficialReviewFixture()
	if CompetitionReviewWithinCheckpointForEpoch(&CompetitionIntegrityReviewResult{Request: request.Review}, nil, 2) {
		t.Fatal("missing checkpoint acquired replacement clock")
	}
}
