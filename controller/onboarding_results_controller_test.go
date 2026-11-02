package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
)

// TestBuildOnboardingResultsRowsPurchaseStarted counts purchase_started_7d per
// variant of an in-app offer experiment. Exposure is the registry assignment,
// so the holdout, which can start a purchase only from the regular plan
// picker, counts the same way as the offer arm; a cohort younger than the
// window counts nothing yet; and the row carries the count to the results
// endpoint as purchase_started_7d.
func TestBuildOnboardingResultsRowsPurchaseStarted(t *testing.T) {
	experiment := &model.OnboardingExperiment{
		Id:         "offer_screen_test",
		Surface:    model.ExperimentSurfaceOfferInApp,
		Status:     model.ExperimentStatusRunning,
		Allocation: map[string]float64{"control": 50, "holdout": 50},
		Variants:   map[string]map[string]any{"control": {"copy": "offer_copy_v1"}, "holdout": {"copy": "none"}},
	}
	config := &model.OnboardingConfig{Experiments: []*model.OnboardingExperiment{experiment}}
	cohortAt := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)

	networkIn := func(variant string) server.Id {
		for {
			networkId := server.NewId()
			if experiment.Assign(networkId) == variant {
				return networkId
			}
		}
	}
	cohort := []*model.NetworkOnboarding{}
	facts := map[server.Id]*onboarding.NetworkFacts{}
	// per arm: a start an hour in, a start after the window, and no start
	const noStart = time.Duration(-1)
	for _, variant := range []string{"control", "holdout"} {
		for _, startedAfter := range []time.Duration{time.Hour, 8 * 24 * time.Hour, noStart} {
			row := &model.NetworkOnboarding{NetworkId: networkIn(variant), CreatedAt: cohortAt, Platform: "android", Path: "a"}
			cohort = append(cohort, row)
			f := &onboarding.NetworkFacts{CohortAt: cohortAt, FirstEventAt: map[string]time.Time{}}
			if startedAfter != noStart {
				f.FirstEventAt[onboarding.EventPurchaseStarted] = cohortAt.Add(startedAfter)
			}
			facts[row.NetworkId] = f
		}
	}

	countsAt := func(now time.Time) map[string]*model.OnboardingResultsRow {
		byVariant := map[string]*model.OnboardingResultsRow{}
		for _, r := range buildOnboardingResultsRows(config, cohort, facts, now) {
			byVariant[r.Experiment+"/"+r.Variant] = r
		}
		connect.AssertEqual(t, 3, len(byVariant))
		return byVariant
	}

	matured := countsAt(cohortAt.Add(9 * 24 * time.Hour))
	connect.AssertEqual(t, 6, matured[ResultsExperimentAll+"/"+ResultsVariantAll].Exposures)
	connect.AssertEqual(t, 2, matured[ResultsExperimentAll+"/"+ResultsVariantAll].PurchaseStarted7d)
	for _, variant := range []string{"control", "holdout"} {
		r := matured[experiment.Id+"/"+variant]
		connect.AssertEqual(t, 3, r.Exposures)
		connect.AssertEqual(t, 1, r.PurchaseStarted7d)
		connect.AssertEqual(t, 9, r.MaturedDays)
	}

	young := countsAt(cohortAt.Add(3 * 24 * time.Hour))
	for _, r := range young {
		connect.AssertEqual(t, 0, r.PurchaseStarted7d)
		connect.AssertEqual(t, 3, r.MaturedDays)
	}

	b, err := json.Marshal(matured[experiment.Id+"/control"])
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, true, strings.Contains(string(b), `"purchase_started_7d":1`))
}
