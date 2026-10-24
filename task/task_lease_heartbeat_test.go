// Heartbeats write claim_time every beat and move release_time only once less
// than half the lease remains, so the beats between extensions are HOT. These
// drive the real claim and the heartbeat statement on a fixed clock.
package task

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Claims one long task through the real claim path.
func claimLeaseHeartbeatTask(t testing.TB, ctx context.Context) (*Task, *taskClaimGuard, *TaskWorker) {
	t.Helper()
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	taskId := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner,
		RunAt(server.NowUtc().Add(-time.Hour)), MaxTime(2*time.Hour))
	worker := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
	claimed, guard, err := worker.takeTasks(1)
	if err != nil || guard == nil || claimed[taskId] == nil {
		worker.Close()
		t.Fatal("heartbeat fixture claim failed", err)
	}
	return claimed[taskId], guard, worker
}

// One heartbeat on the fixed clock, through the production statement.
func leaseHeartbeat(t testing.TB, ctx context.Context, claimed *Task, now time.Time) {
	t.Helper()
	var refreshed int64
	server.Tx(ctx, func(tx server.PgTx) {
		refreshed = refreshTaskTimestampLeasesInTx(ctx, tx, map[server.Id]*Task{claimed.TaskId: claimed}, now)
	}, server.TxReadCommitted, server.OptNoRetry())
	if refreshed != 1 {
		t.Fatal("heartbeat skipped its own live claim", now, refreshed)
	}
}

// A pending row's lease timestamps and the availability they generate.
type taskLeaseState struct {
	claimTime      time.Time
	releaseTime    time.Time
	availableBlock int64
}

