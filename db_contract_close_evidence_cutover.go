package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The zero-copy cutover of contract_close_report_evidence to weekly report_id
// partitions (PARTITION-MAINTENANCE-PLAN.md C5), an owner-run command:
// bringyourctl model migrate close-evidence-partition. It reuses the contract
// cutover's pieces (db_contract_partition_cutover.go): the staged parent
// cloned with LIKE ... INCLUDING INDEXES, the rolled-back attach probe, a
// validated bound check before the attach, quiet-table lock admission with
// short lock waits and retries, and resumable steps. The table becomes the
// legacy partition, <table>_legacy, of every id below the cutover week C.
//
//   - prepare (default, resumable; completed steps are skipped):
//     1. stage the empty parent <table>_p, PARTITION BY RANGE (report_id), with
//     every index of the table and both of its guards;
//     2. attach the staged DEFAULT partition, <table>_default;
//     3. add the check report_id < contract_id_floor(C), NOT VALID;
//     4. move the rows already past it (clients whose clocks run ahead) into
//     the staged parent, so DEFAULT holds them: a move the evidence guard
//     admits only once each copy is in place;
//     5. validate the check. It scans the table under SHARE UPDATE EXCLUSIVE,
//     exempt from transaction_timeout, so reads and writes continue.
//   - finalize: probe the attach, then one transaction swaps the names and
//     attaches the table FROM (MINVALUE) TO (contract_id_floor(C)): catalog
//     work only, since the validated check proves the bound and every index
//     attaches without a build. Then the weeks ahead are created.
//   - rollback (the rollback point): copy the rows outside the legacy
//     partition back into it and make it the plain table again, while those
//     rows are few (RollbackMaxRows), which is until about the cutover week.
//   - abort: before finalize, copy the moved rows back and drop the staged
//     parent and the check.
//
// From step 3 until the swap the check refuses a new report whose id is at or
// past C: its client retries it (ControlSync) and it is admitted after the
// swap. The moved rows are invisible to readers of the table from step 4 until
// the swap, so the epoch census waits meanwhile
// (CloseEvidenceMovedOutsideTableInTx), and an exact retry of a moved report is
// refused by the check rather than counted again. The check refuses every new
// report once C begins, so a prepare still unfinalized AbandonMargin before C
// is abandoned by the maintenance task (abandonStaleCloseEvidenceCutover).
//
// Order with the contract cutover (CONTRACT-PARTITION.md): none is needed, in
// either direction. The evidence has no foreign key, in or out, and neither
// cutover locks the other's tables. The evidence drop reads transfer_contract
// through its outcome index and the census reads contract_close_report by
// contract_id, which hold in both contract layouts. Both need
// contract_id_floor (migration 856), which prepare checks.
type CloseEvidencePartitionMigrateOptions struct {
	// the Monday 00:00 UTC from which new reports go to weekly partitions
	// (prepare only)
	CutoverWeek time.Time
	DryRun      bool
	Finalize    bool
	Rollback    bool
	Abort       bool
	Logf        func(string, ...any)
	// nil uses DefaultCloseEvidencePartitionSettings.
	Settings *CloseEvidencePartitionSettings
}

func MigrateCloseEvidenceToPartitions(ctx context.Context, options CloseEvidencePartitionMigrateOptions) error {
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	settings := options.Settings
	if settings == nil {
		settings = DefaultCloseEvidencePartitionSettings()
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
		return finalizeCloseEvidenceCutover(ctx, true, options.DryRun, logf, settings)
	case options.Rollback:
		return rollbackCloseEvidenceCutover(ctx, options.DryRun, logf, settings)
	case options.Abort:
		return abortCloseEvidenceCutover(ctx, options.DryRun, logf, settings)
	default:
		return prepareCloseEvidenceCutover(ctx, options.CutoverWeek, options.DryRun, logf, settings)
	}
}

var closeEvidenceLegacyBoundPattern = regexp.MustCompile(`^CHECK \(\(report_id < '([0-9a-f-]{36})'::uuid\)\)( NOT VALID)?$`)

// The cutover week of a prepared bound check.
func closeEvidenceLegacyBoundWeek(bound *contractConstraint) (cutoverWeek time.Time, err error) {
	groups := closeEvidenceLegacyBoundPattern.FindStringSubmatch(bound.definition)
	if groups == nil {
		return time.Time{}, fmt.Errorf("%s has an unexpected definition: %s", bound.name, bound.definition)
	}
	floor, err := ParseId(groups[1])
	if err != nil {
		return time.Time{}, err
	}
	cutoverWeek = ContractIdTime(floor)
	if !PartitionGranularityWeekly.RangeStart(cutoverWeek).Equal(cutoverWeek) {
		return time.Time{}, fmt.Errorf("%s does not bound a week: %s", bound.name, bound.definition)
	}
	return cutoverWeek, nil
}

