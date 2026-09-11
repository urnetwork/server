package model

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/urnetwork/server/v2026"
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

// the verdict/assurance values a provider_egress_location row can hold. These
// mirror the column defaults, and are what an unjudged submission is normalized
// to on write -- see SetProviderEgressLocation.
const (
	// ProviderEgressVerdictUnverified is the default: no judgement recorded.
	// Every row written before the ingest path computed verdicts reads as this.
	ProviderEgressVerdictUnverified = "unverified"
	// ProviderEgressAssuranceDirect means the probe reached the provider over a
	// single tunnel from the prober. It is the only assurance in use; multi-hop
	// is P3's concern.
	ProviderEgressAssuranceDirect = "direct"
)

// ProviderEgressLocation is a provider location learned by probing the
// provider's own egress, rather than by looking up its control-connection ip.
//
// Verdict/VerdictReason/Assurance carry the recorded judgement for the probe
// that produced this location. They are advisory: nothing in provider selection
// or scoring reads them. An empty Verdict or Assurance is normalized to the
// column default on write, so a caller that does not compute a judgement stores
// an unjudged direct probe rather than an empty string.
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
	Verdict       string
	VerdictReason string
	Assurance     string
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

	// the verdict columns are NOT NULL with defaults, and this INSERT names
	// every column explicitly -- which bypasses those defaults. A caller that
	// computes no judgement would otherwise store '' rather than 'unverified'
	// and '' rather than 'direct', so normalize here the way countryCode is
	// normalized above. VerdictReason has no default value to fall back to: ""
	// means "no reason", which is exactly the column default.
	verdict := e.Verdict
	if verdict == "" {
		verdict = ProviderEgressVerdictUnverified
	}
	assurance := e.Assurance
	if assurance == "" {
		assurance = ProviderEgressAssuranceDirect
	}

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
				verdict,
				verdict_reason,
				assurance,
				update_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
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
				verdict = $11,
				verdict_reason = $12,
				assurance = $13,
				update_time = $14
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
			verdict,
			e.VerdictReason,
			assurance,
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
				verdict,
				verdict_reason,
				assurance,
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
					&e.Verdict,
					&e.VerdictReason,
					&e.Assurance,
					&e.UpdateTime,
				))
			}
		})
	})
	return e
}

