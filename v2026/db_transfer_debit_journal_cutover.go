package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The zero-copy cutover of transfer_debit_journal to daily contract_id
// partitions (PARTITION-CANDIDATES.md §6.3), an owner-run command:
// bringyourctl model migrate debit-journal-partition. It reuses the contract
// cutover's pieces (db_contract_partition_cutover.go) as the close evidence
// cutover does (db_contract_close_evidence_cutover.go): the staged parent
// cloned with LIKE ... INCLUDING INDEXES, the rolled-back attach probe, a
// validated bound check before the attach, quiet-table lock admission with
// short lock waits and retries, and resumable steps. The table becomes the
// legacy partition, transfer_debit_journal_legacy, of every contract id below
// the cutover day C.
//
//   - prepare (default, resumable; completed steps are skipped):
//     1. stage the empty parent transfer_debit_journal_p, PARTITION BY RANGE
//     (contract_id), with every index and check of the table;
//     2. attach its DEFAULT partition, transfer_debit_journal_default;
//     3. stage the days from C through TransferDebitJournalAheadDays after the
//     later of C and today;
//     4. add the check contract_id < contract_id_floor(C), NOT VALID: one
//     brief ACCESS EXCLUSIVE lock, without a scan;
//     5. validate it. The scan runs under SHARE UPDATE EXCLUSIVE on a session
//     exempt from transaction_timeout, so settlement continues. A debit of a
//     contract minted at or after C by a clock hours ahead fails the
//     validation, and prepare can simply run again once the worker has
//     deleted it.
//   - finalize: probe the attach, then one short transaction swaps the names
//     and attaches the table FROM (MINVALUE) TO (contract_id_floor(C)): catalog
//     work only, since the validated check proves the bound and every index
//     attaches without a build. Then the parent is analyzed and the days
//     ahead created.
//   - rollback (the rollback point, until the legacy partition retires): copy
//     the rows outside the legacy partition back into it and make it the plain
//     table again, under the parent's lock.
//   - abort: before finalize, drop the staged parent and the check.
//
// From step 4 until the swap the check refuses a debit of a contract minted
// at or after C. Contracts are minted with their server's clock, so that is
// none while C is at least FinalizeMargin away; the runway pass abandons a
// prepare still unfinalized AbandonMargin before C
// (abandonStaleTransferDebitJournalCutover), before its check could refuse a
// debit.
type TransferDebitJournalPartitionMigrateOptions struct {
	// the day, 00:00 UTC, from which new contracts' debits go to daily
	// partitions (prepare only)
	CutoverDay time.Time
	DryRun     bool
	Finalize   bool
	Rollback   bool
	Abort      bool
	Logf       func(string, ...any)
	// nil uses DefaultTransferDebitJournalPartitionSettings.
	Settings *TransferDebitJournalPartitionSettings
}

func MigrateTransferDebitJournalToPartitions(ctx context.Context, options TransferDebitJournalPartitionMigrateOptions) error {
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	settings := options.Settings
	if settings == nil {
		settings = DefaultTransferDebitJournalPartitionSettings()
	}
	actions := 0
	for _, action := range []bool{options.Finalize, options.Rollback, options.Abort} {
		if action {
			actions += 1
		}
	}
	if 1 < actions {
		return errors.New("choose one of finalize, rollback and abort")
	}
	switch {
	case options.Finalize:
		return finalizeTransferDebitJournalCutover(ctx, true, options.DryRun, logf, settings)
	case options.Rollback:
		return rollbackTransferDebitJournalCutover(ctx, options.DryRun, logf, settings)
	case options.Abort:
		return abortTransferDebitJournalCutover(ctx, options.DryRun, logf, settings)
	default:
		if options.CutoverDay.IsZero() {
			return errors.New("the cutover day is required to prepare")
		}
		return prepareTransferDebitJournalCutover(ctx, options.CutoverDay, true, options.DryRun, logf, settings)
	}
}

var transferDebitJournalLegacyBoundPattern = regexp.MustCompile(`^CHECK \(\(contract_id < '([0-9a-f-]{36})'::uuid\)\)( NOT VALID)?$`)

// The cutover day of a prepared bound check.
func transferDebitJournalLegacyBoundDay(bound *contractConstraint) (cutoverDay time.Time, err error) {
	groups := transferDebitJournalLegacyBoundPattern.FindStringSubmatch(bound.definition)
	if groups == nil {
		return time.Time{}, fmt.Errorf("%s has an unexpected definition: %s", bound.name, bound.definition)
	}
	floor, err := ParseId(groups[1])
	if err != nil {
		return time.Time{}, err
	}
	cutoverDay = ContractIdTime(floor)
	if !PartitionGranularityDaily.RangeStart(cutoverDay).Equal(cutoverDay) || ContractIdFloor(cutoverDay) != floor {
		return time.Time{}, fmt.Errorf("%s does not bound a day: %s", bound.name, bound.definition)
	}
	return cutoverDay, nil
}

// The published primary key, which holds the partition key.
const transferDebitJournalPrimaryKeyDefinition = "PRIMARY KEY (balance_id, contract_id)"

// Reasons the table cannot become the legacy partition as it is.
func transferDebitJournalCutoverProblems(shape *contractTableShape) (problems []string) {
	if shape == nil {
		return []string{fmt.Sprintf("%s does not exist", transferDebitJournalTable)}
	}
	if shape.relkind != "r" {
		problems = append(problems, fmt.Sprintf("%s is not a plain table (relkind %s)", shape.table, shape.relkind))
	}
	key := slices.IndexFunc(shape.columns, func(column contractColumn) bool {
		return column.name == "contract_id"
	})
	if key < 0 || shape.columns[key].typeName != "uuid" || !shape.columns[key].notNull {
		problems = append(problems, fmt.Sprintf("%s.contract_id is not a NOT NULL uuid", shape.table))
	}
	for _, check := range shape.checks {
		if check.noInherit {
			problems = append(problems, fmt.Sprintf("%s check %s is NO INHERIT", shape.table, check.name))
		}
		// LIKE copies each check as valid, and a valid parent check cannot
		// adopt a NOT VALID one at the swap
		if !check.validated {
			problems = append(problems, fmt.Sprintf("%s check %s is NOT VALID; validate it before the cutover", shape.table, check.name))
		}
	}
	primary := 0
	for _, index := range shape.indexes {
		if !index.valid || !index.ready {
			problems = append(problems, fmt.Sprintf("%s index %s is invalid; drop it (DROP INDEX CONCURRENTLY) or rebuild it before the cutover", shape.table, index.name))
			continue
		}
		if pgMaxIdentifierLength < len(index.name)+len(contractStagedIndexSuffix) || pgMaxIdentifierLength < len(index.name)+len(contractLegacyIndexSuffix) {
			problems = append(problems, fmt.Sprintf("%s index name %s is too long to stage", shape.table, index.name))
		}
		switch {
		case index.primary:
			primary += 1
			if index.constraintDefinition != transferDebitJournalPrimaryKeyDefinition {
				problems = append(problems, fmt.Sprintf("%s primary key is %s, not %s", shape.table, index.constraintDefinition, transferDebitJournalPrimaryKeyDefinition))
			}
		case index.unique:
			// a partitioned table enforces a unique index only through its
			// partition key, and nothing relies on another
			problems = append(problems, fmt.Sprintf("%s unique index %s is not the primary key", shape.table, index.name))
		default:
			if _, ok := contractRetargetIndex(index.definition, index.name, shape.table); !ok {
				problems = append(problems, fmt.Sprintf("%s index %s has an unexpected definition", shape.table, index.name))
			}
		}
	}
	if primary != 1 {
		problems = append(problems, fmt.Sprintf("%s has %d primary keys", shape.table, primary))
	}
	// the journal has none; the staged parent would carry none
	for _, trigger := range shape.triggers {
		problems = append(problems, fmt.Sprintf("%s has trigger %s, which the cutover does not carry", shape.table, trigger.name))
	}
	for _, foreignKey := range append(slices.Clone(shape.foreignKeys), shape.referencedBy...) {
		problems = append(problems, fmt.Sprintf("%s has foreign key %s of %s, which the cutover does not carry", shape.table, foreignKey.name, foreignKey.table))
	}
	return
}

