package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// The egress destination pool (connect/GEOMAP.md §11.4) is data, not code: the
// sites the prober loads, per class, with their load contracts, and the
// candidates a daily refresh promotes when an active site stops telling one
// exit from another. The prober's compiled-in table is the seed the pool starts
// from and its fallback; the operator extends the candidates in
// egress-sites.yml. This file is the storage: the rows, the per-site load tally
// the refresh judges sites by, and the reads the pool route, the refresh and
// the §2.19b monitor share. What the rows mean for a provider's counts is
// decided at ingest (see ProviderEgressHealthScoring).

// Destination sources: where a row came from.
const (
	// ProviderEgressDestinationSourceBuiltin is a row seeded from the prober's
	// built-in table.
	ProviderEgressDestinationSourceBuiltin = "builtin"
	// ProviderEgressDestinationSourceCandidates is a row synced from
	// egress-sites.yml.
	ProviderEgressDestinationSourceCandidates = "candidates"
)

// One place a destination is known not to work from: a lower-case country,
// with or without a region (the whole country when without).
type ProviderEgressDestinationPlace struct {
	Country string `json:"country"`
	Region  string `json:"region,omitempty"`
	// MarkedAt is when the refresh learned the place from the exits there.
	// Nil for a place the operator or the built-in table declared, which only
	// they remove: the refresh unmarks only what it marked.
	MarkedAt *time.Time `json:"marked_at,omitempty"`
	// CanaryPassingSince is when the place's canaries began passing again; the
	// refresh unmarks a learned place once they have passed for
	// ProviderEgressSiteSettings.SiteRegionCooldown, and clears this when one
	// fails.
	CanaryPassingSince *time.Time `json:"canary_passing_since,omitempty"`
}

// Whether this place applies to an exit published at place: the same
// country, and either no region or the same region, compared as the prober
// compares them (ignoring case and surrounding space). An exit with no country
// is covered by nothing.
func (self ProviderEgressDestinationPlace) Covers(place ProviderEgressPlace) bool {
	country := strings.TrimSpace(self.Country)
	if country == "" || !strings.EqualFold(country, strings.TrimSpace(place.CountryCode)) {
		return false
	}
	region := strings.TrimSpace(self.Region)
	return region == "" || strings.EqualFold(region, strings.TrimSpace(place.Region))
}

// A destination's body check in the prober's wire shape
// (egresshealth.BodyCheck). The zero value is no check.
type ProviderEgressDestinationVerify struct {
	Kind string `json:"kind,omitempty"`
	Text string `json:"text,omitempty"`
}

// One pool row.
type ProviderEgressDestination struct {
	Name  string
	Class string
	Url   string
	// Expect is the prober's wire word: body, status or reachable.
	Expect   string
	Status   int
	MaxBytes int
	Headers  map[string]string
	Verify   ProviderEgressDestinationVerify
	// Category and Region say what the site is representative of (search,
	// news, ... ; global, europe, ...). The refresh promotes a candidate of
	// the category a retired site leaves.
	Category     string
	Region       string
	Incompatible []ProviderEgressDestinationPlace
	Source       string
	// Revision is the candidate entry's revision in egress-sites.yml. A higher
	// one refreshes the row's contract and clears its retirement history,
	// which is how the operator re-adds a site retired for good.
	Revision int
	// Active and Probation are the pool state: active and scored, active on
	// probation (served and recorded, never scored), or a candidate.
	Active       bool
	Probation    bool
	AddedTime    time.Time
	PromotedTime *time.Time
	RetiredTime  *time.Time
	RetireReason string
	RetireCount  int
	// FailureShare and SampleCount are the refresh's last judgement of the
	// site, at JudgedTime; AboveRetireSince is when the share first stood above
	// the retire line with enough samples, nil when it does not.
	FailureShare     *float64
	SampleCount      int
	JudgedTime       *time.Time
	AboveRetireSince *time.Time
	UpdateTime       time.Time
}

// Whether the destination is known not to work from place.
func (self *ProviderEgressDestination) IncompatibleWith(place ProviderEgressPlace) bool {
	for _, incompatible := range self.Incompatible {
		if incompatible.Covers(place) {
			return true
		}
	}
	return false
}

// Whether the destination's loads count in a provider's health run:
// active and past probation. A site on probation is served and recorded, but
// it has to earn the right to cost a provider a tier (GEOMAP §11.4).
func (self *ProviderEgressDestination) Scored() bool {
	return self.Active && !self.Probation
}

// Whether the destination may be promoted at now: not active,
// out of any retirement cooldown, and retired fewer than maxRetirements times.
func (self *ProviderEgressDestination) IsCandidate(now time.Time, retireCooldown time.Duration, maxRetirements int) bool {
	if self.Active || maxRetirements <= self.RetireCount {
		return false
	}
	return self.RetiredTime == nil || !now.Before(self.RetiredTime.Add(retireCooldown))
}

const providerEgressDestinationColumns = `
	name,
	class,
	url,
	expect,
	status,
	max_bytes,
	headers,
	verify,
	category,
	region,
	incompatible,
	source,
	revision,
	active,
	probation,
	added_time,
	promoted_time,
	retired_time,
	retire_reason,
	retire_count,
	failure_share,
	sample_count,
	judged_time,
	above_retire_since,
	update_time
`

