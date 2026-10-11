package model

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

func newLegacyDrainTestIntent(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, positive bool) server.Id {
	t.Helper()
	escrow, posts := createNetEscrowOrderingTestContract(ctx, f, 10)
	server.RunPosts(ctx, posts...)
	var amount ByteCount
	if positive {
		amount = 3
	}
	server.Raise(CloseContract(ctx, escrow.ContractId, f.sourceId, amount, false))
	server.Raise(CloseContract(ctx, escrow.ContractId, f.destinationId, amount, false))
	return escrow.ContractId
}

func legacyDrainTestReports(ctx context.Context, id server.Id) []byte {
	var reports []byte
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(party,used_transfer_byte_count,checkpoint,close_time) ORDER BY party) FROM contract_close WHERE contract_id=$1`, id).Scan(&reports))
	})
	return reports
}

func requireLegacyDrainNoFinancialPrefix(t testing.TB, ctx context.Context, id server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var intact bool
		server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND provider_usage IS NULL
			AND EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
			AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
			FROM transfer_contract WHERE contract_id=$1`, id).Scan(&intact))
		if !intact {
			t.Fatal("refused drain changed proof, outcome, intent or payout ledger")
		}
	})
}

func TestLegacySettlementDrainScopeValidation(t *testing.T) {
	id, payer := server.NewId(), server.NewId()
	for _, request := range []LegacySettlementDrainRequest{
		{}, {ExpectedPayerNetworkId: payer}, {ContractIds: []server.Id{id}},
		{ExpectedPayerNetworkId: payer, ContractIds: []server.Id{{}}},
		{ExpectedPayerNetworkId: payer, ContractIds: []server.Id{id, id}},
		{ExpectedPayerNetworkId: payer, ContractIds: make([]server.Id, 33)},
	} {
		if _, err := DrainLegacySettlements(context.Background(), request); err == nil {
			t.Fatal("invalid drain scope reached ownership")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := DrainLegacySettlements(ctx, LegacySettlementDrainRequest{ExpectedPayerNetworkId: payer, ContractIds: []server.Id{id}, Apply: true})
	if err == nil || len(result.Contracts) != 1 || result.Contracts[0].Status != "not_attempted" {
		t.Fatal("canceled drain started work")
	}
	if legacySettlementDrainErrorStatus(errContractInsufficientEscrow) != "accounting_refused" || legacySettlementDrainErrorStatus(errors.New("private details")) != "failed" {
		t.Fatal("financial error was not reduced to a fixed class")
	}
}

func TestLegacySettlementDrainPreviewApplyConservation32(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		})
		ids := make([]server.Id, 32)
		reports := make([][]byte, 32)
		for i := range ids {
			ids[i] = newLegacyDrainTestIntent(t, ctx, f, i >= 24)
			reports[i] = legacyDrainTestReports(ctx, ids[i])
		}
		request := LegacySettlementDrainRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: ids}
		preview, err := DrainLegacySettlements(ctx, request)
		if err != nil || len(preview.Contracts) != 32 {
			t.Fatal("bounded drain preview failed")
		}
		for i, entry := range preview.Contracts {
			if entry.ContractId != ids[i] || entry.Status != "eligible" || entry.FinancialCommitAcknowledged || entry.PostProcessing != "not_started" || entry.Mirror != "not_verified" {
				t.Fatal("preview claimed financial or mirror authority")
			}
			requireLegacyDrainNoFinancialPrefix(t, ctx, ids[i])
			requireLegacySettlementTestState(t, ctx, f, ids[i], true, false, 1000, 320)
		}
		request.Apply = true
		applied, err := DrainLegacySettlements(ctx, request)
		if err != nil || len(applied.Contracts) != 32 {
			t.Fatal("bounded drain apply failed")
		}
		proofs := make([][]byte, 32)
		for i, entry := range applied.Contracts {
			if entry.ContractId != ids[i] || entry.Status != "financial_committed" || !entry.FinancialCommitAcknowledged || entry.PostProcessing != "returned_unverified" || entry.Mirror != "not_verified" {
				t.Fatal("drain misreported financial commit or claimed mirror acknowledgement")
			}
			requireLegacySettlementTestState(t, ctx, f, ids[i], false, true, 976, 0)
			if !bytes.Equal(reports[i], legacyDrainTestReports(ctx, ids[i])) {
				t.Fatal("ordinary drain altered original final reports")
			}
			var usage *contractUsageSnapshot
			proofs[i], usage = readContractExpiryTestSnapshot(t, ctx, ids[i])
			want := ByteCount(0)
			if i >= 24 {
				want = 3
			}
			if usage.ByteCount != want || usage.Expiry != nil {
				t.Fatal("drain synthesized expiry or changed ordinary usage proof")
			}
		}
		replay, err := DrainLegacySettlements(ctx, request)
		if err != nil {
			t.Fatal("settled replay failed")
		}
		for i, entry := range replay.Contracts {
			if entry.FinancialCommitAcknowledged || entry.Status != "busy_intent_or_absent" {
				t.Fatal("replay reclaimed financial ownership")
			}
			after, _ := readContractExpiryTestSnapshot(t, ctx, ids[i])
			if !bytes.Equal(proofs[i], after) {
				t.Fatal("replay rewrote usage proof")
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var ledgerBytes, ledgerRevenue, pendingBytes, pendingRevenue int64
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
				(SELECT COALESCE(sum(payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
				COALESCE(sum((allocation->>'bytes')::bigint),0),COALESCE(sum((allocation->>'revenue')::bigint),0)
				FROM pending_task CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') allocation
				WHERE function_name=$2 AND (args_json::jsonb->>'applied')::boolean=false`, ids, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(&ledgerBytes, &ledgerRevenue, &pendingBytes, &pendingRevenue))
			if ledgerBytes != 24 || ledgerRevenue != 24 || pendingBytes != 24 || pendingRevenue != 24 {
				t.Fatal("exact debit, earnings or durable projection conservation failed")
			}
		})
		projectLegacyProviderTotalsForTest(t, ctx)
		projectLegacyProviderTotalsForTest(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count=24 AND provided_net_revenue_nano_cents=24 FROM account_balance WHERE network_id=$1`, f.destinationNetworkId).Scan(&exact))
			if !exact {
				t.Fatal("provider projection replay duplicated or lost earnings")
			}
		})
	})
}

