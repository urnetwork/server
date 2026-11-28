// Retirement of the contract days at the contract horizon (S1,
// CONTRACT-PARTITION.md "S1: days retire at the reaper's horizon"; daily since
// the owner's decision of 2026-11-17, weekly before).
//
// The contract horizon reaper (model/contract_horizon_reaper.go) is the only
// code that decides which contract may go. It deletes a contract with its
// closes, escrows, sweeps, extender parties and close reports in one
// transaction, under its keep rules, and the usage archive trigger copies a
// credit-bearing contract's proof in the same statement. Retirement deletes no
// row and judges none: it returns the storage of the rows the reaper has
// deleted, and keeps every row the reaper keeps, by construction.
//
//   - A day expires RetireDelay after its upper bound passes the reaper's
//     horizon (ContractDayRetention: 14 days at the shortest) while the
//     reaper's mode is on; otherwise at ContractRetention (91 days). With the
//     mode off the reaper deletes nothing, so an expired day stays.
//   - An expired day whose six leaves hold no tuple at all, live or dead, in a
//     heap or its TOAST, is detached and dropped in one transaction. Nothing a
//     snapshot could see goes with it.
//   - An expired day that still holds rows has each leaf rewritten by CLUSTER
//     once the reaper has emptied it down to the rows it keeps. CLUSTER copies
//     every live and recently dead row version with its transaction ids (it is
//     MVCC-safe, unlike a copy or TRUNCATE), so every snapshot sees the same
//     rows after it, and the old files go back to the filesystem. A leaf goes
//     in its own transaction, which locks that leaf alone; its parents and
//     every other leaf stay untouched.
//   - A compacted day stays while the reaper keeps any of its rows. Once the
//     reaper's rescans have deleted them, the next run empties its leaves with
//     CLUSTER and drops them.
//
// The compaction is kept at day size. The reaper keeps about one contract in
// 900,000 at its horizon (early October on Main), about 140 of a day's 124M at
// 1,439 a second, so nearly every day holds kept rows when it expires, and
// some keep reasons last weeks. Main runs with vacuum_truncate off, so a day's
// leaves keep their files after vacuum, however empty. Without the rewrite, a
// single kept row, or none, would hold a whole day's 140-180 GiB until its
// drop; CLUSTER of a day moves about a seventh of a week's kept rows, and its
// empty result is the proof the drop needs that no snapshot can see a row.
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
	"github.com/urnetwork/glog/v2026"
)

// The shortest contract horizon the reaper applies (model.ContractHorizonFloor:
// 14 days, owner 2026-11-23 for the C1 transition; 35 days from 2026-11-01): a
// day never expires sooner than this past its upper bound, whatever ctx
// carries. It equals the reaper's floor (model's
// TestContractDayRetentionFloorIsTheReaperFloor). Above it, a day the reaper
// empties at a shorter horizon would keep its storage until this floor, and
// the shorter horizon would return nothing after the cutover. Below it, it
// would never bind: the horizon ctx carries is the reaper's, never below the
// reaper's floor.
const ContractDayRetentionFloor = 14 * 24 * time.Hour

