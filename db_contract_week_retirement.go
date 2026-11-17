// Retirement of the contract weeks at the contract horizon (S1,
// CONTRACT-PARTITION.md "S1: weeks retire at the reaper's horizon").
//
// The contract horizon reaper (model/contract_horizon_reaper.go) is the only
// code that decides which contract may go. It deletes a contract with its
// closes, escrows, sweeps, extender parties and close reports in one
// transaction, under its keep rules, and the usage archive trigger copies a
// credit-bearing contract's proof in the same statement. Retirement deletes no
// row and judges none: it returns the storage of the rows the reaper has
// deleted, and keeps every row the reaper keeps, by construction.
//
//   - A week expires RetireDelay after its upper bound passes the reaper's
//     horizon (ContractWeekRetention: 35 days at the shortest) while the
//     reaper's mode is on; otherwise at ContractRetention (91 days). With the
//     mode off the reaper deletes nothing, so an expired week stays.
//   - An expired week whose six leaves hold no tuple at all, live or dead, in a
//     heap or its TOAST, is detached and dropped in one transaction. Nothing a
//     snapshot could see goes with it.
//   - An expired week that still holds rows has each leaf rewritten by CLUSTER
//     once the reaper has emptied it down to the rows it keeps. CLUSTER copies every live and recently dead row version with its
//     transaction ids (it is MVCC-safe, unlike a copy or TRUNCATE), so every
//     snapshot sees the same rows after it, and the old files go back to the
//     filesystem. A leaf goes in its own transaction, which locks that leaf
//     alone; its parents and every other leaf stay untouched.
//   - A compacted week stays while the reaper keeps any of its rows. Once the
//     reaper's rescans have deleted them, the next run empties its leaves with
//     CLUSTER and drops them.
//
// Locks follow the cutover's admission (db_contract_partition_admission.go):
// no step queues behind an autovacuum, a prepared transaction or a transaction
// older than LockSettings.MaxTransactionAge holding or awaiting a lock it
// conflicts with; each ACCESS EXCLUSIVE wait is at most LockTimeout (1 s), and
// the work done under the lock is cut off by its own statement timeout. The
// work under a lock is bounded by the rows the reaper keeps (MaxCompactRows),
// or is catalog work only.
package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/glog"
)

// The shortest contract horizon the reaper applies (model.ContractHorizonFloor,
// owner 2026-11-01): a week never expires sooner than this past its upper
// bound, whatever ctx carries.
const ContractWeekRetentionFloor = 35 * 24 * time.Hour

// Tunables of the retirement of expired contract weeks.
type ContractWeekRetirementSettings struct {
	// How long past the horizon a week waits before it is examined: the reaper
	// reads a contract only its close settle time (2 hours) past the horizon.
	RetireDelay time.Duration
	// Admission before each lock request (MaxTransactionAge must be positive).
	LockSettings ContractPartitionLockSettings
	// Bounds each ACCESS EXCLUSIVE wait, and the lock statement as a whole.
	// Application statements on the leaf (or, for the drop, on the parents)
	// queue behind the request and give up after PgLockTimeout (2 s), so this
	// and the work under the lock stay below it.
	LockTimeout time.Duration
	// A leaf larger than CompactAboveBytes is counted only while the
	// statistics count at most this many live rows in it (n_live_tup, which
	// follows the reaper's deletes as they commit): a leaf the reaper has not
	// emptied, or one that holds more kept rows than a compaction moves (the
	// legacy partition), is left without a read.
	ProbeGateRows int64
	// The most live rows a leaf may hold to be compacted. CLUSTER's work, and
	// so its hold on the leaf's lock, grows with them. A week the reaper keeps
	// more rows of stays, reported.
	MaxCompactRows int64
	// Bounds the count of a leaf's live rows.
	ProbeTimeout time.Duration
	// Bounds CLUSTER's work once it holds its lock. A leaf that takes longer is
	// left as it was for a later run.
	CompactTimeout time.Duration
	// A leaf that still holds live rows is compacted only while its heap and
	// TOAST are larger than this, so a compacted leaf is not rewritten again.
	CompactAboveBytes int64
	// Bounds the drop's catalog work once it holds its locks.
	DropTimeout time.Duration
}