// Differences between the staged parent and the table it will replace,
// beyond the columns and checks: it must be partitioned by contract_id.
func transferDebitJournalStagedProblems(ctx context.Context, shape *contractTableShape, staged *contractTableShape) (problems []string) {
	problems = contractStagedProblems(shape, staged)
	if 0 < len(problems) {
		return
	}
	var partitionKey string
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `SELECT coalesce(pg_get_partkeydef(to_regclass($1)), '')`, "public."+staged.table).Scan(&partitionKey))
	})
	if partitionKey != "RANGE (contract_id)" {
		problems = append(problems, fmt.Sprintf("%s is partitioned by %q, not RANGE (contract_id); abort and prepare again", staged.table, partitionKey))
	}
	return
}

// The staged parent: the table's columns, defaults, storage, checks and every
// index. LIKE ... INCLUDING INDEXES clones each index's stored expression
// tree, so the swap's attach adopts the table's own, and INCLUDING
// CONSTRAINTS does the same for each check, copied as valid
// (transferDebitJournalCutoverProblems). The prepared bound bounds only the
// table, so it is dropped from the copy.
func transferDebitJournalStagedParentSql(shape *contractTableShape) (statements []string) {
	staged := transferDebitJournalStagedParentName()
	statements = append(statements, fmt.Sprintf(
		"CREATE TABLE public.%s (LIKE public.%s INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING INDEXES INCLUDING STORAGE INCLUDING COMPRESSION INCLUDING COMMENTS) PARTITION BY RANGE (contract_id)",
		quoteIdentifier(staged),
		quoteIdentifier(shape.table),
	))
	if shape.legacyBound != nil {
		statements = append(statements, fmt.Sprintf(
			"ALTER TABLE public.%s DROP CONSTRAINT %s",
			quoteIdentifier(staged),
			quoteIdentifier(shape.legacyBound.name),
		))
	}
	for _, column := range shape.columns {
		if 0 <= column.statisticsTarget {
			statements = append(statements, fmt.Sprintf(
				"ALTER TABLE public.%s ALTER COLUMN %s SET STATISTICS %d",
				quoteIdentifier(staged),
				quoteIdentifier(column.name),
				column.statisticsTarget,
			))
		}
	}
	return
}

// The statements that create leaf under the staged parent with bounds, as
// the registry's runway creates one under the published parent: LIKE the
// parent, the family's storage settings, then the attach.
func transferDebitJournalStagedLeafSql(leaf string, bounds string) []string {
	staged := transferDebitJournalStagedParentName()
	return []string{
		fmt.Sprintf(
			"CREATE TABLE public.%s (LIKE public.%s INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING STORAGE INCLUDING COMPRESSION)",
			quoteIdentifier(leaf),
			quoteIdentifier(staged),
		),
		fmt.Sprintf("ALTER TABLE public.%s SET (%s)", quoteIdentifier(leaf), strings.Join(transferDebitJournalLeafReloptions, ", ")),
		fmt.Sprintf("ALTER TABLE public.%s ATTACH PARTITION public.%s %s", quoteIdentifier(staged), quoteIdentifier(leaf), bounds),
	}
}

// The days prepare stages: from the cutover day through AheadDays after the
// later of it and today, so a cutover day already past (only a test makes
// one) still covers today.
func transferDebitJournalStagedDays(cutoverDay time.Time, now time.Time) (days []PartitionRange) {
	last := PartitionGranularityDaily.RangeStart(now)
	if last.Before(cutoverDay) {
		last = cutoverDay
	}
	last = PartitionGranularityDaily.AddRanges(last, TransferDebitJournalAheadDays)
	for day := cutoverDay; !last.Before(day); day = PartitionGranularityDaily.NextRangeStart(day) {
		days = append(days, PartitionGranularityDaily.RangeOf(day))
	}
	return
}

// Runs the test hook of a cutover step, if any.
func (self *TransferDebitJournalPartitionSettings) beforeCutoverStep(step string) error {
	if self.beforeCutoverStepForTest != nil {
		return self.beforeCutoverStepForTest(step)
	}
	return nil
}

// Repeats run while the journal is busy: each attempt first waits out long
// transactions on its relations (so a lock request never queues settlement
// behind one), then runs, and a lock wait that timed out or lost a deadlock is
// retried after the interval.
func transferDebitJournalRetry(ctx context.Context, settings *TransferDebitJournalPartitionSettings, attempts int, quiet bool, run func() error, logf func(string, ...any)) (err error) {
	for attempt := 1; attempt <= attempts; attempt += 1 {
		busy := 0
		if quiet {
			if busyErr := partitionRegistryRun(func() {
				MaintenanceDb(ctx, func(conn PgConn) {
					busy = partitionRelationsBusy(ctx, conn, transferDebitJournalLockRelations(), settings.MaxTransactionAge)
				})
			}); busyErr != nil {
				return busyErr
			}
		}
		if 0 < busy {
			logf("%d transaction(s) older than %s hold transfer debit journal locks (attempt %d/%d)", busy, settings.MaxTransactionAge, attempt, attempts)
			err = fmt.Errorf("%d long transaction(s) hold transfer debit journal locks", busy)
		} else {
			err = run()
			if err == nil || !contractPartitionLockBusy(err) {
				return
			}
			logf("lock busy (attempt %d/%d): %v", attempt, attempts, err)
		}
		if attempt < attempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(settings.RetryInterval):
			}
		}
	}
	return
}

