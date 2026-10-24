// Weekly partitions of contract_close_report_evidence by report_id
// (PARTITION-MAINTENANCE-PLAN.md C5), a family of the partition registry
// (db_partition_registry.go).
//
// The primary key (client_id, report_id) includes the partition key, so ON
// CONFLICT (client_id, report_id) stays enforceable: a report id leads with
// the millisecond its reporting client minted it (connect.NewId), and one
// identity always routes to one partition. The table has no other unique
// index and no foreign key in either direction.
//
// Clients mint report ids with their own clocks, so the layout is:
//   - the legacy partition, <table>_legacy: the table as it was at the cutover
//     (db_contract_close_evidence_cutover.go), every id below the cutover
//     week. Never dropped; the row deletes empty it.
//   - weekly partitions, <table>_wYYYYMMDD, created ahead by the registry's
//     runway. Creating a week first moves that week's rows out of DEFAULT
//     (createCloseEvidenceLeaf), since PostgreSQL refuses to attach a range
//     while DEFAULT holds a row of it.
//   - the DEFAULT partition, <table>_default: every id outside the created
//     weeks, from a clock far ahead or far behind, so no report is ever
//     refused. Never dropped; the row deletes empty it.
//
// A week drops once its upper bound is 91 days past and every row in it meets
// the rule of the evidence guard (migration 857): accepted more than 91 days
// ago, its contract resolved or gone (closeEvidenceWeekBlocker). The drop
// bypasses that row guard, so only the retention task drops, under the guard's
// opt-in (DropGuardSetting, checked by fenceCloseEvidenceDrop), and the
// registry checks again on the detached week. A week holding a straggler stays;
// RemoveExpiredCloseEvidence row-deletes its qualifying rows as before, along
// with the legacy and DEFAULT partitions, and the week drops once the rest
// qualify.
package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const closeEvidenceTable = "contract_close_report_evidence"

// The opt-in of the evidence guard: 'on' for a retention delete or a drop,
// 'move' for the delete half of a move (migration 857 and the move guard).
const closeEvidenceRetentionSetting = "urnetwork.close_evidence_retention"

// A weekly partition indexes the reports it accepts more than this long after
// its week ends, the only ones that can be younger than 91 days once the week
// is 91 days past (closeEvidenceLateIndexSql). Each partition's index carries
// the bound in its predicate, so this is a fact of the schema, not a tunable.
const closeEvidenceLateAcceptance = 24 * time.Hour

// A contract's id is minted by the server that creates it before any report
// of it is accepted, so its id time is at most this far after a report's
// accepted_at, by the clock difference between two servers.
const closeEvidenceContractClockMargin = 24 * time.Hour

// Tunables of the close evidence partitions: the runway's creates, the
// retention task's drops and the owner-run cutover.
type CloseEvidencePartitionSettings struct {
	// Before a transaction requests ACCESS EXCLUSIVE on the evidence table it
	// waits out transactions older than this that hold one of its locks, so
	// the request never queues close reports behind one.
	LockSettings ContractPartitionLockSettings
	// Each lock wait of those transactions: a create, a drop, the swap,
	// rollback and abort. Their lock blocks every close report, so the wait
	// stays short and a busy table is retried.
	LockTimeout time.Duration
	// Bounds each statement of those transactions: catalog work, an attach's
	// scan of the DEFAULT partition and the few rows a move carries.
	StatementTimeout time.Duration
	// A cutover step retries this many times while the table is busy, this
	// long apart.
	MaxAttempts   int
	RetryInterval time.Duration
	// VALIDATE waits this long for its SHARE UPDATE EXCLUSIVE lock, which
	// only vacuum, analyze and DDL contend for.
	ValidateLockTimeout time.Duration
	// Rows per transaction of the cutover's move of rows past its bound.
	MoveBatchSize int
	// The cutover refuses to move more rows past its bound than this: so many
	// clocks that far ahead means the week is wrong, not the clients.
	MaxAheadRows int
	// Rollback copies the rows outside the legacy partition back into it under
	// its lock, and refuses when there are more than this.
	RollbackMaxRows int
	// The cutover week starts at least this long after prepare, leaving time
	// for VALIDATE and the swap.
	PrepareLeadTime time.Duration
	// The swap refuses to start this close to the cutover week.
	FinalizeMargin time.Duration
	// A prepared cutover still unfinalized this long before its week is
	// abandoned (abandonStaleCloseEvidenceCutover): from that week on, the
	// prepared bound would refuse every new report.
	AbandonMargin time.Duration

	// runs inside a create's transaction once it holds its locks
	afterCreateLockForTest func()
	// runs before each cutover step; an error interrupts the step
	beforeCutoverStepForTest func(step string) error
	// runs inside the swap's transaction once it holds its locks
	afterSwapLockForTest func()
}

