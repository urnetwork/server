package model

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"net/netip"
	"slices"
	"testing"
	"time"

	"maps"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestAddClientReliabilityStats(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ipCount := 30
		networkCount := 30
		minClientPerNetworkCount := 1
		maxClientPerNetworkCount := 8

		ips := []netip.Addr{}
		for range ipCount {
			ipv4 := make([]byte, 4)
			mathrand.Read(ipv4)
			ip, ok := netip.AddrFromSlice(ipv4)
			connect.AssertEqual(t, ok, true)
			ips = append(ips, ip)
		}

		networkClientIds := map[server.Id][]server.Id{}
		clientIps := map[server.Id]netip.Addr{}

		totalClientCount := 0
		for i := range networkCount {
			networkId := server.NewId()
			clientCount := minClientPerNetworkCount
			if d := maxClientPerNetworkCount - minClientPerNetworkCount; 0 < d {
				clientCount += mathrand.Intn(d)
			}
			for j := range clientCount {
				clientId := server.NewId()
				ip := ips[mathrand.Intn(len(ips))]

				networkClientIds[networkId] = append(networkClientIds[networkId], clientId)
				clientIps[clientId] = ip

				// connect the client
				Testing_CreateDevice(
					ctx,
					networkId,
					server.NewId(),
					clientId,
					"",
					"",
				)
				clientAddress := "127.0.0.1:20000"
				handlerId := server.NewId()
				connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, clientAddress, handlerId)
				connect.AssertEqual(t, err, nil)
				location := &Location{
					LocationType: "",
					City:         fmt.Sprintf("foo%d", j),
					Region:       fmt.Sprintf("bar%d", i),
					Country:      "United States",
					CountryCode:  "us",
				}
				CreateLocation(ctx, location)
				connectionLocationScores := &ConnectionLocationScores{}
				err = SetConnectionLocation(ctx, connectionId, location.LocationId, connectionLocationScores)

				// fmt.Printf("init client_id[%d] %s\n", totalClientCount, clientId)

				totalClientCount += 1
			}
		}

		netReliabilityScores := map[server.Id]float64{}
		netIndependentReliabilityScores := map[server.Id]float64{}

		// for each block, for each client, add one of stats:
		// - normal
		// - new connection
		// - provider change

		n := 128
		eps := float64(0.001)

		startTime := server.NowUtc()
		for i := range n {
			statsTime := startTime.Add(time.Duration(i) * ReliabilityBlockDuration)

			validIpCounts := map[[32]byte]int{}
			validBlocks := map[server.Id]int{}

			for networkId, clientIds := range networkClientIds {
				for _, clientId := range clientIds {
					ip := clientIps[clientId]
					clientAddressHash := server.ClientIpHashForAddr(ip)

					stats := &ClientReliabilityStats{}

					switch mathrand.Intn(3) {
					case 0:
						// connection new

						stats.ConnectionNewCount = uint64(1 + mathrand.Intn(4))
					case 1:
						// provide change

						stats.ProvideChangedCount = uint64(1 + mathrand.Intn(4))
					default:
						// normal

						stats.ConnectionEstablishedCount = uint64(1 + mathrand.Intn(4))
						stats.ProvideEnabledCount = uint64(1 + mathrand.Intn(4))
						stats.ReceiveMessageCount = uint64(1 + mathrand.Intn(4))
						stats.ReceiveByteCount = ByteCount(1024 + mathrand.Intn(8192))
						stats.SendMessageCount = uint64(1 + mathrand.Intn(4))
						stats.SendByteCount = ByteCount(1024 + mathrand.Intn(8192))

						validIpCounts[clientAddressHash] += 1
						validBlocks[clientId] += 1
					}

					AddClientReliabilityStats(
						ctx,
						networkId,
						clientId,
						clientAddressHash,
						statsTime,
						stats,
					)
				}
			}

			for clientId, count := range validBlocks {
				ip := clientIps[clientId]
				clientAddressHash := server.ClientIpHashForAddr(ip)
				ipCount := validIpCounts[clientAddressHash]

				netReliabilityScores[clientId] += float64(count) / float64(ipCount)
				netIndependentReliabilityScores[clientId] += float64(count)
			}
		}
		endTime := startTime.Add(time.Duration(n) * ReliabilityBlockDuration)

		UpdateClientReliabilityScores(ctx, endTime, true)

		lookbackClientScores := GetAllClientReliabilityScores(ctx)
		orderedLookbackIndexes := slices.Collect(maps.Keys(lookbackClientScores))
		slices.Sort(orderedLookbackIndexes)
		// use the max lookback
		clientScores := lookbackClientScores[orderedLookbackIndexes[len(orderedLookbackIndexes)-1]]
		for clientId, reliabilityScore := range netReliabilityScores {
			d := reliabilityScore - clientScores[clientId].ReliabilityScore
			if d < -eps || eps < d {
				connect.AssertEqual(t, reliabilityScore, clientScores[clientId].ReliabilityScore)
			}
		}
		for clientId, indepententReliabilityScore := range netIndependentReliabilityScores {
			d := indepententReliabilityScore - clientScores[clientId].IndependentReliabilityScore
			if d < -eps || eps < d {
				connect.AssertEqual(t, indepententReliabilityScore, clientScores[clientId].IndependentReliabilityScore)
			}
		}

		UpdateNetworkReliabilityScores(ctx, startTime, endTime, true)
		UpdateNetworkReliabilityWindow(ctx, startTime, endTime, true)

		blockCountPerBucket := ReliabilityBlockCountPerBucket()

		for _, networkScores := range []map[server.Id]ReliabilityScore{
			GetAllNetworkReliabilityScores(ctx),
			GetAllMultipliedNetworkReliabilityScores(ctx),
		} {
			for networkId, clientIds := range networkClientIds {
				netReliabilityScore := float64(0)
				netIndepententReliabilityScore := float64(0)
				for _, clientId := range clientIds {
					netReliabilityScore += netReliabilityScores[clientId]
					netIndepententReliabilityScore += netIndependentReliabilityScores[clientId]
				}
				d := netReliabilityScore - networkScores[networkId].ReliabilityScore
				if d < -eps || eps < d {
					connect.AssertEqual(t, netReliabilityScore, networkScores[networkId].ReliabilityScore)
				}
				d = netIndepententReliabilityScore - networkScores[networkId].IndependentReliabilityScore
				if d < -eps || eps < d {
					connect.AssertEqual(t, netIndepententReliabilityScore, networkScores[networkId].IndependentReliabilityScore)
				}

				isPro := false

				byJwt := jwt.NewByJwt(
					networkId,
					server.NewId(),
					"",
					false,
					isPro,
				).Client(clientIds[0], server.NewId())
				clientSession := session.Testing_CreateClientSession(
					ctx,
					byJwt,
				)
				clientSession.ClientAddress = "1.1.1.1:90000"

				reliabilityWindow, err := GetNetworkReliabilityWindow(clientSession)
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, reliabilityWindow.MaxTotalClientCount, len(clientIds))
				for _, totalClientCount := range reliabilityWindow.TotalClientCounts {
					connect.AssertEqual(t, totalClientCount, len(clientIds))
				}
				// reconstruct the total score from the weight
				windowReliabilityScore := reliabilityWindow.MeanReliabilityWeight * float64(int(reliabilityWindow.MaxBucketNumber-reliabilityWindow.MinBucketNumber)*blockCountPerBucket)
				d = windowReliabilityScore - networkScores[networkId].ReliabilityScore
				if d < -eps || eps < d {
					connect.AssertEqual(t, windowReliabilityScore, networkScores[networkId].ReliabilityScore)
				}
			}
		}

		RemoveOldClientReliabilityStats(ctx, endTime, 10000)

	})
}