// Reads every row, active and candidate, in name order. The pool is a few
// hundred rows and every reader wants all of it.
func GetProviderEgressDestinations(ctx context.Context) []*ProviderEgressDestination {
	destinations := []*ProviderEgressDestination{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+providerEgressDestinationColumns+` FROM provider_egress_destination ORDER BY name`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				destinations = append(destinations, scanProviderEgressDestination(result))
			}
		})
	})
	return destinations
}

// Reads one row in providerEgressDestinationColumns order.
func scanProviderEgressDestination(result server.PgResult) *ProviderEgressDestination {
	d := &ProviderEgressDestination{}
	var headersJson, verifyJson, incompatibleJson []byte
	var failureShare *float32
	server.Raise(result.Scan(
		&d.Name,
		&d.Class,
		&d.Url,
		&d.Expect,
		&d.Status,
		&d.MaxBytes,
		&headersJson,
		&verifyJson,
		&d.Category,
		&d.Region,
		&incompatibleJson,
		&d.Source,
		&d.Revision,
		&d.Active,
		&d.Probation,
		&d.AddedTime,
		&d.PromotedTime,
		&d.RetiredTime,
		&d.RetireReason,
		&d.RetireCount,
		&failureShare,
		&d.SampleCount,
		&d.JudgedTime,
		&d.AboveRetireSince,
		&d.UpdateTime,
	))
	server.Raise(json.Unmarshal(headersJson, &d.Headers))
	server.Raise(json.Unmarshal(verifyJson, &d.Verify))
	server.Raise(json.Unmarshal(incompatibleJson, &d.Incompatible))
	if len(d.Headers) == 0 {
		d.Headers = nil
	}
	if failureShare != nil {
		share := float64(*failureShare)
		d.FailureShare = &share
	}
	return d
}

// Writes one row as given, inserting it or replacing every column. The refresh
// is the only writer of pool state, and it runs once at a time (a RunOnce
// task), so a whole-row write is the simplest correct one.
func SetProviderEgressDestination(ctx context.Context, d *ProviderEgressDestination) {
	server.Tx(ctx, func(tx server.PgTx) {
		setProviderEgressDestination(ctx, tx, d, false)
	})
}

