// Parallel public closes retain their real foreground report/outcome path and
// continue through actual background accounting and complete owner handback.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

const parallelPublicCloseCount = 2048
const parallelPublicCloseJitterSeed uint64 = 0x5eed1a2

// A fixed integer mixer produces the same bounded jitter on every platform.
// The common start gate, actual held owner and positive progress are the
// ordering proof; these short timers only spread the burst slightly.
func parallelPublicCloseJitter(index int) time.Duration {
	value := uint64(index+1)*1664525 + parallelPublicCloseJitterSeed
	return time.Duration(value%21) * time.Millisecond
}

// The source owns original reservations and reports independently of each
// financial implementation. Every identity is synthetic and fixture-local.
type parallelPublicCloseFixture struct {
	finance   legacyFinancialCohortFixture
	sources   []server.Id
	native    []bool
	payerSlot []int
	payers    []netEscrowOrderingTestFixture
	reports   []byte
}

// Only preexisting source reports are compared before and after peer closes.
func parallelPublicCloseSourceReports(ctx context.Context, ids []server.Id) []byte {
	var reports []byte
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(contract_id,
          used_transfer_byte_count,checkpoint,close_time) ORDER BY contract_id)
          FROM contract_close WHERE contract_id=ANY($1) AND party='source'`, ids).Scan(&reports))
	})
	return reports
}

// Setup uses public Redis admission and the existing real legacy constructor,
// then ordinary source final reports. Only the peer final CloseContract calls
// remain. An extra native anchor supplies the positively held debit owner.
func parallelPublicCloseSeed(t testing.TB, ctx context.Context, counts []int) parallelPublicCloseFixture {
	t.Helper()
	var fixture parallelPublicCloseFixture
	if len(counts) == 0 || len(counts) > 64 {
		t.Fatal("public-close payer count exceeds the supported fixture bound")
	}
	for _, count := range counts {
		if count < 32 || 1000000 < int64(count+1)*3*10 {
			t.Fatal("public-close fixture lacks its ten-times expected-use funding margin")
		}
	}
	shared := newNetEscrowOrderingTestFixture(t, ctx)
	fixture.finance.providers = append(fixture.finance.providers, shared)
	for payerSlot, count := range counts {
		payer := newNetEscrowOrderingTestFixture(t, ctx)
		balance := server.NewId()
		balance[15] = byte(payerSlot)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_id=$1,
              start_balance_byte_count=1000000,balance_byte_count=1000000,net_revenue_nano_cents=2000000
              WHERE balance_id=$2`, balance, payer.balanceId))
		}, server.TxReadCommitted, server.OptNoRetry())
		payer.balanceId = balance
		fixture.payers = append(fixture.payers, payer)
		fixture.finance.balances = append(fixture.finance.balances, balance)
		fixture.finance.providers = append(fixture.finance.providers, payer)
		if payerSlot == 0 {
			fixture.finance.payer = payer
		}
		for index := range count {
			providerIndex := 0
			if index%4 == 3 {
				providerIndex = payerSlot + 1
			}
			parallelPublicCloseAdd(t, ctx, &fixture, payerSlot, providerIndex, index%2 == 0)
		}
	}
	parallelPublicCloseAdd(t, ctx, &fixture, 0, 0, true)
	fixture.reports = parallelPublicCloseSourceReports(ctx, fixture.finance.ids)
	return fixture
}

