// A share lane's claims read from a floor past the rows they drained, and the
// lane's periodic full read still reaches a row published below that floor.
package task

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Retains the claim cursors a worker declares, with their arguments.
type taskClaimShareCursorTrace struct {
	stateLock sync.Mutex
	queries   []string
	args      [][]any
}

const taskClaimShareCursorPrefix = "DECLARE pending_task_claim_candidates NO SCROLL CURSOR FOR "

// Records each declared claim cursor's query and arguments.
func (self *taskClaimShareCursorTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, taskClaimShareCursorPrefix) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.queries = append(self.queries, strings.TrimPrefix(data.SQL, taskClaimShareCursorPrefix))
		self.args = append(self.args, slices.Clone(data.Args))
	}
	return ctx
}

// Nothing to record after a statement.
func (self *taskClaimShareCursorTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// The cursors declared so far.
func (self *taskClaimShareCursorTrace) snapshot() ([]string, [][]any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.queries), slices.Clone(self.args)
}

// Buffers a declared cursor's query reads to its end.
func taskClaimShareCursorBuffers(t testing.TB, ctx context.Context, query string, args []any) (buffers int64) {
	t.Helper()
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		var raw []byte
		server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) `+query, args...).Scan(&raw))
		var plans []struct {
			Plan taskClaimQueryTestPlan `json:"Plan"`
		}
		server.Raise(json.Unmarshal(raw, &plans))
		if len(plans) != 1 {
			t.Fatal("cursor plan missing", string(raw))
		}
		buffers = plans[0].Plan.SharedHits + plans[0].Plan.SharedReads
	})
	return
}

// Copies rows of one group into the lane at one-second steps from runAt.
func taskClaimShareCopyRows(t testing.TB, ctx context.Context, lane string, group server.Id, runAt time.Time, count int) []server.Id {
	t.Helper()
	args, err := json.Marshal(&taskClaimGroupTestArgs{GroupId: group})
	server.Raise(err)
	ids := make([]server.Id, 0, count)
	rows := make([][]any, 0, count)
	for index := range count {
		id := server.NewId()
		at := runAt.Add(time.Duration(index) * time.Second)
		ids = append(ids, id)
		rows = append(rows, []any{id, lane, string(args), at, DefaultPriority, int(DefaultMaxTime / time.Second), time.Time{}, at})
	}
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"pending_task"},
			[]string{"task_id", "function_name", "args_json", "run_at", "run_priority", "run_max_time_seconds", "claim_time", "release_time"}, pgx.CopyFromRows(rows))
		server.Raise(err)
	})
	return ids
}

// A worker with a share for a cohort target, beside an ordinary target.
func taskClaimShareFloorWorker(ctx context.Context) (*TaskWorker, string) {
	target := &taskRunCohortTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)}
	settings := DefaultTaskWorkerSettings()
	settings.FairClaimFunctions = true
	settings.TargetClaimShares = map[string]int{target.TargetFunctionName(): 2}
	worker := NewTaskWorker(ctx, settings)
	worker.AddTargets(target, NewTaskTarget(claimProfileAllowed))
	return worker, target.TargetFunctionName()
}

// Claims drained 8000 rows at the lane's head while an older snapshot still
// sees them, so no scan can mark their index entries dead. The lane's first
// claim reads from its start and visits them. The next claim reads from the
// first due row the first claim saw, and its cursor skips them.
func TestTaskClaimShareFloorSkipsTheDrainedPrefix(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, lane := taskClaimShareFloorWorker(ctx)
		defer worker.Close()
		base := server.NowUtc().Truncate(time.Second).Add(-3 * time.Hour)
		group := server.NewId()
		drainedIds := taskClaimShareCopyRows(t, ctx, lane, group, base, 8000)
		liveIds := taskClaimShareCopyRows(t, ctx, lane, group, base.Add(8000*time.Second), 128)
		trace := &taskClaimShareCursorTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		defer func() { server.Raise(scope.Close()) }()
		snapshot, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer snapshot.Release()
		snapshotTx, err := snapshot.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		server.Raise(err)
		defer snapshotTx.Rollback(context.WithoutCancel(ctx))
		var seen int
		server.Raise(snapshotTx.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE task_id=ANY($1)`, drainedIds).Scan(&seen))
		if seen != len(drainedIds) {
			t.Fatal("the older snapshot does not see the prefix", seen)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE task_id=ANY($1)`, drainedIds))
		})
		claimedIds := map[server.Id]bool{}
		for claim := range 2 {
			taskFunctionTestTurn(t, worker, lane)
			claimed := taskRunCohortLaneClaim(t, ctx, worker)
			if len(claimed) != 64 {
				t.Fatal("a lane claim did not take one full cohort", claim, len(claimed))
			}
			for id := range claimed {
				claimedIds[id] = true
			}
		}
		for _, id := range liveIds {
			if !claimedIds[id] {
				t.Fatal("two claims did not take the lane's 128 live rows in order")
			}
		}
		queries, args := trace.snapshot()
		if len(queries) != 2 {
			t.Fatal("expected one cursor per lane claim", len(queries))
		}
		full := taskClaimShareCursorBuffers(t, ctx, queries[0], args[0])
		floored := taskClaimShareCursorBuffers(t, ctx, queries[1], args[1])
		t.Logf("lane cursor buffers: from start=%d, from floor=%d", full, floored)
		if full < 200 {
			t.Fatal("the fixture's prefix stayed out of the lane's scan", full)
		}
		if full < 10*floored {
			t.Fatalf("the second claim's cursor read %d buffers of a %d-buffer drained prefix", floored, full)
		}
	})
}

// A row published further below the floor than claimShareFloorLag, as by a
// publisher whose clock lags, waits for the lane's next full read; the first
// lane claim after claimShareFloorReset takes it. Meanwhile an older ordinary
// backlog takes each lane turn the lane leaves empty.
func TestTaskClaimShareFloorResetReadsAStaleRow(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		worker, lane := taskClaimShareFloorWorker(ctx)
		defer worker.Close()
		now := server.NowUtc().Truncate(time.Second)
		worker.claimNow = func() time.Time { return now }
		backlogIds := map[server.Id]bool{}
		for range 4 {
			backlogIds[ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(now.Add(-2*time.Hour)))] = true
		}
		claim := func() (laneClaimed map[server.Id]*Task, backlog int) {
			taskFunctionTestTurn(t, worker, lane)
			laneClaimed = map[server.Id]*Task{}
			for id, queued := range taskRunCohortLaneClaim(t, ctx, worker) {
				if backlogIds[id] {
					backlog++
				} else {
					laneClaimed[id] = queued
				}
			}
			return
		}
		laneIds := taskClaimShareCopyRows(t, ctx, lane, server.NewId(), now.Add(-time.Hour), 2)
		if claimed, backlog := claim(); len(claimed) != 2 || claimed[laneIds[0]] == nil || backlog != 0 {
			t.Fatal("the lane's first claim did not take its rows", len(claimed), backlog)
		}
		if claimed, backlog := claim(); len(claimed) != 0 || backlog != 1 {
			t.Fatal("an emptied lane's turn did not fall back to the ordinary backlog", len(claimed), backlog)
		}
		staleIds := taskClaimShareCopyRows(t, ctx, lane, server.NewId(), now.Add(-30*time.Minute), 1)
		if claimed, backlog := claim(); len(claimed) != 0 || backlog != 1 {
			t.Fatal("a lane claim read below the lane's floor before its full read", len(claimed), backlog)
		}
		now = now.Add(claimShareFloorReset)
		if claimed, backlog := claim(); len(claimed) != 1 || claimed[staleIds[0]] == nil || backlog != 0 {
			t.Fatal("the lane's full read did not take the row below its floor", len(claimed), backlog)
		}
	})
}
