// Daily partitions of transfer_debit_journal by contract_id
// (PARTITION-CANDIDATES.md §6.3, owner decision 3 on 2026-11-16), a family of
// the partition registry (db_partition_registry.go).
//
// The journal is a queue: a contract's outcome inserts one row per escrow
// balance, and the settlement worker applies and deletes it seconds later
// (model/subscription_transfer_debit.go). Contracts live 60 minutes, so a
// contract day takes its last normal debit about 75 minutes after it ends,
// and its partition is then empty for good. A day leaf, holding a day of churn
// and whatever bloat it gathered, is dropped whole instead of vacuumed back.
//
// The primary key (balance_id, contract_id) holds the partition key, and
// nothing uses ON CONFLICT, a ctid, TRUNCATE or LOCK TABLE on the journal.
// Every statement on it is read committed or autocommit. The layout:
//   - the legacy partition, transfer_debit_journal_legacy: the table as it
//     was at the cutover (db_transfer_debit_journal_cutover.go), every
//     contract id below the cutover day. It drains within about 75 minutes and
//     retires like a day.
//   - daily partitions, transfer_debit_journal_pYYYYMMDD, bounded by
//     contract_id_floor of the day's start and end and created
//     TransferDebitJournalAheadDays ahead by the registry's runway.
//   - the DEFAULT partition, transfer_debit_journal_default: a late debit of a
//     contract whose day was already dropped (an operator scan, a startup
//     closure, an expiry repair), or of an id outside every created day (a
//     clock far off). No debit is ever refused. Never dropped.
//
// A day drops once its upper bound is TransferDebitJournalRetention past and
// it holds no row at all, not even an applied one awaiting its Redis release:
// dropping a row the worker has not deleted would lose a balance change or a
// release. The retention task drops it under the guard's opt-in
// (transfer_debit_journal_drop_guard, which reads the database's clock and
// the leaf itself), and the registry checks emptiness again on the detached
// leaf in the drop's own transaction, where no statement can reach it.
//
// The rotation's exclusive locks stand on the settlement path: a detach takes
// ACCESS EXCLUSIVE on the parent (a DEFAULT partition rules out DETACH
// CONCURRENTLY) and on DEFAULT, and an attach takes ACCESS EXCLUSIVE on
// DEFAULT, which every balance and shard read of the parent locks too. Each is
// catalog work on an empty leaf plus, for an attach, a scan of DEFAULT, which
// holds only stragglers for seconds. So every create and drop first waits out
// transactions older than MaxTransactionAge holding a journal lock, then waits
// at most LockTimeout for its locks, and a busy pass retries on the next one.
// A settlement statement queued behind the request waits at most that plus the
// hold, within the flush's own 250 ms bound. Both run in the taskworker on
// Main, next to the database: no lock is held across a network round trip.
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"
)

const transferDebitJournalTable = "transfer_debit_journal"

// The opt-in of the drop guard: the retention task's drop transaction sets it.
const transferDebitJournalRetentionSetting = "urnetwork.transfer_debit_journal_retention"

// A day's partition drops this long after its upper bound, once empty: the day
// takes its last normal debit about 75 minutes after it ends. The drop guard
// publishes the same two days (transferDebitJournalDropGuardSql).
const TransferDebitJournalRetention = 48 * time.Hour

// Days after the current one that always have a partition. The runway keeps
// them, and the cutover stages them after its cutover day.
const TransferDebitJournalAheadDays = 3

// The storage settings of every leaf the family makes, the published table's
// own: fillfactor 70 (the applied flag's update stays HOT,
// db_write_path_migrations.go) and A5's autovacuum options, which
// playbook-dbs.yml applies to the plain table. A partitioned parent takes no
// storage settings; the legacy partition keeps the table's.
var transferDebitJournalLeafReloptions = []string{
	"fillfactor=70",
	"autovacuum_vacuum_cost_delay=0",
	"autovacuum_vacuum_insert_scale_factor=0",
	"autovacuum_vacuum_insert_threshold=1000",
	"autovacuum_vacuum_scale_factor=0",
	"autovacuum_vacuum_threshold=50",
}

