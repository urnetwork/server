// Rollback admits destructive work only from state read under the same locks
// that protect the detach/drop transaction. Dry-run plans are advisory.
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Per-operation budgets; callers customize a copy of the defaults. Durations
// must be positive (database timeouts at least one millisecond), and attempts nonzero.
type ContractPartitionRollbackSettings struct {
	// Admission policy before requesting exclusive locks.
	LockSettings ContractPartitionLockSettings
	// Maximum wait for each relation lock, to bound queued application traffic.
	LockTimeout time.Duration
	// Maximum execution time of each catalog or emptiness statement.
	StatementTimeout time.Duration
	// Attempts and delay bound retries while contract tables remain busy.
	MaxAttempts   int
	RetryInterval time.Duration
	// Foreign-key validation follows the committed swap and can scan legacy rows.
	ValidationLockTimeout   time.Duration
	ValidationMaxAttempts   int
	ValidationRetryInterval time.Duration
}

// Preserves the existing operator rollback budgets without process-wide state.
func DefaultContractPartitionRollbackSettings() *ContractPartitionRollbackSettings {
	return &ContractPartitionRollbackSettings{
		LockSettings: *DefaultContractPartitionLockSettings(),
		LockTimeout:  3 * time.Second, StatementTimeout: 30 * time.Second,
		MaxAttempts: 60, RetryInterval: 10 * time.Second,
		ValidationLockTimeout: 15 * time.Second, ValidationMaxAttempts: 10,
		ValidationRetryInterval: 10 * time.Second,
	}
}

// Locks parents recursively before discovering leaves, checking emptiness and
// planning any drop. A retry repeats that entire transaction with fresh reads.
func rollbackContractPartitionCutover(ctx context.Context, dryRun bool, logf func(string, ...any), settings *ContractPartitionRollbackSettings) error {
	if settings == nil {
		settings = DefaultContractPartitionRollbackSettings()
	}
	if settings.LockSettings.MaxTransactionAge <= 0 || settings.LockTimeout < time.Millisecond || settings.StatementTimeout < time.Millisecond ||
		settings.ValidationLockTimeout < time.Millisecond || settings.MaxAttempts <= 0 ||
		settings.ValidationMaxAttempts <= 0 || settings.RetryInterval <= 0 || settings.ValidationRetryInterval <= 0 {
		return errors.New("rollback requires positive lock, statement and retry budgets")
	}
	if !IsContractPartitioned(ctx) {
		return errors.New("the contract tables are not partitioned")
	}
	tableNames := []string{}
	for _, table := range ContractPartitionTables {
		tableNames = append(tableNames, "public."+quoteIdentifier(table))
	}
	lockStatements := []string{
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.LockTimeout.Milliseconds()),
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.StatementTimeout.Milliseconds()),
		fmt.Sprintf("LOCK TABLE %s IN ACCESS EXCLUSIVE MODE", strings.Join(tableNames, ", ")),
	}
	if dryRun {
		var statements []string
		var err error
		MaintenanceDb(ctx, func(conn PgConn) {
			statements, _, err = contractPartitionRollbackSqlInConn(ctx, conn)
		})
		if err != nil {
			return err
		}
		logf("would lock and recheck every partition, then roll back in one transaction:\n%s", strings.Join(append(lockStatements, statements...), ";\n"))
		logf("dry run: nothing changed")
		return nil
	}
	var external []contractForeignKey
	err := contractPartitionRetryWhen(ctx, &settings.LockSettings, func() error {
		return contractPartitionTx(ctx, func(tx PgTx) {
			for _, statement := range lockStatements {
				RaisePgResult(tx.Exec(ctx, statement))
			}
			// Read committed takes these snapshots after lock acquisition, including
			// writers and partition attachments that committed while we waited.
			statements, foreignKeys, err := contractPartitionRollbackSqlInConn(ctx, tx)
			Raise(err)
			for _, statement := range statements {
				RaisePgResult(tx.Exec(ctx, statement))
			}
			external = foreignKeys
		})
	}, settings.MaxAttempts, settings.RetryInterval, logf)
	if err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	logf("rolled back: the contract tables are plain tables again")
	for _, foreignKey := range external {
		statement := fmt.Sprintf("ALTER TABLE public.%s VALIDATE CONSTRAINT %s", quoteIdentifier(foreignKey.table), quoteIdentifier(foreignKey.name))
		if err := contractPartitionExecRetry(ctx, []string{
			fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.ValidationLockTimeout.Milliseconds()),
			`SET LOCAL statement_timeout = 0`, liftTransactionTimeoutInTxSql, statement,
		}, settings.ValidationMaxAttempts, settings.ValidationRetryInterval, logf); err != nil {
			return fmt.Errorf("validate %s: %w", foreignKey.name, err)
		}
	}
	logf("%s is restored NOT VALID; validate it off-peak with: ALTER TABLE contract_close_report VALIDATE CONSTRAINT %s",
		contractCloseReportForeignKeyName, contractCloseReportForeignKeyName)
	return nil
}
