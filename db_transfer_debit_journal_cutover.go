package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
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

// Runs statements in one maintenance transaction under the settings' lock and
// statement bounds.
func transferDebitJournalExec(ctx context.Context, settings *TransferDebitJournalPartitionSettings, statements []string) error {
	return contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.LockTimeout.Milliseconds())))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.StatementTimeout.Milliseconds())))
		for _, statement := range statements {
			RaisePgResult(tx.Exec(ctx, statement))
		}
	})
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
		if dryRun {
			logf("would add %s: %s", bound, statement)
		} else {
			// a brief ACCESS EXCLUSIVE lock, without a scan
			err := transferDebitJournalRetry(ctx, settings, settings.MaxAttempts, true, func() error {
				return transferDebitJournalExec(ctx, settings, []string{statement})
			}, logf)
			if err != nil {
				return fmt.Errorf("add %s: %w", bound, err)
			}
			logf("added %s", bound)
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

	statements := append([]string{transferDebitJournalSwapLockSql()}, transferDebitJournalSwapSql(shape, staged, floor)...)
	if dryRun {
		logf("would swap in one transaction, waiting at most %s for the lock:\n%s", settings.LockTimeout, strings.Join(statements, ";\n"))
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

// The swap transaction: its lock, then the prepared state read again under it
// (an abandon or abort that committed while the swap waited leaves the table
// without its bound, and the attach would then scan the table under the
// lock), then the renames and the attach. Returns how long the lock was held
// before the commit, by the database's clock.
func swapTransferDebitJournal(ctx context.Context, cutoverDay time.Time, settings *TransferDebitJournalPartitionSettings) (held time.Duration, err error) {
	err = contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.LockTimeout.Milliseconds())))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.StatementTimeout.Milliseconds())))
		RaisePgResult(tx.Exec(ctx, transferDebitJournalSwapLockSql()))
		var lockedAt time.Time
		Raise(tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&lockedAt))
		if settings.afterSwapLockForTest != nil {
			settings.afterSwapLockForTest()
		}
		current := readContractTableShape(ctx, tx, transferDebitJournalTable)
		currentStaged := readContractTableShape(ctx, tx, transferDebitJournalStagedParentName())
		if current == nil || current.relkind != "r" || currentStaged == nil || current.legacyBound == nil || !current.legacyBound.validated {
			panic(errors.New("the prepared cutover changed while the swap waited for its lock"))
		}
		if day, boundErr := transferDebitJournalLegacyBoundDay(current.legacyBound); boundErr != nil || !day.Equal(cutoverDay) {
			panic(errors.New("the prepared cutover changed while the swap waited for its lock"))
		}
		for _, statement := range transferDebitJournalSwapSql(current, currentStaged, ContractIdFloor(cutoverDay)) {
			RaisePgResult(tx.Exec(ctx, statement))
		}
		var doneAt time.Time
		Raise(tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&doneAt))
		held = doneAt.Sub(lockedAt)
	})
	return
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

// The rollback transaction's statements, from the catalog it reads after
// taking ACCESS EXCLUSIVE on the parent and every partition: detach the
// legacy partition and drop its bound, copy every other partition's rows into
// it, drop the parent with those partitions, and give the legacy table and its
// indexes the published names. expected is the number of rows the copy must
// carry.
func transferDebitJournalRollbackSqlInTx(ctx context.Context, tx PgTx, settings *TransferDebitJournalPartitionSettings) (statements []string, expected int64, err error) {
	table := transferDebitJournalTable
	legacyName := transferDebitJournalLegacyLeafName()
	parent := readContractTableShape(ctx, tx, table)
	legacy := readContractTableShape(ctx, tx, legacyName)
	if parent == nil || parent.relkind != "p" {
		return nil, 0, fmt.Errorf("%s is not partitioned", table)
	}
	// the legacy partition keeps the table's bound under the table's name,
	// which a shape read by the partition's own name lists with its checks
	bound := contractLegacyBoundName(table)
	if legacy == nil || !slices.ContainsFunc(legacy.checks, func(check contractConstraint) bool {
		return check.name == bound
	}) {
		return nil, 0, fmt.Errorf("%s has no legacy partition with its bound; the swap can no longer be undone", table)
	}
	leaves, err := transferDebitJournalPartitionFamily.ListLeaves(ctx, tx, table)
	if err != nil {
		return nil, 0, err
	}
	if !slices.ContainsFunc(leaves, func(leaf PartitionLeaf) bool {
		return leaf.Name == legacyName && leaf.Range.Legacy
	}) {
		return nil, 0, fmt.Errorf("%s is not attached as the legacy partition", legacyName)
	}
	// each other partition counted directly, up to the bound, so the legacy
	// partition is never read
	for _, leaf := range leaves {
		if leaf.Name == legacyName {
			continue
		}
		var count int64
		Raise(tx.QueryRow(
			ctx,
			fmt.Sprintf(`SELECT count(*) FROM (SELECT 1 FROM ONLY public.%s LIMIT $1) outside`, quoteIdentifier(leaf.Name)),
			int64(settings.RollbackMaxRows)+1-expected,
		).Scan(&count))
		expected += count
		if int64(settings.RollbackMaxRows) < expected {
			return nil, 0, fmt.Errorf("more than %d rows are outside %s; the swap can no longer be undone", settings.RollbackMaxRows, legacyName)
		}
	}
	columns := closeEvidenceColumnList(ctx, tx, table)
	statements = append(statements,
		fmt.Sprintf("ALTER TABLE public.%s DETACH PARTITION public.%s", quoteIdentifier(table), quoteIdentifier(legacyName)),
		fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT %s", quoteIdentifier(legacyName), quoteIdentifier(bound)),
		fmt.Sprintf("INSERT INTO public.%[1]s (%[3]s) SELECT %[3]s FROM public.%[2]s", quoteIdentifier(legacyName), quoteIdentifier(table), columns),
		fmt.Sprintf("DROP TABLE public.%s", quoteIdentifier(table)),
		fmt.Sprintf("ALTER TABLE public.%s RENAME TO %s", quoteIdentifier(legacyName), quoteIdentifier(table)),
	)
	for _, index := range legacy.indexes {
		if name, ok := strings.CutSuffix(index.name, contractLegacyIndexSuffix); ok {
			statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(index.name), quoteIdentifier(name)))
		}
	}
	return statements, expected, nil
}

