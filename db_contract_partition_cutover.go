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

	"github.com/jackc/pgx/v5/pgconn"
)

// The zero-copy cutover (CONTRACT-PARTITION.md "Cutover"). Each contract table
// becomes the first partition, "<table>_legacy", of a new parent partitioned by
// contract_id; new contracts go to weekly partitions from the cutover week C.
//
//   - prepare (default): stage the empty parents "<table>_p" with every index of
//     the table and the weekly partitions C..C+4, then add and validate the check
//     contract_id < contract_id_floor(C). Validation scans under SHARE UPDATE
//     EXCLUSIVE, so reads and writes continue. Resumable; completed steps are
//     skipped.
//   - finalize: one transaction swaps all six tables (seconds of catalog work).
//     The validated check proves the legacy bound, so ATTACH skips its scan, and
//     every legacy index attaches to its matching parent index without a build.
//   - rollback: before any weekly partition holds a row, undo the swap.
//   - abort: before finalize, drop the staged parents and the legacy checks.
//
// The prepared check refuses ids from C on, so finalize must run before C. A
// cutover still unfinalized 6 hours before C is abandoned by the maintenance
// task (MaintainContractPartitions) before it can refuse a contract.
type ContractPartitionMigrateOptions struct {
	// the Monday 00:00 UTC from which new contracts go to weekly partitions
	// (prepare only)
	CutoverWeek time.Time
	DryRun      bool
	Finalize    bool
	Rollback    bool
	Abort       bool
	Logf        func(string, ...any)
	// nil uses the default rollback lock and retry budgets.
	RollbackSettings *ContractPartitionRollbackSettings
	// nil uses DefaultContractPartitionCutoverSettings for prepare, finalize
	// and abort.
	CutoverSettings *ContractPartitionCutoverSettings
}

// The foreign key that the cutover drops for good (owner decision
// 2026-10-23): a week's reports are dropped with its contracts. Rollback
// restores it.
const contractCloseReportForeignKeyName = "contract_close_report_contract_id_fkey"
const contractCloseReportForeignKeyDefinition = "FOREIGN KEY (contract_id) REFERENCES transfer_contract(contract_id) ON DELETE CASCADE"

var contractLegacyBoundPattern = regexp.MustCompile(`^CHECK \(\(contract_id < '([0-9a-f-]{36})'::uuid\)\)( NOT VALID)?$`)

