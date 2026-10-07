// Real settlement and admission controls distinguish optional mirror work from
// the availability of reclaimed credit while its durable owner is delayed.
package model

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// The entire starting credit is reserved: legacy600 plus native400. Closing
// legacy usage200 leaves credit800 and must make another native400 possible.
type legacyInlineMirrorFixture struct {
	clients          netEscrowOrderingTestFixture
	contractIds      []server.Id
	reportKVs        map[server.Id][]byte
	proofKVs         map[server.Id][]byte
	nativeId         server.Id
	nativeExpiry     float64
	reservationBytes ByteCount
	usageBytes       ByteCount
}

// Every outcome and post below belongs to ordinary production entry points;
// only initial price metadata and explicit failure/barrier setup are synthetic.
func newLegacyInlineMirrorFixture(t testing.TB, ctx context.Context, count int) *legacyInlineMirrorFixture {
	t.Helper()
	fixture := &legacyInlineMirrorFixture{
		clients:   newNetEscrowOrderingTestFixture(t, ctx),
		reportKVs: map[server.Id][]byte{}, proofKVs: map[server.Id][]byte{},
		reservationBytes: 600 / int64(count), usageBytes: 200 / int64(count),
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, fixture.clients.balanceId))
	})
	for range count {
		escrow, posts := createNetEscrowOrderingTestContract(ctx, fixture.clients, fixture.reservationBytes)
		server.RunPosts(ctx, posts...)
		fixture.contractIds = append(fixture.contractIds, escrow.ContractId)
	}
	native := createRedisAdmissionTest(ctx, fixture.clients, 400)
	fixture.nativeId = native.ContractId
	server.Redis(ctx, func(client server.RedisClient) {
		var err error
		fixture.nativeExpiry, err = client.ZScore(ctx, redisContractReservationKeys(fixture.clients.balanceId)[2], native.ContractId.String()).Result()
		server.Raise(err)
	})
	for _, id := range fixture.contractIds {
		server.Raise(CloseContract(ctx, id, fixture.clients.sourceId, fixture.usageBytes, false))
		server.Raise(CloseContract(ctx, id, fixture.clients.destinationId, fixture.usageBytes, false))
		reports, proof := legacyInlineMirrorEvidence(ctx, id)
		if len(proof) != 0 {
			t.Fatal("queued legacy fixture already had terminal usage")
		}
		fixture.reportKVs[id] = reports
	}
	refreshNetEscrow(ctx, []server.Id{fixture.clients.balanceId})
	fixture.requireState(t, ctx, 0, nil)
	if count := legacyInlineMirrorOwnerCount(ctx, fixture.clients.balanceId); count != 0 {
		t.Fatalf("fixture already has %d financial mirror owners", count)
	}
	if legacyInlineMirrorRawReservation(ctx, fixture.clients.balanceId) != 600 || GetActiveTransferBalanceByteCount(ctx, fixture.clients.sourceNetworkId) != 0 {
		t.Fatal("fixture did not begin with exactly legacy600 plus native400 reserved")
	}
	return fixture
}

// Reports remain byte-identical; terminal usage is decoded and then retained
// byte-for-byte across all owner failures, finalizations and finance replays.
func legacyInlineMirrorEvidence(ctx context.Context, id server.Id) ([]byte, []byte) {
	var reports, proof []byte
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT jsonb_agg(to_jsonb(report) ORDER BY report.party) FROM contract_close report WHERE report.contract_id=$1),
			provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&reports, &proof))
	})
	return reports, proof
}

