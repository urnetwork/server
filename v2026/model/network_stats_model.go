package model

// aggregate counts for the public network stats
// (controller/stats_collector.go)

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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

// the per-epoch users snapshot keys (see SetEpochUsersSnapshot). The prefix
// is new with the settlement epoch clock, so a snapshot of a Sunday block
// (stats.block_users.<n>, the pre-mainnet clock) is never read as an epoch's.
const statsEpochUsersRedisKeyPrefix = "stats.epoch_users."

// a snapshot outlives its epoch by a couple of epochs and then self-expires
const statsEpochUsersSnapshotTtl = 21 * 24 * time.Hour

// SetEpochUsersSnapshot records the running users count for a settlement
// epoch. The stats collector overwrites it on every refresh while the epoch
// is open, so the last write before the rollover is the finished epoch's
// final value — the previous-epoch reference the feed serves. A last-seen
// activity marker cannot reconstruct a past window, which is why the value
// is frozen forward rather than recomputed.
func SetEpochUsersSnapshot(ctx context.Context, epoch uint64, users int64) {
	server.Redis(ctx, func(client server.RedisClient) {
		key := fmt.Sprintf("%s%d", statsEpochUsersRedisKeyPrefix, epoch)
		_, err := client.Set(ctx, key, users, statsEpochUsersSnapshotTtl).Result()
		server.Raise(err)
	})
}

// GetEpochUsersSnapshot reads an epoch's users snapshot; ok is false when
// no snapshot was recorded (collector not running during that epoch).
func GetEpochUsersSnapshot(ctx context.Context, epoch uint64) (int64, bool) {
	var users int64
	var ok bool
	server.Redis(ctx, func(client server.RedisClient) {
		key := fmt.Sprintf("%s%d", statsEpochUsersRedisKeyPrefix, epoch)
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
	// the same providers split by the families they have proven (M2):
	// dualstack is both proven, ipv6 is v6 alone, ipv4 is everything else,
	// because a reliability row written before the proven columns existed
	// reads as v4-only everywhere else in this schema. The three sum to
	// Count.
	Ipv4Count      int64
	Ipv6Count      int64
	DualstackCount int64
}

// CountProvidersByCountry returns the active top-level, connected, valid,
// Public provider count per country — the same population and predicate as
// CountProviderCountries and the public providers map
// (GetProvidersMap), grouped by country code — with the distinct region
// and city counts of that population, and its split by proven ip family (M2:
// dualstack, ipv6 alone, everything else ipv4), which the same scan produces
// so the population is never counted twice. Region and city location ids are
// scoped to their country, so summing the per-country region (city) counts
// gives the distinct region (city) count of the whole population. Country
// codes are stored lower case; they are returned upper case, the ISO form
// map gazetteers key on. Countries with no such provider are absent, so a
// consumer that keeps per-country state must treat absence as zero. The
// result is ordered by country code.
func CountProvidersByCountry(ctx context.Context) []ProviderCountryCount {
	var counts []ProviderCountryCount
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		counts = countProvidersByCountry(ctx, conn)
	})
	return counts
}

