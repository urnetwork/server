package model

// tests for the RELIABILITY2 hot-path changes: sharded per-block hashes,
// packed binary fields, the hscan drain, and the legacy-key transition read.

import (
	"context"
	"fmt"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// TestClientReliabilityStatsShardedDrain records stats for clients spread
// across multiple shards in one block and verifies: each client's counters
// land only in its own shard key (packed binary fields), the drain merges
// every shard into correct absolute pg rows, and all shard keys are removed
// after the drain.
func TestClientReliabilityStatsShardedDrain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()

		// collect clients until at least two distinct shards are covered, so
		// the drain provably merges across shard keys
		clientIds := []server.Id{}
		shards := map[int]bool{}
		for len(clientIds) < 4 || len(shards) < 2 {
			clientId := server.NewId()
			clientIds = append(clientIds, clientId)
			shards[clientReliabilityStatsShard(clientId)] = true
		}

		now := server.NowUtc()
		blockNumber := reliabilityBlockNumber(now)

		stats := &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        2,
			ReceiveByteCount:           256,
			SendMessageCount:           1,
			SendByteCount:              128,
		}

		clientAddressHashes := map[server.Id][32]byte{}
		for i, clientId := range clientIds {
			ip := netip.MustParseAddr(fmt.Sprintf("10.0.0.%d", i+1))
			clientAddressHash := server.ClientIpHashForAddr(ip)
			clientAddressHashes[clientId] = clientAddressHash
			// two records accumulate
			RecordClientReliabilityStatsRange(ctx, networkId, clientId, clientAddressHash, now, now, stats)
			RecordClientReliabilityStatsRange(ctx, networkId, clientId, clientAddressHash, now, now, stats)
		}

		// each client's fields live only in its own shard key, as packed
		// binary fields (the nonzero counters of two records, coalesced by
		// hincrby into one field per counter)
		server.Redis(ctx, func(r server.RedisClient) {
			for _, clientId := range clientIds {
				shardKey := clientReliabilityStatsKey(blockNumber, clientReliabilityStatsShard(clientId))
				fields, err := r.HGetAll(ctx, shardKey).Result()
				connect.AssertEqual(t, err, nil)
				clientFieldCount := 0
				for field := range fields {
					connect.AssertEqual(t, len(field), clientReliabilityPackedFieldLength)
					if string(field[48:64]) == string(clientId.Bytes()) {
						clientFieldCount += 1
					}
				}
				// the 6 nonzero counters of stats
				connect.AssertEqual(t, clientFieldCount, 6)
			}
			// the legacy unsharded key is not written by the new path
			legacyFields, _ := r.HGetAll(ctx, clientReliabilityStatsLegacyKey(blockNumber)).Result()
			connect.AssertEqual(t, len(legacyFields), 0)
		})

		// drain once final and verify absolute counts per client
		later := now.Add(3 * ReliabilityBlockDuration)
		RollupClientReliabilityStats(ctx, later)

		for _, clientId := range clientIds {
			counters, valid, ok := testingGetClientReliabilityRow(ctx, blockNumber, clientAddressHashes[clientId], clientId)
			connect.AssertEqual(t, ok, true)
			connect.AssertEqual(t, counters["connection_established_count"], int64(2))
			connect.AssertEqual(t, counters["provide_enabled_count"], int64(2))
			connect.AssertEqual(t, counters["receive_message_count"], int64(4))
			connect.AssertEqual(t, counters["receive_byte_count"], int64(512))
			connect.AssertEqual(t, counters["send_message_count"], int64(2))
			connect.AssertEqual(t, counters["send_byte_count"], int64(256))
			connect.AssertEqual(t, valid, true)
		}

		// every shard key is gone after the drain
		server.Redis(ctx, func(r server.RedisClient) {
			for shard := 0; shard < clientReliabilityStatsShardCount; shard += 1 {
				fields, _ := r.HGetAll(ctx, clientReliabilityStatsKey(blockNumber, shard)).Result()
				connect.AssertEqual(t, len(fields), 0)
			}
			members, _ := r.SMembers(ctx, clientReliabilityBlocksKey).Result()
			connect.AssertEqual(t, len(members), 0)
		})
	})
}

// Production defaults to the legacy writer until every taskworker can read
// shards. This models an old-reader/new-writer overlap: with the gate closed,
// the new binary still emits exactly the legacy hash the old rollup expects.
// TestClientReliabilityStatsShardedWriter asserts writers always shard: a
// recorded range lands in the shard key and never in the legacy unsharded key.
func TestClientReliabilityStatsShardedWriter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		hash := server.ClientIpHashForAddr(netip.MustParseAddr("10.9.0.1"))
		now := server.NowUtc()
		blockNumber := reliabilityBlockNumber(now)

		RecordClientReliabilityStatsRange(
			ctx,
			networkId,
			clientId,
			hash,
			now,
			now,
			&ClientReliabilityStats{ConnectionEstablishedCount: 1},
		)

		server.Redis(ctx, func(r server.RedisClient) {
			shard, err := r.HGetAll(
				ctx,
				clientReliabilityStatsKey(blockNumber, clientReliabilityStatsShard(clientId)),
			).Result()
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(shard), 1)
			legacy, err := r.HGetAll(ctx, clientReliabilityStatsLegacyKey(blockNumber)).Result()
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(legacy), 0)
		})
	})
}

