// Public API fixtures verify explicit contract selection and safe projections without a database.
package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Scope and prospective policy identities are visibly synthetic.
func automationApiSettings() *Settings {
	settings := validSettings()
	settings.CompetitionId = "test-competition"
	settings.AutomationPolicy = model.CompetitionAutomationPolicy{Enabled: true, Version: 2, Environment: "test-env", SeasonId: "test-season", ElectionRule: model.CompetitionElectionRule,
		ReviewCriteriaVersion: "test-review/1", SettlementPolicyVersion: "test-settlement/1", BurnPolicyVersion: "test-burn/1", RepeatRewardPolicy: "exclude_exact_identity", SettlementMode: "shadow",
		RequiredApiCapability: model.CompetitionAutomationCapability, PolicyApprovalSha256: strings.Repeat("a", 64), SourceLockSha256: strings.Repeat("b", 64), SourceActivationGeneration: 1}
	return settings
}

// Service tests exercise the actual v1/v2 boundary before the store's enqueue method.
func TestCompetitionV2AdmissionContractIsExplicit(t *testing.T) {
	_, store, service := newAutomationStagingApiFixture(t)
	roundId, jobId := store.stagingRound.RoundId, store.enqueueJob.JobId
	principal := &Principal{Id: "test-submitter", Role: "submitter"}
	args := ScoreArgs{RoundId: roundId, Patch: testPatch("test-change")}
	if _, status, evalError := service.Submit(context.Background(), args, principal); status != http.StatusConflict || evalError == nil || evalError.Code != "unsupported_decision_mode" || store.enqueueCalls != 0 {
		t.Fatalf("legacy admission accepted v2: %d %+v calls=%d", status, evalError, store.enqueueCalls)
	}
	accepted, status, evalError := service.SubmitV2(context.Background(), args, principal)
	if evalError != nil || status != http.StatusAccepted || accepted.StatusUrl != "/competition/v2/score/"+jobId.String() || accepted.DecisionMode != model.CompetitionDecisionModeV2 || store.enqueueCalls != 1 {
		t.Fatalf("v2 admission = %+v %d %+v calls=%d", accepted, status, evalError, store.enqueueCalls)
	}
	if _, status, _ := service.SubmitV2(context.Background(), args, &Principal{Id: "test-operator", Role: "operator"}); status != http.StatusForbidden || store.enqueueCalls != 1 {
		t.Fatal("legacy operator acquired v2 submitter authority")
	}
}

// The immutable v2 URL preserves outcome-neutral failure polling before reveal.
func TestCompetitionV2StatusDoesNotLeakBeforePublication(t *testing.T) {
	settings := automationApiSettings()
	policy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	jobId, roundId := server.NewId(), server.NewId()
	store := &fakeStore{getJob: &queuedJob{ScoreJobResult: ScoreJobResult{JobId: jobId, RoundId: roundId, State: "failed", EvalError: submissionError("candidate_build_failed", "private compiler output")}, Round: roundRecord{RoundResult: RoundResult{RoundId: roundId}, PolicyJson: policy}}}
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	principal := &Principal{Id: "test-submitter", Role: "submitter"}
	if _, status, evalError := service.GetScore(context.Background(), jobId, principal); status != http.StatusConflict || evalError == nil || evalError.Code != "unsupported_decision_mode" {
		t.Fatalf("legacy score URL accepted v2: %d %+v", status, evalError)
	}
	result, status, evalError := service.GetScoreV2(context.Background(), jobId, principal)
	if evalError != nil || status != http.StatusOK || result.State != "completed" || result.EvaluationStatus != "failed" || result.EvaluationFailure == nil || result.EvaluationFailure.Code != "candidate_build_failed" || result.Score != nil || result.EvalError != nil {
		t.Fatalf("unsafe v2 status projection: %+v %d %+v", result, status, evalError)
	}
}

