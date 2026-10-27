// The owner-run cutover of finished_task to daily partitions
// (server.MigrateFinishedTasksToPartitions) against live completions: an
// interrupted swap overlapping a completion, a rollback that keeps the writes
// made after the swap, a resumable prepare, abort, and the cleanup's guard of a
// prepare left unfinalized.
package task

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Runs on the plain table, as production before its cutover.
func finishedTaskPlainEnv(t *testing.T, run func(testing.TB, context.Context)) {
	t.Helper()
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		if finishedTaskPartitioned(ctx) {
			t.Skip("URNETWORK_TEST_FINISHED_TASK_PARTITIONS cut the database over already")
		}
		run(t, ctx)
	})
}

// A cutover day two days ahead: past the minimum lead and the abandon margin
// at any time of day.
func testFinishedTaskCutoverDay() time.Time {
	return server.PartitionGranularityDaily.AddRanges(server.PartitionGranularityDaily.RangeStart(server.NowUtc()), 2)
}

// One attempt per step with a short lock wait, so a busy swap fails at once.
func testFinishedTaskCutoverSettings() *server.FinishedTaskPartitionSettings {
	settings := testFinishedTaskPartitionSettings()
	settings.CutoverAttempts = 1
	settings.CutoverRetryInterval = 10 * time.Millisecond
	return settings
}

func migrateFinishedTasks(t testing.TB, ctx context.Context, options server.FinishedTaskPartitionMigrateOptions) error {
	t.Helper()
	options.Logf = t.Logf
	if options.Settings == nil {
		options.Settings = testFinishedTaskCutoverSettings()
	}
	return server.MigrateFinishedTasksToPartitions(ctx, options)
}

// Which relation holds the task's row: the legacy partition, a day, or the
// plain table.
func finishedTaskRowRelations(t testing.TB, ctx context.Context, taskId server.Id) (relations []string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT tableoid::regclass::text FROM finished_task WHERE task_id=$1`, taskId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var relation string
				server.Raise(rows.Scan(&relation))
				relations = append(relations, relation)
			}
		})
	})
	return
}

// The prepared cutover's catalog: the staged parent, its days, and the
// legacy bound with its validity.
type finishedTaskCutoverCatalog struct {
	staged         bool
	stagedDays     []string
	bound          bool
	boundValidated bool
}

func readFinishedTaskCutoverCatalog(t testing.TB, ctx context.Context) (catalog finishedTaskCutoverCatalog) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT to_regclass('public.finished_task_staged') IS NOT NULL`).Scan(&catalog.staged))
		rows, err := conn.Query(ctx, `SELECT child.relname FROM pg_inherits
			JOIN pg_class child ON child.oid = pg_inherits.inhrelid
			WHERE pg_inherits.inhparent = to_regclass('public.finished_task_staged') ORDER BY child.relname`)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var name string
				server.Raise(rows.Scan(&name))
				catalog.stagedDays = append(catalog.stagedDays, name)
			}
		})
		rows, err = conn.Query(ctx, `SELECT convalidated FROM pg_constraint
			WHERE conrelid = to_regclass('public.finished_task') AND conname = 'finished_task_legacy_bound'`)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				catalog.bound = true
				server.Raise(rows.Scan(&catalog.boundValidated))
			}
		})
	})
	return
}

// A target whose transactional Post records an effect, and waits on the
// first scope it is told to hold, inside the completion's transaction.
type finishedTaskCutoverPosts struct {
	stateLock sync.Mutex
	held      server.Id
	entered   chan struct{}
	released  chan struct{}
}

func newFinishedTaskCutoverPosts() *finishedTaskCutoverPosts {
	return &finishedTaskCutoverPosts{entered: make(chan struct{}), released: make(chan struct{})}
}

func (self *finishedTaskCutoverPosts) hold(scope server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.held = scope
}

func (self *finishedTaskCutoverPosts) takeHold(scope server.Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.held != scope {
		return false
	}
	self.held = server.Id{}
	return true
}

