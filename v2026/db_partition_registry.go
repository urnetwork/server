// One partition maintenance registry (PARTITION-MAINTENANCE-PLAN.md A7),
// beside db_maintenance. Every range-partitioned table family declares itself
// once, statically, and the registry drives what the families share: canonical
// ranges and names, create-ahead with catch-up, parent and leaf statistics,
// guarded drops and resume state. The monitor's runway and retention probes
// read the same declarations.
//
// # Declaring a family
//
// A family is a *PartitionFamily registered at package initialization:
//
//	var historyPartitionFamily = server.RegisterPartitionFamily(&server.PartitionFamily{
//		Name:        "provider_egress_health_history",
//		Tables:      []string{"provider_egress_health_history"},
//		KeyColumn:   "measured_at",
//		KeyKind:     server.PartitionKeyTimestamp,
//		Granularity: server.PartitionGranularityDaily,
//		AheadRanges: 3,
//		Retention:   7 * 24 * time.Hour,
//	})
//
// A hook that refers back to the family's own var makes an initialization
// cycle; register such a family from an init function instead.
//
// The declaration says:
//   - the tables, all partitioned on the same ranges of KeyColumn (created in
//     Tables order, dropped children first, in reverse);
//   - the key kind: a utc timestamp (PartitionKeyTimestamp), a uuid id that
//     leads with its creation millisecond, bounded through contract_id_floor
//     (PartitionKeyId), or epoch minutes (PartitionKeyEpochMinute);
//   - the granularity, daily or weekly, and how many ranges after the current
//     one always exist (AheadRanges);
//   - the retention: a range expires once its upper bound is Retention past.
//     Zero never drops a range;
//   - optionally a DEFAULT partition (DefaultPartition, <table>_default, never
//     dropped) and a legacy partition (<table>_legacy, every key below the
//     cutover), which retires like a range unless KeepLegacyPartition;
//   - optionally a guarded drop: DropGuardSetting is a GUC the drop
//     transaction sets on (SET LOCAL <guc> = 'on') before it detaches
//     anything, DropFence runs next in that transaction (take locks, raise to
//     refuse), and DropPrecondition says why a range must stay. The
//     precondition runs before the drop and again in the drop's transaction
//     after every leaf is detached, where no other statement can reach them;
//   - optionally an archive step before a drop (Archive), resumable through a
//     cursor the registry keeps.
//
// Each hook is optional; a nil hook is the registry's default. A hook may
// raise (server.Raise) instead of returning an error; the registry reports
// either as that hook's failure.
//
// # What the registry gives every family
//
//   - Canonical bounds: daily ranges start at 00:00 UTC and weekly ones at
//     Monday 00:00 UTC, so a range never straddles two partitions. Leaves are
//     named <table>_pYYYYMMDD (daily) and <table>_wYYYYMMDD (weekly).
//   - Create-ahead with catch-up (EnsureRunway): every range from the one
//     holding CreateFrom(now) (late writes; now by default) through
//     AheadRanges after the current one is created when no leaf covers it, in
//     one pass after any downtime. A writer can create the range of a key it
//     holds (EnsureRangeFor), so an outage never leaves an insert without a
//     partition. The default create builds a standalone leaf (LIKE the parent)
//     with the parent's TRUNCATE guards and LeafReloptions, then attaches it,
//     which takes only SHARE UPDATE EXCLUSIVE on the parent.
//   - Statistics: autovacuum never analyzes a partitioned parent, so each
//     parent is analyzed (ANALYZE ONLY) once per range period, and with
//     LeafAnalyzeDelay each closed leaf once that long after its range ends.
//   - Guarded drops (DropRange), and the retirement pass (Retire): expired
//     ranges oldest first, each through its precondition, archive and drop.
//   - Resume state (PartitionFamilyState), carried between runs in the
//     registry task's arguments.
//   - Out of the REINDEX rotation, with ReindexExempt.
//
// # Families keep their own rules
//
// A family's own rules stay in its hooks: late writes into old ranges
// (CreateFrom), its own create statements (CreateLeaf), an expiry that is not
// a fixed horizon (RangeExpired, which may expire a range sooner than
// Retention, never later), the checks that keep a range, leaves that keep
// changing after their range closes (LeafAnalyzeDelay; their bloated indexes
// are rebuilt leaf by leaf by db_maintenance's priority rebuild), and families
// that never drop.
//
// # Scheduling
//
// Registering a family schedules it. The runway task (MaintainPartitionRunways)
// creates ahead for every registered family on its own short cadence, apart
// from slow archive work. The retention task (MaintainPartitionRetention)
// analyzes and retires them. A family with its own task (the contract tables,
// client_reliability, network_ping) sets AnalyzeByOwnTask or RetireByOwnTask,
// and that task calls the family's methods or keeps its own code. Nothing runs
// for a family whose first table is not partitioned yet (Partitioned) except
// its BeforeCutover hook on the runway pass, so a family cut over by an
// operator command is safe to register before its cutover.
package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/glog/v2026"
)

// How a family's ranges divide time.
type PartitionGranularity string

const (
	// one range per UTC day, from 00:00
	PartitionGranularityDaily PartitionGranularity = "daily"
	// one range per week, from Monday 00:00 UTC, as the contract weeks
	PartitionGranularityWeekly PartitionGranularity = "weekly"
)

func (self PartitionGranularity) valid() bool {
	return self == PartitionGranularityDaily || self == PartitionGranularityWeekly
}

// The canonical start of the range holding t.
func (self PartitionGranularity) RangeStart(t time.Time) time.Time {
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if self == PartitionGranularityWeekly {
		return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	}
	return day
}

// The start of the range after the one starting at rangeStart.
func (self PartitionGranularity) NextRangeStart(rangeStart time.Time) time.Time {
	return self.AddRanges(rangeStart, 1)
}

// The start of the range count ranges after (before, when negative) the one
// starting at rangeStart.
func (self PartitionGranularity) AddRanges(rangeStart time.Time, count int) time.Time {
	if self == PartitionGranularityWeekly {
		return rangeStart.UTC().AddDate(0, 0, 7*count)
	}
	return rangeStart.UTC().AddDate(0, 0, count)
}

// The range of t: [RangeStart(t), NextRangeStart(RangeStart(t))).
func (self PartitionGranularity) RangeOf(t time.Time) PartitionRange {
	lower := self.RangeStart(t)
	return PartitionRange{Lower: lower, Upper: self.NextRangeStart(lower)}
}

// The letter between a table's name and the date of its leaves.
func (self PartitionGranularity) namePrefix() string {
	if self == PartitionGranularityWeekly {
		return "w"
	}
	return "p"
}

