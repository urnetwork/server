// rank_measured_first through FindProviders2, with the test environment's
// database and Redis: the published pool read as a request reads it. Only
// helpers that predate the setting are used, so the same test runs against
// the code before it.
package model

import (
	"fmt"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
)

// Pushes provider.yml with subscriber policy version 2 and the egress_index
// block's rank_measured_first set as given, keeping the rest of the current
// file, for the rest of the test. The request path reads its settings at
// most once a minute, so its snapshot is dropped.
func pushRankMeasuredFirstForTest(t testing.TB, rankMeasuredFirst bool) {
	t.Helper()
	config := map[string]any{}
	if resource, err := server.Config.SimpleResource(providerConfigResourceName); err == nil && resource != nil {
		parsed, err := resource.ParseE()
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range parsed {
			config[key] = value
		}
	}
	config["subscriber_quality_policy_version"] = 2
	egressIndex := map[string]any{}
	if block, ok := config["egress_index"].(map[string]any); ok {
		for key, value := range block {
			egressIndex[key] = value
		}
	}
	egressIndex["rank_measured_first"] = rankMeasuredFirst
	config["egress_index"] = egressIndex
	encoded, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, encoded))
	requestEgressIndexSettingsSnapshot.Store(nil)
}

// A Quality request whose own bucket and the Speed bucket are empty lends its
// answer from the Online fallback of one published location: three providers
// with URL-probe evidence (a failing ratio, weight 0.55) and score rows, and
// three with neither, which the export gives the neutral URL weight 1 and the
// source query's neutral lookback, weight 1. By the weights alone the three
// without evidence come first. With rank_measured_first on, the measured are
// lent first and the others still fill the answer; off, the order is the one
// before.
func TestFindProviders2OnlineFallbackLendsMeasuredFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		names := map[server.Id]string{}
		measured := []*ClientScore{}
		for i, reliabilityWeight := range []float64{0.9, 0.8, 0.7} {
			score := onlineBackfillScore(true, reliabilityWeight)
			score.UrlProbeSuccessWeight = 0.55
			score.LookbackClientScores = map[int]*ClientScore{
				1: {LookbackIndex: 1, ReliabilityWeight: reliabilityWeight, IndependentReliabilityWeight: 0.99},
				2: {LookbackIndex: 2, ReliabilityWeight: reliabilityWeight, IndependentReliabilityWeight: 0.9},
			}
			names[score.ClientId] = fmt.Sprintf("measured_%d", i)
			measured = append(measured, score)
		}
		unknown := []*ClientScore{}
		for i := range 3 {
			score := onlineBackfillScore(true, 1)
			score.EgressValidUntil = nil
			score.UrlProbeSuccessWeight = 1
			score.LookbackClientScores = map[int]*ClientScore{
				0: {ReliabilityWeight: 1, IndependentReliabilityWeight: 1},
			}
			// distinct speed scores order the full ties of the weights
			score.Scores = map[string]int{RankModeQuality: i + 1, RankModeSpeed: i + 1}
			names[score.ClientId] = fmt.Sprintf("unknown_%d", i)
			unknown = append(unknown, score)
		}
		ids := func(scores ...*ClientScore) []server.Id {
			clientIds := []server.Id{}
			for _, score := range scores {
				clientIds = append(clientIds, score.ClientId)
			}
			return clientIds
		}
		describe := func(clientIds []server.Id) []string {
			described := []string{}
			for _, clientId := range clientIds {
				described = append(described, names[clientId])
			}
			return described
		}

		locationId := server.NewId()
		writeOnlineBackfillSample(ctx, t, locationId, RankModeQuality, false, append(slices.Clone(measured), unknown...))
		writeOnlineBackfillSample(ctx, t, locationId, RankModeSpeed, false, nil)
		find := func(count int) []server.Id {
			providers := egressTestFind(ctx, t, []*ProviderSpec{{LocationId: &locationId}}, RankModeQuality, count, false, server.NewId())
			assertEgressTestNoRepeats(t, providers)
			for _, provider := range providers {
				if provider.Tier != 2*egressTestBackfillOffset() {
					t.Fatalf("%s lent at tier %d, want the Online fallback's %d", names[provider.ClientId], provider.Tier, 2*egressTestBackfillOffset())
				}
			}
			return egressTestIds(providers)
		}

		pushRankMeasuredFirstForTest(t, true)
		if lent := find(3); !slices.Equal(lent, ids(measured...)) {
			t.Fatalf("rank_measured_first on, three places: lent %v, want %v", describe(lent), describe(ids(measured...)))
		}
		everyone := append(slices.Clone(measured), unknown...)
		if lent := find(6); !slices.Equal(lent, ids(everyone...)) {
			t.Fatalf("rank_measured_first on, six places: lent %v, want %v", describe(lent), describe(ids(everyone...)))
		}

		pushRankMeasuredFirstForTest(t, false)
		if lent := find(3); !slices.Equal(lent, ids(unknown...)) {
			t.Fatalf("rank_measured_first off, three places: lent %v, want the order before, %v", describe(lent), describe(ids(unknown...)))
		}
	})
}