// Read-only financial and native-reservation custody, independent of whether
// optional publication has run. No oracle repairs money, reports or counters.
func (self *legacyInlineMirrorFixture) requireState(t testing.TB, ctx context.Context, completed int, admitted *TransferEscrow) {
	t.Helper()
	used := int64(completed) * self.usageBytes
	reserved := int64(len(self.contractIds)-completed) * self.reservationBytes
	server.Db(ctx, func(conn server.PgConn) {
		var terminal, settled, pending int
		var credit, legacyReserved, swept, revenue, provider, providerRevenue int64
		server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
			SELECT allocation FROM pending_task
			CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') allocation
			WHERE function_name=$4 AND (args_json::jsonb->>'applied')::boolean=false
				AND (allocation->>'network_id')::uuid=$3
		) SELECT
			(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
			(SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled),
			(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)),
			(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
			(SELECT COALESCE(sum(balance_byte_count),0) FROM transfer_escrow WHERE contract_id=ANY($1) AND NOT settled),
			(SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
			(SELECT COALESCE(sum(payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
			COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$3),0)+COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0),
			COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$3),0)+COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)`,
			self.contractIds, self.clients.balanceId, self.clients.destinationNetworkId,
			task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(
			&terminal, &settled, &pending, &credit, &legacyReserved, &swept, &revenue, &provider, &providerRevenue))
		if terminal != completed || settled != completed || pending != len(self.contractIds)-completed ||
			credit != 1000-used || legacyReserved != reserved || swept != used || revenue != used || provider != used || providerRevenue != used {
			t.Fatalf("financial custody changed: terminal=%d settled=%d pending=%d credit=%d reserved=%d swept=%d revenue=%d provider=%d/%d expected_used=%d",
				terminal, settled, pending, credit, legacyReserved, swept, revenue, provider, providerRevenue, used)
		}
		var nativeExact bool
		server.Raise(conn.QueryRow(ctx, `SELECT contract.outcome IS NULL AND NOT escrow.settled AND escrow.redis_reserved AND escrow.balance_byte_count=400
			FROM transfer_contract contract JOIN transfer_escrow escrow USING(contract_id)
			WHERE contract.contract_id=$1 AND escrow.balance_id=$2`, self.nativeId, self.clients.balanceId).Scan(&nativeExact))
		if !nativeExact {
			t.Fatal("legacy close changed its surviving native neighbor")
		}
		if admitted != nil {
			server.Raise(conn.QueryRow(ctx, `SELECT contract.outcome IS NULL AND contract.transfer_byte_count=400
				AND NOT escrow.settled AND escrow.redis_reserved AND escrow.balance_byte_count=400
				FROM transfer_contract contract JOIN transfer_escrow escrow USING(contract_id)
				WHERE contract.contract_id=$1 AND escrow.balance_id=$2`, admitted.ContractId, self.clients.balanceId).Scan(&nativeExact))
			if !nativeExact {
				t.Fatal("subsequent public admission did not retain exactly400 native bytes")
			}
		}
	})
	for index, id := range self.contractIds {
		reports, proof := legacyInlineMirrorEvidence(ctx, id)
		if !bytes.Equal(reports, self.reportKVs[id]) {
			t.Fatal("mirror ownership changed an original authenticated close report")
		}
		if index >= completed {
			if len(proof) != 0 {
				t.Fatal("unsettled neighbor acquired a fabricated usage proof")
			}
			continue
		}
		usage, err := decodeContractUsageSnapshot(proof)
		if err != nil || usage == nil || usage.ByteCount != self.usageBytes || len(usage.Providers) != 1 ||
			usage.Providers[0].NetworkId != self.clients.destinationNetworkId || usage.Providers[0].ClientId != self.clients.destinationId || usage.Providers[0].ByteCount != self.usageBytes {
			t.Fatal("ordinary settlement did not preserve exact provider proof", err)
		}
		if previous, ok := self.proofKVs[id]; ok && !bytes.Equal(previous, proof) {
			t.Fatal("owner or financial replay changed immutable provider usage")
		}
		self.proofKVs[id] = slices.Clone(proof)
	}
	server.Redis(ctx, func(client server.RedisClient) {
		keys := redisContractReservationKeys(self.clients.balanceId)
		amount, err := client.HGet(ctx, keys[1], self.nativeId.String()).Int64()
		server.Raise(err)
		expiry, err := client.ZScore(ctx, keys[2], self.nativeId.String()).Result()
		server.Raise(err)
		total, err := client.Get(ctx, keys[0]).Int64()
		server.Raise(err)
		wantTotal := int64(400)
		if admitted != nil {
			wantTotal += 400
			newAmount, err := client.HGet(ctx, keys[1], admitted.ContractId.String()).Int64()
			server.Raise(err)
			if newAmount != 400 {
				t.Fatal("subsequent reservation lost its exact token")
			}
		}
		if amount != 400 || expiry != self.nativeExpiry || total != wantTotal {
			t.Fatalf("native reservation custody changed: neighbor=%d expiry=%f total=%d expected=%d", amount, expiry, total, wantTotal)
		}
	})
}

// A public page must perform one actual transition on the retained fixture.
func (self *legacyInlineMirrorFixture) closeNext(t testing.TB, ctx context.Context, index int) {
	t.Helper()
	id := self.contractIds[index]
	page, err := FlushLegacySettlements(ctx, int(id[15])%LegacySettlementShardCount, nil, 1)
	if err != nil || page.Visited != 1 || page.Completed != 1 || page.BusyOrGone != 0 || page.Failed != 0 {
		t.Fatalf("ordinary financial page failed: %+v err=%v", page, err)
	}
	self.requireState(t, ctx, index+1, nil)
	if count := legacyInlineMirrorOwnerCount(ctx, self.clients.balanceId); count != 1 {
		t.Fatalf("committed settlement has %d mirror owners, want one coalesced owner", count)
	}
	cache := settlementCacheSnapshot(ctx, []server.Id{self.clients.balanceId})
	if len(cache) != 1 || cache[self.clients.balanceId].reserved != int64(len(self.contractIds)-index-1)*self.reservationBytes {
		t.Fatal("settlement did not commit its exact current legacy cache")
	}
}

// The raw legacy counter is deliberately distinguished from the native total.
func legacyInlineMirrorRawReservation(ctx context.Context, id server.Id) int64 {
	var value int64
	server.Redis(ctx, func(client server.RedisClient) {
		var err error
		value, err = client.Get(ctx, netEscrowKey(id)).Int64()
		if !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
	})
	return value
}

// Exact bounded fixture inventory, not a production queue census.
func legacyInlineMirrorOwnerCount(ctx context.Context, id server.Id) int {
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name=$1 AND (args_json::jsonb->>'balance_id')::uuid=$2`,
			task.NewTaskTarget(ApplyLegacyNetEscrowMirror).TargetFunctionName(), id).Scan(&count))
	})
	return count
}

