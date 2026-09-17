package model

// aggregate counts of the extender population for the public network stats
// (connect/EXTENDER.md M2, M4; controller/stats_collector.go)

import (
	"context"
	"strings"

	"github.com/urnetwork/server/v2026"
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