// Each contract has its own client pair while its payer grant and provider
// account deliberately overlap the other concurrent closes.
func parallelPublicCloseAdd(t testing.TB, ctx context.Context, fixture *parallelPublicCloseFixture,
	payerSlot, providerIndex int, native bool) {
	t.Helper()
	payer := fixture.payers[payerSlot]
	provider := fixture.finance.providers[providerIndex]
	payer.sourceId = server.NewId()
	payer.destinationId, payer.destinationNetworkId = provider.destinationId, provider.destinationNetworkId
	Testing_CreateDevice(ctx, payer.sourceNetworkId, server.NewId(), payer.sourceId, "parallel-close-fixture", "synthetic")
	var escrow *TransferEscrow
	if native {
		var err error
		escrow, err = CreateTransferEscrow(ctx, payer.sourceNetworkId, payer.sourceId, payer.destinationNetworkId, payer.destinationId, 16)
		server.Raise(err)
	} else {
		var posts []func() any
		// Use the same real legacy constructor with an explicit non-retrying
		// outer owner. Shared financial admission must never acquire its keys
		// inside a transaction whose callback may be retried implicitly.
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			escrow, posts, err = createTransferEscrowInTx(ctx, tx,
				payer.sourceNetworkId, payer.sourceId, payer.destinationNetworkId, payer.destinationId,
				payer.sourceNetworkId, 16, nil)
			server.Raise(err)
		}, server.TxReadCommitted, server.OptNoRetry())
		server.RunPosts(ctx, posts...)
	}
	server.Raise(CloseContract(ctx, escrow.ContractId, payer.sourceId, 3, false))
	var exact bool
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(balance_id=$2 AND balance_byte_count=16
          AND redis_reserved=$3 AND NOT settled) FROM transfer_escrow WHERE contract_id=$1`,
			escrow.ContractId, payer.balanceId, native).Scan(&exact))
	})
	if !exact {
		t.Fatal("parallel-close setup changed the actual reservation domain or amount")
	}
	fixture.finance.ids = append(fixture.finance.ids, escrow.ContractId)
	fixture.finance.balanceIds = append(fixture.finance.balanceIds, payer.balanceId)
	fixture.finance.reservationAmounts = append(fixture.finance.reservationAmounts, 16)
	fixture.finance.providerIndexes = append(fixture.finance.providerIndexes, providerIndex)
	fixture.sources = append(fixture.sources, payer.sourceId)
	fixture.native = append(fixture.native, native)
	fixture.payerSlot = append(fixture.payerSlot, payerSlot)
}

// One statement observes the complete durable pipeline frontier.
type parallelPublicCloseState struct {
	Reports      int
	Settled      int
	Intents      int
	Journals     int
	Unsettled    int
	Pending      int
	ProviderDone int
	Unfinished   int
	Independent  int
}

// Recurring dispatch/debit rows may remain, but no financial, output or post
// owner may remain after the final joined handback.
func parallelPublicCloseSnapshot(ctx context.Context, observer server.PgConn, fixture parallelPublicCloseFixture, recurring []string) parallelPublicCloseState {
	var state parallelPublicCloseState
	independent := make([]server.Id, 0)
	for index, id := range fixture.finance.ids {
		if fixture.payerSlot[index] != 0 {
			independent = append(independent, id)
		}
	}
	providerName := task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()
	func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
          (SELECT count(*) FROM contract_close WHERE contract_id=ANY($1)),
          (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
          (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)),
          (SELECT count(*) FROM transfer_debit_journal WHERE contract_id=ANY($1)),
          (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND NOT settled),
          (SELECT count(*) FROM pending_task WHERE NOT function_name=ANY($2)),
          (SELECT count(*) FROM finished_task WHERE function_name=$3),
          (SELECT count(*) FROM finished_task WHERE NOT post_completed),
          (SELECT count(*) FROM transfer_contract c WHERE c.contract_id=ANY($4) AND c.outcome='settled'
            AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal j WHERE j.contract_id=c.contract_id)
            AND NOT EXISTS(SELECT 1 FROM transfer_escrow e WHERE e.contract_id=c.contract_id AND NOT e.settled)
            AND EXISTS(SELECT 1 FROM finished_task f WHERE f.function_name=$3 AND f.post_completed
              AND (f.args_json::jsonb->>'contract_id')::uuid=c.contract_id))`,
			fixture.finance.ids, recurring, providerName, independent).Scan(&state.Reports, &state.Settled, &state.Intents,
			&state.Journals, &state.Unsettled, &state.Pending, &state.ProviderDone, &state.Unfinished, &state.Independent))
	}(observer)
	return state
}

// Preserve per-contract allocation identity, actual owner execution, applied
// marker and completed post, rather than relying only on account totals.
func parallelPublicCloseRequireOutputs(t testing.TB, ctx context.Context, fixture parallelPublicCloseFixture) {
	t.Helper()
	providerName := task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()
	expected := map[server.Id]server.Id{}
	for index, id := range fixture.finance.ids {
		expected[id] = fixture.finance.providers[fixture.finance.providerIndexes[index]].destinationNetworkId
	}
	seen := map[server.Id]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT args_json,post_completed,run_start_time,run_end_time FROM finished_task
          WHERE function_name=$1 ORDER BY task_id LIMIT $2`, providerName, len(expected)+1)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var raw string
				var post bool
				var start, end *time.Time
				server.Raise(rows.Scan(&raw, &post, &start, &end))
				payload, err := decodeLegacyProviderTotals(raw)
				if err != nil || !post || start == nil || end == nil || end.Before(*start) || !payload.Applied || seen[payload.ContractId] ||
					len(payload.Totals) != 1 || expected[payload.ContractId] != payload.Totals[0].NetworkId ||
					payload.Totals[0].Bytes != 3 || payload.Totals[0].Revenue != 3 {
					t.Fatal("parallel-close provider owner changed exact custody", err)
				}
				seen[payload.ContractId] = true
			}
		})
	})
	if len(seen) != len(expected) {
		t.Fatal("parallel-close did not finalize every exact provider owner", len(seen), len(expected))
	}
}

// Both report directions must match the actual public calls before the
// replay snapshot can become an expected value. A changed peer report cannot
// validate itself merely by being present after CloseContract returned.
func parallelPublicCloseRequireFinalReports(t testing.TB, ctx context.Context, ids []server.Id) {
	t.Helper()
	var exact bool
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*)=$2 AND bool_and(
    report_count=2 AND sources=1 AND destinations=1 AND correct_values=2)
   FROM (SELECT id,count(c.contract_id) AS report_count,
    count(*) FILTER(WHERE c.party='source') AS sources,
    count(*) FILTER(WHERE c.party='destination') AS destinations,
    count(*) FILTER(WHERE c.used_transfer_byte_count=3 AND NOT c.checkpoint AND c.close_time IS NOT NULL) AS correct_values
    FROM unnest($1::uuid[]) AS fixture(id) LEFT JOIN contract_close c ON c.contract_id=fixture.id
    GROUP BY id) AS reports`, ids, len(ids)).Scan(&exact))
	})
	if !exact {
		t.Fatal("public final report values or exact party ownership differ from the supplied calls")
	}
}

