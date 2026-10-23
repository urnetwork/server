// Autovacuum passes on big relations that ended without completing. The
// progress view owns relation and pass identity, the relation's completion
// counter and timestamp own completion, and the database maintenance task's
// recorded runs own deliberate-maintenance overlap. A discarded pass in its
// index or late heap phase lost the work its last observed progress counts.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const pgAutovacuumDiscardProbeId = "pg/autovacuum-discard"

// SIGNALS.md §2.4a (pg-autovacuum-discard): relation/pass correlation,
// harmful late or big discards, maintenance overlap and lost work.
func NewPgAutovacuumDiscardSignal() Signal {
	return NewPgAutovacuumDiscardSignalWithSettings(DefaultPgAutovacuumDiscardSettings())
}

// The signal with explicit policy.
func NewPgAutovacuumDiscardSignalWithSettings(settings *PgAutovacuumDiscardSettings) Signal {
	return &signalAdapter{
		number: "2.4a", key: "pg-autovacuum-discard", name: "Discarded autovacuum passes",
		probe: &pgAutovacuumDiscardProbe{settings: settings},
	}
}

// The policy of SIGNALS.md §2.4a. Big-table size and the late-heap cutoff
// are initial policy, not a learned production baseline.
type PgAutovacuumDiscardSettings struct {
	// Normal sampling cadence and the whole-run budget.
	Cadence   time.Duration
	RunBudget time.Duration
	// The primary's statement clock may differ from the monitor's by this.
	ClockTolerance time.Duration
	// A discarded pass is harmful when its relation's heap is at least
	// BigRelationHeapBytes and it was last seen past heap scanning or with at
	// least LateHeapFraction of the heap scanned.
	BigRelationHeapBytes float64
	LateHeapFraction     float64
	// Bounds on the per-instance pass and discard tracking and on the census.
	MaxPasses   int
	MaxDiscards int
	MaxCensus   int
	// The database maintenance task function, how far back its finished
	// runs are read, and how many runs one read returns.
	MaintenanceFunction string
	MaintenanceLookback time.Duration
	MaxMaintenanceRuns  int
}

// The §2.4a defaults: the 100 GB relation line of §2.4g, half the heap
// scanned, and DbMaintenance runs over the last hour.
func DefaultPgAutovacuumDiscardSettings() *PgAutovacuumDiscardSettings {
	return &PgAutovacuumDiscardSettings{
		Cadence:              5 * time.Minute,
		RunBudget:            40 * time.Second,
		ClockTolerance:       30 * time.Second,
		BigRelationHeapBytes: 100 * 1000 * 1000 * 1000,
		LateHeapFraction:     0.5,
		MaxPasses:            64,
		MaxDiscards:          256,
		MaxCensus:            4096,
		MaintenanceFunction:  "github.com/urnetwork/server/taskworker/work.DbMaintenance",
		MaintenanceLookback:  time.Hour,
		MaxMaintenanceRuns:   32,
	}
}

// The identity of one autovacuum pass: its relation, worker and the worker's
// backend and query generations.
type pgAutovacuumPassKey struct {
	relationId     int64
	pid            int64
	backendStarted int64
	queryStarted   int64
}

// One pass as last observed.
type pgAutovacuumPass struct {
	key              pgAutovacuumPassKey
	mode             string
	phase            string
	heapTotal        int64
	heapScanned      int64
	heapVacuumed     int64
	indexCycles      int64
	indexesProcessed int64
	indexesTotal     int64
	heapBytes        int64
	autovacuumCount  int64
	seenAt           time.Time
}

// A relation's completion evidence.
type pgAutovacuumRelation struct {
	relationId      int64
	heapBytes       int64
	autovacuumCount int64
	lastAutovacuum  int64
}

// One recorded or running maintenance task window; a zero end is running.
type pgAutovacuumMaintenance struct {
	start int64
	end   int64
}

