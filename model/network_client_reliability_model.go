package model

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/exp/maps"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// reliability data is materialized from the core `client_reliability` table
// which merged in other tables.
// there are several materialized tables that each have different update expectations.
// - `client_reliability` (`*ClientReliabilityStats*`):
//     can be updated in parallel
// - `network_client_location_reliability` (`*ClientLocationReliabilities*`):
//     can be updated in parallel
// - `client_connection_reliability_score` (`*ClientReliabilityScores*`):
//     must be updated by at most one caller at a time.
//     a serial task is expected to update this
// - `network_connection_reliability_score` (`*NetworkReliabilityScores*`) and,
//   `network_client_location_reliability_multiplier` (`*UpdateClientLocationReliabilityMultipliers*`):
//     must be updated by at most one caller at a time.
//     the payout serial task is expected to update this
// - `network_connection_reliability_window` (`*NetworkReliabilityWindow*`) and,
//   `network_connection_reliability_window_score` (`*NetworkReliabilityWindowScores*`):
//     must be updated by at most one caller at a time
//     a serial task is expected to update this

const ClientExpiration = 30 * 24 * time.Hour

var ClientLookbacks = []time.Duration{
	5 * time.Minute,
	60 * time.Minute,
	12 * time.Hour,
	// 6 * 24 * time.Hour,
}

const NetworkWindowLookback = 7 * 24 * time.Hour
const NetworkWindowExpiration = 15 * 24 * time.Hour

const ClientLocationExpiration = 30 * 24 * time.Hour

// How many disconnects a client is forgiven within one block before the block
// counts against it. A disconnect drops the provider's live clients, so it is
// real user impact and repeated disconnects (flapping) must still fail -- but
// at zero tolerance a SINGLE reconnect invalidated the whole block, and at the
// hour threshold one invalid block takes a provider out of the market for an
// hour, so every handler rotation, mobile blip, and NAT rebind disqualified an
// otherwise perfect provider.
//
// Both writers pass this to the `client_reliability_valid` sql function, which
// is the one place the block validity rule is written. It applies to blocks
// written from here on; rows already in the table keep the value they were
// written with.
const ReliabilityAllowDisconnectCountPerBlock = 1

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

// AddClientReliabilityStats writes stats directly to pg. This is the
// fixture/backfill path — the announce hot path must use
// RecordClientReliabilityStatsRange (redis + rollup) instead, since direct
// per-sync upserts were the single largest statement load on the database.
func AddClientReliabilityStats(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	clientAddressHash [32]byte,
	statsTime time.Time,
	stats *ClientReliabilityStats,
) {
	AddClientReliabilityStatsRange(ctx, networkId, clientId, clientAddressHash, statsTime, statsTime, stats)
}

func AddClientReliabilityStatsRange(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	clientAddressHash [32]byte,
	statsStartTime time.Time,
	statsEndTime time.Time,
	stats *ClientReliabilityStats,
) {
	startBlockNumber := statsStartTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)
	endBlockNumber := statsEndTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)

	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for blockNumber := startBlockNumber; blockNumber <= endBlockNumber; blockNumber += 1 {
				batch.Queue(
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
				        send_byte_count,
				        valid
					) VALUES (
						$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
						client_reliability_valid($5, $6, $7, $8, $9, $13)
					)
					ON CONFLICT (block_number, client_address_hash, client_id) DO UPDATE
					SET
						connection_new_count = client_reliability.connection_new_count + $5,
				        connection_established_count = client_reliability.connection_established_count + $6,
				        provide_enabled_count = client_reliability.provide_enabled_count + $7,
				        provide_changed_count = client_reliability.provide_changed_count + $8,
				        receive_message_count = client_reliability.receive_message_count + $9,
				        receive_byte_count = client_reliability.receive_byte_count + $10,
				        send_message_count = client_reliability.send_message_count + $11,
				        send_byte_count = client_reliability.send_byte_count + $12,
				        valid = client_reliability_valid(
				        	client_reliability.connection_new_count + $5,
				        	client_reliability.connection_established_count + $6,
				        	client_reliability.provide_enabled_count + $7,
				        	client_reliability.provide_changed_count + $8,
				        	client_reliability.receive_message_count + $9,
				        	$13
				        )
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
					ReliabilityAllowDisconnectCountPerBlock,
				)
			}
		})

	})
}

// Redis rollup for the reliability stats hot path.
//
// The connection announce loop reports per-client stats every half block for
// every connected provider, which as direct pg upserts was the single largest
// statement load on the database. Instead, RecordClientReliabilityStatsRange
// increments counters in a per-block redis hash (never touching pg), and the
// serial RollupClientReliabilityStats task drains each block into
// `client_reliability` with bulk upserts once the block can no longer receive
// writes. Mirrors the RecordProviderSearchMatches/RollupSearchProviderStats
// pattern.
//
// Consistency: the recorder refuses to write to blocks older than the
// previous block (relative to wall clock), and the rollup only drains a block
// once two full blocks have elapsed since its start. So by the time a block is
// drained it can no longer change, the drain can overwrite pg rows with
// absolute counts, and a re-drain after a crash is idempotent. A stalled
// recorder (>1 block behind) drops that sync's stats with a log line rather
// than corrupting a drained block.

// one redis hash per block: field = <hash hex>:<network_id>:<client_id>:<counter index>,
// value = accumulated counter
func clientReliabilityStatsKey(blockNumber int64) string {
	return fmt.Sprintf("client_reliability_stats.%d", blockNumber)
}

// redis SET of block numbers that have pending (un-drained) counters
const clientReliabilityBlocksKey = "client_reliability_stats_blocks"

// memory backstop for the per-block counters; in normal operation the rollup
// deletes a block's hash within ~2 blocks + one rollup period
const clientReliabilityStatsRedisTtl = 15 * time.Minute

const clientReliabilityCounterCount = 8

// index order of the packed counters; must match the drain insert below
const (
	reliabilityCounterConnectionNew         = 0
	reliabilityCounterConnectionEstablished = 1
	reliabilityCounterProvideEnabled        = 2
	reliabilityCounterProvideChanged        = 3
	reliabilityCounterReceiveMessage        = 4
	reliabilityCounterReceiveByte           = 5
	reliabilityCounterSendMessage           = 6
	reliabilityCounterSendByte              = 7
)

func (self *ClientReliabilityStats) counters() [clientReliabilityCounterCount]int64 {
	return [clientReliabilityCounterCount]int64{
		reliabilityCounterConnectionNew:         int64(self.ConnectionNewCount),
		reliabilityCounterConnectionEstablished: int64(self.ConnectionEstablishedCount),
		reliabilityCounterProvideEnabled:        int64(self.ProvideEnabledCount),
		reliabilityCounterProvideChanged:        int64(self.ProvideChangedCount),
		reliabilityCounterReceiveMessage:        int64(self.ReceiveMessageCount),
		reliabilityCounterReceiveByte:           int64(self.ReceiveByteCount),
		reliabilityCounterSendMessage:           int64(self.SendMessageCount),
		reliabilityCounterSendByte:              int64(self.SendByteCount),
	}
}

func reliabilityBlockNumber(t time.Time) int64 {
	return t.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)
}

