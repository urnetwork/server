package model

import (
	"context"
	"net/netip"
	"time"

	"golang.org/x/exp/maps"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const ReliabilityBlockDuration = 60 * time.Second
const ReliabilityWindowBucketDuration = 15 * time.Minute

type ClientReliabilityStats struct {
	ReceiveMessageCount        uint64
	ReceiveByteCount           ByteCount
	SendMessageCount           uint64
	SendByteCount              ByteCount
	ProvideEnabledCount        uint64
	ProvideChangedCount        uint64
	ConnectionEstablishedCount uint64
	ConnectionNewCount         uint64
}

func AddClientReliabilityStats(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	clientAddressHash [32]byte,
	statsTime time.Time,
	stats *ClientReliabilityStats,
) {
	blockNumber := statsTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO client_reliability (
				block_number,
				client_address_hash,
				network_id,
				client_id,
				connection_new_count,
		        connection_established_count,
		        provide_enabled_count,
		        provide_changed_count,
		        receive_message_count,
		        receive_byte_count,
		        send_message_count,
		        send_byte_count
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (block_number, client_address_hash, client_id) DO UPDATE
			SET
				connection_new_count = client_reliability.connection_new_count + $5,
		        connection_established_count = client_reliability.connection_established_count + $6,
		        provide_enabled_count = client_reliability.provide_enabled_count + $7,
		        provide_changed_count = client_reliability.provide_changed_count + $8,
		        receive_message_count = client_reliability.receive_message_count + $9,
		        receive_byte_count = client_reliability.receive_byte_count + $10,
		        send_message_count = client_reliability.send_message_count + $11,
		        send_byte_count = client_reliability.send_byte_count + $12
			`,
			blockNumber,
			clientAddressHash[:],
			networkId,
			clientId,
			stats.ConnectionNewCount,
			stats.ConnectionEstablishedCount,
			stats.ProvideEnabledCount,
			stats.ProvideChangedCount,
			stats.ReceiveMessageCount,
			stats.ReceiveByteCount,
			stats.SendMessageCount,
			stats.SendByteCount,
		))

	})
}

func RemoveOldClientReliabilityStats(ctx context.Context, minTime time.Time) {
	minBlockNumber := (minTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)) - 1

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM client_reliability
			WHERE block_number <= $1
			`,
			minBlockNumber,
		))
	})
}

type ReliabilityScore struct {
	IndependentReliabilityScore float64
	ReliabilityScore            float64
	ReliabilityWeight           float64
}

// FIXME support country_location_id
// this should run regulalry to keep the client scores up to date
func UpdateClientReliabilityScores(ctx context.Context, minTime time.Time, maxTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		UpdateClientLocationReliabilitiesInTx(tx, ctx, minTime, maxTime)

		minBlockNumber := minTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)
		maxBlockNumber := (maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)) + 1

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM client_connection_reliability_score
			`,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO client_connection_reliability_score (
				client_id,
				independent_reliability_score,
				reliability_score,
				reliability_weight,
				min_block_number,
				max_block_number,
				city_location_id,
				region_location_id,
				country_location_id
			)
			SELECT
			    t.client_id,
			    SUM(1.0) AS independent_reliability_score,
			    SUM(1.0/t.valid_client_count) AS reliability_score,
			    SUM(1.0/t.valid_client_count) / ($2::bigint - $1::bigint) AS reliability_weight,
			    $1 AS min_block_number,
				$2 AS max_block_number,
				t.city_location_id,
				t.region_location_id,
				t.country_location_id
			FROM (
				SELECT
					client_reliability.client_id,
					network_client_location_reliability.city_location_id,
					network_client_location_reliability.region_location_id,
					network_client_location_reliability.country_location_id,
			        COUNT(*) OVER (PARTITION BY client_reliability.block_number, client_reliability.client_address_hash) valid_client_count
			    FROM client_reliability
			    INNER JOIN network_client_location_reliability ON
					network_client_location_reliability.client_id = client_reliability.client_id AND
					network_client_location_reliability.valid = true
			    WHERE
					client_reliability.valid = true AND
			    	$1 <= client_reliability.block_number AND
			    	client_reliability.block_number <= $2
			) t
			GROUP BY t.client_id, t.city_location_id, t.region_location_id, t.country_location_id
			ORDER BY t.client_id
			`,
			minBlockNumber,
			maxBlockNumber,
		))

	}, server.TxReadCommitted)
}

