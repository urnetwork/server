// The acceptance population shares both payer grants and provider accounts.
// It exercises the real public deadline body and both real projection workers;
// scheduler selection and Main throughput are outside this local comparison.
package model

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// Count original driver contention errors, including batch failures. This is
// not a claim that an uncontended statement has literally zero lock duration.
type deadlineOverlapTrace struct{ contention atomic.Int64 }

func (self *deadlineOverlapTrace) inspect(err error) {
	var failure *pgconn.PgError
	if errors.As(err, &failure) {
		switch failure.Code {
		case "40001", "40P01", "55P03":
			self.contention.Add(1)
		}
	}
}
func (self *deadlineOverlapTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (self *deadlineOverlapTrace) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	self.inspect(data.Err)
}
func (self *deadlineOverlapTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}
func (self *deadlineOverlapTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	self.inspect(data.Err)
}
func (self *deadlineOverlapTrace) TraceBatchEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	self.inspect(data.Err)
}

func deadlineOverlapSnapshot(ctx context.Context, ids, balances, providers []server.Id) (state []byte) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_build_array(
			(SELECT jsonb_agg(to_jsonb(c) ORDER BY contract_id) FROM transfer_contract c WHERE contract_id=ANY($1)),
			(SELECT jsonb_agg(to_jsonb(c) ORDER BY contract_id,party) FROM contract_close c WHERE contract_id=ANY($1)),
			(SELECT jsonb_agg(to_jsonb(e) ORDER BY contract_id,balance_id) FROM transfer_escrow e WHERE contract_id=ANY($1)),
			(SELECT jsonb_agg(to_jsonb(s) ORDER BY contract_id,balance_id,network_id) FROM transfer_escrow_sweep s WHERE contract_id=ANY($1)),
			(SELECT jsonb_agg(to_jsonb(b) ORDER BY balance_id) FROM transfer_balance b WHERE balance_id=ANY($2)),
			(SELECT jsonb_agg(to_jsonb(a) ORDER BY network_id) FROM account_balance a WHERE network_id=ANY($3)),
			(SELECT jsonb_agg(to_jsonb(j) ORDER BY contract_id,balance_id) FROM transfer_debit_journal j WHERE contract_id=ANY($1)))`,
			ids, balances, providers).Scan(&state))
	}, server.OptNoRetry())
	return
}

func TestDeadlineDeferred2048ContractsPreserveConservationWithoutRetries(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(WithProviderWorkSessionSource(t.Context(), nil), 4*time.Minute)
		defer cancel()
		const payerCount, perPayer, concurrent = 64, 32, 32
		const total = payerCount * perPayer
		fixtures := make([]netEscrowOrderingTestFixture, payerCount)
		balances, providers := []server.Id{}, []server.Id{}
		for i := range fixtures {
			fixtures[i] = newNetEscrowOrderingTestFixture(t, ctx)
			if i < 4 {
				providers = append(providers, fixtures[i].destinationNetworkId)
			} else {
				fixtures[i].destinationNetworkId = fixtures[i%4].destinationNetworkId
				fixtures[i].destinationId = fixtures[i%4].destinationId
			}
			balances = append(balances, fixtures[i].balanceId)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=100000,balance_byte_count=100000,net_revenue_nano_cents=200000 WHERE balance_id=ANY($1)`, balances))
		})
		ids := make([]server.Id, total)
		for p, f := range fixtures {
			for n := range perPayer {
				id := createRedisAdmissionTest(ctx, f, 100).ContractId
				ids[p*perPayer+n] = id
				server.Raise(CloseContract(ctx, id, f.sourceId, 17, true))
			}
		}
		providerTotalsBatchWriteCounter(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE test_deadline_grant_write(balance_id uuid NOT NULL);
				CREATE FUNCTION test_deadline_grant_write() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN INSERT INTO test_deadline_grant_write VALUES(NEW.balance_id); RETURN NEW; END $$;
				CREATE TRIGGER test_deadline_grant_write AFTER UPDATE ON transfer_balance
				FOR EACH ROW EXECUTE FUNCTION test_deadline_grant_write()`))
		})
		trace := &deadlineOverlapTrace{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		defer func() { server.Raise(scope.Close()) }()
		var refused, uncertain, reruns, waiting atomic.Int64
		observed := server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		observed = server.Testing_WithPgOwnershipObservation(observed, func(event server.PgOwnershipEvent) {
			switch event.Kind {
			case server.PgOwnershipRefused:
				refused.Add(1)
			case server.PgOwnershipUncertain:
				uncertain.Add(1)
			case server.PgOwnershipWaiting:
				waiting.Add(1)
			}
		})
		ctx = observed
		// Each consecutive pair shares a payer. The next pair changes payer;
		// all four providers are shared by sixteen independent grants each.
		jobs := make(chan int, total)
		for i := range total {
			p, n := (i/2)%payerCount, 2*(i/(2*payerCount))+i%2
			jobs <- p*perPayer + n
		}
		close(jobs)
		type completion struct {
			charged  ByteCount
			err      error
			terminal bool
		}
		results := make(chan completion, total)
		var group sync.WaitGroup
		var running [payerCount]int
		peakSamePayer := 0
		var lock sync.Mutex
		started := time.Now()
		for range concurrent {
			group.Add(1)
			go func() {
				defer group.Done()
				for index := range jobs {
					p := index / perPayer
					lock.Lock()
					running[p]++
					peakSamePayer = max(peakSamePayer, running[p])
					lock.Unlock()
					// A small deterministic arrival jitter is the only timing
					// variation; no failure is retried or turned into success.
					timer := time.NewTimer(time.Duration(index%5) * time.Millisecond)
					select {
					case <-timer.C:
					case <-ctx.Done():
					}
					timer.Stop()
					result, err := ReconcileContractAtDeadline(observed, ids[index], time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
					value := completion{err: err}
					if result != nil {
						value.charged, value.terminal = result.Charged, result.Outcome == ContractOutcomeSettled && !result.AlreadyClosed && !result.Missing
					}
					results <- value
					lock.Lock()
					running[p]--
					lock.Unlock()
				}
			}()
		}
		group.Wait()
		close(results)
		elapsed := time.Since(started)
		completed, failed := 0, 0
		var charged ByteCount
		for value := range results {
			if value.err != nil || !value.terminal || value.charged != 17 {
				failed++
			} else {
				completed++
			}
			charged += value.charged
		}
		if completed != total || failed != 0 || charged != 17*total || peakSamePayer < 2 || refused.Load()+uncertain.Load()+reruns.Load()+trace.contention.Load() != 0 {
			t.Fatal("overlapping deadline population failed exact no-retry acceptance", completed, failed, charged, peakSamePayer, refused.Load(), uncertain.Load(), reruns.Load(), trace.contention.Load())
		}
		t.Logf("deadline_local_acceptance contracts=%d payers=%d providers=%d workers=%d elapsed_seconds=%.6f acknowledged_busy_waits=%d errors=0 transaction_reruns=0", total, payerCount, len(providers), concurrent, elapsed.Seconds(), waiting.Load())
		providerTaskIds := []server.Id{}
		server.Db(ctx, func(conn server.PgConn) {
			var terminal, reports, escrows, sweeps, journals, unchangedGrants, grantsWritten, accountsWritten int
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled' AND NOT dispute),
				(SELECT count(*) FROM contract_close WHERE contract_id=ANY($1) AND party='source' AND checkpoint AND used_transfer_byte_count=17),
				(SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND redis_reserved AND payout_byte_count=17),
				(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=ANY($1) AND payout_byte_count=17 AND payout_net_revenue_nano_cents=17),
				(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=ANY($1) AND NOT applied AND debit_byte_count=17),
				(SELECT count(*) FROM transfer_balance WHERE balance_id=ANY($2) AND balance_byte_count=100000),
				(SELECT count(*) FROM test_deadline_grant_write),
				(SELECT count(*) FROM test_provider_total_write)`, ids, balances).Scan(&terminal, &reports, &escrows, &sweeps, &journals, &unchangedGrants, &grantsWritten, &accountsWritten))
			if terminal != total || reports != total || escrows != total || sweeps != total || journals != total || unchangedGrants != payerCount || grantsWritten != 0 || accountsWritten != 0 {
				t.Fatal("deadline body bypassed journals, lost earnings, or wrote a shared projection", terminal, reports, escrows, sweeps, journals, unchangedGrants, grantsWritten, accountsWritten)
			}
			rows, err := conn.Query(ctx, `SELECT task_id,args_json FROM pending_task WHERE function_name=$1`, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName())
			server.WithPgResult(rows, err, func() {
				seen := map[server.Id]bool{}
				selected := map[server.Id]bool{}
				for _, id := range ids {
					selected[id] = true
				}
				for rows.Next() {
					var id server.Id
					var data string
					server.Raise(rows.Scan(&id, &data))
					payload, err := decodeLegacyProviderTotals(data)
					if err != nil || !selected[payload.ContractId] || seen[payload.ContractId] || payload.Applied || len(payload.Totals) != 1 || payload.Totals[0].Bytes != 17 || payload.Totals[0].Revenue != 17 {
						t.Fatal("deadline population lost exact immutable allocation", err)
					}
					seen[payload.ContractId] = true
					providerTaskIds = append(providerTaskIds, id)
				}
			})
		})
		if len(providerTaskIds) != total {
			t.Fatal("deadline population did not retain one projection owner each", len(providerTaskIds))
		}
		for _, f := range fixtures {
			if reserved := Testing_NetEscrowByteCount(ctx, f.balanceId); reserved != 17*perPayer {
				t.Fatal("deadline released unapplied native consumption", reserved)
			}
			applied, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
			if err != nil || busy || applied != perPayer || released != perPayer {
				t.Fatal("batched debit did not preserve exact population", applied, released, busy, err)
			}
			if reserved := Testing_NetEscrowByteCount(ctx, f.balanceId); reserved != 0 {
				t.Fatal("applied debit retained reservation", reserved)
			}
		}
		providerTotalsBatchDue(ctx, providerTaskIds)
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(NewLegacyProviderTotalsTaskTarget())
		remaining := total
		for pass := 0; remaining > 0; pass++ {
			if pass >= total {
				t.Fatal("provider batches did not finish bounded population")
			}
			finished, retried, posts, err := worker.EvalTasks(min(remaining, 256))
			if err != nil || len(finished) == 0 || len(retried)+len(posts) != 0 {
				t.Fatal("provider batch failed or rescheduled exact allocation", len(finished), len(retried), err)
			}
			remaining -= len(finished)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var grants, accounts, grantsWritten, accountsWritten, remainingDebt, remainingOwners int
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_balance WHERE balance_id=ANY($1) AND balance_byte_count=100000-544),
				(SELECT count(*) FROM account_balance WHERE network_id=ANY($2) AND provided_byte_count=8704 AND provided_net_revenue_nano_cents=8704),
				(SELECT count(*) FROM test_deadline_grant_write),
				(SELECT count(*) FROM test_provider_total_write),
				(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=ANY($3)),
				(SELECT count(*) FROM pending_task WHERE task_id=ANY($4))`, balances, providers, ids, providerTaskIds).Scan(&grants, &accounts, &grantsWritten, &accountsWritten, &remainingDebt, &remainingOwners))
			if grants != payerCount || accounts != 4 || grantsWritten != payerCount || accountsWritten < 32 || accountsWritten >= total || remainingDebt+remainingOwners != 0 {
				t.Fatal("batched projections lost conservation or amortization", grants, accounts, grantsWritten, accountsWritten, remainingDebt, remainingOwners)
			}
		})
		before := deadlineOverlapSnapshot(ctx, ids, balances, providers)
		for _, id := range ids {
			result, err := ReconcileContractAtDeadline(ctx, id, server.NowUtc())
			if err != nil || result == nil || !result.AlreadyClosed || result.Charged != 0 {
				t.Fatal("population replay repeated settlement", err)
			}
		}
		if !bytes.Equal(before, deadlineOverlapSnapshot(ctx, ids, balances, providers)) {
			t.Fatal("population terminal replay changed exact financial or original evidence state")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name=$1`, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(&count))
			if count != 0 {
				t.Fatal("population replay republished provider owners", count)
			}
		})
		if trace.contention.Load() != 0 || refused.Load()+uncertain.Load()+reruns.Load() != 0 {
			t.Fatal("projection or replay introduced contention errors or transaction retries", trace.contention.Load(), refused.Load(), uncertain.Load(), reruns.Load())
		}
	})
}
