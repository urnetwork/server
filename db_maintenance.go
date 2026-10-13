// Scheduled database maintenance: a hash rotation rebuilds each public table's
// indexes once per DbReindexEpochs epochs with REINDEX CONCURRENTLY, measures
// each named high-churn partial index of a table too large for the rotation
// and, once an operator enables it, rebuilds it whenever it is bloated, removes
// the invalid copies an interrupted concurrent rebuild leaves behind, and
// refreshes planner statistics.
//
// A concurrent rebuild first adds an invalid _ccnew copy of every index, builds
// and validates each copy while waiting for older transactions, then swaps the
// copies in and drops the old indexes (renamed _ccold). A cancel, timeout, lock
// timeout, or client disconnect before the drop leaves those copies behind;
// each stays maintained by every write and is never read until it is dropped.
// So the rotation drops debris only when it is provably the copy of a valid
// index, cleans before and after every rebuild (after a failed or canceled one
// on a context detached from the canceled one), starts a rebuild only while no
// long transaction, other index build, or cpu saturation would stall or
// overload it, leaves tables above a size bound to cleanup unless an operator
// opts in, and stops at the first interruption or deferral with a record of
// what finished, so its task continues the same epoch instead of skipping it.
//
// No rebuild, drop, vacuum or analyze queues behind a lock that an autovacuum
// holds: after deadlock_timeout PostgreSQL cancels an autovacuum that blocks
// another session, and a large table's vacuum then starts over. The vacuum
// and analyze skip a locked table (SKIP_LOCKED); a rebuild or drop waits for
// its table to clear without taking a place in the lock queue, then leaves the
// table for the continuation or a later cleanup.
package server

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	mathrand "math/rand"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/glog"
)

const DbReindexEpochs = uint64(8)

// The automatic rotation rebuilds a whole table only when its live size (heap,
// toast, and valid indexes, summed over partition leaves) is at most this. A
// whole-table rebuild scans the heap twice per index and keeps a second copy of
// every index until its swap; a larger table such as network_client stays
// cleanup-only unless an operator run sets IncludeLargeTables.
const DefaultDbMaintenanceMaxReindexTableBytes = int64(16 * 1024 * 1024 * 1024)

// A concurrent rebuild waits for every transaction holding an older snapshot,
// such as pg_dump or a long payout, so a rebuild starts only while no
// transaction in the database is this old.
const DefaultDbMaintenanceMaxTransactionAge = 10 * time.Minute

// Backends of the database that are active with no wait event are on a cpu or
// queued for one. At this many the primary (96 logical cpus) is saturated and a
// rebuild only adds to it.
const DefaultDbMaintenanceMaxRunningBackends = 48

// Bounds each lock and snapshot wait of a rebuild. A transaction that starts
// during the rebuild otherwise stalls it, holding its table's
// ShareUpdateExclusiveLock, for as long as that transaction runs.
const DefaultDbMaintenanceReindexLockTimeout = 10 * time.Minute

// Bounds each lock wait of a debris drop. DROP INDEX CONCURRENTLY waits for
// every transaction holding a lock on the table; a later cleanup retries.
const DefaultDbMaintenanceCleanupLockTimeout = 10 * time.Second

// How often a rebuild or drop rereads the locks it waits to clear; see
// dbMaintenanceAwaitTableUnlocked.
const dbMaintenanceLockPollInterval = time.Second

// The daily pending_task vacuum and the final analyze take each table's
// ShareUpdateExclusiveLock only when it is free. Either one, queued for
// deadlock_timeout (1s) behind an autovacuum's lock, made PostgreSQL cancel
// that autovacuum, and a large table's vacuum then starts over: from
// 2026-10-13 07:11:59 to 07:12:12Z the maintenance session cancelled the
// autovacuums of transfer_escrow (while it vacuumed transfer_escrow_pkey),
// transfer_contract (at heap block 51.2M of 62.7M) and pending_task. A skipped
// table is being vacuumed or analyzed by the session that holds it, or is
// analyzed by the next run.
const dbMaintenanceVacuumPendingTaskStatement = `VACUUM (ANALYZE, SKIP_LOCKED) pending_task`
const dbMaintenanceAnalyzeStatement = `ANALYZE (SKIP_LOCKED)`

// Bounds a single rebuild statement.
const dbMaintenanceReindexTimeout = 2 * time.Hour

// Cleanup after a failed or canceled rebuild runs detached from the caller's
// context for at most this long, which fits inside the taskworker drain's 30s
// cancel window.
const dbMaintenanceFailedReindexCleanupTimeout = 20 * time.Second

// Partial indexes of tables too large to rebuild whole, whose predicate admits
// a row at insert and drops it soon after: a contract is open, unresolved and
// without an outcome until it closes and settles, its reap time is set until it
// is reaped, and an escrow is unsettled until its contract settles. Each exit
// is a non-HOT update or a delete that leaves a dead entry, and vacuum frees a
// btree page only once every entry on it is gone. Rows that stay in the
// predicate for days pin nearly every leaf page with a few live entries, and
// new entries land at the right edge of time-ordered keys, so freed space is
// not reused and the index grows with its churn rather than its live rows (on
// 2026-10-12 these held about 310 GB beyond their 55 GB of live entries). Each
// is rebuilt on its own whenever it is bloated; see
// dbMaintenancePriorityIndex.bloated.
var priorityReindexIndexes = []string{
	"transfer_contract_open_partial_create_time",
	"transfer_contract_outcome_null",
	"transfer_contract_reap_time",
	"transfer_contract_unresolved_destination_pair_create_time",
	"transfer_contract_unresolved_payer_transfer_byte_count",
	"transfer_contract_unresolved_source_pair_create_time",
	"transfer_escrow_unsettled_balance_contract",
}

// A priority index is bloated once it is at least this many times its
// estimated compact size, i.e. at least half of it is reclaimable.
const dbMaintenancePriorityIndexBloatFactor = 2

// A rebuild reads its table's heap twice, so a bloated index is rebuilt only
// when the bytes it reclaims are at least the heap's bytes divided by this,
// and at least dbMaintenancePriorityIndexMinReclaimBytes.
const dbMaintenancePriorityIndexHeapDivisor = 100

const dbMaintenancePriorityIndexMinReclaimBytes = int64(1024 * 1024)

// Each priority rebuild reads its table's heap twice (about 1 TB for
// transfer_contract), so a run rebuilds at most this many; the rest wait for
// the next run.
const DefaultDbMaintenanceMaxPriorityRebuilds = 3

// A priority rebuild reclaims tens of GB, so it starts behind, and waits out,
// transactions up to this old: the hour-long payout and reliability
// transactions that defer the table rotation would otherwise defer every
// priority rebuild too.
const DefaultDbMaintenancePriorityMaxTransactionAge = time.Hour

const DefaultDbMaintenancePriorityReindexLockTimeout = time.Hour

// A priority rebuild reads all of transfer_contract's or transfer_escrow's heap
// twice, so the rebuilds stay off until an operator enables them in
// db_maintenance.yml, the maintenance pool's config resource:
//
//	priority_reindex_enabled: true
//
// Until then each run measures and logs the priority indexes and rebuilds
// none. The scheduled task reads the value when a run starts, so a change
// applies to the next run. An absent or unreadable resource, or an absent or
// malformed value, keeps the rebuilds off.
func DbMaintenancePriorityReindexEnabled() bool {
	resource, err := Config.SimpleResource(MaintenancePgConfigResourceName)
	if err != nil {
		return false
	}
	var config struct {
		PriorityReindexEnabled *bool `yaml:"priority_reindex_enabled"`
	}
	if err := resource.UnmarshalYamlE(&config); err != nil {
		return false
	}
	return config.PriorityReindexEnabled != nil && *config.PriorityReindexEnabled
}

// Whether indexName is a priority index or a transient copy that a concurrent
// rebuild makes of one, so a monitor can tell the bounded rebuild of one
// partial index from a whole-table rebuild of its table. A schema-qualified
// public name is accepted.
func IsDbMaintenancePriorityIndex(indexName string) bool {
	indexName = strings.TrimPrefix(indexName, "public.")
	for _, name := range priorityReindexIndexes {
		if indexName == name || isReindexArtifactOf(indexName, name) {
			return true
		}
	}
	return false
}

// One column of a btree index, in index column order.
type dbMaintenanceIndexColumn struct {
	// pg_attribute.attlen of the index column: its fixed width, or negative for
	// a variable width
	length int
	// pg_attribute.attalign: c, s, i or d
	align byte
	// from pg_stats for the indexed table column, or for the index itself when
	// the column is an expression; false when it has not been analyzed
	analyzed     bool
	averageWidth int
	nullFraction float64
}

