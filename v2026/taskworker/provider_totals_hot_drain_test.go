// Provider totals concentrated on a few providers must drain faster than they
// are produced when every statement pays a network round trip, and every
// allocation still applies exactly once.
package taskworker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// Round trip added to every statement, near Main's taskworker-to-primary
// latency under load.
const hotProviderDrainStatementLatency = 2 * time.Millisecond

// Queued allocations older than the test, and the share of all allocations the
// hot provider earns (Main's largest four had 82% on 2026-11-01).
const hotProviderDrainBacklog = 40000
const hotProviderDrainHotShare = 0.7

// Synthetic production: rows queued per tick, the tick, and how long it runs.
const hotProviderDrainTickRows = 30
const hotProviderDrainTick = 100 * time.Millisecond
const hotProviderDrainProduction = 120 * time.Second

// When production ends, every allocation queued more than this long before
// must be applied.
const hotProviderDrainMaxAge = 90 * time.Second

// Delays every statement by a fixed round trip.
type hotProviderDrainLatency struct{}

func (hotProviderDrainLatency) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	time.Sleep(hotProviderDrainStatementLatency)
	return ctx
}

func (hotProviderDrainLatency) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// One queued allocation the test expects to apply once.
type hotProviderDrainAllocation struct {
	taskId    server.Id
	networkId server.Id
	bytes     int64
	revenue   int64
	queued    time.Time
}

// Queues provider allocations as copies of one producer-written template row,
// with their own ids, payloads, run-once keys and due times, unclaimed as a
// producer leaves them.
type hotProviderDrainQueue struct {
	ctx      context.Context
	template server.Id
	hot      server.Id
	others   []server.Id

	stateLock   sync.Mutex
	allocations []hotProviderDrainAllocation
	sequence    int
}

// An id whose leading creation millisecond is at, as task ids are.
func hotProviderDrainId(at time.Time) server.Id {
	id := server.ContractIdFloor(at)
	_, err := rand.Read(id[6:])
	server.Raise(err)
	return id
}

// The next allocation's provider and amounts, hot providers first by share.
func (self *hotProviderDrainQueue) next(queued time.Time) hotProviderDrainAllocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sequence++
	networkId := self.hot
	if float64(self.sequence%100) >= 100*hotProviderDrainHotShare {
		networkId = self.others[self.sequence%len(self.others)]
	}
	return hotProviderDrainAllocation{
		taskId:    hotProviderDrainId(queued),
		networkId: networkId,
		bytes:     int64(1000 + self.sequence%977),
		revenue:   int64(7 + self.sequence%31),
		queued:    queued,
	}
}

// Inserts the allocations in one statement, due when queued.
func (self *hotProviderDrainQueue) insert(allocations []hotProviderDrainAllocation) {
	taskIds := make([]server.Id, len(allocations))
	argsJsons := make([]string, len(allocations))
	runOnceKeys := make([]string, len(allocations))
	runAts := make([]time.Time, len(allocations))
	for index, allocation := range allocations {
		contractId := server.NewId()
		data, err := json.Marshal(&providerTotalsDrainPayload{Private: true, Version: 1, ContractId: contractId,
			Totals: []providerTotalsDrainTotal{{NetworkId: allocation.networkId, Bytes: allocation.bytes, Revenue: allocation.revenue}}})
		server.Raise(err)
		taskIds[index] = allocation.taskId
		argsJsons[index] = string(data)
		runOnceKeys[index] = task.RunOnce("legacy_provider_totals", contractId).String()
		runAts[index] = allocation.queued
	}
	server.Tx(self.ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(self.ctx, `INSERT INTO pending_task (
				task_id,function_name,args_json,client_address,client_address_hash,client_address_port,
				client_by_jwt_json,run_at,run_once_key,run_priority,run_max_time_seconds,claim_time,release_time)
			SELECT copy.task_id,template.function_name,copy.args_json,template.client_address,template.client_address_hash,
				template.client_address_port,template.client_by_jwt_json,copy.run_at,copy.run_once_key,template.run_priority,
				template.run_max_time_seconds,$6,$6
			FROM pending_task AS template,
				unnest($2::uuid[],$3::text[],$4::text[],$5::timestamp[]) AS copy(task_id,args_json,run_once_key,run_at)
			WHERE template.task_id=$1`, self.template, taskIds, argsJsons, runOnceKeys, runAts, time.Time{}))
		if tag.RowsAffected() != int64(len(allocations)) {
			panic("provider allocation copies were not all queued")
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.allocations = append(self.allocations, allocations...)
	}()
}

func (self *hotProviderDrainQueue) queued() []hotProviderDrainAllocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]hotProviderDrainAllocation{}, self.allocations...)
}

