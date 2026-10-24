// finished_task's family in the partition registry
// (PARTITION-MAINTENANCE-PLAN.md C3): daily partitions by run end time, each
// dropped once its day ended finishedTaskPostErrorRetention ago, so about 8
// days are kept. The operator cutover (server.MigrateFinishedTasksToPartitions)
// makes the table partitioned. Before it nothing runs for the family, and
// TaskCleanup keeps deleting rows; after it, retention is the drop and
// TaskCleanup deletes nothing.
//
// No failed Post loses its 7 days: every row of an expired day ended before
// the day's upper bound, which is the retention past, and the drop checks
// that again on the detached partition. Successful history is kept up to 8
// days instead of 1, the storage the owner chose for a drop that does not
// depend on vacuum.
//
// The drop is fenced as TaskCleanup's row delete is. Only a failed Post
// (post_error set, its Post not completed) can have a live finished owner: a
// Post retry (RunPost) holds its row's owner while it runs, and its handback
// acknowledges the row. Every other row's owner ended with the completion that
// wrote it, since a Post failure is recorded in that same transaction, and a
// day gains no failed Post once it has ended. So before a day drops, its
// failed Posts leave through TaskCleanup's own admission (the family's archive
// step): in groups, each admitting the exact finished owners of its rows
// before it deletes them under row locks that skip busy rows. A group whose
// owners are not all free stays. The drop transaction then takes the table's
// exclusive lock and refuses while the day still holds a failed Post, so it
// defers exactly while a Post retry runs and never removes a row that a
// running retry reads. A retry admitted after the release finds no row and
// completes as a no-op, as after TaskCleanup (RunPost's orphan path).
//
// Lookups by task id carry no run end time, so each probes every partition,
// one primary key index probe per day.
package task

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// The family's settings, shared with TaskCleanup's guard of a prepared cutover.
var finishedTaskPartitionSettings = server.DefaultFinishedTaskPartitionSettings()

var finishedTaskPartitionFamily = server.RegisterPartitionFamily(newFinishedTaskPartitionFamily(finishedTaskPartitionSettings))

// The family's declaration with its fence. Its hooks reach the family through
// the local variable, so the package-level registration has no
// initialization cycle.
func newFinishedTaskPartitionFamily(settings *server.FinishedTaskPartitionSettings) *server.PartitionFamily {
	family := &server.PartitionFamily{
		Name:        "finished_task",
		Description: "task history, daily by run end time; a day drops 7 days after it ends, once its failed Posts have left through their finished owners",
		Tables:      []string{"finished_task"},
		KeyColumn:   "run_end_time",
		KeyKind:     server.PartitionKeyTimestamp,
		Granularity: server.PartitionGranularityDaily,
		AheadRanges: settings.AheadDays,
		// the longer of TaskCleanup's two classes: a failed Post's retries
		Retention:        finishedTaskPostErrorRetention,
		LeafAnalyzeDelay: settings.LeafAnalyzeDelay,
		// days are insert-only and dropped whole
		ReindexExempt: true,
		Settings: &server.PartitionFamilySettings{
			CreateLockTimeout:    settings.CreateLockTimeout,
			DropLockTimeout:      settings.ExclusiveLockTimeout,
			DropStatementTimeout: settings.CatalogStatementTimeout,
		},
	}
	family.Archive = func(ctx context.Context, r server.PartitionRange, cursor string, deadline time.Time) (string, bool, error) {
		return releaseFinishedTaskPartitionPosts(ctx, family.LeafName("finished_task", r), cursor, deadline, settings)
	}
	family.DropFence = func(ctx context.Context, tx server.PgTx, r server.PartitionRange) {
		fenceFinishedTaskPartitionDrop(ctx, tx, family.LeafName("finished_task", r), settings)
	}
	family.DropPrecondition = func(ctx context.Context, conn server.PgQuerier, r server.PartitionRange) string {
		return finishedTaskPartitionBlocker(ctx, conn, family.LeafName("finished_task", r))
	}
	return family
}

// Whether finished_task is partitioned yet: its retention is then the drop.
func finishedTaskPartitioned(ctx context.Context) bool {
	return finishedTaskPartitionFamily.IsPartitioned(ctx)
}

// The candidates of one release group: the next failed Posts of the day after
// the cursor, in task id order, which the day's failed Post index serves.
const finishedTaskPartitionPostsSql = `SELECT task_id FROM ONLY %s
	WHERE post_error IS NOT NULL AND NOT post_completed AND task_id > $1
	ORDER BY task_id
	LIMIT $2`

// The admitted candidates that are still failed Posts past the retention are
// locked, skipping rows another transaction holds, and deleted by row address.
// The partition is named, so a row address is unique.
const finishedTaskPartitionReleaseSql = `DELETE FROM ONLY %s
	WHERE ctid = ANY(ARRAY(
		SELECT ctid FROM ONLY %s
		WHERE task_id = ANY($1::uuid[])
			AND post_error IS NOT NULL AND NOT post_completed
			AND run_end_time < $2::timestamp
		ORDER BY task_id
		FOR UPDATE SKIP LOCKED))
	AND task_id = ANY($1::uuid[])`