func DefaultContractWeekRetirementSettings() *ContractWeekRetirementSettings {
	return &ContractWeekRetirementSettings{
		RetireDelay:   24 * time.Hour,
		LockSettings:  *DefaultContractPartitionLockSettings(),
		LockTimeout:   time.Second,
		ProbeGateRows: 1000000,
		// CLUSTER of 20,000 to 40,000 rows took 55-174 ms on PostgreSQL 18.7
		// (14 indexes on transfer_contract's leaf, a tmpfs volume); half that
		// leaves Main's busier disk margin under CompactTimeout
		MaxCompactRows:    10000,
		ProbeTimeout:      5 * time.Second,
		CompactTimeout:    500 * time.Millisecond,
		CompactAboveBytes: 256 * 1024 * 1024,
		DropTimeout:       500 * time.Millisecond,
	}
}

// How long past its upper bound a contract week is kept: the contract horizon
// ctx carries while the reaper's mode is on (WithContractHorizon), between
// ContractWeekRetentionFloor and ContractRetention; without one,
// ContractRetention.
func ContractWeekRetention(ctx context.Context) time.Duration {
	horizon, ok := contractHorizonIn(ctx)
	if !ok {
		return ContractRetention
	}
	return min(max(horizon, ContractWeekRetentionFloor), ContractRetention)
}

// How long past its upper bound a week expires: its retention and the
// settings' delay, never later than ContractRetention, the family's declared
// retention.
func contractWeekExpiry(ctx context.Context, settings *ContractWeekRetirementSettings) time.Duration {
	return min(ContractWeekRetention(ctx)+settings.RetireDelay, ContractRetention)
}

// The family's RangeExpired: whether the week r (or the legacy partition, up to
// the cutover week) has expired at now.
func contractWeekRangeExpired(ctx context.Context, now time.Time, r PartitionRange) bool {
	return contractWeekExpired(ctx, now, r, DefaultContractWeekRetirementSettings())
}

func contractWeekExpired(ctx context.Context, now time.Time, r PartitionRange, settings *ContractWeekRetirementSettings) bool {
	if r.Default || r.Upper.IsZero() {
		return false
	}
	return !now.Before(r.Upper.Add(contractWeekExpiry(ctx, settings)))
}

// The expired ranges of the contract tables, oldest first: the canonical
// leaves of transfer_contract whose week has expired at now.
func contractExpiredWeeks(ctx context.Context, now time.Time, settings *ContractWeekRetirementSettings) (expired []PartitionRange, err error) {
	var leaves []PartitionLeaf
	MaintenanceDb(ctx, func(conn PgConn) {
		leaves, err = contractPartitionFamily.ListLeaves(ctx, conn, ContractPartitionTables[0])
	})
	if err != nil {
		return nil, err
	}
	for _, leaf := range leaves {
		if leaf.Canonical && contractWeekExpired(ctx, now, leaf.Range, settings) {
			expired = append(expired, leaf.Range)
		}
	}
	slices.SortStableFunc(expired, func(a PartitionRange, b PartitionRange) int {
		return a.Upper.Compare(b.Upper)
	})
	return
}

// One week's leaf of one contract table, as the catalog describes it.
type contractWeekLeaf struct {
	table  string
	name   string
	exists bool
	// the leaf's TOAST table, schema-qualified; empty without one
	toast      string
	heapBytes  int64
	toastBytes int64
	// the live rows the statistics count; -1 without statistics
	liveRows   int64
	primaryKey string
}

