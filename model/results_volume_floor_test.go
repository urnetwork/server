package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/reengagement"
)

// The results endpoints' volume floor holds on each experiment's variant
// totals, the sums the readout compares, not per row. Before the fix a row
// under the floor was dropped on its own, so an arm made of small daily
// cells came back nearly empty: in the 2026-11-13 review the re-engagement
// readout lost 159 of 272 control and 162 of 250 holdout exposures, every
// matured reactivation among them.
func TestReengagementResultsVolumeFloorHoldsOnVariantTotals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		day1 := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
		day2 := day1.Add(24 * time.Hour)
		computedAt := time.Date(2026, 11, 13, 2, 45, 0, 0, time.UTC)
		row := func(day time.Time, experiment string, variant string, platform string, exposures int, reactivations int) *ReengagementResultsRow {
			return &ReengagementResultsRow{
				CohortDay: day, CohortDayStr: day.Format(cohortDayLayout),
				Experiment: experiment, Variant: variant, Stream: reengagement.StreamLapsed, Platform: platform, Tier: "standard",
				Exposures: exposures, Reactivation7d: reactivations, MaturedDays: 9, ComputedAt: computedAt,
			}
		}
		for day, rows := range map[time.Time][]*ReengagementResultsRow{
			day1: {
				// control: 4 + 3 + 4 = 11 exposures, every cell under the floor
				row(day1, "reengage_lapsed_1", "control", "android", 4, 1),
				row(day1, "reengage_lapsed_1", "control", "ios", 3, 1),
				// holdout: 4 + 5 = 9, under the floor in total
				row(day1, "reengage_lapsed_1", "holdout", "android", 4, 0),
				row(day1, "reengage_winback_1", "offer", "web", 12, 0),
			},
			day2: {
				row(day2, "reengage_lapsed_1", "control", "android", 4, 0),
				row(day2, "reengage_lapsed_1", "holdout", "android", 5, 1),
			},
		} {
			if err := ReplaceReengagementResults(ctx, day, day, rows, computedAt); err != nil {
				t.Fatal(err)
			}
		}
		const floor = 10
		keys := func(rows []*ReengagementResultsRow) string {
			out := []string{}
			for _, r := range rows {
				out = append(out, r.CohortDayStr+"/"+r.Experiment+"/"+r.Variant+"/"+r.Platform)
			}
			return strings.Join(out, " ")
		}

		// the control arm comes back whole, its cells under the floor
		// included; the holdout arm, under the floor in total, not at all
		rows, next := ListReengagementResults(ctx, ReengagementResultsFilter{
			Experiment: "reengage_lapsed_1", From: day1, To: day2, MinExposures: floor, Limit: 100,
		})
		want := "2026-11-04/reengage_lapsed_1/control/android 2026-11-04/reengage_lapsed_1/control/ios 2026-11-05/reengage_lapsed_1/control/android"
		if got := keys(rows); got != want || next != nil {
			t.Fatalf("rows = %s (next %v), want %s", got, next, want)
		}
		reactivations := 0
		for _, r := range rows {
			reactivations += r.Reactivation7d
		}
		if reactivations != 2 {
			t.Fatalf("control reactivations = %d, want 2", reactivations)
		}

		// every page holds the floor on the whole selection, not on what is
		// left after the cursor
		paged := []*ReengagementResultsRow{}
		filter := ReengagementResultsFilter{Experiment: "reengage_lapsed_1", From: day1, To: day2, MinExposures: floor, Limit: 1}
		for page := 0; ; page += 1 {
			rows, next := ListReengagementResults(ctx, filter)
			paged = append(paged, rows...)
			if next == nil {
				break
			}
			if 3 < page {
				t.Fatalf("page %d still has a next cursor", page)
			}
			filter.After = next
		}
		if got := keys(paged); got != want {
			t.Fatalf("paged rows = %s, want %s", got, want)
		}

		// the totals are the filtered selection's: on android alone the
		// control arm has 8 exposures, under the floor
		rows, _ = ListReengagementResults(ctx, ReengagementResultsFilter{
			Experiment: "reengage_lapsed_1", From: day1, To: day2, Platform: "android", MinExposures: floor, Limit: 100,
		})
		if len(rows) != 0 {
			t.Fatalf("android rows = %s, want none", keys(rows))
		}

		// and the cohort days': day 1 alone leaves control at 7
		rows, _ = ListReengagementResults(ctx, ReengagementResultsFilter{
			From: day1, To: day1, MinExposures: floor, Limit: 100,
		})
		if got := keys(rows); got != "2026-11-04/reengage_winback_1/offer/web" {
			t.Fatalf("day 1 rows = %s, want only the winback offer arm", got)
		}

		// without a floor every row comes back
		rows, _ = ListReengagementResults(ctx, ReengagementResultsFilter{From: day1, To: day2, Limit: 100})
		if len(rows) != 6 {
			t.Fatalf("unfloored rows = %s, want 6", keys(rows))
		}
	})
}

