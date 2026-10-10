// rank_mode "online": every online provider, past the common gates and the
// request's own filters, without subscriber validation, minimums or borrowing.
package model

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Subscriber work so far: SQL batches, and candidates the negative cache was
// asked about (every candidate of a lookup is a hit, a miss or a bypass).
type onlineTestSubscriberReads struct {
	batches    float64
	candidates float64
}

func onlineTestSubscriberReadsNow() onlineTestSubscriberReads {
	return onlineTestSubscriberReads{
		batches: testutil.ToFloat64(subscriberEligibilityEventCounters["sql_batch"]),
		candidates: testutil.ToFloat64(subscriberEligibilityEventCounters["negative_hit"]) +
			testutil.ToFloat64(subscriberEligibilityEventCounters["negative_miss"]) +
			testutil.ToFloat64(subscriberEligibilityEventCounters["capacity_bypass"]),
	}
}

// A fresh negative cache that reports its events, so no earlier refusal can
// hide a candidate read.
func onlineTestFreshSubscriberCache(t testing.TB) {
	t.Helper()
	previous := providerSubscriberNegativeCache
	providerSubscriberNegativeCache = newSubscriberNegativeCache(subscriberNegativeCapacity, time.Now)
	providerSubscriberNegativeCache.observe = observeSubscriberEligibilityEvent
	t.Cleanup(func() { providerSubscriberNegativeCache = previous })
}

// Client ids in a fixed order, to compare an answer as a set.
func onlineTestSorted(clientIds ...server.Id) []server.Id {
	sorted := slices.Clone(clientIds)
	slices.SortFunc(sorted, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return sorted
}

// Counter values, or histogram sample counts, summed over every series whose
// labels include `labels`.
func onlineTestMetric(t testing.TB, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			values := map[string]string{}
			for _, pair := range metric.Label {
				values[pair.GetName()] = pair.GetValue()
			}
			matched := true
			for label, value := range labels {
				matched = matched && values[label] == value
			}
			switch {
			case !matched:
			case metric.Counter != nil:
				total += metric.Counter.GetValue()
			case metric.Histogram != nil:
				total += float64(metric.Histogram.GetSampleCount())
			}
		}
	}
	return total
}

