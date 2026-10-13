// Proxy egress refresh regressions run the real worker, refresh target, Post,
// retry policy and egress feeds on a synthetic clock. Each completed batch of
// feeds costs synthetic time for the network round trips it took, the pause
// between chunks is taken on the same clock, and so is the task's max-time
// timer. A run's length, its budget stop or timeout, the wait before the next
// run, and the time between two feeds of one allocation are all deterministic.
//
// The model is Main's on 2026-10-12: 823,287 allocations with an address, a
// 10 minute egress ttl refreshed every 2 minutes, and a pending refresh that
// had timed out at its 900 s max time 76 times in a row. Fed one at a time, an
// allocation of a client without provide keys cost ten round trips: seven
// Redis commands, the mirror refill, and a PostgreSQL read before and after
// the refill. None of 60,000 sampled proxy clients had provide keys. The
// measured run made about 420 such reads a second, two for each feed: 210
// feeds a second, close to 0.5 ms a round trip. One seeded allocation stands
// for 100 production allocations, which keeps each test to a few thousand
// allocations while every ratio of work to time is unchanged.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const verifyEgressTestAllocationWeight = 100

const verifyEgressTestAllocationCount = 823287 / verifyEgressTestAllocationWeight

// Synthetic time one network round trip costs.
const verifyEgressTestRoundTripCost = verifyEgressTestAllocationWeight * 500 * time.Microsecond

// Main's pending refresh when it was measured.
const verifyEgressTestErrorCount = 76

// A chunk is a handful of round trips however many allocations it holds.
const verifyEgressTestMaxChunkRoundTripCount = 8

// Hooks are called from the worker's body and timer goroutines while the test
// goroutine waits in EvalTasks; stateLock guards everything they share.
type verifyEgressRefreshSimulation struct {
	ctx      context.Context
	owner    *session.ClientSession
	worker   *task.TaskWorker
	runOnce  *task.RunOnceOption
	settings *model.VerifySettings
	baseTime time.Time

	stateLock       sync.Mutex
	now             time.Time
	roundTripCost   time.Duration
	clientFeedTimes map[server.Id][]time.Time
	// The client and round trip counts of each batch the current run fed.
	runBatchClientCounts    []int
	runBatchRoundTripCounts []int
	runStartTime            time.Time
	timerRegistered         chan struct{}
	timer                   chan time.Time
	timerDeadline           time.Time
	timerBodyCtx            context.Context
}

type verifyEgressTestPending struct {
	runAt      time.Time
	argsJson   string
	errorCount int
	errorText  string
}

// A successor's resume position, read without the refresh's own types.
type verifyEgressTestArgs struct {
	After *server.Id `json:"after"`
}

type verifyEgressTestRun struct {
	finishedTaskIds    []server.Id
	rescheduledTaskIds []server.Id
	runLength          time.Duration
	// Until the pending successor or retry is due.
	wait                 time.Duration
	pending              *verifyEgressTestPending
	args                 verifyEgressTestArgs
	batchClientCounts    []int
	batchRoundTripCounts []int
}

func verifyEgressTestEnv(t *testing.T, run func(testing.TB, context.Context)) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		controller.SetStConfig(&controller.StConfig{Enabled: true})
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer func() { controller.SetVerifySettings(nil); controller.SetStConfig(nil) }()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
		defer cancel()
		run(t, ctx)
	})
}

