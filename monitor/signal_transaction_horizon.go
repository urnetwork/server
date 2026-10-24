// SIGNALS.md §2.4l (transaction-horizon): the oldest transaction horizon, the
// point after which no vacuum in the database may remove a row version, is a
// signal of its own (server PARTITION-MAINTENANCE-PLAN.md A6), not only context
// for the §2.4 dead-tuple warning. Every holder counts: sessions (backend_xid,
// backend_xmin), replication slots (xmin, catalog_xmin) and prepared
// transactions, the last two counted even when there are none so a future one
// is caught. The horizon warns once it is 15 minutes old and pages at an hour,
// naming its holder ranked as §2.4 ranks horizon candidates. Query text,
// application names, client addresses, role names and prepared-transaction
// identifiers never leave the database: the frame carries numbers and fixed
// classifications only.
//
// A holder's age is a lower bound from two sources. A client backend's
// snapshot is no newer than its current or last statement, a prepared
// transaction is as old as its prepare, and an inactive slot has held its
// xmin at least since it went inactive. A holder with no such timestamp (an
// active slot, a walsender relaying a standby's feedback) is dated by the
// probe's own history of the next transaction id: the horizon id was assigned
// before the first sample whose next id passed it. Ids are ordered, so any
// holder's bound also bounds the oldest horizon.
package monitor

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const transactionHorizonProbeId = "pg/transaction-horizon"

func NewTransactionHorizonSignal() Signal {
	return NewTransactionHorizonSignalWithSettings(DefaultTransactionHorizonSettings())
}

// The signal with explicit thresholds.
func NewTransactionHorizonSignalWithSettings(settings *TransactionHorizonSettings) Signal {
	return &signalAdapter{
		number: "2.4l", key: "transaction-horizon", name: "Transaction horizon age",
		probe: &transactionHorizonProbe{settings: settings},
	}
}

// The thresholds and bounds of SIGNALS.md §2.4l.
type TransactionHorizonSettings struct {
	Cadence time.Duration
	// Warn once the oldest horizon is at least this old: the application
	// role's transaction_timeout, so only a holder outside that bound gets here.
	WarnAge time.Duration
	// Page once the oldest horizon is at least this old.
	PageAge time.Duration
	// The next-id history kept to date holders without a timestamp; a
	// non-positive value keeps none. It must reach past PageAge to page on one.
	HistoryWindow time.Duration
	// The holders the frame names, in rank order; the census counts them all.
	MaxHolders int
	// The longest text cell the frame may carry.
	MaxCellBytes int
	// The primary's statement clock may differ from the monitor's by this.
	ClockTolerance time.Duration
	// The whole-run budget of the read.
	RunBudget time.Duration
	Sustain   int
}

func DefaultTransactionHorizonSettings() *TransactionHorizonSettings {
	return &TransactionHorizonSettings{
		Cadence:        5 * time.Minute,
		WarnAge:        15 * time.Minute,
		PageAge:        time.Hour,
		HistoryWindow:  75 * time.Minute,
		MaxHolders:     5,
		MaxCellBytes:   64,
		ClockTolerance: 30 * time.Second,
		RunBudget:      40 * time.Second,
		Sustain:        1,
	}
}

// Every frame row has the tag and fourteen cells.
const transactionHorizonColumns = 15

