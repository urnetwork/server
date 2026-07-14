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

// RemoveCompletedContracts must delete a retained contract together with its
// contract_close/transfer_escrow/transfer_escrow_sweep rows in the same pass
// (no orphans for a later sweep), for both removal paths: contracts of
// completed payments, and closed contracts with no sweep. Contracts that are
// still open must be untouched.
func TestRemoveCompletedContractsCascades(t *testing.T) {
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
		paidContractIds := []server.Id{}
		for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
			connect.AssertEqual(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paidContractIds = append(paidContractIds, transferEscrow.ContractId)
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		// pay out the plan
		plan, err := PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		for _, payment := range plan.NetworkPayments {
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			CompletePayment(ctx, payment.PaymentId, "", "0xtest")
		}

		// a closed contract that was never swept (quarantine/corruption path)
		unsweptEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
		connect.AssertEqual(t, err, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE transfer_contract
				SET outcome = $2, close_time = now()
				WHERE contract_id = $1
				`,
				unsweptEscrow.ContractId,
				ContractOutcomeSettled,
			))
		})

		// an open contract that must survive retention
		liveEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
		connect.AssertEqual(t, err, nil)

		// sanity: the paid contracts have sweeps before retention
		_, _, _, sweepCount := testingCountContractRows(ctx, paidContractIds[0])
		connect.AssertNotEqual(t, sweepCount, 0)

		RemoveCompletedContracts(ctx, server.NowUtc().Add(time.Hour))

		// the paid contracts and every dependent row are gone in the same pass
		for _, contractId := range paidContractIds {
			contractCount, closeCount, escrowCount, sweepCount := testingCountContractRows(ctx, contractId)
			connect.AssertEqual(t, contractCount, 0)
			connect.AssertEqual(t, closeCount, 0)
			connect.AssertEqual(t, escrowCount, 0)
			connect.AssertEqual(t, sweepCount, 0)
		}

		// the unswept closed contract and its escrow are gone
		contractCount, closeCount, escrowCount, sweepCount := testingCountContractRows(ctx, unsweptEscrow.ContractId)
		connect.AssertEqual(t, contractCount, 0)
		connect.AssertEqual(t, closeCount, 0)
		connect.AssertEqual(t, escrowCount, 0)
		connect.AssertEqual(t, sweepCount, 0)

		// the open contract survives with its escrow
		contractCount, _, escrowCount, _ = testingCountContractRows(ctx, liveEscrow.ContractId)
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

// A contract whose sweep is planned into a payment that never completes is a
// straggler: it survives the normal 7-day cascade (which requires a completed
// payment) but must be hard deleted with its whole group once it is older
// than StragglerContractExpiration. The pending account_payment row itself is
// never deleted.
func TestRemoveStragglerContracts(t *testing.T) {
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
		contractIds := []server.Id{}
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

		// plan the payments but never complete them: the payments stay pending
		plan, err := PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, len(plan.NetworkPayments), 0)

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

		// the pending payment protects the group inside the straggler window
		RemoveCompletedContracts(ctx, server.NowUtc().Add(time.Hour))
		contractCount, closeCount, escrowCount, sweepCount := testingCountContractRows(ctx, contractIds[0])
		connect.AssertEqual(t, contractCount, 1)
		connect.AssertNotEqual(t, closeCount, 0)
		connect.AssertNotEqual(t, escrowCount, 0)
		connect.AssertEqual(t, sweepCount, 1)

		// age the contracts past the straggler expiration
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE transfer_contract
				SET create_time = $2
				WHERE contract_id = ANY($1)
				`,
				contractIds,
				server.NowUtc().Add(-StragglerContractExpiration-24*time.Hour),
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

		// the pending payment record itself is retained
		connect.AssertNotEqual(t, countPendingPayments(), 0)
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