// How a family's partition key orders in time.
type PartitionKeyKind string

const (
	// a timestamp without time zone holding utc, as everywhere in this schema
	PartitionKeyTimestamp PartitionKeyKind = "timestamp"
	// a uuid id whose first 48 bits are its creation millisecond
	// (connect.NewId); a bound is contract_id_floor of its time
	PartitionKeyId PartitionKeyKind = "id"
	// a bigint count of minutes since the unix epoch (client_reliability's
	// block_number)
	PartitionKeyEpochMinute PartitionKeyKind = "epoch_minute"
)

func (self PartitionKeyKind) valid() bool {
	return self == PartitionKeyTimestamp || self == PartitionKeyId || self == PartitionKeyEpochMinute
}

// The SQL literal of the key bound at t, for a FOR VALUES clause.
func (self PartitionKeyKind) BoundSql(t time.Time) string {
	switch self {
	case PartitionKeyId:
		return contractIdLiteral(ContractIdFloor(t))
	case PartitionKeyEpochMinute:
		return strconv.FormatInt(t.UTC().Unix()/60, 10)
	default:
		return "'" + t.UTC().Format(partitionTimestampLayout) + "'"
	}
}

// The time of one bound value as pg_get_expr prints it, unquoted: an ISO
// timestamp, a uuid, or an integer. exact says the value is the bound of that
// time itself: a uuid with low bits set lies inside its millisecond, so it is
// not a bound this registry makes.
func (self PartitionKeyKind) ParseBound(value string) (t time.Time, exact bool, ok bool) {
	switch self {
	case PartitionKeyId:
		id, err := ParseId(value)
		if err != nil {
			return time.Time{}, false, false
		}
		t = ContractIdTime(id)
		return t, ContractIdFloor(t) == id, true
	case PartitionKeyEpochMinute:
		minutes, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return time.Time{}, false, false
		}
		return time.Unix(minutes*60, 0).UTC(), true, true
	default:
		for _, layout := range []string{partitionTimestampLayout, partitionTimestampMicrosecondLayout, time.DateOnly} {
			if t, err := time.Parse(layout, value); err == nil {
				return t.UTC(), true, true
			}
		}
		return time.Time{}, false, false
	}
}

const partitionTimestampLayout = "2006-01-02 15:04:05"

// A timestamp bound as ListLeaves casts it back in the reading session:
// to_char(<bound>::timestamp, 'YYYY-MM-DD HH24:MI:SS.US').
const partitionTimestampMicrosecondLayout = "2006-01-02 15:04:05.999999"

// A range of a family, the same in each of its tables. A zero Lower is
// MINVALUE (the legacy partition) and a zero Upper MAXVALUE; the DEFAULT
// partition has neither.
type PartitionRange struct {
	Lower time.Time
	Upper time.Time
	// the legacy partition: every key below the cutover
	Legacy bool
	// the DEFAULT partition: every key no other partition takes
	Default bool
}

// Whether the two ranges share a key. The DEFAULT partition overlaps nothing.
func (self PartitionRange) Overlaps(other PartitionRange) bool {
	if self.Default || other.Default {
		return false
	}
	return (self.Upper.IsZero() || other.Lower.IsZero() || other.Lower.Before(self.Upper)) &&
		(other.Upper.IsZero() || self.Lower.IsZero() || self.Lower.Before(other.Upper))
}

// Whether the range holds every key of other.
func (self PartitionRange) Covers(other PartitionRange) bool {
	if self.Default || other.Default {
		return false
	}
	return (self.Lower.IsZero() || (!other.Lower.IsZero() && !other.Lower.Before(self.Lower))) &&
		(self.Upper.IsZero() || (!other.Upper.IsZero() && !self.Upper.Before(other.Upper)))
}

// One attached partition of a family's table, with its bounds as the catalog
// has them.
type PartitionLeaf struct {
	Table string
	Name  string
	Range PartitionRange
	// the registry's own leaf: a canonical range under its canonical name, or
	// the family's legacy or DEFAULT partition. Anything else attached is left
	// alone: never dropped, though it covers what its bounds cover.
	Canonical bool
}

// A connection or a transaction.
type PgQuerier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Tunables of one family's maintenance statements.
type PartitionFamilySettings struct {
	// How long a create waits for its locks before it gives up and is retried
	// on the next run. An attach takes SHARE UPDATE EXCLUSIVE on the parent.
	CreateLockTimeout time.Duration
	// How long the drop transaction waits for each lock. Its detaches take
	// ACCESS EXCLUSIVE on each parent, so a long wait would queue traffic.
	DropLockTimeout time.Duration
	// Bounds each statement of the drop transaction: catalog work and the
	// precondition's reads of the detached leaves.
	DropStatementTimeout time.Duration
}

// Seconds of lock wait, as the contract and log families have used, and a
// minute for a drop's statements.
func DefaultPartitionFamilySettings() *PartitionFamilySettings {
	return &PartitionFamilySettings{
		CreateLockTimeout:    5 * time.Second,
		DropLockTimeout:      5 * time.Second,
		DropStatementTimeout: 60 * time.Second,
	}
}