func newVerifyEgressRefreshSimulation(t testing.TB, ctx context.Context, roundTripCost time.Duration, errorCount int) *verifyEgressRefreshSimulation {
	t.Helper()
	baseTime := server.NowUtc().Truncate(time.Second)
	simulation := &verifyEgressRefreshSimulation{
		runOnce:         task.RunOnce("refresh_verify_proxy_egress"),
		settings:        controller.VerifySettings(),
		baseTime:        baseTime,
		now:             baseTime,
		roundTripCost:   roundTripCost,
		clientFeedTimes: map[server.Id][]time.Time{},
	}
	hookCtx := context.WithValue(ctx, refreshVerifyProxyEgressClockKey{}, &refreshVerifyProxyEgressClock{
		now: simulation.clock,
		// a pause between chunks, at the weight of the chunks it separates
		wait: func(duration time.Duration) { simulation.advance(verifyEgressTestAllocationWeight * duration) },
	})
	hookCtx = task.Testing_WithTaskRunAfter(hookCtx, func(bodyCtx context.Context, maxTime time.Duration) <-chan time.Time {
		return simulation.after(bodyCtx, maxTime)
	})
	hookCtx = model.Testing_WithVerifyProxyEgressRefreshObserver(hookCtx, func(clientIds []server.Id, roundTripCount int) {
		simulation.observe(ctx, clientIds, roundTripCount)
	})
	simulation.ctx = hookCtx
	simulation.owner = session.NewLocalClientSession(ctx, "", nil)
	t.Cleanup(simulation.owner.Cancel)
	simulation.worker = startupClosureWorker(hookCtx, NewRefreshVerifyProxyEgressTaskTarget())
	t.Cleanup(simulation.worker.Close)

	task.ScheduleTask(RefreshVerifyProxyEgress, &RefreshVerifyProxyEgressArgs{}, simulation.owner, simulation.runOnce,
		task.RunAt(baseTime), task.MaxTime(refreshVerifyProxyEgressMaxTime))
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET reschedule_error_count=$2 WHERE run_once_key=$1`,
			simulation.runOnce.String(), errorCount))
	}, server.OptNoRetry())
	return simulation
}

func (self *verifyEgressRefreshSimulation) clock() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *verifyEgressRefreshSimulation) advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(duration)
}

func (self *verifyEgressRefreshSimulation) setRoundTripCost(roundTripCost time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.roundTripCost = roundTripCost
}

// The adapter's max-time timer, armed relative to the run's synthetic start.
func (self *verifyEgressRefreshSimulation) after(bodyCtx context.Context, maxTime time.Duration) <-chan time.Time {
	timer := make(chan time.Time)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.timerRegistered == nil || self.timer != nil {
		return timer
	}
	self.timer = timer
	self.timerDeadline = self.runStartTime.Add(maxTime)
	self.timerBodyCtx = bodyCtx
	close(self.timerRegistered)
	return timer
}

// A batch of feeds completed: charge its round trips, record when each of its
// allocations was fed, then fire the max-time timer if the run has used up its
// max time and wait until the body has observed it.
func (self *verifyEgressRefreshSimulation) observe(ctx context.Context, clientIds []server.Id, roundTripCount int) {
	var timerRegistered chan struct{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.now = self.now.Add(time.Duration(roundTripCount) * self.roundTripCost)
		for _, clientId := range clientIds {
			self.clientFeedTimes[clientId] = append(self.clientFeedTimes[clientId], self.now)
		}
		self.runBatchClientCounts = append(self.runBatchClientCounts, len(clientIds))
		self.runBatchRoundTripCounts = append(self.runBatchRoundTripCounts, roundTripCount)
		timerRegistered = self.timerRegistered
	}()
	if timerRegistered == nil {
		return
	}
	select {
	case <-timerRegistered:
	case <-ctx.Done():
		return
	}
	var timer chan time.Time
	var bodyCtx context.Context
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.timer != nil && !self.now.Before(self.timerDeadline) {
			timer, bodyCtx = self.timer, self.timerBodyCtx
			self.timer = nil
		}
	}()
	if timer == nil {
		return
	}
	close(timer)
	select {
	case <-bodyCtx.Done():
	case <-ctx.Done():
	}
}

// Seeds proxy allocations in client order. Documentation addresses repeat
// across synthetic blocks; a shared address only makes its forward entry
// ambiguous, and every allocation is still fed.
func (self *verifyEgressRefreshSimulation) seed(t testing.TB, count int) []server.Id {
	t.Helper()
	documentationNetworks := []int64{
		192<<24 | 0<<16 | 2<<8,
		198<<24 | 51<<16 | 100<<8,
		203<<24 | 0<<16 | 113<<8,
	}
	clientIds := make([]server.Id, count)
	clientIpv4s := make([]int64, count)
	blocks := make([]string, count)
	for index := range count {
		clientIds[index] = server.NewId()
		address := index % (256 * len(documentationNetworks))
		clientIpv4s[index] = documentationNetworks[address/256] | int64(address%256)
		blocks[index] = fmt.Sprintf("synthetic-%d", index/(256*len(documentationNetworks)))
	}
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `INSERT INTO proxy_client
			(proxy_id,client_id,instance_id,proxy_host,block,client_ipv4,proxy_client_json)
			SELECT seeded.client_id,seeded.client_id,seeded.client_id,'synthetic.example',seeded.block,seeded.client_ipv4,'{}'
			FROM unnest($1::uuid[],$2::bigint[],$3::varchar[]) AS seeded(client_id,client_ipv4,block)`,
			clientIds, clientIpv4s, blocks))
	}, server.OptNoRetry())
	return clientIds
}

func (self *verifyEgressRefreshSimulation) readPending(t testing.TB) *verifyEgressTestPending {
	t.Helper()
	var pendings []*verifyEgressTestPending
	server.Db(self.ctx, func(conn server.PgConn) {
		rows, err := conn.Query(self.ctx, `SELECT run_at,args_json,reschedule_error_count,COALESCE(reschedule_error,'')
			FROM pending_task WHERE run_once_key=$1`, self.runOnce.String())
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				pending := &verifyEgressTestPending{}
				server.Raise(rows.Scan(&pending.runAt, &pending.argsJson, &pending.errorCount, &pending.errorText))
				pendings = append(pendings, pending)
			}
		})
	})
	if len(pendings) != 1 {
		t.Fatalf("refresh must have exactly one pending run, found %d", len(pendings))
	}
	return pendings[0]
}

// Makes the pending run due, runs it, and reads what it left pending.
func (self *verifyEgressRefreshSimulation) run(t testing.TB) *verifyEgressTestRun {
	t.Helper()
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE run_once_key=$1`,
			self.runOnce.String(), time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)))
	}, server.OptNoRetry())
	var runStartTime time.Time
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		runStartTime = self.now
		self.runStartTime = self.now
		self.runBatchClientCounts = nil
		self.runBatchRoundTripCounts = nil
		self.timerRegistered = make(chan struct{})
		self.timer = nil
	}()
	finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err := self.worker.EvalTasks(1)
	handbackTime := server.NowUtc()
	if len(postRescheduledTaskIds) != 0 || err != nil {
		t.Fatal("refresh post or evaluation failed", postRescheduledTaskIds, err)
	}
	if len(finishedTaskIds)+len(rescheduledTaskIds) != 1 {
		t.Fatal("refresh did not run exactly once", finishedTaskIds, rescheduledTaskIds)
	}
	pending := self.readPending(t)
	run := &verifyEgressTestRun{
		finishedTaskIds:    finishedTaskIds,
		rescheduledTaskIds: rescheduledTaskIds,
		wait:               max(0, pending.runAt.Sub(handbackTime).Round(time.Second)),
		pending:            pending,
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		run.runLength = self.now.Sub(runStartTime)
		run.batchClientCounts = self.runBatchClientCounts
		run.batchRoundTripCounts = self.runBatchRoundTripCounts
	}()
	server.Raise(json.Unmarshal([]byte(pending.argsJson), &run.args))
	return run
}

