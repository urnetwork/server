package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/onboarding"
)

// TestOnboardingResultsPurchaseStarted follows purchase_started_7d from the
// events to the results endpoint's rows: the cohort facts carry a network's
// first purchase.started, the outcome counts it inside the 7 days after the
// cohort time, and the count round-trips through onboarding_results_daily and
// is replaced, not added to, when the rollup recomputes the day.
func TestOnboardingResultsPurchaseStarted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cohortAt := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
		day := onboarding.CohortDay(cohortAt)
		now := cohortAt.Add(8 * 24 * time.Hour)

		offerArm := &NetworkOnboarding{NetworkId: server.NewId(), CreatedAt: cohortAt, Platform: "android", Path: "a"}
		holdout := &NetworkOnboarding{NetworkId: server.NewId(), CreatedAt: cohortAt, Platform: "android", Path: "b"}
		late := &NetworkOnboarding{NetworkId: server.NewId(), CreatedAt: cohortAt, Platform: "android", Path: "b"}
		never := &NetworkOnboarding{NetworkId: server.NewId(), CreatedAt: cohortAt, Platform: "android", Path: "b"}

		started := func(networkId server.Id, at time.Time) *OnboardingEvent {
			return &OnboardingEvent{NetworkId: networkId, Name: EventPurchaseStarted, At: at, ReceivedAt: at, Platform: "android", Props: map[string]any{
				"store": OnboardingStorePlay, "plan": PlanYearly, "trial": true,
			}}
		}
		connect.AssertEqual(t, nil, AddOnboardingEvents(ctx, []*OnboardingEvent{
			// the offer screen's purchase, retried a day later
			started(offerArm.NetworkId, cohortAt.Add(2*time.Minute)),
			started(offerArm.NetworkId, cohortAt.Add(26*time.Hour)),
			// the regular plan picker, on day 5
			started(holdout.NetworkId, cohortAt.Add(5*24*time.Hour)),
			// after the window
			started(late.NetworkId, cohortAt.Add(7*24*time.Hour+time.Hour)),
		}))

		cohort := []*NetworkOnboarding{offerArm, holdout, late, never}
		facts := LoadOnboardingCohortFacts(ctx, cohort)
		connect.AssertEqual(t, true, facts[offerArm.NetworkId].FirstEventAt[EventPurchaseStarted].Equal(cohortAt.Add(2*time.Minute)))
		_, ok := facts[never.NetworkId].FirstEventAt[EventPurchaseStarted]
		connect.AssertEqual(t, false, ok)

		row := &OnboardingResultsRow{
			CohortDay:    day,
			CohortDayStr: day.Format(cohortDayLayout),
			Experiment:   "offer_screen_2",
			Variant:      "control",
			Surface:      ExperimentSurfaceOfferInApp,
			Platform:     "android",
			Tier:         "unknown",
			Path:         "a",
			MaturedDays:  onboarding.MaturedDays(day, now),
		}
		for _, network := range cohort {
			row.Add(onboarding.ComputeOutcomes(*facts[network.NetworkId], now))
		}
		connect.AssertEqual(t, 4, row.Exposures)
		connect.AssertEqual(t, 2, row.PurchaseStarted7d)
		connect.AssertEqual(t, 0, row.ProStart14d)

		connect.AssertEqual(t, nil, ReplaceOnboardingResults(ctx, day, day, []*OnboardingResultsRow{row}, now))
		listed, next := ListOnboardingResults(ctx, OnboardingResultsFilter{Experiment: "offer_screen_2", From: day, To: day, Limit: 10})
		connect.AssertEqual(t, true, next == nil)
		connect.AssertEqual(t, 1, len(listed))
		connect.AssertEqual(t, 4, listed[0].Exposures)
		connect.AssertEqual(t, 2, listed[0].PurchaseStarted7d)
		connect.AssertEqual(t, 8, listed[0].MaturedDays)

		// a recompute replaces the day's rows
		row.PurchaseStarted7d = 3
		connect.AssertEqual(t, nil, ReplaceOnboardingResults(ctx, day, day, []*OnboardingResultsRow{row}, now.Add(24*time.Hour)))
		guardrailRows := ListOnboardingResultsForGuardrails(ctx, "offer_screen_2", day, day)
		connect.AssertEqual(t, 1, len(guardrailRows))
		connect.AssertEqual(t, 3, guardrailRows[0].PurchaseStarted7d)
	})
}