func IsContractPartitioned(ctx context.Context) (partitioned bool) {
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE((SELECT relkind = 'p' FROM pg_class WHERE oid = to_regclass('public.transfer_contract')), false)`,
		).Scan(&partitioned))
	})
	return
}

func MigrateContractsToPartitions(ctx context.Context, options ContractPartitionMigrateOptions) error {
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
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
	settings := options.CutoverSettings
	if settings == nil {
		settings = DefaultContractPartitionCutoverSettings()
	}
	if err := settings.validate(); err != nil {
		return err
	}
	switch {
	case options.Finalize:
		return finalizeContractPartitionCutover(ctx, options.DryRun, logf, settings)
	case options.Rollback:
		return rollbackContractPartitionCutover(ctx, options.DryRun, logf, options.RollbackSettings)
	case options.Abort:
		return abortContractPartitionCutover(ctx, options.DryRun, logf, settings)
	default:
		return prepareContractPartitionCutover(ctx, options.CutoverWeek, options.DryRun, logf, settings)
	}
}

// Runs callback in one maintenance transaction and returns its failure.
func contractPartitionTx(ctx context.Context, callback func(PgTx)) (err error) {
	defer func() {
		if r := recover(); r != nil {
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

func contractPartitionExec(ctx context.Context, statements []string) error {
	return contractPartitionTx(ctx, func(tx PgTx) {
		for _, statement := range statements {
			RaisePgResult(tx.Exec(ctx, statement))
		}
	})
}

// A lock wait that timed out or lost a deadlock can succeed on a later try.
func contractPartitionLockBusy(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "40P01")
}

// Admission policy for operations that take exclusive contract table locks.
type ContractPartitionLockSettings struct {
	// Maximum transaction age before deferring a lock request. The default
	// avoids queuing application traffic behind long transactions. Must be positive.
	MaxTransactionAge time.Duration
}

func DefaultContractPartitionLockSettings() *ContractPartitionLockSettings {
	return &ContractPartitionLockSettings{
		MaxTransactionAge: 5 * time.Second,
	}
}

// The transactions older than maxTransactionAge that hold a lock on one of the
// contract tables' parents. The weekly drop's admission; the cutover's steps
// use contractLockAdmission, which also counts autovacuum workers and the
// locks of partitions.
func contractTablesBusy(ctx context.Context, maxTransactionAge time.Duration) (busy int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	relations := []string{}
	for _, table := range ContractPartitionTables {
		relations = append(relations, "public."+table, "public."+contractStagedParentName(table))
	}
	MaintenanceDb(ctx, func(conn PgConn) {
		busy = partitionRelationsBusy(ctx, conn, relations, maxTransactionAge)
	})
	return
}

// Runs statements in one transaction, retrying while their locks are busy.
func contractPartitionExecRetry(ctx context.Context, statements []string, attempts int, wait time.Duration, logf func(string, ...any)) error {
	return contractPartitionRetryWhen(ctx, nil, func() error {
		return contractPartitionExec(ctx, statements)
	}, attempts, wait, logf)
}

// As contractPartitionExecRetry, for statements that lock the scope's live
// contract tables: each attempt first waits until no autovacuum and no long
// transaction holds a lock there (contractLockAdmission), so the lock request
// never queues traffic behind one and never cancels a vacuum.
func contractPartitionExecQuietRetry(ctx context.Context, scope *contractLockScope, statements []string, attempts int, wait time.Duration, logf func(string, ...any)) error {
	return contractPartitionRetryWhen(ctx, scope, func() error {
		return contractPartitionExec(ctx, statements)
	}, attempts, wait, logf)
}

// Repeats the entire operation so each attempt can read authoritative state
// after taking its locks, rather than reusing a previously checked catalog.
// With a scope, each attempt first reads the scope's admission and waits while
// it is busy; a run that ends busy returns ContractPartitionBusyError.
func contractPartitionRetryWhen(ctx context.Context, scope *contractLockScope, run func() error, attempts int, wait time.Duration, logf func(string, ...any)) (err error) {
	for attempt := 1; attempt <= attempts; attempt += 1 {
		var blockers []string
		if scope != nil {
			var admissionErr error
			if blockers, admissionErr = contractLockAdmission(ctx, scope); admissionErr != nil {
				return admissionErr
			}
		}
		if 0 < len(blockers) {
			logf("not queueing behind (attempt %d/%d): %s", attempt, attempts, strings.Join(blockers, "; "))
			err = &ContractPartitionBusyError{Blockers: blockers}
			if attempt == attempts {
				return
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		err = run()
		if err == nil || !contractPartitionLockBusy(err) {
			return
		}
		logf("lock busy (attempt %d/%d): %v", attempt, attempts, err)
		if attempt == attempts {
			return
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return
}

func readContractShapes(ctx context.Context, name func(string) string) (shapes map[string]*contractTableShape) {
	MaintenanceDb(ctx, func(conn PgConn) {
		shapes = readContractShapesInConn(ctx, conn, name)
	})
	return
}

// Uses the caller's connection, including its transaction and table locks.
func readContractShapesInConn(ctx context.Context, conn contractCatalogQuerier, name func(string) string) map[string]*contractTableShape {
	shapes := map[string]*contractTableShape{}
	for _, table := range ContractPartitionTables {
		if shape := readContractTableShape(ctx, conn, name(table)); shape != nil {
			shapes[table] = shape
		}
	}
	return shapes
}

func contractTableName(table string) string {
	return table
}

// A partition of a contract table with its bounds.
type contractPartition struct {
	name      string
	legacy    bool
	weekStart time.Time
	// the legacy partition's upper bound, the cutover week
	upper time.Time
}

var contractPartitionUpperPattern = regexp.MustCompile(`TO \('([0-9a-f-]{36})'\)`)

func listContractPartitions(ctx context.Context, parent string) (partitions []contractPartition, err error) {
	MaintenanceDb(ctx, func(conn PgConn) {
		partitions, err = listContractPartitionsInConn(ctx, conn, parent)
	})
	return
}

// Reads the current children through the connection that owns the parent lock.
func listContractPartitionsInConn(ctx context.Context, conn contractCatalogQuerier, parent string) (partitions []contractPartition, err error) {
	result, queryErr := conn.Query(
		ctx,
		`
			SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
			FROM pg_inherits i
			JOIN pg_class c ON c.oid = i.inhrelid
			WHERE i.inhparent = to_regclass($1)
			ORDER BY c.relname
			`,
		"public."+parent,
	)
	WithPgResult(result, queryErr, func() {
		for result.Next() {
			var name string
			var bound string
			Raise(result.Scan(&name, &bound))
			_, weekStart, legacy, ok := ParseContractPartitionName(name)
			if !ok {
				err = fmt.Errorf("unexpected partition %s of %s", name, parent)
				return
			}
			partition := contractPartition{name: name, legacy: legacy, weekStart: weekStart}
			if legacy {
				groups := contractPartitionUpperPattern.FindStringSubmatch(bound)
				if groups == nil {
					err = fmt.Errorf("unexpected bound of %s: %s", name, bound)
					return
				}
				upper, parseErr := ParseId(groups[1])
				if parseErr != nil {
					err = parseErr
					return
				}
				partition.upper = ContractIdTime(upper)
			} else {
				partition.upper = weekStart.Add(contractPartitionWeek)
			}
			partitions = append(partitions, partition)
		}
	})
	return
}

// Reasons the table cannot become the legacy partition as it is.
func (self *contractTableShape) cutoverProblems() (problems []string) {
	if self.relkind != "r" {
		problems = append(problems, fmt.Sprintf("%s is not a plain table (relkind %s)", self.table, self.relkind))
	}
	contractId := slices.IndexFunc(self.columns, func(column contractColumn) bool {
		return column.name == "contract_id"
	})
	if contractId < 0 || self.columns[contractId].typeName != "uuid" || !self.columns[contractId].notNull {
		problems = append(problems, fmt.Sprintf("%s.contract_id is not a NOT NULL uuid", self.table))
	}
	for _, check := range self.checks {
		if check.noInherit {
			problems = append(problems, fmt.Sprintf("%s check %s is NO INHERIT", self.table, check.name))
		}
	}
	primary := 0
	for _, index := range self.indexes {
		if !index.valid || !index.ready {
			problems = append(problems, fmt.Sprintf("%s index %s is invalid; drop it (DROP INDEX CONCURRENTLY) or rebuild it before the cutover", self.table, index.name))
			continue
		}
		if pgMaxIdentifierLength < len(index.name)+len(contractStagedIndexSuffix) || pgMaxIdentifierLength < len(index.name)+len(contractLegacyIndexSuffix) {
			problems = append(problems, fmt.Sprintf("%s index name %s is too long to stage", self.table, index.name))
		}
		if index.primary {
			primary += 1
			if !strings.HasPrefix(index.constraintDefinition, "PRIMARY KEY (") || !strings.Contains(index.constraintDefinition, "contract_id") {
				problems = append(problems, fmt.Sprintf("%s primary key %s does not include contract_id", self.table, index.name))
			}
		} else if index.unique {
			problems = append(problems, fmt.Sprintf("%s unique index %s is not the primary key", self.table, index.name))
		} else if _, ok := contractRetargetIndex(index.definition, index.name, self.table); !ok {
			problems = append(problems, fmt.Sprintf("%s index %s has an unexpected definition", self.table, index.name))
		}
	}
	if primary != 1 {
		problems = append(problems, fmt.Sprintf("%s has %d primary keys", self.table, primary))
	}
	for _, trigger := range self.triggers {
		if trigger.constraint {
			problems = append(problems, fmt.Sprintf("%s trigger %s is a constraint trigger", self.table, trigger.name))
		}
		if _, ok := contractRetargetTrigger(trigger.definition, self.table, self.table+"_x"); !ok {
			problems = append(problems, fmt.Sprintf("%s trigger %s has an unexpected definition", self.table, trigger.name))
		}
	}
	for _, foreignKey := range self.foreignKeys {
		if !slices.Contains(ContractPartitionTables, foreignKey.referencedTable) {
			problems = append(problems, fmt.Sprintf("%s foreign key %s references %s outside the contract tables", self.table, foreignKey.name, foreignKey.referencedTable))
		}
	}
	return
}

// The statement that stages table's partitioned parent. LIKE ... INCLUDING
// INDEXES clones each index from its stored expression trees: an index built
// from its printed definition can parse into a different tree (a varchar array
// cast moves into its elements), and ATTACH adopts a legacy index only when
// the trees are equal. Checks are added after with their validity, since a
// valid parent check cannot adopt a NOT VALID one.
func (self *contractTableShape) stagedParentSql() (statements []string) {
	return self.stagedParentSqlBy("contract_id")
}

// As stagedParentSql, partitioned by the range of keyColumn, a lowercase
// column name from the code.
func (self *contractTableShape) stagedParentSqlBy(keyColumn string) (statements []string) {
	staged := contractStagedParentName(self.table)
	statements = append(statements, fmt.Sprintf(
		"CREATE TABLE public.%s (LIKE public.%s INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING INDEXES INCLUDING STORAGE INCLUDING COMPRESSION INCLUDING COMMENTS) PARTITION BY RANGE (%s)",
		quoteIdentifier(staged),
		quoteIdentifier(self.table),
		keyColumn,
	))
	for _, check := range self.checks {
		statements = append(statements, fmt.Sprintf(
			"ALTER TABLE public.%s ADD CONSTRAINT %s %s",
			quoteIdentifier(staged),
			quoteIdentifier(check.name),
			check.definition,
		))
	}
	for _, column := range self.columns {
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

// LIKE names the cloned indexes and not-null constraints after the staged
// table. These renames give each index its published name plus the staged
// suffix, matched by definition, and each not-null constraint its published
// name.
func contractStagedRenameSql(shape *contractTableShape, staged *contractTableShape) (statements []string, err error) {
	for _, stagedIndex := range staged.indexes {
		var match *contractIndex
		for i := range shape.indexes {
			index := &shape.indexes[i]
			if index.primary == stagedIndex.primary && contractIndexShape(index.definition) == contractIndexShape(stagedIndex.definition) {
				if match != nil {
					return nil, fmt.Errorf("%s indexes %s and %s have the same definition", shape.table, match.name, index.name)
				}
				match = index
			}
		}
		if match == nil {
			return nil, fmt.Errorf("%s index %s has no counterpart on %s", staged.table, stagedIndex.name, shape.table)
		}
		if name := match.name + contractStagedIndexSuffix; stagedIndex.name != name {
			statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(stagedIndex.name), quoteIdentifier(name)))
		}
	}
	for _, column := range shape.columns {
		i := slices.IndexFunc(staged.columns, func(stagedColumn contractColumn) bool {
			return stagedColumn.name == column.name
		})
		if i < 0 || column.notNullName == "" || staged.columns[i].notNullName == "" || staged.columns[i].notNullName == column.notNullName {
			continue
		}
		statements = append(statements, fmt.Sprintf(
			"ALTER TABLE public.%s RENAME CONSTRAINT %s TO %s",
			quoteIdentifier(staged.table),
			quoteIdentifier(staged.columns[i].notNullName),
			quoteIdentifier(column.notNullName),
		))
	}
	return
}

// Stages table's partitioned parent in one transaction.
func stageContractParent(ctx context.Context, shape *contractTableShape) error {
	return contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
		for _, statement := range shape.stagedParentSql() {
			RaisePgResult(tx.Exec(ctx, statement))
		}
		staged := readContractTableShape(ctx, tx, contractStagedParentName(shape.table))
		statements, err := contractStagedRenameSql(shape, staged)
		Raise(err)
		for _, statement := range statements {
			RaisePgResult(tx.Exec(ctx, statement))
		}
	})
}

// The swap's ATTACH must adopt a valid legacy index for every parent index; a
// parent index without a match is built on the whole table under the swap's
// lock. An empty clone of the table has the same index trees (LIKE), so attach
// one to the staged parent just below the cutover week, in a transaction that
// is rolled back, and check that every parent index adopted one of the clone's
// indexes instead of building its own.
func probeContractLegacyAttach(ctx context.Context, table string, floor Id) (problems []string) {
	staged := contractStagedParentName(table)
	probe := table + "_attach_probe"
	errProbed := errors.New("probed")
	err := contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
		RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = '60s'`))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			"CREATE TABLE public.%s (LIKE public.%s INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING INDEXES)",
			quoteIdentifier(probe),
			quoteIdentifier(table),
		)))
		cloned := map[string]bool{}
		for _, index := range readContractTableShape(ctx, tx, probe).indexes {
			cloned[index.name] = true
		}
		below := ContractIdFloor(ContractIdTime(floor).Add(-time.Millisecond))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			"ALTER TABLE public.%s ATTACH PARTITION public.%s FOR VALUES FROM (%s) TO (%s)",
			quoteIdentifier(staged),
			quoteIdentifier(probe),
			contractIdLiteral(below),
			contractIdLiteral(floor),
		)))
		result, err := tx.Query(
			ctx,
			`
			SELECT parent_index.relname, COALESCE(child_index.relname, '')
			FROM pg_index parent
			JOIN pg_class parent_index ON parent_index.oid = parent.indexrelid
			LEFT JOIN pg_inherits i ON i.inhparent = parent.indexrelid
				AND i.inhrelid IN (SELECT indexrelid FROM pg_index WHERE indrelid = to_regclass($2))
			LEFT JOIN pg_class child_index ON child_index.oid = i.inhrelid
			WHERE parent.indrelid = to_regclass($1)
			ORDER BY parent_index.relname
			`,
			"public."+staged,
			"public."+probe,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var parentIndex, childIndex string
				Raise(result.Scan(&parentIndex, &childIndex))
				if !cloned[childIndex] {
					problems = append(problems, fmt.Sprintf("%s index %s would be built on %s at the swap instead of adopting a legacy index", staged, parentIndex, table))
				}
			}
		})
		panic(errProbed)
	})
	if !errors.Is(err, errProbed) {
		problems = append(problems, fmt.Sprintf("attach probe of %s: %v", table, err))
	}
	return
}