func TestLegacySettlementDrainCustodyRefusals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		for _, kind := range []string{"payer_mismatch", "held", "not_due", "disputed", "intent_policy_changed", "reports_changed", "reservation_mode_changed", "debit_present"} {
			id := newLegacyDrainTestIntent(t, ctx, f, false)
			payer := f.sourceNetworkId
			server.Tx(ctx, func(tx server.PgTx) {
				switch kind {
				case "payer_mismatch":
					payer = server.NewId()
				case "held":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET failure_code='accounting' WHERE contract_id=$1`, id))
				case "not_due":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=(clock_timestamp() AT TIME ZONE 'UTC')+interval '1 hour' WHERE contract_id=$1`, id))
				case "disputed":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`, id))
				case "intent_policy_changed":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET clear_dispute=true WHERE contract_id=$1`, id))
				case "reports_changed":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET checkpoint=true WHERE contract_id=$1 AND party='destination'`, id))
				case "reservation_mode_changed":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET redis_reserved=true WHERE contract_id=$1`, id))
				case "debit_present":
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_debit_journal(contract_id,balance_id,debit_byte_count,shard) VALUES($1,$2,0,$3)`, id, f.balanceId, transferDebitShard(f.balanceId)))
				}
			})
			before := legacyDrainTestReports(ctx, id)
			reservedBefore := Testing_NetEscrowByteCount(ctx, f.balanceId)
			result, err := DrainLegacySettlements(ctx, LegacySettlementDrainRequest{ExpectedPayerNetworkId: payer, ContractIds: []server.Id{id}, Apply: true})
			if err != nil || result.Contracts[0].Status != kind || result.Contracts[0].FinancialCommitAcknowledged {
				t.Fatal("changed custody was admitted or misclassified: " + kind)
			}
			requireLegacyDrainNoFinancialPrefix(t, ctx, id)
			if !bytes.Equal(before, legacyDrainTestReports(ctx, id)) {
				t.Fatal("refusal modified reports")
			}
			if Testing_NetEscrowByteCount(ctx, f.balanceId) != reservedBefore {
				t.Fatal("refused drain changed its observed reservation mirror")
			}
			if kind == "held" {
				server.Db(ctx, func(conn server.PgConn) {
					var held bool
					server.Raise(conn.QueryRow(ctx, `SELECT failure_code='accounting' FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&held))
					if !held {
						t.Fatal("accounting hold was cleared")
					}
				})
			}
		}
	})
}

