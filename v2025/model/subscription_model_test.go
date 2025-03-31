package model

import (
	"context"
	"slices"
	"testing"
	"time"

	// "math"
	mathrand "math/rand"

	"golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"

	"github.com/golang/glog"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestByteCount(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(func() {
		assert.Equal(t, ByteCountHumanReadable(ByteCount(0)), "0b")
		assert.Equal(t, ByteCountHumanReadable(ByteCount(5*1024*1024*1024*1024)), "5tib")

		count, err := ParseByteCount("2")
		assert.Equal(t, err, nil)
		assert.Equal(t, count, ByteCount(2))
		assert.Equal(t, ByteCountHumanReadable(count), "2b")

		count, err = ParseByteCount("5B")
		assert.Equal(t, err, nil)
		assert.Equal(t, count, ByteCount(5))
		assert.Equal(t, ByteCountHumanReadable(count), "5b")

		count, err = ParseByteCount("123KiB")
		assert.Equal(t, err, nil)
		assert.Equal(t, count, ByteCount(123*1024))
		assert.Equal(t, ByteCountHumanReadable(count), "123kib")

		count, err = ParseByteCount("5MiB")
		assert.Equal(t, err, nil)
		assert.Equal(t, count, ByteCount(5*1024*1024))
		assert.Equal(t, ByteCountHumanReadable(count), "5mib")

		count, err = ParseByteCount("1.7GiB")
		assert.Equal(t, err, nil)
		assert.Equal(t, count, ByteCount(17*1024*1024*1024)/ByteCount(10))
		assert.Equal(t, ByteCountHumanReadable(count), "1.7gib")

		count, err = ParseByteCount("13.1TiB")
		assert.Equal(t, err, nil)
		assert.Equal(t, count, ByteCount(131*1024*1024*1024*1024)/ByteCount(10))
		assert.Equal(t, ByteCountHumanReadable(count), "13.1tib")

	})
}

func TestNanoCents(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(func() {
		usd := float64(1.55)
		a := UsdToNanoCents(usd)
		usd2 := NanoCentsToUsd(a)
		a2 := UsdToNanoCents(usd2)

		assert.Equal(t, usd, usd2)
		assert.Equal(t, a, a2)
	})
}

func TestEscrow(t *testing.T) {
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

		for _, payment := range paymentPlan.NetworkPayments {
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

		for _, payment := range paymentPlan.NetworkPayments {
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			CompletePayment(ctx, payment.PaymentId, "")
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
	})
}

func TestCompanionEscrowAndCheckpoint(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		// tests companion and checkpoint
		// this is a more realistic use case
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

		getAccountBalanceResult := GetAccountBalance(destinationSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		getAccountBalanceResult = GetAccountBalance(sourceSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		balanceCode, err := CreateBalanceCode(ctx, 2*netTransferByteCount, 2*netRevenue, "", "", "")
		assert.Equal(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret: balanceCode.Secret,
		}, destinationSession)

		contractIds := GetOpenContractIds(ctx, sourceId, destinationId)
		assert.Equal(t, len(contractIds), 0)

		// test that escrow prevents concurrent contracts

		companionTransferEscrow, err := CreateTransferEscrow(ctx, destinationNetworkId, destinationId, sourceNetworkId, sourceId, netTransferByteCount)
		assert.Equal(t, err, nil)
		transferEscrow, err := CreateCompanionTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, netTransferByteCount, 1*time.Hour)
		assert.Equal(t, err, nil)

		transferBalances := GetActiveTransferBalances(ctx, destinationNetworkId)
		netBalanceByteCount := ByteCount(0)
		for _, transferBalance := range transferBalances {
			netBalanceByteCount += transferBalance.BalanceByteCount
		}
		// nothing left
		assert.Equal(t, netBalanceByteCount, ByteCount(0))

		_, err = CreateTransferEscrow(ctx, destinationNetworkId, destinationId, sourceNetworkId, sourceId, netTransferByteCount)
		assert.NotEqual(t, err, nil)
		_, err = CreateCompanionTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, netTransferByteCount, 1*time.Hour)
		assert.NotEqual(t, err, nil)

		CloseContract(ctx, companionTransferEscrow.ContractId, sourceId, 0, false)
		CloseContract(ctx, companionTransferEscrow.ContractId, destinationId, 0, false)

		CloseContract(ctx, transferEscrow.ContractId, sourceId, 0, false)
		CloseContract(ctx, transferEscrow.ContractId, destinationId, 0, false)

		transferBalances = GetActiveTransferBalances(ctx, destinationNetworkId)
		netBalanceByteCount = ByteCount(0)
		for _, transferBalance := range transferBalances {
			netBalanceByteCount += transferBalance.BalanceByteCount
		}
		assert.Equal(t, netBalanceByteCount, 2*netTransferByteCount)

		companionTransferEscrow, err = CreateTransferEscrow(ctx, destinationNetworkId, destinationId, sourceNetworkId, sourceId, 1024*1024)
		assert.Equal(t, err, nil)
		transferEscrow, err = CreateCompanionTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024*1024, 1*time.Hour)
		assert.Equal(t, err, nil)

		contractIds = GetOpenContractIds(ctx, sourceId, destinationId)
		assert.Equal(t, contractIds, map[server.Id]ContractParty{
			transferEscrow.ContractId: "",
		})

		usedTransferByteCount := ByteCount(1024)
		CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
		CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
		CloseContract(ctx, companionTransferEscrow.ContractId, sourceId, ByteCount(0), false)
		CloseContract(ctx, companionTransferEscrow.ContractId, destinationId, ByteCount(0), false)
		paidByteCount := usedTransferByteCount
		paid := UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))

		contractIds = GetOpenContractIds(ctx, sourceId, destinationId)
		assert.Equal(t, len(contractIds), 0)

		// check that the payout is pending
		getAccountBalanceResult = GetAccountBalance(destinationSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		getAccountBalanceResult = GetAccountBalance(sourceSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, paidByteCount)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, paid)
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		transferBalances = GetActiveTransferBalances(ctx, destinationNetworkId)
		netBalanceByteCount = 0
		for _, transferBalance := range transferBalances {
			netBalanceByteCount += transferBalance.BalanceByteCount
		}
		assert.Equal(t, netBalanceByteCount, 2*netTransferByteCount-paidByteCount)

		args := &CreateAccountWalletExternalArgs{
			Blockchain:       "matic",
			WalletAddress:    "",
			DefaultTokenType: "usdc",
		}
		walletId := CreateAccountWalletExternal(sourceSession, args)
		assert.NotEqual(t, walletId, nil)

		wallet := GetAccountWallet(ctx, *walletId)

		SetPayoutWallet(ctx, sourceNetworkId, wallet.WalletId)

		// plan a payment and complete the payment
		// nothing to plan because the payout does not meet the min threshold
		paymentPlan, err := PlanPayments(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(paymentPlan.NetworkPayments), 0)
		assert.Equal(t, paymentPlan.WithheldNetworkIds, []server.Id{sourceNetworkId})

		usedTransferByteCount = ByteCount(1024 * 1024 * 1024)
		for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			companionTransferEscrow, err := CreateTransferEscrow(ctx, destinationNetworkId, destinationId, sourceNetworkId, sourceId, usedTransferByteCount)
			assert.Equal(t, err, nil)
			transferEscrow, err := CreateCompanionTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount, 1*time.Hour)
			assert.Equal(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			CloseContract(ctx, companionTransferEscrow.ContractId, sourceId, ByteCount(0), false)
			CloseContract(ctx, companionTransferEscrow.ContractId, destinationId, ByteCount(0), false)

			paidByteCount += usedTransferByteCount
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		contractIds = GetOpenContractIds(ctx, sourceId, destinationId)
		assert.Equal(t, len(contractIds), 0)

		getAccountBalanceResult = GetAccountBalance(sourceSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, paidByteCount)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, paid)
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		paymentPlan, err = PlanPayments(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, maps.Keys(paymentPlan.NetworkPayments), []server.Id{sourceNetworkId})

		for _, payment := range paymentPlan.NetworkPayments {
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			CompletePayment(ctx, payment.PaymentId, "")
		}

		// check that the payment is recorded
		getAccountBalanceResult = GetAccountBalance(destinationSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		getAccountBalanceResult = GetAccountBalance(sourceSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, paidByteCount)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, paid)
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, paidByteCount)
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, paid)

		// repeat escrow until it fails due to no balance
		contractCount := 0
		usedTransferByteCount = ByteCount(1024 * 1024 * 1024)
		for {
			companionTransferEscrow, err := CreateTransferEscrow(ctx, destinationNetworkId, destinationId, sourceNetworkId, sourceId, netTransferByteCount)
			assert.Equal(t, err, nil)
			transferEscrow, err := CreateCompanionTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount, 1*time.Hour)
			if err != nil && 1024 < usedTransferByteCount {
				usedTransferByteCount = usedTransferByteCount / 1024
				glog.Infof("Step down contract size to %d bytes.\n", usedTransferByteCount)
				CloseContract(ctx, companionTransferEscrow.ContractId, sourceId, ByteCount(0), false)
				CloseContract(ctx, companionTransferEscrow.ContractId, destinationId, ByteCount(0), false)
				continue
			}
			if netTransferByteCount <= paidByteCount {
				assert.NotEqual(t, err, nil)
				CloseContract(ctx, companionTransferEscrow.ContractId, sourceId, ByteCount(0), false)
				CloseContract(ctx, companionTransferEscrow.ContractId, destinationId, ByteCount(0), false)
				break
			} else {
				assert.Equal(t, err, nil)
			}

			CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, 0 == mathrand.Intn(2))
			CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, 0 == mathrand.Intn(2))
			CloseContract(ctx, companionTransferEscrow.ContractId, sourceId, ByteCount(0), false)
			CloseContract(ctx, companionTransferEscrow.ContractId, destinationId, ByteCount(0), false)
			paidByteCount += usedTransferByteCount
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
			contractCount += 1
		}

		ForceCloseOpenContractIds(ctx, 0)

		// at this point the balance should be half used up

		transferBalances = GetActiveTransferBalances(ctx, destinationNetworkId)
		netBalanceByteCount = 0
		for _, transferBalance := range transferBalances {
			netBalanceByteCount += transferBalance.BalanceByteCount
		}
		assert.Equal(t, netBalanceByteCount, netTransferByteCount)

		paymentPlan, err = PlanPayments(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, maps.Keys(paymentPlan.NetworkPayments), []server.Id{sourceNetworkId})

		for _, payment := range paymentPlan.NetworkPayments {
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			CompletePayment(ctx, payment.PaymentId, "")
		}

		// check that the payment is recorded
		getAccountBalanceResult = GetAccountBalance(destinationSession)
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
		assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

		// the revenue from
		getAccountBalanceResult = GetAccountBalance(sourceSession)
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
	})
}