// helper to read one client_reliability row back from pg
func testingGetClientReliabilityRow(
	ctx context.Context,
	blockNumber int64,
	clientAddressHash [32]byte,
	clientId server.Id,
) (counters map[string]int64, valid bool, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				connection_new_count,
				connection_established_count,
				provide_enabled_count,
				provide_changed_count,
				receive_message_count,
				receive_byte_count,
				send_message_count,
				send_byte_count,
				connection_excused_new_count,
				valid
			FROM client_reliability
			WHERE
				block_number = $1 AND
				client_address_hash = $2 AND
				client_id = $3
			`,
			blockNumber,
			clientAddressHash[:],
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				counters = map[string]int64{}
				var connectionNew, connectionEstablished, provideEnabled, provideChanged int64
				var receiveMessage, receiveByte, sendMessage, sendByte int64
				var connectionExcusedNew int64
				server.Raise(result.Scan(
					&connectionNew,
					&connectionEstablished,
					&provideEnabled,
					&provideChanged,
					&receiveMessage,
					&receiveByte,
					&sendMessage,
					&sendByte,
					&connectionExcusedNew,
					&valid,
				))
				counters["connection_new_count"] = connectionNew
				counters["connection_established_count"] = connectionEstablished
				counters["provide_enabled_count"] = provideEnabled
				counters["provide_changed_count"] = provideChanged
				counters["receive_message_count"] = receiveMessage
				counters["receive_byte_count"] = receiveByte
				counters["send_message_count"] = sendMessage
				counters["send_byte_count"] = sendByte
				counters["connection_excused_new_count"] = connectionExcusedNew
				ok = true
			}
		})
	})
	return
}

func testingGetMaxDrainedBlock(ctx context.Context) (maxDrainedBlock int64, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT max_drained_block FROM client_reliability_rollup WHERE singleton_id = 1`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&maxDrainedBlock))
				ok = true
			}
		})
	})
	return
}

