// Provider totals keep a drain rate behind the queued close backlog, and each
// allocation still applies exactly once.
package taskworker

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Queued closes of retired contracts, older than every provider total and more
// than the claims below can take.
const providerTotalsDrainCloses = 800

// Claim cursors the production worker may open before every provider total
// below has applied. The totals lane alone gets one turn per rotation of about
// two hundred claims and takes one row of interleaved providers per slot.
const providerTotalsDrainCursorBound = 240

// Counts claim cursors and signals once the bound is reached.
type providerTotalsDrainTrace struct {
	stateLock sync.Mutex
	cursors   int
	exceeded  chan struct{}
}

// Counts each declared claim cursor.
func (self *providerTotalsDrainTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, claimCursorPrefix) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.cursors++
		if self.cursors == providerTotalsDrainCursorBound {
			close(self.exceeded)
		}
	}
	return ctx
}

// Nothing to record after a statement.
func (self *providerTotalsDrainTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// The cursors declared so far.
func (self *providerTotalsDrainTrace) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.cursors
}

// One provider's share of a closed contract's payout, in the durable payload.
type providerTotalsDrainTotal struct {
	NetworkId server.Id `json:"network_id"`
	Bytes     int64     `json:"bytes"`
	Revenue   int64     `json:"revenue"`
}

// The payload the closer queues for a closed contract with payouts.
type providerTotalsDrainPayload struct {
	Private    bool                       `json:"_private_task_arguments"`
	Version    int                        `json:"version"`
	ContractId server.Id                  `json:"contract_id"`
	Totals     []providerTotalsDrainTotal `json:"totals"`
	Applied    bool                       `json:"applied"`
}

// Queues one contract's provider totals under its run-once owner, due at runAt.
func queueProviderTotalsDrainAllocation(ctx context.Context, owner *session.ClientSession, totals []providerTotalsDrainTotal, applied bool, runAt time.Time) server.Id {
	contractId := server.NewId()
	slices.SortFunc(totals, func(a, b providerTotalsDrainTotal) int { return a.NetworkId.Cmp(b.NetworkId) })
	data, err := json.Marshal(&providerTotalsDrainPayload{Private: true, Version: 1, ContractId: contractId, Totals: totals, Applied: applied})
	server.Raise(err)
	runOnce := task.RunOnce("legacy_provider_totals", contractId)
	var taskId server.Id
	server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(runOnce)}, func(tx server.PgTx) {
		taskId = task.ScheduleTaskInTx(tx, model.ApplyLegacyProviderTotals, json.RawMessage(data), owner,
			runOnce, task.RunAt(runAt), task.MaxTime(10*time.Second), task.RequireQueueOwnership(tx))
	}, server.TxReadCommitted, server.OptNoRetry())
	return taskId
}