// The production worker, its eight Run loops and its settlement startup face a
// backlog that is mostly one provider's while the same mix keeps arriving, and
// every statement pays a round trip. Before the provider totals drain, one
// provider's rows applied one cohort at a time and the oldest-first queue
// waited behind them, as on Main on 2026-11-01. When production ends, every
// allocation queued more than hotProviderDrainMaxAge earlier must be applied,
// and each provider's account must equal the sum of its applied allocations:
// the rows that left the queue plus those whose applied marker is set.
func TestProductionProviderTotalsDrainOutpacesHotProviderProduction(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
		defer cancel()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		started := server.NowUtc()

		queue := &hotProviderDrainQueue{ctx: ctx, hot: server.NewId()}
		for range 8 {
			queue.others = append(queue.others, server.NewId())
		}
		// The template is a real producer row for a provider of its own. A
		// lease far in the future keeps it from both readers; it is no
		// allocation of the test.
		queue.template = queueProviderTotalsDrainAllocation(ctx, owner, []providerTotalsDrainTotal{{NetworkId: server.NewId(),
			Bytes: 1, Revenue: 1}}, false, started.Add(-10*time.Minute))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET release_time=$2 WHERE task_id=$1`, queue.template, started.Add(24*time.Hour)))
		})
		for offset := 0; offset < hotProviderDrainBacklog; offset += 1000 {
			chunk := []hotProviderDrainAllocation{}
			for index := offset; index < min(offset+1000, hotProviderDrainBacklog); index++ {
				// Queued over the five minutes before the backlog's last row.
				chunk = append(chunk, queue.next(started.Add(-10*time.Minute+time.Duration(index)*5*time.Minute/hotProviderDrainBacklog)))
			}
			queue.insert(chunk)
		}

		work.ScheduleSettlementAccountingTasks(ctx)
		scope, err := server.NewTestPgQueryScope(ctx, hotProviderDrainLatency{})
		server.Raise(err)
		defer func() { server.Raise(scope.Close()) }()

		worker := initTaskWorkerWithSettings(ctx, task.DefaultTaskWorkerSettings(), WorkloadProfileProduction)
		const loops = 8
		runs := make(chan any, loops)
		for range loops {
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
			for range loops {
				if recovered := <-runs; recovered != nil {
					t.Error("a Run loop failed", recovered)
				}
			}
			worker.Close()
		}
		defer stop()

		productionEnd := time.Now().Add(hotProviderDrainProduction)
		for time.Now().Before(productionEnd) {
			tickStart := time.Now()
			now := server.NowUtc()
			chunk := make([]hotProviderDrainAllocation, 0, hotProviderDrainTickRows)
			for range hotProviderDrainTickRows {
				chunk = append(chunk, queue.next(now))
			}
			queue.insert(chunk)
			select {
			case <-ctx.Done():
				t.Fatal("production did not finish before the deadline", ctx.Err())
			case <-time.After(hotProviderDrainTick - time.Since(tickStart)):
			}
		}
		end := server.NowUtc()
		allocations := queue.queued()
		taskIds := make([]server.Id, len(allocations))
		for index, allocation := range allocations {
			taskIds[index] = allocation.taskId
		}
		pending := map[server.Id]bool{}
		readPending := func() {
			pending = map[server.Id]bool{}
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT task_id,COALESCE((args_json::jsonb->>'applied')::boolean,false)
					FROM pending_task WHERE task_id=ANY($1)`, taskIds)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var taskId server.Id
						var applied bool
						server.Raise(rows.Scan(&taskId, &applied))
						pending[taskId] = applied
					}
				})
			})
		}
		readPending()
		stale, backlogLeft := 0, 0
		for _, allocation := range allocations {
			if applied, ok := pending[allocation.taskId]; ok && !applied {
				if allocation.queued.Before(started) {
					backlogLeft++
				}
				if end.Sub(allocation.queued) > hotProviderDrainMaxAge {
					stale++
				}
			}
		}
		stop()
		if stale != 0 {
			t.Errorf("%d of %d allocations queued more than %s before production ended were not applied (%d of %d backlog rows)",
				stale, len(allocations), hotProviderDrainMaxAge, backlogLeft, hotProviderDrainBacklog)
		}

		// Each allocation applies once: the account holds every allocation
		// whose row left the queue or carries its applied marker, and none of
		// the rest.
		readPending()
		type sum struct{ bytes, revenue int64 }
		expected := map[server.Id]*sum{}
		applied := 0
		for _, allocation := range allocations {
			if marker, ok := pending[allocation.taskId]; ok && !marker {
				continue
			}
			applied++
			if expected[allocation.networkId] == nil {
				expected[allocation.networkId] = &sum{}
			}
			expected[allocation.networkId].bytes += allocation.bytes
			expected[allocation.networkId].revenue += allocation.revenue
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, networkId := range append([]server.Id{queue.hot}, queue.others...) {
				var bytes, revenue int64
				server.Raise(conn.QueryRow(ctx, `SELECT COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$1),0),
					COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$1),0)`, networkId).Scan(&bytes, &revenue))
				want := expected[networkId]
				if want == nil {
					want = &sum{}
				}
				if bytes != want.bytes || revenue != want.revenue {
					t.Errorf("a provider account differs from its applied allocations: bytes %d want %d, revenue %d want %d",
						bytes, want.bytes, revenue, want.revenue)
				}
			}
		})
		t.Logf("applied %d of %d allocations (%d backlog) in %s; %d older than %s left unapplied",
			applied, len(allocations), hotProviderDrainBacklog, end.Sub(started).Round(time.Second), stale, hotProviderDrainMaxAge)
	})
}