// The announce hot path records to redis; the rollup drains closed blocks into
// pg with absolute counts. Cover: accumulation across records, the two-block
// finality rule, idempotent re-drain, redis bucket cleanup, the high-water
// mark, and the stale-write clamp.
func TestRecordClientReliabilityStatsRollup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		clientId := server.NewId()
		ip := netip.MustParseAddr("10.11.12.13")
		clientAddressHash := server.ClientIpHashForAddr(ip)

		now := server.NowUtc()
		blockNumber := reliabilityBlockNumber(now)

		stats := &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        3,
			ReceiveByteCount:           1024,
			SendMessageCount:           2,
			SendByteCount:              512,
		}

		// two records in the same block accumulate in redis
		RecordClientReliabilityStatsRange(ctx, networkId, clientId, clientAddressHash, now, now, stats)
		RecordClientReliabilityStatsRange(ctx, networkId, clientId, clientAddressHash, now, now, stats)

		// a range record spanning into the next block duplicates the stats
		// into each block, matching AddClientReliabilityStatsRange
		nextBlockTime := now.Add(ReliabilityBlockDuration)
		RecordClientReliabilityStatsRange(ctx, networkId, clientId, clientAddressHash, now, nextBlockTime, stats)

		// the block is not final yet: a rollup now must not drain it
		RollupClientReliabilityStats(ctx, now)
		_, _, ok := testingGetClientReliabilityRow(ctx, blockNumber, clientAddressHash, clientId)
		connect.AssertEqual(t, ok, false)

		// after two more blocks both written blocks are final
		later := now.Add(3 * ReliabilityBlockDuration)
		RollupClientReliabilityStats(ctx, later)

		counters, valid, ok := testingGetClientReliabilityRow(ctx, blockNumber, clientAddressHash, clientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, counters["connection_established_count"], int64(3))
		connect.AssertEqual(t, counters["provide_enabled_count"], int64(3))
		connect.AssertEqual(t, counters["receive_message_count"], int64(9))
		connect.AssertEqual(t, counters["receive_byte_count"], int64(3072))
		connect.AssertEqual(t, counters["send_message_count"], int64(6))
		connect.AssertEqual(t, counters["send_byte_count"], int64(1536))
		connect.AssertEqual(t, counters["connection_new_count"], int64(0))
		connect.AssertEqual(t, valid, true)

		nextCounters, _, ok := testingGetClientReliabilityRow(ctx, blockNumber+1, clientAddressHash, clientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, nextCounters["receive_message_count"], int64(3))

		// the drained buckets are removed from redis
		server.Redis(ctx, func(r server.RedisClient) {
			members, _ := r.SMembers(ctx, clientReliabilityBlocksKey).Result()
			connect.AssertEqual(t, len(members), 0)
			fields, _ := r.HGetAll(ctx, clientReliabilityStatsKey(blockNumber, clientReliabilityStatsShard(clientId))).Result()
			connect.AssertEqual(t, len(fields), 0)
		})

		// re-drain is idempotent (absolute counts)
		RollupClientReliabilityStats(ctx, later)
		counters, _, ok = testingGetClientReliabilityRow(ctx, blockNumber, clientAddressHash, clientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, counters["receive_message_count"], int64(9))

		// the high-water mark tracks the newest final block, even when idle
		maxDrainedBlock, ok := testingGetMaxDrainedBlock(ctx)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, maxDrainedBlock, reliabilityBlockNumber(later)-2)

		// a record for a block older than the previous block is dropped, so a
		// stalled recorder can never write to an already-drained block
		staleTime := server.NowUtc().Add(-3 * ReliabilityBlockDuration)
		RecordClientReliabilityStatsRange(ctx, networkId, clientId, clientAddressHash, staleTime, staleTime, stats)
		server.Redis(ctx, func(r server.RedisClient) {
			fields, _ := r.HGetAll(ctx, clientReliabilityStatsKey(reliabilityBlockNumber(staleTime), clientReliabilityStatsShard(clientId))).Result()
			connect.AssertEqual(t, len(fields), 0)
		})
	})
}

