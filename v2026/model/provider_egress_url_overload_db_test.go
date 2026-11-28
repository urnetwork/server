package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The overload judge against the database (provider_egress_url_overload.go):
// URL outcomes stored through the operator ingest's store, the judge run as the
// taskworker runs it, and the passes that score providers and decide the
// pools, in the order the taskworker runs them.

// The judge's settings for a fleet of a few dozen outcomes a bucket, where
// Main's buckets hold thousands.
func overloadTestSettings() *ProviderEgressUrlOverloadSettings {
	settings := DefaultProviderEgressUrlOverloadSettings()
	settings.MinOutcomeCount = 6
	return settings
}

// The test fleet: in each of three countries two providers whose URL outcomes
// all pass and one whose all fail, so a third of the fleet's outcomes fail, as
// on a normal day. Each provider is on its own network. base is the start of
// the bucket after the newest outcome a test stores, on the judge's grid.
type overloadTestFleet struct {
	base      time.Time
	countries map[string][]*egressTestProvider
	good      []*egressTestProvider
	bad       []*egressTestProvider
}

// The good providers' eight-hour evidence, five passes each, and the bad
// providers' five failures, measured over the forty minutes that end an hour
// before base.
func newOverloadTestFleet(ctx context.Context, t testing.TB) *overloadTestFleet {
	t.Helper()
	fleet := &overloadTestFleet{
		base:      server.NowUtc().Truncate(overloadTestSettings().BucketDuration),
		countries: map[string][]*egressTestProvider{},
	}
	for _, place := range []struct {
		city, region, country, countryCode string
	}{
		{"Palo Alto", "California", "United States", "us"},
		{"Berlin", "Berlin", "Germany", "de"},
		{"Osaka", "Osaka", "Japan", "jp"},
	} {
		city := egressTestCity(ctx, place.city, place.region, place.country, place.countryCode)
		for i := range 3 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			for lookbackIndex := range 3 {
				egressTestReliability(ctx, provider.clientId, lookbackIndex, 1, 1)
			}
			if i < 2 {
				egressTestHealth(ctx, provider.clientId, fleet.base.Add(-time.Hour), 5, 0)
				fleet.good = append(fleet.good, provider)
			} else {
				egressTestHealth(ctx, provider.clientId, fleet.base.Add(-time.Hour), 5, 5)
				fleet.bad = append(fleet.bad, provider)
			}
			fleet.countries[place.countryCode] = append(fleet.countries[place.countryCode], provider)
		}
	}
	return fleet
}

// Stores one measured URL outcome through the operator ingest's store.
func overloadTestOutcome(ctx context.Context, t testing.TB, clientId server.Id, measuredAt time.Time, ok bool) {
	t.Helper()
	okCount := 0
	if ok {
		okCount = 1
	}
	stored := StoreProviderEgressHealth(ctx, &ProviderEgressHealth{
		RunId:            server.NewId(),
		ClientId:         clientId,
		MeasuredAt:       measuredAt,
		OKCount:          okCount,
		Total:            1,
		ClassResults:     map[string]ProviderEgressHealthClassResult{"site": {OK: okCount, Total: 1}},
		UrlProbeEvidence: fp2TestUrlProbeEvidence(measuredAt, ok),
	})
	if !stored.Stored || stored.Probation || stored.Recheck {
		t.Fatalf("the outcome was stored %+v, want stored and scored", stored)
	}
}

// Runs the judge as the taskworker does, at every ten minutes of measured
// time from `from` through `to`, and returns the runs.
func overloadTestJudge(ctx context.Context, from time.Time, to time.Time, settings *ProviderEgressUrlOverloadSettings) []*ProviderEgressUrlOverloadJudgement {
	judgements := []*ProviderEgressUrlOverloadJudgement{}
	for at := from; !at.After(to); at = at.Add(10 * time.Minute) {
		judgements = append(judgements, JudgeProviderEgressUrlOverload(ctx, at, settings))
	}
	return judgements
}

