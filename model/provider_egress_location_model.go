package model

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// ProviderEgressProbeResourceName is the deployment's probe configuration. The
// probe task snapshots all of it into its durable arguments; the api reads the
// ProviderEgressRules from it at ingest, so the two sides of one rule --
// the prober's guard and the server's verdict -- come from one file.
const ProviderEgressProbeResourceName = "provider_egress_probe.yml"

// The server's half of the probe rules of connect/GEOMAP.md §11.3: when failed
// checks make a provider dark, how a failed or unmeasured check is
// rescheduled, when a batch is the prober's own fault rather than its
// providers', and how precise a GeoLite2 place must be to be stored as a city.
// Every field is a setting: the top-level keys of provider_egress_probe.yml
// override it (see ProviderEgressRulesFromResource).
//
// The yaml and json names are the configuration's and the probe task's
// argument snapshot's, so both can embed this struct and neither restates a
// key.
type ProviderEgressRules struct {
	// DarkConsecutiveFailures is how many failed checks in a row make a
	// provider dark. One failed check is a failure, not a verdict: a slow cold
	// start or a reconnect mid-check fails a single check for reasons that
	// have nothing to do with the exit (GEOMAP §11.2).
	DarkConsecutiveFailures int `yaml:"dark_consecutive_failures" json:"dark_consecutive_failures"`
	// DarkMinimumSpanSeconds is how long the run of failures must span, from
	// the first failed check to the latest, before it is a verdict. With the
	// backoff below, three failures inside the span are one bad half hour,
	// which a provider restart can produce.
	DarkMinimumSpanSeconds int `yaml:"dark_minimum_span_seconds" json:"dark_minimum_span_seconds"`
	// DarkBackoffSeconds is when the next check comes after the n-th failure
	// in a row: element min(n, len)-1. Short enough that a failing provider is
	// confirmed or cleared within the hour, long enough that the checks are
	// not one cold start measured three times.
	DarkBackoffSeconds []int `yaml:"dark_backoff_seconds" json:"dark_backoff_seconds"`
	// DarkBatchGuard is the dark share of a blackhole batch above which its
	// negative results are the prober's fault until proven otherwise: they are
	// discarded and the batch's providers are re-checked after the backoff.
	DarkBatchGuard float64 `yaml:"dark_batch_guard" json:"dark_batch_guard"`
	// DarkBatchGuardMinChecks is the fewest measured checks a batch needs for
	// DarkBatchGuard to judge it. Below it the share is a statement about a
	// handful of providers, not about the prober, and one dark provider in a
	// three-provider tail batch must not be discarded as a prober fault.
	DarkBatchGuardMinChecks int `yaml:"dark_batch_guard_min_checks" json:"dark_batch_guard_min_checks"`
	// RunBatchGuard is the failed share of a full batch's scored loads above
	// which the batch is not submitted: a CDN outage, a broken request profile
	// or a saturated prober host fails everyone at once and would stamp every
	// provider probed in that window with a bad index for days.
	RunBatchGuard float64 `yaml:"run_batch_guard" json:"run_batch_guard"`
	// RunBatchGuardMinRuns is the fewest measured runs a full batch needs for
	// RunBatchGuard to judge it, for the reason DarkBatchGuardMinChecks
	// exists: one genuinely broken exit is a whole batch of one.
	RunBatchGuardMinRuns int `yaml:"run_batch_guard_min_runs" json:"run_batch_guard_min_runs"`
	// CityConfidentRadiusKm is the largest GeoLite2 accuracy radius at which
	// the probed exit is stored as a city. Past it the exit is stored at its
	// region, or its country, so a probe can correct where a provider is
	// listed but never pin it to a city GeoLite2 itself is unsure of.
	CityConfidentRadiusKm int `yaml:"city_confident_radius_km" json:"city_confident_radius_km"`
}

// The rules of connect/GEOMAP.md §11.3.
func DefaultProviderEgressRules() ProviderEgressRules {
	return ProviderEgressRules{
		DarkConsecutiveFailures: 3,
		DarkMinimumSpanSeconds:  30 * 60,
		DarkBackoffSeconds:      []int{5 * 60, 15 * 60, 30 * 60},
		DarkBatchGuard:          0.2,
		DarkBatchGuardMinChecks: 10,
		RunBatchGuard:           0.3,
		RunBatchGuardMinRuns:    3,
		CityConfidentRadiusKm:   25,
	}
}