// End to end: record on the hot path, drain with the rollup, then compute
// scores over the drained blocks. The score windows must clamp to the rollup
// high-water mark so they only span fully drained blocks.
func TestRecordClientReliabilityStatsScores(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		clientId := server.NewId()

		// connect the client with a location so
		// network_client_location_reliability marks it valid
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		clientAddress := "127.0.0.1:20000"
		connectionId, _, _, clientAddressHash, err := ConnectNetworkClient(ctx, clientId, clientAddress, server.NewId())
		connect.AssertEqual(t, err, nil)
		location := &Location{
			City:        "foo",
			Region:      "bar",
			Country:     "United States",
			CountryCode: "us",
		}
		CreateLocation(ctx, location)
		err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
		connect.AssertEqual(t, err, nil)

		stats := &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
			ReceiveByteCount:           1024,
			SendMessageCount:           1,
			SendByteCount:              1024,
		}

		now := server.NowUtc()
		RecordClientReliabilityStatsRange(ctx, networkId, clientId, clientAddressHash, now, now, stats)

		later := now.Add(3 * ReliabilityBlockDuration)
		RollupClientReliabilityStats(ctx, later)

		UpdateClientReliabilityScores(ctx, later, true)

		lookbackClientScores := GetAllClientReliabilityScores(ctx)
		found := false
		for _, clientScores := range lookbackClientScores {
			if score, ok := clientScores[clientId]; ok {
				// one valid block, sole client on the ip
				connect.AssertEqual(t, score.IndependentReliabilityScore, 1.0)
				connect.AssertEqual(t, score.ReliabilityScore, 1.0)
				found = true
			}
		}
		connect.AssertEqual(t, found, true)
	})
}

