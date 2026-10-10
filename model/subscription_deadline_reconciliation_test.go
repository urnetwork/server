// Deadline reconciliation tests exercise actual report, intent, grant and
// terminal transactions. No queued follow-up may be needed to finish accounting.
package model

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Provider totals and exact earnings must already exist when closure returns;
// a second owner or a callback cannot be required to preserve the payment.
func requireDeadlineProviderDurability(t testing.TB, ctx context.Context, networkId, contractId server.Id, bytes ByteCount) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var provided, payout ByteCount
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$1),0),
		 COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$2 AND network_id=$1),0)`, networkId, contractId).Scan(&provided, &payout))
		if provided != bytes || payout != bytes {
			t.Fatal("deadline earnings were not committed", provided, payout, bytes)
		}
	})
}

// Insufficient escrow, depleted credit and deleted grants previously left an
// accepted intent permanently open. Each fixture retains its original reports
// while the deadline pays the funded portion once and releases the reservation.
func TestDeadlineReconciliationClosesAccountingFailures(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, sample := range []struct {
			name                         string
			reported, available, charged ByteCount
			pending                      ByteCount
			missing, redis               bool
		}{
			{name: "underfunded intent", reported: 300, available: 1000, charged: 100},
			{name: "remaining balance", reported: 17, available: 7, charged: 7},
			{name: "empty balance", reported: 17, available: 0, charged: 0},
			{name: "missing grant", reported: 17, missing: true},
			{name: "redis remaining balance", reported: 17, available: 7, charged: 7, redis: true},
			{name: "balance reserved for accepted journal", reported: 17, available: 17, pending: 10, charged: 7},
		} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			var id server.Id
			if sample.redis {
				id = createRedisAdmissionTest(ctx, f, 100).ContractId
			} else {
				contract, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
				server.RunPosts(ctx, posts...)
				id = contract.ContractId
			}
			server.Raise(CloseContract(ctx, id, f.sourceId, sample.reported, true))
			server.Tx(ctx, func(tx server.PgTx) {
				if !sample.redis {
					server.Raise(queueLegacySettlementInTx(ctx, tx, id, ContractOutcomeSettled, true))
				}
				if sample.missing {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id=$1`, f.balanceId))
				} else {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count=$2 WHERE balance_id=$1`, f.balanceId, sample.available))
				}
				if sample.pending > 0 {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_debit_journal(contract_id,balance_id,shard,debit_byte_count)
						VALUES($1,$2,$3,$4)`, server.NewId(), f.balanceId, transferDebitShard(f.balanceId), sample.pending))
				}
			})
			// The model must honor its supplied retirement clock even when the
			// process clock has not reached it. Task eligibility is separate.
			deadline := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
			result, err := ReconcileContractAtDeadline(ctx, id, deadline)
			if err != nil || result == nil || result.Charged != sample.charged || result.Requested != sample.reported {
				t.Fatal("deadline did not reconcile funded usage", sample.name, result, err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var terminal, settled bool
				var payout, remaining, provided ByteCount
				var intents int
				server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL AND NOT dispute,
					(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=$1)
					FROM transfer_contract WHERE contract_id=$1`, id).Scan(&terminal, &intents))
				server.Raise(conn.QueryRow(ctx, `SELECT settled,payout_byte_count FROM transfer_escrow WHERE contract_id=$1`, id).Scan(&settled, &payout))
				server.Raise(conn.QueryRow(ctx, `SELECT COALESCE((SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1),0),
					COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$2),0)`, f.balanceId, f.destinationNetworkId).Scan(&remaining, &provided))
				if !terminal || !settled || intents != 0 || payout != sample.charged || provided != sample.charged || remaining != sample.available-sample.charged {
					t.Fatal("closure and finances did not commit together", sample.name, terminal, settled, intents, payout, remaining, provided)
				}
				var pending ByteCount
				server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(sum(debit_byte_count),0) FROM transfer_debit_journal
					WHERE balance_id=$1 AND NOT applied`, f.balanceId).Scan(&pending))
				if pending != sample.pending || remaining < pending {
					t.Fatal("deadline consumed another contract's accepted debit", sample.name, pending, remaining)
				}
			})
			before := readRedisExpiryRepairTestState(ctx, id)
			result, err = ReconcileContractAtDeadline(ctx, id, deadline)
			if err != nil || !result.AlreadyClosed || result.Charged != 0 || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
				t.Fatal("deadline replay changed financial state", sample.name, result, err)
			}
		}
	})
}