// TestClientReliabilityStatsLegacyDrain simulates a rolling deploy: an
// old-build writer fills the legacy unsharded key with ascii fields while a
// new-build writer fills a shard key in the same block. One drain must merge
// both into pg and remove both keys.
func TestClientReliabilityStatsLegacyDrain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		legacyClientId := server.NewId()
		newClientId := server.NewId()

		legacyHash := server.ClientIpHashForAddr(netip.MustParseAddr("10.1.0.1"))
		newHash := server.ClientIpHashForAddr(netip.MustParseAddr("10.1.0.2"))

		now := server.NowUtc()
		blockNumber := reliabilityBlockNumber(now)

		// old-build writer: legacy key, ascii fields
		// (field = <hash hex>:<network_id>:<client_id>:<counter index>)
		server.Redis(ctx, func(r server.RedisClient) {
			legacyKey := clientReliabilityStatsLegacyKey(blockNumber)
			legacyPrefix := fmt.Sprintf("%x:%s:%s", legacyHash[:], networkId, legacyClientId)
			r.HIncrBy(ctx, legacyKey, fmt.Sprintf("%s:%d", legacyPrefix, reliabilityCounterConnectionEstablished), 1)
			r.HIncrBy(ctx, legacyKey, fmt.Sprintf("%s:%d", legacyPrefix, reliabilityCounterReceiveMessage), 5)
			r.SAdd(ctx, clientReliabilityBlocksKey, fmt.Sprintf("%d", blockNumber))
		})

		// new-build writer in the same block
		RecordClientReliabilityStatsRange(ctx, networkId, newClientId, newHash, now, now, &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ReceiveMessageCount:        3,
		})

		later := now.Add(3 * ReliabilityBlockDuration)
		RollupClientReliabilityStats(ctx, later)

		legacyCounters, _, ok := testingGetClientReliabilityRow(ctx, blockNumber, legacyHash, legacyClientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, legacyCounters["connection_established_count"], int64(1))
		connect.AssertEqual(t, legacyCounters["receive_message_count"], int64(5))

		newCounters, _, ok := testingGetClientReliabilityRow(ctx, blockNumber, newHash, newClientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, newCounters["connection_established_count"], int64(1))
		connect.AssertEqual(t, newCounters["receive_message_count"], int64(3))

		// both key forms removed by the drain
		server.Redis(ctx, func(r server.RedisClient) {
			legacyFields, _ := r.HGetAll(ctx, clientReliabilityStatsLegacyKey(blockNumber)).Result()
			connect.AssertEqual(t, len(legacyFields), 0)
			shardFields, _ := r.HGetAll(ctx, clientReliabilityStatsKey(blockNumber, clientReliabilityStatsShard(newClientId))).Result()
			connect.AssertEqual(t, len(shardFields), 0)
		})
	})
}