// Two stats-valid clients share an ip; one of them is location-invalid
// (multiple connected ips). The location-invalid client earns no score, but it
// still dilutes the shared-ip normalization: valid_client_count counts by
// client_reliability.valid alone, matching the bucket window aggregation.
func TestClientReliabilityScoreSharedIpNormalization(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		validClientId := server.NewId()
		invalidClientId := server.NewId()

		location := &Location{
			City:        "foo",
			Region:      "bar",
			Country:     "United States",
			CountryCode: "us",
		}
		CreateLocation(ctx, location)

		connectHash := func(clientId server.Id, clientAddress string) [32]byte {
			connectionId, _, _, clientAddressHash, err := ConnectNetworkClient(ctx, clientId, clientAddress, server.NewId())
			connect.AssertEqual(t, err, nil)
			err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
			connect.AssertEqual(t, err, nil)
			return clientAddressHash
		}

		Testing_CreateDevice(ctx, networkId, server.NewId(), validClientId, "", "")
		Testing_CreateDevice(ctx, networkId, server.NewId(), invalidClientId, "", "")

		sharedAddressHash := connectHash(validClientId, "10.1.2.3:20000")
		// the invalid client is connected from two different ips, so its
		// client_address_hash_count is 2 and its location reliability is
		// invalid
		connectHash(invalidClientId, "10.1.2.3:20001")
		connectHash(invalidClientId, "10.99.2.3:20002")

		stats := &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
			ReceiveByteCount:           1024,
			SendMessageCount:           1,
			SendByteCount:              1024,
		}

		n := 4
		startTime := server.NowUtc()
		for i := range n {
			statsTime := startTime.Add(time.Duration(i) * ReliabilityBlockDuration)
			// both clients report stats-valid blocks on the shared ip
			AddClientReliabilityStats(ctx, networkId, validClientId, sharedAddressHash, statsTime, stats)
			AddClientReliabilityStats(ctx, networkId, invalidClientId, sharedAddressHash, statsTime, stats)
		}
		endTime := startTime.Add(time.Duration(n) * ReliabilityBlockDuration)

		UpdateClientReliabilityScores(ctx, endTime, true)

		lookbackClientScores := GetAllClientReliabilityScores(ctx)
		orderedLookbackIndexes := slices.Collect(maps.Keys(lookbackClientScores))
		slices.Sort(orderedLookbackIndexes)
		clientScores := lookbackClientScores[orderedLookbackIndexes[len(orderedLookbackIndexes)-1]]

		// the location-invalid client earns no score
		_, ok := clientScores[invalidClientId]
		connect.AssertEqual(t, ok, false)

		// the valid client's per-block contribution is diluted by the
		// stats-valid co-client on the same ip: 1/2 per block
		eps := 0.001
		score := clientScores[validClientId]
		connect.AssertEqual(t, score.IndependentReliabilityScore, float64(n))
		if d := score.ReliabilityScore - float64(n)/2; d < -eps || eps < d {
			connect.AssertEqual(t, score.ReliabilityScore, float64(n)/2)
		}

		// the network score and window score aggregate the same dilution
		UpdateNetworkReliabilityScores(ctx, startTime, endTime, false)
		networkScores := GetAllNetworkReliabilityScores(ctx)
		networkScore, ok := networkScores[networkId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, networkScore.IndependentReliabilityScore, float64(n))
		if d := networkScore.ReliabilityScore - float64(n)/2; d < -eps || eps < d {
			connect.AssertEqual(t, networkScore.ReliabilityScore, float64(n)/2)
		}

		UpdateNetworkReliabilityWindowScores(ctx, endTime, false)
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT
					independent_reliability_score,
					reliability_score
				FROM network_connection_reliability_window_score
				WHERE network_id = $1
				`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				connect.AssertEqual(t, result.Next(), true)
				var independentReliabilityScore float64
				var reliabilityScore float64
				server.Raise(result.Scan(&independentReliabilityScore, &reliabilityScore))
				connect.AssertEqual(t, independentReliabilityScore, float64(n))
				if d := reliabilityScore - float64(n)/2; d < -eps || eps < d {
					connect.AssertEqual(t, reliabilityScore, float64(n)/2)
				}
			})
		})
	})
}

// The score tables are refreshed by upsert + stale-row delete instead of
// delete-all + reinsert. When a later refresh window contains no data, every
// previously written row must be removed as stale.
func TestReliabilityScoreStaleRowsRemoved(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		clientId := server.NewId()

		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		connectionId, _, _, clientAddressHash, err := ConnectNetworkClient(ctx, clientId, "10.5.6.7:20000", server.NewId())
		connect.AssertEqual(t, err, nil)
		location := &Location{
			City:        "foo",
			Region:      "bar",
			Country:     "United States",
			CountryCode: "us",
		}
		CreateLocation(ctx, location)
		err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
		connect.AssertEqual(t, err, nil)

		stats := &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
			ReceiveByteCount:           1024,
			SendMessageCount:           1,
			SendByteCount:              1024,
		}

		startTime := server.NowUtc()
		AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, startTime, stats)
		endTime := startTime.Add(ReliabilityBlockDuration)

		UpdateClientReliabilityScores(ctx, endTime, true)
		UpdateNetworkReliabilityScores(ctx, startTime, endTime, false)
		UpdateNetworkReliabilityWindowScores(ctx, endTime, false)

		clientScores := GetAllClientReliabilityScores(ctx)
		connect.AssertNotEqual(t, len(clientScores), 0)
		networkScores := GetAllNetworkReliabilityScores(ctx)
		_, ok := networkScores[networkId]
		connect.AssertEqual(t, ok, true)

		// refresh far in the future: every window is past the data, so all
		// rows must be removed as stale
		farFuture := endTime.Add(NetworkWindowLookback + 24*time.Hour)
		UpdateClientReliabilityScores(ctx, farFuture, false)
		UpdateNetworkReliabilityScores(ctx, farFuture.Add(-time.Hour), farFuture, false)
		UpdateNetworkReliabilityWindowScores(ctx, farFuture, false)

		lookbackClientScores := GetAllClientReliabilityScores(ctx)
		for _, clientScores := range lookbackClientScores {
			connect.AssertEqual(t, len(clientScores), 0)
		}
		networkScores = GetAllNetworkReliabilityScores(ctx)
		connect.AssertEqual(t, len(networkScores), 0)

		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT COUNT(*) FROM network_connection_reliability_window_score`,
			)
			server.WithPgResult(result, err, func() {
				connect.AssertEqual(t, result.Next(), true)
				var count int
				server.Raise(result.Scan(&count))
				connect.AssertEqual(t, count, 0)
			})
		})
	})
}

