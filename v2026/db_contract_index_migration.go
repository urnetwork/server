package server

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"time"
)

// Index migrations on the contract tables (CONTRACT-PARTITION.md "Correctness
// blockers", migrations). CREATE INDEX CONCURRENTLY cannot run on a
// partitioned parent, and a plain CREATE INDEX on one builds every partition
// under a lock that stops writes, so once the contract tables are partitioned
// a new index is built the way UpgradeClientReliabilitySecondaryIndex builds
// one:
//
//  1. CREATE INDEX IF NOT EXISTS ... ON ONLY the parent: a catalog-only shell,
//     invalid until every partition has a valid attached leaf;
//  2. per partition, CREATE INDEX CONCURRENTLY on the partition, then ALTER
//     INDEX ... ATTACH PARTITION; a partition that already has an attached
//     leaf is skipped, and an invalid leftover of an interrupted build is
//     dropped and built again;
//  3. PostgreSQL marks the shell valid once every partition has one.
//
// A partition created after step 1 builds its leaf of the shell itself when it
// is attached. On a plain table, before the cutover, the migration runs its
// own CREATE INDEX CONCURRENTLY IF NOT EXISTS, after dropping an invalid
// leftover of the same name. Either way the migration fails unless the index
// ends valid, so the runner never records a version whose index is unusable.
// A prepared cutover's staged parent picks the new index up at the swap
// (contractStagedIndexSyncSql).
type contractIndexMigration struct {
	table string
	index string
	// what follows the table in CREATE INDEX: the method, keys, INCLUDE and
	// WHERE, e.g. "(close_time, contract_id) WHERE outcome IS NULL"
	shape string
}

// The online migration that adds index to one of the contract tables. Its
// published SQL is the plain-table form, which the schema audit replays; the
// partitioned build is execution metadata and does not change its identity.
func newContractIndexMigration(table string, index string, shape string) *OnlineSqlMigration {
	if !slices.Contains(ContractPartitionTables, table) {
		panic(fmt.Errorf("%s is not a contract table", table))
	}
	migration := newOnlineSqlMigration(
		fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s %s", index, table, shape),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s %s", index, table, shape),
	)
	migration.contractIndex = &contractIndexMigration{
		table: table,
		index: index,
		shape: shape,
	}
	return migration
}

// Attaching a leaf and creating the shell take brief locks on the parent; a
// statement that waits longer gives up and is tried again, so writers never
// queue behind it for more than this.
const contractIndexMigrationLockTimeout = 5 * time.Second

const contractIndexMigrationLockAttempts = 24

const contractIndexMigrationLockRetryWait = 5 * time.Second

// The name of the leaf a migration builds on a partition: the legacy
// partition's leaves carry the published name and the legacy suffix, as the
// cutover leaves them, and a weekly partition's leaf takes the partition's
// name in place of the table's. A name longer than PostgreSQL keeps ends in a
// hash of the whole name instead, so it stays unique and is never truncated
// by the server.
func (self *contractIndexMigration) leafName(partition string) string {
	var name string
	if _, _, legacy, ok := ParseContractPartitionName(partition); ok && legacy {
		name = self.index + contractLegacyIndexSuffix
	} else if suffix, ok := strings.CutPrefix(self.index, self.table+"_"); ok {
		name = partition + "_" + suffix
	} else {
		name = partition + "_" + self.index
	}
	if len(name) <= pgMaxIdentifierLength {
		return name
	}
	hash := fnv.New32a()
	hash.Write([]byte(name))
	return fmt.Sprintf("%s_%08x", name[:pgMaxIdentifierLength-9], hash.Sum32())
}

func (self *contractIndexMigration) execute(ctx context.Context, conn PgConn, executePlain func() error) error {
	var relkind string
	if err := conn.QueryRow(
		ctx,
		`SELECT coalesce((SELECT relkind::text FROM pg_class WHERE oid = to_regclass($1)), '')`,
		"public."+self.table,
	).Scan(&relkind); err != nil {
		return err
	}
	switch relkind {
	case "r":
		if err := self.dropInvalidLeftover(ctx, conn, self.table, self.index); err != nil {
			return err
		}
		if err := executePlain(); err != nil {
			return err
		}
	case "p":
		if err := self.executePartitioned(ctx, conn); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s is not a table (relkind %q)", self.table, relkind)
	}
	var valid bool
	if err := conn.QueryRow(
		ctx,
		`
		SELECT coalesce((
			SELECT index_state.indisvalid AND index_state.indisready
			FROM pg_index index_state
			WHERE index_state.indexrelid = to_regclass($1) AND index_state.indrelid = to_regclass($2)
		), false)
		`,
		"public."+self.index,
		"public."+self.table,
	).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("index %s on %s is not valid after its migration; rerun to resume", self.index, self.table)
	}
	return nil
}

