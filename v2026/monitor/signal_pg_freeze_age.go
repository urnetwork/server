// Read complete current-database freeze age and positively observed vacuum
// progress. SIGNALS.md §2.4d leaves cancellation/lost-work coverage in §2.4a.
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

const (
	pgFreezeAgeRelationLimit  = 4096
	pgFreezeAgeColumns        = 18
	pgFreezeAgeCadence        = 15 * time.Minute
	pgFreezeAgeClockTolerance = 30 * time.Second
)

// The approved policy warns on age risk, never inferred stalled work.
func NewPgFreezeAgeSignal() Signal {
	return &signalAdapter{
		number: "2.4d", key: "pg-freeze-age", name: "PostgreSQL freeze-age risk",
		probe: &pgFreezeAgeProbe{},
	}
}

// Progress state is finite and belongs to this signal instance. Previously risky
// targets stay tracked across recovery readings; a vanished pass cannot certify recovery.
type pgFreezeAgeProbe struct {
	stateLock        sync.Mutex
	previous         *pgFreezeAgeObservation
	riskTargets      map[string]bool
	trackingOverflow bool
}

func (self *pgFreezeAgeProbe) id() string               { return "pg/freeze-age" }
func (self *pgFreezeAgeProbe) tier() string             { return tierWarn }
func (self *pgFreezeAgeProbe) cadence() time.Duration   { return pgFreezeAgeCadence }
func (self *pgFreezeAgeProbe) runBudget() time.Duration { return 40 * time.Second }

