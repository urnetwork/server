package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2025"
)

const ReliabilityBlockDuration = 60 * time.Second

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
				reliability_weight
			)
			SELECT
			    client_id,
			    SUM(1.0) AS independent_reliability_score,
			    SUM(1.0/w.valid_client_count) AS reliability_score,
			    SUM(1.0/w.valid_client_count) / ($2::bigint - $1::bigint + 1) AS reliability_weight
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
					block_number < $2
				GROUP BY block_number, client_address_hash
			) w ON
				w.block_number = client_reliability.block_number AND 
				w.client_address_hash = client_reliability.client_address_hash

			WHERE 
				client_reliability.valid = true AND
				$1 <= client_reliability.block_number AND
				client_reliability.block_number < $2

			GROUP BY client_id
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
			reliability_weight
		)
		SELECT
			network_id,
			SUM(1.0) AS independent_reliability_score,
		    SUM(1.0/w.valid_client_count) AS reliability_score,
		    SUM(1.0/w.valid_client_count) / ($2::bigint - $1::bigint + 1) AS reliability_weight
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
				block_number < $2
			GROUP BY block_number, client_address_hash
		) w ON
			w.block_number = client_reliability.block_number AND 
			w.client_address_hash = client_reliability.client_address_hash

		WHERE 
			client_reliability.valid = true AND
			$1 <= client_reliability.block_number AND
			client_reliability.block_number < $2

		GROUP BY network_id
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
