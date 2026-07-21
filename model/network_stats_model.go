package model

// aggregate counts for the public network stats
// (controller/stats_collector.go)

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/urnetwork/server"
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

// CountTopLevelClientsActiveSince returns the number of unique active
// top-level clients seen at or after startTime — the "users served this
// block" stat: every top-level client counts as one active user
// (source_client_id IS NULL excludes per-stream child clients, and
// client_id is the primary key so the filtered count is the unique
// count). network_client.auth_time is the durable last-seen marker
// (refreshed on auth and on connect, throttled to once an hour), and rows
// outlive a 7 day window (30 day idle reap). The predicate matches the
// network_client_idle_top_level_auth_time partial index exactly so the
// scan is a bounded ordered range, never a full pass over the hot table.
func CountTopLevelClientsActiveSince(ctx context.Context, startTime time.Time) int64 {
	var count int64
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT COUNT(client_id)
                FROM network_client
                WHERE active = true AND source_client_id IS NULL AND $1 <= auth_time
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