func (self *contractIndexMigration) executePartitioned(ctx context.Context, conn PgConn) error {
	if err := contractIndexMigrationExecRetry(ctx, conn, fmt.Sprintf(
		"CREATE INDEX IF NOT EXISTS %s ON ONLY public.%s %s",
		quoteIdentifier(self.index),
		quoteIdentifier(self.table),
		self.shape,
	)); err != nil {
		return fmt.Errorf("create the %s shell on %s: %w", self.index, self.table, err)
	}

	var partitions []string
	rows, err := conn.Query(
		ctx,
		`
		SELECT partition_class.relname
		FROM pg_inherits partition
		INNER JOIN pg_class partition_class ON partition_class.oid = partition.inhrelid
		WHERE partition.inhparent = to_regclass($1)
		ORDER BY partition_class.relname
		`,
		"public."+self.table,
	)
	if err != nil {
		return err
	}
	for rows.Next() {
		var partition string
		if err := rows.Scan(&partition); err != nil {
			rows.Close()
			return err
		}
		partitions = append(partitions, partition)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, partition := range partitions {
		var attached bool
		if err := conn.QueryRow(
			ctx,
			`
			SELECT EXISTS (
				SELECT 1
				FROM pg_inherits leaf
				INNER JOIN pg_index leaf_index ON leaf_index.indexrelid = leaf.inhrelid
				WHERE leaf.inhparent = to_regclass($1) AND leaf_index.indrelid = to_regclass($2)
			)
			`,
			"public."+self.index,
			"public."+partition,
		).Scan(&attached); err != nil {
			return err
		}
		if attached {
			continue
		}
		leaf := self.leafName(partition)
		if err := self.dropInvalidLeftover(ctx, conn, partition, leaf); err != nil {
			return err
		}
		// CONCURRENTLY cannot run in a transaction block, so this is its own
		// autocommit statement
		if _, err := conn.Exec(ctx, fmt.Sprintf(
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON public.%s %s",
			quoteIdentifier(leaf),
			quoteIdentifier(partition),
			self.shape,
		)); err != nil {
			return fmt.Errorf("build %s on %s: %w", leaf, partition, err)
		}
		if err := contractIndexMigrationExecRetry(ctx, conn, fmt.Sprintf(
			"ALTER INDEX public.%s ATTACH PARTITION public.%s",
			quoteIdentifier(self.index),
			quoteIdentifier(leaf),
		)); err != nil {
			return fmt.Errorf("attach %s to %s: %w", leaf, self.index, err)
		}
	}
	return nil
}

// Drops the invalid index name on table, the leftover of an interrupted
// concurrent build, so the build can run again; IF NOT EXISTS would keep it.
// An index of that name on another relation, or one a build is still working
// on, stops the migration instead.
func (self *contractIndexMigration) dropInvalidLeftover(ctx context.Context, conn PgConn, table string, name string) error {
	var exists bool
	var onTable bool
	var valid bool
	var building bool
	if err := conn.QueryRow(
		ctx,
		`
		SELECT
			index_state.indexrelid IS NOT NULL,
			coalesce(index_state.indrelid = to_regclass($2), false),
			coalesce(index_state.indisvalid, false),
			EXISTS (
				SELECT 1
				FROM pg_stat_progress_create_index progress
				WHERE progress.datname = current_database()
				  AND progress.pid <> pg_backend_pid()
				  AND (progress.relid = to_regclass($2) OR progress.index_relid = to_regclass($1))
			)
		FROM (SELECT 1) one
		LEFT JOIN pg_index index_state ON index_state.indexrelid = to_regclass($1)
		`,
		"public."+name,
		"public."+table,
	).Scan(&exists, &onTable, &valid, &building); err != nil {
		return err
	}
	switch {
	case !exists || (onTable && valid):
		return nil
	case !onTable:
		return fmt.Errorf("index %s exists on another relation than %s", name, table)
	case building:
		return fmt.Errorf("index %s on %s is still being built by another session", name, table)
	}
	_, err := conn.Exec(ctx, fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS public.%s", quoteIdentifier(name)))
	return err
}

// Runs statement in its own transaction under the migration's lock timeout,
// retrying while the lock is busy so writers never queue behind it for long.
func contractIndexMigrationExecRetry(ctx context.Context, conn PgConn, statement string) (err error) {
	for attempt := 1; ; attempt += 1 {
		err = func() error {
			tx, err := conn.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(context.WithoutCancel(ctx))
			if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", contractIndexMigrationLockTimeout.Milliseconds())); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, statement); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
		if err == nil || !contractPartitionLockBusy(err) || contractIndexMigrationLockAttempts <= attempt {
			return
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, context.Cause(ctx))
		case <-time.After(contractIndexMigrationLockRetryWait):
		}
	}
}
