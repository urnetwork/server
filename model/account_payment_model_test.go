package model

import (
	"context"
	"testing"
	"time"

	"github.com/golang/glog"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
	"golang.org/x/exp/maps"
)

func TestCancelAccountPayment(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)

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
		assert.Equal(t, len(paymentPlan.NetworkPayments), 0)
		assert.Equal(t, paymentPlan.WithheldNetworkIds, []server.Id{destinationNetworkId})

		usedTransferByteCount = ByteCount(1024 * 1024 * 1024)

		for paid < 2*UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
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
		assert.Equal(t, maps.Keys(paymentPlan.NetworkPayments), []server.Id{destinationNetworkId})

		for _, payment := range paymentPlan.NetworkPayments {

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

	server.DefaultTestEnv().Run(func() {

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
		assert.Equal(t, transferStats.UnpaidBytesProvided, ByteCount(0))
		assert.Equal(t, transferStats.PaidBytesProvided, ByteCount(0))

		usedTransferByteCount := ByteCount(1024)
		paidByteCount := ByteCount(0)
		paid := NanoCents(0)

		// we want to meet MinWalletPayoutThreshold
		// otherwise the plan will not include the payout
		for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
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
		assert.Equal(t, transferStats.UnpaidBytesProvided, paidByteCount)
		assert.Equal(t, transferStats.PaidBytesProvided, ByteCount(0))

		// Plan payments
		plan, err := PlanPayments(ctx)
		assert.Equal(t, err, nil)

		// Since the plan is in progress, should be marked as paid
		transferStats = GetTransferStats(ctx, destinationNetworkId)
		assert.Equal(t, transferStats.UnpaidBytesProvided, ByteCount(0))
		assert.Equal(t, transferStats.PaidBytesProvided, paidByteCount)

		// mark plan items as complete
		for _, payment := range plan.NetworkPayments {
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")

			CompletePayment(ctx, payment.PaymentId, "")
		}

		transferStats = GetTransferStats(ctx, destinationNetworkId)
		assert.Equal(t, transferStats.PaidBytesProvided, paidByteCount)
		assert.Equal(t, transferStats.UnpaidBytesProvided, ByteCount(0))

	})
}