// Cancellation is an explicit retryable failure, not a successful handoff or a
// reason to discard the accepted financial intent.
func TestDeadlineReconciliationCancellationRollsBack(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		server.Raise(CloseContract(ctx, contract.ContractId, f.sourceId, 17, true))
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(queueLegacySettlementInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled, false))
		})
		before := readRedisExpiryRepairTestState(ctx, contract.ContractId)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if result, err := ReconcileContractAtDeadline(canceled, contract.ContractId, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil || result != nil || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, contract.ContractId)) {
			t.Fatal("canceled reconciliation acknowledged or modified an open contract", result, err)
		}
	})
}

// Immutable, already-retained bad evidence cannot be replaced, but it must not
// make expiration impossible. Retire the contract without rewriting that record.
func TestDeadlineReconciliationRetainsExcludedOpenEvidence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		id, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
		server.Raise(err)
		evidence := `{"version":1,"byte_count":0,"providers":[],"excluded_reason":"legacy_usage_unavailable","legacy_exclusion":{}}`
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET provider_usage=$2 WHERE contract_id=$1`, id, evidence))
		})
		result, err := ReconcileContractAtDeadline(ctx, id, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
		if err != nil || result == nil || result.Outcome != ContractOutcomeSettled {
			t.Fatal("retained excluded evidence kept expiration open", result, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var correct bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome='settled' AND usage_unverified AND provider_usage=$2::jsonb
				FROM transfer_contract WHERE contract_id=$1`, id, evidence).Scan(&correct))
			if !correct {
				t.Fatal("forced closure changed immutable evidence")
			}
		})
	})
}

