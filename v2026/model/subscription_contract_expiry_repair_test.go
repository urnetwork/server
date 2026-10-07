package model

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

func newContractExpiryRepairTestContract(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, positive bool) server.Id {
	t.Helper()
	escrow, posts := createNetEscrowOrderingTestContract(ctx, f, 10)
	server.RunPosts(ctx, posts...)
	var amount ByteCount
	if positive {
		amount = 3
	}
	server.Raise(CloseContract(ctx, escrow.ContractId, f.sourceId, amount, false))
	if positive {
		server.Raise(CloseContract(ctx, escrow.ContractId, f.destinationId, amount, true))
	}
	server.Tx(ctx, func(tx server.PgTx) {
		old := server.NowUtc().Add(-time.Hour)
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, escrow.ContractId, old))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, escrow.ContractId, old))
	})
	return escrow.ContractId
}

func requireContractExpiryRepairUntouched(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id, reports int, proof bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var gotReports, intents, debits, sweeps int
		var unverified, retained, terminal bool
		var credit ByteCount
		server.Raise(conn.QueryRow(ctx, `SELECT usage_unverified,provider_usage IS NOT NULL,outcome IS NOT NULL,
			(SELECT count(*) FROM contract_close WHERE contract_id=$1),
			(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=$1),
			(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=$1),
			(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=$1),
			(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)
			FROM transfer_contract WHERE contract_id=$1`, id, f.balanceId).Scan(&unverified, &retained, &terminal, &gotReports, &intents, &debits, &sweeps, &credit))
		if unverified != proof || retained != proof || terminal || gotReports != reports || intents != 0 || debits != 0 || sweeps != 0 || credit != 1000 {
			t.Fatal("repair refusal changed proof, report, custody, outcome or financial state")
		}
	})
}

