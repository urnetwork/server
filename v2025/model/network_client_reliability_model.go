package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2025"
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
			ON CONFLICT (block_number, client_address_hash, network_id, client_id) DO UPDATE
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

// this should run regulalry to keep the client scores up to date
func UpdateClientReliabilityScores(ctx context.Context, minTime time.Time, maxTime time.Time) {
	minBlockNumber := minTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)
	maxBlockNumber := maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)

	server.Tx(ctx, func(tx server.PgTx) {
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
				max_block_number
			)
			SELECT
			    client_reliability.client_id,
			    SUM(1.0) AS independent_reliability_score,
			    SUM(1.0/w.valid_client_count) AS reliability_score,
			    SUM(1.0/w.valid_client_count) / ($2::bigint - $1::bigint + 1) AS reliability_weight,
			    $1 AS min_block_number,
			    $2 AS max_block_number
			FROM client_reliability

			INNER JOIN (
				SELECT
					block_number,
					client_address_hash,
					COUNT(*) AS valid_client_count
				FROM client_reliability
				WHERE
					valid = true AND
					$1 <= block_number AND
					block_number <= $2
				GROUP BY block_number, client_address_hash
			) w ON
				w.block_number = client_reliability.block_number AND
				w.client_address_hash = client_reliability.client_address_hash

			WHERE
				client_reliability.valid = true AND
				$1 <= client_reliability.block_number AND
				client_reliability.block_number <= $2

			GROUP BY client_reliability.client_id
			`,
			minBlockNumber,
			maxBlockNumber,
		))
	})
}

func GetAllClientReliabilityScores(ctx context.Context) (clientScores map[server.Id]ReliabilityScore) {
	server.Tx(ctx, func(tx server.PgTx) {
		clientScores = GetAllClientReliabilityScoresInTx(tx, ctx)
	})
	return
}

func GetAllClientReliabilityScoresInTx(tx server.PgTx, ctx context.Context) map[server.Id]ReliabilityScore {
	clientScores := map[server.Id]ReliabilityScore{}

	result, err := tx.Query(
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

	return clientScores
}

func UpdateNetworkReliabilityScores(ctx context.Context, minTime time.Time, maxTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		UpdateNetworkReliabilityScoresInTx(tx, ctx, minTime, maxTime)
	})
}

// this should run on payout to compute the latest
func UpdateNetworkReliabilityScoresInTx(tx server.PgTx, ctx context.Context, minTime time.Time, maxTime time.Time) {
	minBlockNumber := minTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)
	maxBlockNumber := maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)

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
			independent_reliability_score,
			reliability_score,
			reliability_weight,
			min_block_number,
			max_block_number
		)
		SELECT
			client_reliability.network_id,
			SUM(1.0) AS independent_reliability_score,
		    SUM(1.0/w.valid_client_count) AS reliability_score,
		    SUM(1.0/w.valid_client_count) / ($2::bigint - $1::bigint + 1) AS reliability_weight,
		    $1 AS min_block_number,
		    $2 AS max_block_number
		FROM client_reliability

		INNER JOIN (
			SELECT
				block_number,
				client_address_hash,
				COUNT(*) AS valid_client_count
			FROM client_reliability
			WHERE
				valid = true AND
				$1 <= block_number AND
				block_number <= $2
			GROUP BY block_number, client_address_hash
		) w ON
			w.block_number = client_reliability.block_number AND
			w.client_address_hash = client_reliability.client_address_hash

		WHERE
			client_reliability.valid = true AND
			$1 <= client_reliability.block_number AND
			client_reliability.block_number <= $2

		GROUP BY client_reliability.network_id
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

	// bucket number -> weight
	ReliabilityWeights map[int64]float64 `json:"reliability_weights"`
	// bucket number -> count
	ClientCounts map[int64]int `json:"client_counts"`
	// bucket number -> count
	TotalClientCounts map[int64]int `json:"client_counts"`
}

func GetNetworkReliabilityWindow(ctx context.Context, networkId server.Id) (reliabilityWindow *ReliabilityWindow) {
	server.Tx(ctx, func(tx server.PgTx) {
		reliabilityWindow = GetNetworkReliabilityWindowInTx(tx, ctx, networkId)
	})
	return
}

func GetNetworkReliabilityWindowInTx(tx server.PgTx, ctx context.Context, networkId server.Id) *ReliabilityWindow {
	result, err := tx.Query(
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
			if maxBucketNumber < 0 || maxBucketNumber < bucketNumber {
				maxBucketNumber = bucketNumber
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

	return &ReliabilityWindow{
		MeanReliabilityWeight: netReliabilityWeight / float64(maxBucketNumber+1-minBucketNumber),
		MinTimeUnixMilli:      time.UnixMilli(0).Add(time.Duration(minBucketNumber) * ReliabilityWindowBucketDuration).UnixMilli(),
		MinBucketNumber:       minBucketNumber,
		MaxTimeUnixMilli:      time.UnixMilli(0).Add(time.Duration(maxBucketNumber+1) * ReliabilityWindowBucketDuration).UnixMilli(),
		MaxBucketNumber:       maxBucketNumber + 1,
		BucketDurationSeconds: int(ReliabilityWindowBucketDuration / time.Second),
		MaxClientCount:        maxClientCount,
		MaxTotalClientCount:   maxTotalClientCount,
		ReliabilityWeights:    reliabilityWeights,
		ClientCounts:          clientCounts,
		TotalClientCounts:     totalClientCounts,
	}
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
		UpdateNetworkReliabilityWindowInTx(tx, ctx, minTime, maxTime)
	})
}

func UpdateNetworkReliabilityWindowInTx(tx server.PgTx, ctx context.Context, minTime time.Time, maxTime time.Time) {
	minBlockNumber := minTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)
	maxBlockNumber := maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)

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
			client_count
		)
		SELECT
			client_reliability.network_id,
			client_reliability.block_number / $3 AS bucket_number,
		    SUM(1.0/w.valid_client_count) / $3 AS reliability_weight,
		    COUNT(DISTINCT client_reliability.client_id) AS client_count

		FROM client_reliability

		INNER JOIN (
			SELECT
				block_number,
				client_address_hash,
				COUNT(*) AS valid_client_count
			FROM client_reliability
			WHERE
				valid = true AND
				$1 <= block_number AND
				block_number <= $2
			GROUP BY block_number, client_address_hash
		) w ON
			w.block_number = client_reliability.block_number AND
			w.client_address_hash = client_reliability.client_address_hash

		WHERE
			client_reliability.valid = true AND
			$1 <= client_reliability.block_number AND
			client_reliability.block_number <= $2

		GROUP BY client_reliability.network_id, (client_reliability.block_number / $3)
		`,
		minBlockNumber,
		maxBlockNumber,
		blockCountPerBucket,
	))

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO network_connection_reliability_window (
			network_id,
			bucket_number,
			total_client_count
		)
		SELECT
			network_id,
			block_number / $3 AS bucket_number,
		    COUNT(DISTINCT client_id) AS total_client_count

		FROM client_reliability

		WHERE
			$1 <= block_number AND
			block_number <= $2

		GROUP BY network_id, (block_number / $3)

		ON CONFLICT (network_id, bucket_number) DO UPDATE
		SET
			total_client_count = network_connection_reliability_window.total_client_count + EXCLUDED.total_client_count
		`,
		minBlockNumber,
		maxBlockNumber,
		blockCountPerBucket,
	))
}