// GetAllProviderEgressCountryCodes reads every provider's latest observed
// egress country in one query, for callers that need to check many providers
// in a single pass. Mirrors GetAllProviderEgressHealthCounts: the counting
// loop in UpdateClientLocations runs over the whole provider population, so a
// per-provider query there would be one round trip per provider.
//
// Codes are lowercased so callers can compare directly against
// location.country_code without normalising at each site.
//
// A provider with no observed location is ABSENT from the map rather than
// present with an empty string. Callers use the two-value lookup and treat
// absence as "not verified", which fails closed.
//
// Only rows observed within ProviderEgressLocationMaxAge are returned, matching
// the bound every other reader applies via GetFreshProviderEgressLocation. That
// invariant is load bearing: the sweeper deliberately retains rows far longer
// than the trust window (see the comment in
// taskworker/work/provider_egress_location_work.go, "reads already ignore stale
// rows"), so an unbounded read here would keep counting a provider as supply
// for a country it was probed in weeks ago and has since moved out of -- the
// exact "advertised in a country it does not egress from" defect this gate
// exists to close. A provider whose only row has aged out is ABSENT, same as
// one never probed, which fails closed.
//
// The cutoff is computed in Go and bound as a parameter rather than written
// into the SQL: observed_at is a naive timestamp holding utc, so comparing it
// against sql now() would cast through the session timezone. This mirrors
// GetFreshProviderEgressLocation exactly.
//
// Verdicts are DELIBERATELY not consulted: a row recorded 'suspect' or
// 'unstable' counts the same as 'verified'. That is consistent with the
// standing rule that verdicts are advisory and nothing in selection or scoring
// reads them (see the ProviderEgressLocation doc comment). Revisit only when
// verdicts become authoritative fleet-wide -- filtering on them here alone
// would silently drop providers from the public count on the strength of a
// judgement nothing else in the system honors, and would shrink this map
// enough to matter for the fleet-wide floor in shouldSkipCountGate.
func GetAllProviderEgressCountryCodes(ctx context.Context) map[server.Id]string {
	countryCodes := map[server.Id]string{}

	minObservedAt := server.NowUtc().Add(-ProviderEgressLocationMaxAge)

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT client_id, country_code
			FROM provider_egress_location
			WHERE
				observed_at >= $1 AND
				country_code IS NOT NULL AND country_code != ''
			`,
			minObservedAt,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var countryCode string
				server.Raise(result.Scan(&clientId, &countryCode))
				countryCodes[clientId] = strings.ToLower(countryCode)
			}
		})
	})

	return countryCodes
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
				pel.verdict,
				pel.verdict_reason,
				pel.assurance,
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
					&e.Verdict,
					&e.VerdictReason,
					&e.Assurance,
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
// accent-stripped, with every rune that is not a letter or a digit dropped. So
// "Frankfurt am Main", "Frankfurt Am Main" and "FRANKFURT AM MAIN" all fold to
// "frankfurtammain", and "São Paulo", "Zürich" and "Kraków" fold to the same
// keys as "Sao Paulo", "Zurich" and "Krakow".
//
// This is deliberately a comparison key only -- it is never stored, and never
// used to build a location_name. It exists so a trivial spelling variant from a
// geolocation source resolves to the existing row instead of being treated as a
// different place.
//
// Diacritics are the single biggest source of these variants: the free
// geolocation sources disagree over whether to emit the local spelling or an
// ASCII transliteration for the same city, and the mmdb import that seeded most
// existing rows made its own choice. Folding is done with an NFD decomposition
// followed by dropping the combining marks (unicode.Mn), which covers the whole
// accent class at once -- a hand-rolled é->e table would have to enumerate the
// world's diacritics and would silently keep missing the ones it forgot.
//
// golang.org/x/text is already a dependency of this module, so this costs no
// new supply-chain surface. (An earlier revision of this function was
// stdlib-only by mistake: that constraint belongs to the prober repo's
// `geolocate` package, not to the server.)
//
// Letters that carry a stroke rather than a combining mark -- ł, ø, đ -- do not
// decompose and therefore still do not fold onto l/o/d. Those fall back to
// country granularity, which is the safe outcome; the alternative is the
// transliteration table this function deliberately avoids.
//
// Punctuation is dropped rather than mapped to a space because the disagreement
// is over whether the separator exists at all ("Washington, D.C." vs
// "Washington DC"). Note this deliberately does not fold "Frankfurt/Main" onto
// "Frankfurt am Main": dropping the separator gives "frankfurtmain" !=
// "frankfurtammain", so that one falls back to country granularity rather than
// matching the wrong row. Falling back is the safe outcome; guessing is not.
func normalizeLocationName(name string) string {
	// NFD splits a precomposed letter ("ü") into its base letter plus a
	// combining mark ("u" + U+0308). The mark is category Mn, which is neither
	// a letter nor a digit, so the filter below drops it; the explicit Mn skip
	// is there to say so rather than to leave it to a category coincidence.
	decomposed := norm.NFD.String(strings.ToLower(name))
	var b strings.Builder
	b.Grow(len(decomposed))
	for _, r := range decomposed {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stripParentheticals removes every parenthesised span from name, so
// "Frankfurt am Main (Innenstadt I)" becomes "Frankfurt am Main ". Nesting is
// tracked, and an unclosed "(" swallows the rest of the string -- a qualifier
// that was truncated by a length limit is still a qualifier.
//
// This is only ever applied to a comparison key, never to anything stored.
func stripParentheticals(name string) string {
	if !strings.ContainsRune(name, '(') {
		return name
	}
	var b strings.Builder
	b.Grow(len(name))
	depth := 0
	for _, r := range name {
		switch r {
		case '(':
			depth += 1
		case ')':
			if 0 < depth {
				depth -= 1
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// matchLocationName returns the location_id of the row in `candidates` whose
// location_name matches `name`, or nil for no match. Candidates must already be
// ordered deterministically by the caller so that two rows folding to the same
// key always resolve the same way.
//
// Three passes, narrowest first:
//
//  1. exact string equality -- the common case, since the winning source usually
//     spells it the way the mmdb import did;
//  2. the normalized fold (see normalizeLocationName): case, punctuation,
//     whitespace and accents;
//  3. the normalized fold with parenthesised qualifiers stripped from BOTH
//     sides. This is the case that motivated the whole feature: one source
//     reports "Frankfurt am Main (Innenstadt I)" -- a district qualifier -- for
//     a host another source calls "Frankfurt am Main". Pass 2 cannot see
//     through that, because dropping the parentheses as punctuation leaves the
//     qualifier's letters in the key.
//
// Pass 3 is the only pass that can plausibly match the wrong row -- two
// same-region rows "Springfield (IL)" and "Springfield (MA)" both reduce to
// "springfield" -- so it requires the stripped key to identify exactly ONE
// candidate and returns nil on any ambiguity. Falling back to country
// granularity is the safe outcome; guessing is not.
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

	base := normalizeLocationName(stripParentheticals(name))
	if base == "" {
		// the name was nothing but a qualifier
		return nil
	}
	var unique *server.Id
	for i, candidateName := range candidateNames {
		if normalizeLocationName(stripParentheticals(candidateName)) == base {
			if unique != nil {
				return nil
			}
			unique = &candidateIds[i]
		}
	}
	return unique
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
// Matching is case-insensitive and ignores punctuation, whitespace, accents and
// parenthesised district qualifiers (see matchLocationName), so the ordinary
// variants resolve to the row that is already there -- including the
// "Frankfurt am Main (Innenstadt I)" case above, which is what this exists for.
// When nothing resolves the caller falls back to country granularity --
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

const providerEgressStaleHealthDueQuery = `
	SELECT
		provider_egress_health.client_id,
		provider_egress_health.measured_at
	FROM provider_egress_health
	INNER JOIN provider_egress_location ON
		provider_egress_location.client_id = provider_egress_health.client_id
	INNER JOIN network_client_location_reliability ON
		network_client_location_reliability.client_id = provider_egress_health.client_id
	INNER JOIN network_client ON
		network_client.client_id = provider_egress_health.client_id

	WHERE
		provider_egress_health.measured_at < $2 AND
		network_client.active = true AND
		network_client.source_client_id IS NULL AND
		network_client_location_reliability.connected = true AND
		network_client_location_reliability.valid = true AND
		EXISTS (
			SELECT 1 FROM provide_key
			WHERE
				provide_key.client_id = provider_egress_health.client_id AND
				provide_key.provide_mode = $1
		) AND
		NOT EXISTS (
			SELECT 1 FROM provider_egress_probe_attempt
			WHERE
				provider_egress_probe_attempt.client_id = provider_egress_health.client_id AND
				$3 <= provider_egress_probe_attempt.attempt_at
		) AND
		(
			$5 <= 1 OR
			((hashtext(provider_egress_health.client_id::text) % $5) + $5) % $5 = $6
		)

	ORDER BY
		provider_egress_health.measured_at ASC,
		provider_egress_health.client_id ASC
	LIMIT $4
`

// GetProviderEgressLocationDue returns the client ids of providers whose full
// egress probe is due: their location or health evidence is stale, their
// location has no corresponding health evidence, or they have no location
// evidence, and they have no attempt newer than minAttemptAt.
//
// This is the durable replacement for the prober's in-memory ttl cache: the
// schedule lives in the database, so a prober restart resumes where it left
// off instead of re-probing everything.
//
// Three things about the shape of this query matter.
//
// First, every lane is joined back to the live provider population: active
// top-level clients with a connected + valid location-reliability row. The
// unlocated lane is sourced from that population directly because its dominant
// case has no provider_egress_location row at all.
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
// The observed-at and attempt-at cutoffs are computed by the caller in Go and
// bound as parameters. The health cutoff is likewise computed in Go from its
// model lifetime. All three columns are naive `timestamp` values holding UTC;
// comparing them to SQL now() would cast through the session timezone.
//
// # Bounded indexed heads, not one outer-join sort
//
// A single statement over the complete live-provider population cannot use the
// evidence timestamps' ordered indexes for a global deadline sort. Instead,
// the location and health tables supply bounded oldest-evidence heads through
// (timestamp, client_id). A third location-indexed head finds providers whose
// accepted location has no health row, which is possible because health
// reporting is non-fatal to location submission. The health head requires an
// extant location, so retained health history after location cleanup remains
// exclusively in the unlocated lane. Go deduplicates those heads and merges
// them by absolute hard expiry: observed_at +
// ProviderEgressLocationMaxAge, measured_at + ProviderEgressHealthMaxAge, or
// observed_at + ProviderEgressHealthMaxAge for missing health. Equal deadlines
// use client_id, so every limit is deterministic. Taking the requested limit
// from each head is sufficient even when they overlap: a head that reaches the
// limit alone supplies that many distinct rows, while a shorter head has
// exposed all candidates in its lane.
//
// Urgent evidence refreshes are admitted before the unlocated lane. The latter
// remains a separately bounded anti-join ordered by client_id and fills only
// unused capacity. Its six-hour attempt floor still prevents an immediate
// failed retry from occupying every poll. This ordering deliberately does not
// choose the unresolved policy between first attempts and retries inside the
// unlocated lane; retaining their latest attempt rows preserves the evidence a
// later explicit policy can use.
//
// Every head repeats the same active, top-level, connected, valid, Public-key,
// recent-attempt, and normalized-shard predicates. Separate statements can see
// a provider cross categories between snapshots, so the Go merge also
// deduplicates unlocated rows before returning them.
//
// GetProviderEgressLocationDue is the unsharded queue: one prober takes the
// whole fleet. Equivalent to GetProviderEgressLocationDueSharded with a single
// shard, and kept so existing callers are unaffected.
func GetProviderEgressLocationDue(
	ctx context.Context,
	minObservedAt time.Time,
	minAttemptAt time.Time,
	limit int,
) []server.Id {
	return GetProviderEgressLocationDueSharded(ctx, minObservedAt, minAttemptAt, limit, 0, 1)
}

// GetProviderEgressLocationDueSharded partitions the queue across independent
// workers via shardIndex/shardCount.
//
// Without partitioning, every worker polling inside the attempt-backoff window
// receives the SAME rows. The queue hands work out but never claims it, and the
// deduplicating NOT EXISTS on provider_egress_probe_attempt only bites once an
// attempt row lands -- at submit time, minutes after the batch went out, which
// is exactly when the other workers are polling. N workers therefore repeat the
// same work instead of dividing it.
//
// Hashing client_id gives each task a disjoint slice with no per-provider locks,
// leases, or new columns. Main stores exactly one recurring task per slice in
// the shared task queue, so a worker that goes away does not own or strand its
// slice; another taskworker can claim the same durable task.
//
// shardCount <= 1 disables sharding, which is the single-prober case.
func GetProviderEgressLocationDueSharded(
	ctx context.Context,
	minObservedAt time.Time,
	minAttemptAt time.Time,
	limit int,
	shardIndex int,
	shardCount int,
) []server.Id {
	clientIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		type dueCandidate struct {
			clientId server.Id
			deadline time.Time
		}

		// The urgent heads may overlap completely. A limit-sized head from each
		// preserves a bounded top-limit union after deduplication.
		headLimit := limit
		urgentByClient := map[server.Id]dueCandidate{}
		addUrgent := func(candidate dueCandidate) {
			if previous, ok := urgentByClient[candidate.clientId]; !ok || candidate.deadline.Before(previous.deadline) {
				urgentByClient[candidate.clientId] = candidate
			}
		}

		// Stale location head. The composite observed_at/client_id index owns
		// both the cutoff and the complete stable ordering.
		result, err := conn.Query(
			ctx,
			`
			SELECT
				provider_egress_location.client_id,
				provider_egress_location.observed_at
			FROM provider_egress_location
			INNER JOIN network_client_location_reliability ON
				network_client_location_reliability.client_id = provider_egress_location.client_id
			INNER JOIN network_client ON
				network_client.client_id = provider_egress_location.client_id

			WHERE
				provider_egress_location.observed_at < $2 AND
				network_client.active = true AND
				network_client.source_client_id IS NULL AND
				network_client_location_reliability.connected = true AND
				network_client_location_reliability.valid = true AND
				EXISTS (
					SELECT 1 FROM provide_key
					WHERE
						provide_key.client_id = provider_egress_location.client_id AND
						provide_key.provide_mode = $1
				) AND
				NOT EXISTS (
					SELECT 1 FROM provider_egress_probe_attempt
					WHERE
						provider_egress_probe_attempt.client_id = provider_egress_location.client_id AND
						$3 <= provider_egress_probe_attempt.attempt_at
				) AND
				(
					$5 <= 1 OR
					((hashtext(provider_egress_location.client_id::text) % $5) + $5) % $5 = $6
				)

			ORDER BY
				provider_egress_location.observed_at ASC,
				provider_egress_location.client_id ASC
			LIMIT $4
			`,
			ProvideModePublic,
			minObservedAt.UTC(),
			minAttemptAt.UTC(),
			headLimit,
			shardCount,
			shardIndex,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var observedAt time.Time
				server.Raise(result.Scan(&clientId, &observedAt))
				addUrgent(dueCandidate{
					clientId: clientId,
					deadline: observedAt.Add(ProviderEgressLocationMaxAge),
				})
			}
		})

		// Stale health head. It requires a current location so a retained health
		// row whose location was removed remains exclusively in the no-location
		// lane. Located providers may overlap the stale-location head; a provider
		// with both deadlines keeps the earlier one.
		minMeasuredAt := server.NowUtc().Add(-ProviderEgressHealthMaxAge / 2)
		result, err = conn.Query(
			ctx,
			providerEgressStaleHealthDueQuery,
			ProvideModePublic,
			minMeasuredAt.UTC(),
			minAttemptAt.UTC(),
			headLimit,
			shardCount,
			shardIndex,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var measuredAt time.Time
				server.Raise(result.Scan(&clientId, &measuredAt))
				addUrgent(dueCandidate{
					clientId: clientId,
					deadline: measuredAt.Add(ProviderEgressHealthMaxAge),
				})
			}
		})

		// Missing-health head. Location submission remains valid when the
		// prober's independent health check is skipped or its non-fatal report
		// fails, while provider publication fails closed without a health row.
		// Drive this anti-join from the existing ordered location index and use
		// the location observation as the start of the existing health lifetime.
		// The common attempt predicate prevents an immediate retry after the
		// normal path reports its just-completed full-probe attempt.
		result, err = conn.Query(
			ctx,
			`
			SELECT
				provider_egress_location.client_id,
				provider_egress_location.observed_at
			FROM provider_egress_location
			INNER JOIN network_client_location_reliability ON
				network_client_location_reliability.client_id = provider_egress_location.client_id
			INNER JOIN network_client ON
				network_client.client_id = provider_egress_location.client_id

			WHERE
				network_client.active = true AND
				network_client.source_client_id IS NULL AND
				network_client_location_reliability.connected = true AND
				network_client_location_reliability.valid = true AND
				EXISTS (
					SELECT 1 FROM provide_key
					WHERE
						provide_key.client_id = provider_egress_location.client_id AND
						provide_key.provide_mode = $1
				) AND
				NOT EXISTS (
					SELECT 1 FROM provider_egress_health
					WHERE
						provider_egress_health.client_id = provider_egress_location.client_id
				) AND
				NOT EXISTS (
					SELECT 1 FROM provider_egress_probe_attempt
					WHERE
						provider_egress_probe_attempt.client_id = provider_egress_location.client_id AND
						$2 <= provider_egress_probe_attempt.attempt_at
				) AND
				(
					$4 <= 1 OR
					((hashtext(provider_egress_location.client_id::text) % $4) + $4) % $4 = $5
				)

			ORDER BY
				provider_egress_location.observed_at ASC,
				provider_egress_location.client_id ASC
			LIMIT $3
			`,
			ProvideModePublic,
			minAttemptAt.UTC(),
			headLimit,
			shardCount,
			shardIndex,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var observedAt time.Time
				server.Raise(result.Scan(&clientId, &observedAt))
				addUrgent(dueCandidate{
					clientId: clientId,
					deadline: observedAt.Add(ProviderEgressHealthMaxAge),
				})
			}
		})

		urgent := make([]dueCandidate, 0, len(urgentByClient))
		for _, candidate := range urgentByClient {
			urgent = append(urgent, candidate)
		}
		sort.Slice(urgent, func(i, j int) bool {
			if urgent[i].deadline.Equal(urgent[j].deadline) {
				return urgent[i].clientId.Less(urgent[j].clientId)
			}
			return urgent[i].deadline.Before(urgent[j].deadline)
		})
		for _, candidate := range urgent {
			if len(clientIds) == limit {
				break
			}
			clientIds = append(clientIds, candidate.clientId)
		}

		if len(clientIds) >= limit {
			return
		}

		// Unlocated providers fill only capacity left by evidenced deadlines.
		// Querying up to limit rather than just the remainder lets the Go-side
		// dedupe survive a category transition between statement snapshots.
		result, err = conn.Query(
			ctx,
			`
			SELECT
				network_client_location_reliability.client_id
			FROM network_client_location_reliability
			INNER JOIN network_client ON
				network_client.client_id = network_client_location_reliability.client_id

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
				) AND
				NOT EXISTS (
					SELECT 1 FROM provider_egress_location
					WHERE
						provider_egress_location.client_id = network_client_location_reliability.client_id
				) AND
				NOT EXISTS (
					SELECT 1 FROM provider_egress_probe_attempt
					WHERE
						provider_egress_probe_attempt.client_id = network_client_location_reliability.client_id AND
						$2 <= provider_egress_probe_attempt.attempt_at
				) AND
				-- hashtext is signed and '%' preserves the sign, so normalize
				-- the modulo into [0, shardCount).
				(
					$4 <= 1 OR
					((hashtext(network_client_location_reliability.client_id::text) % $4) + $4) % $4 = $5
				)

			ORDER BY network_client_location_reliability.client_id ASC
			LIMIT $3
			`,
			ProvideModePublic,
			minAttemptAt.UTC(),
			limit,
			shardCount,
			shardIndex,
		)
		server.WithPgResult(result, err, func() {
			seen := map[server.Id]bool{}
			for _, clientId := range clientIds {
				seen[clientId] = true
			}
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				if seen[clientId] {
					continue
				}
				clientIds = append(clientIds, clientId)
				if len(clientIds) == limit {
					break
				}
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
// minAttemptAt once their provider is located or is no longer an active,
// top-level, connected, valid provider with a Public key. The latest attempt
// for an otherwise eligible no-location provider is durable scheduling
// evidence: deleting it would collapse first attempts and old retries into the
// same state and prevent any explicit least-recent-service policy.
func RemoveExpiredProviderEgressProbeAttempts(ctx context.Context, minAttemptAt time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM provider_egress_probe_attempt AS attempt
			WHERE
				attempt.attempt_at < $1 AND
				(
					EXISTS (
						SELECT 1 FROM provider_egress_location
						WHERE provider_egress_location.client_id = attempt.client_id
					) OR
					NOT EXISTS (
						SELECT 1
						FROM network_client_location_reliability
						INNER JOIN network_client USING (client_id)
						WHERE
							network_client_location_reliability.client_id = attempt.client_id AND
							network_client.active = true AND
							network_client.source_client_id IS NULL AND
							network_client_location_reliability.connected = true AND
							network_client_location_reliability.valid = true AND
							EXISTS (
								SELECT 1 FROM provide_key
								WHERE
									provide_key.client_id = attempt.client_id AND
									provide_key.provide_mode = $2
							)
					)
				)
			`,
			minAttemptAt.UTC(),
			ProvideModePublic,
		))
	})
}
