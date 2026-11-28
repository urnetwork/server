// Reclaimable bloat: the share of each relation that a rewrite would return,
// from the weekly bloat measurement's raw rows (server/db_bloat_measure.go,
// PARTITION-MAINTENANCE-PLAN.md A4), credited with any shrink since and judged
// against the relation's own size. The §2.4 dead-tuple line is a fixed 10M
// rows, so a queue of a few hundred live rows never reaches it while its
// indexes grow without bound; this line scales with each relation.
package monitor

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const tableBloatProbeId = "pg/table-bloat"

// SIGNALS.md §2.4k (table-bloat): relations a rewrite would mostly return.
func NewTableBloatSignal() Signal {
	return NewTableBloatSignalWithSettings(DefaultTableBloatSettings())
}

// The signal with explicit thresholds.
func NewTableBloatSignalWithSettings(settings *TableBloatSettings) Signal {
	return &signalAdapter{
		number: "2.4k", key: "table-bloat", name: "Reclaimable table bloat",
		probe: tableBloatProbe{settings: settings},
	}
}

// The thresholds of SIGNALS.md §2.4k. Sizes are decimal bytes.
type TableBloatSettings struct {
	// Normal sampling cadence and the whole-run budget for both reads.
	Cadence   time.Duration
	RunBudget time.Duration
	// The primary's statement clock may differ from the monitor's by this.
	ClockTolerance time.Duration
	// A relation is bloated when, after the shrink credit, at least this many
	// bytes and this share of its current size are reclaimable. Half is the
	// priority rebuild's line: an index at least twice its compact estimate.
	MinReclaimableBytes float64
	MinReclaimableShare float64
	// Each relation's newest measurement is judged only while it is at most
	// this old; a scan cycle can span several weeks.
	MaxRelationMeasurementAge time.Duration
	// The newest measurement of all older than this means the weekly chain
	// stopped.
	MaxMeasurementAge time.Duration
	// The most bloated relations one read lists.
	RelationLimit       int
	BloatSustain        int
	UnobservableSustain int
	// The §2.4 dead-tuple line, named beside each table's dead rows.
	DeadTupleLine int64
	// The longest text cell a frame may carry.
	MaxCellBytes int
}

// The §2.4k defaults: hourly reads of weekly measurements. A stale chain is
// one that missed a week and its two days of deferrals.
func DefaultTableBloatSettings() *TableBloatSettings {
	return &TableBloatSettings{
		Cadence:                   time.Hour,
		RunBudget:                 70 * time.Second,
		ClockTolerance:            30 * time.Second,
		MinReclaimableBytes:       5 * 1000 * 1000 * 1000,
		MinReclaimableShare:       0.5,
		MaxRelationMeasurementAge: 35 * 24 * time.Hour,
		MaxMeasurementAge:         9 * 24 * time.Hour,
		RelationLimit:             64,
		BloatSustain:              1,
		UnobservableSustain:       2,
		DeadTupleLine:             vacuumDeadTupleAlertFloor,
		MaxCellBytes:              256,
	}
}

// Stateless: each run owns its two bounded reads.
type tableBloatProbe struct {
	settings *TableBloatSettings
}

func (self tableBloatProbe) id() string               { return tableBloatProbeId }
func (self tableBloatProbe) tier() string             { return tierWarn }
func (self tableBloatProbe) cadence() time.Duration   { return self.settings.Cadence }
func (self tableBloatProbe) runBudget() time.Duration { return self.settings.RunBudget }

const tableBloatPresenceColumns = 5

const tableBloatColumns = 11

// Whether the measurement table exists, before a read that names it:
// PostgreSQL resolves every relation a statement names before it runs.
const tableBloatPresenceTemplate = `{hostname_guard}
SELECT 'meta', :'monitor_primary_hostname'::text,
       floor(extract(epoch FROM statement_timestamp()) * 1000000)::bigint::text,
       CASE WHEN pg_is_in_recovery() THEN '1' ELSE '0' END,
       CASE WHEN to_regclass('public.db_bloat_measurement') IS NULL THEN '0' ELSE '1' END;
`