// Every record has eighteen cells. Header count covers all r/t relations in
// this database; the trailer covers the bounded projection. A prefix, empty
// response, duplicate row or count over the cap cannot establish health.
// Unknown version-specific progress fields use -1, never fabricated zero.
const pgFreezeAgeQuery = "\\set monitor_primary_hostname `hostname -s 2>/dev/null || printf ' unavailable'`\n" + `
WITH source AS MATERIALIZED (
 SELECT :'monitor_primary_hostname'::text AS hostname,
        floor(extract(epoch FROM statement_timestamp()) * 1000000)::bigint AS observed_us,
        floor(extract(epoch FROM pg_postmaster_start_time()) * 1000000)::bigint AS started_us,
        (SELECT oid::bigint FROM pg_database WHERE datname = current_database()) AS database_oid,
        CASE WHEN pg_is_in_recovery() THEN '1' ELSE '0' END AS recovery,
        current_setting('autovacuum_freeze_max_age')::bigint AS freeze_limit,
        current_setting('vacuum_failsafe_age')::bigint AS failsafe_age
), relations AS MATERIALIZED (
 SELECT c.oid::bigint AS relation_oid, age(c.relfrozenxid)::bigint AS xid_age,
        c.relfrozenxid::text AS frozen_xid, c.relkind::text AS relation_kind,
        least(s.freeze_limit, coalesce((
          SELECT option_value::bigint FROM pg_options_to_table(c.reloptions)
          WHERE option_name = 'autovacuum_freeze_max_age'
        ), s.freeze_limit)) AS freeze_limit,
        coalesce(floor(extract(epoch FROM st.last_autovacuum) * 1000000)::bigint, 0) AS last_autovacuum_us
 FROM pg_class c CROSS JOIN source s
 LEFT JOIN pg_stat_all_tables st ON st.relid = c.oid
 WHERE c.relkind IN ('r', 't')
), bounded AS MATERIALIZED (
 SELECT * FROM relations ORDER BY relation_oid LIMIT 4096
), projected AS MATERIALIZED (
 SELECT r.*, coalesce(v.pid, 0)::bigint AS pid,
        coalesce(floor(extract(epoch FROM a.backend_start) * 1000000)::bigint, 0) AS backend_started_us,
        coalesce(floor(extract(epoch FROM a.query_start) * 1000000)::bigint, 0) AS query_started_us,
        CASE WHEN v.pid IS NULL THEN 'none'
             WHEN a.backend_type = 'autovacuum worker' AND a.query LIKE 'autovacuum:% (to prevent wraparound)' THEN 'antiwrap'
             WHEN a.backend_type = 'autovacuum worker' AND a.query LIKE 'autovacuum:%' THEN 'ordinary'
             ELSE 'unknown' END AS mode,
        CASE WHEN v.pid IS NULL THEN 'none'
             WHEN v.phase IN ('initializing', 'scanning heap', 'vacuuming indexes', 'vacuuming heap',
                              'cleaning up indexes', 'truncating heap', 'performing final cleanup') THEN v.phase
             ELSE 'unknown' END AS phase,
        CASE WHEN v.pid IS NULL THEN 0 ELSE coalesce(v.heap_blks_total, -1) END AS heap_total,
        CASE WHEN v.pid IS NULL THEN 0 ELSE coalesce(v.heap_blks_scanned, -1) END AS heap_scanned,
        CASE WHEN v.pid IS NULL THEN 0 ELSE coalesce(v.heap_blks_vacuumed, -1) END AS heap_vacuumed,
        CASE WHEN v.pid IS NULL THEN 0 ELSE coalesce((to_jsonb(v)->>'index_vacuum_count')::bigint, -1) END AS index_cycles,
        CASE WHEN v.pid IS NULL THEN 0 ELSE coalesce((to_jsonb(v)->>'indexes_processed')::bigint, -1) END AS indexes_processed,
        CASE WHEN v.pid IS NULL THEN 0 ELSE coalesce((to_jsonb(v)->>'indexes_total')::bigint, -1) END AS indexes_total
 FROM bounded r
 LEFT JOIN pg_stat_progress_vacuum v ON v.datid = (SELECT database_oid FROM source) AND v.relid = r.relation_oid
 LEFT JOIN pg_stat_activity a ON a.pid = v.pid
 ORDER BY r.relation_oid LIMIT 4096
), frames AS (
 SELECT 0 AS ordering, 0::bigint AS relation_order,
        'meta'::text AS kind, s.hostname AS c1, s.observed_us::text AS c2,
        s.started_us::text AS c3, s.database_oid::text AS c4, s.recovery AS c5,
        s.freeze_limit::text AS c6, s.failsafe_age::text AS c7,
        (SELECT count(*) FROM relations)::text AS c8, '1'::text AS c9,
        ''::text AS c10, ''::text AS c11, ''::text AS c12, ''::text AS c13,
        ''::text AS c14, ''::text AS c15, ''::text AS c16, ''::text AS c17
 FROM source s
 UNION ALL
 SELECT 1, p.relation_oid, 'relation', p.relation_oid::text, p.xid_age::text,
        p.freeze_limit::text, p.pid::text, p.backend_started_us::text, p.query_started_us::text,
        p.mode, p.phase, p.heap_total::text, p.heap_scanned::text, p.heap_vacuumed::text,
        p.index_cycles::text, p.indexes_processed::text, p.indexes_total::text,
        p.last_autovacuum_us::text, p.frozen_xid, p.relation_kind
 FROM projected p
 UNION ALL
 SELECT 2, 0, 'end', (SELECT count(*) FROM projected)::text, '4096',
        '', '', '', '', '', '', '', '', '', '', '', '', '', '', ''
)
SELECT kind, c1, c2, c3, c4, c5, c6, c7, c8, c9, c10, c11, c12, c13, c14, c15, c16, c17
FROM frames ORDER BY ordering, relation_order;
`

// Timestamp identities preserve PostgreSQL microsecond precision.
// All identities are source-owned integers; relation/query/application names
// never leave SQL. Pass identity includes both backend and query generations.
type pgFreezeAgeRelation struct {
	relationId       int64
	xidAge           int64
	freezeLimit      int64
	pid              int64
	backendStarted   int64
	queryStarted     int64
	mode             string
	phase            string
	heapTotal        int64
	heapScanned      int64
	heapVacuumed     int64
	indexCycles      int64
	indexesProcessed int64
	indexesTotal     int64
	lastAutovacuum   int64
	frozenXid        int64
	kind             string
}

