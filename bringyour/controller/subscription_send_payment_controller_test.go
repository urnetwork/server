package controller

import (
	"context"
	"fmt"
	"testing"

	"golang.org/x/exp/maps"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
	"github.com/go-playground/assert/v2"
)


var sendPaymentTransactionId = "123456"

func TestSubscriptionSendPayment(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {

		ctx := context.Background()

    netTransferByteCount := model.ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := model.UsdToNanoCents(10.00)

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

		mockCoinbaseClient := &mockCoinbaseApiClient{
			GetTransactionDataFunc: defaultGetTransactionDataHandler,
			SendPaymentFunc: defaultSendPaymentHandler,
		}

		// set a mock coinbase client so we don't make real api calls to coinbase
		SetCoinbaseClient(mockCoinbaseClient)

		// create a mock aws message sender
		mockAWSMessageSender := &mockAWSMessageSender{
			SendMessageFunc: func(userAuth string, template Template, sendOpts ...any) error {
				return nil
			},
		}

		// set the mock aws message sender
		// we don't want to send out messages in our tests
		SetMessageSender(mockAWSMessageSender)

		balanceCode, err := model.CreateBalanceCode(ctx, netTransferByteCount, netRevenue, "", "", "")
    assert.Equal(t, err, nil)
    model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
        Secret: balanceCode.Secret,
    }, sourceSession)

		transferEscrow, err := model.CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024 * 1024)
		assert.Equal(t, err, nil)					

    usedTransferByteCount := model.ByteCount(1024)
    paidByteCount := usedTransferByteCount

		model.CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
    model.CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)

    paid := model.UsdToNanoCents(model.ProviderRevenueShare * model.NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))

		destinationWalletAddress := "0x1234567890"

		wallet := &model.AccountWallet{
			NetworkId: destinationNetworkId,
			WalletType: model.WalletTypeCircleUserControlled,
			Blockchain: "matic",
			WalletAddress: destinationWalletAddress,
			DefaultTokenType: "usdc",
		}
		model.CreateAccountWallet(ctx, wallet)

		model.SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)

		paymentPlan := model.PlanPayments(ctx)
    assert.Equal(t, len(paymentPlan.WalletPayments), 0)
    assert.Equal(t, paymentPlan.WithheldWalletIds, []bringyour.Id{wallet.WalletId})

		usedTransferByteCount = model.ByteCount(1024 * 1024 * 1024)

    for paid < model.MinWalletPayoutThreshold {
        transferEscrow, err := model.CreateTransferEscrow(
					ctx, 
					sourceNetworkId, 
					sourceId, 
					destinationNetworkId, 
					destinationId, 
					usedTransferByteCount,
				)
        assert.Equal(t, err, nil)

        err = model.CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
        assert.Equal(t, err, nil)
        err = model.CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)
        assert.Equal(t, err, nil)
        paidByteCount += usedTransferByteCount
        paid += model.UsdToNanoCents(model.ProviderRevenueShare * model.NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
    }

		contractIds := model.GetOpenContractIds(ctx, sourceId, destinationId)
    assert.Equal(t, len(contractIds), 0)

		paymentPlan = model.PlanPayments(ctx)
		assert.Equal(t, maps.Keys(paymentPlan.WalletPayments), []bringyour.Id{wallet.WalletId})

		// these should hit -> default
		// payment.PaymentRecord should all be empty
		// meaning they will make a call to coinbase to send a transaction
		for _, payment := range paymentPlan.WalletPayments {
			paymentResult, err := CoinbasePayment(&CoinbasePaymentArgs{
				Payment: payment,
			}, destinationSession)
			assert.Equal(t, err, nil)
			assert.Equal(t, paymentResult.Complete, false)

			paymentRecord, err := model.GetPayment(ctx, payment.PaymentId)
			assert.Equal(t, err, nil)
			assert.Equal(t, paymentRecord.PaymentId, payment.PaymentId)
			assert.Equal(t, paymentRecord.TokenAmount, payment.TokenAmount)
			assert.Equal(t, paymentRecord.PaymentRecord, sendPaymentTransactionId)
		}

		pendingPayments := model.GetPendingPaymentsInPlan(ctx, paymentPlan.PaymentPlanId)

		// coinbase api will return a pending status
		// should return that payment is incomplete
		for _, payment := range pendingPayments {
			paymentResult, err := CoinbasePayment(&CoinbasePaymentArgs{
				Payment: payment,
			}, destinationSession)
			assert.Equal(t, err, nil)
			assert.Equal(t, paymentResult.Complete, false)

			// account balance should not yet be updated
			accountBalance := model.GetAccountBalance(destinationSession)
			assert.Equal(t, int64(0), accountBalance.Balance.PaidByteCount)
		}

		// set coinbase mock to return a completed status
		mockCoinbaseClient = &mockCoinbaseApiClient{
			GetTransactionDataFunc: func (transactionId string) (*GetCoinbaseTxDataResult, error) {
				staticData := &GetCoinbaseTxDataResult{
					TxData: &CoinbaseTransactionResponseData{
						TransactionId: sendPaymentTransactionId,
						Type:        "send",
						Status:      "completed",
					},
					ResponseBodyBytes: []byte(
						fmt.Sprintf(
							`{"id":"%s","type":"send","status":"completed"}`, 
							sendPaymentTransactionId,
						),
					),
				}
			
				return staticData, nil
			},
		}

		SetCoinbaseClient(mockCoinbaseClient)

		pendingPayments = model.GetPendingPaymentsInPlan(ctx, paymentPlan.PaymentPlanId)

		// these should hit completed
		for _, payment := range pendingPayments {

			paymentResult, err := CoinbasePayment(&CoinbasePaymentArgs{
				Payment: payment,
			}, destinationSession)
			assert.Equal(t, err, nil)
			assert.Equal(t, paymentResult.Complete, true)

			paymentRecord, err := model.GetPayment(ctx, payment.PaymentId)
			assert.Equal(t, err, nil)
			assert.Equal(t, paymentRecord.Completed, true)

			// check that the account balance has been updated
			accountBalance := model.GetAccountBalance(destinationSession)
			assert.Equal(t, accountBalance.Balance.PaidByteCount, paidByteCount)
		}

	})
}