// What scores and pools a provider by: its window's outcomes, its index now
// and in the rollup, and its buckets.
type overloadTestProviderState struct {
	okCount     int
	total       int
	index       int
	rollupIndex int
	quality     bool
	speed       bool
	online      bool
	reason      string
}

// Runs the passes, then reads every provider's state and the pools' sizes.
func overloadTestSnapshot(ctx context.Context, t testing.TB, fleet *overloadTestFleet) (map[server.Id]overloadTestProviderState, map[string]int64) {
	t.Helper()
	egressTestPasses(ctx, t)
	counts, _ := getAllProviderEgressHealthCountsSnapshot(ctx)
	states := map[server.Id]overloadTestProviderState{}
	for _, provider := range append(append([]*egressTestProvider{}, fleet.good...), fleet.bad...) {
		inspection := InspectProviderEgress(ctx, provider.clientId)
		state := overloadTestProviderState{
			okCount: counts[provider.clientId].OKCount,
			total:   counts[provider.clientId].Total,
			index:   inspection.CurrentIndex.Index,
			quality: inspection.Quality,
			speed:   inspection.Speed,
			online:  inspection.Online,
			reason:  inspection.Reason,
		}
		if inspection.EgressIndex != nil {
			state.rollupIndex = *inspection.EgressIndex
		}
		states[provider.clientId] = state
	}
	pools := map[string]int64{}
	for bucket, indexCounts := range CountProviderEgress(ctx).BucketIndexCounts {
		for _, count := range indexCounts {
			pools[bucket] += count
		}
	}
	return states, pools
}

// The stored bucket that starts at start, or nil.
func overloadTestStoredBucket(ctx context.Context, start time.Time) *ProviderEgressUrlFleetBucket {
	var bucket *ProviderEgressUrlFleetBucket
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
			SELECT bucket_start, bucket_end, outcome_count, error_count, country_outcome_count, country_error_count,
				network_outcome_count, network_error_count, baseline_outcome_count, baseline_error_count,
				discounted, discounted_outcome_count, discounted_error_count
			FROM provider_egress_url_fleet_bucket WHERE bucket_start = $1`, start.UTC())
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				bucket = &ProviderEgressUrlFleetBucket{}
				server.Raise(rows.Scan(
					&bucket.BucketStart, &bucket.BucketEnd, &bucket.OutcomeCount, &bucket.ErrorCount,
					&bucket.CountryOutcomeCount, &bucket.CountryErrorCount, &bucket.NetworkOutcomeCount, &bucket.NetworkErrorCount,
					&bucket.BaselineOutcomeCount, &bucket.BaselineErrorCount,
					&bucket.Discounted, &bucket.DiscountedOutcomeCount, &bucket.DiscountedErrorCount,
				))
			}
		})
	})
	return bucket
}

// The history's scored rows measured in [from, to), discounted or not.
func overloadTestRecordedCount(ctx context.Context, from time.Time, to time.Time) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM provider_egress_health_history
			WHERE measured_at >= $1 AND measured_at < $2 AND `+providerEgressHealthScoredRowsSql(""), from.UTC(), to.UTC()).Scan(&count))
	})
	return
}