// Queued closes of missing contracts lead the ordinary queue, as retired
// contracts' closes led it on Main, and every production lane rotates. The
// production worker's two Run loops must apply 194 provider allocations,
// half of them one provider's rows alternating with seven others', within
// the cursor bound while closes stay queued. Every allocation applies once:
// each provider's account equals the sum of its distinct allocations,
// counting a multi-provider allocation once per provider and an allocation
// already applied before its completion was lost not at all.
func TestProductionProviderTotalsDrainBehindCloseBacklog(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
		defer cancel()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		base := server.NowUtc().Truncate(time.Second).Add(-3 * time.Hour)

		closeIds := make([]server.Id, providerTotalsDrainCloses)
		for index := range closeIds {
			closeIds[index] = server.NewId()
		}
		for offset := 0; offset < len(closeIds); offset += 64 {
			chunk := closeIds[offset:min(offset+64, len(closeIds))]
			keys := make([]server.PgOwnershipKey, 0, len(chunk))
			for _, contractId := range chunk {
				keys = append(keys, task.RunOnceOwnershipKey(model.ScheduledContractCloseRunOnce(contractId)))
			}
			server.OwnedTx(ctx, keys, func(tx server.PgTx) {
				for _, contractId := range chunk {
					task.ScheduleTaskInTx(tx, work.CloseScheduledContract, &work.CloseScheduledContractArgs{Private: true,
						ScheduledContractClose: work.ScheduledContractClose{ContractId: contractId, Deadline: base}}, owner,
						model.ScheduledContractCloseRunOnce(contractId), task.RunAt(base.Add(-time.Minute)),
						task.MaxTime(30*time.Second), task.RequireQueueOwnership(tx))
				}
			}, server.TxReadCommitted, server.OptNoRetry())
		}

		hot := server.NewId()
		others := make([]server.Id, 7)
		for index := range others {
			others[index] = server.NewId()
		}
		type sum struct{ bytes, revenue int64 }
		expected := map[server.Id]*sum{}
		credit := func(networkId server.Id, bytes, revenue int64) {
			if expected[networkId] == nil {
				expected[networkId] = &sum{}
			}
			expected[networkId].bytes += bytes
			expected[networkId].revenue += revenue
		}
		totalsIds := []server.Id{}
		for index := range 192 {
			networkId := hot
			if index%2 == 1 {
				networkId = others[(index/2)%len(others)]
			}
			bytes, revenue := int64(1000+index), int64(7+3*index)
			credit(networkId, bytes, revenue)
			totalsIds = append(totalsIds, queueProviderTotalsDrainAllocation(ctx, owner,
				[]providerTotalsDrainTotal{{NetworkId: networkId, Bytes: bytes, Revenue: revenue}}, false,
				base.Add(time.Hour+time.Duration(index)*time.Second)))
		}
		// One contract paid two providers; it applies under both accounts.
		credit(others[0], 5000, 50)
		credit(others[1], 6000, 60)
		totalsIds = append(totalsIds, queueProviderTotalsDrainAllocation(ctx, owner, []providerTotalsDrainTotal{
			{NetworkId: others[0], Bytes: 5000, Revenue: 50},
			{NetworkId: others[1], Bytes: 6000, Revenue: 60},
		}, false, base.Add(time.Hour+100*time.Second)))
		// This allocation committed its credit and marker before its handback
		// was lost. The account already holds it once.
		credit(others[2], 7000, 70)
		totalsIds = append(totalsIds, queueProviderTotalsDrainAllocation(ctx, owner,
			[]providerTotalsDrainTotal{{NetworkId: others[2], Bytes: 7000, Revenue: 70}}, true,
			base.Add(time.Hour+101*time.Second)))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_balance
				(network_id,provided_byte_count,provided_net_revenue_nano_cents) VALUES ($1,7000,70)`, others[2]))
		})
		originals := task.GetTasks(ctx, totalsIds...)
		if len(originals) != len(totalsIds) {
			t.Fatal("fixture lost a provider allocation")
		}

		trace := &providerTotalsDrainTrace{exceeded: make(chan struct{})}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		defer func() { server.Raise(scope.Close()) }()
		worker := initTaskWorkerWithSettings(ctx, task.DefaultTaskWorkerSettings(), WorkloadProfileProduction)
		runs := make(chan any, 2)
		for range 2 {
			go func() { runs <- server.HandleError(worker.Run) }()
		}
		joined := false
		stop := func() {
			if joined {
				return
			}
			joined = true
			worker.Drain()
			worker.WaitFinalHandback()
			for range 2 {
				if recovered := <-runs; recovered != nil {
					t.Error("a Run loop failed", recovered)
				}
			}
			worker.Close()
		}
		defer stop()
		pendingTotals := func() (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE task_id=ANY($1)`, totalsIds).Scan(&count))
			})
			return
		}
		exceeded := false
		for pending := pendingTotals(); pending != 0; pending = pendingTotals() {
			select {
			case <-trace.exceeded:
				exceeded = true
			case <-ctx.Done():
				t.Fatal("provider totals did not drain before the deadline", pending, ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
			if exceeded {
				stop()
				t.Fatalf("%d claim cursors left %d of %d provider allocations queued behind the close backlog",
					trace.count(), pendingTotals(), len(totalsIds))
			}
		}
		cursors := trace.count()
		stop()

		var queuedCloses int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name=$1`,
				work.NewScheduledContractClosureTaskTarget().TargetFunctionName()).Scan(&queuedCloses))
		})
		if queuedCloses == 0 || queuedCloses == providerTotalsDrainCloses {
			t.Fatalf("the close backlog did not keep the ordinary queue: %d of %d queued", queuedCloses, providerTotalsDrainCloses)
		}
		finished := task.GetFinishedTasks(ctx, totalsIds...)
		if len(finished) != len(totalsIds) {
			t.Fatal("a provider allocation did not finish exactly once", len(finished), len(totalsIds))
		}
		for _, taskId := range totalsIds {
			var before, after providerTotalsDrainPayload
			err := errors.Join(json.Unmarshal([]byte(originals[taskId].ArgsJson), &before), json.Unmarshal([]byte(finished[taskId].ArgsJson), &after))
			if err != nil || !after.Applied || after.ContractId != before.ContractId || !slices.Equal(after.Totals, before.Totals) ||
				finished[taskId].RescheduleError != "" {
				t.Fatal("a finished allocation changed or lost its applied marker", err)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			for networkId, want := range expected {
				var bytes, revenue int64
				server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count,provided_net_revenue_nano_cents
					FROM account_balance WHERE network_id=$1`, networkId).Scan(&bytes, &revenue))
				if bytes != want.bytes || revenue != want.revenue {
					t.Fatalf("a provider account differs from its allocations: bytes %d want %d, revenue %d want %d",
						bytes, want.bytes, revenue, want.revenue)
				}
			}
		})
		t.Logf("drained %d provider allocations in %d claim cursors with %d of %d closes queued",
			len(totalsIds), cursors, queuedCloses, providerTotalsDrainCloses)
	})
}