// RecordClientReliabilityStatsRange is the hot-path replacement for
// AddClientReliabilityStatsRange: it accumulates the stats in redis and never
// writes pg. It is best-effort — a redis error is swallowed (with a log) so a
// stats hiccup can never take down the reporting connection.
func RecordClientReliabilityStatsRange(
	ctx context.Context,
	networkId server.Id,
	clientId server.Id,
	clientAddressHash [32]byte,
	statsStartTime time.Time,
	statsEndTime time.Time,
	stats *ClientReliabilityStats,
) {
	server.HandleError(func() {
		startBlockNumber := reliabilityBlockNumber(statsStartTime)
		endBlockNumber := reliabilityBlockNumber(statsEndTime)

		// never write to a block the rollup may already have drained. Writers
		// touch only the current and previous block; the rollup drains a block
		// only after two full blocks have elapsed, so the two can never race.
		minBlockNumber := reliabilityBlockNumber(server.NowUtc()) - 1
		if startBlockNumber < minBlockNumber {
			glog.Infof(
				"[ncr]drop reliability stats for stale blocks [%d, %d) client_id=%s\n",
				startBlockNumber,
				min(minBlockNumber, endBlockNumber+1),
				clientId,
			)
			startBlockNumber = minBlockNumber
			if endBlockNumber < startBlockNumber {
				return
			}
		}

		counters := stats.counters()
		fieldPrefix := fmt.Sprintf(
			"%s:%s:%s",
			hex.EncodeToString(clientAddressHash[:]),
			networkId,
			clientId,
		)

		server.Redis(ctx, func(r server.RedisClient) {
			blockNumberStrs := []interface{}{}
			for blockNumber := startBlockNumber; blockNumber <= endBlockNumber; blockNumber += 1 {
				statsKey := clientReliabilityStatsKey(blockNumber)
				// every command targets the same hash key (one slot), so
				// batching in a transaction is cluster-safe
				r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
					for i, count := range counters {
						if count != 0 {
							pipe.HIncrBy(ctx, statsKey, fmt.Sprintf("%s:%d", fieldPrefix, i), count)
						}
					}
					// backstop ttl only — the rollup normally deletes the
					// block hash shortly after the block closes
					pipe.Expire(ctx, statsKey, clientReliabilityStatsRedisTtl)
					return nil
				})
				blockNumberStrs = append(blockNumberStrs, strconv.FormatInt(blockNumber, 10))
			}
			// the blocks set is a different key (different slot), so it must be
			// separate commands, not part of the transactions above
			if 0 < len(blockNumberStrs) {
				r.SAdd(ctx, clientReliabilityBlocksKey, blockNumberStrs...)
				r.Expire(ctx, clientReliabilityBlocksKey, clientReliabilityStatsRedisTtl)
			}
		})
	})
}

// RollupClientReliabilityStats drains closed per-block redis counters into
// `client_reliability` with bulk upserts, and advances the drain high-water
// mark (`client_reliability_rollup`) that the score computations clamp their
// windows to. A block is drained only once two full blocks have elapsed since
// its start, at which point the recorder can no longer write to it; the drain
// then overwrites with absolute counts, so a re-drain after a partial failure
// is idempotent.
func RollupClientReliabilityStats(ctx context.Context, now time.Time) {
	currentBlockNumber := reliabilityBlockNumber(now)
	// blocks <= this are final: the recorder writes only to the current and
	// previous block
	maxFinalBlockNumber := currentBlockNumber - 2

	// raise on error rather than treating "cannot list" as "nothing pending":
	// advancing the high-water mark past blocks that are still buffered in
	// redis would make the score windows silently skip them
	var blockNumberStrs []string
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		blockNumberStrs, err = r.SMembers(ctx, clientReliabilityBlocksKey).Result()
		server.Raise(err)
	})

	blockNumbers := []int64{}
	for _, blockNumberStr := range blockNumberStrs {
		blockNumber, err := strconv.ParseInt(blockNumberStr, 10, 64)
		if err != nil {
			server.Redis(ctx, func(r server.RedisClient) {
				r.SRem(ctx, clientReliabilityBlocksKey, blockNumberStr)
			})
			continue
		}
		if blockNumber <= maxFinalBlockNumber {
			blockNumbers = append(blockNumbers, blockNumber)
		}
	}
	// ascending so pg fills in block order
	slices.Sort(blockNumbers)

	for _, blockNumber := range blockNumbers {
		statsKey := clientReliabilityStatsKey(blockNumber)

		// raise on error: an empty read would delete the bucket below and
		// silently drop the block's stats
		var fields map[string]string
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			fields, err = r.HGetAll(ctx, statsKey).Result()
			server.Raise(err)
		})

		upsertClientReliabilityStatsBlock(ctx, blockNumber, fields)

		// record coverage before dropping the redis bucket, so a crash
		// in between re-drains and re-covers the unchanged bucket
		coverClientReliabilityBlock(ctx, blockNumber)
		recordClientReliabilityBlockHealth(ctx, blockNumber)

		// the block is fully in pg; drop the redis bucket.
		// a crash between the upsert and here re-drains the unchanged bucket
		// on the next run, which overwrites the same values.
		server.Redis(ctx, func(r server.RedisClient) {
			r.Del(ctx, statsKey)
			r.SRem(ctx, clientReliabilityBlocksKey, strconv.FormatInt(blockNumber, 10))
		})
	}

	// every block <= maxFinalBlockNumber is now either drained or was never
	// written; advance the high-water mark even when idle so the score windows
	// keep tracking the clock
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO client_reliability_rollup (singleton_id, max_drained_block, update_time)
			VALUES (1, $1, $2)
			ON CONFLICT (singleton_id) DO UPDATE
			SET
				max_drained_block = GREATEST(client_reliability_rollup.max_drained_block, $1),
				update_time = $2
			`,
			maxFinalBlockNumber,
			server.NowUtc(),
		))
	})
}

// upsertClientReliabilityStatsBlock writes one drained block's counters into
// `client_reliability` as chunked multi-row upserts. Counts are absolute for
// the block (the block can no longer change once drained), so the upsert
// overwrites rather than adds and re-running is idempotent.
func upsertClientReliabilityStatsBlock(
	ctx context.Context,
	blockNumber int64,
	fields map[string]string,
) {
	type rowKey struct {
		clientAddressHashHex string
		networkIdStr         string
		clientIdStr          string
	}
	rows := map[rowKey]*[clientReliabilityCounterCount]int64{}
	for field, countStr := range fields {
		parts := strings.Split(field, ":")
		if len(parts) != 4 {
			glog.Infof("[ncr]rollup drop malformed field %s\n", field)
			continue
		}
		counterIndex, err := strconv.Atoi(parts[3])
		if err != nil || counterIndex < 0 || clientReliabilityCounterCount <= counterIndex {
			glog.Infof("[ncr]rollup drop malformed field %s\n", field)
			continue
		}
		count, err := strconv.ParseInt(countStr, 10, 64)
		if err != nil {
			glog.Infof("[ncr]rollup drop malformed count %s=%s\n", field, countStr)
			continue
		}
		key := rowKey{
			clientAddressHashHex: parts[0],
			networkIdStr:         parts[1],
			clientIdStr:          parts[2],
		}
		counters, ok := rows[key]
		if !ok {
			counters = &[clientReliabilityCounterCount]int64{}
			rows[key] = counters
		}
		counters[counterIndex] += count
	}

	orderedKeys := maps.Keys(rows)
	slices.SortFunc(orderedKeys, func(a rowKey, b rowKey) int {
		if c := strings.Compare(a.clientAddressHashHex, b.clientAddressHashHex); c != 0 {
			return c
		}
		return strings.Compare(a.clientIdStr, b.clientIdStr)
	})

	rollupChunkCount := 5000
	for chunk := range slices.Chunk(orderedKeys, rollupChunkCount) {
		clientAddressHashes := [][]byte{}
		networkIdStrs := []string{}
		clientIdStrs := []string{}
		counterColumns := [clientReliabilityCounterCount][]int64{}
		for i := range counterColumns {
			counterColumns[i] = []int64{}
		}
		for _, key := range chunk {
			clientAddressHash, err := hex.DecodeString(key.clientAddressHashHex)
			if err != nil || len(clientAddressHash) != 32 {
				glog.Infof("[ncr]rollup drop malformed client address hash %s\n", key.clientAddressHashHex)
				continue
			}
			if _, err := server.ParseId(key.networkIdStr); err != nil {
				glog.Infof("[ncr]rollup drop malformed network id %s\n", key.networkIdStr)
				continue
			}
			if _, err := server.ParseId(key.clientIdStr); err != nil {
				glog.Infof("[ncr]rollup drop malformed client id %s\n", key.clientIdStr)
				continue
			}
			clientAddressHashes = append(clientAddressHashes, clientAddressHash)
			networkIdStrs = append(networkIdStrs, key.networkIdStr)
			clientIdStrs = append(clientIdStrs, key.clientIdStr)
			counters := rows[key]
			for i := range counterColumns {
				counterColumns[i] = append(counterColumns[i], counters[i])
			}
		}
		if len(clientAddressHashes) == 0 {
			continue
		}

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
					send_byte_count,
					valid
				)
				SELECT
					$1,
					t.client_address_hash,
					t.network_id,
					t.client_id,
					t.connection_new_count,
					t.connection_established_count,
					t.provide_enabled_count,
					t.provide_changed_count,
					t.receive_message_count,
					t.receive_byte_count,
					t.send_message_count,
					t.send_byte_count,
					client_reliability_valid(
						t.connection_new_count,
						t.connection_established_count,
						t.provide_enabled_count,
						t.provide_changed_count,
						t.receive_message_count,
						$13
					)
				FROM unnest(
					$2::bytea[],
					$3::uuid[],
					$4::uuid[],
					$5::bigint[],
					$6::bigint[],
					$7::bigint[],
					$8::bigint[],
					$9::bigint[],
					$10::bigint[],
					$11::bigint[],
					$12::bigint[]
				) AS t(
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
				)
				ON CONFLICT (block_number, client_address_hash, client_id) DO UPDATE
				SET
					network_id = EXCLUDED.network_id,
					connection_new_count = EXCLUDED.connection_new_count,
					connection_established_count = EXCLUDED.connection_established_count,
					provide_enabled_count = EXCLUDED.provide_enabled_count,
					provide_changed_count = EXCLUDED.provide_changed_count,
					receive_message_count = EXCLUDED.receive_message_count,
					receive_byte_count = EXCLUDED.receive_byte_count,
					send_message_count = EXCLUDED.send_message_count,
					send_byte_count = EXCLUDED.send_byte_count,
					valid = EXCLUDED.valid
				`,
				blockNumber,
				clientAddressHashes,
				networkIdStrs,
				clientIdStrs,
				counterColumns[reliabilityCounterConnectionNew],
				counterColumns[reliabilityCounterConnectionEstablished],
				counterColumns[reliabilityCounterProvideEnabled],
				counterColumns[reliabilityCounterProvideChanged],
				counterColumns[reliabilityCounterReceiveMessage],
				counterColumns[reliabilityCounterReceiveByte],
				counterColumns[reliabilityCounterSendMessage],
				counterColumns[reliabilityCounterSendByte],
				ReliabilityAllowDisconnectCountPerBlock,
			))
		})
	}
}