// Bytes of tuples, live or dead, in the heap and its TOAST. Zero means the leaf
// holds no tuple at all: nothing any snapshot could read.
func (self contractWeekLeaf) bytes() int64 {
	return self.heapBytes + self.toastBytes
}

// The six leaves of a week ($1, leaf names in table order), each with its
// storage, live row count and primary key.
const contractWeekLeavesSql = `
	SELECT
		leaf.name,
		relation.oid IS NOT NULL,
		coalesce(toast.oid::regclass::text, ''),
		coalesce(pg_relation_size(relation.oid), 0),
		coalesce(pg_relation_size(toast.oid), 0),
		coalesce(activity.n_live_tup, -1),
		coalesce((
			SELECT index_class.relname::text
			FROM pg_index AS index
			JOIN pg_class AS index_class ON index_class.oid = index.indexrelid
			WHERE index.indrelid = relation.oid AND index.indisprimary
		), '')
	FROM unnest($1::text[]) WITH ORDINALITY AS leaf (name, position)
	LEFT JOIN pg_class AS relation ON relation.oid = to_regclass('public.' || quote_ident(leaf.name))
	LEFT JOIN pg_class AS toast ON toast.oid = relation.reltoastrelid AND relation.reltoastrelid <> 0
	LEFT JOIN pg_stat_user_tables AS activity ON activity.relid = relation.oid
	ORDER BY leaf.position
`

func readContractWeekLeavesInConn(ctx context.Context, conn PgQuerier, r PartitionRange) (leaves []contractWeekLeaf) {
	names := contractPartitionFamily.LeafNames(r)
	result, err := conn.Query(ctx, contractWeekLeavesSql, names)
	WithPgResult(result, err, func() {
		for i := 0; result.Next(); i += 1 {
			leaf := contractWeekLeaf{table: ContractPartitionTables[i]}
			Raise(result.Scan(
				&leaf.name,
				&leaf.exists,
				&leaf.toast,
				&leaf.heapBytes,
				&leaf.toastBytes,
				&leaf.liveRows,
				&leaf.primaryKey,
			))
			leaves = append(leaves, leaf)
		}
	})
	return
}

func readContractWeekLeaves(ctx context.Context, r PartitionRange) (leaves []contractWeekLeaf, err error) {
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			leaves = readContractWeekLeavesInConn(ctx, conn, r)
		})
	})
	return
}

// The family's DropPrecondition: the week stays while any of its leaves holds
// a tuple, live or dead. Only the reaper deletes contract rows; a drop takes
// none with it.
func contractWeekNotEmpty(ctx context.Context, conn PgQuerier, r PartitionRange) string {
	for _, leaf := range readContractWeekLeavesInConn(ctx, conn, r) {
		if !leaf.exists {
			continue
		}
		if 0 < leaf.bytes() {
			return fmt.Sprintf("%s holds %d bytes of rows", leaf.name, leaf.bytes())
		}
	}
	return ""
}

func contractStatementTimeoutSql(timeout time.Duration) string {
	return fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", max(int64(1), timeout.Milliseconds()))
}

// Counts a leaf's live rows, up to limit, along its key: the walk CLUSTER
// makes, which also brings the kept rows' pages into the cache for it. Like any
// index scan it marks the key entries of rows every snapshot sees deleted as
// dead, so where vacuum has not removed the reaper's last deletes yet (Main's
// leaves vacuum only every 5M to 25M dead rows), each run's walk gets further
// until it finishes within ProbeTimeout, and CLUSTER then skips those entries.
// A read only transaction; its lock (ACCESS SHARE) waits only behind an
// exclusive one.
func contractWeekLeafLiveRows(ctx context.Context, leaf contractWeekLeaf, limit int64, settings *ContractWeekRetirementSettings) (rows int64, err error) {
	err = partitionRegistryRun(func() {
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, contractLockTimeoutSql(settings.LockTimeout)))
			RaisePgResult(tx.Exec(ctx, contractStatementTimeoutSql(settings.ProbeTimeout)))
			// the key's order, never a scan of the whole heap
			RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
			RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_bitmapscan = off`))
			RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_indexonlyscan = off`))
			Raise(tx.QueryRow(
				ctx,
				fmt.Sprintf(`SELECT count(*) FROM (SELECT xmin FROM ONLY public.%s ORDER BY contract_id LIMIT $1) AS live`, quoteIdentifier(leaf.name)),
				limit,
			).Scan(&rows))
		}, TxReadCommitted, OptNoRetry(), pgx.ReadOnly)
	})
	return
}