// Seconds of lock wait and statement time, as the contract cutover uses, a
// minute between busy retries for up to ten minutes, and a day of lead time.
func DefaultCloseEvidencePartitionSettings() *CloseEvidencePartitionSettings {
	return &CloseEvidencePartitionSettings{
		LockSettings:        *DefaultContractPartitionLockSettings(),
		LockTimeout:         3 * time.Second,
		StatementTimeout:    30 * time.Second,
		MaxAttempts:         60,
		RetryInterval:       10 * time.Second,
		ValidateLockTimeout: 60 * time.Second,
		MoveBatchSize:       1000,
		MaxAheadRows:        100000,
		RollbackMaxRows:     100000,
		PrepareLeadTime:     24 * time.Hour,
		FinalizeMargin:      10 * time.Minute,
		AbandonMargin:       6 * time.Hour,
	}
}

// The partition registry's family of the close evidence (see the file
// comment). Registered from init, since its hooks refer back to it.
var closeEvidencePartitionFamily *PartitionFamily

func init() {
	closeEvidencePartitionFamily = RegisterPartitionFamily(newCloseEvidencePartitionFamily())
}

// The family's declaration, unregistered.
func newCloseEvidencePartitionFamily() *PartitionFamily {
	return &PartitionFamily{
		Name:        "close_evidence",
		Description: "contract_close_report_evidence, weekly by report_id; a week drops once every row meets the evidence guard's rule, and RemoveExpiredCloseEvidence row-deletes the rest",
		Tables:      []string{closeEvidenceTable},
		KeyColumn:   "report_id",
		KeyKind:     PartitionKeyId,
		Granularity: PartitionGranularityWeekly,
		AheadRanges: ContractPartitionAheadWeeks,
		Retention:   ContractRetention,
		// clocks outside the created weeks
		DefaultPartition: true,
		// the table before the cutover, emptied by the row deletes
		KeepLegacyPartition: true,
		DropGuardSetting:    closeEvidenceRetentionSetting,
		// stragglers and row deletes land after a week closes
		LeafAnalyzeDelay: 24 * time.Hour,
		ReindexExempt:    true,
		// A client clock behind by days keeps its weeks that much longer,
		// by design: the week waits for its stragglers' 91 days. A quarter
		// past retention is worth a look.
		MonitorRetentionGrace: ContractRetention,
		Settings: &PartitionFamilySettings{
			CreateLockTimeout:    3 * time.Second,
			DropLockTimeout:      3 * time.Second,
			DropStatementTimeout: 60 * time.Second,
		},
		CreateLeaf: func(ctx context.Context, table string, r PartitionRange) error {
			return createCloseEvidenceLeaf(ctx, table, r, DefaultCloseEvidencePartitionSettings())
		},
		DropFence: func(ctx context.Context, tx PgTx, r PartitionRange) {
			fenceCloseEvidenceDrop(ctx, tx, r, DefaultCloseEvidencePartitionSettings())
		},
		DropPrecondition: closeEvidenceWeekBlocker,
	}
}

// Whether the evidence table is partitioned yet.
func IsCloseEvidencePartitioned(ctx context.Context) bool {
	return closeEvidencePartitionFamily.IsPartitioned(ctx)
}

func closeEvidenceLegacyLeafName() string {
	return PartitionLegacyLeafName(closeEvidenceTable)
}

func closeEvidenceDefaultLeafName() string {
	return PartitionDefaultLeafName(closeEvidenceTable)
}

// The staged partitioned parent of a prepared cutover, named as the contract
// cutover names its own (contractStagedParentName).
func closeEvidenceStagedParentName() string {
	return contractStagedParentName(closeEvidenceTable)
}

// The name of a weekly partition's index of its late reports.
func closeEvidenceLateIndexName(leaf string) string {
	return leaf + "_late"
}

// The relations whose long-held locks would queue a request for ACCESS
// EXCLUSIVE on the evidence table: the table, its staged parent, and its
// legacy and DEFAULT partitions.
func closeEvidenceLockRelations() []string {
	return []string{
		"public." + closeEvidenceTable,
		"public." + closeEvidenceStagedParentName(),
		"public." + closeEvidenceLegacyLeafName(),
		"public." + closeEvidenceDefaultLeafName(),
	}
}