// Real connections, measured history and publication. An online request
// answers every online provider -- native Quality and Speed members and those
// in neither bucket -- and nothing a common gate, the target or a request
// filter removes. Forced or not, under either reader, it reads no subscriber
// fact, while Quality on the same fixture stays strict.
func TestFindProviders2OnlineAnswersEveryOnlineProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		city := egressTestCity(ctx, "Online City", "Online Region", "United States", "us")
		elsewhere := egressTestCity(ctx, "Other Online City", "Online Region", "United States", "us")
		// native Quality and Speed: a verified subscriber with a passing ratio
		quality := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		// native Speed only: a passing ratio and no subscriber fact
		speed := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{})
		// in neither bucket: a failing 2/3 ratio past the speed cutoff, and
		// no URL evidence or client sample at all
		failing := egressTestConnect(ctx, t, city, egressTestSlow, nil, &ConnectionLocationScores{})
		unprobed := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, &ConnectionLocationScores{})
		// the common gates
		risky := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{ArinRisk: true})
		intercepted := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		unreliable := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		// network-only, and another target
		private := egressTestConnect(ctx, t, city, egressTestFast, map[ProvideMode][]byte{ProvideModeNetwork: []byte("synthetic-private")}, nil)
		otherTarget := egressTestConnect(ctx, t, elsewhere, egressTestFast, nil, nil)
		for _, provider := range []*egressTestProvider{quality, speed, risky, intercepted, unreliable, private, otherTarget} {
			egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 0)
		}
		egressTestHealth(ctx, failing.clientId, server.NowUtc(), 3, 1)
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{ClientId: intercepted.clientId, MeasuredAt: server.NowUtc(), TLSAuthenticationFailure: true})
		egressTestReliability(ctx, unreliable.clientId, 1, 0, 0)
		egressTestPasses(ctx, t)

		// The buckets as the exporter published them: two of the online
		// providers pass no minimum, and only one is a Quality member.
		published := egressTestCachedScores(ctx, t, city, RankModeQuality, false)
		for _, member := range []struct {
			provider       *egressTestProvider
			quality, speed bool
		}{
			{quality, true, true},
			{speed, false, true},
			{failing, false, false},
			{unprobed, false, false},
		} {
			score := published[member.provider.clientId]
			if score == nil || !score.Online || score.PassesMinimums[RankModeQuality] != member.quality || score.PassesMinimums[RankModeSpeed] != member.speed {
				t.Fatal("fixture bucket membership changed")
			}
		}

		online := []server.Id{quality.clientId, speed.clientId, failing.clientId, unprobed.clientId}
		for _, nativeReader := range []bool{false, true} {
			func() {
				pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(fmt.Sprintf("subscriber_quality_policy_version: 2\negress_index:\n  native_reader_enabled: %t\n", nativeReader)))
				defer pop()
				requestEgressIndexSettingsSnapshot.Store(nil)
				defer requestEgressIndexSettingsSnapshot.Store(nil)
				for _, force := range []bool{false, true} {
					before := onlineTestSubscriberReadsNow()
					providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 20, force, server.NewId())
					if after := onlineTestSubscriberReadsNow(); after != before {
						t.Fatalf("reader=%t force=%t: online read subscriber facts: %+v -> %+v", nativeReader, force, before, after)
					}
					assertEgressTestNoRepeats(t, providers)
					if !slices.Equal(onlineTestSorted(egressTestIds(providers)...), onlineTestSorted(online...)) {
						t.Fatalf("reader=%t force=%t: online answered %d providers, want the %d online providers", nativeReader, force, len(providers), len(online))
					}
					for _, provider := range providers {
						if provider.Tier != 0 {
							t.Fatalf("reader=%t force=%t: online answered tier %d, want one uniform tier", nativeReader, force, provider.Tier)
						}
					}
				}
			}()
		}

		// The caller's own network-only provider is online supply to it.
		ownNetwork := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 20, false, private.networkId)
		if !slices.Equal(onlineTestSorted(egressTestIds(ownNetwork)...), onlineTestSorted(append(slices.Clone(online), private.clientId)...)) {
			t.Fatal("online lost the caller's own network-only provider")
		}

		// Explicit and final-destination exclusions apply as in every mode.
		for _, force := range []bool{false, true} {
			clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "online-test", false, false))
			result, err := FindProviders2(&FindProviders2Args{
				Specs: egressTestLocationSpec(city), RankMode: RankModeOnline, Count: 20, ForceCount: true, ForceMinimum: force,
				ExcludeClientIds: []server.Id{quality.clientId}, ExcludeDestinations: [][]server.Id{{unprobed.clientId}},
			}, clientSession)
			if err != nil || result == nil || !slices.Equal(onlineTestSorted(egressTestIds(result.Providers)...), onlineTestSorted(speed.clientId, failing.clientId)) {
				t.Fatalf("force=%t: online bypassed explicit request exclusions", force)
			}
		}

		// Quality on the same fixture still validates current subscriber
		// facts, and the counters above see it do so.
		before := onlineTestSubscriberReadsNow()
		strict := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, 20, true, server.NewId())
		if !slices.Equal(egressTestIds(strict), []server.Id{quality.clientId}) {
			t.Fatal("forced Quality returned unverified access")
		}
		if after := onlineTestSubscriberReadsNow(); after.batches <= before.batches || after.candidates <= before.candidates {
			t.Fatal("subscriber counters did not observe forced Quality validation")
		}
	})
}