func GetAllClientReliabilityScores(ctx context.Context) (clientScores map[server.Id]ReliabilityScore) {
	server.Db(ctx, func(conn server.PgConn) {
		clientScores = map[server.Id]ReliabilityScore{}

		result, err := conn.Query(
			ctx,
			`
			SELECT
				client_id,
				independent_reliability_score,
				reliability_score,
				reliability_weight
			FROM client_connection_reliability_score
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var s ReliabilityScore
				server.Raise(result.Scan(
					&clientId,
					&s.IndependentReliabilityScore,
					&s.ReliabilityScore,
					&s.ReliabilityWeight,
				))
				clientScores[clientId] = s
			}
		})
	})
	return
}

func UpdateNetworkReliabilityScores(ctx context.Context, minTime time.Time, maxTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		UpdateNetworkReliabilityScoresInTx(tx, ctx, minTime, maxTime)
	}, server.TxReadCommitted)
}

// FIXME support country_location_id
// this should run on payout to compute the latest
func UpdateNetworkReliabilityScoresInTx(tx server.PgTx, ctx context.Context, minTime time.Time, maxTime time.Time) {
	UpdateClientLocationReliabilitiesInTx(tx, ctx, minTime, maxTime)

	minBlockNumber := minTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)
	maxBlockNumber := (maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)) + 1

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		DELETE FROM network_connection_reliability_score
		`,
	))

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO network_connection_reliability_score (
			network_id,
			country_location_id,
			independent_reliability_score,
			reliability_score,
			reliability_weight,
			min_block_number,
			max_block_number
		)
		SELECT
		    t.network_id,
		    t.country_location_id,
		    SUM(1.0) AS independent_reliability_score,
		    SUM(1.0/t.valid_client_count) AS reliability_score,
		    SUM(1.0/t.valid_client_count) / ($2::bigint - $1::bigint) AS reliability_weight,
		    $1 AS min_block_number,
			$2 AS max_block_number
		FROM (
			SELECT
				client_reliability.network_id,
				network_client_location_reliability.country_location_id,
		        COUNT(*) OVER (PARTITION BY block_number, client_address_hash) valid_client_count
		    FROM client_reliability
		    INNER JOIN network_client_location_reliability ON
				network_client_location_reliability.client_id = client_reliability.client_id AND
				network_client_location_reliability.valid = true
		    WHERE
		    	client_reliability.valid = true AND
		    	$1 <= client_reliability.block_number AND
		    	client_reliability.block_number <= $2
		) t
		GROUP BY t.network_id, t.country_location_id
		ORDER BY t.network_id, t.country_location_id
		`,
		minBlockNumber,
		maxBlockNumber,
	))
}

func GetAllNetworkReliabilityScores(ctx context.Context) (networkScores map[server.Id]ReliabilityScore) {
	server.Tx(ctx, func(tx server.PgTx) {
		networkScores = GetAllNetworkReliabilityScoresInTx(tx, ctx)
	})
	return
}

func GetAllNetworkReliabilityScoresInTx(tx server.PgTx, ctx context.Context) map[server.Id]ReliabilityScore {
	networkScores := map[server.Id]ReliabilityScore{}

	result, err := tx.Query(
		ctx,
		`
		SELECT
			network_id,
			independent_reliability_score,
			reliability_score,
			reliability_weight
		FROM network_connection_reliability_score
		`,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var networkId server.Id
			var s ReliabilityScore
			server.Raise(result.Scan(
				&networkId,
				&s.IndependentReliabilityScore,
				&s.ReliabilityScore,
				&s.ReliabilityWeight,
			))
			if c, ok := networkScores[networkId]; ok {
				s.IndependentReliabilityScore += c.IndependentReliabilityScore
				s.ReliabilityScore += c.ReliabilityScore
				s.ReliabilityWeight += c.ReliabilityWeight
			}
			networkScores[networkId] = s
		}
	})

	return networkScores
}

func GetAllMultipliedNetworkReliabilityScores(ctx context.Context) (networkScores map[server.Id]ReliabilityScore) {
	server.Tx(ctx, func(tx server.PgTx) {
		networkScores = GetAllMultipliedNetworkReliabilityScoresInTx(tx, ctx)
	})
	return
}

func GetAllMultipliedNetworkReliabilityScoresInTx(tx server.PgTx, ctx context.Context) map[server.Id]ReliabilityScore {
	networkScores := map[server.Id]ReliabilityScore{}

	result, err := tx.Query(
		ctx,
		`
		SELECT
			network_id,
			independent_reliability_score * COALESCE(network_client_location_reliability_multiplier.reliability_multiplier, 1.0) AS independent_reliability_score,
			reliability_score * COALESCE(network_client_location_reliability_multiplier.reliability_multiplier, 1.0) AS reliability_score,
			reliability_weight * COALESCE(network_client_location_reliability_multiplier.reliability_multiplier, 1.0) AS reliability_weight
		FROM network_connection_reliability_score

		LEFT JOIN network_client_location_reliability_multiplier ON
			network_client_location_reliability_multiplier.country_location_id = network_connection_reliability_score.country_location_id
		`,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var networkId server.Id
			var s ReliabilityScore
			server.Raise(result.Scan(
				&networkId,
				&s.IndependentReliabilityScore,
				&s.ReliabilityScore,
				&s.ReliabilityWeight,
			))
			if c, ok := networkScores[networkId]; ok {
				s.IndependentReliabilityScore += c.IndependentReliabilityScore
				s.ReliabilityScore += c.ReliabilityScore
				s.ReliabilityWeight += c.ReliabilityWeight
			}
			networkScores[networkId] = s
		}
	})

	return networkScores
}

type ReliabilityWindow struct {
	MeanReliabilityWeight float64 `json:"mean_reliability_weight"`
	MinTimeUnixMilli      int64   `json:"min_time_unix_milli"`
	MinBucketNumber       int64   `json:"min_bucket_number"`
	MaxTimeUnixMilli      int64   `json:"max_time_unix_milli"`
	// exclusive
	MaxBucketNumber       int64 `json:"max_bucket_number"`
	BucketDurationSeconds int   `json:"bucket_duration_seconds"`

	MaxClientCount int `json:"max_client_count"`
	// valid+invalid
	MaxTotalClientCount int `json:"max_total_client_count"`

	// relative bucket number = (bucket number) - (min bucket number)

	// indexed by relative bucket number
	ReliabilityWeights []float64 `json:"reliability_weights"`
	// indexed by relative bucket number
	ClientCounts []int `json:"client_counts"`
	// indexed by relative bucket number
	TotalClientCounts []int `json:"total_client_counts"`

	CountryMultipliers []*CountryMultiplier `json:"country_multipliers"`
}

type CountryMultiplier struct {
	CountryLocationId     server.Id `json:"country_location_id"`
	Country               string    `json:"country"`
	CountryCode           string    `json:"country_code"`
	ReliabilityMultiplier float64   `json:"reliability_multiplier"`
}

func GetNetworkReliabilityWindow(clientSession *session.ClientSession) (reliabilityWindow *ReliabilityWindow, err error) {
	networkId := clientSession.ByJwt.NetworkId
	var clientIp netip.Addr
	clientIp, _, err = clientSession.ParseClientIpPort()
	if err != nil {
		return nil, err
	}
	reliabilityWindow = getNetworkReliabilityWindow(clientSession.Ctx, networkId, &clientIp)
	return
}

func getNetworkReliabilityWindow(
	ctx context.Context,
	networkId server.Id,
	clientIp *netip.Addr,
) (reliabilityWindow *ReliabilityWindow) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				bucket_number,
		        reliability_weight,
		        client_count,
		        total_client_count
			FROM network_connection_reliability_window

			WHERE
				network_id = $1
			`,
			networkId,
		)

		netReliabilityWeight := float64(0)
		minBucketNumber := int64(-1)
		// inclusive
		maxBucketNumber := int64(-1)

		maxClientCount := 0
		maxTotalClientCount := 0

		reliabilityWeights := map[int64]float64{}
		clientCounts := map[int64]int{}
		totalClientCounts := map[int64]int{}

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var bucketNumber int64
				var reliabilityWeight float64
				var clientCount int
				var totalClientCount int
				server.Raise(result.Scan(
					&bucketNumber,
					&reliabilityWeight,
					&clientCount,
					&totalClientCount,
				))

				netReliabilityWeight += reliabilityWeight
				if minBucketNumber < 0 || bucketNumber < minBucketNumber {
					minBucketNumber = bucketNumber
				}
				if maxBucketNumber < 0 || maxBucketNumber < bucketNumber+1 {
					maxBucketNumber = bucketNumber + 1
				}

				if maxClientCount < clientCount {
					maxClientCount = clientCount
				}
				if maxTotalClientCount < totalClientCount {
					maxTotalClientCount = totalClientCount
				}

				reliabilityWeights[bucketNumber] = reliabilityWeight
				if 0 < clientCount {
					clientCounts[bucketNumber] = clientCount
				}
				if 0 < totalClientCount {
					totalClientCounts[bucketNumber] = totalClientCount
				}
			}
		})

		countryMultipliers := map[server.Id]*CountryMultiplier{}

		result, err = conn.Query(
			ctx,
			`
			SELECT
				network_connection_reliability_score.country_location_id,
				location.location_name,
				location.country_code,
				COALESCE(network_client_location_reliability_multiplier.reliability_multiplier, 1.0) AS reliability_multiplier
			FROM network_connection_reliability_score
			INNER JOIN location ON
				location.location_id = network_connection_reliability_score.country_location_id
			LEFT JOIN network_client_location_reliability_multiplier ON
				network_client_location_reliability_multiplier.country_location_id = network_connection_reliability_score.country_location_id
			WHERE network_connection_reliability_score.network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				m := &CountryMultiplier{}
				server.Raise(result.Scan(
					&m.CountryLocationId,
					&m.Country,
					&m.CountryCode,
					&m.ReliabilityMultiplier,
				))
				countryMultipliers[m.CountryLocationId] = m
			}
		})

		// include the caller ip for completeness
		if clientIp != nil {
			ipInfo, err := server.GetIpInfo(*clientIp)
			if err == nil && ipInfo.CountryCode != "" {
				result, err := conn.Query(
					ctx,
					`
					SELECT
						location.location_id,
						location.location_name,
						location.country_code,
						COALESCE(network_client_location_reliability_multiplier.reliability_multiplier, 1.0) AS reliability_multiplier
					FROM location
					INNER JOIN network_client_location_reliability_multiplier ON
						network_client_location_reliability_multiplier.country_location_id = location.location_id
					WHERE
						location.location_type = $1 AND
						location.country_code = $2
					`,
					LocationTypeCountry,
					ipInfo.CountryCode,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						m := &CountryMultiplier{}
						server.Raise(result.Scan(
							&m.CountryLocationId,
							&m.Country,
							&m.CountryCode,
							&m.ReliabilityMultiplier,
						))
						countryMultipliers[m.CountryLocationId] = m
					}
				})
			}
		}

		n := maxBucketNumber - minBucketNumber
		reliabilityWeightsSlice := make([]float64, n)
		clientCountsSlice := make([]int, n)
		totalClientCountsSlice := make([]int, n)

		for i := range n {
			bucketNumber := minBucketNumber + i
			reliabilityWeightsSlice[i] = reliabilityWeights[bucketNumber]
			clientCountsSlice[i] = clientCounts[bucketNumber]
			totalClientCountsSlice[i] = totalClientCounts[bucketNumber]
		}

		reliabilityWindow = &ReliabilityWindow{
			MeanReliabilityWeight: netReliabilityWeight / float64(maxBucketNumber-minBucketNumber),
			MinTimeUnixMilli:      time.UnixMilli(0).Add(time.Duration(minBucketNumber) * ReliabilityWindowBucketDuration).UnixMilli(),
			MinBucketNumber:       minBucketNumber,
			MaxTimeUnixMilli:      time.UnixMilli(0).Add(time.Duration(maxBucketNumber) * ReliabilityWindowBucketDuration).UnixMilli(),
			MaxBucketNumber:       maxBucketNumber,
			BucketDurationSeconds: int(ReliabilityWindowBucketDuration / time.Second),
			MaxClientCount:        maxClientCount,
			MaxTotalClientCount:   maxTotalClientCount,
			ReliabilityWeights:    reliabilityWeightsSlice,
			ClientCounts:          clientCountsSlice,
			TotalClientCounts:     totalClientCountsSlice,
			CountryMultipliers:    maps.Values(countryMultipliers),
		}
	})
	return
}

func ReliabilityBlockCountPerBucket() int {
	return max(
		1,
		// round
		int((ReliabilityWindowBucketDuration+ReliabilityBlockDuration/2)/ReliabilityBlockDuration),
	)
}

func UpdateNetworkReliabilityWindow(ctx context.Context, minTime time.Time, maxTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		UpdateNetworkReliabilityScoresInTx(tx, ctx, minTime, maxTime)

		minBlockNumber := minTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)
		maxBlockNumber := (maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)) + 1

		blockCountPerBucket := ReliabilityBlockCountPerBucket()

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM network_connection_reliability_window
			`,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_connection_reliability_window (
				network_id,
				bucket_number,
				reliability_weight,
				client_count,
				total_client_count
			)
			SELECT
			    t.network_id,
			    t.bucket_number,
			    SUM(CASE WHEN t.valid = true THEN 1.0/t.valid_client_count ELSE 0 END) / $3 AS reliability_weight,
			    COUNT(DISTINCT t.client_id) FILTER (WHERE t.valid = true) AS client_count,
			    COUNT(DISTINCT t.client_id) AS total_client_count
			FROM (
				SELECT
					valid,
					network_id,
					client_id,
					block_number / $3 AS bucket_number,
			        COUNT(*) FILTER (WHERE valid = true) OVER (PARTITION BY block_number, client_address_hash) valid_client_count
			    FROM client_reliability
			    WHERE
			    	$1 <= block_number AND
			    	block_number <= $2
			) t
			GROUP BY t.network_id, t.bucket_number
			ORDER BY t.network_id, t.bucket_number
			`,
			minBlockNumber,
			maxBlockNumber,
			blockCountPerBucket,
		))
	}, server.TxReadCommitted)
}

type cityRegionCountry struct {
	cityLocationId    server.Id
	regionLocationId  server.Id
	countryLocationId server.Id
}

type clientLocationReliability struct {
	locations map[cityRegionCountry]int

	clientAddressHashes map[[32]byte]int

	netTypeScores      map[int]int
	netTypeScoreSpeeds map[int]int
}

// server.ComplexValue
func (self *clientLocationReliability) Values() []any {
	// [0] city_location_id
	// [1] region_location_id
	// [2] country_location_id
	// [3] client_address_hash_count
	// [4] location_count
	// [5] max_net_type_score
	// [6] max_net_type_score_speed

	values := make([]any, 7)
	if 1 == len(self.locations) {
		location := maps.Keys(self.locations)[0]
		values[0] = &location.cityLocationId
		values[1] = &location.regionLocationId
		values[2] = &location.countryLocationId
	}
	// else leave locations nil

	values[3] = len(self.clientAddressHashes)
	values[4] = len(self.locations)
	// values[5] = self.connected

	maxNetTypeScore := 0
	for netTypeScore, _ := range self.netTypeScores {
		maxNetTypeScore = max(maxNetTypeScore, netTypeScore)
	}
	values[5] = maxNetTypeScore

	maxNetTypeScoreSpeed := 0
	for netTypeScoreSpeed, _ := range self.netTypeScoreSpeeds {
		maxNetTypeScoreSpeed = max(maxNetTypeScoreSpeed, netTypeScoreSpeed)
	}
	values[6] = maxNetTypeScoreSpeed

	return values
}

// this should be called regularly
// a valid client will have one connected location and one connected address hash
func UpdateClientLocationReliabilitiesInTx(tx server.PgTx, ctx context.Context, minTime time.Time, maxTime time.Time) {

	updateBlockNumber := maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)

	// old entries are not deleted on each update, but the connected status is updated
	// - connected clients are updated, and the valid state is reset to match the latest
	// - if a disconnected client is not in `network_client_location_reliability`,
	//   the most recent disconnected connection is used to update the values

	// for each client, summarize all the active locations

	result, err := tx.Query(
		ctx,
		`
		SELECT
			network_client_location.client_id,
			network_client_connection.client_address_hash,	
			network_client_location.city_location_id,
	        network_client_location.region_location_id,
	        network_client_location.country_location_id,
			network_client_location.net_type_score,
			network_client_location.net_type_score_speed

		FROM network_client_connection

		INNER JOIN network_client_location ON
			network_client_location.connection_id = network_client_connection.connection_id

		WHERE
			network_client_connection.connected = true
		`,
	)

	clientLocationReliabilities := map[server.Id]*clientLocationReliability{}
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var clientId server.Id
			var clientAddressHash [32]byte
			var cityLocationId server.Id
			var regionLocationId server.Id
			var countryLocationId server.Id
			var netTypeScore int
			var netTypeScoreSpeed int
			clientAddressHashSlice := clientAddressHash[:]
			server.Raise(result.Scan(
				&clientId,
				&clientAddressHashSlice,
				&cityLocationId,
				&regionLocationId,
				&countryLocationId,
				&netTypeScore,
				&netTypeScoreSpeed,
			))
			r, ok := clientLocationReliabilities[clientId]
			if !ok {
				r = &clientLocationReliability{
					locations:           map[cityRegionCountry]int{},
					clientAddressHashes: map[[32]byte]int{},
					netTypeScores:       map[int]int{},
					netTypeScoreSpeeds:  map[int]int{},
				}
				clientLocationReliabilities[clientId] = r
			}
			r.locations[cityRegionCountry{
				cityLocationId:    cityLocationId,
				regionLocationId:  regionLocationId,
				countryLocationId: countryLocationId,
			}] += 1
			r.clientAddressHashes[clientAddressHash] += 1
			r.netTypeScores[netTypeScore] += 1
			r.netTypeScoreSpeeds[netTypeScoreSpeed] += 1
		}
	})

	result, err = tx.Query(
		ctx,
		`
		SELECT
			network_client_location.client_id,
			network_client_connection.client_address_hash,	
			network_client_location.city_location_id,
	        network_client_location.region_location_id,
	        network_client_location.country_location_id,
			network_client_location.net_type_score,
			network_client_location.net_type_score_speed

		FROM network_client_connection

		INNER JOIN network_client_location ON
			network_client_location.connection_id = network_client_connection.connection_id

		LEFT JOIN network_client_location_reliability ON
			network_client_location_reliability.client_id = network_client_connection.client_id

		WHERE
			network_client_connection.connected = false AND
			$1 <= network_client_connection.disconnect_time AND
			network_client_connection.disconnect_time < $2 AND
			network_client_location_reliability.client_id IS NULL

		ORDER BY network_client_connection.disconnect_time DESC

		`,
		minTime,
		maxTime,
	)

	// if client id does not exist, fill it in
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var clientId server.Id
			var clientAddressHash [32]byte
			var cityLocationId server.Id
			var regionLocationId server.Id
			var countryLocationId server.Id
			var netTypeScore int
			var netTypeScoreSpeed int
			clientAddressHashSlice := clientAddressHash[:]
			server.Raise(result.Scan(
				&clientId,
				&clientAddressHashSlice,
				&cityLocationId,
				&regionLocationId,
				&countryLocationId,
				&netTypeScore,
				&netTypeScoreSpeed,
			))
			r, ok := clientLocationReliabilities[clientId]
			if !ok {
				r = &clientLocationReliability{
					locations:           map[cityRegionCountry]int{},
					clientAddressHashes: map[[32]byte]int{},
					netTypeScores:       map[int]int{},
					netTypeScoreSpeeds:  map[int]int{},
				}
				clientLocationReliabilities[clientId] = r

				r.locations[cityRegionCountry{
					cityLocationId:    cityLocationId,
					regionLocationId:  regionLocationId,
					countryLocationId: countryLocationId,
				}] += 1
				r.clientAddressHashes[clientAddressHash] += 1
				r.netTypeScores[netTypeScore] += 1
				r.netTypeScoreSpeeds[netTypeScoreSpeed] += 1
			}
			// else there is already an entry, don't update
		}
	})

	server.CreateTempJoinTableInTx(
		ctx,
		tx,
		`
			temp_network_client_location_reliability(
				client_id uuid ->
				city_location_id uuid NULL,
	            region_location_id uuid NULL,
	            country_location_id uuid NULL,
	            client_address_hash_count int,
	            location_count int,
	            max_net_type_score smallint,
	            max_net_type_score_speed smallint
	        )
	    `,
		clientLocationReliabilities,
	)

	server.RaisePgResult(tx.Exec(
		ctx,
		`
	    INSERT INTO network_client_location_reliability (
	    	client_id,
	    	update_block_number,
			city_location_id,
	        region_location_id,
	        country_location_id,
	        client_address_hash_count,
	        location_count,
	        connected,
	        max_net_type_score,
	        max_net_type_score_speed
	    )
	    SELECT
	    	client_id,
	    	$1 AS update_block_number,
	    	city_location_id,
	        region_location_id,
	        country_location_id,
	        client_address_hash_count,
	        location_count,
	        true AS connected,
	        max_net_type_score,
	        max_net_type_score_speed
	    FROM temp_network_client_location_reliability
	    ORDER BY client_id
	    ON CONFLICT (client_id) DO UPDATE
	    SET
	    	update_block_number = $1,
	    	city_location_id = EXCLUDED.city_location_id,
	        region_location_id = EXCLUDED.region_location_id,
	        country_location_id = EXCLUDED.country_location_id,
	        client_address_hash_count = EXCLUDED.client_address_hash_count,
	        location_count = EXCLUDED.location_count,
	        connected = true,
	        max_net_type_score = EXCLUDED.max_net_type_score,
	        max_net_type_score_speed = EXCLUDED.max_net_type_score_speed
	    `,
		updateBlockNumber,
	))

	// TODO on pg17 this could be part of a MERGE with source missing
	server.RaisePgResult(tx.Exec(
		ctx,
		`
	    UPDATE network_client_location_reliability
	    SET
	    	connected = false
	    FROM (
	    	SELECT
	    		network_client_location_reliability.client_id
	    	FROM network_client_location_reliability
	    	LEFT JOIN temp_network_client_location_reliability ON
	    		temp_network_client_location_reliability.client_id = network_client_location_reliability.client_id
	    	WHERE temp_network_client_location_reliability.client_id IS NULL
	    ) t
	    WHERE
	    	network_client_location_reliability.client_id = t.client_id AND
	    	network_client_location_reliability.connected = true
	    `,
	))

	// result, err = tx.Query(
	// 	ctx,
	// 	`
	// 	SELECT client_id FROM network_client_location_reliability
	// 	WHERE valid = true AND connected = true AND city_location_id IS NOT NULL AND region_location_id IS NOT NULL AND country_location_id IS NOT NULL
	// 	ORDER BY client_id
	// 	`,
	// )
	// server.WithPgResult(result, err, func() {
	// 	for i := 0; result.Next(); i += 1 {
	// 		var clientId server.Id
	// 		server.Raise(result.Scan(&clientId))
	// 		fmt.Printf("valid connected client_id[%d] %s\n", i, clientId)
	// 	}
	// })
}