func testingGetReliabilitySyncRanges(ctx context.Context) (ranges [][2]int64) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT min_block_number, max_block_number
			FROM client_reliability_sync
			ORDER BY min_block_number
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var minBlockNumber int64
				var maxBlockNumber int64
				server.Raise(result.Scan(&minBlockNumber, &maxBlockNumber))
				ranges = append(ranges, [2]int64{minBlockNumber, maxBlockNumber})
			}
		})
	})
	return
}

// Coverage range bookkeeping for the drained blocks: contiguous drains extend
// the newest range, re-drains are no-ops, a drain after lost blocks starts a
// new range, and the covered count treats pre-first-range blocks as covered
// while returning 0 for an entirely uncovered window.
func TestReliabilityCoveredBlockCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		covered := func(minBlockNumber int64, maxBlockNumber int64) (coveredBlockCount int64) {
			server.Tx(ctx, func(tx server.PgTx) {
				coveredBlockCount = reliabilityCoveredBlockCount(ctx, tx, minBlockNumber, maxBlockNumber)
			})
			return
		}

		b := int64(1000000)

		// no coverage rows at all: the full window width counts
		connect.AssertEqual(t, covered(b, b+10), int64(10))

		coverClientReliabilityBlock(ctx, b)
		// contiguous: extends the newest range
		coverClientReliabilityBlock(ctx, b+1)
		// re-drain of a covered block: no change
		coverClientReliabilityBlock(ctx, b+1)
		// after a gap (blocks b+2..b+4 lost): starts a new range
		coverClientReliabilityBlock(ctx, b+5)

		connect.AssertEqual(t, testingGetReliabilitySyncRanges(ctx), [][2]int64{{b, b + 1}, {b + 5, b + 5}})

		// blocks before the first range count as covered
		connect.AssertEqual(t, covered(b-10, b), int64(10))
		// the lost blocks are uncovered
		connect.AssertEqual(t, covered(b, b+6), int64(3))
		connect.AssertEqual(t, covered(b, b+7), int64(3))
		// an entirely uncovered window returns 0
		connect.AssertEqual(t, covered(b+2, b+5), int64(0))

		// expiration deletes fully expired ranges and clamps the straddler
		blockTime := func(blockNumber int64) time.Time {
			return time.UnixMilli(blockNumber * int64(ReliabilityBlockDuration/time.Millisecond)).UTC()
		}
		RemoveOldClientReliabilityStats(ctx, blockTime(b+2).Add(ClientExpiration), 1000)
		connect.AssertEqual(t, testingGetReliabilitySyncRanges(ctx), [][2]int64{{b + 1, b + 1}, {b + 5, b + 5}})
	})
}

// Redis counters lost before the rollup drains them (redis restart/expiry,
// drain outage) leave blocks outside every coverage range. The score weights
// normalize by covered blocks only, so the loss must not register as client
// unreliability.
func TestClientReliabilityDrainGapExcused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		clientId := server.NewId()

		// connect the client with a location so
		// network_client_location_reliability marks it valid
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		connectionId, _, _, clientAddressHash, err := ConnectNetworkClient(ctx, clientId, "127.0.0.1:20000", server.NewId())
		connect.AssertEqual(t, err, nil)
		location := &Location{
			City:        "foo",
			Region:      "bar",
			Country:     "United States",
			CountryCode: "us",
		}
		CreateLocation(ctx, location)
		err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
		connect.AssertEqual(t, err, nil)

		stats := &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
			ReceiveByteCount:           1024,
			SendMessageCount:           1,
			SendByteCount:              1024,
		}

		base := server.NowUtc()

		// block b0 is recorded and drained
		RecordClientReliabilityStatsRange(ctx, networkId, clientId, clientAddressHash, base, base, stats)
		RollupClientReliabilityStats(ctx, base.Add(3*ReliabilityBlockDuration))

		// blocks base+1..base+4 are lost before draining (nothing recorded)

		// block base+5 is recorded and drained
		b5Time := base.Add(5 * ReliabilityBlockDuration)
		RecordClientReliabilityStatsRange(ctx, networkId, clientId, clientAddressHash, b5Time, b5Time, stats)
		scoreTime := base.Add(8 * ReliabilityBlockDuration)
		RollupClientReliabilityStats(ctx, scoreTime)

		UpdateClientReliabilityScores(ctx, scoreTime, true)

		// the shortest lookback window spans only the lossy region: the client
		// reported in every covered block there, so its weight is a full 1.0
		// (without coverage the gap would register as 1 present / 6 blocks)
		lookbackClientScores := GetAllClientReliabilityScores(ctx)
		score, ok := lookbackClientScores[0][clientId]
		connect.AssertEqual(t, ok, true)
		eps := 0.001
		connect.AssertEqual(t, score.IndependentReliabilityScore, 1.0)
		if d := score.IndependentReliabilityWeight - 1.0; d < -eps || eps < d {
			connect.AssertEqual(t, score.IndependentReliabilityWeight, 1.0)
		}
		if d := score.ReliabilityWeight - 1.0; d < -eps || eps < d {
			connect.AssertEqual(t, score.ReliabilityWeight, 1.0)
		}
	})
}