type mockCoinbaseApiClient struct {
	GetTransactionDataFunc func(transactionId string) (*GetCoinbaseTxDataResult, error)
	SendPaymentFunc func(sendRequest *CoinbaseSendRequest, session *session.ClientSession) (*CoinbaseSendResponseData, error)
}

func (m *mockCoinbaseApiClient) getTransactionData(transactionId string) (*GetCoinbaseTxDataResult, error) {
	return m.GetTransactionDataFunc(transactionId)
}

func (m *mockCoinbaseApiClient) sendPayment(
	sendRequest *CoinbaseSendRequest, 
	session *session.ClientSession,
) (*CoinbaseSendResponseData, error) {
	return m.SendPaymentFunc(sendRequest, session)
}

func defaultGetTransactionDataHandler(transactionId string) (*GetCoinbaseTxDataResult, error) {
	staticData := &GetCoinbaseTxDataResult{
			TxData: &CoinbaseTransactionResponseData{
				TransactionId: sendPaymentTransactionId,
				Type:        "send",
				Status:      "pending",
			},
			ResponseBodyBytes: []byte(
				fmt.Sprintf(`{"id":"%s","type":"send","status":"USD"}`, 
				sendPaymentTransactionId),
			),
	}

	return staticData, nil
}

func defaultSendPaymentHandler(
	sendRequest *CoinbaseSendRequest, 
	session *session.ClientSession,
) (*CoinbaseSendResponseData, error) {
	staticData := &CoinbaseSendResponseData{
		TransactionId: sendPaymentTransactionId,
		Network: &CoinbaseSendResponseNetwork{
			Status: "pending",
			Hash: "0x0123",
			Name: "ethereum",
		},
	}

	return staticData, nil
}

type mockAWSMessageSender struct {
	SendMessageFunc func(userAuth string, template Template, sendOpts ...any) error
}

func (c *mockAWSMessageSender) SendAccountMessageTemplate(userAuth string, template Template, sendOpts ...any) error {
	return c.SendMessageFunc(userAuth, template, sendOpts...)
}