// Each relation's newest measurement inside the window, the ones over both
// lines as measured (the shrink credit only lowers them) that still exist,
// with their current sizes, and the current row counters of their tables.
// Eleven text cells per row; counts in the meta and end rows bind the rows.
const tableBloatMeasurementsTemplate = `{hostname_guard}
WITH source AS MATERIALIZED (
 SELECT :'monitor_primary_hostname'::text AS hostname,
        floor(extract(epoch FROM statement_timestamp()) * 1000000)::bigint AS observed_us,
        CASE WHEN pg_is_in_recovery() THEN '1' ELSE '0' END AS recovery,
        (SELECT floor(extract(epoch FROM max(measure_time)) * 1000000)::bigint FROM db_bloat_measurement) AS newest_us
), newest AS MATERIALIZED (
 SELECT DISTINCT ON (m.table_name, m.relation_schema, m.relation_name)
        m.table_name, m.relation_kind, m.relation_schema, m.relation_name, m.method,
        m.measure_time, m.relation_bytes, m.reclaimable_bytes
 FROM db_bloat_measurement m
 WHERE m.measure_time >= (statement_timestamp() AT TIME ZONE 'UTC') - {max_relation_age_seconds} * interval '1 second'
 ORDER BY m.table_name, m.relation_schema, m.relation_name, m.measure_time DESC
), candidate AS MATERIALIZED (
 SELECT newest.*, to_regclass(format('%I.%I', newest.relation_schema, newest.relation_name)) AS current_relation
 FROM newest
 WHERE newest.reclaimable_bytes >= {min_reclaimable_bytes}
   AND newest.reclaimable_bytes >= {min_reclaimable_share} * newest.relation_bytes
), listed AS MATERIALIZED (
 SELECT candidate.*, pg_relation_size(candidate.current_relation) AS current_bytes
 FROM candidate
 WHERE candidate.current_relation IS NOT NULL
 ORDER BY candidate.reclaimable_bytes DESC, candidate.table_name, candidate.relation_schema, candidate.relation_name
 LIMIT {relation_limit}
), listed_table AS MATERIALIZED (
 SELECT DISTINCT listed.table_name, to_regclass(format('%I.%I', 'public', listed.table_name)) AS table_relation
 FROM listed
), table_stat AS MATERIALIZED (
 SELECT listed_table.table_name,
        coalesce(listed_table.table_relation::oid::bigint, 0) AS table_oid,
        coalesce(stat.n_live_tup, -1) AS live_rows,
        coalesce(stat.n_dead_tup, -1) AS dead_rows
 FROM listed_table
 LEFT JOIN pg_stat_user_tables stat ON stat.relid = listed_table.table_relation
)
SELECT 'meta', s.hostname, s.observed_us::text, s.recovery, coalesce(s.newest_us::text, 'none'),
       (SELECT count(*) FROM newest)::text,
       (SELECT count(*) FROM candidate WHERE candidate.current_relation IS NOT NULL)::text,
       '', '', '', ''
FROM source s
UNION ALL SELECT 'relation', l.table_name, l.relation_kind, l.relation_schema, l.relation_name, l.method,
       floor(extract(epoch FROM l.measure_time) * 1000000)::bigint::text,
       l.relation_bytes::text, l.reclaimable_bytes::text, l.current_bytes::text, l.current_relation::oid::text
FROM listed l
UNION ALL SELECT 'table', t.table_name, t.table_oid::text, t.live_rows::text, t.dead_rows::text, '', '', '', '', '', ''
FROM table_stat t
UNION ALL SELECT 'end', (SELECT count(*) FROM listed)::text, (SELECT count(*) FROM table_stat)::text, '', '', '', '', '', '', '', '';
`

func (self tableBloatProbe) presenceQuery() string {
	return strings.NewReplacer("{hostname_guard}", pgPrimaryHostnameMetaCommand).Replace(tableBloatPresenceTemplate)
}

// The measurement read with this instance's window, lines and list bound; one
// row past the bound shows the list was cut.
func (self tableBloatProbe) measurementsQuery() string {
	settings := self.settings
	return strings.NewReplacer(
		"{hostname_guard}", pgPrimaryHostnameMetaCommand,
		"{max_relation_age_seconds}", strconv.FormatInt(int64(settings.MaxRelationMeasurementAge/time.Second), 10),
		"{min_reclaimable_bytes}", strconv.FormatFloat(settings.MinReclaimableBytes, 'f', 0, 64),
		"{min_reclaimable_share}", strconv.FormatFloat(settings.MinReclaimableShare, 'f', -1, 64),
		"{relation_limit}", strconv.Itoa(settings.RelationLimit+1),
	).Replace(tableBloatMeasurementsTemplate)
}

