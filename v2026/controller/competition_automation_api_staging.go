// Explicit staging uses the season-scoped v2 query; an older store cannot
// silently substitute a production or legacy staging round at readiness.
package controller

import "context"

type competitionAutomationCurrentRoundStore interface {
	CurrentAutomationRound(context.Context, *Settings) (*roundRecord, error)
}

// Keeps legacy production discovery unchanged and requires the optional
// scoped capability only for a configured automated staging season.
func competitionConfiguredCurrentRound(ctx context.Context, settings *Settings, store Store) (*roundRecord, error) {
	if settings == nil {
		return nil, ErrConflict
	}
	if settings.AutomationPolicy.Enabled && settings.AutomationPolicy.Staging {
		reader, ok := store.(competitionAutomationCurrentRoundStore)
		if !ok {
			return nil, ErrUnsupportedDecisionMode
		}
		return reader.CurrentAutomationRound(ctx, settings)
	}
	return store.CurrentRound(ctx, settings)
}
