// A real API can authenticate published source for intake before a worker or
// shared baseline is ready. These checks never grant candidate execution or
// replace the existing independent grading-activation receipts.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var errCompetitionIntakeFirstPending = errors.New("intake is admitted but exact grading runtime or baseline is pending")
var errCompetitionSourceOperatorHandoff = errors.New("source publication requires current explicit operator handoff")

// Opt-in is prospective, scoped and independent from the running epoch's
// frozen policy. Merely staging it must not modify epoch-one snapshots.
func competitionIntakeFirstApplies(settings *Settings, intent *model.CompetitionSourceActivation) bool {
	return settings != nil && settings.IntakeFirstPolicy != nil && model.ValidateCompetitionIntakeFirstPolicy(settings.IntakeFirstPolicy) == nil &&
		settings.Enabled && settings.AutomationPolicy.Enabled && !settings.AutomationPolicy.Staging && intent != nil && !intent.Staging &&
		intent.TargetEpoch >= settings.IntakeFirstPolicy.FromEpoch && intent.TargetEpoch <= settings.SeasonPolicy.EpochCount &&
		intent.Environment == settings.AutomationPolicy.Environment && intent.CompetitionId == settings.CompetitionId && intent.SeasonId == settings.AutomationPolicy.SeasonId
}

// Only the actual recorded API receipt participates. A worker receipt, a
// caller's success bit or a different deployment image cannot stand in for it.
func competitionIntakeFirstApiReceiptDigest(intent *model.CompetitionSourceActivation, receipt model.CompetitionSourceReadiness) (string, error) {
	if !sourcePreparationReceiptMatches(intent, receipt) || receipt.Role != "api" || receipt.RecordedAt.IsZero() {
		return "", ErrReviewNotReady
	}
	return model.CompetitionAutomationDigest(receipt)
}

// The canonical existing schema-two snapshot freezes real image/command and
// instrument pins plus all numerical rules before opening. Database JSONB key
// ordering is not mistaken for another policy identity.
func competitionIntakeFirstFrozenSettings(settings *Settings, intent *model.CompetitionSourceActivation) (*Settings, string, error) {
	if !competitionIntakeFirstApplies(settings, intent) || intent.AbandonedAt != nil || intent.TargetRoundId == (server.Id{}) {
		return nil, "", ErrConflict
	}
	intentDigest, err := competitionPreparationIntentDigest(*intent)
	if err != nil || intentDigest != intent.IntentSha256 {
		return nil, "", ErrConflict
	}
	frozen, err := sourceSettingsFromIntent(settings, intent)
	if err != nil {
		return nil, "", err
	}
	content, err := policySnapshot(frozen)
	if err != nil {
		return nil, "", err
	}
	if err := competitionIntakeFirstNumericalPolicy(content); err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(content)
	return frozen, hex.EncodeToString(digest[:]), nil
}

// The new control capability must not accidentally open a scorer-one/two
// successor while its prospective coverage instrument has not been composed.
// The typed frozen-policy decoder owns numerical/version validation; this check
// requires the new leaf without choosing or duplicating its approved ratios.
func competitionIntakeFirstNumericalPolicy(content json.RawMessage) error {
	var identity struct {
		ScorerVersion    string `json:"scorer_version"`
		EvaluationPolicy struct {
			G2VolumePolicy     model.CompetitionG2VolumePolicy `json:"g2_volume_policy"`
			WorkCoveragePolicy json.RawMessage                 `json:"work_coverage_policy"`
		} `json:"evaluation_policy"`
	}
	if err := json.Unmarshal(content, &identity); err != nil {
		return err
	}
	if identity.ScorerVersion != "sim-latency-score/3" || identity.EvaluationPolicy.G2VolumePolicy != model.CompetitionG2VolumePolicyMinimumWork ||
		len(identity.EvaluationPolicy.WorkCoveragePolicy) == 0 || string(identity.EvaluationPolicy.WorkCoveragePolicy) == "null" {
		return errors.New("source-only prospective intake requires its explicitly frozen coverage instrument and floors-only policy")
	}
	_, err := decodeRoundPolicySnapshot(content)
	return err
}