// TODO escrow benchmark to see how many contracts can be opened and closed in some time period (e.g. 15s)

func TestBalanceCode(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkIdA := server.NewId()

		userIdA := server.NewId()
		guestMode := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode),
		)

		checkResult0, err := CheckBalanceCode(
			&CheckBalanceCodeArgs{
				Secret: "foobar",
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, checkResult0.Error, nil)

		balanceCode, err := CreateBalanceCode(
			ctx,
			1024,
			100,
			"test-purchase-1",
			"rest-purchase-1-receipt",
			"test@bringyour.com",
		)
		assert.Equal(t, err, nil)

		balanceCodeId2, err := GetBalanceCodeIdForPurchaseEventId(ctx, balanceCode.PurchaseEventId)
		assert.Equal(t, err, nil)
		assert.Equal(t, balanceCode.BalanceCodeId, balanceCodeId2)

		_, err = GetBalanceCodeIdForPurchaseEventId(ctx, "test-purchase-nothing")
		assert.NotEqual(t, err, nil)

		balanceCode2, err := GetBalanceCode(ctx, balanceCode.BalanceCodeId)
		assert.Equal(t, err, nil)
		assert.Equal(t, *balanceCode, *balanceCode2)

		checkResult1, err := CheckBalanceCode(
			&CheckBalanceCodeArgs{
				Secret: balanceCode.Secret,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, checkResult1.Error, nil)
		assert.Equal(t, checkResult1.Balance.BalanceByteCount, ByteCount(1024))

		redeemResult0, err := RedeemBalanceCode(
			&RedeemBalanceCodeArgs{
				Secret: balanceCode.Secret,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, redeemResult0.Error, nil)
		assert.Equal(t, redeemResult0.TransferBalance.BalanceByteCount, ByteCount(1024))
	})
}

