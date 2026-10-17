// Database controls for the request-time readers of the published canary
// decision (arindbctl/ARINDB.md, "Publication and request-time consistency").
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Publishes a decision serving servingEpoch, or none when it is 0, and makes
// the process read it now rather than after the cache ttl.
func publishArindbServingEpochForTest(t testing.TB, servingEpoch int64) {
	t.Helper()
	ctx := t.Context()
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Del(ctx, arindbCanaryDecisionKey).Err())
	})
	if servingEpoch != 0 {
		pair := testCanaryPairOf(testCanaryLatestEpoch, testCanaryOldEpoch)
		decision := decideArindbCanary(pair, nil, testCanaryMembership, defaultArindbCanarySettings(), nil, server.NowUtc().Add(-time.Minute), server.NowUtc())
		switch servingEpoch {
		case testCanaryOldEpoch:
			decision.State, decision.ServingBasis = ArindbCanaryStateFallback, ArindbCanaryBasisTripped
		case testCanaryLatestEpoch:
			decision.State, decision.ServingBasis = ArindbCanaryStateLatest, ArindbCanaryBasisPassed
		default:
			t.Fatalf("no state serves epoch %d", servingEpoch)
		}
		decision.ServingEpoch = servingEpoch
		if err := writeArindbCanaryDecision(ctx, decision, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	previous := currentArindbServingEpoch
	currentArindbServingEpoch = &arindbServingEpochCache{}
	t.Cleanup(func() { currentArindbServingEpoch = previous })
	providerSubscriberNegativeCache.reset()
}

// The live-row Quality guard reads each row's slot whose epoch is the
// published one and refuses a row with neither; risk is always the latest
// slot's; without a decision it reads the latest slot, as before the canary.
// A provider without a live connection is offline under every serving epoch,
// whatever its slots say, and a live row the published epoch refuses is a
// Quality refusal, never offline.
func TestSubscriberGuardFollowsThePublishedServingEpochAcrossAMixedFleet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		lookup := server.NowUtc().Truncate(time.Microsecond)
		city := egressTestCity(ctx, "Guard City", "Guard Region", "Guard Country", "zz")
		connect := func(scores *ConnectionLocationScores) server.Id {
			scores.ArinLookupAt = &lookup
			return egressTestConnect(ctx, t, city, egressTestUnsampled, nil, scores).clientId
		}
		// restarted: the latest calls it unknown, the old a subscriber
		restarted := connect(&ConnectionLocationScores{ArinDatabaseBuildEpoch: testCanaryLatestEpoch, ArinNonQuality: true,
			ArinOldDatabaseBuildEpoch: testCanaryOldEpoch, ArinOldQualityVerified: true})
		// not restarted yet: its latest database is the pair's old one
		lagging := connect(&ConnectionLocationScores{ArinDatabaseBuildEpoch: testCanaryOldEpoch, ArinQualityVerified: true})
		// restarted: a subscriber under the latest, excluded under the old
		improved := connect(&ConnectionLocationScores{ArinDatabaseBuildEpoch: testCanaryLatestEpoch, ArinQualityVerified: true,
			ArinOldDatabaseBuildEpoch: testCanaryOldEpoch, ArinOldNonQuality: true})
		// risky under the latest only
		risky := connect(&ConnectionLocationScores{ArinDatabaseBuildEpoch: testCanaryLatestEpoch, ArinRisk: true,
			ArinOldDatabaseBuildEpoch: testCanaryOldEpoch, ArinOldQualityVerified: true})
		// a subscriber under both slots, disconnected: no live connection
		disconnected := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, &ConnectionLocationScores{ArinLookupAt: &lookup,
			ArinDatabaseBuildEpoch: testCanaryLatestEpoch, ArinQualityVerified: true,
			ArinOldDatabaseBuildEpoch: testCanaryOldEpoch, ArinOldQualityVerified: true})
		if err := DisconnectNetworkClient(ctx, disconnected.connectionId); err != nil {
			t.Fatal(err)
		}
		offline := disconnected.clientId
		ids := []server.Id{restarted, lagging, improved, risky, offline}
		for _, c := range []struct {
			name         string
			servingEpoch int64
			admitted     []server.Id
		}{
			{name: "fallback to the old", servingEpoch: testCanaryOldEpoch, admitted: []server.Id{restarted, lagging}},
			{name: "the latest", servingEpoch: testCanaryLatestEpoch, admitted: []server.Id{improved}},
			{name: "no decision", servingEpoch: 0, admitted: []server.Id{lagging, improved}},
		} {
			publishArindbServingEpochForTest(t, c.servingEpoch)
			negative, riskyIds, offlineIds, err := getProviderSubscriberExclusions(ctx, ids)
			if err != nil {
				t.Fatal(err)
			}
			if !riskyIds[risky] || negative[risky] {
				t.Errorf("%s: the latest slot's risk was not a common refusal", c.name)
			}
			if !offlineIds[offline] || negative[offline] || riskyIds[offline] {
				t.Errorf("%s: the provider without a live connection was not read offline", c.name)
			}
			for _, id := range ids {
				if id != offline && offlineIds[id] {
					t.Errorf("%s: live provider %s was read offline", c.name, id)
				}
				admitted := !negative[id] && !riskyIds[id] && !offlineIds[id]
				want := false
				for _, a := range c.admitted {
					want = want || a == id
				}
				if admitted != want {
					t.Errorf("%s: provider %s admitted=%t, want %t", c.name, id, admitted, want)
				}
			}
		}
	})
}

