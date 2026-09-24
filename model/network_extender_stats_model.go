package model

// aggregate counts of the extender population for the public network stats
// (connect/EXTENDER.md M2, M4; controller/stats_collector.go)

import (
	"context"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// ExtenderCountryCount is the online extender population located in one
// country (see CountExtendersByCountry).
type ExtenderCountryCount struct {
	// upper-case ISO 3166-1 alpha-2 country code, empty for an extender whose
	// activation resolved no country at all
	CountryCode string
	// the country location's name, or the upper-case code for an extender
	// activated before the location ids existed (M1)
	Country string
	// online extenders in the country
	Count int64
	// the same extenders split by the families they have active addresses on
	// (M2): dualstack is both, ipv4 and ipv6 are the one family the extender
	// has. Every online extender has at least one active address and an
	// address is v4 or v6, so the three sum to Count.
	Ipv4Count      int64
	Ipv6Count      int64
	DualstackCount int64
}

// CountExtendersByCountry returns the online extender population per country.
//
// An online extender is an active network_extender row with at least one
// active address row (M2) — the same population the drip rotates through and
// the directory publishes, so a revoked extender and one whose every address
// ran out of probe attempts are both absent.
//
// The country code is the one the extender last activated from, upper-cased
// like the provider counts so both feed the same gazetteer. The country label
// is the name of the country location the activation resolved (M1); an
// extender activated before those columns existed has no country location, and
// its label falls back to the code. MIN over the group ignores nulls, so one
// filled row names the whole country group.
//
// An extender whose activation resolved no country at all is returned under
// the empty code rather than dropped: it is online, so it must count in the
// population total, and the caller decides whether a series with no country
// label is worth publishing (the collector counts it in the totals and leaves
// it out of the per-country gauge). The result is ordered by country code.
func CountExtendersByCountry(ctx context.Context) []ExtenderCountryCount {
	counts := []ExtenderCountryCount{}
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			WITH online AS (
				SELECT
					network_extender.extender_id,
					UPPER(network_extender.country_code) AS country_code,
					network_extender.country_location_id,
					BOOL_OR(network_extender_address.ip_version = 4) AS ipv4,
					BOOL_OR(network_extender_address.ip_version = 6) AS ipv6
				FROM network_extender
				INNER JOIN network_extender_address ON
					network_extender_address.extender_id = network_extender.extender_id AND
					network_extender_address.active
				WHERE network_extender.active
				GROUP BY network_extender.extender_id
			)
			SELECT
				online.country_code,
				MIN(location.location_name),
				COUNT(*),
				COUNT(*) FILTER (WHERE online.ipv4 AND NOT online.ipv6),
				COUNT(*) FILTER (WHERE online.ipv6 AND NOT online.ipv4),
				COUNT(*) FILTER (WHERE online.ipv4 AND online.ipv6)
			FROM online
			LEFT JOIN location ON
				location.location_id = online.country_location_id
			GROUP BY online.country_code
			ORDER BY online.country_code
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var count ExtenderCountryCount
				var country *string
				server.Raise(result.Scan(
					&count.CountryCode,
					&country,
					&count.Count,
					&count.Ipv4Count,
					&count.Ipv6Count,
					&count.DualstackCount,
				))
				count.CountryCode = strings.ToUpper(strings.TrimSpace(count.CountryCode))
				if country != nil && strings.TrimSpace(*country) != "" {
					count.Country = *country
				} else {
					count.Country = count.CountryCode
				}
				counts = append(counts, count)
			}
		})
	})
	return counts
}

// ExtenderGossipPublishCount is the state of the gossip release queue for one
// publish kind (see NetworkExtenderPublishKind*).
//
// Pending rows are what the gossip publisher has not drained yet, so the pair
// (pending, oldest pending) is the queue's health: a pending count that does
// not fall, or an oldest age that keeps climbing, means releases are not
// reaching the network even though activations are still writing rows.
type ExtenderGossipPublishCount struct {
	Kind int
	// Pending is rows with no published_time.
	Pending int64
	// OldestPendingSeconds is the age of the oldest pending row, 0 when none.
	OldestPendingSeconds float64
	// Released24h is rows published in the trailing 24 hours.
	Released24h int64
	// Extenders24h is the distinct extenders behind Released24h. A single
	// extender re-released repeatedly by the drip inflates the row count
	// without widening what the network actually learned, so the two are
	// reported separately.
	Extenders24h int64
}

// CountExtenderGossipPublish returns the gossip release queue per kind.
//
// Every known kind is returned even when it has no rows, so a kind that has
// gone quiet is a zero rather than an absent series -- the same rule the ip
// family gauges follow (M4).
func CountExtenderGossipPublish(ctx context.Context, now time.Time) []ExtenderGossipPublishCount {
	counts := map[int]*ExtenderGossipPublishCount{}
	for _, kind := range []int{
		NetworkExtenderPublishKindRecord,
		NetworkExtenderPublishKindRevocation,
	} {
		counts[kind] = &ExtenderGossipPublishCount{Kind: kind}
	}

	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				kind,
				COUNT(*) FILTER (WHERE published_time IS NULL),
				COALESCE(
					EXTRACT(EPOCH FROM ($1 - MIN(create_time) FILTER (WHERE published_time IS NULL))),
					0
				),
				COUNT(*) FILTER (WHERE $2 <= published_time),
				COUNT(DISTINCT extender_id) FILTER (WHERE $2 <= published_time)
			FROM network_extender_publish
			GROUP BY kind
			`,
			now.UTC(),
			now.UTC().Add(-24*time.Hour),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var count ExtenderGossipPublishCount
				server.Raise(result.Scan(
					&count.Kind,
					&count.Pending,
					&count.OldestPendingSeconds,
					&count.Released24h,
					&count.Extenders24h,
				))
				if known, ok := counts[count.Kind]; ok {
					*known = count
				}
			}
		})
	})

	ordered := []ExtenderGossipPublishCount{}
	for _, kind := range []int{
		NetworkExtenderPublishKindRecord,
		NetworkExtenderPublishKindRevocation,
	} {
		ordered = append(ordered, *counts[kind])
	}
	return ordered
}

// ExtenderHourContractCount is one extender's contracts in one hour bucket of
// create_time.
type ExtenderHourContractCount struct {
	ExtenderId server.Id
	Contracts  int64
}

// CountExtenderContractsByHour returns the contracts each extender carried in
// the hour beginning at hourStart.
//
// One grouped query over a closed hour, which is all the counter needs: a
// complete bucket never changes, so it is read once and never rescanned. That
// is what makes a per-extender time series affordable -- the alternative, a
// cumulative lifetime count refreshed every few minutes, rescans the whole
// table forever.
//
// Contracts are counted distinct: one contract names an extender once per
// party, so a contract with the same extender on both ends must not count
// twice.
func CountExtenderContractsByHour(ctx context.Context, hourStart time.Time) []ExtenderHourContractCount {
	counts := []ExtenderHourContractCount{}
	hourStart = hourStart.UTC().Truncate(time.Hour)
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				extender_id,
				COUNT(DISTINCT contract_id)
			FROM contract_extender
			WHERE $1 <= create_time AND create_time < $2
			GROUP BY extender_id
			`,
			hourStart,
			hourStart.Add(time.Hour),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var count ExtenderHourContractCount
				server.Raise(result.Scan(
					&count.ExtenderId,
					&count.Contracts,
				))
				counts = append(counts, count)
			}
		})
	})
	return counts
}

// The ping counts the extenders dashboard reads are in network_ping_model.go,
// beside the table they count.