// Build the actual task owner and finalizer; only the fixture's due time changes.
func legacyInlineMirrorWorker(t testing.TB, ctx context.Context, id server.Id) (*task.TaskWorker, server.Id) {
	t.Helper()
	owner := legacyMirrorTestOwner(t, ctx, id)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$1,release_time=$1 WHERE task_id=$2`, time.Time{}, owner.TaskId))
	})
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	worker.AddTargets(task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost))
	return worker, owner.TaskId
}

type legacyInlineMirrorWorkerResult struct {
	finished, retried, postRetried []server.Id
	err                            error
}

// Goroutines return failures to the test owner rather than calling FailNow.
func legacyInlineMirrorEvaluate(worker *task.TaskWorker) (result legacyInlineMirrorWorkerResult) {
	server.HandleError(func() {
		result.finished, result.retried, result.postRetried, result.err = worker.EvalTasks(1)
	}, func(err error) { result.err = err })
	return
}

func (self legacyInlineMirrorWorkerResult) requireFinished(t testing.TB, id server.Id) {
	t.Helper()
	if self.err != nil || len(self.finished) != 1 || self.finished[0] != id || len(self.retried) != 0 || len(self.postRetried) != 0 {
		t.Fatalf("real mirror worker did not finalize its exact owner: finished=%v retry=%v post_retry=%v err=%v", self.finished, self.retried, self.postRetried, self.err)
	}
}

// Complete one exact owner synchronously through ordinary finalization.
func legacyInlineMirrorRunWorker(t testing.TB, ctx context.Context, id server.Id) server.Id {
	t.Helper()
	worker, taskId := legacyInlineMirrorWorker(t, ctx, id)
	defer worker.Close()
	legacyInlineMirrorEvaluate(worker).requireFinished(t, taskId)
	return taskId
}

// Replays cannot spend the payer twice or republish another native token.
func (self *legacyInlineMirrorFixture) requireReplay(t testing.TB, ctx context.Context, admitted *TransferEscrow) {
	t.Helper()
	for _, id := range self.contractIds {
		completed, busy, _, err := flushLegacySettlement(ctx, id)
		if err != nil || completed || !busy {
			t.Fatal("terminal settlement replay reacquired financial ownership", err)
		}
	}
	projectLegacyProviderTotalsForTest(t, ctx)
	self.requireState(t, ctx, len(self.contractIds), admitted)
}

type legacyInlineMirrorGateKey struct{}

// One marked publication can be held or lose its reply. The hook observes the
// real fenced Lua on the ordinary client and never manufactures Redis values.
type legacyInlineMirrorGate struct {
	key       string
	entered   chan struct{}
	allow     chan struct{}
	lostReply bool
	hit       atomic.Bool
}

func (self *legacyInlineMirrorGate) DialHook(next redis.DialHook) redis.DialHook { return next }
func (self *legacyInlineMirrorGate) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

// The script is sent through a real cluster-compatible pipeline.
func (self *legacyInlineMirrorGate) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		if ctx.Value(legacyInlineMirrorGateKey{}) != self {
			return next(ctx, commands)
		}
		matched := false
		for _, command := range commands {
			args := command.Args()
			matched = matched || command.Name() == "eval" && len(args) > 3 && args[1] == netEscrowSnapshotScript && args[3] == self.key
		}
		if !matched || self.hit.Swap(true) {
			return next(ctx, commands)
		}
		close(self.entered)
		if self.allow != nil {
			select {
			case <-self.allow:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err := next(ctx, commands)
		if err == nil && self.lostReply {
			return io.ErrUnexpectedEOF
		}
		return err
	}
}

// Register once on the fixture client; unmarked operations remain unaffected.
func legacyInlineMirrorMarked(ctx context.Context, gate *legacyInlineMirrorGate) context.Context {
	server.Redis(ctx, func(client server.RedisClient) { client.AddHook(gate) })
	return context.WithValue(ctx, legacyInlineMirrorGateKey{}, gate)
}

// The baseline finishes all finance but enters the held optional foreground
// publication. The candidate finishes the public page without that command.
func TestLegacyInlineMirrorDoesNotJoinForegroundPublication(t *testing.T) {
	legacyMirrorTestEnv(t, func(t testing.TB, ctx context.Context) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		fixture := newLegacyInlineMirrorFixture(t, ctx, 1)
		gate := &legacyInlineMirrorGate{key: netEscrowKey(fixture.clients.balanceId), entered: make(chan struct{}), allow: make(chan struct{})}
		marked := legacyInlineMirrorMarked(ctx, gate)
		allow := sync.OnceFunc(func() { close(gate.allow) })
		type result struct {
			page LegacySettlementFlushResult
			err  error
		}
		done := make(chan result, 1)
		retired := make(chan struct{})
		go func() {
			defer close(retired)
			var completed result
			server.HandleError(func() {
				completed.page, completed.err = FlushLegacySettlements(marked, int(fixture.contractIds[0][15])%LegacySettlementShardCount, nil, 1)
			}, func(err error) { completed.err = err })
			done <- completed
		}()
		defer func() {
			allow()
			cancel()
			select {
			case <-retired:
			case <-time.After(10 * time.Second):
				t.Error("foreground publication owner failed to join")
			}
		}()
		var observed result
		select {
		case <-gate.entered:
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE NOWAIT`, fixture.clients.balanceId))
			}, server.TxReadCommitted, server.OptNoRetry())
			fixture.requireState(t, ctx, 1, nil)
			allow()
			select {
			case observed = <-done:
			case <-ctx.Done():
				t.Fatal("released foreground post did not return")
			}
		case observed = <-done:
			allow()
		case <-ctx.Done():
			t.Fatal("foreground control reached neither completion nor its command barrier")
		}
		if observed.err != nil || observed.page.Completed != 1 || observed.page.Visited != 1 || observed.page.Failed != 0 {
			t.Fatal("foreground scheduling changed financial completion", observed.err)
		}
		fixture.requireState(t, ctx, 1, nil)
		legacyInlineMirrorRunWorker(t, ctx, fixture.clients.balanceId)
		admitted := createRedisAdmissionTest(ctx, fixture.clients, 400)
		fixture.requireReplay(t, ctx, admitted)
		if legacyInlineMirrorRawReservation(ctx, fixture.clients.balanceId) != 0 || GetActiveTransferBalanceByteCount(ctx, fixture.clients.sourceNetworkId) != 0 {
			t.Fatal("owner publication did not preserve the fully reserved final credit")
		}
		if gate.hit.Load() {
			t.Fatal("financial page joined a duplicate warm legacy mirror publication")
		}
	})
}

