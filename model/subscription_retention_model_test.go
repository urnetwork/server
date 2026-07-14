package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// row counts for one contract across the contract tables
func testingCountContractRows(ctx context.Context, contractId server.Id) (contractCount int, closeCount int, escrowCount int, sweepCount int) {
	server.Db(ctx, func(conn server.PgConn) {
		count := func(sql string) int {
			c := 0
			result, err := conn.Query(ctx, sql, contractId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&c))
				}
			})
			return c
		}
		contractCount = count(`SELECT COUNT(*) FROM transfer_contract WHERE contract_id = $1`)
		closeCount = count(`SELECT COUNT(*) FROM contract_close WHERE contract_id = $1`)
		escrowCount = count(`SELECT COUNT(*) FROM transfer_escrow WHERE contract_id = $1`)
		sweepCount = count(`SELECT COUNT(*) FROM transfer_escrow_sweep WHERE contract_id = $1`)
	})
	return
}

// testingReapTime reads transfer_contract.reap_time for one contract; nil when
// the contract is missing or reap_time is unset.
func testingReapTime(ctx context.Context, contractId server.Id) *time.Time {
	var reapTime *time.Time
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT reap_time FROM transfer_contract WHERE contract_id = $1`,
			contractId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&reapTime))
			}
		})
	})
	return reapTime
}

// testingSettledPayoutContracts funds a fresh source network, sets a payout
// wallet on a fresh destination network, then closes and settles enough
// contracts to reach the wallet payout threshold. Each returned contract is
// settled with a transfer_escrow_sweep, so a caller can drive
// PlanPayments/CompletePayment and retention on a known group.
func testingSettledPayoutContracts(ctx context.Context, t testing.TB) (
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractIds []server.Id,
) {
	sourceNetworkId = server.NewId()
	sourceId = server.NewId()
	destinationNetworkId = server.NewId()
	destinationId = server.NewId()

	sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId: sourceNetworkId,
		ClientId:  &sourceId,
	})
	destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId: destinationNetworkId,
		ClientId:  &destinationId,
	})

	// fund the source network
	netTransferByteCount := ByteCount(1024 * 1024)
	netRevenue := UsdToNanoCents(10.00)
	balanceCode, err := CreateBalanceCode(
		ctx,
		netTransferByteCount,
		365*24*time.Hour,
		netRevenue,
		"",
		"",
		"",
	)
	connect.AssertEqual(t, err, nil)
	RedeemBalanceCode(&RedeemBalanceCodeArgs{
		Secret:    balanceCode.Secret,
		NetworkId: sourceSession.ByJwt.NetworkId,
	}, sourceSession.Ctx)

	// a wallet to receive the payout
	walletId := CreateAccountWalletExternal(destinationSession, &CreateAccountWalletExternalArgs{
		NetworkId:        destinationNetworkId,
		Blockchain:       "matic",
		WalletAddress:    "",
		DefaultTokenType: "usdc",
	})
	connect.AssertNotEqual(t, walletId, nil)
	err = SetPayoutWallet(ctx, destinationNetworkId, *walletId)
	connect.AssertEqual(t, err, nil)

	// close and settle enough contracts to meet the payout threshold
	usedTransferByteCount := ByteCount(1024)
	paid := NanoCents(0)
	for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
		transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
		connect.AssertEqual(t, err, nil)

		err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
		connect.AssertEqual(t, err, nil)
		err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
		connect.AssertEqual(t, err, nil)
		contractIds = append(contractIds, transferEscrow.ContractId)
		paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
	}
	return
}

// After a payment completes, RemoveCompletedContracts must hard delete each of
// its contracts together with the contract_close/transfer_escrow/
// transfer_escrow_sweep rows in the same pass (no orphans for a later sweep) --
// but only once now() passes the contract's reap_time (complete_time +
// CompletedContractExpiration). A contract whose reap_time is still in the future
// and an open/live contract must both be left untouched.
func TestRemoveCompletedContractsCascades(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sourceNetworkId, sourceId, destinationNetworkId, destinationId, paidContractIds := testingSettledPayoutContracts(ctx, t)

		// pay out the plan; completing a payment stamps reap_time on its contracts
		completeTime := server.NowUtc()
		plan, err := PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		for _, payment := range plan.NetworkPayments {
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			CompletePayment(ctx, payment.PaymentId, "", "0xtest")
		}

		// each paid contract now has reap_time ~= complete_time + 7d (in the future)
		for _, contractId := range paidContractIds {
			reapTime := testingReapTime(ctx, contractId)
			connect.AssertEqual(t, reapTime != nil, true)
			expected := completeTime.Add(CompletedContractExpiration)
			connect.AssertEqual(t, reapTime.After(expected.Add(-time.Hour)) && reapTime.Before(expected.Add(time.Hour)), true)
		}

		// an open contract that must survive retention
		liveEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, ByteCount(1024))
		connect.AssertEqual(t, err, nil)

		// sanity: the paid contracts have sweeps before retention
		_, _, _, sweepCount := testingCountContractRows(ctx, paidContractIds[0])
		connect.AssertNotEqual(t, sweepCount, 0)

		// reap_time is in the future, so a reap now must leave the paid contracts
		// in place (future reap_time is not yet due)
		RemoveCompletedContracts(ctx, server.NowUtc().Add(time.Hour))
		for _, contractId := range paidContractIds {
			contractCount, _, _, _ := testingCountContractRows(ctx, contractId)
			connect.AssertEqual(t, contractCount, 1)
		}

		// advance the paid contracts past their reap_time so they are due
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_contract SET reap_time = $2 WHERE contract_id = ANY($1)`,
				paidContractIds,
				server.NowUtc().Add(-time.Hour),
			))
		})

		RemoveCompletedContracts(ctx, server.NowUtc().Add(time.Hour))

		// the paid contracts and every dependent row are gone in the same pass
		for _, contractId := range paidContractIds {
			contractCount, closeCount, escrowCount, sweepCount := testingCountContractRows(ctx, contractId)
			connect.AssertEqual(t, contractCount, 0)
			connect.AssertEqual(t, closeCount, 0)
			connect.AssertEqual(t, escrowCount, 0)
			connect.AssertEqual(t, sweepCount, 0)
		}

		// the open contract survives with its escrow
		contractCount, _, escrowCount, _ := testingCountContractRows(ctx, liveEscrow.ContractId)
		connect.AssertEqual(t, contractCount, 1)
		connect.AssertNotEqual(t, escrowCount, 0)

		// balances are removed once their end time passes retention
		countBalances := func() int {
			c := 0
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT COUNT(*) FROM transfer_balance WHERE network_id = $1`,
					sourceNetworkId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&c))
					}
				})
			})
			return c
		}
		connect.AssertNotEqual(t, countBalances(), 0)
		RemoveCompletedContracts(ctx, server.NowUtc().Add(2*365*24*time.Hour))
		connect.AssertEqual(t, countBalances(), 0)
	})
}

// SweepOrphanContractData is the safety net for dependent rows whose contract
// no longer exists (from older releases or interrupted statements). It must
// remove all orphans across bounded batches while leaving live contracts'
// rows in place.
func TestSweepOrphanContractData(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})

		netTransferByteCount := ByteCount(1024 * 1024)
		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			365*24*time.Hour,
			UsdToNanoCents(10.00),
			"",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceSession.ByJwt.NetworkId,
		}, sourceSession.Ctx)

		// a live contract with a close row that must survive the sweep
		usedTransferByteCount := ByteCount(1024)
		liveEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
		connect.AssertEqual(t, err, nil)
		err = CloseContract(ctx, liveEscrow.ContractId, sourceId, usedTransferByteCount, false)
		connect.AssertEqual(t, err, nil)

		// orphan rows: dependents of contract ids that do not exist
		orphanCount := 3
		server.Tx(ctx, func(tx server.PgTx) {
			for range orphanCount {
				orphanContractId := server.NewId()
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO contract_close (contract_id, party, used_transfer_byte_count)
					VALUES ($1, $2, $3)
					`,
					orphanContractId,
					ContractPartySource,
					1024,
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO transfer_escrow (contract_id, balance_id, balance_byte_count)
					VALUES ($1, $2, $3)
					`,
					orphanContractId,
					server.NewId(),
					1024,
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO transfer_escrow_sweep (contract_id, balance_id, network_id, payout_byte_count, payout_net_revenue_nano_cents)
					VALUES ($1, $2, $3, $4, $5)
					`,
					orphanContractId,
					server.NewId(),
					destinationNetworkId,
					1024,
					0,
				))
			}
		})

		// limit=1 forces one delete per batch, exercising the batch loop
		removedCount := SweepOrphanContractData(ctx, 1)
		connect.AssertEqual(t, removedCount, int64(3*orphanCount))

		// the live contract's rows survive
		contractCount, closeCount, escrowCount, _ := testingCountContractRows(ctx, liveEscrow.ContractId)
		connect.AssertEqual(t, contractCount, 1)
		connect.AssertEqual(t, closeCount, 1)
		connect.AssertNotEqual(t, escrowCount, 0)

		// all orphans are gone
		server.Db(ctx, func(conn server.PgConn) {
			count := func(sql string) int {
				c := 0
				result, err := conn.Query(ctx, sql)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&c))
					}
				})
				return c
			}
			orphanCloseCount := count(`
				SELECT COUNT(*) FROM contract_close
				WHERE NOT EXISTS (
					SELECT 1 FROM transfer_contract
					WHERE transfer_contract.contract_id = contract_close.contract_id
				)
			`)
			connect.AssertEqual(t, orphanCloseCount, 0)
			orphanEscrowCount := count(`
				SELECT COUNT(*) FROM transfer_escrow
				WHERE NOT EXISTS (
					SELECT 1 FROM transfer_contract
					WHERE transfer_contract.contract_id = transfer_escrow.contract_id
				)
			`)
			connect.AssertEqual(t, orphanEscrowCount, 0)
			orphanSweepCount := count(`
				SELECT COUNT(*) FROM transfer_escrow_sweep
				WHERE NOT EXISTS (
					SELECT 1 FROM transfer_contract
					WHERE transfer_contract.contract_id = transfer_escrow_sweep.contract_id
				)
			`)
			connect.AssertEqual(t, orphanSweepCount, 0)
		})
	})
}