func RemoveOldClientReliabilityStats(ctx context.Context, maxTime time.Time, limit int) (removedCount int64) {
	minTime := maxTime.Add(-ClientExpiration)
	minBlockNumber := (minTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)) - 1

	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			DELETE FROM client_reliability
			USING (
			    SELECT
			        block_number,
			        client_address_hash,
			        network_id,
			        client_id
			    FROM client_reliability
			    WHERE block_number <= $1
			    ORDER BY block_number
			    LIMIT $2
			) t
			WHERE
			    client_reliability.block_number = t.block_number AND
			    client_reliability.client_address_hash = t.client_address_hash AND
			    client_reliability.network_id = t.network_id AND
			    client_reliability.client_id = t.client_id
			`,
			minBlockNumber,
			limit,
		)
		server.Raise(err)
		removedCount = tag.RowsAffected()

		removeExpiredClientReliabilityCompanions(ctx, tx, minBlockNumber)
	})
	return
}

// removeExpiredClientReliabilityCompanions trims the small per-block companion
// tables to the same horizon as the stats. Shared by the legacy row-delete
// retention above and the partition-drop retention
// (MaintainClientReliabilityPartitions).
func removeExpiredClientReliabilityCompanions(ctx context.Context, tx server.PgTx, minBlockNumber int64) {
	// trim expired block health rows alongside the stats they describe
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		DELETE FROM client_reliability_block
		WHERE block_number <= $1
		`,
		minBlockNumber,
	))

	// trim expired coverage ranges. The straddling range keeps its newer
	// half so gap accounting stays exact inside the retained window.
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		DELETE FROM client_reliability_sync
		WHERE max_block_number < $1
		`,
		minBlockNumber,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		UPDATE client_reliability_sync
		SET min_block_number = $1
		WHERE min_block_number < $1 AND $1 <= max_block_number
		`,
		minBlockNumber,
	))
}

type ReliabilityScore struct {
	IndependentReliabilityScore  float64
	IndependentReliabilityWeight float64
	ReliabilityScore             float64
	ReliabilityWeight            float64
}

// the score queries below share this shape: rows are valid `client_reliability`
// entries joined to a valid location, and each row contributes
// 1/valid_client_count, where valid_client_count is the number of valid
// clients sharing the row's (block_number, client_address_hash). The count
// comes from a pre-aggregated GROUP BY subquery (`valid_counts`) instead of a
// window function over the joined set: the window forced a sort of every row
// in the block range, while the subquery streams off the
// (valid, block_number, client_address_hash) index and merge-joins back in
// index order, so no full-range sort is needed. valid_client_count counts by
// client_reliability.valid alone (not the location join), matching the bucket
// window aggregation and the per-ip normalization intent.

// reliabilityRollupBlockShift returns how many blocks to shift a score window
// back so that it ends at the redis-rollup high-water mark (the newest block
// fully drained into `client_reliability` by RollupClientReliabilityStats).
// Shifting (rather than truncating) preserves the window width, so the
// weight normalization ($2-$1) keeps its scale. Returns 0 when the rollup has
// never run (pre-rollup deploys) or is already caught up.
func reliabilityRollupBlockShift(ctx context.Context, tx server.PgTx, maxBlockNumber int64) (shift int64) {
	result, err := tx.Query(
		ctx,
		`
		SELECT max_drained_block FROM client_reliability_rollup
		WHERE singleton_id = 1
		`,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var maxDrainedBlock int64
			server.Raise(result.Scan(&maxDrainedBlock))
			if maxDrainedBlock+1 < maxBlockNumber {
				shift = maxBlockNumber - (maxDrainedBlock + 1)
			}
		}
	})
	return
}

// coverClientReliabilityBlock records that blockNumber's redis counters are
// fully drained into `client_reliability`, extending the newest coverage
// range in `client_reliability_sync` when contiguous and starting a new range
// after a gap. Blocks left outside every range (redis loss, drain outage) are
// skipped by the score denominators (`reliabilityCoveredBlockCount`) instead
// of being counted as unreliable gaps.
func coverClientReliabilityBlock(ctx context.Context, blockNumber int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		// the common case: extend the newest range by one block
		tag, err := tx.Exec(
			ctx,
			`
			UPDATE client_reliability_sync
			SET max_block_number = $1, update_time = $2
			WHERE min_block_number = (
				SELECT MAX(min_block_number) FROM client_reliability_sync
			) AND max_block_number = $1 - 1
			`,
			blockNumber,
			server.NowUtc(),
		)
		server.Raise(err)
		if tag.RowsAffected() == 1 {
			return
		}

		// already covered (re-drain of an unchanged block after a crash)
		covered := false
		result, err := tx.Query(
			ctx,
			`
			SELECT 1 FROM client_reliability_sync
			WHERE min_block_number <= $1 AND $1 <= max_block_number
			`,
			blockNumber,
		)
		server.WithPgResult(result, err, func() {
			covered = result.Next()
		})
		if covered {
			return
		}

		// start a new range after a gap
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO client_reliability_sync (min_block_number, max_block_number, update_time)
			VALUES ($1, $1, $2)
			ON CONFLICT (min_block_number) DO UPDATE
			SET
				max_block_number = GREATEST(client_reliability_sync.max_block_number, $1),
				update_time = $2
			`,
			blockNumber,
			server.NowUtc(),
		))
	})
}

// recordClientReliabilityBlockHealth counts how many clients reported in a
// drained block. The counts drive `reliabilityDegradedBlocks`: a block whose
// valid client count collapses relative to its neighbors was a platform event,
// not a client event. Counting from the drained pg rows (rather than the redis
// fields) reuses the `valid` generated column, so the rule can never drift.
func recordClientReliabilityBlockHealth(ctx context.Context, blockNumber int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO client_reliability_block (block_number, client_count, valid_client_count)
			SELECT
				$1,
				COUNT(*),
				COUNT(*) FILTER (WHERE valid)
			FROM client_reliability
			WHERE block_number = $1
			ON CONFLICT (block_number) DO UPDATE
			SET
				client_count = EXCLUDED.client_count,
				valid_client_count = EXCLUDED.valid_client_count
			`,
			blockNumber,
		))
	})
}