// Removes the failed Posts of an expired day's partition through their exact
// finished owners: groups after the cursor (a task id), each one read-committed
// transaction that admits the owners of its candidates before any row lock and
// leaves the group whole when one is busy. Runs until the deadline, then
// returns the cursor to resume from; done once a pass reaches the end of the
// day's failed Posts, though a busy group's rows remain for the drop's fence
// to refuse. A row is removed only once it ended more than
// finishedTaskPostErrorRetention ago, by the cleanup's clock.
func releaseFinishedTaskPartitionPosts(ctx context.Context, leaf string, cursor string, deadline time.Time, settings *server.FinishedTaskPartitionSettings) (next string, done bool, err error) {
	var after server.Id
	if cursor != "" {
		if after, err = server.ParseId(cursor); err != nil {
			return "", false, fmt.Errorf("release cursor %q of %s: %w", cursor, leaf, err)
		}
	}
	quoted := "public." + pgx.Identifier{leaf}.Sanitize()
	postErrorMinTime := taskCleanupNow(ctx)().Add(-finishedTaskPostErrorRetention)
	for {
		visited := 0
		var last server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			visited = 0
			taskIds := make([]server.Id, 0, settings.ReleaseGroupSize)
			rows, queryErr := tx.Query(ctx, fmt.Sprintf(finishedTaskPartitionPostsSql, quoted), after, settings.ReleaseGroupSize)
			server.WithPgResult(rows, queryErr, func() {
				for rows.Next() {
					var taskId server.Id
					server.Raise(rows.Scan(&taskId))
					taskIds = append(taskIds, taskId)
				}
			})
			visited = len(taskIds)
			if visited == 0 {
				return
			}
			last = taskIds[visited-1]
			keys := make([]server.PgOwnershipKey, len(taskIds))
			for index, taskId := range taskIds {
				keys[index] = taskFinishedOwnershipKey(taskId)
			}
			admitted, admitErr := server.TryTxOwnershipExec(ctx, tx, keys)
			server.Raise(admitErr)
			if !admitted {
				return
			}
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(finishedTaskPartitionReleaseSql, quoted, quoted), taskIds, postErrorMinTime))
		}, server.TxReadCommitted, server.OptNoRetry())
		if visited < settings.ReleaseGroupSize {
			return "", true, nil
		}
		// the cursor advances once the group's transaction has returned
		after = last
		if !time.Now().Before(deadline) {
			return after.String(), false, nil
		}
	}
}

// The drop transaction's fence, after the registry's lock and statement
// bounds and before any detach. It defers while a long transaction holds a
// finished_task lock, so the exclusive request never queues completions
// behind one; locks the parent and every partition, which a Post retry's
// statements wait behind until the drop commits; and then refuses while the
// day still holds a failed Post, whose owner was busy when the release ran.
// Raises to refuse.
func fenceFinishedTaskPartitionDrop(ctx context.Context, tx server.PgTx, leaf string, settings *server.FinishedTaskPartitionSettings) {
	if holders := server.FinishedTaskLongLockHolders(ctx, tx, settings.MaxTransactionAge); 0 < holders {
		server.Raise(fmt.Errorf("%d transaction(s) older than %s hold finished_task locks", holders, settings.MaxTransactionAge))
	}
	server.RaisePgResult(tx.Exec(ctx, `LOCK TABLE public.finished_task IN ACCESS EXCLUSIVE MODE`))
	var remaining int
	server.Raise(tx.QueryRow(
		ctx,
		fmt.Sprintf(`SELECT count(*) FROM ONLY public.%s WHERE post_error IS NOT NULL AND NOT post_completed`, pgx.Identifier{leaf}.Sanitize()),
	).Scan(&remaining))
	if 0 < remaining {
		server.Raise(fmt.Errorf("%d failed Post(s) in %s wait for their finished owners", remaining, leaf))
	}
}

// Why a day must stay: it holds a task that ended within the failed Post
// retention. A day expires only once its upper bound is that far past, so this
// holds only when a clock or a declaration is wrong. It runs before the drop
// and again on the detached partition in the drop's transaction.
func finishedTaskPartitionBlocker(ctx context.Context, conn server.PgQuerier, leaf string) string {
	postErrorMinTime := taskCleanupNow(ctx)().Add(-finishedTaskPostErrorRetention)
	var recent bool
	server.Raise(conn.QueryRow(
		ctx,
		fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM ONLY public.%s WHERE run_end_time >= $1::timestamp)`, pgx.Identifier{leaf}.Sanitize()),
		postErrorMinTime,
	).Scan(&recent))
	if recent {
		return fmt.Sprintf("%s holds a task that ended within the %s failed Post retention", leaf, finishedTaskPostErrorRetention)
	}
	return ""
}