// A closed contract with no completed-payment sweep is a straggler: it never
// gets a reap_time from CompletePayment, so it survives the normal completed-
// payout cascade. Once it ages past StragglerContractExpiration the reaper's
// assign pass stamps reap_time = now() and the delete pass hard deletes it with
// its whole group. This covers both a contract whose sweep is planned into a
// payment that never completes and a contract that was never swept at all. The
// pending account_payment row itself is never deleted.
func TestRemoveStragglerContracts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sourceNetworkId, sourceId, destinationNetworkId, destinationId, contractIds := testingSettledPayoutContracts(ctx, t)

		// plan the payments but never complete them: the payments stay pending, so
		// CompletePayment never stamps reap_time on these contracts
		plan, err := PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, len(plan.NetworkPayments), 0)

		// a closed contract that was never swept at all (quarantine/corruption
		// path) is also a straggler, reaped only once it ages past the window
		sweeplessEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, ByteCount(1024))
		connect.AssertEqual(t, err, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE transfer_contract
				SET outcome = $2, close_time = now()
				WHERE contract_id = $1
				`,
				sweeplessEscrow.ContractId,
				ContractOutcomeSettled,
			))
		})

		countPendingPayments := func() int {
			c := 0
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT COUNT(*) FROM account_payment
					WHERE network_id = $1 AND NOT completed AND NOT canceled
					`,
					destinationNetworkId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&c))
					}
				})
			})
			return c
		}
		connect.AssertNotEqual(t, countPendingPayments(), 0)

		// inside the straggler window the contracts are too young to be reaped, so
		// they carry no reap_time and survive
		RemoveCompletedContracts(ctx, server.NowUtc().Add(time.Hour))
		contractCount, closeCount, escrowCount, sweepCount := testingCountContractRows(ctx, contractIds[0])
		connect.AssertEqual(t, contractCount, 1)
		connect.AssertNotEqual(t, closeCount, 0)
		connect.AssertNotEqual(t, escrowCount, 0)
		connect.AssertEqual(t, sweepCount, 1)
		connect.AssertEqual(t, testingReapTime(ctx, contractIds[0]) == nil, true)
		sweeplessCount, _, _, _ := testingCountContractRows(ctx, sweeplessEscrow.ContractId)
		connect.AssertEqual(t, sweeplessCount, 1)

		// age the whole group (pending-payment contracts + the sweep-less one) past
		// the straggler expiration
		agedIds := append(append([]server.Id{}, contractIds...), sweeplessEscrow.ContractId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE transfer_contract
				SET create_time = $2
				WHERE contract_id = ANY($1)
				`,
				agedIds,
				server.NowUtc().Add(-StragglerContractExpiration-24*time.Hour),
			))
		})

		// the reaper's assign pass marks every aged closed straggler with a
		// reap_time; the delete pass then hard deletes each with its whole group.
		// Split this across two runs and advance the assigned reap_time into the
		// clear past between them, so the delete is deterministic regardless of
		// DB/app clock skew: the assign stamps SQL now() (the DB clock) while the
		// delete compares reap_time against server.NowUtc() (the app clock), so a
		// just-assigned straggler is only due once the app clock passes it (in
		// production, a later 30-minute run).
		RemoveCompletedContracts(ctx, server.NowUtc().Add(time.Hour))
		for _, contractId := range agedIds {
			// each aged straggler is now either already reaped or marked for it,
			// which proves the assign pass ran
			reaped := func() bool {
				c, _, _, _ := testingCountContractRows(ctx, contractId)
				return c == 0
			}()
			connect.AssertEqual(t, reaped || testingReapTime(ctx, contractId) != nil, true)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_contract SET reap_time = $2 WHERE contract_id = ANY($1) AND reap_time IS NOT NULL`,
				agedIds,
				server.NowUtc().Add(-time.Hour),
			))
		})
		RemoveCompletedContracts(ctx, server.NowUtc().Add(time.Hour))

		// the whole group is gone, sweeps included
		for _, contractId := range contractIds {
			contractCount, closeCount, escrowCount, sweepCount := testingCountContractRows(ctx, contractId)
			connect.AssertEqual(t, contractCount, 0)
			connect.AssertEqual(t, closeCount, 0)
			connect.AssertEqual(t, escrowCount, 0)
			connect.AssertEqual(t, sweepCount, 0)
		}
		// the sweep-less straggler is gone too, escrow included
		sweeplessCount, _, sweeplessEscrowCount, _ := testingCountContractRows(ctx, sweeplessEscrow.ContractId)
		connect.AssertEqual(t, sweeplessCount, 0)
		connect.AssertEqual(t, sweeplessEscrowCount, 0)

		// the pending payment record itself is retained
		connect.AssertNotEqual(t, countPendingPayments(), 0)
	})
}