// a block whose valid client count falls below this fraction of the window
// median is degraded: a platform event took a set of clients out at once. The
// affected clients lose the block they could not announce in AND the block
// they reconnected in (`connection_new_count = 0` invalidates a reconnect
// block), so at a 0.99 threshold a single connect deploy would otherwise drop
// every provider on a rotated handler out of the market for a full lookback.
// Normal churn moves this count by well under a percent per block, so the
// fraction only has to be under 1 to catch a synchronized drop while never
// firing on churn.
const ReliabilityBlockDegradedFraction = 0.95

// the median needs enough blocks to be meaningful, and a network with only a
// handful of providers has too much relative noise to judge -- both fall back
// to "no blocks are degraded" (the pre-existing behavior)
const reliabilityDegradedMinBlockCount = 10
const reliabilityDegradedMinMedian = 20

// block health is a property of the block, not of the window being scored, so
// the median is taken over a fixed neighborhood rather than the score window:
// the shortest lookback (`ClientLookbacks[0]`) is only a handful of blocks
// wide and could never establish a median of its own
const reliabilityDegradedMedianBlockCount = 60

// reliabilityDegradedBlocks returns the blocks in [minBlockNumber,
// maxBlockNumber) that a platform event took out. These are excused for every
// client: they count toward neither the numerator nor the denominator of the
// reliability weights, exactly like a block that never drained.
func reliabilityDegradedBlocks(ctx context.Context, tx server.PgTx, minBlockNumber int64, maxBlockNumber int64) (degradedBlockNumbers []int64) {
	// never nil: a nil slice binds as SQL NULL, and `block_number = ANY(NULL)`
	// is NULL, so the score queries' `NOT (... = ANY($n))` would filter out
	// every row and wipe every provider score -- the outage this excusal
	// exists to prevent
	degradedBlockNumbers = []int64{}

	medianMinBlockNumber := min(minBlockNumber, maxBlockNumber-reliabilityDegradedMedianBlockCount)

	result, err := tx.Query(
		ctx,
		`
		WITH neighborhood AS (
			SELECT block_number, valid_client_count
			FROM client_reliability_block
			WHERE $1 <= block_number AND block_number < $3
		), stat AS (
			SELECT
				COUNT(*) AS block_count,
				percentile_cont(0.5) WITHIN GROUP (ORDER BY valid_client_count) AS median_valid_client_count
			FROM neighborhood
		)
		SELECT neighborhood.block_number
		FROM neighborhood, stat
		WHERE
			$2 <= neighborhood.block_number AND
			$4 <= stat.block_count AND
			$5 <= stat.median_valid_client_count AND
			neighborhood.valid_client_count < $6 * stat.median_valid_client_count
		ORDER BY neighborhood.block_number
		`,
		medianMinBlockNumber,
		minBlockNumber,
		maxBlockNumber,
		reliabilityDegradedMinBlockCount,
		reliabilityDegradedMinMedian,
		ReliabilityBlockDegradedFraction,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var blockNumber int64
			server.Raise(result.Scan(&blockNumber))
			degradedBlockNumbers = append(degradedBlockNumbers, blockNumber)
		}
	})
	return
}

// reliabilityCoveredBlockCount returns how many blocks in
// [minBlockNumber, maxBlockNumber) count toward reliability weight
// denominators. A block counts unless it is after the first coverage range
// and outside every range: those blocks were buffered in redis but never
// drained (redis restart/expiry, drain outage), so treating them as window
// time would register the loss as client unreliability. Blocks before the
// first range (pre-rollup history, or fixture/backfill environments with no
// coverage rows at all) count as covered, which preserves the plain window
// width in those cases.
func reliabilityCoveredBlockCount(ctx context.Context, tx server.PgTx, minBlockNumber int64, maxBlockNumber int64) int64 {
	var uncoveredBlockCount int64
	result, err := tx.Query(
		ctx,
		`
		SELECT COUNT(*) FROM generate_series($1::bigint, $2::bigint - 1) AS gs(block_number)
		WHERE
			gs.block_number >= (SELECT MIN(min_block_number) FROM client_reliability_sync) AND
			NOT EXISTS (
				SELECT 1 FROM client_reliability_sync
				WHERE min_block_number <= gs.block_number AND gs.block_number <= max_block_number
			)
		`,
		minBlockNumber,
		maxBlockNumber,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&uncoveredBlockCount))
		}
	})
	// an entirely uncovered window returns 0: there is no synced data to
	// judge, and callers keep their previous scores instead of recomputing
	return (maxBlockNumber - minBlockNumber) - uncoveredBlockCount
}

// the drain must have advanced its high-water mark within this long of now
// for the reliability stats to compute; see `ClientReliabilityRollupSynced`
const ReliabilityRollupStaleAfter = 10 * time.Minute

// ClientReliabilityRollupSynced returns false when the redis->pg drain
// (`RollupClientReliabilityStats`) has not advanced the high-water mark
// within `ReliabilityRollupStaleAfter` of now. The stats updates wait for the
// drain rather than recompute over windows that drift away from the present.
// Returns true when the rollup has never run (fixture/backfill environments
// write pg directly and have no drain).
func ClientReliabilityRollupSynced(ctx context.Context, now time.Time) (synced bool) {
	synced = true
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT update_time FROM client_reliability_rollup
			WHERE singleton_id = 1
			`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var updateTime time.Time
				server.Raise(result.Scan(&updateTime))
				synced = now.UTC().Sub(updateTime) < ReliabilityRollupStaleAfter
			}
		})
	})
	return
}

// this should run regulalry to keep the client scores up to date
func UpdateClientReliabilityScores(ctx context.Context, maxTime time.Time, complete bool) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		if complete {
			maxLookback := ClientLookbacks[len(ClientLookbacks)-1]
			minTime := maxTime.Add(-maxLookback)
			UpdateClientLocationReliabilitiesInTx(tx, ctx, minTime, maxTime)
		}

		var shift int64
		{
			maxBlockNumber := (maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)) + 1
			shift = reliabilityRollupBlockShift(ctx, tx, maxBlockNumber)
		}

		for lookbackIndex, lookback := range ClientLookbacks {
			minTime := maxTime.Add(-lookback)
			minBlockNumber := minTime.UTC().UnixMilli()/int64(ReliabilityBlockDuration/time.Millisecond) - shift
			maxBlockNumber := (maxTime.UTC().UnixMilli()/int64(ReliabilityBlockDuration/time.Millisecond) + 1) - shift

			// weights normalize by the covered (drained) blocks in the window,
			// minus the blocks a platform event took out, so neither a lost
			// drain nor a connect deploy registers as client unreliability.
			// a window with nothing left to judge keeps the previous scores.
			coveredBlockCount := reliabilityCoveredBlockCount(ctx, tx, minBlockNumber, maxBlockNumber)
			degradedBlockNumbers := reliabilityDegradedBlocks(ctx, tx, minBlockNumber, maxBlockNumber)
			effectiveBlockCount := coveredBlockCount - int64(len(degradedBlockNumbers))
			if effectiveBlockCount <= 0 {
				glog.Infof("[ncr]no usable blocks in lookback %d window [%d, %d); keeping previous scores\n", lookbackIndex, minBlockNumber, maxBlockNumber)
				continue
			}
			if 0 < len(degradedBlockNumbers) {
				glog.Infof("[ncr]excusing %d degraded blocks in lookback %d window [%d, %d)\n", len(degradedBlockNumbers), lookbackIndex, minBlockNumber, maxBlockNumber)
			}

			// upsert the fresh scores, then remove rows not refreshed in this
			// round (identified by a stale max_block_number). This replaces the
			// previous delete-all-and-reinsert, which rewrote the entire table
			// every round and kept it permanently bloated.
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO client_connection_reliability_score (
					client_id,
					lookback_index,
					independent_reliability_score,
					independent_reliability_weight,
					reliability_score,
					reliability_weight,
					min_block_number,
					max_block_number,
					city_location_id,
					region_location_id,
					country_location_id
				)
				SELECT
				    client_reliability.client_id,
				    $3,
				    SUM(1.0) AS independent_reliability_score,
				    SUM(1.0) / $4::bigint AS independent_reliability_weight,
				    SUM(1.0/valid_counts.valid_client_count) AS reliability_score,
				    SUM(1.0/valid_counts.valid_client_count) / $4::bigint AS reliability_weight,
				    $1 AS min_block_number,
					$2 AS max_block_number,
					network_client_location_reliability.city_location_id,
					network_client_location_reliability.region_location_id,
					network_client_location_reliability.country_location_id
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
						block_number < $2 AND
						NOT (block_number = ANY($5::bigint[]))
					GROUP BY block_number, client_address_hash
				) valid_counts ON
					valid_counts.block_number = client_reliability.block_number AND
					valid_counts.client_address_hash = client_reliability.client_address_hash
				INNER JOIN network_client_location_reliability ON
					network_client_location_reliability.client_id = client_reliability.client_id AND
					network_client_location_reliability.valid = true
				WHERE
					client_reliability.valid = true AND
					$1 <= client_reliability.block_number AND
					client_reliability.block_number < $2 AND
					NOT (client_reliability.block_number = ANY($5::bigint[]))
				GROUP BY
					client_reliability.client_id,
					network_client_location_reliability.city_location_id,
					network_client_location_reliability.region_location_id,
					network_client_location_reliability.country_location_id
				ON CONFLICT (client_id, lookback_index) DO UPDATE
				SET
					independent_reliability_score = EXCLUDED.independent_reliability_score,
					independent_reliability_weight = EXCLUDED.independent_reliability_weight,
					reliability_score = EXCLUDED.reliability_score,
					reliability_weight = EXCLUDED.reliability_weight,
					min_block_number = EXCLUDED.min_block_number,
					max_block_number = EXCLUDED.max_block_number,
					city_location_id = EXCLUDED.city_location_id,
					region_location_id = EXCLUDED.region_location_id,
					country_location_id = EXCLUDED.country_location_id
				`,
				minBlockNumber,
				maxBlockNumber,
				lookbackIndex,
				effectiveBlockCount,
				degradedBlockNumbers,
			))

			server.RaisePgResult(tx.Exec(
				ctx,
				`
				DELETE FROM client_connection_reliability_score
				WHERE lookback_index = $1 AND max_block_number != $2
				`,
				lookbackIndex,
				maxBlockNumber,
			))

		}

	}, server.TxReadCommitted)
}