// Writes a row. onlyIfAbsent inserts and never replaces, for the seed and the
// candidate sync.
func setProviderEgressDestination(ctx context.Context, tx server.PgTx, d *ProviderEgressDestination, onlyIfAbsent bool) {
	headers := d.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	headersJson, err := json.Marshal(headers)
	server.Raise(err)
	verifyJson, err := json.Marshal(d.Verify)
	server.Raise(err)
	incompatible := d.Incompatible
	if incompatible == nil {
		incompatible = []ProviderEgressDestinationPlace{}
	}
	incompatibleJson, err := json.Marshal(incompatible)
	server.Raise(err)
	var failureShare *float32
	if d.FailureShare != nil {
		share := float32(*d.FailureShare)
		failureShare = &share
	}
	utc := func(t *time.Time) *time.Time {
		if t == nil {
			return nil
		}
		u := t.UTC()
		return &u
	}
	addedTime := d.AddedTime
	if addedTime.IsZero() {
		addedTime = server.NowUtc()
	}
	conflict := `
		ON CONFLICT (name) DO UPDATE SET
			class = EXCLUDED.class,
			url = EXCLUDED.url,
			expect = EXCLUDED.expect,
			status = EXCLUDED.status,
			max_bytes = EXCLUDED.max_bytes,
			headers = EXCLUDED.headers,
			verify = EXCLUDED.verify,
			category = EXCLUDED.category,
			region = EXCLUDED.region,
			incompatible = EXCLUDED.incompatible,
			source = EXCLUDED.source,
			revision = EXCLUDED.revision,
			active = EXCLUDED.active,
			probation = EXCLUDED.probation,
			added_time = EXCLUDED.added_time,
			promoted_time = EXCLUDED.promoted_time,
			retired_time = EXCLUDED.retired_time,
			retire_reason = EXCLUDED.retire_reason,
			retire_count = EXCLUDED.retire_count,
			failure_share = EXCLUDED.failure_share,
			sample_count = EXCLUDED.sample_count,
			judged_time = EXCLUDED.judged_time,
			above_retire_since = EXCLUDED.above_retire_since,
			update_time = EXCLUDED.update_time
	`
	if onlyIfAbsent {
		conflict = `ON CONFLICT (name) DO NOTHING`
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO provider_egress_destination (`+providerEgressDestinationColumns+`)
		VALUES (
			$1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9, $10, $11::jsonb, $12, $13,
			$14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25
		)
		`+conflict,
		d.Name,
		d.Class,
		d.Url,
		d.Expect,
		d.Status,
		d.MaxBytes,
		string(headersJson),
		string(verifyJson),
		d.Category,
		d.Region,
		string(incompatibleJson),
		d.Source,
		d.Revision,
		d.Active,
		d.Probation,
		addedTime.UTC(),
		utc(d.PromotedTime),
		utc(d.RetiredTime),
		d.RetireReason,
		d.RetireCount,
		failureShare,
		d.SampleCount,
		utc(d.JudgedTime),
		utc(d.AboveRetireSince),
		server.NowUtc(),
	))
}

// Fills an empty pool with seed, every row active and scored, and reports
// whether it did. The seed is the prober's own built-in table, which is what
// the prober measures when there is no pool, so seeding changes nothing about
// any provider's counts the day it happens. A pool that has any row is never
// reseeded: from then on the refresh owns it.
func SeedProviderEgressDestinations(ctx context.Context, seed []*ProviderEgressDestination) bool {
	seeded := false
	server.Tx(ctx, func(tx server.PgTx) {
		// two first uses racing both see an empty table; the advisory lock
		// makes the second see the first's rows, and a key conflict could not
		// corrupt anything anyway
		server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('provider_egress_destination_seed'))`))
		var count int
		server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM provider_egress_destination`).Scan(&count))
		if 0 < count {
			return
		}
		now := server.NowUtc()
		for _, d := range seed {
			row := *d
			row.Active = true
			row.Probation = false
			row.AddedTime = now
			if row.Source == "" {
				row.Source = ProviderEgressDestinationSourceBuiltin
			}
			setProviderEgressDestination(ctx, tx, &row, true)
		}
		seeded = true
	})
	return seeded
}

// Brings egress-sites.yml into the pool and returns the names it changed: a
// new name becomes a candidate, and a file entry whose revision is higher than
// its row's refreshes the row's load contract and clears its retirement
// history -- how the operator re-adds a site retired for good, or corrects
// one. Pool state is otherwise the refresh's: a candidate listed again at the
// same revision changes nothing, and an active row keeps being active whatever
// the file says.
func SyncProviderEgressDestinationCandidates(ctx context.Context, candidates []*ProviderEgressDestination) []string {
	changed := []string{}
	server.Tx(ctx, func(tx server.PgTx) {
		existing := map[string]*ProviderEgressDestination{}
		result, err := tx.Query(
			ctx,
			`SELECT `+providerEgressDestinationColumns+` FROM provider_egress_destination FOR UPDATE`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				d := scanProviderEgressDestination(result)
				existing[d.Name] = d
			}
		})
		// a row's places after its file entry changed: the entry's declared
		// places, plus the places the refresh learned, which are the
		// refresh's to keep or unmark
		mergeDeclaredPlaces := func(current []ProviderEgressDestinationPlace, declared []ProviderEgressDestinationPlace) []ProviderEgressDestinationPlace {
			merged := []ProviderEgressDestinationPlace{}
			seen := map[string]bool{}
			key := func(place ProviderEgressDestinationPlace) string {
				return strings.ToLower(strings.TrimSpace(place.Country)) + "/" + strings.ToLower(strings.TrimSpace(place.Region))
			}
			for _, place := range declared {
				place.MarkedAt = nil
				place.CanaryPassingSince = nil
				if !seen[key(place)] {
					seen[key(place)] = true
					merged = append(merged, place)
				}
			}
			for _, place := range current {
				if place.MarkedAt != nil && !seen[key(place)] {
					seen[key(place)] = true
					merged = append(merged, place)
				}
			}
			return merged
		}
		now := server.NowUtc()
		for _, candidate := range candidates {
			row, ok := existing[candidate.Name]
			if !ok {
				add := *candidate
				add.Source = ProviderEgressDestinationSourceCandidates
				add.Active = false
				add.Probation = false
				add.AddedTime = now
				setProviderEgressDestination(ctx, tx, &add, true)
				changed = append(changed, candidate.Name)
				continue
			}
			if candidate.Revision <= row.Revision {
				continue
			}
			update := *row
			update.Class = candidate.Class
			update.Url = candidate.Url
			update.Expect = candidate.Expect
			update.Status = candidate.Status
			update.MaxBytes = candidate.MaxBytes
			update.Headers = candidate.Headers
			update.Verify = candidate.Verify
			update.Category = candidate.Category
			update.Region = candidate.Region
			update.Incompatible = mergeDeclaredPlaces(row.Incompatible, candidate.Incompatible)
			update.Source = ProviderEgressDestinationSourceCandidates
			update.Revision = candidate.Revision
			update.RetireCount = 0
			update.RetireReason = ""
			if !update.Active {
				update.RetiredTime = nil
			}
			setProviderEgressDestination(ctx, tx, &update, false)
			changed = append(changed, candidate.Name)
		}
	})
	sort.Strings(changed)
	return changed
}

// What a health run's counts may include, from the pool as it stands: every
// site on probation, and every site marked incompatible with some place.
// Ingest drops a load of either from the counts (GEOMAP §11.3, §11.4): a new
// site has to earn the right to cost a provider a tier, and a site blocked in
// a country says nothing about that country's exits.
type ProviderEgressHealthScoring struct {
	destinations map[string]*ProviderEgressDestination
}

// Reads the pool for ingest.
func GetProviderEgressHealthScoring(ctx context.Context) *ProviderEgressHealthScoring {
	scoring := &ProviderEgressHealthScoring{destinations: map[string]*ProviderEgressDestination{}}
	for _, d := range GetProviderEgressDestinations(ctx) {
		scoring.destinations[d.Name] = d
	}
	return scoring
}

// The scoring of a pool the caller already holds.
func NewProviderEgressHealthScoring(destinations []*ProviderEgressDestination) *ProviderEgressHealthScoring {
	scoring := &ProviderEgressHealthScoring{destinations: map[string]*ProviderEgressDestination{}}
	for _, d := range destinations {
		scoring.destinations[d.Name] = d
	}
	return scoring
}

// Whether a load of name, by an exit published at place, counts. A
// name the pool does not hold counts: it is a built-in destination the prober
// fell back to, and the pool not knowing it is no reason to take it out.
func (self *ProviderEgressHealthScoring) Scores(name string, place ProviderEgressPlace) bool {
	if self == nil {
		return true
	}
	d, ok := self.destinations[name]
	if !ok {
		return true
	}
	return !d.Probation && !d.IncompatibleWith(place)
}

// The pool's class for name, "" when it does not hold it.
func (self *ProviderEgressHealthScoring) Class(name string) string {
	if self == nil {
		return ""
	}
	if d, ok := self.destinations[name]; ok {
		return d.Class
	}
	return ""
}

// One load of one run, as the site tally counts it.
type ProviderEgressSiteLoad struct {
	Name string
	Ok   bool
	// Healthy is whether the exit passed nine in ten of the run's other scored
	// sites: the exits the refresh judges a site by.
	Healthy bool
	// Canary is a load from a place the site is marked incompatible with,
	// counted only toward unmarking the place.
	Canary bool
}

// One run's per-place outcome.
type ProviderEgressRunTally struct {
	Place ProviderEgressPlace
	// Healthy is whether the run passed nine in ten of its scored sites.
	Healthy bool
	// EchoFailed is a warm-up the operator's /ip echo never answered.
	EchoFailed bool
}

// Adds one submitted run to the site and place tallies of its UTC day, in one
// transaction, so a run is counted whole or not at all.
func AddProviderEgressRunTally(ctx context.Context, measuredAt time.Time, run ProviderEgressRunTally, loads []ProviderEgressSiteLoad) {
	day := measuredAt.UTC().Truncate(24 * time.Hour)
	countryCode := strings.ToLower(strings.TrimSpace(run.Place.CountryCode))
	region := strings.TrimSpace(run.Place.Region)
	if 128 < len(region) {
		// the column's width; a place name that long is not one a mark could
		// match anyway
		region = region[:128]
	}
	boolInt := func(value bool) int {
		if value {
			return 1
		}
		return 0
	}
	server.Tx(ctx, func(tx server.PgTx) {
		now := server.NowUtc()
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO provider_egress_place_tally (
				tally_day, country_code, region, run_count, healthy_run_count, echo_failure_count, update_time
			)
			VALUES ($1, $2, $3, 1, $4, $5, $6)
			ON CONFLICT (tally_day, country_code, region) DO UPDATE SET
				run_count = provider_egress_place_tally.run_count + 1,
				healthy_run_count = provider_egress_place_tally.healthy_run_count + EXCLUDED.healthy_run_count,
				echo_failure_count = provider_egress_place_tally.echo_failure_count + EXCLUDED.echo_failure_count,
				update_time = EXCLUDED.update_time
			`,
			day,
			countryCode,
			region,
			boolInt(run.Healthy),
			boolInt(run.EchoFailed),
			now,
		))
		// one row per site: a run loads a site at most once, but a batch of
		// loads is summed defensively rather than trusted to be unique
		type counts struct {
			loads, failures, healthyLoads, healthyFailures, canaryLoads, canaryPasses int
		}
		bySite := map[string]*counts{}
		names := []string{}
		for _, load := range loads {
			name := strings.TrimSpace(load.Name)
			if name == "" || 128 < len(name) {
				continue
			}
			c, ok := bySite[name]
			if !ok {
				c = &counts{}
				bySite[name] = c
				names = append(names, name)
			}
			if load.Canary {
				c.canaryLoads += 1
				c.canaryPasses += boolInt(load.Ok)
				continue
			}
			c.loads += 1
			c.failures += boolInt(!load.Ok)
			if load.Healthy {
				c.healthyLoads += 1
				c.healthyFailures += boolInt(!load.Ok)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			c := bySite[name]
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO provider_egress_site_tally (
					tally_day, name, country_code, region,
					load_count, failure_count, healthy_load_count, healthy_failure_count,
					canary_load_count, canary_pass_count, update_time
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
				ON CONFLICT (tally_day, name, country_code, region) DO UPDATE SET
					load_count = provider_egress_site_tally.load_count + EXCLUDED.load_count,
					failure_count = provider_egress_site_tally.failure_count + EXCLUDED.failure_count,
					healthy_load_count = provider_egress_site_tally.healthy_load_count + EXCLUDED.healthy_load_count,
					healthy_failure_count = provider_egress_site_tally.healthy_failure_count + EXCLUDED.healthy_failure_count,
					canary_load_count = provider_egress_site_tally.canary_load_count + EXCLUDED.canary_load_count,
					canary_pass_count = provider_egress_site_tally.canary_pass_count + EXCLUDED.canary_pass_count,
					update_time = EXCLUDED.update_time
				`,
				day,
				name,
				countryCode,
				region,
				c.loads,
				c.failures,
				c.healthyLoads,
				c.healthyFailures,
				c.canaryLoads,
				c.canaryPasses,
				now,
			))
		}
	})
}

// One site's summed loads at one place.
type ProviderEgressSiteTally struct {
	Name                string
	Place               ProviderEgressPlace
	LoadCount           int
	FailureCount        int
	HealthyLoadCount    int
	HealthyFailureCount int
	CanaryLoadCount     int
	CanaryPassCount     int
}

// Sums the site tally over the days at or after minDay, per site and place.
func GetProviderEgressSiteTallies(ctx context.Context, minDay time.Time) []ProviderEgressSiteTally {
	tallies := []ProviderEgressSiteTally{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				name,
				country_code,
				region,
				sum(load_count)::bigint,
				sum(failure_count)::bigint,
				sum(healthy_load_count)::bigint,
				sum(healthy_failure_count)::bigint,
				sum(canary_load_count)::bigint,
				sum(canary_pass_count)::bigint
			FROM provider_egress_site_tally
			WHERE $1 <= tally_day
			GROUP BY name, country_code, region
			ORDER BY name, country_code, region
			`,
			minDay.UTC().Truncate(24*time.Hour),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var t ProviderEgressSiteTally
				var loads, failures, healthyLoads, healthyFailures, canaryLoads, canaryPasses int64
				server.Raise(result.Scan(
					&t.Name,
					&t.Place.CountryCode,
					&t.Place.Region,
					&loads,
					&failures,
					&healthyLoads,
					&healthyFailures,
					&canaryLoads,
					&canaryPasses,
				))
				t.LoadCount = int(loads)
				t.FailureCount = int(failures)
				t.HealthyLoadCount = int(healthyLoads)
				t.HealthyFailureCount = int(healthyFailures)
				t.CanaryLoadCount = int(canaryLoads)
				t.CanaryPassCount = int(canaryPasses)
				tallies = append(tallies, t)
			}
		})
	})
	return tallies
}