// These two controls deliberately retain immediate usable credit as a release
// requirement; eventual repair must not make a new admission regression green.
func TestLegacyInlineMirrorPreservesExistingNetworkAdmission(t *testing.T) {
	testLegacyInlineMirrorImmediateAdmission(t, false)
}

func TestLegacyInlineMirrorPreservesNewDestinationAdmission(t *testing.T) {
	testLegacyInlineMirrorImmediateAdmission(t, true)
}

func testLegacyInlineMirrorImmediateAdmission(t *testing.T, newDestination bool) {
	legacyMirrorTestEnv(t, func(t testing.TB, ctx context.Context) {
		fixture := newLegacyInlineMirrorFixture(t, ctx, 1)
		fixture.closeNext(t, ctx, 0)
		clients := fixture.clients
		if newDestination {
			clients.destinationNetworkId, clients.destinationId = server.NewId(), server.NewId()
			testingCreatePaymentClient(ctx, clients.destinationNetworkId, clients.destinationId)
		}
		// No worker runs: this is exact starvation without timing assumptions.
		beforeAvailable := GetActiveTransferBalanceByteCount(ctx, clients.sourceNetworkId)
		admitted, beforeErr := CreateTransferEscrow(ctx, clients.sourceNetworkId, clients.sourceId, clients.destinationNetworkId, clients.destinationId, 400)
		beforeSucceeded := beforeErr == nil && admitted != nil && admitted.TransferByteCount == 400
		if beforeErr != nil && admitted != nil {
			t.Fatal("failed public request retained a published contract", beforeErr)
		}
		if beforeErr != nil && !errors.Is(beforeErr, errRedisReservationInsufficient) {
			t.Fatal("admission control failed outside the retained-credit predicate", beforeErr)
		}
		fixture.requireState(t, ctx, 1, admitted)
		// A new payer with a different existing grant has no legacy projection
		// debt; it must still admit while this exact owner's queue is starved.
		unrelated := newNetEscrowOrderingTestFixture(t, ctx)
		fresh := createRedisAdmissionTest(ctx, unrelated, 400)
		if fresh.TransferByteCount != 400 || GetActiveTransferBalanceByteCount(ctx, unrelated.sourceNetworkId) != 600 {
			t.Fatal("starved balance blocked an independent new network")
		}
		legacyInlineMirrorRunWorker(t, ctx, fixture.clients.balanceId)
		if admitted == nil {
			admitted = createRedisAdmissionTest(ctx, clients, 400)
		}
		fixture.requireReplay(t, ctx, admitted)
		if legacyInlineMirrorOwnerCount(ctx, fixture.clients.balanceId) != 0 || legacyInlineMirrorRawReservation(ctx, fixture.clients.balanceId) != 0 ||
			GetActiveTransferBalanceByteCount(ctx, fixture.clients.sourceNetworkId) != 0 {
			t.Fatal("completed durable owner did not restore exact released capacity")
		}
		if !beforeSucceeded || beforeAvailable != 400 {
			t.Fatalf("reclaimed credit waited for a starved mirror owner: available=%d admitted=%t err=%v", beforeAvailable, beforeSucceeded, beforeErr)
		}
	})
}

