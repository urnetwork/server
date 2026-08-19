package model

// aggregate counts for the public network stats
// (controller/stats_collector.go)

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
// valid provider holding a Public provide key — the same population the public
// providers map and /network/provider-locations draw from. It is the number
// of countries in CountProvidersByCountry, so the two public numbers can
// never disagree.
//
// The Public-only predicate is the same one UpdateClientLocations applies
// (network_client_location_model.go). This is a public stat, so it must answer
// the same question the public provider list does: how many countries a
// stranger can actually pick a provider in. GetProvideRelationship returns
// ProvideModePublic for a cross-network pair, so only a Public provide key
// makes a provider generally reachable — a ProvideModeNetwork provider is
// usable only inside its own network and is effectively private. Counting
// those here would report countries with no pickable supply at all.
func CountProviderCountries(ctx context.Context) int64 {
	return int64(len(CountProvidersByCountry(ctx)))
}

// ProviderCountryCount is the connected, valid, Public provider population
// located in one country (see CountProvidersByCountry).
type ProviderCountryCount struct {
	// upper-case ISO 3166-1 alpha-2 country code
	CountryCode string
	// the country name from the canonical country location row
	Country string
	// distinct providers in the country
	Count int64
	// distinct located regions (states, provinces) with a provider
	RegionCount int64
	// distinct located cities with a provider
	CityCount int64
}

// CountProvidersByCountry returns the connected, valid, Public provider
// count per country — the same population and predicate as
// CountProviderCountries and the public providers map
// (GetProvidersMap), grouped by country code — with the distinct region
// and city counts of that population. Region and city location ids are
// scoped to their country, so summing the per-country region (city) counts
// gives the distinct region (city) count of the whole population. Country
// codes are stored lower case; they are returned upper case, the ISO form
// map gazetteers key on. Countries with no such provider are absent, so a
// consumer that keeps per-country state must treat absence as zero. The
// result is ordered by country code.
func CountProvidersByCountry(ctx context.Context) []ProviderCountryCount {
	counts := []ProviderCountryCount{}
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    location.country_code,
                    MIN(location.location_name),
                    COUNT(DISTINCT network_client_location_reliability.client_id),
                    COUNT(DISTINCT network_client_location_reliability.region_location_id),
                    COUNT(DISTINCT network_client_location_reliability.city_location_id)
                FROM network_client_location_reliability
                INNER JOIN location ON
                    location.location_id = network_client_location_reliability.country_location_id
                WHERE
                    network_client_location_reliability.connected = true AND
                    network_client_location_reliability.valid = true AND
                    EXISTS (
                        SELECT 1 FROM provide_key
                        WHERE
                            provide_key.client_id = network_client_location_reliability.client_id AND
                            provide_key.provide_mode = $1
                    )
                GROUP BY location.country_code
                ORDER BY location.country_code
            `,
			ProvideModePublic,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var count ProviderCountryCount
				server.Raise(result.Scan(
					&count.CountryCode,
					&count.Country,
					&count.Count,
					&count.RegionCount,
					&count.CityCount,
				))
				count.CountryCode = strings.ToUpper(strings.TrimSpace(count.CountryCode))
				counts = append(counts, count)
			}
		})
	})
	return counts
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