// One place's summed runs.
type ProviderEgressPlaceTally struct {
	Place            ProviderEgressPlace
	RunCount         int
	HealthyRunCount  int
	EchoFailureCount int
}

// Sums the place tally over the days at or after minDay.
func GetProviderEgressPlaceTallies(ctx context.Context, minDay time.Time) []ProviderEgressPlaceTally {
	tallies := []ProviderEgressPlaceTally{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				country_code,
				region,
				sum(run_count)::bigint,
				sum(healthy_run_count)::bigint,
				sum(echo_failure_count)::bigint
			FROM provider_egress_place_tally
			WHERE $1 <= tally_day
			GROUP BY country_code, region
			ORDER BY country_code, region
			`,
			minDay.UTC().Truncate(24*time.Hour),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var t ProviderEgressPlaceTally
				var runs, healthyRuns, echoFailures int64
				server.Raise(result.Scan(&t.Place.CountryCode, &t.Place.Region, &runs, &healthyRuns, &echoFailures))
				t.RunCount = int(runs)
				t.HealthyRunCount = int(healthyRuns)
				t.EchoFailureCount = int(echoFailures)
				tallies = append(tallies, t)
			}
		})
	})
	return tallies
}

// The first tally day a window of length window ending at now covers. Days are
// whole UTC days, so a window reads between window and window plus one day of
// runs; the extra part-day is today's, which is the freshest evidence there
// is.
func ProviderEgressSiteTallyWindowStart(now time.Time, window time.Duration) time.Time {
	return now.UTC().Add(-window).Truncate(24 * time.Hour)
}

// Drops tally days before minDay.
func RemoveExpiredProviderEgressTallies(ctx context.Context, minDay time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		day := minDay.UTC().Truncate(24 * time.Hour)
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_site_tally WHERE tally_day < $1`, day))
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_place_tally WHERE tally_day < $1`, day))
	})
}

// Ok/total of one class over a set of runs.
type ProviderEgressClassTotal struct {
	Ok    int
	Total int
}

// Sums the scored class tallies of the health runs measured at or after
// minMeasuredAt, per class and over all (""). It is the fleet-wide failure
// share the refresh and the gauges read: a prober fault fails everyone at
// once, and this is where that shows first.
func GetProviderEgressHealthClassTotals(ctx context.Context, minMeasuredAt time.Time) map[string]ProviderEgressClassTotal {
	totals := map[string]ProviderEgressClassTotal{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				class.key,
				sum(COALESCE((class.value ->> 'ok')::bigint, 0))::bigint,
				sum(COALESCE((class.value ->> 'total')::bigint, 0))::bigint
			FROM provider_egress_health
			CROSS JOIN LATERAL jsonb_each(provider_egress_health.class_results) AS class
			WHERE $1 <= provider_egress_health.measured_at
			GROUP BY class.key
			`,
			minMeasuredAt.UTC(),
		)
		server.WithPgResult(result, err, func() {
			all := ProviderEgressClassTotal{}
			for result.Next() {
				var class string
				var ok, total int64
				server.Raise(result.Scan(&class, &ok, &total))
				totals[class] = ProviderEgressClassTotal{Ok: int(ok), Total: int(total)}
				all.Ok += int(ok)
				all.Total += int(total)
			}
			totals[""] = all
		})
	})
	return totals
}