// Tunables of the journal's partitions: the runway's creates, the retention
// task's drops and the owner-run cutover.
type TransferDebitJournalPartitionSettings struct {
	// A create, a drop and the cutover's exclusive steps first wait out
	// transactions older than this that hold a journal lock, so their lock
	// request never queues settlement behind one.
	MaxTransactionAge time.Duration
	// Each lock wait of a create, a drop and the cutover's exclusive steps.
	// Settlement statements queue behind the request, then behind the hold,
	// and the debit flush gives up after 250 ms, so the wait plus the hold
	// stays below it; a busy table is retried on the next pass.
	LockTimeout time.Duration
	// Bounds each statement of those transactions: catalog work, an attach's
	// scan of DEFAULT and the few rows a move carries.
	StatementTimeout time.Duration
	// A cutover step retries this many times while the journal is busy, this
	// long apart.
	MaxAttempts   int
	RetryInterval time.Duration
	// VALIDATE waits this long for its SHARE UPDATE EXCLUSIVE lock, which only
	// vacuum, analyze and DDL contend for.
	ValidateLockTimeout time.Duration
	// Rollback copies the rows outside the legacy partition back into it
	// under its lock, and refuses when there are more than this.
	RollbackMaxRows int
	// The cutover day starts at least this long after prepare, leaving time
	// for VALIDATE and the swap.
	PrepareLeadTime time.Duration
	// The swap refuses to start this close to the cutover day.
	FinalizeMargin time.Duration
	// A prepared cutover still unfinalized this long before its day is
	// abandoned by the runway pass: from that day on its bound would refuse
	// every debit.
	AbandonMargin time.Duration

	// runs inside a drop's transaction once its fence holds the locks
	afterDropLockForTest func()
	// runs before each cutover step; an error interrupts the step
	beforeCutoverStepForTest func(step string) error
	// runs once the swap read the prepared state, before its script; an
	// error interrupts the swap
	beforeSwapScriptForTest func() error
}

// Lock waits of 100 ms: with the holds measured on a test database (a day's
// attach 24–38 ms, a drop's remainder about 30 ms, the cutover's swap about
// 100 ms) a settlement statement queued behind one waits at most about 200 ms,
// within the flush's 250 ms. Ten seconds of catalog work; a day's lead and two
// hours' margin.
func DefaultTransferDebitJournalPartitionSettings() *TransferDebitJournalPartitionSettings {
	return &TransferDebitJournalPartitionSettings{
		MaxTransactionAge:   5 * time.Second,
		LockTimeout:         100 * time.Millisecond,
		StatementTimeout:    10 * time.Second,
		MaxAttempts:         60,
		RetryInterval:       5 * time.Second,
		ValidateLockTimeout: 60 * time.Second,
		RollbackMaxRows:     200000,
		PrepareLeadTime:     12 * time.Hour,
		FinalizeMargin:      2 * time.Hour,
		AbandonMargin:       2 * time.Hour,
	}
}

// The drop guard: admits a drop only from the retention task's transaction
// (the opt-in), only of a partition of the journal bounded above (never
// DEFAULT), only once its upper bound is two days past by the database's
// clock, and only while the partition holds no row. The fence calls it with
// the leaf's lock held, before the detach. CREATE OR REPLACE FUNCTION changes
// only a catalog row, with no table lock, and nothing calls it before the
// cutover, so it applies before or after any deploy.
const transferDebitJournalDropGuardSql = `
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE FUNCTION transfer_debit_journal_drop_guard(leaf text) RETURNS void LANGUAGE plpgsql AS $debit_drop_guard$
DECLARE
 upper_bound uuid;
 held boolean;
BEGIN
 IF current_setting('urnetwork.transfer_debit_journal_retention',true) IS DISTINCT FROM 'on' THEN
  RAISE EXCEPTION 'transfer debit journal partitions are dropped only by their retention';
 END IF;
 SELECT (regexp_match(pg_get_expr(c.relpartbound,c.oid),'TO \(''([0-9a-f-]{36})''\)$'))[1]::uuid INTO upper_bound
 FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
 WHERE i.inhparent=to_regclass('public.transfer_debit_journal') AND c.oid=to_regclass('public.'||quote_ident(leaf));
 IF upper_bound IS NULL OR contract_id_floor(now()-interval '2 days')<upper_bound THEN
  RAISE EXCEPTION 'transfer debit journal partitions are kept 2 days past their contracts';
 END IF;
 EXECUTE format('SELECT EXISTS (SELECT 1 FROM ONLY public.%I)',leaf) INTO held;
 IF held THEN
  RAISE EXCEPTION 'transfer debit journal partition % holds debits',leaf USING ERRCODE='55000';
 END IF;
END
$debit_drop_guard$;
`