// One relation over both lines as measured.
type tableBloatRelation struct {
	tableName     string
	kind          string
	schema        string
	name          string
	method        string
	measured      time.Time
	measuredBytes int64
	// what the measurement found reclaimable
	measuredReclaimableBytes int64
	currentBytes             int64
	oid                      int64
}

// The bytes still reclaimable: what the measurement found, less what the
// relation has shrunk by since (a rebuild or VACUUM FULL returned it), never
// below zero. Growth since is not added: the measurement cannot say whether
// it is live.
func (self tableBloatRelation) reclaimableBytes() int64 {
	return max(0, self.measuredReclaimableBytes-max(0, self.measuredBytes-self.currentBytes))
}

// The reclaimable share of the relation's current size.
func (self tableBloatRelation) share() float64 {
	if self.currentBytes <= 0 {
		return 0
	}
	return float64(self.reclaimableBytes()) / float64(self.currentBytes)
}

// A listed table's current statistics row; negative counters are unknown.
type tableBloatTable struct {
	oid      int64
	liveRows int64
	deadRows int64
}

// The validated measurement read.
type tableBloatObservation struct {
	observed  time.Time
	newest    time.Time
	hasNewest bool
	// relations with a measurement inside the window, and those over both
	// lines as measured that still exist
	windowRelations int64
	candidates      int64
	relations       []tableBloatRelation
	tables          map[string]tableBloatTable
	truncated       bool
}

var tableBloatNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// A relation's name when it is a plain identifier, or its oid.
func tableBloatName(name string, oid int64) string {
	if tableBloatNamePattern.MatchString(name) {
		return name
	}
	if 0 < oid {
		return "relation-" + strconv.FormatInt(oid, 10)
	}
	return "relation-withheld"
}

// The identity, role and clock of one read's meta row.
func (self tableBloatProbe) parseSource(row pgRow, primary string, now time.Time) (time.Time, string) {
	shortName, _, _ := strings.Cut(primary, ".")
	if row[1] != shortName || shortName == "" || row[3] != "0" {
		return time.Time{}, "source-identity-or-role-unavailable"
	}
	observed, ok := retentionLivenessMicros(row[2])
	if !ok {
		return time.Time{}, "invalid-source-clock"
	}
	if observed.Sub(now).Abs() > self.settings.ClockTolerance {
		return time.Time{}, "source-clock-unavailable"
	}
	return observed, ""
}

// The presence read: the primary answered and holds the measurement table.
func (self tableBloatProbe) parsePresence(rows []pgRow, primary string, now time.Time) string {
	if len(rows) != 1 || len(rows[0]) != tableBloatPresenceColumns || rows[0][0] != "meta" {
		return "incomplete-frame"
	}
	for _, cell := range rows[0] {
		if len(cell) > self.settings.MaxCellBytes {
			return "overbound-cell"
		}
	}
	if _, reason := self.parseSource(rows[0], primary, now); reason != "" {
		return reason
	}
	switch rows[0][4] {
	case "1":
		return ""
	case "0":
		return "measurement-table-missing"
	default:
		return "invalid-presence"
	}
}

