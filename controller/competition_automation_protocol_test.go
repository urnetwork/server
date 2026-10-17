// Final protocol regressions exercise authenticated history independently of external settlement.
package controller

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Copies nested evidence so one malformed fixture cannot corrupt another case.
func cloneApexRewardResult(t *testing.T, result model.CompetitionV2RoundResult) model.CompetitionV2RoundResult {
	t.Helper()
	content, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var copy model.CompetitionV2RoundResult
	if err := json.Unmarshal(content, &copy); err != nil {
		t.Fatal(err)
	}
	return copy
}

// A real incident appends an explicit terminal revision while preserving the original seal.
func apexInvalidatedResult(t *testing.T) (ApexRewardScope, time.Time, model.CompetitionV2RoundResult, model.CompetitionRewardEvent, model.CompetitionRewardEvent) {
	t.Helper()
	scope, now, first, second := rewardAdapterFixture(t)
	result := rewardAdapterResult(t, scope, first, second)
	second.Decision.Action, second.Decision.ReasonCode, second.Decision.ReviewId = "invalidated", "EVALUATION_INTEGRITY_INCIDENT", nil
	second.Decision.DecisionSha256 = ""
	second.Decision.DecisionSha256, _ = model.CompetitionAutomationDigest(second.Decision)
	second.PayloadSha256, _ = model.CompetitionAutomationDigest(second.Decision)
	result.Decisions[1] = second.Decision
	result.IntegrityReviews, result.Invalidated = nil, true
	result.Incidents = []model.CompetitionAutomationIncident{{IncidentId: server.RequireParseId("00000000-0000-0000-0000-000000000501"), RoundId: result.Seal.RoundId,
		Environment: scope.Environment, CompetitionId: scope.CompetitionId, SeasonId: scope.SeasonId, CreatedAt: now, RequestSha256: strings.Repeat("1", 64),
		Request: model.CompetitionAutomationIncidentArgs{ExpectedRewardRevision: 1, InvalidatesEvaluation: true, ReasonCode: "EVALUATION_INTEGRITY_INCIDENT"}}}
	return scope, now, result, first, second
}

// Neither a content hash nor a null winner substitutes for an exact scoped integrity incident.
func TestApexV2InvalidationRequiresExactIncidentProvenance(t *testing.T) {
	scope, now, result, _, _ := apexInvalidatedResult(t)
	if err := ValidateApexV2RoundResult(result, scope, now); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*model.CompetitionV2RoundResult){
		func(result *model.CompetitionV2RoundResult) { result.Invalidated = false },
		func(result *model.CompetitionV2RoundResult) { result.Incidents = nil },
		func(result *model.CompetitionV2RoundResult) { result.Incidents[0].Environment = "other-env" },
		func(result *model.CompetitionV2RoundResult) { result.Incidents[0].Request.ExpectedRewardRevision = 2 },
		func(result *model.CompetitionV2RoundResult) { result.Incidents[0].CreatedAt = now.Add(time.Second) },
	}
	for index, mutate := range mutations {
		copy := cloneApexRewardResult(t, result)
		mutate(&copy)
		if err := ValidateApexV2RoundResult(copy, scope, now); err == nil {
			t.Fatalf("invalid incident mutation %d accepted", index)
		}
	}
}