// Estimates the bytes of a fresh build of entryCount entries: leaf pages filled
// to fillfactor with the average leaf tuple (its header, a null bitmap when a
// column can be null, the aligned column data less the expected width of null
// columns, and its line pointer) plus the metapage. Internal pages add under
// 2% and are left out. False when the entry count is unknown (a never vacuumed
// or analyzed index has reltuples -1) or a variable-width column has not been
// analyzed.
func dbMaintenanceEstimateBtreeBytes(
	columns []dbMaintenanceIndexColumn,
	entryCount float64,
	fillfactor int,
	blockSize int64,
) (int64, bool) {
	if len(columns) == 0 || entryCount < 0 || fillfactor <= 0 || 100 < fillfactor || blockSize <= 0 {
		return 0, false
	}
	alignUp := func(n int, alignment int) int {
		return (n + alignment - 1) / alignment * alignment
	}
	dataBytes := 0
	expectedNullBytes := 0.0
	nullable := false
	for _, column := range columns {
		width := column.length
		if width <= 0 {
			if !column.analyzed {
				return 0, false
			}
			width = column.averageWidth
		}
		alignment := 1
		switch column.align {
		case 's':
			alignment = 2
		case 'i':
			alignment = 4
		case 'd':
			alignment = 8
		}
		dataBytes = alignUp(dataBytes, alignment) + width
		if 0 < column.nullFraction {
			nullable = true
			expectedNullBytes += column.nullFraction * float64(width)
		}
	}
	// IndexTupleData; a null bitmap moves the data to the next 8-byte boundary
	headerBytes := 8
	if nullable {
		headerBytes = 16
	}
	tupleBytes := alignUp(headerBytes+int(math.Ceil(float64(dataBytes)-expectedNullBytes)), 8)
	// plus its line pointer
	entryBytes := int64(tupleBytes + 4)
	// less the page header and the btree special space
	usableBytes := (blockSize - 24 - 16) * int64(fillfactor) / 100
	entriesPerPage := max(usableBytes/entryBytes, 1)
	leafPages := int64(math.Ceil(entryCount / float64(entriesPerPage)))
	return (leafPages + 1) * blockSize, true
}

// A priority index's size against its estimated compact size.
type dbMaintenancePriorityIndex struct {
	indexName  string
	tableName  string
	indexBytes int64
	// the main fork of the owning table, which a rebuild reads twice
	heapBytes int64
	// zero when the compact size cannot be estimated
	compactBytes int64
}

// Whether a rebuild is due: the index is at least the bloat factor times its
// compact estimate, and the bytes it would reclaim justify reading the heap
// twice.
func (self *dbMaintenancePriorityIndex) bloated() bool {
	if self.compactBytes <= 0 {
		return false
	}
	reclaimableBytes := self.indexBytes - self.compactBytes
	return dbMaintenancePriorityIndexBloatFactor*self.compactBytes <= self.indexBytes &&
		dbMaintenancePriorityIndexMinReclaimBytes <= reclaimableBytes &&
		self.heapBytes <= reclaimableBytes*dbMaintenancePriorityIndexHeapDivisor
}

// The bloated indexes not yet done this epoch, smallest first, at most
// maxRebuilds of them.
func dbMaintenanceSelectPriorityIndexes(
	priorityIndexes []dbMaintenancePriorityIndex,
	doneIndexNames []string,
	maxRebuilds int,
) []dbMaintenancePriorityIndex {
	selectedIndexes := []dbMaintenancePriorityIndex{}
	for _, priorityIndex := range priorityIndexes {
		if priorityIndex.bloated() && !slices.Contains(doneIndexNames, priorityIndex.indexName) {
			selectedIndexes = append(selectedIndexes, priorityIndex)
		}
	}
	slices.SortFunc(selectedIndexes, func(a dbMaintenancePriorityIndex, b dbMaintenancePriorityIndex) int {
		return cmp.Or(cmp.Compare(a.indexBytes, b.indexBytes), strings.Compare(a.indexName, b.indexName))
	})
	if maxRebuilds < len(selectedIndexes) {
		selectedIndexes = selectedIndexes[:max(maxRebuilds, 0)]
	}
	return selectedIndexes
}

// One row per column of each named index that is a valid btree index on an
// ordinary public table, in index column order. A column's statistics come
// from its table column, or from the index for an expression column.
const dbMaintenancePriorityIndexColumnsQuery = `
	SELECT
		index_class.relname,
		table_class.relname,
		pg_relation_size(index_class.oid),
		pg_relation_size(table_class.oid),
		index_class.reltuples::float8,
		coalesce((
			SELECT index_option.option_value::int
			FROM pg_options_to_table(index_class.reloptions) index_option
			WHERE index_option.option_name = 'fillfactor'
		), 90),
		current_setting('block_size')::bigint,
		index_attribute.attlen::int,
		index_attribute.attalign::text,
		column_stats.avg_width::int,
		column_stats.null_frac::float8
	FROM pg_class index_class
	INNER JOIN pg_namespace index_namespace ON index_namespace.oid = index_class.relnamespace
	INNER JOIN pg_am index_am ON index_am.oid = index_class.relam
	INNER JOIN pg_index index_state ON index_state.indexrelid = index_class.oid
	INNER JOIN pg_class table_class ON table_class.oid = index_state.indrelid
	INNER JOIN pg_attribute index_attribute
		ON index_attribute.attrelid = index_class.oid
		AND 0 < index_attribute.attnum
	LEFT JOIN pg_attribute table_attribute
		ON table_attribute.attrelid = table_class.oid
		AND table_attribute.attnum = index_state.indkey[index_attribute.attnum - 1]
		AND 0 < table_attribute.attnum
	LEFT JOIN pg_stats column_stats
		ON column_stats.schemaname = 'public'
		AND NOT column_stats.inherited
		AND (
			(column_stats.tablename = table_class.relname AND column_stats.attname = table_attribute.attname)
			OR (table_attribute.attname IS NULL AND column_stats.tablename = index_class.relname AND column_stats.attname = index_attribute.attname)
		)
	WHERE index_namespace.nspname = 'public'
	  AND index_class.relname = ANY($1::text[])
	  AND index_am.amname = 'btree'
	  AND index_state.indisvalid
	  AND table_class.relkind = 'r'
	ORDER BY index_class.relname, index_attribute.attnum
`

// The size and compact estimate of each named index that exists as a valid
// btree index on an ordinary public table; a dropped or renamed name is
// skipped. An index whose compact size cannot be estimated has compactBytes 0
// and is never bloated.
func dbMaintenancePriorityIndexSizes(ctx context.Context, indexNames []string) (priorityIndexes []dbMaintenancePriorityIndex, returnErr error) {
	type indexState struct {
		priorityIndex dbMaintenancePriorityIndex
		entryCount    float64
		fillfactor    int
		blockSize     int64
		columns       []dbMaintenanceIndexColumn
	}
	indexStates := []*indexState{}
	HandleError(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, dbMaintenancePriorityIndexColumnsQuery, indexNames)
			WithPgResult(result, err, func() {
				for result.Next() {
					var state indexState
					var column dbMaintenanceIndexColumn
					var align string
					var averageWidth *int32
					var nullFraction *float64
					Raise(result.Scan(
						&state.priorityIndex.indexName,
						&state.priorityIndex.tableName,
						&state.priorityIndex.indexBytes,
						&state.priorityIndex.heapBytes,
						&state.entryCount,
						&state.fillfactor,
						&state.blockSize,
						&column.length,
						&align,
						&averageWidth,
						&nullFraction,
					))
					if align != "" {
						column.align = align[0]
					}
					if averageWidth != nil && nullFraction != nil {
						column.analyzed = true
						column.averageWidth = int(*averageWidth)
						column.nullFraction = *nullFraction
					}
					n := len(indexStates)
					if n == 0 || indexStates[n-1].priorityIndex.indexName != state.priorityIndex.indexName {
						indexStates = append(indexStates, &state)
					}
					last := indexStates[len(indexStates)-1]
					last.columns = append(last.columns, column)
				}
			})
		}, OptNoRetry())
	}, func(recovered error) {
		returnErr = recovered
	})
	if returnErr != nil {
		return nil, returnErr
	}
	for _, state := range indexStates {
		if compactBytes, ok := dbMaintenanceEstimateBtreeBytes(state.columns, state.entryCount, state.fillfactor, state.blockSize); ok {
			state.priorityIndex.compactBytes = compactBytes
		}
		priorityIndexes = append(priorityIndexes, state.priorityIndex)
	}
	return priorityIndexes, nil
}