// Fixed framing, the primary's identity and role, a source clock near the
// monitor's, and rows bound by the counts.
func (self tableBloatProbe) parseMeasurements(rows []pgRow, primary string, now time.Time) (tableBloatObservation, string) {
	settings := self.settings
	observation := tableBloatObservation{tables: map[string]tableBloatTable{}}
	metas, ends := 0, 0
	var end pgRow
	for _, row := range rows {
		if len(row) != tableBloatColumns {
			return observation, "invalid-frame-width"
		}
		for _, cell := range row {
			if len(cell) > settings.MaxCellBytes {
				return observation, "overbound-cell"
			}
		}
		switch row[0] {
		case "meta":
			metas++
			observed, reason := self.parseSource(row, primary, now)
			if reason != "" {
				return observation, reason
			}
			observation.observed = observed
			// the taskworker's clock stamps measurements, so one may be a
			// little ahead of the primary's
			if row[4] != "none" {
				newest, ok := retentionLivenessMicros(row[4])
				if !ok {
					return observation, "invalid-newest-measurement"
				}
				observation.newest, observation.hasNewest = newest, true
			}
			windowRelations, okWindow := pgFreezeAgeInteger(row[5], 0, math.MaxInt64)
			candidates, okCandidates := pgFreezeAgeInteger(row[6], 0, math.MaxInt64)
			if !okWindow || !okCandidates || windowRelations < candidates {
				return observation, "invalid-meta-row"
			}
			observation.windowRelations, observation.candidates = windowRelations, candidates
		case "relation":
			relation := tableBloatRelation{tableName: row[1], kind: row[2], schema: row[3], name: row[4], method: row[5]}
			measured, okMeasured := retentionLivenessMicros(row[6])
			measuredBytes, okBytes := pgFreezeAgeInteger(row[7], 0, math.MaxInt64)
			reclaimable, okReclaimable := pgFreezeAgeInteger(row[8], 0, math.MaxInt64)
			current, okCurrent := pgFreezeAgeInteger(row[9], 0, math.MaxInt64)
			oid, okOid := pgFreezeAgeInteger(row[10], 1, math.MaxUint32)
			if !okMeasured || !okBytes || !okReclaimable || !okCurrent || !okOid || measuredBytes < reclaimable ||
				!slices.Contains([]string{"heap", "toast", "index"}, relation.kind) ||
				relation.tableName == "" || relation.schema == "" || relation.name == "" || relation.method == "" {
				return observation, "invalid-relation-row"
			}
			relation.measured, relation.measuredBytes, relation.measuredReclaimableBytes = measured, measuredBytes, reclaimable
			relation.currentBytes, relation.oid = current, oid
			observation.relations = append(observation.relations, relation)
		case "table":
			oid, okOid := pgFreezeAgeInteger(row[2], 0, math.MaxUint32)
			liveRows, okLive := pgFreezeAgeInteger(row[3], -1, math.MaxInt64)
			deadRows, okDead := pgFreezeAgeInteger(row[4], -1, math.MaxInt64)
			if !okOid || !okLive || !okDead || row[1] == "" {
				return observation, "invalid-table-row"
			}
			observation.tables[row[1]] = tableBloatTable{oid: oid, liveRows: liveRows, deadRows: deadRows}
		case "end":
			ends++
			end = row
		default:
			return observation, "unexpected-frame-kind"
		}
	}
	if metas != 1 || ends != 1 {
		return observation, "incomplete-frame"
	}
	relationCount, okRelations := pgFreezeAgeInteger(end[1], 0, math.MaxInt32)
	tableCount, okTables := pgFreezeAgeInteger(end[2], 0, math.MaxInt32)
	if !okRelations || !okTables || int(relationCount) != len(observation.relations) || int(tableCount) != len(observation.tables) ||
		observation.candidates < relationCount {
		return observation, "incomplete-frame"
	}
	for _, relation := range observation.relations {
		if _, ok := observation.tables[relation.tableName]; !ok {
			return observation, "incomplete-frame"
		}
	}
	if len(observation.relations) > settings.RelationLimit {
		observation.relations, observation.truncated = observation.relations[:settings.RelationLimit], true
	}
	return observation, ""
}

