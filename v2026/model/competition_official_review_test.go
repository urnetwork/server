// Operator amendments use synthetic identities and never impersonate a Snow
// union or grant authority through the ordinary review validator.
package model

import (
	"github.com/urnetwork/server/v2026"
	"strings"
	"testing"
	"time"
)

func officialReviewFixture() (CompetitionAutomationPolicy, CompetitionOfficialReviewImport) {
	policy, review := winnerProviderReviewFixture(CompetitionSnowWinnerReviewPolicy)
	review.ReviewerModel, review.ReviewerReasoning = "gpt-6-astra", "max"
	review.ProductionBenefitAddendum = CompetitionProductionBenefitReviewAddendum()
	review.OfficialReviewOverride = &CompetitionOfficialReviewOverride{AuthorizationSha256: strings.Repeat("e", 64), CompletedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), NativeQualificationWaived: true, UnacceptedCalibrationAcknowledged: true}
	return policy, CompetitionOfficialReviewImport{RoundId: server.Id{0: 2}, Review: review}
}

func TestCompetitionOfficialReviewRequiresDistinctImportAuthority(t *testing.T) {
	policy, request := officialReviewFixture()
	if err := ValidateCompetitionOfficialReviewImport(request, policy); err != nil {
		t.Fatal(err)
	}
	if ValidateCompetitionIntegrityReview(request.Review, policy) == nil {
		t.Fatal("ordinary route acquired exception authority")
	}
	if ValidateCompetitionSnowMemberContent(request.Review, policy) == nil {
		t.Fatal("native member route acquired exception authority")
	}
	for _, mutate := range []func(*CompetitionOfficialReviewImport){
		func(r *CompetitionOfficialReviewImport) { r.Review.ReviewerModel = CompetitionSnowWinnerReviewModel },
		func(r *CompetitionOfficialReviewImport) {
			r.Review.OfficialReviewOverride.NativeQualificationWaived = false
		},
		func(r *CompetitionOfficialReviewImport) {
			r.Review.OfficialReviewOverride.UnacceptedCalibrationAcknowledged = false
		},
		func(r *CompetitionOfficialReviewImport) { r.Review.OfficialReviewOverride.AuthorizationSha256 = "" },
		func(r *CompetitionOfficialReviewImport) { r.Review.OfficialReviewOverride.CompletedAt = time.Time{} },
		func(r *CompetitionOfficialReviewImport) { r.Review.Findings[0].Model = "synthetic-fabricated-member" },
		func(r *CompetitionOfficialReviewImport) { r.Review.Findings[0].Disposition = "uncertain" },
		func(r *CompetitionOfficialReviewImport) { r.Review.Findings = r.Review.Findings[:5] },
		func(r *CompetitionOfficialReviewImport) { r.Review.HandoffId = server.NewId().String() },
		func(r *CompetitionOfficialReviewImport) { r.Review.ProductionBenefitAddendum = nil },
	} {
		p, r := officialReviewFixture()
		mutate(&r)
		if ValidateCompetitionOfficialReviewImport(r, p) == nil {
			t.Fatal("incomplete or falsely attributed amendment accepted")
		}
	}
}

func TestCompetitionOfficialReviewDoesNotRenewNativeCheckpoint(t *testing.T) {
	_, args := officialReviewFixture()
	proof := args.Review.OfficialReviewOverride
	checkpoint := CompetitionWinnerReviewCheckpoint{RoundId: args.RoundId, JobId: args.Review.JobId, RewardRevision: args.Review.ExpectedRevision, RewardDecisionSha256: args.Review.ExpectedDecisionSha256, StartedAt: proof.CompletedAt.Add(-2 * time.Hour), Deadline: proof.CompletedAt.Add(-time.Hour), HandoffId: server.NewId().String()}
	before := checkpoint
	review := CompetitionIntegrityReviewResult{RoundId: args.RoundId, CreatedAt: proof.CompletedAt.Add(time.Minute), Request: args.Review}
	if !CompetitionReviewWithinCheckpoint(&review, &checkpoint) || checkpoint != before {
		t.Fatal("explicit amendment reset or required falsifying the historical clock")
	}
	checkpoint.JobId = server.NewId()
	if CompetitionReviewWithinCheckpoint(&review, &checkpoint) {
		t.Fatal("exception crossed elected identities")
	}
	checkpoint = before
	review.ModelCompletedAt = &proof.CompletedAt
	if CompetitionReviewWithinCheckpoint(&review, &checkpoint) {
		t.Fatal("exception impersonated a native completion")
	}
}