// The reap_time backfills seed the indexed retention column for rows that
// predate reap_time: BackfillCompletedContractReapTime stamps completed
// contracts with complete_time + CompletedContractExpiration, and
// BackfillStragglerContractReapTime stamps aged closed-but-never-completed
// contracts with now(). Both feed the same indexed reaper.
func TestBackfillContractReapTime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sourceNetworkId, sourceId, destinationNetworkId, destinationId, paidContractIds := testingSettledPayoutContracts(ctx, t)

		// complete the payments, then clear reap_time to simulate rows created
		// before the reap_time column existed (each test has its own database, so
		// this global clear is scoped to this test's data)
		completeTime := server.NowUtc()
		plan, err := PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		for _, payment := range plan.NetworkPayments {
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			CompletePayment(ctx, payment.PaymentId, "", "0xtest")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET reap_time = NULL`))
		})
		for _, contractId := range paidContractIds {
			connect.AssertEqual(t, testingReapTime(ctx, contractId) == nil, true)
		}

		// the completed backfill re-derives reap_time = complete_time + 7d
		backfilled := BackfillCompletedContractReapTime(ctx, 1000)
		connect.AssertNotEqual(t, backfilled, int64(0))
		for _, contractId := range paidContractIds {
			reapTime := testingReapTime(ctx, contractId)
			connect.AssertEqual(t, reapTime != nil, true)
			expected := completeTime.Add(CompletedContractExpiration)
			connect.AssertEqual(t, reapTime.After(expected.Add(-time.Hour)) && reapTime.Before(expected.Add(time.Hour)), true)
		}

		// a closed contract with no completed payment, aged past the straggler
		// window, gets no reap_time until the straggler backfill assigns now()
		sweeplessEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, ByteCount(1024))
		connect.AssertEqual(t, err, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE transfer_contract
				SET outcome = $2, close_time = now(), create_time = $3
				WHERE contract_id = $1
				`,
				sweeplessEscrow.ContractId,
				ContractOutcomeSettled,
				server.NowUtc().Add(-StragglerContractExpiration-24*time.Hour),
			))
		})
		connect.AssertEqual(t, testingReapTime(ctx, sweeplessEscrow.ContractId) == nil, true)

		before := server.NowUtc()
		straggler := BackfillStragglerContractReapTime(ctx, 1000)
		connect.AssertNotEqual(t, straggler, int64(0))
		reapTime := testingReapTime(ctx, sweeplessEscrow.ContractId)
		connect.AssertEqual(t, reapTime != nil, true)
		// assigned ~ now (not the aged create_time)
		connect.AssertEqual(t, reapTime.After(before.Add(-time.Hour)) && reapTime.Before(server.NowUtc().Add(time.Hour)), true)
	})
}

