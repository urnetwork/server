package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/geo/solve"
	"github.com/urnetwork/server/model"
)

// SIGNALS.md §2.19c maps to signal_derived_locations.go and
// signal_derived_locations_test.go. The derive job (connect/GEOMAP.md §5,
// taskworker/work/derive_location_work.go) places every provider and extender
// from the day's co-signed pings behind publish gates, and a wrong parameter
// does not fail: it places nodes wrong, publishes nothing, or moves every node
// a little every run. The signal reads the job's table, the day of pings, the
// job's pending_task row and the job's own run history in Redis, and reports
// counts, shares and the name of the threshold crossed. It never selects a
// client id, an extender id or a coordinate.

// The signal with the defaults of SIGNALS.md §2.19c.
func NewDerivedLocationsSignal() Signal {
	return NewDerivedLocationsSignalWithSettings(DefaultDerivedLocationsSettings())
}

// The signal with explicit thresholds, for an environment whose population the
// defaults do not fit.
func NewDerivedLocationsSignalWithSettings(settings *DerivedLocationsSettings) Signal {
	return &signalAdapter{
		number: "2.19c", key: "derived-locations", name: "Derived-location health",
		probe: derivedLocationsProbe{settings: settings},
	}
}

// The derive job's task function, as pending_task names it.
const deriveLocationsTaskFunction = "github.com/urnetwork/server/taskworker/work.DeriveLocations"

// The thresholds of SIGNALS.md §2.19c, named as the section names them, each
// with the default the section gives. The ones the section leaves open -- the
// population a share is judged on, and how far the supply may dip before a
// smaller publication is the data's doing -- carry the defaults documented
// beside them there.
type DerivedLocationsSettings struct {
	// not running: the last derivation is older than this, twice the job's
	// cadence (controller.ExtenderPingReportSettings.DeriveInterval)
	DeriveMaxRunAge time.Duration

	// supply gone: fewer co-signed pings in the last complete hour than this,
	// while at least the given extenders are active and providers connected
	DeriveMinCosignedPerHour int
	DeriveSupplyMinExtenders int
	DeriveSupplyMinProviders int

	// the least pings in the last complete hour a share of them is judged on:
	// below it the refusal, verdict and wire shares are noise
	DeriveMinPingSamples int

	// refusals: rejected pings, and rate-limited ones (reason 6), as shares of
	// the hour's verdicts
	DeriveMaxRefusalShare     float64
	DeriveMaxRateLimitedShare float64

	// no verdicts: pings without a verdict, as a share of the hour's pings
	DeriveMaxUnknownShare float64

	// the least nodes a share of them is judged on: published nodes for the
	// crossing, thin-evidence and reputation shares and the previous run of a
	// collapse, sources for the excluded share
	DeriveMinNodeSamples int

	// published collapse: the published count under this share of the
	// previous run's, while the co-signed supply stayed at or above
	// DeriveCollapseSupplyShare of the previous run's -- a day's rolling window
	// wobbles by a few percent between runs, which is not a fall
	DerivePublishedCollapseShare float64
	DeriveCollapseSupplyShare    float64

	// residual not improving: on each of the last DeriveResidualRuns runs the
	// fleet RMS residual at the derived positions is above DeriveMaxResidualKm,
	// or not below the residual at genesis
	DeriveMaxResidualKm float64
	DeriveResidualRuns  int

	// crossings: country and region crossings as shares of published nodes
	DeriveMaxCountryCrossingShare float64
	DeriveMaxRegionCrossingShare  float64

	// exclusions: excluded sources as a share of sources, or the published
	// nodes' median reputation under DeriveMinMedianReputation
	DeriveMaxExcludedShare    float64
	DeriveMinMedianReputation float64

	// non-convergence: the last reputation round hit the sweep cap on this
	// many consecutive runs, or the last run refused more than this share of
	// its solved nodes as still moving when the solve stopped (the solver's
	// PublishMaxLastStepKm gate)
	DeriveCapRuns             int
	DeriveMaxStillMovingShare float64

	// thin evidence: published nodes at exactly MinDerivePeers peers (the
	// job's peer gate) as a share of published nodes
	MinDerivePeers             int
	DeriveMaxThinEvidenceShare float64

	// Capacity: the planner's projection of the next run within
	// DeriveCapacityMargin of the solve's budget on the taskworker host. The
	// budget is the one the run recorded beside its projection, which is the
	// job's own; MaxSolveSeconds and MaxSolveBytes stand in only for a record
	// that carries none.
	DeriveCapacityMargin float64
	MaxSolveSeconds      float64
	MaxSolveBytes        int64

	// Sweep. A derived row older than PingRetention plus DeriveSweepGrace, one
	// sweep interval, is past its cut. network_ping is dropped a whole day
	// partition at a time (GEOMAP §5.7), so a row's age says nothing about the
	// sweep -- a row lives until its whole day goes -- and the partitions
	// themselves are judged instead: a drop is overdue once a partition's
	// upper bound is older than PingPartitionKeepTimeout, the span the sweep
	// keeps a ping past its create time before it drops the partition, plus
	// DeriveSweepGrace for that sweep to run; creation is overdue once no
	// partition covers the day before the last one the sweep keeps ahead
	// (PingPartitionAheadDays), tomorrow at two days ahead. The spans are the
	// ingest's and the ping model's own settings, so the signal and the sweep
	// cannot disagree.
	PingRetention            time.Duration
	DeriveSweepGrace         time.Duration
	PingPartitionKeepTimeout time.Duration
	PingPartitionAheadDays   int

	// clock or wire: pings with a zero round trip, and with a round trip
	// beyond DeriveHalfPlanetRttMs (half the earth's circumference at the
	// solver's km per ms), as shares of the hour's pings
	DeriveMaxZeroRttShare          float64
	DeriveMaxBeyondHalfPlanetShare float64
	DeriveHalfPlanetRttMs          int
}

// The defaults of SIGNALS.md §2.19c. The cadence, the retention, the sweep's
// interval and keep span, the partitions kept ahead, the peer gate and the
// half-planet round trip are the ingest's, the ping model's and the solver's
// own, so the signal follows them when they change; the solve budget is the
// job's default, which every run records beside its projection.
func DefaultDerivedLocationsSettings() *DerivedLocationsSettings {
	solveSettings := solve.DefaultSettings()
	pingReportSettings := controller.DefaultExtenderPingReportSettings()
	return &DerivedLocationsSettings{
		DeriveMaxRunAge: 2 * pingReportSettings.DeriveInterval,

		DeriveMinCosignedPerHour: 100,
		DeriveSupplyMinExtenders: 1,
		DeriveSupplyMinProviders: 1,

		DeriveMinPingSamples: 100,

		DeriveMaxRefusalShare:     0.10,
		DeriveMaxRateLimitedShare: 0.05,

		DeriveMaxUnknownShare: 0.30,

		DeriveMinNodeSamples: 20,

		DerivePublishedCollapseShare: 0.5,
		DeriveCollapseSupplyShare:    0.9,

		DeriveMaxResidualKm: 50,
		DeriveResidualRuns:  2,

		DeriveMaxCountryCrossingShare: 0.02,
		DeriveMaxRegionCrossingShare:  0.10,

		DeriveMaxExcludedShare:    0.10,
		DeriveMinMedianReputation: 0.5,

		DeriveCapRuns:             3,
		DeriveMaxStillMovingShare: 0.05,

		MinDerivePeers:             solveSettings.MinDerivePeers,
		DeriveMaxThinEvidenceShare: 0.30,

		DeriveCapacityMargin: 0.2,
		MaxSolveSeconds:      600,
		MaxSolveBytes:        8 * 1024 * 1024 * 1024,

		PingRetention:            pingReportSettings.Retention,
		DeriveSweepGrace:         pingReportSettings.SweepTimeout,
		PingPartitionKeepTimeout: pingReportSettings.KeepTimeout(),
		PingPartitionAheadDays:   model.DefaultNetworkPingPartitionSettings().AheadDays,

		DeriveMaxZeroRttShare:          0.01,
		DeriveMaxBeyondHalfPlanetShare: 0.05,
		DeriveHalfPlanetRttMs:          model.DefaultNetworkPingTallySettings().HalfPlanetRttMs,
	}
}