// The onboarding endpoint holds the same floor on its variant totals.
func TestOnboardingResultsVolumeFloorHoldsOnVariantTotals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		day1 := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
		day2 := day1.Add(24 * time.Hour)
		computedAt := time.Date(2026, 12, 3, 2, 0, 0, 0, time.UTC)
		row := func(day time.Time, variant string, platform string, path string, exposures int, connected int) *OnboardingResultsRow {
			return &OnboardingResultsRow{
				CohortDay: day, CohortDayStr: day.Format(cohortDayLayout),
				Experiment: "email_sequence_2", Variant: variant, Surface: ExperimentSurfaceEmailSequence,
				Platform: platform, Tier: "standard", Path: path,
				Exposures: exposures, Connect7d: connected, MaturedDays: onboarding.MaturedDays(day, computedAt), ComputedAt: computedAt,
			}
		}
		for day, rows := range map[time.Time][]*OnboardingResultsRow{
			day1: {
				// control: 6 + 5 = 11, both cells under the floor
				row(day1, "control", "android", "b", 6, 4),
				// holdout: 3 + 2 = 5
				row(day1, "holdout", "android", "b", 3, 1),
			},
			day2: {
				row(day2, "control", "ios", "a", 5, 3),
				row(day2, "holdout", "ios", "a", 2, 2),
			},
		} {
			if err := ReplaceOnboardingResults(ctx, day, day, rows, computedAt); err != nil {
				t.Fatal(err)
			}
		}
		keys := func(rows []*OnboardingResultsRow) string {
			out := []string{}
			for _, r := range rows {
				out = append(out, r.CohortDayStr+"/"+r.Variant+"/"+r.Platform)
			}
			return strings.Join(out, " ")
		}
		want := "2026-11-24/control/android 2026-11-25/control/ios"

		rows, next := ListOnboardingResults(ctx, OnboardingResultsFilter{
			Experiment: "email_sequence_2", From: day1, To: day2, MinExposures: 10, Limit: 100,
		})
		if got := keys(rows); got != want || next != nil {
			t.Fatalf("rows = %s (next %v), want %s", got, next, want)
		}

		paged := []*OnboardingResultsRow{}
		filter := OnboardingResultsFilter{Experiment: "email_sequence_2", From: day1, To: day2, MinExposures: 10, Limit: 1}
		for page := 0; ; page += 1 {
			rows, next := ListOnboardingResults(ctx, filter)
			paged = append(paged, rows...)
			if next == nil {
				break
			}
			if 2 < page {
				t.Fatalf("page %d still has a next cursor", page)
			}
			filter.After = next
		}
		if got := keys(paged); got != want {
			t.Fatalf("paged rows = %s, want %s", got, want)
		}

		rows, _ = ListOnboardingResults(ctx, OnboardingResultsFilter{
			Experiment: "email_sequence_2", From: day1, To: day2, Platform: "ios", MinExposures: 10, Limit: 100,
		})
		if len(rows) != 0 {
			t.Fatalf("ios rows = %s, want none", keys(rows))
		}
	})
}