// The transactions other than this one, older than maxTransactionAge, that
// hold a lock on one of relations. Reads through conn, which may be the
// caller's own transaction.
const partitionRelationsBusySql = `
	SELECT count(DISTINCT activity.pid)
	FROM pg_locks lock
	JOIN pg_stat_activity activity ON activity.pid = lock.pid
	WHERE lock.relation = ANY (ARRAY(SELECT to_regclass(name) FROM unnest($1::text[]) name))
		AND activity.pid <> pg_backend_pid()
		AND activity.xact_start < now() - make_interval(secs => $2)
`

func partitionRelationsBusy(ctx context.Context, conn PgQuerier, relations []string, maxTransactionAge time.Duration) (busy int) {
	Raise(conn.QueryRow(ctx, partitionRelationsBusySql, relations, maxTransactionAge.Seconds()).Scan(&busy))
	return
}

// Refuses, by raising, when a long transaction holds an evidence table lock.
func requireCloseEvidenceQuiet(ctx context.Context, conn PgQuerier, settings *CloseEvidencePartitionSettings) {
	if busy := partitionRelationsBusy(ctx, conn, closeEvidenceLockRelations(), settings.LockSettings.MaxTransactionAge); 0 < busy {
		panic(fmt.Errorf("%d transaction(s) older than %s hold close evidence locks; retried later", busy, settings.LockSettings.MaxTransactionAge))
	}
}

// The columns of table in their order, quoted, for a copy that names them.
func closeEvidenceColumnList(ctx context.Context, conn PgQuerier, table string) string {
	columns := []string{}
	result, err := conn.Query(
		ctx,
		`
		SELECT attname FROM pg_attribute
		WHERE attrelid = to_regclass($1) AND 0 < attnum AND NOT attisdropped
		ORDER BY attnum
		`,
		"public."+table,
	)
	WithPgResult(result, err, func() {
		for result.Next() {
			var column string
			Raise(result.Scan(&column))
			columns = append(columns, quoteIdentifier(column))
		}
	})
	if len(columns) == 0 {
		panic(fmt.Errorf("%s has no columns", table))
	}
	return strings.Join(columns, ", ")
}

// The weekly partition's index of the reports it accepts more than a day
// after its week ends. Partial, so it holds only stragglers: it lets the drop
// precondition prove every row older than 91 days without reading the week.
func closeEvidenceLateIndexSql(leaf string, r PartitionRange) string {
	return fmt.Sprintf(
		"CREATE INDEX %s ON public.%s USING btree (accepted_at) WHERE accepted_at >= %s",
		quoteIdentifier(closeEvidenceLateIndexName(leaf)),
		quoteIdentifier(leaf),
		closeEvidenceLateBoundSql(r),
	)
}

// The predicate bound of a week's late index, as a timestamp literal.
func closeEvidenceLateBoundSql(r PartitionRange) string {
	return "'" + r.Upper.Add(closeEvidenceLateAcceptance).UTC().Format(partitionTimestampLayout) + "'::timestamp"
}

// The statements that create table's weekly leaf for r and take over its rows
// from the DEFAULT partition, in one transaction that holds ACCESS EXCLUSIVE
// on the parent and DEFAULT. createStatements are the registry's
// (CreateLeafSql), ending with the attach; primaryKey is the parent's primary
// key definition and columns its quoted column list.
func closeEvidenceWeekLeafSql(table string, leaf string, r PartitionRange, createStatements []string, primaryKey string, columns string, defaultExists bool) (statements []string, move []string, attach string, err error) {
	if len(createStatements) == 0 || !strings.Contains(createStatements[len(createStatements)-1], " ATTACH PARTITION ") {
		return nil, nil, "", fmt.Errorf("the create of %s does not end with its attach", leaf)
	}
	statements = slices.Clone(createStatements[:len(createStatements)-1])
	attach = createStatements[len(createStatements)-1]
	// the move's guard checks each row's copy by its key, and the attach
	// adopts this key as the leaf's partition of the parent's
	statements = append(statements, fmt.Sprintf(
		"ALTER TABLE public.%s ADD CONSTRAINT %s %s",
		quoteIdentifier(leaf),
		quoteIdentifier(leaf+"_pkey"),
		primaryKey,
	))
	statements = append(statements, closeEvidenceLateIndexSql(leaf, r))
	if defaultExists {
		lower := contractIdLiteral(ContractIdFloor(r.Lower))
		upper := contractIdLiteral(ContractIdFloor(r.Upper))
		defaultLeaf := quoteIdentifier(PartitionDefaultLeafName(table))
		move = []string{
			fmt.Sprintf(
				"INSERT INTO public.%s (%s) SELECT %s FROM public.%s WHERE %s <= report_id AND report_id < %s",
				quoteIdentifier(leaf), columns, columns, defaultLeaf, lower, upper,
			),
			fmt.Sprintf("SET LOCAL %s = 'move'", closeEvidenceRetentionSetting),
			fmt.Sprintf("DELETE FROM public.%s WHERE %s <= report_id AND report_id < %s", defaultLeaf, lower, upper),
			fmt.Sprintf("SET LOCAL %s = ''", closeEvidenceRetentionSetting),
		}
	}
	return
}