// The planner settings of a compaction: with every page and CPU cost zero,
// CLUSTER's choice between an index scan and a sequential scan with a sort
// (plan_cluster_use_sort) compares two zero costs and takes the index scan.
// The scan then visits only the entries vacuum left in the key, the kept rows,
// however large the leaf's files and however its rows correlate with them.
// PostgreSQL 18 counts disabled plans apart from their costs, so enable_*
// settings do not reach that choice.
var contractCompactPlannerSql = []string{
	`SET LOCAL seq_page_cost = 0`,
	`SET LOCAL random_page_cost = 0`,
	`SET LOCAL cpu_tuple_cost = 0`,
	`SET LOCAL cpu_index_tuple_cost = 0`,
	`SET LOCAL cpu_operator_cost = 0`,
	`SET LOCAL enable_indexscan = on`,
}

// The statements of a leaf's compaction, after its admission: the leaf's lock,
// waited for at most LockTimeout; then CLUSTER along the primary key, which
// rewrites the heap, its TOAST and every index with only the live and recently
// dead row versions, under CompactTimeout; then the index's clustered mark
// cleared, so the leaf's catalog matches the others'.
func contractCompactLeafSql(leaf contractWeekLeaf, settings *ContractWeekRetirementSettings) (statements []string) {
	statements = append(
		statements,
		contractLockTimeoutSql(settings.LockTimeout),
		contractStatementTimeoutSql(settings.LockTimeout),
		fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(leaf.name)),
		contractStatementTimeoutSql(settings.CompactTimeout),
	)
	statements = append(statements, contractCompactPlannerSql...)
	statements = append(
		statements,
		fmt.Sprintf("CLUSTER public.%s USING %s", quoteIdentifier(leaf.name), quoteIdentifier(leaf.primaryKey)),
		fmt.Sprintf("ALTER TABLE public.%s SET WITHOUT CLUSTER", quoteIdentifier(leaf.name)),
	)
	return
}

// The admission of a leaf's compaction: the leaf and its TOAST, which CLUSTER
// locks too and autovacuum vacuums apart from the leaf.
func contractCompactScope(leaf contractWeekLeaf, settings *ContractWeekRetirementSettings) *contractLockScope {
	return &contractLockScope{
		relations: []string{"public." + leaf.name},
		toast:     true,
		request:   contractLockAccessExclusive,
		settings:  settings.LockSettings,
	}
}

// Rewrites a leaf with only the row versions it still holds. Returns
// ContractPartitionBusyError without locking anything when its admission finds
// a vacuum or a long transaction there.
func compactContractWeekLeaf(ctx context.Context, leaf contractWeekLeaf, settings *ContractWeekRetirementSettings) error {
	if leaf.primaryKey == "" {
		return fmt.Errorf("%s has no primary key", leaf.name)
	}
	blockers, err := contractLockAdmission(ctx, contractCompactScope(leaf, settings))
	if err != nil {
		return err
	}
	if 0 < len(blockers) {
		return &ContractPartitionBusyError{Blockers: blockers}
	}
	return contractPartitionExec(ctx, contractCompactLeafSql(leaf, settings))
}

