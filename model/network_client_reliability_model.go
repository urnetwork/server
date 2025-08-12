package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

const ReliabilityBlockSize = 60 * time.Second

type ConnectionReliabilityStats struct {
	ReceiveMessageCount        uint64
	ReceiveByteCount           ByteCount
	SendMessageCount           uint64
	SendByteCount              ByteCount
	ProvideEnabledCount        uint64
	ProvideChangeCount         uint64
	ConnectionEstablishedCount uint64
	ConnectionNewCount         uint64
}

func AddConnectionReliabilityStats(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	clientAddressHash []byte,
	statsTime time.Time,
	stats *ConnectionReliabilityStats,
) {
	blockNumber := statsTime.UTC().UnixMilli() / int64(ReliabilityBlockSize/time.Millisecond)

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
				connection_new_count = connection_new_count + $5,
		        connection_established_count = connection_established_count + $6,
		        provide_enabled_count = provide_enabled_count + $7,
		        provide_changed_count = provide_changed_count + $8,
		        receive_message_count = receive_message_count + $9,
		        receive_byte_count = receive_byte_count + $10,
		        send_message_count = send_message_count + $11,
		        send_byte_count = send_byte_count + $12
			`,
			blockNumber,
			clientAddressHash,
			networkId,
			clientId,
			stats.ConnectionNewCount,
			stats.ConnectionEstablishedCount,
			stats.ProvideEnabledCount,
			stats.ProvideChangeCount,
			stats.ReceiveMessageCount,
			stats.ReceiveByteCount,
			stats.SendMessageCount,
			stats.SendByteCount,
		))

	})
}

func RemoveOldConnectionReliabilityStats(ctx context.Context, minTime time.Time) {
	minBlockNumber := (minTime.UTC().UnixMilli() / int64(ReliabilityBlockSize/time.Millisecond)) - 1

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
	ReliabilityScore  float64
	ReliabilityWeight float64
}

// this should run regulalry to keep the client scores up to date
func UpdateClientReliabilityScores(ctx context.Context, minTime time.Time, maxTime time.Time) {
	now := server.NowUtc()
	minBlockNumber := minTime.UTC().UnixMilli() / int64(ReliabilityBlockSize/time.Millisecond)
	maxBlockNumber := maxTime.UTC().UnixMilli() / int64(ReliabilityBlockSize/time.Millisecond)

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			MERGE INTO client_connection_reliability_score
			USING (
				SELECT
					network_id,
				    client_id,
				    SUM(1/w.valid_client_count) AS weighted_valid_count
				FROM client_connection_reliability_blocks

				INNER JOIN (
					SELECT
						block_number,
						client_address_hash,
						COUNT(*) AS valid_client_count
					WHERE
						valid = true
					GROUP BY block_number, client_address_hash
				) w ON
					w.block_number = client_connection_reliability_blocks.block_number AND 
					w.client_address_hash = client_connection_reliability_blocks.client_address_hash

				WHERE
					$1 <= block_number AND
					block_number < $2 AND
					valid = true

				GROUP BY network_id, client_id
			) t
			ON
				t.client_id = client_connection_reliability_score.client_id AND
				w.block_number
				w.client_address_hash = client_connection_reliability_score.client_address_hash
			WHEN MATCHED THEN UPDATE SET
			    reliability_score = t.weighted_valid_count,
			    reliability_weight = t.weighted_valid_count / ($2 - $1 + 1)
			WHEN NOT MATCHED THEN INSERT (
				client_id,
				reliability_score,
				reliability_weight
			) VALUES (
				t.client_id,
				t.weighted_valid_count,
				t.weighted_valid_count / ($2 - $1 + 1),
			)
			WHEN NOT MATCHED BY SOURCE THEN DELETE
			`,
			minBlockNumber,
			maxBlockNumber,
		))
	})
}

func GetAllClientReliabilityScores(ctx context.Context) map[server.Id]ReliabilityScore {
	clientScores := map[server.Id]ReliabilityScore{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				client_id,
				reliability_score,
				reliability_weight
			FROM client_connection_reliability_score
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var s ReliabilityScore
				server.Raise(server.Scan(
					&clientId,
					&s.ReliabilityScore,
					&s.ReliabilityWeight,
				))
				clientScores[clientId] = s
			}
		})
	})

	return clientScores
}

// this should run on payout to compute the latest
func UpdateNetworkReliabilityScoresInTx(tx server.PgTx, ctx context.Context, minTime time.Time, maxTime time.Time) {
	now := server.NowUtc()
	minBlockNumber := minTime.UTC().UnixMilli() / int64(ReliabilityBlockSize/time.Millisecond)
	maxBlockNumber := maxTime.UTC().UnixMilli() / int64(ReliabilityBlockSize/time.Millisecond)

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		MERGE INTO network_connection_reliability_score
		USING (
			SELECT
				network_id,
			    SUM(1/w.valid_client_count) AS weighted_valid_count
			FROM client_connection_reliability_blocks

			INNER JOIN (
				SELECT
					block_number,
					client_address_hash,
					COUNT(*) AS valid_client_count
				WHERE
					valid = true
				GROUP BY block_number, client_address_hash
			) w ON
				w.block_number = client_connection_reliability_blocks.block_number AND 
				w.client_address_hash = client_connection_reliability_blocks.client_address_hash

			WHERE
				$1 <= block_number AND
				block_number < $2 AND
				valid = true

			GROUP BY network_id
		) t
		ON
			t.network_id = network_connection_reliability_score.network_id
		WHEN MATCHED THEN UPDATE SET
		    reliability_score = t.weighted_valid_count,
		    reliability_weight = t.weighted_valid_count / ($2 - $1 + 1)
		WHEN NOT MATCHED THEN INSERT (
			network_id,
			reliability_score,
			reliability_weight
		) VALUES (
			t.network_id,
			t.weighted_valid_count,
			t.weighted_valid_count / ($2 - $1 + 1),
		)
		WHEN NOT MATCHED BY SOURCE THEN DELETE
		`,
		minBlockNumber,
		maxBlockNumber,
	))
}

func GetAllNetworkReliabilityScores(ctx context.Context) map[server.Id]ReliabilityScore {
	networkScores := map[server.Id]ReliabilityScore{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_id,
				reliability_score,
				reliability_weight
			FROM network_connection_reliability_score
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var s ReliabilityScore
				server.Raise(server.Scan(
					&networkId,
					&s.ReliabilityScore,
					&s.ReliabilityWeight,
				))
				networkScores[networkId] = s
			}
		})
	})

	return networkScores
}