func TestLegacySettlementDrainOwnershipAndPayerRace(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		request := LegacySettlementDrainRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: []server.Id{id}, Apply: true}
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		for _, owner := range []string{"intent", "contract", "grant"} {
			held, err := conn.Begin(ctx)
			server.Raise(err)
			func() {
				defer held.Rollback(context.Background())
				switch owner {
				case "intent":
					server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM legacy_settlement_intent WHERE contract_id=$1 FOR UPDATE`, id))
				case "contract":
					server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, id))
				case "grant":
					server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balanceId))
				}
				result, err := DrainLegacySettlements(ctx, request)
				want := map[string]string{"intent": "busy_intent_or_absent", "contract": "busy_contract_or_absent", "grant": "busy_grant_set"}[owner]
				if err != nil || result.Contracts[0].Status != want || result.Contracts[0].FinancialCommitAcknowledged {
					t.Fatal("locked owner was bypassed or misattributed")
				}
				requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
			}()
		}
		other := newNetEscrowOrderingTestFixture(t, ctx)
		request.Apply = false
		preview, err := DrainLegacySettlements(ctx, request)
		if err != nil || preview.Contracts[0].Status != "eligible" {
			t.Fatal("initial scope preview failed")
		}
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=$2 WHERE contract_id=$1`, id, other.sourceNetworkId))
		request.Apply = true
		result, err := DrainLegacySettlements(ctx, request)
		if err != nil || result.Contracts[0].Status != "busy_contract_or_absent" {
			t.Fatal("payer-change owner was bypassed")
		}
		server.Raise(held.Commit(ctx))
		result, err = DrainLegacySettlements(ctx, request)
		if err != nil || result.Contracts[0].Status != "payer_mismatch" || result.Contracts[0].FinancialCommitAcknowledged {
			t.Fatal("preview was treated as an apply custody token")
		}
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
	})
}

