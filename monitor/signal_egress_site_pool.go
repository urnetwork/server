package monitor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// SIGNALS.md §2.19b maps to signal_egress_site_pool.go and
// signal_egress_site_pool_test.go. The prober's verdicts are only as good as
// the sites it loads (connect/GEOMAP.md §11.4): the pool is data a daily task,
// RefreshEgressDestinations, keeps representative from our own load tally. The
// signal watches that the task does its job and that what it cannot heal is
// seen: a site failing everyone that stays, the task not running, a class or a
// place too thin to sample, a site blocked in a place and not yet marked, a
// country whose exits cannot be reached at all, a backfill that hides an
// emptied bucket, a blackhole retry queue that is not served, and a fault on
// the prober rather than the pool. It reads PostgreSQL for the pool, the
// tally, the task and the checks, and Mimir for the backfill and the batch
// guards; its reasons are site names, classes, country codes and region names
// -- all bounded -- and never a provider id.
func NewEgressSitePoolSignal() Signal {
	return NewEgressSitePoolSignalWithSettings(DefaultEgressSitePoolSettings())
}

// The signal with explicit thresholds.
func NewEgressSitePoolSignalWithSettings(settings *EgressSitePoolSettings) Signal {
	return &signalAdapter{
		number: "2.19b", key: "egress-site-pool", name: "Egress site pool freshness",
		probe: egressSitePoolProbe{settings: settings},
	}
}

const refreshEgressDestinationsTaskFunction = "github.com/urnetwork/server/taskworker/work.RefreshEgressDestinations"

// egressSitePoolProbeId is the alert id of every finding.
const egressSitePoolProbeId = "pg/egress-site-pool"

// The thresholds of §2.19b the pool's own settings do not carry. The pool's
// retire line, sample sizes, region shares and the prober-fault line are the
// refresh task's own (model.ProviderEgressSiteSettings, read from
// egress-sites.yml), so the monitor judges by the numbers the task acts on.
type EgressSitePoolSettings struct {
	// pool needs refresh: an active site above the retire line this long
	NeedsRefreshAge time.Duration
	// backfill sustained: the borrowed share of the providers FindProviders2
	// answered with, per rank mode, above BackfillShare over BackfillWindow
	BackfillShare  float64
	BackfillWindow time.Duration
	// prober fault: any batch guard trip within GuardTripWindow; and the
	// fewest scored loads a class needs in the prober-fault window before its
	// failed share is judged
	GuardTripWindow          time.Duration
	ProberFaultMinClassLoads int
	// country failure pattern: every observed active scored site fails at
	// least this share of its loads, over the region minimum of runs
	CountryUnreachableShare float64
	// the most failing sites, places or countries one cadence lists
	MaxListed int
	// the largest Mimir answer, including bounded per-slot source coverage
	MaxResponseBytes int
}

// The defaults of SIGNALS.md §2.19b.
func DefaultEgressSitePoolSettings() *EgressSitePoolSettings {
	return &EgressSitePoolSettings{
		NeedsRefreshAge:          24 * time.Hour,
		BackfillShare:            0.5,
		BackfillWindow:           time.Hour,
		GuardTripWindow:          time.Hour,
		ProberFaultMinClassLoads: 100,
		CountryUnreachableShare:  0.9,
		MaxListed:                20,
		MaxResponseBytes:         2 * 1024 * 1024,
	}
}

// Carries the thresholds and the seam that reads the pool's settings, so a
// test judges by its own numbers rather than a workstation's config.
type egressSitePoolProbe struct {
	settings *EgressSitePoolSettings
	// loadPoolSettings reads the refresh's settings, the dark rules and the
	// candidate list; nil reads the deployment's.
	loadPoolSettings func() (*egressSitePoolContext, error)
}

// Implements probe: the alert id of every finding.
func (egressSitePoolProbe) id() string { return egressSitePoolProbeId }

// Implements probe: every condition warns.
func (egressSitePoolProbe) tier() string { return tierWarn }

// Implements probe: the pool and its tally change slowly.
func (egressSitePoolProbe) cadence() time.Duration { return 5 * time.Minute }

// What the signal reads from configuration: the pool refresh's settings, the
// probe's dark rules, and the names egress-sites.yml lists (candidates the
// refresh will sync on its next run).
type egressSitePoolContext struct {
	siteSettings         *model.ProviderEgressSiteSettings
	rules                model.ProviderEgressRules
	candidateNameClasses map[string]string
}

// Reads the deployment's configuration. An unusable egress-sites.yml is an
// error: the refresh refuses to run on it too, and the signal must not judge
// the pool by numbers the task is not using.
func loadEgressSitePoolContext() (*egressSitePoolContext, error) {
	sites, err := controller.LoadProviderEgressSites()
	if err != nil {
		return nil, err
	}
	candidateNameClasses := map[string]string{}
	for _, candidate := range sites.Candidates {
		candidateNameClasses[candidate.Name] = candidate.Class
	}
	return &egressSitePoolContext{
		siteSettings:         sites.Settings,
		rules:                model.GetProviderEgressRules(),
		candidateNameClasses: candidateNameClasses,
	}, nil
}

// One pool row as the signal reads it.
type egressSitePoolDestination struct {
	name                  string
	class                 string
	active                bool
	probation             bool
	retiredAgeSeconds     int64
	retireCount           int64
	aboveRetireAgeSeconds int64
	failureShare          float64
	sampleCount           int64
}

// One active site failing the healthy exits of one place that is not in its
// incompatible list.
type egressSitePoolRegionalFailure struct {
	name            string
	class           string
	country         string
	region          string
	healthyLoads    int64
	healthyFailures int64
}

// One class whose compatible active sites at one place are fewer than its
// sample size.
type egressSitePoolThinPlace struct {
	class           string
	country         string
	region          string
	compatibleSites int64
	runs            int64
	sampleSize      int64
}

// One country where every observed active scored site has a high failed-load
// share. Sites without a load tally are not represented.
type egressSitePoolUnreachableCountry struct {
	country      string
	sites        int64
	loads        int64
	failures     int64
	runs         int64
	echoFailures int64
}