// countProvidersByCountry is the connection-scoped form used when a caller
// already holds a replica connection (notably ComputeStats). Keeping the live
// public summaries on that same connection avoids opening a nested pool
// checkout during the hourly historical export.
func countProvidersByCountry(ctx context.Context, conn server.PgConn) []ProviderCountryCount {
	counts := []ProviderCountryCount{}
	result, err := conn.Query(
		ctx,
		`
                SELECT
                    location.country_code,
                    MIN(location.location_name),
                    COUNT(DISTINCT network_client_location_reliability.client_id),
                    COUNT(DISTINCT network_client_location_reliability.region_location_id),
                    COUNT(DISTINCT network_client_location_reliability.city_location_id),
                    COUNT(DISTINCT network_client_location_reliability.client_id) FILTER (
                        WHERE NOT network_client_location_reliability.ipv6_proven
                    ),
                    COUNT(DISTINCT network_client_location_reliability.client_id) FILTER (
                        WHERE
                            network_client_location_reliability.ipv6_proven AND
                            NOT network_client_location_reliability.ipv4_proven
                    ),
                    COUNT(DISTINCT network_client_location_reliability.client_id) FILTER (
                        WHERE
                            network_client_location_reliability.ipv4_proven AND
                            network_client_location_reliability.ipv6_proven
                    )
                FROM network_client_location_reliability
                INNER JOIN network_client ON
                    network_client.client_id = network_client_location_reliability.client_id
                INNER JOIN location ON
                    location.location_id = network_client_location_reliability.country_location_id
                WHERE
                    network_client.active = true AND
                    network_client.source_client_id IS NULL AND
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
				&count.Ipv4Count,
				&count.Ipv6Count,
				&count.DualstackCount,
			))
			count.CountryCode = strings.ToUpper(strings.TrimSpace(count.CountryCode))
			counts = append(counts, count)
		}
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
// pair). It is keyed by the top-level identity a stamp writes, so every
// child of one top-level client shares one entry: a hosted proxy's payers are
// its per-window children, and a gate keyed by payer never hit for them. The
// recorded time is the marker the database last reported, so the gate skips
// only a stamp the db-side predicate would skip too. After a process restart
// the worst case is one extra probe per top-level identity.
var contractTimeStampGate = newBoundedClientMap[time.Time](contractTimeStampGateMaxCount)

// bounds the gate map: top-level identities are few, but a payer whose
// top-level identity this process has not seen keys the gate by itself
const contractTimeStampGateMaxCount = 100_000

// The top-level identity of each contract client this process share-locked at
// a contract's lifecycle boundary (`lockActiveContractClientsInTx`), so the
// stamp after commit keys its gate without a round trip. source_client_id
// never changes once a client exists, so an entry stays true for the row's
// life.
var contractClientTopLevels = newBoundedClientMap[server.Id](contractClientTopLevelsMaxCount)

// An entry is needed only from the lifecycle read to the stamp after commit;
// the bound keeps per-window churn small.
const contractClientTopLevelsMaxCount = 16_384

// Remembers the top-level identity of a client read at a contract's lifecycle
// boundary: its source client, or the client itself when it is top-level.
func rememberContractClientTopLevel(clientId server.Id, sourceClientId *server.Id) {
	topLevelClientId := clientId
	if sourceClientId != nil {
		topLevelClientId = *sourceClientId
	}
	contractClientTopLevels.Store(clientId, topLevelClientId)
}

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
	// a payer this process has not seen at a lifecycle boundary keys the gate
	// by itself, which is exact for a top-level payer
	topLevelClientId, ok := contractClientTopLevels.Load(payerClientId)
	if !ok {
		topLevelClientId = payerClientId
	}
	if stampedTime, ok := contractTimeStampGate.Load(topLevelClientId); ok && now.Sub(stampedTime) < clientAuthTimeRefreshMinInterval {
		return
	}
	// claimed before the write, so concurrent contracts of one identity do
	// not all probe
	contractTimeStampGate.Store(topLevelClientId, now)

	server.HandleError(func() {
		defer func() {
			if r := recover(); r != nil {
				if err, ok := r.(error); ok && server.IsLockTimeout(err) {
					// the row's lockers held it past the bound: a later
					// contract stamps it after the retry interval, not the hour
					contractTimeStampGate.Store(topLevelClientId, now.Add(contractTimeStampRetryInterval-clientAuthTimeRefreshMinInterval))
					return
				}
				panic(r)
			}
		}()
		var stampedTopLevelClientId server.Id
		var contractTime *time.Time
		found := false
		// a bounded wait, so the stamp never pins a pooled server behind the
		// row's lockers (every contract creation share-locks client rows)
		server.Tx(ctx, func(tx server.PgTx) {
			found = false
			result, err := tx.Query(
				ctx,
				`
					WITH top_level AS (
						SELECT top.client_id, top.contract_time
						FROM network_client AS payer
						INNER JOIN network_client AS top ON
							top.client_id = COALESCE(payer.source_client_id, payer.client_id)
						WHERE payer.client_id = $1
					),
					stamp AS (
						UPDATE network_client
						SET contract_time = $2
						FROM top_level
						WHERE
							network_client.client_id = top_level.client_id AND
							(network_client.contract_time IS NULL OR network_client.contract_time < $3)
						RETURNING network_client.contract_time
					)
					SELECT
						top_level.client_id,
						COALESCE((SELECT stamp.contract_time FROM stamp), top_level.contract_time)
					FROM top_level
				`,
				payerClientId,
				now,
				now.Add(-clientAuthTimeRefreshMinInterval),
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&stampedTopLevelClientId, &contractTime))
					found = true
				}
			})
		}, server.TxReadCommitted, server.TxLockTimeout(contractTimeStampLockTimeout))
		if found && contractTime != nil {
			// the marker the database holds: no later than the true marker,
			// which only moves forward, so the gate never skips a stamp the
			// db-side predicate would make
			contractClientTopLevels.Store(payerClientId, stampedTopLevelClientId)
			contractTimeStampGate.Store(stampedTopLevelClientId, *contractTime)
		}
	})
}

// The stamp is best effort and trails a committed contract, so it waits for a
// busy row only briefly.
const contractTimeStampLockTimeout = 500 * time.Millisecond

// After a stamp the row's lockers refused, a later contract retries this soon
// rather than after the full refresh interval.
const contractTimeStampRetryInterval = time.Minute

// Testing_ResetContractTimeStampGate clears the process-local stamp
// throttle so a test can observe consecutive stamps.
func Testing_ResetContractTimeStampGate() {
	contractTimeStampGate.Clear()
	contractClientTopLevels.Clear()
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
// CountTopLevelClientsWithContractInEpoch is the feed's `users` definition
// (CountTopLevelClientsWithContractSince) applied to one settlement epoch
// window [startTime, endTime): distinct top-level client identities created
// before the window closed that had contract usage at or after it opened. It
// is the attested `total_users` of the epoch's signed payout artifact.
// contract_time is a last-seen marker, so the count is exact when taken at the
// epoch close (the StEpochClose task) and, recomputed later, can additionally
// include only clients that were active again after the close; it never counts
// a client created after the window.
func CountTopLevelClientsWithContractInEpoch(ctx context.Context, startTime time.Time, endTime time.Time) int64 {
	var count int64
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT COUNT(client_id)
                FROM network_client
                WHERE active = true AND source_client_id IS NULL AND $1 <= contract_time AND create_time < $2
            `,
			startTime,
			endTime,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

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