func TestContractExpiryRepairScopeValidation(t *testing.T) {
	id, payer := server.NewId(), server.NewId()
	for _, request := range []ContractExpiryRepairRequest{
		{}, {ExpectedPayerNetworkId: payer}, {ContractIds: []server.Id{id}},
		{ExpectedPayerNetworkId: payer, ContractIds: []server.Id{{}}},
		{ExpectedPayerNetworkId: payer, ContractIds: []server.Id{id, id}},
		{ExpectedPayerNetworkId: payer, ContractIds: make([]server.Id, 33)},
	} {
		if _, err := RepairContractExpiry(context.Background(), request); err == nil {
			t.Fatal("invalid explicit scope admitted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := RepairContractExpiry(ctx, ContractExpiryRepairRequest{ExpectedPayerNetworkId: payer, ContractIds: []server.Id{id}, Apply: true})
	if err == nil || len(result.Contracts) != 1 || result.Contracts[0].Status != "not_attempted" {
		t.Fatal("canceled scope started work")
	}
	for _, test := range []struct {
		err  error
		want string
	}{
		{contractExpiryRepairRefusal("payer_mismatch"), "payer_mismatch"},
		{context.Canceled, "canceled"}, {context.DeadlineExceeded, "deadline"},
		{&pgconn.PgError{Code: "55P03", Message: "private"}, "busy"},
		{&pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}, "statement_timeout"},
		{errors.New("private financial details"), "failed"},
	} {
		if got := contractExpiryRepairErrorStatus(test.err); got != test.want {
			t.Fatal("unsafe or incorrect error class")
		}
	}
}

// The complete maximum selected cohort covers both observed report shapes.
// Repair queues the established owner; only that owner debits and releases.
func TestContractExpiryRepairPreviewApplyAccounting32(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		ids := make([]server.Id, 32)
		for i := range ids {
			ids[i] = newContractExpiryRepairTestContract(t, ctx, f, i >= 30)
		}
		request := ContractExpiryRepairRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: ids}
		preview, err := RepairContractExpiry(ctx, request)
		if err != nil || len(preview.Contracts) != 32 {
			t.Fatal("bounded preview failed")
		}
		for i, entry := range preview.Contracts {
			if entry.ContractId != ids[i] || entry.Status != "eligible" || entry.ProofCommitted {
				t.Fatal("preview is not a read-only eligibility observation")
			}
			reports := 1
			if i >= 30 {
				reports = 2
			}
			requireContractExpiryRepairUntouched(t, ctx, f, ids[i], reports, false)
		}
		if Testing_NetEscrowByteCount(ctx, f.balanceId) != 320 {
			t.Fatal("preview changed reservation mirror")
		}
		request.Apply = true
		applied, err := RepairContractExpiry(ctx, request)
		if err != nil || len(applied.Contracts) != 32 {
			t.Fatal("bounded cohort apply failed")
		}
		proofs := make([][]byte, 32)
		for i, entry := range applied.Contracts {
			if entry.ContractId != ids[i] || entry.Status != "legacy_intent_present" || !entry.ProofCommitted || entry.ObservedOutcome != nil || entry.LegacyIntentPresent == nil || !*entry.LegacyIntentPresent {
				t.Fatal("apply did not retain proof and delegate custody")
			}
			var snapshot *contractUsageSnapshot
			proofs[i], snapshot = readContractExpiryTestSnapshot(t, ctx, ids[i])
			if snapshot.Expiry == nil {
				t.Fatal("original expiry proof absent")
			}
			if i < 30 {
				if len(snapshot.Expiry.Reports) != 1 || snapshot.ByteCount != 0 {
					t.Fatal("missing peer became invented verified usage")
				}
			} else if len(snapshot.Expiry.Reports) != 2 || snapshot.ByteCount != 3 || !snapshot.Expiry.Reports[ContractPartyDestination].Checkpoint {
				t.Fatal("original positive checkpoint proof changed")
			}
			requireLegacySettlementTestState(t, ctx, f, ids[i], true, false, 1000, 320)
		}
		replay, err := RepairContractExpiry(ctx, request)
		if err != nil {
			t.Fatal("idempotent delegated replay failed")
		}
		for i, entry := range replay.Contracts {
			if entry.Status != "legacy_intent_present" || entry.ProofCommitted {
				t.Fatal("replay bypassed pending owner")
			}
			after, _ := readContractExpiryTestSnapshot(t, ctx, ids[i])
			if !bytes.Equal(proofs[i], after) {
				t.Fatal("replay rewrote retained original proof")
			}
		}
		for _, index := range []int{0, 30} {
			completed, busy, _, err := flushLegacySettlement(ctx, ids[index])
			if err != nil || !completed || busy {
				t.Fatal("ordinary financial owner failed after repair")
			}
			completed, _, _, err = flushLegacySettlement(ctx, ids[index])
			if err != nil || completed {
				t.Fatal("financial replay repeated consumption")
			}
		}
		requireLegacySettlementTestState(t, ctx, f, ids[0], false, true, 997, 300)
		requireLegacySettlementTestState(t, ctx, f, ids[30], false, true, 997, 300)
		requireLegacyProviderDurability(t, ctx, f, ids[30], 3)
		for _, index := range []int{0, 30} {
			after, _ := readContractExpiryTestSnapshot(t, ctx, ids[index])
			if !bytes.Equal(proofs[index], after) {
				t.Fatal("settlement rewrote original proof")
			}
		}
	})
}

func TestContractExpiryRepairRefusals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		for _, kind := range []string{"payer_mismatch", "recent_report", "disputed", "legacy_intent_present", "reservation_mode_changed"} {
			id := newContractExpiryRepairTestContract(t, ctx, f, false)
			payer := f.sourceNetworkId
			server.Tx(ctx, func(tx server.PgTx) {
				switch kind {
				case "payer_mismatch":
					payer = server.NewId()
				case "recent_report":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, id, server.NowUtc()))
				case "disputed":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`, id))
				case "legacy_intent_present":
					server.RaisePgResult(tx.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, id))
					server.Raise(queueLegacySettlementInTx(ctx, tx, id, ContractOutcomeSettled, false))
					server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET failure_code='accounting',next_attempt_time=clock_timestamp()+interval '1 hour' WHERE contract_id=$1`, id))
				case "reservation_mode_changed":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET redis_reserved=true WHERE contract_id=$1`, id))
				}
			})
			result, err := RepairContractExpiry(ctx, ContractExpiryRepairRequest{ExpectedPayerNetworkId: payer, ContractIds: []server.Id{id}, Apply: true})
			if err != nil || len(result.Contracts) != 1 || result.Contracts[0].Status != kind || result.Contracts[0].ProofCommitted {
				t.Fatal("unsafe state was admitted or incorrectly classified: " + kind)
			}
			if kind != "legacy_intent_present" {
				requireContractExpiryRepairUntouched(t, ctx, f, id, 1, false)
			} else {
				server.Db(ctx, func(conn server.PgConn) {
					var preserved bool
					server.Raise(conn.QueryRow(ctx, `SELECT failure_code='accounting' AND next_attempt_time>clock_timestamp() FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&preserved))
					if !preserved {
						t.Fatal("accounting hold was altered")
					}
				})
			}
		}
		result, err := RepairContractExpiry(ctx, ContractExpiryRepairRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: []server.Id{server.NewId()}, Apply: true})
		if err != nil || result.Contracts[0].Status != "missing" {
			t.Fatal("missing explicit contract not refused")
		}
	})
}