// This builds a comparison value only. The store must independently recheck
// current source/review/publication authority, API receipt and its own fence.
func competitionIntakeFirstActivationValue(settings *Settings, intent *model.CompetitionSourceActivation, receipt model.CompetitionSourceReadiness, now time.Time) (*model.CompetitionSourceIntakeActivation, error) {
	_, policyDigest, err := competitionIntakeFirstFrozenSettings(settings, intent)
	if err != nil {
		return nil, err
	}
	apiDigest, err := competitionIntakeFirstApiReceiptDigest(intent, receipt)
	if err != nil {
		return nil, err
	}
	value := &model.CompetitionSourceIntakeActivation{Schema: 1, Kind: model.CompetitionIntakeFirstKind, Policy: *settings.IntakeFirstPolicy,
		Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, Generation: intent.Generation, ExpectedGeneration: intent.ExpectedGeneration,
		TargetEpoch: intent.TargetEpoch, TargetRoundId: intent.TargetRoundId, IntentSha256: intent.IntentSha256, RoundPolicySha256: policyDigest,
		ApiReadinessSha256: apiDigest, ApiRuntimeImageDigest: receipt.RuntimeImageDigest, SeasonEndsAt: settings.SeasonEndsAt.UTC(), RetainUntil: settings.RetainUntil.UTC(), CreatedAt: now.UTC()}
	if err := model.ValidateCompetitionSourceIntakeActivation(value); err != nil {
		return nil, err
	}
	return value, nil
}

// Intake authority survives later genuine runtime activation, not abandonment,
// a changed source/rule object or another API receipt. No worker ACK is inferred.
func competitionIntakeFirstActivationMatches(settings *Settings, intent *model.CompetitionSourceActivation, round *roundRecord, receipt model.CompetitionSourceReadiness, activation *model.CompetitionSourceIntakeActivation) (*Settings, error) {
	if model.ValidateCompetitionSourceIntakeActivation(activation) != nil {
		return nil, ErrConflict
	}
	expected, err := competitionIntakeFirstActivationValue(settings, intent, receipt, activation.CreatedAt)
	if err != nil || *expected != *activation {
		return nil, ErrConflict
	}
	frozen, _, err := competitionIntakeFirstFrozenSettings(settings, intent)
	if err != nil || round == nil || round.RoundId != intent.TargetRoundId || round.Epoch != intent.TargetEpoch || round.Staging ||
		round.CompetitionId != settings.CompetitionId || !storedPolicyMatches(frozen, round.PolicyJson) {
		return nil, ErrConflict
	}
	copy := *activation
	frozen.intakeFirstActivation = &copy
	return frozen, nil
}

// Product promotion authority never aliases the separately approved trusted
// instrument overlay or relabels its source lock as the manuscript's source.
func competitionIntakeFirstProductLock(intent *model.CompetitionSourceActivation) string {
	if intent == nil {
		return ""
	}
	if intent.TrustedInstrument != nil {
		return intent.TrustedInstrument.SelectedProductSourceLockSha256
	}
	return intent.SourceLockSha256
}

// A trusted overlay does not change which product the human is approving.
func competitionIntakeFirstProductKind(intent *model.CompetitionSourceActivation) string {
	if intent == nil {
		return ""
	}
	if intent.TrustedInstrument != nil {
		return intent.TrustedInstrument.SelectedProductKind
	}
	return intent.Kind
}

// This is an additional human barrier after genuine provider review. The
// concrete caller still rechecks current election/review state in its one
// transaction, so rejected/replaced decisions cannot reuse an earlier handoff.
func competitionSourceOperatorHandoffMatches(intent *model.CompetitionSourceActivation, handoff *model.CompetitionSourceOperatorHandoff) bool {
	return competitionSourceOperatorHandoffTargets(intent, handoff, true)
}