func TestPaymentPlanSubsidy(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()

		// add subscription to both source and destination
		AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
			NetworkId:        sourceNetworkId,
			SubscriptionType: SubscriptionTypeSupporter,
			StartTime:        server.NowUtc(),
			EndTime:          server.NowUtc().Add(24 * time.Hour),
		})
		AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
			NetworkId:        destinationNetworkId,
			SubscriptionType: SubscriptionTypeSupporter,
			StartTime:        server.NowUtc(),
			EndTime:          server.NowUtc().Add(24 * time.Hour),
		})

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})
		destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: destinationNetworkId,
			ClientId:  &destinationId,
		})

		getAccountBalanceResult := GetAccountBalance(sourceSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		getAccountBalanceResult = GetAccountBalance(destinationSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		balanceCode, err := CreateBalanceCode(ctx, netTransferByteCount, netRevenue, "", "", "")
		assert.Equal(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret: balanceCode.Secret,
		}, sourceSession)

		contractIds := GetOpenContractIds(ctx, sourceId, destinationId)
		assert.Equal(t, len(contractIds), 0)

		// test that escrow prevents concurrent contracts

		transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, netTransferByteCount)
		assert.Equal(t, err, nil)

		transferBalances := GetActiveTransferBalances(ctx, sourceNetworkId)
		netBalanceByteCount := ByteCount(0)
		for _, transferBalance := range transferBalances {
			netBalanceByteCount += transferBalance.BalanceByteCount
		}
		// nothing left
		assert.Equal(t, netBalanceByteCount, ByteCount(0))

		_, err = CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, netTransferByteCount)
		assert.NotEqual(t, err, nil)

		CloseContract(ctx, transferEscrow.ContractId, sourceId, 0, false)
		CloseContract(ctx, transferEscrow.ContractId, destinationId, 0, false)

		transferBalances = GetActiveTransferBalances(ctx, sourceNetworkId)
		netBalanceByteCount = ByteCount(0)
		for _, transferBalance := range transferBalances {
			netBalanceByteCount += transferBalance.BalanceByteCount
		}
		assert.Equal(t, netBalanceByteCount, netTransferByteCount)

		transferEscrow, err = CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024*1024)
		assert.Equal(t, err, nil)

		contractIds = GetOpenContractIds(ctx, sourceId, destinationId)
		assert.Equal(t, contractIds, map[server.Id]ContractParty{
			transferEscrow.ContractId: "",
		})

		usedTransferByteCount := ByteCount(1024)
		CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
		CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
		paidByteCount := usedTransferByteCount
		paid := UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))

		contractIds = GetOpenContractIds(ctx, sourceId, destinationId)
		assert.Equal(t, len(contractIds), 0)

		// check that the payout is pending
		getAccountBalanceResult = GetAccountBalance(sourceSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		getAccountBalanceResult = GetAccountBalance(destinationSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, paidByteCount)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, paid)
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		transferBalances = GetActiveTransferBalances(ctx, sourceNetworkId)
		netBalanceByteCount = 0
		for _, transferBalance := range transferBalances {
			netBalanceByteCount += transferBalance.BalanceByteCount
		}
		assert.Equal(t, netBalanceByteCount, netTransferByteCount-paidByteCount)

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

		// plan a payment and complete the payment
		// nothing to plan because the payout does not meet the min threshold
		paymentPlan, err := PlanPayments(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(paymentPlan.NetworkPayments), 0)
		assert.Equal(t, paymentPlan.WithheldNetworkIds, []server.Id{destinationNetworkId})

		subsidyPayment := GetSubsidyPayment(ctx, paymentPlan.PaymentPlanId)
		assert.NotEqual(t, subsidyPayment, nil)
		assert.Equal(t, subsidyPayment.NetPayout, NanoCents(0))
		assert.Equal(t, subsidyPayment.PaidUserCount, 1)
		assert.Equal(t, subsidyPayment.ActiveUserCount, 1)
		assert.Equal(t, subsidyPayment.NetPayoutByteCountPaid, paidByteCount)
		assert.Equal(t, subsidyPayment.NetPayoutByteCountUnpaid, ByteCount(0))

		usedTransferByteCount = ByteCount(1024 * 1024 * 1024)
		for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
			assert.Equal(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			paidByteCount += usedTransferByteCount
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		contractIds = GetOpenContractIds(ctx, sourceId, destinationId)
		assert.Equal(t, len(contractIds), 0)

		getAccountBalanceResult = GetAccountBalance(destinationSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, paidByteCount)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, paid)
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		paymentPlan, err = PlanPayments(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, maps.Keys(paymentPlan.NetworkPayments), []server.Id{destinationNetworkId})

		subsidyPayment = GetSubsidyPayment(ctx, paymentPlan.PaymentPlanId)
		assert.NotEqual(t, subsidyPayment, nil)
		assert.Equal(t, subsidyPayment.NetPayout, NanoCents(0))
		assert.Equal(t, subsidyPayment.PaidUserCount, 1)
		assert.Equal(t, subsidyPayment.ActiveUserCount, 1)
		assert.Equal(t, subsidyPayment.NetPayoutByteCountPaid, paidByteCount)
		assert.Equal(t, subsidyPayment.NetPayoutByteCountUnpaid, ByteCount(0))
		subsidyPaidByteCount := paidByteCount

		for _, payment := range paymentPlan.NetworkPayments {
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")

			CompletePayment(ctx, payment.PaymentId, "")
		}

		// check that the payment is recorded
		getAccountBalanceResult = GetAccountBalance(sourceSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		getAccountBalanceResult = GetAccountBalance(destinationSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, paidByteCount)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, paid)
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, paidByteCount)
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, paid)

		// repeat escrow until it fails due to no balance
		contractCount := 0
		usedTransferByteCount = ByteCount(1024 * 1024 * 1024)
		for {
			transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
			if err != nil && 1024 < usedTransferByteCount {
				usedTransferByteCount = usedTransferByteCount / 1024
				glog.Infof("Step down contract size to %d bytes.\n", usedTransferByteCount)
				continue
			}
			if netTransferByteCount <= paidByteCount {
				assert.NotEqual(t, err, nil)
				break
			} else {
				assert.Equal(t, err, nil)
			}

			CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			paidByteCount += usedTransferByteCount
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
			contractCount += 1
		}
		// at this point the balance should be fully used up

		transferBalances = GetActiveTransferBalances(ctx, sourceNetworkId)
		assert.Equal(t, transferBalances, []*TransferBalance{})

		paymentPlan, err = PlanPayments(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, maps.Keys(paymentPlan.NetworkPayments), []server.Id{destinationNetworkId})

		subsidyPayment = GetSubsidyPayment(ctx, paymentPlan.PaymentPlanId)
		assert.NotEqual(t, subsidyPayment, nil)
		assert.Equal(t, subsidyPayment.NetPayout, NanoCents(0))
		assert.Equal(t, subsidyPayment.PaidUserCount, 1)
		assert.Equal(t, subsidyPayment.ActiveUserCount, 1)
		assert.Equal(t, subsidyPayment.NetPayoutByteCountPaid, paidByteCount-subsidyPaidByteCount)
		assert.Equal(t, subsidyPayment.NetPayoutByteCountUnpaid, ByteCount(0))
		subsidyPaidByteCount = paidByteCount - subsidyPaidByteCount

		for _, payment := range paymentPlan.NetworkPayments {
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")

			CompletePayment(ctx, payment.PaymentId, "")
			UpdatePaymentWallet(ctx, payment.PaymentId)
		}

		// check that the payment is recorded
		getAccountBalanceResult = GetAccountBalance(sourceSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		// the revenue from
		getAccountBalanceResult = GetAccountBalance(destinationSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, netTransferByteCount)
		// each contract can have a 1 nanocent rounding error
		if e := getAccountBalanceResult.Balance.ProvidedNetRevenue - UsdToNanoCents(ProviderRevenueShare*NanoCentsToUsd(netRevenue)); e < -int64(contractCount) || int64(contractCount) < e {
			assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, UsdToNanoCents(ProviderRevenueShare*NanoCentsToUsd(netRevenue)))
		}
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, netTransferByteCount)
		// each contract can have a 1 nanocent rounding error
		if e := getAccountBalanceResult.Balance.PaidNetRevenue - UsdToNanoCents(ProviderRevenueShare*NanoCentsToUsd(netRevenue)); e < -int64(contractCount) || int64(contractCount) < e {
			assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, UsdToNanoCents(ProviderRevenueShare*NanoCentsToUsd(netRevenue)))
		}

		// there shoud be no more payments
		paymentPlan, err = PlanPayments(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(paymentPlan.NetworkPayments), 0)

		subsidyPayment = GetSubsidyPayment(ctx, paymentPlan.PaymentPlanId)
		assert.Equal(t, subsidyPayment, nil)

	})
}