func TestSubscriptionPaymentId(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkIdA := server.NewId()

		userIdA := server.NewId()
		guestMode := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode),
		)

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)

		result, err := SubscriptionCreatePaymentId(&SubscriptionCreatePaymentIdArgs{}, clientSessionA)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result, nil)

		resultNetworkId, err := SubscriptionGetNetworkIdForPaymentId(ctx, result.SubscriptionPaymentId)
		assert.Equal(t, err, nil)
		assert.Equal(t, networkIdA, resultNetworkId)
	})
}

func TestInitialBalance(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkIdA := server.NewId()
		userIdA := server.NewId()

		networkIdB := server.NewId()
		userIdB := server.NewId()

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)

		networkIds := FindNetworksWithoutTransferBalance(ctx)
		assert.Equal(t, 2, len(networkIds))
		assert.Equal(t, true, slices.Contains(networkIds, networkIdA))
		assert.Equal(t, true, slices.Contains(networkIds, networkIdB))

		for _, networkId := range networkIds {
			initialTransferBalance := ByteCount(30 * 1024 * 1024 * 1024)
			initialTransferBalanceDuration := 30 * 24 * time.Hour

			startTime := server.NowUtc()
			endTime := startTime.Add(initialTransferBalanceDuration)
			AddBasicTransferBalance(
				ctx,
				networkId,
				initialTransferBalance,
				startTime,
				endTime,
			)

			transferBalances := GetActiveTransferBalances(ctx, networkId)
			assert.Equal(t, 1, len(transferBalances))
			transferBalance := transferBalances[0]
			assert.Equal(t, initialTransferBalance, transferBalance.BalanceByteCount)
			assert.Equal(t, startTime, transferBalance.StartTime)
			assert.Equal(t, endTime, transferBalance.EndTime)
		}
	})
}