// The probe of the signal, with its thresholds.
type derivedLocationsProbe struct {
	settings *DerivedLocationsSettings
}

// Implements probe.
func (self derivedLocationsProbe) id() string { return "pg/derived-locations" }

// Implements probe.
func (self derivedLocationsProbe) tier() string { return tierWarn }

// Implements probe.
func (self derivedLocationsProbe) cadence() time.Duration { return 15 * time.Minute }

// Every finding is about the derive phase as a whole; the class names the
// condition and the frame its part.
const derivedLocationsTarget = "derive-locations"

// The published table (§2.19c's first query), with the sweep's cut.
type derivedLocationsTable struct {
	published        int64
	crossedRegion    int64
	crossedCountry   int64
	atMinPeers       int64
	medianResidualKm float64
	medianReputation float64
	// -1 while the table is empty
	lastUpdateAgeSeconds int64
	expired              int64
}

// One hour of pings (§2.19c's second query).
type derivedLocationsPingHour struct {
	cosigned         int64
	rejected         int64
	rateLimited      int64
	unknown          int64
	relayed          int64
	zeroRtt          int64
	beyondHalfPlanet int64
}

// Every ping of the hour: co-signed, rejected, or without a verdict.
func (self derivedLocationsPingHour) pings() int64 {
	return self.cosigned + self.rejected + self.unknown
}

// The pings of the hour a target gave a verdict on.
func (self derivedLocationsPingHour) verdicts() int64 {
	return self.cosigned + self.rejected
}

// Adds another hour's counts.
func (self *derivedLocationsPingHour) add(other derivedLocationsPingHour) {
	self.cosigned += other.cosigned
	self.rejected += other.rejected
	self.rateLimited += other.rateLimited
	self.unknown += other.unknown
	self.relayed += other.relayed
	self.zeroRtt += other.zeroRtt
	self.beyondHalfPlanet += other.beyondHalfPlanet
}

// The day of pings, by hour.
type derivedLocationsPings struct {
	// the last complete clock hour, zero when it had no ping
	lastHour derivedLocationsPingHour
	// the day, and the hours of it that had a ping
	day      derivedLocationsPingHour
	dayHours int
}

// The job's pending_task row, and the population the ping supply is expected
// from.
type derivedLocationsState struct {
	taskRows           int64
	failingTaskRows    int64
	maxRescheduleCount int64
	// bounded at the settings' minimums: "at least this many"
	activeExtenders    int64
	connectedProviders int64
}

// The day partitions of network_ping, as the sweep keeps them.
type derivedLocationsPartitions struct {
	// whether network_ping is a partitioned table at all; before the
	// conversion migration it is not, and there is nothing to judge
	partitioned bool
	partitions  int64
	// the sweep's partitions whose upper bound is past the drop cut, and the
	// age of the oldest of those bounds, -1 when there is none
	overdueDrops                 int64
	oldestOverdueUpperAgeSeconds int64
	// whether a partition covers the day the creation check reads
	checkedDayCovered bool
}

// The job's run history in Redis, newest first.
type derivedLocationsHistory struct {
	observable bool
	// why it could not be read, a fixed reason
	reason    string
	malformed int
	runs      []*model.DeriveLocationsRun
}

// Implements probe: the four queries and the run history, judged together.
func (self derivedLocationsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	if settings == nil {
		settings = DefaultDerivedLocationsSettings()
	}
	table, err := queryDerivedLocationsTable(ctx, env, settings)
	if err != nil {
		return nil, err
	}
	pings, err := queryDerivedLocationsPings(ctx, env, settings)
	if err != nil {
		return nil, err
	}
	state, err := queryDerivedLocationsState(ctx, env, settings)
	if err != nil {
		return nil, err
	}
	partitions, err := queryDerivedLocationsPartitions(ctx, env, settings)
	if err != nil {
		return nil, err
	}
	// the runs the comparisons across runs need
	historyDepth := max(2, settings.DeriveResidualRuns, settings.DeriveCapRuns)
	history := readDerivedLocationsHistory(ctx, env, historyDepth)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return evaluateDerivedLocations(settings, env.now(), table, pings, state, partitions, history), nil
}

// The sweep's cuts, in seconds before now: a derived row past the first is
// past its retention and a sweep interval; a partition whose upper bound is
// past the second is past the span the sweep keeps it and a sweep interval
// more, so the sweep should have dropped it. The day the creation check reads,
// after today: the last day the sweep keeps ahead, less one, so the hour after
// midnight before the sweep adds the new last day reads as nothing missing.
func derivedLocationsSweepCutSeconds(settings *DerivedLocationsSettings) (derivedLocationCutSeconds int64, partitionDropCutSeconds int64, checkedAheadDays int) {
	derivedLocationCutSeconds = int64((settings.PingRetention + settings.DeriveSweepGrace) / time.Second)
	partitionDropCutSeconds = int64((settings.PingPartitionKeepTimeout + settings.DeriveSweepGrace) / time.Second)
	checkedAheadDays = max(0, settings.PingPartitionAheadDays-1)
	return derivedLocationCutSeconds, partitionDropCutSeconds, checkedAheadDays
}

// The first query of §2.19c: the published table's counts, medians and age.
func derivedLocationsTableQuery(settings *DerivedLocationsSettings) string {
	derivedLocationCutSeconds, _, _ := derivedLocationsSweepCutSeconds(settings)
	return fmt.Sprintf(`
/* monitor-signal-2.19c-derived-locations-table */
SELECT count(*) AS published,
       count(*) FILTER (WHERE crossed_region) AS crossed_region,
       count(*) FILTER (WHERE crossed_country) AS crossed_country,
       count(*) FILTER (WHERE peer_count <= %d) AS at_min_peers,
       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY residual_km), -1) AS median_residual_km,
       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY reputation), -1) AS median_reputation,
       COALESCE(round(extract(epoch FROM (now() AT TIME ZONE 'utc') - max(update_time)))::bigint, -1) AS last_update_age_seconds,
       count(*) FILTER (WHERE update_time < (now() AT TIME ZONE 'utc') - interval '%d seconds') AS expired
FROM derived_location;
`, settings.MinDerivePeers, derivedLocationCutSeconds)
}

// Runs and parses the first query; a malformed row is an error.
func queryDerivedLocationsTable(ctx context.Context, env *probeEnv, settings *DerivedLocationsSettings) (derivedLocationsTable, error) {
	rows, err := env.runner.pg(ctx, derivedLocationsTableQuery(settings))
	if err != nil {
		return derivedLocationsTable{}, err
	}
	row, err := pgAggregateRow(rows, 8)
	if err != nil {
		return derivedLocationsTable{}, fmt.Errorf("derived-location table: %w", err)
	}
	integers := [6]int64{}
	for i, column := range []int{0, 1, 2, 3, 6, 7} {
		value, err := parseStrictInt64(row.str(column))
		if err != nil {
			return derivedLocationsTable{}, fmt.Errorf("derived-location table column %d: %w", column, err)
		}
		integers[i] = value
	}
	medianResidualKm, err := strconv.ParseFloat(row.str(4), 64)
	if err != nil {
		return derivedLocationsTable{}, fmt.Errorf("derived-location table median residual: %w", err)
	}
	medianReputation, err := strconv.ParseFloat(row.str(5), 64)
	if err != nil {
		return derivedLocationsTable{}, fmt.Errorf("derived-location table median reputation: %w", err)
	}
	return derivedLocationsTable{
		published:            integers[0],
		crossedRegion:        integers[1],
		crossedCountry:       integers[2],
		atMinPeers:           integers[3],
		medianResidualKm:     medianResidualKm,
		medianReputation:     medianReputation,
		lastUpdateAgeSeconds: integers[4],
		expired:              integers[5],
	}, nil
}

