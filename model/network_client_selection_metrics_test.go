// Pins request-local selection explanations without real target identities.
package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// Reads only the named metric and fixed labels; no provider identifier enters
// diagnostics or a failure message. Absence is zero for the pre-fix control.
func selectionMetricCount(t testing.TB, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			matched := 0
			for _, pair := range metric.Label {
				if want, ok := labels[pair.GetName()]; ok && want == pair.GetValue() {
					matched++
				}
			}
			if matched == len(labels) {
				if metric.Counter != nil {
					return metric.Counter.GetValue()
				}
				return metric.Gauge.GetValue()
			}
		}
	}
	return 0
}

// A deliberately empty specification is not evidence of exhausted discovery.
func TestFindProviders2SelectionExplainsEmptyRequest(t *testing.T) {
	labels := map[string]string{"target_kind": "none", "request_class": "default_minimum", "ip_family": "any", "rank_mode": "quality", "outcome": "zero", "reason": "no_specs"}
	before := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels)
	result, err := FindProviders2(&FindProviders2Args{}, &session.ClientSession{Ctx: context.Background()})
	if err != nil || result == nil || len(result.Providers) != 0 {
		t.Fatalf("empty request changed its existing result: error=%v", err)
	}
	if after := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels); after != before+1 {
		t.Fatalf("empty request attribution count=%g, want %g", after, before+1)
	}
}

// Invalid filters terminate before any cache lookup and must remain errors,
// never manufacture an empty successful response or leak arbitrary labels.
func TestFindProviders2SelectionSeparatesInvalidFilterError(t *testing.T) {
	labels := map[string]string{"target_kind": "none", "request_class": "default_minimum", "ip_family": "unknown", "rank_mode": "quality", "outcome": "error", "reason": "validate"}
	before := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels)
	result, err := FindProviders2(&FindProviders2Args{IpFamily: "synthetic-invalid.example"}, &session.ClientSession{Ctx: context.Background()})
	if err == nil || result != nil {
		t.Fatal("invalid family ceased being a request error")
	}
	if after := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels); after != before+1 {
		t.Fatalf("invalid request attribution count=%g, want %g", after, before+1)
	}
	if active := selectionMetricCount(t, "urnetwork_findproviders2_stage_inflight", map[string]string{"stage": "validate"}); active != 0 {
		t.Fatalf("completed validation retained %g active requests", active)
	}
}

// Request ownership releases the one inflight stage on both cancellation and
// panic. Error text and recovered panic values never enter a metric label.
func TestFindProviders2SelectionStageReleasedOnCanceledAndPanickedExit(t *testing.T) {
	for _, outcome := range []string{"canceled", "error"} {
		ctx, cancel := context.WithCancel(context.Background())
		labels := map[string]string{"target_kind": "none", "request_class": "default_minimum", "ip_family": "any", "rank_mode": "quality", "outcome": outcome, "reason": "directory"}
		before := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels)
		func() {
			defer func() {
				if value := recover(); outcome == "error" && value == nil {
					t.Fatal("panic control did not unwind")
				} else if outcome == "canceled" && value != nil {
					t.Fatal("cancellation unexpectedly panicked")
				}
			}()
			observation := newFindProviders2SelectionObservation(&FindProviders2Args{})
			defer observation.finish(ctx)
			observation.enter("directory")
			if outcome == "error" {
				panic("synthetic-private.example")
			}
			cancel()
		}()
		cancel()
		if after := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels); after != before+1 {
			t.Fatalf("%s exit attribution count=%g, want %g", outcome, after, before+1)
		}
		for _, stage := range []string{"validate", "directory"} {
			if active := selectionMetricCount(t, "urnetwork_findproviders2_stage_inflight", map[string]string{"stage": stage}); active != 0 {
				t.Fatalf("%s exit retained %g active %s requests", outcome, active, stage)
			}
		}
	}
}

// One explicit cache page per mode eliminates random page draw. The selector
// result and the completed-request explanation must agree for each boundary.
func TestFindProviders2SelectionExplainsIntentCacheAndRequestFilters(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, reason := range []string{"returned", "intentional_zero", "cache_empty", "cache_missing", "cache_page_gap", "filtered_hard", "filtered_network", "filtered_family", "filtered_explicit", "filtered_destinations", "filtered_explicit_mixed", "filtered_mixed"} {
			locationId, networkId := server.NewId(), server.NewId()
			candidate := onlineBackfillScore(true, 1)
			args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &locationId}}, Count: 3, ForceCount: true}
			scores := []*ClientScore{candidate}
			requestClass, outcome := "count_positive", "zero"
			switch reason {
			case "returned":
				outcome = "nonempty"
			case "intentional_zero":
				args.Count, requestClass = 0, "count_zero"
			case "cache_empty":
				scores = nil
			case "filtered_network":
				candidate.NetworkOnly = true
			case "filtered_family":
				candidate.IpFamilies = ClientScoreIpFamilyV6
			case "filtered_explicit":
				args.ExcludeClientIds = []server.Id{candidate.ClientId}
			case "filtered_destinations":
				args.ExcludeDestinations = [][]server.Id{{server.NewId(), candidate.ClientId}}
			case "filtered_explicit_mixed":
				args.ExcludeClientIds = []server.Id{candidate.ClientId}
				args.ExcludeDestinations = [][]server.Id{{candidate.ClientId}}
			case "filtered_mixed":
				candidate.NetworkOnly = true
				excluded := onlineBackfillScore(true, 1)
				args.ExcludeClientIds = []server.Id{excluded.ClientId}
				scores = append(scores, excluded)
			}
			if reason != "cache_missing" {
				for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
					writeOnlineBackfillSample(ctx, t, locationId, mode, false, scores)
				}
			}
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.SAdd(ctx, providerHardExclusionsKey, providerHardExclusionsReadyMember).Err())
				if reason == "filtered_hard" {
					server.Raise(r.SAdd(ctx, providerHardExclusionsKey, candidate.ClientId.String()).Err())
				}
				if reason == "cache_page_gap" {
					callerLocationIds := []server.Id{{}}
					for _, id := range countryCodeLocationIds() {
						callerLocationIds = append(callerLocationIds, id)
					}
					for _, callerLocationId := range callerLocationIds {
						for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
							server.Raise(r.Del(ctx, clientScoreLocationSampleKey(false, mode, locationId, callerLocationId, 0)).Err())
						}
					}
				}
			})
			metricReason := reason
			if reason == "returned" {
				metricReason = "returned_small_sample"
			} else if reason == "filtered_explicit" {
				metricReason = "filtered_client_ids"
			}
			labels := map[string]string{"target_kind": "location_unknown", "request_class": requestClass, "ip_family": "any", "rank_mode": "quality", "outcome": outcome, "reason": metricReason}
			before := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels)
			clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(networkId, server.NewId(), "selection-metrics-test", false, false))
			result, err := FindProviders2(args, clientSession)
			if err != nil {
				t.Fatalf("%s unexpectedly failed: %v", reason, err)
			}
			if nonempty := len(result.Providers) != 0; nonempty != (outcome == "nonempty") {
				t.Fatalf("%s result nonempty=%t, want %s", reason, nonempty, outcome)
			}
			if after := selectionMetricCount(t, "urnetwork_findproviders2_selection_outcomes_total", labels); after != before+1 {
				t.Fatalf("%s attribution count=%g, want %g", reason, after, before+1)
			}
		}
	})
}