func TestClosePartialContract(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkIdA := server.NewId()
		userIdA := server.NewId()
		clientIdA := server.NewId()

		networkIdB := server.NewId()
		userIdB := server.NewId()
		clientIdB := server.NewId()

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)

		initialTransferBalance := ByteCount(30 * 1024 * 1024 * 1024)

		for _, networkId := range []server.Id{networkIdA, networkIdB} {
			initialTransferBalanceDuration := 30 * 24 * time.Hour

			startTime := server.NowUtc()
			endTime := startTime.Add(initialTransferBalanceDuration)
			AddBasicTransferBalance(
				ctx,
				networkId,
				initialTransferBalance,
				startTime,
				endTime,
			)
		}

		for i := range 2 {
			var sourceNetworkId server.Id
			var sourceId server.Id
			var destinationNetworkId server.Id
			var destinationId server.Id
			if i == 0 {
				sourceNetworkId = networkIdA
				sourceId = clientIdA
				destinationNetworkId = networkIdB
				destinationId = clientIdB
			} else {
				sourceNetworkId = networkIdB
				sourceId = clientIdB
				destinationNetworkId = networkIdA
				destinationId = clientIdA
			}

			for j := range 2 {
				// create new contract with escrow
				contractId, _, err := CreateContract(
					ctx,
					sourceNetworkId,
					sourceId,
					destinationNetworkId,
					destinationId,
					ByteCount(1024*1024),
				)
				assert.Equal(t, err, nil)

				var close1Id server.Id
				var close2Id server.Id
				if j == 0 {
					close1Id = sourceId
					close2Id = destinationId
				} else {
					close1Id = destinationId
					close2Id = sourceId
				}

				err = CloseContract(
					ctx,
					contractId,
					close1Id,
					512*1024,
					false,
				)
				assert.Equal(t, err, nil)

				contractClose, closed := GetContractClose(ctx, contractId)
				assert.Equal(t, closed, false)

				err = CloseContract(
					ctx,
					contractId,
					close2Id,
					512*1024,
					false,
				)
				assert.Equal(t, err, nil)

				contractClose, closed = GetContractClose(ctx, contractId)
				assert.Equal(t, closed, true)
				assert.Equal(t, contractClose.Dispute, false)
				assert.Equal(t, contractClose.Outcome, ContractOutcomeSettled)

				// double close should fail
				err = CloseContract(
					ctx,
					contractId,
					close1Id,
					512*1024,
					false,
				)
				assert.NotEqual(t, err, nil)

				err = CloseContract(
					ctx,
					contractId,
					close2Id,
					512*1024,
					false,
				)
				assert.NotEqual(t, err, nil)
			}
		}

		endingTransferBalanceA := GetActiveTransferBalanceByteCount(ctx, networkIdA)
		endingTransferBalanceB := GetActiveTransferBalanceByteCount(ctx, networkIdB)
		assert.Equal(t, endingTransferBalanceA, initialTransferBalance-2*512*1024)
		assert.Equal(t, endingTransferBalanceB, initialTransferBalance-2*512*1024)
	})
}