// The second query of §2.19c: the day of pings by hour, from the fleet's hour
// tally the ingest keeps (GEOMAP §2.7), never the rows -- at the target a
// day's partition of network_ping is 275 GiB. The day is the 24 clock hours up
// to and including the current one, and the beyond-half-planet count is the
// tally's own, taken at the ingest's threshold, which DeriveHalfPlanetRttMs
// follows by default.
func derivedLocationsPingsQuery(settings *DerivedLocationsSettings) string {
	return fmt.Sprintf(`
/* monitor-signal-2.19c-derived-locations-pings */
SELECT hour,
       hour = date_trunc('hour', now() AT TIME ZONE 'utc') - interval '1 hour' AS last_complete_hour,
       COALESCE(sum(ping_count) FILTER (WHERE cosign = %d), 0)::bigint AS cosigned,
       COALESCE(sum(ping_count) FILTER (WHERE cosign = %d), 0)::bigint AS rejected,
       COALESCE(sum(ping_count) FILTER (WHERE cosign = %d AND cosign_reason = %d), 0)::bigint AS rate_limited,
       COALESCE(sum(ping_count) FILTER (WHERE cosign = %d), 0)::bigint AS unknown,
       COALESCE(sum(ping_count) FILTER (WHERE relayed), 0)::bigint AS relayed,
       sum(zero_rtt_count)::bigint AS zero_rtt,
       sum(beyond_half_planet_count)::bigint AS beyond_half_planet
FROM network_ping_hour_tally
WHERE date_trunc('hour', now() AT TIME ZONE 'utc') - interval '23 hours' <= hour
GROUP BY hour ORDER BY hour;
`,
		model.NetworkPingCosignCosigned,
		model.NetworkPingCosignRejected,
		model.NetworkPingCosignRejected,
		connect.ExtenderProbeVerdictReasonRateLimited,
		model.NetworkPingCosignUnknown,
	)
}

// Runs and parses the second query; a malformed row, or two rows claiming the
// last complete hour, is an error.
func queryDerivedLocationsPings(ctx context.Context, env *probeEnv, settings *DerivedLocationsSettings) (derivedLocationsPings, error) {
	rows, err := env.runner.pg(ctx, derivedLocationsPingsQuery(settings))
	if err != nil {
		return derivedLocationsPings{}, err
	}
	pings := derivedLocationsPings{}
	lastHours := 0
	for _, row := range rows {
		if len(row) != 9 {
			return derivedLocationsPings{}, fmt.Errorf("derived-location pings: expected 9 columns, got %d", len(row))
		}
		values := [7]int64{}
		for i := range values {
			value, err := parseStrictInt64(row.str(2 + i))
			if err != nil {
				return derivedLocationsPings{}, fmt.Errorf("derived-location pings column %d: %w", 2+i, err)
			}
			values[i] = value
		}
		hour := derivedLocationsPingHour{
			cosigned:         values[0],
			rejected:         values[1],
			rateLimited:      values[2],
			unknown:          values[3],
			relayed:          values[4],
			zeroRtt:          values[5],
			beyondHalfPlanet: values[6],
		}
		pings.day.add(hour)
		pings.dayHours += 1
		if migrationBool(row.str(1)) {
			lastHours += 1
			pings.lastHour = hour
		}
	}
	if 1 < lastHours {
		return derivedLocationsPings{}, fmt.Errorf("derived-location pings: %d rows claim the last complete hour", lastHours)
	}
	return pings, nil
}

// The job's pending_task row, and the population the supply is expected from,
// bounded at the settings' minimums.
func derivedLocationsStateQuery(settings *DerivedLocationsSettings) string {
	return fmt.Sprintf(`
/* monitor-signal-2.19c-derived-locations-state */
SELECT
 (SELECT count(*) FROM pending_task WHERE function_name = '%[1]s'),
 (SELECT count(*) FROM pending_task WHERE function_name = '%[1]s' AND reschedule_error_count > 0),
 (SELECT COALESCE(max(reschedule_error_count), 0) FROM pending_task WHERE function_name = '%[1]s'),
 (SELECT count(*) FROM (SELECT 1 FROM network_extender WHERE active LIMIT %[2]d) AS active_extender),
 (SELECT count(*) FROM (
   SELECT 1 FROM network_client_connection ncc
   WHERE ncc.connected AND EXISTS (
    SELECT 1 FROM provide_key pk
    WHERE pk.client_id = ncc.client_id AND pk.provide_mode = %[3]d
   )
   LIMIT %[4]d
  ) AS connected_provider);
`,
		deriveLocationsTaskFunction,
		max(1, settings.DeriveSupplyMinExtenders),
		model.ProvideModePublic,
		max(1, settings.DeriveSupplyMinProviders),
	)
}

// The day partitions of network_ping, from the catalog: whether the table is
// partitioned, how many partitions it has, the partitions named as the sweep
// names its own (network_ping_pYYYYMMDD) whose upper bound is past the drop
// cut, and whether a partition covers the day the creation check reads. A
// bound is read back from pg_get_expr's text and cast in the same session, as
// the sweep reads it; a bound that is not a finite timestamp -- an unbounded
// end, a default partition -- covers no day. A partition under another name is the
// migration signal's (§8.9), never dropped by the sweep, so it is never an
// overdue drop here. Counts only: no partition name leaves the database.
func derivedLocationsPartitionsQuery(settings *DerivedLocationsSettings) string {
	_, partitionDropCutSeconds, checkedAheadDays := derivedLocationsSweepCutSeconds(settings)
	return fmt.Sprintf(`
/* monitor-signal-2.19c-derived-locations-partitions */
WITH partition_bound AS (
  SELECT partition_relation.relname AS name,
         regexp_match(
           pg_get_expr(partition_relation.relpartbound, partition_relation.oid),
           '^FOR VALUES FROM \(''([^'']*)''\) TO \(''([^'']*)''\)$'
         ) AS bounds
  FROM pg_inherits AS inheritance
  JOIN pg_class AS partition_relation ON partition_relation.oid = inheritance.inhrelid
  WHERE inheritance.inhparent = to_regclass('public.network_ping')
),
finite_bound AS (
  SELECT name,
         CASE WHEN isfinite(bounds[1]::timestamp) THEN bounds[1]::timestamp END AS lower_bound,
         CASE WHEN isfinite(bounds[2]::timestamp) THEN bounds[2]::timestamp END AS upper_bound
  FROM partition_bound
  WHERE bounds IS NOT NULL
),
overdue AS (
  SELECT upper_bound
  FROM finite_bound
  WHERE name ~ '^network_ping_p[0-9]{8}$' AND
        upper_bound < (now() AT TIME ZONE 'utc') - interval '%[1]d seconds'
),
checked_day AS (
  SELECT date_trunc('day', now() AT TIME ZONE 'utc') + interval '%[2]d days' AS day
)
SELECT
 COALESCE((SELECT relkind = 'p' FROM pg_class WHERE oid = to_regclass('public.network_ping')), false) AS partitioned,
 (SELECT count(*) FROM partition_bound) AS partitions,
 (SELECT count(*) FROM overdue) AS overdue_drops,
 COALESCE((SELECT round(extract(epoch FROM (now() AT TIME ZONE 'utc') - min(upper_bound)))::bigint FROM overdue), -1) AS oldest_overdue_upper_age_seconds,
 EXISTS (
  SELECT 1 FROM finite_bound, checked_day
  WHERE lower_bound < checked_day.day + interval '1 day' AND checked_day.day < upper_bound
 ) AS checked_day_covered;
`, partitionDropCutSeconds, checkedAheadDays)
}