func GetAllClientReliabilityScores(ctx context.Context) (lookbackClientScores map[int]map[server.Id]ReliabilityScore) {
	server.Db(ctx, func(conn server.PgConn) {
		lookbackClientScores = map[int]map[server.Id]ReliabilityScore{}

		result, err := conn.Query(
			ctx,
			`
			SELECT
				client_id,
				lookback_index,
				independent_reliability_score,
				independent_reliability_weight,
				reliability_score,
				reliability_weight
			FROM client_connection_reliability_score
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var lookbackIndex int
				var s ReliabilityScore
				server.Raise(result.Scan(
					&clientId,
					&lookbackIndex,
					&s.IndependentReliabilityScore,
					&s.IndependentReliabilityWeight,
					&s.ReliabilityScore,
					&s.ReliabilityWeight,
				))
				clientScores, ok := lookbackClientScores[lookbackIndex]
				if !ok {
					clientScores = map[server.Id]ReliabilityScore{}
					lookbackClientScores[lookbackIndex] = clientScores
				}
				clientScores[clientId] = s
			}
		})
	})
	return
}

func UpdateNetworkReliabilityScores(ctx context.Context, minTime time.Time, maxTime time.Time, complete bool) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		UpdateNetworkReliabilityScoresInTx(tx, ctx, minTime, maxTime, complete)
	}, server.TxReadCommitted)
}

// this should run on payout to compute the latest
func UpdateNetworkReliabilityScoresInTx(tx server.PgTx, ctx context.Context, minTime time.Time, maxTime time.Time, complete bool) {
	if complete {
		UpdateClientLocationReliabilitiesInTx(tx, ctx, minTime, maxTime)
	}

	minBlockNumber := minTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)
	maxBlockNumber := (maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)) + 1

	shift := reliabilityRollupBlockShift(ctx, tx, maxBlockNumber)
	minBlockNumber -= shift
	maxBlockNumber -= shift

	// weights normalize by the covered (drained) blocks in the window, minus
	// the blocks a platform event took out, so neither a lost drain nor a
	// connect deploy registers as client unreliability. a window with nothing
	// left to judge keeps the previous scores.
	coveredBlockCount := reliabilityCoveredBlockCount(ctx, tx, minBlockNumber, maxBlockNumber)
	degradedBlockNumbers := reliabilityDegradedBlocks(ctx, tx, minBlockNumber, maxBlockNumber)
	effectiveBlockCount := coveredBlockCount - int64(len(degradedBlockNumbers))
	if effectiveBlockCount <= 0 {
		glog.Infof("[ncr]no usable blocks in network score window [%d, %d); keeping previous scores\n", minBlockNumber, maxBlockNumber)
		return
	}

	// upsert the fresh scores, then remove rows not refreshed in this round
	// (identified by a stale max_block_number). This replaces the previous
	// delete-all-and-reinsert, which rewrote the entire table every round and
	// kept it permanently bloated.
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO network_connection_reliability_score (
			network_id,
			country_location_id,
			independent_reliability_score,
			independent_reliability_weight,
			reliability_score,
			reliability_weight,
			min_block_number,
			max_block_number
		)
		SELECT
		    client_reliability.network_id,
		    network_client_location_reliability.country_location_id,
		    SUM(1.0) AS independent_reliability_score,
		    SUM(1.0) / $3::bigint AS independent_reliability_weight,
		    SUM(1.0/valid_counts.valid_client_count) AS reliability_score,
		    SUM(1.0/valid_counts.valid_client_count) / $3::bigint AS reliability_weight,
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
				block_number < $2 AND
				NOT (block_number = ANY($4::bigint[]))
			GROUP BY block_number, client_address_hash
		) valid_counts ON
			valid_counts.block_number = client_reliability.block_number AND
			valid_counts.client_address_hash = client_reliability.client_address_hash
		INNER JOIN network_client_location_reliability ON
			network_client_location_reliability.client_id = client_reliability.client_id AND
			network_client_location_reliability.valid = true
		WHERE
			client_reliability.valid = true AND
			$1 <= client_reliability.block_number AND
			client_reliability.block_number < $2 AND
			NOT (client_reliability.block_number = ANY($4::bigint[]))
		GROUP BY
			client_reliability.network_id,
			network_client_location_reliability.country_location_id
		ON CONFLICT (network_id, country_location_id) DO UPDATE
		SET
			independent_reliability_score = EXCLUDED.independent_reliability_score,
			independent_reliability_weight = EXCLUDED.independent_reliability_weight,
			reliability_score = EXCLUDED.reliability_score,
			reliability_weight = EXCLUDED.reliability_weight,
			min_block_number = EXCLUDED.min_block_number,
			max_block_number = EXCLUDED.max_block_number
		`,
		minBlockNumber,
		maxBlockNumber,
		effectiveBlockCount,
		degradedBlockNumbers,
	))

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		DELETE FROM network_connection_reliability_score
		WHERE max_block_number != $1
		`,
		maxBlockNumber,
	))
}

func GetAllNetworkReliabilityScores(ctx context.Context) (networkScores map[server.Id]ReliabilityScore) {
	server.Db(ctx, func(conn server.PgConn) {
		networkScores = getAllNetworkReliabilityScores(conn, ctx)
	})
	return
}

func getAllNetworkReliabilityScores(q server.PgCanQuery, ctx context.Context) map[server.Id]ReliabilityScore {
	networkScores := map[server.Id]ReliabilityScore{}

	result, err := q.Query(
		ctx,
		`
		SELECT
			network_id,
			independent_reliability_score,
			independent_reliability_weight,
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
				&s.IndependentReliabilityWeight,
				&s.ReliabilityScore,
				&s.ReliabilityWeight,
			))
			if c, ok := networkScores[networkId]; ok {
				s.IndependentReliabilityScore += c.IndependentReliabilityScore
				s.IndependentReliabilityWeight += c.IndependentReliabilityWeight
				s.ReliabilityScore += c.ReliabilityScore
				s.ReliabilityWeight += c.ReliabilityWeight
			}
			networkScores[networkId] = s
		}
	})

	return networkScores
}