// Reports why the rules cannot be applied, or nil.
func (self ProviderEgressRules) Validate() error {
	var problems []string
	if self.DarkConsecutiveFailures < 1 {
		problems = append(problems, fmt.Sprintf("dark_consecutive_failures %d must be at least 1", self.DarkConsecutiveFailures))
	}
	if self.DarkMinimumSpanSeconds < 0 {
		problems = append(problems, fmt.Sprintf("dark_minimum_span_seconds %d must not be negative", self.DarkMinimumSpanSeconds))
	}
	if len(self.DarkBackoffSeconds) == 0 {
		problems = append(problems, "dark_backoff_seconds must name at least one step")
	}
	for i, seconds := range self.DarkBackoffSeconds {
		if seconds < 1 {
			problems = append(problems, fmt.Sprintf("dark_backoff_seconds[%d] %d must be positive", i, seconds))
		}
	}
	// a guard of 0 would discard every batch with one dark provider, and one
	// above 1 could never trip
	if !(0 < self.DarkBatchGuard && self.DarkBatchGuard <= 1) {
		problems = append(problems, fmt.Sprintf("dark_batch_guard %v must be in (0, 1]", self.DarkBatchGuard))
	}
	if !(0 < self.RunBatchGuard && self.RunBatchGuard <= 1) {
		problems = append(problems, fmt.Sprintf("run_batch_guard %v must be in (0, 1]", self.RunBatchGuard))
	}
	if self.DarkBatchGuardMinChecks < 1 {
		problems = append(problems, fmt.Sprintf("dark_batch_guard_min_checks %d must be at least 1", self.DarkBatchGuardMinChecks))
	}
	if self.RunBatchGuardMinRuns < 1 {
		problems = append(problems, fmt.Sprintf("run_batch_guard_min_runs %d must be at least 1", self.RunBatchGuardMinRuns))
	}
	if self.CityConfidentRadiusKm < 1 {
		problems = append(problems, fmt.Sprintf("city_confident_radius_km %d must be at least 1", self.CityConfidentRadiusKm))
	}
	if 0 < len(problems) {
		return errors.New("provider egress rules: " + strings.Join(problems, "; "))
	}
	return nil
}

// DarkMinimumSpanSeconds as a duration.
func (self ProviderEgressRules) DarkMinimumSpan() time.Duration {
	return time.Duration(self.DarkMinimumSpanSeconds) * time.Second
}

// When the check after consecutiveFailures failures in a row is
// due: the schedule's element min(consecutiveFailures, len)-1, and its first
// element for a provider with no failure (a check that was not measured, or a
// batch the guard discarded).
func (self ProviderEgressRules) DarkBackoff(consecutiveFailures int) time.Duration {
	if len(self.DarkBackoffSeconds) == 0 {
		return 0
	}
	step := min(max(consecutiveFailures, 1), len(self.DarkBackoffSeconds)) - 1
	return time.Duration(self.DarkBackoffSeconds[step]) * time.Second
}

// The defaults with the overrides of one provider_egress_probe.yml. A missing
// file is the defaults. An unreadable or invalid one is also the defaults,
// returned with the error: the api must keep judging checks through a typo in
// the probe's configuration, and the probe task, which validates the same file
// before it schedules anything, is where the typo is refused.
func ProviderEgressRulesFromResource(resource *server.SimpleResource, err error) (ProviderEgressRules, error) {
	defaults := DefaultProviderEgressRules()
	if err != nil {
		if errors.Is(err, server.ErrResourceNotFound) {
			return defaults, nil
		}
		return defaults, err
	}
	if resource == nil {
		return defaults, nil
	}
	rules := DefaultProviderEgressRules()
	if err := resource.UnmarshalYamlE(&rules); err != nil {
		return defaults, err
	}
	if err := rules.Validate(); err != nil {
		return defaults, err
	}
	return rules, nil
}

// The rules as read at loadTime, for the cached read.
type providerEgressRulesSnapshot struct {
	rules    ProviderEgressRules
	loadTime time.Time
}

// providerEgressRulesStaleAfter bounds how old the rules a request path reads
// may be: ingest and the dark set run far too often to read and parse a file
// each time, and a configuration change reaches them within this.
const providerEgressRulesStaleAfter = time.Minute

var currentProviderEgressRules atomic.Pointer[providerEgressRulesSnapshot]

// Drops the cached rules on a reset, so a test's pushed resource is read.
func init() {
	server.OnReset(func() {
		currentProviderEgressRules.Store(nil)
	})
}