func (self *finishedTaskCutoverPosts) target(ctx context.Context) *runOnceGenerationTarget {
	target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
	target.after = func(tx server.PgTx, queued *Task) error {
		var args runOnceGenerationArgs
		server.Raise(json.Unmarshal([]byte(queued.ArgsJson), &args))
		if self.takeHold(args.Scope) {
			close(self.entered)
			taskQueueWait(ctx, self.released)
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO test_finished_task_post_effect (scope) VALUES ($1)`, args.Scope))
		return nil
	}
	return target
}

// An interrupted cutover overlapping a live completion: the swap waits for
// its lock behind a completion that moved its row and is running its
// transactional Post, gives up within its lock wait, and leaves the table
// plain. The completion then commits its one row and its one Post effect,
// and the finalize run again swaps: the row is in the legacy partition,
// exactly once, and so is a completion after the swap, before the cutover
// day. A rollback then copies nothing and keeps both.
func TestFinishedTaskCutoverOverlappingLiveCompletion(t *testing.T) {
	finishedTaskPlainEnv(t, func(t testing.TB, ctx context.Context) {
		createFinishedTaskPostEffects(t, ctx)
		cutoverDay := testFinishedTaskCutoverDay()
		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{CutoverDay: cutoverDay}); err != nil {
			t.Fatal(err)
		}
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		posts := newFinishedTaskCutoverPosts()
		worker := runOnceGenerationWorker(ctx, posts.target(ctx))
		defer worker.Close()
		scope := server.NewId()
		posts.hold(scope)
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
		evalDone := make(chan error, 1)
		go func() {
			var evalErr error
			server.HandleError(func() {
				if finished, retried, postIds, err := worker.EvalTasks(1); err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(postIds) != 0 {
					evalErr = err
					if evalErr == nil {
						evalErr = context.Canceled
					}
				}
			}, func(err error) { evalErr = err })
			evalDone <- evalErr
		}()
		taskQueueWait(ctx, posts.entered)

		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{Finalize: true}); err == nil || !strings.Contains(err.Error(), "lock timeout") {
			t.Fatal("the swap did not give up behind the live completion", err)
		}
		if finishedTaskPartitioned(ctx) || !readFinishedTaskCutoverCatalog(t, ctx).boundValidated {
			t.Fatal("the interrupted swap changed the table")
		}
		close(posts.released)
		if err := taskQueueError(ctx, evalDone); err != nil {
			t.Fatal("the live completion did not finish", err)
		}

		settings := testFinishedTaskCutoverSettings()
		settings.CutoverAttempts = 5
		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{Finalize: true, Settings: settings}); err != nil {
			t.Fatal(err)
		}
		if !finishedTaskPartitioned(ctx) {
			t.Fatal("the finalize run again did not swap")
		}
		if relations := finishedTaskRowRelations(t, ctx, id); len(relations) != 1 || relations[0] != "finished_task_legacy" || finishedTaskPostEffects(t, ctx, scope) != 1 {
			t.Fatal("the completion is not one row in the legacy partition with one Post effect", relations)
		}
		laterScope := server.NewId()
		later := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: laterScope}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
		if finished, _, _, err := worker.EvalTasks(1); err != nil || len(finished) != 1 || finished[0] != later {
			t.Fatal("a completion after the swap did not finish", err)
		}
		if relations := finishedTaskRowRelations(t, ctx, later); len(relations) != 1 || relations[0] != "finished_task_legacy" {
			t.Fatal("a completion before the cutover day did not land in the legacy partition", relations)
		}
		if len(GetFinishedTasks(ctx, id, later)) != 2 {
			t.Fatal("the reader lost a row across the swap")
		}

		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{Rollback: true, Settings: settings}); err != nil {
			t.Fatal(err)
		}
		if finishedTaskPartitioned(ctx) || len(GetFinishedTasks(ctx, id, later)) != 2 ||
			finishedTaskPostEffects(t, ctx, scope) != 1 || finishedTaskPostEffects(t, ctx, laterScope) != 1 {
			t.Fatal("the rollback before the cutover day lost a row or an effect")
		}
	})
}

// A rollback after the cutover day keeps the writes made since: the rows of
// the daily partitions come back into the plain table with the legacy rows,
// under the published names and keys, and row retention resumes. It refuses
// to copy more rows than its limit, and a task id in both the legacy
// partition and a day; a dry run changes nothing.
func TestFinishedTaskCutoverRollbackKeepsWritesAfterTheSwap(t *testing.T) {
	finishedTaskPartitionEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		worker := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		defer worker.Close()
		current := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
		if finished, _, _, err := worker.EvalTasks(1); err != nil || len(finished) != 1 || finished[0] != current {
			t.Fatal("the completion did not finish", err)
		}
		cutoverDay := server.TestingFinishedTaskCutoverDay(server.NowUtc())
		older := seedFinishedTaskPartitionFailedPosts(t, ctx, 1, cutoverDay.AddDate(0, 0, 2).Add(time.Hour))[0]
		legacy := seedFinishedTaskPartitionFailedPosts(t, ctx, 1, cutoverDay.Add(-time.Hour))[0]
		ids := []server.Id{current, older, legacy}
		before := GetFinishedTasks(ctx, ids...)
		if relations := finishedTaskRowRelations(t, ctx, legacy); len(relations) != 1 || relations[0] != "finished_task_legacy" {
			t.Fatal("the seed did not reach the legacy partition", relations)
		}

		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{Rollback: true, DryRun: true}); err != nil || !finishedTaskPartitioned(ctx) {
			t.Fatal("a dry run changed the table", err)
		}
		limited := testFinishedTaskCutoverSettings()
		limited.RollbackMaxCopyRows = 1
		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{Rollback: true, Settings: limited}); err == nil || !strings.Contains(err.Error(), "more than 1 rows") || !finishedTaskPartitioned(ctx) {
			t.Fatal("the rollback copied past its limit", err)
		}
		// a second row of a legacy task in a day, as only a writer other
		// than the move could make
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO finished_task
                (task_id,function_name,args_json,run_at,run_priority,run_max_time_seconds,run_start_time,run_end_time,result_json)
                VALUES ($1,'synthetic','{}',$2,0,30,$2,$2,'{}')`, legacy, server.NowUtc()))
		}, server.TxReadCommitted, server.OptNoRetry())
		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{Rollback: true}); err == nil || !strings.Contains(err.Error(), "two rows of one task") || !finishedTaskPartitioned(ctx) {
			t.Fatal("the rollback copied a second row of one task", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM finished_task WHERE task_id=$1 AND run_end_time >= $2`, legacy, cutoverDay))
		}, server.TxReadCommitted, server.OptNoRetry())

		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{Rollback: true}); err != nil {
			t.Fatal(err)
		}
		if finishedTaskPartitioned(ctx) {
			t.Fatal("the rollback left the table partitioned")
		}
		after := GetFinishedTasks(ctx, ids...)
		for _, id := range ids {
			if after[id] == nil || before[id] == nil || after[id].ResultJson != before[id].ResultJson || !after[id].RunEndTime.Equal(before[id].RunEndTime) {
				t.Fatal("the rollback lost or changed a row", id)
			}
			if relations := finishedTaskRowRelations(t, ctx, id); len(relations) != 1 || relations[0] != "finished_task" {
				t.Fatal("a row is not in the plain table", relations)
			}
		}
		var primaryKey string
		var indexes []string
		var leftovers int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
				WHERE conrelid = 'public.finished_task'::regclass AND contype = 'p' AND conname = 'finished_task_pkey'`).Scan(&primaryKey))
			rows, err := conn.Query(ctx, `SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'finished_task' ORDER BY indexname`)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var name string
					server.Raise(rows.Scan(&name))
					indexes = append(indexes, name)
				}
			})
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM pg_constraint WHERE conname = 'finished_task_legacy_bound') +
				(SELECT count(*) FROM pg_class WHERE relname = 'finished_task_legacy' OR relname LIKE 'finished\_task\_p2%')`).Scan(&leftovers))
		})
		if primaryKey != "PRIMARY KEY (task_id, run_end_time)" || strings.Join(indexes, ",") != "finished_task_pkey,finished_task_post_retry,finished_task_run_end_time" || leftovers != 0 {
			t.Fatal("the plain table did not get its published key and indexes back", primaryKey, indexes, leftovers)
		}
		if removed := RemoveFinishedTasks(ctx, server.NowUtc().Add(time.Hour), server.NowUtc().Add(time.Hour)); removed != int64(len(ids)) {
			t.Fatal("row retention did not resume on the plain table", removed)
		}
		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{Rollback: true}); err != nil {
			t.Fatal("a rollback of a plain table did not succeed as a no-op", err)
		}
	})
}

// Prepare resumes where it stopped: a day removed, or the bound removed or
// left unvalidated, is made again, and nothing is made twice. A dry run
// changes nothing; another cutover day is refused until abort, which removes
// the prepare and is itself repeatable.
func TestFinishedTaskCutoverPrepareResumesAndAborts(t *testing.T) {
	finishedTaskPlainEnv(t, func(t testing.TB, ctx context.Context) {
		cutoverDay := testFinishedTaskCutoverDay()
		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{CutoverDay: cutoverDay, DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if catalog := readFinishedTaskCutoverCatalog(t, ctx); catalog.staged || catalog.bound {
			t.Fatal("a dry run prepared", catalog)
		}
		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{CutoverDay: cutoverDay}); err != nil {
			t.Fatal(err)
		}
		prepared := readFinishedTaskCutoverCatalog(t, ctx)
		settings := testFinishedTaskCutoverSettings()
		if !prepared.staged || len(prepared.stagedDays) != settings.AheadDays+1 || !prepared.boundValidated {
			t.Fatal("prepare did not stage the days and validate the bound", prepared)
		}

		second := finishedTaskPartitionFamily.LeafName("finished_task", finishedTaskPartitionFamily.RangeAt(cutoverDay.AddDate(0, 0, 1)))
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE finished_task_staged DETACH PARTITION `+second))
			server.RaisePgResult(tx.Exec(ctx, `DROP TABLE `+second))
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE finished_task DROP CONSTRAINT finished_task_legacy_bound`))
		}, server.TxReadCommitted, server.OptNoRetry())
		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{CutoverDay: cutoverDay}); err != nil {
			t.Fatal(err)
		}
		if resumed := readFinishedTaskCutoverCatalog(t, ctx); strings.Join(resumed.stagedDays, ",") != strings.Join(prepared.stagedDays, ",") || !resumed.boundValidated {
			t.Fatal("prepare did not resume", resumed)
		}

		// as if stopped between adding the bound and validating it
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE finished_task DROP CONSTRAINT finished_task_legacy_bound`))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
				`ALTER TABLE finished_task ADD CONSTRAINT finished_task_legacy_bound CHECK (run_end_time < '%s') NOT VALID`,
				cutoverDay.Format(time.DateTime),
			)))
		}, server.TxReadCommitted, server.OptNoRetry())
		if catalog := readFinishedTaskCutoverCatalog(t, ctx); !catalog.bound || catalog.boundValidated {
			t.Fatal("the unvalidated bound was not set up", catalog)
		}
		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{CutoverDay: cutoverDay}); err != nil {
			t.Fatal(err)
		}
		if catalog := readFinishedTaskCutoverCatalog(t, ctx); !catalog.boundValidated {
			t.Fatal("prepare did not validate the bound it found", catalog)
		}

		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{CutoverDay: cutoverDay.AddDate(0, 0, 1)}); err == nil {
			t.Fatal("a prepare for another day was accepted")
		}
		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{Finalize: true, DryRun: true}); err != nil || finishedTaskPartitioned(ctx) {
			t.Fatal("a finalize dry run swapped", err)
		}
		for range 2 {
			if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{Abort: true}); err != nil {
				t.Fatal(err)
			}
		}
		if catalog := readFinishedTaskCutoverCatalog(t, ctx); catalog.staged || catalog.bound {
			t.Fatal("abort left the prepare", catalog)
		}
		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{Finalize: true}); err == nil {
			t.Fatal("finalize swapped without a prepare")
		}
	})
}