func GetAllNetworkReliabilityScoresInTx(tx server.PgTx, ctx context.Context) map[server.Id]ReliabilityScore {
	return getAllNetworkReliabilityScores(tx, ctx)
}

func GetAllMultipliedNetworkReliabilityScores(ctx context.Context) (networkScores map[server.Id]ReliabilityScore) {
	server.Db(ctx, func(conn server.PgConn) {
		networkScores = getAllMultipliedNetworkReliabilityScores(conn, ctx)
	})
	return
}

func getAllMultipliedNetworkReliabilityScores(q server.PgCanQuery, ctx context.Context) map[server.Id]ReliabilityScore {
	networkScores := map[server.Id]ReliabilityScore{}

	result, err := q.Query(
		ctx,
		`
		SELECT
			network_id,
			independent_reliability_score * COALESCE(network_client_location_reliability_multiplier.reliability_multiplier, 1.0) AS independent_reliability_score,
			independent_reliability_weight * COALESCE(network_client_location_reliability_multiplier.reliability_multiplier, 1.0) AS independent_reliability_weight,
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
				&s.IndependentReliabilityWeight,
				&s.ReliabilityScore,
				&s.ReliabilityWeight,
			))
			if c, ok := networkScores[networkId]; ok {
				s.IndependentReliabilityScore += c.IndependentReliabilityScore
				s.IndependentReliabilityWeight += c.IndependentReliabilityWeight
				s.ReliabilityScore += c.ReliabilityScore
				s.ReliabilityWeight += c.ReliabilityWeight
			}
			networkScores[networkId] = s
		}
	})

	return networkScores
}

func GetAllMultipliedNetworkReliabilityScoresInTx(tx server.PgTx, ctx context.Context) map[server.Id]ReliabilityScore {
	return getAllMultipliedNetworkReliabilityScores(tx, ctx)
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
	// stats read of precomputed window rows: tolerates replica delay
	server.ReplicaDb(ctx, func(conn server.PgConn) {
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
				network_connection_reliability_window_score.country_location_id,
				location.location_name,
				location.country_code,
				COALESCE(network_client_location_reliability_multiplier.reliability_multiplier, 1.0) AS reliability_multiplier
			FROM network_connection_reliability_window_score
			INNER JOIN location ON
				location.location_id = network_connection_reliability_window_score.country_location_id
			LEFT JOIN network_client_location_reliability_multiplier ON
				network_client_location_reliability_multiplier.country_location_id = network_connection_reliability_window_score.country_location_id
			WHERE network_connection_reliability_window_score.network_id = $1
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

		// drop the latest data point which may be incomplete
		if minBucketNumber < maxBucketNumber {
			maxBucketNumber -= 1
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

		meanReliabilityWeight := float64(0)
		if 0 < n {
			meanReliabilityWeight = netReliabilityWeight / float64(n)
		}
		reliabilityWindow = &ReliabilityWindow{
			MeanReliabilityWeight: meanReliabilityWeight,
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

func UpdateNetworkReliabilityWindow(ctx context.Context, minTime time.Time, maxTime time.Time, complete bool) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		UpdateNetworkReliabilityWindowScoresInTx(tx, ctx, maxTime, complete)

		minBlockNumber := minTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)
		maxBlockNumber := (maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)) + 1

		// buckets are absolute, so cap (not shift) the range at the rollup
		// high-water mark. A trailing partial bucket converges on later runs
		// because each bucket is upserted whole.
		maxBlockNumber -= reliabilityRollupBlockShift(ctx, tx, maxBlockNumber)

		blockCountPerBucket := ReliabilityBlockCountPerBucket()

		// round to whole blocks
		// min round down
		if c := minBlockNumber % int64(blockCountPerBucket); c != 0 {
			minBlockNumber -= int64(c)
		}
		// max round up
		if c := maxBlockNumber % int64(blockCountPerBucket); c != 0 {
			maxBlockNumber += int64(blockCountPerBucket) - c
		}

		if maxBlockNumber <= minBlockNumber {
			return
		}

		degradedBlockNumbers := reliabilityDegradedBlocks(ctx, tx, minBlockNumber, maxBlockNumber)

		// each bucket's weight normalizes by the usable (drained, not
		// platform-degraded) blocks in the bucket rather than the full bucket
		// width, so neither a lost drain nor a connect deploy registers as
		// unreliability. Blocks before the first coverage range count as
		// covered (pre-rollup history and fixture/backfill environments); a
		// bucket with no usable blocks falls back to the full width.
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
			    client_reliability.network_id,
			    client_reliability.block_number / $3 AS bucket_number,
			    SUM(CASE WHEN client_reliability.valid = true THEN 1.0/valid_counts.valid_client_count ELSE 0 END) / COALESCE(NULLIF(MAX(covered.covered_block_count), 0), $3) AS reliability_weight,
			    COUNT(DISTINCT client_reliability.client_id) FILTER (WHERE client_reliability.valid = true) AS client_count,
			    COUNT(DISTINCT client_reliability.client_id) AS total_client_count
			FROM client_reliability
			LEFT JOIN (
				SELECT
					block_number,
					client_address_hash,
					COUNT(*) AS valid_client_count
				FROM client_reliability
				WHERE
					valid = true AND
					$1 <= block_number AND
					block_number < $2 AND
					NOT (block_number = ANY($4::bigint[]))
				GROUP BY block_number, client_address_hash
			) valid_counts ON
				valid_counts.block_number = client_reliability.block_number AND
				valid_counts.client_address_hash = client_reliability.client_address_hash
			LEFT JOIN (
				SELECT
					gs.block_number / $3 AS bucket_number,
					COUNT(*) AS covered_block_count
				FROM generate_series($1::bigint, $2::bigint - 1) AS gs(block_number)
				WHERE
					(
						gs.block_number < (SELECT MIN(min_block_number) FROM client_reliability_sync) OR
						EXISTS (
							SELECT 1 FROM client_reliability_sync
							WHERE min_block_number <= gs.block_number AND gs.block_number <= max_block_number
						)
					) AND
					NOT (gs.block_number = ANY($4::bigint[]))
				GROUP BY gs.block_number / $3
			) covered ON
				covered.bucket_number = client_reliability.block_number / $3
			WHERE
				$1 <= client_reliability.block_number AND
				client_reliability.block_number < $2 AND
				NOT (client_reliability.block_number = ANY($4::bigint[]))
			GROUP BY client_reliability.network_id, client_reliability.block_number / $3
			ON CONFLICT (network_id, bucket_number) DO UPDATE
			SET
				reliability_weight = EXCLUDED.reliability_weight,
				client_count = EXCLUDED.client_count,
				total_client_count = EXCLUDED.total_client_count
			`,
			minBlockNumber,
			maxBlockNumber,
			blockCountPerBucket,
			degradedBlockNumbers,
		))
	}, server.TxReadCommitted)
}

