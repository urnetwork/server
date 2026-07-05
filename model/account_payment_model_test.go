package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/glog"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
	"golang.org/x/exp/maps"
)

func TestCancelAccountPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

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

		assert.Equal(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceNetworkId,
		}, sourceSession.Ctx)

		transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024*1024)
		assert.Equal(t, err, nil)

		usedTransferByteCount := ByteCount(1024)
		paidByteCount := usedTransferByteCount

		CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
		CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)

		paid := UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))

		destinationWalletAddress := "0x1234567890"

		args := &CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    destinationWalletAddress,
			DefaultTokenType: "USDC",
		}
		walletId := CreateAccountWalletExternal(destinationSession, args)
		assert.NotEqual(t, walletId, nil)

		wallet := GetAccountWallet(ctx, *walletId)

		err = SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)
		assert.Equal(t, err, nil)

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
			assert.Equal(t, payment.Blockchain, "MATIC")
			assert.NotEqual(t, payment.CancelTime, nil)
		}

	})
}

func TestGetNetworkProvideStats(t *testing.T) {

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
		assert.Equal(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceSession.ByJwt.NetworkId,
		}, sourceSession.Ctx)

		// create a wallet to receive the payout
		args := &CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "matic",
			WalletAddress:    "",
			DefaultTokenType: "usdc",
		}
		walletId := CreateAccountWalletExternal(destinationSession, args)
		assert.NotEqual(t, walletId, nil)

		wallet := GetAccountWallet(ctx, *walletId)
		assert.NotEqual(t, wallet, nil)

		err = SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)
		assert.Equal(t, err, nil)

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

		// Since the plan is in progress
		// and the payments are not marked as completed, should be still marked as unpaid
		transferStats = GetTransferStats(ctx, destinationNetworkId)
		assert.Equal(t, transferStats.UnpaidBytesProvided, paidByteCount)
		assert.Equal(t, transferStats.PaidBytesProvided, ByteCount(0))

		// mark plan items as complete
		for _, payment := range plan.NetworkPayments {
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")

			mockTxHash := "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

			CompletePayment(ctx, payment.PaymentId, "", mockTxHash)
		}

		// items are completed, should be marked as paid bytes
		transferStats = GetTransferStats(ctx, destinationNetworkId)
		assert.Equal(t, transferStats.PaidBytesProvided, paidByteCount)
		assert.Equal(t, transferStats.UnpaidBytesProvided, ByteCount(0))

	})
}

func TestPlanPaymentsNeverAssignsForeignWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()
		foreignNetworkId := server.NewId()
		foreignClientId := server.NewId()

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})
		destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: destinationNetworkId,
			ClientId:  &destinationId,
		})
		foreignSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: foreignNetworkId,
			ClientId:  &foreignClientId,
		})

		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			365*24*time.Hour,
			netRevenue,
			"",
			"",
			"",
		)
		assert.Equal(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceNetworkId,
		}, sourceSession.Ctx)

		// the destination owns an active wallet and sets it for payout
		destinationWalletAddress := "0xdddd"
		destinationWalletId := CreateAccountWalletExternal(destinationSession, &CreateAccountWalletExternalArgs{
			NetworkId:        destinationNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    destinationWalletAddress,
			DefaultTokenType: "USDC",
		})
		assert.NotEqual(t, destinationWalletId, nil)
		err = SetPayoutWallet(ctx, destinationNetworkId, *destinationWalletId)
		assert.Equal(t, err, nil)

		// another network owns a separate wallet
		foreignWalletId := CreateAccountWalletExternal(foreignSession, &CreateAccountWalletExternalArgs{
			NetworkId:        foreignNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    "0xffff",
			DefaultTokenType: "USDC",
		})
		assert.NotEqual(t, foreignWalletId, nil)

		// provide enough traffic to exceed the min payout
		usedTransferByteCount := ByteCount(1024 * 1024 * 1024)
		paid := NanoCents(0)
		for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
			assert.Equal(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		// corrupt the payout wallet to point at another network's wallet,
		// simulating rows written before ownership validation existed
		corruptPayoutWallet := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE payout_wallet SET wallet_id = $2 WHERE network_id = $1`,
					destinationNetworkId,
					*foreignWalletId,
				))
			})
		}
		corruptPayoutWallet()

		paymentPlan, err := PlanPayments(ctx)
		assert.Equal(t, err, nil)
		payment, ok := paymentPlan.NetworkPayments[destinationNetworkId]
		assert.Equal(t, ok, true)
		// the plan must never assign a wallet owned by another network
		assert.Equal(t, payment.WalletId, nil)

		// updating the payment wallet must also refuse the foreign wallet
		UpdatePaymentWallet(ctx, payment.PaymentId)
		updatedPayment, err := GetPayment(ctx, payment.PaymentId)
		assert.Equal(t, err, nil)
		assert.Equal(t, updatedPayment.WalletId, nil)

		// once the network sets its own wallet, the payment adopts it
		err = SetPayoutWallet(ctx, destinationNetworkId, *destinationWalletId)
		assert.Equal(t, err, nil)
		UpdatePaymentWallet(ctx, payment.PaymentId)
		updatedPayment, err = GetPayment(ctx, payment.PaymentId)
		assert.Equal(t, err, nil)
		assert.Equal(t, *updatedPayment.WalletId, *destinationWalletId)
		assert.Equal(t, *updatedPayment.WalletAddress, destinationWalletAddress)

		// corrupting the payout wallet after assignment must not redirect the payment
		corruptPayoutWallet()
		UpdatePaymentWallet(ctx, payment.PaymentId)
		updatedPayment, err = GetPayment(ctx, payment.PaymentId)
		assert.Equal(t, err, nil)
		assert.Equal(t, *updatedPayment.WalletId, *destinationWalletId)
		assert.Equal(t, *updatedPayment.WalletAddress, destinationWalletAddress)

	})
}

func TestPaymentPlanSubsidyEqualWeight(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)

		paidPayerNetworkId := server.NewId()
		paidPayerClientId := server.NewId()
		freePayerNetworkId := server.NewId()
		freePayerClientId := server.NewId()
		providerANetworkId := server.NewId()
		providerAClientId := server.NewId()
		providerBNetworkId := server.NewId()
		providerBClientId := server.NewId()

		paidPayerSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: paidPayerNetworkId,
			ClientId:  &paidPayerClientId,
		})
		providerASession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: providerANetworkId,
			ClientId:  &providerAClientId,
		})
		providerBSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: providerBNetworkId,
			ClientId:  &providerBClientId,
		})

		// the paid payer purchases a balance (paid traffic)
		balanceCode, err := CreateBalanceCode(
			ctx,
			netTransferByteCount,
			365*24*time.Hour,
			netRevenue,
			"",
			"",
			"",
		)
		assert.Equal(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: paidPayerNetworkId,
		}, paidPayerSession.Ctx)

		// the free payer gets a no-cost balance (free traffic)
		err = AddBasicTransferBalance(
			ctx,
			freePayerNetworkId,
			netTransferByteCount,
			server.NowUtc().Add(-1*time.Hour),
			server.NowUtc().Add(365*24*time.Hour),
		)
		assert.Equal(t, err, nil)

		// both providers own wallets set for payout
		providerAWalletId := CreateAccountWalletExternal(providerASession, &CreateAccountWalletExternalArgs{
			NetworkId:        providerANetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    "0xaaaa",
			DefaultTokenType: "USDC",
		})
		assert.NotEqual(t, providerAWalletId, nil)
		err = SetPayoutWallet(ctx, providerANetworkId, *providerAWalletId)
		assert.Equal(t, err, nil)

		providerBWalletId := CreateAccountWalletExternal(providerBSession, &CreateAccountWalletExternalArgs{
			NetworkId:        providerBNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    "0xbbbb",
			DefaultTokenType: "USDC",
		})
		assert.NotEqual(t, providerBWalletId, nil)
		err = SetPayoutWallet(ctx, providerBNetworkId, *providerBWalletId)
		assert.Equal(t, err, nil)

		// equal traffic: the paid payer routes via provider A,
		// the free payer routes via provider B
		usedTransferByteCount := ByteCount(1024 * 1024 * 1024)

		escrowA, err := CreateTransferEscrow(ctx, paidPayerNetworkId, paidPayerClientId, providerANetworkId, providerAClientId, usedTransferByteCount)
		assert.Equal(t, err, nil)
		err = CloseContract(ctx, escrowA.ContractId, paidPayerClientId, usedTransferByteCount, false)
		assert.Equal(t, err, nil)
		err = CloseContract(ctx, escrowA.ContractId, providerAClientId, usedTransferByteCount, false)
		assert.Equal(t, err, nil)

		escrowB, err := CreateTransferEscrow(ctx, freePayerNetworkId, freePayerClientId, providerBNetworkId, providerBClientId, usedTransferByteCount)
		assert.Equal(t, err, nil)
		err = CloseContract(ctx, escrowB.ContractId, freePayerClientId, usedTransferByteCount, false)
		assert.Equal(t, err, nil)
		err = CloseContract(ctx, escrowB.ContractId, providerBClientId, usedTransferByteCount, false)
		assert.Equal(t, err, nil)

		// backdate the contracts so the subsidy covers half an epoch
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_contract SET create_time = $1`,
				server.NowUtc().Add(-15*24*time.Hour),
			))
		})

		paymentPlan, err := PlanPayments(ctx)
		assert.Equal(t, err, nil)

		subsidyPayment := GetSubsidyPayment(ctx, paymentPlan.PaymentPlanId)
		assert.NotEqual(t, subsidyPayment, nil)
		// paid and unpaid byte counts are still recorded for reporting
		assert.Equal(t, subsidyPayment.NetPayoutByteCountPaid, usedTransferByteCount)
		assert.Equal(t, subsidyPayment.NetPayoutByteCountUnpaid, usedTransferByteCount)
		assert.Equal(t, subsidyPayment.PaidUserCount, 1)
		assert.Equal(t, subsidyPayment.ActiveUserCount, 2)

		paymentA, ok := paymentPlan.NetworkPayments[providerANetworkId]
		assert.Equal(t, ok, true)
		paymentB, ok := paymentPlan.NetworkPayments[providerBNetworkId]
		assert.Equal(t, ok, true)

		// all traffic is weighted equally:
		// equal bytes earn equal subsidy whether the traffic was paid or free
		assert.NotEqual(t, paymentA.SubsidyPayout, NanoCents(0))
		assert.Equal(t, paymentA.SubsidyPayout, paymentB.SubsidyPayout)

		// the subsidy net payout is the sum of both provider subsidies
		assert.Equal(t, subsidyPayment.NetPayout, paymentA.SubsidyPayout+paymentB.SubsidyPayout)

		// approximately half of the min payout pot (0.5 scale of the epoch)
		minExpected := UsdToNanoCents(0.499 * EnvSubsidyConfig().MinPayoutUsd)
		maxExpected := UsdToNanoCents(0.502 * EnvSubsidyConfig().MinPayoutUsd)
		if subsidyPayment.NetPayout < minExpected || maxExpected < subsidyPayment.NetPayout {
			assert.Equal(t, subsidyPayment.NetPayout, UsdToNanoCents(0.5*EnvSubsidyConfig().MinPayoutUsd))
		}

		// the payments also carry the wallets owned by each provider
		assert.Equal(t, *paymentA.WalletId, *providerAWalletId)
		assert.Equal(t, *paymentB.WalletId, *providerBWalletId)

	})
}

func TestPaymentPlanSubsidy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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
		assert.Equal(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceSession.ByJwt.NetworkId,
		}, sourceSession.Ctx)

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
		assert.Equal(t, contractIds, map[server.Id][]ContractParty{
			transferEscrow.ContractId: []ContractParty{},
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
			NetworkId:        destinationNetworkId,
			Blockchain:       "matic",
			WalletAddress:    "",
			DefaultTokenType: "usdc",
		}
		walletId := CreateAccountWalletExternal(destinationSession, args)
		assert.NotEqual(t, walletId, nil)

		wallet := GetAccountWallet(ctx, *walletId)
		assert.NotEqual(t, wallet, nil)

		err = SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)
		assert.Equal(t, err, nil)

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

		mockTxHash := "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

		for _, payment := range paymentPlan.NetworkPayments {
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			RemovePaymentRecord(ctx, payment.PaymentId)
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")

			CompletePayment(ctx, payment.PaymentId, "", mockTxHash)
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

			CompletePayment(ctx, payment.PaymentId, "", mockTxHash)
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
