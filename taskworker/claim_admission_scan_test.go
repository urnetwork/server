// Production placement must not make a claim scan the queued close backlog.
package taskworker

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Queued closes ahead of every other row, far more than one candidate window.
const claimScanBacklog = 3000

// Executions the fixture keeps in flight before the observed claim.
const claimScanHeld = 8

// Records every claim cursor the worker declares, with its arguments.
type claimCursorTrace struct {
	stateLock sync.Mutex
	queries   []string
	args      [][]any
}

const claimCursorPrefix = "DECLARE pending_task_claim_candidates NO SCROLL CURSOR FOR "

// Retains the candidate query of each declared claim cursor.
func (self *claimCursorTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, claimCursorPrefix) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.queries = append(self.queries, strings.TrimPrefix(data.SQL, claimCursorPrefix))
		self.args = append(self.args, slices.Clone(data.Args))
	}
	return ctx
}

// Nothing to record after a statement.
func (self *claimCursorTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Starts a new observation window.
func (self *claimCursorTrace) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.queries, self.args = nil, nil
}

// The cursors declared since the last reset.
func (self *claimCursorTrace) snapshot() ([]string, [][]any) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.queries), slices.Clone(self.args)
}

// Holds the first executions until release so they stay claimed; later
// executions run the real target immediately.
type claimScanHeldTarget struct {
	task.Target
	held    atomic.Int64
	started chan struct{}
	release chan struct{}
}

// Delegates the completion declaration to the real target.
func (self *claimScanHeldTarget) TaskCompletionOwnershipKeys(queued *task.Task, result string) ([]server.PgOwnershipKey, error) {
	return self.Target.(task.TaskCompletionOwnershipTarget).TaskCompletionOwnershipKeys(queued, result)
}

// Blocks the first held executions before the real body runs.
func (self *claimScanHeldTarget) Run(ctx context.Context, queued *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	if self.held.Add(1) <= claimScanHeld {
		self.started <- struct{}{}
		<-self.release
	}
	return self.Target.Run(ctx, queued)
}

// Rows the planner discarded by filter while executing a claim candidate query.
func claimScanRowsRemoved(t testing.TB, ctx context.Context, query string, args []any) (removed int) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		var raw []byte
		server.Raise(tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+query, args...).Scan(&raw))
		var explained []struct {
			Plan map[string]any `json:"Plan"`
		}
		server.Raise(json.Unmarshal(raw, &explained))
		var visit func(node map[string]any)
		visit = func(node map[string]any) {
			if value, ok := node["Rows Removed by Filter"].(float64); ok {
				removed += int(value)
			}
			children, _ := node["Plans"].([]any)
			for _, child := range children {
				if childNode, ok := child.(map[string]any); ok {
					visit(childNode)
				}
			}
		}
		for _, plan := range explained {
			visit(plan.Plan)
		}
	}, server.TxReadCommitted, pgx.ReadOnly)
	return
}

// Queued closes dominate the oldest end of the queue. With production
// placement and several closes already claimed, the next claim must still
// read about one candidate window. A claim that filters the whole backlog took
// longer than its context on the production queue, and the canceled statement
// closed the collector's shared session for every live finalization.
func TestProductionClaimScanStaysBoundedBehindScheduledCloseBacklog(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-claim-scan", server.NewId())
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "synthetic-source", "synthetic")
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "synthetic-destination", "synthetic")
		ids := make([]server.Id, claimScanBacklog)
		for index := range ids {
			ids[index] = server.NewId()
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract (
				contract_id,source_network_id,source_id,destination_network_id,destination_id,
				transfer_byte_count,usage_origin_is_source,create_time,expiration_time)
				SELECT contract_id,$2,$3,$2,$4,100,true,$5,NULL FROM unnest($1::uuid[]) AS candidate(contract_id)`,
				ids, networkId, sourceId, destinationId, server.NowUtc().Add(-3*time.Hour)))
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		if _, err := work.ScheduleOpenContractClosures(&work.ScheduleOpenContractClosuresArgs{PageSize: 1024,
			StartedAt: server.NowUtc().Truncate(time.Microsecond).Add(-2 * time.Hour)}, owner); err != nil {
			t.Fatal("startup pass did not publish the synthetic backlog", err)
		}

		trace := &claimCursorTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		defer func() { server.Raise(scope.Close()) }()
		settings := task.DefaultTaskWorkerSettings()
		settings.BatchSize = claimScanHeld
		worker := task.NewTaskWorker(ctx, taskWorkerSettingsForProfile(settings, WorkloadProfileProduction))
		defer worker.Close()
		held := &claimScanHeldTarget{Target: work.NewScheduledContractClosureTaskTarget(),
			started: make(chan struct{}, claimScanHeld), release: make(chan struct{})}
		worker.AddTargets(held)
		heldDone := make(chan struct{})
		var heldErr error
		go func() {
			defer close(heldDone)
			_, _, _, heldErr = worker.EvalTasks(claimScanHeld)
		}()
		released := false
		defer func() {
			if !released {
				close(held.release)
			}
			<-heldDone
		}()
		for range claimScanHeld {
			<-held.started
		}

		trace.reset()
		if _, retried, posts, err := worker.EvalTasks(1); err != nil || len(retried)+len(posts) != 0 {
			t.Fatal("claim beside held closes failed", retried, posts, err)
		}
		queries, args := trace.snapshot()
		if len(queries) == 0 {
			t.Fatal("observed claim declared no candidate cursor")
		}
		removed := 0
		for index, query := range queries {
			removed = max(removed, claimScanRowsRemoved(t, ctx, query, args[index]))
		}
		close(held.release)
		released = true
		<-heldDone
		if heldErr != nil {
			t.Fatal("held closes did not finish", heldErr)
		}
		if window := settings.BatchSize + 64; window < removed {
			t.Fatalf("claim scan discarded %d queued closes to fill a window of %d", removed, window)
		}
	})
}