// One validated observation.
type pgAutovacuumObservation struct {
	host        string
	observed    time.Time
	started     int64
	databaseId  int64
	blockSize   int64
	passes      map[pgAutovacuumPassKey]pgAutovacuumPass
	relations   map[int64]pgAutovacuumRelation
	maintenance []pgAutovacuumMaintenance
	// the maintenance read reached its bound
	maintenanceOver bool
}

// The primary, postmaster start and database that tracked passes belong to.
type pgAutovacuumGeneration struct {
	host       string
	started    int64
	databaseId int64
}

// An open harmful discard: the lost pass and the completion baseline a later
// pass must pass to recover it.
type pgAutovacuumDiscard struct {
	pass        pgAutovacuumPass
	discardedBy time.Time
	maintenance string
	count       int
}

// The tracked state belongs to this signal instance; stateLock guards it.
// It is bounded by MaxPasses and MaxDiscards and starts empty at every
// watcher start, so a pass seen only by an earlier process is unknown.
type pgAutovacuumDiscardProbe struct {
	settings         *PgAutovacuumDiscardSettings
	stateLock        sync.Mutex
	generation       pgAutovacuumGeneration
	passes           map[pgAutovacuumPassKey]pgAutovacuumPass
	discards         map[int64]pgAutovacuumDiscard
	trackingOverflow bool
}

func (self *pgAutovacuumDiscardProbe) id() string               { return pgAutovacuumDiscardProbeId }
func (self *pgAutovacuumDiscardProbe) tier() string             { return tierWarn }
func (self *pgAutovacuumDiscardProbe) cadence() time.Duration   { return self.settings.Cadence }
func (self *pgAutovacuumDiscardProbe) runBudget() time.Duration { return self.settings.RunBudget }

const pgAutovacuumDiscardColumns = 15

// Tracked relation identities, so a tracked relation that shrank below the
// size line still reports its completion evidence.
func (self *pgAutovacuumDiscardProbe) trackedRelationIds() []int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	seen := map[int64]bool{}
	for key := range self.passes {
		seen[key.relationId] = true
	}
	for relationId := range self.discards {
		seen[relationId] = true
	}
	relationIds := make([]int64, 0, len(seen))
	for relationId := range seen {
		relationIds = append(relationIds, relationId)
	}
	sort.Slice(relationIds, func(i, j int) bool { return relationIds[i] < relationIds[j] })
	return relationIds
}

