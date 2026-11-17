package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// USDC provider payouts ended (owner decisions 2026-11-12 and 2026-11-13), so
// /transfer/stats reports no provided byte as unpaid: not settled traffic, not
// a pending payment's payout bytes, not a canceled payment's sweeps. Before, it
// reported each of them as unpaid. A completed payment's bytes are paid.
func TestGetNetworkProvideStats(t *testing.T) {

	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()
		testingCreatePaymentClient(ctx, sourceNetworkId, sourceId)
		testingCreatePaymentClient(ctx, destinationNetworkId, destinationId)

		sourceSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})

		// fund the source network
		netTransferByteCount := ByteCount(1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)
		subscriptionYearDuration := 365 * 24 * time.Hour

		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			subscriptionYearDuration,
			netRevenue,
			"",
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)
		testingRedeemPaymentBalanceCode(t, sourceSession.Ctx, sourceSession.ByJwt.NetworkId, balanceCode.Secret)

		// Check network stats
		// Everything should be 0
		transferStats := GetTransferStats(ctx, destinationNetworkId)
		connect.AssertEqual(t, transferStats.UnpaidBytesProvided, ByteCount(0))
		connect.AssertEqual(t, transferStats.PaidBytesProvided, ByteCount(0))

		usedTransferByteCount := ByteCount(1024)
		paidByteCount := ByteCount(0)
		contractIds := []server.Id{}
		for range 4 {
			transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
			connect.AssertEqual(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paidByteCount += usedTransferByteCount
			contractIds = append(contractIds, transferEscrow.ContractId)
		}

		// settled traffic with no payment
		transferStats = GetTransferStats(ctx, destinationNetworkId)
		connect.AssertEqual(t, transferStats.UnpaidBytesProvided, ByteCount(0))
		connect.AssertEqual(t, transferStats.PaidBytesProvided, ByteCount(0))

		// a pending payment of half the sweeps, as a plan left it, and a
		// safely canceled payment of the other half
		paymentId := testingAssignSweepPayment(ctx, destinationNetworkId, contractIds[:2], nil)
		canceledPaymentId := testingAssignSweepPayment(ctx, destinationNetworkId, contractIds[2:], nil)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE account_payment SET canceled = true, cancel_time = $2 WHERE payment_id = $1`,
				canceledPaymentId,
				server.NowUtc(),
			))
		})
		transferStats = GetTransferStats(ctx, destinationNetworkId)
		connect.AssertEqual(t, transferStats.UnpaidBytesProvided, ByteCount(0))
		connect.AssertEqual(t, transferStats.PaidBytesProvided, ByteCount(0))

		// the payment completed, as the sender left it: the bytes are paid
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE account_payment SET completed = true, complete_time = $2, payment_record = 'synthetic-record', tx_hash = 'synthetic-tx' WHERE payment_id = $1`,
				paymentId,
				server.NowUtc(),
			))
		})
		transferStats = GetTransferStats(ctx, destinationNetworkId)
		connect.AssertEqual(t, transferStats.PaidBytesProvided, paidByteCount/2)
		connect.AssertEqual(t, transferStats.UnpaidBytesProvided, ByteCount(0))

	})
}