// The reliability stats wait for the redis->pg drain: a rollup high-water
// mark that has not advanced recently reads as not synced. Environments where
// the rollup never runs (fixtures write pg directly) read as synced.
func TestClientReliabilityRollupSynced(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		now := server.NowUtc()

		// no rollup record yet
		connect.AssertEqual(t, ClientReliabilityRollupSynced(ctx, now), true)

		RollupClientReliabilityStats(ctx, now)
		connect.AssertEqual(t, ClientReliabilityRollupSynced(ctx, now), true)
		connect.AssertEqual(t, ClientReliabilityRollupSynced(ctx, now.Add(ReliabilityRollupStaleAfter+time.Minute)), false)
	})
}

// The block validity rule tolerates exactly one reconnect. A reconnect drops
// the provider's live clients, so it is real user impact: repeated reconnects
// (flapping) still invalidate the block. But a single reconnect used to
// invalidate it too, and at the hour threshold one invalid block takes a
// provider out of the market for an hour — so any handler rotation, mobile
// blip, or NAT rebind disqualified an otherwise perfect provider.
func TestClientReliabilityReconnectTolerated(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		ip := netip.MustParseAddr("10.20.30.40")
		clientAddressHash := server.ClientIpHashForAddr(ip)

		now := server.NowUtc()

		// each case is one block's accumulated counters
		validFor := func(stats *ClientReliabilityStats) bool {
			clientId := server.NewId()
			AddClientReliabilityStats(ctx, networkId, clientId, clientAddressHash, now, stats)
			_, valid, ok := testingGetClientReliabilityRow(
				ctx,
				reliabilityBlockNumber(now),
				clientAddressHash,
				clientId,
			)
			connect.AssertEqual(t, ok, true)
			return valid
		}

		// steady state: established, providing, passing traffic
		connect.AssertEqual(t, validFor(&ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
		}), true)

		// one reconnect, then re-established in the same block: tolerated
		connect.AssertEqual(t, validFor(&ClientReliabilityStats{
			ConnectionNewCount:         1,
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
		}), true)

		// flapping: two reconnects in one block is still unreliable
		connect.AssertEqual(t, validFor(&ClientReliabilityStats{
			ConnectionNewCount:         2,
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
		}), false)

		// a reconnect with no re-established sync in the block (the connection
		// came back across the block boundary) is still invalid — the hour
		// threshold carries the slack for this
		connect.AssertEqual(t, validFor(&ClientReliabilityStats{
			ConnectionNewCount:  1,
			ProvideEnabledCount: 1,
			ReceiveMessageCount: 1,
		}), false)

		// the other invalidating conditions are unchanged
		connect.AssertEqual(t, validFor(&ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ProvideChangedCount:        1,
			ReceiveMessageCount:        1,
		}), false)
		connect.AssertEqual(t, validFor(&ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
		}), false)
	})
}

