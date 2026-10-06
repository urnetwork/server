// Real Redis admission and PostgreSQL owners exercise retained usage and exact
// transaction boundaries. All identities and accounting values are synthetic.
package model

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The report matrix covers every observed shape without copying any customer
// values. Positive one-sided usage must still be charged, never zeroed.
func newRedisExpiryRepairTestContract(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, shape string) server.Id {
	t.Helper()
	escrow := createRedisAdmissionTest(ctx, f, 100)
	switch shape {
	case "reportless":
	case "source_zero":
		server.Raise(CloseContract(ctx, escrow.ContractId, f.sourceId, 0, false))
	case "source_positive":
		server.Raise(CloseContract(ctx, escrow.ContractId, f.sourceId, 17, false))
	case "destination_checkpoint":
		server.Raise(CloseContract(ctx, escrow.ContractId, f.destinationId, 11, true))
	case "positive_checkpoint":
		server.Raise(CloseContract(ctx, escrow.ContractId, f.sourceId, 17, false))
		server.Raise(CloseContract(ctx, escrow.ContractId, f.destinationId, 11, true))
	case "insufficient":
		server.Raise(CloseContract(ctx, escrow.ContractId, f.sourceId, 111, false))
		server.Raise(CloseContract(ctx, escrow.ContractId, f.destinationId, 111, true))
	default:
		t.Fatal("unknown synthetic report shape")
	}
	server.Tx(ctx, func(tx server.PgTx) {
		old := server.NowUtc().Add(-time.Hour)
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, escrow.ContractId, old))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, escrow.ContractId, old))
	})
	return escrow.ContractId
}

