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

// ProviderEgressProbeAttemptBackoff is how long a probe *attempt* defers a
// provider from being offered up again, whether or not the attempt succeeded.
//
// It is much shorter than the staleness window a successful probe buys
// (providerEgressDueAge in api/handlers, half ProviderEgressLocationMaxAge): a
// provider that fails to probe should be retried periodically -- the fault may
// be transient -- just not on every single poll, which is what starves the rest
// of the queue.
const ProviderEgressProbeAttemptBackoff = 6 * time.Hour

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

// ProviderEgressProbeAttempt records that the prober tried a provider, whether
// or not the try produced a location.
//
// A provider that has never been probed successfully has no
// ProviderEgressLocation row at all, so an attempt cannot be recorded there --
// see the provider_egress_probe_attempt migration for why that matters.
// ProbeFailure is "" for a successful attempt, otherwise a short failure class
// (`tunnel_failed`, `no_consensus`, ...).
type ProviderEgressProbeAttempt struct {
	ClientId     server.Id
	AttemptAt    time.Time
	ProbeFailure string
	UpdateTime   time.Time
}

// SetProviderEgressProbeAttempt upserts the last probe attempt for a provider.
//
// Like SetProviderEgressLocation the upsert is monotonic in its timestamp: a
// replayed or out-of-order report older than what is already stored is dropped
// rather than moving the provider's last-attempt time backwards, which would
// hand it back to the prober early.
func SetProviderEgressProbeAttempt(ctx context.Context, a *ProviderEgressProbeAttempt) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO provider_egress_probe_attempt (
				client_id,
				attempt_at,
				probe_failure,
				update_time
			)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (client_id) DO UPDATE
			SET
				attempt_at = $2,
				probe_failure = $3,
				update_time = $4
			WHERE provider_egress_probe_attempt.attempt_at < EXCLUDED.attempt_at
			`,
			a.ClientId,
			a.AttemptAt.UTC(),
			a.ProbeFailure,
			server.NowUtc(),
		))
	})
}

// GetProviderEgressProbeAttempt returns the last recorded probe attempt for a
// provider, or nil.
func GetProviderEgressProbeAttempt(ctx context.Context, clientId server.Id) *ProviderEgressProbeAttempt {
	var a *ProviderEgressProbeAttempt
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				client_id,
				attempt_at,
				probe_failure,
				update_time
			FROM provider_egress_probe_attempt
			WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				a = &ProviderEgressProbeAttempt{}
				server.Raise(result.Scan(
					&a.ClientId,
					&a.AttemptAt,
					&a.ProbeFailure,
					&a.UpdateTime,
				))
			}
		})
	})
	return a
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

// GetProviderEgressLocationDue returns the client ids of providers whose
// egress location is due for a probe: no fresh success (newest probe older than
// minObservedAt, or never probed) *and* no recent attempt (last attempt older
// than minAttemptAt, or never attempted). Oldest first, so the
// longest-unprobed are handed out first, capped at limit.
//
// This is the durable replacement for the prober's in-memory ttl cache: the
// schedule lives in the database, so a prober restart resumes where it left
// off instead of re-probing everything.
//
// Three things about the shape of this query matter.
//
// First, candidates are sourced from the live provider population
// (network_client_location_reliability, connected + valid) and the egress row
// is LEFT JOINed on. The dominant case by far is a provider that has *never*
// been probed and therefore has no provider_egress_location row at all;
// selecting from provider_egress_location would return exactly the providers
// that least need probing and none of the ones that most do.
//
// Second, only providers holding a Public provide key are returned. Probing
// tunnels through the provider itself, which means opening a contract from
// outside the provider's own network -- something a provider without a Public
// key refuses. Offering one to the prober would burn a probe slot on a
// guaranteed failure. This is the same filter UpdateClientLocations and
// UpdateClientScores apply (network_client_location_model.go).
//
// Third, a recent *attempt* defers a provider the same way a recent success
// does. Without that, a provider that connects and holds a Public provide key
// but always fails to probe -- for any reason other than the missing Public key
// screened for above -- never gets an egress row, so its observed_at stays
// NULL, so it sorts ahead of every stale-but-refreshable provider on every
// single poll, forever. Enough such providers to fill a batch and no healthy
// provider is ever refreshed again, while this endpoint goes on returning a
// full, plausible-looking batch. The in-memory ttl cache this replaced was
// incidentally immune, because it marked a provider probed whether or not the
// probe worked; moving the schedule server-side dropped that protection, and
// provider_egress_probe_attempt is what restores it.
//
// Both cutoffs are computed by the caller in Go and bound as parameters:
// observed_at and attempt_at are naive `timestamp` columns holding utc, and
// comparing them against sql now() would cast through the session timezone and
// silently skip a window.
//
// # Two passes, not one
//
// Expressed as a single statement this is a scan of
// network_client_location_reliability with two LEFT JOINs, sorted on
// observed_at from an outer-joined table. That sort cannot use an index: the
// column being ordered on does not exist for most of the rows being ordered.
// At beta's 40 providers that is free. At 100k it is a full scan plus an
// unindexable sort, on every poll.
//
// The ordering makes the split possible. `NULLS FIRST` means every never-probed
// provider sorts ahead of every probed one, so the result is always the
// concatenation of two independently ordered groups:
//
//  1. never probed -- no provider_egress_location row at all. This is the
//     dominant group (it is why the ordering is NULLS FIRST), and within it
//     every observed_at is equally absent, so the order is client_id alone. As
//     an anti-join with no outer-joined column in the ORDER BY it is an ordered
//     index scan over (valid, connected, client_id) with a LIMIT: no sort, and
//     it stops as soon as the batch is full.
//  2. stale but probed -- has a row, older than minObservedAt. Only reached
//     when pass 1 came up short of the limit. Driven from
//     provider_egress_location itself, where observed_at is a real, indexable
//     column: an ordered range scan over (observed_at, client_id).
//
// Both passes carry the same eligibility predicates, so the concatenation is
// row-for-row what the single statement returned, in the same order, under the
// same limit. `attempt_at IS NULL OR attempt_at < $n` becomes the equivalent
// `NOT EXISTS (... AND $n <= attempt_at)` -- equivalent because client_id is the
// primary key of provider_egress_probe_attempt, so there is at most one row to
// quantify over. The same holds for `observed_at IS NULL` on
// provider_egress_location, whose client_id is likewise a primary key and whose
// observed_at is NOT NULL: the only way that test is true is that no row exists.
func GetProviderEgressLocationDue(
	ctx context.Context,
	minObservedAt time.Time,
	minAttemptAt time.Time,
	limit int,
) []server.Id {
	clientIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_client_location_reliability.client_id
			FROM network_client_location_reliability

			LEFT JOIN provider_egress_location ON
				provider_egress_location.client_id = network_client_location_reliability.client_id

			LEFT JOIN provider_egress_probe_attempt ON
				provider_egress_probe_attempt.client_id = network_client_location_reliability.client_id

			WHERE
				network_client_location_reliability.connected = true AND
				network_client_location_reliability.valid = true AND
				EXISTS (
					SELECT 1 FROM provide_key
					WHERE
						provide_key.client_id = network_client_location_reliability.client_id AND
						provide_key.provide_mode = $1
				) AND
				(
					provider_egress_location.observed_at IS NULL OR
					provider_egress_location.observed_at < $2
				) AND
				(
					provider_egress_probe_attempt.attempt_at IS NULL OR
					provider_egress_probe_attempt.attempt_at < $3
				)

			-- never-probed sorts ahead of merely stale: a missing observed_at
			-- is infinitely old. client_id breaks the tie so batch composition
			-- is deterministic instead of plan-dependent -- otherwise the whole
			-- never-probed population ties on NULL and which slice of it the
			-- prober gets back under a limit is whatever order the executor
			-- happened to produce.
			ORDER BY
				provider_egress_location.observed_at ASC NULLS FIRST,
				network_client_location_reliability.client_id ASC
			LIMIT $4
			`,
			ProvideModePublic,
			minObservedAt.UTC(),
			minAttemptAt.UTC(),
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})
	return clientIds
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

// RemoveExpiredProviderEgressProbeAttempts drops attempts older than
// minAttemptAt. An attempt only carries information for as long as it defers
// the provider (ProviderEgressProbeAttemptBackoff); past that the row is just
// storage held for a client id that may no longer exist.
func RemoveExpiredProviderEgressProbeAttempts(ctx context.Context, minAttemptAt time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM provider_egress_probe_attempt WHERE attempt_at < $1`,
			minAttemptAt.UTC(),
		))
	})
}