// One frame per read: the primary's identity, clock and next transaction id;
// the ranked holders; and an end row whose census binds the rows. Holders are
// ranked as §2.4 ranks horizon candidates: the oldest combined horizon first,
// then the oldest transaction start. A lazy VACUUM (reported by
// pg_stat_progress_vacuum) and an autovacuum worker are not holders, since
// every other vacuum ignores their snapshots, and a session of another
// database holds none of this database's tables.
const transactionHorizonTemplate = `{hostname_guard}
WITH source AS MATERIALIZED (
 SELECT :'monitor_primary_hostname'::text AS hostname,
        floor(extract(epoch FROM statement_timestamp()) * 1000000)::bigint AS observed_us,
        CASE WHEN pg_is_in_recovery() THEN '1' ELSE '0' END AS recovery,
        pg_snapshot_xmax(pg_current_snapshot())::text AS next_xid,
        current_setting('max_prepared_transactions') AS max_prepared,
        (SELECT oid FROM pg_database WHERE datname = current_database()) AS database_oid
), vacuuming AS MATERIALIZED (
 SELECT pid FROM pg_stat_progress_vacuum
), session_holder AS MATERIALIZED (
 SELECT 'session'::text AS kind,
        a.pid::bigint AS pid,
        a.backend_type::text AS backend_type,
        coalesce(a.state, '') AS state,
        CASE WHEN r.oid IS NULL THEN 'none' WHEN r.rolsuper THEN 'superuser' ELSE 'ordinary' END AS role_kind,
        CASE
         WHEN a.backend_type = 'walsender' THEN 'replication'
         WHEN lower(ltrim(a.query)) LIKE 'reindex%' THEN 'reindex'
         WHEN lower(ltrim(a.query)) LIKE 'create%index%' THEN 'index-build'
         WHEN lower(ltrim(a.query)) LIKE 'vacuum%' THEN 'vacuum'
         WHEN lower(ltrim(a.query)) LIKE 'cluster%' THEN 'cluster'
         WHEN lower(ltrim(a.query)) LIKE 'analyze%' THEN 'analyze'
         WHEN lower(ltrim(a.query)) LIKE 'alter table%validate constraint%' THEN 'validate'
         WHEN lower(ltrim(a.query)) LIKE 'copy %' AND lower(a.query) LIKE '% to stdout%' THEN 'dump'
         ELSE 'application'
        END AS shape,
        coalesce(age(a.backend_xid), -1)::bigint AS xid_age,
        coalesce(age(a.backend_xmin), -1)::bigint AS xmin_age,
        coalesce(floor(extract(epoch FROM a.xact_start) * 1000000)::bigint, 0) AS xact_start_us,
        coalesce(floor(extract(epoch FROM coalesce(a.xact_start, a.query_start)) * 1000000)::bigint, 0) AS started_us,
        CASE WHEN a.backend_xmin IS NOT NULL AND a.backend_type IN ('client backend', 'parallel worker')
             THEN coalesce(floor(extract(epoch FROM a.query_start) * 1000000)::bigint, 0)
             ELSE 0 END AS bound_us,
        ''::text AS slot_name,
        ''::text AS slot_type
 FROM pg_stat_activity a
 CROSS JOIN source s
 LEFT JOIN pg_roles r ON r.oid = a.usesysid
 WHERE (a.backend_xid IS NOT NULL OR a.backend_xmin IS NOT NULL)
   AND a.backend_type <> 'autovacuum worker'
   AND a.pid <> pg_backend_pid()
   AND a.pid NOT IN (SELECT pid FROM vacuuming)
   AND (a.datid IS NULL OR a.datid = s.database_oid)
), slot_holder AS MATERIALIZED (
 SELECT 'slot'::text, coalesce(slot.active_pid, 0)::bigint, ''::text,
        CASE WHEN slot.active THEN 'active' ELSE 'inactive' END, ''::text, 'replication-slot'::text,
        coalesce(age(slot.xmin), -1)::bigint, coalesce(age(slot.catalog_xmin), -1)::bigint,
        0::bigint,
        coalesce(floor(extract(epoch FROM slot.inactive_since) * 1000000)::bigint, 0),
        CASE WHEN slot.active THEN 0
             ELSE coalesce(floor(extract(epoch FROM slot.inactive_since) * 1000000)::bigint, 0) END,
        slot.slot_name::text, coalesce(slot.slot_type, '')
 FROM pg_replication_slots slot
 WHERE slot.xmin IS NOT NULL OR slot.catalog_xmin IS NOT NULL
), prepared_holder AS MATERIALIZED (
 SELECT 'prepared'::text, 0::bigint, ''::text, ''::text,
        CASE WHEN r.oid IS NULL THEN 'none' WHEN r.rolsuper THEN 'superuser' ELSE 'ordinary' END,
        'prepared-transaction'::text,
        age(p.transaction)::bigint, -1::bigint,
        floor(extract(epoch FROM p.prepared) * 1000000)::bigint,
        floor(extract(epoch FROM p.prepared) * 1000000)::bigint,
        floor(extract(epoch FROM p.prepared) * 1000000)::bigint,
        ''::text, ''::text
 FROM pg_prepared_xacts p
 LEFT JOIN pg_roles r ON r.rolname = p.owner
), holder AS MATERIALIZED (
 SELECT * FROM session_holder
 UNION ALL SELECT * FROM slot_holder
 UNION ALL SELECT * FROM prepared_holder
), ranked AS MATERIALIZED (
 SELECT holder.*,
        row_number() OVER (
         ORDER BY greatest(holder.xid_age, holder.xmin_age) DESC,
                  CASE WHEN holder.started_us = 0 THEN 1 ELSE 0 END,
                  holder.started_us, holder.kind, holder.pid, holder.slot_name
        ) AS rank
 FROM holder
), frames AS (
 SELECT 0 AS ordering, 0::bigint AS rank_order, 'meta'::text AS tag,
        s.hostname AS c1, s.observed_us::text AS c2, s.recovery AS c3, s.next_xid AS c4, s.max_prepared AS c5,
        ''::text AS c6, ''::text AS c7, ''::text AS c8, ''::text AS c9, ''::text AS c10,
        ''::text AS c11, ''::text AS c12, ''::text AS c13, ''::text AS c14
 FROM source s
 UNION ALL
 SELECT 1, r.rank, 'holder', r.rank::text, r.kind, r.pid::text, r.backend_type, r.state, r.role_kind, r.shape,
        r.xid_age::text, r.xmin_age::text, r.xact_start_us::text, r.started_us::text, r.bound_us::text,
        r.slot_name, r.slot_type
 FROM ranked r
 WHERE r.rank <= {holder_limit}
 UNION ALL
 SELECT 2, 0, 'end',
        (SELECT count(*) FROM ranked)::text,
        (SELECT count(*) FROM ranked WHERE ranked.rank <= {holder_limit})::text,
        (SELECT count(*) FROM session_holder)::text,
        (SELECT count(*) FROM pg_replication_slots)::text,
        (SELECT count(*) FROM slot_holder)::text,
        (SELECT count(*) FROM pg_prepared_xacts)::text,
        coalesce((SELECT min(ranked.bound_us) FROM ranked WHERE ranked.bound_us > 0), 0)::text,
        coalesce((SELECT ranked.rank FROM ranked WHERE ranked.bound_us > 0 ORDER BY ranked.bound_us, ranked.rank LIMIT 1), 0)::text,
        '', '', '', '', '', ''
)
SELECT tag, c1, c2, c3, c4, c5, c6, c7, c8, c9, c10, c11, c12, c13, c14
FROM frames ORDER BY ordering, rank_order;
`