// These tables are too large or too frequently updated to rebuild as a whole;
// they use partition turnover, targeted indexes, or tuned autovacuum plus an
// explicitly scheduled one-time pg_repack instead.
var dbMaintenanceSkipReindexTables = map[string]bool{
	"client_reliability":                  true,
	"contract_participant":                true,
	"contract_close":                      true,
	"network_client":                      true,
	"network_client_location_reliability": true,
	"network_client_connection":           true,
	"transfer_contract":                   true,
	"transfer_escrow":                     true,
	"transfer_escrow_sweep":               true,
}

// Daily reliability partitions are dropped whole at retention, so their
// indexes never live long enough to benefit from reindexing.
var dbMaintenanceSkipReindexTablePattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^client_reliability_p[0-9]{8}$`)
})

func dbMaintenanceShouldReindexTable(tableName string) bool {
	return !dbMaintenanceSkipReindexTables[tableName] &&
		!dbMaintenanceSkipReindexTablePattern().MatchString(tableName)
}

// Returns the ChooseRelationName label that ends a REINDEX CONCURRENTLY
// transient index name: ccnew or ccold, optionally followed by the conflict
// counter PostgreSQL appends (1, 2, ...).
func reindexArtifactLabel(indexName string) (string, bool) {
	i := strings.LastIndexByte(indexName, '_')
	if i < 0 {
		return "", false
	}
	label := indexName[i+1:]
	for _, base := range []string{"ccnew", "ccold"} {
		counter, ok := strings.CutPrefix(label, base)
		if !ok {
			continue
		}
		if counter == "" {
			return label, true
		}
		if counter[0] != '0' {
			if _, err := strconv.ParseUint(counter, 10, 32); err == nil {
				return label, true
			}
		}
	}
	return "", false
}

// Mirrors PostgreSQL's makeObjectName(original, NULL, label), the transient
// name REINDEX CONCURRENTLY gives a copy of original: original is clipped at a
// character boundary so the whole name fits in NAMEDATALEN-1 bytes.
func reindexArtifactName(original string, label string) string {
	const maxNameBytes = 63
	available := maxNameBytes - len(label) - 1
	if available < len(original) {
		n := available
		for 0 < n && !utf8.RuneStart(original[n]) {
			n -= 1
		}
		original = original[:n]
	}
	return original + "_" + label
}

// Whether candidate is the transient name REINDEX CONCURRENTLY gives a copy of
// original.
func isReindexArtifactOf(candidate string, original string) bool {
	label, ok := reindexArtifactLabel(candidate)
	return ok && candidate != original && reindexArtifactName(original, label) == candidate
}

// Whether the name carries a REINDEX CONCURRENTLY transient suffix. The suffix
// alone never authorizes a drop; see isReindexArtifactOf.
func isIncompleteIndexName(indexName string) bool {
	_, ok := reindexArtifactLabel(indexName)
	return ok
}

func DefaultDbMaintenanceOptions() *DbMaintenanceOptions {
	return &DbMaintenanceOptions{
		Reindex: true,
		Cleanup: true,
		Analyze: true,
	}
}

// A zero bound selects its default.
type DbMaintenanceOptions struct {
	Reindex bool
	Cleanup bool
	Analyze bool
	// explicit operator opt-in that lifts MaxReindexTableBytes
	IncludeLargeTables   bool
	MaxReindexTableBytes int64
	MaxTransactionAge    time.Duration
	MaxRunningBackends   int
	ReindexLockTimeout   time.Duration
	CleanupLockTimeout   time.Duration
	// Rebuild the bloated priority indexes. Without it a run measures and logs
	// them and rebuilds none; see DbMaintenancePriorityReindexEnabled.
	PriorityReindex bool
	// bounds of the bloat-triggered priority index rebuilds
	MaxPriorityRebuilds        int
	PriorityMaxTransactionAge  time.Duration
	PriorityReindexLockTimeout time.Duration
	// finished by an earlier interrupted or deferred run of the same epoch
	DoneTables  []string
	DoneIndexes []string
}

func (self *DbMaintenanceOptions) maxReindexTableBytes() int64 {
	if 0 < self.MaxReindexTableBytes {
		return self.MaxReindexTableBytes
	}
	return DefaultDbMaintenanceMaxReindexTableBytes
}

func (self *DbMaintenanceOptions) maxTransactionAge() time.Duration {
	if 0 < self.MaxTransactionAge {
		return self.MaxTransactionAge
	}
	return DefaultDbMaintenanceMaxTransactionAge
}

func (self *DbMaintenanceOptions) maxRunningBackends() int {
	if 0 < self.MaxRunningBackends {
		return self.MaxRunningBackends
	}
	return DefaultDbMaintenanceMaxRunningBackends
}

func (self *DbMaintenanceOptions) reindexLockTimeout() time.Duration {
	if 0 < self.ReindexLockTimeout {
		return self.ReindexLockTimeout
	}
	return DefaultDbMaintenanceReindexLockTimeout
}

func (self *DbMaintenanceOptions) cleanupLockTimeout() time.Duration {
	if 0 < self.CleanupLockTimeout {
		return self.CleanupLockTimeout
	}
	return DefaultDbMaintenanceCleanupLockTimeout
}

func (self *DbMaintenanceOptions) maxPriorityRebuilds() int {
	if 0 < self.MaxPriorityRebuilds {
		return self.MaxPriorityRebuilds
	}
	return DefaultDbMaintenanceMaxPriorityRebuilds
}

func (self *DbMaintenanceOptions) priorityMaxTransactionAge() time.Duration {
	if 0 < self.PriorityMaxTransactionAge {
		return self.PriorityMaxTransactionAge
	}
	return DefaultDbMaintenancePriorityMaxTransactionAge
}

func (self *DbMaintenanceOptions) priorityReindexLockTimeout() time.Duration {
	if 0 < self.PriorityReindexLockTimeout {
		return self.PriorityReindexLockTimeout
	}
	return DefaultDbMaintenancePriorityReindexLockTimeout
}

// Why a run stopped before finishing its epoch.
type DbMaintenanceStop string

const (
	// the context ended (drain, deadline, or caller)
	DbMaintenanceInterrupted DbMaintenanceStop = "interrupted"
	// the load guard refused the next rebuild, or a rebuild's table stayed
	// locked by another session
	DbMaintenanceDeferred DbMaintenanceStop = "deferred"
)

// The done lists include the options' done lists, so a continuation passes
// them back unchanged.
type DbMaintenanceOutcome struct {
	// empty when the epoch finished
	Stopped     DbMaintenanceStop
	Reason      string
	DoneTables  []string
	DoneIndexes []string
}

// One sample of the database state that decides whether a rebuild may start.
type dbMaintenanceLoad struct {
	oldestTransactionAge time.Duration
	otherIndexBuilds     int
	runningBackends      int
}

// Returns why a rebuild must not start now, or "" when it may. Another index
// build would contend with it for the same old snapshots and cpu, an old
// transaction would hold it in a snapshot wait, and saturation means it would
// take cpu from user traffic. A table rebuild and a priority index rebuild
// pass different transaction age bounds.
func dbMaintenanceDeferReason(load dbMaintenanceLoad, maxTransactionAge time.Duration, maxRunningBackends int) string {
	if 0 < load.otherIndexBuilds {
		return fmt.Sprintf("%d other index build(s) in progress", load.otherIndexBuilds)
	}
	if maxTransactionAge <= load.oldestTransactionAge {
		precision := time.Second
		if load.oldestTransactionAge < time.Second {
			precision = time.Millisecond
		}
		return fmt.Sprintf(
			"a transaction has been open for %s (limit %s)",
			load.oldestTransactionAge.Round(precision),
			maxTransactionAge,
		)
	}
	if maxRunningBackends <= load.runningBackends {
		return fmt.Sprintf("%d backends running on cpu (limit %d)", load.runningBackends, maxRunningBackends)
	}
	return ""
}

// Scoped to the current database, which is the scope of a concurrent
// rebuild's snapshot waits. Autovacuum is exempt from those waits.
const dbMaintenanceLoadQuery = `
	SELECT
		coalesce((
			SELECT max(extract(epoch FROM now() - activity.xact_start))
			FROM pg_stat_activity activity
			WHERE activity.datname = current_database()
			  AND activity.pid <> pg_backend_pid()
			  AND activity.xact_start IS NOT NULL
			  AND activity.backend_type <> 'autovacuum worker'
		), 0)::float8,
		(
			SELECT count(*)
			FROM pg_stat_progress_create_index progress
			WHERE progress.datname = current_database()
			  AND progress.pid <> pg_backend_pid()
		)::int,
		(
			SELECT count(*)
			FROM pg_stat_activity activity
			WHERE activity.datname = current_database()
			  AND activity.pid <> pg_backend_pid()
			  AND activity.state = 'active'
			  AND activity.wait_event_type IS NULL
		)::int