// Reasons the table cannot become the legacy partition as it is.
func closeEvidenceCutoverProblems(shape *contractTableShape) (problems []string) {
	if shape == nil {
		return []string{fmt.Sprintf("%s does not exist", closeEvidenceTable)}
	}
	if shape.relkind != "r" {
		problems = append(problems, fmt.Sprintf("%s is not a plain table (relkind %s)", shape.table, shape.relkind))
	}
	key := slices.IndexFunc(shape.columns, func(column contractColumn) bool {
		return column.name == "report_id"
	})
	if key < 0 || shape.columns[key].typeName != "uuid" || !shape.columns[key].notNull {
		problems = append(problems, fmt.Sprintf("%s.report_id is not a NOT NULL uuid", shape.table))
	}
	for _, check := range shape.checks {
		if check.noInherit {
			problems = append(problems, fmt.Sprintf("%s check %s is NO INHERIT", shape.table, check.name))
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
		if index.primary {
			primary += 1
			if index.constraintDefinition != "PRIMARY KEY (client_id, report_id)" {
				problems = append(problems, fmt.Sprintf("%s primary key %s is not (client_id, report_id): %s", shape.table, index.name, index.constraintDefinition))
			}
		} else if index.unique {
			// a partitioned table enforces a unique index only through the
			// partition key, and nothing here relies on another
			problems = append(problems, fmt.Sprintf("%s unique index %s is not the primary key", shape.table, index.name))
		} else if _, ok := contractRetargetIndex(index.definition, index.name, shape.table); !ok {
			problems = append(problems, fmt.Sprintf("%s index %s has an unexpected definition", shape.table, index.name))
		}
	}
	if primary != 1 {
		problems = append(problems, fmt.Sprintf("%s has %d primary keys", shape.table, primary))
	}
	for _, trigger := range shape.triggers {
		if trigger.constraint {
			problems = append(problems, fmt.Sprintf("%s trigger %s is a constraint trigger", shape.table, trigger.name))
		}
		if _, ok := contractRetargetTrigger(trigger.definition, shape.table, shape.table+"_x"); !ok {
			problems = append(problems, fmt.Sprintf("%s trigger %s has an unexpected definition", shape.table, trigger.name))
		}
	}
	for _, foreignKey := range append(slices.Clone(shape.foreignKeys), shape.referencedBy...) {
		problems = append(problems, fmt.Sprintf("%s foreign key %s of %s: the evidence has none, and the cutover carries none", shape.table, foreignKey.name, foreignKey.table))
	}
	return
}

// Differences between the staged parent and the table it will replace,
// beyond the columns and checks (contractStagedProblems): it must be
// partitioned by report_id and carry the table's triggers.
func closeEvidenceStagedProblems(ctx context.Context, shape *contractTableShape, staged *contractTableShape) (problems []string) {
	problems = contractStagedProblems(shape, staged)
	if 0 < len(problems) {
		return
	}
	var partitionKey string
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `SELECT coalesce(pg_get_partkeydef(to_regclass($1)), '')`, "public."+staged.table).Scan(&partitionKey))
	})
	if partitionKey != "RANGE (report_id)" {
		problems = append(problems, fmt.Sprintf("%s is partitioned by %q, not RANGE (report_id); abort and prepare again", staged.table, partitionKey))
	}
	if !slices.Equal(closeEvidenceTriggerDefinitions(shape, staged.table), closeEvidenceTriggerDefinitions(staged, staged.table)) {
		problems = append(problems, fmt.Sprintf("%s triggers differ from %s; abort and prepare again", staged.table, shape.table))
	}
	return
}

// The trigger definitions of shape as they would read on table, with their
// enabled states, in name order.
func closeEvidenceTriggerDefinitions(shape *contractTableShape, table string) (definitions []string) {
	for _, trigger := range shape.triggers {
		definition, _ := contractRetargetTrigger(trigger.definition, shape.table, table)
		definitions = append(definitions, definition+" "+trigger.enabled)
	}
	return
}

// The staged parent: the table's columns, defaults, storage and every index
// (LIKE ... INCLUDING INDEXES clones each index's stored expression tree, so
// the swap's attach adopts the table's own), then its checks with their
// validity, and the table's guards: a row trigger on the parent clones onto
// every partition attached to it, and its TRUNCATE guard covers a TRUNCATE of
// the parent.
func closeEvidenceStagedParentSql(shape *contractTableShape) (statements []string, err error) {
	staged := closeEvidenceStagedParentName()
	statements = shape.stagedParentSqlBy("report_id")
	for _, trigger := range shape.triggers {
		definition, ok := contractRetargetTrigger(trigger.definition, shape.table, staged)
		if !ok {
			return nil, fmt.Errorf("cannot retarget trigger %s of %s", trigger.name, shape.table)
		}
		statements = append(statements, definition)
		statements = append(statements, contractTriggerEnabledSql(staged, trigger)...)
	}
	return
}

// Stages the parent in one transaction, giving its indexes the table's index
// names plus the staged suffix.
func stageCloseEvidenceParent(ctx context.Context, shape *contractTableShape) error {
	statements, err := closeEvidenceStagedParentSql(shape)
	if err != nil {
		return err
	}
	return contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
		for _, statement := range statements {
			RaisePgResult(tx.Exec(ctx, statement))
		}
		staged := readContractTableShape(ctx, tx, closeEvidenceStagedParentName())
		renames, err := contractStagedRenameSql(shape, staged)
		Raise(err)
		for _, statement := range renames {
			RaisePgResult(tx.Exec(ctx, statement))
		}
	})
}