// Target type is independent of caller country. Old cache schemas and
// arbitrary location-type strings must stay bounded and explicitly unknown.
func TestFindProviders2SelectionTargetKindsAreBounded(t *testing.T) {
	countryId, regionId, cityId, legacyId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	directory := map[server.Id]*locationDirectoryEntry{
		regionId: {LocationType: LocationTypeRegion},
		cityId:   {LocationType: LocationTypeCity},
		legacyId: {LocationType: "synthetic-private.example"},
	}
	for _, testCase := range []struct {
		id   server.Id
		want string
	}{
		{id: countryId, want: "country"}, {id: regionId, want: "region"}, {id: cityId, want: "city"}, {id: legacyId, want: "location_unknown"},
	} {
		args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &testCase.id}}}
		if got := findProviders2TargetKind(args, map[string]server.Id{"xx": countryId}, directory); got != testCase.want {
			t.Fatalf("target kind=%s, want %s", got, testCase.want)
		}
	}
	args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &countryId}, {LocationId: &cityId}}}
	if got := findProviders2TargetKind(args, map[string]server.Id{"xx": countryId}, directory); got != "mixed" {
		t.Fatalf("mixed target kinds=%s", got)
	}
}

// The new directory field round-trips through the existing cache, while an
// old producer remains readable with an unknown type rather than a guessed one.
func TestFindProviders2SelectionDirectoryTypeCompatibility(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, id := t.Context(), server.NewId()
		setLocationDirectoryCache(ctx, map[server.Id]*locationDirectoryEntry{id: {Name: "Synthetic City", CountryCode: "xx", LocationType: LocationTypeCity}}, time.Minute)
		if entries := getLocationDirectoryCache(ctx); entries[id] == nil || entries[id].LocationType != LocationTypeCity {
			t.Fatal("directory cache lost the target type")
		}
		legacy, err := json.Marshal([]*locationDirectoryRow{{LocationId: id, Name: "Synthetic City", CountryCode: "xx"}})
		if err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, locationDirectoryRedisKey, string(legacy), time.Minute).Err())
		})
		if entries := getLocationDirectoryCache(ctx); entries[id] == nil || entries[id].LocationType != "" {
			t.Fatal("legacy directory was rejected or assigned a guessed target type")
		}
	})
}

// Online supply elsewhere cannot satisfy a specific target: alternate-mode
// backfill uses that same target, not a global pool. This is a healthy control
// for the prior inference that global supply makes every empty target a bug.
func TestFindProviders2SelectionBackfillDoesNotWidenTarget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		servedId, emptyId := server.NewId(), server.NewId()
		online := onlineBackfillScore(true, 1)
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			writeOnlineBackfillSample(ctx, t, servedId, mode, false, []*ClientScore{online})
			writeOnlineBackfillSample(ctx, t, emptyId, mode, false, nil)
		}
		if got := egressTestFind(ctx, t, []*ProviderSpec{{LocationId: &servedId}}, RankModeQuality, 1, false, server.NewId()); len(got) != 1 {
			t.Fatal("same-window online supply control failed")
		}
		if got := egressTestFind(ctx, t, []*ProviderSpec{{LocationId: &emptyId}}, RankModeQuality, 1, false, server.NewId()); len(got) != 0 {
			t.Fatal("location-specific request widened to unrelated online supply")
		}
	})
}

// Bounds instrumentation overhead independently of Redis/PostgreSQL latency.
// This models a normal request that visits both primary and alternate stages.
func BenchmarkFindProviders2SelectionObservation(b *testing.B) {
	args := &FindProviders2Args{Count: 20, ForceCount: true}
	b.ReportAllocs()
	for b.Loop() {
		observation := newFindProviders2SelectionObservation(args)
		for _, stage := range []string{"caller_location", "load_primary", "hard_exclusions", "filter", "select", "directory", "select", "load_backfill", "hard_exclusions", "filter", "select", "record_matches"} {
			observation.enter(stage)
		}
		observation.complete(20)
		observation.finish(context.Background())
	}
}