// Recording a rejection still requires the same current candidate identity;
// only subsequent publication checks require a positive disposition.
func competitionSourceOperatorHandoffTargets(intent *model.CompetitionSourceActivation, handoff *model.CompetitionSourceOperatorHandoff, approval bool) bool {
	if intent == nil || handoff == nil || handoff.Schema != 1 || handoff.Kind != model.CompetitionSourceOperatorHandoffKind ||
		model.ValidateCompetitionSourceOperatorHandoffArgs(handoff.Request) != nil || handoff.Revision != handoff.Request.ExpectedRevision+1 ||
		!tokenNamePattern.MatchString(handoff.ActorId) || handoff.CreatedAt.IsZero() ||
		handoff.Environment != intent.Environment || handoff.CompetitionId != intent.CompetitionId || handoff.SeasonId != intent.SeasonId ||
		handoff.Request.TargetEpoch != intent.TargetEpoch || handoff.Request.RewardDecisionSha256 != intent.RewardDecisionDigest ||
		handoff.Request.ProductSourceLockSha256 != competitionIntakeFirstProductLock(intent) {
		return false
	}
	digest, err := model.CompetitionAutomationDigest(handoff.Request)
	if err != nil || digest != handoff.RequestSha256 {
		return false
	}
	switch handoff.Request.Disposition {
	case "approve_promotion", "reject_promotion":
		return (!approval || handoff.Request.Disposition == "approve_promotion") && competitionIntakeFirstProductKind(intent) == "reviewed_source_promotion" && intent.ReviewId != nil && handoff.Request.ReviewId != nil && *intent.ReviewId == *handoff.Request.ReviewId &&
			handoff.Request.PatchSha256 == intent.ApprovedPatchSha256 && handoff.Request.ReviewDecisionSha256 == intent.ReviewDecisionDigest &&
			(intent.WinnerReview == nil || handoff.Request.SelectedJobId != nil && *handoff.Request.SelectedJobId == intent.WinnerReview.JobId)
	case "approve_no_winner_carry":
		return competitionIntakeFirstProductKind(intent) == "reviewed_source_carry_forward" && intent.Reason == "no_reward_winner" && intent.ReviewId == nil && intent.WinnerReview == nil
	default:
		return false
	}
}

// Choose the whole window only after external source/workload publication.
// Existing committed dates are returned by the store before calling this;
// an old announced reservation is never silently rescheduled as ASAP intake.
func competitionIntakeFirstOpening(settings *Settings, intent *model.CompetitionSourceActivation, grant *model.CompetitionSourceIntakeActivation, now time.Time) (GenerateRoundArgs, error) {
	if !competitionIntakeFirstApplies(settings, intent) || model.ValidateCompetitionSourceIntakeActivation(grant) != nil ||
		grant.Policy != *settings.IntakeFirstPolicy || grant.Environment != intent.Environment || grant.CompetitionId != intent.CompetitionId || grant.SeasonId != intent.SeasonId ||
		grant.TargetRoundId != intent.TargetRoundId || grant.TargetEpoch != intent.TargetEpoch || grant.Generation != intent.Generation || grant.ExpectedGeneration != intent.ExpectedGeneration ||
		grant.IntentSha256 != intent.IntentSha256 || !grant.SeasonEndsAt.Equal(settings.SeasonEndsAt) || !grant.RetainUntil.Equal(settings.RetainUntil) || !intent.OpensAt.IsZero() ||
		!now.Before(settings.SeasonEndsAt) || now.Before(grant.CreatedAt) || now.Before(intent.PreparedAt.Add(sourcePreparationInterval(intent))) {
		return GenerateRoundArgs{}, ErrConflict
	}
	window := time.Duration(settings.SeasonPolicy.SubmissionWindowSeconds) * time.Second
	if window != 7*24*time.Hour {
		return GenerateRoundArgs{}, ErrConflict
	}
	opens := now.UTC().Truncate(time.Microsecond)
	closes := opens.Add(window)
	if settings.SeasonEndsAt.Before(closes) || settings.RetainUntil.Before(closes) {
		return GenerateRoundArgs{}, fmt.Errorf("%w: a complete successor intake window requires an explicit season/retention amendment", ErrSeasonComplete)
	}
	return GenerateRoundArgs{OpensAt: opens, ClosesAt: closes, RevealAt: closes}, nil
}
