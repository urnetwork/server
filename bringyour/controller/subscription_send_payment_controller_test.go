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
			ClientId:  &sourceId,
		})
		destinationSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: destinationNetworkId,
			ClientId:  &destinationId,
		})

		mockCircleClient := &mockCircleApiClient{
			GetTransactionFunc:            defaultGetTransactionDataHandler,
			CreateTransferTransactionFunc: defaultSendPaymentHandler,
			EstimateTransferFeeFunc:       defaultEstimateFeeHandler,
		}

		// set a mock coinbase client so we don't make real api calls to coinbase
		SetCircleClient(mockCircleClient)

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

		transferEscrow, err := model.CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024*1024)
		assert.Equal(t, err, nil)

		usedTransferByteCount := model.ByteCount(1024)
		paidByteCount := usedTransferByteCount

		model.CloseContract(ctx, transferEscrow.ContractId, sourceId, usedTransferByteCount, false)
		model.CloseContract(ctx, transferEscrow.ContractId, destinationId, usedTransferByteCount, false)

		paid := model.UsdToNanoCents(model.ProviderRevenueShare * model.NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))

		destinationWalletAddress := "0x1234567890"

		args := &model.CreateAccountWalletExternalArgs{
			Blockchain:       "MATIC",
			WalletAddress:    destinationWalletAddress,
			DefaultTokenType: "USDC",
		}
		walletId := model.CreateAccountWalletExternal(destinationSession, args)
		assert.NotEqual(t, walletId, nil)

		wallet := model.GetAccountWallet(ctx, *walletId)

		model.SetPayoutWallet(ctx, destinationNetworkId, wallet.WalletId)

		paymentPlan, err := model.PlanPayments(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(paymentPlan.WalletPayments), 0)
		assert.Equal(t, paymentPlan.WithheldWalletIds, []bringyour.Id{wallet.WalletId})

		usedTransferByteCount = model.ByteCount(1024 * 1024 * 1024)

		for paid < 2*model.UsdToNanoCents(model.EnvSubsidyConfig().MinWalletPayoutUsd) {
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

		paymentPlan, err = model.PlanPayments(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, maps.Keys(paymentPlan.WalletPayments), []bringyour.Id{wallet.WalletId})

		// these should hit -> default
		// payment.PaymentRecord should all be empty
		// meaning they will make a call to send a transaction to the provider
		for _, payment := range paymentPlan.WalletPayments {

			// initiate payment
			complete, err := advancePayment(payment, destinationSession)
			assert.Equal(t, err, nil)
			assert.Equal(t, complete, false)

			// fetch the payment record which should have some updated fields
			paymentRecord, err := model.GetPayment(ctx, payment.PaymentId)
			assert.Equal(t, err, nil)

			// check the wallet address is correctly joined
			assert.Equal(t, paymentRecord.WalletAddress, destinationWalletAddress)

			// estimate fee
			estimatedFees, err := mockCircleClient.EstimateTransferFee(
				*paymentRecord.TokenAmount,
				wallet.WalletAddress,
				"MATIC",
			)
			assert.Equal(t, err, nil)

			// deduct fee from payment amount
			fee, err := CalculateFee(
				*estimatedFees.Medium,
				"MATIC",
			)
			assert.Equal(t, err, nil)

			// convert fee to usdc
			feeInUSDC, err := ConvertFeeToUSDC("MATIC", *fee)
			assert.Equal(t, err, nil)

			assert.Equal(t, err, nil)
			assert.Equal(t, paymentRecord.PaymentId, payment.PaymentId)

			// payoutAmount (in USD) - fee (in USD) = token amount
			assert.Equal(t, model.NanoCentsToUsd(paymentRecord.Payout)-*feeInUSDC, float64(*paymentRecord.TokenAmount))
			assert.Equal(t, paymentRecord.PaymentRecord, sendPaymentTransactionId)
		}

		pendingPayments := model.GetPendingPaymentsInPlan(ctx, paymentPlan.PaymentPlanId)

		// coinbase api will return a pending status
		// should return that payment is incomplete
		for _, payment := range pendingPayments {
			complete, err := advancePayment(payment, destinationSession)
			assert.Equal(t, err, nil)
			assert.Equal(t, complete, false)

			// account balance should not yet be updated
			accountBalance := model.GetAccountBalance(destinationSession)
			assert.Equal(t, int64(0), accountBalance.Balance.PaidByteCount)
		}

		// set coinbase mock to return a completed status
		mockCircleClient = &mockCircleApiClient{
			GetTransactionFunc: func(transactionId string) (*GetTransactionResult, error) {
				staticData := &GetTransactionResult{
					Transaction: CircleTransaction{
						State:      "COMPLETE",
						Id:         transactionId,
						Blockchain: "MATIC",
					},
					ResponseBodyBytes: []byte(
						fmt.Sprintf(
							`{"id":"%s","state":"COMPLETED","blockchain":"MATIC"}`,
							sendPaymentTransactionId,
						),
					),
				}

				return staticData, nil
			},
		}

		SetCircleClient(mockCircleClient)

		pendingPayments = model.GetPendingPaymentsInPlan(ctx, paymentPlan.PaymentPlanId)

		// these should hit completed
		for _, payment := range pendingPayments {

			complete, err := advancePayment(payment, destinationSession)
			assert.Equal(t, err, nil)
			assert.Equal(t, complete, true)

			paymentRecord, err := model.GetPayment(ctx, payment.PaymentId)
			assert.Equal(t, err, nil)
			assert.Equal(t, paymentRecord.Completed, true)

			// check that the account balance has been updated
			accountBalance := model.GetAccountBalance(destinationSession)
			assert.Equal(t, accountBalance.Balance.PaidByteCount, paidByteCount)
		}

		networkPayments, err := GetNetworkAccountPayments(destinationSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(networkPayments.AccountPayments), len(pendingPayments))
		assert.Equal(t, networkPayments.AccountPayments[0].NetworkId, destinationSession.ByJwt.NetworkId)

		sourcePayments, err := GetNetworkAccountPayments(sourceSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(sourcePayments.AccountPayments), 0)
	})
}