// The refresh task's pending_task state.
type egressSitePoolTask struct {
	rows               int64
	failingRows        int64
	maxRescheduleCount int64
	// overdue is how far past its run time the row is, in seconds; negative
	// when it is not due yet, and meaningless without a row
	overdueSeconds int64
}

// The blackhole retry queue's state.
type egressSitePoolRetries struct {
	retryRows         int64
	overdueRows       int64
	maxOverdueSeconds int64
}

// One class's scored loads in the prober-fault window.
type egressSitePoolClassLoads struct {
	ok    int64
	total int64
}

// What Mimir says: borrowed and answered providers per rank mode over the
// backfill window, and guard trips per schedule over the guard window.
// observable is false when it could not be read.
type egressSitePoolMetrics struct {
	observable       bool
	reason           string
	coverage         string
	backfillComplete bool
	guardComplete    bool
	borrowed         map[string]float64
	answered         map[string]float64
	guardTrips       map[string]float64
}

// One cadence's evidence.
type egressSitePoolObservation struct {
	destinations         []egressSitePoolDestination
	regionalFailures     []egressSitePoolRegionalFailure
	thinPlaces           []egressSitePoolThinPlace
	unreachableCountries []egressSitePoolUnreachableCountry
	task                 egressSitePoolTask
	retries              egressSitePoolRetries
	classLoads           map[string]egressSitePoolClassLoads
	metrics              egressSitePoolMetrics
}

// Implements probe: one cadence, from configuration, PostgreSQL and Mimir.
func (self egressSitePoolProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	schema := egressSitePoolSchemaContract()
	ready, err := probeSchemaReady(ctx, env, schema)
	if err != nil {
		return nil, err
	}
	if !ready {
		return []finding{probeSchemaUnavailableFinding(schema, pgTarget(env))}, nil
	}
	settings := self.settings
	if settings == nil {
		settings = DefaultEgressSitePoolSettings()
	}
	load := self.loadPoolSettings
	if load == nil {
		load = loadEgressSitePoolContext
	}
	poolContext, err := load()
	if err != nil {
		return nil, fmt.Errorf("egress site pool settings: %w", err)
	}
	observation, err := queryEgressSitePool(ctx, env, settings, poolContext)
	if err != nil {
		return nil, err
	}
	observation.metrics = readEgressSitePoolMetrics(ctx, env, settings)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append(evaluateEgressSitePool(settings, poolContext, pgTarget(env), observation), healthyFinding(schema.probeId, tierWarn, schema.class, pgTarget(env))), nil
}

// The first tally day of the refresh's window, as the refresh computes it
// (model.ProviderEgressSiteTallyWindowStart).
func egressSitePoolWindowStartSql(siteSettings *model.ProviderEgressSiteSettings) string {
	return fmt.Sprintf(
		`date_trunc('day', (now() AT TIME ZONE 'utc') - interval '%d seconds')::date`,
		siteSettings.SiteWindowSeconds,
	)
}

// egressSitePoolCoversSql is whether one of a destination's incompatible
// places covers (country_code, region), as the prober compares them.
const egressSitePoolCoversSql = `EXISTS (
	SELECT 1 FROM jsonb_array_elements(d.incompatible) AS mark
	WHERE lower(trim(mark ->> 'country')) = %[1]s
	  AND (COALESCE(trim(mark ->> 'region'), '') = '' OR lower(trim(mark ->> 'region')) = lower(%[2]s))
)`

// Every pool row with the refresh's judgement of it.
func egressSitePoolDestinationsQuery() string {
	return `
/* monitor-signal-2.19b-egress-site-pool-destinations */
SELECT name, class, active::text, probation::text,
       COALESCE(floor(extract(epoch FROM (now() AT TIME ZONE 'utc') - retired_time))::bigint, -1)::text,
       retire_count::text,
       COALESCE(floor(extract(epoch FROM (now() AT TIME ZONE 'utc') - above_retire_since))::bigint, -1)::text,
       COALESCE(failure_share, -1)::text,
       sample_count::text
FROM provider_egress_destination
ORDER BY name;
`
}

// Active scored sites failing the healthy exits of a place where most sites
// pass, and that the site is not marked for.
func egressSitePoolRegionalQuery(settings *EgressSitePoolSettings, siteSettings *model.ProviderEgressSiteSettings) string {
	return fmt.Sprintf(`
/* monitor-signal-2.19b-egress-site-pool-regional */
WITH window_tally AS (
    SELECT name, country_code, region,
           sum(healthy_load_count)::bigint AS healthy_loads,
           sum(healthy_failure_count)::bigint AS healthy_failures
    FROM provider_egress_site_tally
    WHERE %[1]s <= tally_day AND country_code <> ''
    GROUP BY name, country_code, region
), places AS (
    SELECT name, country_code, ''::text AS region,
           sum(healthy_loads)::bigint AS healthy_loads, sum(healthy_failures)::bigint AS healthy_failures
    FROM window_tally
    GROUP BY name, country_code
    UNION ALL
    SELECT name, country_code, region, healthy_loads, healthy_failures
    FROM window_tally
    WHERE region <> ''
), scored AS (
    SELECT places.*
    FROM places
    JOIN provider_egress_destination d ON d.name = places.name
    WHERE d.active AND NOT d.probation
), place_passing AS (
    SELECT country_code, region,
           count(*) AS sites,
           count(*) FILTER (WHERE %[2]f * healthy_loads <= healthy_failures) AS failing
    FROM scored
    WHERE 0 < healthy_loads
    GROUP BY country_code, region
)
SELECT s.name, d.class, s.country_code, s.region, s.healthy_loads::text, s.healthy_failures::text
FROM scored s
JOIN provider_egress_destination d ON d.name = s.name
JOIN place_passing pp ON pp.country_code = s.country_code AND pp.region = s.region
WHERE %[3]d <= s.healthy_loads
  AND %[2]f * s.healthy_loads <= s.healthy_failures
  AND 2 * pp.failing < pp.sites
  AND COALESCE(d.failure_share, 0) <= %[4]f
  AND NOT %[5]s
ORDER BY s.healthy_failures::float8 / s.healthy_loads DESC, s.name, s.country_code, s.region
LIMIT %[6]d;
`,
		egressSitePoolWindowStartSql(siteSettings),
		siteSettings.SiteRegionFailShare,
		siteSettings.SiteRegionMinSamples,
		siteSettings.SiteRetireShare,
		fmt.Sprintf(egressSitePoolCoversSql, "s.country_code", "s.region"),
		settings.MaxListed,
	)
}