// The read with this instance's holder bound.
func transactionHorizonQuery(settings *TransactionHorizonSettings) string {
	return strings.NewReplacer(
		"{hostname_guard}", pgPrimaryHostnameMetaCommand,
		"{holder_limit}", strconv.Itoa(settings.holderLimit()),
	).Replace(transactionHorizonTemplate)
}

// The frame names at least the first-ranked holder.
func (self *TransactionHorizonSettings) holderLimit() int {
	return max(1, self.MaxHolders)
}

// One ranked holder, with numbers and fixed classifications only.
type transactionHorizonHolder struct {
	rank        int64
	kind        string
	pid         int64
	backendType string
	state       string
	roleKind    string
	shape       string
	// backend_xid, a slot's xmin or a prepared transaction's id; -1 when absent
	xidAge int64
	// backend_xmin or a slot's catalog_xmin; -1 when absent
	xminAge int64
	// microseconds; zero when absent
	xactStart int64
	started   int64
	bound     int64
	slotName  string
	slotType  string
}

func (self transactionHorizonHolder) horizonAge() int64 {
	return max(self.xidAge, self.xminAge)
}

// One complete read.
type transactionHorizonObservation struct {
	host        string
	observed    time.Time
	nextXid     uint64
	maxPrepared int64
	holders     []transactionHorizonHolder
	holderCount int64
	sessions    int64
	slots       int64
	slotHolders int64
	prepared    int64
	// the earliest timestamp bound over every holder, and its rank
	earliestBound     int64
	earliestBoundRank int64
}