// The statements that create table's partition for weekStart under parent: an
// empty table with parentShape's columns and checks, the given storage
// settings and TRUNCATE guards, then ATTACH. ATTACH takes only SHARE UPDATE
// EXCLUSIVE on the parent, builds the parent's indexes on the empty table and
// clones the parent's row triggers.
func contractWeekPartitionSql(parent string, parentShape *contractTableShape, table string, weekStart time.Time, reloptions []string, guards *contractTableShape, guardsFrom string) (statements []string, err error) {
	name := ContractPartitionName(table, weekStart)
	leaf := &contractTableShape{
		table:      name,
		columns:    parentShape.columns,
		checks:     parentShape.checks,
		reloptions: reloptions,
	}
	statements = append(statements, leaf.createTableSql(name, false)...)
	statements = append(statements, leaf.reloptionsSql(name)...)
	if guards != nil {
		guardStatements, guardErr := guards.truncateGuardsSql(guardsFrom, name)
		if guardErr != nil {
			return nil, guardErr
		}
		statements = append(statements, guardStatements...)
	}
	statements = append(statements, fmt.Sprintf(
		"ALTER TABLE public.%s ATTACH PARTITION public.%s FOR VALUES FROM (%s) TO (%s)",
		quoteIdentifier(parent),
		quoteIdentifier(name),
		contractIdLiteral(ContractIdFloor(weekStart)),
		contractIdLiteral(ContractIdFloor(weekStart.Add(contractPartitionWeek))),
	))
	return
}