func TestClosePartialContractWithCheckpoint(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkIdA := server.NewId()
		userIdA := server.NewId()
		clientIdA := server.NewId()

		networkIdB := server.NewId()
		userIdB := server.NewId()
		clientIdB := server.NewId()

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)

		initialTransferBalance := ByteCount(30 * 1024 * 1024 * 1024)

		for _, networkId := range []server.Id{networkIdA, networkIdB} {
			initialTransferBalanceDuration := 30 * 24 * time.Hour

			startTime := server.NowUtc()
			endTime := startTime.Add(initialTransferBalanceDuration)
			AddBasicTransferBalance(
				ctx,
				networkId,
				initialTransferBalance,
				startTime,
				endTime,
			)
		}

		for i := range 2 {
			var sourceNetworkId server.Id
			var sourceId server.Id
			var destinationNetworkId server.Id
			var destinationId server.Id
			if i == 0 {
				sourceNetworkId = networkIdA
				sourceId = clientIdA
				destinationNetworkId = networkIdB
				destinationId = clientIdB
			} else {
				sourceNetworkId = networkIdB
				sourceId = clientIdB
				destinationNetworkId = networkIdA
				destinationId = clientIdA
			}

			for j := range 2 {
				// create new contract with escrow
				contractId, _, err := CreateContract(
					ctx,
					sourceNetworkId,
					sourceId,
					destinationNetworkId,
					destinationId,
					ByteCount(1024*1024),
				)
				assert.Equal(t, err, nil)

				var close1Id server.Id
				var close2Id server.Id
				if j == 0 {
					close1Id = sourceId
					close2Id = destinationId
				} else {
					close1Id = destinationId
					close2Id = sourceId
				}

				err = CloseContract(
					ctx,
					contractId,
					close1Id,
					512*1024,
					true,
				)
				assert.Equal(t, err, nil)

				_, closed := GetContractClose(ctx, contractId)
				assert.Equal(t, closed, false)

				err = CloseContract(
					ctx,
					contractId,
					close2Id,
					512*1024,
					true,
				)
				assert.Equal(t, err, nil)

				_, closed = GetContractClose(ctx, contractId)
				assert.Equal(t, closed, false)
			}
		}

		ForceCloseOpenContractIds(ctx, 0)

		endingTransferBalanceA := GetActiveTransferBalanceByteCount(ctx, networkIdA)
		endingTransferBalanceB := GetActiveTransferBalanceByteCount(ctx, networkIdB)
		assert.Equal(t, endingTransferBalanceA, initialTransferBalance-2*512*1024)
		assert.Equal(t, endingTransferBalanceB, initialTransferBalance-2*512*1024)
	})
}