// Places with exits in the window where a class has fewer compatible active
// sites than its sample size.
func egressSitePoolPlaceThinQuery(settings *EgressSitePoolSettings, siteSettings *model.ProviderEgressSiteSettings) string {
	classValues := []string{}
	for _, class := range model.ProviderEgressSiteClasses {
		classValues = append(classValues, fmt.Sprintf("('%s', %d)", class, siteSettings.SiteSampleSize[class]))
	}
	return fmt.Sprintf(`
/* monitor-signal-2.19b-egress-site-pool-places */
WITH window_places AS (
    SELECT country_code, region, sum(run_count)::bigint AS runs
    FROM provider_egress_place_tally
    WHERE %[1]s <= tally_day AND country_code <> ''
    GROUP BY country_code, region
), places AS (
    SELECT country_code, ''::text AS region, sum(runs)::bigint AS runs
    FROM window_places
    GROUP BY country_code
    UNION ALL
    SELECT country_code, region, runs FROM window_places WHERE region <> ''
), classes AS (
    SELECT * FROM (VALUES %[2]s) AS class_sizes(class, sample_size)
), compatible AS (
    SELECT c.class, p.country_code, p.region, p.runs, c.sample_size,
           (
               SELECT count(*)
               FROM provider_egress_destination d
               WHERE d.active AND d.class = c.class AND NOT %[3]s
           ) AS compatible_sites
    FROM places p
    CROSS JOIN classes c
)
SELECT class, country_code, region, compatible_sites::text, runs::text, sample_size::text
FROM compatible
WHERE compatible_sites < sample_size
ORDER BY compatible_sites, class, country_code, region
LIMIT %[4]d;
`,
		egressSitePoolWindowStartSql(siteSettings),
		strings.Join(classValues, ", "),
		fmt.Sprintf(egressSitePoolCoversSql, "p.country_code", "p.region"),
		settings.MaxListed,
	)
}

// Countries where every active scored site fails the exits.
func egressSitePoolUnreachableQuery(settings *EgressSitePoolSettings, siteSettings *model.ProviderEgressSiteSettings) string {
	return fmt.Sprintf(`
/* monitor-signal-2.19b-egress-site-pool-countries */
WITH site_country AS (
    SELECT t.name, t.country_code,
           sum(t.load_count)::bigint AS loads, sum(t.failure_count)::bigint AS failures
    FROM provider_egress_site_tally t
    JOIN provider_egress_destination d ON d.name = t.name
    WHERE %[1]s <= t.tally_day AND t.country_code <> '' AND d.active AND NOT d.probation
    GROUP BY t.name, t.country_code
), country_runs AS (
    SELECT country_code, sum(run_count)::bigint AS runs, sum(echo_failure_count)::bigint AS echo_failures
    FROM provider_egress_place_tally
    WHERE %[1]s <= tally_day AND country_code <> ''
    GROUP BY country_code
)
SELECT sc.country_code, count(*)::text, sum(sc.loads)::text, sum(sc.failures)::text,
       cr.runs::text, cr.echo_failures::text
FROM site_country sc
JOIN country_runs cr ON cr.country_code = sc.country_code
WHERE 0 < sc.loads
GROUP BY sc.country_code, cr.runs, cr.echo_failures
HAVING %[2]d <= cr.runs AND bool_and(%[3]f * sc.loads <= sc.failures)
ORDER BY sc.country_code
LIMIT %[4]d;
`,
		egressSitePoolWindowStartSql(siteSettings),
		siteSettings.SiteRegionMinSamples,
		settings.CountryUnreachableShare,
		settings.MaxListed,
	)
}

// The refresh task's pending_task state.
func egressSitePoolTaskQuery() string {
	return `
/* monitor-signal-2.19b-egress-site-pool-task */
SELECT count(*)::text,
       count(*) FILTER (WHERE reschedule_error IS NOT NULL)::text,
       COALESCE(max(reschedule_error_count), 0)::text,
       COALESCE(max(floor(extract(epoch FROM (now() AT TIME ZONE 'utc') - run_at)))::bigint, 0)::text
FROM pending_task
WHERE function_name = '` + refreshEgressDestinationsTaskFunction + `';
`
}

// Blackhole retries of eligible providers, and those more than one backoff
// step past their due time.
func egressSitePoolRetriesQuery(rules model.ProviderEgressRules) string {
	steps := []string{}
	for _, seconds := range rules.DarkBackoffSeconds {
		steps = append(steps, strconv.Itoa(seconds))
	}
	return fmt.Sprintf(`
/* monitor-signal-2.19b-egress-site-pool-retries */
WITH clock AS MATERIALIZED (
    SELECT now() AT TIME ZONE 'utc' AS utc_now
), eligible AS MATERIALIZED (
    SELECT nclr.client_id
    FROM network_client_location_reliability nclr
    INNER JOIN network_client nc USING (client_id)
    WHERE nc.active AND nc.source_client_id IS NULL
      AND nclr.connected AND nclr.valid
      AND EXISTS (
          SELECT 1 FROM provide_key pk
          WHERE pk.client_id = nclr.client_id AND pk.provide_mode = 3
      )
), retries AS (
    SELECT pbc.next_due_at, clock.utc_now,
           (ARRAY[%[1]s])[LEAST(GREATEST(pbc.consecutive_failures, 1), %[2]d)] AS step_seconds
    FROM provider_blackhole_check pbc
    JOIN eligible USING (client_id)
    CROSS JOIN clock
    WHERE pbc.next_due_at IS NOT NULL
      AND (0 < pbc.consecutive_failures OR pbc.failure = '%[3]s')
)
SELECT count(*)::text,
       count(*) FILTER (WHERE next_due_at < utc_now - step_seconds * interval '1 second')::text,
       COALESCE(max(floor(extract(epoch FROM utc_now - next_due_at)))
           FILTER (WHERE next_due_at < utc_now - step_seconds * interval '1 second')::bigint, 0)::text
FROM retries;
`,
		strings.Join(steps, ", "),
		len(steps),
		model.ProviderBlackholeNotMeasuredFailure,
	)
}