// The cutover week of a prepared legacy bound check.
func (self *contractTableShape) legacyBoundWeek() (cutoverWeek time.Time, err error) {
	groups := contractLegacyBoundPattern.FindStringSubmatch(self.legacyBound.definition)
	if groups == nil {
		return time.Time{}, fmt.Errorf("%s has an unexpected %s: %s", self.table, self.legacyBound.name, self.legacyBound.definition)
	}
	floor, err := ParseId(groups[1])
	if err != nil {
		return time.Time{}, err
	}
	return ContractIdTime(floor), nil
}

func prepareContractPartitionCutover(ctx context.Context, cutoverWeek time.Time, dryRun bool, logf func(string, ...any), settings *ContractPartitionCutoverSettings) error {
	if cutoverWeek.IsZero() {
		return errors.New("the cutover week is required to prepare")
	}
	if cutoverWeek.Before(NowUtc().Add(24 * time.Hour)) {
		return fmt.Errorf("the cutover week %s must start at least 24h from now, to leave time for validation and the swap", cutoverWeek.UTC().Format(time.DateOnly))
	}
	return prepareContractPartitionCutoverForWeek(ctx, cutoverWeek, dryRun, logf, settings)
}

// The statement that adds table's legacy bound, NOT VALID: a brief ACCESS
// EXCLUSIVE lock, without a scan.
func contractLegacyBoundAddSql(table string, floor Id) string {
	return fmt.Sprintf(
		"ALTER TABLE public.%s ADD CONSTRAINT %s CHECK (contract_id < %s) NOT VALID",
		quoteIdentifier(table),
		quoteIdentifier(contractLegacyBoundName(table)),
		contractIdLiteral(floor),
	)
}

// Runs one prepare step on table's lock scope: up to StepAttempts admission
// reads, then the statements once under their own lock timeout. A scope that
// stays busy, or a lock wait that times out, defers the step to the next run
// (the reason is returned, nothing changed); any other failure is an error.
func contractPreparedStep(ctx context.Context, scope *contractLockScope, statements []string, settings *ContractPartitionCutoverSettings, logf func(string, ...any)) (deferred string, err error) {
	err = contractPartitionRetryWhen(ctx, scope, func() error {
		return contractPartitionExec(ctx, statements)
	}, settings.StepAttempts, settings.StepRetryInterval, logf)
	var busy *ContractPartitionBusyError
	switch {
	case err == nil:
		return "", nil
	case errors.As(err, &busy):
		return strings.Join(busy.Blockers, "; "), nil
	case contractPartitionLockBusy(err):
		return err.Error(), nil
	default:
		return "", err
	}
}

