// A large network's exempt index past its rebuild age. A count that found a
// complete index older than providerIntentExemptRebuildAge dropped it, and
// every count until the rebuild completed walked every registry member, two or
// three commands each. On 2026-11-03 and 11-04 the slowlog of the redis node
// holding the slot of a network of tens of thousands of members (node 37, 6416
// on edge-5) grew by one to seven entries at each hourly rebuild, with calls of
// up to 0.57 s, where the slow calls between rebuilds took 10 to 23 ms. Each
// one held every key on the node.
package model

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Writes a network's registries and intent state as before the index existed:
// `memberCount` registry members, three in four connected and the rest
// installs, at distinct expiries, every third one qualified with a live
// presence. Returns the connected members that are exempt and those that are
// not, in sweep order, and the exempt count.
func testingProviderIntentLargePreIndexNetwork(
	ctx context.Context,
	now time.Time,
	memberCount int,
) (networkId server.Id, exemptConnectedIds []server.Id, normalConnectedIds []server.Id, exemptCount int) {
	networkId = server.NewId()
	nowMs := now.UnixMilli()
	connectedCount := memberCount * 3 / 4
	for start := 0; start < memberCount; start += providerIntentExemptBuildLimit {
		server.Redis(ctx, func(r server.RedisClient) {
			pipe := r.TxPipeline()
			for i := start; i < min(start+providerIntentExemptBuildLimit, memberCount); i += 1 {
				clientId := server.NewId()
				member := string(clientId.Bytes())
				registryKey := networkPeerConnectedKey(networkId)
				if connectedCount <= i {
					registryKey = networkPeerConnectedProviderKey(networkId)
				}
				// distinct expiries, as registrations written over time have
				pipe.ZAdd(ctx, registryKey, redis.Z{Score: float64(nowMs + time.Hour.Milliseconds() + int64(i)), Member: member})
				exempt := i%3 == 0
				if exempt {
					pipe.ZAdd(ctx, providerIntentPresentKey(networkId), redis.Z{Score: float64(nowMs + time.Hour.Milliseconds()), Member: member})
					pipe.ZAdd(ctx, providerIntentExpireKey(networkId), redis.Z{Score: float64(nowMs + time.Hour.Milliseconds()), Member: member})
					pipe.HSet(ctx, providerIntentKey(networkId), member, "qualified {}")
					exemptCount += 1
				}
				if i < connectedCount {
					if exempt {
						exemptConnectedIds = append(exemptConnectedIds, clientId)
					} else {
						normalConnectedIds = append(normalConnectedIds, clientId)
					}
				}
			}
			_, err := pipe.Exec(ctx)
			server.Raise(err)
		})
	}
	return
}

// Counts until a network without an index has a complete one. These are the
// first counts of the network, which walk while the index builds; each is
// exact.
func testingProviderIntentCompleteExemptBuild(t testing.TB, ctx context.Context, networkId server.Id, now time.Time, normalCount int, exemptCount int) {
	for counts := 1; testingProviderIntentExemptBuildState(ctx, networkId) != "#ready"; counts += 1 {
		normal, exempt := getNetworkConnectedClientCountsAt(ctx, networkId, now)
		connect.AssertEqual(t, normal, normalCount)
		connect.AssertEqual(t, exempt, exemptCount)
		if 8 < counts {
			t.Fatalf("the first build did not complete in %d counts", counts)
		}
	}
}

// Reads whether a network's index holds `#ready`, the age of the build time it
// records on the redis clock, and the -inf build state beside it, "" when
// none. A re-sweep in place keeps `#ready` and its old build time until it
// completes.
func testingProviderIntentExemptReady(ctx context.Context, networkId server.Id) (ready bool, age time.Duration, sentinel string) {
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.Pipeline()
		builtCmd := pipe.ZScore(ctx, providerIntentExemptKey(networkId), "#ready")
		statesCmd := pipe.ZRangeByScore(ctx, providerIntentExemptKey(networkId), &redis.ZRangeBy{
			Min:   "-inf",
			Max:   "-inf",
			Count: 2,
		})
		timeCmd := pipe.Time(ctx)
		_, err := pipe.Exec(ctx)
		if err != nil && err != server.RedisNil {
			panic(err)
		}
		if built, err := builtCmd.Result(); err == nil {
			ready = true
			if math.IsInf(built, -1) {
				age = time.Duration(math.MaxInt64)
			} else {
				age = timeCmd.Val().Sub(time.UnixMilli(int64(-built)))
			}
		}
		for _, state := range statesCmd.Val() {
			if state != "#ready" {
				sentinel = state
				break
			}
		}
	})
	return
}

