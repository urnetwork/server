// Runtime readiness derives only genuinely activated source changes, then checks the entire frozen policy.
package controller

import (
	"context"
	"errors"

	"github.com/urnetwork/server/model"
)

type competitionAutomationSourceIntentReader interface {
	SourceActivation(context.Context, *Settings, int64) (*model.CompetitionSourceActivation, error)
}

// A season's static credentials and runner pins remain trusted configuration;
// only the exact source identity approved for this frozen round can advance.
func settingsForAutomationRound(ctx context.Context, settings *Settings, store Store, round *roundRecord) (*Settings, error) {
	if round == nil {
		return nil, ErrUnsupportedDecisionMode
	}
	policy, err := decodeRoundPolicySnapshot(round.PolicyJson)
	if err != nil {
		return nil, err
	}
	if policy.AutomationPolicy == nil || !policy.AutomationPolicy.Enabled {
		return nil, ErrUnsupportedDecisionMode
	}
	if policy.AutomationPolicy.Staging != round.Staging || settings.AutomationPolicy.Staging != round.Staging {
		return nil, ErrConflict
	}
	reader, ok := store.(competitionAutomationSourceIntentReader)
	if !ok {
		return nil, errors.New("runtime cannot load the frozen source activation")
	}
	intent, err := reader.SourceActivation(ctx, settings, policy.AutomationPolicy.SourceActivationGeneration)
	if err != nil {
		return nil, err
	}
	if intent == nil || intent.Staging != round.Staging || intent.ActivatedAt == nil || intent.AbandonedAt != nil || intent.TargetRoundId != round.RoundId || intent.TargetEpoch != round.Epoch {
		return nil, ErrConflict
	}
	frozen, err := sourceSettingsFromIntent(settings, intent)
	if err != nil {
		return nil, err
	}
	frozen, err = settingsForFrozenRound(frozen, round)
	if err != nil {
		return nil, err
	}
	if frozen.AutomationPolicy.RequiredApiCapability == model.CompetitionRuntimeBlobCapability {
		if err := LoadCompetitionRuntimeManifestContext(ctx, frozen, intent); err != nil {
			return nil, err
		}
		frozen.runtimeIntentSha256 = intent.IntentSha256
	}
	if competitionIntakeFirstApplies(settings, intent) {
		reader, ok := store.(competitionSourceIntakeReader)
		if !ok {
			return nil, ErrReviewNotReady
		}
		grant, err := reader.SourceIntakeActivation(ctx, settings, intent.Generation)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if grant != nil {
			return settingsForCompetitionApiRound(ctx, settings, store, round)
		}
	}
	return frozen, nil
}

// New-capability reads independently authenticate installation even when
// admission is not involved. Historical contracts retain their prior behavior.
func requireCompetitionRoundRuntime(ctx context.Context, settings *Settings, store Store, round *roundRecord) error {
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

// Season-wide projections require this runtime's current installed source;
// one earlier API role receipt cannot stand in for a late-joining replica.
func requireConfiguredCompetitionRuntime(ctx context.Context, settings *Settings, store Store) error {
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
	// This entrypoint is used by workers. API discovery uses the separate
	// purpose-aware helper and must never relax this activated-source check.
	_, err = settingsForAutomationRound(ctx, settings, store, round)
	return err
}