// The command runs from an operator's machine, about 90 ms from Main, so each
// step that takes an exclusive journal lock is one script sent in one
// message, BEGIN to COMMIT (transferDebitJournalScript): its lock is held for
// the database's own work and no network round trip. What the script's
// statements were computed from is read before the lock and asserted
// unchanged under it, in SQL (transferDebitJournalFingerprintSql).

// Where a script marks the moment it holds its lock, and the rows its
// rollback copied.
const transferDebitJournalLockedAtSetting = "urnetwork.debit_journal_locked_at"
const transferDebitJournalCopiedSetting = "urnetwork.debit_journal_copied"

const transferDebitJournalMarkLockedSql = "SELECT set_config('" + transferDebitJournalLockedAtSetting + "', clock_timestamp()::text, true)"

// How long the script has held its lock, in seconds, by the database's clock.
const transferDebitJournalHeldExpression = "extract(epoch FROM clock_timestamp() - current_setting('" + transferDebitJournalLockedAtSetting + "')::timestamptz)::float8"

func transferDebitJournalLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// The bounds every script starts with.
func transferDebitJournalScriptBounds(settings *TransferDebitJournalPartitionSettings) []string {
	return []string{
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.LockTimeout.Milliseconds()),
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.StatementTimeout.Milliseconds()),
	}
}

// Raises message unless condition holds, as one statement of a script.
func transferDebitJournalAssertSql(condition string, message string) string {
	return fmt.Sprintf(
		"DO $debit_journal$ BEGIN IF NOT coalesce(%s, false) THEN RAISE EXCEPTION '%%', %s; END IF; END $debit_journal$",
		condition,
		transferDebitJournalLiteral(message),
	)
}

// A text fingerprint of the relations a script's statements were computed
// from: each one's kind, constraints, index names and partitions with their
// bounds. A relation that is gone reads as "-".
func transferDebitJournalFingerprintSql(relations ...string) string {
	parts := []string{}
	for _, relation := range relations {
		regclass := fmt.Sprintf("to_regclass(%s)", transferDebitJournalLiteral("public."+quoteIdentifier(relation)))
		parts = append(
			parts,
			fmt.Sprintf(`coalesce((SELECT relkind::text FROM pg_class WHERE oid = %s), '-')`, regclass),
			fmt.Sprintf(`coalesce((SELECT string_agg(conname::text || ' ' || convalidated::text || ' ' || pg_get_constraintdef(oid), ', ' ORDER BY conname::text COLLATE "C") FROM pg_constraint WHERE conrelid = %s), '')`, regclass),
			fmt.Sprintf(`coalesce((SELECT string_agg(index_class.relname::text, ', ' ORDER BY index_class.relname::text COLLATE "C") FROM pg_index JOIN pg_class index_class ON index_class.oid = pg_index.indexrelid WHERE pg_index.indrelid = %s), '')`, regclass),
			fmt.Sprintf(`coalesce((SELECT string_agg(child.relname::text || ' ' || pg_get_expr(child.relpartbound, child.oid), ', ' ORDER BY child.relname::text COLLATE "C") FROM pg_inherits JOIN pg_class child ON child.oid = pg_inherits.inhrelid WHERE pg_inherits.inhparent = %s), '')`, regclass),
		)
	}
	return "concat_ws(' | ', " + strings.Join(parts, ", ") + ")"
}

// The assertion, under a script's lock, that its relations still read as
// fingerprint did before the lock.
func transferDebitJournalUnchangedSql(fingerprint string, message string, relations ...string) string {
	return transferDebitJournalAssertSql(
		fmt.Sprintf("%s = %s", transferDebitJournalFingerprintSql(relations...), transferDebitJournalLiteral(fingerprint)),
		message,
	)
}

// A snapshot read's expressions (pg_get_expr) take ACCESS SHARE on their
// relation, which waits behind an exclusive lock; the read gives up after
// this, and the step retries.
const transferDebitJournalReadLockTimeout = 5 * time.Second

// Reads the relations' shapes and their fingerprint in one repeatable read
// snapshot, so the statements computed from the shapes fit the fingerprint.
func readTransferDebitJournalShapes(ctx context.Context, relations ...string) (shapes map[string]*contractTableShape, fingerprint string, err error) {
	shapes = map[string]*contractTableShape{}
	err = partitionRegistryRun(func() {
		MaintenanceTx(ctx, func(tx PgTx) {
			for _, relation := range relations {
				shapes[relation] = readContractTableShape(ctx, tx, relation)
			}
			Raise(tx.QueryRow(ctx, "SELECT "+transferDebitJournalFingerprintSql(relations...)).Scan(&fingerprint))
		}, TxRepeatableRead, TxLockTimeout(transferDebitJournalReadLockTimeout), OptNoRetry())
	})
	return
}