// Creates table's leaf for r (the family's CreateLeaf). The DEFAULT partition
// is created as the registry creates it. A week first takes over its rows from
// DEFAULT, in one transaction: it waits out long transactions on the table,
// takes ACCESS EXCLUSIVE on the parent and then DEFAULT (the order a detach
// takes them in), creates the leaf with its key and its late index, copies the
// week's DEFAULT rows into it, deletes them from DEFAULT as a move (the
// evidence guard admits each delete only once its copy is in the leaf), and
// attaches it. The parent lock is what makes the move safe against a
// concurrent insert into the new week: a writer blocks on the parent before it
// resolves the partition its row goes to, and once the attach commits it
// replans and routes the row into the new week. Locking DEFAULT alone would
// let a writer route its row to DEFAULT first, and then fail the partition
// check once the week exists. The lock lasts milliseconds: catalog work, the
// attach's scan of DEFAULT (which only clocks outside the created weeks fill)
// and the few rows a move carries.
func createCloseEvidenceLeaf(ctx context.Context, table string, r PartitionRange, settings *CloseEvidencePartitionSettings) error {
	family := closeEvidencePartitionFamily
	if r.Legacy {
		return fmt.Errorf("the legacy partition of %s is made by its cutover, not created", table)
	}
	lockTimeout := family.settings().CreateLockTimeout
	return partitionRegistryTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", lockTimeout.Milliseconds())))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.StatementTimeout.Milliseconds())))
		if r.Default {
			statements, err := family.CreateLeafSql(ctx, tx, table, r)
			Raise(err)
			for _, statement := range statements {
				RaisePgResult(tx.Exec(ctx, statement))
			}
			return
		}
		requireCloseEvidenceQuiet(ctx, tx, settings)
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("LOCK TABLE ONLY public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(table))))
		var defaultExists bool
		Raise(tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+PartitionDefaultLeafName(table)).Scan(&defaultExists))
		if defaultExists {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(PartitionDefaultLeafName(table)))))
		}
		if settings.afterCreateLockForTest != nil {
			settings.afterCreateLockForTest()
		}
		// another create may have won while this one waited
		leaves, err := family.ListLeaves(ctx, tx, table)
		Raise(err)
		if partitionRangeOverlapped(leaves, r) {
			return
		}
		var primaryKey string
		Raise(tx.QueryRow(
			ctx,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = to_regclass($1) AND contype = 'p'`,
			"public."+table,
		).Scan(&primaryKey))
		createStatements, err := family.CreateLeafSql(ctx, tx, table, r)
		Raise(err)
		leaf := family.LeafName(table, r)
		statements, move, attach, err := closeEvidenceWeekLeafSql(
			table,
			leaf,
			r,
			createStatements,
			primaryKey,
			closeEvidenceColumnList(ctx, tx, table),
			defaultExists,
		)
		Raise(err)
		for _, statement := range statements {
			RaisePgResult(tx.Exec(ctx, statement))
		}
		if 0 < len(move) {
			copied := RaisePgResult(tx.Exec(ctx, move[0])).RowsAffected()
			RaisePgResult(tx.Exec(ctx, move[1]))
			deleted := RaisePgResult(tx.Exec(ctx, move[2])).RowsAffected()
			RaisePgResult(tx.Exec(ctx, move[3]))
			if copied != deleted {
				panic(fmt.Errorf("moved %d rows of %s out of DEFAULT but copied %d", deleted, leaf, copied))
			}
		}
		RaisePgResult(tx.Exec(ctx, attach))
	})
}

// Runs first in a week's drop transaction, after the registry set the guard's
// opt-in: refuses, by raising, a drop that is not the retention task's (one
// without the opt-in, of the legacy or DEFAULT partition, or of a week whose
// upper bound is not 91 days past), and a busy table. Then takes the locks the
// detach needs, parent first, so a lock wait happens before any change.
func fenceCloseEvidenceDrop(ctx context.Context, tx PgTx, r PartitionRange, settings *CloseEvidencePartitionSettings) {
	if r.Legacy || r.Default || r.Lower.IsZero() || r.Upper.IsZero() {
		panic(errors.New("the legacy and DEFAULT partitions of the close evidence are never dropped"))
	}
	var optedIn bool
	var expired bool
	Raise(tx.QueryRow(
		ctx,
		fmt.Sprintf(
			`SELECT coalesce(current_setting('%s', true) = 'on', false), %s + interval '91 days' <= (now() AT TIME ZONE 'UTC')`,
			closeEvidenceRetentionSetting,
			PartitionKeyTimestamp.BoundSql(r.Upper)+"::timestamp",
		),
	).Scan(&optedIn, &expired))
	if !optedIn {
		panic(fmt.Errorf("a close evidence week is dropped only by the retention task, under %s", closeEvidenceRetentionSetting))
	}
	if !expired {
		panic(fmt.Errorf("the close evidence week of %s is not 91 days past", r.Lower.Format(time.DateOnly)))
	}
	requireCloseEvidenceQuiet(ctx, tx, settings)
	RaisePgResult(tx.Exec(ctx, fmt.Sprintf("LOCK TABLE ONLY public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(closeEvidenceTable))))
	RaisePgResult(tx.Exec(ctx, fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(closeEvidenceDefaultLeafName()))))
}

// The reasons a week stays.
const (
	// the week's rows accepted before its late index starts may be younger
	// than 91 days for one more day; it drops the next day, and the row
	// deletes leave it alone
	closeEvidenceBlockerRetentionDay = "within a day of its retention"
	closeEvidenceBlockerNoLateIndex  = "no index of its late reports"
	closeEvidenceBlockerRecentReport = "a report accepted within 91 days"
	closeEvidenceBlockerUnresolved   = "a report of an unresolved contract"
)

// Why the expired week r must stay, or "" when every row in it meets the
// evidence guard's rule (the family's DropPrecondition): accepted more than 91
// days ago, its contract resolved or gone. It reads indexes only:
//   - rows accepted up to a day after the week are older than 91 days once the
//     week is a day past its retention, and the week's late index holds every
//     row accepted after that, so its newest entry decides the rest;
//   - every row is then older than 91 days, and a contract's id is minted
//     before any report of it is accepted, so only an unresolved contract
//     with an id from before then can be one of the week's: those few
//     (transfer_contract_outcome_null) are each looked up in the week's
//     contract index.
//
// It runs before the drop, and again on the detached week in the drop's
// transaction, where no statement can reach the week.
func closeEvidenceWeekBlocker(ctx context.Context, conn PgQuerier, r PartitionRange) string {
	if r.Legacy || r.Default || r.Lower.IsZero() || r.Upper.IsZero() {
		return "the legacy and DEFAULT partitions are never dropped"
	}
	leaf := closeEvidencePartitionFamily.LeafName(closeEvidenceTable, r)
	var blocker string
	Raise(conn.QueryRow(ctx, closeEvidenceWeekBlockerSql(leaf, r)).Scan(&blocker))
	return blocker
}

// The precondition as one statement, so it reads one snapshot. The late index
// is read through a predicate equal to its own, so the planner proves the
// partial index applies and reads its newest entry.
func closeEvidenceWeekBlockerSql(leaf string, r PartitionRange) string {
	lateBound := closeEvidenceLateBoundSql(r)
	return fmt.Sprintf(
		`
		SELECT CASE
			WHEN (now() AT TIME ZONE 'UTC') - interval '91 days' < %[2]s
				THEN '%[3]s'
			WHEN NOT coalesce((SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('public.%[4]s')), false)
				THEN '%[5]s'
			WHEN (now() AT TIME ZONE 'UTC') - interval '91 days' <= (
				SELECT max(late.accepted_at) FROM public.%[1]s late WHERE late.accepted_at >= %[2]s
			)
				THEN '%[6]s'
			WHEN EXISTS (
				SELECT 1
				FROM transfer_contract
				CROSS JOIN LATERAL (
					SELECT 1 FROM public.%[1]s evidence
					WHERE evidence.contract_id = transfer_contract.contract_id
					LIMIT 1
				) reported
				WHERE transfer_contract.outcome IS NULL
					AND transfer_contract.contract_id < contract_id_floor(now() - interval '91 days' + interval '%[7]d seconds')
			)
				THEN '%[8]s'
			ELSE ''
		END
		`,
		quoteIdentifier(leaf),
		lateBound,
		closeEvidenceBlockerRetentionDay,
		closeEvidenceLateIndexName(leaf),
		closeEvidenceBlockerNoLateIndex,
		closeEvidenceBlockerRecentReport,
		int64(closeEvidenceContractClockMargin/time.Second),
		closeEvidenceBlockerUnresolved,
	)
}

// The leaves RemoveExpiredCloseEvidence row-deletes, in order: on the plain
// table, the table; once partitioned, the DEFAULT partition, then each
// expired week that a straggler keeps (its precondition reports a blocker
// other than the day after retention), oldest first, then the legacy
// partition. A week the drop will take is left to it, as are the weeks not
// yet expired.
func closeEvidenceRemovalTargets(ctx context.Context, now time.Time) (targets []string, err error) {
	if !IsCloseEvidencePartitioned(ctx) {
		return []string{closeEvidenceTable}, nil
	}
	family := closeEvidencePartitionFamily
	var leaves []PartitionLeaf
	blockers := map[string]string{}
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			var listErr error
			leaves, listErr = family.ListLeaves(ctx, conn, closeEvidenceTable)
			Raise(listErr)
			for _, leaf := range leaves {
				if leaf.Canonical && !leaf.Range.Legacy && !leaf.Range.Default && family.IsExpired(ctx, now, leaf.Range) {
					blockers[leaf.Name] = closeEvidenceWeekBlocker(ctx, conn, leaf.Range)
				}
			}
		})
	})
	if err != nil {
		return nil, err
	}
	return closeEvidenceRemovalOrder(leaves, blockers), nil
}

// The removal targets of leaves, given the blockers of the expired weeks (see
// closeEvidenceRemovalTargets).
func closeEvidenceRemovalOrder(leaves []PartitionLeaf, blockers map[string]string) (targets []string) {
	weeks := []PartitionLeaf{}
	legacy := ""
	for _, leaf := range leaves {
		switch {
		case leaf.Range.Default:
			targets = append(targets, leaf.Name)
		case leaf.Range.Legacy:
			legacy = leaf.Name
		default:
			if blocker, expired := blockers[leaf.Name]; expired && blocker != "" && blocker != closeEvidenceBlockerRetentionDay {
				weeks = append(weeks, leaf)
			}
		}
	}
	slices.SortStableFunc(weeks, func(a PartitionLeaf, b PartitionLeaf) int {
		return a.Range.Lower.Compare(b.Range.Lower)
	})
	for _, week := range weeks {
		targets = append(targets, week.Name)
	}
	if legacy != "" {
		targets = append(targets, legacy)
	}
	return
}

// Returned by the epoch census while a prepared cutover holds moved rows
// outside the evidence table.
var ErrCloseEvidenceCutoverMoving = errors.New("close evidence rows are staged for the partition cutover; the census waits for the swap")

// Whether a prepared cutover holds rows moved out of the evidence table, in
// tx's snapshot, which must be the snapshot of every read that follows
// (REPEATABLE READ). Until its swap, readers of the table do not see the moved
// rows, so a census read in that snapshot would miss them. The staged parent is
// found through pg_class, which the snapshot reads like any table. A name is
// resolved against the current catalog, so a staged parent that the swap or
// an abort has removed since the snapshot counts as moving rather than being
// read.
func CloseEvidenceMovedOutsideTableInTx(ctx context.Context, tx PgTx) (moved bool, err error) {
	var staged bool
	var current bool
	err = tx.QueryRow(
		ctx,
		`
		SELECT
			EXISTS (
				SELECT 1 FROM pg_class staged
				JOIN pg_namespace namespace ON namespace.oid = staged.relnamespace
				WHERE namespace.nspname = 'public' AND staged.relname = $1
			),
			to_regclass('public.' || $1) IS NOT NULL
		`,
		closeEvidenceStagedParentName(),
	).Scan(&staged, &current)
	if err != nil || !staged {
		return false, err
	}
	if !current {
		return true, nil
	}
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM public.%s)`, quoteIdentifier(closeEvidenceStagedParentName()))).Scan(&moved)
	return
}
