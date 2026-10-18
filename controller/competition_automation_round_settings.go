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
	return settingsForFrozenRound(frozen, round)
}
