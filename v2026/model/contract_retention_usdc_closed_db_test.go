// USDC payouts ended (owner decisions 2026-11-12 and 2026-11-13: "settle closed
// rows, let's close out usdc entirely"). The close-out canceled every pending
// payment and journaled it with a usdc_payout_closed event, and a safely
// canceled payment, closed or not, is forfeited (owner decision 2026-11-13).
// Retention counts such a payment settled, as a completed one, in every reaper
// that reads payments: the contract horizon reaper, the due-contract delete
// pass and the straggler stamp. Before, a closed or safely canceled payment
// kept its contracts until the 300-day straggler horizon, and a closed keyed
// one (paid elsewhere) forever. A contract of an obligation that is still open
// stays.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A payment row of the shape, and with the close-out's closure event when
// closed.
func insertUsdcClosedTestPayment(t testing.TB, ctx context.Context, networkId server.Id, shape horizonTestPayment, closed bool) server.Id {
	t.Helper()
	paymentId := insertHorizonTestPayment(t, ctx, networkId, shape)
	if closed {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_account_payment (event_id, payment_id, event_type, event_details)
				VALUES (`+server.UsdcPayoutClosedEventIdSql("$1::uuid")+`, $1, $2, '{"reason":"wallet_less"}')`,
				paymentId, server.UsdcPayoutClosedEventType))
		}, server.TxReadCommitted, server.OptNoRetry())
	}
	return paymentId
}

// The payment shapes of the close-out, of safely canceled payments and of the
// obligations still open. Each closed or safely canceled one is settled; each
// open one holds.
type usdcClosedTestPayments struct {
	// closed by the close-out, never sent (wallet_less, dust, review_held,
	// other)
	closedUnsent server.Id
	// closed by the close-out as paid by another Circle transfer, keyed
	closedPaidElsewhere server.Id
	pending             server.Id
	// closed, then made pending again by a writer unaware of the marker
	revived server.Id
	// a canceled external attempt the close-out never closed
	canceledKeyed server.Id
	// safely canceled, never closed
	canceledSafe server.Id
	// canceled with a usdc_payout_closed event under an id that is not its
	// closure's
	foreignEvent server.Id
}

func insertUsdcClosedTestPayments(t testing.TB, ctx context.Context, networkId server.Id) *usdcClosedTestPayments {
	t.Helper()
	payments := &usdcClosedTestPayments{
		closedUnsent:        insertUsdcClosedTestPayment(t, ctx, networkId, horizonTestPayment{canceled: true}, true),
		closedPaidElsewhere: insertUsdcClosedTestPayment(t, ctx, networkId, horizonTestPayment{canceled: true, circleIdempotentId: horizonTestPointer(server.NewId())}, true),
		pending:             insertUsdcClosedTestPayment(t, ctx, networkId, horizonTestPayment{}, false),
		revived:             insertUsdcClosedTestPayment(t, ctx, networkId, horizonTestPayment{}, true),
		canceledKeyed:       insertUsdcClosedTestPayment(t, ctx, networkId, horizonTestPayment{canceled: true, circleIdempotentId: horizonTestPointer(server.NewId())}, false),
		canceledSafe:        insertUsdcClosedTestPayment(t, ctx, networkId, horizonTestPayment{canceled: true}, false),
		foreignEvent:        insertUsdcClosedTestPayment(t, ctx, networkId, horizonTestPayment{canceled: true}, false),
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_account_payment (event_id, payment_id, event_type, event_details)
			VALUES ($1, $2, $3, '{}')`, server.NewId(), payments.foreignEvent, server.UsdcPayoutClosedEventType))
	}, server.TxReadCommitted, server.OptNoRetry())
	return payments
}