// Publication never serializes private review findings or authenticated token names.
func TestCompetitionV2PublicProjectionRedactsPrivateFindings(t *testing.T) {
	scope, now, first, second := rewardAdapterFixture(t)
	private := rewardAdapterResult(t, scope, first, second)
	private.IntegrityReviews[0].ActorId = "private-reviewer-token-name"
	private.IntegrityReviews[0].Request.IdempotencyKey = "private-request-key"
	private.IntegrityReviews[0].Request.PrivateEvidenceSha256 = strings.Repeat("9", 64)
	private.IntegrityReviews[0].Request.PublicExplanation = "A specific published criterion failed."
	private.IntegrityReviews[0].Request.WithheldEvidenceExplanation = "Reproduction details are restricted for safety."
	private.IntegrityReviews[0].Request.Findings = []model.CompetitionReviewFinding{{Criterion: "containment", Disposition: "fail", ReasonCode: "SECRET_ACCESS", Explanation: "private exploit reproduction", EvidenceSha256: strings.Repeat("8", 64)}}
	private.SettlementReceipts = []model.CompetitionSettlementReceipt{{ActorId: "private-settlement-token-name", AccountingReference: "private-accounting-reference"}}
	if _, err := publicCompetitionV2Result(&private, now.Add(-time.Second)); err == nil {
		t.Fatal("unpublished result escaped")
	}
	public, err := publicCompetitionV2Result(&private, now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(public)
	for _, secret := range []string{"private-reviewer-token-name", "private-request-key", "private exploit reproduction", "private-settlement-token-name", "private-accounting-reference", strings.Repeat("9", 64), strings.Repeat("8", 64)} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("private material escaped: %q", secret)
		}
	}
	if public.IntegrityReviews[0].Request.PublicExplanation == "" || public.IntegrityReviews[0].Request.WithheldEvidenceExplanation == "" || private.IntegrityReviews[0].Request.Findings[0].Explanation == "" {
		t.Fatal("redaction lost public explanation or mutated durable private evidence")
	}
}

// Review of a different job cannot approve the current provisional recipient or its source.
func TestCompetitionV2ApprovalStateBelongsToCurrentCandidate(t *testing.T) {
	scope, now, first, second := rewardAdapterFixture(t)
	result := rewardAdapterResult(t, scope, first, second)
	result.ProvisionalRewardWinner = first.Decision.SelectedJobId
	result.IntegrityReviews[0].Request.JobId = server.NewId()
	result.IntegrityReviews[0].Request.RewardDisposition = "approved"
	result.IntegrityReviews[0].Request.SourceDisposition = "approved"
	public, err := publicCompetitionV2Result(&result, now)
	if err != nil || public.IntegrityReview != "not_reviewed" || public.SourceApproval != "pending" {
		t.Fatalf("unrelated approval leaked: %+v %v", public, err)
	}
	result.IntegrityReviews[0].Request.JobId = *first.Decision.SelectedJobId
	public, err = publicCompetitionV2Result(&result, now)
	if err != nil || public.IntegrityReview != "approved" || public.SourceApproval != "approved" {
		t.Fatalf("genuine current approval missing: %+v %v", public, err)
	}
}

// A reward appeal does not silently reapprove source rejected by an earlier integrity decision.
func TestCompetitionV2SourceApprovalDoesNotResurrectAfterDisqualification(t *testing.T) {
	jobId := server.RequireParseId("00000000-0000-0000-0000-000000000401")
	reviews := []model.CompetitionIntegrityReviewResult{
		{Request: model.CompetitionIntegrityReviewRequest{JobId: jobId, RewardDisposition: "approved", SourceDisposition: "approved"}},
		{Request: model.CompetitionIntegrityReviewRequest{JobId: jobId, RewardDisposition: "disqualified", SourceDisposition: "unchanged"}},
		{Request: model.CompetitionIntegrityReviewRequest{JobId: jobId, RewardDisposition: "restored", SourceDisposition: "unchanged"}},
	}
	if integrity, source := competitionV2CurrentReviewState(&jobId, reviews); integrity != "approved" || source != "rejected" {
		t.Fatalf("reward appeal resurrected old source approval: %s %s", integrity, source)
	}
	reviews[2].Request.SourceDisposition = "approved"
	if integrity, source := competitionV2CurrentReviewState(&jobId, reviews); integrity != "approved" || source != "approved" {
		t.Fatalf("fresh source approval ignored: %s %s", integrity, source)
	}
}

// Prospective criteria changes cannot rewrite the review contract of an already sealed round.
func TestCompetitionV2ReviewUsesFrozenCriteria(t *testing.T) {
	settings := automationApiSettings()
	policy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	scope, _, first, second := rewardAdapterFixture(t)
	result := rewardAdapterResult(t, scope, first, second)
	request := competitionAutomationReviewRequest(&result, result.Seal.Entries[0], false)
	request.CriteriaVersion = settings.AutomationPolicy.ReviewCriteriaVersion
	settings.AutomationPolicy.ReviewCriteriaVersion = "future-criteria/2"
	store := &fakeStore{round: &roundRecord{RoundResult: RoundResult{RoundId: result.Seal.RoundId}, PolicyJson: policy}}
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	if status, evalError := service.validateV2IntegrityReview(context.Background(), settings, result.Seal.RoundId, request); evalError != nil || status != http.StatusOK {
		t.Fatalf("historical criteria refused: %d %+v", status, evalError)
	}
	request.CriteriaVersion = settings.AutomationPolicy.ReviewCriteriaVersion
	if status, evalError := service.validateV2IntegrityReview(context.Background(), settings, result.Seal.RoundId, request); status != http.StatusBadRequest || evalError == nil || evalError.Retriable {
		t.Fatalf("future criteria rewrote the old round: %d %+v", status, evalError)
	}
}