// Reports why name cannot key a row.
func ValidateProviderEgressDestinationName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("no name")
	}
	if name != strings.TrimSpace(name) || strings.ContainsAny(name, ", ") {
		// failed_names is comma-joined, so a name with a comma or a space
		// could never be matched back to its row
		return fmt.Errorf("name %q must not carry commas or spaces", name)
	}
	if 128 < len(name) {
		return fmt.Errorf("name %q is longer than 128 bytes", name)
	}
	return nil
}

// ProviderEgressSitesResourceName is the pool's curation: the refresh settings,
// the request profile the prober loads sites with, and the candidate list
// (connect/GEOMAP.md §11.4). It lives in config/all, beside nothing
// environment-specific, since what makes a site representative does not
// depend on the deployment.
const ProviderEgressSitesResourceName = "egress-sites.yml"

// The pool refresh's rules (connect/GEOMAP.md §11.4). Every field is a
// setting: the `settings` block of egress-sites.yml overrides it. Durations
// are whole seconds in the file, like the probe's.
type ProviderEgressSiteSettings struct {
	// SiteRefreshIntervalSeconds is the refresh cadence.
	SiteRefreshIntervalSeconds int `yaml:"site_refresh_interval_seconds"`
	// SiteRefreshMaxTimeSeconds is how long one refresh may run before the
	// task system ends it. It has to cover one host check of a candidate at
	// the probe's load rules -- a refresh checks its candidates at once, each
	// with the prober's retries, about 31 minutes at the default rules --
	// beside reads and writes that take seconds; a refresh refuses to start
	// on a bound that does not.
	SiteRefreshMaxTimeSeconds int `yaml:"site_refresh_max_time_seconds"`
	// SiteWindowSeconds is how much of the site tally a refresh judges.
	SiteWindowSeconds int `yaml:"site_window_seconds"`
	// SiteHealthyExitShare is what makes an exit healthy for judging a site:
	// it passed at least this share of the other scored sites of the same run.
	SiteHealthyExitShare float64 `yaml:"site_healthy_exit_share"`
	// SiteRetireShare is the failure share among healthy exits above which an
	// active site is retired: it fails exits that work, so it cannot tell one
	// exit from another.
	SiteRetireShare float64 `yaml:"site_retire_share"`
	// SiteMinSamples is the fewest loads a judgement stands on, for retiring a
	// site and for passing its probation.
	SiteMinSamples int `yaml:"site_min_samples"`
	// SiteProbationShare is the pass share, over SiteMinSamples healthy exits,
	// a promoted site needs before its loads count.
	SiteProbationShare float64 `yaml:"site_probation_share"`
	// SitePoolSize is the active pool size to keep per class. The defaults are
	// the built-in table's class sizes, so the seed is a full pool.
	SitePoolSize map[string]int `yaml:"site_pool_size"`
	// SiteSampleSize is how many sites of each class one run draws (the
	// prober's sample sizes, egresshealth.SamplePerRun): a place with fewer
	// compatible sites than this in a class cannot be sampled
	// representatively there.
	SiteSampleSize map[string]int `yaml:"site_sample_size"`
	// SiteMaxRetirePerRun bounds retirements per class per refresh, so a fault
	// no guard caught cannot empty a class in a day.
	SiteMaxRetirePerRun int `yaml:"site_max_retire_per_run"`
	// SiteRetireCooldownSeconds is how long a retired site waits before it is
	// a candidate again.
	SiteRetireCooldownSeconds int `yaml:"site_retire_cooldown_seconds"`
	// SiteMaxRetirements is how many retirements drop a site for good, until
	// the operator re-adds it with a new revision.
	SiteMaxRetirements int `yaml:"site_max_retirements"`
	// SiteRegionFailShare is the failure share among a place's healthy exits
	// at which a site that works elsewhere is marked incompatible there.
	SiteRegionFailShare float64 `yaml:"site_region_fail_share"`
	// SiteRegionMinSamples is the fewest loads at a place a mark stands on.
	SiteRegionMinSamples int `yaml:"site_region_min_samples"`
	// SiteRegionCanaryShare is the share of pool fetches that ask for a marked
	// site to be loaded, unscored, from the places it is marked for anyway.
	SiteRegionCanaryShare float64 `yaml:"site_region_canary_share"`
	// SiteRegionCooldownSeconds is how long a learned mark's canaries must
	// pass before the place is unmarked.
	SiteRegionCooldownSeconds int `yaml:"site_region_cooldown_seconds"`
	// SiteProberFaultShare is the prober-fault line: while the fleet-wide
	// failure share of scored loads is above it, the refresh does nothing,
	// since retiring sites during a prober fault empties the pool for nothing.
	SiteProberFaultShare float64 `yaml:"site_prober_fault_share"`
	// SiteProberFaultWindowSeconds is what "while" means for that line: the
	// health runs measured this recently. A prober fault fails every run at
	// once, so the newest runs show it first; the window's own three days
	// would dilute it for days.
	SiteProberFaultWindowSeconds int `yaml:"site_prober_fault_window_seconds"`
	// SiteTallyRetentionSeconds is how long the site tally is kept: longer
	// than the window, so a site on probation is judged on every load since
	// it was promoted.
	SiteTallyRetentionSeconds int `yaml:"site_tally_retention_seconds"`
	// SiteCandidateChecksPerPromotion is how many candidates, in promotion
	// order, the taskworker host loads for one opening before it gives up on
	// the opening until the next refresh. Each is loaded with the prober's
	// retries, so a candidate that keeps failing holds a check for its whole
	// retry schedule; they are loaded at once, and the first in order that
	// loaded cleanly is promoted.
	SiteCandidateChecksPerPromotion int `yaml:"site_candidate_checks_per_promotion"`
}