// Before/after equality includes the complete synthetic contract, reports,
// escrow tuples, and journals. It never prints the captured values.
func readRedisExpiryRepairTestState(ctx context.Context, contractId server.Id) []byte {
	var state []byte
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_build_object(
			'contract',to_jsonb(c),
			'reports',(SELECT jsonb_agg(to_jsonb(r) ORDER BY party) FROM contract_close r WHERE contract_id=$1),
			'escrows',(SELECT jsonb_agg(to_jsonb(e) ORDER BY balance_id) FROM transfer_escrow e WHERE contract_id=$1),
			'journals',(SELECT jsonb_agg(to_jsonb(j) ORDER BY balance_id) FROM transfer_debit_journal j WHERE contract_id=$1))
			FROM transfer_contract c WHERE contract_id=$1`, contractId).Scan(&state))
	})
	return state
}

// Credit and active-neighbor reservation conservation are independent of the
// terminal observation. Only the existing debit worker changes grant credit.
func requireRedisExpiryRepairTestCredit(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, credit, reserved ByteCount) {
	t.Helper()
	var got ByteCount
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&got))
	})
	if got != credit || Testing_NetEscrowByteCount(ctx, f.balanceId) != reserved {
		t.Fatal("grant credit or intact active-neighbor reservation was not conserved")
	}
}

// Pins the unchanged legacy refusal before exercising the new scoped owner.
// A RED run substitutes the legacy entry point for the single new call below.
func TestRedisContractExpiryRepairObservedShapesConserve(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, test := range []struct {
			shape       string
			consumed    ByteCount
			provider    ByteCount
			reportCount int
		}{
			{"reportless", 0, 0, 0}, {"source_zero", 0, 0, 1},
			{"source_positive", 17, 0, 1}, {"destination_checkpoint", 11, 0, 1},
			{"positive_checkpoint", 14, 11, 2},
		} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			id := newRedisExpiryRepairTestContract(t, ctx, f, test.shape)
			neighbor := createRedisAdmissionTest(ctx, f, 37)
			request := ContractExpiryRepairRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: []server.Id{id}}
			before, neighborBefore := readRedisExpiryRepairTestState(ctx, id), readRedisExpiryRepairTestState(ctx, neighbor.ContractId)
			legacy, err := RepairContractExpiry(ctx, request)
			if err != nil || legacy.Contracts[0].Status != "reservation_mode_changed" {
				t.Fatal("unchanged legacy scope no longer rejects Redis custody")
			}
			preview, err := RepairRedisContractExpiry(ctx, request)
			if err != nil || preview.Contracts[0].Status != "eligible" || preview.Contracts[0].ProofCommitted ||
				!bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
				t.Fatal("Redis preview is not an eligible read-only observation")
			}
			requireRedisExpiryRepairTestCredit(t, ctx, f, 1000, 137)
			request.Apply = true
			applied, err := RepairRedisContractExpiry(ctx, request)
			if err != nil || applied.Contracts[0].Status != "terminal" || !applied.Contracts[0].ProofCommitted {
				t.Fatal("quiet Redis scope did not complete through the ordinary owner")
			}
			proof, snapshot := readContractExpiryTestSnapshot(t, ctx, id)
			if snapshot.Expiry == nil || len(snapshot.Expiry.Reports) != test.reportCount || snapshot.ByteCount != test.provider {
				t.Fatal("synthetic peer report replaced original provider usage")
			}
			server.Db(ctx, func(conn server.PgConn) {
				var debit ByteCount
				var pending bool
				server.Raise(conn.QueryRow(ctx, `SELECT debit_byte_count,NOT applied FROM transfer_debit_journal WHERE contract_id=$1 AND balance_id=$2`, id, f.balanceId).Scan(&debit, &pending))
				if debit != test.consumed || !pending {
					t.Fatal("ordinary mean consumption was changed or premature credit writeback occurred")
				}
			})
			requireRedisExpiryRepairTestCredit(t, ctx, f, 1000, 37+test.consumed)
			replayed, err := RepairRedisContractExpiry(ctx, request)
			if err != nil || replayed.Contracts[0].Status != "terminal" || replayed.Contracts[0].ProofCommitted {
				t.Fatal("terminal replay attempted another closure")
			}
			_, _, _, err = flushTransferDebitBalance(ctx, f.balanceId)
			server.Raise(err)
			_, _, _, err = flushTransferDebitBalance(ctx, f.balanceId)
			server.Raise(err)
			requireRedisExpiryRepairTestCredit(t, ctx, f, 1000-test.consumed, 37)
			after, _ := readContractExpiryTestSnapshot(t, ctx, id)
			if !bytes.Equal(proof, after) || !bytes.Equal(neighborBefore, readRedisExpiryRepairTestState(ctx, neighbor.ContractId)) {
				t.Fatal("closure or debit replay altered proof or healthy neighbor")
			}
		}
	})
}

// Reversed companion billing keeps the original payer and usage provider,
// while a second payer remains unchanged.
func TestRedisContractExpiryRepairCompanionAndPayer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, other := newNetEscrowOrderingTestFixture(t, ctx), newNetEscrowOrderingTestFixture(t, ctx)
		origin := createRedisAdmissionTest(ctx, f, 100)
		companion, err := CreateCompanionTransferEscrow(ctx, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, f.sourceId, 100, time.Hour)
		server.Raise(err)
		if companion == nil || companion.CompanionContractId == nil || *companion.CompanionContractId != origin.ContractId {
			t.Fatal("synthetic companion creation failed")
		}
		server.Raise(CloseContract(ctx, companion.ContractId, f.destinationId, 17, false))
		server.Raise(CloseContract(ctx, companion.ContractId, f.sourceId, 11, true))
		server.Tx(ctx, func(tx server.PgTx) {
			old := server.NowUtc().Add(-time.Hour)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, companion.ContractId, old))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, companion.ContractId, old))
		})
		before := readRedisExpiryRepairTestState(ctx, companion.ContractId)
		wrong, err := RepairRedisContractExpiry(ctx, ContractExpiryRepairRequest{ExpectedPayerNetworkId: other.sourceNetworkId, ContractIds: []server.Id{companion.ContractId}, Apply: true})
		if err != nil || wrong.Contracts[0].Status != "payer_mismatch" || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, companion.ContractId)) {
			t.Fatal("foreign payer was admitted")
		}
		result, err := RepairRedisContractExpiry(ctx, ContractExpiryRepairRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: []server.Id{companion.ContractId}, Apply: true})
		if err != nil || result.Contracts[0].Status != "terminal" {
			t.Fatal("companion expiry failed")
		}
		_, proof := readContractExpiryTestSnapshot(t, ctx, companion.ContractId)
		if proof.ByteCount != 11 || len(proof.Providers) != 1 || proof.Providers[0].ClientId != f.destinationId || proof.Providers[0].NetworkId != f.destinationNetworkId {
			t.Fatal("companion usage was attributed to the payer or current direction")
		}
		requireRedisExpiryRepairTestCredit(t, ctx, f, 1000, 114)
		requireRedisExpiryRepairTestCredit(t, ctx, other, 1000, 0)
		_, _, _, err = flushTransferDebitBalance(ctx, f.balanceId)
		server.Raise(err)
		requireRedisExpiryRepairTestCredit(t, ctx, f, 986, 100)
	})
}

// Every unsafe admission refuses before proof or report mutation. The journal
// case deliberately has a balance outside this contract's escrow set.
func TestRedisContractExpiryRepairAdmissionRefusals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, test := range []struct{ change, status string }{
			{"recent", "recent_report"}, {"dispute", "disputed"}, {"intent", "legacy_intent_present"},
			{"journal", "debit_present"}, {"legacy", "reservation_mode_changed"},
			{"settled", "reservation_mode_changed"}, {"absent", "reservation_mode_changed"},
			{"foreign", "custody_invalid"}, {"stream", "stream_scope_unsupported"},
			{"extender", "extender_scope_unsupported"}, {"cap", "escrow_scope_capped"},
		} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			id := newRedisExpiryRepairTestContract(t, ctx, f, "source_zero")
			if test.change == "cap" {
				for range contractRedisExpiryRepairMaxEscrows {
					AddBasicTransferBalance(ctx, f.sourceNetworkId, 10, server.NowUtc(), server.NowUtc().Add(time.Hour))
				}
			}
			server.Tx(ctx, func(tx server.PgTx) {
				switch test.change {
				case "recent":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, id, server.NowUtc()))
				case "dispute":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`, id))
				case "intent":
					server.RaisePgResult(tx.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, id))
					server.Raise(queueLegacySettlementInTx(ctx, tx, id, ContractOutcomeSettled, false))
				case "journal":
					journalBalanceId := server.NewId()
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_debit_journal(contract_id,balance_id,debit_byte_count,shard) VALUES($1,$2,1,$3)`, id, journalBalanceId, transferDebitShard(journalBalanceId)))
				case "legacy":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET redis_reserved=false WHERE contract_id=$1`, id))
				case "settled":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET settled=true WHERE contract_id=$1`, id))
				case "absent":
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_escrow WHERE contract_id=$1`, id))
				case "foreign":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET network_id=$2 WHERE balance_id=$1`, f.balanceId, f.destinationNetworkId))
				case "stream":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET stream_id=$2 WHERE contract_id=$1`, id, server.NewId()))
				case "extender":
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_extender(contract_id,extender_id,party,client_id,network_id,create_time) VALUES($1,$2,'source',$3,$4,clock_timestamp())`, id, server.NewId(), f.destinationId, f.destinationNetworkId))
				case "cap":
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count,redis_reserved) SELECT $1,balance_id,1,true FROM transfer_balance WHERE network_id=$2 AND balance_id<>$3`, id, f.sourceNetworkId, f.balanceId))
				}
			})
			before := readRedisExpiryRepairTestState(ctx, id)
			result, err := RepairRedisContractExpiry(ctx, ContractExpiryRepairRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: []server.Id{id}, Apply: true})
			if err != nil || result.Contracts[0].Status != test.status || result.Contracts[0].ProofCommitted || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
				t.Fatal("unsafe admission mutated state or misclassified refusal: " + test.change)
			}
		}
	})
}

