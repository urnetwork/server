// Dispatcher publication amortizes transport while retaining each queue owner.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

type legacyDispatchPublicationCounts struct {
	Queries    int
	Batches    int
	Statements int
	Replies    int
	BatchEnds  int
	Failed     bool
}

type legacyDispatchPublicationTrace struct {
	enabled   atomic.Bool
	stateLock sync.Mutex
	counts    legacyDispatchPublicationCounts
}

type legacyDispatchPublicationQueryKey struct{}

func legacyDispatchPublicationSql(sql string) bool {
	return strings.HasPrefix(strings.Join(strings.Fields(sql), " "), "INSERT INTO pending_task (")
}

// Observe real pgx query submissions, batch submissions and consumed replies.
// These are driver exchanges, not IP packets or a modeled production rate.
func (self *legacyDispatchPublicationTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if self.enabled.Load() && legacyDispatchPublicationSql(data.SQL) {
		self.stateLock.Lock()
		self.counts.Queries++
		self.counts.Statements++
		self.stateLock.Unlock()
		return context.WithValue(ctx, legacyDispatchPublicationQueryKey{}, true)
	}
	return ctx
}

func (self *legacyDispatchPublicationTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if observed, _ := ctx.Value(legacyDispatchPublicationQueryKey{}).(bool); observed {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.counts.Replies++
		self.counts.Failed = self.counts.Failed || data.Err != nil
	}
}

func (self *legacyDispatchPublicationTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if self.enabled.Load() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.counts.Batches++
		for _, query := range data.Batch.QueuedQueries {
			if legacyDispatchPublicationSql(query.SQL) {
				self.counts.Statements++
			} else {
				self.counts.Failed = true
			}
		}
		return context.WithValue(ctx, legacyDispatchPublicationQueryKey{}, true)
	}
	return ctx
}

func (self *legacyDispatchPublicationTrace) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if observed, _ := ctx.Value(legacyDispatchPublicationQueryKey{}).(bool); observed {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.counts.Replies++
		self.counts.Failed = self.counts.Failed || data.Err != nil || !legacyDispatchPublicationSql(data.SQL)
	}
}

func (self *legacyDispatchPublicationTrace) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	if observed, _ := ctx.Value(legacyDispatchPublicationQueryKey{}).(bool); observed {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.counts.BatchEnds++
		self.counts.Failed = self.counts.Failed || data.Err != nil
	}
}

func (self *legacyDispatchPublicationTrace) snapshot() legacyDispatchPublicationCounts {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.counts
}

func legacyDispatchPublicationFixture() (*FlushLegacySettlementsArgs, *FlushLegacySettlementsResult) {
	stamp := server.NowUtc().Truncate(time.Microsecond)
	end := server.NewId()
	result := &FlushLegacySettlementsResult{Dispatch: &model.LegacySettlementDispatchResult{
		Private:            true,
		Cursor:             &model.LegacySettlementCursor{ContractId: end, NextAttemptTime: stamp.Add(-time.Minute), PassEndTime: stamp},
		PayerCursor:        &model.LegacySettlementPayerCursor{After: &end, End: end, PassEndTime: stamp},
		SourceCursor:       &model.LegacySettlementPayerCursor{End: end, PassEndTime: stamp},
		RegistrationCursor: &model.LegacySettlementOwnerCursor{After: &end, End: end},
	}}
	for range 16 {
		id := server.NewId()
		// An equal UUID still names two independent queue domains.
		result.Dispatch.PayerNetworkIds = append(result.Dispatch.PayerNetworkIds, id)
		result.Dispatch.SourceClientIds = append(result.Dispatch.SourceClientIds, id)
	}
	return &FlushLegacySettlementsArgs{Shard: 3}, result
}

func legacyDispatchPublicationMetrics(t testing.TB) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal("task commit counters could not be read", err)
	}
	values := map[string]float64{}
	for _, family := range families {
		if slices.Contains([]string{"urnetwork_task_submitted_total", "urnetwork_task_balked_total", "urnetwork_task_finished_total"}, family.GetName()) {
			for _, metric := range family.Metric {
				values[family.GetName()] += metric.GetCounter().GetValue()
			}
		}
	}
	if len(values) != 3 {
		t.Fatal("task commit counters are incomplete")
	}
	return values
}