type pgFreezeAgeObservation struct {
	host        string
	observed    int64
	started     int64
	databaseId  int64
	freezeLimit int64
	failsafeAge int64
	relations   map[int64]pgFreezeAgeRelation
}

// Syntax and domains are separate: the only negative sentinel is progress -1.
func pgFreezeAgeInteger(raw string, minimum, maximum int64) (int64, bool) {
	if raw == "" || len(raw) > 20 {
		return 0, false
	}
	for i, digit := range raw {
		if digit == '-' && i == 0 && minimum < 0 {
			continue
		}
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	return value, err == nil && minimum <= value && value <= maximum
}

// No top-N subset is ever complete. Source clock tolerance follows the
// existing direct-primary reader; all public failures use fixed reasons.
func parsePgFreezeAge(rows []pgRow, primary string, now time.Time) (*pgFreezeAgeObservation, string) {
	if len(rows) < 3 || len(rows) > pgFreezeAgeRelationLimit+2 {
		return nil, "incomplete-or-overbound-frame"
	}
	for _, row := range rows {
		if len(row) != pgFreezeAgeColumns {
			return nil, "invalid-frame-width"
		}
		for _, cell := range row {
			if len(cell) > 256 {
				return nil, "overbound-cell"
			}
		}
	}
	meta, end := rows[0], rows[len(rows)-1]
	shortName, _, _ := strings.Cut(primary, ".")
	if meta[0] != "meta" || meta[1] != shortName || shortName == "" || meta[5] != "0" || meta[9] != "1" || end[0] != "end" {
		return nil, "source-identity-or-framing-unavailable"
	}
	for _, value := range append(append([]string{}, meta[10:]...), end[3:]...) {
		if value != "" {
			return nil, "unexpected-frame-field"
		}
	}
	stamp, okStamp := pgFreezeAgeInteger(meta[2], 1, math.MaxInt64)
	started, okStarted := pgFreezeAgeInteger(meta[3], 1, stamp)
	databaseId, okDatabase := pgFreezeAgeInteger(meta[4], 1, math.MaxUint32)
	freeze, okFreeze := pgFreezeAgeInteger(meta[6], 1, math.MaxInt32)
	failsafe, okFailsafe := pgFreezeAgeInteger(meta[7], 0, math.MaxInt32)
	count, okCount := pgFreezeAgeInteger(meta[8], 1, pgFreezeAgeRelationLimit)
	returned, okReturned := pgFreezeAgeInteger(end[1], 1, pgFreezeAgeRelationLimit)
	if !okStamp || !okStarted || !okDatabase || !okFreeze || !okFailsafe || !okCount || !okReturned ||
		end[2] != "4096" || count != returned || int(count) != len(rows)-2 {
		return nil, "invalid-or-incomplete-census"
	}
	tolerance := pgFreezeAgeClockTolerance.Microseconds()
	if stamp < now.UnixMicro()-tolerance || stamp > now.UnixMicro()+tolerance {
		return nil, "source-clock-unavailable"
	}
	observation := &pgFreezeAgeObservation{host: primary, observed: stamp, started: started,
		databaseId: databaseId, freezeLimit: freeze, failsafeAge: failsafe, relations: map[int64]pgFreezeAgeRelation{}}
	for _, row := range rows[1 : len(rows)-1] {
		if row[0] != "relation" || (row[17] != "r" && row[17] != "t") {
			return nil, "invalid-relation-frame"
		}
		values := map[int]int64{}
		for _, field := range []struct {
			index            int
			minimum, maximum int64
		}{
			{1, 1, math.MaxUint32}, {2, 0, math.MaxInt32}, {3, 1, freeze},
			{4, 0, math.MaxInt32}, {5, 0, stamp}, {6, 0, stamp},
			{9, -1, math.MaxInt64}, {10, -1, math.MaxInt64}, {11, -1, math.MaxInt64},
			{12, -1, math.MaxInt64}, {13, -1, math.MaxInt64}, {14, -1, math.MaxInt64},
			{15, 0, stamp}, {16, 1, math.MaxUint32},
		} {
			value, valid := pgFreezeAgeInteger(row[field.index], field.minimum, field.maximum)
			if !valid {
				return nil, "invalid-relation-number"
			}
			values[field.index] = value
		}
		relation := pgFreezeAgeRelation{relationId: values[1], xidAge: values[2], freezeLimit: values[3],
			pid: values[4], backendStarted: values[5], queryStarted: values[6], mode: row[7], phase: row[8],
			heapTotal: values[9], heapScanned: values[10], heapVacuumed: values[11], indexCycles: values[12],
			indexesProcessed: values[13], indexesTotal: values[14], lastAutovacuum: values[15], frozenXid: values[16], kind: row[17]}
		if _, duplicate := observation.relations[relation.relationId]; duplicate {
			return nil, "duplicate-relation"
		}
		if relation.mode != "none" && relation.mode != "antiwrap" && relation.mode != "ordinary" && relation.mode != "unknown" {
			return nil, "invalid-vacuum-mode"
		}
		switch relation.phase {
		case "none", "unknown", "initializing", "scanning heap", "vacuuming indexes", "vacuuming heap", "cleaning up indexes", "truncating heap", "performing final cleanup":
		default:
			return nil, "invalid-vacuum-phase"
		}
		if relation.pid == 0 {
			if relation.mode != "none" || relation.phase != "none" || relation.backendStarted != 0 || relation.queryStarted != 0 ||
				relation.heapTotal != 0 || relation.heapScanned != 0 || relation.heapVacuumed != 0 || relation.indexCycles != 0 || relation.indexesProcessed != 0 || relation.indexesTotal != 0 {
				return nil, "inconsistent-absent-pass"
			}
		} else if relation.mode == "none" || relation.phase == "none" ||
			(relation.backendStarted != 0 && relation.backendStarted < started) ||
			(relation.queryStarted != 0 && relation.queryStarted < relation.backendStarted) {
			return nil, "inconsistent-pass-generation"
		}
		if (relation.heapTotal >= 0 && (relation.heapScanned > relation.heapTotal || relation.heapVacuumed > relation.heapTotal)) ||
			(relation.indexesTotal >= 0 && relation.indexesProcessed > relation.indexesTotal) {
			return nil, "inconsistent-progress-counters"
		}
		observation.relations[relation.relationId] = relation
	}
	return observation, ""
}

// Only positive work proves advancement. Equal counters during a long index
// phase, a phase name alone, resets and a new pass remain unconfirmed.
func pgFreezeAgeAdvancing(before, after pgFreezeAgeRelation) bool {
	if before.mode != "antiwrap" || after.mode != "antiwrap" || before.relationId != after.relationId ||
		before.pid <= 0 || before.pid != after.pid || before.backendStarted <= 0 || before.backendStarted != after.backendStarted ||
		before.queryStarted <= 0 || before.queryStarted != after.queryStarted {
		return false
	}
	if before.heapTotal < 0 || before.heapTotal != after.heapTotal ||
		before.heapScanned < 0 || before.heapVacuumed < 0 ||
		after.heapScanned < before.heapScanned || after.heapVacuumed < before.heapVacuumed {
		return false
	}
	if before.indexCycles >= 0 && after.indexCycles > before.indexCycles {
		return true
	}
	if before.phase != after.phase || before.indexCycles != after.indexCycles ||
		(before.indexesProcessed >= 0 && after.indexesProcessed < before.indexesProcessed) {
		return false
	}
	switch after.phase {
	case "scanning heap":
		return before.heapScanned >= 0 && after.heapScanned > before.heapScanned
	case "vacuuming heap":
		return before.heapVacuumed >= 0 && after.heapVacuumed > before.heapVacuumed
	case "vacuuming indexes", "cleaning up indexes":
		return before.indexesTotal > 0 && before.indexesTotal == after.indexesTotal &&
			before.indexesProcessed >= 0 && after.indexesProcessed > before.indexesProcessed
	default:
		return false
	}
}

// All errors and invalid frames break progress continuity but preserve open
// previously observed age-risk targets. Cancellation retains both parent and observed source cause.
func (self *pgFreezeAgeProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		self.resetProgress()
		return nil, err
	}
	primaries := env.cfg.hostsWithRole("pg-primary")
	if len(primaries) != 1 {
		self.resetProgress()
		return []finding{pgFreezeAgeUnknown("primary", "primary-inventory-unavailable-or-ambiguous", nil)}, nil
	}
	primary := primaries[0]
	rows, err := env.runner.pg(ctx, pgFreezeAgeQuery)
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		self.resetProgress()
		contextErr := ctx.Err()
		if contextErr == nil {
			contextErr = context.Canceled
		}
		if err != nil {
			return nil, &signalContextError{contextErr: contextErr, sourceErr: err}
		}
		return nil, contextErr
	}
	if err != nil {
		self.resetProgress()
		return []finding{pgFreezeAgeUnknown(primary.name, "source-unavailable", err)}, nil
	}
	observation, reason := parsePgFreezeAge(rows, primary.name, env.now())
	if err := ctx.Err(); err != nil {
		self.resetProgress()
		return nil, err
	}
	if reason != "" {
		self.resetProgress()
		return []finding{pgFreezeAgeUnknown(primary.name, reason, nil)}, nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := ctx.Err(); err != nil {
		self.previous = nil
		return nil, err
	}
	if self.previous != nil && self.previous.host == observation.host && self.previous.started == observation.started &&
		self.previous.databaseId == observation.databaseId && observation.observed <= self.previous.observed {
		self.previous = nil
		return []finding{pgFreezeAgeUnknown(primary.name, "nonadvancing-source-clock", nil)}, nil
	}
	if self.riskTargets == nil {
		self.riskTargets = map[string]bool{}
	}
	comparable := self.previous != nil && self.previous.host == observation.host && self.previous.started == observation.started &&
		self.previous.databaseId == observation.databaseId && observation.observed > self.previous.observed &&
		observation.observed-self.previous.observed <= (pgFreezeAgeCadence+2*pgFreezeAgeClockTolerance).Microseconds()
	findings := []finding{}
	present := map[string]bool{}
	relationIds := make([]int64, 0, len(observation.relations))
	for relationId := range observation.relations {
		relationIds = append(relationIds, relationId)
	}
	sort.Slice(relationIds, func(i, j int) bool { return relationIds[i] < relationIds[j] })
	for _, relationId := range relationIds {
		relation := observation.relations[relationId]
		target := fmt.Sprintf("%s/database-%d/relation-%d", primary.name, observation.databaseId, relationId)
		present[target] = true
		advancing := false
		if comparable {
			if previous, found := self.previous.relations[relationId]; found {
				advancing = pgFreezeAgeAdvancing(previous, relation)
			}
		}
		if relation.xidAge <= relation.freezeLimit || advancing {
			findings = append(findings, healthyFinding(self.id(), tierWarn, "pg-freeze-age-risk", target))
			continue
		}
		if len(self.riskTargets) < pgFreezeAgeRelationLimit || self.riskTargets[target] {
			self.riskTargets[target] = true
		} else {
			self.trackingOverflow = true
		}
		progress := "advancement-unconfirmed"
		if relation.pid == 0 {
			progress = "no-pass-observed"
		}
		findings = append(findings, finding{
			probeId: self.id(), tier: tierWarn, class: "pg-freeze-age-risk", target: target, sustain: 2,
			symptom:   "A relation exceeds its configured autovacuum freeze-age limit without confirmed anti-wraparound advancement",
			mechanism: "The measured transaction-ID age is above the effective configured freeze limit. Positive work in the same identified anti-wraparound pass has not been established; this is age risk, not a diagnosis of stalled or canceled vacuum work.",
			baseline:  "Every eligible relation in the configured database is at or below its effective freeze limit, or a same-pass anti-wraparound work counter positively advances between fresh complete observations.",
			observed:  fmt.Sprintf("xid_age=%d freeze_limit=%d configured_failsafe_age=%d progress=%s phase=%s", relation.xidAge, relation.freezeLimit, observation.failsafeAge, progress, relation.phase),
			evidence:  fmt.Sprintf("relation_count=%d pass_mode=%s heap_scanned=%d heap_total=%d heap_vacuumed=%d index_cycles=%d indexes_processed=%d indexes_total=%d", len(observation.relations), relation.mode, relation.heapScanned, relation.heapTotal, relation.heapVacuumed, relation.indexCycles, relation.indexesProcessed, relation.indexesTotal),
			context:   "Long index work can leave counters unchanged while useful work continues. Phase changes, a new backend/pass, a missing version field, counter resets or pass disappearance do not prove a stall or recovery. The configured failsafe value is context, not proof that failsafe mode is active. Coverage is this database only; cancellation and lost-work attribution remain unimplemented in §2.4a.",
			action:    "Have the PostgreSQL owner confirm the active pass, freeze settings and source completeness. Let independently progressing work finish; do not cancel a vacuum, queue conflicting maintenance, change freeze settings or restart the primary from this risk observation.",
			verify:    "A complete fresh same-primary census reports this relation at or below its effective freeze limit, or two complete same-generation observations establish positive work in the same anti-wraparound pass. Missing source, missing relation or a vanished pass is not recovery.",
			playbook:  "SIGNALS.md §2.4d and §2.4a",
		})
	}
	for target := range self.riskTargets {
		if !present[target] {
			findings = append(findings, pgFreezeAgeUnknown(primary.name, "previous-risk-target-unobserved", nil))
			break
		}
	}
	if self.trackingOverflow {
		findings = append(findings, pgFreezeAgeUnknown(primary.name, "risk-tracking-overbound", nil))
	}
	self.previous = observation
	return findings, nil
}