// The root cause of 2026-11-25: our own overload failed probes across the
// whole fleet, and those failures counted against the providers. In one
// five-minute bucket every provider of every country and network fails 30 of
// its 36 outcomes, the good ones three of four; before the judge has seen the
// bucket, that takes every good provider out of Quality and Speed, as the
// 00:24Z pass did. Once the judge discounts the bucket, every provider's
// window, index, rollup and buckets, and the pools, are what they were
// before the overload, and the outcomes are still recorded. A late outcome of
// the bucket is discounted with it, and counted once.
func TestProviderEgressUrlOverloadKeepsScoresAndPoolsThroughAFleetWideOverload(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		egressTestCountryFloorOff(t)
		settings := overloadTestSettings()
		fleet := newOverloadTestFleet(ctx, t)
		overloadStart := fleet.base.Add(-30 * time.Minute)
		overloadEnd := overloadStart.Add(settings.BucketDuration)

		// the judge has run through the normal hours before the overload
		overloadTestJudge(ctx, fleet.base.Add(-2*time.Hour), overloadStart.Add(-time.Second), settings)
		before, beforePools := overloadTestSnapshot(ctx, t, fleet)
		for _, provider := range fleet.good {
			if state := before[provider.clientId]; !state.quality || !state.speed || state.okCount != 5 || state.total != 5 {
				t.Fatalf("a good provider before the overload is %+v, want 5 of 5 in Quality and Speed", state)
			}
		}
		connect.AssertEqual(t, beforePools[string(RankModeQuality)], int64(len(fleet.good)))

		// the overload: every provider fails, the good ones three of four
		for i, provider := range append(append([]*egressTestProvider{}, fleet.good...), fleet.bad...) {
			for j := range 4 {
				at := overloadStart.Add(time.Duration(4*i+j) * 5 * time.Second)
				overloadTestOutcome(ctx, t, provider.clientId, at, j == 3 && i < len(fleet.good))
			}
		}

		// the same failures before any verdict lower every good provider's
		// score and take it out of Quality and Speed
		unjudged, unjudgedPools := overloadTestSnapshot(ctx, t, fleet)
		for _, provider := range fleet.good {
			if state := unjudged[provider.clientId]; state.quality || state.speed || state.okCount != 6 || state.total != 9 || state.index <= before[provider.clientId].index {
				t.Fatalf("a good provider through the unjudged overload is %+v, want 6 of 9 out of Quality and Speed with a worse index", state)
			}
		}
		connect.AssertEqual(t, unjudgedPools[string(RankModeQuality)], int64(0))

		// the judge runs on through the overload; then every provider and
		// pool is as before it
		judgements := overloadTestJudge(ctx, overloadStart, fleet.base, settings)
		after, afterPools := overloadTestSnapshot(ctx, t, fleet)
		for clientId, state := range before {
			if after[clientId] != state {
				t.Errorf("provider %s after the judged overload is %+v, want %+v as before it", clientId, after[clientId], state)
			}
		}
		connect.AssertEqual(t, afterPools, beforePools)
		// recorded, only not scored
		connect.AssertEqual(t, overloadTestRecordedCount(ctx, overloadStart, overloadEnd), 36)

		bucket := overloadTestStoredBucket(ctx, overloadStart)
		if bucket == nil || !bucket.Discounted {
			t.Fatalf("the overload's bucket is %+v, want discounted", bucket)
		}
		connect.AssertEqual(t, bucket.BucketEnd.UTC(), overloadEnd)
		connect.AssertEqual(t, bucket.OutcomeCount, 36)
		connect.AssertEqual(t, bucket.ErrorCount, 30)
		// each country two good providers and a bad one, each network one
		connect.AssertEqual(t, bucket.CountryOutcomeCount, 12)
		connect.AssertEqual(t, bucket.CountryErrorCount, 10)
		connect.AssertEqual(t, bucket.NetworkOutcomeCount, 4)
		connect.AssertEqual(t, bucket.NetworkErrorCount, 4)
		connect.AssertEqual(t, bucket.BaselineOutcomeCount, int64(45))
		connect.AssertEqual(t, bucket.BaselineErrorCount, int64(15))
		discountedOutcomes, discountedErrors, entered := 0, 0, 0
		for _, judgement := range judgements {
			discountedOutcomes += judgement.DiscountedOutcomeCount
			discountedErrors += judgement.DiscountedErrorCount
			entered += len(judgement.Entered)
		}
		connect.AssertEqual(t, discountedOutcomes, 36)
		connect.AssertEqual(t, discountedErrors, 30)
		connect.AssertEqual(t, entered, 1)

		// a late outcome of the bucket is discounted with it and counted
		// once; a run that finds nothing new counts nothing
		overloadTestOutcome(ctx, t, fleet.good[0].clientId, overloadEnd.Add(-time.Second), false)
		late := JudgeProviderEgressUrlOverload(ctx, fleet.base, settings)
		connect.AssertEqual(t, late.DiscountedOutcomeCount, 1)
		connect.AssertEqual(t, late.DiscountedErrorCount, 1)
		connect.AssertEqual(t, len(late.Entered), 0)
		again := JudgeProviderEgressUrlOverload(ctx, fleet.base, settings)
		connect.AssertEqual(t, again.DiscountedOutcomeCount, 0)
		lateState, _ := overloadTestSnapshot(ctx, t, fleet)
		connect.AssertEqual(t, lateState[fleet.good[0].clientId], before[fleet.good[0].clientId])

		// a judged bucket is kept as long as the history it judged
		JudgeProviderEgressUrlOverload(ctx, overloadEnd.Add(settings.Retention-time.Second), settings)
		if overloadTestStoredBucket(ctx, overloadStart) == nil {
			t.Fatal("the judged bucket was removed inside its retention")
		}
		if removed := JudgeProviderEgressUrlOverload(ctx, overloadEnd.Add(settings.Retention+time.Second), settings).RemovedCount; removed != 1 || overloadTestStoredBucket(ctx, overloadStart) != nil {
			t.Fatalf("the judged bucket outlived its retention (%d removed)", removed)
		}
	})
}

