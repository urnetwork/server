// The full score export chooses, applies and publishes one serving database
// (arindbctl/ARINDB.md, "Comparison and breaker" and "Publication and
// request-time consistency").
package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

const (
	testArindbDbOldEpoch    = int64(1790000000)
	testArindbDbLatestEpoch = int64(1790086400)
)

// Makes the export compare latestEpoch against oldEpoch, or against no old
// database when oldEpoch is 0, whatever the process's config holds.
func useArindbCanaryPairForTest(t testing.TB, latestEpoch int64, oldEpoch int64) {
	t.Helper()
	previous := arindbCanaryPairIdentities
	arindbCanaryPairIdentities = func() (server.ArinDatabaseIdentity, server.ArinDatabaseIdentity, error) {
		latest := server.ArinDatabaseIdentity{State: server.ArinDatabaseLoaded, BuildEpoch: latestEpoch, Sha256: fmt.Sprintf("%064x", latestEpoch)}
		old := server.ArinDatabaseIdentity{State: server.ArinDatabaseMissing}
		if oldEpoch != 0 {
			old = server.ArinDatabaseIdentity{State: server.ArinDatabaseLoaded, BuildEpoch: oldEpoch, Sha256: fmt.Sprintf("%064x", oldEpoch)}
		}
		return latest, old, nil
	}
	t.Cleanup(func() { arindbCanaryPairIdentities = previous })
}

// Providers the latest database calls unknown and the old one a verified
// subscriber, all with passing URL health.
func connectArindbCanaryProvidersForTest(t testing.TB, location *Location, n int) []*egressTestProvider {
	t.Helper()
	ctx := t.Context()
	lookup := server.NowUtc().Truncate(time.Microsecond)
	providers := []*egressTestProvider{}
	for range n {
		provider := egressTestConnect(ctx, t, location, egressTestUnsampled, nil, &ConnectionLocationScores{
			ArinLookupAt: &lookup, ArinDatabaseBuildEpoch: testArindbDbLatestEpoch, ArinNonQuality: true,
			ArinOldDatabaseBuildEpoch: testArindbDbOldEpoch, ArinOldQualityVerified: true,
		})
		egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 0)
		providers = append(providers, provider)
	}
	return providers
}

// A latest database that removes all Quality trips the breaker: the export
// publishes fallback before writing targets, serves the old classification
// to every provider and records the choice in the native census.
func TestUpdateClientScoresServesTheOldClassificationWhenTheLatestTrips(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		useArindbCanaryPairForTest(t, testArindbDbLatestEpoch, testArindbDbOldEpoch)
		ctx := t.Context()
		city := egressTestCity(ctx, "Canary City", "Canary Region", "Canary Country", "zz")
		providers := connectArindbCanaryProvidersForTest(t, city, 4)
		egressTestPasses(ctx, t)

		decision, err := GetArindbCanaryDecision(ctx)
		if err != nil || decision == nil {
			t.Fatalf("no published decision: %v", err)
		}
		quality := decision.Buckets[RankModeQuality]
		if decision.State != ArindbCanaryStateFallback || decision.ServingEpoch != testArindbDbOldEpoch ||
			quality.Old != 4 || quality.Latest != 0 || !quality.Tripped || decision.Coverage.Paired != 4 {
			t.Fatalf("decision state=%s serving=%d quality=%+v coverage=%+v", decision.State, decision.ServingEpoch, quality, decision.Coverage)
		}
		census, err := GetClientScoreNativeCensus(ctx)
		if err != nil || census == nil || census.Buckets[RankModeQuality].Providers != 4 || census.Arindb == nil ||
			census.Arindb.DecisionId != decision.DecisionId || census.Arindb.State != ArindbCanaryStateFallback {
			t.Fatalf("census does not follow the published decision: %+v %v", census, err)
		}
		scores := egressTestCachedScores(ctx, t, city, RankModeQuality, false)
		for _, provider := range providers {
			if score := scores[provider.clientId]; score == nil || !score.PassesMinimums[RankModeQuality] {
				t.Fatalf("provider %s lost the old classification's Quality", provider.clientId)
			}
		}
	})
}

// Without an old database in the indexer's pair the export is single and
// serves each row's latest slot, as before the canary.
func TestUpdateClientScoresIsSingleWithoutAnOldDatabase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		useArindbCanaryPairForTest(t, testArindbDbLatestEpoch, 0)
		ctx := t.Context()
		city := egressTestCity(ctx, "Single City", "Single Region", "Single Country", "zz")
		providers := connectArindbCanaryProvidersForTest(t, city, 2)
		egressTestPasses(ctx, t)

		decision, err := GetArindbCanaryDecision(ctx)
		if err != nil || decision == nil || decision.State != ArindbCanaryStateSingle || decision.ServingEpoch != 0 || decision.SingleReason != "old_missing" {
			t.Fatalf("decision %+v %v", decision, err)
		}
		scores := egressTestCachedScores(ctx, t, city, RankModeSpeed, false)
		for _, provider := range providers {
			score := scores[provider.clientId]
			if score == nil || score.PassesMinimums[RankModeQuality] || !score.PassesMinimums[RankModeSpeed] {
				t.Fatalf("single changed the latest classification of %s", provider.clientId)
			}
		}
	})
}