// Runs and parses the partition query; a malformed row is an error.
func queryDerivedLocationsPartitions(ctx context.Context, env *probeEnv, settings *DerivedLocationsSettings) (derivedLocationsPartitions, error) {
	rows, err := env.runner.pg(ctx, derivedLocationsPartitionsQuery(settings))
	if err != nil {
		return derivedLocationsPartitions{}, err
	}
	row, err := pgAggregateRow(rows, 5)
	if err != nil {
		return derivedLocationsPartitions{}, fmt.Errorf("derived-location partitions: %w", err)
	}
	values := [3]int64{}
	for i, column := range []int{1, 2, 3} {
		value, err := parseStrictInt64(row.str(column))
		if err != nil {
			return derivedLocationsPartitions{}, fmt.Errorf("derived-location partitions column %d: %w", column, err)
		}
		values[i] = value
	}
	return derivedLocationsPartitions{
		partitioned:                  migrationBool(row.str(0)),
		partitions:                   values[0],
		overdueDrops:                 values[1],
		oldestOverdueUpperAgeSeconds: values[2],
		checkedDayCovered:            migrationBool(row.str(4)),
	}, nil
}

// Runs and parses the state query; a malformed row is an error.
func queryDerivedLocationsState(ctx context.Context, env *probeEnv, settings *DerivedLocationsSettings) (derivedLocationsState, error) {
	rows, err := env.runner.pg(ctx, derivedLocationsStateQuery(settings))
	if err != nil {
		return derivedLocationsState{}, err
	}
	row, err := pgAggregateRow(rows, 5)
	if err != nil {
		return derivedLocationsState{}, fmt.Errorf("derived-location state: %w", err)
	}
	values := [5]int64{}
	for i := range values {
		value, err := parseStrictInt64(row.str(i))
		if err != nil {
			return derivedLocationsState{}, fmt.Errorf("derived-location state column %d: %w", i, err)
		}
		values[i] = value
	}
	return derivedLocationsState{
		taskRows:           values[0],
		failingTaskRows:    values[1],
		maxRescheduleCount: values[2],
		activeExtenders:    values[3],
		connectedProviders: values[4],
	}, nil
}

// Reads the newest `depth` runs the job recorded
// (model.DeriveLocationsRunsRedisKey). A history that cannot be read is
// unobservable rather than empty: the comparisons across runs then report
// nothing, neither a fault nor health.
func readDerivedLocationsHistory(ctx context.Context, env *probeEnv, depth int) derivedLocationsHistory {
	redisHost := env.cfg.hostByRole("redis-cluster")
	if redisHost == nil {
		return derivedLocationsHistory{reason: "no-redis-cluster-host"}
	}
	out, err := env.runner.redis(
		ctx, redisHost, redisHost.redisEntryPort,
		"-c", "--raw", "LRANGE", model.DeriveLocationsRunsRedisKey, "0", strconv.Itoa(depth-1),
	)
	if err != nil {
		return derivedLocationsHistory{reason: "redis-read-failed"}
	}
	history := derivedLocationsHistory{observable: true}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		run := &model.DeriveLocationsRun{}
		if json.Unmarshal([]byte(line), run) != nil || run.RunTime.IsZero() {
			history.malformed += 1
			continue
		}
		history.runs = append(history.runs, run)
	}
	if 0 < history.malformed {
		// a record the signal cannot read may sit between two it can, and a
		// comparison across it would compare runs that are not consecutive
		return derivedLocationsHistory{reason: "malformed-run-record", malformed: history.malformed}
	}
	return history
}

// Why a run's solved nodes were not published, by the first gate each failed
// (§5.4) and then the job's own gates, as a finding's context carries it.
func derivedLocationsRefusals(run *model.DeriveLocationsRun) string {
	return fmt.Sprintf(
		"refused: few_pings=%d few_peers=%d still_moving=%d no_improvement=%d probe_country=%d unmapped=%d",
		run.RefusedFewPings,
		run.RefusedFewPeers,
		run.RefusedStillMoving,
		run.RefusedNoImprovement,
		run.RefusedProbeCountry,
		run.Unmapped,
	)
}