// The rules of connect/GEOMAP.md §11.4.
func DefaultProviderEgressSiteSettings() *ProviderEgressSiteSettings {
	return &ProviderEgressSiteSettings{
		SiteRefreshIntervalSeconds: 24 * 60 * 60,
		SiteRefreshMaxTimeSeconds:  90 * 60,
		SiteWindowSeconds:          3 * 24 * 60 * 60,
		SiteHealthyExitShare:       0.9,
		SiteRetireShare:            0.5,
		SiteMinSamples:             200,
		SiteProbationShare:         0.5,
		// the built-in table's classes: 7 DoH operators, 14 portal and echo
		// endpoints, 18 cdn edges and mirrors, 100 sites
		SitePoolSize: map[string]int{
			"dns":          7,
			"connectivity": 14,
			"cdn":          18,
			"site":         100,
		},
		SiteSampleSize: map[string]int{
			"dns":          6,
			"connectivity": 8,
			"cdn":          10,
			"site":         26,
		},
		SiteMaxRetirePerRun:          1,
		SiteRetireCooldownSeconds:    30 * 24 * 60 * 60,
		SiteMaxRetirements:           3,
		SiteRegionFailShare:          0.9,
		SiteRegionMinSamples:         30,
		SiteRegionCanaryShare:        0.05,
		SiteRegionCooldownSeconds:    30 * 24 * 60 * 60,
		SiteProberFaultShare:         0.2,
		SiteProberFaultWindowSeconds: 60 * 60,
		SiteTallyRetentionSeconds:    14 * 24 * 60 * 60,
		// four candidates at once keep a refresh that finds two dead ones in a
		// row inside one retry schedule
		SiteCandidateChecksPerPromotion: 4,
	}
}