// A held actual warm owner owns no financial lock. A second close coalesces
// into it; its stale acknowledgement leaves a successor that restores credit.
func TestLegacyInlineMirrorHeldOwnerCoalescesAndRestoresAdmission(t *testing.T) {
	legacyMirrorTestEnv(t, func(t testing.TB, ctx context.Context) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		fixture := newLegacyInlineMirrorFixture(t, ctx, 2)
		fixture.closeNext(t, ctx, 0)
		first := legacyMirrorTestOwner(t, ctx, fixture.clients.balanceId)
		gate := &legacyInlineMirrorGate{key: netEscrowKey(fixture.clients.balanceId), entered: make(chan struct{}), allow: make(chan struct{})}
		marked := legacyInlineMirrorMarked(ctx, gate)
		allow := sync.OnceFunc(func() { close(gate.allow) })
		worker, taskId := legacyInlineMirrorWorker(t, marked, fixture.clients.balanceId)
		defer worker.Close()
		done := make(chan legacyInlineMirrorWorkerResult, 1)
		retired := make(chan struct{})
		go func() {
			defer close(retired)
			done <- legacyInlineMirrorEvaluate(worker)
		}()
		defer func() {
			allow()
			cancel()
			select {
			case <-retired:
			case <-time.After(10 * time.Second):
				t.Error("held durable mirror failed to join")
			}
		}()
		select {
		case <-gate.entered:
		case <-retired:
			t.Fatal("durable mirror never entered its real publication")
		case <-ctx.Done():
			t.Fatal("durable mirror barrier not reached")
		}
		fixture.closeNext(t, ctx, 1)
		if legacyMirrorTestOwner(t, ctx, fixture.clients.balanceId).TaskId != first.TaskId {
			t.Fatal("concurrent close did not retain the active coalesced owner")
		}
		allow()
		var finished legacyInlineMirrorWorkerResult
		select {
		case finished = <-done:
		case <-retired:
			select {
			case finished = <-done:
			default:
				t.Fatal("durable owner exited before finalization")
			}
		case <-ctx.Done():
			t.Fatal("released durable owner did not finish")
		}
		finished.requireFinished(t, taskId)
		if next := legacyMirrorTestOwner(t, ctx, fixture.clients.balanceId); next.TaskId == first.TaskId {
			t.Fatal("older publication lost the successor for the new financial revision")
		}
		legacyInlineMirrorRunWorker(t, ctx, fixture.clients.balanceId)
		admitted := createRedisAdmissionTest(ctx, fixture.clients, 400)
		fixture.requireReplay(t, ctx, admitted)
		if legacyInlineMirrorOwnerCount(ctx, fixture.clients.balanceId) != 0 || legacyInlineMirrorRawReservation(ctx, fixture.clients.balanceId) != 0 {
			t.Fatal("successor did not finish exact released-capacity publication")
		}
	})
}

