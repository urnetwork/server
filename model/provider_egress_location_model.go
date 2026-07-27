package model

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/urnetwork/server"
)

// ProviderEgressLocationMaxAge bounds how long a probed egress location is
// trusted. Past this, the location is ignored and the caller falls back to the
// mmdb lookup on the observed control ip.
const ProviderEgressLocationMaxAge = 7 * 24 * time.Hour

// ProviderEgressLocation is a provider location learned by probing the
// provider's own egress, rather than by looking up its control-connection ip.
type ProviderEgressLocation struct {
	ClientId      server.Id
	LocationId    server.Id
	CountryCode   string
	ASN           int
	Org           string
	Hosting       bool
	Proxy         bool
	Mobile        bool
	CityConfident bool
	ObservedAt    time.Time
	UpdateTime    time.Time
}

// SetProviderEgressLocation upserts the probed location for a provider. The
// upsert is monotonic in observed_at: a replayed or out-of-order submission
// older than what is already stored is silently dropped rather than
// clobbering a newer probe result.
func SetProviderEgressLocation(ctx context.Context, e *ProviderEgressLocation) {
	// country codes are stored/compared lowercased (see CreateLocation in
	// network_client_location_model.go); the geolocation APIs that feed this
	// return uppercase codes (e.g. "US"), so normalize before writing.
	countryCode := strings.ToLower(e.CountryCode)

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO provider_egress_location (
				client_id,
				location_id,
				country_code,
				asn,
				org,
				hosting,
				proxy,
				mobile,
				city_confident,
				observed_at,
				update_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (client_id) DO UPDATE
			SET
				location_id = $2,
				country_code = $3,
				asn = $4,
				org = $5,
				hosting = $6,
				proxy = $7,
				mobile = $8,
				city_confident = $9,
				observed_at = $10,
				update_time = $11
			WHERE provider_egress_location.observed_at < EXCLUDED.observed_at
			`,
			e.ClientId,
			e.LocationId,
			countryCode,
			e.ASN,
			e.Org,
			e.Hosting,
			e.Proxy,
			e.Mobile,
			e.CityConfident,
			e.ObservedAt.UTC(),
			server.NowUtc(),
		))
	})
}

// GetProviderEgressLocation returns the stored location for a provider, or nil.
func GetProviderEgressLocation(ctx context.Context, clientId server.Id) *ProviderEgressLocation {
	var e *ProviderEgressLocation
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				client_id,
				location_id,
				country_code,
				asn,
				org,
				hosting,
				proxy,
				mobile,
				city_confident,
				observed_at,
				update_time
			FROM provider_egress_location
			WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				e = &ProviderEgressLocation{}
				server.Raise(result.Scan(
					&e.ClientId,
					&e.LocationId,
					&e.CountryCode,
					&e.ASN,
					&e.Org,
					&e.Hosting,
					&e.Proxy,
					&e.Mobile,
					&e.CityConfident,
					&e.ObservedAt,
					&e.UpdateTime,
				))
			}
		})
	})
	return e
}

// GetFreshProviderEgressLocation is GetProviderEgressLocation, filtered to
// entries probed within maxAge. The cutoff is computed in Go and bound as a
// parameter: observed_at is a naive timestamp holding utc, and comparing it
// against sql now() would cast through the session timezone.
func GetFreshProviderEgressLocation(
	ctx context.Context,
	clientId server.Id,
	maxAge time.Duration,
) *ProviderEgressLocation {
	e := GetProviderEgressLocation(ctx, clientId)
	if e == nil {
		return nil
	}
	if e.ObservedAt.Before(server.NowUtc().Add(-maxAge)) {
		return nil
	}
	return e
}

// GetFreshProviderEgressLocationForConnection resolves the probed provider
// egress location for a connection in a single query, joining
// network_client_connection to provider_egress_location on client_id. This
// exists for the connect-announce hot path (SetConnectionLocation), which
// previously spent two round trips per connection -- resolving the client id
// for the connection, then fetching its fresh egress location -- before ever
// reaching the mmdb fallback; collapsing to one query matters on a path that
// runs for every connection and inside a retry loop.
//
// As with GetFreshProviderEgressLocation, the maxAge cutoff is computed in Go
// and compared in Go: observed_at is a naive timestamp holding utc, and
// comparing it against sql now() would cast through the session timezone.
func GetFreshProviderEgressLocationForConnection(
	ctx context.Context,
	connectionId server.Id,
	maxAge time.Duration,
) *ProviderEgressLocation {
	var e *ProviderEgressLocation
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				pel.client_id,
				pel.location_id,
				pel.country_code,
				pel.asn,
				pel.org,
				pel.hosting,
				pel.proxy,
				pel.mobile,
				pel.city_confident,
				pel.observed_at,
				pel.update_time
			FROM network_client_connection ncc
			INNER JOIN provider_egress_location pel ON pel.client_id = ncc.client_id
			WHERE ncc.connection_id = $1
			`,
			connectionId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				e = &ProviderEgressLocation{}
				server.Raise(result.Scan(
					&e.ClientId,
					&e.LocationId,
					&e.CountryCode,
					&e.ASN,
					&e.Org,
					&e.Hosting,
					&e.Proxy,
					&e.Mobile,
					&e.CityConfident,
					&e.ObservedAt,
					&e.UpdateTime,
				))
			}
		})
	})
	if e == nil {
		return nil
	}
	if e.ObservedAt.Before(server.NowUtc().Add(-maxAge)) {
		return nil
	}
	return e
}