// The horizon reaper deletes a contract past the horizon whose sweep belongs
// to a closed payment, never sent or keyed, or to a safely canceled one, closed
// or not; before, it kept a closed or safely canceled one as
// payment_incomplete and a closed keyed one as payment_hold. It keeps a
// contract of a pending payment, a revived closed one or a canceled keyed one
// (payment_hold).
func TestContractHorizonReapsContractsOfClosedPayments(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		usePayoutTransition(t)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		old := now.Add(-120 * 24 * time.Hour)
		payments := insertUsdcClosedTestPayments(t, ctx, f.destinationNetworkId)
		next := 0
		contract := func(paymentId server.Id) *horizonTestContract {
			next += 1
			contract := newHorizonTestContract(old.Add(time.Duration(next)*time.Second), "settled")
			contract.paymentId = &paymentId
			return contract
		}

		eligible := []*horizonTestContract{
			contract(payments.closedUnsent),
			contract(payments.closedPaidElsewhere),
			contract(payments.canceledSafe),
			contract(payments.foreignEvent),
		}
		protected := map[string][]*horizonTestContract{
			contractHorizonProtectedPaymentHold: {
				contract(payments.pending),
				contract(payments.revived),
				contract(payments.canceledKeyed),
			},
		}
		all := append([]*horizonTestContract{}, eligible...)
		for _, contracts := range protected {
			all = append(all, contracts...)
		}
		insertHorizonTestContracts(t, ctx, f, all...)
		before := map[server.Id][6]int{}
		for _, contract := range all {
			before[contract.contractId], _ = countHorizonTestRows(t, ctx, contract.contractId)
		}

		report, _, err := RemoveContractsPastHorizon(ctx, now, ContractHorizonCursor{}, horizonTestSettings())
		if err != nil {
			t.Fatal(err)
		}
		for _, contract := range eligible {
			if rows, archived := countHorizonTestRows(t, ctx, contract.contractId); rows != [6]int{} || archived != 1 {
				t.Errorf("a contract of a closed payment kept rows %v or archived %d", rows, archived)
			}
		}
		for reason, contracts := range protected {
			for _, contract := range contracts {
				if rows, _ := countHorizonTestRows(t, ctx, contract.contractId); rows != before[contract.contractId] {
					t.Errorf("a %s contract changed: %v, was %v", reason, rows, before[contract.contractId])
				}
			}
			if report.Protected[reason] != int64(len(contracts)) {
				t.Errorf("protected %s = %d, want %d", reason, report.Protected[reason], len(contracts))
			}
		}
		if report.Eligible != int64(len(eligible)) || report.Deleted["transfer_contract"] != int64(len(eligible)) {
			t.Fatalf("wrong report: %+v", report)
		}
	})
}