// These are peer closes after a native owner has already acquired this grant.
// Their immutable outcomes/journals can be durable while that owner retains
// sole custody of escrow metadata and Redis reservation release.
type parallelPublicCloseHeldNativeState struct {
	Contracts                 int
	Outcomes                  int
	Journals                  int
	MetadataSettled           int
	OriginalReservationRows   int
	OriginalRedisReservations int
}

func parallelPublicCloseAcknowledgedHotNative(fixture parallelPublicCloseFixture, acknowledged []bool) []server.Id {
	ids := []server.Id{}
	for index, id := range fixture.finance.ids[:len(fixture.finance.ids)-1] {
		if acknowledged[index] && fixture.native[index] && fixture.payerSlot[index] == 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func parallelPublicCloseHeldNativeMetadataObserved(ctx context.Context, observer server.PgConn, fixture parallelPublicCloseFixture, acknowledged []bool) bool {
	ids := parallelPublicCloseAcknowledgedHotNative(fixture, acknowledged)
	var settled bool
	server.Raise(observer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_escrow
      WHERE contract_id=ANY($1) AND balance_id=$2 AND settled)`, ids, fixture.payers[0].balanceId).Scan(&settled))
	return settled
}

func parallelPublicCloseHeldNativeSnapshot(ctx context.Context, observer server.PgConn, fixture parallelPublicCloseFixture, acknowledged []bool) parallelPublicCloseHeldNativeState {
	ids := parallelPublicCloseAcknowledgedHotNative(fixture, acknowledged)
	state := parallelPublicCloseHeldNativeState{Contracts: len(ids)}
	func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
   (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
   (SELECT count(*) FROM transfer_debit_journal WHERE contract_id=ANY($1) AND balance_id=$2 AND NOT applied),
   (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND balance_id=$2 AND settled),
   (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND balance_id=$2 AND balance_byte_count=16 AND redis_reserved)`,
			ids, fixture.payers[0].balanceId).Scan(&state.Outcomes, &state.Journals, &state.MetadataSettled, &state.OriginalReservationRows))
	}(observer)
	server.Redis(ctx, func(client server.RedisClient) {
		tokens, err := client.HGetAll(ctx, redisContractReservationKeys(fixture.payers[0].balanceId)[1]).Result()
		server.Raise(err)
		for _, id := range ids {
			if tokens[id.String()] == strconv.FormatInt(16, 10) {
				state.OriginalRedisReservations++
			}
		}
	})
	return state
}

// Use the already-reserved direct observer so2048 public callers cannot
// queue the ownership witness behind their ordinary pool demand. The exact
// state/query predicate is identical to the qualified holder component.
func parallelPublicCloseRequireHeldOwner(t testing.TB, ctx context.Context, observer server.PgConn, pid int32) {
	t.Helper()
	var state, query string
	server.Raise(observer.QueryRow(ctx, `SELECT state,query FROM pg_stat_activity WHERE pid=$1 AND datname=current_database()`, pid).Scan(&state, &query))
	expected := "SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR NO KEY UPDATE SKIP LOCKED"
	if state != "idle in transaction" || strings.Join(strings.Fields(query), " ") != expected {
		t.Fatal("native debit barrier lost exact granted owner", state)
	}
}

