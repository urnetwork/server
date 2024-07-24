package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/session"
)


func TestByteCount(t *testing.T) { (&bringyour.TestEnv{ApplyDbMigrations:false}).Run(func() {
    assert.Equal(t, ByteCountHumanReadable(ByteCount(0)), "0B")
    assert.Equal(t, ByteCountHumanReadable(ByteCount(5 * 1024 * 1024 * 1024 * 1024)), "5TiB")

    count, err := ParseByteCount("5MiB")
    assert.Equal(t, err, nil)
    assert.Equal(t, count, ByteCount(5 * 1024 * 1024))

    count, err = ParseByteCount("1.7GiB")
    assert.Equal(t, err, nil)
    assert.Equal(t, count, ByteCount(17 * 1024 * 1024 * 1024) / ByteCount(10))

    count, err = ParseByteCount("13.1TiB")
    assert.Equal(t, err, nil)
    assert.Equal(t, count, ByteCount(131 * 1024 * 1024 * 1024 * 1024) / ByteCount(10))
    
})}


func TestNanoCents(t *testing.T) { (&bringyour.TestEnv{ApplyDbMigrations:false}).Run(func() {
    usd := float64(1.55)
    a := UsdToNanoCents(usd)
    usd2 := NanoCentsToUsd(a)
    a2 := UsdToNanoCents(usd2)

    assert.Equal(t, usd, usd2)
    assert.Equal(t, a, a2)
})}


func TestEscrow(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
    ctx := context.Background()

    netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
    netRevenue := UsdToNanoCents(10.00)

    sourceNetworkId := bringyour.NewId()
    sourceId := bringyour.NewId()
    destinationNetworkId := bringyour.NewId()
    destinationId := bringyour.NewId()

    sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
        NetworkId: sourceNetworkId,
        ClientId: &sourceId,
    })
    destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
        NetworkId: destinationNetworkId,
        ClientId: &destinationId,
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



    transferEscrow, err = CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024 * 1024)
    assert.Equal(t, err, nil)

    contractIds = GetOpenContractIds(ctx, sourceId, destinationId)
    assert.Equal(t, contractIds, map[bringyour.Id]ContractParty{
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
    assert.Equal(t, netBalanceByteCount, netTransferByteCount - paidByteCount)


    wallet := &AccountWallet{
        WalletId: bringyour.NewId(),
        NetworkId: destinationNetworkId,
        WalletType: WalletTypeCircleUserControlled,
        Blockchain: "matic",
        WalletAddress: "",
        DefaultTokenType: "usdc",
    }
    CreateAccountWallet(ctx, wallet)
    SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)

    // plan a payment and complete the payment
    // nothing to plan because the payout does not meet the min threshold
    paymentPlan := PlanPayments(ctx)
    assert.Equal(t, len(paymentPlan.WalletPayments), 0)
    assert.Equal(t, paymentPlan.WithheldWalletIds, []bringyour.Id{wallet.WalletId})


    usedTransferByteCount = ByteCount(1024 * 1024 * 1024)
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

    contractIds = GetOpenContractIds(ctx, sourceId, destinationId)
    assert.Equal(t, len(contractIds), 0)

    getAccountBalanceResult = GetAccountBalance(destinationSession)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedByteCount, paidByteCount)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, paid)
    assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, ByteCount(0))
    assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

    paymentPlan = PlanPayments(ctx)
    assert.Equal(t, maps.Keys(paymentPlan.WalletPayments), []bringyour.Id{wallet.WalletId})

    for _, payment := range paymentPlan.WalletPayments {
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
    usedTransferByteCount = ByteCount(1024 * 1024 * 1024)
    for {
        transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, usedTransferByteCount)
        if err != nil && 1024 < usedTransferByteCount {
            usedTransferByteCount = usedTransferByteCount / 1024
            bringyour.Logger().Printf("Step down contract size to %d bytes.\n", usedTransferByteCount)
            continue
        }
        if netTransferByteCount <= paidByteCount {
            assert.NotEqual(t, err, nil)
            return
        } else {
            assert.Equal(t, err, nil)
        }

        CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
        CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
        paidByteCount += usedTransferByteCount
        paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
    }
    // at this point the balance should be fully used up
    
    transferBalances = GetActiveTransferBalances(ctx, sourceNetworkId)
    assert.Equal(t, transferBalances, []*TransferBalance{})

    paymentPlan = PlanPayments(ctx)
    assert.Equal(t, maps.Keys(paymentPlan.WalletPayments), []bringyour.Id{wallet.WalletId})

    for _, payment := range paymentPlan.WalletPayments {
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
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue)))
    assert.Equal(t, getAccountBalanceResult.Balance.PaidByteCount, netTransferByteCount)
    assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue)))


    // there shoud be no more payments
    paymentPlan = PlanPayments(ctx)
    assert.Equal(t, len(paymentPlan.WalletPayments), 0)
})}


// TODO escrow benchmark to see how many contracts can be opened and closed in some time period (e.g. 15s)


