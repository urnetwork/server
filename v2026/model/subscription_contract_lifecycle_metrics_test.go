// Lifecycle counters observe first committed insertion and first committed
// terminal transition. Financial state remains an independent test oracle.
package model

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

// The two counters have separate snapshot boundaries. Read only after the
// test's real owners have joined; never replace or reset the global collectors.
type contractLifecycleCounterTestSnapshot struct {
	opened server.TxCommitCounterSnapshot
	closed server.TxCommitCounterSnapshot
}

func readContractLifecycleCounterTestSnapshot(t testing.TB) contractLifecycleCounterTestSnapshot {
	t.Helper()
	snapshot := contractLifecycleCounterTestSnapshot{
		opened: contractOpenedCounter.Snapshot(),
		closed: contractClosedCounter.Snapshot(),
	}
	if !snapshot.opened.Stable || !snapshot.closed.Stable || snapshot.opened.Overflow || snapshot.closed.Overflow {
		t.Fatal("lifecycle counter snapshot has incomplete coverage")
	}
	return snapshot
}

// Check uncertainty and unsupported-owner deltas as well as confirmed events.
func requireContractLifecycleCounterTestDelta(t testing.TB, before contractLifecycleCounterTestSnapshot, opened, closed uint64) {
	t.Helper()
	after := readContractLifecycleCounterTestSnapshot(t)
	for _, check := range []struct {
		name   string
		before server.TxCommitCounterSnapshot
		after  server.TxCommitCounterSnapshot
		want   uint64
	}{
		{name: "opened", before: before.opened, after: after.opened, want: opened},
		{name: "closed", before: before.closed, after: after.closed, want: closed},
	} {
		if check.after.Confirmed < check.before.Confirmed || check.after.Confirmed-check.before.Confirmed != check.want ||
			check.after.Uncertain != check.before.Uncertain || check.after.Untracked != check.before.Untracked {
			t.Fatalf("%s counter before=%+v after=%+v want delta=%d", check.name, check.before, check.after, check.want)
		}
	}
}