// A real provider-side outage in one country is no overload of ours: in the
// bucket every German provider fails all four outcomes while the rest of the
// fleet does as on any day. The fleet's share is above the line, but without
// Germany it is its usual third, so the bucket is scored, and the German good
// providers leave Quality and Speed while the others keep them.
func TestProviderEgressUrlOverloadScoresAOneCountryOutageAgainstItsProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		egressTestCountryFloorOff(t)
		settings := overloadTestSettings()
		fleet := newOverloadTestFleet(ctx, t)
		outageStart := fleet.base.Add(-30 * time.Minute)

		overloadTestJudge(ctx, fleet.base.Add(-2*time.Hour), outageStart.Add(-time.Second), settings)
		before, _ := overloadTestSnapshot(ctx, t, fleet)

		n := 0
		for countryCode, providers := range fleet.countries {
			for i, provider := range providers {
				outcomes := 1
				if countryCode == "de" {
					outcomes = 4
				}
				for range outcomes {
					at := outageStart.Add(time.Duration(n) * 5 * time.Second)
					n++
					// elsewhere a good provider passes and a bad one fails
					overloadTestOutcome(ctx, t, provider.clientId, at, countryCode != "de" && i < 2)
				}
			}
		}

		overloadTestJudge(ctx, outageStart, fleet.base, settings)
		bucket := overloadTestStoredBucket(ctx, outageStart)
		if bucket == nil || bucket.Discounted {
			t.Fatalf("the outage's bucket is %+v, want judged and scored", bucket)
		}
		// 14 of 18 failed, above the line; without Germany, 2 of 6
		connect.AssertEqual(t, bucket.OutcomeCount, 18)
		connect.AssertEqual(t, bucket.ErrorCount, 14)
		connect.AssertEqual(t, bucket.CountryOutcomeCount, 12)
		connect.AssertEqual(t, bucket.CountryErrorCount, 12)

		after, _ := overloadTestSnapshot(ctx, t, fleet)
		for countryCode, providers := range fleet.countries {
			for _, provider := range providers[:2] {
				state := after[provider.clientId]
				if countryCode == "de" {
					if state.quality || state.speed || state.okCount != 5 || state.total != 9 {
						t.Errorf("a German good provider through its country's outage is %+v, want 5 of 9 out of Quality and Speed", state)
					}
				} else if state.quality != before[provider.clientId].quality || state.speed != before[provider.clientId].speed || state.okCount != 6 || state.total != 6 {
					t.Errorf("a good provider outside the outage is %+v, want 6 of 6 in its buckets as before (%+v)", state, before[provider.clientId])
				}
			}
		}
	})
}