func prepareContractPartitionCutoverForWeek(ctx context.Context, cutoverWeek time.Time, dryRun bool, logf func(string, ...any), settings *ContractPartitionCutoverSettings) error {
	if IsContractPartitioned(ctx) {
		return errors.New("the contract tables are already partitioned")
	}
	cutoverWeek = cutoverWeek.UTC()
	if !ContractPartitionWeekStart(cutoverWeek).Equal(cutoverWeek) {
		return fmt.Errorf("the cutover week must start Monday 00:00 UTC: %s", cutoverWeek)
	}
	floor := ContractIdFloor(cutoverWeek)
	if err := checkContractIdFloorFunction(ctx, cutoverWeek, floor); err != nil {
		return err
	}
	logf("cutover week %s: contract ids below %s stay in the legacy partitions", cutoverWeek.Format(time.DateOnly), floor)
	logf("finalize must complete before %s; unfinished preparation is then abandoned",
		cutoverWeek.Add(-contractCutoverAbandonMargin).Format(time.RFC3339),
	)

	shapes := readContractShapes(ctx, contractTableName)
	staged := readContractShapes(ctx, contractStagedParentName)
	var problems []string
	for _, table := range ContractPartitionTables {
		shape := shapes[table]
		if shape == nil {
			problems = append(problems, fmt.Sprintf("%s does not exist", table))
			continue
		}
		problems = append(problems, shape.cutoverProblems()...)
		if shape.legacyBound != nil {
			if week, err := shape.legacyBoundWeek(); err != nil {
				problems = append(problems, err.Error())
			} else if !week.Equal(cutoverWeek) {
				problems = append(problems, fmt.Sprintf("%s is prepared for the cutover week %s; abort it first", table, week.Format(time.DateOnly)))
			}
		}
		if stagedShape := staged[table]; stagedShape != nil {
			problems = append(problems, contractStagedProblems(shape, stagedShape)...)
		}
	}
	if 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the cutover", len(problems))
	}

	// table: why its legacy bound waits for the next run
	var deferred []string
	for _, table := range ContractPartitionTables {
		shape := shapes[table]
		stagedName := contractStagedParentName(table)
		if staged[table] == nil {
			if dryRun {
				logf("would stage %s, then give its indexes their published names plus %q:\n%s", stagedName, contractStagedIndexSuffix, strings.Join(shape.stagedParentSql(), ";\n"))
			} else {
				if err := stageContractParent(ctx, shape); err != nil {
					return fmt.Errorf("stage %s: %w", stagedName, err)
				}
				logf("staged %s with %d indexes", stagedName, len(shape.indexes))
			}
		}
		var stagedShape *contractTableShape
		if !dryRun {
			MaintenanceDb(ctx, func(conn PgConn) {
				stagedShape = readContractTableShape(ctx, conn, stagedName)
			})
		} else {
			stagedShape = staged[table]
			if stagedShape == nil {
				stagedShape = shape
			}
		}
		existing := map[string]bool{}
		if staged[table] != nil || !dryRun {
			partitions, err := listContractPartitions(ctx, stagedName)
			if err != nil {
				return err
			}
			for _, partition := range partitions {
				existing[partition.name] = true
			}
		}
		for week := 0; week <= ContractPartitionAheadWeeks; week += 1 {
			weekStart := cutoverWeek.Add(time.Duration(week) * contractPartitionWeek)
			name := ContractPartitionName(table, weekStart)
			if existing[name] {
				continue
			}
			statements, err := contractWeekPartitionSql(stagedName, stagedShape, table, weekStart, shape.reloptions, shape, table)
			if err != nil {
				return err
			}
			if dryRun {
				logf("would create %s:\n%s", name, strings.Join(statements, ";\n"))
				continue
			}
			if err := contractPartitionExec(ctx, append([]string{`SET LOCAL lock_timeout = '5s'`}, statements...)); err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
			logf("created %s", name)
		}

		bound := contractLegacyBoundName(table)
		// Neither step queues behind an autovacuum or a long transaction on
		// the table: a busy table is deferred to the next run, and the other
		// tables go on.
		if shape.legacyBound == nil {
			statement := contractLegacyBoundAddSql(table, floor)
			if dryRun {
				logf("would add %s: %s", bound, statement)
			} else {
				scope := &contractLockScope{relations: []string{"public." + table}, request: contractLockAccessExclusive, settings: settings.LockSettings}
				reason, err := contractPreparedStep(ctx, scope, []string{contractLockTimeoutSql(settings.LockTimeout), statement}, settings, logf)
				if err != nil {
					return fmt.Errorf("add %s: %w", bound, err)
				}
				if reason != "" {
					logf("deferred adding %s to the next run: %s", bound, reason)
					deferred = append(deferred, fmt.Sprintf("%s: %s", table, reason))
					continue
				}
				logf("added %s", bound)
			}
		}
		if shape.legacyBound == nil || !shape.legacyBound.validated {
			statement := fmt.Sprintf("ALTER TABLE public.%s VALIDATE CONSTRAINT %s", quoteIdentifier(table), quoteIdentifier(bound))
			if dryRun {
				logf("would validate %s (scans %s under SHARE UPDATE EXCLUSIVE)", bound, table)
				continue
			}
			logf("validating %s: scans %s under SHARE UPDATE EXCLUSIVE; reads and writes continue", bound, table)
			validateStart := time.Now()
			scope := &contractLockScope{relations: []string{"public." + table}, request: contractLockShareUpdateExclusive, settings: settings.LockSettings}
			reason, err := contractPreparedStep(ctx, scope, []string{contractLockTimeoutSql(settings.ValidateLockTimeout), `SET LOCAL statement_timeout = 0`, liftTransactionTimeoutInTxSql, statement}, settings, logf)
			if err != nil {
				return fmt.Errorf("validate %s: %w", bound, err)
			}
			if reason != "" {
				logf("deferred validating %s to the next run: %s", bound, reason)
				deferred = append(deferred, fmt.Sprintf("%s: %s", table, reason))
				continue
			}
			logf("validated %s in %s", bound, time.Since(validateStart).Round(time.Second))
		}
	}
	if dryRun {
		logf("dry run: nothing changed")
		return nil
	}
	if 0 < len(deferred) {
		return &ContractPartitionDeferredError{Deferred: deferred}
	}
	logf("prepared; run --finalize before %s", cutoverWeek.Add(-contractCutoverAbandonMargin).Format(time.RFC3339))
	return nil
}

// The SQL function and the Go floor must agree on every bound.
func checkContractIdFloorFunction(ctx context.Context, at time.Time, floor Id) (err error) {
	MaintenanceDb(ctx, func(conn PgConn) {
		var exists bool
		Raise(conn.QueryRow(ctx, `SELECT to_regprocedure('public.contract_id_floor(timestamp with time zone)') IS NOT NULL`).Scan(&exists))
		if !exists {
			err = errors.New("contract_id_floor() is missing; apply the database migrations first")
			return
		}
		var value Id
		Raise(conn.QueryRow(ctx, `SELECT contract_id_floor($1)`, at).Scan(&value))
		if value != floor {
			err = fmt.Errorf("contract_id_floor(%s) = %s, want %s", at.Format(time.RFC3339), value, floor)
		}
	})
	return
}

// Differences between a staged parent and the table it will replace.
func contractStagedProblems(shape *contractTableShape, staged *contractTableShape) (problems []string) {
	if staged.relkind != "p" {
		problems = append(problems, fmt.Sprintf("%s is not partitioned", staged.table))
		return
	}
	if !contractTableShapeColumnsEqual(shape.columns, staged.columns) {
		problems = append(problems, fmt.Sprintf("%s columns differ from %s; abort and prepare again", staged.table, shape.table))
	}
	checks := func(checks []contractConstraint) (definitions []string) {
		for _, check := range checks {
			definitions = append(definitions, check.name+" "+strings.TrimSuffix(check.definition, " NOT VALID"))
		}
		return
	}
	if !slices.Equal(checks(shape.checks), checks(staged.checks)) {
		problems = append(problems, fmt.Sprintf("%s checks differ from %s; abort and prepare again", staged.table, shape.table))
	}
	return
}