// A prepare left unfinalized would refuse every completion from its cutover
// day on. The cleanup leaves it alone before the abandon margin and abandons
// it inside the margin, and its sweep still runs; a completion then lands in
// the plain table.
func TestFinishedTaskCutoverAbandonedByCleanup(t *testing.T) {
	finishedTaskPlainEnv(t, func(t testing.TB, ctx context.Context) {
		cutoverDay := testFinishedTaskCutoverDay()
		if err := migrateFinishedTasks(t, ctx, server.FinishedTaskPartitionMigrateOptions{CutoverDay: cutoverDay}); err != nil {
			t.Fatal(err)
		}
		margin := finishedTaskPartitionSettings.CutoverAbandonMargin
		for _, test := range []struct {
			now       time.Time
			abandoned bool
		}{
			{now: cutoverDay.Add(-margin - time.Minute)},
			{now: cutoverDay.Add(-margin), abandoned: true},
		} {
			cleanupOwner := session.NewLocalClientSession(context.WithValue(ctx, taskCleanupClockKey{}, func() time.Time { return test.now }), "", nil)
			result, err := TaskCleanup(&TaskCleanupArgs{}, cleanupOwner)
			cleanupOwner.Cancel()
			if err != nil || result == nil {
				t.Fatal("the cleanup did not run", err)
			}
			if catalog := readFinishedTaskCutoverCatalog(t, ctx); catalog.staged == test.abandoned || catalog.bound == test.abandoned {
				t.Fatal("the cleanup's guard misjudged the prepare at", test.now, catalog)
			}
		}
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		worker := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		defer worker.Close()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
		if finished, _, _, err := worker.EvalTasks(1); err != nil || len(finished) != 1 || finished[0] != id {
			t.Fatal("a completion did not finish after the abandon", err)
		}
		if relations := finishedTaskRowRelations(t, ctx, id); len(relations) != 1 || relations[0] != "finished_task" {
			t.Fatal("the completion is not in the plain table", relations)
		}
	})
}