// The staged DEFAULT partition, under the name it keeps after the swap: a
// table LIKE the staged parent, the table's TRUNCATE guards (a parent's
// statement trigger does not fire for a statement that names a partition) and
// storage settings, attached as DEFAULT. The attach clones the row guard and
// builds the parent's indexes on the empty table.
func closeEvidenceStagedDefaultSql(shape *contractTableShape) (statements []string, err error) {
	staged := closeEvidenceStagedParentName()
	leaf := closeEvidenceDefaultLeafName()
	statements = append(statements, fmt.Sprintf(
		"CREATE TABLE public.%s (LIKE public.%s INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING STORAGE INCLUDING COMPRESSION)",
		quoteIdentifier(leaf),
		quoteIdentifier(staged),
	))
	statements = append(statements, shape.reloptionsSql(leaf)...)
	guards, err := shape.truncateGuardsSql(shape.table, leaf)
	if err != nil {
		return nil, err
	}
	statements = append(statements, guards...)
	statements = append(statements, fmt.Sprintf(
		"ALTER TABLE public.%s ATTACH PARTITION public.%s DEFAULT",
		quoteIdentifier(staged),
		quoteIdentifier(leaf),
	))
	return
}

// Whether the staged parent has its DEFAULT partition attached.
func closeEvidenceStagedDefaultAttached(ctx context.Context) (attached bool) {
	MaintenanceDb(ctx, func(conn PgConn) {
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
			"public."+closeEvidenceStagedParentName(),
			closeEvidenceDefaultLeafName(),
		).Scan(&attached))
	})
	return
}

// Runs the test hook of a cutover step, if any.
func (self *CloseEvidencePartitionSettings) beforeCutoverStep(step string) error {
	if self.beforeCutoverStepForTest != nil {
		return self.beforeCutoverStepForTest(step)
	}
	return nil
}

// Repeats run while the evidence table is busy: each attempt first waits out
// long transactions on its relations (so a lock request never queues traffic
// behind one), then runs, and a lock wait that timed out or lost a deadlock is
// retried after the interval.
func closeEvidenceRetry(ctx context.Context, settings *CloseEvidencePartitionSettings, attempts int, quiet bool, run func() error, logf func(string, ...any)) (err error) {
	for attempt := 1; attempt <= attempts; attempt += 1 {
		busy := 0
		if quiet {
			if busyErr := partitionRegistryRun(func() {
				MaintenanceDb(ctx, func(conn PgConn) {
					busy = partitionRelationsBusy(ctx, conn, closeEvidenceLockRelations(), settings.LockSettings.MaxTransactionAge)
				})
			}); busyErr != nil {
				return busyErr
			}
		}
		if 0 < busy {
			logf("%d transaction(s) older than %s hold close evidence locks (attempt %d/%d)", busy, settings.LockSettings.MaxTransactionAge, attempt, attempts)
			err = fmt.Errorf("%d long transaction(s) hold close evidence locks", busy)
		} else {
			err = run()
			if err == nil || !contractPartitionLockBusy(err) {
				return
			}
			logf("lock busy (attempt %d/%d): %v", attempt, attempts, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settings.RetryInterval):
		}
	}
	return
}

// Runs statements in one maintenance transaction under the settings' lock and
// statement bounds; afterLock runs once the first statement (the lock) holds.
func closeEvidenceExec(ctx context.Context, settings *CloseEvidencePartitionSettings, statements []string, afterLock func()) error {
	return contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.LockTimeout.Milliseconds())))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.StatementTimeout.Milliseconds())))
		for i, statement := range statements {
			RaisePgResult(tx.Exec(ctx, statement))
			if i == 0 && afterLock != nil {
				afterLock()
			}
		}
	})
}

func prepareCloseEvidenceCutover(ctx context.Context, cutoverWeek time.Time, dryRun bool, logf func(string, ...any), settings *CloseEvidencePartitionSettings) error {
	if cutoverWeek.IsZero() {
		return errors.New("the cutover week is required to prepare")
	}
	if cutoverWeek.Before(NowUtc().Add(settings.PrepareLeadTime)) {
		return fmt.Errorf("the cutover week %s must start at least %s from now, to leave time for validation and the swap", cutoverWeek.UTC().Format(time.DateOnly), settings.PrepareLeadTime)
	}
	return prepareCloseEvidenceCutoverForWeek(ctx, cutoverWeek, dryRun, logf, settings)
}