// No append-only appeal may reuse measurements from a terminally invalidated evaluation.
func TestApexRewardInboxRefusesDecisionAfterInvalidation(t *testing.T) {
	scope, now, _, first, invalidated := apexInvalidatedResult(t)
	store, err := NewApexRewardFileStore(filepath.Join(t.TempDir(), "private-inbox"), scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPage(model.CompetitionRewardEventPage{Events: []model.CompetitionRewardEvent{first, invalidated}, NextCursor: invalidated.Cursor}, now); err != nil {
		t.Fatal(err)
	}
	next := first
	next.EventId, next.Cursor = server.RequireParseId("00000000-0000-0000-0000-000000000502"), invalidated.Cursor+1
	next.Decision.Revision, next.Decision.Action, next.Decision.SupersededRevision = 3, "replace", 2
	next.Decision.PreviousDecisionSha256 = invalidated.Decision.DecisionSha256
	next.Decision.DecisionSha256 = ""
	next.Decision.DecisionSha256, _ = model.CompetitionAutomationDigest(next.Decision)
	next.PayloadSha256, _ = model.CompetitionAutomationDigest(next.Decision)
	if err := validateApexRewardEvent(next, scope, now); err != nil {
		t.Fatalf("fixture should fail on its predecessor, not syntax: %v", err)
	}
	if err := store.RecordPage(model.CompetitionRewardEventPage{Events: []model.CompetitionRewardEvent{next}, NextCursor: next.Cursor}, now); err == nil {
		t.Fatal("invalidated measurements created another reward revision")
	}
	if cursor, records, err := store.Snapshot(); err != nil || cursor != invalidated.Cursor || len(records) != 2 {
		t.Fatalf("refusal altered the inbox: %d %d %v", cursor, len(records), err)
	}
}

// Human-friendly approval labels are derived state, not independent authority.
func TestApexV2ConformanceRejectsInventedApprovalAndMissingReplacement(t *testing.T) {
	scope, now, first, second := rewardAdapterFixture(t)
	result := rewardAdapterResult(t, scope, first, second)
	result.Decisions, result.RewardRevision, result.ProvisionalRewardWinner = result.Decisions[:1], 1, first.Decision.SelectedJobId
	review := result.IntegrityReviews[0]
	result.IntegrityReviews, result.IntegrityReview, result.SourceApproval = nil, "not_reviewed", "pending"
	if err := ValidateApexV2RoundResult(result, scope, now); err != nil {
		t.Fatal(err)
	}
	result.IntegrityReview, result.SourceApproval = "approved", "approved"
	if err := ValidateApexV2RoundResult(result, scope, now); err == nil {
		t.Fatal("unreviewed source acquired a fabricated approval")
	}
	result.IntegrityReview, result.SourceApproval = "disqualified", "rejected"
	review.RewardDecision = first.Decision
	result.IntegrityReviews = []model.CompetitionIntegrityReviewResult{review}
	if err := ValidateApexV2RoundResult(result, scope, now); err == nil {
		t.Fatal("a disqualification without its replacement revision was accepted")
	}
}

// Reviews cannot be transplanted between rounds or bound to different archived evidence.
func TestApexV2ConformanceRejectsMisboundOrIncompleteReview(t *testing.T) {
	scope, now, first, second := rewardAdapterFixture(t)
	result := rewardAdapterResult(t, scope, first, second)
	mutations := []func(*model.CompetitionV2RoundResult){
		func(result *model.CompetitionV2RoundResult) { result.IntegrityReviews[0].RoundId = first.EventId },
		func(result *model.CompetitionV2RoundResult) {
			result.IntegrityReviews[0].Request.ScoreSha256 = strings.Repeat("0", 64)
		},
		func(result *model.CompetitionV2RoundResult) { result.IntegrityReviews[0].Request.Findings = nil },
		func(result *model.CompetitionV2RoundResult) {
			result.IntegrityReviews[0].Request.RewardDisposition = "automatic_approval"
		},
	}
	for index, mutate := range mutations {
		copy := cloneApexRewardResult(t, result)
		mutate(&copy)
		if err := ValidateApexV2RoundResult(copy, scope, now); err == nil {
			t.Fatalf("misbound review mutation %d accepted", index)
		}
	}
}

// Percentage serialization tolerance is shared with eligibility; ranking still uses exact raw scores.
func TestApexV2ConformanceUsesSharedMarginNormalization(t *testing.T) {
	scope, now, first, second := rewardAdapterFixture(t)
	result := rewardAdapterResult(t, scope, first, second)
	result.Seal.TakeoverMargin = math.Nextafter(result.Seal.TakeoverMargin, 1)
	result.Seal.SealSha256 = ""
	result.Seal.SealSha256, _ = model.CompetitionAutomationDigest(result.Seal)
	decision := first.Decision
	decision.SealSha256, decision.DecisionSha256 = result.Seal.SealSha256, ""
	decision.DecisionSha256, _ = model.CompetitionAutomationDigest(decision)
	result.Decisions, result.RewardRevision, result.ProvisionalRewardWinner = []model.CompetitionRewardDecision{decision}, 1, decision.SelectedJobId
	result.IntegrityReviews, result.IntegrityReview, result.SourceApproval = nil, "not_reviewed", "pending"
	if err := ValidateApexV2RoundResult(result, scope, now); err != nil {
		t.Fatalf("adapter diverged from shared percentage normalization: %v", err)
	}
}

// The readiness boundary observes the exact activated source without trusting arbitrary policy JSON.
type automationReadinessStore struct {
	*fakeStore
	intent *model.CompetitionSourceActivation
}

// Immutable generation lookup never resolves a moving source ref.
func (self *automationReadinessStore) SourceActivation(_ context.Context, _ *Settings, generation int64) (*model.CompetitionSourceActivation, error) {
	if self.intent == nil || self.intent.Generation != generation {
		return nil, ErrNotFound
	}
	return self.intent, nil
}

// A real host would compare these identities with its independent qualification report.
func (self *automationReadinessStore) Readiness(_ context.Context, settings *Settings) (map[string]bool, error) {
	return map[string]bool{"frozen_source": settings.BaseSha == self.intent.Commits["server"] && settings.AutomationPolicy.SourceActivationGeneration == self.intent.Generation &&
		settings.EvaluationPolicy.HostQualificationSha256 == self.intent.HostQualificationSha256}, nil
}

// Season-static settings do not require a per-epoch manual config rollout to become ready.
func TestCompetitionV2ReadinessBindsActivatedReservedRoundSource(t *testing.T) {
	settings := automationApiSettings()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	intent := &model.CompetitionSourceActivation{Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
		Generation: 2, SourceGeneration: 1, TargetEpoch: 2, TargetRoundId: server.RequireParseId("00000000-0000-0000-0000-000000000503"),
		EvaluatorImageDigest: settings.EvaluatorImageDigest, HostQualificationSha256: strings.Repeat("9", 64), Threshold: .2, ActivatedAt: &now, State: "activated", Commits: map[string]string{}}
	for _, name := range []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "operator-proxy", "warp", "gvisor"} {
		intent.Commits[name] = strings.Repeat("c", 40)
	}
	var err error
	intent.SourceLockSha256, err = model.CompetitionSourceLockDigest(intent.Commits)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := sourceSettingsFromIntent(settings, intent)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := policySnapshot(frozen)
	if err != nil {
		t.Fatal(err)
	}
	store := &automationReadinessStore{fakeStore: &fakeStore{round: &roundRecord{RoundResult: RoundResult{RoundId: intent.TargetRoundId, Epoch: intent.TargetEpoch}, PolicyJson: policy}}, intent: intent}
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	if result, evalError := service.Ready(context.Background()); evalError != nil || !result.Ready {
		t.Fatalf("exact approved source needs a manual config rewrite: %+v %+v", result, evalError)
	}
	if settings.BaseSha == frozen.BaseSha || settings.AutomationPolicy.SourceActivationGeneration != 1 {
		t.Fatal("readiness mutated the season's settings")
	}
	for _, mutate := range []func(*model.CompetitionSourceActivation){
		func(intent *model.CompetitionSourceActivation) { intent.TargetRoundId = server.Id{} },
		func(intent *model.CompetitionSourceActivation) { intent.ActivatedAt = nil },
		func(intent *model.CompetitionSourceActivation) { intent.TargetEpoch++ },
	} {
		copy := *intent
		mutate(&copy)
		store.intent = &copy
		if result, evalError := service.Ready(context.Background()); evalError == nil || evalError.Code != "source_not_ready" || result.Ready {
			t.Fatalf("unactivated or differently bound source became ready: %+v %+v", result, evalError)
		}
	}
	store.intent = intent
	frozen.EvaluationPolicy.ScorerSha256 = strings.Repeat("0", 64)
	store.round.PolicyJson, err = policySnapshot(frozen)
	if err != nil {
		t.Fatal(err)
	}
	if result, evalError := service.Ready(context.Background()); evalError == nil || result.Ready {
		t.Fatal("source loader trusted an unrelated changed scorer policy")
	}
}