// Payments stuck pending past HungPaymentExpiration are canceled, which
// releases their sweeps back to the payout planner for a fresh payment.
// Recent pending payments are untouched.
func TestCancelHungAccountPayments(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})
		destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: destinationNetworkId,
			ClientId:  &destinationId,
		})

		netTransferByteCount := ByteCount(1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)
		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			365*24*time.Hour,
			netRevenue,
			"",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceSession.ByJwt.NetworkId,
		}, sourceSession.Ctx)

		walletId := CreateAccountWalletExternal(destinationSession, &CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "matic",
			WalletAddress:    "",
			DefaultTokenType: "usdc",
		})
		connect.AssertNotEqual(t, walletId, nil)
		err = SetPayoutWallet(ctx, destinationNetworkId, *walletId)
		connect.AssertEqual(t, err, nil)

		usedTransferByteCount := ByteCount(1024)
		paid := NanoCents(0)
		for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
			connect.AssertEqual(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		plan, err := PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, len(plan.NetworkPayments), 0)

		// a recent pending payment is not hung
		connect.AssertEqual(t, CancelHungAccountPayments(ctx, server.NowUtc()), int64(0))

		// age the payments past the hung expiration
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE account_payment SET create_time = $1 WHERE NOT completed AND NOT canceled`,
				server.NowUtc().Add(-HungPaymentExpiration-24*time.Hour),
			))
		})

		canceledCount := CancelHungAccountPayments(ctx, server.NowUtc())
		connect.AssertNotEqual(t, canceledCount, int64(0))

		// the sweeps are re-planned into fresh pending payments
		plan2, err := PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, len(plan2.NetworkPayments), 0)
	})
}

// removeContractBatches must fully drain the eligible set across bounded
// batches even when candidate rows collapse to fewer contract deletes: a
// contract can have several sweeps, so a full LIMIT batch of sweep candidate
// rows can delete fewer than LIMIT contracts. The loop therefore terminates on
// an empty batch, not a short one -- a short batch would strand eligible
// contracts and re-create the per-minute rescan the batching was meant to
// avoid. This is what lets the retention task run every 30 minutes instead of
// every minute.
func TestRemoveContractBatchesDrainsDuplicateCandidates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		contractIds := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		oldCreateTime := server.NowUtc().Add(-30 * 24 * time.Hour)

		server.Tx(ctx, func(tx server.PgTx) {
			for _, contractId := range contractIds {
				// closed contract (outcome set -> open = false)
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO transfer_contract (
						contract_id, source_network_id, source_id,
						destination_network_id, destination_id,
						transfer_byte_count, create_time, outcome
					)
					VALUES ($1, $2, $2, $2, $2, $3, $4, $5)
					`,
					contractId, networkId, 1024, oldCreateTime, ContractOutcomeSettled,
				))
				// two sweeps per contract -> duplicate candidate contract_ids, so a
				// full batch of two sweep rows can be a single contract
				for range 2 {
					server.RaisePgResult(tx.Exec(
						ctx,
						`
						INSERT INTO transfer_escrow_sweep (
							contract_id, balance_id, network_id,
							payout_byte_count, payout_net_revenue_nano_cents
						)
						VALUES ($1, $2, $3, $4, $5)
						`,
						contractId, server.NewId(), networkId, 1024, 0,
					))
				}
			}
		})

		countRemaining := func() (contracts int, sweeps int) {
			server.Db(ctx, func(conn server.PgConn) {
				count := func(sql string) int {
					c := 0
					result, err := conn.Query(ctx, sql, contractIds)
					server.WithPgResult(result, err, func() {
						if result.Next() {
							server.Raise(result.Scan(&c))
						}
					})
					return c
				}
				contracts = count(`SELECT COUNT(*) FROM transfer_contract WHERE contract_id = ANY($1)`)
				sweeps = count(`SELECT COUNT(*) FROM transfer_escrow_sweep WHERE contract_id = ANY($1)`)
			})
			return
		}

		contracts, sweeps := countRemaining()
		connect.AssertEqual(t, contracts, 3)
		connect.AssertEqual(t, sweeps, 6)

		// batch size 2, block-1 shape (candidate from sweeps, cascade the sweeps,
		// delete the contract). With two sweeps per contract a batch may delete a
		// single contract, so a short batch must not end the drain.
		removeContractBatches(
			ctx,
			`
			WITH candidate AS (
				SELECT transfer_escrow_sweep.contract_id
				FROM transfer_escrow_sweep
				INNER JOIN transfer_contract ON
					transfer_contract.contract_id = transfer_escrow_sweep.contract_id
				WHERE transfer_contract.create_time < $1
				LIMIT $2
			), deleted_sweep AS (
				DELETE FROM transfer_escrow_sweep
				USING candidate
				WHERE transfer_escrow_sweep.contract_id = candidate.contract_id
			)
			DELETE FROM transfer_contract
			USING candidate
			WHERE transfer_contract.contract_id = candidate.contract_id
			`,
			server.NowUtc(),
			2,
		)

		contracts, sweeps = countRemaining()
		connect.AssertEqual(t, contracts, 0)
		connect.AssertEqual(t, sweeps, 0)
	})
}