// Whether a network's index re-derived itself since it was aged: `#ready`
// records a build within the last minute and no build state is left.
func testingProviderIntentExemptRebuilt(ctx context.Context, networkId server.Id) bool {
	ready, age, sentinel := testingProviderIntentExemptReady(ctx, networkId)
	return ready && sentinel == "" && 0 <= age && age < time.Minute
}

// A network of eight build limits of registry members, with a complete index
// past its age. Each count after the age reads the index while its entries are
// re-derived in place, and derives at most a build limit of members and two
// more, so it runs the same bounded number of commands whatever the network's
// size; no count walks, which alone took two or three commands per member. The
// re-sweep completes in about one count per build limit of members, then
// records a new build time. The commands are the count script's own
// (testingCountedScript); the production counts report the index path.
func TestProviderIntentExemptIndexPastItsAgeCountsWithoutWalking(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		memberCount := 8 * providerIntentExemptBuildLimit
		networkId, _, _, exemptCount := testingProviderIntentLargePreIndexNetwork(ctx, now, memberCount)
		normalCount := memberCount - exemptCount
		testingProviderIntentCompleteExemptBuild(t, ctx, networkId, now, normalCount, exemptCount)

		testingAgeProviderIntentExemptIndex(t, ctx, networkId, providerIntentExemptRebuildAge+10*time.Minute)
		// a build limit of derivations and two more, at most seven commands each,
		// and the count's own
		maxCommands := int64(7*(providerIntentExemptBuildLimit+2) + 64)
		counts := 0
		for {
			commands, result := testingCountedScript(t, ctx, providerIntentCountClientsLua, providerIntentKeys(networkId), providerIntentCountArgs(now)...)
			counts += 1
			ready, age, sentinel := testingProviderIntentExemptReady(ctx, networkId)
			t.Logf("count %d past the age: %d commands, path %d, ready %t, build %s ago, state %q", counts, commands, result[2], ready, age.Round(time.Second), sentinel)
			if result[2] != int64(1) || maxCommands < commands {
				t.Fatalf(
					"count %d past the age of a %d member index: path %d (1 the index, 2 the walk) in %d commands, want the index in at most %d",
					counts,
					memberCount,
					result[2],
					commands,
					maxCommands,
				)
			}
			connect.AssertEqual(t, result[0], int64(normalCount))
			connect.AssertEqual(t, result[1], int64(exemptCount))
			if !ready {
				t.Fatalf("count %d past the age dropped the index", counts)
			}
			if testingProviderIntentExemptRebuilt(ctx, networkId) {
				break
			}
			if memberCount/providerIntentExemptBuildLimit+2 < counts {
				t.Fatalf("the re-sweep did not complete in %d counts", counts)
			}
		}
		// the re-sweep is spread over the counts, a build limit each
		if counts < memberCount/providerIntentExemptBuildLimit {
			t.Fatalf("the re-sweep of %d members completed in %d counts", memberCount, counts)
		}
		indexedCommands, result := testingCountedScript(t, ctx, providerIntentCountClientsLua, providerIntentKeys(networkId), providerIntentCountArgs(now)...)
		connect.AssertEqual(t, result[2], int64(1))
		if 13 < indexedCommands {
			t.Fatalf("a count with the index re-derived took %d commands", indexedCommands)
		}

		// the production counts read the index throughout
		walkCounter := providerIntentCountsCounter.WithLabelValues(providerIntentCountPathWalk)
		indexCounter := providerIntentCountsCounter.WithLabelValues(providerIntentCountPathIndex)
		testingAgeProviderIntentExemptIndex(t, ctx, networkId, providerIntentExemptRebuildAge+10*time.Minute)
		walksBefore := testutil.ToFloat64(walkCounter)
		indexedBefore := testutil.ToFloat64(indexCounter)
		productionCounts := 0
		for {
			normal, exempt := getNetworkConnectedClientCountsAt(ctx, networkId, now)
			productionCounts += 1
			connect.AssertEqual(t, normal, normalCount)
			connect.AssertEqual(t, exempt, exemptCount)
			if testingProviderIntentExemptRebuilt(ctx, networkId) {
				break
			}
			if memberCount/providerIntentExemptBuildLimit+2 < productionCounts {
				t.Fatalf("the re-sweep did not complete in %d production counts", productionCounts)
			}
		}
		connect.AssertEqual(t, productionCounts, counts)
		connect.AssertEqual(t, testutil.ToFloat64(walkCounter)-walksBefore, float64(0))
		connect.AssertEqual(t, testutil.ToFloat64(indexCounter)-indexedBefore, float64(productionCounts))
	})
}