// The admission of a week's drop: the six parents alone, whose DETACH takes
// ACCESS EXCLUSIVE on each, and the week's leaves and their TOAST, which the
// DROP locks.
func contractDropWeekScope(leaves []contractWeekLeaf, settings *ContractWeekRetirementSettings) *contractLockScope {
	scope := &contractLockScope{toast: true, request: contractLockAccessExclusive, settings: settings.LockSettings}
	for _, table := range ContractPartitionTables {
		scope.only = append(scope.only, "public."+table)
	}
	for _, leaf := range leaves {
		scope.relations = append(scope.relations, "public."+leaf.name)
	}
	return scope
}

// The drop refused because a leaf holds a tuple once its locks were held.
var errContractWeekNotEmpty = errors.New("the week holds rows")

// Detaches and drops a week's six physically empty leaves in one transaction,
// children first. The locks are taken in one statement, parents first, as
// application statements take them, with every wait at most LockTimeout and
// the statement at most that long; under them the leaves are read again and
// the drop refuses unless each is still empty, so no row can be lost to a
// write that landed after the run's first read. Files are unlinked after the
// commit releases the locks.
func dropContractWeekLeaves(ctx context.Context, r PartitionRange, leaves []contractWeekLeaf, settings *ContractWeekRetirementSettings) (dropped []string, err error) {
	blockers, err := contractLockAdmission(ctx, contractDropWeekScope(leaves, settings))
	if err != nil {
		return nil, err
	}
	if 0 < len(blockers) {
		return nil, &ContractPartitionBusyError{Blockers: blockers}
	}
	tables := slices.Clone(ContractPartitionTables)
	slices.Reverse(tables)
	locked := []string{}
	for _, table := range tables {
		locked = append(locked, "ONLY public."+quoteIdentifier(table))
	}
	for _, table := range tables {
		locked = append(locked, "public."+quoteIdentifier(contractPartitionFamily.LeafName(table, r)))
	}
	err = contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, contractLockTimeoutSql(settings.LockTimeout)))
		RaisePgResult(tx.Exec(ctx, contractStatementTimeoutSql(settings.LockTimeout)))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("LOCK TABLE %s IN ACCESS EXCLUSIVE MODE", strings.Join(locked, ", "))))
		RaisePgResult(tx.Exec(ctx, contractStatementTimeoutSql(settings.DropTimeout)))
		for _, leaf := range readContractWeekLeavesInConn(ctx, tx, r) {
			if !leaf.exists || 0 < leaf.bytes() {
				panic(fmt.Errorf("%w: %s holds %d bytes", errContractWeekNotEmpty, leaf.name, leaf.bytes()))
			}
		}
		for _, table := range tables {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
				"ALTER TABLE public.%s DETACH PARTITION public.%s",
				quoteIdentifier(table),
				quoteIdentifier(contractPartitionFamily.LeafName(table, r)),
			)))
		}
		for _, table := range tables {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("DROP TABLE public.%s", quoteIdentifier(contractPartitionFamily.LeafName(table, r)))))
		}
	})
	if err != nil {
		return nil, err
	}
	for _, table := range tables {
		dropped = append(dropped, contractPartitionFamily.LeafName(table, r))
	}
	return
}

// Why a step did not run: a busy admission, a lock or statement timeout, or
// another failure, in one line.
func contractRetirementFailure(step string, err error) string {
	var busy *ContractPartitionBusyError
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &busy):
		return fmt.Sprintf("%s waits, without queueing, for: %s", step, strings.Join(busy.Blockers, "; "))
	case errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "40P01"):
		return fmt.Sprintf("%s: its lock was busy; retried next run", step)
	case errors.As(err, &pgErr) && pgErr.Code == "57014":
		return fmt.Sprintf("%s: cut off by its statement timeout; nothing changed", step)
	default:
		return fmt.Sprintf("%s: %s", step, err)
	}
}