`

// Lock modes that conflict with the ShareUpdateExclusiveLock that REINDEX
// CONCURRENTLY and DROP INDEX CONCURRENTLY take on the table first.
var dbMaintenanceShareUpdateExclusiveConflicts = map[string]bool{
	"ShareUpdateExclusiveLock": true,
	"ShareLock":                true,
	"ShareRowExclusiveLock":    true,
	"ExclusiveLock":            true,
	"AccessExclusiveLock":      true,
}

// One granted relation lock that another backend holds in a table's scope (see
// dbMaintenanceTableLocksQuery). pid is zero for a prepared transaction.
type dbMaintenanceHeldLock struct {
	pid         int32
	backendType string
	relation    string
	mode        string
}

// Returns why a REINDEX CONCURRENTLY or DROP INDEX CONCURRENTLY must not queue
// for its table's lock now, or "" when nothing it would wait for is held. A
// statement that waits for deadlock_timeout (1s) behind a lock an autovacuum
// worker holds makes PostgreSQL cancel that autovacuum, and a vacuum of a large
// table then starts over, so any lock of an autovacuum worker in the scope
// defers, whatever its mode; another backend's lock defers when its mode
// conflicts.
func dbMaintenanceLockConflictReason(heldLocks []dbMaintenanceHeldLock) string {
	for _, heldLock := range heldLocks {
		if heldLock.backendType == "autovacuum worker" {
			return fmt.Sprintf("autovacuum pid %d holds %s on %s", heldLock.pid, heldLock.mode, heldLock.relation)
		}
	}
	for _, heldLock := range heldLocks {
		if dbMaintenanceShareUpdateExclusiveConflicts[heldLock.mode] {
			backendType := heldLock.backendType
			if backendType == "" {
				backendType = "prepared transaction"
			}
			return fmt.Sprintf("%s pid %d holds %s on %s", backendType, heldLock.pid, heldLock.mode, heldLock.relation)
		}
	}
	return ""
}

// The granted relation locks that other backends hold on a public table ($1),
// the members of its partition tree, their toast tables, and the indexes of all
// of those: everything REINDEX TABLE CONCURRENTLY locks, and a superset of what
// REINDEX INDEX CONCURRENTLY and DROP INDEX CONCURRENTLY lock.
const dbMaintenanceTableLocksQuery = `
	WITH member AS (
		SELECT format('%I.%I', 'public', $1::text)::regclass::oid AS relid
		UNION
		SELECT tree.relid
		FROM pg_partition_tree(format('%I.%I', 'public', $1::text)::regclass) tree
	), scoped_relation AS (
		SELECT member.relid
		FROM member
		UNION
		SELECT table_class.reltoastrelid
		FROM member
		INNER JOIN pg_class table_class ON table_class.oid = member.relid
		WHERE table_class.reltoastrelid <> 0
	), scoped AS (
		SELECT scoped_relation.relid
		FROM scoped_relation
		UNION
		SELECT index_state.indexrelid
		FROM scoped_relation
		INNER JOIN pg_index index_state ON index_state.indrelid = scoped_relation.relid
	)
	SELECT coalesce(held.pid, 0),
	       coalesce(activity.backend_type, ''),
	       held.relation::regclass::text,
	       held.mode
	FROM pg_locks held
	INNER JOIN scoped ON scoped.relid = held.relation
	LEFT JOIN pg_stat_activity activity ON activity.pid = held.pid
	WHERE held.locktype = 'relation'
	  AND held.database = (SELECT oid FROM pg_database WHERE datname = current_database())
	  AND held.granted
	  AND held.pid IS DISTINCT FROM pg_backend_pid()
	ORDER BY 1, 3, 4
`

// Waits, without queuing on any lock, until no lock in tableName's scope would
// make a REINDEX CONCURRENTLY or DROP INDEX CONCURRENTLY wait or cancel an
// autovacuum, rereading pg_locks on conn every dbMaintenanceLockPollInterval
// for at most bound. Returns "" once the scope is clear, or the last reason
// when bound passes first. A lock taken between the last read and the caller's
// statement still makes that statement queue (an autovacuum that starts in that
// gap is cancelled as before); the read is one round trip before the statement,
// and an autovacuum never waits for a lock the statement already holds.
func dbMaintenanceAwaitTableUnlocked(ctx context.Context, conn PgConn, tableName string, bound time.Duration) (string, error) {
	readHeldLocks := func() ([]dbMaintenanceHeldLock, error) {
		rows, err := conn.Query(ctx, dbMaintenanceTableLocksQuery, tableName)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		heldLocks := []dbMaintenanceHeldLock{}
		for rows.Next() {
			var heldLock dbMaintenanceHeldLock
			if err := rows.Scan(&heldLock.pid, &heldLock.backendType, &heldLock.relation, &heldLock.mode); err != nil {
				return nil, err
			}
			heldLocks = append(heldLocks, heldLock)
		}
		return heldLocks, rows.Err()
	}

	deadline := time.Now().Add(bound)
	for {
		heldLocks, err := readHeldLocks()
		if err != nil {
			return "", err
		}
		reason := dbMaintenanceLockConflictReason(heldLocks)
		remaining := time.Until(deadline)
		if reason == "" || remaining <= 0 {
			return reason, nil
		}
		select {
		case <-ctx.Done():
			return "", context.Cause(ctx)
		case <-time.After(min(remaining, dbMaintenanceLockPollInterval)):
		}
	}
}

// A rebuild or drop that did not start because the lock it would take was held
// past its bound; see dbMaintenanceLockConflictReason.
type dbMaintenanceLockConflict struct {
	reason string
}

func (self *dbMaintenanceLockConflict) Error() string {
	return self.reason
}

// One reading of dbMaintenanceLoadQuery on a pooled maintenance session.
func dbMaintenanceSampleLoad(ctx context.Context) (load dbMaintenanceLoad, err error) {
	HandleError(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			var oldestSeconds float64
			Raise(conn.QueryRow(ctx, dbMaintenanceLoadQuery).Scan(
				&oldestSeconds,
				&load.otherIndexBuilds,
				&load.runningBackends,
			))
			load.oldestTransactionAge = time.Duration(oldestSeconds * float64(time.Second))
		}, OptNoRetry())
	}, func(recovered error) {
		err = recovered
	})
	return
}

// Heap, toast, and valid indexes of the table and every member of its partition
// tree (pg_partition_tree lists nothing for a table that is not partitioned; a
// partitioned member has no storage of its own). Invalid debris is excluded;
// cleanup drops it.
const dbMaintenanceTableBytesQuery = `
	WITH member AS (
		SELECT format('%I.%I', 'public', $1::text)::regclass::oid AS relid
		UNION
		SELECT tree.relid
		FROM pg_partition_tree(format('%I.%I', 'public', $1::text)::regclass) tree
	)
	SELECT coalesce(sum(
		pg_table_size(member.relid) + coalesce((
			SELECT sum(pg_relation_size(index_state.indexrelid))
			FROM pg_index index_state
			WHERE index_state.indrelid = member.relid
			  AND index_state.indisvalid
		), 0)
	), 0)::bigint
	FROM member
`

// The size the automatic rebuild bound compares; see dbMaintenanceTableBytesQuery.
func dbMaintenanceTableBytes(ctx context.Context, tableName string) (bytes int64, err error) {
	HandleError(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, dbMaintenanceTableBytesQuery, tableName).Scan(&bytes))
		}, OptNoRetry())
	}, func(recovered error) {
		err = recovered
	})
	return
}

// Whether another backend holds or awaits the table's ShareUpdateExclusiveLock:
// a vacuum or analyze (autovacuum included, running or queued for the lock) or
// another concurrent index build. A rebuild requests the same lock, so it would
// cancel a regular autovacuum after deadlock_timeout, throwing away hours of
// its work on these tables and pushing them toward an anti-wraparound vacuum,
// and it would wait behind an anti-wraparound one, which is never canceled.
const dbMaintenanceTableLockedQuery = `
	SELECT EXISTS (
		SELECT 1
		FROM pg_locks lock_state
		WHERE lock_state.locktype = 'relation'
		  AND lock_state.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND lock_state.relation = format('%I.%I', 'public', $1::text)::regclass
		  AND lock_state.mode = 'ShareUpdateExclusiveLock'
		  AND lock_state.pid <> pg_backend_pid()
	)
