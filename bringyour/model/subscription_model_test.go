package model


import (
    "context"
    "testing"

    "golang.org/x/exp/maps"

    "github.com/go-playground/assert/v2"

    "bringyour.com/bringyour"
    "bringyour.com/bringyour/jwt"
    "bringyour.com/bringyour/session"
)


func TestNanoCents(t *testing.T) { (&bringyour.TestEnv{ApplyDbMigrations:false}).Run(func() {
    usd := float64(1.55)
    a := USDToNanoCents(usd)
    usd2 := NanoCentsToUSD(a)
    a2 := USDToNanoCents(usd2)

    assert.Equal(t, usd, usd2)
    assert.Equal(t, a, a2)
})}


func TestEscrow(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
    ctx := context.Background()

    netTransferBytes := 1024 * 1024 * 1024 * 1024
    netRevenue := USDToNanoCents(10.00)

    sourceNetworkId := bringyour.NewId()
    sourceId := bringyour.NewId()
    destinationNetworkId := bringyour.NewId()
    destinationId := bringyour.NewId()

    sourceSession := session.NewLocalClientSession(ctx, &jwt.ByJwt{
        NetworkId: sourceNetworkId,
        ClientId: &sourceId,
    })
    destinationSession := session.NewLocalClientSession(ctx, &jwt.ByJwt{
        NetworkId: destinationNetworkId,
        ClientId: &destinationId,
    })

    getAccountBalanceResult := GetAccountBalance(sourceSession)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedBytes, 0)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
    assert.Equal(t, getAccountBalanceResult.Balance.PaidBytes, 0)
    assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

    getAccountBalanceResult = GetAccountBalance(destinationSession)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedBytes, 0)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
    assert.Equal(t, getAccountBalanceResult.Balance.PaidBytes, 0)
    assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

    balanceCode := CreateBalanceCode(ctx, netTransferBytes, netRevenue)
    RedeemBalanceCode(&RedeemBalanceCodeArgs{
        Secret: balanceCode.Secret,
    }, sourceSession)

    transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024 * 1024)
    assert.Equal(t, err, nil)

    contractIds := GetOpenContractIds(ctx, sourceId, destinationId)
    assert.Equal(t, contractIds, []bringyour.Id{transferEscrow.ContractId})

    usedTransferBytes := 1024
    CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferBytes)
    CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferBytes)
    paidBytes := usedTransferBytes
    paid := USDToNanoCents(ProviderRevenueShare * NanoCentsToUSD(netRevenue) * float64(usedTransferBytes) / float64(netTransferBytes))

    contractIds = GetOpenContractIds(ctx, sourceId, destinationId)
    assert.Equal(t, len(contractIds), 0)

    // check that the payout is pending
    getAccountBalanceResult = GetAccountBalance(sourceSession)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedBytes, 0)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
    assert.Equal(t, getAccountBalanceResult.Balance.PaidBytes, 0)
    assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

    getAccountBalanceResult = GetAccountBalance(destinationSession)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedBytes, paidBytes)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, paid)
    assert.Equal(t, getAccountBalanceResult.Balance.PaidBytes, 0)
    assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))


    wallet := &AccountWallet{
        NetworkId: destinationNetworkId,
        WalletType: WalletTypeCircleUsdcMatic,
        WalletAddress: "",
        DefaultTokenType: "usdc",
    }
    CreateAccountWallet(ctx, wallet)
    SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)

    // plan a payment and complete the payment
    // nothing to plan because the payout does not meet the min threshold
    paymentPlan := PlanPayments(ctx)
    assert.Equal(t, len(paymentPlan.WalletPayments), 0)


    for paid < MinWalletPayoutThreshold {
        transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024 * 1024)
        assert.NotEqual(t, err, nil)

        usedTransferBytes = 1024
        CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferBytes)
        CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferBytes)
        paidBytes += usedTransferBytes
        paid += USDToNanoCents(ProviderRevenueShare * NanoCentsToUSD(netRevenue) * float64(usedTransferBytes) / float64(netTransferBytes))
        bringyour.Logger().Printf("PAID %d %d\n", paidBytes, paid)
    }

    paymentPlan = PlanPayments(ctx)
    assert.Equal(t, maps.Keys(paymentPlan.WalletPayments), []bringyour.Id{wallet.WalletId})

    for _, payment := range paymentPlan.WalletPayments {
        SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUSD(payment.Payout), "")
        CompletePayment(ctx, payment.PaymentId, "")
    }
    
    // check that the payment is recorded
    getAccountBalanceResult = GetAccountBalance(sourceSession)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedBytes, 0)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
    assert.Equal(t, getAccountBalanceResult.Balance.PaidBytes, 0)
    assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

    getAccountBalanceResult = GetAccountBalance(destinationSession)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedBytes, paidBytes)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, paid)
    assert.Equal(t, getAccountBalanceResult.Balance.PaidBytes, paidBytes)
    assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, paid)


    // repeat escrow until it fails due to no balance
    for {
        transferEscrow, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024 * 1024)
        if netTransferBytes <= paidBytes {
            assert.NotEqual(t, err, nil)
            return
        } else {
            assert.Equal(t, err, nil)
        }

        usedTransferBytes = 1024
        CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferBytes)
        CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferBytes)
        paidBytes += usedTransferBytes
        paid += USDToNanoCents(ProviderRevenueShare * NanoCentsToUSD(netRevenue) * float64(usedTransferBytes) / float64(netTransferBytes))
        bringyour.Logger().Printf("PAID %d %d\n", paidBytes, paid)
    }
    // at this point the balance should be fully used up
    
    transferBalances := GetActiveTransferBalances(ctx, sourceNetworkId)
    assert.Equal(t, transferBalances, []*TransferBalance{})

    paymentPlan = PlanPayments(ctx)
    assert.Equal(t, maps.Keys(paymentPlan.WalletPayments), []bringyour.Id{wallet.WalletId})

    for _, payment := range paymentPlan.WalletPayments {
        SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUSD(payment.Payout), "")
        CompletePayment(ctx, payment.PaymentId, "")
    }

    // check that the payment is recorded
    getAccountBalanceResult = GetAccountBalance(sourceSession)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedBytes, 0)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, NanoCents(0))
    assert.Equal(t, getAccountBalanceResult.Balance.PaidBytes, 0)
    assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, NanoCents(0))

    // the revenue from 
    getAccountBalanceResult = GetAccountBalance(destinationSession)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedBytes, netTransferBytes)
    assert.Equal(t, getAccountBalanceResult.Balance.ProvidedNetRevenue, USDToNanoCents(ProviderRevenueShare * NanoCentsToUSD(netRevenue)))
    assert.Equal(t, getAccountBalanceResult.Balance.PaidBytes, netTransferBytes)
    assert.Equal(t, getAccountBalanceResult.Balance.PaidNetRevenue, USDToNanoCents(ProviderRevenueShare * NanoCentsToUSD(netRevenue)))


    // there shoud be no more payments
    paymentPlan = PlanPayments(ctx)
    assert.Equal(t, len(paymentPlan.WalletPayments), 0)
})}


// TODO escrow benchmark to see how many contracts can be opened and closed in some time period (e.g. 15s)