// TestClientReliabilityStatsDeployMixedDrain walks the rolling-deploy
// timeline across two blocks:
//   - block A (pre-deploy): a client written only through the legacy path;
//   - block B (mid-deploy): one client written through BOTH paths — its
//     syncs land on an old-build process (legacy ascii fields) and a
//     new-build process (packed shard fields) in the same block, with
//     overlapping and disjoint counter indexes — plus a legacy-only client
//     and a new-only client.
//
// One rollup must drain both blocks: the both-paths client's counters SUM
// across forms per counter index into a single pg row, the single-path
// clients drain normally, a re-run leaves the rows unchanged, and every key
// form of both blocks is removed.
func TestClientReliabilityStatsDeployMixedDrain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		preDeployClientId := server.NewId()
		mixedClientId := server.NewId()
		newOnlyClientId := server.NewId()

		preDeployHash := server.ClientIpHashForAddr(netip.MustParseAddr("10.2.0.1"))
		mixedHash := server.ClientIpHashForAddr(netip.MustParseAddr("10.2.0.2"))
		newOnlyHash := server.ClientIpHashForAddr(netip.MustParseAddr("10.2.0.3"))

		// block A is the previous block, still writable by the recorder and
		// drained in the same rollup as block B
		blockBTime := server.NowUtc()
		blockBNumber := reliabilityBlockNumber(blockBTime)
		blockANumber := blockBNumber - 1

		// legacy ascii writer, as an old-build process does it
		legacyIncr := func(r server.RedisClient, blockNumber int64, hash [32]byte, clientId server.Id, counterIndex int, count int64) {
			legacyKey := clientReliabilityStatsLegacyKey(blockNumber)
			field := fmt.Sprintf("%x:%s:%s:%d", hash[:], networkId, clientId, counterIndex)
			r.HIncrBy(ctx, legacyKey, field, count)
			r.SAdd(ctx, clientReliabilityBlocksKey, fmt.Sprintf("%d", blockNumber))
		}

		server.Redis(ctx, func(r server.RedisClient) {
			// block A: pre-deploy, legacy only
			legacyIncr(r, blockANumber, preDeployHash, preDeployClientId, reliabilityCounterConnectionEstablished, 1)
			legacyIncr(r, blockANumber, preDeployHash, preDeployClientId, reliabilityCounterReceiveMessage, 7)

			// block B: the same pre-deploy client still on an old process...
			legacyIncr(r, blockBNumber, preDeployHash, preDeployClientId, reliabilityCounterReceiveMessage, 2)
			// ...and the mixed client's sync that landed on an old process:
			// receive_message overlaps the new-path write below (must sum),
			// connection_new exists only in the legacy form
			legacyIncr(r, blockBNumber, mixedHash, mixedClientId, reliabilityCounterReceiveMessage, 10)
			legacyIncr(r, blockBNumber, mixedHash, mixedClientId, reliabilityCounterConnectionNew, 1)
		})

		// block B: the mixed client's other sync landed on a new-build process
		RecordClientReliabilityStatsRange(ctx, networkId, mixedClientId, mixedHash, blockBTime, blockBTime, &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ReceiveMessageCount:        3,
			SendByteCount:              64,
		})
		// and a purely new-build client
		RecordClientReliabilityStatsRange(ctx, networkId, newOnlyClientId, newOnlyHash, blockBTime, blockBTime, &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ReceiveMessageCount:        4,
		})

		// both blocks final -> one rollup drains A then B
		later := blockBTime.Add(3 * ReliabilityBlockDuration)
		RollupClientReliabilityStats(ctx, later)

		// block A: legacy-only row
		countersA, _, ok := testingGetClientReliabilityRow(ctx, blockANumber, preDeployHash, preDeployClientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, countersA["connection_established_count"], int64(1))
		connect.AssertEqual(t, countersA["receive_message_count"], int64(7))

		// block B: the pre-deploy client's continuing legacy writes
		countersPre, _, ok := testingGetClientReliabilityRow(ctx, blockBNumber, preDeployHash, preDeployClientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, countersPre["receive_message_count"], int64(2))

		// block B: the mixed client sums across forms per counter index
		countersMixed, _, ok := testingGetClientReliabilityRow(ctx, blockBNumber, mixedHash, mixedClientId)
		connect.AssertEqual(t, ok, true)
		// overlap: 10 legacy + 3 packed
		connect.AssertEqual(t, countersMixed["receive_message_count"], int64(13))
		// legacy-only counter
		connect.AssertEqual(t, countersMixed["connection_new_count"], int64(1))
		// packed-only counters
		connect.AssertEqual(t, countersMixed["connection_established_count"], int64(1))
		connect.AssertEqual(t, countersMixed["send_byte_count"], int64(64))

		// block B: the new-only client
		countersNew, _, ok := testingGetClientReliabilityRow(ctx, blockBNumber, newOnlyHash, newOnlyClientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, countersNew["connection_established_count"], int64(1))
		connect.AssertEqual(t, countersNew["receive_message_count"], int64(4))

		// re-running the rollup leaves the drained rows unchanged
		RollupClientReliabilityStats(ctx, later)
		countersMixed, _, ok = testingGetClientReliabilityRow(ctx, blockBNumber, mixedHash, mixedClientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, countersMixed["receive_message_count"], int64(13))

		// every key form of both blocks is removed
		server.Redis(ctx, func(r server.RedisClient) {
			for _, blockNumber := range []int64{blockANumber, blockBNumber} {
				legacyFields, _ := r.HGetAll(ctx, clientReliabilityStatsLegacyKey(blockNumber)).Result()
				connect.AssertEqual(t, len(legacyFields), 0)
				for shard := 0; shard < clientReliabilityStatsShardCount; shard += 1 {
					shardFields, _ := r.HGetAll(ctx, clientReliabilityStatsKey(blockNumber, shard)).Result()
					connect.AssertEqual(t, len(shardFields), 0)
				}
			}
			members, _ := r.SMembers(ctx, clientReliabilityBlocksKey).Result()
			connect.AssertEqual(t, len(members), 0)
		})
	})
}

// TestClientReliabilityStatsShardFunction pins the shard mapping properties:
// deterministic, in range, and derived from the client id alone.
func TestClientReliabilityStatsShardFunction(t *testing.T) {
	for i := 0; i < 1000; i += 1 {
		clientId := server.NewId()
		shard := clientReliabilityStatsShard(clientId)
		if shard < 0 || clientReliabilityStatsShardCount <= shard {
			t.Fatalf("shard %d out of range for %s", shard, clientId)
		}
		if shard != clientReliabilityStatsShard(clientId) {
			t.Fatalf("shard not deterministic for %s", clientId)
		}
	}
}