// ProviderEgressSiteClasses are the classes the pool keeps, in report order:
// the prober's (egresshealth.Classes).
var ProviderEgressSiteClasses = []string{"dns", "connectivity", "cdn", "site"}

// Reports why the settings cannot be applied, or nil.
func (self *ProviderEgressSiteSettings) Validate() error {
	problems := []string{}
	positive := func(name string, value int) {
		if value < 1 {
			problems = append(problems, fmt.Sprintf("%s %d must be positive", name, value))
		}
	}
	share := func(name string, value float64) {
		if !(0 < value && value <= 1) {
			problems = append(problems, fmt.Sprintf("%s %v must be in (0, 1]", name, value))
		}
	}
	positive("site_refresh_interval_seconds", self.SiteRefreshIntervalSeconds)
	positive("site_refresh_max_time_seconds", self.SiteRefreshMaxTimeSeconds)
	positive("site_window_seconds", self.SiteWindowSeconds)
	positive("site_min_samples", self.SiteMinSamples)
	positive("site_max_retire_per_run", self.SiteMaxRetirePerRun)
	positive("site_retire_cooldown_seconds", self.SiteRetireCooldownSeconds)
	positive("site_max_retirements", self.SiteMaxRetirements)
	positive("site_region_min_samples", self.SiteRegionMinSamples)
	positive("site_region_cooldown_seconds", self.SiteRegionCooldownSeconds)
	positive("site_tally_retention_seconds", self.SiteTallyRetentionSeconds)
	positive("site_prober_fault_window_seconds", self.SiteProberFaultWindowSeconds)
	positive("site_candidate_checks_per_promotion", self.SiteCandidateChecksPerPromotion)
	share("site_healthy_exit_share", self.SiteHealthyExitShare)
	share("site_retire_share", self.SiteRetireShare)
	share("site_probation_share", self.SiteProbationShare)
	share("site_region_fail_share", self.SiteRegionFailShare)
	share("site_region_canary_share", self.SiteRegionCanaryShare)
	share("site_prober_fault_share", self.SiteProberFaultShare)
	for _, class := range ProviderEgressSiteClasses {
		positive("site_pool_size."+class, self.SitePoolSize[class])
		positive("site_sample_size."+class, self.SiteSampleSize[class])
	}
	if self.SiteTallyRetentionSeconds < self.SiteWindowSeconds {
		problems = append(problems, "site_tally_retention_seconds must cover site_window_seconds")
	}
	if 0 < len(problems) {
		return errors.New("egress site settings: " + strings.Join(problems, "; "))
	}
	return nil
}