// The next transaction id at one earlier read.
type transactionHorizonSample struct {
	observed time.Time
	nextXid  uint64
}

// The next-id history is finite and belongs to this signal instance; a new
// primary, a source clock or id that went backwards, or an expired sample
// starts it over.
type transactionHorizonProbe struct {
	settings  *TransactionHorizonSettings
	stateLock sync.Mutex
	host      string
	samples   []transactionHorizonSample
}

func (self *transactionHorizonProbe) id() string               { return transactionHorizonProbeId }
func (self *transactionHorizonProbe) tier() string             { return tierWarn }
func (self *transactionHorizonProbe) cadence() time.Duration   { return self.settings.Cadence }
func (self *transactionHorizonProbe) runBudget() time.Duration { return self.settings.RunBudget }

var (
	transactionHorizonBackendTypePattern = regexp.MustCompile(`^[a-z][a-z ]{0,39}$`)
	transactionHorizonSlotNamePattern    = regexp.MustCompile(`^[a-z0-9_]{1,63}$`)
)

var transactionHorizonStates = map[string]bool{
	"": true, "active": true, "idle": true, "idle in transaction": true,
	"idle in transaction (aborted)": true, "fastpath function call": true, "disabled": true,
}

var transactionHorizonShapes = map[string]bool{
	"replication": true, "reindex": true, "index-build": true, "vacuum": true, "cluster": true,
	"analyze": true, "validate": true, "dump": true, "application": true,
}

// A signed integer cell within [minimum, maximum].
func transactionHorizonInteger(raw string, minimum, maximum int64) (int64, bool) {
	return pgFreezeAgeInteger(raw, minimum, maximum)
}

// Fixed framing, the primary's identity and role, a source clock near the
// monitor's own, and a census that binds the rows. Any gap is a reason.
func (self *transactionHorizonProbe) parse(rows []pgRow, primary string, now time.Time) (*transactionHorizonObservation, string) {
	settings := self.settings
	holderLimit := settings.holderLimit()
	if len(rows) < 2 || len(rows) > holderLimit+2 {
		return nil, "incomplete-or-overbound-frame"
	}
	for _, row := range rows {
		if len(row) != transactionHorizonColumns {
			return nil, "invalid-frame-width"
		}
		for _, cell := range row {
			if len(cell) > settings.MaxCellBytes {
				return nil, "overbound-cell"
			}
		}
	}
	meta, end := rows[0], rows[len(rows)-1]
	shortName, _, _ := strings.Cut(primary, ".")
	if meta[0] != "meta" || end[0] != "end" || shortName == "" || meta[1] != shortName || meta[3] != "0" {
		return nil, "source-identity-or-role-unavailable"
	}
	for _, cell := range append(append([]string{}, meta[6:]...), end[9:]...) {
		if cell != "" {
			return nil, "unexpected-frame-field"
		}
	}
	observed, ok := retentionLivenessMicros(meta[2])
	if !ok {
		return nil, "invalid-source-clock"
	}
	if observed.Sub(now).Abs() > settings.ClockTolerance {
		return nil, "source-clock-unavailable"
	}
	nextXid, err := strconv.ParseUint(meta[4], 10, 64)
	if err != nil || nextXid == 0 {
		return nil, "invalid-next-transaction-id"
	}
	maxPrepared, ok := transactionHorizonInteger(meta[5], 0, math.MaxInt32)
	if !ok {
		return nil, "invalid-prepared-transaction-limit"
	}
	observation := &transactionHorizonObservation{host: primary, observed: observed, nextXid: nextXid, maxPrepared: maxPrepared}
	census := [8]int64{}
	for i := range census {
		value, ok := transactionHorizonInteger(end[i+1], 0, math.MaxInt64)
		if !ok {
			return nil, "invalid-census"
		}
		census[i] = value
	}
	observation.holderCount, observation.sessions = census[0], census[2]
	observation.slots, observation.slotHolders, observation.prepared = census[3], census[4], census[5]
	observation.earliestBound, observation.earliestBoundRank = census[6], census[7]
	// A statement or transaction can start while the read runs, a little
	// after the read's own statement timestamp.
	latestUs := observed.Add(settings.ClockTolerance).UnixMicro()
	returned := census[1]
	if returned != int64(len(rows)-2) || returned != min(observation.holderCount, int64(holderLimit)) ||
		observation.holderCount != observation.sessions+observation.slotHolders+observation.prepared ||
		observation.slotHolders > observation.slots ||
		(observation.earliestBound == 0) != (observation.earliestBoundRank == 0) ||
		observation.earliestBoundRank > observation.holderCount ||
		observation.earliestBound > latestUs {
		return nil, "invalid-census"
	}
	for index, row := range rows[1 : len(rows)-1] {
		holder, reason := self.parseHolder(row, latestUs)
		if reason != "" {
			return nil, reason
		}
		if holder.rank != int64(index+1) {
			return nil, "invalid-holder-rank"
		}
		if 0 < index && observation.holders[index-1].horizonAge() < holder.horizonAge() {
			return nil, "invalid-holder-order"
		}
		observation.holders = append(observation.holders, holder)
	}
	return observation, ""
}

