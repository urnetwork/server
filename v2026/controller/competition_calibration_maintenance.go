// Calibration maintenance is one new bounded approval, never a generic waiver
// for offline evaluators or a replay of the initial revision-one intake permit.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// Each service owns its authority settings. Non-positive duration disables the
// permit; the default allows the approved roughly nine-hour serial calibration
// and verification, measured from the sole durable hold's original creation.
type competitionCalibrationMaintenanceAuthority struct {
	Scope                    competitionIntakeAuthority
	OwnerSha256              string
	EvaluationRevision       int
	EvaluationIdempotencyKey string
	EvaluationRequestSha256  string
	MaximumDuration          time.Duration
}

// Raw owner-record bytes and the seven-field Go-order canonical request are
// pinned separately. Neither a new revision nor an idempotent replay renews it.
func approvedCompetitionCalibrationMaintenanceAuthority() competitionCalibrationMaintenanceAuthority {
	return competitionCalibrationMaintenanceAuthority{
		Scope:                    approvedCompetitionIntakeAuthority(),
		OwnerSha256:              "de4bbd4bd9b281a50afc9fb5387b6361f20351388578acddc8f28284981a013c",
		EvaluationRevision:       4,
		EvaluationIdempotencyKey: "r2-epoch1-calibration-maintenance-intake-20261030-v1",
		EvaluationRequestSha256:  "e792b5aed90c6fcb0f4f5c844919e72e084e9fb4d6403a678fff5a0e47a2b5dd",
		MaximumDuration:          10 * time.Hour,
	}
}

// Only the active evaluation maintenance hold may differ from the original
// approval. Admission remains unheld and reward delivery retains its exact hold.
func (self competitionCalibrationMaintenanceAuthority) holdsMatch(status competitionAutomationHoldStatus, now time.Time) bool {
	hold := status.EvaluationHold
	if self.MaximumDuration <= 0 || self.EvaluationRevision < 1 || self.EvaluationIdempotencyKey == "" ||
		!sha256Pattern.MatchString(self.OwnerSha256) || !sha256Pattern.MatchString(self.EvaluationRequestSha256) ||
		hold == nil || status.Admission || !status.Evaluation || !status.RewardDelivery ||
		hold.Environment != self.Scope.Environment || hold.CompetitionId != self.Scope.CompetitionId || hold.SeasonId != self.Scope.SeasonId ||
		hold.Revision != self.EvaluationRevision || hold.Request.ExpectedRevision != self.EvaluationRevision-1 ||
		hold.Request.Kind != "evaluation" || !hold.Request.Active || hold.Request.ReasonCode != "CALIBRATION_MAINTENANCE" ||
		hold.Request.IdempotencyKey != self.EvaluationIdempotencyKey || hold.Request.EvidenceSha256 != self.OwnerSha256 ||
		hold.RequestSha256 != self.EvaluationRequestSha256 || hold.CreatedAt.IsZero() || now.Before(hold.CreatedAt) || !now.Before(hold.CreatedAt.Add(self.MaximumDuration)) {
		return false
	}
	digest, err := model.CompetitionAutomationDigest(hold.Request)
	if err != nil || digest != self.EvaluationRequestSha256 {
		return false
	}
	// Reuse only the original reward check, not the original evaluation hold.
	reward := status.RewardDeliveryHold
	if reward == nil {
		return false
	}
	rewardDigest, err := model.CompetitionAutomationDigest(reward.Request)
	return err == nil && rewardDigest == self.Scope.RewardRequestSha256 && reward.Environment == self.Scope.Environment && reward.CompetitionId == self.Scope.CompetitionId && reward.SeasonId == self.Scope.SeasonId &&
		reward.HoldId.String() == self.Scope.RewardHoldId && reward.Revision == 1 && reward.RequestSha256 == self.Scope.RewardRequestSha256 &&
		reward.Request.Kind == "reward_delivery" && reward.Request.Active && reward.Request.ReasonCode == "OPERATOR_HOLD" && reward.Request.EvidenceSha256 == self.Scope.OwnerSha256
}

// Revalidate the previously authenticated intent under the same scope lock and
// transaction as each enqueue pass. No database or archive connection is nested.
func checkCompetitionCalibrationMaintenanceSource(ctx context.Context, reader competitionIntakeReader, settings *Settings, round *roundRecord, expectedIntentSha256 string) error {
	if !sha256Pattern.MatchString(expectedIntentSha256) {
		return errors.New("authenticated maintenance runtime intent is unavailable")
	}
	var content []byte
	var intentSha256 string
	var activatedAt, abandonedAt *time.Time
	var currentGeneration int64
	var invalidated bool
	err := reader.QueryRow(ctx, `SELECT a.intent_json,a.intent_sha256,a.activated_at,a.abandoned_at,p.generation,
 EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=$5 AND invalidates_evaluation)
 FROM competition_source_activation a JOIN competition_source_pointer p
 USING(environment,competition_id,season_id)
 WHERE a.environment=$1 AND a.competition_id=$2 AND a.season_id=$3 AND a.generation=$4`,
		settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, settings.AutomationPolicy.SourceActivationGeneration, round.RoundId).
		Scan(&content, &intentSha256, &activatedAt, &abandonedAt, &currentGeneration, &invalidated)
	if err != nil || activatedAt == nil || abandonedAt != nil || invalidated || currentGeneration != settings.AutomationPolicy.SourceActivationGeneration || intentSha256 != expectedIntentSha256 {
		return errors.New("calibration maintenance source is no longer active")
	}
	var intent model.CompetitionSourceActivation
	if json.Unmarshal(content, &intent) != nil || intent.IntentSha256 != expectedIntentSha256 || intent.Generation != currentGeneration || intent.TargetRoundId != round.RoundId || intent.TargetEpoch != round.Epoch {
		return errors.New("calibration maintenance source identity changed")
	}
	intent.ActivatedAt, intent.AbandonedAt, intent.State = activatedAt, nil, "active"
	digest, err := competitionPreparationIntentDigest(intent)
	if err != nil || digest != expectedIntentSha256 {
		return errors.New("calibration maintenance source digest changed")
	}
	frozen, err := sourceSettingsFromIntent(settings, &intent)
	if err != nil || !storedPolicyMatches(frozen, round.PolicyJson) {
		return errors.New("calibration maintenance frozen source changed")
	}
	_, err = competitionRuntimeManifestContent(frozen, &intent)
	return err
}