func TestFeeToUsd(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		coinbaseClient := &mockCoinbaseClient{
			FetchExchangeRatesFunc: func(currencyTicker string) (*CoinbaseExchangeRatesResults, error) {
				staticData := &CoinbaseExchangeRatesResults{
					Currency: "MATIC",
					Rates: map[string]string{
						"USDC": "0.50",
					},
				}

				return staticData, nil
			},
		}

		SetCoinbaseClient(coinbaseClient)
		// fee of 1 Matic for testing
		fee := 1.0

		feeInUSDC, err := ConvertFeeToUSDC("MATIC", fee)
		assert.Equal(t, err, nil)
		assert.Equal(t, *feeInUSDC, 0.50)
	})
}

type mockCircleApiClient struct {
	GetTransactionFunc            func(id string) (*GetTransactionResult, error)
	CreateTransferTransactionFunc func(
		amount float64,
		destinationAddress string,
		network string,
	) (*CreateTransferTransactionResult, error)
	EstimateTransferFeeFunc func(
		amount float64,
		destinationAddress string,
		network string,
	) (*FeeEstimateResult, error)
}

func (m *mockCircleApiClient) GetTransaction(id string) (*GetTransactionResult, error) {
	return m.GetTransactionFunc(id)
}

func (m *mockCircleApiClient) CreateTransferTransaction(
	amount float64,
	destinationAddress string,
	network string,
) (*CreateTransferTransactionResult, error) {
	return m.CreateTransferTransactionFunc(
		amount,
		destinationAddress,
		network,
	)
}

func (m *mockCircleApiClient) EstimateTransferFee(
	amount float64,
	destinationAddress string,
	network string,
) (*FeeEstimateResult, error) {
	return m.EstimateTransferFeeFunc(
		amount,
		destinationAddress,
		network,
	)
}

func defaultGetTransactionDataHandler(transactionId string) (*GetTransactionResult, error) {
	return &GetTransactionResult{
		Transaction: CircleTransaction{
			State:      "INITIATED",
			Id:         transactionId,
			Blockchain: "MATIC",
		},
		ResponseBodyBytes: []byte(
			fmt.Sprintf(
				`{"id":"%s","state":"INITIATED","blockchain":"MATIC"}`,
				sendPaymentTransactionId,
			),
		),
	}, nil
}

func defaultSendPaymentHandler(
	amount float64,
	destinationAddress string,
	network string,
) (*CreateTransferTransactionResult, error) {
	staticData := &CreateTransferTransactionResult{
		Id:    sendPaymentTransactionId,
		State: "INITIATED",
	}

	return staticData, nil
}

func defaultEstimateFeeHandler(
	amount float64,
	destinationAddress string,
	network string,
) (*FeeEstimateResult, error) {
	staticData := &FeeEstimateResult{
		High: &FeeEstimate{
			GasLimit:    "58858",
			PriorityFee: "45",
			BaseFee:     "0.000000035",
			MaxFee:      "30.926400608",
		},
		Medium: &FeeEstimate{
			GasLimit:    "58858",
			PriorityFee: "36.499999998",
			BaseFee:     "0.000000035",
			MaxFee:      "30.926400608",
		},
		Low: &FeeEstimate{
			GasLimit:    "58858",
			PriorityFee: "32.940489888",
			BaseFee:     "0.000000035",
			MaxFee:      "30.926400608",
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

type mockCoinbaseClient struct {
	FetchExchangeRatesFunc func(currencyTicker string) (*CoinbaseExchangeRatesResults, error)
}

func (m *mockCoinbaseClient) FetchExchangeRates(currencyTicker string) (*CoinbaseExchangeRatesResults, error) {
	return m.FetchExchangeRatesFunc(currencyTicker)
}