// One family's static declaration and its rules (see the file comment).
type PartitionFamily struct {
	// The registry's name for the family, in logs, task state and monitor
	// findings: lowercase letters, digits and underscores.
	Name string
	// One line on what the family holds and who maintains it, for alerts.
	Description string
	// The partitioned parents, all on the same ranges of KeyColumn. Ranges are
	// created in this order and their leaves detached and dropped in reverse,
	// so a referencing table goes after the table it references. The first
	// table is the family's anchor: its kind says whether the family is
	// partitioned yet, and its leaves list the family's ranges.
	Tables      []string
	KeyColumn   string
	KeyKind     PartitionKeyKind
	Granularity PartitionGranularity
	// Ranges after the current one that always exist. At least one.
	AheadRanges int
	// A range expires once its upper bound is this far past. Zero never
	// drops a range. RangeExpired may expire a range sooner, never later; the
	// monitor judges blocked retention by this bound.
	Retention time.Duration
	// The family keeps a DEFAULT partition, <table>_default, for keys outside
	// its ranges. It is created with the runway, never dropped, and never
	// covers a range.
	DefaultPartition bool
	// The legacy partition, <table>_legacy, is never dropped. Without it the
	// legacy partition retires like a range once its upper bound, the
	// cutover, is Retention past.
	KeepLegacyPartition bool
	// A GUC the drop transaction sets on before any detach, for a family whose
	// guards refuse a drop without it: a custom setting such as
	// urnetwork.close_evidence_retention. Empty sets none.
	DropGuardSetting string
	// Each new leaf's storage settings, for example
	// "autovacuum_vacuum_scale_factor=0". Empty leaves the defaults.
	LeafReloptions []string
	// A closed leaf is analyzed once, this long after its range ends, when
	// late writes and updates have landed. Zero leaves leaves to autovacuum.
	LeafAnalyzeDelay time.Duration
	// The family's own task analyzes its parents and leaves, and the registry
	// does not.
	AnalyzeByOwnTask bool
	// The family's own task retires its expired ranges, and the registry does
	// not.
	RetireByOwnTask bool
	// The family's partitioned parents and their leaves stay out of
	// db_maintenance's REINDEX rotation: its leaves are dropped whole.
	ReindexExempt bool
	// The monitor pages when fewer ranges than this are covered after the
	// current one. Zero uses min(2, AheadRanges).
	MonitorRunwayRanges int
	// The monitor reports a range this long past its retention that still
	// exists. Zero uses two ranges.
	MonitorRetentionGrace time.Duration
	// The SIGNALS.md section whose probe watches this family's runway, or its
	// blocked retention, in place of the registry's own probes. Empty uses
	// the registry's.
	RunwaySignal    string
	RetentionSignal string
	// Tables an operator cutover of the family keeps beside its tables while
	// it stages or until the old table is dropped, such as <table>_new and
	// <table>_old. db audit does not report them or their indexes as drift.
	CutoverTables []string
	// nil uses DefaultPartitionFamilySettings.
	Settings *PartitionFamilySettings

	// Whether the family is partitioned yet. nil: the first table is a
	// partitioned table (relkind p).
	Partitioned func(ctx context.Context) bool
	// Runs on each runway pass while the family is not partitioned yet, for a
	// family an operator command cuts over: for example to abandon a prepared
	// cutover that could no longer be finalized before its cutover week. Its
	// failure is the runway report's, retried on the next pass. nil: nothing.
	BeforeCutover func(ctx context.Context, now time.Time) error
	// The earliest time whose range must exist, for late writes into old
	// ranges. nil: now.
	CreateFrom func(ctx context.Context, now time.Time) time.Time
	// Creates table's leaf for r and attaches it. nil: the registry's default
	// (CreateLeafSql in one transaction).
	CreateLeaf func(ctx context.Context, table string, r PartitionRange) error
	// Whether r has expired at now. nil: its upper bound is Retention past.
	RangeExpired func(ctx context.Context, now time.Time, r PartitionRange) bool
	// Runs first in the drop transaction, after DropGuardSetting and before
	// any detach: take the locks that fence the range's writers and readers,
	// and raise to refuse the drop.
	DropFence func(ctx context.Context, tx PgTx, r PartitionRange)
	// Why r must stay, or "" when it can be dropped. It runs before the drop
	// on a maintenance connection, and again on the drop's transaction after
	// every leaf is detached.
	DropPrecondition func(ctx context.Context, conn PgQuerier, r PartitionRange) string
	// The step before r is dropped, such as archiving rows the drop would
	// lose. It works until deadline and returns done once r may be dropped,
	// or the cursor an interrupted step resumes from.
	Archive func(ctx context.Context, r PartitionRange, cursor string, deadline time.Time) (nextCursor string, done bool, err error)
}

var partitionFamilyNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
var partitionSettingNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)

// The registered families, by name. Registration happens at package
// initialization; reads come later and from any goroutine.
type partitionRegistry struct {
	stateLock sync.Mutex
	families  map[string]*PartitionFamily
}

func newPartitionRegistry() *partitionRegistry {
	return &partitionRegistry{families: map[string]*PartitionFamily{}}
}

var registeredPartitionFamilies = newPartitionRegistry()

// Why family is not a declaration the registry can drive, or nil.
func validatePartitionFamily(family *PartitionFamily) error {
	if family == nil {
		return errors.New("no family")
	}
	if !partitionFamilyNamePattern.MatchString(family.Name) {
		return fmt.Errorf("family name %q is not a lowercase identifier", family.Name)
	}
	if len(family.Tables) == 0 {
		return fmt.Errorf("family %s has no tables", family.Name)
	}
	for i, table := range family.Tables {
		if !partitionFamilyNamePattern.MatchString(table) {
			return fmt.Errorf("family %s table %q is not a lowercase identifier", family.Name, table)
		}
		if slices.Contains(family.Tables[:i], table) {
			return fmt.Errorf("family %s lists %s twice", family.Name, table)
		}
		// the longest leaf name is <table>_default
		if pgMaxIdentifierLength < len(table)+len("_default") {
			return fmt.Errorf("family %s table %s is too long to name its leaves", family.Name, table)
		}
	}
	if !partitionFamilyNamePattern.MatchString(family.KeyColumn) {
		return fmt.Errorf("family %s key column %q is not a lowercase identifier", family.Name, family.KeyColumn)
	}
	if !family.KeyKind.valid() {
		return fmt.Errorf("family %s has key kind %q", family.Name, family.KeyKind)
	}
	if !family.Granularity.valid() {
		return fmt.Errorf("family %s has granularity %q", family.Name, family.Granularity)
	}
	if family.AheadRanges < 1 {
		return fmt.Errorf("family %s keeps %d ranges ahead; it must keep at least one", family.Name, family.AheadRanges)
	}
	if family.Retention < 0 || family.LeafAnalyzeDelay < 0 || family.MonitorRetentionGrace < 0 || family.MonitorRunwayRanges < 0 {
		return fmt.Errorf("family %s has a negative duration or count", family.Name)
	}
	if family.MonitorRunwayRanges > family.AheadRanges {
		return fmt.Errorf("family %s pages below %d ranges ahead but keeps only %d", family.Name, family.MonitorRunwayRanges, family.AheadRanges)
	}
	if family.DropGuardSetting != "" && !partitionSettingNamePattern.MatchString(family.DropGuardSetting) {
		return fmt.Errorf("family %s drop guard %q is not a custom setting name", family.Name, family.DropGuardSetting)
	}
	if family.Settings != nil {
		settings := family.Settings
		if settings.CreateLockTimeout < time.Millisecond || settings.DropLockTimeout < time.Millisecond || settings.DropStatementTimeout < time.Millisecond {
			return fmt.Errorf("family %s settings need positive timeouts of at least a millisecond", family.Name)
		}
	}
	return nil
}

// Validates family and adds it, refusing a second family of the same name or
// one that claims another family's table.
func (self *partitionRegistry) register(family *PartitionFamily) error {
	if err := validatePartitionFamily(family); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.families[family.Name]; ok {
		return fmt.Errorf("family %s is registered twice", family.Name)
	}
	for _, other := range self.families {
		for _, table := range family.Tables {
			if slices.Contains(other.Tables, table) {
				return fmt.Errorf("family %s claims %s, which family %s owns", family.Name, table, other.Name)
			}
		}
	}
	self.families[family.Name] = family
	return nil
}