`

func dbMaintenanceTableLocked(ctx context.Context, tableName string) (locked bool, err error) {
	HandleError(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, dbMaintenanceTableLockedQuery, tableName).Scan(&locked))
		}, OptNoRetry())
	}, func(recovered error) {
		err = recovered
	})
	return
}

// Lists each invalid index with a transient suffix in the scope, with one row
// per valid index of the same relation that has an identical definition (or
// one row with a null original when there is none). $1 names the owner table
// (null: every public table); the scope covers its partition leaves and their
// toast tables. $2 restricts the list to one index for the recheck before a
// drop. An index is omitted while a CREATE INDEX or REINDEX is in progress on
// its table, because its suffixed siblings may be that operation's live
// copies.
const dbMaintenanceReindexDebrisQuery = `
	WITH owner_table AS (
		SELECT table_class.oid
		FROM pg_class table_class
		INNER JOIN pg_namespace table_namespace
			ON table_namespace.oid = table_class.relnamespace
		WHERE table_namespace.nspname = 'public'
		  AND table_class.relkind IN ('r', 'p')
		  AND ($1::text IS NULL OR table_class.relname = $1::text)
	), scoped_table AS (
		-- pg_partition_tree lists nothing for a table that is not partitioned
		SELECT owner_table.oid
		FROM owner_table
		UNION
		SELECT tree.relid
		FROM owner_table
		CROSS JOIN LATERAL pg_partition_tree(owner_table.oid) tree
	), scoped_relation AS (
		SELECT scoped_table.oid AS relid, scoped_table.oid AS table_oid
		FROM scoped_table
		UNION
		SELECT table_class.reltoastrelid, table_class.oid
		FROM scoped_table
		INNER JOIN pg_class table_class ON table_class.oid = scoped_table.oid
		WHERE table_class.reltoastrelid <> 0
	), active_table AS (
		SELECT coalesce(toast_owner.oid, progress.relid) AS table_oid, progress.index_relid
		FROM pg_stat_progress_create_index progress
		LEFT JOIN pg_class toast_owner ON toast_owner.reltoastrelid = progress.relid
		WHERE progress.datname = current_database()
	)
	SELECT candidate_class.oid::bigint,
	       candidate_namespace.nspname,
	       candidate_class.relname,
	       table_class.relname,
	       original_class.relname
	FROM scoped_relation
	INNER JOIN pg_class table_class ON table_class.oid = scoped_relation.table_oid
	INNER JOIN pg_index candidate ON candidate.indrelid = scoped_relation.relid
	INNER JOIN pg_class candidate_class ON candidate_class.oid = candidate.indexrelid
	INNER JOIN pg_namespace candidate_namespace ON candidate_namespace.oid = candidate_class.relnamespace
	LEFT JOIN pg_index original
		ON original.indrelid = candidate.indrelid
		AND original.indexrelid <> candidate.indexrelid
		AND original.indisvalid
		AND original.indisunique = candidate.indisunique
		AND original.indnullsnotdistinct = candidate.indnullsnotdistinct
		AND original.indisexclusion = candidate.indisexclusion
		AND original.indnkeyatts = candidate.indnkeyatts
		AND original.indkey::text = candidate.indkey::text
		AND original.indclass::text = candidate.indclass::text
		AND original.indcollation::text = candidate.indcollation::text
		AND original.indoption::text = candidate.indoption::text
		AND pg_get_expr(original.indexprs, original.indrelid) IS NOT DISTINCT FROM pg_get_expr(candidate.indexprs, candidate.indrelid)
		AND pg_get_expr(original.indpred, original.indrelid) IS NOT DISTINCT FROM pg_get_expr(candidate.indpred, candidate.indrelid)
		AND (SELECT relam FROM pg_class WHERE oid = original.indexrelid) = candidate_class.relam
	LEFT JOIN pg_class original_class ON original_class.oid = original.indexrelid
	WHERE NOT candidate.indisvalid
	  AND candidate_class.relname ~ '_cc(new|old)([1-9][0-9]*)?$'
	  AND ($2::bigint IS NULL OR candidate_class.oid = $2::bigint)
	  AND NOT EXISTS (
		SELECT 1
		FROM active_table
		WHERE active_table.table_oid = scoped_relation.table_oid
		   OR active_table.index_relid = candidate.indexrelid
	  )
	ORDER BY candidate_namespace.nspname, candidate_class.relname, original_class.relname
