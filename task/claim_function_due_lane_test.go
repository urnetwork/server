// A lane turn goes to the next lane with due work, so a low-volume function is
// not starved by registered lanes that are empty.
package task

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Empty lanes registered beside the two queued functions.
const claimFunctionEmptyLanes = 62

// Registers the dominant function with many empty alias lanes, and the sparse
// function in its own lane.
func newClaimFunctionDueLaneWorker(ctx context.Context) (*TaskWorker, *claimProfileTarget, *claimProfileTarget) {
	settings := DefaultTaskWorkerSettings()
	settings.FairClaimFunctions = true
	worker := NewTaskWorker(ctx, settings)
	aliases := make([]string, claimFunctionEmptyLanes)
	for index := range aliases {
		aliases[index] = fmt.Sprintf("fixture.example/tasks.EmptyLane%02d", index)
	}
	dominant := &claimProfileTarget{Target: NewTaskTarget(claimProfileAllowed, aliases...)}
	sparse := &claimProfileTarget{Target: NewTaskTarget(claimProfileExcluded)}
	worker.AddTargets(dominant, sparse)
	return worker, dominant, sparse
}

// Starts the shared rotation at the lane at index.
func claimFunctionTestStart(t testing.TB, worker *TaskWorker, index int) {
	t.Helper()
	taskFunctionTestTurn(t, worker, worker.claimFunctionNames[index%len(worker.claimFunctionNames)])
}

// An older backlog of one function fills every ordinary claim, and almost every
// registered lane is empty. The rotation starts right after the sparse lane.
// The sparse function's newer due task must be claimed within one lane turn per
// probe window plus the dominant lane's own turn, not after a lane turn for
// every registered lane.
func TestTaskClaimFunctionLaneTurnSkipsEmptyLanes(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		worker, dominant, sparse := newClaimFunctionDueLaneWorker(ctx)
		defer worker.Close()
		// A fixed claim clock keeps every lane passed for the whole test, so
		// the bound counts claims however long each one takes.
		claimAt := server.NowUtc()
		worker.claimNow = func() time.Time { return claimAt }
		past := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
		for range 200 {
			ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(past))
		}
		ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, owner, RunAt(past.Add(time.Hour)))
		lanes := len(worker.claimFunctionNames)
		claimFunctionTestStart(t, worker, slices.Index(worker.claimFunctionNames, sparse.TargetFunctionName())+1)
		bound := 2 * ((lanes+claimFunctionProbeLimit-1)/claimFunctionProbeLimit + 1)
		for claim := 0; claim < bound && sparse.runs.Load() == 0; claim++ {
			finished, retried, posts, err := worker.EvalTasks(1)
			if err != nil || len(finished) != 1 || len(retried)+len(posts) != 0 {
				t.Fatal("claim failed", claim, finished, retried, posts, err)
			}
		}
		if sparse.runs.Load() != 1 {
			t.Fatalf("due task waited past %d claims behind %d registered lanes", bound, lanes)
		}
		if dominant.runs.Load() == 0 || 200 <= dominant.runs.Load() {
			t.Fatal("test did not keep an older backlog in progress", dominant.runs.Load())
		}
	})
}

// Records the lane probes a claim issues, with their arguments.
type claimFunctionProbeTrace struct {
	stateLock sync.Mutex
	queries   []string
	args      [][]any
}

// Retains lane probes only.
func (self *claimFunctionProbeTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "WITH ORDINALITY AS lane(name,position)") {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.queries = append(self.queries, data.SQL)
		self.args = append(self.args, slices.Clone(data.Args))
	}
	return ctx
}