// The families, ordered by name.
func (self *partitionRegistry) list() []*PartitionFamily {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	families := make([]*PartitionFamily, 0, len(self.families))
	for _, family := range self.families {
		families = append(families, family)
	}
	slices.SortFunc(families, func(a *PartitionFamily, b *PartitionFamily) int {
		return strings.Compare(a.Name, b.Name)
	})
	return families
}

// Registers a family's static declaration and returns it, for a package-level
// var. A declaration the registry cannot drive panics at initialization, as
// does a second family of the same name or a table another family owns.
func RegisterPartitionFamily(family *PartitionFamily) *PartitionFamily {
	if err := registeredPartitionFamilies.register(family); err != nil {
		panic(fmt.Errorf("partition registry: %w", err))
	}
	return family
}

// Every registered family, ordered by name.
func PartitionFamilies() []*PartitionFamily {
	return registeredPartitionFamilies.list()
}

// The registered family named name, or nil.
func GetPartitionFamily(name string) *PartitionFamily {
	for _, family := range PartitionFamilies() {
		if family.Name == name {
			return family
		}
	}
	return nil
}

// The registered family whose parent or leaf table is, or nil.
func PartitionFamilyOfTable(table string) *PartitionFamily {
	for _, family := range PartitionFamilies() {
		if family.ownsTable(table) {
			return family
		}
	}
	return nil
}

// Whether table is one of the family's parents or one of its own leaves.
func (self *PartitionFamily) ownsTable(table string) bool {
	if slices.Contains(self.Tables, table) {
		return true
	}
	for _, parent := range self.Tables {
		if _, ok := self.ParseLeafName(parent, table); ok {
			return true
		}
	}
	return false
}

func (self *PartitionFamily) settings() *PartitionFamilySettings {
	if self.Settings != nil {
		return self.Settings
	}
	return DefaultPartitionFamilySettings()
}

// The name of the legacy partition of table.
func PartitionLegacyLeafName(table string) string {
	return table + "_legacy"
}

// The name of the DEFAULT partition of table.
func PartitionDefaultLeafName(table string) string {
	return table + "_default"
}

// The registry's name for table's leaf of r: <table>_pYYYYMMDD for a daily
// range, <table>_wYYYYMMDD for a weekly one, or the legacy or DEFAULT
// partition's name.
func (self *PartitionFamily) LeafName(table string, r PartitionRange) string {
	switch {
	case r.Default:
		return PartitionDefaultLeafName(table)
	case r.Legacy:
		return PartitionLegacyLeafName(table)
	default:
		return table + "_" + self.Granularity.namePrefix() + r.Lower.UTC().Format("20060102")
	}
}

// The leaf names of r, one per table in Tables order.
func (self *PartitionFamily) LeafNames(r PartitionRange) []string {
	names := make([]string, 0, len(self.Tables))
	for _, table := range self.Tables {
		names = append(names, self.LeafName(table, r))
	}
	return names
}

// The range a leaf name stands for, when it is one the registry gives table:
// a canonical range, the legacy partition (whose upper bound only the catalog
// knows), or the DEFAULT partition.
func (self *PartitionFamily) ParseLeafName(table string, name string) (r PartitionRange, ok bool) {
	suffix, found := strings.CutPrefix(name, table+"_")
	if !found {
		return PartitionRange{}, false
	}
	switch suffix {
	case "legacy":
		return PartitionRange{Legacy: true}, true
	case "default":
		if !self.DefaultPartition {
			return PartitionRange{}, false
		}
		return PartitionRange{Default: true}, true
	}
	date, found := strings.CutPrefix(suffix, self.Granularity.namePrefix())
	if !found || len(date) != 8 || strings.Trim(date, "0123456789") != "" {
		return PartitionRange{}, false
	}
	lower, err := time.Parse("20060102", date)
	if err != nil || !self.Granularity.RangeStart(lower).Equal(lower) {
		return PartitionRange{}, false
	}
	return self.Granularity.RangeOf(lower), true
}

// The canonical range holding t.
func (self *PartitionFamily) RangeAt(t time.Time) PartitionRange {
	return self.Granularity.RangeOf(t)
}