// Runs statements as one script in one message on a maintenance session,
// between BEGIN and COMMIT, and returns the values of its last one-row
// result. A failed script is rolled back before the session goes back, or the
// session is discarded.
func transferDebitJournalScript(ctx context.Context, statements []string) (values []string, err error) {
	conn, err := AcquireMaintenanceDbConn(ctx)
	if err != nil {
		return
	}
	committed := false
	defer func() {
		if committed {
			conn.Release()
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, rollbackErr := conn.Exec(rollbackCtx, "ROLLBACK"); rollbackErr != nil {
			discardPgConnection(rollbackCtx, conn)
			return
		}
		conn.Release()
	}()
	script := strings.Join(append(append([]string{"BEGIN"}, statements...), "COMMIT"), ";\n")
	results, err := conn.Conn().PgConn().Exec(ctx, script).ReadAll()
	if err != nil {
		return nil, err
	}
	for _, result := range results {
		if result.Err != nil {
			return nil, result.Err
		}
		if 0 < len(result.FieldDescriptions) && len(result.Rows) == 1 {
			values = values[:0]
			for _, value := range result.Rows[0] {
				values = append(values, string(value))
			}
		}
	}
	committed = true
	return
}

// The script's report value at index, in seconds, as a duration.
func transferDebitJournalReportSeconds(values []string, index int) (time.Duration, error) {
	if len(values) <= index {
		return 0, fmt.Errorf("the script reported %d values", len(values))
	}
	seconds, err := strconv.ParseFloat(values[index], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// Whether the staged parent's DEFAULT partition is attached.
func transferDebitJournalStagedDefaultAttached(ctx context.Context, conn PgQuerier) (attached bool) {
	Raise(conn.QueryRow(
		ctx,
		`
		SELECT EXISTS (
			SELECT 1 FROM pg_inherits inheritance
			JOIN pg_class child ON child.oid = inheritance.inhrelid
			WHERE inheritance.inhparent = to_regclass($1)
				AND child.relname = $2
				AND pg_get_expr(child.relpartbound, child.oid) = 'DEFAULT'
		)
		`,
		"public."+transferDebitJournalStagedParentName(),
		transferDebitJournalDefaultLeafName(),
	).Scan(&attached))
	return
}

// An operator's new prepare needs the cutover day PrepareLeadTime ahead; one
// that resumes a prepare of the same day does not. A test cuts over at a day
// already past.
func prepareTransferDebitJournalCutover(ctx context.Context, cutoverDay time.Time, operator bool, dryRun bool, logf func(string, ...any), settings *TransferDebitJournalPartitionSettings) error {
	if IsTransferDebitJournalPartitioned(ctx) {
		return errors.New("the transfer debit journal is already partitioned")
	}
	cutoverDay = cutoverDay.UTC()
	if !PartitionGranularityDaily.RangeStart(cutoverDay).Equal(cutoverDay) {
		return fmt.Errorf("the cutover day must start at 00:00 UTC: %s", cutoverDay)
	}
	floor := ContractIdFloor(cutoverDay)
	if err := checkContractIdFloorFunction(ctx, cutoverDay, floor); err != nil {
		return err
	}
	var guardExists bool
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `SELECT to_regprocedure('public.transfer_debit_journal_drop_guard(text)') IS NOT NULL`).Scan(&guardExists))
	})
	if !guardExists {
		return errors.New("transfer_debit_journal_drop_guard() is missing; apply the database migrations first")
	}
	logf("cutover day %s: debits of contract ids below %s stay in %s", cutoverDay.Format(time.DateOnly), floor, transferDebitJournalLegacyLeafName())
	logf("finalize must complete before %s; an unfinished prepare is then abandoned", cutoverDay.Add(-settings.AbandonMargin).Format(time.RFC3339))

	var shape *contractTableShape
	var staged *contractTableShape
	MaintenanceDb(ctx, func(conn PgConn) {
		shape = readContractTableShape(ctx, conn, transferDebitJournalTable)
		staged = readContractTableShape(ctx, conn, transferDebitJournalStagedParentName())
	})
	problems := transferDebitJournalCutoverProblems(shape)
	if shape != nil && shape.legacyBound != nil {
		if day, err := transferDebitJournalLegacyBoundDay(shape.legacyBound); err != nil {
			problems = append(problems, err.Error())
		} else if !day.Equal(cutoverDay) {
			problems = append(problems, fmt.Sprintf("%s is prepared for the cutover day %s; abort it first", transferDebitJournalTable, day.Format(time.DateOnly)))
		}
	}
	if operator && shape != nil && shape.legacyBound == nil && cutoverDay.Before(NowUtc().Add(settings.PrepareLeadTime)) {
		problems = append(problems, fmt.Sprintf("the cutover day %s must start at least %s from now, to leave time for the validation and the swap", cutoverDay.Format(time.DateOnly), settings.PrepareLeadTime))
	}
	if shape != nil && staged != nil {
		problems = append(problems, transferDebitJournalStagedProblems(ctx, shape, staged)...)
	}
	if 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the cutover", len(problems))
	}

	stagedName := transferDebitJournalStagedParentName()
	if err := settings.beforeCutoverStep("stage"); err != nil {
		return err
	}
	if staged == nil {
		statements := transferDebitJournalStagedParentSql(shape)
		if dryRun {
			logf("would stage %s, then give its indexes the table's names plus %q:\n%s", stagedName, contractStagedIndexSuffix, strings.Join(statements, ";\n"))
		} else {
			err := contractPartitionTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
				for _, statement := range statements {
					RaisePgResult(tx.Exec(ctx, statement))
				}
				renames, err := contractStagedRenameSql(shape, readContractTableShape(ctx, tx, stagedName))
				Raise(err)
				for _, statement := range renames {
					RaisePgResult(tx.Exec(ctx, statement))
				}
			})
			if err != nil {
				return fmt.Errorf("stage %s: %w", stagedName, err)
			}
			logf("staged %s with %d indexes", stagedName, len(shape.indexes))
		}
	}

	if err := settings.beforeCutoverStep("default"); err != nil {
		return err
	}
	var defaultAttached bool
	if !dryRun || staged != nil {
		MaintenanceDb(ctx, func(conn PgConn) {
			defaultAttached = transferDebitJournalStagedDefaultAttached(ctx, conn)
		})
	}
	if !defaultAttached {
		statements := transferDebitJournalStagedLeafSql(transferDebitJournalDefaultLeafName(), "DEFAULT")
		if dryRun {
			logf("would attach %s:\n%s", transferDebitJournalDefaultLeafName(), strings.Join(statements, ";\n"))
		} else {
			if err := contractPartitionExec(ctx, append([]string{`SET LOCAL lock_timeout = '5s'`}, statements...)); err != nil {
				return fmt.Errorf("attach %s: %w", transferDebitJournalDefaultLeafName(), err)
			}
			logf("attached %s to %s", transferDebitJournalDefaultLeafName(), stagedName)
		}
	}

	if err := settings.beforeCutoverStep("days"); err != nil {
		return err
	}
	var leaves []PartitionLeaf
	if !dryRun || staged != nil {
		var listErr error
		MaintenanceDb(ctx, func(conn PgConn) {
			leaves, listErr = transferDebitJournalPartitionFamily.ListLeaves(ctx, conn, stagedName)
		})
		if listErr != nil {
			return listErr
		}
	}
	for _, day := range transferDebitJournalStagedDays(cutoverDay, NowUtc()) {
		if slices.ContainsFunc(leaves, func(leaf PartitionLeaf) bool { return leaf.Range.Overlaps(day) }) {
			continue
		}
		name := transferDebitJournalPartitionFamily.LeafName(transferDebitJournalTable, day)
		statements := transferDebitJournalStagedLeafSql(name, fmt.Sprintf(
			"FOR VALUES FROM (%s) TO (%s)",
			PartitionKeyId.BoundSql(day.Lower),
			PartitionKeyId.BoundSql(day.Upper),
		))
		if dryRun {
			logf("would create %s:\n%s", name, strings.Join(statements, ";\n"))
			continue
		}
		if err := contractPartitionExec(ctx, append([]string{`SET LOCAL lock_timeout = '5s'`}, statements...)); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		logf("created %s", name)
	}

	if err := settings.beforeCutoverStep("bound"); err != nil {
		return err
	}
	bound := contractLegacyBoundName(transferDebitJournalTable)
	if shape.legacyBound == nil {
		statement := fmt.Sprintf(
			"ALTER TABLE public.%s ADD CONSTRAINT %s CHECK (contract_id < %s) NOT VALID",
			quoteIdentifier(transferDebitJournalTable),
			quoteIdentifier(bound),
			contractIdLiteral(floor),
		)
		// a brief ACCESS EXCLUSIVE lock, without a scan, in one message
		script := append(
			transferDebitJournalScriptBounds(settings),
			fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(transferDebitJournalTable)),
			transferDebitJournalMarkLockedSql,
			statement,
			"SELECT "+transferDebitJournalHeldExpression,
		)
		if dryRun {
			logf("would add %s in one message:\n%s", bound, strings.Join(script, ";\n"))
		} else {
			var held time.Duration
			err := transferDebitJournalRetry(ctx, settings, settings.MaxAttempts, true, func() error {
				values, scriptErr := transferDebitJournalScript(ctx, script)
				if scriptErr != nil {
					return scriptErr
				}
				held, scriptErr = transferDebitJournalReportSeconds(values, 0)
				return scriptErr
			}, logf)
			if err != nil {
				return fmt.Errorf("add %s: %w", bound, err)
			}
			logf("added %s; its lock was held %s", bound, held.Round(time.Microsecond))
		}
	}
	if shape.legacyBound == nil || !shape.legacyBound.validated {
		if err := settings.beforeCutoverStep("validate"); err != nil {
			return err
		}
		if dryRun {
			logf("would validate %s (scans %s under SHARE UPDATE EXCLUSIVE)", bound, transferDebitJournalTable)
		} else {
			logf("validating %s: scans %s under SHARE UPDATE EXCLUSIVE; settlement continues", bound, transferDebitJournalTable)
			validateStart := time.Now()
			// SHARE UPDATE EXCLUSIVE never blocks settlement, so the step does
			// not wait for a quiet table
			err := transferDebitJournalRetry(ctx, settings, settings.MaxAttempts, false, func() error {
				return validateTransferDebitJournalLegacyBound(ctx, bound, settings)
			}, logf)
			if err != nil {
				return fmt.Errorf("validate %s: %w", bound, err)
			}
			logf("validated %s in %s", bound, time.Since(validateStart).Round(time.Second))
		}
	}
	if dryRun {
		logf("dry run: nothing changed")
	} else {
		logf("prepared; run --finalize before %s", cutoverDay.Add(-settings.AbandonMargin).Format(time.RFC3339))
	}
	return nil
}