// The allocations fed at least once.
func (self *verifyEgressRefreshSimulation) fedClientIds(clientIds []server.Id) map[server.Id]bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	fedClientIds := map[server.Id]bool{}
	for _, clientId := range clientIds {
		if 0 < len(self.clientFeedTimes[clientId]) {
			fedClientIds[clientId] = true
		}
	}
	return fedClientIds
}

// A sweep slowed to more than one budget, here by round trips twenty times
// Main's, behind a pending refresh that had already timed out 76 times. Each
// run must stop starting chunks at its budget, well inside its max time,
// finish, and re-arm a successor that is due at once and resumes after the
// last client it fed; the sweep's last run re-arms after the refresh interval.
// Before the fix the run fed allocations one at a time until its max time
// cancelled it, which dropped the re-arm, and every retry started again at the
// first allocation.
func TestRefreshVerifyProxyEgressStopsAtBudgetAndResumes(t *testing.T) {
	verifyEgressTestEnv(t, func(t testing.TB, ctx context.Context) {
		roundTripCost := 20 * verifyEgressTestRoundTripCost
		simulation := newVerifyEgressRefreshSimulation(t, ctx, roundTripCost, verifyEgressTestErrorCount)
		clientIds := simulation.seed(t, verifyEgressTestAllocationCount)
		chunkPause := verifyEgressTestAllocationWeight * refreshVerifyProxyEgressChunkPause

		fedCount := 0
		budgetStopCount := 0
		for {
			run := simulation.run(t)
			if len(run.finishedTaskIds) != 1 || len(run.rescheduledTaskIds) != 0 {
				t.Fatalf("refresh did not finish inside its max time: finished=%v rescheduled=%v error=%q",
					run.finishedTaskIds, run.rescheduledTaskIds, run.pending.errorText)
			}
			for _, clientCount := range run.batchClientCounts {
				fedCount += clientCount
			}
			fed := simulation.fedClientIds(clientIds)
			for index, clientId := range clientIds {
				if fed[clientId] != (index < fedCount) {
					t.Fatalf("run did not continue the sweep in client order: allocation %d fed=%t, want the first %d fed",
						index, fed[clientId], fedCount)
				}
			}
			if run.args.After == nil {
				if fedCount != len(clientIds) || run.wait != simulation.settings.EgressRefreshInterval {
					t.Fatalf("sweep end did not re-arm a new sweep after the refresh interval: fed %d of %d, wait=%s",
						fedCount, len(clientIds), run.wait)
				}
				break
			}

			budgetStopCount += 1
			for _, clientCount := range run.batchClientCounts {
				if clientCount != model.VerifyProxyEgressRefreshChunkSize {
					t.Fatalf("budgeted run fed a batch of %d, not whole chunks", clientCount)
				}
			}
			lastChunkCost := time.Duration(run.batchRoundTripCounts[len(run.batchRoundTripCounts)-1]) * roundTripCost
			if run.runLength < refreshVerifyProxyEgressBudget || refreshVerifyProxyEgressBudget <= run.runLength-lastChunkCost-chunkPause {
				t.Fatalf("refresh did not stop at its budget: ran %s, budget %s, last chunk %s after a %s pause",
					run.runLength, refreshVerifyProxyEgressBudget, lastChunkCost, chunkPause)
			}
			if refreshVerifyProxyEgressMaxTime <= run.runLength {
				t.Fatalf("budgeted run of %s reached its max time", run.runLength)
			}
			if run.wait != 0 || run.pending.errorCount != 0 {
				t.Fatalf("budget stop did not re-arm at once: wait=%s errors=%d", run.wait, run.pending.errorCount)
			}
			if *run.args.After != clientIds[fedCount-1] {
				t.Fatal("successor does not resume after the last fed allocation", run.pending.argsJson)
			}
		}
		if budgetStopCount == 0 {
			t.Fatal("budget fixture finished its sweep inside one budget")
		}
	})
}