func prepareCloseEvidenceCutoverForWeek(ctx context.Context, cutoverWeek time.Time, dryRun bool, logf func(string, ...any), settings *CloseEvidencePartitionSettings) error {
	if IsCloseEvidencePartitioned(ctx) {
		return errors.New("the close evidence is already partitioned")
	}
	cutoverWeek = cutoverWeek.UTC()
	if !PartitionGranularityWeekly.RangeStart(cutoverWeek).Equal(cutoverWeek) {
		return fmt.Errorf("the cutover week must start Monday 00:00 UTC: %s", cutoverWeek)
	}
	floor := ContractIdFloor(cutoverWeek)
	if err := checkContractIdFloorFunction(ctx, cutoverWeek, floor); err != nil {
		return err
	}
	logf("cutover week %s: report ids below %s stay in %s", cutoverWeek.Format(time.DateOnly), floor, closeEvidenceLegacyLeafName())
	logf("finalize must complete before %s; unfinished preparation is then abandoned", cutoverWeek.Add(-settings.AbandonMargin).Format(time.RFC3339))

	var shape *contractTableShape
	var staged *contractTableShape
	MaintenanceDb(ctx, func(conn PgConn) {
		shape = readContractTableShape(ctx, conn, closeEvidenceTable)
		staged = readContractTableShape(ctx, conn, closeEvidenceStagedParentName())
	})
	problems := closeEvidenceCutoverProblems(shape)
	if shape != nil && shape.legacyBound != nil {
		if week, err := closeEvidenceLegacyBoundWeek(shape.legacyBound); err != nil {
			problems = append(problems, err.Error())
		} else if !week.Equal(cutoverWeek) {
			problems = append(problems, fmt.Sprintf("%s is prepared for the cutover week %s; abort it first", closeEvidenceTable, week.Format(time.DateOnly)))
		}
	}
	if shape != nil && staged != nil {
		problems = append(problems, closeEvidenceStagedProblems(ctx, shape, staged)...)
	}
	if 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the cutover", len(problems))
	}

	stagedName := closeEvidenceStagedParentName()
	if err := settings.beforeCutoverStep("stage"); err != nil {
		return err
	}
	if staged == nil {
		statements, err := closeEvidenceStagedParentSql(shape)
		if err != nil {
			return err
		}
		if dryRun {
			logf("would stage %s, then give its indexes the table's names plus %q:\n%s", stagedName, contractStagedIndexSuffix, strings.Join(statements, ";\n"))
		} else {
			if err := stageCloseEvidenceParent(ctx, shape); err != nil {
				return fmt.Errorf("stage %s: %w", stagedName, err)
			}
			logf("staged %s with %d indexes", stagedName, len(shape.indexes))
		}
	}

	if err := settings.beforeCutoverStep("default"); err != nil {
		return err
	}
	// a dry run without a staged parent has no DEFAULT partition to look for
	if (dryRun && staged == nil) || !closeEvidenceStagedDefaultAttached(ctx) {
		statements, err := closeEvidenceStagedDefaultSql(shape)
		if err != nil {
			return err
		}
		if dryRun {
			logf("would attach %s:\n%s", closeEvidenceDefaultLeafName(), strings.Join(statements, ";\n"))
		} else {
			if err := contractPartitionExec(ctx, append([]string{`SET LOCAL lock_timeout = '5s'`}, statements...)); err != nil {
				return fmt.Errorf("attach %s: %w", closeEvidenceDefaultLeafName(), err)
			}
			logf("attached %s to %s", closeEvidenceDefaultLeafName(), stagedName)
		}
	}

	if err := settings.beforeCutoverStep("bound"); err != nil {
		return err
	}
	bound := contractLegacyBoundName(closeEvidenceTable)
	if shape.legacyBound == nil {
		statement := fmt.Sprintf(
			"ALTER TABLE public.%s ADD CONSTRAINT %s CHECK (report_id < %s) NOT VALID",
			quoteIdentifier(closeEvidenceTable),
			quoteIdentifier(bound),
			contractIdLiteral(floor),
		)
		if dryRun {
			logf("would add %s: %s", bound, statement)
		} else {
			// a brief ACCESS EXCLUSIVE lock, without a scan
			err := closeEvidenceRetry(ctx, settings, settings.MaxAttempts, true, func() error {
				return closeEvidenceExec(ctx, settings, []string{statement}, nil)
			}, logf)
			if err != nil {
				return fmt.Errorf("add %s: %w", bound, err)
			}
			logf("added %s; reports at or past %s are refused, and retried by their clients, until the swap", bound, cutoverWeek.Format(time.DateOnly))
		}
	}

	if shape.legacyBound == nil || !shape.legacyBound.validated {
		if err := settings.beforeCutoverStep("move"); err != nil {
			return err
		}
		if dryRun {
			logf("would move the rows at or past the bound into %s (a scan of %s), then validate %s (another scan, under SHARE UPDATE EXCLUSIVE)", stagedName, closeEvidenceTable, bound)
		} else {
			moved, err := moveCloseEvidenceAheadRows(ctx, floor, settings)
			if err != nil {
				return fmt.Errorf("move the rows past %s: %w", bound, err)
			}
			logf("moved %d row(s) at or past the bound into %s", moved, closeEvidenceDefaultLeafName())

			if err := settings.beforeCutoverStep("validate"); err != nil {
				return err
			}
			logf("validating %s: scans %s under SHARE UPDATE EXCLUSIVE; reads and writes continue", bound, closeEvidenceTable)
			validateStart := time.Now()
			err = closeEvidenceRetry(ctx, settings, 10, false, func() error {
				return validateCloseEvidenceLegacyBound(ctx, bound, settings)
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
		logf("prepared; run --finalize before %s", cutoverWeek.Add(-settings.AbandonMargin).Format(time.RFC3339))
	}
	return nil
}

// Moves the rows at or past floor into the staged parent, where they route to
// its DEFAULT partition, and returns how many moved. The scan reads the whole
// table on a maintenance session exempt from transaction_timeout; the bound
// check is already in place, so no new row past it can arrive. Each batch is
// one transaction: copy into the staged parent, then delete as a move, which
// the evidence guard admits row by row only once the identical copy is there.
// The counts must agree, so a row that left the table meanwhile (only a
// retention delete can) rolls the batch back for the next run.
func moveCloseEvidenceAheadRows(ctx context.Context, floor Id, settings *CloseEvidencePartitionSettings) (moved int64, err error) {
	type evidenceKey struct {
		clientId Id
		reportId Id
	}
	var keys []evidenceKey
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			WithoutTransactionTimeout(ctx, conn, func() {
				RaisePgResult(conn.Exec(ctx, `SET statement_timeout = 0`))
				defer func() {
					RaisePgResult(conn.Exec(context.WithoutCancel(ctx), `RESET statement_timeout`))
				}()
				result, queryErr := conn.Query(
					ctx,
					fmt.Sprintf(`SELECT client_id, report_id FROM ONLY public.%s WHERE $1 <= report_id LIMIT $2`, quoteIdentifier(closeEvidenceTable)),
					floor,
					settings.MaxAheadRows+1,
				)
				WithPgResult(result, queryErr, func() {
					for result.Next() {
						var key evidenceKey
						Raise(result.Scan(&key.clientId, &key.reportId))
						keys = append(keys, key)
					}
				})
			})
		})
	})
	if err != nil {
		return 0, err
	}
	if settings.MaxAheadRows < len(keys) {
		return 0, fmt.Errorf("more than %d rows are at or past the bound; choose a later cutover week", settings.MaxAheadRows)
	}
	if len(keys) == 0 {
		return 0, nil
	}
	var columns string
	MaintenanceDb(ctx, func(conn PgConn) {
		columns = closeEvidenceColumnList(ctx, conn, closeEvidenceTable)
	})
	staged := quoteIdentifier(closeEvidenceStagedParentName())
	table := quoteIdentifier(closeEvidenceTable)
	for start := 0; start < len(keys); start += settings.MoveBatchSize {
		if stepErr := settings.beforeCutoverStep("move batch"); stepErr != nil {
			return moved, stepErr
		}
		batch := keys[start:min(start+settings.MoveBatchSize, len(keys))]
		clientIds := make([]Id, 0, len(batch))
		reportIds := make([]Id, 0, len(batch))
		for _, key := range batch {
			clientIds = append(clientIds, key.clientId)
			reportIds = append(reportIds, key.reportId)
		}
		var deleted int64
		err = contractPartitionTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.LockTimeout.Milliseconds())))
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.StatementTimeout.Milliseconds())))
			copied := RaisePgResult(tx.Exec(
				ctx,
				fmt.Sprintf(
					`
					INSERT INTO public.%[1]s (%[3]s)
					SELECT %[3]s FROM ONLY public.%[2]s
					WHERE (client_id, report_id) IN (SELECT * FROM unnest($1::uuid[], $2::uuid[]))
					`,
					staged, table, columns,
				),
				clientIds,
				reportIds,
			)).RowsAffected()
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL %s = 'move'", closeEvidenceRetentionSetting)))
			deleted = RaisePgResult(tx.Exec(
				ctx,
				fmt.Sprintf(
					`
					DELETE FROM ONLY public.%s
					WHERE (client_id, report_id) IN (SELECT * FROM unnest($1::uuid[], $2::uuid[]))
					`,
					table,
				),
				clientIds,
				reportIds,
			)).RowsAffected()
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL %s = ''", closeEvidenceRetentionSetting)))
			if copied != deleted {
				panic(fmt.Errorf("moved %d rows past the bound but copied %d", deleted, copied))
			}
		})
		if err != nil {
			return moved, err
		}
		moved += deleted
	}
	return moved, nil
}