func RemoveOldNetworkReliabilityWindow(ctx context.Context, maxTime time.Time, limit int) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		minTime := maxTime.Add(-NetworkWindowExpiration)
		minBlockNumber := minTime.UTC().UnixMilli()/int64(ReliabilityBlockDuration/time.Millisecond) - 1

		blockCountPerBucket := ReliabilityBlockCountPerBucket()

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM network_connection_reliability_window
			USING (
			    SELECT
			        network_id,
			        bucket_number
			    FROM network_connection_reliability_window
			    WHERE bucket_number <= $1
			    ORDER BY network_id, bucket_number
			    LIMIT $2
			) t
			WHERE
			    network_connection_reliability_window.network_id = t.network_id AND
			    network_connection_reliability_window.bucket_number = t.bucket_number
			`,
			minBlockNumber/int64(blockCountPerBucket),
			limit,
		))
	}, server.TxReadCommitted)
}

func UpdateNetworkReliabilityWindowScores(ctx context.Context, maxTime time.Time, complete bool) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		UpdateNetworkReliabilityWindowScoresInTx(tx, ctx, maxTime, complete)
	}, server.TxReadCommitted)
}

func UpdateNetworkReliabilityWindowScoresInTx(tx server.PgTx, ctx context.Context, maxTime time.Time, complete bool) {
	minTime := maxTime.Add(-NetworkWindowLookback)

	if complete {
		UpdateClientLocationReliabilitiesInTx(tx, ctx, minTime, maxTime)
	}

	minBlockNumber := minTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)
	maxBlockNumber := (maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)) + 1

	shift := reliabilityRollupBlockShift(ctx, tx, maxBlockNumber)
	minBlockNumber -= shift
	maxBlockNumber -= shift

	// weights normalize by the covered (drained) blocks in the window, minus
	// the blocks a platform event took out, so neither a lost drain nor a
	// connect deploy registers as client unreliability. a window with nothing
	// left to judge keeps the previous scores.
	coveredBlockCount := reliabilityCoveredBlockCount(ctx, tx, minBlockNumber, maxBlockNumber)
	degradedBlockNumbers := reliabilityDegradedBlocks(ctx, tx, minBlockNumber, maxBlockNumber)
	effectiveBlockCount := coveredBlockCount - int64(len(degradedBlockNumbers))
	if effectiveBlockCount <= 0 {
		glog.Infof("[ncr]no usable blocks in window score window [%d, %d); keeping previous scores\n", minBlockNumber, maxBlockNumber)
		return
	}

	// upsert the fresh scores, then remove rows not refreshed in this round
	// (identified by a stale max_block_number). This replaces the previous
	// delete-all-and-reinsert, which rewrote the entire table every round and
	// kept it permanently bloated.
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO network_connection_reliability_window_score (
			network_id,
			country_location_id,
			independent_reliability_score,
			independent_reliability_weight,
			reliability_score,
			reliability_weight,
			min_block_number,
			max_block_number
		)
		SELECT
		    client_reliability.network_id,
		    network_client_location_reliability.country_location_id,
		    SUM(1.0) AS independent_reliability_score,
		    SUM(1.0) / $3::bigint AS independent_reliability_weight,
		    SUM(1.0/valid_counts.valid_client_count) AS reliability_score,
		    SUM(1.0/valid_counts.valid_client_count) / $3::bigint AS reliability_weight,
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
				block_number < $2 AND
				NOT (block_number = ANY($4::bigint[]))
			GROUP BY block_number, client_address_hash
		) valid_counts ON
			valid_counts.block_number = client_reliability.block_number AND
			valid_counts.client_address_hash = client_reliability.client_address_hash
		INNER JOIN network_client_location_reliability ON
			network_client_location_reliability.client_id = client_reliability.client_id AND
			network_client_location_reliability.valid = true
		WHERE
			client_reliability.valid = true AND
			$1 <= client_reliability.block_number AND
			client_reliability.block_number < $2 AND
			NOT (client_reliability.block_number = ANY($4::bigint[]))
		GROUP BY
			client_reliability.network_id,
			network_client_location_reliability.country_location_id
		ON CONFLICT (network_id, country_location_id) DO UPDATE
		SET
			independent_reliability_score = EXCLUDED.independent_reliability_score,
			independent_reliability_weight = EXCLUDED.independent_reliability_weight,
			reliability_score = EXCLUDED.reliability_score,
			reliability_weight = EXCLUDED.reliability_weight,
			min_block_number = EXCLUDED.min_block_number,
			max_block_number = EXCLUDED.max_block_number
		`,
		minBlockNumber,
		maxBlockNumber,
		effectiveBlockCount,
		degradedBlockNumbers,
	))

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		DELETE FROM network_connection_reliability_window_score
		WHERE max_block_number != $1
		`,
		maxBlockNumber,
	))
}

type cityRegionCountry struct {
	cityLocationId    server.Id
	regionLocationId  server.Id
	countryLocationId server.Id
}

type clientLocationReliability struct {
	networkId server.Id
	locations map[cityRegionCountry]int

	clientAddressHashes map[[32]byte]int

	netTypeScores            map[int]int
	netTypeScoreSpeeds       map[int]int
	allBytesPerSecond        map[ByteCount]int
	allRelativeLatencyMillis map[int]int
}

// server.ComplexValue
func (self *clientLocationReliability) Values() []any {
	// [0] network_id
	// [1] city_location_id
	// [2] region_location_id
	// [3] country_location_id
	// [4] client_address_hash_count
	// [5] location_count
	// [6] max_net_type_score
	// [7] max_net_type_score_speed
	// [8] max_bytes_per_second
	// [9] min_relative_latency_ms
	// [10] has_speed_test
	// [11] has_latency_test

	values := make([]any, 12)

	values[0] = self.networkId

	if 1 == len(self.locations) {
		location := maps.Keys(self.locations)[0]
		values[1] = &location.cityLocationId
		values[2] = &location.regionLocationId
		values[3] = &location.countryLocationId
	}
	// else leave locations nil

	values[4] = len(self.clientAddressHashes)
	values[5] = len(self.locations)
	// values[5] = self.connected

	maxNetTypeScore := 0
	for netTypeScore, _ := range self.netTypeScores {
		maxNetTypeScore = max(maxNetTypeScore, netTypeScore)
	}
	values[6] = maxNetTypeScore

	maxNetTypeScoreSpeed := 0
	for netTypeScoreSpeed, _ := range self.netTypeScoreSpeeds {
		maxNetTypeScoreSpeed = max(maxNetTypeScoreSpeed, netTypeScoreSpeed)
	}
	values[7] = maxNetTypeScoreSpeed

	// use the mean
	maxBytesPerSecond := sampleMeanByteCount(self.allBytesPerSecond)
	values[8] = maxBytesPerSecond

	// use the mean
	minRelativeLatencyMillis := sampleMeanInt(self.allRelativeLatencyMillis)
	values[9] = minRelativeLatencyMillis

	values[10] = 0 < len(self.allBytesPerSecond)
	values[11] = 0 < len(self.allRelativeLatencyMillis)

	return values
}

func sampleMeanInt(m map[int]int) int {
	net := 0
	n := 0
	for s, c := range m {
		if 0 < c {
			net += s
			n += c
		}
	}
	if n == 0 {
		return 0
	}
	return (net + n/2) / n
}

func sampleMeanByteCount(m map[ByteCount]int) ByteCount {
	net := ByteCount(0)
	n := 0
	for s, c := range m {
		if 0 < c {
			net += s
			n += c
		}
	}
	if n == 0 {
		return 0
	}
	return (net + ByteCount(n/2)) / ByteCount(n)
}

func UpdateClientLocationReliabilities(ctx context.Context, minTime time.Time, maxTime time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		UpdateClientLocationReliabilitiesInTx(tx, ctx, minTime, maxTime)
	}, server.TxReadCommitted)
}

// this should be called regularly
// a valid client will have one connected location and one connected address hash
func UpdateClientLocationReliabilitiesInTx(tx server.PgTx, ctx context.Context, minTime time.Time, maxTime time.Time) {
	updateBlockNumber := maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)

	// old entries are not deleted on each update, but the connected status is updated
	// - connected clients are updated, and the valid state is reset to match the latest
	// - if a disconnected client is not in `network_client_location_reliability`,
	//   the most recent disconnected connection is used to update the values

	clientLocationReliabilities := map[server.Id]*clientLocationReliability{}

	// for each client, summarize all the active locations
	result, err := tx.Query(
		ctx,
		`
		SELECT
			network_client.client_id,
			network_client.network_id,
			network_client_connection.client_address_hash,	
			network_client_location.city_location_id,
	        network_client_location.region_location_id,
	        network_client_location.country_location_id,
			network_client_location.net_type_score,
			network_client_location.net_type_score_speed,
			COALESCE(network_client_speed.bytes_per_second, 0) AS bytes_per_second,
			COALESCE(network_client_latency.latency_ms - network_client_connection.expected_latency_ms, 0) AS relative_latency_ms,
			network_client_speed.bytes_per_second IS NOT NULL AS has_speed_test,
			network_client_latency.latency_ms IS NOT NULL AS has_latency_test

		FROM network_client_connection

		INNER JOIN network_client ON
			network_client.client_id = network_client_connection.client_id 

		INNER JOIN network_client_location ON
			network_client_location.connection_id = network_client_connection.connection_id

		LEFT JOIN network_client_latency ON
			network_client_latency.connection_id = network_client_connection.connection_id

		LEFT JOIN network_client_speed ON
			network_client_speed.connection_id = network_client_connection.connection_id

		WHERE
			network_client_connection.connected = true
		`,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var clientId server.Id
			var networkId server.Id
			var clientAddressHash [32]byte
			var cityLocationId server.Id
			var regionLocationId server.Id
			var countryLocationId server.Id
			var netTypeScore int
			var netTypeScoreSpeed int
			var bytesPerSecond ByteCount
			var relativeLatencyMillis int
			var hasSpeedTest bool
			var hasLatencyTest bool
			var clientAddressHashSlice []byte
			server.Raise(result.Scan(
				&clientId,
				&networkId,
				&clientAddressHashSlice,
				&cityLocationId,
				&regionLocationId,
				&countryLocationId,
				&netTypeScore,
				&netTypeScoreSpeed,
				&bytesPerSecond,
				&relativeLatencyMillis,
				&hasSpeedTest,
				&hasLatencyTest,
			))
			// scanning assigns a fresh slice, so copy into the fixed-size key
			copy(clientAddressHash[:], clientAddressHashSlice)
			r, ok := clientLocationReliabilities[clientId]
			if !ok {
				r = &clientLocationReliability{
					locations:                map[cityRegionCountry]int{},
					clientAddressHashes:      map[[32]byte]int{},
					netTypeScores:            map[int]int{},
					netTypeScoreSpeeds:       map[int]int{},
					allBytesPerSecond:        map[ByteCount]int{},
					allRelativeLatencyMillis: map[int]int{},
				}
				clientLocationReliabilities[clientId] = r
			}
			r.networkId = networkId
			r.locations[cityRegionCountry{
				cityLocationId:    cityLocationId,
				regionLocationId:  regionLocationId,
				countryLocationId: countryLocationId,
			}] += 1
			r.clientAddressHashes[clientAddressHash] += 1
			r.netTypeScores[netTypeScore] += 1
			r.netTypeScoreSpeeds[netTypeScoreSpeed] += 1
			if hasSpeedTest {
				r.allBytesPerSecond[bytesPerSecond] += 1
			}
			if hasLatencyTest {
				r.allRelativeLatencyMillis[relativeLatencyMillis] += 1
			}
		}
	})

	// fill in disconnected clients that are missing from `network_client_location_reliability`
	result, err = tx.Query(
		ctx,
		`
		SELECT
			network_client.client_id,
			network_client.network_id,
			network_client_connection.client_address_hash,	
			network_client_location.city_location_id,
	        network_client_location.region_location_id,
	        network_client_location.country_location_id,
			network_client_location.net_type_score,
			network_client_location.net_type_score_speed,
			COALESCE(network_client_speed.bytes_per_second, 0) AS bytes_per_second,
			COALESCE(network_client_latency.latency_ms - network_client_connection.expected_latency_ms, 0) AS relative_latency_ms,
			network_client_speed.bytes_per_second IS NOT NULL AS has_speed_test,
			network_client_latency.latency_ms IS NOT NULL AS has_latency_test

		FROM network_client_connection

		INNER JOIN network_client ON
			network_client.client_id = network_client_connection.client_id

		INNER JOIN network_client_location ON
			network_client_location.connection_id = network_client_connection.connection_id

		LEFT JOIN network_client_latency ON
			network_client_latency.connection_id = network_client_connection.connection_id

		LEFT JOIN network_client_speed ON
			network_client_speed.connection_id = network_client_connection.connection_id

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
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var clientId server.Id
			var networkId server.Id
			var clientAddressHash [32]byte
			var cityLocationId server.Id
			var regionLocationId server.Id
			var countryLocationId server.Id
			var netTypeScore int
			var netTypeScoreSpeed int
			var bytesPerSecond ByteCount
			var relativeLatencyMillis int
			var hasSpeedTest bool
			var hasLatencyTest bool
			var clientAddressHashSlice []byte
			server.Raise(result.Scan(
				&clientId,
				&networkId,
				&clientAddressHashSlice,
				&cityLocationId,
				&regionLocationId,
				&countryLocationId,
				&netTypeScore,
				&netTypeScoreSpeed,
				&bytesPerSecond,
				&relativeLatencyMillis,
				&hasSpeedTest,
				&hasLatencyTest,
			))
			// scanning assigns a fresh slice, so copy into the fixed-size key
			copy(clientAddressHash[:], clientAddressHashSlice)
			r, ok := clientLocationReliabilities[clientId]
			if !ok {
				r = &clientLocationReliability{
					networkId:                networkId,
					locations:                map[cityRegionCountry]int{},
					clientAddressHashes:      map[[32]byte]int{},
					netTypeScores:            map[int]int{},
					netTypeScoreSpeeds:       map[int]int{},
					allBytesPerSecond:        map[ByteCount]int{},
					allRelativeLatencyMillis: map[int]int{},
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
				if hasSpeedTest {
					r.allBytesPerSecond[bytesPerSecond] += 1
				}
				if hasLatencyTest {
					r.allRelativeLatencyMillis[relativeLatencyMillis] += 1
				}
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
				network_id uuid,
				city_location_id uuid NULL,
	            region_location_id uuid NULL,
	            country_location_id uuid NULL,
	            client_address_hash_count int,
	            location_count int,
	            max_net_type_score smallint,
	            max_net_type_score_speed smallint,
	            max_bytes_per_second bigint,
	            min_relative_latency_ms integer,
	            has_speed_test bool,
	            has_latency_test bool 
	        )
	    `,
		clientLocationReliabilities,
	)

	server.RaisePgResult(tx.Exec(
		ctx,
		`
	    INSERT INTO network_client_location_reliability (
	    	client_id,
	    	network_id,
	    	update_block_number,
			city_location_id,
	        region_location_id,
	        country_location_id,
	        client_address_hash_count,
	        location_count,
	        connected,
	        max_net_type_score,
	        max_net_type_score_speed,
	        max_bytes_per_second,
	        min_relative_latency_ms,
	        has_speed_test,
	        has_latency_test
	    )
	    SELECT
	    	client_id,
	    	network_id,
	    	$1 AS update_block_number,
	    	city_location_id,
	        region_location_id,
	        country_location_id,
	        client_address_hash_count,
	        location_count,
	        true AS connected,
	        max_net_type_score,
	        max_net_type_score_speed,
	        max_bytes_per_second,
	        min_relative_latency_ms,
	        has_speed_test,
	        has_latency_test
	    FROM temp_network_client_location_reliability
	    ORDER BY client_id
	    ON CONFLICT (client_id) DO UPDATE
	    SET
	    	network_id = EXCLUDED.network_id,
	    	update_block_number = $1,
	    	city_location_id = EXCLUDED.city_location_id,
	        region_location_id = EXCLUDED.region_location_id,
	        country_location_id = EXCLUDED.country_location_id,
	        client_address_hash_count = EXCLUDED.client_address_hash_count,
	        location_count = EXCLUDED.location_count,
	        connected = true,
	        max_net_type_score = EXCLUDED.max_net_type_score,
	        max_net_type_score_speed = EXCLUDED.max_net_type_score_speed,
	        max_bytes_per_second = EXCLUDED.max_bytes_per_second,
	        min_relative_latency_ms = EXCLUDED.min_relative_latency_ms,
	        has_speed_test = EXCLUDED.has_speed_test,
	        has_latency_test = EXCLUDED.has_latency_test
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

func RemoveOldClientLocationReliabilities(ctx context.Context, maxTime time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		minTime := maxTime.Add(-ClientLocationExpiration)
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