// External custody or report changes at each real transaction boundary must
// withdraw the adapter. Its already committed proof remains immutable.
func TestContractExpiryRepairContinuationRechecks(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		other := newNetEscrowOrderingTestFixture(t, ctx)
		for _, kind := range []string{"payer_mismatch", "legacy_intent_present", "continuation_changed"} {
			for stopAt := 1; stopAt <= 2; stopAt++ {
				id := newContractExpiryRepairTestContract(t, ctx, f, false)
				scope := &contractExpiryRepairScope{contractId: id, expectedPayer: f.sourceNetworkId}
				var fresh *contractExpiryState
				contractExpiryContinuationTx(ctx, id, scope, func(tx server.PgTx) {
					var err error
					fresh, err = prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute))
					server.Raise(err)
				})
				before, _ := readContractExpiryTestSnapshot(t, ctx, id)
				step := 0
				scope.beforeTxForTest = func() {
					step++
					if step != stopAt {
						return
					}
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, id))
						switch kind {
						case "payer_mismatch":
							server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=$2 WHERE contract_id=$1`, id, other.sourceNetworkId))
						case "legacy_intent_present":
							server.Raise(queueLegacySettlementInTx(ctx, tx, id, ContractOutcomeSettled, false))
						case "continuation_changed":
							server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=close_time+interval '1 microsecond' WHERE contract_id=$1 AND party='source'`, id))
						}
					})
				}
				err := captureContractExpiryRepair(func() error { return continueContractExpiry(ctx, "[test]", fresh, scope) })
				if contractExpiryRepairErrorStatus(err) != kind || step != stopAt {
					t.Fatal("continuation did not recheck locked current state")
				}
				after, _ := readContractExpiryTestSnapshot(t, ctx, id)
				if !bytes.Equal(before, after) {
					t.Fatal("withdrawn continuation changed original proof")
				}
				server.Db(ctx, func(conn server.PgConn) {
					var intact bool
					server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND NOT dispute AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1) AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1) FROM transfer_contract WHERE contract_id=$1`, id).Scan(&intact))
					if !intact {
						t.Fatal("withdrawn continuation changed financial state")
					}
				})
				if kind != "legacy_intent_present" {
					reports := 1
					if stopAt == 2 {
						reports = 2
					}
					requireContractExpiryRepairUntouched(t, ctx, f, id, reports, true)
				}
			}
		}
		// Three refusal kinds at the report and settlement boundaries retain
		// six independent ten-byte reservations.
		if Testing_NetEscrowByteCount(ctx, f.balanceId) != 60 {
			t.Fatal("refused continuations released custody")
		}
	})
}

func TestContractExpiryRepairBusyAndCanceledPrefix(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		id := newContractExpiryRepairTestContract(t, ctx, f, false)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, id))
		request := ContractExpiryRepairRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: []server.Id{id}}
		preview, err := RepairContractExpiry(ctx, request)
		if err != nil || preview.Contracts[0].Status != "eligible" {
			t.Fatal("read-only preview waited for mutation ownership")
		}
		request.Apply = true
		result, err := RepairContractExpiry(ctx, request)
		if err != nil || result.Contracts[0].Status != "busy" || result.Contracts[0].ProofCommitted {
			t.Fatal("bounded mutation lock did not refuse busy custody")
		}
		requireContractExpiryRepairUntouched(t, ctx, f, id, 1, false)
		server.Raise(held.Rollback(ctx))
		scope := &contractExpiryRepairScope{contractId: id, expectedPayer: f.sourceNetworkId}
		var fresh *contractExpiryState
		contractExpiryContinuationTx(ctx, id, scope, func(tx server.PgTx) {
			var err error
			fresh, err = prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute))
			server.Raise(err)
		})
		before, _ := readContractExpiryTestSnapshot(t, ctx, id)
		held, err = conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		pid := contractLifecycleTestBackendPid(t, ctx, held)
		server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, id))
		stopped, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- captureContractExpiryRepair(func() error { return continueContractExpiry(stopped, "[test]", fresh, scope) })
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
		err = <-done
		joined = true
		if err == nil {
			t.Fatal("canceled in-flight continuation succeeded")
		}
		server.Raise(held.Rollback(ctx))
		after, _ := readContractExpiryTestSnapshot(t, ctx, id)
		if !bytes.Equal(before, after) {
			t.Fatal("cancellation lost committed original proof")
		}
		requireContractExpiryRepairUntouched(t, ctx, f, id, 1, true)
		result, err = RepairContractExpiry(ctx, request)
		if err != nil || result.Contracts[0].Status != "legacy_intent_present" {
			t.Fatal("retained proof did not resume through existing owner")
		}
		after, _ = readContractExpiryTestSnapshot(t, ctx, id)
		if !bytes.Equal(before, after) {
			t.Fatal("restart reconstructed synthetic proof")
		}
	})
}

// A real authenticated checkpoint can arrive after the proof commits. The
// adapter must not finalize that newly active checkpoint from stale state.
func TestContractExpiryRepairLateReportAndRollback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		// Preparation already retained the flag. The report transaction is
		// now the only boundary before finalization of this checkpoint.
		{
			id := newContractExpiryRepairTestContract(t, ctx, f, true)
			scope := &contractExpiryRepairScope{contractId: id, expectedPayer: f.sourceNetworkId}
			var fresh *contractExpiryState
			contractExpiryContinuationTx(ctx, id, scope, func(tx server.PgTx) {
				var err error
				fresh, err = prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute))
				server.Raise(err)
			})
			before, snapshot := readContractExpiryTestSnapshot(t, ctx, id)
			if snapshot.ByteCount != 3 {
				t.Fatal("wrong original proof fixture")
			}
			step := 0
			scope.beforeTxForTest = func() {
				step++
				if step == 1 {
					server.Raise(CloseContract(ctx, id, f.destinationId, 1, true))
				}
			}
			err := captureContractExpiryRepair(func() error { return continueContractExpiry(ctx, "[test]", fresh, scope) })
			if contractExpiryRepairErrorStatus(err) != "continuation_changed" || step != 1 {
				t.Fatal("new authenticated checkpoint was finalized")
			}
			after, _ := readContractExpiryTestSnapshot(t, ctx, id)
			if !bytes.Equal(before, after) {
				t.Fatal("late report rewrote retained lower bound")
			}
			requireContractExpiryRepairUntouched(t, ctx, f, id, 2, true)
			server.Db(ctx, func(conn server.PgConn) {
				var retained bool
				server.Raise(conn.QueryRow(ctx, `SELECT checkpoint AND used_transfer_byte_count=4 FROM contract_close WHERE contract_id=$1 AND party='destination'`, id).Scan(&retained))
				if !retained {
					t.Fatal("late report's amount or checkpoint changed")
				}
			})
		}
		id := newContractExpiryRepairTestContract(t, ctx, f, false)
		scope := &contractExpiryRepairScope{contractId: id, expectedPayer: f.sourceNetworkId}
		rollback := errors.New("test preparation rollback")
		err := captureContractExpiryRepair(func() error {
			contractExpiryContinuationTx(ctx, id, scope, func(tx server.PgTx) {
				_, err := prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute))
				server.Raise(err)
				server.Raise(rollback)
			})
			return nil
		})
		if !errors.Is(err, rollback) || scope.continuation != nil {
			t.Fatal("uncommitted proof became continuation authority")
		}
		requireContractExpiryRepairUntouched(t, ctx, f, id, 1, false)
		if Testing_NetEscrowByteCount(ctx, f.balanceId) != 20 {
			t.Fatal("withdrawal or rollback released reservation")
		}
	})
}