// Every allocation's lease must be renewed before the egress ttl lapses, for
// as long as it stays allocated. At Main's allocation count that takes a sweep
// of chunks, each a handful of round trips. Before the fix one run fed
// allocations one at a time, ten round trips each, timed out after its max
// time with most of them unfed, and the generic backoff kept every lease
// lapsed until a retry 30 to 90 minutes later.
func TestRefreshVerifyProxyEgressKeepsEveryAllocationLive(t *testing.T) {
	verifyEgressTestEnv(t, func(t testing.TB, ctx context.Context) {
		simulation := newVerifyEgressRefreshSimulation(t, ctx, verifyEgressTestRoundTripCost, verifyEgressTestErrorCount)
		clientIds := simulation.seed(t, verifyEgressTestAllocationCount)
		window := 15 * time.Minute
		for {
			run := simulation.run(t)
			for index, clientCount := range run.batchClientCounts {
				if clientCount == model.VerifyProxyEgressRefreshChunkSize && verifyEgressTestMaxChunkRoundTripCount < run.batchRoundTripCounts[index] {
					t.Fatalf("a chunk of %d allocations took %d round trips", clientCount, run.batchRoundTripCounts[index])
				}
			}
			if window <= simulation.clock().Sub(simulation.baseTime)+run.wait {
				break
			}
			simulation.advance(run.wait)
		}

		// Each lease was renewed by the create-side feed at the start.
		windowEnd := simulation.baseTime.Add(window)
		var longestGap time.Duration
		var longestGapClientIndex int
		func() {
			simulation.stateLock.Lock()
			defer simulation.stateLock.Unlock()
			for index, clientId := range clientIds {
				feedTime := simulation.baseTime
				for _, nextFeedTime := range append(append([]time.Time{}, simulation.clientFeedTimes[clientId]...), windowEnd) {
					if gap := nextFeedTime.Sub(feedTime); longestGap < gap {
						longestGap, longestGapClientIndex = gap, index
					}
					feedTime = nextFeedTime
				}
			}
		}()
		if simulation.settings.EgressTtl <= longestGap {
			t.Fatalf("allocation %d of %d went %s without a feed, beyond the %s egress ttl",
				longestGapClientIndex, len(clientIds), longestGap, simulation.settings.EgressTtl)
		}
	})
}