// The fleet's scored loads per class within the prober-fault window.
func egressSitePoolClassLoadsQuery(siteSettings *model.ProviderEgressSiteSettings) string {
	return fmt.Sprintf(`
/* monitor-signal-2.19b-egress-site-pool-fleet */
SELECT class.key,
       sum(COALESCE((class.value ->> 'ok')::bigint, 0))::text,
       sum(COALESCE((class.value ->> 'total')::bigint, 0))::text
FROM provider_egress_health
CROSS JOIN LATERAL jsonb_each(provider_egress_health.class_results) AS class
WHERE (now() AT TIME ZONE 'utc') - interval '%[1]d seconds' <= provider_egress_health.measured_at
  AND class.key IN ('%[2]s')
GROUP BY class.key
ORDER BY class.key;
`,
		siteSettings.SiteProberFaultWindowSeconds,
		strings.Join(model.ProviderEgressSiteClasses, "', '"),
	)
}

// Runs the PostgreSQL half of one cadence. Every query returns a fixed shape
// with bounded rows; a malformed answer fails the probe rather than being
// judged.
func queryEgressSitePool(
	ctx context.Context,
	env *probeEnv,
	settings *EgressSitePoolSettings,
	poolContext *egressSitePoolContext,
) (egressSitePoolObservation, error) {
	observation := egressSitePoolObservation{classLoads: map[string]egressSitePoolClassLoads{}}
	integer := func(row pgRow, column int) (int64, error) {
		return parseStrictInt64(row.str(column))
	}

	rows, err := env.runner.pg(ctx, egressSitePoolDestinationsQuery())
	if err != nil {
		return observation, err
	}
	for _, row := range rows {
		if len(row) != 9 {
			return observation, fmt.Errorf("egress site pool destinations returned an invalid row shape")
		}
		d := egressSitePoolDestination{name: row.str(0), class: row.str(1)}
		if d.active, err = strconv.ParseBool(row.str(2)); err != nil {
			return observation, fmt.Errorf("egress site pool destinations returned an invalid active flag")
		}
		if d.probation, err = strconv.ParseBool(row.str(3)); err != nil {
			return observation, fmt.Errorf("egress site pool destinations returned an invalid probation flag")
		}
		for column, target := range map[int]*int64{4: &d.retiredAgeSeconds, 5: &d.retireCount, 6: &d.aboveRetireAgeSeconds, 8: &d.sampleCount} {
			if *target, err = integer(row, column); err != nil {
				return observation, fmt.Errorf("egress site pool destinations returned an invalid integer field %d", column)
			}
		}
		if d.failureShare, err = strconv.ParseFloat(row.str(7), 64); err != nil || math.IsNaN(d.failureShare) {
			return observation, fmt.Errorf("egress site pool destinations returned an invalid failure share")
		}
		observation.destinations = append(observation.destinations, d)
	}

	rows, err = env.runner.pg(ctx, egressSitePoolRegionalQuery(settings, poolContext.siteSettings))
	if err != nil {
		return observation, err
	}
	for _, row := range rows {
		if len(row) != 6 {
			return observation, fmt.Errorf("egress site pool regional returned an invalid row shape")
		}
		regionalFailure := egressSitePoolRegionalFailure{name: row.str(0), class: row.str(1), country: row.str(2), region: row.str(3)}
		if regionalFailure.healthyLoads, err = integer(row, 4); err != nil {
			return observation, fmt.Errorf("egress site pool regional returned an invalid load count")
		}
		if regionalFailure.healthyFailures, err = integer(row, 5); err != nil {
			return observation, fmt.Errorf("egress site pool regional returned an invalid failure count")
		}
		observation.regionalFailures = append(observation.regionalFailures, regionalFailure)
	}

	rows, err = env.runner.pg(ctx, egressSitePoolPlaceThinQuery(settings, poolContext.siteSettings))
	if err != nil {
		return observation, err
	}
	for _, row := range rows {
		if len(row) != 6 {
			return observation, fmt.Errorf("egress site pool places returned an invalid row shape")
		}
		thinPlace := egressSitePoolThinPlace{class: row.str(0), country: row.str(1), region: row.str(2)}
		for column, target := range map[int]*int64{3: &thinPlace.compatibleSites, 4: &thinPlace.runs, 5: &thinPlace.sampleSize} {
			if *target, err = integer(row, column); err != nil {
				return observation, fmt.Errorf("egress site pool places returned an invalid integer field %d", column)
			}
		}
		observation.thinPlaces = append(observation.thinPlaces, thinPlace)
	}

	rows, err = env.runner.pg(ctx, egressSitePoolUnreachableQuery(settings, poolContext.siteSettings))
	if err != nil {
		return observation, err
	}
	for _, row := range rows {
		if len(row) != 6 {
			return observation, fmt.Errorf("egress site pool countries returned an invalid row shape")
		}
		unreachableCountry := egressSitePoolUnreachableCountry{country: row.str(0)}
		for column, target := range map[int]*int64{1: &unreachableCountry.sites, 2: &unreachableCountry.loads, 3: &unreachableCountry.failures, 4: &unreachableCountry.runs, 5: &unreachableCountry.echoFailures} {
			if *target, err = integer(row, column); err != nil {
				return observation, fmt.Errorf("egress site pool countries returned an invalid integer field %d", column)
			}
		}
		observation.unreachableCountries = append(observation.unreachableCountries, unreachableCountry)
	}

	rows, err = env.runner.pg(ctx, egressSitePoolTaskQuery())
	if err != nil {
		return observation, err
	}
	row, err := pgAggregateRow(rows, 4)
	if err != nil {
		return observation, fmt.Errorf("egress site pool task: %w", err)
	}
	for column, target := range map[int]*int64{0: &observation.task.rows, 1: &observation.task.failingRows, 2: &observation.task.maxRescheduleCount, 3: &observation.task.overdueSeconds} {
		if *target, err = integer(row, column); err != nil {
			return observation, fmt.Errorf("egress site pool task returned an invalid integer field %d", column)
		}
	}

	rows, err = env.runner.pg(ctx, egressSitePoolRetriesQuery(poolContext.rules))
	if err != nil {
		return observation, err
	}
	row, err = pgAggregateRow(rows, 3)
	if err != nil {
		return observation, fmt.Errorf("egress site pool retries: %w", err)
	}
	for column, target := range map[int]*int64{0: &observation.retries.retryRows, 1: &observation.retries.overdueRows, 2: &observation.retries.maxOverdueSeconds} {
		if *target, err = integer(row, column); err != nil {
			return observation, fmt.Errorf("egress site pool retries returned an invalid integer field %d", column)
		}
	}

	rows, err = env.runner.pg(ctx, egressSitePoolClassLoadsQuery(poolContext.siteSettings))
	if err != nil {
		return observation, err
	}
	for _, row := range rows {
		if len(row) != 3 {
			return observation, fmt.Errorf("egress site pool fleet returned an invalid row shape")
		}
		loads := egressSitePoolClassLoads{}
		if loads.ok, err = integer(row, 1); err != nil {
			return observation, fmt.Errorf("egress site pool fleet returned an invalid ok count")
		}
		if loads.total, err = integer(row, 2); err != nil || loads.total < loads.ok {
			return observation, fmt.Errorf("egress site pool fleet returned an invalid total count")
		}
		observation.classLoads[row.str(0)] = loads
	}
	return observation, nil
}