// Validates the bound check on a maintenance session exempt from
// transaction_timeout, with no statement bound: it scans the whole table.
func validateTransferDebitJournalLegacyBound(ctx context.Context, bound string, settings *TransferDebitJournalPartitionSettings) error {
	return partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			WithoutTransactionTimeout(ctx, conn, func() {
				TxInConn(ctx, conn, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.ValidateLockTimeout.Milliseconds())))
					RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = 0`))
					RaisePgResult(tx.Exec(ctx, fmt.Sprintf("ALTER TABLE public.%s VALIDATE CONSTRAINT %s", quoteIdentifier(transferDebitJournalTable), quoteIdentifier(bound))))
				}, TxReadCommitted, OptNoRetry())
			})
		}, OptNoRetry())
	})
}

// The swap, after its lock: the table and its indexes move to the legacy
// names, the table attaches below floor, and the staged parent and its
// indexes take the published names.
func transferDebitJournalSwapSql(shape *contractTableShape, staged *contractTableShape, floor Id) (statements []string) {
	table := transferDebitJournalTable
	stagedName := transferDebitJournalStagedParentName()
	legacy := transferDebitJournalLegacyLeafName()
	statements = append(statements, fmt.Sprintf("ALTER TABLE public.%s RENAME TO %s", quoteIdentifier(table), quoteIdentifier(legacy)))
	for _, index := range shape.indexes {
		statements = append(statements, fmt.Sprintf(
			"ALTER INDEX public.%s RENAME TO %s",
			quoteIdentifier(index.name),
			quoteIdentifier(index.name+contractLegacyIndexSuffix),
		))
	}
	statements = append(statements, fmt.Sprintf(
		"ALTER TABLE public.%s ATTACH PARTITION public.%s FOR VALUES FROM (MINVALUE) TO (%s)",
		quoteIdentifier(stagedName),
		quoteIdentifier(legacy),
		contractIdLiteral(floor),
	))
	statements = append(statements, fmt.Sprintf("ALTER TABLE public.%s RENAME TO %s", quoteIdentifier(stagedName), quoteIdentifier(table)))
	for _, index := range staged.indexes {
		name, _ := strings.CutSuffix(index.name, contractStagedIndexSuffix)
		statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(index.name), quoteIdentifier(name)))
	}
	return
}

// The lock the swap takes first: the table and the staged parent with its
// partitions, so the renames and the attach see no concurrent statement.
func transferDebitJournalSwapLockSql() string {
	return fmt.Sprintf(
		"LOCK TABLE public.%s, public.%s IN ACCESS EXCLUSIVE MODE",
		quoteIdentifier(transferDebitJournalTable),
		quoteIdentifier(transferDebitJournalStagedParentName()),
	)
}

func finalizeTransferDebitJournalCutover(ctx context.Context, deadline bool, dryRun bool, logf func(string, ...any), settings *TransferDebitJournalPartitionSettings) error {
	if IsTransferDebitJournalPartitioned(ctx) {
		logf("the transfer debit journal is already partitioned")
		if dryRun {
			return nil
		}
		// a swap that committed before its analyze and runway ran
		return completeTransferDebitJournalCutover(ctx, logf)
	}
	var shape *contractTableShape
	var staged *contractTableShape
	var defaultAttached bool
	var leaves []PartitionLeaf
	var listErr error
	MaintenanceDb(ctx, func(conn PgConn) {
		shape = readContractTableShape(ctx, conn, transferDebitJournalTable)
		staged = readContractTableShape(ctx, conn, transferDebitJournalStagedParentName())
		if staged != nil {
			defaultAttached = transferDebitJournalStagedDefaultAttached(ctx, conn)
			leaves, listErr = transferDebitJournalPartitionFamily.ListLeaves(ctx, conn, transferDebitJournalStagedParentName())
		}
	})
	problems := transferDebitJournalCutoverProblems(shape)
	var cutoverDay time.Time
	var syncStatements []string
	if shape != nil {
		if shape.legacyBound == nil || !shape.legacyBound.validated {
			problems = append(problems, fmt.Sprintf("%s is not prepared: its bound is missing or not validated", transferDebitJournalTable))
		} else if day, err := transferDebitJournalLegacyBoundDay(shape.legacyBound); err != nil {
			problems = append(problems, err.Error())
		} else {
			cutoverDay = day
		}
		switch {
		case staged == nil:
			problems = append(problems, fmt.Sprintf("%s is not staged", transferDebitJournalStagedParentName()))
		case listErr != nil:
			problems = append(problems, listErr.Error())
		default:
			problems = append(problems, transferDebitJournalStagedProblems(ctx, shape, staged)...)
			statements, indexProblems := contractStagedIndexSyncSql(shape, staged)
			problems = append(problems, indexProblems...)
			syncStatements = statements
			if !defaultAttached {
				problems = append(problems, fmt.Sprintf("%s has no DEFAULT partition", transferDebitJournalStagedParentName()))
			}
			if !cutoverDay.IsZero() && !slices.ContainsFunc(leaves, func(leaf PartitionLeaf) bool {
				return leaf.Range.Covers(PartitionGranularityDaily.RangeOf(cutoverDay))
			}) {
				problems = append(problems, fmt.Sprintf("%s has no partition for the cutover day", transferDebitJournalStagedParentName()))
			}
		}
	}
	if 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the swap", len(problems))
	}
	if deadline && !NowUtc().Before(cutoverDay.Add(-settings.FinalizeMargin)) {
		return fmt.Errorf("too close to the cutover day %s; abort and prepare a later day", cutoverDay.Format(time.DateOnly))
	}

	if 0 < len(syncStatements) {
		if dryRun {
			logf("would sync staged indexes:\n%s", strings.Join(syncStatements, ";\n"))
		} else {
			if err := contractPartitionExec(ctx, append([]string{`SET LOCAL lock_timeout = '5s'`}, syncStatements...)); err != nil {
				return fmt.Errorf("sync staged indexes: %w", err)
			}
			logf("synced %d staged index change(s)", len(syncStatements))
			MaintenanceDb(ctx, func(conn PgConn) {
				staged = readContractTableShape(ctx, conn, transferDebitJournalStagedParentName())
			})
		}
	}

	if err := settings.beforeCutoverStep("probe"); err != nil {
		return err
	}
	floor := ContractIdFloor(cutoverDay)
	if problems := probeContractLegacyAttach(ctx, transferDebitJournalTable, floor); 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the swap", len(problems))
	}
	logf("probed the swap: every parent index adopts an index of %s", transferDebitJournalTable)

	if dryRun {
		_, fingerprint, err := readTransferDebitJournalShapes(ctx, transferDebitJournalTable, transferDebitJournalStagedParentName())
		if err != nil {
			return err
		}
		statements := transferDebitJournalSwapScript(shape, staged, fingerprint, cutoverDay, settings)
		logf("would send the swap in one message, waiting at most %s for the lock:\n%s", settings.LockTimeout, strings.Join(statements, ";\n"))
		logf("dry run: nothing changed")
		return nil
	}
	if err := settings.beforeCutoverStep("swap"); err != nil {
		return err
	}
	var held time.Duration
	err := transferDebitJournalRetry(ctx, settings, settings.MaxAttempts, true, func() (swapErr error) {
		held, swapErr = swapTransferDebitJournal(ctx, cutoverDay, settings)
		return
	}, logf)
	if err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	logf("swapped %s; its locks were held %s; debits of contracts from %s go to daily partitions", transferDebitJournalTable, held.Round(time.Microsecond), cutoverDay.Format(time.DateOnly))
	return completeTransferDebitJournalCutover(ctx, logf)
}

// The swap as one script: its lock, the prepared state asserted unchanged
// under it (an abort or abandon that committed while the swap waited leaves
// the table without its bound, and the attach would then scan the table under
// the lock), the renames and the attach, then how long the lock has been held,
// by the database's clock, as its last row.
func transferDebitJournalSwapScript(shape *contractTableShape, staged *contractTableShape, fingerprint string, cutoverDay time.Time, settings *TransferDebitJournalPartitionSettings) []string {
	statements := transferDebitJournalScriptBounds(settings)
	statements = append(
		statements,
		transferDebitJournalSwapLockSql(),
		transferDebitJournalMarkLockedSql,
		transferDebitJournalUnchangedSql(fingerprint, "the prepared cutover changed while the swap waited for its lock", transferDebitJournalTable, transferDebitJournalStagedParentName()),
	)
	statements = append(statements, transferDebitJournalSwapSql(shape, staged, ContractIdFloor(cutoverDay))...)
	return append(statements, "SELECT "+transferDebitJournalHeldExpression)
}

// The swap: the table and the staged parent read with their fingerprint, the
// prepared state checked, then the swap script in one message. Returns how
// long its lock was held before the commit, by the database's clock.
func swapTransferDebitJournal(ctx context.Context, cutoverDay time.Time, settings *TransferDebitJournalPartitionSettings) (held time.Duration, err error) {
	shapes, fingerprint, err := readTransferDebitJournalShapes(ctx, transferDebitJournalTable, transferDebitJournalStagedParentName())
	if err != nil {
		return
	}
	shape := shapes[transferDebitJournalTable]
	staged := shapes[transferDebitJournalStagedParentName()]
	if shape == nil || shape.relkind != "r" || staged == nil || shape.legacyBound == nil || !shape.legacyBound.validated {
		return 0, errors.New("the prepared cutover changed before the swap")
	}
	if day, boundErr := transferDebitJournalLegacyBoundDay(shape.legacyBound); boundErr != nil || !day.Equal(cutoverDay) {
		return 0, errors.New("the prepared cutover changed before the swap")
	}
	if settings.beforeSwapScriptForTest != nil {
		if err = settings.beforeSwapScriptForTest(); err != nil {
			return
		}
	}
	values, err := transferDebitJournalScript(ctx, transferDebitJournalSwapScript(shape, staged, fingerprint, cutoverDay, settings))
	if err != nil {
		return
	}
	return transferDebitJournalReportSeconds(values, 0)
}

// After the swap: the parent starts without statistics, and the days ahead
// are created now rather than at the runway task's next run. Both repeat
// safely, and the registry's tasks do both later anyway, so a failure here is
// reported and does not undo a completed swap.
func completeTransferDebitJournalCutover(ctx context.Context, logf func(string, ...any)) error {
	if err := contractPartitionExec(ctx, []string{`SET LOCAL statement_timeout = 0`, liftTransactionTimeoutInTxSql, fmt.Sprintf("ANALYZE ONLY public.%s", quoteIdentifier(transferDebitJournalTable))}); err != nil {
		logf("could not analyze %s (the retention task analyzes it): %s", transferDebitJournalTable, err)
	} else {
		logf("analyzed %s", transferDebitJournalTable)
	}
	var runway *PartitionRunwayReport
	if err := partitionRegistryRun(func() {
		runway = transferDebitJournalPartitionFamily.EnsureRunway(ctx, NowUtc())
	}); err != nil {
		logf("could not create the days ahead (the runway task retries): %s", err)
		return nil
	}
	if 0 < len(runway.Errors) {
		logf("could not create every day ahead (the runway task retries): %s", strings.Join(runway.Errors, "; "))
	}
	logf("created %v; %d day(s) ahead are covered", runway.Created, runway.FutureRanges)
	return nil
}

// What a rollback is computed from, read in one repeatable read snapshot: the
// legacy partition's shape (its index names), the column list, the
// fingerprint of the parent and the legacy partition, and the partitions
// other than the legacy one. Refuses unless the journal is partitioned with
// its legacy partition attached as such and still carrying its bound.
func readTransferDebitJournalRollback(ctx context.Context) (legacy *contractTableShape, columns string, fingerprint string, others []PartitionLeaf, err error) {
	table := transferDebitJournalTable
	legacyName := transferDebitJournalLegacyLeafName()
	err = partitionRegistryRun(func() {
		MaintenanceTx(ctx, func(tx PgTx) {
			parent := readContractTableShape(ctx, tx, table)
			legacy = readContractTableShape(ctx, tx, legacyName)
			if parent == nil || parent.relkind != "p" {
				panic(fmt.Errorf("%s is not partitioned", table))
			}
			// the legacy partition keeps the table's bound under the table's
			// name, which a shape read by the partition's own name lists with
			// its checks
			bound := contractLegacyBoundName(table)
			if legacy == nil || !slices.ContainsFunc(legacy.checks, func(check contractConstraint) bool {
				return check.name == bound
			}) {
				panic(fmt.Errorf("%s has no legacy partition with its bound; the swap can no longer be undone", table))
			}
			leaves, listErr := transferDebitJournalPartitionFamily.ListLeaves(ctx, tx, table)
			Raise(listErr)
			if !slices.ContainsFunc(leaves, func(leaf PartitionLeaf) bool {
				return leaf.Name == legacyName && leaf.Range.Legacy
			}) {
				panic(fmt.Errorf("%s is not attached as the legacy partition", legacyName))
			}
			for _, leaf := range leaves {
				if leaf.Name != legacyName {
					others = append(others, leaf)
				}
			}
			columns = closeEvidenceColumnList(ctx, tx, table)
			Raise(tx.QueryRow(ctx, "SELECT "+transferDebitJournalFingerprintSql(table, legacyName)).Scan(&fingerprint))
		}, TxRepeatableRead, TxLockTimeout(transferDebitJournalReadLockTimeout), OptNoRetry())
	})
	return
}

// The rollback as one script: the lock on the parent and every partition, the
// two asserted unchanged since they were read, the legacy partition detached,
// the rows outside it bounded by RollbackMaxRows and copied back into it, the
// parent dropped with its other partitions, and the published names restored.
// Its last row is the rows copied and how long the lock has been held.
func transferDebitJournalRollbackScript(legacy *contractTableShape, columns string, fingerprint string, settings *TransferDebitJournalPartitionSettings) []string {
	table := quoteIdentifier(transferDebitJournalTable)
	legacyName := quoteIdentifier(transferDebitJournalLegacyLeafName())
	statements := transferDebitJournalScriptBounds(settings)
	statements = append(
		statements,
		// the live rows are a small share of each partition's heap: count and
		// copy them through the primary key
		"SET LOCAL enable_seqscan = off",
		fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", table),
		transferDebitJournalMarkLockedSql,
		transferDebitJournalUnchangedSql(fingerprint, "the partitions changed while the rollback waited for its lock; run it again", transferDebitJournalTable, transferDebitJournalLegacyLeafName()),
		fmt.Sprintf("ALTER TABLE public.%s DETACH PARTITION public.%s", table, legacyName),
		transferDebitJournalAssertSql(
			fmt.Sprintf("(SELECT count(*) FROM (SELECT 1 FROM public.%s LIMIT %d) outside) <= %d", table, settings.RollbackMaxRows+1, settings.RollbackMaxRows),
			fmt.Sprintf("more than %d rows are outside %s; the swap can no longer be undone", settings.RollbackMaxRows, transferDebitJournalLegacyLeafName()),
		),
		fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT %s", legacyName, quoteIdentifier(contractLegacyBoundName(transferDebitJournalTable))),
		fmt.Sprintf(
			"WITH copied AS (INSERT INTO public.%[1]s (%[3]s) SELECT %[3]s FROM public.%[2]s RETURNING 1) SELECT set_config('%[4]s', count(*)::text, true) FROM copied",
			legacyName,
			table,
			columns,
			transferDebitJournalCopiedSetting,
		),
		fmt.Sprintf("DROP TABLE public.%s", table),
		fmt.Sprintf("ALTER TABLE public.%s RENAME TO %s", legacyName, table),
	)
	for _, index := range legacy.indexes {
		if name, ok := strings.CutSuffix(index.name, contractLegacyIndexSuffix); ok {
			statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(index.name), quoteIdentifier(name)))
		}
	}
	return append(statements, fmt.Sprintf("SELECT current_setting('%s')::bigint, %s", transferDebitJournalCopiedSetting, transferDebitJournalHeldExpression))
}

// Undoes the swap while the legacy partition exists, in one script: the rows
// written outside the legacy partition since the swap are copied back into
// it, and the legacy table becomes the plain table again.
func rollbackTransferDebitJournalCutover(ctx context.Context, dryRun bool, logf func(string, ...any), settings *TransferDebitJournalPartitionSettings) error {
	if !IsTransferDebitJournalPartitioned(ctx) {
		return errors.New("the transfer debit journal is not partitioned")
	}
	if dryRun {
		legacy, columns, fingerprint, others, err := readTransferDebitJournalRollback(ctx)
		if err != nil {
			return err
		}
		// counted without the lock, so the count is a sample
		var outside int64
		err = partitionRegistryRun(func() {
			MaintenanceDb(ctx, func(conn PgConn) {
				for _, leaf := range others {
					var count int64
					Raise(conn.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM ONLY public.%s", quoteIdentifier(leaf.Name))).Scan(&count))
					outside += count
				}
			})
		})
		if err != nil {
			return err
		}
		logf("about %d row(s) are outside %s now; would send the rollback in one message, waiting at most %s for the lock:\n%s", outside, transferDebitJournalLegacyLeafName(), settings.LockTimeout, strings.Join(transferDebitJournalRollbackScript(legacy, columns, fingerprint, settings), ";\n"))
		logf("dry run: nothing changed")
		return nil
	}
	var copied int64
	var held time.Duration
	err := transferDebitJournalRetry(ctx, settings, settings.MaxAttempts, true, func() error {
		legacy, columns, fingerprint, _, err := readTransferDebitJournalRollback(ctx)
		if err != nil {
			return err
		}
		values, err := transferDebitJournalScript(ctx, transferDebitJournalRollbackScript(legacy, columns, fingerprint, settings))
		if err != nil {
			return err
		}
		if len(values) != 2 {
			return fmt.Errorf("the rollback reported %d values", len(values))
		}
		if copied, err = strconv.ParseInt(values[0], 10, 64); err != nil {
			return err
		}
		held, err = transferDebitJournalReportSeconds(values, 1)
		return err
	}, logf)
	if err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	logf("rolled back: %s is a plain table again, with the %d debit(s) written outside its legacy partition since the swap; its lock was held %s", transferDebitJournalTable, copied, held.Round(time.Microsecond))
	return nil
}

// The abort as one script: the lock on the table and the staged parent, the
// table asserted still plain, then the bound check and the staged parent with
// its partitions dropped. Nothing writes to the staged parent before the swap.
func transferDebitJournalAbortScript(stagedExists bool, settings *TransferDebitJournalPartitionSettings) []string {
	table := quoteIdentifier(transferDebitJournalTable)
	staged := quoteIdentifier(transferDebitJournalStagedParentName())
	statements := transferDebitJournalScriptBounds(settings)
	if stagedExists {
		statements = append(statements, fmt.Sprintf("LOCK TABLE public.%s, public.%s IN ACCESS EXCLUSIVE MODE", table, staged))
	} else {
		statements = append(statements, fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", table))
	}
	statements = append(
		statements,
		transferDebitJournalMarkLockedSql,
		transferDebitJournalAssertSql(
			fmt.Sprintf("(SELECT relkind FROM pg_class WHERE oid = to_regclass(%s)) = 'r'", transferDebitJournalLiteral("public."+table)),
			"the transfer debit journal is partitioned; use rollback",
		),
		fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT IF EXISTS %s", table, quoteIdentifier(contractLegacyBoundName(transferDebitJournalTable))),
	)
	if stagedExists {
		statements = append(statements, fmt.Sprintf("DROP TABLE public.%s", staged))
	}
	return append(statements, "SELECT "+transferDebitJournalHeldExpression)
}

// Drops a prepared, unfinalized cutover in one script (see
// transferDebitJournalAbortScript).
func abortTransferDebitJournalCutover(ctx context.Context, dryRun bool, logf func(string, ...any), settings *TransferDebitJournalPartitionSettings) error {
	if IsTransferDebitJournalPartitioned(ctx) {
		return errors.New("the transfer debit journal is partitioned; use rollback")
	}
	stagedExists := func() (exists bool, err error) {
		err = partitionRegistryRun(func() {
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+transferDebitJournalStagedParentName()).Scan(&exists))
			})
		})
		return
	}
	if dryRun {
		exists, err := stagedExists()
		if err != nil {
			return err
		}
		logf("would send in one message, waiting at most %s for the lock:\n%s", settings.LockTimeout, strings.Join(transferDebitJournalAbortScript(exists, settings), ";\n"))
		logf("dry run: nothing changed")
		return nil
	}
	var held time.Duration
	err := transferDebitJournalRetry(ctx, settings, settings.MaxAttempts, true, func() error {
		exists, err := stagedExists()
		if err != nil {
			return err
		}
		values, err := transferDebitJournalScript(ctx, transferDebitJournalAbortScript(exists, settings))
		if err != nil {
			return err
		}
		held, err = transferDebitJournalReportSeconds(values, 0)
		return err
	}, logf)
	if err != nil {
		return fmt.Errorf("abort: %w", err)
	}
	logf("aborted the cutover of %s; its lock was held %s", transferDebitJournalTable, held.Round(time.Microsecond))
	return nil
}

// The prepared cutover day, if a cutover is prepared and not finalized.
func preparedTransferDebitJournalCutoverDay(ctx context.Context) (cutoverDay time.Time, prepared bool) {
	var shape *contractTableShape
	var stagedExists bool
	MaintenanceDb(ctx, func(conn PgConn) {
		shape = readContractTableShape(ctx, conn, transferDebitJournalTable)
		Raise(conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+transferDebitJournalStagedParentName()).Scan(&stagedExists))
	})
	if shape == nil || shape.relkind != "r" {
		return time.Time{}, false
	}
	prepared = stagedExists
	if shape.legacyBound != nil {
		prepared = true
		if day, err := transferDebitJournalLegacyBoundDay(shape.legacyBound); err == nil {
			cutoverDay = day
		}
	}
	return
}

// Abandons a prepared cutover that is still unfinalized AbandonMargin before
// its day (the family's BeforeCutover, on every runway pass while the journal
// is plain): from that day on its bound would refuse every debit. One attempt
// a pass. Returns whether it abandoned one; a failure is logged and retried
// on the next pass.
func abandonStaleTransferDebitJournalCutover(ctx context.Context, now time.Time, settings *TransferDebitJournalPartitionSettings, logf func(string, ...any)) bool {
	var cutoverDay time.Time
	var prepared bool
	if err := partitionRegistryRun(func() {
		cutoverDay, prepared = preparedTransferDebitJournalCutoverDay(ctx)
	}); err != nil {
		logf("could not read the transfer debit journal cutover state: %s", err)
		return false
	}
	if !prepared || cutoverDay.IsZero() || now.Before(cutoverDay.Add(-settings.AbandonMargin)) {
		return false
	}
	logf(
		"abandoning the transfer debit journal partition cutover prepared for %s: it was not finalized by %s, and its bound would refuse every debit from that day",
		cutoverDay.Format(time.DateOnly),
		cutoverDay.Add(-settings.AbandonMargin).Format(time.RFC3339),
	)
	abandonSettings := *settings
	abandonSettings.MaxAttempts = 1
	if err := abortTransferDebitJournalCutover(ctx, false, logf, &abandonSettings); err != nil {
		logf("could not abandon the prepared transfer debit journal cutover (retried next pass): %s", err)
		return false
	}
	return true
}

// URNETWORK_TEST_DEBIT_JOURNAL_PARTITIONS=1 runs every test on a partitioned
// transfer_debit_journal, cut over at TestingTransferDebitJournalCutoverDay.
func testTransferDebitJournalPartitions() bool {
	switch strings.ToLower(os.Getenv("URNETWORK_TEST_DEBIT_JOURNAL_PARTITIONS")) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// Ten days back: debits a test writes for contracts of the last ten days land
// in their day's partition, older ones in the legacy partition.
func TestingTransferDebitJournalCutoverDay(now time.Time) time.Time {
	return PartitionGranularityDaily.AddRanges(PartitionGranularityDaily.RangeStart(now), -10)
}

// Cuts a freshly migrated test database's journal over to daily partitions
// from cutoverDay, which may be past: earlier contract ids stay in the legacy
// partition, the days from cutoverDay through the days ahead of today are
// created, and other ids go to DEFAULT.
func Testing_PartitionTransferDebitJournal(ctx context.Context, cutoverDay time.Time) {
	if IsTransferDebitJournalPartitioned(ctx) {
		return
	}
	settings := DefaultTransferDebitJournalPartitionSettings()
	logf := func(string, ...any) {}
	Raise(prepareTransferDebitJournalCutover(ctx, cutoverDay, false, false, logf, settings))
	Raise(finalizeTransferDebitJournalCutover(ctx, false, false, logf, settings))
}