func (self *transactionHorizonProbe) parseHolder(row pgRow, latestUs int64) (transactionHorizonHolder, string) {
	if row[0] != "holder" {
		return transactionHorizonHolder{}, "unexpected-frame-kind"
	}
	values := map[int]int64{}
	for _, field := range []struct {
		index            int
		minimum, maximum int64
	}{
		{1, 1, math.MaxInt32}, {3, 0, math.MaxInt32}, {8, -1, math.MaxInt32}, {9, -1, math.MaxInt32},
		{10, 0, latestUs}, {11, 0, latestUs}, {12, 0, latestUs},
	} {
		value, ok := transactionHorizonInteger(row[field.index], field.minimum, field.maximum)
		if !ok {
			return transactionHorizonHolder{}, "invalid-holder-number"
		}
		values[field.index] = value
	}
	holder := transactionHorizonHolder{
		rank: values[1], kind: row[2], pid: values[3], backendType: row[4], state: row[5], roleKind: row[6], shape: row[7],
		xidAge: values[8], xminAge: values[9], xactStart: values[10], started: values[11], bound: values[12],
		slotName: row[13], slotType: row[14],
	}
	if holder.horizonAge() < 0 {
		return transactionHorizonHolder{}, "invalid-holder-horizon"
	}
	switch holder.kind {
	case "session":
		if holder.pid == 0 || !transactionHorizonBackendTypePattern.MatchString(holder.backendType) ||
			!transactionHorizonStates[holder.state] || !transactionHorizonShapes[holder.shape] ||
			holder.slotName != "" || holder.slotType != "" {
			return transactionHorizonHolder{}, "invalid-session-holder"
		}
	case "slot":
		if holder.backendType != "" || (holder.state != "active" && holder.state != "inactive") ||
			holder.roleKind != "" || holder.shape != "replication-slot" ||
			!transactionHorizonSlotNamePattern.MatchString(holder.slotName) ||
			(holder.slotType != "physical" && holder.slotType != "logical") {
			return transactionHorizonHolder{}, "invalid-slot-holder"
		}
	case "prepared":
		if holder.pid != 0 || holder.backendType != "" || holder.state != "" || holder.shape != "prepared-transaction" ||
			holder.xminAge != -1 || holder.bound == 0 || holder.slotName != "" || holder.slotType != "" {
			return transactionHorizonHolder{}, "invalid-prepared-holder"
		}
	default:
		return transactionHorizonHolder{}, "invalid-holder-kind"
	}
	if holder.kind != "slot" {
		switch holder.roleKind {
		case "superuser", "ordinary", "none":
		default:
			return transactionHorizonHolder{}, "invalid-holder-role"
		}
	}
	return holder, ""
}