// The rules as the deployment configures them, at most
// providerEgressRulesStaleAfter old. Two callers racing past the age both read
// the file, which is harmless.
func GetProviderEgressRules() ProviderEgressRules {
	snapshot := currentProviderEgressRules.Load()
	if snapshot == nil || providerEgressRulesStaleAfter <= time.Since(snapshot.loadTime) {
		rules, err := ProviderEgressRulesFromResource(
			server.Config.SimpleResource(ProviderEgressProbeResourceName),
		)
		if err != nil {
			glog.Errorf("[egress]%s is unusable (%s); the egress rules keep their defaults\n", ProviderEgressProbeResourceName, err)
		}
		snapshot = &providerEgressRulesSnapshot{
			rules:    rules,
			loadTime: time.Now(),
		}
		currentProviderEgressRules.Store(snapshot)
	}
	return snapshot.rules
}

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

// ProbeRunBatchGuardClass is the attempt failure class of a full batch the run
// guard held back (ProviderEgressRules.RunBatchGuard): the batch's providers
// are re-offered after the first dark-backoff step instead of the ordinary
// attempt backoff, since nothing about them was learned.
const ProbeRunBatchGuardClass = "run_batch_guard"

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
//
// There is no hosting, proxy or mobile verdict any more (connect/GEOMAP.md
// §11.3, D24): those came from ip-intelligence vendors, which the prober no
// longer consults. Their columns are no longer written or read, and go one
// release later.
type ProviderEgressLocation struct {
	ClientId    server.Id
	LocationId  server.Id
	CountryCode string
	ASN         int
	Org         string
	// Deprecated: Hosting, Proxy and Mobile are neither stored by
	// SetProviderEgressLocation nor read back, and are always false on a row
	// read from the table. They remain for the release that still carries the
	// columns, so callers written against them keep compiling; nothing may
	// read them.
	Hosting bool
	Proxy   bool
	Mobile  bool
	// CityConfident is whether LocationId is a city row: GeoLite2 placed the
	// exit within ProviderEgressRules.CityConfidentRadiusKm (see
	// controller.ResolveProviderEgressExit).
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
				city_confident,
				observed_at,
				verdict,
				verdict_reason,
				assurance,
				update_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (client_id) DO UPDATE
			SET
				location_id = $2,
				country_code = $3,
				asn = $4,
				org = $5,
				city_confident = $6,
				observed_at = $7,
				verdict = $8,
				verdict_reason = $9,
				assurance = $10,
				update_time = $11
			WHERE provider_egress_location.observed_at < EXCLUDED.observed_at
			`,
			e.ClientId,
			e.LocationId,
			countryCode,
			e.ASN,
			e.Org,
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

// GetLocation returns the canonical location row, or nil, with the city,
// region and country rows it is filed under: their ids, names and GeoNames ids
// (connect/GEOMAP.md §4.2), and the row's own coordinates. The ids of a
// granularity the row does not reach stay zero -- a country row has no city.
func GetLocation(ctx context.Context, locationId server.Id) *Location {
	return getLocationWhere(ctx, "location.location_id = $1", locationId)
}

// The attempt backoff over the provider_egress_probe_attempt row named alias:
// an attempt at or after minAttemptAtParam defers the provider, except a batch
// the run guard held back (ProbeRunBatchGuardClass), which defers it only
// until minGuardAttemptAtParam -- nothing about those providers was learned,
// and the batch is retried after the first backoff step as a guarded blackhole
// batch is (GEOMAP §11.3).
func providerEgressAttemptDeferredSql(alias string, minAttemptAtParam string, minGuardAttemptAtParam string) string {
	return fmt.Sprintf(
		// the parameters are cast: a CASE of two untyped parameters resolves
		// to text, which does not compare with a timestamp
		`CASE WHEN %[1]s.probe_failure = '%[4]s' THEN %[3]s::timestamp ELSE %[2]s::timestamp END <= %[1]s.attempt_at`,
		alias,
		minAttemptAtParam,
		minGuardAttemptAtParam,
		ProbeRunBatchGuardClass,
	)
}

// Where a provider is published: the country and region of its reliability
// rollup's location ids. The due lists carry it so the prober draws a
// provider's sample only from the destinations compatible with it
// (connect/GEOMAP.md §11.3), and the health ingest uses it to drop a load of a
// site incompatible with it.
type ProviderEgressPlace struct {
	// CountryCode is lowercase alpha-2; "" when the rollup has no country.
	CountryCode string
	// Region is the region location's name, as the incompatible places name
	// it; "" when the rollup has none.
	Region string
}

// Reads the published place of each provider in one query. A provider without
// a rollup row is absent, which the due list sends as a provider with no
// place: it excludes nothing.
func GetProviderEgressPlaces(ctx context.Context, clientIds []server.Id) map[server.Id]ProviderEgressPlace {
	places := map[server.Id]ProviderEgressPlace{}
	if len(clientIds) == 0 {
		return places
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_client_location_reliability.client_id,
				COALESCE(country_location.country_code, ''),
				COALESCE(region_location.location_name, '')
			FROM network_client_location_reliability
			LEFT JOIN location country_location ON
				country_location.location_id = network_client_location_reliability.country_location_id
			LEFT JOIN location region_location ON
				region_location.location_id = network_client_location_reliability.region_location_id
			WHERE network_client_location_reliability.client_id = ANY($1)
			`,
			clientIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var place ProviderEgressPlace
				server.Raise(result.Scan(&clientId, &place.CountryCode, &place.Region))
				place.CountryCode = strings.ToLower(strings.TrimSpace(place.CountryCode))
				place.Region = strings.TrimSpace(place.Region)
				places[clientId] = place
			}
		})
	})
	return places
}