// External tests supply the actual registered work targets and scheduling.
// Neither a fake financial callback nor an inline output helper makes progress.
func TestingParallelPublicCloseSharedPayers(t *testing.T, counts []int, shard, debit task.Target,
	scheduleShard, scheduleDebit func(*session.ClientSession, server.PgTx), nativeOwnerResource string) {
	if os.Getenv("URN_PARALLEL_PUBLIC_CLOSE_NATIVE") != "1" {
		t.Skip("explicit isolated parallel public-close correctness control required")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
		defer cancel()
		// Use the production fixed-window policy through its explicit test
		// context: five seconds instead of thirty. The same context reaches
		// every actual scheduler and the Run worker; elapsed time includes it.
		ctx = Testing_WithLegacyPayerSettlementCollectionWindow(ctx)
		diagnostics := newParallelPublicCloseDiagnostics(t)
		diagnostics.event("fixture_seed_started", -1)
		fixture := parallelPublicCloseSeed(t, ctx, counts)
		diagnostics.event("fixture_seed_completed", -1)
		if len(fixture.finance.ids) != parallelPublicCloseCount+1 {
			t.Fatal("parallel-close requires2048 simultaneous peers plus one actual native anchor")
		}
		retries := &parallelCloseRetryObservation{}
		ctx = retries.context(ctx)
		ownership := newParallelCloseCommonOwnership(fixture)
		ctx = server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			ownership.observe(event)
			if event.Kind == server.PgOwnershipAdmitted {
				for _, key := range event.Keys {
					if key == ownership.hot && diagnostics.hotOwnerAdmitted.CompareAndSwap(false, true) {
						diagnostics.event("first_hot_grant_owner_admitted", -1)
					}
				}
			}
		})
		barrier, closeBarrier := newNativeDebitOwnerBarrierOnResource(t, ctx, fixture.payers[0].balanceId, nativeOwnerResource, func() {
			ownership.backendEnd()
			diagnostics.backendEnd()
		})
		defer closeBarrier()
		// The outer two-route observer records every error, including owner
		// rollback or timeout. Only the explicitly selected inner route pauses.
		protocol, closeProtocol := legacyFinancialRunProtocolBind(t, ctx)
		defer closeProtocol()
		observer := acquireContractLifecycleTestConnection(t, ctx)
		var observerOnce sync.Once
		releaseObserver := func() { observerOnce.Do(observer.Release) }
		defer releaseObserver()
		beforeCounters := parallelCloseCounterSnapshot(t)
		beforeClosed := contractClosedCounter.Snapshot()
		protocol.enabled(true)
		// Keep the native owner prelude, every configured coalescing delay and
		// the final worker handback in the complete fixture interval. Public
		// call latency is recorded independently below, starting at invocation.
		pipelineStarted := time.Now()
		events := map[string]int64{}
		recordEvent := func(name string) {
			events[name] = time.Since(pipelineStarted).Nanoseconds()
			diagnostics.event(name, events[name])
		}
		recordEvent("pipeline_started")
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(shard, debit, NewLegacyProviderTotalsTaskTarget(), NewLegacyNetEscrowMirrorTaskTarget())
		worker.AddTargets(legacyPayerPipelineAdditionalTargets()...)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		anchor := len(fixture.finance.ids) - 1
		recordEvent("anchor_close_invoked")
		server.Raise(CloseContract(ctx, fixture.finance.ids[anchor], fixture.finance.providers[fixture.finance.providerIndexes[anchor]].destinationId, 3, false))
		recordEvent("anchor_close_return_observed")
		recordEvent("native_debit_schedule_invoked")
		server.Tx(ctx, func(tx server.PgTx) { scheduleDebit(owner, tx) }, server.TxReadCommitted, server.OptNoRetry())
		recordEvent("native_debit_schedule_committed")
		done := make(chan struct{})
		var runErr error
		go func() {
			defer close(done)
			diagnostics.event("actual_worker_run_entered", -1)
			server.HandleError(worker.Run, func(err error) { runErr = err })
			diagnostics.event("actual_worker_run_returned", -1)
		}()
		var callers sync.WaitGroup
		var callersReady sync.WaitGroup
		start := make(chan struct{})
		var startOnce sync.Once
		startCallers := func() { startOnce.Do(func() { close(start) }) }
		stopped := false
		controlComplete := false
		roles := []string{"legacy_dispatch", "native_debit", "payer", "provider", "mirror", "run_post"}
		functions := []string{shard.TargetFunctionName(), debit.TargetFunctionName(),
			NewLegacyPayerSettlementTaskTarget().TargetFunctionName(),
			NewLegacyProviderTotalsTaskTarget().TargetFunctionName(), NewLegacyNetEscrowMirrorTaskTarget().TargetFunctionName(),
			task.NewTaskTarget(worker.RunPost).TargetFunctionName()}
		observeCurrent := func(name string) {
			diagnostics.log(name, map[string]any{"holder_commits": barrier.commits.Load(),
				"holder_rollbacks": barrier.rollbacks.Load(), "holder_lost": barrier.lost.Load(),
				"holder_release_requested": barrier.released.Load(), "wire": protocol.snapshot(),
				"ownership": ownership.snapshot(), "actual_tx_reruns": retries.callbacks.Load(),
				"counters": diagnostics.counters(beforeCounters), "financial_custody": contractClosedCounter.Snapshot(),
				"test_parent_context_done":  ctx.Err() != nil,
				"native_page_budget_ns":     (15 * time.Second).Nanoseconds(),
				"native_page_context_cause": "not exposed by the unchanged target API; terminal observation does not identify its initiator"})
		}
		defer func() {
			if !controlComplete {
				observeCurrent("before_failure_cleanup")
				diagnostics.taskSnapshot(ctx, observer, roles, functions)
			}
			barrier.Release()
			if !stopped {
				cancel()
				worker.Close()
			}
			startCallers()
			joined := make(chan struct{})
			go func() { callers.Wait(); <-done; close(joined) }()
			select {
			case <-joined:
			case <-time.After(10 * time.Second):
				// Closing the owned proxies unblocks socket I/O before the
				// final finite join. A failed join is never accepted as success.
				releaseObserver()
				closeProtocol()
				closeBarrier()
				select {
				case <-joined:
				case <-time.After(10 * time.Second):
					t.Error("parallel-close callers or worker failed bounded cleanup join")
				}
			}
		}()
		recordEvent("waiting_for_native_owner")
		var pid int32
		select {
		case pid = <-barrier.held:
		case <-done:
			t.Fatal("actual worker retired before native owner barrier", runErr)
		case <-ctx.Done():
			t.Fatal("actual native owner never acquired its grant", ctx.Err())
		}
		recordEvent("native_owner_ready_t_barrier_observed")
		func() {
			defer diagnostics.query("first_owner_witness")()
			parallelPublicCloseRequireHeldOwner(t, ctx, observer, pid)
		}()
		firstCommonKeyHeld := parallelPublicCloseCommonKeyHeld(ctx, observer, pid, "transfer_balance", fixture.payers[0].balanceId)
		ownership.beginHeldWindow(uint32(pid))
		recordEvent("native_debit_owner_held_witness")
		// The real recovery owner is scheduled before the public burst. Its
		// default delays and all actual work remain unchanged.
		recordEvent("legacy_dispatch_schedule_invoked_while_held")
		server.Tx(ctx, func(tx server.PgTx) { scheduleShard(owner, tx) }, server.TxReadCommitted, server.OptNoRetry())
		recordEvent("legacy_dispatch_schedule_committed")
		type closeReply struct {
			index int
			err   error
		}
		replies := make(chan closeReply, parallelPublicCloseCount)
		invocationNs := make([]int64, parallelPublicCloseCount)
		returnNs := make([]int64, parallelPublicCloseCount)
		jitterNs := make([]int64, parallelPublicCloseCount)
		var invoked sync.WaitGroup
		var burstStart time.Time
		for index := range parallelPublicCloseCount {
			callers.Add(1)
			callersReady.Add(1)
			invoked.Add(1)
			go func() {
				defer callers.Done()
				callersReady.Done()
				<-start
				jitter := parallelPublicCloseJitter(index)
				jitterNs[index] = jitter.Nanoseconds()
				if jitter > 0 {
					select {
					case <-time.After(jitter):
					case <-ctx.Done():
						invoked.Done()
						replies <- closeReply{index: index, err: ctx.Err()}
						return
					}
				}
				invocationNs[index] = time.Since(burstStart).Nanoseconds()
				if diagnostics.invocations.Add(1) == parallelPublicCloseCount {
					diagnostics.event("all_public_call_functions_entered", -1)
				}
				invoked.Done()
				var err error
				server.HandleError(func() {
					server.Raise(CloseContract(ctx, fixture.finance.ids[index], fixture.finance.providers[fixture.finance.providerIndexes[index]].destinationId, 3, false))
				}, func(caught error) { err = caught })
				returnNs[index] = time.Since(burstStart).Nanoseconds()
				if diagnostics.returns.Add(1) == parallelPublicCloseCount {
					diagnostics.event("all_public_call_functions_returned", -1)
				}
				replies <- closeReply{index: index, err: err}
			}()
		}
		ready := make(chan struct{})
		go func() { callersReady.Wait(); close(ready) }()
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("parallel callers did not all reach the common rendezvous", ctx.Err())
		}
		recordEvent("all_public_callers_ready")
		burstStart = time.Now()
		events["public_burst_start"] = burstStart.Sub(pipelineStarted).Nanoseconds()
		diagnostics.event("public_burst_start", events["public_burst_start"])
		startCallers()
		allInvoked := make(chan struct{})
		go func() { invoked.Wait(); close(allInvoked) }()
		acknowledged := make([]bool, parallelPublicCloseCount)
		ackCount, hotNativeAck := 0, 0
		acceptReply := func(reply closeReply) {
			if reply.index < 0 || reply.index >= parallelPublicCloseCount || acknowledged[reply.index] || reply.err != nil {
				t.Fatal("public peer-final close failed or repeated its acknowledgement", reply.index, reply.err)
			}
			acknowledged[reply.index] = true
			ackCount++
			diagnostics.acknowledge(fixture.native[reply.index] && fixture.payerSlot[reply.index] == 0)
			if fixture.native[reply.index] && fixture.payerSlot[reply.index] == 0 {
				hotNativeAck++
			}
		}
		recurring := []string{shard.TargetFunctionName(), debit.TargetFunctionName()}
		var heldState parallelPublicCloseState
		invokedAll := false
		progressReady := false
		legacyMetadataWhileHeld := false
		progressPoll := time.After(10 * time.Millisecond)
		for !progressReady {
			select {
			case reply := <-replies:
				acceptReply(reply)
			case <-allInvoked:
				invokedAll = true
				allInvoked = nil
				recordEvent("all_public_invocations_observed")
			case <-ctx.Done():
				t.Fatal("public burst and independent payer did not progress while the real owner remained held", heldState, ctx.Err())
			case <-done:
				t.Fatal("worker retired before held-owner progress proof", runErr)
			case <-progressPoll:
				if invokedAll && hotNativeAck > 0 {
					heldState = parallelPublicCloseObserveHeldFrontier(heldState, func() parallelPublicCloseState {
						defer diagnostics.query("held_frontier_snapshot")()
						return parallelPublicCloseSnapshot(ctx, observer, fixture, recurring)
					})
					if heldState.Independent > 0 && diagnostics.independentObserved.CompareAndSwap(false, true) {
						recordEvent("first_independent_full_output_snapshot")
					}
					if diagnostics.backendEndObserved.Load() && !diagnostics.taskSnapshotCaptured {
						observeCurrent("first_safe_observer_point_after_native_backend_end")
						diagnostics.taskSnapshot(ctx, observer, roles, functions)
					}
					// The candidate waits for an actual common-key contender.
					// An explicitly pre-custody RED can instead positively expose
					// the forbidden foreground metadata write, then finish the
					// same accounting/replay oracles before its final verdict.
					// This alternate does not qualify custody-fixed/no-owner code.
					if !ownership.observedContender() && !legacyMetadataWhileHeld {
						func() {
							defer diagnostics.query("held_native_metadata_probe")()
							legacyMetadataWhileHeld = parallelPublicCloseHeldNativeMetadataObserved(ctx, observer, fixture, acknowledged)
						}()
					}
					progressReady = heldState.Independent > 0 && (ownership.observedContender() || legacyMetadataWhileHeld)
				}
				progressPoll = time.After(10 * time.Millisecond)
			}
		}
		recordEvent("independent_completed_output_observed_while_held")
		var heldNative parallelPublicCloseHeldNativeState
		func() {
			defer diagnostics.query("held_native_snapshot")()
			heldNative = parallelPublicCloseHeldNativeSnapshot(ctx, observer, fixture, acknowledged)
		}()
		func() {
			defer diagnostics.query("second_owner_witness")()
			parallelPublicCloseRequireHeldOwner(t, ctx, observer, pid)
		}()
		secondCommonKeyHeld := parallelPublicCloseCommonKeyHeld(ctx, observer, pid, "transfer_balance", fixture.payers[0].balanceId)
		closedWhileHeld := ownership.closeHeldWindow()
		recordEvent("holder_release_requested_after_second_witness")
		barrier.Release()
		heldAcknowledgements := ackCount
		for ackCount < parallelPublicCloseCount {
			select {
			case reply := <-replies:
				acceptReply(reply)
			case <-ctx.Done():
				t.Fatal("public peer-final close did not return within its unchanged parent budget", ctx.Err())
			case <-done:
				t.Fatal("worker retired while public peer calls remained", runErr)
			}
		}
		callers.Wait()
		recordEvent("all_public_acknowledgements_joined")
		parallelPublicCloseRequireFinalReports(t, ctx, fixture.finance.ids)
		fixture.finance.reports = legacyFinancialCohortReports(ctx, fixture.finance.ids)
		// These exact intervals distinguish an actual concurrent burst from a
		// sequential caller loop. Their wall span is measured, not guessed from
		// the configured jitter or asserted against machine-dependent latency.
		type closeEdge struct {
			at    int64
			delta int
		}
		edges := make([]closeEdge, 0, 2*parallelPublicCloseCount)
		callLatencies := make([]int64, parallelPublicCloseCount)
		firstInvocation, lastInvocation := invocationNs[0], invocationNs[0]
		for index := range parallelPublicCloseCount {
			if invocationNs[index] <= 0 || returnNs[index] < invocationNs[index] || jitterNs[index] != parallelPublicCloseJitter(index).Nanoseconds() {
				t.Fatal("public burst invocation interval or seeded jitter is incomplete", index)
			}
			firstInvocation = min(firstInvocation, invocationNs[index])
			lastInvocation = max(lastInvocation, invocationNs[index])
			callLatencies[index] = returnNs[index] - invocationNs[index]
			edges = append(edges, closeEdge{at: invocationNs[index], delta: 1}, closeEdge{at: returnNs[index], delta: -1})
		}
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].at != edges[j].at {
				return edges[i].at < edges[j].at
			}
			return edges[i].delta < edges[j].delta
		})
		active, peak := 0, 0
		for _, edge := range edges {
			active += edge.delta
			peak = max(peak, active)
		}
		if active != 0 || peak < 2 {
			t.Fatal("public closes did not execute as overlapping calls", peak, active)
		}
		sort.Slice(callLatencies, func(i, j int) bool { return callLatencies[i] < callLatencies[j] })
		callLatency := map[string]int64{"min": callLatencies[0], "max": callLatencies[len(callLatencies)-1]}
		for _, percentile := range []int{50, 95, 99} {
			// Nearest rank gives a deterministic statistic over the exact2048
			// actual invocations; it is not a machine-dependent acceptance gate.
			index := (percentile*len(callLatencies)+99)/100 - 1
			callLatency["p"+strconv.Itoa(percentile)] = callLatencies[index]
		}
		var final parallelPublicCloseState
		for {
			final = parallelPublicCloseSnapshot(ctx, observer, fixture, recurring)
			if final.Settled == parallelPublicCloseCount+1 && final.Intents == 0 && events["all_financial_outcomes_observed"] == 0 {
				recordEvent("all_financial_outcomes_observed")
			}
			if final.Settled == parallelPublicCloseCount+1 && final.Intents == 0 && final.Journals == 0 && final.Unsettled == 0 &&
				final.Pending == 0 && final.ProviderDone == parallelPublicCloseCount+1 && final.Unfinished == 0 && worker.InflightCount() == 0 {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("public parallel-close pipeline did not drain every actual output", final)
			case <-done:
				t.Fatal("worker ended before full accounting/output completion", runErr)
			case <-time.After(10 * time.Millisecond):
			}
		}
		recordEvent("all_outputs_observed_before_drain")
		worker.Drain()
		handedBack := worker.WaitFinalHandback()
		worker.Close()
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("parallel-close Run did not join within its finite parent context", ctx.Err())
		}
		stopped = true
		final = parallelPublicCloseSnapshot(ctx, observer, fixture, recurring)
		if !handedBack || runErr != nil || worker.DrainCanceledCount() != 0 || final.Settled != parallelPublicCloseCount+1 || final.Intents != 0 ||
			final.Journals != 0 || final.Unsettled != 0 || final.Pending != 0 || final.ProviderDone != parallelPublicCloseCount+1 || final.Unfinished != 0 {
			t.Fatal("parallel-close output custody changed during final joined handback", final, runErr)
		}
		recordEvent("all_outputs_rechecked_after_drain_handback_and_run_join")
		fullFixtureNs := events["all_outputs_rechecked_after_drain_handback_and_run_join"]
		publicBurstFullNs := fullFixtureNs - events["public_burst_start"]
		protocol.enabled(false)
		wire := protocol.snapshot()
		delta := parallelCloseCounterDelta(t, beforeCounters, parallelCloseCounterSnapshot(t))
		workReruns := retries.callbacks.Load()
		afterClosed := contractClosedCounter.Snapshot()
		if !beforeClosed.Stable || !afterClosed.Stable || afterClosed.Confirmed-beforeClosed.Confirmed != parallelPublicCloseCount+1 ||
			afterClosed.Uncertain != beforeClosed.Uncertain || afterClosed.Untracked != beforeClosed.Untracked {
			t.Fatal("parallel-close changed exact financial commit custody")
		}
		if !bytes.Equal(fixture.reports, parallelPublicCloseSourceReports(ctx, fixture.finance.ids)) {
			t.Fatal("parallel-close changed preexisting source reports")
		}
		legacyFinancialCohortRequire(t, ctx, fixture.finance, legacyFinancialCohortCompleted(fixture.finance.ids))
		parallelPublicCloseRequireOutputs(t, ctx, fixture)
		for _, balance := range fixture.finance.balances {
			if Testing_NetEscrowByteCount(ctx, balance) != 0 {
				t.Fatal("parallel-close retained native or legacy reserved capacity")
			}
		}
		for index, id := range fixture.finance.ids {
			err := CloseContract(ctx, id, fixture.finance.providers[fixture.finance.providerIndexes[index]].destinationId, 3, false)
			if !errors.Is(err, errContractAlreadySettled) {
				t.Fatal("ordinary public replay lost its terminal refusal", err)
			}
		}
		legacyFinancialCohortRequire(t, ctx, fixture.finance, legacyFinancialCohortCompleted(fixture.finance.ids))
		replayed := contractClosedCounter.Snapshot()
		if !replayed.Stable || replayed.Confirmed != afterClosed.Confirmed || replayed.Uncertain != afterClosed.Uncertain || replayed.Untracked != afterClosed.Untracked {
			t.Fatal("public replay repeated financial ownership")
		}
		// Preserve actual scheduled/start/end timestamps instead of inferring
		// a count of polling sleeps from aggregate wall time. These stored UTC
		// timestamps are separate from the monotonic controller offsets.
		payerOrdinals := map[server.Id]int{}
		for ordinal, payer := range fixture.payers {
			payerOrdinals[payer.sourceNetworkId] = ordinal
		}
		payerTurns := make([]map[string]any, 0)
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT args_json,result_json,run_at,run_start_time,run_end_time
              FROM finished_task WHERE function_name=$1
              ORDER BY run_start_time,task_id LIMIT 4097`, task.NewTaskTarget(ApplyLegacyPayerSettlements).TargetFunctionName())
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var args, result string
					var runAt time.Time
					var start, end *time.Time
					server.Raise(rows.Scan(&args, &result, &runAt, &start, &end))
					var scope LegacyPayerSettlementArgs
					var page LegacySettlementFlushResult
					server.Raise(json.Unmarshal([]byte(args), &scope))
					server.Raise(json.Unmarshal([]byte(result), &page))
					ordinal, present := payerOrdinals[scope.PayerNetworkId]
					if !scope.Private || !present || start == nil || end == nil || end.Before(*start) || len(payerTurns) == 4096 {
						t.Fatal("public-close payer turn escaped bounded timing/identity custody")
					}
					payerTurns = append(payerTurns, map[string]any{"payer_ordinal": ordinal,
						"run_at_utc_unix_ns": runAt.UnixNano(), "run_start_utc_unix_ns": start.UnixNano(), "run_end_utc_unix_ns": end.UnixNano(),
						"scheduled_to_start_ns": start.Sub(runAt).Nanoseconds(), "function_interval_ns": end.Sub(*start).Nanoseconds(),
						"visited": page.Visited, "completed": page.Completed, "busy_or_gone": page.BusyOrGone, "failed": page.Failed})
				}
			})
		})
		raw, err := json.Marshal(map[string]any{"contracts": parallelPublicCloseCount + 1, "parallel_peer_calls": parallelPublicCloseCount, "payer_counts": counts,
			"initial_credit_each": 1000000, "expected_usage_each_contract": 3, "minimum_credit_usage_multiple": 10,
			"configured_payer_collection_window_ns": (5 * time.Second).Nanoseconds(), "production_payer_collection_window_ns": (30 * time.Second).Nanoseconds(),
			"jitter_seed": parallelPublicCloseJitterSeed, "planned_jitter_ns": jitterNs, "invocation_offsets_ns": invocationNs, "return_offsets_ns": returnNs,
			"event_offsets_from_pipeline_start_ns": events, "public_call_latency_ns": callLatency,
			"finished_payer_turns":                            payerTurns,
			"full_fixture_including_native_anchor_prelude_ns": fullFixtureNs, "public_burst_through_all_outputs_and_join_ns": publicBurstFullNs,
			"observed_public_closes_per_second_including_all_outputs_and_held_owner": float64(parallelPublicCloseCount) * float64(time.Second) / float64(publicBurstFullNs),
			"invocation_span_ns": lastInvocation - firstInvocation, "peak_public_call_intervals": peak, "acknowledgements_observed_while_held": heldAcknowledgements,
			"held_state": heldState, "held_native_state": heldNative, "final_state": final, "actual_work_tx_reruns": workReruns, "actual_including_replay_tx_reruns": retries.callbacks.Load(), "counter_deltas": delta,
			"wire": wire, "holder_commits": barrier.commits.Load(), "holder_rollbacks": barrier.rollbacks.Load(), "holder_lost": barrier.lost.Load(),
			"native_owner_resource": nativeOwnerResource, "common_ownership": ownership.snapshot(),
			"same_backend_common_key_first_witness": firstCommonKeyHeld, "same_backend_common_key_second_witness": secondCommonKeyHeld,
			"exclusion_window_closed_while_backend_held": closedWhileHeld, "pre_custody_metadata_release_evidence": legacyMetadataWhileHeld,
			"scope": "public closes plus actual registered worker outputs; artificial held-owner correctness fixture, not production capacity; held_state retains the first committed independent output snapshot, while contender and second owner/key witnesses remain separate; complete intervals include configured collection, eligibility/polling, posts, Drain and Run join; invocation latency is separate; exclusion covers the positively held hot-grant window ending before Release, not delayed COMMIT/Released ordering; all-writer source coverage and account/payment/task conflict controls remain required"})
		server.Raise(err)
		t.Logf("parallel_public_close_owner_control=%s", raw)
		// Preserve all complete finance and replay evidence before the causal
		// zero-retry verdict. The final shared-owner graph must also prove
		// business-entry exclusion; zero wire errors alone cannot supply it.
		if barrier.commits.Load() != 1 || barrier.rollbacks.Load() != 0 || barrier.lost.Load() != 0 {
			t.Fatal("held native accounting owner did not commit exactly once")
		}
		if heldNative.Contracts == 0 || heldNative.Outcomes != heldNative.Contracts || heldNative.Journals != heldNative.Contracts ||
			heldNative.MetadataSettled != 0 || heldNative.OriginalReservationRows != heldNative.Contracts || heldNative.OriginalRedisReservations != heldNative.Contracts {
			t.Fatal("public native peer closes entered metadata/release work while another admitted debit owner held their grant", heldNative)
		}
		if !firstCommonKeyHeld || !secondCommonKeyHeld || !closedWhileHeld {
			t.Fatal("native debit owner lacked the same-backend common key during the positive held window")
		}
		if err := ownership.requireComplete(); err != nil {
			t.Fatal(err)
		}
		if err := parallelCloseForbiddenCounters(delta, retries.callbacks.Load()); err != nil {
			t.Fatal(err)
		}
		for route, counters := range wire {
			if counters["ready_replies_observed"] == 0 {
				t.Fatal("parallel-close missing observed PostgreSQL route", route)
			}
			for key, value := range counters {
				if len(key) >= len("error_response_") && key[:len("error_response_")] == "error_response_" && value != 0 {
					t.Fatal(fmt.Sprintf("parallel-close wire refusal %s/%s=%d", route, key, value))
				}
			}
		}
		controlComplete = true
	})
}

// The burst's timer distribution is reproducible and strictly bounded; the
// owner/rendezvous barriers, rather than this timer, establish causal ordering.
func TestParallelPublicCloseSeededJitter(t *testing.T) {
	counts := map[time.Duration]int{}
	for index := range parallelPublicCloseCount {
		value := parallelPublicCloseJitter(index)
		if value < 0 || value > 20*time.Millisecond || value%time.Millisecond != 0 || value != parallelPublicCloseJitter(index) {
			t.Fatal("public burst jitter left its deterministic bound", index, value)
		}
		counts[value]++
	}
	if len(counts) < 3 {
		t.Fatal("public burst did not retain a spread of slight jitter", len(counts))
	}
}