// Stages parent indexes for indexes added to the table since prepare and drops
// staged indexes the table no longer has. The staged partitions are empty, so
// each build is instant.
func contractStagedIndexSyncSql(shape *contractTableShape, staged *contractTableShape) (statements []string, problems []string) {
	stagedName := staged.table
	for _, index := range shape.indexes {
		stagedIndex := contractTableShapeIndex(staged, index.name+contractStagedIndexSuffix)
		if stagedIndex == nil {
			if index.primary {
				problems = append(problems, fmt.Sprintf("%s has no staged primary key", stagedName))
				continue
			}
			definition, _ := contractRetargetIndex(index.definition, index.name+contractStagedIndexSuffix, stagedName)
			statements = append(statements, definition)
		} else if contractIndexShape(stagedIndex.definition) != contractIndexShape(index.definition) {
			problems = append(problems, fmt.Sprintf("%s index %s differs from %s; abort and prepare again: %s <> %s", stagedName, stagedIndex.name, index.name, stagedIndex.definition, index.definition))
		}
	}
	for _, stagedIndex := range staged.indexes {
		name, ok := strings.CutSuffix(stagedIndex.name, contractStagedIndexSuffix)
		if !ok || contractTableShapeIndex(shape, name) == nil {
			if stagedIndex.primary {
				problems = append(problems, fmt.Sprintf("%s primary key %s has no counterpart", stagedName, stagedIndex.name))
				continue
			}
			statements = append(statements, fmt.Sprintf("DROP INDEX public.%s", quoteIdentifier(stagedIndex.name)))
		}
	}
	return
}

// Foreign keys into the contract tables from other tables survive the swap as
// NOT VALID constraints on the parents, validated after it. Foreign keys
// between contract tables are dropped: their partitions are dropped together.
func contractExternalForeignKeys(shapes map[string]*contractTableShape) (external []contractForeignKey, internal []contractForeignKey) {
	for _, table := range ContractPartitionTables {
		for _, foreignKey := range shapes[table].referencedBy {
			if slices.Contains(ContractPartitionTables, foreignKey.table) {
				internal = append(internal, foreignKey)
			} else {
				external = append(external, foreignKey)
			}
		}
	}
	return
}

// The swap transaction with the default lock timeout.
func contractSwapSql(shapes map[string]*contractTableShape, staged map[string]*contractTableShape, floor Id) (statements []string) {
	return contractSwapSqlWithin(shapes, staged, floor, DefaultContractPartitionCutoverSettings().LockTimeout)
}

// The relations the swap locks: every table and its staged parent (with the
// staged weekly partitions below it), and the tables of outside foreign keys.
func contractSwapLockRelations(shapes map[string]*contractTableShape) (relations []string) {
	for _, table := range ContractPartitionTables {
		relations = append(relations, "public."+table, "public."+contractStagedParentName(table))
	}
	external, _ := contractExternalForeignKeys(shapes)
	for _, foreignKey := range external {
		relations = append(relations, "public."+foreignKey.table)
	}
	return
}

// The swap transaction. Every table is locked first, in table order, so the
// renames and attaches see no concurrent statement.
func contractSwapSqlWithin(shapes map[string]*contractTableShape, staged map[string]*contractTableShape, floor Id, lockTimeout time.Duration) (statements []string) {
	// catalog work only: a statement that would scan or build is cut short,
	// and each lock wait queues application statements, which give up after
	// PgLockTimeout, for at most lockTimeout
	statements = append(statements, contractLockTimeoutSql(lockTimeout), `SET LOCAL statement_timeout = '30s'`)
	external, internal := contractExternalForeignKeys(shapes)
	locked := []string{}
	for _, table := range ContractPartitionTables {
		locked = append(locked, "public."+quoteIdentifier(table), "public."+quoteIdentifier(contractStagedParentName(table)))
	}
	for _, foreignKey := range external {
		locked = append(locked, "public."+quoteIdentifier(foreignKey.table))
	}
	statements = append(statements, fmt.Sprintf("LOCK TABLE %s IN ACCESS EXCLUSIVE MODE", strings.Join(locked, ", ")))
	for _, foreignKey := range append(internal, external...) {
		statements = append(statements, fmt.Sprintf(
			"ALTER TABLE public.%s DROP CONSTRAINT %s",
			quoteIdentifier(foreignKey.table),
			quoteIdentifier(foreignKey.name),
		))
	}
	for _, table := range ContractPartitionTables {
		shape := shapes[table]
		legacy := ContractLegacyPartitionName(table)
		stagedName := contractStagedParentName(table)
		// the parent's row triggers are cloned onto the legacy partition, and
		// a parent statement trigger fires for every partition; only the
		// TRUNCATE guards stay, for a TRUNCATE that names the partition
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
		for _, index := range staged[table].indexes {
			name, _ := strings.CutSuffix(index.name, contractStagedIndexSuffix)
			statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(index.name), quoteIdentifier(name)))
		}
		for _, trigger := range shape.triggers {
			statements = append(statements, trigger.definition)
			statements = append(statements, contractTriggerEnabledSql(table, trigger)...)
		}
	}
	for _, foreignKey := range external {
		statements = append(statements, fmt.Sprintf(
			"ALTER TABLE public.%s ADD CONSTRAINT %s %s NOT VALID",
			quoteIdentifier(foreignKey.table),
			quoteIdentifier(foreignKey.name),
			foreignKey.definition,
		))
	}
	return
}

// A recreated trigger is enabled; restore a disabled or replica-only state.
func contractTriggerEnabledSql(table string, trigger contractTrigger) []string {
	var mode string
	switch trigger.enabled {
	case "D":
		mode = "DISABLE TRIGGER"
	case "R":
		mode = "ENABLE REPLICA TRIGGER"
	case "A":
		mode = "ENABLE ALWAYS TRIGGER"
	default:
		return nil
	}
	return []string{fmt.Sprintf("ALTER TABLE public.%s %s %s", quoteIdentifier(table), mode, quoteIdentifier(trigger.name))}
}

func finalizeContractPartitionCutover(ctx context.Context, dryRun bool, logf func(string, ...any), settings *ContractPartitionCutoverSettings) error {
	return finalizeContractPartitionCutoverWithDeadline(ctx, true, dryRun, logf, settings)
}