// One primary-guarded statement: current-database vacuum progress with
// worker generations, completion evidence for big and tracked relations, and
// maintenance task windows. Fifteen text cells per row; the end row binds
// the counts. Names, SQL and application text never leave SQL.
func (self *pgAutovacuumDiscardProbe) query(trackedRelationIds []int64) string {
	settings := self.settings
	tracked := make([]string, 0, len(trackedRelationIds))
	for _, relationId := range trackedRelationIds {
		tracked = append(tracked, strconv.FormatInt(relationId, 10))
	}
	function := strings.ReplaceAll(settings.MaintenanceFunction, "'", "''")
	lookback := strconv.FormatInt(int64(settings.MaintenanceLookback/time.Second), 10)
	empty := func(count int) string {
		return strings.TrimSuffix(strings.Repeat("'', ", count), ", ")
	}
	return "\\set monitor_primary_hostname `hostname -s 2>/dev/null || printf ' unavailable'`\n" + `
WITH source AS MATERIALIZED (
 SELECT :'monitor_primary_hostname'::text AS hostname,
        floor(extract(epoch FROM statement_timestamp()) * 1000000)::bigint AS observed_us,
        floor(extract(epoch FROM pg_postmaster_start_time()) * 1000000)::bigint AS started_us,
        (SELECT oid::bigint FROM pg_database WHERE datname = current_database()) AS database_oid,
        CASE WHEN pg_is_in_recovery() THEN '1' ELSE '0' END AS recovery,
        current_setting('block_size')::bigint AS block_size
), passes AS MATERIALIZED (
 SELECT v.relid::bigint AS relation_oid, v.pid::bigint AS pid,
        coalesce(floor(extract(epoch FROM a.backend_start) * 1000000)::bigint, 0) AS backend_started_us,
        coalesce(floor(extract(epoch FROM a.query_start) * 1000000)::bigint, 0) AS query_started_us,
        CASE WHEN a.backend_type = 'autovacuum worker' AND a.query LIKE 'autovacuum:% (to prevent wraparound)' THEN 'antiwrap'
             WHEN a.backend_type = 'autovacuum worker' AND a.query LIKE 'autovacuum:%' THEN 'ordinary'
             WHEN a.backend_type = 'client backend' THEN 'manual'
             ELSE 'unknown' END AS mode,
        CASE WHEN v.phase IN ('initializing', 'scanning heap', 'vacuuming indexes', 'vacuuming heap',
                              'cleaning up indexes', 'truncating heap', 'performing final cleanup') THEN v.phase
             ELSE 'unknown' END AS phase,
        coalesce(v.heap_blks_total, -1) AS heap_total, coalesce(v.heap_blks_scanned, -1) AS heap_scanned,
        coalesce(v.heap_blks_vacuumed, -1) AS heap_vacuumed,
        coalesce((to_jsonb(v)->>'index_vacuum_count')::bigint, -1) AS index_cycles,
        coalesce((to_jsonb(v)->>'indexes_processed')::bigint, -1) AS indexes_processed,
        coalesce((to_jsonb(v)->>'indexes_total')::bigint, -1) AS indexes_total,
        coalesce(pg_relation_size(v.relid), -1) AS heap_bytes,
        coalesce(st.autovacuum_count, -1) AS autovacuum_count
 FROM pg_stat_progress_vacuum v
 LEFT JOIN pg_stat_activity a ON a.pid = v.pid
 LEFT JOIN pg_stat_all_tables st ON st.relid = v.relid
 WHERE v.datid = (SELECT database_oid FROM source)
 ORDER BY v.relid, v.pid LIMIT ` + strconv.Itoa(settings.MaxPasses+1) + `
), relations AS MATERIALIZED (
 SELECT c.oid::bigint AS relation_oid, pg_relation_size(c.oid)::bigint AS heap_bytes,
        coalesce(st.autovacuum_count, -1)::bigint AS autovacuum_count,
        coalesce(floor(extract(epoch FROM st.last_autovacuum) * 1000000)::bigint, 0) AS last_autovacuum_us
 FROM pg_class c LEFT JOIN pg_stat_all_tables st ON st.relid = c.oid
 WHERE c.relkind IN ('r', 't', 'm')
   AND (pg_relation_size(c.oid) >= ` + strconv.FormatFloat(settings.BigRelationHeapBytes, 'f', 0, 64) +
		` OR c.oid = ANY(ARRAY[` + strings.Join(tracked, ",") + `]::oid[]))
 ORDER BY c.oid LIMIT ` + strconv.Itoa(settings.MaxCensus+1) + `
), maintenance AS MATERIALIZED (
 SELECT * FROM (
  SELECT floor(extract(epoch FROM f.run_start_time) * 1000000)::bigint AS start_us,
         floor(extract(epoch FROM f.run_end_time) * 1000000)::bigint AS end_us
  FROM finished_task f
  WHERE f.run_end_time >= (statement_timestamp() AT TIME ZONE 'UTC') - ` + lookback + ` * interval '1 second'
    AND f.function_name = '` + function + `'
  UNION ALL
  SELECT floor(extract(epoch FROM p.claim_time) * 1000000)::bigint, 0::bigint
  FROM pending_task p
  WHERE (statement_timestamp() AT TIME ZONE 'UTC') < p.release_time AND p.function_name = '` + function + `'
 ) windows ORDER BY start_us LIMIT ` + strconv.Itoa(settings.MaxMaintenanceRuns+1) + `
)
SELECT 'meta', s.hostname, s.observed_us::text, s.started_us::text, s.database_oid::text, s.recovery, s.block_size::text, ` + empty(8) + ` FROM source s
UNION ALL SELECT 'pass', p.relation_oid::text, p.pid::text, p.backend_started_us::text, p.query_started_us::text, p.mode, p.phase,
       p.heap_total::text, p.heap_scanned::text, p.heap_vacuumed::text, p.index_cycles::text, p.indexes_processed::text,
       p.indexes_total::text, p.heap_bytes::text, p.autovacuum_count::text FROM passes p
UNION ALL SELECT 'relation', r.relation_oid::text, r.heap_bytes::text, r.autovacuum_count::text, r.last_autovacuum_us::text, ` + empty(10) + ` FROM relations r
UNION ALL SELECT 'maintenance', m.start_us::text, m.end_us::text, ` + empty(12) + ` FROM maintenance m
UNION ALL SELECT 'end', (SELECT count(*) FROM passes)::text, (SELECT count(*) FROM relations)::text, (SELECT count(*) FROM maintenance)::text, ` + empty(11) + `;
`
}