`

// An invalid index that is provably a REINDEX CONCURRENTLY copy of original.
type dbMaintenanceReindexDebris struct {
	indexOid  int64
	schema    string
	indexName string
	tableName string
	original  string
}

// Lists on conn the debris in one table's scope (every public table when
// tableName is empty; one index when indexOid is nonzero): each invalid index
// that is the transient copy of a valid index with the same definition on the
// same relation, and separately the suffixed invalid indexes that are not.
func listDbMaintenanceReindexDebris(
	ctx context.Context,
	conn PgConn,
	tableName string,
	indexOid int64,
) (debris []dbMaintenanceReindexDebris, kept []string, returnErr error) {
	var scope any
	if tableName != "" {
		scope = tableName
	}
	var only any
	if indexOid != 0 {
		only = indexOid
	}
	rows, err := conn.Query(ctx, dbMaintenanceReindexDebrisQuery, scope, only)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	candidates := []dbMaintenanceReindexDebris{}
	eligible := map[int64]bool{}
	for rows.Next() {
		var candidate dbMaintenanceReindexDebris
		var original *string
		if err := rows.Scan(&candidate.indexOid, &candidate.schema, &candidate.indexName, &candidate.tableName, &original); err != nil {
			return nil, nil, err
		}
		if original != nil && isReindexArtifactOf(candidate.indexName, *original) && !eligible[candidate.indexOid] {
			eligible[candidate.indexOid] = true
			candidate.original = *original
			debris = append(debris, candidate)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, candidate := range candidates {
		name := pgx.Identifier{candidate.schema, candidate.indexName}.Sanitize()
		if !eligible[candidate.indexOid] && !slices.Contains(kept, name) {
			kept = append(kept, name)
		}
	}
	return debris, kept, nil
}

// Drops the debris in one table's scope (every public table when tableName is
// empty) on a direct session of its own, so its lock timeout never returns to
// the pool. Only an index listed by listDbMaintenanceReindexDebris is dropped,
// and only if a recheck just before its drop still lists it; a valid index
// never qualifies. A drop does not queue behind an autovacuum, which the wait
// would cancel, or behind any lock that conflicts with the table's
// ShareUpdateExclusiveLock: it waits for the scope to clear for at most
// lockTimeout (see dbMaintenanceAwaitTableUnlocked). A drop that times out on
// that or on a lock leaves the rest of that table's debris for a later
// cleanup, since the same lock holder would block it; any other failed drop is
// counted and the next one proceeds. Returns how much listed debris remains.
func dbMaintenanceDropReindexDebris(ctx context.Context, tableName string, lockTimeout time.Duration) (remaining int, returnErr error) {
	conn, err := AcquireMaintenanceDbConn(ctx)
	if err != nil {
		return 0, err
	}
	defer discardPgConnection(ctx, conn)

	if _, err := conn.Exec(ctx, fmt.Sprintf("SET lock_timeout = %d", lockTimeout.Milliseconds())); err != nil {
		return 0, err
	}
	debris, kept, err := listDbMaintenanceReindexDebris(ctx, conn, tableName, 0)
	if err != nil {
		return 0, err
	}
	for _, name := range kept {
		glog.Infof("[db]maintenance kept invalid index %s: it is not the copy of a valid index\n", name)
	}

	lockedTables := map[string]bool{}
	for i, candidate := range debris {
		if lockedTables[candidate.tableName] {
			remaining += 1
			continue
		}
		recheck, _, err := listDbMaintenanceReindexDebris(ctx, conn, candidate.tableName, candidate.indexOid)
		if err != nil {
			return remaining + len(debris) - i, err
		}
		if len(recheck) != 1 {
			// it was dropped, renamed, became valid, or is now part of an active build
			continue
		}
		indexName := pgx.Identifier{candidate.schema, candidate.indexName}.Sanitize()
		glog.Infof(
			"[db]maintenance found incomplete index[%d/%d] %s on table %s (copy of %s)\n",
			i+1,
			len(debris),
			indexName,
			candidate.tableName,
			candidate.original,
		)
		reason, err := dbMaintenanceAwaitTableUnlocked(ctx, conn, candidate.tableName, lockTimeout)
		if err != nil {
			return remaining + len(debris) - i, err
		}
		if reason != "" {
			glog.Infof("[db]maintenance left %s for a later cleanup: %s\n", indexName, reason)
			remaining += 1
			lockedTables[candidate.tableName] = true
			continue
		}
		if _, err := conn.Exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+indexName); err != nil {
			glog.Infof("[db]maintenance drop %s failed: %s\n", indexName, err)
			remaining += 1
			if ctx.Err() != nil || conn.Conn().IsClosed() {
				return remaining + len(debris) - i - 1, err
			}
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.LockNotAvailable {
				lockedTables[candidate.tableName] = true
			}
		}
	}
	return remaining, nil
}

// Runs one REINDEX ... CONCURRENTLY statement on tableName (or one of its
// indexes) on a direct session of its own, so its bounded settings never
// return to the pool. Parallel maintenance workers are disabled so a rebuild
// takes one cpu, not up to max_parallel_maintenance_workers more. The rebuild
// does not queue for the table's ShareUpdateExclusiveLock behind an autovacuum,
// which the wait would cancel, or behind a conflicting lock: it waits for the
// scope to clear for at most lockTimeout and otherwise returns a
// *dbMaintenanceLockConflict without starting. lockTimeout still bounds each
// lock and snapshot wait of the later phases. pid names the server backend for
// the cleanup after a failure; it is zero when the statement never started.
func dbMaintenanceRunReindex(ctx context.Context, tableName string, statement string, lockTimeout time.Duration) (pid int32, err error) {
	conn, err := AcquireMaintenanceDbConn(ctx)
	if err != nil {
		return 0, err
	}
	defer discardPgConnection(ctx, conn)

	if _, err = conn.Exec(ctx, fmt.Sprintf("SET lock_timeout = %d", lockTimeout.Milliseconds())); err != nil {
		return
	}
	if _, err = conn.Exec(ctx, "SET max_parallel_maintenance_workers = 0"); err != nil {
		return
	}
	var backendPid int32
	if err = conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&backendPid); err != nil {
		return
	}
	// the last read before the statement, to keep the race window one round trip
	reason, err := dbMaintenanceAwaitTableUnlocked(ctx, conn, tableName, lockTimeout)
	if err != nil {
		return 0, err
	}
	if reason != "" {
		return 0, &dbMaintenanceLockConflict{reason: reason}
	}
	pid = backendPid
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, dbMaintenanceReindexTimeout)
	defer timeoutCancel()
	_, err = conn.Exec(timeoutCtx, statement)
	return
}

// Cleans up after a failed or canceled rebuild on a context detached from ctx,
// so a drain or deadline that ended the rebuild does not also end its cleanup.
// A canceled client returns before the server has aborted the statement (pgx
// sends the cancel request asynchronously), so this first waits for the
// rebuild's backend to leave index-build progress, canceling it again if it
// is still there, and only then drops the debris.
func dbMaintenanceCleanupAfterFailedReindex(ctx context.Context, tableName string, pid int32, lockTimeout time.Duration) (int, error) {
	cleanupCtx, cleanupCancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		dbMaintenanceFailedReindexCleanupTimeout,
	)
	defer cleanupCancel()

	if pid != 0 {
		var awaitErr error
		HandleError(func() {
			MaintenanceDb(cleanupCtx, func(conn PgConn) {
				for {
					var active bool
					Raise(conn.QueryRow(
						cleanupCtx,
						`SELECT EXISTS (SELECT 1 FROM pg_stat_progress_create_index WHERE pid = $1)`,
						pid,
					).Scan(&active))
					if !active {
						return
					}
					RaisePgResult(conn.Exec(
						cleanupCtx,
						`SELECT pg_cancel_backend(progress.pid) FROM pg_stat_progress_create_index progress WHERE progress.pid = $1`,
						pid,
					))
					select {
					case <-cleanupCtx.Done():
						panic(cleanupCtx.Err())
					case <-time.After(100 * time.Millisecond):
					}
				}
			}, OptNoRetry())
		}, func(recovered error) {
			awaitErr = recovered
		})
		if awaitErr != nil {
			return 0, awaitErr
		}
	}
	return dbMaintenanceDropReindexDebris(cleanupCtx, tableName, lockTimeout)
}

type dbMaintenanceObjectStep string

const (
	dbMaintenanceCleanupBefore dbMaintenanceObjectStep = "cleanup-before"
	dbMaintenanceReindex       dbMaintenanceObjectStep = "reindex"
	dbMaintenanceCleanupAfter  dbMaintenanceObjectStep = "cleanup-after"
)

// dbMaintenanceObjectSteps makes cleanup a guard around every rebuild. A
// timed-out REINDEX CONCURRENTLY can leave an invalid _ccnew/_ccold relation;
// cleaning only after the entire epoch lets a later timeout or task
// cancellation strand it, and the next epoch then creates a numbered sibling.
func dbMaintenanceObjectSteps(cleanup bool, reindex bool) []dbMaintenanceObjectStep {
	steps := []dbMaintenanceObjectStep{}
	if cleanup {
		steps = append(steps, dbMaintenanceCleanupBefore)
	}
	if reindex {
		steps = append(steps, dbMaintenanceReindex)
		if cleanup {
			steps = append(steps, dbMaintenanceCleanupAfter)
		}
	}
	return steps
}

// runDbMaintenanceObjectSteps refuses to start a rebuild when its prerequisite
// cleanup failed. A failed rebuild still reaches cleanup-after so its own
// incomplete artifact is removed before maintenance advances to another
// object.
func runDbMaintenanceObjectSteps(steps []dbMaintenanceObjectStep, run func(dbMaintenanceObjectStep) bool) {
	for _, step := range steps {
		if ok := run(step); !ok && step == dbMaintenanceCleanupBefore {
			return
		}
	}
}

func DbMaintenanceWithDefaults(ctx context.Context, epoch uint64) *DbMaintenanceOutcome {
	return DbMaintenance(ctx, epoch, DefaultDbMaintenanceOptions())
}

// Runs one epoch of the rotation (see the file comment), after first rebuilding
// up to MaxPriorityRebuilds bloated priority indexes when PriorityReindex is
// set. A context that is already done panics at the first query, as before, so
// a canceled task is not acknowledged; once the run has started it returns an
// outcome instead, whose Stopped field says whether the epoch finished.
func DbMaintenance(ctx context.Context, epoch uint64, opts *DbMaintenanceOptions) *DbMaintenanceOutcome {

	// regularly reindex tables to avoid bloat:
	// 1. tables are reindexed over `DbReindexEpochs` epochs
	//    e.g. `DbReindexEpochs=4` means all tables will be reindexed over 4 maintenance epochs
	// 2. ANALYZE is called after each maintenance to update the planner stats

	// note `REINDEX CONCURRENTLY` can be safely run in the background
	// see https://www.postgresql.org/docs/current/sql-reindex.html

	outcome := &DbMaintenanceOutcome{
		DoneTables:  slices.Clone(opts.DoneTables),
		DoneIndexes: slices.Clone(opts.DoneIndexes),
	}

	tableNames := []string{}
	reindexTableNames := []string{}

	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				table_name
			FROM information_schema.tables
			WHERE
				table_schema = 'public' AND
				table_type = 'BASE TABLE'
			`,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var tableName string
				Raise(result.Scan(&tableName))
				tableNames = append(tableNames, tableName)
			}
		})

		for _, tableName := range tableNames {
			hash := fnv.New64()
			hash.Write([]byte(tableName))
			b := make([]byte, 8)
			// cycle the hash each generation
			binary.BigEndian.PutUint64(b, epoch/DbReindexEpochs)
			hash.Write(b)
			h := hash.Sum64()
			if h%DbReindexEpochs == epoch%DbReindexEpochs {
				reindexTableNames = append(reindexTableNames, tableName)
			}
		}
	})

	slices.Sort(reindexTableNames)

	// Every run measures the priority indexes and logs their sizes. With
	// PriorityReindex it rebuilds those bloated now, whatever the epoch; an
	// unmeasurable index is left alone. Without it the run logs which are
	// bloated and rebuilds none, so none is recorded done.
	priorityIndexes := []dbMaintenancePriorityIndex{}
	if opts.Reindex {
		measuredIndexes, err := dbMaintenancePriorityIndexSizes(ctx, priorityReindexIndexes)
		if err != nil {
			glog.Infof("[db]maintenance priority index sizes unavailable: %s\n", err)
		}
		for _, measuredIndex := range measuredIndexes {
			glog.Infof(
				"[db]maintenance priority index %s on %s is %d bytes (compact estimate %d bytes, heap %d bytes)\n",
				measuredIndex.indexName,
				measuredIndex.tableName,
				measuredIndex.indexBytes,
				measuredIndex.compactBytes,
				measuredIndex.heapBytes,
			)
		}
		if opts.PriorityReindex {
			priorityIndexes = dbMaintenanceSelectPriorityIndexes(measuredIndexes, opts.DoneIndexes, opts.maxPriorityRebuilds())
		} else {
			bloatedIndexNames := []string{}
			for _, measuredIndex := range measuredIndexes {
				if measuredIndex.bloated() {
					bloatedIndexNames = append(bloatedIndexNames, measuredIndex.indexName)
				}
			}
			glog.Infof(
				"[db]maintenance priority index rebuilds are off (priority_reindex_enabled in %s); bloated, not rebuilt: %s\n",
				MaintenancePgConfigResourceName,
				strings.Join(bloatedIndexNames, ", "),
			)
		}
	}
	priorityIndexNames := []string{}
	for _, priorityIndex := range priorityIndexes {
		priorityIndexNames = append(priorityIndexNames, priorityIndex.indexName)
	}
	glog.Infof(
		"[db]maintenance %d/%d tables (in random order): %s; priority indexes: %s\n",
		len(reindexTableNames),
		len(tableNames),
		strings.Join(reindexTableNames, ", "),
		strings.Join(priorityIndexNames, ", "),
	)
	reindexTableNames = slices.DeleteFunc(reindexTableNames, func(tableName string) bool {
		return slices.Contains(opts.DoneTables, tableName)
	})
	if 0 < len(opts.DoneTables) || 0 < len(opts.DoneIndexes) {
		glog.Infof(
			"[db]maintenance continuing epoch %d: %d table(s) and %d priority index(es) already done\n",
			epoch,
			len(opts.DoneTables),
			len(opts.DoneIndexes),
		)
	}

	mathrand.Shuffle(len(reindexTableNames), func(i int, j int) {
		reindexTableNames[i], reindexTableNames[j] = reindexTableNames[j], reindexTableNames[i]
	})

	stop := func(stopped DbMaintenanceStop, reason string) *DbMaintenanceOutcome {
		outcome.Stopped = stopped
		outcome.Reason = reason
		glog.Infof(
			"[db]maintenance epoch %d %s: %s (%d table(s) and %d priority index(es) done)\n",
			epoch,
			stopped,
			reason,
			len(outcome.DoneTables),
			len(outcome.DoneIndexes),
		)
		return outcome
	}
	interrupted := func() *DbMaintenanceOutcome {
		return stop(DbMaintenanceInterrupted, context.Cause(ctx).Error())
	}

	cleanup := func(tableName string) bool {
		remaining, err := dbMaintenanceDropReindexDebris(ctx, tableName, opts.cleanupLockTimeout())
		if err != nil {
			glog.Infof("[db]maintenance cleanup %s failed: %s\n", tableName, err)
		}
		return err == nil && remaining == 0
	}

	// Leftovers of any earlier interruption, on any table, are dropped first.
	// Cleanup is not load-guarded: a drop removes write amplification.
	if opts.Cleanup {
		startTime := time.Now()
		cleanup("")
		glog.Infof(
			"[db]maintenance sweep took %.2fs\n",
			float64(time.Since(startTime)/time.Millisecond)/1000.0,
		)
		if ctx.Err() != nil {
			return interrupted()
		}
	}

	// Set by runObject when the object's rebuild did not start because its
	// table stayed locked (see dbMaintenanceRunReindex); the object is left for
	// the continuation and the run goes on with the next one.
	objectLocked := false
	// The objects left that way, in order.
	lockedObjectNames := []string{}

	// Runs one object's ordered steps. The guard decides before the object's
	// steps start, so a deferral leaves the object for the continuation. It
	// returns false when the run must stop before this object finished.
	// maxTransactionAge bounds the transactions the rebuild starts behind, and
	// lockTimeout bounds both its wait for cleanupTableName to clear and each
	// lock and snapshot wait of the rebuild itself.
	runObject := func(
		prefix string,
		objectName string,
		cleanupTableName string,
		reindexStatement string,
		maxTransactionAge time.Duration,
		lockTimeout time.Duration,
	) bool {
		objectLocked = false
		if ctx.Err() != nil {
			interrupted()
			return false
		}
		rebuild := reindexStatement != ""
		if rebuild {
			load, err := dbMaintenanceSampleLoad(ctx)
			if err != nil {
				if ctx.Err() != nil {
					interrupted()
				} else {
					stop(DbMaintenanceDeferred, fmt.Sprintf("load sample failed: %s", err))
				}
				return false
			}
			if reason := dbMaintenanceDeferReason(load, maxTransactionAge, opts.maxRunningBackends()); reason != "" {
				stop(DbMaintenanceDeferred, reason)
				return false
			}
		}

		var reindexPid int32
		reindexFailed := false
		steps := dbMaintenanceObjectSteps(opts.Cleanup, rebuild)
		runDbMaintenanceObjectSteps(steps, func(step dbMaintenanceObjectStep) bool {
			if step != dbMaintenanceCleanupAfter && ctx.Err() != nil {
				return false
			}
			glog.Infof("[db]maintenance %s %s %s\n", prefix, step, objectName)
			startTime := time.Now()
			ok := true
			switch step {
			case dbMaintenanceCleanupBefore:
				ok = cleanup(cleanupTableName)
				if !ok && rebuild {
					glog.Infof("[db]maintenance %s skip reindex %s: incomplete indexes remain\n", prefix, objectName)
				}
			case dbMaintenanceReindex:
				var err error
				reindexPid, err = dbMaintenanceRunReindex(ctx, cleanupTableName, reindexStatement, lockTimeout)
				var lockConflict *dbMaintenanceLockConflict
				if errors.As(err, &lockConflict) {
					// nothing started, so nothing to clean up after
					glog.Infof("[db]maintenance %s %s %s deferred: %s\n", prefix, step, objectName, lockConflict.reason)
					objectLocked = true
				} else if err != nil {
					glog.Infof("[db]maintenance %s %s %s failed: %s\n", prefix, step, objectName, err)
					reindexFailed = true
					ok = false
					if !opts.Cleanup {
						// a run without cleanup steps still removes its own rebuild's copies
						dbMaintenanceCleanupAfterFailedReindex(ctx, cleanupTableName, reindexPid, opts.cleanupLockTimeout())
					}
				}
			case dbMaintenanceCleanupAfter:
				if reindexFailed {
					remaining, err := dbMaintenanceCleanupAfterFailedReindex(ctx, cleanupTableName, reindexPid, opts.cleanupLockTimeout())
					if err != nil || 0 < remaining {
						glog.Infof(
							"[db]maintenance %s %s %s left %d incomplete index(es) for a later cleanup: %v\n",
							prefix,
							step,
							objectName,
							remaining,
							err,
						)
					}
					ok = err == nil && remaining == 0
				} else if ctx.Err() == nil {
					ok = cleanup(cleanupTableName)
				}
			}
			glog.Infof(
				"[db]maintenance %s %s %s took %.2fs\n",
				prefix,
				step,
				objectName,
				float64(time.Since(startTime)/time.Millisecond)/1000.0,
			)
			return ok
		})
		if ctx.Err() != nil {
			// the continuation repeats this object
			interrupted()
			return false
		}
		return true
	}

	// The bloated priority indexes go first, smallest first: a long table
	// rotation that ends in a deferral must not starve them. One whose table is
	// being vacuumed is skipped at once, without deferring the run, rather than
	// cancel, queue behind or wait out a vacuum that can run for hours; every run
	// measures the priority indexes again. A lock this check misses that the
	// rebuild must not queue behind (one taken after the check, a conflicting
	// lock in another mode, or an autovacuum's lock on the toast table or an
	// index) makes the rebuild wait for the table to clear, without queuing, for
	// at most the priority lock bound (see dbMaintenanceRunReindex). An index
	// still locked then is left out of the done list and defers the run, as a
	// locked table does, so the continuation retries it. Each keeps its debris
	// cleanup adjacent to its own rebuild on the table that owns it, and its size
	// afterwards is logged against its estimate, so an estimate that would keep
	// an index selected shows up in the log.
	for i, priorityIndex := range priorityIndexes {
		prefix := fmt.Sprintf("priority index[%d/%d]", i+1, len(priorityIndexes))
		if locked, err := dbMaintenanceTableLocked(ctx, priorityIndex.tableName); err != nil || locked {
			if ctx.Err() != nil {
				interrupted()
				break
			}
			glog.Infof(
				"[db]maintenance %s skip %s: its table %s is being vacuumed, analyzed or indexed (err %v)\n",
				prefix,
				priorityIndex.indexName,
				priorityIndex.tableName,
				err,
			)
			continue
		}
		reindexStatement := `REINDEX INDEX CONCURRENTLY ` + pgx.Identifier{"public", priorityIndex.indexName}.Sanitize()
		if !runObject(
			prefix,
			priorityIndex.indexName,
			priorityIndex.tableName,
			reindexStatement,
			opts.priorityMaxTransactionAge(),
			opts.priorityReindexLockTimeout(),
		) {
			break
		}
		if objectLocked {
			lockedObjectNames = append(lockedObjectNames, priorityIndex.indexName)
			continue
		}
		outcome.DoneIndexes = append(outcome.DoneIndexes, priorityIndex.indexName)
		if rebuiltIndexes, err := dbMaintenancePriorityIndexSizes(ctx, []string{priorityIndex.indexName}); err == nil {
			for _, rebuiltIndex := range rebuiltIndexes {
				glog.Infof(
					"[db]maintenance %s %s is %d bytes, was %d (compact estimate %d bytes, still bloated %t)\n",
					prefix,
					rebuiltIndex.indexName,
					rebuiltIndex.indexBytes,
					priorityIndex.indexBytes,
					rebuiltIndex.compactBytes,
					rebuiltIndex.bloated(),
				)
			}
		}
	}

	// Cleanup and reindex each selected table as one ordered unit. In
	// particular, a failed rebuild gets its cleanup attempt before maintenance
	// advances to the next table, and a failed prerequisite cleanup prevents a
	// new numbered _ccnew sibling from being created.
	for i, tableName := range reindexTableNames {
		if outcome.Stopped != "" {
			break
		}
		reindexStatement := ""
		if opts.Reindex && dbMaintenanceShouldReindexTable(tableName) {
			reindexStatement = `REINDEX TABLE CONCURRENTLY ` + pgx.Identifier{"public", tableName}.Sanitize()
			if !opts.IncludeLargeTables {
				bytes, err := dbMaintenanceTableBytes(ctx, tableName)
				if err != nil {
					if ctx.Err() != nil {
						return interrupted()
					}
					glog.Infof("[db]maintenance table %s size unavailable, cleanup only: %s\n", tableName, err)
					reindexStatement = ""
				} else if maxBytes := opts.maxReindexTableBytes(); maxBytes < bytes {
					glog.Infof(
						"[db]maintenance table %s is %d bytes, above the %d byte automatic rebuild bound: cleanup only\n",
						tableName,
						bytes,
						maxBytes,
					)
					reindexStatement = ""
				}
			}
		}
		prefix := fmt.Sprintf("table[%d/%d]", i+1, len(reindexTableNames))
		if !runObject(
			prefix,
			tableName,
			tableName,
			reindexStatement,
			opts.maxTransactionAge(),
			opts.reindexLockTimeout(),
		) {
			break
		}
		if objectLocked {
			lockedObjectNames = append(lockedObjectNames, tableName)
			continue
		}
		outcome.DoneTables = append(outcome.DoneTables, tableName)
	}

	if outcome.Stopped == DbMaintenanceInterrupted {
		return outcome
	}
	if outcome.Stopped == "" && 0 < len(lockedObjectNames) {
		stop(DbMaintenanceDeferred, fmt.Sprintf("locked by another session: %s", strings.Join(lockedObjectNames, ", ")))
	}

	// A deferred run still analyzes, so the daily pending_task vacuum keeps
	// its cadence while rebuilds wait for a quieter primary.
	if opts.Analyze {
		// The task queue is tiny but extremely update-heavy. An explicit daily
		// vacuum bounds its poll-index debt even when normal autovacuum timing is
		// unlucky; the database idle-transaction timeout ensures xmin cannot pin
		// these versions indefinitely.
		HandleError(func() {
			MaintenanceDb(ctx, func(conn PgConn) {
				glog.Infof("[db]maintenance vacuum analyze pending_task\n")
				RaisePgResult(conn.Exec(ctx, dbMaintenanceVacuumPendingTaskStatement))
			}, OptNoRetry())
		})

		HandleError(func() {
			MaintenanceDb(ctx, func(conn PgConn) {
				glog.Infof("[db]maintenance final analyze\n")
				// final analyze
				startTime := time.Now()
				RaisePgResult(conn.Exec(
					ctx,
					dbMaintenanceAnalyzeStatement,
				))
				endTime := time.Now()
				glog.Infof(
					"[db]maintenance final analyze took %.2fs\n",
					float64(endTime.Sub(startTime)/time.Millisecond)/1000.0,
				)
			}, OptNoRetry())
		})
	}
	return outcome
}