func TestClosePartialCompanionContractWithCheckpoint(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkIdA := server.NewId()
		userIdA := server.NewId()
		clientIdA := server.NewId()

		networkIdB := server.NewId()
		userIdB := server.NewId()
		clientIdB := server.NewId()

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)

		initialTransferBalance := ByteCount(30 * 1024 * 1024 * 1024)

		for _, networkId := range []server.Id{networkIdA, networkIdB} {
			initialTransferBalanceDuration := 30 * 24 * time.Hour

			startTime := server.NowUtc()
			endTime := startTime.Add(initialTransferBalanceDuration)
			AddBasicTransferBalance(
				ctx,
				networkId,
				initialTransferBalance,
				startTime,
				endTime,
			)
		}

		for i := range 2 {
			var sourceNetworkId server.Id
			var sourceId server.Id
			var destinationNetworkId server.Id
			var destinationId server.Id
			if i == 0 {
				sourceNetworkId = networkIdA
				sourceId = clientIdA
				destinationNetworkId = networkIdB
				destinationId = clientIdB
			} else {
				sourceNetworkId = networkIdB
				sourceId = clientIdB
				destinationNetworkId = networkIdA
				destinationId = clientIdA
			}

			for j := range 2 {
				// create new contract with escrow
				var contractId server.Id
				var err error
				if i == 0 {
					contractId, _, err = CreateContract(
						ctx,
						sourceNetworkId,
						sourceId,
						destinationNetworkId,
						destinationId,
						ByteCount(1024*1024),
					)
				} else {
					_, _, err := CreateContract(
						ctx,
						destinationNetworkId,
						destinationId,
						sourceNetworkId,
						sourceId,
						ByteCount(1024*1024),
					)
					assert.Equal(t, err, nil)
					contractId, _, err = CreateCompanionContract(
						ctx,
						sourceNetworkId,
						sourceId,
						destinationNetworkId,
						destinationId,
						ByteCount(1024*1024),
						1*time.Hour,
					)
				}
				assert.Equal(t, err, nil)

				var close1Id server.Id
				var close2Id server.Id
				if j == 0 {
					close1Id = sourceId
					close2Id = destinationId
				} else {
					close1Id = destinationId
					close2Id = sourceId
				}

				err = CloseContract(
					ctx,
					contractId,
					close1Id,
					512*1024,
					false,
				)
				assert.Equal(t, err, nil)

				_, closed := GetContractClose(ctx, contractId)
				assert.Equal(t, closed, false)

				err = CloseContract(
					ctx,
					contractId,
					close2Id,
					512*1024,
					true,
				)
				assert.Equal(t, err, nil)

				_, closed = GetContractClose(ctx, contractId)
				assert.Equal(t, closed, false)
			}
		}

		ForceCloseOpenContractIds(ctx, 0)

		endingTransferBalanceA := GetActiveTransferBalanceByteCount(ctx, networkIdA)
		endingTransferBalanceB := GetActiveTransferBalanceByteCount(ctx, networkIdB)
		assert.Equal(t, endingTransferBalanceA, initialTransferBalance-4*512*1024)
		assert.Equal(t, endingTransferBalanceB, initialTransferBalance)
	})
}

func TestClosePartialContractNoEscrow(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkIdA := server.NewId()
		userIdA := server.NewId()
		clientIdA := server.NewId()

		networkIdB := server.NewId()
		userIdB := server.NewId()
		clientIdB := server.NewId()

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)

		initialTransferBalance := ByteCount(30 * 1024 * 1024 * 1024)

		for _, networkId := range []server.Id{networkIdA, networkIdB} {
			initialTransferBalanceDuration := 30 * 24 * time.Hour

			startTime := server.NowUtc()
			endTime := startTime.Add(initialTransferBalanceDuration)
			AddBasicTransferBalance(
				ctx,
				networkId,
				initialTransferBalance,
				startTime,
				endTime,
			)
		}

		for i := range 2 {
			var sourceNetworkId server.Id
			var sourceId server.Id
			var destinationNetworkId server.Id
			var destinationId server.Id
			if i == 0 {
				sourceNetworkId = networkIdA
				sourceId = clientIdA
				destinationNetworkId = networkIdB
				destinationId = clientIdB
			} else {
				sourceNetworkId = networkIdB
				sourceId = clientIdB
				destinationNetworkId = networkIdA
				destinationId = clientIdA
			}

			for j := range 2 {
				// create new contract with escrow
				contractId, err := CreateContractNoEscrow(
					ctx,
					sourceNetworkId,
					sourceId,
					destinationNetworkId,
					destinationId,
					ByteCount(1024*1024),
				)
				assert.Equal(t, err, nil)

				var close1Id server.Id
				var close2Id server.Id
				if j == 0 {
					close1Id = sourceId
					close2Id = destinationId
				} else {
					close1Id = destinationId
					close2Id = sourceId
				}

				err = CloseContract(
					ctx,
					contractId,
					close1Id,
					512*1024,
					false,
				)
				assert.Equal(t, err, nil)

				contractClose, closed := GetContractClose(ctx, contractId)
				assert.Equal(t, closed, false)

				err = CloseContract(
					ctx,
					contractId,
					close2Id,
					512*1024,
					false,
				)
				assert.Equal(t, err, nil)

				contractClose, closed = GetContractClose(ctx, contractId)
				assert.Equal(t, closed, true)
				assert.Equal(t, contractClose.Dispute, false)
				assert.Equal(t, contractClose.Outcome, ContractOutcomeSettled)

				// double close should fail
				err = CloseContract(
					ctx,
					contractId,
					close1Id,
					512*1024,
					false,
				)
				assert.NotEqual(t, err, nil)

				err = CloseContract(
					ctx,
					contractId,
					close2Id,
					512*1024,
					false,
				)
				assert.NotEqual(t, err, nil)
			}
		}

		endingTransferBalanceA := GetActiveTransferBalanceByteCount(ctx, networkIdA)
		endingTransferBalanceB := GetActiveTransferBalanceByteCount(ctx, networkIdB)
		assert.Equal(t, endingTransferBalanceA, initialTransferBalance)
		assert.Equal(t, endingTransferBalanceB, initialTransferBalance)
	})
}

func TestAddRefreshTransferBalanceToAllNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		userIdA := server.NewId()
		networkIdA := server.NewId()
		userIdB := server.NewId()
		networkIdB := server.NewId()

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)

		subscriptionStartTime := server.NowUtc().Add(-15 * 24 * time.Hour)
		subscriptionEndTime := subscriptionStartTime.Add(30 * 24 * time.Hour)
		AddSubscriptionRenewal(
			ctx,
			&SubscriptionRenewal{
				NetworkId:        networkIdB,
				SubscriptionType: SubscriptionTypeSupporter,
				StartTime:        subscriptionStartTime,
				EndTime:          subscriptionEndTime,
				NetRevenue:       NanoCents(0),
			},
		)
		assert.Equal(t, HasSubscriptionRenewal(ctx, networkIdA, SubscriptionTypeSupporter), false)
		assert.Equal(t, HasSubscriptionRenewal(ctx, networkIdB, SubscriptionTypeSupporter), true)

		startTime := server.NowUtc()
		endTime := startTime.Add(24 * time.Hour)
		supporterTransferBalances := map[bool]ByteCount{
			false: 1 * Mib,
			true:  4 * Mib,
		}
		addedTransferBalances := AddRefreshTransferBalanceToAllNetworks(
			ctx,
			startTime,
			endTime,
			supporterTransferBalances,
		)

		assert.Equal(t, len(addedTransferBalances), 2)
		assert.Equal(t, addedTransferBalances[networkIdA], 1*Mib)
		assert.Equal(t, addedTransferBalances[networkIdB], 4*Mib)

		transferBalancesA := GetActiveTransferBalances(ctx, networkIdA)
		assert.Equal(t, 1, len(transferBalancesA))
		transferBalanceA := transferBalancesA[0]
		assert.Equal(t, addedTransferBalances[networkIdA], transferBalanceA.BalanceByteCount)
		assert.Equal(t, startTime, transferBalanceA.StartTime)
		assert.Equal(t, endTime, transferBalanceA.EndTime)

		transferBalancesB := GetActiveTransferBalances(ctx, networkIdB)
		assert.Equal(t, 1, len(transferBalancesB))
		transferBalanceB := transferBalancesB[0]
		assert.Equal(t, addedTransferBalances[networkIdB], transferBalanceB.BalanceByteCount)
		assert.Equal(t, startTime, transferBalanceB.StartTime)
		assert.Equal(t, endTime, transferBalanceB.EndTime)
	})
}

// FIXME a subsidy test where N clients pay each other
// FIXME each client uses a different amount of data, but sends to peer clients following the same offset distribution as the others
// FIXME the end result is that everyone should be paid the same, even though they get different amounts of data