// Writes without the index (a rollout or a rollback) leave a large network's
// complete index behind its sources: two exempt connected members fail and a
// normal one qualifies, each keeping its registration and intent keys, so the
// count's own checks cannot see it. Within its age the index is trusted and
// lags the writes. Past it the counts read the index while it re-derives its
// entries in place, never walking and never counting below zero, and once the
// re-sweep completes they equal the walk again.
func TestProviderIntentExemptIndexPastItsAgeRepairsWritesWithoutTheIndex(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		memberCount := 6 * providerIntentExemptBuildLimit
		networkId, exemptConnectedIds, normalConnectedIds, exemptCount := testingProviderIntentLargePreIndexNetwork(ctx, now, memberCount)
		testingProviderIntentCompleteExemptBuild(t, ctx, networkId, now, memberCount-exemptCount, exemptCount)

		nowMs := now.UnixMilli()
		server.Redis(ctx, func(r server.RedisClient) {
			pipe := r.TxPipeline()
			// the first and the last exempt connected member in sweep order fail
			for _, clientId := range []server.Id{exemptConnectedIds[0], exemptConnectedIds[len(exemptConnectedIds)-1]} {
				pipe.HSet(ctx, providerIntentKey(networkId), string(clientId.Bytes()), "normal {}")
			}
			// and the last normal one qualifies
			member := string(normalConnectedIds[len(normalConnectedIds)-1].Bytes())
			pipe.ZAdd(ctx, providerIntentPresentKey(networkId), redis.Z{Score: float64(nowMs + time.Hour.Milliseconds()), Member: member})
			pipe.ZAdd(ctx, providerIntentExpireKey(networkId), redis.Z{Score: float64(nowMs + time.Hour.Milliseconds()), Member: member})
			pipe.HSet(ctx, providerIntentKey(networkId), member, "qualified {}")
			_, err := pipe.Exec(ctx)
			server.Raise(err)
		})
		oracleNormal, oracleExempt := testingProviderIntentOracleCounts(ctx, networkId, now)
		connect.AssertEqual(t, oracleExempt, int64(exemptCount-1))

		normal, exempt := getNetworkConnectedClientCountsAt(ctx, networkId, now)
		connect.AssertEqual(t, int64(normal), oracleNormal-1)
		connect.AssertEqual(t, int64(exempt), oracleExempt+1)

		testingAgeProviderIntentExemptIndex(t, ctx, networkId, providerIntentExemptRebuildAge+10*time.Minute)
		for counts := 1; ; counts += 1 {
			commands, result := testingCountedScript(t, ctx, providerIntentCountClientsLua, providerIntentKeys(networkId), providerIntentCountArgs(now)...)
			t.Logf("count %d past the age: %d commands, path %d, normal %d exempt %d (walk %d, %d)", counts, commands, result[2], result[0], result[1], oracleNormal, oracleExempt)
			if result[2] != int64(1) {
				t.Fatalf("count %d past the age: path %d (1 the index, 2 the walk) in %d commands, want the index", counts, result[2], commands)
			}
			if result[0].(int64) < 0 || result[1].(int64) < 0 {
				t.Fatalf("count %d past the age: normal %d exempt %d", counts, result[0], result[1])
			}
			if testingProviderIntentExemptRebuilt(ctx, networkId) {
				break
			}
			if memberCount/providerIntentExemptBuildLimit+2 < counts {
				t.Fatalf("the re-sweep did not complete in %d counts", counts)
			}
		}
		normal, exempt = getNetworkConnectedClientCountsAt(ctx, networkId, now)
		connect.AssertEqual(t, int64(normal), oracleNormal)
		connect.AssertEqual(t, int64(exempt), oracleExempt)
	})
}