// Losing the reply retains the durable obligation even when Redis applied the
// absolute value. Repeated real publication and finalization cannot double pay.
func TestLegacyInlineMirrorOwnerErrorReplayPreservesAdmission(t *testing.T) {
	legacyMirrorTestEnv(t, func(t testing.TB, ctx context.Context) {
		fixture := newLegacyInlineMirrorFixture(t, ctx, 1)
		fixture.closeNext(t, ctx, 0)
		owner := legacyMirrorTestOwner(t, ctx, fixture.clients.balanceId)
		gate := &legacyInlineMirrorGate{key: netEscrowKey(fixture.clients.balanceId), entered: make(chan struct{}), lostReply: true}
		marked := legacyInlineMirrorMarked(ctx, gate)
		target := task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost)
		result, post, err := target.RunSpecific(marked, owner)
		if !gate.hit.Load() || !errors.Is(err, io.ErrUnexpectedEOF) || result != nil || post != nil || legacyInlineMirrorOwnerCount(ctx, fixture.clients.balanceId) != 1 {
			t.Fatal("lost acknowledgement falsely discharged durable mirror ownership", err)
		}
		admitted := createRedisAdmissionTest(ctx, fixture.clients, 400)
		fixture.requireState(t, ctx, 1, admitted)
		legacyInlineMirrorRunWorker(t, ctx, fixture.clients.balanceId)
		fixture.requireReplay(t, ctx, admitted)
		if legacyInlineMirrorRawReservation(ctx, fixture.clients.balanceId) != 0 || GetActiveTransferBalanceByteCount(ctx, fixture.clients.sourceNetworkId) != 0 {
			t.Fatal("mirror replay changed exact final reservation availability")
		}
	})
}

// Explicit owner deletion is a fixture fault, not a supported normal state.
// The missing-row error and no-op financial replay cannot fabricate a repair;
// only the existing independent reconciler restores the abandoned projection.
func TestLegacyInlineMirrorMissingOwnerRequiresIndependentRepair(t *testing.T) {
	legacyMirrorTestEnv(t, func(t testing.TB, ctx context.Context) {
		fixture := newLegacyInlineMirrorFixture(t, ctx, 1)
		fixture.closeNext(t, ctx, 0)
		owner := legacyMirrorTestOwner(t, ctx, fixture.clients.balanceId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE task_id=$1`, owner.TaskId))
		})
		result, post, err := task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost).RunSpecific(ctx, owner)
		if err == nil || result != nil || post != nil {
			t.Fatal("missing durable row admitted an invented mirror owner")
		}
		before := legacyInlineMirrorRawReservation(ctx, fixture.clients.balanceId)
		fixture.requireReplay(t, ctx, nil)
		if legacyInlineMirrorOwnerCount(ctx, fixture.clients.balanceId) != 0 || legacyInlineMirrorRawReservation(ctx, fixture.clients.balanceId) != before {
			t.Fatal("terminal finance replay silently manufactured optional repair work")
		}
		ReconcileNetEscrowForNetwork(ctx, fixture.clients.sourceNetworkId, true)
		admitted := createRedisAdmissionTest(ctx, fixture.clients, 400)
		fixture.requireReplay(t, ctx, admitted)
		if legacyInlineMirrorRawReservation(ctx, fixture.clients.balanceId) != 0 || GetActiveTransferBalanceByteCount(ctx, fixture.clients.sourceNetworkId) != 0 {
			t.Fatal("independent repair failed to restore exact released capacity")
		}
	})
}