// The presence read, then the measurement read; each failure is one fixed
// reason for the measurement target.
func (self tableBloatProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if probeEnded(ctx) {
		return nil, ctx.Err()
	}
	primaries := env.cfg.hostsWithRole("pg-primary")
	if len(primaries) != 1 {
		return []finding{self.unknown("primary/bloat-measurement", "primary-inventory-unavailable-or-ambiguous", nil, "")}, nil
	}
	primary := primaries[0]
	target := primary.name + "/bloat-measurement"
	now := env.now().UTC()
	read := func(query string) ([]pgRow, error, error) {
		rows, err := env.runner.pg(ctx, query)
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, nil, lifecycleErr
		}
		return rows, err, nil
	}

	rows, err, lifecycleErr := read(self.presenceQuery())
	if lifecycleErr != nil {
		return nil, lifecycleErr
	}
	if err != nil {
		return []finding{self.unknown(target, "presence-source-unavailable", err, "")}, nil
	}
	if reason := self.parsePresence(rows, primary.name, now); reason != "" {
		return []finding{self.unknown(target, reason, nil, "")}, nil
	}

	rows, err, lifecycleErr = read(self.measurementsQuery())
	if lifecycleErr != nil {
		return nil, lifecycleErr
	}
	if err != nil {
		return []finding{self.unknown(target, "measurement-source-unavailable", err, "")}, nil
	}
	observation, reason := self.parseMeasurements(rows, primary.name, now)
	if reason != "" {
		return []finding{self.unknown(target, reason, nil, "")}, nil
	}
	findings := self.bloat(primary.name, observation)
	switch {
	case !observation.hasNewest:
		findings = append(findings, self.unknown(target, "no-measurement", nil, ""))
	case self.settings.MaxMeasurementAge < observation.observed.Sub(observation.newest):
		findings = append(findings, self.unknown(target, "measurement-stale", nil,
			fmt.Sprintf(" newest_measurement_age_days=%.1f", observation.observed.Sub(observation.newest).Hours()/24)))
	default:
		findings = append(findings, healthyFinding(tableBloatProbeId, tierWarn, "table-bloat-unobservable", target))
	}
	return findings, nil
}

// One finding per table with a relation over both lines after the shrink
// credit, in table order.
func (self tableBloatProbe) bloat(primary string, observation tableBloatObservation) []finding {
	settings := self.settings
	bloatedRelations := map[string][]tableBloatRelation{}
	for _, relation := range observation.relations {
		if float64(relation.reclaimableBytes()) < settings.MinReclaimableBytes || relation.share() < settings.MinReclaimableShare {
			continue
		}
		bloatedRelations[relation.tableName] = append(bloatedRelations[relation.tableName], relation)
	}
	tableNames := []string{}
	for tableName := range bloatedRelations {
		tableNames = append(tableNames, tableName)
	}
	slices.Sort(tableNames)

	findings := []finding{}
	for _, tableName := range tableNames {
		relations := bloatedRelations[tableName]
		slices.SortFunc(relations, func(a tableBloatRelation, b tableBloatRelation) int {
			return cmp.Or(
				cmp.Compare(b.reclaimableBytes(), a.reclaimableBytes()),
				strings.Compare(a.schema+"."+a.name, b.schema+"."+b.name),
			)
		})
		table := observation.tables[tableName]
		label := tableBloatName(tableName, table.oid)
		var reclaimableBytes, currentBytes int64
		kinds := []string{}
		parts := []string{}
		for _, relation := range relations {
			reclaimableBytes += relation.reclaimableBytes()
			currentBytes += relation.currentBytes
			if !slices.Contains(kinds, relation.kind) {
				kinds = append(kinds, relation.kind)
			}
			parts = append(parts, fmt.Sprintf("%s %s %s size_gb=%.1f reclaimable_gb=%.1f share_pct=%.1f measured_days_ago=%.1f",
				tableBloatName(relation.name, relation.oid), relation.kind, relation.method,
				gb(float64(relation.currentBytes)), gb(float64(relation.reclaimableBytes())), 100*relation.share(),
				observation.observed.Sub(relation.measured).Hours()/24))
		}
		slices.Sort(kinds)
		counter := func(value int64) string {
			if value < 0 {
				return "unknown"
			}
			return strconv.FormatInt(value, 10)
		}
		evidence := "Each relation's newest row of db_bloat_measurement within the window, written by the weekly measure_db_bloat task (server/db_bloat_measure.go), against its current size on the primary, in one primary-guarded read-only query. A heap or TOAST relation was measured by pgstattuple_approx (its row records the share it read) or by the statistics estimate (its row records why); an index by the btree estimate of the priority rebuild."
		if observation.truncated {
			evidence += fmt.Sprintf(" relation_list=truncated at the %d with the most measured reclaimable bytes.", settings.RelationLimit)
		}
		indexOnly := len(kinds) == 1 && kinds[0] == "index"
		symptom := fmt.Sprintf("A rewrite of %s would return about %.1f GB, %.0f%% of its bloated relations", label, gb(float64(reclaimableBytes)), 100*float64(reclaimableBytes)/float64(max(currentBytes, 1)))
		if indexOnly {
			symptom += "; its indexes hold it"
		}
		findings = append(findings, finding{
			probeId: tableBloatProbeId, tier: tierWarn, class: "table-bloat", target: primary + "/" + label, sustain: settings.BloatSustain,
			symptom:   symptom,
			mechanism: "Vacuum makes dead space reusable inside a relation but never returns it, and a btree page is freed only once every entry on it is dead: new keys of a time-ordered index land at its right edge, so a page that keeps one long-lived entry is never reused. A churn table's heap and indexes then grow with its churn, not its live rows. The §2.4 dead-tuple line is fixed at 10M rows, so a queue of a few hundred live rows never reaches it while its indexes grow without bound; this line is a share of each relation's own size.",
			baseline:  fmt.Sprintf("Every heap, TOAST relation and index has under %.0f GB or under %.0f%% of its current size reclaimable, on its newest weekly measurement within %.0f days, less any shrink since.", settings.MinReclaimableBytes/1e9, 100*settings.MinReclaimableShare, settings.MaxRelationMeasurementAge.Hours()/24),
			observed: fmt.Sprintf("reclaimable_gb=%.1f relations=%d kinds=%s live_rows=%s dead_rows=%s dead_tuple_line=%d bloated=[%s]",
				gb(float64(reclaimableBytes)), len(relations), strings.Join(kinds, ","), counter(table.liveRows), counter(table.deadRows), settings.DeadTupleLine, strings.Join(parts, "; ")),
			evidence: evidence,
			context:  "False-positive qualifiers: the btree and statistics estimates are estimates (deduplicated btree entries make an index smaller than its estimate, stale statistics move both), and a measurement up to a week old keeps its share while the relation grows; a relation that shrank since is credited with the shrink. False-negative qualifiers: a relation is judged only on its newest measurement, a scan cycle longer than the window leaves relations unjudged, only the most bloated relations are listed, and growth since the last measurement is not counted. Live and dead rows are the table's current statistics counters.",
			action:   "For an index: confirm the hot-table vacuum and index rebuilds of PARTITION-MAINTENANCE-PLAN.md A1 and A2 run, then rebuild it online with REINDEX INDEX CONCURRENTLY through the maintenance rotation or the priority rebuild (db_maintenance.go). For a heap or TOAST relation: stop the writer making the dead versions (Part B) and confirm vacuum keeps up (§2.4, A1); the space is reused in place and returns to the filesystem only in a maintenance-window VACUUM FULL until PostgreSQL 19's online compaction (owner decision; never pg_repack). A table over 50 GB with persistent bloat goes to Part C or D, not to repeated compaction.",
			verify:   "The next weekly measurement, or the relation's shrink after a rebuild, takes every relation of the table under the line.",
			playbook: "SIGNALS.md §2.4k and PARTITION-MAINTENANCE-PLAN.md A4",
		})
	}
	return findings
}