// Whether the family's tables are partitioned yet.
func (self *PartitionFamily) IsPartitioned(ctx context.Context) (partitioned bool) {
	if self.Partitioned != nil {
		return self.Partitioned(ctx)
	}
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE((SELECT relkind = 'p' FROM pg_class WHERE oid = to_regclass($1)), false)`,
			"public."+self.Tables[0],
		).Scan(&partitioned))
	})
	return
}

var partitionBoundPattern = regexp.MustCompile(`^FOR VALUES FROM \((MINVALUE|'([^']*)'|(-?[0-9]+))\) TO \((MAXVALUE|'([^']*)'|(-?[0-9]+))\)$`)

// The attached partitions of table with their bounds. A timestamp bound is
// cast back in the reading session, so it parses whatever the session's date
// style. A bound that is not a range of the family's key is an error.
func (self *PartitionFamily) ListLeaves(ctx context.Context, conn PgQuerier, table string) (leaves []PartitionLeaf, err error) {
	result, queryErr := conn.Query(
		ctx,
		`
		SELECT
			child.relname,
			bound.expression,
			CASE WHEN $2 AND bound.lower_text IS NOT NULL THEN to_char(bound.lower_text::timestamp, 'YYYY-MM-DD HH24:MI:SS.US') END,
			CASE WHEN $2 AND bound.upper_text IS NOT NULL THEN to_char(bound.upper_text::timestamp, 'YYYY-MM-DD HH24:MI:SS.US') END
		FROM pg_inherits inheritance
		JOIN pg_class child ON child.oid = inheritance.inhrelid
		CROSS JOIN LATERAL (
			SELECT
				pg_get_expr(child.relpartbound, child.oid) AS expression,
				(regexp_match(pg_get_expr(child.relpartbound, child.oid), '^FOR VALUES FROM \(''([^'']*)''\) TO'))[1] AS lower_text,
				(regexp_match(pg_get_expr(child.relpartbound, child.oid), 'TO \(''([^'']*)''\)$'))[1] AS upper_text
		) bound
		WHERE inheritance.inhparent = to_regclass($1)
		ORDER BY child.relname
		`,
		"public."+table,
		self.KeyKind == PartitionKeyTimestamp,
	)
	WithPgResult(result, queryErr, func() {
		for result.Next() {
			var name string
			var expression string
			var lowerTimestamp *string
			var upperTimestamp *string
			Raise(result.Scan(&name, &expression, &lowerTimestamp, &upperTimestamp))
			leaf, ok := self.ParseLeaf(table, name, expression, lowerTimestamp, upperTimestamp)
			if !ok {
				err = fmt.Errorf("unexpected bound of %s of %s: %s", name, table, expression)
				return
			}
			leaves = append(leaves, leaf)
		}
	})
	return
}

// One listed partition from its name and pg_get_expr(relpartbound). For a
// timestamp key, lowerTimestamp and upperTimestamp are the bounds cast back in
// the reading session (to_char(<bound>::timestamp, 'YYYY-MM-DD HH24:MI:SS.US'),
// nil for MINVALUE and MAXVALUE), so the parse holds whatever its date style;
// other keys parse from the expression. ok is false for a bound that is not a
// range of the family's key.
func (self *PartitionFamily) ParseLeaf(table string, name string, expression string, lowerTimestamp *string, upperTimestamp *string) (leaf PartitionLeaf, ok bool) {
	leaf = PartitionLeaf{Table: table, Name: name}
	if expression == "DEFAULT" {
		leaf.Range = PartitionRange{Default: true}
		leaf.Canonical = self.DefaultPartition && name == PartitionDefaultLeafName(table)
		return leaf, true
	}
	groups := partitionBoundPattern.FindStringSubmatch(expression)
	if groups == nil {
		return leaf, false
	}
	bound := func(marker string, quoted string, number string, cast *string) (t time.Time, exact bool, ok bool) {
		switch {
		case marker == "MINVALUE" || marker == "MAXVALUE":
			return time.Time{}, true, true
		case self.KeyKind == PartitionKeyTimestamp:
			if cast == nil {
				return time.Time{}, false, false
			}
			return self.KeyKind.ParseBound(*cast)
		case number != "":
			return self.KeyKind.ParseBound(number)
		default:
			return self.KeyKind.ParseBound(quoted)
		}
	}
	lower, lowerExact, lowerOk := bound(groups[1], groups[2], groups[3], lowerTimestamp)
	upper, upperExact, upperOk := bound(groups[4], groups[5], groups[6], upperTimestamp)
	if !lowerOk || !upperOk || (!lower.IsZero() && !upper.IsZero() && !lower.Before(upper)) {
		return leaf, false
	}
	leaf.Range = PartitionRange{Lower: lower, Upper: upper}
	exact := lowerExact && upperExact
	if lower.IsZero() && !upper.IsZero() && name == PartitionLegacyLeafName(table) {
		leaf.Range.Legacy = true
		leaf.Canonical = exact
	} else if r, named := self.ParseLeafName(table, name); named && !r.Legacy && !r.Default {
		leaf.Canonical = exact && r.Lower.Equal(lower) && r.Upper.Equal(upper)
	}
	return leaf, true
}

// Whether a leaf other than the DEFAULT partition holds every key of r.
func PartitionRangeCovered(leaves []PartitionLeaf, r PartitionRange) bool {
	return slices.ContainsFunc(leaves, func(leaf PartitionLeaf) bool {
		return leaf.Range.Covers(r)
	})
}

// Whether a leaf other than the DEFAULT partition shares a key with r, so a
// new leaf for r would overlap it.
func partitionRangeOverlapped(leaves []PartitionLeaf, r PartitionRange) bool {
	return slices.ContainsFunc(leaves, func(leaf PartitionLeaf) bool {
		return leaf.Range.Overlaps(r)
	})
}

// The ranges of the creation window that no leaf overlaps, oldest first: from
// the range holding createFrom (clamped to now) through AheadRanges after the
// range holding now. A range a leaf only partly overlaps is not created, since
// its leaf could not attach; the runway counts it uncovered.
func (self *PartitionFamily) MissingRanges(leaves []PartitionLeaf, createFrom time.Time, now time.Time) (missing []PartitionRange) {
	if now.Before(createFrom) {
		createFrom = now
	}
	last := self.Granularity.AddRanges(self.Granularity.RangeStart(now), self.AheadRanges)
	for start := self.Granularity.RangeStart(createFrom); !last.Before(start); start = self.Granularity.NextRangeStart(start) {
		r := self.Granularity.RangeOf(start)
		if !partitionRangeOverlapped(leaves, r) {
			missing = append(missing, r)
		}
	}
	return
}

// The most ranges a runway count reaches, so an open upper bound ends the
// count.
const partitionRunwayCountLimit = 1000

// Whether the range holding now is covered, and how many consecutive ranges
// after it are, up to a bound.
func (self *PartitionFamily) Runway(leaves []PartitionLeaf, now time.Time) (currentCovered bool, futureRanges int) {
	current := self.Granularity.RangeStart(now)
	currentCovered = PartitionRangeCovered(leaves, self.Granularity.RangeOf(current))
	for start := self.Granularity.NextRangeStart(current); futureRanges < partitionRunwayCountLimit; start = self.Granularity.NextRangeStart(start) {
		if !PartitionRangeCovered(leaves, self.Granularity.RangeOf(start)) {
			break
		}
		futureRanges += 1
	}
	return
}

// The statements of the registry's default create of table's leaf for r: a
// standalone table LIKE the parent, the family's leaf storage settings, the
// parent's TRUNCATE guards repeated on the leaf (a statement trigger on the
// parent does not fire for a statement that names a partition), then the
// attach. Read through conn, which may be the create's own transaction.
func (self *PartitionFamily) CreateLeafSql(ctx context.Context, conn PgQuerier, table string, r PartitionRange) (statements []string, err error) {
	leaf := self.LeafName(table, r)
	statements = append(statements, fmt.Sprintf(
		"CREATE TABLE public.%s (LIKE public.%s INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING STORAGE INCLUDING COMPRESSION)",
		quoteIdentifier(leaf),
		quoteIdentifier(table),
	))
	if 0 < len(self.LeafReloptions) {
		statements = append(statements, fmt.Sprintf("ALTER TABLE public.%s SET (%s)", quoteIdentifier(leaf), strings.Join(self.LeafReloptions, ", ")))
	}
	result, queryErr := conn.Query(
		ctx,
		`
		SELECT tgname, pg_get_triggerdef(oid)
		FROM pg_trigger
		WHERE tgrelid = to_regclass($1) AND NOT tgisinternal AND tgparentid = 0 AND (tgtype & 32) <> 0
		ORDER BY tgname
		`,
		"public."+table,
	)
	WithPgResult(result, queryErr, func() {
		for result.Next() {
			var name string
			var definition string
			Raise(result.Scan(&name, &definition))
			retargeted, ok := contractRetargetTrigger(definition, table, leaf)
			if !ok {
				err = fmt.Errorf("cannot repeat trigger %s of %s on %s", name, table, leaf)
				return
			}
			statements = append(statements, retargeted)
		}
	})
	if err != nil {
		return nil, err
	}
	bounds := "DEFAULT"
	if !r.Default {
		lower := "MINVALUE"
		if !r.Lower.IsZero() {
			lower = self.KeyKind.BoundSql(r.Lower)
		}
		upper := "MAXVALUE"
		if !r.Upper.IsZero() {
			upper = self.KeyKind.BoundSql(r.Upper)
		}
		bounds = fmt.Sprintf("FOR VALUES FROM (%s) TO (%s)", lower, upper)
	}
	statements = append(statements, fmt.Sprintf(
		"ALTER TABLE public.%s ATTACH PARTITION public.%s %s",
		quoteIdentifier(table),
		quoteIdentifier(leaf),
		bounds,
	))
	return
}

// Runs callback in one maintenance transaction at read committed, without
// retries, and returns its failure.
func partitionRegistryTx(ctx context.Context, callback func(PgTx)) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if IsDoneError(r) {
				panic(r)
			}
			if recoveredErr, ok := r.(error); ok {
				err = recoveredErr
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	MaintenanceTx(ctx, callback, TxReadCommitted, OptNoRetry())
	return
}

// Runs callback and returns what it raised, as an error.
func partitionRegistryRun(callback func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if IsDoneError(r) {
				panic(r)
			}
			if recoveredErr, ok := r.(error); ok {
				err = recoveredErr
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	callback()
	return
}

// Creates table's leaf for r, with the family's CreateLeaf or the default.
func (self *PartitionFamily) CreateRange(ctx context.Context, table string, r PartitionRange) error {
	if self.CreateLeaf != nil {
		return partitionRegistryRun(func() {
			Raise(self.CreateLeaf(ctx, table, r))
		})
	}
	return partitionRegistryTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", self.settings().CreateLockTimeout.Milliseconds())))
		statements, err := self.CreateLeafSql(ctx, tx, table, r)
		Raise(err)
		for _, statement := range statements {
			RaisePgResult(tx.Exec(ctx, statement))
		}
	})
}

// What one runway pass did for a family.
type PartitionRunwayReport struct {
	Family      string
	Partitioned bool
	Created     []string
	// whether every table covers the current range, and the fewest
	// consecutive covered ranges after it across the tables
	CurrentCovered bool
	FutureRanges   int
	// each create or listing that failed; it is retried on the next pass
	Errors []string
}

// Creates every missing leaf of the creation window in every table, and the
// DEFAULT partitions, in one pass: after downtime the pass catches up at once.
// A failed create is reported and retried on the next pass.
func (self *PartitionFamily) EnsureRunway(ctx context.Context, now time.Time) *PartitionRunwayReport {
	report := &PartitionRunwayReport{Family: self.Name, CurrentCovered: true, FutureRanges: -1}
	if !self.IsPartitioned(ctx) {
		report.FutureRanges = 0
		if self.BeforeCutover != nil {
			if err := partitionRegistryRun(func() {
				Raise(self.BeforeCutover(ctx, now))
			}); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("before cutover: %s", err))
			}
		}
		return report
	}
	report.Partitioned = true
	createFrom := now
	if self.CreateFrom != nil {
		createFrom = self.CreateFrom(ctx, now)
	}
	for _, table := range self.Tables {
		var leaves []PartitionLeaf
		var listErr error
		MaintenanceDb(ctx, func(conn PgConn) {
			leaves, listErr = self.ListLeaves(ctx, conn, table)
		})
		if listErr != nil {
			report.Errors = append(report.Errors, listErr.Error())
			report.CurrentCovered = false
			report.FutureRanges = 0
			continue
		}
		creates := []PartitionRange{}
		if self.DefaultPartition && !slices.ContainsFunc(leaves, func(leaf PartitionLeaf) bool { return leaf.Range.Default }) {
			creates = append(creates, PartitionRange{Default: true})
		}
		creates = append(creates, self.MissingRanges(leaves, createFrom, now)...)
		for _, r := range creates {
			name := self.LeafName(table, r)
			if err := self.CreateRange(ctx, table, r); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("create %s: %s", name, err))
				continue
			}
			report.Created = append(report.Created, name)
			leaves = append(leaves, PartitionLeaf{Table: table, Name: name, Range: r, Canonical: true})
		}
		currentCovered, futureRanges := self.Runway(leaves, now)
		report.CurrentCovered = report.CurrentCovered && currentCovered
		if report.FutureRanges < 0 || futureRanges < report.FutureRanges {
			report.FutureRanges = futureRanges
		}
	}
	if report.FutureRanges < 0 {
		report.FutureRanges = 0
	}
	return report
}

// Creates the range holding t in every table where no leaf covers it yet, for
// a writer whose insert found no partition. Returns the leaves it created.
func (self *PartitionFamily) EnsureRangeFor(ctx context.Context, t time.Time) (created []string, err error) {
	r := self.RangeAt(t)
	for _, table := range self.Tables {
		var leaves []PartitionLeaf
		var listErr error
		MaintenanceDb(ctx, func(conn PgConn) {
			leaves, listErr = self.ListLeaves(ctx, conn, table)
		})
		if listErr != nil {
			return created, listErr
		}
		if partitionRangeOverlapped(leaves, r) {
			continue
		}
		if createErr := self.CreateRange(ctx, table, r); createErr != nil {
			return created, fmt.Errorf("create %s: %w", self.LeafName(table, r), createErr)
		}
		created = append(created, self.LeafName(table, r))
	}
	return
}

// Whether r has expired at now: the family's RangeExpired, or the declared
// retention. The DEFAULT partition, a kept legacy partition, an open upper
// bound and a family without retention never expire.
func (self *PartitionFamily) IsExpired(ctx context.Context, now time.Time, r PartitionRange) bool {
	if r.Default || r.Upper.IsZero() || (r.Legacy && self.KeepLegacyPartition) {
		return false
	}
	if self.RangeExpired != nil {
		return self.RangeExpired(ctx, now, r)
	}
	return 0 < self.Retention && !now.Before(r.Upper.Add(self.Retention))
}

// The expired ranges of the family's own leaves of its first table, oldest
// first. A leaf the registry did not name is never expired.
func (self *PartitionFamily) ExpiredRanges(ctx context.Context, now time.Time) (expired []PartitionRange, err error) {
	var leaves []PartitionLeaf
	MaintenanceDb(ctx, func(conn PgConn) {
		leaves, err = self.ListLeaves(ctx, conn, self.Tables[0])
	})
	if err != nil {
		return nil, err
	}
	for _, leaf := range leaves {
		if leaf.Canonical && self.IsExpired(ctx, now, leaf.Range) {
			expired = append(expired, leaf.Range)
		}
	}
	slices.SortStableFunc(expired, func(a PartitionRange, b PartitionRange) int {
		return a.Upper.Compare(b.Upper)
	})
	return
}

// A drop refused by its precondition in the drop transaction.
type PartitionDropBlockedError struct {
	Blocker string
}

func (self *PartitionDropBlockedError) Error() string {
	return self.Blocker + " after detach"
}

// Detaches and drops r's leaves in one transaction, children first: the
// guard setting, the fence, every detach, the precondition again on the
// detached leaves (which no other statement can reach any more), then the
// drops. A detach takes ACCESS EXCLUSIVE on its parent briefly, so the lock
// wait is short and a busy run retries next time.
func (self *PartitionFamily) DropRange(ctx context.Context, r PartitionRange) (dropped []string, err error) {
	if r.Default {
		return nil, fmt.Errorf("the DEFAULT partition of %s is never dropped", self.Name)
	}
	if r.Legacy && self.KeepLegacyPartition {
		return nil, fmt.Errorf("the legacy partition of %s is never dropped", self.Name)
	}
	tables := slices.Clone(self.Tables)
	slices.Reverse(tables)
	settings := self.settings()
	err = partitionRegistryTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.DropLockTimeout.Milliseconds())))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.DropStatementTimeout.Milliseconds())))
		if self.DropGuardSetting != "" {
			// the name passed partitionSettingNamePattern at registration
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL %s = 'on'", self.DropGuardSetting)))
		}
		if self.DropFence != nil {
			self.DropFence(ctx, tx, r)
		}
		for _, table := range tables {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
				"ALTER TABLE public.%s DETACH PARTITION public.%s",
				quoteIdentifier(table),
				quoteIdentifier(self.LeafName(table, r)),
			)))
		}
		if self.DropPrecondition != nil {
			if blocker := self.DropPrecondition(ctx, tx, r); blocker != "" {
				panic(&PartitionDropBlockedError{Blocker: blocker})
			}
		}
		for _, table := range tables {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("DROP TABLE public.%s", quoteIdentifier(self.LeafName(table, r)))))
		}
	})
	if err != nil {
		return nil, err
	}
	for _, table := range tables {
		dropped = append(dropped, self.LeafName(table, r))
	}
	return
}

// Where a family's interrupted maintenance resumes. The registry keeps one per
// family in its task's arguments.
type PartitionFamilyState struct {
	// the range period (its start, YYYY-MM-DD) whose parents were last
	// analyzed
	AnalyzedPeriod string `json:"analyzed_period,omitempty"`
	// the first table's leaf whose archive step is in progress, and the
	// step's cursor
	ArchiveLeaf   string `json:"archive_leaf,omitempty"`
	ArchiveCursor string `json:"archive_cursor,omitempty"`
}

// What one retirement pass did for a family.
type PartitionRetireReport struct {
	Family  string
	Dropped []string
	// expired ranges that stay, by their first table's leaf, with the reason
	Blocked map[string]string
	// ranges whose archive step finished this pass
	Archived []string
	// work remains for an immediate rerun
	More bool
}

// Retires the family's expired ranges, oldest first, until deadline: each
// passes its precondition, then its archive step, then the guarded drop. An
// archive step cut short by the deadline stops the pass and leaves its cursor
// in the returned state; a blocked range stays and is reported.
func (self *PartitionFamily) Retire(ctx context.Context, now time.Time, state PartitionFamilyState, deadline time.Time) (report *PartitionRetireReport, next PartitionFamilyState) {
	report = &PartitionRetireReport{Family: self.Name, Blocked: map[string]string{}}
	next = state
	if !self.IsPartitioned(ctx) {
		return
	}
	expired, err := self.ExpiredRanges(ctx, now)
	if err != nil {
		glog.Errorf("[partitions]%s: %s\n", self.Name, err)
		return
	}
	for _, r := range expired {
		name := self.LeafName(self.Tables[0], r)
		if !time.Now().Before(deadline) {
			report.More = true
			return
		}
		if self.DropPrecondition != nil {
			var blocker string
			checkErr := partitionRegistryRun(func() {
				MaintenanceDb(ctx, func(conn PgConn) {
					blocker = self.DropPrecondition(ctx, conn, r)
				})
			})
			if checkErr != nil {
				blocker = fmt.Sprintf("precondition: %s", checkErr)
			}
			if blocker != "" {
				report.Blocked[name] = blocker
				continue
			}
		}
		if self.Archive != nil {
			cursor := ""
			if state.ArchiveLeaf == name {
				cursor = state.ArchiveCursor
			}
			var nextCursor string
			var done bool
			archiveErr := partitionRegistryRun(func() {
				var stepErr error
				nextCursor, done, stepErr = self.Archive(ctx, r, cursor, deadline)
				Raise(stepErr)
			})
			if archiveErr != nil {
				report.Blocked[name] = fmt.Sprintf("archive: %s", archiveErr)
				continue
			}
			if !done {
				next.ArchiveLeaf = name
				next.ArchiveCursor = nextCursor
				report.More = true
				return
			}
			report.Archived = append(report.Archived, name)
			next.ArchiveLeaf = ""
			next.ArchiveCursor = ""
		}
		dropped, dropErr := self.DropRange(ctx, r)
		if dropErr != nil {
			report.Blocked[name] = dropErr.Error()
			continue
		}
		report.Dropped = append(report.Dropped, dropped...)
	}
	return
}

// One ANALYZE in its own maintenance transaction, without a statement or
// transaction bound: a large leaf's ANALYZE can outlast the role's 15 minutes.
func partitionRegistryAnalyze(ctx context.Context, statement string) bool {
	err := partitionRegistryTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = 0`))
		RaisePgResult(tx.Exec(ctx, liftTransactionTimeoutInTxSql))
		RaisePgResult(tx.Exec(ctx, statement))
	})
	if err != nil {
		glog.Errorf("[partitions]%s: %s\n", statement, err)
		return false
	}
	return true
}