// Retires each expired week, oldest first, until deadline (see the file
// comment).
func retireExpiredContractWeeks(ctx context.Context, now time.Time, settings *ContractWeekRetirementSettings, deadline time.Time, report *ContractPartitionReport) {
	expired, err := contractExpiredWeeks(ctx, now, settings)
	if err != nil {
		glog.Errorf("[cp]%s\n", err)
		return
	}
	for _, r := range expired {
		if !time.Now().Before(deadline) {
			report.More = true
			return
		}
		retireContractWeek(ctx, r, settings, deadline, report)
	}
}

// One expired week: compacts each leaf the reaper has emptied down to the rows
// it keeps, and drops the week once its leaves hold no tuple.
func retireContractWeek(ctx context.Context, r PartitionRange, settings *ContractWeekRetirementSettings, deadline time.Time, report *ContractPartitionReport) {
	week := contractPartitionFamily.LeafName(ContractPartitionTables[0], r)
	leaves, err := readContractWeekLeaves(ctx, r)
	if err != nil {
		report.Blocked[week] = fmt.Sprintf("read its leaves: %s", err)
		return
	}
	missing := []string{}
	for _, leaf := range leaves {
		if !leaf.exists {
			missing = append(missing, leaf.name)
		}
	}
	if 0 < len(missing) {
		report.Blocked[week] = fmt.Sprintf("leaves missing: %s", strings.Join(missing, ", "))
		return
	}
	empty := func(leaves []contractWeekLeaf) bool {
		return !slices.ContainsFunc(leaves, func(leaf contractWeekLeaf) bool {
			return 0 < leaf.bytes()
		})
	}

	if !empty(leaves) {
		kept := int64(0)
		blocked := false
		for _, leaf := range leaves {
			if leaf.bytes() == 0 {
				continue
			}
			if !time.Now().Before(deadline) {
				report.More = true
				return
			}
			small := leaf.bytes() <= settings.CompactAboveBytes
			if !small && settings.ProbeGateRows < leaf.liveRows {
				report.Blocked[leaf.name] = fmt.Sprintf(
					"about %d live rows by the statistics in %d bytes: the reaper has not emptied it, or keeps more than a compaction moves",
					leaf.liveRows,
					leaf.bytes(),
				)
				blocked = true
				continue
			}
			live, err := contractWeekLeafLiveRows(ctx, leaf, settings.MaxCompactRows+1, settings)
			if err != nil {
				report.Blocked[leaf.name] = contractRetirementFailure("count its live rows", err)
				blocked = true
				continue
			}
			if settings.MaxCompactRows < live {
				report.Blocked[leaf.name] = fmt.Sprintf(
					"more than %d live rows: the reaper keeps them, or has not reaped the week",
					settings.MaxCompactRows,
				)
				blocked = true
				continue
			}
			kept += live
			if 0 < live && small {
				// compact already
				continue
			}
			if err := compactContractWeekLeaf(ctx, leaf, settings); err != nil {
				report.Blocked[leaf.name] = contractRetirementFailure("compact", err)
				blocked = true
				continue
			}
			report.Compacted = append(report.Compacted, leaf.name)
			contractPartitionGlogf("compacted %s: %d bytes held %d live rows", leaf.name, leaf.bytes(), live)
		}
		if blocked {
			return
		}
		if leaves, err = readContractWeekLeaves(ctx, r); err != nil {
			report.Blocked[week] = fmt.Sprintf("read its leaves: %s", err)
			return
		}
		if !empty(leaves) {
			// the rows the reaper keeps, until its rescans delete them
			report.Kept[week] = kept
			return
		}
	}

	if !time.Now().Before(deadline) {
		report.More = true
		return
	}
	dropped, err := dropContractWeekLeaves(ctx, r, leaves, settings)
	if err != nil {
		if errors.Is(err, errContractWeekNotEmpty) {
			report.Blocked[week] = "a row landed in the week before its drop; retried next run"
		} else {
			report.Blocked[week] = contractRetirementFailure("drop", err)
		}
		return
	}
	report.Dropped = append(report.Dropped, dropped...)
}
