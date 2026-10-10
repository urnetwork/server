// Administrative online-index recovery preserves a completed build whose
// caller died before the normal migration success/identity transaction.
package server

import (
	"context"
	"fmt"
)

// Execution metadata does not alter published production/audit SQL or its
// identity. Only explicitly enrolled migrations use this recovery protocol.
type onlineIndexMigrationCompletion struct {
	version    int32
	table      string
	index      string
	definition string
	predicate  string
}

// Enroll one published index without changing its registered DDL or identity.
func (self *OnlineSqlMigration) withIndexCompletion(version int32, table, index, definition, predicate string) *OnlineSqlMigration {
	self.completion = &onlineIndexMigrationCompletion{
		version:    version,
		table:      table,
		index:      index,
		definition: definition,
		predicate:  predicate,
	}
	return self
}

// One direct maintenance connection owns admission, catalog inspection and
// concurrent DDL. A canceled owner never turns an uncertain probe into success.
// Older/out-of-band builders are also detected before any recovery statement.
func executeOnlineSqlMigrationOnConn(ctx context.Context, conn PgConn, migration *OnlineSqlMigration) (returnErr error) {
	execute := func() error {
		return executeOnlineSqlMigration(ctx, migration, func(ctx context.Context, sql string) error {
			_, err := conn.Exec(ctx, sql)
			return err
		})
	}
	completion := migration.completion
	if completion == nil {
		return execute()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The two-integer MIGR namespace serializes enrolled migration owners.
	// MaintenanceDb connects directly, so this session lock spans DDL commits.
	const lockNamespace = int32(0x4d494752)
	var admitted bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::integer,$2::integer)`, lockNamespace, completion.version).Scan(&admitted); err != nil {
		// An unread admission reply cannot prove that the session owns no
		// lock. Dispose it rather than returning uncertain custody to the pool.
		closePgConnection(ctx, conn.Conn())
		return err
	}
	if !admitted {
		return fmt.Errorf("migration %d has an active index recovery owner", completion.version)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), PgCloseTimeout)
		defer cancel()
		var released bool
		err := conn.QueryRow(cleanup, `SELECT pg_advisory_unlock($1::integer,$2::integer)`, lockNamespace, completion.version).Scan(&released)
		if err != nil || !released {
			// Destroy an uncertain session before MaintenanceDb can return it
			// to its pool with another owner's admission lock still attached.
			closePgConnection(cleanup, conn.Conn())
			if returnErr == nil {
				returnErr = fmt.Errorf("migration %d could not release index recovery ownership: %v", completion.version, err)
			}
		}
	}()
	complete, busy, err := completion.inspect(ctx, conn)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if complete {
		return nil
	}
	if busy {
		return fmt.Errorf("migration %d index %s has an active builder or conflicting maintenance owner", completion.version, completion.index)
	}
	if err := execute(); err != nil {
		return err
	}
	complete, _, err = completion.inspect(ctx, conn)
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("migration %d index %s did not reach its exact usable definition", completion.version, completion.index)
	}
	return ctx.Err()
}

// The exact public relation, definition, predicate and all usability flags
// supply completion evidence. Progress plus relation locks protect surviving
// builders, including sessions whose progress details are privilege-masked.
func (self *onlineIndexMigrationCompletion) inspect(ctx context.Context, conn PgConn) (complete, busy bool, err error) {
	var resolved bool
	err = conn.QueryRow(ctx, `
		WITH resolved AS (
		 SELECT to_regclass('public.'||$1) AS table_oid,
		        to_regclass('public.'||$2) AS index_oid
		), target AS (
		 SELECT resolved.*, catalog.indrelid AS actual_table_oid
		 FROM resolved LEFT JOIN pg_index AS catalog ON catalog.indexrelid=resolved.index_oid
		)
		SELECT
		 (to_regclass($1) IS NOT DISTINCT FROM target.table_oid)
		 AND (to_regclass($2) IS NOT DISTINCT FROM target.index_oid),
		 EXISTS(SELECT 1 FROM pg_index AS catalog
		        WHERE catalog.indexrelid=target.index_oid AND catalog.indrelid=target.table_oid
		          AND catalog.indisvalid AND catalog.indisready AND catalog.indislive
		          AND pg_get_indexdef(catalog.indexrelid)=$3
		          AND pg_get_expr(catalog.indpred,catalog.indrelid)=$4),
		 EXISTS(SELECT 1 FROM pg_stat_progress_create_index AS progress
		        WHERE progress.pid<>pg_backend_pid()
		          AND progress.datid=(SELECT oid FROM pg_database WHERE datname=current_database())
		          AND (progress.relid IN (target.table_oid,target.actual_table_oid) OR progress.index_relid=target.index_oid))
		 OR EXISTS(SELECT 1 FROM pg_locks AS held
		           WHERE held.pid IS DISTINCT FROM pg_backend_pid() AND held.locktype='relation'
		             AND held.database=(SELECT oid FROM pg_database WHERE datname=current_database())
		             AND held.relation IN (target.table_oid,target.index_oid,target.actual_table_oid)
		             AND held.granted AND held.mode IN ('ShareUpdateExclusiveLock','ShareLock','ShareRowExclusiveLock','ExclusiveLock','AccessExclusiveLock'))
		FROM target`, self.table, self.index, self.definition, self.predicate).Scan(&resolved, &complete, &busy)
	if err == nil && !resolved {
		err = fmt.Errorf("migration %d recovery search path does not resolve its exact public objects", self.version)
	}
	return
}