// The refresh cadence as a duration.
func (self *ProviderEgressSiteSettings) SiteRefreshInterval() time.Duration {
	return time.Duration(self.SiteRefreshIntervalSeconds) * time.Second
}

// The bound on one refresh as a duration.
func (self *ProviderEgressSiteSettings) SiteRefreshMaxTime() time.Duration {
	return time.Duration(self.SiteRefreshMaxTimeSeconds) * time.Second
}

// The judged window as a duration.
func (self *ProviderEgressSiteSettings) SiteWindow() time.Duration {
	return time.Duration(self.SiteWindowSeconds) * time.Second
}

// The retirement cooldown as a duration.
func (self *ProviderEgressSiteSettings) SiteRetireCooldown() time.Duration {
	return time.Duration(self.SiteRetireCooldownSeconds) * time.Second
}

// How long a learned mark's canaries must pass, as a duration.
func (self *ProviderEgressSiteSettings) SiteRegionCooldown() time.Duration {
	return time.Duration(self.SiteRegionCooldownSeconds) * time.Second
}

// The prober-fault window as a duration.
func (self *ProviderEgressSiteSettings) SiteProberFaultWindow() time.Duration {
	return time.Duration(self.SiteProberFaultWindowSeconds) * time.Second
}

// The tally retention as a duration.
func (self *ProviderEgressSiteSettings) SiteTallyRetention() time.Duration {
	return time.Duration(self.SiteTallyRetentionSeconds) * time.Second
}

// The defaults with the `settings` block of one egress-sites.yml. A block that
// does not validate is refused whole: the refresh retires and promotes on
// these numbers, and a typo in one of them must stop the refresh rather than
// retire the pool on it.
func ProviderEgressSiteSettingsFromYaml(unmarshal func(any) error) (*ProviderEgressSiteSettings, error) {
	settings := DefaultProviderEgressSiteSettings()
	document := struct {
		Settings *ProviderEgressSiteSettings `yaml:"settings"`
	}{Settings: settings}
	if err := unmarshal(&document); err != nil {
		return nil, err
	}
	// a map in the file replaces the default map entry by entry, so a file
	// that names one class keeps the others' defaults
	defaults := DefaultProviderEgressSiteSettings()
	for class, size := range defaults.SitePoolSize {
		if _, ok := settings.SitePoolSize[class]; !ok {
			settings.SitePoolSize[class] = size
		}
	}
	for class, size := range defaults.SiteSampleSize {
		if _, ok := settings.SiteSampleSize[class]; !ok {
			settings.SiteSampleSize[class] = size
		}
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	return settings, nil
}

// Reads the deployment's settings: the defaults when egress-sites.yml is
// absent, an error when it is present and unusable.
func GetProviderEgressSiteSettings() (*ProviderEgressSiteSettings, error) {
	resource, err := server.Config.SimpleResource(ProviderEgressSitesResourceName)
	if err != nil {
		if errors.Is(err, server.ErrResourceNotFound) {
			return DefaultProviderEgressSiteSettings(), nil
		}
		return nil, err
	}
	return ProviderEgressSiteSettingsFromYaml(resource.UnmarshalYamlE)
}

// One site's loads on one day, every place summed.
type ProviderEgressSiteDayTotal struct {
	Day                 time.Time
	Name                string
	LoadCount           int
	FailureCount        int
	HealthyLoadCount    int
	HealthyFailureCount int
}

// Sums the site tally per site and day, over the days at or after minDay, for
// the named sites only: what judging a site on probation on every load since
// its promotion day needs, without reading every place's rows for the whole
// retention.
func GetProviderEgressSiteDayTotals(ctx context.Context, minDay time.Time, names []string) []ProviderEgressSiteDayTotal {
	totals := []ProviderEgressSiteDayTotal{}
	if len(names) == 0 {
		return totals
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				tally_day,
				name,
				sum(load_count)::bigint,
				sum(failure_count)::bigint,
				sum(healthy_load_count)::bigint,
				sum(healthy_failure_count)::bigint
			FROM provider_egress_site_tally
			WHERE $1 <= tally_day AND name = ANY($2)
			GROUP BY tally_day, name
			ORDER BY tally_day, name
			`,
			minDay.UTC().Truncate(24*time.Hour),
			names,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var t ProviderEgressSiteDayTotal
				var loads, failures, healthyLoads, healthyFailures int64
				server.Raise(result.Scan(&t.Day, &t.Name, &loads, &failures, &healthyLoads, &healthyFailures))
				t.LoadCount = int(loads)
				t.FailureCount = int(failures)
				t.HealthyLoadCount = int(healthyLoads)
				t.HealthyFailureCount = int(healthyFailures)
				totals = append(totals, t)
			}
		})
	})
	return totals
}