var transferDebitJournalDropGuardMigration = newSqlMigration(transferDebitJournalDropGuardSql)

// The version of the drop guard's migration, found by its SQL: versions are
// positional, so it follows the migration wherever the list places it. Zero
// when the list lacks it.
func TransferDebitJournalDropGuardMigrationVersion() int {
	return sqlMigrationVersion(transferDebitJournalDropGuardSql)
}

// The guard's function body as the migration publishes it, for the monitor.
func TransferDebitJournalDropGuardBodySql() string {
	return strings.Split(transferDebitJournalDropGuardSql, "$debit_drop_guard$")[1]
}

// The partition registry's family of the journal (see the file comment).
// Registered from init, since its hooks refer back to it.
var transferDebitJournalPartitionFamily *PartitionFamily

func init() {
	transferDebitJournalPartitionFamily = RegisterPartitionFamily(newTransferDebitJournalPartitionFamily())
}

// The family's declaration, unregistered.
func newTransferDebitJournalPartitionFamily() *PartitionFamily {
	settings := DefaultTransferDebitJournalPartitionSettings()
	return &PartitionFamily{
		Name:        transferDebitJournalTable,
		Description: "transfer_debit_journal, daily by contract_id; a day drops 2 days after it ends once it holds no debit",
		Tables:      []string{transferDebitJournalTable},
		KeyColumn:   "contract_id",
		KeyKind:     PartitionKeyId,
		Granularity: PartitionGranularityDaily,
		AheadRanges: TransferDebitJournalAheadDays,
		Retention:   TransferDebitJournalRetention,
		// late debits of contracts whose day is gone, and ids far off
		DefaultPartition: true,
		// the table before the cutover drains like a day and retires like one
		KeepLegacyPartition: false,
		DropGuardSetting:    transferDebitJournalRetentionSetting,
		LeafReloptions:      transferDebitJournalLeafReloptions,
		ReindexExempt:       true,
		// a staged parent of a prepared cutover
		CutoverTables: []string{contractStagedParentName(transferDebitJournalTable)},
		Settings: &PartitionFamilySettings{
			CreateLockTimeout:    settings.LockTimeout,
			DropLockTimeout:      settings.LockTimeout,
			DropStatementTimeout: settings.StatementTimeout,
		},
		BeforeCutover: func(ctx context.Context, now time.Time) error {
			abandonStaleTransferDebitJournalCutover(ctx, now, DefaultTransferDebitJournalPartitionSettings(), transferDebitJournalPartitionLogf)
			return nil
		},
		CreateLeaf: func(ctx context.Context, table string, r PartitionRange) error {
			report, err := createTransferDebitJournalLeaf(ctx, table, r, DefaultTransferDebitJournalPartitionSettings())
			if err == nil && !report.Skipped {
				transferDebitJournalPartitionLogf("created %s: the attach held its locks %s, %d straggler(s) moved out of DEFAULT", report.Leaf, report.AttachDuration.Round(time.Microsecond), report.Moved)
			}
			return err
		},
		DropFence: func(ctx context.Context, tx PgTx, r PartitionRange) {
			fenceTransferDebitJournalDrop(ctx, tx, r, DefaultTransferDebitJournalPartitionSettings())
		},
		DropPrecondition: transferDebitJournalDayBlocker,
	}
}

func transferDebitJournalPartitionLogf(format string, args ...any) {
	glog.Infof("[partitions]transfer_debit_journal: "+format+"\n", args...)
}

// Whether the journal is partitioned yet.
func IsTransferDebitJournalPartitioned(ctx context.Context) bool {
	return transferDebitJournalPartitionFamily.IsPartitioned(ctx)
}

func transferDebitJournalLegacyLeafName() string {
	return PartitionLegacyLeafName(transferDebitJournalTable)
}

func transferDebitJournalDefaultLeafName() string {
	return PartitionDefaultLeafName(transferDebitJournalTable)
}

func transferDebitJournalStagedParentName() string {
	return contractStagedParentName(transferDebitJournalTable)
}

// The relations whose long-held locks would queue a request for ACCESS
// EXCLUSIVE on the journal: every statement names the parent, and a create or
// drop also locks DEFAULT; the staged parent and the legacy partition while a
// cutover is prepared or draining.
func transferDebitJournalLockRelations() []string {
	return []string{
		"public." + transferDebitJournalTable,
		"public." + transferDebitJournalStagedParentName(),
		"public." + transferDebitJournalLegacyLeafName(),
		"public." + transferDebitJournalDefaultLeafName(),
	}
}