// A real authenticated checkpoint and a custody edit are injected at exact
// acknowledged transaction boundaries, without sleeps or scheduler polling.
func TestRedisContractExpiryRepairContinuationFences(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, test := range []struct{ change, status string }{
			{"report", "continuation_changed"}, {"escrow", "custody_changed"},
			{"origin", "custody_changed"}, {"companion", "custody_changed"},
		} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			id := newRedisExpiryRepairTestContract(t, ctx, f, "positive_checkpoint")
			scope := &contractExpiryRepairScope{contractId: id, expectedPayer: f.sourceNetworkId, redis: &contractRedisExpiryRepairScope{cutoff: server.NowUtc().Add(-5 * time.Minute)}}
			var fresh *contractExpiryState
			contractExpiryContinuationTx(ctx, id, scope, func(tx server.PgTx) {
				var err error
				fresh, err = prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute))
				server.Raise(err)
			})
			before, _ := readContractExpiryTestSnapshot(t, ctx, id)
			calls := 0
			scope.beforeTxForTest = func() {
				calls++
				if calls != 1 {
					t.Fatal("refusal allowed a later continuation")
				}
				if test.change == "report" {
					server.Raise(CloseContract(ctx, id, f.destinationId, 1, true))
					return
				}
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, id))
					switch test.change {
					case "escrow":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=balance_byte_count+1 WHERE contract_id=$1`, id))
					case "origin":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=NOT usage_origin_is_source WHERE contract_id=$1`, id))
					case "companion":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET companion_contract_id=$2 WHERE contract_id=$1`, id, server.NewId()))
					}
				})
			}
			err := captureContractExpiryRepair(func() error { return continueContractExpiry(ctx, "[synthetic]", fresh, scope) })
			if err == nil || contractExpiryRepairErrorStatus(err) != test.status || calls != 1 {
				t.Fatal("changed live reports or custody were used by a stale continuation")
			}
			after, _ := readContractExpiryTestSnapshot(t, ctx, id)
			if !bytes.Equal(before, after) {
				t.Fatal("withdrawal rewrote retained original proof")
			}
			requireRedisExpiryRepairTestCredit(t, ctx, f, 1000, 100)
			if test.change == "report" {
				state := readRedisExpiryRepairTestState(ctx, id)
				restarted, err := RepairRedisContractExpiry(ctx, ContractExpiryRepairRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: []server.Id{id}, Apply: true})
				if err != nil || restarted.Contracts[0].Status != "recent_report" || restarted.Contracts[0].ProofCommitted || !bytes.Equal(state, readRedisExpiryRepairTestState(ctx, id)) {
					t.Fatal("new request used sticky proof to bypass a fresh report")
				}
			}
		}
	})
}

// A rollback cannot publish custody authority. A lost final commit ACK keeps
// durable consumption and conservative reservation until the existing worker.
func TestRedisContractExpiryRepairRollbackLostAckAndCancellation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		id := newRedisExpiryRepairTestContract(t, ctx, f, "positive_checkpoint")
		_ = createRedisAdmissionTest(ctx, f, 37)
		scope := &contractExpiryRepairScope{contractId: id, expectedPayer: f.sourceNetworkId, redis: &contractRedisExpiryRepairScope{cutoff: server.NowUtc().Add(-5 * time.Minute)}}
		before := readRedisExpiryRepairTestState(ctx, id)
		rollback := errors.New("synthetic precommit rollback")
		err := captureContractExpiryRepair(func() error {
			contractExpiryContinuationTx(ctx, id, scope, func(tx server.PgTx) {
				_, err := prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute))
				server.Raise(err)
				server.Raise(rollback)
			})
			return nil
		})
		if !errors.Is(err, rollback) || scope.continuation != nil || scope.redis.custody != nil || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
			t.Fatal("rollback published proof or continuation authority")
		}
		var fresh *contractExpiryState
		contractExpiryContinuationTx(ctx, id, scope, func(tx server.PgTx) {
			var err error
			fresh, err = prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute))
			server.Raise(err)
		})
		proof, _ := readContractExpiryTestSnapshot(t, ctx, id)
		canceled, cancel := context.WithCancel(ctx)
		scope.beforeTxForTest = cancel
		err = captureContractExpiryRepair(func() error { return continueContractExpiry(canceled, "[synthetic]", fresh, scope) })
		cancel()
		if err == nil {
			t.Fatal("canceled continuation wrote reports")
		}
		requireRedisExpiryRepairTestCredit(t, ctx, f, 1000, 137)
		scope.beforeTxForTest = nil
		commits := 0
		lost := errors.New("synthetic lost commit acknowledgement")
		scope.afterCommitForTest = func() {
			commits++
			if commits == 2 {
				panic(lost)
			}
		}
		err = captureContractExpiryRepair(func() error { return continueContractExpiry(ctx, "[synthetic]", fresh, scope) })
		if !errors.Is(err, lost) || commits != 2 {
			t.Fatal("lost acknowledgement boundary was not exercised")
		}
		requireRedisExpiryRepairTestCredit(t, ctx, f, 1000, 137)
		replay, err := RepairRedisContractExpiry(ctx, ContractExpiryRepairRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: []server.Id{id}, Apply: true})
		if err != nil || replay.Contracts[0].Status != "terminal" || replay.Contracts[0].ProofCommitted {
			t.Fatal("ambiguous terminal commit was retried as fresh work")
		}
		_, _, _, err = flushTransferDebitBalance(ctx, f.balanceId)
		server.Raise(err)
		requireRedisExpiryRepairTestCredit(t, ctx, f, 986, 37)
		after, _ := readContractExpiryTestSnapshot(t, ctx, id)
		if !bytes.Equal(proof, after) {
			t.Fatal("recovery changed original provider proof")
		}
	})
}

// Accounting refusal retains every report and reservation. There is no
// quarantine, refund, capacity substitution, or usage truncation in this adapter.
func TestRedisContractExpiryRepairAccountingRefusal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		id := newRedisExpiryRepairTestContract(t, ctx, f, "insufficient")
		result, err := RepairRedisContractExpiry(ctx, ContractExpiryRepairRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: []server.Id{id}, Apply: true})
		if err != nil || result.Contracts[0].Status != "failed" || !result.Contracts[0].ProofCommitted {
			t.Fatal("insufficient custody was treated as financial completion")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var intact bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND NOT dispute
				AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
				AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
				AND (SELECT count(*)=2 AND bool_and(used_transfer_byte_count=111) FROM contract_close WHERE contract_id=$1)
				FROM transfer_contract WHERE contract_id=$1`, id).Scan(&intact))
			if !intact {
				t.Fatal("accounting refusal discarded consumption or claimed outcome")
			}
		})
		requireRedisExpiryRepairTestCredit(t, ctx, f, 1000, 100)
	})
}

// Redis settlement must not take the healthy neighbor's shared grant debit
// lock. The held lock is the explicit ordering witness, not elapsed time.
func TestRedisContractExpiryRepairHeldGrantNeighbor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		id := newRedisExpiryRepairTestContract(t, ctx, f, "positive_checkpoint")
		neighbor := createRedisAdmissionTest(ctx, f, 37)
		before := readRedisExpiryRepairTestState(ctx, neighbor.ContractId)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balanceId))
		result, err := RepairRedisContractExpiry(ctx, ContractExpiryRepairRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: []server.Id{id}, Apply: true})
		if err != nil || result.Contracts[0].Status != "terminal" {
			t.Fatal("Redis expiry waited for unrelated grant debit ownership")
		}
		if !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, neighbor.ContractId)) {
			t.Fatal("held-grant neighbor was altered")
		}
		server.Raise(held.Rollback(ctx))
		requireRedisExpiryRepairTestCredit(t, ctx, f, 1000, 51)
	})
}
