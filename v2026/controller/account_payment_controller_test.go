package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

var sendPaymentTransactionId = "123456"

func TestSubscriptionSendPayment(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		netTransferByteCount := model.ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := model.UsdToNanoCents(10.00)

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

		subscriptionYearDuration := 365 * 24 * time.Hour

		balanceCode, err := model.CreateBalanceCode(
			ctx,
			netTransferByteCount,
			subscriptionYearDuration,
			netRevenue,
			"",
			"",
			"",
		)
		assert.Equal(t, err, nil)
		model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: sourceSession.ByJwt.NetworkId,
		}, ctx)

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
		assert.Equal(t, len(paymentPlan.NetworkPayments), 0)
		assert.Equal(t, paymentPlan.WithheldNetworkIds, []server.Id{destinationNetworkId})

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
		assert.Equal(t, maps.Keys(paymentPlan.NetworkPayments), []server.Id{destinationNetworkId})

		// these should hit -> default
		// payment.PaymentRecord should all be empty
		// meaning they will make a call to send a transaction to the provider
		for _, payment := range paymentPlan.NetworkPayments {

			// initiate payment
			complete, canceled, err := advancePayment(payment, destinationSession)
			assert.Equal(t, err, nil)
			assert.Equal(t, complete, false)
			assert.Equal(t, canceled, false)

			// fetch the payment record which should have some updated fields
			paymentRecord, err := model.GetPayment(ctx, payment.PaymentId)
			assert.Equal(t, err, nil)

			// check the wallet address is correctly joined
			assert.Equal(t, paymentRecord.WalletAddress, destinationWalletAddress)

			// estimate fee
			// estimatedFees, err := mockCircleClient.EstimateTransferFee(
			// 	ctx,
			// 	*paymentRecord.TokenAmount,
			// 	wallet.WalletAddress,
			// 	"MATIC",
			// )
			// assert.Equal(t, err, nil)

			// // deduct fee from payment amount
			// fee, err := CalculateFee(
			// 	*estimatedFees.Medium,
			// 	"MATIC",
			// )
			// assert.Equal(t, err, nil)

			// // convert fee to usdc
			// feeInUSDC, err := ConvertFeeToUSDC(ctx, "MATIC", *fee)
			feeInUSDC := 0.01 // this is now hardcoded due to Circle API errors

			assert.Equal(t, err, nil)
			assert.Equal(t, paymentRecord.PaymentId, payment.PaymentId)
			assert.Equal(t, paymentRecord.Blockchain, "MATIC")

			// payoutAmount (in USD) - fee (in USD) = token amount
			assert.Equal(t, model.NanoCentsToUsd(paymentRecord.Payout)-feeInUSDC, float64(*paymentRecord.TokenAmount))
			assert.Equal(t, paymentRecord.PaymentRecord, sendPaymentTransactionId)
		}

		pendingPayments := model.GetPendingPaymentsInPlan(ctx, paymentPlan.PaymentPlanId)

		// coinbase api will return a pending status
		// should return that payment is incomplete
		for _, payment := range pendingPayments {
			complete, canceled, err := advancePayment(payment, destinationSession)
			assert.Equal(t, err, nil)
			assert.Equal(t, complete, false)
			assert.Equal(t, canceled, false)

			// account balance should not yet be updated
			accountBalance := model.GetAccountBalance(destinationSession)
			assert.Equal(t, int64(0), accountBalance.Balance.PaidByteCount)
		}

		mockTxHash := "0x1234567890abcdef"

		// set coinbase mock to return a completed status
		mockCircleClient = &mockCircleApiClient{
			GetTransactionFunc: func(ctx context.Context, transactionId string) (*GetTransactionResult, error) {
				staticData := &GetTransactionResult{
					Transaction: CircleTransaction{
						State:      "COMPLETE",
						Id:         transactionId,
						Blockchain: "MATIC",
						TxHash:     mockTxHash,
					},
					ResponseBodyBytes: []byte(
						fmt.Sprintf(
							`{"id":"%s","state":"COMPLETED","blockchain":"MATIC","tx_hash":%s}`,
							sendPaymentTransactionId,
							mockTxHash,
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

			complete, canceled, err := advancePayment(payment, destinationSession)
			assert.Equal(t, err, nil)
			assert.Equal(t, complete, true)
			assert.Equal(t, canceled, false)

			paymentRecord, err := model.GetPayment(ctx, payment.PaymentId)
			assert.Equal(t, err, nil)
			assert.Equal(t, paymentRecord.Completed, true)
			assert.Equal(t, paymentRecord.TxHash, mockTxHash)
			assert.Equal(t, paymentRecord.Blockchain, "MATIC")

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
	server.DefaultTestEnv().Run(func() {
		coinbaseClient := &mockCoinbaseClient{
			FetchExchangeRatesFunc: func(ctx context.Context, currencyTicker string) (*CoinbaseExchangeRatesResults, error) {
				staticData := &CoinbaseExchangeRatesResults{
					Currency: "MATIC",
					Rates: map[string]string{
						"USDC": "0.50",
					},
				}

				return staticData, nil
			},
		}

		ctx := context.Background()

		SetCoinbaseClient(coinbaseClient)
		// fee of 1 Matic for testing
		fee := 1.0

		feeInUSDC, err := ConvertFeeToUSDC(ctx, "MATIC", fee)
		assert.Equal(t, err, nil)
		assert.Equal(t, feeInUSDC, 0.50)
	})
}

type mockCircleApiClient struct {
	GetTransactionFunc func(
		ctx context.Context,
		id string,
	) (*GetTransactionResult, error)
	CreateTransferTransactionFunc func(
		ctx context.Context,
		amount float64,
		destinationAddress string,
		network string,
	) (*CreateTransferTransactionResult, error)
	EstimateTransferFeeFunc func(
		ctx context.Context,
		amount float64,
		destinationAddress string,
		network string,
	) (*FeeEstimateResult, error)
}

func (m *mockCircleApiClient) GetTransaction(
	ctx context.Context,
	id string,
) (*GetTransactionResult, error) {
	return m.GetTransactionFunc(ctx, id)
}

func (m *mockCircleApiClient) CreateTransferTransaction(
	ctx context.Context,
	amount float64,
	destinationAddress string,
	network string,
) (*CreateTransferTransactionResult, error) {
	return m.CreateTransferTransactionFunc(
		ctx,
		amount,
		destinationAddress,
		network,
	)
}

func (m *mockCircleApiClient) EstimateTransferFee(
	ctx context.Context,
	amount float64,
	destinationAddress string,
	network string,
) (*FeeEstimateResult, error) {
	return m.EstimateTransferFeeFunc(
		ctx,
		amount,
		destinationAddress,
		network,
	)
}

func defaultGetTransactionDataHandler(ctx context.Context, transactionId string) (*GetTransactionResult, error) {
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
	ctx context.Context,
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
	ctx context.Context,
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
	FetchExchangeRatesFunc func(ctx context.Context, currencyTicker string) (*CoinbaseExchangeRatesResults, error)
}

func (m *mockCoinbaseClient) FetchExchangeRates(ctx context.Context, currencyTicker string) (*CoinbaseExchangeRatesResults, error) {
	return m.FetchExchangeRatesFunc(ctx, currencyTicker)
}