func finalizeContractPartitionCutoverWithDeadline(ctx context.Context, deadline bool, dryRun bool, logf func(string, ...any), settings *ContractPartitionCutoverSettings) error {
	if IsContractPartitioned(ctx) {
		return errors.New("the contract tables are already partitioned")
	}
	shapes := readContractShapes(ctx, contractTableName)
	staged := readContractShapes(ctx, contractStagedParentName)
	var problems []string
	var cutoverWeek time.Time
	var syncStatements []string
	for _, table := range ContractPartitionTables {
		shape := shapes[table]
		if shape == nil {
			problems = append(problems, fmt.Sprintf("%s does not exist", table))
			continue
		}
		problems = append(problems, shape.cutoverProblems()...)
		if shape.legacyBound == nil || !shape.legacyBound.validated {
			problems = append(problems, fmt.Sprintf("%s is not prepared: its legacy bound is missing or not validated", table))
			continue
		}
		week, err := shape.legacyBoundWeek()
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if cutoverWeek.IsZero() {
			cutoverWeek = week
		} else if !week.Equal(cutoverWeek) {
			problems = append(problems, fmt.Sprintf("%s is prepared for %s, not %s", table, week.Format(time.DateOnly), cutoverWeek.Format(time.DateOnly)))
		}
		stagedShape := staged[table]
		if stagedShape == nil {
			problems = append(problems, fmt.Sprintf("%s is not staged", contractStagedParentName(table)))
			continue
		}
		problems = append(problems, contractStagedProblems(shape, stagedShape)...)
		statements, indexProblems := contractStagedIndexSyncSql(shape, stagedShape)
		problems = append(problems, indexProblems...)
		syncStatements = append(syncStatements, statements...)
		partitions, err := listContractPartitions(ctx, contractStagedParentName(table))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !slices.ContainsFunc(partitions, func(partition contractPartition) bool {
			return partition.weekStart.Equal(week)
		}) {
			problems = append(problems, fmt.Sprintf("%s has no partition for the cutover week", contractStagedParentName(table)))
		}
	}
	if 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the swap", len(problems))
	}
	if deadline && !NowUtc().Before(cutoverWeek.Add(-contractCutoverFinalizeMargin)) {
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
			staged = readContractShapes(ctx, contractStagedParentName)
		}
	}

	floor := ContractIdFloor(cutoverWeek)
	for _, table := range ContractPartitionTables {
		problems = append(problems, probeContractLegacyAttach(ctx, table, floor)...)
	}
	if 0 < len(problems) {
		for _, problem := range problems {
			logf("problem: %s", problem)
		}
		return fmt.Errorf("%d problem(s) block the swap", len(problems))
	}
	logf("probed the swap: every parent index adopts a legacy index")

	statements := contractSwapSqlWithin(shapes, staged, floor, settings.LockTimeout)
	scope := &contractLockScope{relations: contractSwapLockRelations(shapes), request: contractLockAccessExclusive, settings: settings.LockSettings}
	if dryRun {
		if blockers, err := contractLockAdmission(ctx, scope); err != nil {
			return err
		} else if 0 < len(blockers) {
			logf("the swap would wait now, without queueing, for: %s", strings.Join(blockers, "; "))
		} else {
			logf("the swap would be admitted now: no autovacuum and no transaction older than %s holds a lock on the contract tables", settings.LockSettings.MaxTransactionAge)
		}
		logf("would swap in one transaction:\n%s", strings.Join(statements, ";\n"))
		logf("would preserve extended statistics under the swap locks: rename originals for rollback, then copy their definitions and metadata to the published parents")
		logf("dry run: nothing changed")
		return nil
	}
	swapStart := time.Now()
	var timing contractSwapTiming
	if err := contractPartitionRetryWhen(ctx, scope, func() error {
		return contractPartitionSwap(ctx, statements, &timing)
	}, settings.SwapAttempts, settings.SwapRetryInterval, logf); err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	logf(
		"swapped the contract tables in %s: the swap waited %s for its locks and held them %s (catalog work and commit); contracts from %s go to weekly partitions",
		time.Since(swapStart).Round(time.Millisecond),
		timing.lockWait.Round(time.Millisecond),
		timing.held.Round(time.Millisecond),
		cutoverWeek.Format(time.DateOnly),
	)

	external, _ := contractExternalForeignKeys(shapes)
	return analyzeAndValidateContractParents(ctx, external, logf)
}

// The parents start without statistics, and recreated foreign keys from
// other tables start NOT VALID. Neither step blocks writes for long.
func analyzeAndValidateContractParents(ctx context.Context, external []contractForeignKey, logf func(string, ...any)) error {
	for _, table := range ContractPartitionTables {
		statement := fmt.Sprintf("ANALYZE ONLY public.%s", quoteIdentifier(table))
		if err := contractPartitionExec(ctx, []string{`SET LOCAL statement_timeout = 0`, liftTransactionTimeoutInTxSql, statement}); err != nil {
			return fmt.Errorf("analyze %s: %w", table, err)
		}
	}
	logf("analyzed the parents")
	for _, foreignKey := range external {
		statement := fmt.Sprintf("ALTER TABLE public.%s VALIDATE CONSTRAINT %s", quoteIdentifier(foreignKey.table), quoteIdentifier(foreignKey.name))
		err := contractPartitionExecRetry(ctx, []string{`SET LOCAL lock_timeout = '15s'`, `SET LOCAL statement_timeout = 0`, liftTransactionTimeoutInTxSql, statement}, 10, 10*time.Second, logf)
		if err != nil {
			return fmt.Errorf("validate %s: %w", foreignKey.name, err)
		}
		logf("validated %s", foreignKey.name)
	}
	return nil
}