// Free closes perform no provider total write. A held provider owner must not
// reject them; the old unconditional account ownership made sibling free
// expirations fail solely because they shared an endpoint network.
func TestDeadlineReconciliationFreeClosureAvoidsFinancialOwnership(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		id, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
		server.Raise(err)
		held, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			var failure error
			server.HandleError(func() {
				server.OwnedTx(ctx, accountBalanceOwnershipKeys([]server.Id{f.destinationNetworkId}), func(tx server.PgTx) {
					close(held)
					<-release
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { failure = err })
			done <- failure
		}()
		select {
		case <-held:
		case err := <-done:
			t.Fatal("could not hold provider account", err)
		}
		defer func() { close(release); server.Raise(<-done) }()
		result, err := ReconcileContractAtDeadline(ctx, id, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
		if err != nil || result == nil || result.Outcome != ContractOutcomeSettled {
			t.Fatal("free expiration competed for unrelated financial ownership", result, err)
		}
		if _, closed := GetContractClose(ctx, id); !closed {
			t.Fatal("free expiration acknowledged an open contract")
		}
	})
}

// The optional signed outcome cannot encode malformed historical reports. Its
// strict ordinary path used to roll back an otherwise valid forced close.
// Keep the original evidence and refuse that receipt, while committing closure.
func TestDeadlineReconciliationMalformedSignedEvidence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		for _, sample := range []struct {
			party ContractParty
			count ByteCount
		}{
			{party: ContractPartySource, count: -1},
			{party: ContractParty("invalid"), count: 17},
		} {
			id := f.contract(t)
			server.Tx(f.ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint)
					VALUES($1,$2,$3,true)`, id, sample.party, sample.count))
			})
			result, err := ReconcileContractAtDeadline(f.ctx, id, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
			if err != nil || result == nil || result.Outcome != ContractOutcomeSettled || result.Charged != 0 {
				t.Fatal("malformed optional evidence blocked expiration", sample, result, err)
			}
			server.Db(f.ctx, func(conn server.PgConn) {
				var terminal, reservation, signed bool
				var count ByteCount
				server.Raise(conn.QueryRow(f.ctx, `SELECT outcome IS NOT NULL,
					EXISTS(SELECT 1 FROM provider_work_reservation_original WHERE contract_id=$1),
					EXISTS(SELECT 1 FROM provider_work_outcome_original WHERE contract_id=$1),
					(SELECT used_transfer_byte_count FROM contract_close WHERE contract_id=$1 AND party=$2)
					FROM transfer_contract WHERE contract_id=$1`, id, sample.party).Scan(&terminal, &reservation, &signed, &count))
				if !terminal || !reservation || signed || count != sample.count {
					t.Fatal("forced close fabricated or discarded signed evidence", terminal, reservation, signed, count)
				}
			})
		}
	})
}

// Evidence refusal is limited to malformed data. A database failure during
// signed publication must roll back closure and leave the real cause retryable.
func TestDeadlineReconciliationSignedPublicationFailureRollsBack(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		id := f.contract(t)
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `CREATE FUNCTION synthetic_deadline_publication_failure() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'synthetic signed publication failure'; END $$;
			CREATE TRIGGER synthetic_deadline_publication_failure BEFORE INSERT ON provider_work_outcome_original
			FOR EACH ROW EXECUTE FUNCTION synthetic_deadline_publication_failure()`))
		})
		before := readRedisExpiryRepairTestState(f.ctx, id)
		result, err := ReconcileContractAtDeadline(f.ctx, id, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
		if err == nil || result != nil || !bytes.Equal(before, readRedisExpiryRepairTestState(f.ctx, id)) {
			t.Fatal("publication failure was swallowed or acknowledged", result, err)
		}
	})
}

// A deadline must use the same reconciliation policy through the payer task,
// its cohort fallback, and the global expiry scan. Pending retry times cannot
// hide expired contracts from the expiry owner.
func TestDeadlineReconciliationAdjacentOwners(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, path := range []string{"payer", "cohort", "expiry sweep"} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			count := 1
			if path == "cohort" {
				count = 2
			}
			ids := []server.Id{}
			for range count {
				contract, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
				server.RunPosts(ctx, posts...)
				ids = append(ids, contract.ContractId)
				server.Raise(CloseContract(ctx, contract.ContractId, f.sourceId, 17, true))
				server.Tx(ctx, func(tx server.PgTx) {
					server.Raise(queueLegacySettlementInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled, false))
				})
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=ANY($1)`, ids, server.NowUtc().Add(-time.Hour)))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count=7 WHERE balance_id=$1`, f.balanceId))
				due := server.NowUtc().Add(-time.Minute)
				if path == "expiry sweep" {
					due = server.NowUtc().Add(time.Hour)
				}
				server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=ANY($1)`, ids, due))
			})
			if path == "cohort" {
				attempts, err := flushLegacySettlementCohort(ctx, ids)
				if err != nil || len(attempts) == 0 || !attempts[0].fallback {
					t.Fatal("expired cohort bypassed reconciliation", attempts, err)
				}
			}
			if path == "expiry sweep" {
				completed, _, err := ForceCloseOpenContractIdsPage(ctx, server.NowUtc().Add(-5*time.Minute), 32, 1, 0, 0, nil)
				if err != nil || completed != 1 {
					t.Fatal("expiry scan skipped due accepted intent", completed, err)
				}
			} else {
				page, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, 8)
				if err != nil || page.Completed != count || page.Failed != 0 {
					t.Fatal("payer failed deadline reconciliation", path, page, err)
				}
			}
			server.Db(ctx, func(conn server.PgConn) {
				var terminal int
				var balance, charged, provided ByteCount
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome IS NOT NULL`, ids).Scan(&terminal))
				server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count,
					(SELECT sum(payout_byte_count) FROM transfer_escrow WHERE contract_id=ANY($2)),
					(SELECT provided_byte_count FROM account_balance WHERE network_id=$3)
					FROM transfer_balance WHERE balance_id=$1`, f.balanceId, ids, f.destinationNetworkId).Scan(&balance, &charged, &provided))
				if terminal != count || balance != 0 || charged != 7 || provided != 7 {
					t.Fatal("adjacent owner overcharged or left a contract open", path, terminal, balance, charged, provided)
				}
			})
			if remaining := Testing_NetEscrowByteCount(ctx, f.balanceId); remaining != 0 {
				t.Fatal("deadline retained a spent reservation", path, remaining)
			}
		}
	})
}