// VacuumFullAllTables runs VACUUM (FULL, ANALYZE) on every ordinary public
// table (including partition leaves) except those matched by excludes. An
// exclude matches a table by exact name OR as an underscore-namespaced parent,
// so `client_reliability` also skips client_reliability_new, its partitions,
// and client_reliability_copy_progress.
//
// VACUUM FULL takes an ACCESS EXCLUSIVE lock and rewrites the table + all its
// indexes into fresh compact files, returning reclaimed bloat to the OS (a
// plain VACUUM does not). It therefore (a) must be run with the system offline
// — it blocks all access to each table for the duration — and (b) needs
// transient free disk of roughly the table's current size while the rewrite is
// in flight. Tables are processed one at a time, smallest first, so peak
// transient use is bounded by the largest single table rather than their sum.
//
// Runs on the maintenance connection in autocommit (VACUUM cannot run inside a
// transaction). A table that cannot be locked within lock_timeout, or that
// errors mid-rewrite (e.g. out of disk — VACUUM FULL rolls back cleanly,
// leaving the table intact), is logged and skipped; the run continues.
func VacuumFullAllTables(ctx context.Context, excludes []string, logf func(string, ...any)) {
	skip := func(t string) bool {
		for _, e := range excludes {
			if t == e || strings.HasPrefix(t, e+"_") {
				return true
			}
		}
		return false
	}

	type tableInfo struct {
		name   string
		size   int64
		pretty string
	}

	all := []tableInfo{}
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT c.relname, pg_total_relation_size(c.oid), pg_size_pretty(pg_total_relation_size(c.oid))
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relkind = 'r'
			ORDER BY pg_total_relation_size(c.oid) ASC
			`,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var ti tableInfo
				Raise(result.Scan(&ti.name, &ti.size, &ti.pretty))
				all = append(all, ti)
			}
		})
	}, OptReadWrite())

	todo := []tableInfo{}
	for _, ti := range all {
		if skip(ti.name) {
			logf("skip %s (%s) — excluded", ti.name, ti.pretty)
		} else {
			todo = append(todo, ti)
		}
	}
	logf("VACUUM FULL plan: %d tables to vacuum, %d skipped", len(todo), len(all)-len(todo))

	MaintenanceDb(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, `SET statement_timeout = 0`))
		// so a table held by another session (e.g. the client_reliability copy)
		// is skipped rather than blocking the whole run
		RaisePgResult(conn.Exec(ctx, `SET lock_timeout = '60s'`))

		for i, ti := range todo {
			startTime := time.Now()
			logf("[%d/%d] VACUUM FULL %s (%s) ...", i+1, len(todo), ti.name, ti.pretty)

			// table name comes from the catalog (not user input); interpolate
			// like the REINDEX path above. VACUUM cannot take bind parameters.
			_, err := conn.Exec(ctx, `VACUUM (FULL, ANALYZE) `+ti.name)
			if err != nil {
				logf("[%d/%d] %s SKIPPED/ERROR after %.1fs: %v", i+1, len(todo), ti.name, time.Since(startTime).Seconds(), err)
				continue
			}

			after := ti.pretty
			r2, e2 := conn.Query(ctx, `SELECT pg_size_pretty(pg_total_relation_size(to_regclass($1)))`, "public."+ti.name)
			WithPgResult(r2, e2, func() {
				if r2.Next() {
					Raise(r2.Scan(&after))
				}
			})
			logf("[%d/%d] %s done: %s -> %s in %.1fs", i+1, len(todo), ti.name, ti.pretty, after, time.Since(startTime).Seconds())
		}
	}, OptReadWrite(), OptNoRetry())

	logf("VACUUM FULL complete")
}