// Fixed framing, the primary's identity and role, a near source clock and
// canonical numbers in their domains. Any gap is one fixed reason.
func (self *pgAutovacuumDiscardProbe) parse(rows []pgRow, primary string, now time.Time) (pgAutovacuumObservation, string) {
	settings := self.settings
	observation := pgAutovacuumObservation{passes: map[pgAutovacuumPassKey]pgAutovacuumPass{}, relations: map[int64]pgAutovacuumRelation{}}
	shortName, _, _ := strings.Cut(primary, ".")
	metas, ends := 0, 0
	var end pgRow
	number := func(raw string, minimum, maximum int64) (int64, bool) {
		return pgFreezeAgeInteger(raw, minimum, maximum)
	}
	for _, row := range rows {
		if len(row) != pgAutovacuumDiscardColumns {
			return observation, "invalid-frame-width"
		}
		for _, cell := range row {
			if len(cell) > 256 {
				return observation, "overbound-cell"
			}
		}
		switch row[0] {
		case "meta":
			metas++
			observedUs, okObserved := number(row[2], 1, math.MaxInt64)
			started, okStarted := number(row[3], 1, math.MaxInt64)
			databaseId, okDatabase := number(row[4], 1, math.MaxUint32)
			blockSize, okBlock := number(row[6], 1024, 32768)
			switch {
			case row[1] != shortName || shortName == "" || row[5] != "0":
				return observation, "source-identity-or-role-unavailable"
			case !okObserved || !okStarted || !okDatabase || !okBlock || started > observedUs:
				return observation, "invalid-source-frame"
			}
			observation.host, observation.started, observation.databaseId, observation.blockSize = primary, started, databaseId, blockSize
			observation.observed = time.UnixMicro(observedUs).UTC()
			if observation.observed.Sub(now).Abs() > settings.ClockTolerance {
				return observation, "source-clock-unavailable"
			}
		case "pass":
			values := map[int]int64{}
			for _, field := range []struct {
				index            int
				minimum, maximum int64
			}{
				{1, 1, math.MaxUint32}, {2, 1, math.MaxInt32}, {3, 0, math.MaxInt64}, {4, 0, math.MaxInt64},
				{7, -1, math.MaxInt64}, {8, -1, math.MaxInt64}, {9, -1, math.MaxInt64}, {10, -1, math.MaxInt64},
				{11, -1, math.MaxInt64}, {12, -1, math.MaxInt64}, {13, -1, math.MaxInt64}, {14, -1, math.MaxInt64},
			} {
				value, valid := number(row[field.index], field.minimum, field.maximum)
				if !valid {
					return observation, "invalid-pass-number"
				}
				values[field.index] = value
			}
			switch row[5] {
			case "antiwrap", "ordinary", "manual", "unknown":
			default:
				return observation, "invalid-pass-mode"
			}
			switch row[6] {
			case "unknown", "initializing", "scanning heap", "vacuuming indexes", "vacuuming heap", "cleaning up indexes", "truncating heap", "performing final cleanup":
			default:
				return observation, "invalid-pass-phase"
			}
			pass := pgAutovacuumPass{
				key:  pgAutovacuumPassKey{relationId: values[1], pid: values[2], backendStarted: values[3], queryStarted: values[4]},
				mode: row[5], phase: row[6], heapTotal: values[7], heapScanned: values[8], heapVacuumed: values[9],
				indexCycles: values[10], indexesProcessed: values[11], indexesTotal: values[12], heapBytes: values[13], autovacuumCount: values[14],
			}
			if (pass.heapTotal >= 0 && (pass.heapScanned > pass.heapTotal || pass.heapVacuumed > pass.heapTotal)) ||
				(pass.indexesTotal >= 0 && pass.indexesProcessed > pass.indexesTotal) {
				return observation, "inconsistent-pass-counters"
			}
			if _, duplicate := observation.passes[pass.key]; duplicate {
				return observation, "duplicate-pass"
			}
			observation.passes[pass.key] = pass
		case "relation":
			relationId, okId := number(row[1], 1, math.MaxUint32)
			heapBytes, okHeap := number(row[2], 0, math.MaxInt64)
			count, okCount := number(row[3], -1, math.MaxInt64)
			last, okLast := number(row[4], 0, math.MaxInt64)
			if !okId || !okHeap || !okCount || !okLast {
				return observation, "invalid-relation-number"
			}
			if _, duplicate := observation.relations[relationId]; duplicate {
				return observation, "duplicate-relation"
			}
			observation.relations[relationId] = pgAutovacuumRelation{relationId: relationId, heapBytes: heapBytes, autovacuumCount: count, lastAutovacuum: last}
		case "maintenance":
			start, okStart := number(row[1], 1, math.MaxInt64)
			finish, okEnd := number(row[2], 0, math.MaxInt64)
			if !okStart || !okEnd || (finish != 0 && finish < start) {
				return observation, "invalid-maintenance-window"
			}
			observation.maintenance = append(observation.maintenance, pgAutovacuumMaintenance{start: start, end: finish})
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
	passCount, okPasses := number(end[1], 0, math.MaxInt32)
	relationCount, okRelations := number(end[2], 0, math.MaxInt32)
	maintenanceCount, okMaintenance := number(end[3], 0, math.MaxInt32)
	if !okPasses || !okRelations || !okMaintenance || int(passCount) != len(observation.passes) ||
		int(relationCount) != len(observation.relations) || int(maintenanceCount) != len(observation.maintenance) {
		return observation, "incomplete-frame"
	}
	if len(observation.passes) > settings.MaxPasses || len(observation.relations) > settings.MaxCensus {
		return observation, "census-overbound"
	}
	if len(observation.maintenance) > settings.MaxMaintenanceRuns {
		observation.maintenance, observation.maintenanceOver = observation.maintenance[:settings.MaxMaintenanceRuns], true
	}
	return observation, ""
}

// Whether the last observed state of a lost pass was harmful to lose.
func (self *pgAutovacuumDiscardProbe) harmful(pass pgAutovacuumPass) bool {
	settings := self.settings
	if float64(pass.heapBytes) < settings.BigRelationHeapBytes {
		return false
	}
	switch pass.phase {
	case "vacuuming indexes", "vacuuming heap", "cleaning up indexes", "truncating heap", "performing final cleanup":
		return true
	case "scanning heap":
		return pass.heapTotal > 0 && pass.heapScanned >= 0 && float64(pass.heapScanned) >= settings.LateHeapFraction*float64(pass.heapTotal)
	}
	return false
}

// Whether a relation's evidence shows a pass completed after the given pass
// was last seen running: its completion counter grew, or its completion time
// is later. No pass completes on a relation while another vacuums it, so this
// both classifies an ended pass and recovers an open discard; the completion
// time still recovers one after a statistics reset.
func pgAutovacuumCompletedSince(pass pgAutovacuumPass, relation pgAutovacuumRelation) bool {
	return (pass.autovacuumCount >= 0 && relation.autovacuumCount > pass.autovacuumCount) ||
		relation.lastAutovacuum > pass.seenAt.UnixMicro()
}

// Deliberate maintenance overlapping the interval in which the pass ended.
func (self *pgAutovacuumDiscardProbe) maintenanceOverlap(observation pgAutovacuumObservation, from time.Time) string {
	settings := self.settings
	if from.Before(observation.observed.Add(-settings.MaintenanceLookback)) {
		return "unknown-window-older-than-lookback"
	}
	start, end := from.UnixMicro(), observation.observed.UnixMicro()
	for _, window := range observation.maintenance {
		if window.start <= end && (window.end == 0 || window.end >= start) {
			return "db-maintenance"
		}
	}
	if observation.maintenanceOver {
		return "unknown-maintenance-runs-overbound"
	}
	return "none-observed"
}

// Classify every tracked pass that ended, update the bounded tracking, and
// report open harmful discards until a later pass completes.
func (self *pgAutovacuumDiscardProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	primaries := env.cfg.hostsWithRole("pg-primary")
	if len(primaries) != 1 {
		return []finding{self.unknown("primary", "primary-inventory-unavailable-or-ambiguous", nil, 2)}, nil
	}
	primary := primaries[0]
	rows, err := env.runner.pg(ctx, self.query(self.trackedRelationIds()))
	if ctx.Err() != nil {
		if err != nil {
			return nil, &signalContextError{contextErr: ctx.Err(), sourceErr: err}
		}
		return nil, ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return nil, &signalContextError{contextErr: context.Canceled, sourceErr: err}
	}
	if err != nil {
		return []finding{self.unknown(primary.name, "source-unavailable", err, 2)}, nil
	}
	observation, reason := self.parse(rows, primary.name, env.now().UTC())
	if reason != "" {
		return []finding{self.unknown(primary.name, reason, nil, 2)}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return self.reduce(observation), nil
}

// The state transition for one complete observation.
func (self *pgAutovacuumDiscardProbe) reduce(observation pgAutovacuumObservation) []finding {
	settings := self.settings
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.passes == nil {
		self.passes = map[pgAutovacuumPassKey]pgAutovacuumPass{}
	}
	if self.discards == nil {
		self.discards = map[int64]pgAutovacuumDiscard{}
	}
	findings := []finding{}
	generation := pgAutovacuumGeneration{host: observation.host, started: observation.started, databaseId: observation.databaseId}
	unknownReasons := []string{}
	if self.generation != generation && len(self.passes) != 0 {
		// A restart or another database cannot classify passes it did not
		// run; open discards still recover by a later completion time.
		unknownReasons = append(unknownReasons, fmt.Sprintf("source-generation-changed unclassified_passes=%d", len(self.passes)))
		self.passes = map[pgAutovacuumPassKey]pgAutovacuumPass{}
	}
	self.generation = generation

	benign := 0
	passKeys := make([]pgAutovacuumPassKey, 0, len(self.passes))
	for key := range self.passes {
		passKeys = append(passKeys, key)
	}
	sort.Slice(passKeys, func(i, j int) bool {
		return passKeys[i].relationId < passKeys[j].relationId || (passKeys[i].relationId == passKeys[j].relationId && passKeys[i].queryStarted < passKeys[j].queryStarted)
	})
	for _, key := range passKeys {
		previous := self.passes[key]
		if _, running := observation.passes[key]; running {
			continue
		}
		delete(self.passes, key)
		relation, present := observation.relations[key.relationId]
		if !present || pgAutovacuumCompletedSince(previous, relation) {
			// A dropped relation or a completed pass lost nothing.
			continue
		}
		if !self.harmful(previous) {
			benign++
			continue
		}
		discard, open := self.discards[key.relationId]
		if !open && len(self.discards) >= settings.MaxDiscards {
			self.trackingOverflow = true
			continue
		}
		discard.pass, discard.discardedBy, discard.count = previous, observation.observed, discard.count+1
		discard.maintenance = self.maintenanceOverlap(observation, previous.seenAt)
		self.discards[key.relationId] = discard
	}
	for key, pass := range observation.passes {
		if pass.mode != "antiwrap" && pass.mode != "ordinary" {
			continue
		}
		if float64(pass.heapBytes) < settings.BigRelationHeapBytes {
			continue
		}
		if _, tracked := self.passes[key]; !tracked && len(self.passes) >= settings.MaxPasses {
			self.trackingOverflow = true
			continue
		}
		pass.seenAt = observation.observed
		self.passes[key] = pass
	}

	relationIds := make([]int64, 0, len(self.discards))
	for relationId := range self.discards {
		relationIds = append(relationIds, relationId)
	}
	sort.Slice(relationIds, func(i, j int) bool { return relationIds[i] < relationIds[j] })
	for _, relationId := range relationIds {
		discard := self.discards[relationId]
		target := fmt.Sprintf("%s/database-%d/relation-%d", observation.host, observation.databaseId, relationId)
		relation, present := observation.relations[relationId]
		if !present || pgAutovacuumCompletedSince(discard.pass, relation) {
			delete(self.discards, relationId)
			findings = append(findings, healthyFinding(pgAutovacuumDiscardProbeId, tierWarn, "pg-autovacuum-discard", target))
			continue
		}
		findings = append(findings, self.discardFinding(target, discard, observation, benign))
	}
	if self.trackingOverflow {
		unknownReasons = append(unknownReasons, "tracking-overbound")
	}
	if len(unknownReasons) != 0 {
		// A generation change is a one-run event: show it on its own run.
		findings = append(findings, self.unknown(observation.host, strings.Join(unknownReasons, ","), nil, 1))
	} else {
		findings = append(findings, healthyFinding(pgAutovacuumDiscardProbeId, tierWarn, "pg-autovacuum-discard-unobservable", observation.host))
	}
	return findings
}

// One open harmful discard, with its lost work and any replacement pass.
func (self *pgAutovacuumDiscardProbe) discardFinding(target string, discard pgAutovacuumDiscard, observation pgAutovacuumObservation, benign int) finding {
	settings := self.settings
	pass := discard.pass
	scanned := "unknown"
	lost := "unknown"
	if pass.heapScanned >= 0 && pass.heapTotal > 0 {
		scanned = strconv.FormatFloat(100*float64(pass.heapScanned)/float64(pass.heapTotal), 'f', 1, 64)
		lost = strconv.FormatFloat(gb(float64(pass.heapScanned)*float64(observation.blockSize)), 'f', 1, 64)
	}
	replacement := "none"
	for key, current := range observation.passes {
		if key.relationId == pass.key.relationId {
			replacement = fmt.Sprintf("%s phase=%q heap_scanned=%d heap_total=%d", current.mode, current.phase, current.heapScanned, current.heapTotal)
		}
	}
	return finding{
		probeId: pgAutovacuumDiscardProbeId, tier: tierWarn, class: "pg-autovacuum-discard", target: target, sustain: 1,
		symptom:   fmt.Sprintf("An autovacuum pass on a %.0f GB relation ended in %s without completing; its work is lost until a later pass completes", gb(float64(pass.heapBytes)), pass.phase),
		mechanism: "The pass's relation and worker generation left pg_stat_progress_vacuum while the relation's autovacuum_count stayed unchanged and last_autovacuum stayed before the pass began, so the pass ended without completing: a lock-conflict cancellation, a termination, an error or a crash. Its heap scan and index work up to that point are discarded and the next pass starts again from the beginning.",
		baseline:  fmt.Sprintf("Autovacuum passes on relations of at least %.0f GB complete; a pass is harmful to lose once it is past heap scanning or has scanned %.0f%% of the heap.", gb(settings.BigRelationHeapBytes), 100*settings.LateHeapFraction),
		observed: fmt.Sprintf("mode=%s last_phase=%q heap_scanned_pct_at_least=%s lost_heap_gb_at_least=%s heap_blocks_scanned=%d heap_blocks_total=%d indexes_processed=%d indexes_total=%d index_cycles=%d relation_heap_gb=%.1f maintenance_overlap=%s discard_window_s=%.0f discards=%d replacement_pass=%s other_small_or_early_discards=%d",
			pass.mode, pass.phase, scanned, lost, pass.heapScanned, pass.heapTotal, pass.indexesProcessed, pass.indexesTotal, pass.indexCycles,
			gb(float64(pass.heapBytes)), discard.maintenance, discard.discardedBy.Sub(pass.seenAt).Seconds(), discard.count, replacement, benign),
		evidence: "One primary-guarded read-only statement: current-database pg_stat_progress_vacuum joined with worker generations, pg_stat_all_tables completion evidence and the database maintenance task's recorded and claimed windows. The progress figures are the pass's last observation before it ended, so the lost work is a lower bound.",
		context:  "Correlate pg-autovacuum-cancel (§2.4e) records in the same window: a qualified cancellation record names a lock-conflict cancel, while none with complete log coverage points to a termination, an error or a crash. maintenance_overlap=db-maintenance means the daily DbMaintenance task ran in that window. False-positive qualifier: a statistics reset can hide a completion's counter, though the completion time still recovers it. False-negative qualifiers: a pass that starts and ends between two observations is never seen, and the true phase at the end can be later than the last observation.",
		action:   "Find the conflicting statement before changing anything: correlate the window with §2.4e cancellation records, DbMaintenance's SKIP_LOCKED vacuum and analyze and its REINDEX rotation, and any operator maintenance. Let the replacement pass finish; do not start conflicting maintenance on this relation, cancel the new pass, or retune autovacuum from this finding.",
		verify:   "The relation's autovacuum_count advances or last_autovacuum moves past the discarded pass on a later observation.",
		playbook: "SIGNALS.md §2.4a and §2.4e",
	}
}

// Missing evidence never certifies that no pass was lost.
func (self *pgAutovacuumDiscardProbe) unknown(target, reason string, err error, sustain int) finding {
	observed := "reason=" + reason
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: pgAutovacuumDiscardProbeId, tier: tierWarn, class: "pg-autovacuum-discard-unobservable", target: target, sustain: sustain,
		symptom:   "Whether big-relation autovacuum passes were discarded is unknown",
		mechanism: "The primary inventory, its guard, the bounded statement, its framing or this instance's bounded pass tracking could not be established, or a postmaster or database change left earlier passes unclassifiable.",
		baseline:  "One configured primary answers the guarded statement with a complete frame on every cadence, and tracking stays inside its bounds.",
		observed:  observed,
		evidence:  "Only fixed reasons and error classes are retained; relation names, SQL, application text and source errors stay private.",
		context:   "Unknown is not healthy. Open discard findings stay open until their relation's later pass completes.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the bounded primary read. A pass seen only before a restart of this watcher or the postmaster cannot be classified; follow the relation's next pass instead."),
		verify:    "Complete observations resume and tracked passes are classified on the next runs.",
		playbook:  "SIGNALS.md §2.4a",
	}
}