// The old Post performs 33 separately awaited query submissions while owning
// every key. The same real native writes now acknowledge one complete batch.
func TestLegacyDispatcherPublicationUsesOneBatchAndKeepsCoalescing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		trace := &legacyDispatchPublicationTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		defer func() { server.Raise(scope.Close()) }()
		args, result := legacyDispatchPublicationFixture()
		publish := func() {
			withLegacyDispatcherQueueTestTx(ctx, args.Shard, result, func(tx server.PgTx) {
				trace.enabled.Store(true)
				defer trace.enabled.Store(false)
				server.Raise(FlushLegacySettlementsPost(args, result, owner, tx))
			})
		}
		beforeMetrics := legacyDispatchPublicationMetrics(t)
		before := server.NowUtc()
		publish()
		after := server.NowUtc()
		counts := trace.snapshot()
		if counts != (legacyDispatchPublicationCounts{Batches: 1, Statements: 33, Replies: 33, BatchEnds: 1}) {
			t.Fatalf("dispatcher retained per-owner publication exchanges: %+v; want one acknowledged batch of 33", counts)
		}
		queue := readExpiryRecoveryQueue(t, ctx)
		if len(queue) != 33 {
			t.Fatal("bounded dispatch omitted or duplicated an owner", len(queue))
		}
		for index, ids := range [][]server.Id{result.Dispatch.PayerNetworkIds, result.Dispatch.SourceClientIds} {
			kind, name, target := model.ContractCloseOwnerPayerNetwork, "flush_legacy_payer_settlements", model.NewLegacyPayerSettlementTaskTarget()
			if index == 1 {
				kind, name, target = model.ContractCloseOwnerSourceClient, "flush_legacy_source_settlements", model.NewLegacySourceSettlementTaskTarget()
			}
			for _, id := range ids {
				row, found := queue[task.RunOnce(name, id).String()]
				var decoded model.LegacyPayerSettlementArgs
				if !found || json.Unmarshal([]byte(row.args), &decoded) != nil || !decoded.Private || decoded.Owner == nil ||
					*decoded.Owner != (model.ContractCloseOwner{Kind: kind, Id: id}) || decoded.Cursor != nil || row.function != target.TargetFunctionName() ||
					row.runAt.Before(before.Add(30*time.Second)) || row.runAt.After(after.Add(30*time.Second)) ||
					(index == 0 && decoded.PayerNetworkId != id) || (index == 1 && decoded.PayerNetworkId != (server.Id{})) {
					t.Fatal("batched publication changed typed owner, collection window or target")
				}
			}
		}
		shardKey := task.RunOnce("flush_legacy_settlements_3").String()
		var continuation FlushLegacySettlementsArgs
		row := queue[shardKey]
		wanted := &FlushLegacySettlementsArgs{Shard: args.Shard, Cursor: result.Dispatch.Cursor,
			PayerCursor: result.Dispatch.PayerCursor, SourceCursor: result.Dispatch.SourceCursor, RegistrationCursor: result.Dispatch.RegistrationCursor}
		if json.Unmarshal([]byte(row.args), &continuation) != nil || !reflect.DeepEqual(&continuation, wanted) ||
			row.runAt.Before(before.Add(2*time.Second)) || row.runAt.After(after.Add(2*time.Second)) {
			t.Fatal("batch did not retain the complete dispatcher continuation and idle delay")
		}
		metrics := legacyDispatchPublicationMetrics(t)
		if metrics["urnetwork_task_submitted_total"]-beforeMetrics["urnetwork_task_submitted_total"] != 33 ||
			metrics["urnetwork_task_balked_total"] != beforeMetrics["urnetwork_task_balked_total"] {
			t.Fatal("committed batch did not publish exactly its inserted task counters")
		}
		// Retain an active owner's old args/claim; one earlier existing wake must
		// stay earlier, and later wakes must advance without losing generation.
		earlyKey := task.RunOnce("flush_legacy_payer_settlements", result.Dispatch.PayerNetworkIds[0]).String()
		stamp := server.NowUtc().Truncate(time.Microsecond)
		withLegacyDispatcherQueueTestTx(ctx, args.Shard, result, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$1,claim_time=$2,release_time=$3,claim_generation=7`,
				stamp.Add(time.Hour), stamp, stamp.Add(5*time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2 WHERE run_once_key=$1`, earlyKey, stamp.Add(-time.Minute)))
		})
		original := readExpiryRecoveryQueue(t, ctx)
		result.Dispatch.More = true
		result.Dispatch.Cursor, result.Dispatch.PayerCursor, result.Dispatch.SourceCursor, result.Dispatch.RegistrationCursor = nil, nil, nil, nil
		for attempt := range 2 {
			before = server.NowUtc()
			publish()
			after = server.NowUtc()
			current := readExpiryRecoveryQueue(t, ctx)
			if len(current) != 33 {
				t.Fatal("replayed batch duplicated a queue owner")
			}
			for key, first := range original {
				row := current[key]
				if row.id != first.id || row.args != first.args || row.function != first.function || row.claim != first.claim ||
					!row.claimTime.Equal(first.claimTime) || !row.releaseTime.Equal(first.releaseTime) ||
					row.generation != first.generation+int64(attempt)+1 || row.wakeAt == nil {
					t.Fatal("batch conflict lost the active owner's identity, cursor, claim or requested generation")
				}
				latest := after.Add(30 * time.Second)
				if key == shardKey {
					latest = after
				}
				if row.runAt.After(latest) || row.wakeAt.After(latest) || (key == earlyKey && !row.runAt.Equal(first.runAt)) {
					t.Fatal("coalesced batch postponed an earlier requested wake")
				}
				if attempt == 0 && key != earlyKey {
					earliest := before.Add(30 * time.Second)
					if key == shardKey {
						earliest = before
					}
					if row.runAt.Before(earliest) {
						t.Fatal("batch bypassed its ordinary collection window")
					}
				}
			}
		}
		counts = trace.snapshot()
		if counts != (legacyDispatchPublicationCounts{Batches: 3, Statements: 99, Replies: 99, BatchEnds: 3}) {
			t.Fatal("replay reverted to serial publication or omitted a reply", counts)
		}
		metrics = legacyDispatchPublicationMetrics(t)
		if metrics["urnetwork_task_submitted_total"]-beforeMetrics["urnetwork_task_submitted_total"] != 33 ||
			metrics["urnetwork_task_balked_total"]-beforeMetrics["urnetwork_task_balked_total"] != 66 {
			t.Fatal("coalesced batch changed acknowledged submission accounting")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT count(*)=33 AND bool_and(run_max_time_seconds=30) FROM pending_task`).Scan(&exact))
			if !exact {
				t.Fatal("batched publication changed an owner timeout")
			}
		})
	})
}

// The last queued statement fails after every owner INSERT. Draining that
// failure must abort the whole publication and every commit counter.
func TestLegacyDispatcherPublicationLastReplyFailureRollsBackWholeBatch(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		args, result := legacyDispatchPublicationFixture()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE test_legacy_dispatch_failure (run_once_key text PRIMARY KEY)`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO test_legacy_dispatch_failure VALUES($1)`, task.RunOnce("flush_legacy_settlements_3").String()))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION test_legacy_dispatch_failure_guard() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF EXISTS(SELECT 1 FROM test_legacy_dispatch_failure WHERE run_once_key=NEW.run_once_key) THEN
					RAISE EXCEPTION 'synthetic last dispatcher publication refusal' USING ERRCODE='23514';
				END IF;
				RETURN NEW;
			END $$`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER test_legacy_dispatch_failure_guard BEFORE INSERT OR UPDATE ON pending_task
				FOR EACH ROW EXECUTE FUNCTION test_legacy_dispatch_failure_guard()`))
		})
		before := legacyDispatchPublicationMetrics(t)
		failure := server.HandleError(func() {
			withLegacyDispatcherQueueTestTx(ctx, args.Shard, result, func(tx server.PgTx) {
				server.Raise(FlushLegacySettlementsPost(args, result, owner, tx))
			})
		})
		if failure == nil || !strings.Contains(fmt.Sprint(failure), "synthetic last dispatcher publication refusal") ||
			len(readExpiryRecoveryQueue(t, ctx)) != 0 || !reflect.DeepEqual(before, legacyDispatchPublicationMetrics(t)) {
			t.Fatal("failed final batch reply donated committed queue authority or counters", failure)
		}
		server.Tx(ctx, func(tx server.PgTx) { server.RaisePgResult(tx.Exec(ctx, `DELETE FROM test_legacy_dispatch_failure`)) })
		withLegacyDispatcherQueueTestTx(ctx, args.Shard, result, func(tx server.PgTx) {
			server.Raise(FlushLegacySettlementsPost(args, result, owner, tx))
		})
		after := legacyDispatchPublicationMetrics(t)
		if len(readExpiryRecoveryQueue(t, ctx)) != 33 || after["urnetwork_task_submitted_total"]-before["urnetwork_task_submitted_total"] != 33 ||
			after["urnetwork_task_balked_total"] != before["urnetwork_task_balked_total"] {
			t.Fatal("retry after an atomic batch failure lost or repeated a durable owner")
		}
	})
}

func legacyDispatchAwait(ctx context.Context, signal <-chan struct{}) {
	select {
	case <-signal:
	case <-ctx.Done():
		panic(ctx.Err())
	}
}

type legacyDispatchEval struct {
	finished []server.Id
	retried  []server.Id
	posts    []server.Id
	err      error
}

// A batch never releases its keys before commit. While the completed publisher
// is held at an unrelated post barrier, a real child waits outside BEGIN and an
// independent child finishes. Releasing the publisher lets the exact waiter
// finish once, with both its financial owner and registration wake committed.
func TestLegacyDispatcherPublicationCommitReleasesActualCloseChild(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 30*time.Second)
		defer cancel()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		var ids [2]server.Id
		var closeOwners [2]model.ContractCloseOwner
		deadline := server.NowUtc().Truncate(time.Microsecond).Add(-2 * time.Minute)
		for index := range ids {
			networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-dispatch-publication-%d", index), server.NewId())
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "synthetic-source", "synthetic")
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "synthetic-destination", "synthetic")
			var err error
			if index == 0 {
				model.AddBasicTransferBalance(ctx, networkId, 1000, server.NowUtc(), server.NowUtc().Add(time.Hour))
				ids[index], _, err = model.CreateContract(ctx, networkId, sourceId, networkId, destinationId, 100)
				closeOwners[index] = model.ContractCloseOwner{Kind: model.ContractCloseOwnerPayerNetwork, Id: networkId}
			} else {
				ids[index], err = model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 100)
				closeOwners[index] = model.ContractCloseOwner{Kind: model.ContractCloseOwnerSourceClient, Id: sourceId}
			}
			server.Raise(err)
			server.Raise(model.CloseContract(ctx, ids[index], sourceId, 17, true))
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
					VALUES($1,$2,'settled',$3)`, ids[index], int(ids[index][15])%model.LegacySettlementShardCount, deadline))
			})
			key := task.RunOnce("close_scheduled_contract", ids[index])
			server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(key)}, func(tx server.PgTx) {
				scheduleContractClose(owner, tx, &CloseScheduledContractArgs{Private: true,
					ScheduledContractClose: ScheduledContractClose{ContractId: ids[index], Deadline: deadline.Add(time.Duration(index) * time.Minute)}})
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		queue := readExpiryRecoveryQueue(t, ctx)
		first := queue[task.RunOnce("close_scheduled_contract", ids[0]).String()].id
		second := queue[task.RunOnce("close_scheduled_contract", ids[1]).String()].id
		args, result := legacyDispatchPublicationFixture()
		result.Dispatch.PayerNetworkIds[0] = closeOwners[0].Id
		for args.Shard == int(ids[0][15])%model.LegacySettlementShardCount || args.Shard == int(ids[1][15])%model.LegacySettlementShardCount {
			args.Shard = (args.Shard + 1) % model.LegacySettlementShardCount
		}
		payerKey := task.RunOnceOwnershipKey(task.RunOnce("flush_legacy_payer_settlements", closeOwners[0].Id))
		ready, release, waiting := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce, waitOnce sync.Once
		ownerDone, childDone := make(chan struct{}), make(chan struct{})
		var ownerErr error
		var completed legacyDispatchEval
		workerCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipWaiting && slices.Contains(event.Keys, payerKey) {
				waitOnce.Do(func() { close(waiting) })
			}
		})
		worker := startupClosureWorker(workerCtx, NewScheduledContractClosureTaskTarget())
		independent := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		startedChild := false
		defer func() {
			releaseOnce.Do(func() { close(release) })
			worker.Close()
			independent.Close()
			cancel()
			// Every started owner joins before TestEnv restores its resources.
			// The test process timeout bounds a broken cleanup; teardown cannot
			// proceed while a goroutine still owns a database connection.
			<-ownerDone
			if startedChild {
				<-childDone
			}
		}()
		go func() {
			defer close(ownerDone)
			server.HandleError(func() {
				withLegacyDispatcherQueueTestTx(ctx, args.Shard, result, func(tx server.PgTx) {
					server.Raise(FlushLegacySettlementsPost(args, result, owner, tx))
					close(ready)
					legacyDispatchAwait(ctx, release)
				})
			}, func(err error) { ownerErr = err })
		}()
		legacyDispatchAwait(ctx, ready)
		startedChild = true
		go func() {
			defer close(childDone)
			server.HandleError(func() {
				completed.finished, completed.retried, completed.posts, completed.err = worker.EvalTasks(1)
			}, func(err error) { completed.err = err })
		}()
		legacyDispatchAwait(ctx, waiting)
		pending := task.GetTasks(ctx, first)[first]
		if pending == nil || pending.ClaimGeneration != 1 || task.GetFinishedTasks(ctx, first)[first] != nil {
			t.Fatal("held publisher did not retain the actual claimed child's unfinished custody")
		}
		finished, retried, posts, err := independent.EvalTasks(1)
		if err != nil || !reflect.DeepEqual(finished, []server.Id{second}) || len(retried)+len(posts) != 0 || task.GetFinishedTasks(ctx, second)[second] == nil {
			t.Fatal("held unrelated publication stopped independent child finalization", finished, retried, posts, err)
		}
		if !reflect.DeepEqual(pending, task.GetTasks(ctx, first)[first]) || task.GetFinishedTasks(ctx, first)[first] != nil {
			t.Fatal("waiting child changed its claim before acquiring its real owner keys")
		}
		releaseOnce.Do(func() { close(release) })
		legacyDispatchAwait(ctx, ownerDone)
		legacyDispatchAwait(ctx, childDone)
		if ownerErr != nil || completed.err != nil || !reflect.DeepEqual(completed.finished, []server.Id{first}) || len(completed.retried)+len(completed.posts) != 0 ||
			task.GetTasks(ctx, first)[first] != nil || task.GetFinishedTasks(ctx, first)[first] == nil {
			t.Fatal("acknowledged publication did not release the exact child for ordinary finalization", ownerErr, completed.err)
		}
		queue = readExpiryRecoveryQueue(t, ctx)
		payer := queue[task.RunOnce("flush_legacy_payer_settlements", closeOwners[0].Id).String()]
		registration, registered := queue[task.RunOnce(fmt.Sprintf("flush_legacy_settlements_%d", int(ids[0][15])%model.LegacySettlementShardCount)).String()]
		if payer.id == (server.Id{}) || payer.generation != 1 || payer.wakeAt == nil || !registered || registration.runAt.After(server.NowUtc()) {
			t.Fatal("child finalization did not preserve the coalesced payer and registration wakes")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var held bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND payer_network_id=$2
				AND EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
				AND EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1 AND NOT settled)
				AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
				AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
				FROM transfer_contract WHERE contract_id=$1`, ids[0], closeOwners[0].Id).Scan(&held))
			if !held {
				t.Fatal("queue handoff bypassed the actual paid contract's financial owner")
			}
		})
	})
}

// This wrapper holds only the interval after the real owner has read EOF and
// before its real finalizer runs. It keeps the original target and declaration.
type legacyDispatchEmptyOwnerTarget struct {
	task.Target
	ready    chan struct{}
	release  chan struct{}
	observed *model.LegacyPayerSettlementResult
	runErr   error
}

func (self *legacyDispatchEmptyOwnerTarget) TaskCompletionOwnershipKeys(queued *task.Task, result string) ([]server.PgOwnershipKey, error) {
	return self.Target.(task.TaskCompletionOwnershipTarget).TaskCompletionOwnershipKeys(queued, result)
}

func (self *legacyDispatchEmptyOwnerTarget) Run(ctx context.Context, queued *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	result, post, err := self.Target.Run(ctx, queued)
	self.observed, _ = result.(*model.LegacyPayerSettlementResult)
	self.runErr = err
	close(self.ready)
	legacyDispatchAwait(ctx, self.release)
	return result, post, err
}

func TestLegacyDispatcherPublicationAfterOwnerEofKeepsFutureSuccessor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		args, result := legacyDispatchPublicationFixture()
		owner := model.ContractCloseOwner{Kind: model.ContractCloseOwnerSourceClient, Id: result.Dispatch.SourceClientIds[0]}
		key := task.RunOnce("flush_legacy_source_settlements", owner.Id)
		server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(key)}, func(tx server.PgTx) {
			model.ScheduleLegacyCloseSettlementsInTx(client, tx, owner, nil, server.NowUtc().Add(-time.Minute))
		}, server.TxReadCommitted, server.OptNoRetry())
		original := readExpiryRecoveryQueue(t, ctx)[key.String()]
		target := &legacyDispatchEmptyOwnerTarget{Target: model.NewLegacySourceSettlementTaskTarget(), ready: make(chan struct{}), release: make(chan struct{})}
		worker := startupClosureWorker(ctx, target)
		done := make(chan struct{})
		var completed legacyDispatchEval
		var releaseOnce sync.Once
		defer func() {
			releaseOnce.Do(func() { close(target.release) })
			worker.Close()
			cancel()
			// Cancellation is a request; the actual return proves retirement.
			<-done
		}()
		go func() {
			defer close(done)
			server.HandleError(func() {
				completed.finished, completed.retried, completed.posts, completed.err = worker.EvalTasks(1)
			}, func(err error) { completed.err = err })
		}()
		legacyDispatchAwait(ctx, target.ready)
		if target.runErr != nil || target.observed == nil || target.observed.Visited != 0 || target.observed.Failed != 0 ||
			target.observed.More || target.observed.Cursor != nil {
			t.Fatal("fixture owner did not return its real empty EOF before publication", target.runErr)
		}
		before := server.NowUtc()
		withLegacyDispatcherQueueTestTx(ctx, args.Shard, result, func(tx server.PgTx) {
			server.Raise(FlushLegacySettlementsPost(args, result, client, tx))
		})
		after := server.NowUtc()
		active := readExpiryRecoveryQueue(t, ctx)[key.String()]
		if active.id != original.id || active.args != original.args || active.claim != 1 || active.generation != 1 || active.wakeAt == nil ||
			active.wakeAt.Before(before.Add(30*time.Second)) || active.wakeAt.After(after.Add(30*time.Second)) {
			t.Fatal("batched wake after real EOF did not retain active-owner generation and future deadline")
		}
		releaseOnce.Do(func() { close(target.release) })
		legacyDispatchAwait(ctx, done)
		if completed.err != nil || !reflect.DeepEqual(completed.finished, []server.Id{original.id}) || len(completed.retried)+len(completed.posts) != 0 {
			t.Fatal("real EOF owner did not finalize after its batched wake", completed.err)
		}
		next, found := readExpiryRecoveryQueue(t, ctx)[key.String()]
		if !found || next.id == original.id || next.args != original.args || !next.runAt.Equal(*active.wakeAt) || next.claim != 0 ||
			task.GetTasks(ctx, original.id)[original.id] != nil || task.GetFinishedTasks(ctx, original.id)[original.id] == nil {
			t.Fatal("batch publication after last empty read lost its exact future successor")
		}
	})
}