// Validates the bound check on a maintenance session exempt from
// transaction_timeout, with no statement bound: it scans the whole table.
func validateCloseEvidenceLegacyBound(ctx context.Context, bound string, settings *CloseEvidencePartitionSettings) error {
	return partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			WithoutTransactionTimeout(ctx, conn, func() {
				tx, err := conn.Begin(ctx)
				Raise(err)
				defer tx.Rollback(context.WithoutCancel(ctx))
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.ValidateLockTimeout.Milliseconds())))
				RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = 0`))
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf("ALTER TABLE public.%s VALIDATE CONSTRAINT %s", quoteIdentifier(closeEvidenceTable), quoteIdentifier(bound))))
				Raise(tx.Commit(ctx))
			})
		})
	})
}

// The swap transaction. It locks the table and the staged parent (with its
// DEFAULT partition) first, so the renames and the attach see no concurrent
// statement, drops the table's row guard (the parent's clones onto it at the
// attach; its TRUNCATE guard stays, for a TRUNCATE that names the partition),
// moves the table and its indexes to the legacy names, attaches it below
// floor, and gives the staged parent and its indexes the published names.
func closeEvidenceSwapSql(shape *contractTableShape, staged *contractTableShape, floor Id) (statements []string) {
	table := closeEvidenceTable
	stagedName := closeEvidenceStagedParentName()
	legacy := closeEvidenceLegacyLeafName()
	statements = append(statements, fmt.Sprintf(
		"LOCK TABLE public.%s, public.%s IN ACCESS EXCLUSIVE MODE",
		quoteIdentifier(table),
		quoteIdentifier(stagedName),
	))
	for _, trigger := range shape.triggers {
		if !trigger.truncate {
			statements = append(statements, fmt.Sprintf("DROP TRIGGER %s ON public.%s", quoteIdentifier(trigger.name), quoteIdentifier(table)))
		}
	}
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

func finalizeCloseEvidenceCutover(ctx context.Context, deadline bool, dryRun bool, logf func(string, ...any), settings *CloseEvidencePartitionSettings) error {
	if IsCloseEvidencePartitioned(ctx) {
		return errors.New("the close evidence is already partitioned")
	}
	var shape *contractTableShape
	var staged *contractTableShape
	MaintenanceDb(ctx, func(conn PgConn) {
		shape = readContractTableShape(ctx, conn, closeEvidenceTable)
		staged = readContractTableShape(ctx, conn, closeEvidenceStagedParentName())
	})
	problems := closeEvidenceCutoverProblems(shape)
	var cutoverWeek time.Time
	var syncStatements []string
	if shape != nil {
		if shape.legacyBound == nil || !shape.legacyBound.validated {
			problems = append(problems, fmt.Sprintf("%s is not prepared: its bound is missing or not validated", closeEvidenceTable))
		} else if week, err := closeEvidenceLegacyBoundWeek(shape.legacyBound); err != nil {
			problems = append(problems, err.Error())
		} else {
			cutoverWeek = week
		}
		if staged == nil {
			problems = append(problems, fmt.Sprintf("%s is not staged", closeEvidenceStagedParentName()))
		} else {
			problems = append(problems, closeEvidenceStagedProblems(ctx, shape, staged)...)
			statements, indexProblems := contractStagedIndexSyncSql(shape, staged)
			problems = append(problems, indexProblems...)
			syncStatements = statements
			if !closeEvidenceStagedDefaultAttached(ctx) {
				problems = append(problems, fmt.Sprintf("%s has no DEFAULT partition", closeEvidenceStagedParentName()))
			}
		}
	}
	if 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the swap", len(problems))
	}
	if deadline && !NowUtc().Before(cutoverWeek.Add(-settings.FinalizeMargin)) {
		return fmt.Errorf("too close to the cutover week %s; abort and prepare a later week", cutoverWeek.Format(time.DateOnly))
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
				staged = readContractTableShape(ctx, conn, closeEvidenceStagedParentName())
			})
		}
	}

	if err := settings.beforeCutoverStep("probe"); err != nil {
		return err
	}
	floor := ContractIdFloor(cutoverWeek)
	if problems := probeContractLegacyAttach(ctx, closeEvidenceTable, floor); 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the swap", len(problems))
	}
	logf("probed the swap: every parent index adopts an index of %s", closeEvidenceTable)

	statements := closeEvidenceSwapSql(shape, staged, floor)
	if dryRun {
		logf("would swap in one transaction:\n%s", strings.Join(statements, ";\n"))
		logf("dry run: nothing changed")
		return nil
	}
	if err := settings.beforeCutoverStep("swap"); err != nil {
		return err
	}
	swapStart := time.Now()
	err := closeEvidenceRetry(ctx, settings, settings.MaxAttempts, true, func() error {
		return closeEvidenceExec(ctx, settings, statements, settings.afterSwapLockForTest)
	}, logf)
	if err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	logf("swapped %s in %s; reports from %s go to weekly partitions", closeEvidenceTable, time.Since(swapStart).Round(time.Millisecond), cutoverWeek.Format(time.DateOnly))
	return completeCloseEvidenceCutover(ctx, logf)
}

// After the swap: the parent starts without statistics, and the weeks ahead
// are created now rather than at the runway task's next run. Both repeat
// safely, and the registry's tasks do both later anyway, so a failure here is
// reported and does not undo a completed swap.
func completeCloseEvidenceCutover(ctx context.Context, logf func(string, ...any)) error {
	if err := contractPartitionExec(ctx, []string{`SET LOCAL statement_timeout = 0`, fmt.Sprintf("ANALYZE ONLY public.%s", quoteIdentifier(closeEvidenceTable))}); err != nil {
		logf("could not analyze %s (the retention task analyzes it): %s", closeEvidenceTable, err)
	} else {
		logf("analyzed %s", closeEvidenceTable)
	}
	var runway *PartitionRunwayReport
	if err := partitionRegistryRun(func() {
		runway = closeEvidencePartitionFamily.EnsureRunway(ctx, NowUtc())
	}); err != nil {
		logf("could not create the weeks ahead (the runway task retries): %s", err)
		return nil
	}
	if 0 < len(runway.Errors) {
		logf("could not create every week ahead (the runway task retries): %s", strings.Join(runway.Errors, "; "))
	}
	logf("created %v; %d week(s) ahead are covered", runway.Created, runway.FutureRanges)
	return nil
}

// The rollback transaction's statements, from the catalog it reads after
// taking ACCESS EXCLUSIVE on the parent and every partition: detach the
// legacy partition and drop its bound, copy every other partition's rows into
// it, drop the parent with those partitions (the guard's drop bypass is the
// point: their rows are copied), and give the legacy table and its indexes
// the published names and its row guard back (a detach drops the clones).
// expected is the number of rows the copy must carry.
func closeEvidenceRollbackSqlInTx(ctx context.Context, tx PgTx, settings *CloseEvidencePartitionSettings) (statements []string, expected int64, err error) {
	table := closeEvidenceTable
	legacyName := closeEvidenceLegacyLeafName()
	parent := readContractTableShape(ctx, tx, table)
	legacy := readContractTableShape(ctx, tx, legacyName)
	if parent == nil || parent.relkind != "p" {
		return nil, 0, fmt.Errorf("%s is not partitioned", table)
	}
	if legacy == nil || legacy.legacyBound == nil {
		return nil, 0, fmt.Errorf("%s has no legacy partition with its bound", table)
	}
	leaves, err := closeEvidencePartitionFamily.ListLeaves(ctx, tx, table)
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
			fmt.Sprintf(`SELECT count(*) FROM (SELECT 1 FROM public.%s LIMIT $1) outside`, quoteIdentifier(leaf.Name)),
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
		fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT %s", quoteIdentifier(legacyName), quoteIdentifier(legacy.legacyBound.name)),
		fmt.Sprintf("INSERT INTO public.%[1]s (%[3]s) SELECT %[3]s FROM public.%[2]s", quoteIdentifier(legacyName), quoteIdentifier(table), columns),
		fmt.Sprintf("DROP TABLE public.%s", quoteIdentifier(table)),
		fmt.Sprintf("ALTER TABLE public.%s RENAME TO %s", quoteIdentifier(legacyName), quoteIdentifier(table)),
	)
	for _, index := range legacy.indexes {
		if name, ok := strings.CutSuffix(index.name, contractLegacyIndexSuffix); ok {
			statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(index.name), quoteIdentifier(name)))
		}
	}
	for _, trigger := range parent.triggers {
		if trigger.truncate && slices.ContainsFunc(legacy.triggers, func(legacyTrigger contractTrigger) bool {
			return legacyTrigger.name == trigger.name
		}) {
			// the legacy partition kept its own guard
			continue
		}
		statements = append(statements, trigger.definition)
		statements = append(statements, contractTriggerEnabledSql(table, trigger)...)
	}
	return statements, expected, nil
}

// Undoes the swap while few rows are outside the legacy partition: one
// transaction locks the parent and every partition, reads the catalog and the
// count under the lock, copies those rows back and makes the legacy table the
// plain table again. The copy's count must match, or nothing changes.
func rollbackCloseEvidenceCutover(ctx context.Context, dryRun bool, logf func(string, ...any), settings *CloseEvidencePartitionSettings) error {
	if !IsCloseEvidencePartitioned(ctx) {
		return errors.New("the close evidence is not partitioned")
	}
	lock := fmt.Sprintf("LOCK TABLE public.%s IN ACCESS EXCLUSIVE MODE", quoteIdentifier(closeEvidenceTable))
	if dryRun {
		var statements []string
		var expected int64
		err := contractPartitionTx(ctx, func(tx PgTx) {
			var err error
			statements, expected, err = closeEvidenceRollbackSqlInTx(ctx, tx, settings)
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
	err := closeEvidenceRetry(ctx, settings, settings.MaxAttempts, true, func() error {
		return contractPartitionTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.LockTimeout.Milliseconds())))
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.StatementTimeout.Milliseconds())))
			RaisePgResult(tx.Exec(ctx, lock))
			// read committed: the catalog and count are read after the lock,
			// including every write and attach that committed while it waited
			statements, expected, err := closeEvidenceRollbackSqlInTx(ctx, tx, settings)
			Raise(err)
			copied = 0
			for _, statement := range statements {
				tag := RaisePgResult(tx.Exec(ctx, statement))
				if strings.HasPrefix(statement, "INSERT INTO ") {
					copied = tag.RowsAffected()
				}
			}
			if copied != expected {
				panic(fmt.Errorf("copied %d rows back into %s, want %d", copied, closeEvidenceLegacyLeafName(), expected))
			}
		})
	}, logf)
	if err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	logf("rolled back: %s is a plain table again, with the %d row(s) written outside its legacy partition since the swap", closeEvidenceTable, copied)
	return nil
}

// Drops a prepared, unfinalized cutover: in one transaction, under ACCESS
// EXCLUSIVE on the table and the staged parent, the bound check goes, the
// rows the prepare moved come back into the table, and the staged parent is
// dropped with its DEFAULT partition.
func abortCloseEvidenceCutover(ctx context.Context, dryRun bool, logf func(string, ...any), settings *CloseEvidencePartitionSettings) error {
	if IsCloseEvidencePartitioned(ctx) {
		return errors.New("the close evidence is partitioned; use rollback")
	}
	table := quoteIdentifier(closeEvidenceTable)
	stagedName := closeEvidenceStagedParentName()
	staged := quoteIdentifier(stagedName)
	bound := quoteIdentifier(contractLegacyBoundName(closeEvidenceTable))
	// the lock, the bound's drop, the copy back and the staged parent's drop,
	// or only the bound's drop when nothing is staged
	abortSql := func(tx PgTx) (statements []string) {
		var stagedExists bool
		Raise(tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+stagedName).Scan(&stagedExists))
		if !stagedExists {
			return []string{fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT IF EXISTS %s", table, bound)}
		}
		columns := closeEvidenceColumnList(ctx, tx, closeEvidenceTable)
		return []string{
			fmt.Sprintf("LOCK TABLE public.%s, public.%s IN ACCESS EXCLUSIVE MODE", table, staged),
			fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT IF EXISTS %s", table, bound),
			fmt.Sprintf("INSERT INTO public.%[1]s (%[3]s) SELECT %[3]s FROM public.%[2]s", table, staged, columns),
			fmt.Sprintf("DROP TABLE public.%s", staged),
		}
	}
	if dryRun {
		var statements []string
		err := contractPartitionTx(ctx, func(tx PgTx) {
			statements = abortSql(tx)
		})
		if err != nil {
			return err
		}
		logf("would run in one transaction:\n%s", strings.Join(statements, ";\n"))
		logf("dry run: nothing changed")
		return nil
	}
	var restored int64
	err := closeEvidenceRetry(ctx, settings, settings.MaxAttempts, true, func() error {
		return contractPartitionTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.LockTimeout.Milliseconds())))
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.StatementTimeout.Milliseconds())))
			restored = 0
			for _, statement := range abortSql(tx) {
				if !strings.HasPrefix(statement, "INSERT INTO ") {
					RaisePgResult(tx.Exec(ctx, statement))
					continue
				}
				// counted under the lock, which the first statement took
				var expected int64
				Raise(tx.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM public.%s", staged)).Scan(&expected))
				restored = RaisePgResult(tx.Exec(ctx, statement)).RowsAffected()
				if restored != expected {
					panic(fmt.Errorf("restored %d moved rows into %s, want %d", restored, closeEvidenceTable, expected))
				}
			}
		})
	}, logf)
	if err != nil {
		return fmt.Errorf("abort: %w", err)
	}
	logf("aborted the cutover of %s; restored %d moved row(s)", closeEvidenceTable, restored)
	return nil
}

// The prepared cutover week, if a cutover is prepared and not finalized.
func preparedCloseEvidenceCutoverWeek(ctx context.Context) (cutoverWeek time.Time, prepared bool) {
	var shape *contractTableShape
	var stagedExists bool
	MaintenanceDb(ctx, func(conn PgConn) {
		shape = readContractTableShape(ctx, conn, closeEvidenceTable)
		Raise(conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+closeEvidenceStagedParentName()).Scan(&stagedExists))
	})
	if shape == nil || shape.relkind != "r" {
		return time.Time{}, false
	}
	prepared = stagedExists
	if shape.legacyBound != nil {
		prepared = true
		if week, err := closeEvidenceLegacyBoundWeek(shape.legacyBound); err == nil {
			cutoverWeek = week
		}
	}
	return
}

// Abandons a prepared cutover that is still unfinalized AbandonMargin before
// its week (the maintenance task's guard): from that week on, its bound would
// refuse every new report. Returns whether it abandoned one; a failure is
// logged and retried on the next run.
func abandonStaleCloseEvidenceCutover(ctx context.Context, now time.Time, settings *CloseEvidencePartitionSettings, logf func(string, ...any)) bool {
	var cutoverWeek time.Time
	var prepared bool
	if err := partitionRegistryRun(func() {
		if !IsCloseEvidencePartitioned(ctx) {
			cutoverWeek, prepared = preparedCloseEvidenceCutoverWeek(ctx)
		}
	}); err != nil {
		logf("could not read the close evidence cutover state: %s", err)
		return false
	}
	if !prepared || cutoverWeek.IsZero() || now.Before(cutoverWeek.Add(-settings.AbandonMargin)) {
		return false
	}
	logf(
		"abandoning the close evidence partition cutover prepared for %s: it was not finalized by %s, and its bound would refuse every report from that week",
		cutoverWeek.Format(time.DateOnly),
		cutoverWeek.Add(-settings.AbandonMargin).Format(time.RFC3339),
	)
	abandonSettings := *settings
	abandonSettings.MaxAttempts = 1
	if err := abortCloseEvidenceCutover(ctx, false, logf, &abandonSettings); err != nil {
		logf("could not abandon the prepared close evidence cutover (retried next run): %s", err)
		return false
	}
	return true
}

// The close evidence cutover is not schema drift (as acceptContractPartitionCutover):
// while it is prepared, the staged parent with its indexes and the bound check
// on the table are its own work in progress, which finalize or abort removes.
// After the swap the parent matches the migrated table, its indexes read
// without ONLY, and its partitions are not part of the expected schema.
func (d *SchemaDiff) acceptCloseEvidencePartitionCutover() {
	staged := closeEvidenceStagedParentName()
	if d.actual.partitioned[staged] && !d.expected.tables[staged] {
		d.extraTables = slices.DeleteFunc(d.extraTables, func(table string) bool {
			return table == staged
		})
		d.extraIdx = slices.DeleteFunc(d.extraIdx, func(name string) bool {
			return d.actual.indexes[name].table == staged
		})
	}
	extra := slices.DeleteFunc(d.extraConstraints[closeEvidenceTable], func(name string) bool {
		return name == contractLegacyBoundName(closeEvidenceTable)
	})
	if len(extra) == 0 {
		delete(d.extraConstraints, closeEvidenceTable)
	} else {
		d.extraConstraints[closeEvidenceTable] = extra
	}
}

// Cuts a freshly migrated test database's close evidence over to weekly
// partitions from cutoverWeek, which may be past: earlier ids stay in the
// legacy partition, the weeks from the current one on are created, and other
// ids go to DEFAULT until their week is created.
func Testing_PartitionCloseEvidence(ctx context.Context, cutoverWeek time.Time) {
	if IsCloseEvidencePartitioned(ctx) {
		return
	}
	settings := DefaultCloseEvidencePartitionSettings()
	Raise(prepareCloseEvidenceCutoverForWeek(ctx, cutoverWeek, false, func(string, ...any) {}, settings))
	Raise(finalizeCloseEvidenceCutover(ctx, false, false, func(string, ...any) {}, settings))
}