// An unavailable observation is unknown for the measurement target.
func (self tableBloatProbe) unknown(target string, reason string, err error, detail string) finding {
	observed := "reason=" + reason + detail
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: tableBloatProbeId, tier: tierWarn, class: "table-bloat-unobservable", target: target, sustain: self.settings.UnobservableSustain,
		symptom:   "Reclaimable bloat is unknown (" + reason + ")",
		mechanism: "The primary inventory, its hostname and role guard, a bounded read or its fixed framing could not be established, the measurement table is missing, or the weekly measurement has not run recently, so whether any relation is bloated is unknown.",
		baseline:  fmt.Sprintf("One configured primary answers both guarded reads with complete fixed frames and a statement clock near the monitor's, and db_bloat_measurement holds a measurement under %.0f days old.", self.settings.MaxMeasurementAge.Hours()/24),
		observed:  observed,
		evidence:  "Only fixed reasons, ages and error classes are retained; SQL errors, relation contents and credentials stay private.",
		context:   "Unknown is not healthy: a bloat finding raised earlier stays open while this lasts. Before the migration that creates db_bloat_measurement the table is missing, and the first weekly run records the first measurement shortly after the deploy that seeds its chain.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the bounded primary read. For a missing or stale measurement, check the measure_db_bloat task (task canaries, [db]bloat log lines) and its deferrals; never compact by hand to clear this."),
		verify:    "A complete read finds a measurement under the age line.",
		playbook:  "SIGNALS.md §2.4k",
	}
}