// Durable policy refusals are non-retriable conflicts, not a storage-outage retry loop.
func TestCompetitionV2AdmissionPreservesTypedTransitionRefusals(t *testing.T) {
	for _, refusal := range []error{ErrConflict, ErrAutomationHeld, ErrEvaluationInvalidated, ErrAutomationDisabled} {
		_, store, service := newAutomationStagingApiFixture(t)
		store.enqueueErr = refusal
		_, status, evalError := service.SubmitV2(context.Background(), ScoreArgs{RoundId: store.stagingRound.RoundId, Patch: testPatch("synthetic")}, &Principal{Id: "test-submitter", Role: "submitter"})
		if status != http.StatusConflict || evalError == nil || evalError.Retriable || evalError.Kind != "submission" || store.enqueueCalls != 1 || store.readyCalls != 1 {
			t.Fatalf("durable refusal %v became a retry or bypassed readiness: %d %+v ready=%d enqueue=%d", refusal, status, evalError, store.readyCalls, store.enqueueCalls)
		}
	}
}

// Invalid cursors cannot silently reset a durable consumer to the beginning.
func TestCompetitionV2EventQueryIsBoundedAndStrict(t *testing.T) {
	invalid := []string{"after_cursor=-1", "after_cursor=01", "after_cursor=+1", "after_cursor=1&after_cursor=2", "limit=0", "limit=201", "limit=9999999999999999999999999999", "cursor=1", "limit=1.0"}
	for _, query := range invalid {
		if _, _, evalError := competitionRewardEventQuery(httptest.NewRequest(http.MethodGet, "/competition/v2/events?"+query, nil)); evalError == nil {
			t.Fatalf("accepted malformed query %q", query)
		}
	}
	cursor, limit, evalError := competitionRewardEventQuery(httptest.NewRequest(http.MethodGet, "/competition/v2/events?after_cursor=42&limit=200", nil))
	if evalError != nil || cursor != 42 || limit != 200 {
		t.Fatalf("valid cursor query = %d %d %+v", cursor, limit, evalError)
	}
}

// Syntactically invalid receipts are client errors, not retriable infrastructure failures.
func TestCompetitionV2ReceiptValidationSeparatesReceiptAndPayment(t *testing.T) {
	scope, _, event, _ := rewardAdapterFixture(t)
	receipt := model.CompetitionSettlementReceipt{EventId: event.EventId, Environment: scope.Environment, CompetitionId: scope.CompetitionId, SeasonId: scope.SeasonId,
		RoundId: event.Decision.RoundId, Revision: 1, PayloadSha256: event.PayloadSha256, DecisionSha256: event.Decision.DecisionSha256, Status: "received"}
	if evalError := validateCompetitionReceiptRequest(receipt); evalError != nil {
		t.Fatal(evalError)
	}
	for _, status := range []string{"paid", "applied", "confirmed"} {
		receipt.Status = status
		if evalError := validateCompetitionReceiptRequest(receipt); evalError == nil || evalError.Kind != "submission" || evalError.Retriable {
			t.Fatalf("invalid accounting status %q accepted: %+v", status, evalError)
		}
	}
}

// A genuine provisional decision is accepted without fabricating legacy honesty approval.
func TestApexV2ConformanceUsesSealedEligibilityAndImmutableHistory(t *testing.T) {
	scope, now, first, second := rewardAdapterFixture(t)
	result := rewardAdapterResult(t, scope, first, second)
	if err := ValidateApexV2RoundResult(result, scope, now); err != nil {
		t.Fatal(err)
	}
	result.Decisions = result.Decisions[:1]
	result.RewardRevision = 1
	result.ProvisionalRewardWinner = first.Decision.SelectedJobId
	result.IntegrityReviews = nil
	result.IntegrityReview, result.SourceApproval = "not_reviewed", "pending"
	if err := ValidateApexV2RoundResult(result, scope, now); err != nil {
		t.Fatalf("provisional unreviewed v2 winner rejected: %v", err)
	}
	result.Seal.Entries[0].Rank = 2
	if err := ValidateApexV2RoundResult(result, scope, now); err == nil {
		t.Fatal("mutated sealed ranking accepted")
	}
}