/**
 * Test that the account points are paid out correctly to networks per payout.
 */
func TestAccountPointsPerPayout(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10)

		/**
		 * Network A and B will be the providers
		 */
		networkIdA := server.NewId()
		userIdA := server.NewId()
		// clientIdA := server.NewId()
		// clientSessionA := session.Testing_CreateClientSession(
		// 	ctx,
		// 	jwt.NewByJwt(networkIdA, userIdA, "a", false),
		// )

		networkIdB := server.NewId()
		userIdB := server.NewId()
		// clientSessionB := session.Testing_CreateClientSession(
		// 	ctx,
		// 	jwt.NewByJwt(networkIdB, userIdB, "b", false),
		// )

		/**
		 * We'll use network C will use Network A and B as providers
		 */
		networkIdC := server.NewId()
		userIdC := server.NewId()

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)
		Testing_CreateNetwork(ctx, networkIdC, "c", userIdC)
		clientSessionC := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdC, userIdC, "c", false),
		)

		/**
		 * Create balance for network C
		 */
		balanceCode, err := CreateBalanceCode(ctx, 2*netTransferByteCount, 2*netRevenue, "", "", "")
		assert.Equal(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret: balanceCode.Secret,
		}, clientSessionC)

		usedTransferByteCount := ByteCount(1024 * 1024 * 1024)

		/**
		 * Network A provides data to Network C
		 */
		paid := NanoCents(0)
		for paid < UsdToNanoCents(2.00) {
			transferEscrow, err := CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdA, userIdA, usedTransferByteCount)
			assert.Equal(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, userIdA, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		/**
		 * Network B provides twices as much data to Network C
		 */
		paid = NanoCents(0)
		for paid < UsdToNanoCents(3.00) {
			transferEscrow, err := CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdB, userIdB, usedTransferByteCount)
			assert.Equal(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, userIdB, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		/**
		 * Plan payments
		 */
		paymentPlan, err := PlanPayments(ctx)
		assert.Equal(t, err, nil)

		assert.Equal(t, len(paymentPlan.NetworkPayments), 2)

		// get payment for network A
		paymentA, ok := paymentPlan.NetworkPayments[networkIdA]
		assert.Equal(t, ok, true)

		paymentB, ok := paymentPlan.NetworkPayments[networkIdB]
		assert.Equal(t, ok, true)
		assert.Equal(t, paymentA.PaymentId != paymentB.PaymentId, true)

		/**
		 * total payout: 5.00 USDC
		 * total points should be 500 * 1m
		 * network A points should be 2 / 5 * 1m = 400000
		 * network B points should be 3 / 5 * 1m = 600000
		 */
		assert.Equal(t, int(paymentA.AccountPoints), 400_000)
		assert.Equal(t, int(paymentB.AccountPoints), 600_000)

	})
}