// A run can still overrun its max time, for example on a stalled Redis. The
// timeout must not stretch the cadence: the pending run keeps its position and
// retries within the error retry cap, not after the generic backoff, which
// reached its one hour cap on Main.
func TestRefreshVerifyProxyEgressTimeoutRetriesWithinCap(t *testing.T) {
	verifyEgressTestEnv(t, func(t testing.TB, ctx context.Context) {
		// Two round trips alone outlast the max time.
		simulation := newVerifyEgressRefreshSimulation(t, ctx, refreshVerifyProxyEgressMaxTime/2, verifyEgressTestErrorCount)
		clientIds := simulation.seed(t, verifyEgressTestAllocationCount)

		timedOut := simulation.run(t)
		if len(timedOut.rescheduledTaskIds) != 1 || !strings.Contains(timedOut.pending.errorText, "Timeout") {
			t.Fatalf("stalled refresh did not time out: finished=%v error=%q", timedOut.finishedTaskIds, timedOut.pending.errorText)
		}
		if refreshVerifyProxyEgressErrorRetryCap < timedOut.wait {
			t.Fatalf("timed-out refresh retries after %s, beyond its %s cap", timedOut.wait, refreshVerifyProxyEgressErrorRetryCap)
		}
		if timedOut.args.After != nil {
			t.Fatal("timed-out refresh changed its resume position", timedOut.pending.argsJson)
		}

		simulation.setRoundTripCost(verifyEgressTestRoundTripCost)
		simulation.advance(timedOut.wait)
		retried := simulation.run(t)
		if len(retried.finishedTaskIds) != 1 || retried.pending.errorCount != 0 || retried.args.After != nil {
			t.Fatalf("retry after the timeout did not finish its sweep and re-arm: finished=%v error=%q args=%s",
				retried.finishedTaskIds, retried.pending.errorText, retried.pending.argsJson)
		}
		if fed := simulation.fedClientIds(clientIds); len(fed) != len(clientIds) {
			t.Fatalf("retry after the timeout fed %d of %d allocations", len(fed), len(clientIds))
		}
	})
}