// Tunables of the retirement of expired contract days.
type ContractDayRetirementSettings struct {
	// How long past the horizon a day waits before it is examined: the reaper
	// reads a contract only its close settle time (2 hours) past the horizon,
	// and lags it further for the hours of its weekly rescan. A day examined
	// before the reaper has emptied it waits, reported, without a lock (the
	// statistics gate, ProbeGateRows).
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
	// so its hold on the leaf's lock, grows with them. A day the reaper keeps
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

func DefaultContractDayRetirementSettings() *ContractDayRetirementSettings {
	return &ContractDayRetirementSettings{
		// the reaper's close settle time (2 h), the hours its weekly rescan
		// holds back its frontier (about 3 h at 1,900 contracts a second,
		// 2026-11-16), and an hour more. Held a day past the horizon, as the
		// weeks were, each day would hold 18 hours more of Main's inflow,
		// 105-135 GiB at 140-180 GiB a day.
		RetireDelay:   6 * time.Hour,
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

// How long past its upper bound a contract day is kept: the contract horizon
// ctx carries while the reaper's mode is on (WithContractHorizon), between
// ContractDayRetentionFloor and ContractRetention; without one,
// ContractRetention.
func ContractDayRetention(ctx context.Context) time.Duration {
	horizon, ok := contractHorizonIn(ctx)
	if !ok {
		return ContractRetention
	}
	return min(max(horizon, ContractDayRetentionFloor), ContractRetention)
}

// How long past its upper bound a day expires: its retention and the
// settings' delay, never later than ContractRetention, the family's declared
// retention.
func contractDayExpiry(ctx context.Context, settings *ContractDayRetirementSettings) time.Duration {
	return min(ContractDayRetention(ctx)+settings.RetireDelay, ContractRetention)
}

// The family's RangeExpired: whether the day r (or the legacy partition, up to
// the cutover day) has expired at now.
func contractDayRangeExpired(ctx context.Context, now time.Time, r PartitionRange) bool {
	return contractDayExpired(ctx, now, r, DefaultContractDayRetirementSettings())
}

func contractDayExpired(ctx context.Context, now time.Time, r PartitionRange, settings *ContractDayRetirementSettings) bool {
	if r.Default || r.Upper.IsZero() {
		return false
	}
	return !now.Before(r.Upper.Add(contractDayExpiry(ctx, settings)))
}

// The expired ranges of the contract tables, oldest first: the canonical
// leaves of transfer_contract whose day has expired at now.
func contractExpiredDays(ctx context.Context, now time.Time, settings *ContractDayRetirementSettings) (expired []PartitionRange, err error) {
	var leaves []PartitionLeaf
	MaintenanceDb(ctx, func(conn PgConn) {
		leaves, err = contractPartitionFamily.ListLeaves(ctx, conn, ContractPartitionTables[0])
	})
	if err != nil {
		return nil, err
	}
	for _, leaf := range leaves {
		if leaf.Canonical && contractDayExpired(ctx, now, leaf.Range, settings) {
			expired = append(expired, leaf.Range)
		}
	}
	slices.SortStableFunc(expired, func(a PartitionRange, b PartitionRange) int {
		return a.Upper.Compare(b.Upper)
	})
	return
}

// One day's leaf of one contract table, as the catalog describes it.
type contractDayLeaf struct {
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
func (self contractDayLeaf) bytes() int64 {
	return self.heapBytes + self.toastBytes
}

// The six leaves of a day ($1, leaf names in table order), each with its
// storage, live row count and primary key.
const contractDayLeavesSql = `
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

func readContractDayLeavesInConn(ctx context.Context, conn PgQuerier, r PartitionRange) (leaves []contractDayLeaf) {
	names := contractPartitionFamily.LeafNames(r)
	result, err := conn.Query(ctx, contractDayLeavesSql, names)
	WithPgResult(result, err, func() {
		for i := 0; result.Next(); i += 1 {
			leaf := contractDayLeaf{table: ContractPartitionTables[i]}
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

func readContractDayLeaves(ctx context.Context, r PartitionRange) (leaves []contractDayLeaf, err error) {
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			leaves = readContractDayLeavesInConn(ctx, conn, r)
		})
	})
	return
}

// The family's DropPrecondition: the day stays while any of its leaves holds
// a tuple, live or dead. Only the reaper deletes contract rows; a drop takes
// none with it.
func contractDayNotEmpty(ctx context.Context, conn PgQuerier, r PartitionRange) string {
	for _, leaf := range readContractDayLeavesInConn(ctx, conn, r) {
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
func contractDayLeafLiveRows(ctx context.Context, leaf contractDayLeaf, limit int64, settings *ContractDayRetirementSettings) (rows int64, err error) {
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
func contractCompactLeafSql(leaf contractDayLeaf, settings *ContractDayRetirementSettings) (statements []string) {
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
func contractCompactScope(leaf contractDayLeaf, settings *ContractDayRetirementSettings) *contractLockScope {
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
func compactContractDayLeaf(ctx context.Context, leaf contractDayLeaf, settings *ContractDayRetirementSettings) error {
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

// The admission of a day's drop: the six parents alone, whose DETACH takes
// ACCESS EXCLUSIVE on each, and the day's leaves and their TOAST, which the
// DROP locks.
func contractDropDayScope(leaves []contractDayLeaf, settings *ContractDayRetirementSettings) *contractLockScope {
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
var errContractDayNotEmpty = errors.New("the day holds rows")

// Detaches and drops a day's six physically empty leaves in one transaction,
// children first. The locks are taken in one statement, parents first, as
// application statements take them, with every wait at most LockTimeout and
// the statement at most that long; under them the leaves are read again and
// the drop refuses unless each is still empty, so no row can be lost to a
// write that landed after the run's first read. Files are unlinked after the
// commit releases the locks.
func dropContractDayLeaves(ctx context.Context, r PartitionRange, leaves []contractDayLeaf, settings *ContractDayRetirementSettings) (dropped []string, err error) {
	blockers, err := contractLockAdmission(ctx, contractDropDayScope(leaves, settings))
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
		for _, leaf := range readContractDayLeavesInConn(ctx, tx, r) {
			if !leaf.exists || 0 < leaf.bytes() {
				panic(fmt.Errorf("%w: %s holds %d bytes", errContractDayNotEmpty, leaf.name, leaf.bytes()))
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

// Retires each expired day, oldest first, until deadline (see the file
// comment).
func retireExpiredContractDays(ctx context.Context, now time.Time, settings *ContractDayRetirementSettings, deadline time.Time, report *ContractPartitionReport) {
	expired, err := contractExpiredDays(ctx, now, settings)
	if err != nil {
		glog.Errorf("[cp]%s\n", err)
		return
	}
	for _, r := range expired {
		if !time.Now().Before(deadline) {
			report.More = true
			return
		}
		retireContractDay(ctx, r, settings, deadline, report)
	}
}

// One expired day: compacts each leaf the reaper has emptied down to the rows
// it keeps, and drops the day once its leaves hold no tuple.
func retireContractDay(ctx context.Context, r PartitionRange, settings *ContractDayRetirementSettings, deadline time.Time, report *ContractPartitionReport) {
	day := contractPartitionFamily.LeafName(ContractPartitionTables[0], r)
	leaves, err := readContractDayLeaves(ctx, r)
	if err != nil {
		report.Blocked[day] = fmt.Sprintf("read its leaves: %s", err)
		return
	}
	missing := []string{}
	for _, leaf := range leaves {
		if !leaf.exists {
			missing = append(missing, leaf.name)
		}
	}
	if 0 < len(missing) {
		report.Blocked[day] = fmt.Sprintf("leaves missing: %s", strings.Join(missing, ", "))
		return
	}
	empty := func(leaves []contractDayLeaf) bool {
		return !slices.ContainsFunc(leaves, func(leaf contractDayLeaf) bool {
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
			live, err := contractDayLeafLiveRows(ctx, leaf, settings.MaxCompactRows+1, settings)
			if err != nil {
				report.Blocked[leaf.name] = contractRetirementFailure("count its live rows", err)
				blocked = true
				continue
			}
			if settings.MaxCompactRows < live {
				report.Blocked[leaf.name] = fmt.Sprintf(
					"more than %d live rows: the reaper keeps them, or has not reaped the day",
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
			if err := compactContractDayLeaf(ctx, leaf, settings); err != nil {
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
		if leaves, err = readContractDayLeaves(ctx, r); err != nil {
			report.Blocked[day] = fmt.Sprintf("read its leaves: %s", err)
			return
		}
		if !empty(leaves) {
			if kept == 0 {
				// CLUSTER copied rows the reaper deleted after the oldest
				// snapshot of the server began: its horizon is the oldest
				// transaction running in any database, its own snapshot
				// included. They go once that transaction ends.
				report.Blocked[day] = "its leaves hold no live row, only rows deleted after the oldest open transaction (of any database) began, or written since the count; it drops once they are dead to every snapshot"
				return
			}
			// the rows the reaper keeps, until its rescans delete them
			report.Kept[day] = kept
			return
		}
	}

	if !time.Now().Before(deadline) {
		report.More = true
		return
	}
	dropped, err := dropContractDayLeaves(ctx, r, leaves, settings)
	if err != nil {
		if errors.Is(err, errContractDayNotEmpty) {
			report.Blocked[day] = "a row landed in the day before its drop; retried next run"
		} else {
			report.Blocked[day] = contractRetirementFailure("drop", err)
		}
		return
	}
	report.Dropped = append(report.Dropped, dropped...)
}

// Waits until every row deleted before the call is dead to every snapshot, so
// that a compaction (CLUSTER) copies none of them, and returns nil; or returns
// what still holds them once timeout has passed. Two things decide it:
//   - every transaction id assigned before the call has ended, in every
//     database of the server, prepared transactions included: CLUSTER takes
//     its horizon from its own snapshot too, and a snapshot's xmin is the
//     oldest transaction id running anywhere on the server;
//   - no other backend of this database holds a snapshot or a transaction id
//     from before the call.
//
// A wait for the second alone, which the S1 tests used, let a compaction copy
// the rows it had deleted whenever another database of the shared local test
// server held a transaction open across them, and the day stayed
// (TestContractDayRetirementKeepsWhatTheReaperKeeps, 2026-11-17).
func Testing_WaitForSnapshotHorizon(ctx context.Context, timeout time.Duration) error {
	// the first transaction id not yet assigned when the call starts
	var since string
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `SELECT pg_snapshot_xmax(pg_current_snapshot())::text`).Scan(&since))
	})
	deadline := time.Now().Add(timeout)
	for {
		var passed bool
		var holders string
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, testingSnapshotHorizonSql, since).Scan(&passed, &holders))
		})
		if passed {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("transactions from before transaction id %s still run, or hold a snapshot of this database: %s", since, holders)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Whether every transaction id below $1 has ended on the server and no other
// backend of this database holds a snapshot or a transaction id below it; and
// what does, one line each: a backend of any database with such a transaction
// id, a backend of this database with such a snapshot, a prepared
// transaction. A snapshot taken now has the oldest running transaction id of
// the server as its xmin (lazy vacuums aside), the horizon CLUSTER computes
// from its own.
const testingSnapshotHorizonSql = `
	WITH since AS (
		SELECT $1::text::xid8 AS full_id, (($1::text::numeric % 4294967296)::bigint)::text::xid AS id
	), holders AS (
		SELECT format('pid %s (%s, database %s) transaction id %s, its transaction from %s', activity.pid, activity.backend_type, activity.datname, activity.backend_xid, activity.xact_start) AS line
		FROM pg_stat_activity AS activity, since
		WHERE activity.pid <> pg_backend_pid() AND activity.backend_xid IS NOT NULL AND age(since.id) < age(activity.backend_xid)
		UNION ALL
		SELECT format('pid %s (%s) of this database holds a snapshot from transaction id %s, its transaction from %s', activity.pid, activity.backend_type, activity.backend_xmin, activity.xact_start)
		FROM pg_stat_activity AS activity, since
		WHERE activity.pid <> pg_backend_pid() AND activity.datname = current_database() AND activity.backend_xmin IS NOT NULL AND age(since.id) < age(activity.backend_xmin)
		UNION ALL
		SELECT format('prepared transaction %s (database %s) transaction id %s, prepared %s', prepared.gid, prepared.database, prepared.transaction, prepared.prepared)
		FROM pg_prepared_xacts AS prepared, since
		WHERE age(since.id) < age(prepared.transaction)
	)
	SELECT
		since.full_id <= pg_snapshot_xmin(pg_current_snapshot()) AND NOT EXISTS (SELECT 1 FROM holders),
		coalesce((SELECT string_agg(line, '; ') FROM holders), '')
	FROM since
`