// Degraded-block classification uses a LOCAL (60-block) median: a sharp
// synchronized drop (deploy, drain hiccup) inside an otherwise-healthy
// neighborhood is excused, while gradual/diurnal change is not. This also
// documents the KNOWN LIMITATION (see reliabilityDegradedMedianBlockCount): a
// SUSTAINED multi-hour collapse adapts the local median down and is NOT
// excused -- its blocks age out of the lookbacks instead. A future two-signal
// event detector should flip the second assertion.
func TestClientReliabilityDegradedLocalMedian(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		base := int64(30_000_000)
		server.Tx(ctx, func(tx server.PgTx) {
			server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
				// healthy hour, one sharp-drop block in the middle
				for b := base - 60; b < base; b += 1 {
					valid := int64(1000)
					if b == base-30 {
						valid = 300
					}
					batch.Queue(
						`INSERT INTO client_reliability_block (block_number, client_count, valid_client_count) VALUES ($1, $2, $3)`,
						b, 1200, valid,
					)
				}
				// a later sustained 2h collapse
				for b := base; b < base+120; b += 1 {
					batch.Queue(
						`INSERT INTO client_reliability_block (block_number, client_count, valid_client_count) VALUES ($1, $2, $3)`,
						b, 400, 300,
					)
				}
			})
		})

		// the sharp drop inside the healthy neighborhood is excused
		var degraded []int64
		server.Tx(ctx, func(tx server.PgTx) {
			degraded = reliabilityDegradedBlocks(ctx, tx, base-60, base)
		})
		connect.AssertEqual(t, len(degraded), 1)
		connect.AssertEqual(t, degraded[0], base-30)

		// deep inside the sustained collapse the local median has adapted:
		// nothing is excused (the documented limitation -- these blocks age out
		// of the lookbacks rather than being excused)
		server.Tx(ctx, func(tx server.PgTx) {
			degraded = reliabilityDegradedBlocks(ctx, tx, base+60, base+120)
		})
		connect.AssertEqual(t, len(degraded), 0)
	})
}

// A drain-excused reconnect is recorded as `connection_excused_new_count`
// instead of `connection_new_count`, so it never enters
// `client_reliability_valid` and cannot invalidate the block
// (CONNECTDRAIN2.md §3.1).
func TestClientReliabilityExcusedNewValid(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		ip := netip.MustParseAddr("10.11.12.14")
		clientAddressHash := server.ClientIpHashForAddr(ip)

		now := server.NowUtc()
		blockNumber := reliabilityBlockNumber(now)

		establishedStats := &ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
		}

		// an excused reconnect plus an established sync in the same block
		// keeps the block valid
		excusedClientId := server.NewId()
		RecordClientReliabilityStatsRange(ctx, networkId, excusedClientId, clientAddressHash, now, now, &ClientReliabilityStats{
			ConnectionExcusedNewCount: 1,
		})
		RecordClientReliabilityStatsRange(ctx, networkId, excusedClientId, clientAddressHash, now, now, establishedStats)

		// an organic reconnect within the forgiveness keeps the block valid
		organicClientId := server.NewId()
		RecordClientReliabilityStatsRange(ctx, networkId, organicClientId, clientAddressHash, now, now, &ClientReliabilityStats{
			ConnectionNewCount: 1,
		})
		RecordClientReliabilityStatsRange(ctx, networkId, organicClientId, clientAddressHash, now, now, establishedStats)

		// a flapping client past the forgiveness invalidates the block
		flappingClientId := server.NewId()
		RecordClientReliabilityStatsRange(ctx, networkId, flappingClientId, clientAddressHash, now, now, &ClientReliabilityStats{
			ConnectionNewCount: 2,
		})
		RecordClientReliabilityStatsRange(ctx, networkId, flappingClientId, clientAddressHash, now, now, establishedStats)

		later := now.Add(3 * ReliabilityBlockDuration)
		RollupClientReliabilityStats(ctx, later)

		counters, valid, ok := testingGetClientReliabilityRow(ctx, blockNumber, clientAddressHash, excusedClientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, counters["connection_excused_new_count"], int64(1))
		connect.AssertEqual(t, counters["connection_new_count"], int64(0))
		connect.AssertEqual(t, valid, true)

		_, valid, ok = testingGetClientReliabilityRow(ctx, blockNumber, clientAddressHash, organicClientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, valid, true)

		_, valid, ok = testingGetClientReliabilityRow(ctx, blockNumber, clientAddressHash, flappingClientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, valid, false)

		// the direct pg fixture path carries the excused counter too
		fixtureClientId := server.NewId()
		AddClientReliabilityStats(ctx, networkId, fixtureClientId, clientAddressHash, now, &ClientReliabilityStats{
			ConnectionExcusedNewCount:  1,
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
		})
		counters, valid, ok = testingGetClientReliabilityRow(ctx, blockNumber, clientAddressHash, fixtureClientId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, counters["connection_excused_new_count"], int64(1))
		connect.AssertEqual(t, valid, true)
	})
}