// GetLocation returns the canonical location row, or nil.
//
// Note: the location table also has a location_name column, but it holds the
// name for whichever granularity that specific row represents (e.g. a city
// row's own name), not a single name field on the Location struct. Location
// instead splits City/Region/Country by joining sibling rows (see
// IndexSearchLocationsInTx in network_client_location_model.go). This helper
// only needs to resolve identity/type, so it selects the columns that map
// directly onto Location's fields and leaves City/Region/Country empty.
func GetLocation(ctx context.Context, locationId server.Id) *Location {
	var loc *Location
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT location_id, location_type, city_location_id, region_location_id, country_location_id, country_code
			FROM location
			WHERE location_id = $1
			`,
			locationId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				loc = &Location{}
				// city_location_id/region_location_id are only set once the
				// row's hierarchy reaches that granularity (e.g. a country
				// row has both NULL); server.Id.Scan errors on a nil source,
				// so scan through nullable pointers as in
				// IndexSearchLocationsInTx (network_client_location_model.go).
				var cityLocationId *server.Id
				var regionLocationId *server.Id
				var countryLocationId *server.Id
				server.Raise(result.Scan(
					&loc.LocationId,
					&loc.LocationType,
					&cityLocationId,
					&regionLocationId,
					&countryLocationId,
					&loc.CountryCode,
				))
				if cityLocationId != nil {
					loc.CityLocationId = *cityLocationId
				}
				if regionLocationId != nil {
					loc.RegionLocationId = *regionLocationId
				}
				if countryLocationId != nil {
					loc.CountryLocationId = *countryLocationId
				}
			}
		})
	})
	return loc
}

// normalizeLocationName folds a location name to a comparison key: lowercased,
// with every rune that is not a letter or a digit dropped. So
// "Frankfurt am Main", "Frankfurt Am Main" and "FRANKFURT AM MAIN" all fold to
// "frankfurtammain" and match the one row that already exists.
//
// This is deliberately a comparison key only -- it is never stored, and never
// used to build a location_name. It exists so a trivial spelling variant from a
// geolocation source resolves to the existing row instead of being treated as a
// different place.
//
// Punctuation is dropped rather than mapped to a space because the disagreement
// is over whether the separator exists at all ("Washington, D.C." vs
// "Washington DC"). Note this deliberately does not fold "Frankfurt/Main" onto
// "Frankfurt am Main": dropping the separator gives "frankfurtmain" !=
// "frankfurtammain", so that one falls back to country granularity rather than
// matching the wrong row. Falling back is the safe outcome; guessing is not.
// Stdlib only, by design -- a transliteration/fuzzy-match dependency is a large
// amount of new behaviour to take on for an ingest path whose failure mode is
// already "use the country".
func normalizeLocationName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// matchLocationNameInTx returns the location_id of the row in `candidates`
// whose location_name matches `name`, preferring an exact match and falling
// back to a normalized one (see normalizeLocationName), or nil for no match.
// Candidates must already be ordered deterministically by the caller so that
// two rows folding to the same key always resolve the same way.
func matchLocationName(name string, candidateIds []server.Id, candidateNames []string) *server.Id {
	for i, candidateName := range candidateNames {
		if candidateName == name {
			return &candidateIds[i]
		}
	}
	normalized := normalizeLocationName(name)
	if normalized == "" {
		// nothing comparable survives folding (e.g. a name of only
		// punctuation); an empty key would match any other such row
		return nil
	}
	for i, candidateName := range candidateNames {
		if normalizeLocationName(candidateName) == normalized {
			return &candidateIds[i]
		}
	}
	return nil
}

// MatchExistingLocation resolves (countryCode, region, city) against location
// rows that ALREADY EXIST and returns the city-granular row, or nil if any
// level of the hierarchy does not resolve. It never inserts anything.
//
// This is the resolver the provider egress ingest path uses instead of
// CreateLocation. CreateLocation deduplicates a city on its exact
// location_name, so an unrecognised spelling does not fail -- it silently
// creates a new, permanent row in the shared `location` table and indexes it
// for search. A geolocation probe has no business defining the world's cities:
// the three free sources the prober reaches consensus over demonstrably
// disagree on spelling (we observed "Frankfurt am Main (Innenstadt I)" against
// "Frankfurt am Main" for one host), and the consensus stores the winning
// source's original display string. Each variant would become its own row,
// those rows outlive a code revert, and there is no cleanup path.
//
// Matching is case-insensitive and ignores punctuation and whitespace
// differences, so the ordinary variants resolve to the row that is already
// there. When nothing resolves the caller falls back to country granularity --
// see SubmitProviderEgressLocation. Falling back loses precision for one
// submission; creating a row corrupts shared data permanently.
//
// Each level tries an exact, fully-indexed match first (the common case: the
// winning source usually spells it the way the mmdb import did) and only scans
// the level's candidates when that misses.
func MatchExistingLocation(
	ctx context.Context,
	countryCode string,
	region string,
	city string,
) *Location {
	countryCode = strings.ToLower(strings.TrimSpace(countryCode))
	region = strings.TrimSpace(region)
	city = strings.TrimSpace(city)
	if countryCode == "" || region == "" || city == "" {
		return nil
	}

	var match *Location
	server.Db(ctx, func(conn server.PgConn) {
		// country: keyed on country_code alone, exactly as CreateLocation
		// dedupes it, so there is no name to match here
		var countryLocationId server.Id
		var countryName string
		found := false
		result, err := conn.Query(
			ctx,
			`
			SELECT location_id, location_name
			FROM location
			WHERE location_type = $1 AND country_code = $2
			ORDER BY location_id
			LIMIT 1
			`,
			LocationTypeCountry,
			countryCode,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&countryLocationId, &countryName))
				found = true
			}
		})
		if !found {
			return
		}

		// region, within that country
		regionLocationId := matchChildLocation(
			ctx,
			conn,
			LocationTypeRegion,
			countryCode,
			region,
			`
			SELECT location_id, location_name
			FROM location
			WHERE
				location_type = $1 AND
				country_code = $2 AND
				location_name = $3 AND
				country_location_id = $4
			`,
			`
			SELECT location_id, location_name
			FROM location
			WHERE
				location_type = $1 AND
				country_code = $2 AND
				country_location_id = $3
			ORDER BY location_id
			`,
			[]any{countryLocationId},
		)
		if regionLocationId == nil {
			return
		}

		// city, within that region
		cityLocationId := matchChildLocation(
			ctx,
			conn,
			LocationTypeCity,
			countryCode,
			city,
			`
			SELECT location_id, location_name
			FROM location
			WHERE
				location_type = $1 AND
				country_code = $2 AND
				location_name = $3 AND
				region_location_id = $4 AND
				country_location_id = $5
			`,
			`
			SELECT location_id, location_name
			FROM location
			WHERE
				location_type = $1 AND
				country_code = $2 AND
				region_location_id = $3 AND
				country_location_id = $4
			ORDER BY location_id
			`,
			[]any{*regionLocationId, countryLocationId},
		)
		if cityLocationId == nil {
			return
		}

		match = &Location{
			LocationType:      LocationTypeCity,
			City:              city,
			Region:            region,
			Country:           countryName,
			CountryCode:       countryCode,
			LocationId:        *cityLocationId,
			CityLocationId:    *cityLocationId,
			RegionLocationId:  *regionLocationId,
			CountryLocationId: countryLocationId,
		}
	})
	return match
}

// matchChildLocation runs the exact-match query first and only falls back to
// scanning the level's candidates when it misses. `parents` are the parent
// location ids the two queries scope on: the exact query binds them after
// (location_type, country_code, name), the candidate query after
// (location_type, country_code).
func matchChildLocation(
	ctx context.Context,
	conn server.PgConn,
	locationType LocationType,
	countryCode string,
	name string,
	exactSql string,
	candidatesSql string,
	parents []any,
) *server.Id {
	exactArgs := append([]any{locationType, countryCode, name}, parents...)
	var exactId *server.Id
	result, err := conn.Query(ctx, exactSql, exactArgs...)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var locationId server.Id
			var locationName string
			server.Raise(result.Scan(&locationId, &locationName))
			exactId = &locationId
		}
	})
	if exactId != nil {
		return exactId
	}

	candidateArgs := append([]any{locationType, countryCode}, parents...)
	candidateIds := []server.Id{}
	candidateNames := []string{}
	result, err = conn.Query(ctx, candidatesSql, candidateArgs...)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var locationId server.Id
			var locationName string
			server.Raise(result.Scan(&locationId, &locationName))
			candidateIds = append(candidateIds, locationId)
			candidateNames = append(candidateNames, locationName)
		}
	})
	return matchLocationName(name, candidateIds, candidateNames)
}

// RemoveExpiredProviderEgressLocations drops entries probed before
// minObservedAt.
func RemoveExpiredProviderEgressLocations(ctx context.Context, minObservedAt time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM provider_egress_location WHERE observed_at < $1`,
			minObservedAt.UTC(),
		))
	})
}