// Refuses, by raising, while a transaction older than MaxTransactionAge holds
// a journal lock: the caller's lock request would queue settlement behind it.
func requireTransferDebitJournalQuiet(ctx context.Context, conn PgQuerier, settings *TransferDebitJournalPartitionSettings) {
	if busy := partitionRelationsBusy(ctx, conn, transferDebitJournalLockRelations(), settings.MaxTransactionAge); 0 < busy {
		panic(fmt.Errorf("%d transaction(s) older than %s hold transfer debit journal locks; retried later", busy, settings.MaxTransactionAge))
	}
}

// What one create of a day did, and how long its attach held its locks, as
// the database's clock measured it: from just before the attach statement
// until it returned, the stretch every balance and shard read of the journal
// waits through on DEFAULT. The commit follows at once.
type TransferDebitJournalCreateReport struct {
	Leaf string
	// another create had already made the range
	Skipped bool
	// rows of the range moved out of DEFAULT before the attach
	Moved          int64
	AttachDuration time.Duration
}

// The statements that move the day's rows out of DEFAULT into the new leaf,
// and how the counts must agree. Both run under ACCESS EXCLUSIVE on the parent
// and DEFAULT, so no statement routes a row of the range meanwhile.
func transferDebitJournalMoveSql(leaf string, r PartitionRange, columns string) (copySql string, deleteSql string) {
	lower := contractIdLiteral(ContractIdFloor(r.Lower))
	upper := contractIdLiteral(ContractIdFloor(r.Upper))
	defaultLeaf := quoteIdentifier(transferDebitJournalDefaultLeafName())
	copySql = fmt.Sprintf(
		"INSERT INTO public.%s (%s) SELECT %s FROM ONLY public.%s WHERE %s <= contract_id AND contract_id < %s",
		quoteIdentifier(leaf), columns, columns, defaultLeaf, lower, upper,
	)
	deleteSql = fmt.Sprintf("DELETE FROM ONLY public.%s WHERE %s <= contract_id AND contract_id < %s", defaultLeaf, lower, upper)
	return
}

// Creates table's leaf for r (the family's CreateLeaf). The DEFAULT partition
// is created as the registry creates it. A day waits out long transactions on
// the journal, then:
//   - without rows of its range in DEFAULT (the normal case: a day three ahead
//     has no contracts yet), runs the registry's create. Its attach takes
//     SHARE UPDATE EXCLUSIVE on the parent and ACCESS EXCLUSIVE on DEFAULT,
//     which it scans. A row of the range that a writer routed to DEFAULT
//     between the check and the attach fails the attach, which the next pass
//     retries once the worker has deleted it.
//   - with such rows (a clock far ahead), takes ACCESS EXCLUSIVE on the parent
//     and then DEFAULT, the order a detach takes them in, creates the leaf,
//     moves the rows into it and attaches it. The parent lock keeps a writer
//     from routing another row of the range into DEFAULT meanwhile.
func createTransferDebitJournalLeaf(ctx context.Context, table string, r PartitionRange, settings *TransferDebitJournalPartitionSettings) (report TransferDebitJournalCreateReport, err error) {
	family := transferDebitJournalPartitionFamily
	if r.Legacy {
		return report, fmt.Errorf("the legacy partition of %s is made by its cutover, not created", table)
	}
	report.Leaf = family.LeafName(table, r)
	err = partitionRegistryTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.LockTimeout.Milliseconds())))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.StatementTimeout.Milliseconds())))
		requireTransferDebitJournalQuiet(ctx, tx, settings)
		// another create may have won
		leaves, listErr := family.ListLeaves(ctx, tx, table)
		Raise(listErr)
		if partitionRangeOverlapped(leaves, r) || (r.Default && transferDebitJournalHasDefault(leaves)) {
			report.Skipped = true
			return
		}
		statements, createErr := family.CreateLeafSql(ctx, tx, table, r)
		Raise(createErr)
		attach := statements[len(statements)-1]
		defaultExists := transferDebitJournalHasDefault(leaves)
		var ahead bool
		if !r.Default && defaultExists {
			Raise(tx.QueryRow(ctx, fmt.Sprintf(
				"SELECT EXISTS (SELECT 1 FROM ONLY public.%s WHERE %s <= contract_id AND contract_id < %s)",
				quoteIdentifier(transferDebitJournalDefaultLeafName()),
				contractIdLiteral(ContractIdFloor(r.Lower)),
				contractIdLiteral(ContractIdFloor(r.Upper)),
			)).Scan(&ahead))
		}
		if ahead {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("LOCK TABLE ONLY public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(table))))
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(transferDebitJournalDefaultLeafName()))))
		}
		for _, statement := range statements[:len(statements)-1] {
			RaisePgResult(tx.Exec(ctx, statement))
		}
		if ahead {
			copySql, deleteSql := transferDebitJournalMoveSql(report.Leaf, r, closeEvidenceColumnList(ctx, tx, table))
			copied := RaisePgResult(tx.Exec(ctx, copySql)).RowsAffected()
			deleted := RaisePgResult(tx.Exec(ctx, deleteSql)).RowsAffected()
			if copied != deleted {
				panic(fmt.Errorf("moved %d rows of %s out of DEFAULT but copied %d", deleted, report.Leaf, copied))
			}
			report.Moved = copied
		}
		var attachStart time.Time
		Raise(tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&attachStart))
		RaisePgResult(tx.Exec(ctx, attach))
		var attachEnd time.Time
		Raise(tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&attachEnd))
		report.AttachDuration = attachEnd.Sub(attachStart)
	})
	return
}