func RemoveOldClientLocationReliabilities(ctx context.Context, minTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		minBlockNumber := (minTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)) - 1

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM network_client_location_reliability
			WHERE update_block_number <= $1
			`,
			minBlockNumber,
		))
	})
}

func UpdateClientLocationReliabilityMultipliersWithDefaults(ctx context.Context) {
	server.Tx(ctx, func(tx server.PgTx) {
		UpdateClientLocationReliabilityMultipliersWithDefaultsInTx(tx, ctx)
	})
}

func UpdateClientLocationReliabilityMultipliersWithDefaultsInTx(tx server.PgTx, ctx context.Context) {
	c := EnvSubsidyConfig()
	UpdateClientLocationReliabilityMultipliersInTx(
		tx,
		ctx,
		c.CountryReliabilityWeightTarget,
		c.MaxCountryReliabilityMultiplier,
	)
}

// call this at the end of plan payments
func UpdateClientLocationReliabilityMultipliersInTx(
	tx server.PgTx,
	ctx context.Context,
	reliabilityWeightTarget float64,
	maxMultiplier float64,
) {
	// based total network weights per country, compute multipliers

	result, err := tx.Query(
		ctx,
		`
		SELECT
			country_location_id,
			SUM(reliability_weight) AS net_reliability_weight
		FROM (
			SELECT
				country_location_id,
				reliability_weight

			FROM network_connection_reliability_score

			UNION ALL 

			SELECT
				country_location_id,
				0 AS reliability_weight
			FROM location
			WHERE location_type = 'country'
		) t

		GROUP BY country_location_id
		`,
	)

	countryNetReliabilityWeights := map[server.Id]float64{}
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var countryLocationId server.Id
			var netReliabilityWeight float64
			server.Raise(result.Scan(&countryLocationId, &netReliabilityWeight))
			countryNetReliabilityWeights[countryLocationId] += netReliabilityWeight
		}
	})

	countryReliabilityMuplipliers := map[server.Id]float64{}
	for locationId, netReliabilityWeight := range countryNetReliabilityWeights {
		multiplier := maxMultiplier
		if 0.1 < netReliabilityWeight {
			multiplier = min(multiplier, reliabilityWeightTarget/netReliabilityWeight)
		}
		multiplier = max(multiplier, 1.0)
		countryReliabilityMuplipliers[locationId] = multiplier
	}

	server.CreateTempJoinTableInTx(
		ctx,
		tx,
		"temp_reliability_multiplier(country_location_id uuid -> reliability_multiplier double precision)",
		countryReliabilityMuplipliers,
	)

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		DELETE FROM network_client_location_reliability_multiplier
		`,
	))

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO network_client_location_reliability_multiplier (
			country_location_id,
			reliability_multiplier
		)
		SELECT
			country_location_id,
			reliability_multiplier
		FROM temp_reliability_multiplier
		`,
	))
}

func GetAllClientLocationReliabilityMultipliers(ctx context.Context) (countryMultipliers map[server.Id]*CountryMultiplier) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_client_location_reliability_multiplier.country_location_id,
				location.location_name,
				location.country_code,
				network_client_location_reliability_multiplier.reliability_multiplier
			FROM network_client_location_reliability_multiplier
			INNER JOIN location ON
				location.location_id = network_client_location_reliability_multiplier.country_location_id
			`,
		)
		countryMultipliers = map[server.Id]*CountryMultiplier{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				m := &CountryMultiplier{}
				server.Raise(result.Scan(
					&m.CountryLocationId,
					&m.Country,
					&m.CountryCode,
					&m.ReliabilityMultiplier,
				))
				countryMultipliers[m.CountryLocationId] = m
			}
		})
	})
	return countryMultipliers
}