// The due-contract delete pass deletes a due contract inside the straggler
// horizon (a stamp of the former 90-day rule) whose sweep belongs to a closed
// payment, never sent or keyed, or to a safely canceled one, closed or not;
// before, it repaired each to reap_time = NULL, as holding no completed payment
// or as a hold. It repairs the contract of every open obligation.
func TestDueContractReapDeletesContractsOfClosedPayments(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		young := now.Add(-100 * 24 * time.Hour)
		payments := insertUsdcClosedTestPayments(t, ctx, f.destinationNetworkId)
		next := 0
		contract := func(paymentId server.Id) *horizonTestContract {
			next += 1
			contract := newHorizonTestContract(young.Add(time.Duration(next)*time.Second), "settled")
			contract.paymentId = &paymentId
			return contract
		}
		deleted := []*horizonTestContract{
			contract(payments.closedUnsent),
			contract(payments.closedPaidElsewhere),
			contract(payments.canceledSafe),
			contract(payments.foreignEvent),
		}
		repaired := []*horizonTestContract{
			contract(payments.pending),
			contract(payments.revived),
			contract(payments.canceledKeyed),
		}
		all := append(append([]*horizonTestContract{}, deleted...), repaired...)
		insertHorizonTestContracts(t, ctx, f, all...)
		contractIds := []server.Id{}
		for _, contract := range all {
			contractIds = append(contractIds, contract.contractId)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET reap_time = $2 WHERE contract_id = ANY($1)`,
				contractIds, now.Add(-time.Hour)))
		}, server.TxReadCommitted, server.OptNoRetry())

		removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 100)

		for _, contract := range deleted {
			if rows := countReaperDependentTestRows(t, ctx, contract.contractId); rows != [6]int{} {
				t.Errorf("a due contract of a closed payment kept rows %v", rows)
			}
		}
		for _, contract := range repaired {
			if rows := countReaperDependentTestRows(t, ctx, contract.contractId); rows[0] != 1 || rows[3] != 1 {
				t.Errorf("a due contract of an open payment lost rows %v", rows)
			}
			if reapTime := testingReapTime(ctx, contract.contractId); reapTime != nil {
				t.Errorf("a due contract of an open payment kept reap_time %v", reapTime)
			}
		}
	})
}

// The due-contract delete pass deletes a due contract inside the straggler
// horizon whose sweep is unpaid, and one with no sweep: USDC payouts ended, so
// an unpaid sweep is forfeited (owner decision 2026-11-13). Before, it repaired
// both to reap_time = NULL as paid by no settled payment. It still repairs one
// whose sweep's payment row is missing, as the contract horizon reaper keeps
// it (payment_incomplete).
func TestDueContractReapDeletesUnpaidContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		young := now.Add(-100 * 24 * time.Hour)
		unpaid := newHorizonTestContract(young, "settled")
		unswept := newHorizonTestContract(young.Add(time.Second), "settled")
		unswept.unswept = true
		missingPaymentId := server.NewId()
		missingPayment := newHorizonTestContract(young.Add(2*time.Second), "settled")
		missingPayment.paymentId = &missingPaymentId
		insertHorizonTestContracts(t, ctx, f, unpaid, unswept, missingPayment)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET reap_time = $2 WHERE contract_id = ANY($1)`,
				[]server.Id{unpaid.contractId, unswept.contractId, missingPayment.contractId}, now.Add(-time.Hour)))
		}, server.TxReadCommitted, server.OptNoRetry())

		removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 100)

		for _, contract := range []*horizonTestContract{unpaid, unswept} {
			if rows := countReaperDependentTestRows(t, ctx, contract.contractId); rows != [6]int{} {
				t.Errorf("a due unpaid contract kept rows %v", rows)
			}
		}
		if rows := countReaperDependentTestRows(t, ctx, missingPayment.contractId); rows[0] != 1 || rows[3] != 1 {
			t.Errorf("a due contract of a missing payment lost rows %v", rows)
		}
		if reapTime := testingReapTime(ctx, missingPayment.contractId); reapTime != nil {
			t.Errorf("a due contract of a missing payment kept reap_time %v", reapTime)
		}
	})
}

// The straggler stamp gives an aged contract of a closed keyed payment (paid
// elsewhere) a reap_time, as it gives one of a closed payment never sent;
// before, it skipped the keyed one as a hold forever. It never stamps a
// contract of a pending payment, a revived closed one or a canceled keyed one.
func TestStragglerStampsContractsOfClosedPayments(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		aged := now.Add(-StragglerContractExpiration - 24*time.Hour)
		payments := insertUsdcClosedTestPayments(t, ctx, f.destinationNetworkId)
		next := 0
		contract := func(paymentId server.Id) *horizonTestContract {
			next += 1
			contract := newHorizonTestContract(aged.Add(time.Duration(next)*time.Second), "settled")
			contract.paymentId = &paymentId
			return contract
		}
		stamped := []*horizonTestContract{
			contract(payments.closedUnsent),
			contract(payments.closedPaidElsewhere),
			contract(payments.canceledSafe),
		}
		held := []*horizonTestContract{
			contract(payments.pending),
			contract(payments.revived),
			contract(payments.canceledKeyed),
		}
		insertHorizonTestContracts(t, ctx, f, append(append([]*horizonTestContract{}, stamped...), held...)...)

		assignStragglerReapTimeBatches(ctx, now.Add(-StragglerContractExpiration), 100)

		for _, contract := range stamped {
			if testingReapTime(ctx, contract.contractId) == nil {
				t.Errorf("an aged contract of a settled or safely canceled payment was not stamped")
			}
		}
		for _, contract := range held {
			if reapTime := testingReapTime(ctx, contract.contractId); reapTime != nil {
				t.Errorf("an aged contract of an open payment was stamped %v", reapTime)
			}
		}
	})
}