// providerEgressStaleHealthDueQuery is formatted with the current-dark
// predicate (%[1]s, see ProviderBlackholeDarkSql) and the attempt-backoff
// predicate (%[2]s, see providerEgressAttemptDeferredSql) before it is run,
// so its own modulo operators are written %% to survive the formatting.
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
				%[2]s
		) AND
		NOT EXISTS (
			SELECT 1 FROM provider_blackhole_check
			WHERE
				provider_blackhole_check.client_id = provider_egress_health.client_id AND
				%[1]s
		) AND
		(
			$5 <= 1 OR
			((hashtext(provider_egress_health.client_id::text) %% $5) + $5) %% $5 = $6
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
// Fourth, a current dark verdict defers the expensive full probe: the
// consecutive failed checks of GEOMAP §11.3 (ProviderBlackholeDarkSql), not a
// single failed check, which is only a failure and may be a slow cold start.
// The cheap blackhole queue remains independent and retries failures on its
// short backoff, so a passing recheck makes the provider immediately
// full-probeable again. If that queue stalls, its verdict ages out after
// ProviderBlackholeCheckMaxAge and this queue fails open. Missing checks, stale
// checks, a run of failures short of dark, and current passing checks never
// exclude a provider.
//
// The observed-at and attempt-at cutoffs are computed by the caller in Go and
// bound as parameters. The health and current-blackhole cutoffs are likewise
// computed in Go from their model lifetimes. All four timestamps are naive
// `timestamp` values holding UTC; comparing them to SQL now() would cast
// through the session timezone.
//
// # Bounded indexed heads, not one outer-join sort
//
// A single statement over the complete live-provider population cannot use the
// evidence timestamps' ordered indexes for a global deadline sort. Instead,
// the location and health tables supply bounded oldest-evidence heads through
// (timestamp, client_id). A third output-bounded anti-health head finds
// providers whose accepted location has no health row, which is possible
// because health reporting is non-fatal to location submission. Absence in a
// different table has no observed_at range key; PostgreSQL may correctly scan
// and sort the small location table when missing-health rows are rare. The
// health head requires an extant location, so retained health history after
// location cleanup remains exclusively in the unlocated lane. Go deduplicates
// those heads and merges them by absolute hard expiry: observed_at +
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
	clientIds, _ := GetProviderEgressLocationDueShardedWithDiagnostics(ctx, minObservedAt, minAttemptAt, limit, shardIndex, shardCount)
	return clientIds
}

