package model

// aggregate counts for the public network stats
// (controller/stats_collector.go)

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server/v2026"
)

// CountNetworks returns the total number of networks at the operator.
func CountNetworks(ctx context.Context) int64 {
	var count int64
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(*) FROM network`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

// the per-block users snapshot keys (see SetBlockUsersSnapshot)
const statsBlockUsersRedisKeyPrefix = "stats.block_users."

// a snapshot outlives its block by a few blocks and then self-expires
const statsBlockUsersSnapshotTtl = 21 * 24 * time.Hour

// SetBlockUsersSnapshot records the running users count for a block. The
// stats collector overwrites it on every refresh while the block is open,
// so the last write before rollover is the finished block's final value —
// the previous-block reference the feed serves. A last-seen activity
// marker cannot reconstruct a past window, which is why the value is
// frozen forward rather than recomputed.
func SetBlockUsersSnapshot(ctx context.Context, block int, users int64) {
	server.Redis(ctx, func(client server.RedisClient) {
		key := fmt.Sprintf("%s%d", statsBlockUsersRedisKeyPrefix, block)
		_, err := client.Set(ctx, key, users, statsBlockUsersSnapshotTtl).Result()
		server.Raise(err)
	})
}

// GetBlockUsersSnapshot reads a block's users snapshot; ok is false when
// no snapshot was recorded (collector not running during that block).
func GetBlockUsersSnapshot(ctx context.Context, block int) (int64, bool) {
	var users int64
	var ok bool
	server.Redis(ctx, func(client server.RedisClient) {
		key := fmt.Sprintf("%s%d", statsBlockUsersRedisKeyPrefix, block)
		value, err := client.Get(ctx, key).Result()
		if err != nil {
			return
		}
		if parsed, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil {
			users = parsed
			ok = true
		}
	})
	return users, ok
}

// CountProviderCountries returns the number of countries with a connected,
// valid provider — the same population the public providers map draws from.
// The (connected, valid, country_location_id) index covers the scan; joining
// location dedupes any non-canonical country location rows by country code.
func CountProviderCountries(ctx context.Context) int64 {
	var count int64
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT COUNT(DISTINCT location.country_code)
                FROM network_client_location_reliability
                INNER JOIN location ON
                    location.location_id = network_client_location_reliability.country_location_id
                WHERE
                    network_client_location_reliability.connected = true AND
                    network_client_location_reliability.valid = true
            `,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

// A block user is a top-level client identity with contract-creating
// usage. network_client.contract_time marks the last time a transfer
// contract was created with the top-level client — or one of its child
// clients (per-stream window clients, including a hosted proxy's egress
// clients) — as the paying side. Contract creation is the only activity
// marker that recurs for the whole time an identity is in use: auth and
// connect events fire only at session setup, so they miss a client in
// continuous use across a block rollover, and they count connected-but-
// idle clients that transfer nothing.

// process-local stamp throttle: skips the db probe entirely on the
// contract hot path (`createTransferEscrowInTx` runs before every transfer
// pair). The db-side predicate remains the authority — after a process
// restart the worst case is one extra probe per paying client.
var contractTimeStampGate sync.Map // payer client id -> last stamp time.Time
var contractTimeStampGateCount atomic.Int64

// bounds the gate map: paying client ids churn (window clients are
// per-stream), so entries accumulate for the process lifetime. A wholesale
// clear costs one extra db probe per client.
const contractTimeStampGateMaxCount = 100_000

// StampTopLevelClientContractTime records contract-creating usage for the
// top-level identity of payerClientId: it sets contract_time = now on the
// top-level row (payerClientId itself, or its source client when
// payerClientId is a child), throttled to once per
// `clientAuthTimeRefreshMinInterval`. Never raises: the contract this
// stamp trails is already committed, so a failed stamp must not convert a
// created contract into a caller-visible error (the throttle refires
// within the hour).
func StampTopLevelClientContractTime(ctx context.Context, payerClientId server.Id) {
	now := server.NowUtc()
	if last, ok := contractTimeStampGate.Load(payerClientId); ok {
		if now.Sub(last.(time.Time)) < clientAuthTimeRefreshMinInterval {
			return
		}
	} else if contractTimeStampGateMaxCount < contractTimeStampGateCount.Add(1) {
		contractTimeStampGate.Clear()
		contractTimeStampGateCount.Store(1)
	}
	contractTimeStampGate.Store(payerClientId, now)

	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(
				ctx,
				`
	                UPDATE network_client AS top
	                SET contract_time = $2
	                FROM network_client AS payer
	                WHERE
	                    payer.client_id = $1 AND
	                    top.client_id = COALESCE(payer.source_client_id, payer.client_id) AND
	                    (top.contract_time IS NULL OR top.contract_time < $3)
	            `,
				payerClientId,
				now,
				now.Add(-clientAuthTimeRefreshMinInterval),
			))
		})
	})
}

// Testing_ResetContractTimeStampGate clears the process-local stamp
// throttle so a test can observe consecutive stamps.
func Testing_ResetContractTimeStampGate() {
	contractTimeStampGate.Clear()
	contractTimeStampGateCount.Store(0)
}

// CountTopLevelClientsWithContractSince returns the number of unique
// active top-level clients whose identity created a transfer contract at
// or after startTime (see StampTopLevelClientContractTime) — the "users
// served this block" stat: every top-level client in use counts as one
// active user, use by a child client counts for its top-level parent, and
// client_id is the primary key so the filtered count is the unique count.
// The predicate matches the network_client_top_level_contract_time partial
// index exactly ($1 <= contract_time implies contract_time IS NOT NULL) so
// the scan is a bounded ordered range, never a full pass over the hot
// table.
func CountTopLevelClientsWithContractSince(ctx context.Context, startTime time.Time) int64 {
	var count int64
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT COUNT(client_id)
                FROM network_client
                WHERE active = true AND source_client_id IS NULL AND $1 <= contract_time
            `,
			startTime,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}