// An existing reservation lookup and duplicate SQL custody are not new opens.
// Journal application after a Redis terminal commit is not another close.
func TestContractLifecycleCountersRedisCreateReuseCloseReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		before := readContractLifecycleCounterTestSnapshot(t)
		contract := createRedisAdmissionTest(ctx, fixture, 100)
		requireContractLifecycleCounterTestDelta(t, before, 1, 0)
		existing := GetOpenTransferEscrowsOrderedByPriorityCreateTime(ctx, fixture.sourceId, fixture.destinationId, 100)
		if len(existing) != 1 || existing[0].ContractId != contract.ContractId {
			t.Fatal("open-reservation reuse did not return the existing contract")
		}
		var duplicateErr error
		server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				admission := &redisContractAdmission{contractId: contract.ContractId}
				_, _, err := createRedisTransferEscrowInTx(ctx, tx, admission,
					fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId,
					fixture.sourceNetworkId, 100, nil)
				server.Raise(err)
			}, server.TxReadCommitted, server.OptNoRetry())
		}, func(err error) { duplicateErr = err })
		if duplicateErr == nil {
			t.Fatal("duplicate SQL custody was not refused")
		}
		if Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 100 {
			t.Fatal("reuse or duplicate changed the reservation")
		}
		server.Raise(CloseContract(ctx, contract.ContractId, fixture.sourceId, 11, false))
		requireContractLifecycleCounterTestDelta(t, before, 1, 0)
		server.Raise(CloseContract(ctx, contract.ContractId, fixture.destinationId, 11, false))
		requireContractLifecycleCounterTestDelta(t, before, 1, 1)
		credit, pending, applied := asyncDebitTestState(t, ctx, fixture.balanceId)
		if credit != 1000 || pending != 1 || applied != 0 {
			t.Fatal("terminal counter was confused with completed payer debit", credit, pending, applied)
		}
		projectLegacyProviderTotalsForTest(t, ctx)
		requireLegacyProviderDurability(t, ctx, fixture, contract.ContractId, 11)
		count, released, busy, err := flushTransferDebitBalance(ctx, fixture.balanceId)
		if err != nil || busy || count != 1 || released != 1 {
			t.Fatal("actual debit owner did not complete", count, released, busy, err)
		}
		closed, err := settleContract(ctx, contract.ContractId)
		if err != nil || closed {
			t.Fatal("terminal replay claimed another transition", closed, err)
		}
		count, released, busy, err = flushTransferDebitBalance(ctx, fixture.balanceId)
		credit, pending, applied = asyncDebitTestState(t, ctx, fixture.balanceId)
		if err != nil || busy || count != 0 || released != 0 || credit != 989 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 0 {
			t.Fatal("replay changed settled debit or reservation", credit, pending, applied, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var contracts, terminal int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE outcome=$2)
				FROM transfer_contract WHERE contract_id=$1`, contract.ContractId, ContractOutcomeSettled).Scan(&contracts, &terminal))
			if contracts != 1 || terminal != 1 {
				t.Fatal("lifecycle counters disagree with durable contract state")
			}
		})
		requireContractLifecycleCounterTestDelta(t, before, 1, 1)
	})
}

// A queued legacy intent is still nonterminal. Run real asynchronous mirror
// and provider owners before using their state as the conservation oracle.
func TestContractLifecycleCountersLegacyCreateFinancialCloseReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		before := readContractLifecycleCounterTestSnapshot(t)
		fixture, id := legacySettlementTestIntent(t, ctx)
		requireContractLifecycleCounterTestDelta(t, before, 1, 0)
		requireLegacySettlementTestState(t, ctx, fixture, id, true, false, 1000, 100)
		completed, busy, busyGate, err := flushLegacySettlement(ctx, id)
		if err != nil || !completed || busy {
			t.Fatal("real legacy owner did not complete", completed, busy, err)
		}
		requireContractLifecycleCounterTestDelta(t, before, 1, 1)
		legacyMirrorTestRun(t, ctx, fixture.balanceId)
		projectLegacyProviderTotalsForTest(t, ctx)
		requireLegacySettlementTestState(t, ctx, fixture, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, fixture, id, 11)
		// The exact deleted intent returns the existing busy-or-gone result;
		// financial idempotence is proved by the unchanged state and counters.
		completed, busy, busyGate, err = flushLegacySettlement(ctx, id)
		if err != nil || completed || !busy || busyGate != legacySettlementBusyIntent {
			t.Fatal("terminal replay lost its missing-intent disposition", completed, busy, busyGate, err)
		}
		legacyMirrorTestRun(t, ctx, fixture.balanceId)
		projectLegacyProviderTotalsForTest(t, ctx)
		requireLegacySettlementTestState(t, ctx, fixture, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, fixture, id, 11)
		requireContractLifecycleCounterTestDelta(t, before, 1, 1)
	})
}

// Closing a no-escrow contract changes lifecycle without financial work.
func TestContractLifecycleCountersNoEscrowCreateCloseReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		before := readContractLifecycleCounterTestSnapshot(t)
		id, err := CreateContractNoEscrow(ctx, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId, 100)
		server.Raise(err)
		server.Raise(CloseContract(ctx, id, fixture.sourceId, 11, false))
		requireContractLifecycleCounterTestDelta(t, before, 1, 0)
		server.Raise(CloseContract(ctx, id, fixture.destinationId, 11, false))
		closed, err := settleContract(ctx, id)
		if err != nil || closed {
			t.Fatal("no-escrow replay repeated the terminal transition", closed, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var terminal bool
			var escrowRows, journalRows, sweepRows int
			var credit ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT outcome=$3,
				(SELECT count(*) FROM transfer_escrow WHERE contract_id=$1),
				(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=$1),
				(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=$1),
				(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)
				FROM transfer_contract WHERE contract_id=$1`, id, fixture.balanceId, ContractOutcomeSettled).Scan(&terminal, &escrowRows, &journalRows, &sweepRows, &credit))
			if !terminal || escrowRows != 0 || journalRows != 0 || sweepRows != 0 || credit != 1000 {
				t.Fatal("no-escrow completion invented financial work")
			}
		})
		requireContractLifecycleCounterTestDelta(t, before, 1, 1)
	})
}