// Analyzes each parent (ANALYZE ONLY: autovacuum never analyzes a
// partitioned parent) once per range period: when the period holding now is
// not analyzedPeriod and time is left. The next period is returned once every
// parent is analyzed, so a failed one is retried.
func (self *PartitionFamily) AnalyzeParents(ctx context.Context, now time.Time, analyzedPeriod string, deadline time.Time) (analyzed []string, nextAnalyzedPeriod string) {
	nextAnalyzedPeriod = analyzedPeriod
	period := self.Granularity.RangeStart(now).Format(time.DateOnly)
	if period == analyzedPeriod || !time.Now().Before(deadline) {
		return
	}
	parents := true
	for _, table := range self.Tables {
		if partitionRegistryAnalyze(ctx, fmt.Sprintf("ANALYZE ONLY public.%s", quoteIdentifier(table))) {
			analyzed = append(analyzed, table)
		} else {
			parents = false
		}
	}
	if parents {
		nextAnalyzedPeriod = period
	}
	return
}

// Analyzes each closed range leaf (not the legacy or DEFAULT partition) once,
// LeafAnalyzeDelay after its range ends, unless it was analyzed since then.
func (self *PartitionFamily) AnalyzeClosedLeaves(ctx context.Context, now time.Time, deadline time.Time) (analyzed []string) {
	if self.LeafAnalyzeDelay <= 0 {
		return
	}
	for _, table := range self.Tables {
		var leaves []PartitionLeaf
		var listErr error
		MaintenanceDb(ctx, func(conn PgConn) {
			leaves, listErr = self.ListLeaves(ctx, conn, table)
		})
		if listErr != nil {
			glog.Errorf("[partitions]%s\n", listErr)
			continue
		}
		for _, leaf := range leaves {
			if !leaf.Canonical || leaf.Range.Legacy || leaf.Range.Default || leaf.Range.Upper.IsZero() {
				continue
			}
			settled := leaf.Range.Upper.Add(self.LeafAnalyzeDelay)
			if now.Before(settled) || !time.Now().Before(deadline) {
				continue
			}
			var lastAnalyze *time.Time
			MaintenanceDb(ctx, func(conn PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT GREATEST(last_analyze, last_autoanalyze) FROM pg_stat_user_tables WHERE relid = to_regclass($1)`,
					"public."+leaf.Name,
				)
				WithPgResult(result, err, func() {
					if result.Next() {
						Raise(result.Scan(&lastAnalyze))
					}
				})
			})
			if lastAnalyze != nil && !lastAnalyze.Before(settled) {
				continue
			}
			if partitionRegistryAnalyze(ctx, fmt.Sprintf("ANALYZE public.%s", quoteIdentifier(leaf.Name))) {
				analyzed = append(analyzed, leaf.Name)
			}
		}
	}
	return
}

// Creates ahead for every registered family (EnsureRunway), the runway task's
// pass. A family's failures are its report's.
func MaintainPartitionRunways(ctx context.Context, now time.Time) (reports []*PartitionRunwayReport) {
	for _, family := range PartitionFamilies() {
		var report *PartitionRunwayReport
		if err := partitionRegistryRun(func() {
			report = family.EnsureRunway(ctx, now)
		}); err != nil {
			report = &PartitionRunwayReport{Family: family.Name, Errors: []string{err.Error()}}
		}
		reports = append(reports, report)
	}
	return
}

// The registry's resume state across families, carried in the retention
// task's arguments.
type PartitionMaintenanceState struct {
	Families map[string]PartitionFamilyState `json:"families,omitempty"`
	// the family the next pass starts with, so a family that runs out of
	// time does not starve the ones after it
	ResumeFamily string `json:"resume_family,omitempty"`
}

// What one retention pass did.
type PartitionRetentionReport struct {
	// leaves and parents analyzed, by family
	Analyzed map[string][]string
	Retired  []*PartitionRetireReport
	More     bool
}

// The retention task's pass, within budget: for every registered, partitioned
// family, analyzes its parents and closed leaves unless AnalyzeByOwnTask, and
// retires its expired ranges unless RetireByOwnTask. The pass starts with the
// family the last one stopped at.
func MaintainPartitionRetention(ctx context.Context, now time.Time, state PartitionMaintenanceState, budget time.Duration) (report *PartitionRetentionReport, next PartitionMaintenanceState) {
	report = &PartitionRetentionReport{Analyzed: map[string][]string{}}
	deadline := time.Now().Add(budget)
	next = PartitionMaintenanceState{Families: map[string]PartitionFamilyState{}}
	families := PartitionFamilies()
	start := slices.IndexFunc(families, func(family *PartitionFamily) bool {
		return family.Name == state.ResumeFamily
	})
	if 0 < start {
		families = append(families[start:], families[:start]...)
	}
	for _, family := range families {
		familyState := state.Families[family.Name]
		next.Families[family.Name] = familyState
		if !time.Now().Before(deadline) {
			if next.ResumeFamily == "" {
				next.ResumeFamily = family.Name
			}
			report.More = true
			continue
		}
		if !family.AnalyzeByOwnTask || !family.RetireByOwnTask {
			partitioned := false
			if err := partitionRegistryRun(func() {
				partitioned = family.IsPartitioned(ctx)
			}); err != nil || !partitioned {
				continue
			}
		}
		if !family.AnalyzeByOwnTask {
			err := partitionRegistryRun(func() {
				analyzed := family.AnalyzeClosedLeaves(ctx, now, deadline)
				var parents []string
				parents, familyState.AnalyzedPeriod = family.AnalyzeParents(ctx, now, familyState.AnalyzedPeriod, deadline)
				if analyzed = append(analyzed, parents...); 0 < len(analyzed) {
					report.Analyzed[family.Name] = analyzed
				}
			})
			if err != nil {
				glog.Errorf("[partitions]%s analyze: %s\n", family.Name, err)
			}
		}
		if !family.RetireByOwnTask {
			var retireReport *PartitionRetireReport
			retireState := familyState
			err := partitionRegistryRun(func() {
				retireReport, retireState = family.Retire(ctx, now, familyState, deadline)
			})
			if err != nil {
				glog.Errorf("[partitions]%s retire: %s\n", family.Name, err)
			} else {
				familyState = retireState
				report.Retired = append(report.Retired, retireReport)
				if retireReport.More {
					report.More = true
					if next.ResumeFamily == "" {
						next.ResumeFamily = family.Name
					}
				}
			}
		}
		next.Families[family.Name] = familyState
	}
	return
}

// Whether db_maintenance's REINDEX rotation leaves table alone because a
// ReindexExempt family owns it: one of its parents once partitioned, or one of
// its own leaves.
func PartitionRegistryReindexExempt(table string, partitioned bool) bool {
	return registeredPartitionFamilies.reindexExempt(table, partitioned)
}

func (self *partitionRegistry) reindexExempt(table string, partitioned bool) bool {
	for _, family := range self.list() {
		if !family.ReindexExempt {
			continue
		}
		if slices.Contains(family.Tables, table) {
			if partitioned {
				return true
			}
			continue
		}
		if family.ownsTable(table) {
			return true
		}
	}
	return false
}
