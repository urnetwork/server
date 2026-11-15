// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
)

// The rollup writes every experiment's rows once more under the sign-up
// cohort, "<experiment>@<cohort>" (owner, 2026-11-15: decide on the likely
// human cohort): the request's label from enrollment, outranked by a burst,
// and for a sign-up from before the labels, a browser without a language is
// likely bot and anything else unknown. The experiment's own rows still count
// everyone.
func TestBuildOnboardingResultsRowsByCohort(t *testing.T) {
	experiment := &model.OnboardingExperiment{
		Id:         "email_sequence_test",
		Surface:    model.ExperimentSurfaceEmailSequence,
		Status:     model.ExperimentStatusRunning,
		Allocation: map[string]float64{"control": 100},
		Variants:   map[string]map[string]any{"control": {"template": "default"}},
	}
	config := &model.OnboardingConfig{Experiments: []*model.OnboardingExperiment{experiment}}
	cohortAt := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	now := cohortAt.Add(9 * 24 * time.Hour)

	cohort := []*model.NetworkOnboarding{}
	facts := map[server.Id]*onboarding.NetworkFacts{}
	add := func(n int, minuteStep time.Duration, platform string, locale string, country string, event string) {
		for i := 0; i < n; i++ {
			at := cohortAt.Add(time.Duration(i) * minuteStep)
			row := &model.NetworkOnboarding{NetworkId: server.NewId(), CreatedAt: at, Email: true, Platform: platform, Locale: locale, Country: country, Path: "b"}
			cohort = append(cohort, row)
			f := &onboarding.NetworkFacts{CohortAt: at, FirstEventAt: map[string]time.Time{}}
			if event != "" {
				f.FirstEventAt[event] = at
			}
			facts[row.NetworkId] = f
		}
	}
	// 3 likely human, spread over hours: no burst
	add(3, time.Hour, "ios", "", "US", model.EventSignupLikelyHuman)
	// 2 likely bot by request
	add(2, time.Hour, "android", "zh-CN", "US", model.EventSignupLikelyBot)
	// a burst of 10 in one hour, labeled human by request: all likely bot
	add(10, time.Minute, "ios", "", "JP", model.EventSignupLikelyHuman)
	// before the labels: a browser without a language, and an app
	add(1, time.Minute, onboarding.PlatformWeb, "", "DE", "")
	add(1, time.Minute, "android", "pt-BR", "BR", "")

	exposures := map[string]int{}
	for _, r := range buildOnboardingResultsRows(config, cohort, facts, now) {
		exposures[r.Experiment] += r.Exposures
	}
	connect.AssertEqual(t, 17, exposures[experiment.Id])
	connect.AssertEqual(t, 3, exposures[onboarding.CohortExperimentId(experiment.Id, onboarding.CohortHuman)])
	connect.AssertEqual(t, 13, exposures[onboarding.CohortExperimentId(experiment.Id, onboarding.CohortBot)])
	connect.AssertEqual(t, 1, exposures[onboarding.CohortExperimentId(experiment.Id, onboarding.CohortUnknown)])
	connect.AssertEqual(t, 17, exposures[ResultsExperimentAll])
	connect.AssertEqual(t, 3, exposures[onboarding.CohortExperimentId(ResultsExperimentAll, onboarding.CohortHuman)])
}

// The offer email experiment (surface offer.email) counts only the networks
// it reached, by the exposure event of their first offer email in the card
// branch, whenever they signed up: a network that signed up before the
// experiment's start but got its offer email after is in, and one never sent
// an offer email (or only a store-billed one) is out.
func TestBuildOnboardingResultsRowsOfferEmailExposure(t *testing.T) {
	experiment := &model.OnboardingExperiment{
		Id:         "offer_email_test",
		Surface:    model.ExperimentSurfaceOfferEmail,
		Status:     model.ExperimentStatusRunning,
		Start:      "2026-11-24",
		Allocation: map[string]float64{"control": 50, "crypto": 50},
		Variants:   map[string]map[string]any{"control": {"copy": "current"}, "crypto": {"copy": "email_offer_crypto"}},
	}
	config := &model.OnboardingConfig{Experiments: []*model.OnboardingExperiment{experiment}}
	signedUp := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	now := signedUp.Add(20 * 24 * time.Hour)
	cohort := []*model.NetworkOnboarding{}
	facts := map[server.Id]*onboarding.NetworkFacts{}
	exposed := 0
	for i := 0; i < 8; i++ {
		row := &model.NetworkOnboarding{NetworkId: server.NewId(), CreatedAt: signedUp.Add(time.Duration(i) * time.Hour), Email: true, Platform: onboarding.PlatformWeb, Locale: "en-US", Path: "b"}
		cohort = append(cohort, row)
		f := &onboarding.NetworkFacts{CohortAt: row.CreatedAt, FirstEventAt: map[string]time.Time{model.EventSignupLikelyHuman: row.CreatedAt}}
		if i%2 == 0 {
			// E4b on day 7, after the start
			f.FirstEventAt[model.EventOfferEmailExposed] = row.CreatedAt.Add(7 * 24 * time.Hour)
			exposed += 1
		}
		facts[row.NetworkId] = f
	}
	exposures := map[string]int{}
	for _, r := range buildOnboardingResultsRows(config, cohort, facts, now) {
		exposures[r.Experiment] += r.Exposures
	}
	connect.AssertEqual(t, exposed, exposures[experiment.Id])
	connect.AssertEqual(t, exposed, exposures[onboarding.CohortExperimentId(experiment.Id, onboarding.CohortHuman)])

	// a draft never counts, exposure event or not
	experiment.Status = model.ExperimentStatusDraft
	exposures = map[string]int{}
	for _, r := range buildOnboardingResultsRows(config, cohort, facts, now) {
		exposures[r.Experiment] += r.Exposures
	}
	connect.AssertEqual(t, 0, exposures[experiment.Id])
}