func TestLegacySettlementDrainRollbackLostAckAndAccounting(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		request := LegacySettlementDrainRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: []server.Id{id}, Apply: true}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		})
		before := legacyDrainTestReports(ctx, id)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		_, completed, busy, _, err := drainLegacySettlementInTx(ctx, tx, id, f.sourceNetworkId)
		server.Raise(err)
		if !completed || busy {
			t.Fatal("rollback control did not reach financial prefix")
		}
		server.Raise(tx.Rollback(ctx))
		requireLegacyDrainNoFinancialPrefix(t, ctx, id)
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=1 WHERE contract_id=$1`, id))
		})
		result, err := DrainLegacySettlements(ctx, request)
		if err != nil || result.Contracts[0].Status != "accounting_refused" || result.Contracts[0].FinancialCommitAcknowledged {
			t.Fatal("insufficient custody was cleared or committed")
		}
		requireLegacyDrainNoFinancialPrefix(t, ctx, id)
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=100 WHERE contract_id=$1`, id))
		})
		collision := providerTotalsTestTask(ctx, id, f.destinationNetworkId)
		payload := task.GetTasks(ctx, collision)[collision].ArgsJson
		result, err = DrainLegacySettlements(ctx, request)
		if err != nil || result.Contracts[0].Status != "failed" || result.Contracts[0].FinancialCommitAcknowledged {
			t.Fatal("projection collision did not roll back complete financial prefix")
		}
		requireLegacyDrainNoFinancialPrefix(t, ctx, id)
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		if task.GetTasks(ctx, collision)[collision].ArgsJson != payload {
			t.Fatal("collision altered existing projection owner")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE task_id=$1`, collision))
		})
		tx, err = conn.Begin(ctx)
		server.Raise(err)
		posts, completed, busy, _, err := drainLegacySettlementInTx(ctx, tx, id, f.sourceNetworkId)
		server.Raise(err)
		if !completed || busy {
			t.Fatal("lost-ack control never owned settlement")
		}
		server.Raise(tx.Commit(ctx)) // Deliberately omit reply consumption and all posts.
		result, err = DrainLegacySettlements(ctx, request)
		if err != nil || result.Contracts[0].FinancialCommitAcknowledged || result.Contracts[0].Status != "busy_intent_or_absent" {
			t.Fatal("lost-ack replay repeated financial transition")
		}
		requireLegacyProviderDurability(t, ctx, f, id, 11, 11)
		ReconcileNetEscrowForNetwork(ctx, f.sourceNetworkId, true)
		server.RunPosts(ctx, posts...)
		server.RunPosts(ctx, posts...)
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, f, id, 11, 11)
		if !bytes.Equal(before, legacyDrainTestReports(ctx, id)) {
			t.Fatal("settlement or replay modified final reports")
		}
	})
}

func TestLegacySettlementDrainCancellationKeepsCommittedPrefix(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		first, firstID := legacySettlementTestIntent(t, ctx)
		// Create a second real grant for the same payer, selecting it while the
		// first grant's availability window is closed. Both contracts retain
		// their admitted payer and the original grant retains all reserved credit.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2 WHERE balance_id=$1`, first.balanceId, server.NowUtc().Add(-time.Minute)))
		})
		AddBasicTransferBalance(ctx, first.sourceNetworkId, 1000, server.NowUtc(), server.NowUtc().Add(time.Hour))
		balances := GetActiveTransferBalances(ctx, first.sourceNetworkId)
		if len(balances) != 1 {
			t.Fatal("second active payer grant was not isolated")
		}
		second := first
		second.balanceId = balances[0].BalanceId
		escrow, posts := createNetEscrowOrderingTestContract(ctx, second, 100)
		server.RunPosts(ctx, posts...)
		secondID := escrow.ContractId
		server.Raise(CloseContract(ctx, secondID, second.sourceId, 11, false))
		server.Raise(CloseContract(ctx, secondID, second.destinationId, 11, false))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2 WHERE balance_id=$1`, first.balanceId, server.NowUtc().Add(time.Hour)))
		})
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		pid := contractLifecycleTestBackendPid(t, ctx, held)
		var owned bool
		rows, err := held.Query(ctx, `SELECT balance_id FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1 FOR UPDATE`, second.balanceId)
		server.WithPgResult(rows, err, func() { owned = rows.Next() })
		if !owned {
			t.Fatal("cancellation cache barrier missing")
		}
		stopped, stop := context.WithCancel(ctx)
		type response struct {
			result LegacySettlementDrainResult
			err    error
		}
		done := make(chan response, 1)
		go func() {
			result, err := DrainLegacySettlements(stopped, LegacySettlementDrainRequest{ExpectedPayerNetworkId: first.sourceNetworkId, ContractIds: []server.Id{firstID, secondID}, Apply: true})
			done <- response{result, err}
		}()
		joined := false
		defer func() {
			stop()
			held.Rollback(context.Background())
			if !joined {
				<-done
			}
		}()
		requireContractLifecycleBlockedBy(t, ctx, held, pid)
		stop()
		reply := <-done
		joined = true
		server.Raise(held.Rollback(ctx))
		if reply.err == nil || len(reply.result.Contracts) != 2 || !reply.result.Contracts[0].FinancialCommitAcknowledged || reply.result.Contracts[1].FinancialCommitAcknowledged {
			t.Fatal("cancellation erased committed prefix or acknowledged rollback")
		}
		requireLegacySettlementTestState(t, ctx, first, firstID, false, true, 989, 0)
		requireLegacySettlementTestState(t, ctx, second, secondID, true, false, 1000, 100)
		requireLegacyDrainNoFinancialPrefix(t, ctx, secondID)
	})
}