// Roll back after each real transition, using the server-owned transaction
// wrapper so registration coverage is meaningful, rather than a raw test tx.
func TestContractLifecycleCountersRollbackDiscardsOpenAndClose(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		before := readContractLifecycleCounterTestSnapshot(t)
		rollbackErr := errors.New("synthetic lifecycle rollback")
		var observedErr error
		var rolledBackId server.Id
		server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				contract, _, err := createTransferEscrowInTx(ctx, tx,
					fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId,
					fixture.sourceNetworkId, 100, nil)
				server.Raise(err)
				rolledBackId = contract.ContractId
				server.Raise(rollbackErr)
			}, server.TxReadCommitted, server.OptNoRetry())
		}, func(err error) { observedErr = err })
		if !errors.Is(observedErr, rollbackErr) {
			t.Fatal("creation did not reach forced rollback", observedErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var contracts, reservations int
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_contract WHERE contract_id=$1),
				(SELECT count(*) FROM transfer_escrow WHERE contract_id=$1)`, rolledBackId).Scan(&contracts, &reservations))
			if contracts != 0 || reservations != 0 {
				t.Fatal("rolled-back creation retained custody")
			}
		})
		requireContractLifecycleCounterTestDelta(t, before, 0, 0)
		contract := createRedisAdmissionTest(ctx, fixture, 100)
		observedErr = nil
		server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
					(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
					VALUES($1,'source',11,now(),false),($1,'destination',11,now(),false)`, contract.ContractId))
				_, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
				server.Raise(err)
				if !closed {
					t.Fatal("rollback did not reach the real outcome owner")
				}
				server.Raise(rollbackErr)
			}, server.TxReadCommitted, server.OptNoRetry())
		}, func(err error) { observedErr = err })
		if !errors.Is(observedErr, rollbackErr) {
			t.Fatal("outcome did not reach forced rollback", observedErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var terminal bool
			var reports, sweeps int
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL,
				(SELECT count(*) FROM contract_close WHERE contract_id=$1),
				(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=$1)
				FROM transfer_contract WHERE contract_id=$1`, contract.ContractId).Scan(&terminal, &reports, &sweeps))
			if terminal || reports != 0 || sweeps != 0 {
				t.Fatal("rolled-back outcome retained accounting")
			}
		})
		credit, pending, applied := asyncDebitTestState(t, ctx, fixture.balanceId)
		if credit != 1000 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 100 {
			t.Fatal("rollback changed payer credit or reservation")
		}
		requireLegacyProviderDurability(t, ctx, fixture, contract.ContractId, 0)
		requireContractLifecycleCounterTestDelta(t, before, 1, 0)
	})
}

// Quarantine is terminal lifecycle, while its typed error and unchanged money
// explicitly retain the financial rejection. A later sweep cannot close twice.
func TestContractLifecycleCountersMalformedQuarantineClosesOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		before := readContractLifecycleCounterTestSnapshot(t)
		const escrow = ByteCount(32 * 1024 * 1024)
		fixture := newForceCloseDisputeFixture(t, ctx, true, true, 2*escrow, 2*escrow, escrow)
		original := fixture.state(t, ctx)
		requireContractLifecycleCounterTestDelta(t, before, 1, 0)
		selected, err := ForceCloseOpenContractIds(ctx, fixture.cutoff, 10, 1, 1, 0)
		var accounting *ForceCloseAccountingError
		if selected != 1 || !errors.As(err, &accounting) || !errors.Is(err, errContractInsufficientEscrow) ||
			accounting.VerifiedCloseCount() != 1 || accounting.QuarantinedAccountingRejectionCount() != 1 {
			t.Fatal("quarantine lost its explicit accounting rejection", selected, err)
		}
		quarantined := fixture.state(t, ctx)
		if quarantined.outcome != ContractOutcomeSettled || quarantined.dispute || quarantined.open || quarantined.streamFound ||
			quarantined.escrowSettled || quarantined.escrowPayoutByteCount != original.escrowPayoutByteCount ||
			quarantined.payerBalanceByteCount != original.payerBalanceByteCount ||
			quarantined.providerEarnedByteCount != original.providerEarnedByteCount ||
			quarantined.providerPayoutByteCount != original.providerPayoutByteCount || quarantined.netEscrowByteCount != 0 {
			t.Fatal("terminal quarantine changed its no-payout accounting policy")
		}
		requireContractLifecycleCounterTestDelta(t, before, 1, 1)
		selected, err = ForceCloseOpenContractIds(ctx, fixture.cutoff, 10, 1, 1, 0)
		if selected != 0 || err != nil || fixture.state(t, ctx) != quarantined {
			t.Fatal("replayed sweep repeated quarantine or changed accounting", selected, err)
		}
		requireContractLifecycleCounterTestDelta(t, before, 1, 1)
	})
}

// Both process counters are exported even when idle, without event, backend,
// customer or contract labels. Scraping does not query financial state.
func TestContractLifecycleCountersExportWithoutLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(contractOpenedMetric, contractClosedMetric)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 2 {
		t.Fatal("expected exactly two lifecycle counter families")
	}
	expected := map[string]uint64{
		"urnetwork_contract_opened_total": contractOpenedCounter.ConfirmedCount(),
		"urnetwork_contract_closed_total": contractClosedCounter.ConfirmedCount(),
	}
	for _, family := range families {
		value, ok := expected[family.GetName()]
		if !ok || len(family.Metric) != 1 || len(family.Metric[0].Label) != 0 || family.Metric[0].Counter == nil || family.Metric[0].Counter.GetValue() != float64(value) {
			t.Fatal("lifecycle export lost monotonic counter type, zero presence or no-label shape")
		}
		delete(expected, family.GetName())
	}
	if len(expected) != 0 {
		t.Fatal("a lifecycle counter is missing")
	}
}