func TestBalanceCode(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
    ctx := context.Background()

    networkIdA := bringyour.NewId()

    userIdA := bringyour.NewId()

    clientSessionA := session.Testing_CreateClientSession(
        ctx,
        jwt.NewByJwt(networkIdA, userIdA, "a"),
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
})}


func TestSubscriptionPaymentId(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
    ctx := context.Background()

    networkIdA := bringyour.NewId()

    userIdA := bringyour.NewId()

    clientSessionA := session.Testing_CreateClientSession(
        ctx,
        jwt.NewByJwt(networkIdA, userIdA, "a"),
    )

    Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)


    result, err := SubscriptionCreatePaymentId(&SubscriptionCreatePaymentIdArgs{}, clientSessionA)
    assert.Equal(t, err, nil)
    assert.NotEqual(t, result, nil)

    resultNetworkId, err := SubscriptionGetNetworkIdForPaymentId(ctx, result.SubscriptionPaymentId)
    assert.Equal(t, err, nil)
    assert.Equal(t, networkIdA, resultNetworkId)
})}


func TestInitialBalance(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
    ctx := context.Background()

    networkIdA := bringyour.NewId()
    userIdA := bringyour.NewId()

    networkIdB := bringyour.NewId()
    userIdB := bringyour.NewId()

    Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
    Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)

    networkIds := FindNetworksWithoutTransferBalance(ctx)
    assert.Equal(t, 2, len(networkIds))
    assert.Equal(t, true, slices.Contains(networkIds, networkIdA))
    assert.Equal(t, true, slices.Contains(networkIds, networkIdB))

    for _, networkId := range networkIds {
        initialTransferBalance := ByteCount(30 * 1024 * 1024 * 1024)
        initialTransferBalanceDuration := 30 * 24 * time.Hour

        startTime := bringyour.NowUtc()
        endTime := startTime.Add(initialTransferBalanceDuration)
        success := AddBasicTransferBalance(
            ctx,
            networkId,
            initialTransferBalance,
            startTime,
            endTime,
        )
        assert.Equal(t, true, success)

        transferBalances := GetActiveTransferBalances(ctx, networkId)
        assert.Equal(t, 1, len(transferBalances))
        transferBalance := transferBalances[0]
        assert.Equal(t, initialTransferBalance, transferBalance.BalanceByteCount)
        assert.Equal(t, startTime, transferBalance.StartTime)
        assert.Equal(t, endTime, transferBalance.EndTime)
    }
})}

func TestGetPendingPayments(t *testing.T) { 
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
            ClientId: &sourceId,
        })

        // populate source balance
		balanceCode, err := CreateBalanceCode(ctx, netTransferByteCount, netRevenue, "user_balance_a", "", "")
        assert.Equal(t, err, nil)
        RedeemBalanceCode(&RedeemBalanceCodeArgs{
            Secret: balanceCode.Secret,
        }, sourceSession)

        usedTransferByteCount := ByteCount(1024)
        paid := UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
        paidByteCount := usedTransferByteCount

        usedTransferByteCount = ByteCount(1024 * 1024 * 1024)

        for paid < MinWalletPayoutThreshold {
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

        wallet := &AccountWallet{
            WalletId: bringyour.NewId(),
            NetworkId: destinationNetworkId,
            WalletType: WalletTypeCircleUserControlled,
            Blockchain: "matic",
            WalletAddress: "0x1234567890",
            DefaultTokenType: "usdc",
        }
        CreateAccountWallet(ctx, wallet)

        SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)

        plan := PlanPayments(ctx)

        paymentMap := make(map[bringyour.Id]map[bringyour.Id]bool)
        planPaymentCount := make(map[bringyour.Id]int)

        for _, payment := range plan.WalletPayments {
            SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")

            if _, exists := paymentMap[payment.PaymentPlanId]; !exists {
                paymentMap[payment.PaymentPlanId] = make(map[bringyour.Id]bool)
                planPaymentCount[payment.PaymentPlanId] = 0
            }

            paymentMap[payment.PaymentPlanId][payment.PaymentId] = true
            planPaymentCount[payment.PaymentPlanId] += 1
        }

        pendingPayments := GetPendingPayments(ctx)

        // this should match the planned payments we just created
        assert.Equal(t, len(pendingPayments), len(plan.WalletPayments))

        pendingPaymentsByPlan := GetPendingPaymentsInPlan(ctx, plan.PaymentPlanId)

        assert.Equal(t, planPaymentCount[plan.PaymentPlanId], len(pendingPaymentsByPlan))

        for _, pendingPayment := range pendingPaymentsByPlan {
            if _, exists := paymentMap[pendingPayment.PaymentPlanId][pendingPayment.PaymentId]; !exists {
                t.Errorf("Pending payment ID %v not found in planned payments", pendingPayment.PaymentId)
                continue
            }
            CompletePayment(ctx, pendingPayment.PaymentId, "")
        }

        // since payments have been marked completed
        // this should be empty
        pendingPaymentsByPlan = GetPendingPaymentsInPlan(ctx, plan.PaymentPlanId)
        
        assert.Equal(t, len(pendingPaymentsByPlan), 0)

        pendingPayments = GetPendingPayments(ctx)
        assert.Equal(t, len(pendingPayments), 0)
    })
}