// Records this read's next id and returns the history before it: oldest
// first, from one primary, within the window, with ids and clocks advancing.
func (self *transactionHorizonProbe) history(observation *transactionHorizonObservation) []transactionHorizonSample {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.host != observation.host {
		self.host = observation.host
		self.samples = nil
	}
	if last := len(self.samples) - 1; 0 <= last &&
		(observation.nextXid < self.samples[last].nextXid || !self.samples[last].observed.Before(observation.observed)) {
		self.samples = nil
	}
	kept := self.samples[:0]
	for _, sample := range self.samples {
		if observation.observed.Sub(sample.observed) <= self.settings.HistoryWindow {
			kept = append(kept, sample)
		}
	}
	history := append([]transactionHorizonSample(nil), kept...)
	if 0 < self.settings.HistoryWindow {
		kept = append(kept, transactionHorizonSample{observed: observation.observed, nextXid: observation.nextXid})
	}
	self.samples = kept
	return history
}

// How long ago an id this many ids before the next one was assigned, at
// least: the time since the first earlier read whose next id had passed it.
func transactionHorizonHistoryBound(observation *transactionHorizonObservation, history []transactionHorizonSample, ageXids int64) time.Duration {
	if ageXids < 0 || uint64(ageXids)+1 >= observation.nextXid {
		return 0
	}
	horizonXid := observation.nextXid - 1 - uint64(ageXids)
	for _, sample := range history {
		if horizonXid < sample.nextXid {
			return observation.observed.Sub(sample.observed)
		}
	}
	return 0
}

func (self *transactionHorizonProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if probeEnded(ctx) {
		return nil, ctx.Err()
	}
	primaries := env.cfg.hostsWithRole("pg-primary")
	if len(primaries) != 1 {
		return []finding{self.unknown("primary", "primary-inventory-unavailable-or-ambiguous", nil)}, nil
	}
	primary := primaries[0].name
	rows, err := env.runner.pg(ctx, transactionHorizonQuery(self.settings))
	if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
		return nil, lifecycleErr
	}
	if err != nil {
		return []finding{self.unknown(primary, "horizon-source-unavailable", err)}, nil
	}
	observation, reason := self.parse(rows, primary, env.now().UTC())
	if reason != "" {
		return []finding{self.unknown(primary, reason, nil)}, nil
	}
	history := self.history(observation)
	healthyUnknown := healthyFinding(transactionHorizonProbeId, tierWarn, "transaction-horizon-unobservable", primary)
	if len(observation.holders) == 0 {
		return []finding{healthyFinding(transactionHorizonProbeId, tierWarn, "transaction-horizon", primary), healthyUnknown}, nil
	}

	top := observation.holders[0]
	// The oldest horizon is at least as old as any holder's bound.
	historyBound := transactionHorizonHistoryBound(observation, history, top.horizonAge())
	timestampBound := time.Duration(0)
	if 0 < observation.earliestBound {
		timestampBound = max(0, observation.observed.Sub(time.UnixMicro(observation.earliestBound)))
	}
	age := max(historyBound, timestampBound)
	settings := self.settings
	if age < settings.WarnAge {
		return []finding{healthyFinding(transactionHorizonProbeId, tierWarn, "transaction-horizon", primary), healthyUnknown}, nil
	}
	tier := tierWarn
	if settings.PageAge <= age {
		tier = tierPage
	}
	historySpan := time.Duration(0)
	if 0 < len(history) {
		historySpan = observation.observed.Sub(history[0].observed)
	}
	ageSource := "age_source=history"
	if historyBound < timestampBound {
		ageSource = fmt.Sprintf("age_source=timestamp age_source_holder=%d", observation.earliestBoundRank)
	}
	describe := func(holder transactionHorizonHolder) string {
		since := func(us int64) string {
			if us == 0 {
				return "unknown"
			}
			return strconv.FormatInt(int64(max(0, observation.observed.Sub(time.UnixMicro(us))).Seconds()), 10)
		}
		switch holder.kind {
		case "slot":
			return fmt.Sprintf("holder[%d] kind=slot slot=%s type=%s state=%s xmin_age_xids=%d catalog_xmin_age_xids=%d inactive_age_s=%s",
				holder.rank, holder.slotName, holder.slotType, holder.state, holder.xidAge, holder.xminAge, since(holder.started))
		case "prepared":
			return fmt.Sprintf("holder[%d] kind=prepared xid_age_xids=%d prepared_age_s=%s role=%s gid=withheld",
				holder.rank, holder.xidAge, since(holder.started), holder.roleKind)
		default:
			return fmt.Sprintf("holder[%d] kind=session pid=%d backend=%s state=%s role=%s shape=%s backend_xid_age_xids=%d backend_xmin_age_xids=%d xact_age_s=%s application=withheld query=withheld",
				holder.rank, holder.pid, holder.backendType, holder.state, holder.roleKind, holder.shape, holder.xidAge, holder.xminAge, since(holder.xactStart))
		}
	}
	evidence := []string{}
	for _, holder := range observation.holders {
		evidence = append(evidence, describe(holder))
	}

	mechanism, holderContext, action := self.attribution(top, observation)
	return []finding{
		{
			probeId: transactionHorizonProbeId, tier: tier, class: "transaction-horizon", target: primary, sustain: settings.Sustain,
			symptom: fmt.Sprintf("The oldest transaction horizon on %s is at least %s old; its holder ranked first is a %s",
				primary, age.Truncate(time.Second), self.holderName(top)),
			mechanism: "No vacuum may remove a row version that a transaction older than the horizon could still see, so dead rows accumulate behind it in every table of the database, the hot queues first (§2.4). The application role's transaction_timeout ends every application transaction at 15 minutes, so a horizon this old is held by something outside that bound. " + mechanism,
			baseline:  fmt.Sprintf("The oldest horizon stays under %s (warn) and %s (page); application transactions end at 15 minutes.", settings.WarnAge, settings.PageAge),
			observed: fmt.Sprintf("horizon_age_s>=%d horizon_age_xids=%d %s history_s=%d holders=%d session_holders=%d slots=%d slot_holders=%d prepared_transactions=%d max_prepared_transactions=%d",
				int64(age.Seconds()), top.horizonAge(), ageSource, int64(historySpan.Seconds()),
				observation.holderCount, observation.sessions, observation.slots, observation.slotHolders, observation.prepared, observation.maxPrepared),
			evidence: strings.Join(evidence, "\n"),
			context:  holderContext,
			action:   action,
			verify:   fmt.Sprintf("The holder ends or is resolved, the horizon's age falls under %s on consecutive samples, and the §2.4 dead-tuple counts stop rising.", settings.WarnAge),
			playbook: "SIGNALS.md §2.4l",
		},
		healthyUnknown,
	}, nil
}