// The committed lease state of one pending row.
func readTaskLeaseState(t testing.TB, ctx context.Context, taskId server.Id) (state taskLeaseState) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT claim_time,release_time,available_block FROM pending_task WHERE task_id=$1`, taskId).Scan(
			&state.claimTime, &state.releaseTime, &state.availableBlock))
	})
	return
}

// A two-hour task at the production heartbeat cadence, on a fixed clock: every
// heartbeat writes claim_time, release_time never falls behind the heartbeat,
// and it moves exactly at the first heartbeat with less than half a lease left.
func TestTaskHeartbeatExtendsReleaseTimeAtHalfLease(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		claimed, guard, worker := claimLeaseHeartbeatTask(t, ctx)
		defer worker.Close()
		defer guard.release()
		interval := ReleaseTimeout / 3
		initial := readTaskLeaseState(t, ctx, claimed.TaskId)
		if !initial.releaseTime.Equal(claimed.ClaimTime.Add(TaskLeaseTimeout)) {
			t.Fatal("claim did not publish one lease", claimed.ClaimTime, initial.releaseTime)
		}
		release := initial.releaseTime
		beats, extensions := 0, 0
		for now := claimed.ClaimTime.Add(interval); !now.After(claimed.ClaimTime.Add(2 * time.Hour)); now = now.Add(interval) {
			due := release.Sub(now) < TaskLeaseTimeout/2
			leaseHeartbeat(t, ctx, claimed, now)
			state := readTaskLeaseState(t, ctx, claimed.TaskId)
			switch {
			case !state.claimTime.Equal(now):
				t.Fatal("heartbeat did not write claim_time", now, state.claimTime)
			case due && !state.releaseTime.Equal(now.Add(TaskLeaseTimeout)):
				t.Fatal("heartbeat did not extend at the half-lease point", now, release, state.releaseTime)
			case !due && !state.releaseTime.Equal(release):
				t.Fatal("heartbeat moved release_time with more than half a lease left", now, release, state.releaseTime)
			case state.releaseTime.Sub(now) < TaskLeaseTimeout/2 || TaskLeaseTimeout < state.releaseTime.Sub(now):
				// More than a heartbeat interval stays between now and release_time.
				t.Fatal("release_time left the half-lease band ahead of the heartbeat", now, state.releaseTime)
			}
			beats++
			if due {
				extensions++
			}
			release = state.releaseTime
		}
		// One extension per half lease and heartbeat: 720 beats, 45 extensions.
		period := int(TaskLeaseTimeout/2/interval) + 1
		if beats != 720 || extensions != beats/period {
			t.Fatal("unexpected heartbeat or extension count", beats, extensions, period)
		}
	})
}

// Between extensions a heartbeat changes no indexed column, so PostgreSQL
// updates it HOT; an extension moves available_block, which both poll indexes
// key on. The backend's pending table counters also hold earlier transactions
// until it flushes them, at most once a second and only between transactions,
// so each heartbeat transaction reads them before and after its update.
func TestTaskHeartbeatBetweenExtensionsIsHot(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT pg_get_indexdef(indexrelid),COALESCE(pg_get_expr(indpred,indrelid),'')
				FROM pg_index WHERE indrelid='pending_task'::regclass`)
			count := 0
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var definition, predicate string
					server.Raise(rows.Scan(&definition, &predicate))
					if strings.Contains(definition, "claim_time") || strings.Contains(predicate, "claim_time") {
						t.Fatal("an index covers the heartbeat column", definition, predicate)
					}
					count++
				}
			})
			var fillfactor bool
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(reloptions,ARRAY[]::text[]) @> ARRAY['fillfactor=50']
				FROM pg_class WHERE oid='pending_task'::regclass`).Scan(&fillfactor))
			if count == 0 || !fillfactor {
				t.Fatal("pending_task catalog fixture is incomplete", count, fillfactor)
			}
		})
		claimed, guard, worker := claimLeaseHeartbeatTask(t, ctx)
		defer worker.Close()
		defer guard.release()
		interval := ReleaseTimeout / 3
		state := readTaskLeaseState(t, ctx, claimed.TaskId)
		hot := 0
		// Through the first extension, at the sixteenth beat.
		for beat := 1; beat <= 24; beat++ {
			now := claimed.ClaimTime.Add(time.Duration(beat) * interval)
			extend := state.releaseTime.Sub(now) < TaskLeaseTimeout/2
			var refreshed, updated, hotUpdated int64
			server.Tx(ctx, func(tx server.PgTx) {
				counters := func() (updated int64, hotUpdated int64) {
					server.Raise(tx.QueryRow(ctx, `SELECT pg_stat_get_xact_tuples_updated('pending_task'::regclass),
						pg_stat_get_xact_tuples_hot_updated('pending_task'::regclass)`).Scan(&updated, &hotUpdated))
					return
				}
				updatedBefore, hotBefore := counters()
				refreshed = refreshTaskTimestampLeasesInTx(ctx, tx, map[server.Id]*Task{claimed.TaskId: claimed}, now)
				updatedAfter, hotAfter := counters()
				updated, hotUpdated = updatedAfter-updatedBefore, hotAfter-hotBefore
			}, server.TxReadCommitted, server.OptNoRetry())
			after := readTaskLeaseState(t, ctx, claimed.TaskId)
			switch {
			case refreshed != 1 || updated != 1:
				t.Fatal("heartbeat did not update exactly its row", beat, refreshed, updated)
			case extend && (after.availableBlock == state.availableBlock || hotUpdated != 0):
				t.Fatal("an extension must move available_block, which no HOT update can", beat, after.availableBlock, hotUpdated)
			case !extend && (after.availableBlock != state.availableBlock || hotUpdated != 1):
				t.Fatal("a heartbeat between extensions moved available_block or was not HOT", beat, after.availableBlock, hotUpdated)
			}
			if !extend {
				hot++
			}
			state = after
		}
		if hot != 23 {
			t.Fatal("unexpected HOT heartbeat count", hot)
		}
	})
}

// A crashed worker's session takes its advisory lock with it, and its task
// becomes claimable once release_time passes, within the one-second
// availability block. Before release_time the timestamp still holds it.
func TestTaskCrashedOwnerReclaimableAtReleaseTime(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		claimed, guard, worker := claimLeaseHeartbeatTask(t, ctx)
		defer worker.Close()
		defer guard.release()
		interval := ReleaseTimeout / 3
		// Heartbeats through the first extension, the last one at the sixteenth beat.
		var last time.Time
		for beat := 1; beat <= 16; beat++ {
			last = claimed.ClaimTime.Add(time.Duration(beat) * interval)
			leaseHeartbeat(t, ctx, claimed, last)
		}
		release := readTaskLeaseState(t, ctx, claimed.TaskId).releaseTime
		if !release.Equal(last.Add(TaskLeaseTimeout)) {
			t.Fatal("the last heartbeat did not extend the lease", last, release)
		}
		// The owner dies: no more heartbeats, and its session's advisory lock goes.
		guard.release()
		successor := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		defer successor.Close()
		successor.claimNow = func() time.Time { return release.Add(-time.Second) }
		early, earlyGuard, err := successor.takeTasks(1)
		if earlyGuard != nil {
			earlyGuard.release()
		}
		if err != nil || len(early) != 0 {
			t.Fatal("a dead owner's task was claimable before its release_time", err, len(early))
		}
		successor.claimNow = func() time.Time { return release.Add(2 * time.Second) }
		reclaimed, reclaimGuard, err := successor.takeTasks(1)
		if reclaimGuard != nil {
			defer reclaimGuard.release()
		}
		if err != nil || reclaimed[claimed.TaskId] == nil || reclaimed[claimed.TaskId].ClaimGeneration <= claimed.ClaimGeneration {
			t.Fatal("a dead owner's task was not reclaimable at its release_time", err, len(reclaimed))
		}
	})
}
