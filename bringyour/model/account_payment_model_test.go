package model

import (
	"context"
	"testing"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/session"
	"github.com/go-playground/assert/v2"
	"golang.org/x/exp/maps"
)

func TestAccountPayment(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {

	})
}

func TestCancelAccountPayment(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)

		sourceNetworkId := bringyour.NewId()
		sourceId := bringyour.NewId()
		destinationNetworkId := bringyour.NewId()
		destinationId := bringyour.NewId()

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})
		destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: destinationNetworkId,
			ClientId:  &destinationId,
		})

		balanceCode, err := CreateBalanceCode(ctx, netTransferByteCount, netRevenue, "", "", "")
		assert.Equal(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret: balanceCode.Secret,
		}, sourceSession)

		transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024*1024)
		assert.Equal(t, err, nil)

		usedTransferByteCount := ByteCount(1024)
		paidByteCount := usedTransferByteCount

		CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
		CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)

		paid := UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))

		destinationWalletAddress := "0x1234567890"

		args := &CreateAccountWalletExternalArgs{
			Blockchain:       "MATIC",
			WalletAddress:    destinationWalletAddress,
			DefaultTokenType: "USDC",
		}
		walletId := CreateAccountWalletExternal(destinationSession, args)
		assert.NotEqual(t, walletId, nil)

		wallet := GetAccountWallet(ctx, *walletId)

		SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)

		paymentPlan, err := PlanPayments(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(paymentPlan.WalletPayments), 0)
		assert.Equal(t, paymentPlan.WithheldWalletIds, []bringyour.Id{wallet.WalletId})

		usedTransferByteCount = ByteCount(1024 * 1024 * 1024)

		for paid < MinWalletPayoutThreshold*2 {
			transferEscrow, err := CreateTransferEscrow(
				ctx,
				sourceNetworkId,
				sourceId,
				destinationNetworkId,
				destinationId,
				usedTransferByteCount,
			)
			assert.Equal(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			paidByteCount += usedTransferByteCount
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		contractIds := GetOpenContractIds(ctx, sourceId, destinationId)
		assert.Equal(t, len(contractIds), 0)

		paymentPlan, err = PlanPayments(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, maps.Keys(paymentPlan.WalletPayments), []bringyour.Id{wallet.WalletId})

		for _, payment := range paymentPlan.WalletPayments {

			assert.Equal(t, payment.Canceled, false)

			CancelPayment(ctx, payment.PaymentId)

			payment, err := GetPayment(ctx, payment.PaymentId)
			assert.Equal(t, err, nil)
			assert.Equal(t, payment.Canceled, true)
			assert.NotEqual(t, payment.CancelTime, nil)
		}

	})
}

func TestGetNetworkProvideStats(t *testing.T) {

	bringyour.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		sourceNetworkId := bringyour.NewId()
		sourceId := bringyour.NewId()
		destinationNetworkId := bringyour.NewId()
		destinationId := bringyour.NewId()

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
		balanceCode, err := CreateBalanceCode(ctx, netTransferByteCount, netRevenue, "", "", "")
		assert.Equal(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret: balanceCode.Secret,
		}, sourceSession)

		// create a wallet to receive the payout
		args := &CreateAccountWalletExternalArgs{
			Blockchain:       "matic",
			WalletAddress:    "",
			DefaultTokenType: "usdc",
		}
		walletId := CreateAccountWalletExternal(destinationSession, args)
		assert.NotEqual(t, walletId, nil)

		wallet := GetAccountWallet(ctx, *walletId)
		assert.NotEqual(t, wallet, nil)

		SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)

		// Check network stats
		// Everything should be 0
		transferStats := GetTransferStats(ctx, destinationNetworkId)
		assert.Equal(t, transferStats.UnpaidBytesProvided, int(0))
		assert.Equal(t, transferStats.PaidBytesProvided, int(0))

		usedTransferByteCount := ByteCount(1024)
		paidByteCount := int64(0)
		paid := int64(0)

		// we want to meet MinWalletPayoutThreshold
		// otherwise the plan will not include the payout
		for paid < MinWalletPayoutThreshold {
			transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
			assert.Equal(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			paidByteCount += usedTransferByteCount
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		// Check network stats
		// Should register unpaid byte count
		transferStats = GetTransferStats(ctx, destinationNetworkId)
		assert.Equal(t, int64(transferStats.UnpaidBytesProvided), paidByteCount)
		assert.Equal(t, transferStats.PaidBytesProvided, int(0))

		// Plan payments
		plan, err := PlanPayments(ctx)
		assert.Equal(t, err, nil)

		// Since the plan is incomplete, should be still marked as unpaid
		transferStats = GetTransferStats(ctx, destinationNetworkId)
		assert.Equal(t, int64(transferStats.UnpaidBytesProvided), paidByteCount)
		assert.Equal(t, transferStats.PaidBytesProvided, int(0))

		// mark plan items as complete
		for _, payment := range plan.WalletPayments {
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			CompletePayment(ctx, payment.PaymentId, "")
		}

		transferStats = GetTransferStats(ctx, destinationNetworkId)
		assert.Equal(t, int64(transferStats.PaidBytesProvided), paidByteCount)
		assert.Equal(t, transferStats.UnpaidBytesProvided, int(0))

	})

}