// Provider status and the provider-egress counts read the rollup slot of the
// published epoch, so a fallback restores the Quality that the status and
// the dashboard report.
func TestRollupReadersFollowThePublishedServingEpoch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		lookup := server.NowUtc().Truncate(time.Microsecond)
		city := egressTestCity(ctx, "Status City", "Status Region", "Status Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{
			ArinLookupAt: &lookup, ArinDatabaseBuildEpoch: testCanaryLatestEpoch, ArinNonQuality: true,
			ArinOldDatabaseBuildEpoch: testCanaryOldEpoch, ArinOldQualityVerified: true,
		})
		egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 0)
		egressTestReliability(ctx, provider.clientId, 1, 1, 1)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		for _, c := range []struct {
			name         string
			servingEpoch int64
			quality      bool
		}{
			{name: "no decision", servingEpoch: 0, quality: false},
			{name: "fallback to the old", servingEpoch: testCanaryOldEpoch, quality: true},
			{name: "the latest", servingEpoch: testCanaryLatestEpoch, quality: false},
		} {
			publishArindbServingEpochForTest(t, c.servingEpoch)
			statuses := GetProviderStatuses(ctx, provider.networkId, []server.Id{provider.clientId})
			if len(statuses) != 1 {
				t.Fatalf("%s: %d statuses", c.name, len(statuses))
			}
			if notEligible := statuses[0].Reason == ProviderStatusReasonNotEligible; notEligible == c.quality {
				t.Errorf("%s: status reason %s, want quality=%t", c.name, statuses[0].Reason, c.quality)
			}
			counts := CountProviderEgress(ctx)
			quality := int64(0)
			for _, count := range counts.BucketIndexCounts[RankModeQuality] {
				quality += count
			}
			if (quality == 1) != c.quality || counts.ReasonCounts[ProviderExcludedArinNonQuality] != map[bool]int64{true: 0, false: 1}[c.quality] {
				t.Errorf("%s: quality count %d non-quality reasons %d, want quality=%t", c.name, quality, counts.ReasonCounts[ProviderExcludedArinNonQuality], c.quality)
			}
			if inspection := InspectProviderEgress(ctx, provider.clientId); inspection.Quality != c.quality || inspection.ArinServingEpoch != c.servingEpoch {
				t.Errorf("%s: inspection quality=%t epoch=%d", c.name, inspection.Quality, inspection.ArinServingEpoch)
			}
		}
	})
}