// A finding of one class with the section's playbook.
func egressSitePoolFinding(class string, target string, frame string, sustain int) finding {
	return finding{
		probeId: egressSitePoolProbeId, tier: tierWarn,
		class: class, target: target, frame: frame, sustain: sustain,
		playbook: "SIGNALS.md §2.19b",
	}
}

// Judges the nine conditions of §2.19b. Each condition emits its failing
// parts, or one healthy finding when none fails, or nothing when its evidence
// is unobservable: a healthy finding resolves every open part of its class, so
// it must never stand in for "unknown".
func evaluateEgressSitePool(
	settings *EgressSitePoolSettings,
	poolContext *egressSitePoolContext,
	target string,
	observation egressSitePoolObservation,
) []finding {
	siteSettings := poolContext.siteSettings
	findings := []finding{}
	emit := func(class string, failing []finding) {
		if 0 < len(failing) {
			findings = append(findings, failing...)
			return
		}
		findings = append(findings, healthyFinding(egressSitePoolProbeId, tierWarn, class, target))
	}
	placeKey := func(country string, region string) string {
		if region == "" {
			return country
		}
		return country + "/" + region
	}

	// pool needs refresh
	{
		failing := []finding{}
		needsRefreshSeconds := int64(settings.NeedsRefreshAge / time.Second)
		for _, d := range observation.destinations {
			if !d.active || d.probation || d.aboveRetireAgeSeconds < needsRefreshSeconds {
				continue
			}
			f := egressSitePoolFinding("egress-site-pool-needs-refresh", target, d.name, 1)
			f.symptom = fmt.Sprintf("Active %s site %s has stood above the retire line on healthy exits for more than %s", d.class, d.name, settings.NeedsRefreshAge)
			f.mechanism = "A site that fails exits known to work cannot tell one exit from another; it lowers every provider's index and pushes healthy providers past the one-in-ten line. RefreshEgressDestinations retires it on its next run, at most SiteMaxRetirePerRun per class, unless the run is skipped for a prober fault."
			f.baseline = fmt.Sprintf("No active scored site stays above SiteRetireShare=%.2f over at least SiteMinSamples=%d healthy exits for more than %s.", siteSettings.SiteRetireShare, siteSettings.SiteMinSamples, settings.NeedsRefreshAge)
			f.observed = fmt.Sprintf("site=%s class=%s failure_share=%.3f samples=%d above_retire_seconds=%d threshold=%ds", d.name, d.class, d.failureShare, d.sampleCount, d.aboveRetireAgeSeconds, needsRefreshSeconds)
			f.evidence = "The refresh's own judgement on the pool row (failure_share, sample_count, above_retire_since); the site name is bounded by the pool, and no provider identity is read."
			f.context = "Several sites of one class above the line at once are retired one a day by design; a refresh skipped for a prober fault leaves them in place, which the prober-fault finding names."
			f.action = "Confirm the RefreshEgressDestinations run and its [egresssites] log line; if it ran and skipped, read the prober-fault finding first. If it ran and retired another site of the class, the class is degrading faster than the per-run cap: review the site against egress-sites.yml."
			f.verify = "The site is retired or falls under the retire line within one refresh cadence, and the quality-bucket count of §2.9a stops falling."
			failing = append(failing, f)
			if settings.MaxListed <= len(failing) {
				break
			}
		}
		emit("egress-site-pool-needs-refresh", failing)
	}

	// refresh not running
	{
		failing := []finding{}
		cadenceSeconds := int64(siteSettings.SiteRefreshIntervalSeconds)
		switch {
		case observation.task.rows == 0:
			f := egressSitePoolFinding("egress-site-refresh-not-running", target, "task-missing", 2)
			f.symptom = "The pool refresh has no pending_task row, so the pool is not judged, retired or refilled"
			f.mechanism = "RefreshEgressDestinations is a RunOnce chain whose Post schedules the next run. Without a row the chain is lost and the pool freezes as it stands."
			f.baseline = "Exactly one pending_task row for " + refreshEgressDestinationsTaskFunction + ", without a reschedule error."
			f.observed = "pending_task_rows=0 threshold=one_pending_task_row"
			f.action = "Confirm the taskworker registers RefreshEgressDestinations and InitTasks arms it; a restart of a taskworker from this source re-arms the chain. Do not insert a row by hand."
			f.verify = "One RefreshEgressDestinations row exists and a refresh runs within its cadence."
			failing = append(failing, f)
		case 0 < observation.task.failingRows:
			f := egressSitePoolFinding("egress-site-refresh-not-running", target, "task-parked", 2)
			f.symptom = "The pool refresh's pending_task row retains an error from an earlier retry"
			f.mechanism = "A stored reschedule error records a failed attempt and can remain while a later retry is actively claimed. A failed refresh can delay pool maintenance, but this sample does not establish that execution has stopped."
			f.baseline = "The RefreshEgressDestinations row carries no reschedule error."
			f.observed = fmt.Sprintf("pending_task_rows=%d failing_rows=%d max_reschedule_error_count=%d threshold=no_reschedule_error execution_state=unobserved", observation.task.rows, observation.task.failingRows, observation.task.maxRescheduleCount)
			f.context = "The legacy task-parked frame preserves ticket identity only. This query reads no claim heartbeat or attempt phase; a separate RunPost task is not joined to this row."
			f.action = "Read the row's reschedule error and the taskworker's [egresssites] log lines; an unusable egress-sites.yml names its first bad entry. Repair it and let the chain retry."
			f.verify = "The reschedule error clears and a refresh runs."
			failing = append(failing, f)
		case cadenceSeconds < observation.task.overdueSeconds:
			f := egressSitePoolFinding("egress-site-refresh-not-running", target, "stale-run", 2)
			f.symptom = "The pool refresh's current pending schedule is more than one cadence overdue"
			f.mechanism = "The current run_at is a due anchor, not an observed last completion. An old schedule can accompany a delayed claim, active long run, retry or separate post phase; this aggregate does not distinguish them."
			f.baseline = fmt.Sprintf("The row is never more than SiteRefreshInterval=%ds past its run time.", cadenceSeconds)
			f.observed = fmt.Sprintf("overdue_seconds=%d threshold=%ds schedule_anchor=run_at execution_state=unobserved", observation.task.overdueSeconds, cadenceSeconds)
			f.context = "Rescheduling can mask older delay; claim heartbeat, last completed run, RunPost linkage and running artifact remain unobserved here."
			f.action = "Correlate the schedule with bounded taskworker claim/heartbeat and completed refresh evidence (§1.2, §8.9) before diagnosing worker absence or changing the chain."
			f.verify = "A completed refresh advances pool judgement and schedules the next run. Moving run_at alone is not proof that the pool was refreshed."
			failing = append(failing, f)
		}
		emit("egress-site-refresh-not-running", failing)
	}

	// pool thin
	{
		failing := []finding{}
		for _, class := range model.ProviderEgressSiteClasses {
			active, candidates := int64(0), int64(0)
			names := map[string]bool{}
			for _, d := range observation.destinations {
				names[d.name] = true
				if d.class != class {
					continue
				}
				switch {
				case d.active:
					active++
				case int64(siteSettings.SiteMaxRetirements) <= d.retireCount:
				case d.retiredAgeSeconds < 0 || int64(siteSettings.SiteRetireCooldownSeconds) <= d.retiredAgeSeconds:
					candidates++
				}
			}
			// a candidate listed in egress-sites.yml and not yet synced is a
			// candidate the next refresh brings in
			for name, candidateClass := range poolContext.candidateNameClasses {
				if candidateClass == class && !names[name] {
					candidates++
				}
			}
			poolSize := int64(siteSettings.SitePoolSize[class])
			if poolSize <= active && 0 < candidates {
				continue
			}
			reason := "below-pool-size"
			if poolSize <= active {
				reason = "candidates-exhausted"
			}
			f := egressSitePoolFinding("egress-site-pool-thin", target, class+"/"+reason, 2)
			f.symptom = fmt.Sprintf("The %s class holds %d active sites against its pool size of %d, with %d candidates to promote", class, active, poolSize, candidates)
			f.mechanism = "The refresh keeps SitePoolSize sites per class by promoting a candidate for every retirement, and heals only as far as the candidate list reaches. A class under its size samples fewer sites per run; a class with no candidate left cannot replace the next site it retires."
			f.baseline = fmt.Sprintf("At least SitePoolSize=%d active %s sites and at least one promotable candidate.", poolSize, class)
			f.observed = fmt.Sprintf("class=%s active=%d pool_size=%d candidates=%d reason=%s", class, active, poolSize, candidates, reason)
			f.evidence = "Pool rows by state, and the candidate names egress-sites.yml lists; no provider identity."
			f.action = "Extend the " + class + " candidates in config/all/egress-sites.yml with sites of the category that left, each with a load contract a plain GET satisfies; a site retired for good returns by raising its entry's revision."
			f.verify = "The next refresh promotes into the class and the active count reaches its pool size."
			failing = append(failing, f)
		}
		emit("egress-site-pool-thin", failing)
	}

	// regional failure unmarked
	{
		failing := []finding{}
		for _, regionalFailure := range observation.regionalFailures {
			key := placeKey(regionalFailure.country, regionalFailure.region)
			f := egressSitePoolFinding("egress-site-regional-failure-unmarked", target, regionalFailure.name+"@"+key, 2)
			f.symptom = fmt.Sprintf("Site %s fails %d of %d healthy exits in %s while it works elsewhere, and %s is not in its incompatible list", regionalFailure.name, regionalFailure.healthyFailures, regionalFailure.healthyLoads, key, key)
			f.mechanism = "A site blocked in a country or region says nothing about the exits there, and until it is marked every exit there pays for it in its index. The refresh marks a place where most sites pass and this one fails at least SiteRegionFailShare of the healthy exits."
			f.baseline = fmt.Sprintf("No active site fails SiteRegionFailShare=%.2f of at least SiteRegionMinSamples=%d healthy exits at a place where most sites pass without that place being marked.", siteSettings.SiteRegionFailShare, siteSettings.SiteRegionMinSamples)
			f.observed = fmt.Sprintf("site=%s class=%s place=%s healthy_exits=%d failed=%d", regionalFailure.name, regionalFailure.class, key, regionalFailure.healthyLoads, regionalFailure.healthyFailures)
			f.evidence = "The per-site, per-place load tally over the refresh window; the site, the country code and the region name are bounded, and no provider identity leaves PostgreSQL."
			f.action = "Confirm the next refresh marks the place (its log line names it); if the refresh is not running, see that finding. A site blocked everywhere in a region is also a candidate-list gap for that region."
			f.verify = "The place appears in the site's incompatible list and the site's loads from there stop counting."
			failing = append(failing, f)
		}
		emit("egress-site-regional-failure-unmarked", failing)
	}

	// place pool thin
	{
		failing := []finding{}
		for _, thinPlace := range observation.thinPlaces {
			key := placeKey(thinPlace.country, thinPlace.region)
			f := egressSitePoolFinding("egress-site-place-pool-thin", target, thinPlace.class+"@"+key, 2)
			f.symptom = fmt.Sprintf("Only %d active %s sites are compatible with %s, under the class's sample of %d", thinPlace.compatibleSites, thinPlace.class, key, thinPlace.sampleSize)
			f.mechanism = "The prober draws a provider's sample only from the sites compatible with its place; a class thinner than its sample there takes all of them and reports the class short, so runs there are not representative."
			f.baseline = "Every place with exits has at least the class sample size of compatible active sites in each class."
			f.observed = fmt.Sprintf("class=%s place=%s compatible=%d sample_size=%d runs_in_window=%d", thinPlace.class, key, thinPlace.compatibleSites, thinPlace.sampleSize, thinPlace.runs)
			f.evidence = "Active pool rows against their incompatible places, for the places the tally saw exits at; bounded class, country and region names only."
			f.action = "Add " + thinPlace.class + " candidates that work from " + key + " to config/all/egress-sites.yml; the refresh promotes candidates compatible with a thin place first."
			f.verify = "The compatible count for the place reaches the sample size after a refresh."
			failing = append(failing, f)
		}
		emit("egress-site-place-pool-thin", failing)
	}

	// country unreachable
	{
		failing := []finding{}
		for _, unreachableCountry := range observation.unreachableCountries {
			f := egressSitePoolFinding("egress-country-unreachable", target, unreachableCountry.country, 2)
			f.symptom = fmt.Sprintf("Every observed active scored site fails at least %.0f%% of its measured loads from %s", 100*settings.CountryUnreachableShare, unreachableCountry.country)
			f.mechanism = "A shared failed-load pattern does not isolate its cause. Correlated site policy, provider paths, shared route or admission capacity, and the operator's /ip echo remain alternatives; unobserved active sites are not part of this denominator."
			f.baseline = fmt.Sprintf("No country with at least SiteRegionMinSamples=%d runs has all observed active scored sites failing %.0f%% of their measured loads.", siteSettings.SiteRegionMinSamples, 100*settings.CountryUnreachableShare)
			f.observed = fmt.Sprintf("country=%s sites=%d loads=%d failed=%d runs=%d echo_failures=%d coverage=observed-sites-only", unreachableCountry.country, unreachableCountry.sites, unreachableCountry.loads, unreachableCountry.failures, unreachableCountry.runs, unreachableCountry.echoFailures)
			f.evidence = "Only active non-probation sites with positive load tallies in the refresh window are included. Loads and runs are not distinct-provider counts; the country code is bounded and no provider identity is read."
			f.context = "The legacy country-unreachable class names an observed pattern, not universal reachability. Read with §2.19a, but category overlap is not a same-provider or same-attempt join."
			f.action = "Establish coverage and compare bounded site-specific, provider-route, admission and /ip echo controls before changing the pool. Do not mark all sites incompatible or rule out the sites from this aggregate alone."
			f.verify = "Later comparable observed-site loads improve with coverage retained. A country or failing site disappearing from the tally is not proof of recovery."
			failing = append(failing, f)
		}
		emit("egress-country-unreachable", failing)
	}

	// A parsed vector is not evidence of complete fleet or window coverage.
	if !observation.metrics.backfillComplete || !observation.metrics.guardComplete {
		f := egressSitePoolFinding("egress-site-pool-unobservable", target, "metrics", 2)
		f.symptom = "Backfill or guard recovery is unverified because the complete process window is not observable"
		f.mechanism = "Unreadable, empty, partial, stale, restarted or excluded sources cannot supply healthy zeros. Valid positive observations remain visible, but no healthy sentinel resolves their class without complete paired coverage."
		f.baseline = "Every desired API and Taskworker slot has one generation throughout the window, fresh coherent counter/start pairs, no resets, all fixed children and positive answer denominators."
		f.observed = "reason=" + observation.metrics.reason + " " + observation.metrics.coverage
		f.context = "API rank-mode children are lazy: absence is not zero. Idle answers do not establish a healthy backfill share. Source claims and process timestamps do not attest executable ancestry."
		f.action = "Restore bounded Mimir coverage and active services.yml/monitor inventory agreement; let new processes complete the window. Do not clear incidents by treating absent children or excluded hosts as healthy."
		f.verify = "Complete paired same-generation coverage returns; traffic-bearing backfill is below the threshold, and zero guards have a measured passing-class control."
		findings = append(findings, f)
	} else {
		findings = append(findings, healthyFinding(egressSitePoolProbeId, tierWarn, "egress-site-pool-unobservable", target))
	}
	{
		failing := []finding{}
		for _, rankMode := range []string{"quality", "speed"} {
			answered, answeredSeen := observation.metrics.answered[rankMode]
			borrowed, borrowedSeen := observation.metrics.borrowed[rankMode]
			if !answeredSeen || !borrowedSeen || answered <= 0 || borrowed <= settings.BackfillShare*answered {
				continue
			}
			f := egressSitePoolFinding("egress-backfill-sustained", target, rankMode, 2)
			f.symptom = fmt.Sprintf("The observed %s answers borrowed more than half their providers from another bucket over %s", rankMode, settings.BackfillWindow)
			f.mechanism = "Backfill can conceal a thin native bucket in returned answer sizes. This observed ratio is diagnostic; partial exporter coverage cannot establish the whole fleet's ratio or recovery."
			f.baseline = fmt.Sprintf("Borrowed providers at most BackfillShare=%.2f of answered providers per rank mode over %s, with complete coverage before recovery.", settings.BackfillShare, settings.BackfillWindow)
			f.observed = fmt.Sprintf("rank_mode=%s borrowed=%.0f answered=%.0f share=%.3f %s", rankMode, borrowed, answered, borrowed/answered, observation.metrics.coverage)
			f.evidence = "PromQL increases for urnetwork_provider_backfill_sum and urnetwork_provider_answered_total; rounded/extrapolated observations, not exact request counts."
			f.context = "A valid positive subset is retained alongside incomplete-coverage findings. No independent request-intent, target-location, caller or delivered-route join is present."
			f.action = "Inspect exclusion-reason, probe coverage and bucket-index evidence. Do not diagnose supply from answer size alone or clear this finding while Mimir coverage is missing."
			f.verify = "Both rank modes have traffic-bearing complete paired windows and the borrowed shares remain at or below the configured threshold for the resolution cadences."
			failing = append(failing, f)
		}
		if len(failing) > 0 || observation.metrics.backfillComplete {
			emit("egress-backfill-sustained", failing)
		}
	}

	// retry queue starved
	{
		failing := []finding{}
		if 0 < observation.retries.overdueRows {
			f := egressSitePoolFinding("egress-retry-queue-starved", target, "", 2)
			f.symptom = fmt.Sprintf("%d blackhole retries are more than one backoff step past their next_due_at", observation.retries.overdueRows)
			f.mechanism = "A failed check schedules its retry a backoff step out, and the due query serves due rows before first checks. A retry that is not served leaves a failing provider neither confirmed dark nor cleared, and the pool is silently smaller than the checks admit."
			f.baseline = "No retry of an eligible provider is more than one DarkBackoff step past its next_due_at."
			f.observed = fmt.Sprintf("retry_rows=%d overdue_rows=%d max_overdue_seconds=%d", observation.retries.retryRows, observation.retries.overdueRows, observation.retries.maxOverdueSeconds)
			f.evidence = "Aggregate counts over provider_blackhole_check for eligible providers; no provider identity leaves PostgreSQL."
			f.action = "Check blackhole capacity and shard progress (§2.19): a batch guard that keeps tripping, or a lane starved by full runs, both leave retries unserved."
			f.verify = "Overdue retries drain to zero for two cadences."
			failing = append(failing, f)
		}
		emit("egress-retry-queue-starved", failing)
	}

	// protective candidates, not an attribution of the common cause
	{
		failing := []finding{}
		allAbove := true
		passingClass := false
		observed := []string{}
		for _, class := range model.ProviderEgressSiteClasses {
			loads := observation.classLoads[class]
			if loads.total < int64(settings.ProberFaultMinClassLoads) {
				allAbove = false
				observed = append(observed, fmt.Sprintf("%s=%d/%d", class, loads.total-loads.ok, loads.total))
				continue
			}
			share := float64(loads.total-loads.ok) / float64(loads.total)
			if share <= siteSettings.SiteProberFaultShare {
				allAbove = false
				passingClass = true
			}
			observed = append(observed, fmt.Sprintf("%s=%d/%d", class, loads.total-loads.ok, loads.total))
		}
		if allAbove {
			f := egressSitePoolFinding("egress-prober-fault", target, "failure-share", 2)
			f.symptom = fmt.Sprintf("The recorded failed-load share after retries is above %.0f%% in every class", 100*siteSettings.SiteProberFaultShare)
			f.mechanism = "This cross-class pattern is a protective candidate, not proof that the prober is at fault or that destination, provider-path or scoring causes are absent. Refresh policy defers site retirement under this condition; the alert does not attest refresh execution."
			f.baseline = fmt.Sprintf("At least one class under SiteProberFaultShare=%.2f over the last %ds of runs.", siteSettings.SiteProberFaultShare, siteSettings.SiteProberFaultWindowSeconds)
			f.observed = "failed/total " + strings.Join(observed, " ")
			f.evidence = "Class tallies of the health runs measured in the window; bounded classes only."
			f.context = "Recorded class tallies do not establish complete fleet coverage or join a particular provider, batch, destination-pool snapshot or exact running artifact. Read together with §2.19a."
			f.action = "Compare bounded request-profile, host-capacity, route, destination-pool and scoring evidence before assigning a cause or changing the pool. Preserve the protective policy while the owning evidence is unknown."
			f.verify = "Some class falls under the line for an hour."
			failing = append(failing, f)
		}
		if observation.metrics.observable {
			schedules := []string{}
			for schedule, trips := range observation.metrics.guardTrips {
				if 0 < trips && (schedule == "full" || schedule == "blackhole") {
					schedules = append(schedules, schedule)
				}
			}
			sort.Strings(schedules)
			for _, schedule := range schedules {
				f := egressSitePoolFinding("egress-prober-fault", target, "guard-"+schedule, 1)
				f.symptom = fmt.Sprintf("The %s batch guard tripped within the last %s", schedule, settings.GuardTripWindow)
				f.mechanism = "A guard trip is a protective candidate, not proof of a common prober cause. Blackhole policy rewrites ordinary negative checks as not_measured while retaining passing and TLS-authentication evidence; full policy withholds health, location and tally publication and reports guarded attempts. A durable retry still requires successful reporting."
				f.baseline = "No batch guard trips."
				f.observed = fmt.Sprintf("schedule=%s trips=%.0f", schedule, observation.metrics.guardTrips[schedule])
				f.evidence = "urnetwork_egress_probe_batch_guard_trips_total from the taskworkers; the [egress] guard log line carries the share."
				f.context = "The observed counter can be a positive partial subset; it does not identify a same-batch result or prove a shared cause. For full trips, preserve the provider place and scoring snapshot with the exact running artifact before attributing a place-scoring defect. Read together with §2.19a."
				f.action = "Check profile, capacity, route, destination pool and funding/readiness as candidates. For full trips, require a same-batch result, provider place, scoring snapshot and exact running artifact join; neither aggregate overlap nor a local fix establishes live cause. Do not relax TLS or dark-state policy from the trip alone."
				f.verify = "No trip for an hour."
				failing = append(failing, f)
			}
		}
		if len(failing) > 0 || observation.metrics.guardComplete && passingClass {
			emit("egress-prober-fault", failing)
		}
	}
	return findings
}