// Returns the unchanged due selection and identity-free counts of the rows
// actually selected. Expiry uses the selection clock, not a later scrape clock.
func GetProviderEgressLocationDueShardedWithDiagnostics(
	ctx context.Context,
	minObservedAt time.Time,
	minAttemptAt time.Time,
	limit int,
	shardIndex int,
	shardCount int,
) ([]server.Id, ProviderEgressDueDiagnostics) {
	now := server.NowUtc()
	rules := GetProviderEgressRules()
	minBlackholeCheckedAt := now.Add(-ProviderBlackholeCheckMaxAge)
	// a batch the run guard held back measured nothing about its providers,
	// so they come round again after the first backoff step rather than the
	// ordinary attempt backoff
	minGuardAttemptAt := now.Add(-rules.DarkBackoff(0))
	clientIds := []server.Id{}
	diagnostics := ProviderEgressDueDiagnostics{}
	server.Db(ctx, func(conn server.PgConn) {
		type dueCandidate struct {
			clientId server.Id
			deadline time.Time
			lane     ProviderEgressDueLane
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
						`+providerEgressAttemptDeferredSql("provider_egress_probe_attempt", "$3", "$8")+`
				) AND
				NOT EXISTS (
					SELECT 1 FROM provider_blackhole_check
					WHERE
						provider_blackhole_check.client_id = provider_egress_location.client_id AND
						`+ProviderBlackholeDarkSql("provider_blackhole_check", "$7", rules)+`
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
			minBlackholeCheckedAt.UTC(),
			minGuardAttemptAt.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var observedAt time.Time
				server.Raise(result.Scan(&clientId, &observedAt))
				addUrgent(dueCandidate{
					clientId: clientId,
					deadline: observedAt.Add(ProviderEgressLocationMaxAge),
					lane:     ProviderEgressDueStaleLocation,
				})
			}
		})

		// Stale health head. It requires a current location so a retained health
		// row whose location was removed remains exclusively in the no-location
		// lane. Located providers may overlap the stale-location head; a provider
		// with both deadlines keeps the earlier one.
		minMeasuredAt := now.Add(-ProviderEgressHealthMaxAge / 2)
		result, err = conn.Query(
			ctx,
			fmt.Sprintf(
				providerEgressStaleHealthDueQuery,
				ProviderBlackholeDarkSql("provider_blackhole_check", "$7", rules),
				providerEgressAttemptDeferredSql("provider_egress_probe_attempt", "$3", "$8"),
			),
			ProvideModePublic,
			minMeasuredAt.UTC(),
			minAttemptAt.UTC(),
			headLimit,
			shardCount,
			shardIndex,
			minBlackholeCheckedAt.UTC(),
			minGuardAttemptAt.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var measuredAt time.Time
				server.Raise(result.Scan(&clientId, &measuredAt))
				addUrgent(dueCandidate{
					clientId: clientId,
					deadline: measuredAt.Add(ProviderEgressHealthMaxAge),
					lane:     ProviderEgressDueStaleHealth,
				})
			}
		})

		// Missing-health head. Location submission remains valid when the
		// prober's independent health check is skipped or its non-fatal report
		// fails, while provider publication fails closed without a health row.
		// Use the location observation as the start of the existing health
		// lifetime. Unlike the stale-location head, this has no observed_at range:
		// missing health is absence in another table. The existing ordered index is
		// available, but PostgreSQL may correctly choose a small-table sequential
		// scan, anti-join, and bounded output sort when missing rows are rare. The
		// LIMIT bounds output, not necessarily scanned location rows. Revisit the
		// storage/query shape only if measured table growth or latency makes that
		// pre-existing plan material; do not force an index by changing due
		// semantics. The common attempt predicate prevents an immediate retry after
		// the normal path reports its just-completed full-probe attempt.
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
						`+providerEgressAttemptDeferredSql("provider_egress_probe_attempt", "$2", "$7")+`
				) AND
				NOT EXISTS (
					SELECT 1 FROM provider_blackhole_check
					WHERE
						provider_blackhole_check.client_id = provider_egress_location.client_id AND
						`+ProviderBlackholeDarkSql("provider_blackhole_check", "$6", rules)+`
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
			minBlackholeCheckedAt.UTC(),
			minGuardAttemptAt.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var observedAt time.Time
				server.Raise(result.Scan(&clientId, &observedAt))
				addUrgent(dueCandidate{
					clientId: clientId,
					deadline: observedAt.Add(ProviderEgressHealthMaxAge),
					lane:     ProviderEgressDueMissingHealth,
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
			diagnostics.record(candidate.lane, candidate.deadline, now)
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
						`+providerEgressAttemptDeferredSql("provider_egress_probe_attempt", "$2", "$7")+`
				) AND
				NOT EXISTS (
					SELECT 1 FROM provider_blackhole_check
					WHERE
						provider_blackhole_check.client_id = network_client_location_reliability.client_id AND
						`+ProviderBlackholeDarkSql("provider_blackhole_check", "$6", rules)+`
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
			minBlackholeCheckedAt.UTC(),
			minGuardAttemptAt.UTC(),
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
				diagnostics.record(ProviderEgressDueNoLocation, time.Time{}, now)
				if len(clientIds) == limit {
					break
				}
			}
		})
	})
	return clientIds, diagnostics
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
