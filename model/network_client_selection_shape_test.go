// Small-list telemetry must preserve the real selector's request and filter
// boundaries without exposing requested destinations or candidate identities.
package model

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Two independent cache pages may contain the same candidates. Each case
// exercises FindProviders2, not a separately reconstructed selection result.
func TestFindProviders2SelectionSmallResultBoundaries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, testCase := range []struct {
			boundary string
			reason   string
		}{
			{boundary: "sample", reason: "returned_small_sample"},
			{boundary: "positive_count", reason: "returned_small_sample"},
			{boundary: "unused_exclusions", reason: "returned_small_sample"},
			{boundary: "requested", reason: "returned_small_requested"},
			{boundary: "client_ids", reason: "returned_small_filtered_client_ids"},
			{boundary: "destinations", reason: "returned_small_filtered_destinations"},
			{boundary: "both", reason: "returned_small_filtered_explicit_mixed"},
			{boundary: "hard", reason: "returned_small_filtered_hard"},
			{boundary: "network", reason: "returned_small_filtered_network"},
			{boundary: "family", reason: "returned_small_filtered_family"},
			{boundary: "mixed", reason: "returned_small_filtered_mixed"},
			{boundary: "cache_missing", reason: "returned_small_cache_unknown"},
			{boundary: "cache_page_gap", reason: "returned_small_cache_unknown"},
			{boundary: "backfill_error", reason: "returned_small_cache_unknown"},
			{boundary: "mixed_direct", reason: "returned_small_mixed_direct"},
		} {
			locationId, networkId := server.NewId(), server.NewId()
			args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &locationId}}}
			scores := []*ClientScore{onlineBackfillScore(true, 1), onlineBackfillScore(true, 1)}
			requestClass := "default_minimum"
			if testCase.boundary == "requested" || testCase.boundary == "client_ids" || testCase.boundary == "destinations" || testCase.boundary == "both" || testCase.boundary == "hard" || testCase.boundary == "network" || testCase.boundary == "family" || testCase.boundary == "mixed" {
				scores = append(scores, onlineBackfillScore(true, 1), onlineBackfillScore(true, 1))
			}
			switch testCase.boundary {
			case "requested":
				args.Count, args.ForceCount, requestClass = 2, true, "count_small"
			case "positive_count":
				args.Count, args.ForceCount, requestClass = 3, true, "count_positive"
			case "unused_exclusions":
				args.ExcludeClientIds = []server.Id{server.NewId()}
				args.ExcludeDestinations = [][]server.Id{{server.NewId()}}
			case "client_ids", "both":
				args.ExcludeClientIds = []server.Id{scores[2].ClientId, scores[3].ClientId}
			case "network":
				scores[2].NetworkOnly, scores[3].NetworkOnly = true, true
			case "family":
				scores[2].IpFamilies, scores[3].IpFamilies = ClientScoreIpFamilyV6, ClientScoreIpFamilyV6
			case "mixed":
				scores[2].NetworkOnly = true
				args.ExcludeClientIds = []server.Id{scores[3].ClientId}
			case "mixed_direct":
				scores = scores[:1]
				directId := server.NewId()
				writeSubscriberFactsForScores(ctx, []*ClientScore{{ClientId: directId}})
				args.Specs = append(args.Specs, &ProviderSpec{ClientId: &directId})
			}
			if testCase.boundary == "destinations" || testCase.boundary == "both" {
				args.ExcludeDestinations = [][]server.Id{{server.NewId(), scores[2].ClientId}, {scores[3].ClientId}}
			}
			writeOnlineBackfillSample(ctx, t, locationId, RankModeQuality, false, scores)
			if testCase.boundary != "cache_missing" {
				writeOnlineBackfillSample(ctx, t, locationId, RankModeSpeed, false, scores)
			}
			if testCase.boundary == "cache_page_gap" || testCase.boundary == "backfill_error" {
				callerLocationIds := []server.Id{{}}
				for _, id := range countryCodeLocationIds() {
					callerLocationIds = append(callerLocationIds, id)
				}
				server.Redis(ctx, func(r server.RedisClient) {
					for _, callerLocationId := range callerLocationIds {
						if testCase.boundary == "cache_page_gap" {
							server.Raise(r.Del(ctx, clientScoreLocationSampleKey(false, RankModeSpeed, locationId, callerLocationId, 0)).Err())
						} else {
							key := clientScoreLocationCountsKey(false, RankModeSpeed, locationId, callerLocationId)
							server.Raise(r.Del(ctx, key).Err())
							server.Raise(r.LPush(ctx, key, "synthetic-incompatible-cache").Err())
						}
					}
				})
			}
			if testCase.boundary == "hard" {
				server.Redis(ctx, func(r server.RedisClient) {
					server.Raise(r.SAdd(ctx, providerHardExclusionsKey, scores[2].ClientId.String(), scores[3].ClientId.String()).Err())
				})
			}
			targetKind := "location_unknown"
			if testCase.boundary == "mixed_direct" {
				targetKind = "mixed"
			}
			labels := map[string]string{"target_kind": targetKind, "request_class": requestClass, "ip_family": "any", "rank_mode": "quality", "outcome": "nonempty", "reason": testCase.reason}
			before := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels)
			clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(networkId, server.NewId(), "selection-shape-test", false, false))
			result, err := FindProviders2(args, clientSession)
			if err != nil || result == nil || len(result.Providers) != 2 {
				t.Fatalf("%s did not preserve the expected two-provider response: error=%v", testCase.boundary, err)
			}
			if after := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels); after != before+1 {
				t.Fatalf("%s small-result attribution count=%g, want %g", testCase.boundary, after, before+1)
			}
		}
	})
}

// Returned-count bands remain finite; a direct response never claims that the
// discovery pool met a count request or had any eligible candidates.
func TestFindProviders2SelectionResultBandsAndDirectIntent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, testCase := range []struct {
			count  int
			direct bool
			reason string
		}{
			{count: 1, direct: true, reason: "returned_small_direct"},
			{count: 2, direct: true, reason: "returned_small_direct"},
			{count: 3, reason: "returned_3_9"},
			{count: 10, reason: "returned_10_plus"},
		} {
			locationId := server.NewId()
			args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &locationId}}}
			scores := []*ClientScore{}
			for range testCase.count {
				scores = append(scores, onlineBackfillScore(true, 1))
			}
			targetKind := "location_unknown"
			if testCase.direct {
				targetKind, args.Specs = "direct", nil
				for _, score := range scores {
					args.Specs = append(args.Specs, &ProviderSpec{ClientId: &score.ClientId})
				}
			}
			for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
				writeOnlineBackfillSample(ctx, t, locationId, rankMode, false, scores)
			}
			labels := map[string]string{"target_kind": targetKind, "request_class": "default_minimum", "ip_family": "any", "rank_mode": "quality", "outcome": "nonempty", "reason": testCase.reason}
			before := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels)
			clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "selection-band-test", false, false))
			result, err := FindProviders2(args, clientSession)
			if err != nil || result == nil || len(result.Providers) != testCase.count {
				t.Fatalf("result band changed response count=%d direct=%t: error=%v", testCase.count, testCase.direct, err)
			}
			if after := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels); after != before+1 {
				t.Fatalf("result band %s count=%g, want %g", testCase.reason, after, before+1)
			}
		}
	})
}