// Builds rollback from the caller's catalog snapshot. Execution requires the
// caller to hold exclusive locks on every parent and its inherited children.
func contractPartitionRollbackSqlInConn(ctx context.Context, conn contractCatalogQuerier) (statements []string, external []contractForeignKey, err error) {
	parents := readContractShapesInConn(ctx, conn, contractTableName)
	legacies := readContractShapesInConn(ctx, conn, ContractLegacyPartitionName)
	var problems []string
	weekly := map[string][]string{}
	for _, table := range ContractPartitionTables {
		if parents[table] == nil || parents[table].relkind != "p" {
			problems = append(problems, fmt.Sprintf("%s is not partitioned", table))
			continue
		}
		if legacies[table] == nil {
			problems = append(problems, fmt.Sprintf("%s has no legacy partition", table))
			continue
		}
		partitions, err := listContractPartitionsInConn(ctx, conn, table)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for _, partition := range partitions {
			if partition.legacy {
				continue
			}
			var empty bool
			rows, queryErr := conn.Query(ctx, fmt.Sprintf(`SELECT NOT EXISTS (SELECT 1 FROM public.%s)`, quoteIdentifier(partition.name)))
			WithPgResult(rows, queryErr, func() {
				if rows.Next() {
					Raise(rows.Scan(&empty))
				}
			})
			if !empty {
				problems = append(problems, fmt.Sprintf("%s holds rows; the swap can no longer be undone", partition.name))
			}
			weekly[table] = append(weekly[table], partition.name)
		}
	}
	if 0 < len(problems) {
		return nil, nil, fmt.Errorf("rollback preconditions: %s", strings.Join(problems, "; "))
	}

	external, _ = contractExternalForeignKeys(parents)
	locked := []string{}
	for _, foreignKey := range external {
		locked = append(locked, "public."+quoteIdentifier(foreignKey.table))
	}
	if 0 < len(locked) {
		statements = append(statements, fmt.Sprintf("LOCK TABLE %s IN ACCESS EXCLUSIVE MODE", strings.Join(locked, ", ")))
	}
	for _, foreignKey := range external {
		statements = append(statements, fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT %s", quoteIdentifier(foreignKey.table), quoteIdentifier(foreignKey.name)))
	}
	for _, table := range ContractPartitionTables {
		parent := parents[table]
		legacy := legacies[table]
		legacyName := ContractLegacyPartitionName(table)
		for _, trigger := range parent.triggers {
			statements = append(statements, fmt.Sprintf("DROP TRIGGER %s ON public.%s", quoteIdentifier(trigger.name), quoteIdentifier(table)))
		}
		for _, name := range weekly[table] {
			statements = append(statements,
				fmt.Sprintf("ALTER TABLE public.%s DETACH PARTITION public.%s", quoteIdentifier(table), quoteIdentifier(name)),
				fmt.Sprintf("DROP TABLE public.%s", quoteIdentifier(name)),
			)
		}
		statements = append(statements,
			fmt.Sprintf("ALTER TABLE public.%s DETACH PARTITION public.%s", quoteIdentifier(table), quoteIdentifier(legacyName)),
			fmt.Sprintf("DROP TABLE public.%s", quoteIdentifier(table)),
			fmt.Sprintf("ALTER TABLE public.%s RENAME TO %s", quoteIdentifier(legacyName), quoteIdentifier(table)),
			fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT %s", quoteIdentifier(table), quoteIdentifier(contractLegacyBoundName(table))),
		)
		for _, index := range legacy.indexes {
			if name, ok := strings.CutSuffix(index.name, contractLegacyIndexSuffix); ok {
				statements = append(statements, fmt.Sprintf("ALTER INDEX public.%s RENAME TO %s", quoteIdentifier(index.name), quoteIdentifier(name)))
			}
		}
		statisticsStatements, statisticsErr := contractStatisticsRollbackSql(readContractStatistics(ctx, conn, legacyName), readContractStatistics(ctx, conn, table))
		if statisticsErr != nil {
			return nil, nil, statisticsErr
		}
		statements = append(statements, statisticsStatements...)
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
	}
	statements = append(statements, fmt.Sprintf(
		"ALTER TABLE public.contract_close_report ADD CONSTRAINT %s %s NOT VALID",
		quoteIdentifier(contractCloseReportForeignKeyName),
		contractCloseReportForeignKeyDefinition,
	))
	for _, foreignKey := range external {
		statements = append(statements, fmt.Sprintf(
			"ALTER TABLE public.%s ADD CONSTRAINT %s %s NOT VALID",
			quoteIdentifier(foreignKey.table),
			quoteIdentifier(foreignKey.name),
			foreignKey.definition,
		))
	}
	return statements, external, nil
}

// The prepared cutover week, if a cutover is prepared and not finalized.
func preparedContractCutoverWeek(ctx context.Context) (cutoverWeek time.Time, prepared bool) {
	shapes := readContractShapes(ctx, contractTableName)
	staged := readContractShapes(ctx, contractStagedParentName)
	for _, table := range ContractPartitionTables {
		if staged[table] != nil {
			prepared = true
		}
		if shape := shapes[table]; shape != nil && shape.relkind == "r" && shape.legacyBound != nil {
			prepared = true
			if week, err := shape.legacyBoundWeek(); err == nil && (cutoverWeek.IsZero() || week.Before(cutoverWeek)) {
				cutoverWeek = week
			}
		}
	}
	return
}

// Drops each table's staged parent and legacy bound, one table at a time: each
// table waits only for its own quiet moment (contractLockAdmission).
func abortContractPartitionCutover(ctx context.Context, dryRun bool, logf func(string, ...any), settings *ContractPartitionCutoverSettings) error {
	if IsContractPartitioned(ctx) {
		return errors.New("the contract tables are partitioned; use rollback")
	}
	for _, table := range ContractPartitionTables {
		statements := []string{
			fmt.Sprintf("DROP TABLE IF EXISTS public.%s", quoteIdentifier(contractStagedParentName(table))),
			fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT IF EXISTS %s", quoteIdentifier(table), quoteIdentifier(contractLegacyBoundName(table))),
		}
		if dryRun {
			logf("would run:\n%s", strings.Join(statements, ";\n"))
			continue
		}
		scope := &contractLockScope{
			relations: []string{"public." + table, "public." + contractStagedParentName(table)},
			request:   contractLockAccessExclusive,
			settings:  settings.LockSettings,
		}
		if err := contractPartitionExecQuietRetry(ctx, scope, append([]string{contractLockTimeoutSql(settings.LockTimeout)}, statements...), settings.AbortAttempts, settings.AbortRetryInterval, logf); err != nil {
			return fmt.Errorf("abort %s: %w", table, err)
		}
		logf("aborted the cutover of %s", table)
	}
	return nil
}

// URNETWORK_TEST_CONTRACT_PARTITIONS=1 runs every test on partitioned contract
// tables (CONTRACT-PARTITION.md): see Testing_PartitionContracts.
func testContractPartitions() bool {
	switch strings.ToLower(os.Getenv("URNETWORK_TEST_CONTRACT_PARTITIONS")) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// Cuts a freshly migrated, empty test database over to partitioned contract
// tables from the current week: earlier ids go to the legacy partitions and
// current ones to the weekly partitions, as in production after the cutover.
func Testing_PartitionContracts(ctx context.Context) {
	if IsContractPartitioned(ctx) {
		return
	}
	cutoverWeek := ContractPartitionWeekStart(NowUtc())
	Raise(prepareContractPartitionCutoverForWeek(ctx, cutoverWeek, false, func(string, ...any) {}, DefaultContractPartitionCutoverSettings()))
	Raise(finalizeContractPartitionCutoverWithDeadline(ctx, false, false, func(string, ...any) {}, DefaultContractPartitionCutoverSettings()))
}