// A source gap never bridges two otherwise compatible progress snapshots.
func (self *pgFreezeAgeProbe) resetProgress() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.previous = nil
}

// Visibility protects every previous risk ticket from absence-based recovery.
func pgFreezeAgeUnknown(target, reason string, err error) finding {
	observed := "reason=" + reason
	if err != nil {
		observed += " error_class=" + classifyObservationError(err)
	}
	return finding{
		probeId: "pg/freeze-age", tier: tierWarn, class: "pg-freeze-age-unobservable", target: target, sustain: 2,
		symptom:   "PostgreSQL freeze-age coverage or progress continuity is unknown",
		mechanism: "A missing, failed, stale, overbound, ambiguous or incomplete source cannot establish the absence of freeze-age risk.",
		baseline:  "One configured primary supplies a complete bounded, uniquely framed current-database relation census and supported source-owned fields.",
		observed:  observed,
		evidence:  "Only fixed reasons and error classes are retained; raw SQL, source cells, relation names, errors and credentials are withheld.",
		action:    "Restore the bounded primary observation and verify database scope and supported progress fields. Retain existing age-risk findings and the independent vacuum-health findings.",
		verify:    "A complete fresh same-primary census restores source authority; only a below-limit age or positively observed same-pass anti-wraparound progress can clear an age-risk target.",
		playbook:  "SIGNALS.md §2.4d",
	}
}