// Undoes the swap while the legacy partition exists: one transaction locks
// the parent and every partition, reads the catalog and the count under the
// lock, copies the rows outside the legacy partition back and makes the legacy
// table the plain table again. The copy's count must match, or nothing
// changes.
func rollbackTransferDebitJournalCutover(ctx context.Context, dryRun bool, logf func(string, ...any), settings *TransferDebitJournalPartitionSettings) error {
	if !IsTransferDebitJournalPartitioned(ctx) {
		return errors.New("the transfer debit journal is not partitioned")
	}
	lock := fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(transferDebitJournalTable))
	if dryRun {
		var statements []string
		var expected int64
		err := contractPartitionTx(ctx, func(tx PgTx) {
			var err error
			statements, expected, err = transferDebitJournalRollbackSqlInTx(ctx, tx, settings)
			Raise(err)
		})
		if err != nil {
			return err
		}
		logf("would lock and recheck every partition, copy back %d row(s), then roll back in one transaction:\n%s", expected, strings.Join(append([]string{lock}, statements...), ";\n"))
		logf("dry run: nothing changed")
		return nil
	}
	var copied int64
	err := transferDebitJournalRetry(ctx, settings, settings.MaxAttempts, true, func() error {
		return contractPartitionTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.LockTimeout.Milliseconds())))
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.StatementTimeout.Milliseconds())))
			RaisePgResult(tx.Exec(ctx, lock))
			// read committed: the catalog and count are read after the lock,
			// including every debit and attach that committed while it waited
			statements, expected, err := transferDebitJournalRollbackSqlInTx(ctx, tx, settings)
			Raise(err)
			copied = 0
			for _, statement := range statements {
				tag := RaisePgResult(tx.Exec(ctx, statement))
				if strings.HasPrefix(statement, "INSERT INTO ") {
					copied = tag.RowsAffected()
				}
			}
			if copied != expected {
				panic(fmt.Errorf("copied %d rows back into %s, want %d", copied, transferDebitJournalLegacyLeafName(), expected))
			}
		})
	}, logf)
	if err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	logf("rolled back: %s is a plain table again, with the %d debit(s) written outside its legacy partition since the swap", transferDebitJournalTable, copied)
	return nil
}

// Drops a prepared, unfinalized cutover in one transaction: the bound check
// goes, and the staged parent is dropped with its partitions, which nothing
// writes to before the swap.
func abortTransferDebitJournalCutover(ctx context.Context, dryRun bool, logf func(string, ...any), settings *TransferDebitJournalPartitionSettings) error {
	if IsTransferDebitJournalPartitioned(ctx) {
		return errors.New("the transfer debit journal is partitioned; use rollback")
	}
	table := quoteIdentifier(transferDebitJournalTable)
	stagedName := transferDebitJournalStagedParentName()
	bound := quoteIdentifier(contractLegacyBoundName(transferDebitJournalTable))
	abortSql := func(conn PgQuerier) []string {
		var stagedExists bool
		Raise(conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+stagedName).Scan(&stagedExists))
		if !stagedExists {
			return []string{fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT IF EXISTS %s", table, bound)}
		}
		return []string{
			fmt.Sprintf("LOCK TABLE public.%s, public.%s IN ACCESS EXCLUSIVE MODE", table, quoteIdentifier(stagedName)),
			fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT IF EXISTS %s", table, bound),
			fmt.Sprintf("DROP TABLE public.%s", quoteIdentifier(stagedName)),
		}
	}
	if dryRun {
		var statements []string
		err := partitionRegistryRun(func() {
			MaintenanceDb(ctx, func(conn PgConn) {
				statements = abortSql(conn)
			})
		})
		if err != nil {
			return err
		}
		logf("would run in one transaction:\n%s", strings.Join(statements, ";\n"))
		logf("dry run: nothing changed")
		return nil
	}
	err := transferDebitJournalRetry(ctx, settings, settings.MaxAttempts, true, func() error {
		return contractPartitionTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.LockTimeout.Milliseconds())))
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.StatementTimeout.Milliseconds())))
			for _, statement := range abortSql(tx) {
				RaisePgResult(tx.Exec(ctx, statement))
			}
		})
	}, logf)
	if err != nil {
		return fmt.Errorf("abort: %w", err)
	}
	logf("aborted the cutover of %s", transferDebitJournalTable)
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