// The SN25 validator's seed request asks for the online bucket: best
// available, count 8, its own client excluded, force_minimum. Most of the
// pool fails current subscriber facts, which the same request in Quality must
// refuse; online answers a full block from all of it without one subscriber
// read and never answers the validator itself.
func TestFindProviders2OnlineValidatorSeedRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		egressTestCity(ctx, "Online Seed City", "Online Seed Region", "United States", "us")
		resetCountryCodeLocationIds()
		location := countryCodeLocationIds()["us"]
		if location == (server.Id{}) {
			t.Fatal("fixture requires the real US best-available target")
		}
		const loaded, verifiedCount = 1000, 10
		scores := []*ClientScore{}
		pool := map[server.Id]bool{}
		verified := map[server.Id]bool{}
		unverified := []server.Id{}
		for i := range loaded {
			// online, mostly passing no native minimum
			score := onlineBackfillScore(true, 1)
			if i%5 == 0 {
				score = nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
			}
			scores = append(scores, score)
			pool[score.ClientId] = true
			if i < verifiedCount {
				verified[score.ClientId] = true
			} else {
				unverified = append(unverified, score.ClientId)
			}
		}
		self := unverified[len(unverified)-1]
		writeOnlineBackfillSample(ctx, t, location, RankModeQuality, false, scores)
		writeOnlineBackfillSample(ctx, t, location, RankModeQuality, true, scores)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, "UPDATE network_client_location SET arin_quality_verified=false WHERE client_id=ANY($1)", unverified))
		})
		seed := func(rankMode RankMode) (*FindProviders2Result, error) {
			clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "online-seed", false, false))
			return FindProviders2(&FindProviders2Args{
				Specs:            []*ProviderSpec{{BestAvailable: true}},
				Count:            8,
				ExcludeClientIds: []server.Id{self},
				RankMode:         rankMode,
				ForceMinimum:     true,
			}, clientSession)
		}

		for range 3 {
			before := onlineTestSubscriberReadsNow()
			result, err := seed(RankModeOnline)
			if err != nil || result == nil {
				t.Fatalf("online seed request failed: result=%v err=%v", result, err)
			}
			if after := onlineTestSubscriberReadsNow(); after != before {
				t.Fatalf("online seed request read subscriber facts: %+v -> %+v", before, after)
			}
			// count 8 without force_count asks for the minimum block of 20
			if len(result.Providers) != 20 {
				t.Fatalf("online seed request returned %d providers, want 20", len(result.Providers))
			}
			assertEgressTestNoRepeats(t, result.Providers)
			unverifiedAnswered := 0
			for _, provider := range result.Providers {
				if provider.ClientId == self || !pool[provider.ClientId] {
					t.Fatal("online seed request answered the validator itself or outside the online pool")
				}
				if !verified[provider.ClientId] {
					unverifiedAnswered++
				}
			}
			if unverifiedAnswered == 0 {
				t.Fatal("online answered only subscriber-verified providers")
			}
		}

		// The same request in Quality stays strict on the same pool.
		before := onlineTestSubscriberReadsNow()
		strict, err := seed(RankModeQuality)
		if err != nil || strict == nil || len(strict.Providers) != verifiedCount {
			t.Fatalf("forced Quality seed request changed: result=%v err=%v", strict, err)
		}
		for _, provider := range strict.Providers {
			if !verified[provider.ClientId] {
				t.Fatal("forced Quality returned a member current subscriber facts refuse")
			}
		}
		if after := onlineTestSubscriberReadsNow(); after.batches <= before.batches {
			t.Fatal("forced Quality skipped subscriber validation")
		}
	})
}