// Nothing to record after a statement.
func (self *claimFunctionProbeTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// The probe a real lane turn issues reads at most one queue row per probed
// lane, through the function index, beside an older backlog far larger than a
// claim window. A probe that filtered that backlog would repeat the claim scan
// that outlived its context on Main. Both normal plan cache policies are read.
// The next lane turn's probe seeks none of the lanes the first found empty.
func TestTaskClaimFunctionProbeReadsOneRowPerLane(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, dominant, sparse := newClaimFunctionDueLaneWorker(ctx)
		defer worker.Close()
		// The lanes the first turn found empty stay passed for the second turn
		// however long the ordinary claim between them takes.
		claimAt := server.NowUtc()
		worker.claimNow = func() time.Time { return claimAt }
		const backlog = 20_000
		past := server.NowUtc().Add(-48 * time.Hour).Truncate(time.Second)
		rows := make([][]any, 0, backlog+1)
		for index := range backlog + 1 {
			var id server.Id
			binary.BigEndian.PutUint64(id[8:], uint64(index+1))
			name, runAt := dominant.TargetFunctionName(), past
			if index == backlog {
				name, runAt = sparse.TargetFunctionName(), past.Add(time.Hour)
			}
			rows = append(rows, []any{id, name, `{}`, runAt, DefaultPriority, int(DefaultMaxTime / time.Second), past, past})
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := tx.CopyFrom(ctx, pgx.Identifier{"pending_task"},
				[]string{"task_id", "function_name", "args_json", "run_at", "run_priority", "run_max_time_seconds", "claim_time", "release_time"}, pgx.CopyFromRows(rows))
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE pending_task`))
		})
		// Start inside the empty lanes so one window spans empty lanes, the
		// dominant lane and the sparse lane.
		dominantIndex := slices.Index(worker.claimFunctionNames, dominant.TargetFunctionName())
		claimFunctionTestStart(t, worker, dominantIndex-claimFunctionProbeLimit/2)
		trace := &claimFunctionProbeTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		// A lane turn, an ordinary turn, then the next lane turn.
		claimedSparse := false
		for claim := range 3 {
			claimed, guard, _, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{poll: &taskClaimPoll{}})
			if guard != nil {
				guard.release()
			}
			if err != nil || len(claimed) != 1 {
				t.Fatal("claim failed", claim, len(claimed), err)
			}
			for _, queued := range claimed {
				claimedSparse = claimedSparse || updateFunctionName(queued.FunctionName) == sparse.TargetFunctionName()
			}
		}
		server.Raise(scope.Close())
		if len(trace.queries) != 2 || !claimedSparse {
			t.Fatal("two lane turns did not probe once each and reach the sparse lane", len(trace.queries), claimedSparse)
		}
		lanes := trace.args[0][0].([]string)
		dominantPosition := slices.Index(lanes, dominant.TargetFunctionName())
		if len(lanes) != claimFunctionProbeLimit || dominantPosition <= 0 || !slices.Contains(lanes, sparse.TargetFunctionName()) {
			t.Fatal("probe window did not span empty, dominant and sparse lanes", lanes)
		}
		for _, lane := range trace.args[1][0].([]string) {
			if slices.Contains(lanes[:dominantPosition], lane) {
				t.Fatal("next lane turn sought a lane already found empty", lane)
			}
		}
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			server.MaintenanceDb(ctx, func(conn server.PgConn) {
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				defer tx.Rollback(ctx)
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = `+mode))
				var raw []byte
				server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) `+trace.queries[0], trace.args[0]...).Scan(&raw))
				var plans []struct {
					Plan taskClaimQueryTestPlan `json:"Plan"`
				}
				server.Raise(json.Unmarshal(raw, &plans))
				if len(plans) != 1 {
					t.Fatal("missing lane probe plan", mode)
				}
				read, indexed := float64(0), true
				var walk func(taskClaimQueryTestPlan)
				walk = func(plan taskClaimQueryTestPlan) {
					if plan.RelationName == "pending_task" {
						read += (plan.ActualRows + plan.FilteredRows) * plan.ActualLoops
						indexed = indexed && plan.IndexName == "pending_task_function_poll_order"
					}
					for _, child := range plan.Plans {
						walk(child)
					}
				}
				walk(plans[0].Plan)
				t.Logf("lane probe mode=%s rows_read=%.0f plan=%s", mode, read, raw)
				if !indexed || float64(len(lanes)) < read {
					t.Fatalf("lane probe read %.0f queue rows for %d lanes beside a %d-row backlog (indexed=%t)", read, len(lanes), backlog, indexed)
				}
			})
		}
	})
}

// Records the lane cursors a claim declares, with their arguments.
type claimFunctionCursorTrace struct {
	stateLock sync.Mutex
	queries   []string
	args      [][]any
}

// Retains cursor declarations only, as their SELECT.
func (self *claimFunctionCursorTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if query, ok := strings.CutPrefix(data.SQL, "DECLARE pending_task_claim_candidates NO SCROLL CURSOR FOR "); ok {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.queries = append(self.queries, query)
		self.args = append(self.args, slices.Clone(data.Args))
	}
	return ctx
}

// Nothing to record after a statement.
func (self *claimFunctionCursorTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Queue buffers one candidate query reads, in a custom plan.
func claimFunctionCursorBuffers(t testing.TB, ctx context.Context, query string, args []any) int64 {
	t.Helper()
	var buffers int64
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		defer tx.Rollback(ctx)
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = force_custom_plan`))
		var raw []byte
		server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) `+query, args...).Scan(&raw))
		var plans []struct {
			Plan taskClaimQueryTestPlan `json:"Plan"`
		}
		server.Raise(json.Unmarshal(raw, &plans))
		if len(plans) != 1 {
			t.Fatal("missing lane cursor plan")
		}
		buffers = plans[0].Plan.SharedHits + plans[0].Plan.SharedReads
	})
	return buffers
}

// A lane's queue prefix can hold thousands of deleted rows that an older
// snapshot still sees, as a long backup holds them on Main; there every lane
// seek over a busy function read tens of thousands of buffers. The probe passes
// that prefix once to find the lane's first due row. The cursor that claims
// from the lane must start at that row instead of reading the prefix again.
// Prefix rows are scattered in the heap, so each one costs a buffer to pass.
func TestTaskClaimFunctionLaneCursorStartsAtProbedRow(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, _, sparse := newClaimFunctionDueLaneWorker(ctx)
		defer worker.Close()
		claimAt := server.NowUtc()
		worker.claimNow = func() time.Time { return claimAt }
		const prefix = 4_000
		past := claimAt.Add(-48 * time.Hour).Truncate(time.Second)
		ids := make([]server.Id, prefix+1)
		for index := range ids {
			binary.BigEndian.PutUint64(ids[index][8:], uint64(index+1))
		}
		// One prefix row per block, written in a stride order so that rows
		// adjacent in the lane lie about a thousand rows apart in the heap, as
		// a queue's churned rows do. The due row follows an hour later.
		rows := make([][]any, 0, prefix+1)
		for position := range prefix {
			index := position * 1597 % prefix
			rows = append(rows, []any{ids[index], sparse.TargetFunctionName(), `{}`, past.Add(time.Duration(index) * time.Second),
				DefaultPriority, int(DefaultMaxTime / time.Second), past, past})
		}
		rows = append(rows, []any{ids[prefix], sparse.TargetFunctionName(), `{}`, past.Add(time.Duration(prefix)*time.Second + time.Hour),
			DefaultPriority, int(DefaultMaxTime / time.Second), past, past})
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := tx.CopyFrom(ctx, pgx.Identifier{"pending_task"},
				[]string{"task_id", "function_name", "args_json", "run_at", "run_priority", "run_max_time_seconds", "claim_time", "release_time"}, pgx.CopyFromRows(rows))
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE pending_task`))
		})
		// The scope observes every later fixture connection; all of them are
		// released before it closes.
		trace := &claimFunctionCursorTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		defer scope.Close()
		// The held snapshot still sees the prefix after it is deleted, so no
		// scan can prune it or mark its index entries dead.
		holder, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(holder.Release)
		}
		defer release()
		snapshot, err := holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		server.Raise(err)
		var seen int
		server.Raise(snapshot.QueryRow(ctx, `SELECT count(*) FROM pending_task`).Scan(&seen))
		if seen != len(ids) {
			t.Fatal("snapshot did not see the queued lane", seen)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE task_id=ANY($1)`, ids[:prefix]))
		})

		// Start just before the sparse lane, so the turn probes a window and
		// chooses the sparse lane, the only lane with work.
		claimFunctionTestStart(t, worker, slices.Index(worker.claimFunctionNames, sparse.TargetFunctionName())+len(worker.claimFunctionNames)-1)
		claimed, guard, _, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{poll: &taskClaimPoll{}})
		if guard != nil {
			guard.release()
		}
		if err != nil || len(claimed) != 1 || claimed[ids[prefix]] == nil {
			t.Fatal("lane turn did not claim the due row behind the deleted prefix", len(claimed), err)
		}
		lane := -1
		for index, args := range trace.args {
			if slices.Contains(args, any(sparse.TargetFunctionName())) {
				lane = index
			}
		}
		if lane < 0 || len(trace.queries) != 1 {
			t.Fatal("lane turn did not declare exactly its lane cursor", len(trace.queries))
		}
		unbounded, unboundedArgs := worker.taskFunctionCandidatesQuery(claimAt.Unix()/BlockSizeSeconds, 1+64, true, sparse.TargetFunctionName())
		prefixReads := claimFunctionCursorBuffers(t, ctx, unbounded, unboundedArgs)
		cursorReads := claimFunctionCursorBuffers(t, ctx, trace.queries[lane], trace.args[lane])
		server.Raise(snapshot.Rollback(ctx))
		release()
		server.Raise(scope.Close())
		t.Logf("lane cursor read %d buffers; an unbounded lane cursor reads %d beside a %d-row deleted prefix", cursorReads, prefixReads, prefix)
		if prefixReads < prefix/2 {
			t.Fatal("fixture prefix did not cost its unbounded scan a read per two rows", prefixReads)
		}
		if 64 < cursorReads {
			t.Fatalf("lane cursor read %d buffers again after its probe passed a %d-row prefix", cursorReads, prefix)
		}
	})
}