func (self *transactionHorizonProbe) holderName(holder transactionHorizonHolder) string {
	switch holder.kind {
	case "slot":
		return fmt.Sprintf("%s replication slot %s (%s)", holder.slotType, holder.slotName, holder.state)
	case "prepared":
		return "prepared transaction"
	default:
		return fmt.Sprintf("%s session (pid %d, %s, %s role, %s)", holder.backendType, holder.pid, holder.state, holder.roleKind, holder.shape)
	}
}

// The fixed reading of the first-ranked holder: what holds the horizon, what
// else to know, and what to do. Its shape comes from fixed statement prefixes
// classified inside the database; the statement itself never leaves it.
func (self *transactionHorizonProbe) attribution(holder transactionHorizonHolder, observation *transactionHorizonObservation) (mechanism string, holderContext string, action string) {
	switch {
	case holder.kind == "prepared":
		return "A prepared transaction keeps its id, and the horizon, until COMMIT PREPARED or ROLLBACK PREPARED, across restarts; the application does not use two-phase commit.",
			fmt.Sprintf("max_prepared_transactions=%d. A prepared transaction outlives the session that prepared it, so no session timeout ends it.", observation.maxPrepared),
			"Name its owner and identifier in a private session (pg_prepared_xacts), confirm with that owner whether it should commit, then COMMIT PREPARED or ROLLBACK PREPARED it. Do not restart PostgreSQL to clear it: a prepared transaction survives the restart."
	case holder.kind == "slot" || holder.shape == "replication":
		return "A replication holder keeps the horizon: a slot's xmin or catalog_xmin, or a standby's hot_standby_feedback relayed by its walsender, holds back cleanup for the standby's own queries. An inactive slot keeps holding until it is dropped.",
			"A standby's long query is bounded only by the standby's own settings; the primary's transaction_timeout does not reach it. A logical slot's catalog_xmin holds back the system catalogs only.",
			"Find the standby's long query (pg_stat_activity on the standby) or the slot's consumer. Drop an abandoned slot (pg_drop_replication_slot) only with the owner's go, after confirming no standby or consumer still needs it."
	case holder.roleKind == "superuser":
		return "A superuser session holds the horizon. The 15-minute bound is a default of the application roles only, so operator sessions run unbounded unless they set it themselves; pg_dump 17 and later lifts it for its own session.",
			"An operator's dump, VALIDATE, REINDEX or ad hoc query pins cleanup in every table for as long as it runs.",
			"Ask the operator who owns the pid whether it is near done; cancel it (pg_cancel_backend) only with their go when dead rows rise on the hot tables (§2.4)."
	case holder.shape == "reindex" || holder.shape == "index-build" || holder.shape == "vacuum" ||
		holder.shape == "cluster" || holder.shape == "analyze" || holder.shape == "validate":
		return "A maintenance statement holds the horizon. A maintenance operation that legitimately runs past the 15-minute bound lifts it for its own session (server WithoutTransactionTimeout), and this alarm is its horizon alarm: a concurrent rebuild holds its snapshot through each build and validate phase.",
			"The rotation bounds a whole-table rebuild at two hours and starts one only while no transaction is older than ten minutes; an operator's command (bringyourctl db vacuum, a partition cutover, a migration) may run longer.",
			"Check its progress (pg_stat_progress_create_index, pg_stat_progress_cluster). Let it finish when it is advancing and the hot tables' dead rows hold (§2.4); otherwise cancel it (pg_cancel_backend): the rotation resumes the table at its next run and drops the rebuild's debris (§2.2a)."
	case holder.shape == "dump":
		return "A COPY TO stdout snapshot, as pg_dump runs, holds the horizon for the whole dump.",
			"pg_dump 17 and later lifts the bound for its own session, so a dump as the application role runs unbounded too.",
			"Confirm the dump is planned and progressing; reschedule a dump that overlaps peak write churn rather than cancel one near its end."
	default:
		return "An application session holds the horizon past the 15-minute bound. Either it started before its role picked up the bound (role defaults reach a session only when it starts: the pools recycle connections over 8 hours plus up to 15 minutes, and the transaction pooler its servers per server_lifetime), or its operation lifted the bound for itself.",
			fmt.Sprintf("state=%s. A session idle in a transaction is ended by idle_in_transaction_session_timeout (60s); an active one runs until its statement returns.", holder.state),
			"Find the pid's task or request in a private session (pg_stat_activity) and the service it runs in. If the bound has not reached its pool, recycle that pool (restart the service, or RECONNECT the PgBouncer pool) and verify SHOW transaction_timeout; otherwise fix or page the operation so it fits the bound. Cancel it only when its owner confirms that is safe."
	}
}

func (self *transactionHorizonProbe) unknown(target, reason string, err error) finding {
	observed := "reason=" + reason
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: transactionHorizonProbeId, tier: tierWarn, class: "transaction-horizon-unobservable", target: target, sustain: 2,
		symptom:   "The age of the oldest transaction horizon is unknown",
		mechanism: "The primary inventory, its hostname and role guard, the bounded horizon read, its fixed framing or its census could not be established, so an old horizon holder could be present unseen.",
		baseline:  "One configured primary answers the guarded horizon read with a complete frame whose census binds its holders.",
		observed:  observed,
		evidence:  "Only fixed reasons, counts and error classes are retained; statements, application names, client addresses, role names and SQL errors stay private.",
		action:    "Restore the guarded read (the primary inventory, psql on the primary, the frame), then rerun the signal. Until then use the §2.4 horizon candidate as context only.",
		verify:    "The next read returns a complete frame and a concrete healthy or old-horizon result.",
		playbook:  "SIGNALS.md §2.4l",
	}
}