// Online keeps every common and request-only refusal on a published pool: a
// current hard exclusion, another network's network-only provider, a family
// outside the filter, explicit and final-destination exclusions. A record
// that is neither online nor native never answers, a native record without
// the online flag does, and revoked subscriber facts change nothing. Its
// metrics carry the online label.
func TestFindProviders2OnlineKeepsCommonAndRequestFilters(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		location, callerNetworkId := server.NewId(), server.NewId()
		onlineOnly := onlineBackfillScore(true, 1)
		quality := nativeTestScore(RankModeQuality, ipFamilyFacetDualstack)
		speed := nativeTestScore(RankModeSpeed, ipFamilyFacetV4Only)
		unflagged := nativeTestScore(RankModeSpeed, ipFamilyFacetV4Only)
		unflagged.Online = false
		notOnline := onlineBackfillScore(true, 1)
		notOnline.Online, notOnline.PassesMinimums = false, nil
		hard := onlineBackfillScore(true, 1)
		ownNetwork := onlineBackfillScore(true, 1)
		ownNetwork.NetworkOnly, ownNetwork.NetworkId = true, callerNetworkId
		otherNetwork := onlineBackfillScore(true, 1)
		otherNetwork.NetworkOnly = true
		explicit := onlineBackfillScore(true, 1)
		intermediary, finalHop := onlineBackfillScore(true, 1), onlineBackfillScore(true, 1)
		// a v6-only record on a v4-only page, as a legacy page can hold one
		wrongFamily := onlineBackfillScore(true, 1)
		wrongFamily.IpFamilies = ClientScoreIpFamilyV6
		v6 := onlineBackfillScore(true, 1)
		v6.IpFamilies = ClientScoreIpFamilyV6
		nativeTestPublishLocation(t, location, RankModeQuality, map[ipFamilyFacet][]*ClientScore{
			ipFamilyFacetDualstack: {quality},
			ipFamilyFacetV4Only:    {onlineOnly, speed, unflagged, notOnline, hard, ownNetwork, otherNetwork, explicit, intermediary, finalHop, wrongFamily},
			ipFamilyFacetV6Only:    {v6},
		})
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.SAdd(ctx, providerHardExclusionsKey, hard.ClientId.String()).Err())
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location SET arin_quality_verified=false WHERE connection_id=$1`, quality.ClientId))
		})
		find := func(ipFamily string, force bool) []server.Id {
			clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(callerNetworkId, server.NewId(), "online-filter-test", false, false))
			result, err := FindProviders2(&FindProviders2Args{
				Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: RankModeOnline, Count: 20, ForceCount: true, ForceMinimum: force, IpFamily: ipFamily,
				ExcludeClientIds: []server.Id{explicit.ClientId}, ExcludeDestinations: [][]server.Id{{intermediary.ClientId, finalHop.ClientId}},
			}, clientSession)
			if err != nil || result == nil {
				t.Fatalf("online request failed: %v", err)
			}
			assertEgressTestNoRepeats(t, result.Providers)
			return onlineTestSorted(egressTestIds(result.Providers)...)
		}

		want := onlineTestSorted(onlineOnly.ClientId, quality.ClientId, speed.ClientId, unflagged.ClientId, ownNetwork.ClientId, intermediary.ClientId)
		for _, nativeReader := range []bool{false, true} {
			func() {
				pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(fmt.Sprintf("subscriber_quality_policy_version: 2\negress_index:\n  native_reader_enabled: %t\n", nativeReader)))
				defer pop()
				requestEgressIndexSettingsSnapshot.Store(nil)
				defer requestEgressIndexSettingsSnapshot.Store(nil)
				for _, force := range []bool{false, true} {
					before := onlineTestSubscriberReadsNow()
					if got := find("", force); !slices.Equal(got, want) {
						t.Fatalf("reader=%t force=%t: online answered %d providers, want %d", nativeReader, force, len(got), len(want))
					}
					if after := onlineTestSubscriberReadsNow(); after != before {
						t.Fatalf("reader=%t force=%t: online read subscriber facts", nativeReader, force)
					}
				}
			}()
		}
		// The filter names the families; v6-capable reads only its own pages.
		if got := find("v6-capable", false); !slices.Equal(got, onlineTestSorted(quality.ClientId, v6.ClientId)) {
			t.Fatalf("v6-capable online answered %d providers, want 2", len(got))
		}

		// Metrics name the mode. A non-forced answer is observed as one that
		// borrowed nothing; a forced one is not, as in every mode.
		backfill := func() (uint64, float64) {
			metric := &dto.Metric{}
			if err := findProviders2BackfillProviders.WithLabelValues(RankModeOnline).(prometheus.Metric).Write(metric); err != nil {
				t.Fatal(err)
			}
			return metric.GetHistogram().GetSampleCount(), metric.GetHistogram().GetSampleSum()
		}
		for _, force := range []bool{false, true} {
			requestClass := "count_positive"
			if force {
				requestClass = "forced_minimum"
			}
			selection := map[string]string{"rank_mode": RankModeOnline, "request_class": requestClass, "outcome": "nonempty"}
			outcomes := map[string]string{"rank_mode": RankModeOnline, "force_minimum": fmt.Sprint(force)}
			beforeSelection := onlineTestMetric(t, "urnetwork_findproviders2_selection_outcomes_total", selection)
			beforeOutcomes := onlineTestMetric(t, "urnetwork_findproviders2_outcomes_total", outcomes)
			beforeUnknown := onlineTestMetric(t, "urnetwork_findproviders2_selection_outcomes_total", map[string]string{"rank_mode": "unknown"})
			beforeAnswers, beforeBorrowed := backfill()
			beforeAnswered := testutil.ToFloat64(findProviders2AnsweredProviders.WithLabelValues(RankModeOnline))
			answered := len(find("", force))
			if onlineTestMetric(t, "urnetwork_findproviders2_selection_outcomes_total", selection) != beforeSelection+1 ||
				onlineTestMetric(t, "urnetwork_findproviders2_outcomes_total", outcomes) != beforeOutcomes+1 ||
				onlineTestMetric(t, "urnetwork_findproviders2_selection_outcomes_total", map[string]string{"rank_mode": "unknown"}) != beforeUnknown {
				t.Fatalf("force=%t: online outcome was not labelled online", force)
			}
			answers, borrowed := backfill()
			wantAnswers, wantAnswered := beforeAnswers+1, beforeAnswered+float64(answered)
			if force {
				wantAnswers, wantAnswered = beforeAnswers, beforeAnswered
			}
			if answers != wantAnswers || borrowed != beforeBorrowed || testutil.ToFloat64(findProviders2AnsweredProviders.WithLabelValues(RankModeOnline)) != wantAnswered {
				t.Fatalf("force=%t: online answer observation changed: answers=%d borrowed=%g", force, answers-beforeAnswers, borrowed-beforeBorrowed)
			}
		}
	})
}

// An online draw prefers no family. The preferred facet alone can fill the
// answer here, which keeps a Speed request in dualstack, but online still
// reaches the v4-only providers.
func TestFindProviders2OnlineDrawsAcrossFamilies(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		location := server.NewId()
		scores := map[ipFamilyFacet][]*ClientScore{}
		v4 := map[server.Id]bool{}
		// five full dualstack pages fill a 20-provider request's 1,000 rows
		for range 5 * ClientScoreSampleCount {
			scores[ipFamilyFacetDualstack] = append(scores[ipFamilyFacetDualstack], nativeTestScore(RankModeSpeed, ipFamilyFacetDualstack))
		}
		for range ClientScoreSampleCount {
			score := nativeTestScore(RankModeSpeed, ipFamilyFacetV4Only)
			v4[score.ClientId] = true
			scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
		}
		nativeTestPublishLocation(t, location, RankModeQuality, scores)
		nativeTestPublishLocation(t, location, RankModeSpeed, scores)
		onlineV4, speedV4 := 0, 0
		// Online misses every v4-only provider in one request with
		// probability under 0.18, so ten requests all miss under 1e-7.
		for range 10 {
			for _, provider := range egressTestFind(ctx, t, []*ProviderSpec{{LocationId: &location}}, RankModeOnline, 20, false, server.NewId()) {
				if v4[provider.ClientId] {
					onlineV4++
				}
			}
			for _, provider := range egressTestFind(ctx, t, []*ProviderSpec{{LocationId: &location}}, RankModeSpeed, 20, false, server.NewId()) {
				if v4[provider.ClientId] {
					speedV4++
				}
			}
		}
		if speedV4 != 0 {
			t.Fatal("Speed lost its dualstack-first family order")
		}
		if onlineV4 == 0 {
			t.Fatal("online draws never reached the v4-only family")
		}
	})
}