func transferDebitJournalHasDefault(leaves []PartitionLeaf) bool {
	for _, leaf := range leaves {
		if leaf.Range.Default {
			return true
		}
	}
	return false
}

// Runs first in a day's drop transaction, after the registry set the guard's
// opt-in: refuses, by raising, a busy journal and the DEFAULT partition; then
// takes the locks the detach needs in the order every statement takes them,
// the parent, DEFAULT, then the leaf; then asks the guard, which reads the
// database's clock and the leaf under its lock.
func fenceTransferDebitJournalDrop(ctx context.Context, tx PgTx, r PartitionRange, settings *TransferDebitJournalPartitionSettings) {
	if r.Default {
		panic(errors.New("the DEFAULT partition of the transfer debit journal is never dropped"))
	}
	family := transferDebitJournalPartitionFamily
	leaf := family.LeafName(transferDebitJournalTable, r)
	requireTransferDebitJournalQuiet(ctx, tx, settings)
	RaisePgResult(tx.Exec(ctx, fmt.Sprintf("LOCK TABLE ONLY public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(transferDebitJournalTable))))
	var defaultExists bool
	Raise(tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+transferDebitJournalDefaultLeafName()).Scan(&defaultExists))
	if defaultExists {
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(transferDebitJournalDefaultLeafName()))))
	}
	RaisePgResult(tx.Exec(ctx, fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(leaf))))
	if settings.afterDropLockForTest != nil {
		settings.afterDropLockForTest()
	}
	RaisePgResult(tx.Exec(ctx, `SELECT transfer_debit_journal_drop_guard($1)`, leaf))
}

// The reason a day must stay (the family's DropPrecondition): it holds a row,
// pending or applied and awaiting its Redis release. It runs before the drop,
// and again on the detached leaf in the drop's transaction, where no statement
// can reach it.
func transferDebitJournalDayBlocker(ctx context.Context, conn PgQuerier, r PartitionRange) string {
	if r.Default {
		return "the DEFAULT partition is never dropped"
	}
	leaf := transferDebitJournalPartitionFamily.LeafName(transferDebitJournalTable, r)
	var held bool
	Raise(conn.QueryRow(ctx, fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM ONLY public.%s)", quoteIdentifier(leaf))).Scan(&held))
	if held {
		return "debits not yet applied and released"
	}
	return ""
}

// The journal's cutover is not schema drift: while it is prepared, the staged
// parent with its indexes (PartitionFamily.CutoverTables) and the bound check
// on the table are its own work in progress, which finalize or abort removes.
// After the swap the parent matches the migrated table, its indexes read
// without ONLY, and its partitions are not part of the expected schema.
func (d *SchemaDiff) acceptTransferDebitJournalPartitionCutover() {
	extra := d.extraConstraints[transferDebitJournalTable]
	kept := extra[:0]
	for _, name := range extra {
		if name != contractLegacyBoundName(transferDebitJournalTable) {
			kept = append(kept, name)
		}
	}
	if len(kept) == 0 {
		delete(d.extraConstraints, transferDebitJournalTable)
	} else {
		d.extraConstraints[transferDebitJournalTable] = kept
	}
}
