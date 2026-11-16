// Intake and grading share one exact frozen source/runtime envelope but do not
// share activation authority. No API-only grant can authorize worker execution.
package controller

import (
	"context"
	"errors"

	"github.com/urnetwork/server/v2026/model"
)

type competitionSourceIntakeReader interface {
	SourceIntakeActivation(context.Context, *Settings, int64) (*model.CompetitionSourceIntakeActivation, error)
}

type competitionSourceLifecycleReader interface {
	SourceLifecycleStatus(context.Context, *Settings, int64) (*CompetitionSourceLifecycleStatus, error)
}

// Historical rows retain strict activated-source derivation. An explicit new
// grant instead authenticates its real API receipt, full frozen rules and fresh
// retained envelope without pretending a worker has acknowledged anything.
func settingsForCompetitionApiRound(ctx context.Context, settings *Settings, store Store, round *roundRecord) (*Settings, error) {
	if settings == nil || settings.IntakeFirstPolicy == nil || round == nil || round.Staging || round.Epoch < settings.IntakeFirstPolicy.FromEpoch {
		return settingsForAutomationRound(ctx, settings, store, round)
	}
	policy, err := decodeRoundPolicySnapshot(round.PolicyJson)
	if err != nil || policy.AutomationPolicy == nil {
		return nil, ErrConflict
	}
	intents, hasIntents := store.(competitionAutomationSourceIntentReader)
	grants, hasGrants := store.(competitionSourceIntakeReader)
	receipts, hasReceipts := store.(interface {
		sourcePreparationReceipts(context.Context, *Settings, int64) ([]model.CompetitionSourceReadiness, error)
	})
	if !hasIntents || !hasGrants || !hasReceipts {
		return nil, errors.New("source-only intake capability is not available")
	}
	generation := policy.AutomationPolicy.SourceActivationGeneration
	grant, err := grants.SourceIntakeActivation(ctx, settings, generation)
	if errors.Is(err, ErrNotFound) {
		// Opting into a future lifecycle cannot reinterpret a historical row.
		return settingsForAutomationRound(ctx, settings, store, round)
	}
	if err != nil {
		return nil, err
	}
	intent, err := intents.SourceActivation(ctx, settings, generation)
	if err != nil {
		return nil, err
	}
	roles, err := receipts.sourcePreparationReceipts(ctx, settings, generation)
	if err != nil {
		return nil, err
	}
	var api *model.CompetitionSourceReadiness
	for index := range roles {
		if roles[index].Role == "api" {
			if api != nil {
				return nil, ErrConflict
			}
			api = &roles[index]
		}
	}
	if api == nil {
		return nil, ErrReviewNotReady
	}
	frozen, err := competitionIntakeFirstActivationMatches(settings, intent, round, *api, grant)
	if err != nil {
		return nil, err
	}
	if err := LoadCompetitionRuntimeManifestContext(ctx, frozen, intent); err != nil {
		return nil, err
	}
	frozen.runtimeIntentSha256 = intent.IntentSha256
	return frozen, nil
}

// Submission screening, owner polling and discovery all preserve source
// authentication even while the independently qualified evaluator is pending.
func requireCompetitionApiRoundRuntime(ctx context.Context, settings *Settings, store Store, round *roundRecord) error {
	if !competitionRoundUsesV2(round) {
		return nil
	}
	policy, err := decodeRoundPolicySnapshot(round.PolicyJson)
	if err != nil {
		return err
	}
	if policy.AutomationPolicy.RequiredApiCapability == model.CompetitionRuntimeBlobCapability {
		_, err = settingsForCompetitionApiRound(ctx, settings, store, round)
	}
	return err
}

// This is API-purpose only; existing worker startup still calls the strict
// requireConfiguredCompetitionRuntime implementation without an intake waiver.
func requireConfiguredCompetitionApiRuntime(ctx context.Context, settings *Settings, store Store) error {
	if !settings.AutomationPolicy.Enabled || settings.AutomationPolicy.RequiredApiCapability != model.CompetitionRuntimeBlobCapability {
		return nil
	}
	round, err := competitionConfiguredCurrentRound(ctx, settings, store)
	if err != nil {
		return err
	}
	if round == nil {
		return ErrReviewNotReady
	}
	return requireCompetitionApiRoundRuntime(ctx, settings, store, round)
}

// Positive source-only status requires the concrete authenticated reader.
// Missing support or a runtime pointer without its immutable R9 control is not
// converted into public grading readiness.
func competitionIntakeFirstGradingReady(ctx context.Context, settings *Settings, store Store) (bool, error) {
	if settings.intakeFirstActivation == nil {
		return true, nil
	}
	reader, ok := store.(competitionSourceLifecycleReader)
	if !ok {
		return false, errors.New("grading-purpose lifecycle authority is unavailable")
	}
	status, err := reader.SourceLifecycleStatus(ctx, settings, settings.intakeFirstActivation.Generation)
	if err != nil {
		return false, err
	}
	return status != nil && status.Schema == 1 && status.Capability == CompetitionSourceLifecycleCapability && status.Purpose == CompetitionSourceLifecycleGrading &&
		status.Intake != nil && *status.Intake == *settings.intakeFirstActivation && status.Round != nil && status.Round.RoundId == status.Intake.TargetRoundId &&
		status.Round.Epoch == status.Intake.TargetEpoch && !status.Round.Canceled && status.ApiAcknowledged && status.WorkerAcknowledged && status.GradingRuntimeActivated &&
		sha256Pattern.MatchString(status.BaselineSha256) && status.HostAdmissionReady && status.GradingReady, nil
}
