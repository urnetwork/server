package model

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"net/netip"
	"slices"
	"testing"
	"time"

	"golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"

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
			assert.Equal(t, ok, true)
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
				assert.Equal(t, err, nil)
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
		orderedLookbackIndexes := maps.Keys(lookbackClientScores)
		slices.Sort(orderedLookbackIndexes)
		// use the max lookback
		clientScores := lookbackClientScores[orderedLookbackIndexes[len(orderedLookbackIndexes)-1]]
		for clientId, reliabilityScore := range netReliabilityScores {
			d := reliabilityScore - clientScores[clientId].ReliabilityScore
			if d < -eps || eps < d {
				assert.Equal(t, reliabilityScore, clientScores[clientId].ReliabilityScore)
			}
		}
		for clientId, indepententReliabilityScore := range netIndependentReliabilityScores {
			d := indepententReliabilityScore - clientScores[clientId].IndependentReliabilityScore
			if d < -eps || eps < d {
				assert.Equal(t, indepententReliabilityScore, clientScores[clientId].IndependentReliabilityScore)
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
					assert.Equal(t, netReliabilityScore, networkScores[networkId].ReliabilityScore)
				}
				d = netIndepententReliabilityScore - networkScores[networkId].IndependentReliabilityScore
				if d < -eps || eps < d {
					assert.Equal(t, netIndepententReliabilityScore, networkScores[networkId].IndependentReliabilityScore)
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
				assert.Equal(t, err, nil)
				assert.Equal(t, reliabilityWindow.MaxTotalClientCount, len(clientIds))
				for _, totalClientCount := range reliabilityWindow.TotalClientCounts {
					assert.Equal(t, totalClientCount, len(clientIds))
				}
				// reconstruct the total score from the weight
				windowReliabilityScore := reliabilityWindow.MeanReliabilityWeight * float64(int(reliabilityWindow.MaxBucketNumber-reliabilityWindow.MinBucketNumber)*blockCountPerBucket)
				d = windowReliabilityScore - networkScores[networkId].ReliabilityScore
				if d < -eps || eps < d {
					assert.Equal(t, windowReliabilityScore, networkScores[networkId].ReliabilityScore)
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
				server.Raise(result.Scan(
					&connectionNew,
					&connectionEstablished,
					&provideEnabled,
					&provideChanged,
					&receiveMessage,
					&receiveByte,
					&sendMessage,
					&sendByte,
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
		assert.Equal(t, ok, false)

		// after two more blocks both written blocks are final
		later := now.Add(3 * ReliabilityBlockDuration)
		RollupClientReliabilityStats(ctx, later)

		counters, valid, ok := testingGetClientReliabilityRow(ctx, blockNumber, clientAddressHash, clientId)
		assert.Equal(t, ok, true)
		assert.Equal(t, counters["connection_established_count"], int64(3))
		assert.Equal(t, counters["provide_enabled_count"], int64(3))
		assert.Equal(t, counters["receive_message_count"], int64(9))
		assert.Equal(t, counters["receive_byte_count"], int64(3072))
		assert.Equal(t, counters["send_message_count"], int64(6))
		assert.Equal(t, counters["send_byte_count"], int64(1536))
		assert.Equal(t, counters["connection_new_count"], int64(0))
		assert.Equal(t, valid, true)

		nextCounters, _, ok := testingGetClientReliabilityRow(ctx, blockNumber+1, clientAddressHash, clientId)
		assert.Equal(t, ok, true)
		assert.Equal(t, nextCounters["receive_message_count"], int64(3))

		// the drained buckets are removed from redis
		server.Redis(ctx, func(r server.RedisClient) {
			members, _ := r.SMembers(ctx, clientReliabilityBlocksKey).Result()
			assert.Equal(t, len(members), 0)
			fields, _ := r.HGetAll(ctx, clientReliabilityStatsKey(blockNumber)).Result()
			assert.Equal(t, len(fields), 0)
		})

		// re-drain is idempotent (absolute counts)
		RollupClientReliabilityStats(ctx, later)
		counters, _, ok = testingGetClientReliabilityRow(ctx, blockNumber, clientAddressHash, clientId)
		assert.Equal(t, ok, true)
		assert.Equal(t, counters["receive_message_count"], int64(9))

		// the high-water mark tracks the newest final block, even when idle
		maxDrainedBlock, ok := testingGetMaxDrainedBlock(ctx)
		assert.Equal(t, ok, true)
		assert.Equal(t, maxDrainedBlock, reliabilityBlockNumber(later)-2)

		// a record for a block older than the previous block is dropped, so a
		// stalled recorder can never write to an already-drained block
		staleTime := server.NowUtc().Add(-3 * ReliabilityBlockDuration)
		RecordClientReliabilityStatsRange(ctx, networkId, clientId, clientAddressHash, staleTime, staleTime, stats)
		server.Redis(ctx, func(r server.RedisClient) {
			fields, _ := r.HGetAll(ctx, clientReliabilityStatsKey(reliabilityBlockNumber(staleTime))).Result()
			assert.Equal(t, len(fields), 0)
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
		assert.Equal(t, err, nil)
		location := &Location{
			City:        "foo",
			Region:      "bar",
			Country:     "United States",
			CountryCode: "us",
		}
		CreateLocation(ctx, location)
		err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
		assert.Equal(t, err, nil)

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
				assert.Equal(t, score.IndependentReliabilityScore, 1.0)
				assert.Equal(t, score.ReliabilityScore, 1.0)
				found = true
			}
		}
		assert.Equal(t, found, true)
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

		connect := func(clientId server.Id, clientAddress string) [32]byte {
			connectionId, _, _, clientAddressHash, err := ConnectNetworkClient(ctx, clientId, clientAddress, server.NewId())
			assert.Equal(t, err, nil)
			err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
			assert.Equal(t, err, nil)
			return clientAddressHash
		}

		Testing_CreateDevice(ctx, networkId, server.NewId(), validClientId, "", "")
		Testing_CreateDevice(ctx, networkId, server.NewId(), invalidClientId, "", "")

		sharedAddressHash := connect(validClientId, "10.1.2.3:20000")
		// the invalid client is connected from two different ips, so its
		// client_address_hash_count is 2 and its location reliability is
		// invalid
		connect(invalidClientId, "10.1.2.3:20001")
		connect(invalidClientId, "10.99.2.3:20002")

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
		orderedLookbackIndexes := maps.Keys(lookbackClientScores)
		slices.Sort(orderedLookbackIndexes)
		clientScores := lookbackClientScores[orderedLookbackIndexes[len(orderedLookbackIndexes)-1]]

		// the location-invalid client earns no score
		_, ok := clientScores[invalidClientId]
		assert.Equal(t, ok, false)

		// the valid client's per-block contribution is diluted by the
		// stats-valid co-client on the same ip: 1/2 per block
		eps := 0.001
		score := clientScores[validClientId]
		assert.Equal(t, score.IndependentReliabilityScore, float64(n))
		if d := score.ReliabilityScore - float64(n)/2; d < -eps || eps < d {
			assert.Equal(t, score.ReliabilityScore, float64(n)/2)
		}

		// the network score and window score aggregate the same dilution
		UpdateNetworkReliabilityScores(ctx, startTime, endTime, false)
		networkScores := GetAllNetworkReliabilityScores(ctx)
		networkScore, ok := networkScores[networkId]
		assert.Equal(t, ok, true)
		assert.Equal(t, networkScore.IndependentReliabilityScore, float64(n))
		if d := networkScore.ReliabilityScore - float64(n)/2; d < -eps || eps < d {
			assert.Equal(t, networkScore.ReliabilityScore, float64(n)/2)
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
				assert.Equal(t, result.Next(), true)
				var independentReliabilityScore float64
				var reliabilityScore float64
				server.Raise(result.Scan(&independentReliabilityScore, &reliabilityScore))
				assert.Equal(t, independentReliabilityScore, float64(n))
				if d := reliabilityScore - float64(n)/2; d < -eps || eps < d {
					assert.Equal(t, reliabilityScore, float64(n)/2)
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
		assert.Equal(t, err, nil)
		location := &Location{
			City:        "foo",
			Region:      "bar",
			Country:     "United States",
			CountryCode: "us",
		}
		CreateLocation(ctx, location)
		err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
		assert.Equal(t, err, nil)

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
		assert.NotEqual(t, len(clientScores), 0)
		networkScores := GetAllNetworkReliabilityScores(ctx)
		_, ok := networkScores[networkId]
		assert.Equal(t, ok, true)

		// refresh far in the future: every window is past the data, so all
		// rows must be removed as stale
		farFuture := endTime.Add(NetworkWindowLookback + 24*time.Hour)
		UpdateClientReliabilityScores(ctx, farFuture, false)
		UpdateNetworkReliabilityScores(ctx, farFuture.Add(-time.Hour), farFuture, false)
		UpdateNetworkReliabilityWindowScores(ctx, farFuture, false)

		lookbackClientScores := GetAllClientReliabilityScores(ctx)
		for _, clientScores := range lookbackClientScores {
			assert.Equal(t, len(clientScores), 0)
		}
		networkScores = GetAllNetworkReliabilityScores(ctx)
		assert.Equal(t, len(networkScores), 0)

		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT COUNT(*) FROM network_connection_reliability_window_score`,
			)
			server.WithPgResult(result, err, func() {
				assert.Equal(t, result.Next(), true)
				var count int
				server.Raise(result.Scan(&count))
				assert.Equal(t, count, 0)
			})
		})
	})
}