// Part over whole, 0 for an empty whole.
func derivedLocationsShare(part int64, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

// The part of a finding every condition shares.
func derivedLocationsFinding(class string, frame string, sustain int) finding {
	return finding{
		probeId: "pg/derived-locations", tier: tierWarn,
		class: class, target: derivedLocationsTarget, frame: frame, sustain: sustain,
		playbook: "SIGNALS.md §2.19c",
	}
}

// Judges the thirteen conditions of §2.19c. Each condition emits its failing
// parts, or one healthy finding when none fails, or nothing when its evidence
// is unobservable: a healthy finding resolves every open part of its class, so
// it must never stand in for "unknown".
func evaluateDerivedLocations(
	settings *DerivedLocationsSettings,
	now time.Time,
	table derivedLocationsTable,
	pings derivedLocationsPings,
	state derivedLocationsState,
	partitions derivedLocationsPartitions,
	history derivedLocationsHistory,
) []finding {
	findings := []finding{}
	emit := func(class string, failing []finding) {
		if 0 < len(failing) {
			findings = append(findings, failing...)
			return
		}
		findings = append(findings, healthyFinding("pg/derived-locations", tierWarn, class, derivedLocationsTarget))
	}

	if history.observable {
		findings = append(findings, healthyFinding("pg/derived-locations", tierWarn, "derive-run-history-unobservable", derivedLocationsTarget))
	} else {
		f := derivedLocationsFinding("derive-run-history-unobservable", "", 2)
		f.symptom = "The derive job's run history cannot be read, so the comparisons across runs are not evaluated"
		f.mechanism = "The job records every derivation at the head of a Redis list (model.DeriveLocationsRunsRedisKey). The last-run age, the published collapse, the residual across runs, the exclusions share and the sweep-cap streak are read from it; without it those conditions report neither a fault nor health."
		f.baseline = "The history is readable, with the newest run at its head; an empty history is normal until the first derivation, eight hours after deploy."
		f.observed = fmt.Sprintf("reason=%s malformed_records=%d", history.reason, history.malformed)
		f.action = "Check the Redis cluster the application writes (§1.4) and the redis-cluster host in the monitor inventory. A malformed record means a writer other than the derive job, or a changed record shape: compare it with model.DeriveLocationsRun before trusting any run comparison."
		f.verify = "The next run of this signal reads the history and evaluates every condition."
		findings = append(findings, f)
	}

	// not running
	{
		failing := []finding{}
		if state.taskRows == 0 {
			f := derivedLocationsFinding("derive-not-running", "task-missing", 2)
			f.symptom = "The derive job has no pending_task row, so no derivation will run"
			f.mechanism = "DeriveLocations is a RunOnce chain whose Post schedules the next run. Without a row the chain is lost: the published rows expire a day after the last run and every node falls back to its genesis."
			f.baseline = "Exactly one pending_task row for " + deriveLocationsTaskFunction + ", without a reschedule error."
			f.observed = "pending_task_rows=0 threshold=one_pending_task_row"
			f.action = "Confirm the taskworker registers DeriveLocations and InitTasks arms it (a restart of a taskworker from this source re-arms the chain). Do not insert a row by hand."
			f.verify = "One DeriveLocations row exists and a derivation is recorded within the job's cadence."
			failing = append(failing, f)
		}
		if 0 < state.failingTaskRows {
			f := derivedLocationsFinding("derive-not-running", "task-parked", 2)
			f.symptom = "The derive job's pending_task row is failing and parked on its error backoff"
			f.mechanism = "A derivation that raises is rescheduled with exponential backoff, so it retries late and the published rows age toward the sweep."
			f.baseline = "The DeriveLocations row carries no reschedule error."
			f.observed = fmt.Sprintf("pending_task_rows=%d failing_rows=%d max_reschedule_error_count=%d threshold=no_reschedule_error", state.taskRows, state.failingTaskRows, state.maxRescheduleCount)
			f.action = "Read the DeriveLocations reschedule error and the taskworker's [derive] log lines; repair the failing dependency (database, Redis, the place list) and let the chain retry."
			f.verify = "The row's reschedule error clears and a derivation is recorded."
			failing = append(failing, f)
		}
		lastRunAgeSeconds := int64(-1)
		lastRunSource := ""
		switch {
		case history.observable && 0 < len(history.runs):
			lastRunAgeSeconds = int64(now.Sub(history.runs[0].RunTime) / time.Second)
			lastRunSource = "run_history"
		case 0 <= table.lastUpdateAgeSeconds:
			// a history that was lost with the rows still present
			lastRunAgeSeconds = table.lastUpdateAgeSeconds
			lastRunSource = "table"
		}
		maxRunAgeSeconds := int64(settings.DeriveMaxRunAge / time.Second)
		if maxRunAgeSeconds < lastRunAgeSeconds {
			f := derivedLocationsFinding("derive-not-running", "stale-run", 2)
			f.symptom = "The last derivation is older than twice the derive job's cadence"
			f.mechanism = "Derivations are due every eight hours. Past two cadences the published rows are no longer being renewed and expire a day after the last run, so nodes fall back to genesis one by one without any error."
			f.baseline = fmt.Sprintf("A derivation at most DeriveMaxRunAge=%s old.", settings.DeriveMaxRunAge)
			f.observed = fmt.Sprintf("last_run_age_seconds=%d source=%s threshold=DeriveMaxRunAge=%ds", lastRunAgeSeconds, lastRunSource, maxRunAgeSeconds)
			f.action = "Check the DeriveLocations pending_task row (claimed, overdue, failing) and the taskworker's [derive] log lines; a derivation that exceeds its max time is cancelled and retried."
			f.verify = "A new derivation is recorded and its age falls under the threshold."
			failing = append(failing, f)
		}
		emit("derive-not-running", failing)
	}

	// supply gone
	{
		failing := []finding{}
		expected := int64(settings.DeriveSupplyMinExtenders) <= state.activeExtenders &&
			int64(settings.DeriveSupplyMinProviders) <= state.connectedProviders
		if expected && pings.lastHour.cosigned < int64(settings.DeriveMinCosignedPerHour) {
			f := derivedLocationsFinding("derive-supply-gone", "", 1)
			f.symptom = "Co-signed pings stopped arriving while providers and extenders are connected"
			f.mechanism = "Every derivation is solved on the day's co-signed pings. With the pingers, their reporters or the ingest stopped, the next derivations solve on less and less and publish fewer nodes."
			f.baseline = fmt.Sprintf("At least DeriveMinCosignedPerHour=%d co-signed pings in the last complete hour while at least %d extender(s) are active and %d provider(s) connected.", settings.DeriveMinCosignedPerHour, settings.DeriveSupplyMinExtenders, settings.DeriveSupplyMinProviders)
			f.observed = fmt.Sprintf("cosigned_last_hour=%d threshold=DeriveMinCosignedPerHour=%d active_extenders_at_least=%d connected_providers_at_least=%d pings_last_hour=%d", pings.lastHour.cosigned, settings.DeriveMinCosignedPerHour, state.activeExtenders, state.connectedProviders, pings.lastHour.pings())
			f.context = fmt.Sprintf("day: cosigned=%d rejected=%d unknown=%d hours_with_pings=%d", pings.day.cosigned, pings.day.rejected, pings.day.unknown, pings.dayHours)
			f.action = "Follow the ping path: the extenders' peer pingers and the providers' probe passes (connect), the pingers' reporters, and POST /network/ping-report on the api; a refusal or verdict finding beside this one narrows it."
			f.verify = "The co-signed count of the next complete hour returns above the threshold."
			failing = append(failing, f)
		}
		emit("derive-supply-gone", failing)
	}

	pingSamples := int64(settings.DeriveMinPingSamples)

	// refusals
	{
		failing := []finding{}
		verdicts := pings.lastHour.verdicts()
		if pingSamples <= verdicts {
			if share := derivedLocationsShare(pings.lastHour.rejected, verdicts); settings.DeriveMaxRefusalShare < share {
				f := derivedLocationsFinding("derive-refusals", "rejected", 1)
				f.symptom = "Targets refuse an unusual share of ping claims"
				f.mechanism = "A refusal is never a measurement. A high share is targets refusing honest claims -- the gate tolerance, clock skew -- or pingers under-claiming; either way the solve loses the measurements and the refusal rates skew every source's reputation."
				f.baseline = fmt.Sprintf("Rejected pings at most DeriveMaxRefusalShare=%.2f of the last complete hour's verdicts.", settings.DeriveMaxRefusalShare)
				f.observed = fmt.Sprintf("rejected_last_hour=%d verdicts_last_hour=%d share=%.4f threshold=DeriveMaxRefusalShare=%.2f", pings.lastHour.rejected, verdicts, share, settings.DeriveMaxRefusalShare)
				f.action = "Read the refusal reasons on the extenders dashboard: rtt below observed is the gate tolerance or a clock, nonce and wrong extender are the probe path, a bad signature is a key mismatch. Fix the path, not the threshold."
				f.verify = "The next complete hour's rejected share falls under the threshold."
				failing = append(failing, f)
			}
			if share := derivedLocationsShare(pings.lastHour.rateLimited, verdicts); settings.DeriveMaxRateLimitedShare < share {
				f := derivedLocationsFinding("derive-refusals", "rate-limited", 1)
				f.symptom = "Targets refuse an unusual share of ping claims for rate"
				f.mechanism = "A probe refused for rate (reason 6) is not evidence against anyone, but it is a lost measurement: the extenders' admission limits (connect/EXTENDER.md A12) are set too tight for the ping cadence, or an NLayer front's shared address is being limited."
				f.baseline = fmt.Sprintf("Rate-limited refusals at most DeriveMaxRateLimitedShare=%.2f of the last complete hour's verdicts.", settings.DeriveMaxRateLimitedShare)
				f.observed = fmt.Sprintf("rate_limited_last_hour=%d verdicts_last_hour=%d share=%.4f threshold=DeriveMaxRateLimitedShare=%.2f relayed_last_hour=%d", pings.lastHour.rateLimited, verdicts, share, settings.DeriveMaxRateLimitedShare, pings.lastHour.relayed)
				f.action = "Compare the extenders' per-source probe limit with the peer ping spread and refresh cadence; a share concentrated on relayed pings is the fronts' shared address."
				f.verify = "The next complete hour's rate-limited share falls under the threshold."
				failing = append(failing, f)
			}
		}
		emit("derive-refusals", failing)
	}

	// no verdicts
	{
		failing := []finding{}
		all := pings.lastHour.pings()
		if pingSamples <= all {
			if share := derivedLocationsShare(pings.lastHour.unknown, all); settings.DeriveMaxUnknownShare < share {
				f := derivedLocationsFinding("derive-no-verdicts", "", 1)
				f.symptom = "An unusual share of pings end without a verdict from their target"
				f.mechanism = "A ping with no verdict is neither a measurement nor a refusal. A high share is extenders on a binary that predates the verdict frame, or the verdict lost on a carrier."
				f.baseline = fmt.Sprintf("Pings without a verdict at most DeriveMaxUnknownShare=%.2f of the last complete hour's pings.", settings.DeriveMaxUnknownShare)
				f.observed = fmt.Sprintf("unknown_last_hour=%d pings_last_hour=%d share=%.4f threshold=DeriveMaxUnknownShare=%.2f", pings.lastHour.unknown, all, share, settings.DeriveMaxUnknownShare)
				f.action = "Check the extender binary rollout and which carriers the unanswered pings used; an old binary is upgraded, a carrier that drops the verdict frame is a transport bug."
				f.verify = "The next complete hour's unknown share falls under the threshold."
				failing = append(failing, f)
			}
		}
		emit("derive-no-verdicts", failing)
	}

	// published collapse
	if history.observable {
		failing := []finding{}
		if 2 <= len(history.runs) {
			current := history.runs[0]
			previous := history.runs[1]
			collapsed := int64(settings.DeriveMinNodeSamples) <= int64(previous.Published) &&
				float64(current.Published) < settings.DerivePublishedCollapseShare*float64(previous.Published)
			supplyHeld := settings.DeriveCollapseSupplyShare*float64(previous.CosignedPings) <= float64(current.CosignedPings)
			if collapsed && supplyHeld {
				f := derivedLocationsFinding("derive-published-collapse", "", 1)
				f.symptom = "The last derivation published under half as many nodes as the one before, on as many pings"
				f.mechanism = "With the co-signed supply held, a fall in the published count is the publish gates or the solve, not the data: a changed gate, a calibration that stopped improving on genesis, or a solve that no longer converges."
				f.baseline = fmt.Sprintf("Published at least DerivePublishedCollapseShare=%.2f of the previous run's while the supply stays at or above DeriveCollapseSupplyShare=%.2f of the previous run's.", settings.DerivePublishedCollapseShare, settings.DeriveCollapseSupplyShare)
				f.observed = fmt.Sprintf("published=%d previous_published=%d share=%.4f threshold=DerivePublishedCollapseShare=%.2f cosigned_pings=%d previous_cosigned_pings=%d", current.Published, previous.Published, derivedLocationsShare(int64(current.Published), int64(previous.Published)), settings.DerivePublishedCollapseShare, current.CosignedPings, previous.CosignedPings)
				f.context = fmt.Sprintf("last run %s; previous run %s", derivedLocationsRefusals(current), derivedLocationsRefusals(previous))
				f.action = "Read the two runs' [derive] log lines: the residual against genesis, the excluded sources, the unmapped and probe-country counts. Change one DeriveSettings value at a time and wait one run."
				f.verify = "The next derivation publishes at least the threshold share of the run before the collapse."
				failing = append(failing, f)
			}
		}
		emit("derive-published-collapse", failing)
	}

	// residual not improving
	if history.observable {
		failing := []finding{}
		runs := max(1, settings.DeriveResidualRuns)
		if runs <= len(history.runs) {
			notImproving := true
			parts := []string{}
			for _, run := range history.runs[:runs] {
				aboveMax := settings.DeriveMaxResidualKm < run.ResidualKm
				notBelowGenesis := run.GenesisResidualKm <= run.ResidualKm
				// a run with no terms has no residual to judge; that is the
				// supply's finding
				if run.Terms <= 0 || (!aboveMax && !notBelowGenesis) {
					notImproving = false
				}
				parts = append(parts, fmt.Sprintf("%.2f/%.2f", run.ResidualKm, run.GenesisResidualKm))
			}
			if notImproving {
				f := derivedLocationsFinding("derive-residual-not-improving", "", 1)
				f.symptom = fmt.Sprintf("The derived positions do not explain the pings on %d consecutive runs", runs)
				f.mechanism = "The fleet RMS residual at the derived positions is above its ceiling or no better than genesis: the km per ms slope, the overhead or the whole-millisecond wire (GEOMAP §9) are off, and the corrections are noise."
				f.baseline = fmt.Sprintf("On each run the residual is at most DeriveMaxResidualKm=%.0f km and below the residual at genesis.", settings.DeriveMaxResidualKm)
				f.observed = fmt.Sprintf("residual_km/genesis_residual_km newest_first=%s threshold=DeriveMaxResidualKm=%.0f runs=DeriveResidualRuns=%d", strings.Join(parts, ","), settings.DeriveMaxResidualKm, runs)
				f.action = "Calibrate: change one of KmPerMs, OverheadMs or the aggregate in DeriveSettings, wait one run, and read the residual panel."
				f.verify = "The next run's residual falls under the ceiling and below the residual at genesis."
				failing = append(failing, f)
			}
		}
		emit("derive-residual-not-improving", failing)
	}

	nodeSamples := int64(settings.DeriveMinNodeSamples)

	// crossings
	{
		failing := []finding{}
		if nodeSamples <= table.published {
			if share := derivedLocationsShare(table.crossedCountry, table.published); settings.DeriveMaxCountryCrossingShare < share {
				f := derivedLocationsFinding("derive-crossings", "country", 1)
				f.symptom = "An unusual share of published nodes map outside the country their genesis placed them in"
				f.mechanism = "The country containment term prices every crossing heavily, so a surviving crossing is the pings insisting: a wrong genesis source, a bad slope, or a colluding cluster. These nodes are published in the derived country."
				f.baseline = fmt.Sprintf("Country crossings at most DeriveMaxCountryCrossingShare=%.2f of published nodes.", settings.DeriveMaxCountryCrossingShare)
				f.observed = fmt.Sprintf("crossed_country=%d published=%d share=%.4f threshold=DeriveMaxCountryCrossingShare=%.2f", table.crossedCountry, table.published, share, settings.DeriveMaxCountryCrossingShare)
				f.action = "Read the crossings panel and the job's log; a crossing cluster in one place is a genesis source or a colluding cluster, a fleet-wide rise is the slope."
				f.verify = "The next run's country crossing share falls under the threshold."
				failing = append(failing, f)
			}
			if share := derivedLocationsShare(table.crossedRegion, table.published); settings.DeriveMaxRegionCrossingShare < share {
				f := derivedLocationsFinding("derive-crossings", "region", 1)
				f.symptom = "An unusual share of published nodes map outside the region their genesis placed them in"
				f.mechanism = "The region containment term prices every crossing, so a surviving crossing is the pings insisting; a high share is a wrong genesis source or a bad slope."
				f.baseline = fmt.Sprintf("Region crossings at most DeriveMaxRegionCrossingShare=%.2f of published nodes.", settings.DeriveMaxRegionCrossingShare)
				f.observed = fmt.Sprintf("crossed_region=%d published=%d share=%.4f threshold=DeriveMaxRegionCrossingShare=%.2f", table.crossedRegion, table.published, share, settings.DeriveMaxRegionCrossingShare)
				f.action = "Read the crossings panel; calibrate one setting at a time and wait one run."
				f.verify = "The next run's region crossing share falls under the threshold."
				failing = append(failing, f)
			}
		}
		emit("derive-crossings", failing)
	}

	// exclusions. The median reputation is the table's and is judged whatever
	// the history; the excluded share is the last run's, so without the
	// history the class can report a fault but never health.
	{
		failing := []finding{}
		if history.observable && 0 < len(history.runs) {
			run := history.runs[0]
			if nodeSamples <= int64(run.Sources) {
				if share := derivedLocationsShare(int64(run.ExcludedSources), int64(run.Sources)); settings.DeriveMaxExcludedShare < share {
					f := derivedLocationsFinding("derive-exclusions", "excluded-share", 1)
					f.symptom = "The last derivation excluded an unusual share of its sources"
					f.mechanism = "Reputation excludes a source past four sigma from the population. A large excluded share means the population statistics are skewed by something systematic, not by a few bad sources."
					f.baseline = fmt.Sprintf("Excluded sources at most DeriveMaxExcludedShare=%.2f of the run's sources.", settings.DeriveMaxExcludedShare)
					f.observed = fmt.Sprintf("excluded_sources=%d sources=%d share=%.4f threshold=DeriveMaxExcludedShare=%.2f", run.ExcludedSources, run.Sources, share, settings.DeriveMaxExcludedShare)
					f.context = fmt.Sprintf("expected_peers: extender=%d provider=%d (the run's own, from ExtenderPeerSampleSize and ProviderProbeSampleSize)", run.ExpectedExtenderPeers, run.ExpectedProviderPeers)
					f.action = "Read the job's log line, which names the excluded sources, and look for what they share: a pinger kind, a binary, a place. A pinger kind excluded wholesale is often its expected peers set above what its pinger is designed to measure (a provider's probe window is four extenders), which marks every one of them down on coverage before the next round excludes it. Calibrate the spread floors only once the cause is known."
					f.verify = "The next run's excluded share falls under the threshold."
					failing = append(failing, f)
				}
			}
		}
		if nodeSamples <= table.published && 0 <= table.medianReputation && table.medianReputation < settings.DeriveMinMedianReputation {
			f := derivedLocationsFinding("derive-exclusions", "median-reputation", 1)
			f.symptom = "The median reputation of the published nodes is low"
			f.mechanism = "A source's weight is 1/(1 + z²) against the population; a low median means the population itself is spread by something systematic, and most nodes are solved with little weight on their own measurements."
			f.baseline = fmt.Sprintf("Median reputation at least DeriveMinMedianReputation=%.2f.", settings.DeriveMinMedianReputation)
			f.observed = fmt.Sprintf("median_reputation=%.4f published=%d threshold=DeriveMinMedianReputation=%.2f", table.medianReputation, table.published, settings.DeriveMinMedianReputation)
			if history.observable && 0 < len(history.runs) {
				f.context = fmt.Sprintf("expected_peers: extender=%d provider=%d (the last run's own, from ExtenderPeerSampleSize and ProviderProbeSampleSize)", history.runs[0].ExpectedExtenderPeers, history.runs[0].ExpectedProviderPeers)
			}
			f.action = "Read the reputation statistics in the job's log. A population marked down on coverage is measured against more peers than its pingers are designed to reach (a provider's probe window is four extenders): check the expected peers before the spread floors, and calibrate one setting at a time."
			f.verify = "The next run's median reputation returns above the threshold."
			failing = append(failing, f)
		}
		if history.observable || 0 < len(failing) {
			emit("derive-exclusions", failing)
		}
	}

	// non-convergence
	if history.observable {
		failing := []finding{}
		runs := max(1, settings.DeriveCapRuns)
		if runs <= len(history.runs) {
			capped := true
			sweeps := []string{}
			for _, run := range history.runs[:runs] {
				if run.Converged {
					capped = false
				}
				sweeps = append(sweeps, fmt.Sprintf("%d/%d", run.LastRoundSweeps, run.SweepCap))
			}
			if capped {
				f := derivedLocationsFinding("derive-non-convergence", "sweep-cap", 1)
				f.symptom = fmt.Sprintf("The last reputation round hit the sweep cap on %d consecutive runs", runs)
				f.mechanism = "A solve that stops on the sweep cap rather than its step tolerance ends where the cap left it; every accepted step still lowers the objective, so the positions are not wrong, but they are not settled either, and the next run's warm start carries that on."
				f.baseline = fmt.Sprintf("The last round converges on at least one of any DeriveCapRuns=%d consecutive runs.", runs)
				f.observed = fmt.Sprintf("last_round_sweeps/cap newest_first=%s threshold=DeriveCapRuns=%d", strings.Join(sweeps, ","), runs)
				f.context = fmt.Sprintf("last run: still_moving=%d nodes refused as still moving when the solve stopped", history.runs[0].RefusedStillMoving)
				f.action = "Raise the sweep cap or revisit the damping in DeriveSettings, one at a time, and wait one run."
				f.verify = "The next run's last round converges."
				failing = append(failing, f)
			}
		}
		// The nodes still moving when the last run stopped are refused
		// publication and keep their genesis. A share of them that climbs is a
		// solve ending before it settles -- on the sweep cap, which more sweeps
		// fix at the price of the capacity projection, or on a stagnation stop
		// that comes too early -- and the published count falls with it.
		if 0 < len(history.runs) {
			run := history.runs[0]
			if int64(settings.DeriveMinNodeSamples) <= int64(run.Nodes) {
				if share := derivedLocationsShare(int64(run.RefusedStillMoving), int64(run.Nodes)); settings.DeriveMaxStillMovingShare < share {
					f := derivedLocationsFinding("derive-non-convergence", "still-moving", 1)
					f.symptom = "The last derivation stopped with an unusual share of its nodes still moving"
					f.mechanism = "The solve refuses to publish a node that moved more than PublishMaxLastStepKm in its last sweep: it has not arrived anywhere yet. The solve stops on the sweep cap or when the objective stops improving; a share of nodes still sliding at that stop is a solve that ends before it settles, and those nodes keep their genesis."
					f.baseline = fmt.Sprintf("Nodes refused as still moving at most DeriveMaxStillMovingShare=%.2f of the run's solved nodes.", settings.DeriveMaxStillMovingShare)
					f.observed = fmt.Sprintf("still_moving=%d nodes=%d share=%.4f threshold=DeriveMaxStillMovingShare=%.2f converged=%t stagnated=%t last_round_sweeps=%d sweep_cap=%d", run.RefusedStillMoving, run.Nodes, share, settings.DeriveMaxStillMovingShare, run.Converged, run.Stagnated, run.LastRoundSweeps, run.SweepCap)
					if 2 <= len(history.runs) {
						previous := history.runs[1]
						f.context = fmt.Sprintf("previous run: still_moving=%d nodes=%d share=%.4f", previous.RefusedStillMoving, previous.Nodes, derivedLocationsShare(int64(previous.RefusedStillMoving), int64(previous.Nodes)))
					}
					f.action = "On the sweep cap (converged=false), raise MaxIterations in DeriveSettings and read the capacity projection, since every sweep costs the whole graph; on a stagnation stop (stagnated=true), lengthen StagnationSweeps or lower StagnationRelativeImprovement. One setting at a time, and wait one run."
					f.verify = "The next run's still-moving share falls under the threshold."
					failing = append(failing, f)
				}
			}
		}
		emit("derive-non-convergence", failing)
	}

	// thin evidence
	{
		failing := []finding{}
		if nodeSamples <= table.published {
			if share := derivedLocationsShare(table.atMinPeers, table.published); settings.DeriveMaxThinEvidenceShare < share {
				f := derivedLocationsFinding("derive-thin-evidence", "", 1)
				f.symptom = "An unusual share of published nodes rest on the fewest peers the gate allows"
				f.mechanism = "Two peers fix a node only up to the line through them: either of two mirror points explains its pings equally, and GEOMAP §9 records about 1.5% of such synthetic nodes converging to the wrong one, which is why the gate asks for three (D11). A node at the gate still rests on the least geometry that fixes it: three peers near one line barely break the tie, and one bad peer among three has nothing to outvote it. A high share at the gate is a publication resting on that thinnest evidence."
				f.baseline = fmt.Sprintf("Published nodes at exactly MinDerivePeers=%d peers at most DeriveMaxThinEvidenceShare=%.2f of published nodes.", settings.MinDerivePeers, settings.DeriveMaxThinEvidenceShare)
				f.observed = fmt.Sprintf("at_min_peers=%d published=%d share=%.4f threshold=DeriveMaxThinEvidenceShare=%.2f min_derive_peers=%d", table.atMinPeers, table.published, share, settings.DeriveMaxThinEvidenceShare, settings.MinDerivePeers)
				f.action = "Raise the ping coverage so nodes reach more peers; raising MinDerivePeers in DeriveSettings trades the thin publications for more few_peers refusals and fewer published nodes. Wait one run."
				f.verify = "The next run's share at the gate falls under the threshold."
				failing = append(failing, f)
			}
		}
		emit("derive-thin-evidence", failing)
	}

	// capacity: the planner's projection of the next run against the budget
	// the run recorded with it
	if history.observable {
		failing := []finding{}
		if 0 < len(history.runs) {
			run := history.runs[0]
			maxSolveSeconds := run.MaxSolveSeconds
			if maxSolveSeconds <= 0 {
				maxSolveSeconds = settings.MaxSolveSeconds
			}
			maxSolveBytes := run.MaxSolveBytes
			if maxSolveBytes <= 0 {
				maxSolveBytes = settings.MaxSolveBytes
			}
			for _, resource := range []struct {
				frame      string
				projected  float64
				budget     float64
				budgetName string
				unit       string
			}{
				{frame: "seconds", projected: run.ProjectedSeconds, budget: maxSolveSeconds, budgetName: "MaxSolveSeconds", unit: "s"},
				{frame: "bytes", projected: float64(run.ProjectedBytes), budget: float64(maxSolveBytes), budgetName: "MaxSolveBytes", unit: "B"},
			} {
				if !(0 < resource.budget && (1-settings.DeriveCapacityMargin)*resource.budget <= resource.projected) {
					continue
				}
				share := resource.projected / resource.budget
				f := derivedLocationsFinding("derive-capacity", resource.frame, 1)
				f.symptom = fmt.Sprintf("The derive job's next run is projected within DeriveCapacityMargin of its %s budget on this host", resource.frame)
				f.mechanism = "Every run projects the next from its own measured costs -- seconds per term and per node sweep at the host's cores, bytes per term and per node, the sweeps it took -- and records the projection beside the solve's budget. The job does not partition when the fleet outgrows the host: it runs over budget, late or out of memory, so the projection is the warning."
				f.baseline = fmt.Sprintf("A projection under 1-DeriveCapacityMargin=%.2f of %s.", 1-settings.DeriveCapacityMargin, resource.budgetName)
				f.observed = fmt.Sprintf("projected=%.0f%s budget=%.0f%s share=%.4f threshold=DeriveCapacityMargin=%.2f terms=%d nodes=%d sweeps=%d cores=%d", resource.projected, resource.unit, resource.budget, resource.unit, share, settings.DeriveCapacityMargin, run.Terms, run.Nodes, run.Sweeps, run.Cores)
				f.action = "Give the taskworker host more cores (seconds) or memory (bytes), and raise the job's MaxSolveSeconds or MaxSolveBytes to match; the partitioning GEOMAP §5.3 keeps on paper is the step after the host."
				f.verify = "The next run's projection falls under the margin."
				failing = append(failing, f)
			}
		}
		emit("derive-capacity", failing)
	}

	// sweep: the ping partitions, then the derived rows
	{
		failing := []finding{}
		derivedLocationCutSeconds, partitionDropCutSeconds, checkedAheadDays := derivedLocationsSweepCutSeconds(settings)
		const sweepAction = "Check the RemoveExpiredPings pending_task row and its reschedule error, and the [ping] partition lines of the taskworker log: a partition create or drop waits a bounded time for its lock and retries on the next sweep."
		// before the conversion migration network_ping is a plain table, and
		// there are no partitions to judge
		if partitions.partitioned && 0 < partitions.overdueDrops {
			f := derivedLocationsFinding("derive-sweep-stalled", "partition-drop", 1)
			f.symptom = "network_ping keeps a day partition the sweep should have dropped"
			f.mechanism = "The hourly RemoveExpiredPings sweep drops a day partition of network_ping once its upper bound is older than the span it keeps a ping -- the retention, or the two clock skews a report is accepted across if longer, and a sweep interval -- and never deletes a row. A partition past that and one more sweep interval means the drops stopped: the table grows by a day a day, and the derivation's window reads the same days it always did, so nothing else shows it."
			f.baseline = fmt.Sprintf("No sweep partition whose upper bound is older than PingPartitionKeepTimeout=%s plus DeriveSweepGrace=%s.", settings.PingPartitionKeepTimeout, settings.DeriveSweepGrace)
			f.observed = fmt.Sprintf("overdue_partitions=%d oldest_upper_bound_age_seconds=%d partitions=%d threshold=PingPartitionKeepTimeout+DeriveSweepGrace=%ds", partitions.overdueDrops, partitions.oldestOverdueUpperAgeSeconds, partitions.partitions, partitionDropCutSeconds)
			f.action = sweepAction
			f.verify = "The next sweep drops every partition past the cut."
			failing = append(failing, f)
		}
		if partitions.partitioned && !partitions.checkedDayCovered {
			f := derivedLocationsFinding("derive-sweep-stalled", "partition-ahead", 1)
			f.symptom = "No network_ping partition covers the day the sweep should already have created"
			f.mechanism = "The hourly RemoveExpiredPings sweep keeps today's partition and the next days' ahead of the inserts, so the ingest never finds its day missing. When none covers the day before the last one it keeps ahead, the sweep has not created partitions for a day: the ingest then creates its day itself, under the table's exclusive lock on the report path, and a sweep outage longer than the days ahead shows up there first."
			f.baseline = fmt.Sprintf("A partition covering today plus %d day(s): PingPartitionAheadDays=%d, less the day the sweep adds after midnight.", checkedAheadDays, settings.PingPartitionAheadDays)
			f.observed = fmt.Sprintf("checked_day=today+%d covered=false partitions=%d threshold=PingPartitionAheadDays=%d", checkedAheadDays, partitions.partitions, settings.PingPartitionAheadDays)
			f.action = sweepAction
			f.verify = "The next sweep creates the days ahead and the checked day is covered."
			failing = append(failing, f)
		}
		if 0 < table.expired {
			f := derivedLocationsFinding("derive-sweep-stalled", "derived_location", 1)
			f.symptom = "derived_location holds rows older than the retention and an hour"
			f.mechanism = "The hourly RemoveExpiredPings sweep deletes the derived rows past PingRetention, one row per node. Rows past that and one sweep interval mean the sweep is not running, and a node that stopped pinging keeps its derived location."
			f.baseline = fmt.Sprintf("No derived row older than PingRetention=%s plus DeriveSweepGrace=%s.", settings.PingRetention, settings.DeriveSweepGrace)
			f.observed = fmt.Sprintf("expired_rows=%d table=derived_location threshold=PingRetention+DeriveSweepGrace=%ds", table.expired, derivedLocationCutSeconds)
			f.action = sweepAction
			f.verify = "The next sweep leaves no row past the cut."
			failing = append(failing, f)
		}
		emit("derive-sweep-stalled", failing)
	}

	// clock or wire
	{
		failing := []finding{}
		all := pings.lastHour.pings()
		if pingSamples <= all {
			if share := derivedLocationsShare(pings.lastHour.zeroRtt, all); settings.DeriveMaxZeroRttShare < share {
				f := derivedLocationsFinding("derive-clock-or-wire", "zero-rtt", 1)
				f.symptom = "An unusual share of pings report a zero round trip"
				f.mechanism = "A zero round trip implies a distance of nothing: a clock that does not advance across the probe, or a report that lost its round trip on the wire."
				f.baseline = fmt.Sprintf("Zero round trips at most DeriveMaxZeroRttShare=%.2f of the last complete hour's pings.", settings.DeriveMaxZeroRttShare)
				f.observed = fmt.Sprintf("zero_rtt_last_hour=%d pings_last_hour=%d share=%.4f threshold=DeriveMaxZeroRttShare=%.2f", pings.lastHour.zeroRtt, all, share, settings.DeriveMaxZeroRttShare)
				f.action = "Check the pingers' clock source and the report encoding of rtt_ms."
				f.verify = "The next complete hour's zero share falls under the threshold."
				failing = append(failing, f)
			}
			if share := derivedLocationsShare(pings.lastHour.beyondHalfPlanet, all); settings.DeriveMaxBeyondHalfPlanetShare < share {
				f := derivedLocationsFinding("derive-clock-or-wire", "beyond-half-planet", 1)
				f.symptom = "An unusual share of pings report a round trip longer than half the planet"
				f.mechanism = fmt.Sprintf("At the solver's slope no two points on earth are more than %d ms apart; longer round trips are queues, a clock, or pingers waiting before they attest, and they read as distances the geometry cannot hold.", settings.DeriveHalfPlanetRttMs)
				f.baseline = fmt.Sprintf("Round trips beyond DeriveHalfPlanetRttMs=%d at most DeriveMaxBeyondHalfPlanetShare=%.2f of the last complete hour's pings.", settings.DeriveHalfPlanetRttMs, settings.DeriveMaxBeyondHalfPlanetShare)
				f.observed = fmt.Sprintf("beyond_half_planet_last_hour=%d pings_last_hour=%d share=%.4f threshold=DeriveMaxBeyondHalfPlanetShare=%.2f", pings.lastHour.beyondHalfPlanet, all, share, settings.DeriveMaxBeyondHalfPlanetShare)
				f.action = "Check whether the long round trips concentrate on relayed pings, one pinger kind or one carrier; a pinger that delays its attestation is what reputation's bias term is for."
				f.verify = "The next complete hour's share falls under the threshold."
				failing = append(failing, f)
			}
		}
		emit("derive-clock-or-wire", failing)
	}

	return findings
}
